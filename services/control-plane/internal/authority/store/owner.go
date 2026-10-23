package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// The owner verbs' refusals (B3-D12). cmd/account-authority maps each to its
// exit reason. ErrRefused is wrapped with the verb's own reason, which the
// CLI prints.
var (
	// ErrNotLeader: the verb's connection reached a standby (B3-D7).
	ErrNotLeader = errors.New("authority store: not the leader")
	// ErrNotReplicated: the verb committed locally and no synchronous standby
	// confirmed it before its deadline (D-225). A re-run converges (B3-D8).
	ErrNotReplicated = errors.New("authority store: committed locally, not replicated")
	// ErrRefused: a verb-specific refusal, such as a digest mismatch, an
	// unknown key or a wrong state.
	ErrRefused = errors.New("authority store: refused")
)

// OwnerConfig is what OpenOwner needs. Signer is nil unless the verb signs,
// which only clear-held does (B3-D1).
type OwnerConfig struct {
	DB      *sql.DB // the authority_owner pool
	Signer  signer.Signer
	Anchors []byte
	// TrustFile is the trust file the verb was started with, nil for none.
	// revoke-key matches against it as well as the installed trust, once the
	// whole file verifies under the anchors (B3-D10); no other verb reads it.
	TrustFile []byte
	Probe     ReplicationProbe // nil is the SQL probe
	Logger    *slog.Logger
	// InRecovery is the leader check's role read, Hooks.InRecovery's shape;
	// nil reads pg_is_in_recovery() on the verb's connection (B3-D7).
	InRecovery func(ctx context.Context, conn *sql.Conn) (bool, error)
}

// Owner is the owner verbs' store (B3-D3, §3.4): restore-begin,
// reconcile-complete, clear-held, seal-legacy, end-drain, revoke-key and
// replication-gate. It runs over the authority_owner pool, the role the
// database guards demand (account_heads_guard, delegations_guard,
// revoked_keys' grants), and holds none of serve's state: no admission
// slots, and no D-241 memory, which is why clear-held runs with serve
// stopped (B3-D1).
type Owner struct {
	db         *sql.DB
	sig        signer.Signer // nil unless clear-held
	anchors    ac.Anchors
	probe      ReplicationProbe
	log        *slog.Logger
	inRecovery func(ctx context.Context, conn *sql.Conn) (bool, error) // nil is pg_is_in_recovery()
	// anchorBytes are cfg.Anchors as given: clear-held opens its commit core
	// through Open, which decodes them itself.
	anchorBytes []byte
	// trustFile is cfg.TrustFile as given, nil for none: revoke-key's alone.
	trustFile []byte
	// afterRestoreCancel is a test seam: nil outside tests. clear-held calls it
	// after a row 22 committed and before that account's L1 transaction. A
	// non-nil return ends the account there and is classified as any other
	// error (clearRefusal): an ErrRefused refuses the account, anything else
	// stops the run (Review Focus 2). Only the seam can refuse an account
	// after its row 22 committed; no production error there is a refusal.
	afterRestoreCancel func() error
}

// OpenOwner validates cfg, as Open does, and performs no I/O. It refuses a
// nil pool and anchors that do not decode. A nil Signer is valid: Open's
// non-nil-signer rule binds serve alone (B3-D3).
func OpenOwner(cfg OwnerConfig) (*Owner, error) {
	if cfg.DB == nil {
		return nil, errors.New("authority store: nil database")
	}
	a, err := ac.DecodeAnchors(cfg.Anchors)
	if err != nil {
		return nil, errors.New("authority store: anchors do not decode")
	}
	o := &Owner{db: cfg.DB, sig: cfg.Signer, anchors: a, probe: cfg.Probe, log: cfg.Logger, inRecovery: cfg.InRecovery,
		anchorBytes: bytes.Clone(cfg.Anchors), trustFile: bytes.Clone(cfg.TrustFile)} // Clone keeps nil nil
	if o.probe == nil {
		o.probe = SQLProbe{}
	}
	if o.log == nil {
		o.log = slog.Default()
	}
	return o, nil
}

// conn is where every owner verb gets its connection, with its leader and
// realm checks (B3-D7, step 1a's rule; B2-D10). Two other places make the
// same checks: checkBeforeLock runs conn once before a verb takes ServeLock,
// and clear-held's row 22, on the commit core's own connection, reads the
// role before BEGIN and checks the realm inside its transaction
// (commitRestoreCancel). conn hands back a connection only when the role read
// on that same connection succeeded and said primary, and then meta, read on
// it too, exists and names the anchors' realm.
// A standby closes it and refuses with ErrNotLeader; a role read that fails
// closes it and returns that failure, never ErrNotLeader, so the operator is
// not sent looking for a primary that may be this one. A primary with no meta
// row is ErrNoMeta and one of another realm ErrRealmMismatch, each closing the
// connection with nothing read or written past meta, so an
// AUTHORITY_MIGRATE_DATABASE_URL aimed at another realm's primary mutates
// nothing (Codex round 2, G4); a realm read that fails is that failure. The
// caller closes the returned connection.
func (o *Owner) conn(ctx context.Context) (*sql.Conn, error) {
	c, err := o.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("authority store: owner connection: %w", err)
	}
	inRecovery, err := readInRecovery(ctx, c, o.inRecovery)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("authority store: role read: %w", err)
	}
	if inRecovery {
		_ = c.Close()
		return nil, ErrNotLeader
	}
	// checkRealm reads only the anchors of its Store, as RestoreBegin's
	// loadTrust does.
	if err := (&Store{anchors: o.anchors}).checkRealm(ctx, c); err != nil {
		_ = c.Close()
		if errors.Is(err, ErrNoMeta) || errors.Is(err, ErrRealmMismatch) {
			return nil, err
		}
		return nil, fmt.Errorf("authority store: realm read: %w", err)
	}
	return c, nil
}

// checkBeforeLock is conn's leader and realm checks, on a connection it
// closes, for a verb that takes ServeLock next (stopped, ClearHeld). Without
// it a misaimed AUTHORITY_MIGRATE_DATABASE_URL, a standby or another realm's
// primary, whose serve holds the lock, would exit lock, whose remedy (a re-run
// converges) does not apply there, and a run against another realm's idle
// primary would hold that realm's ServeLock until conn refused it. The checks
// that bind are conn's, made again once the lock is held, so a failover
// between the two is still refused.
func (o *Owner) checkBeforeLock(ctx context.Context) error {
	c, err := o.conn(ctx)
	if err != nil {
		return err
	}
	_ = c.Close()
	return nil
}

// finish is every owner verb's last step, after its commit on conn (D-225):
// it returns nil only once waitReplicated, the wait release uses, reports the
// commit replicated, or reports replication not required. Anything else is
// ErrNotReplicated, wrapping the cause when there is one: the verb committed
// locally and nothing confirmed a standby has it, which a re-run converges
// (B3-D8). ctx bounds the wait; the caller sets its deadline.
func (o *Owner) finish(ctx context.Context, conn *sql.Conn) error {
	ok, err := waitReplicated(ctx, conn, o.probe)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotReplicated, err)
	}
	if !ok {
		return ErrNotReplicated
	}
	return nil
}
