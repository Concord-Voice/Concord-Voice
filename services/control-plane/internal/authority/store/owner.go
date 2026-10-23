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
	// after a row 22 committed and before that account's L1 transaction, and a
	// non-nil return refuses the account there (Review Focus 2).
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

// conn is where every owner verb gets its connection, and so the one place
// its leader check lives (B3-D7, step 1a's rule). It hands back conn only
// when the role read on that same connection succeeded and said primary. A
// standby closes it and refuses with ErrNotLeader; a role read that fails
// closes it and returns that failure, never ErrNotLeader, so the operator is
// not sent looking for a primary that may be this one. The caller closes the
// returned connection.
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
	return c, nil
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
