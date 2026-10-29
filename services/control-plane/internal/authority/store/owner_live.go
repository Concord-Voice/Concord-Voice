package store

import (
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"math"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// The live owner verbs (§3.4, DoR:1732–1735): seal-legacy, revoke-key,
// replication-gate and end-drain, which run against a live serve (D-171).
// Each is one transaction on a connection that passed the leader check (B3-D7), then
// finish on that same connection (D-225), so a re-run of a verb that
// committed but did not replicate converges (B3-D8): every re-run still
// ends on finish, whether or not its transaction changed anything, because
// the first run's commit may be the one still waiting on a standby.

// Owner verbs, as audit rows and refusals name them.
const (
	verbRevokeKey  = "revoke-key"
	verbEndDrain   = "end-drain"
	verbSealLegacy = "seal-legacy"
)

const refusedTrustUndecodable = "the trust file does not decode"

// revoked_keys.kind's three values (D-207).
const (
	revokeCAK      = "cak"
	revokeOperator = "operator"
	revokeCPTLS    = "cp_tls"
)

// refused is ErrRefused with the verb's reason. A reason never carries a
// digest, a key or an account id (CWE-532): the CLI prints it.
func refused(verb, reason string) error {
	return fmt.Errorf("%w: %s: %s", ErrRefused, verb, reason)
}

// write is every live verb's shape: the leader check on the verb's own
// connection (conn), body in one transaction, commit, then finish. A body
// error rolls back and is returned as it is, so ErrRefused stays ErrRefused.
// The transaction is READ COMMITTED by name, as every store transaction is,
// rather than by the database default: SealLegacy's digest must be read in a
// snapshot taken after its meta lock, which REPEATABLE READ would take at the
// lock statement itself (population.go).
func (o *Owner) write(ctx context.Context, verb string, body func(ctx context.Context, tx *sql.Tx) error) error {
	return o.writeAfter(ctx, verb, nil, body)
}

// writeAfter is write with prepare, when non-nil, run on the verb's own
// connection after the leader check and before the transaction begins:
// restore-begin installs the trust file there (DoR:1744 step 4). A prepare
// error is returned as it is, and nothing after it runs. prepare and body are
// handed writeAfter's own ctx, which stopped derives from the serve lock's
// watch, so a closure never runs under a context the verb no longer has.
//
// A prepare that returned nil may have committed (restore-begin's install
// commits in its own transaction), so from there every path ends on finish,
// a failed begin, body or commit included: otherwise rows it installed would
// skip the replication gate. The transaction's error is then returned as it
// is when finish succeeds, and joined with finish's error when it does not,
// so the exit is replication and the re-run converges (B3-D8). Without
// prepare, an error before the commit returns as it is: nothing committed.
func (o *Owner) writeAfter(ctx context.Context, verb string, prepare func(ctx context.Context, conn *sql.Conn) error,
	body func(ctx context.Context, tx *sql.Tx) error) error {
	conn, err := o.conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if prepare == nil {
		if err := transact(ctx, conn, verb, body); err != nil {
			return err
		}
		return o.finish(ctx, conn)
	}
	if err := prepare(ctx, conn); err != nil {
		return err
	}
	err = transact(ctx, conn, verb, body)
	ferr := o.finish(ctx, conn)
	switch {
	case err == nil:
		return ferr
	case ferr != nil:
		return errors.Join(err, ferr)
	}
	return err
}

// transact is a live verb's one transaction on conn: begin, the owner
// timeouts, body, commit. A body error rolls back and is returned as it is,
// so ErrRefused stays ErrRefused.
func transact(ctx context.Context, conn *sql.Conn, verb string, body func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("authority store: %s: begin: %w", verb, err)
	}
	if err := setOwnerTimeouts(ctx, tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("authority store: %s: timeouts: %w", verb, err)
	}
	if err := body(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("authority store: %s: commit: %w", verb, err)
	}
	return nil
}

// setOwnerTimeouts is an owner verb transaction's local bounds: a lock waits
// at most 2 s and the transaction may sit idle at most 10 s, setLocalTimeouts'
// two. It sets no statement_timeout, deliberately: reconcile-complete floors
// every pending head in statements that grow with the realm (B3-D13's
// throughput is unmeasured), and a 5 s bound could make it fail on every run
// during an incident. A statement is bounded by the verb's own deadline
// (ownerDeadline, 240 s).
func setOwnerTimeouts(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{`SET LOCAL lock_timeout = '2s'`, `SET LOCAL idle_in_transaction_session_timeout = '10s'`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// RevokeKey is revoke-key (D-207, B3-D10): it inserts digest into
// revoked_keys under kind, once the digest is in the form serve compares for
// that kind and some delegation the installed trust accepts lists it:
//   - cak and operator: ac.KeyDigest of a 97-byte point the delegation lists
//     under cak_public_keys or operator_public_keys;
//   - cp_tls: one of the delegation's cp_tls_pins, the SHA-384 of the key's
//     SPKI DER (ac.TLSPin). A delegation lists CP TLS keys only as pins, so
//     any other cp_tls digest, a key digest included, is refused with a
//     message naming the pin form.
//
// A delegation that is current or superseded counts: containment is for a
// key a superseded delegation still lists (P-OVERLAP). So does a delegation
// only the trust file holds, so a key can be contained before the delegation
// listing it is installed; but only once the whole file verifies under the
// anchors (fileDelegations). A file that does not refuses the verb, an
// installed digest included, rather than shrinking the match to the installed
// half: the operator named that file, and a silent fallback would hide that it
// is not the realm's. A digest listed only under another kind, an unknown
// digest, a wrong length and an unknown kind are refused, since revoked_keys
// accepts any 48 bytes and a mistyped row silently never matches (DoR:1734).
// An existing row converges to nil (B3-D8).
func (o *Owner) RevokeKey(ctx context.Context, kind string, digest []byte) error {
	return o.write(ctx, verbRevokeKey, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkRevocationArgs(kind, digest); err != nil {
			return err
		}
		fileDs, err := fileDelegations(o.anchors, o.trustFile)
		if err != nil {
			return err
		}
		ds, err := installedDelegations(ctx, tx, o.anchors)
		if err != nil {
			return fmt.Errorf("authority store: revoke-key: installed trust: %w", err)
		}
		if !listed(digest, listedDigests(kind, append(ds, fileDs...))) {
			return unlisted(kind)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, $2)
			ON CONFLICT (key_digest) DO NOTHING`, digest, kind); err != nil {
			return fmt.Errorf("authority store: revoke-key: insert: %w", err)
		}
		return nil
	})
}

func checkRevocationArgs(kind string, digest []byte) error {
	switch kind {
	case revokeCAK, revokeOperator, revokeCPTLS:
	default:
		return refused(verbRevokeKey, "the kind is cak, operator or cp_tls")
	}
	if len(digest) != sha512.Size384 {
		return refused(verbRevokeKey, "a digest is 48 bytes, SHA-384")
	}
	return nil
}

// unlisted is the refusal of a digest no installed or trust-file delegation
// lists under kind, naming the form that kind takes.
func unlisted(kind string) error {
	if kind == revokeCPTLS {
		return refused(verbRevokeKey, "cp_tls takes the pin, the SHA-384 of the key's SPKI DER as a delegation "+
			"lists it in cp_tls_pins, and no installed or trust-file delegation lists this one")
	}
	return refused(verbRevokeKey, kind+" takes the SHA-384 of the key's 97-byte point, and no installed or "+
		"trust-file delegation lists this one as "+kind)
}

// fileDelegations is every delegation in file, once the whole file verifies
// under anchors alone: the envelope decodes (§2.6), and A2's Trust over the
// anchors admits every item, in file order, with nothing ignored and no halt.
// A trust file carries every succession and delegation ever issued, so a
// genuine one verifies whole; an ignored item is one the anchors did not sign
// (T2/T3), and a halt (T4) is a trust no client keeps using. Either refuses,
// as an undecodable file does, where LoadTrust and AddTrust skip an ignored
// item: revoke-key's match must never rest on a file part of which is not the
// realm's. A nil file is no file and lists nothing.
func fileDelegations(anchors ac.Anchors, file []byte) ([]ac.Delegation, error) {
	if file == nil {
		return nil, nil
	}
	env, err := ac.CheckTrustEnvelope(file)
	if err != nil {
		return nil, refused(verbRevokeKey, refusedTrustUndecodable)
	}
	tr, err := ac.NewTrust(anchors)
	if err != nil {
		return nil, fmt.Errorf("authority store: revoke-key: anchors: %w", err)
	}
	var ds []ac.Delegation
	for _, it := range env.Items {
		var halt *ac.HaltError
		switch err := tr.AddSigned(it); {
		case errors.As(err, &halt):
			return nil, refused(verbRevokeKey, "the trust file's trust halts (T4)")
		case err != nil: // ignored (T2/T3) or malformed (EN5)
			return nil, refused(verbRevokeKey, "an item of the trust file does not verify under the anchors")
		}
		sg, err := ac.DecodeSigned(it)
		if err != nil {
			return nil, refused(verbRevokeKey, refusedTrustUndecodable)
		}
		if sg.Type != "delegation" {
			continue
		}
		d, err := ac.DecodeDelegation(sg.Msg)
		if err != nil {
			return nil, refused(verbRevokeKey, refusedTrustUndecodable)
		}
		ds = append(ds, d)
	}
	return ds, nil
}

// installedDelegations is every delegation row the installed trust accepts,
// decoded: buildTrust's verdict, so a row serve's trust ignores lists nothing
// here either (B1-D37).
func installedDelegations(ctx context.Context, q queryer, anchors ac.Anchors) ([]ac.Delegation, error) {
	_, accepted, err := buildTrust(ctx, q, anchors)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT bytes FROM authority.delegations ORDER BY product_root_gen, serial`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ds []ac.Delegation
	for rows.Next() {
		var msg []byte
		if err := rows.Scan(&msg); err != nil {
			return nil, err
		}
		if !accepted[string(ac.Digest(msg))] {
			continue // ignored by the trust, or inserted after the build
		}
		d, err := ac.DecodeDelegation(msg)
		if err != nil {
			return nil, err // an accepted row that does not decode
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}

// listedDigests is the digests ds lists for kind, each in the form serve
// compares for that kind (DoR:1734).
func listedDigests(kind string, ds []ac.Delegation) [][]byte {
	var out [][]byte
	for _, d := range ds {
		switch kind {
		case revokeCAK:
			for _, k := range d.CAKPublicKeys {
				out = append(out, ac.KeyDigest(k))
			}
		case revokeOperator:
			for _, k := range d.OperatorPublicKeys {
				out = append(out, ac.KeyDigest(k))
			}
		case revokeCPTLS:
			out = append(out, d.CPTLSPins...)
		}
	}
	return out
}

// listed compares digest with every candidate in constant time and without
// stopping at a match.
func listed(digest []byte, candidates [][]byte) bool {
	hit := 0
	for _, c := range candidates {
		hit |= subtle.ConstantTimeCompare(c, digest)
	}
	return hit == 1
}

// ReplicationGate is replication-gate on|off (D-200): it sets
// meta.replication_required. Off, finish skips its wait; on, finish confirms
// this very write. The app role cannot write the column (DoR:1401), so only
// this verb moves it. The same value again converges to nil (B3-D8).
func (o *Owner) ReplicationGate(ctx context.Context, on bool) error {
	return o.write(ctx, "replication-gate", func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE authority.meta SET replication_required = $1 WHERE id = 1`, on)
		if err != nil {
			return fmt.Errorf("authority store: replication-gate: update: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return errors.New("authority store: replication-gate: meta has no row")
		}
		return nil
	})
}

// EndDrain is end-drain <gen>:<serial> (D-164): it sets drain_grace_ms = 0
// on a superseded delegation, so serve refuses its CAK from the next request
// (D-171). "Superseded" is serve's own T9 predicate, supersedingInstalledAt
// over this transaction's trust: a higher serial of gen that the trust
// accepts. A higher serial the trust ignored supersedes nothing (B1-D36), so
// it cannot be used to cut short the grace of the delegation that is still
// current. It reads the installed trust alone, never the trust file: a
// delegation only the file holds is not one serve signs under or drains, so it
// neither is a delegation to end nor supersedes one. The delegation itself must
// be one the trust accepts; an unknown
// or unaccepted (gen, serial) and the current delegation are refused. Grace
// already 0 converges to nil (B3-D8).
func (o *Owner) EndDrain(ctx context.Context, gen, serial uint64) error {
	return o.write(ctx, verbEndDrain, func(ctx context.Context, tx *sql.Tx) error {
		if gen > math.MaxInt64 || serial > math.MaxInt64 { // i64 would saturate onto another row's key
			return refused(verbEndDrain, "no installed delegation has that generation and serial")
		}
		_, accepted, err := buildTrust(ctx, tx, o.anchors)
		if err != nil {
			return fmt.Errorf("authority store: end-drain: installed trust: %w", err)
		}
		var msg []byte
		err = tx.QueryRowContext(ctx, `SELECT bytes FROM authority.delegations WHERE product_root_gen = $1 AND serial = $2`,
			i64(gen), i64(serial)).Scan(&msg)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return refused(verbEndDrain, "no installed delegation has that generation and serial")
		case err != nil:
			return fmt.Errorf("authority store: end-drain: read: %w", err)
		case !accepted[string(ac.Digest(msg))]:
			return refused(verbEndDrain, "the installed trust does not accept that delegation")
		}
		sup, err := supersedingInstalledAt(ctx, tx, gen, serial, accepted)
		if err != nil {
			return fmt.Errorf("authority store: end-drain: superseding read: %w", err)
		}
		if !sup.Valid {
			return refused(verbEndDrain, "that delegation is not superseded, and only a superseded delegation drains")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE authority.delegations SET drain_grace_ms = 0
			WHERE product_root_gen = $1 AND serial = $2 AND drain_grace_ms <> 0`, i64(gen), i64(serial)); err != nil {
			return fmt.Errorf("authority store: end-drain: update: %w", err)
		}
		return nil
	})
}

// SealLegacy is seal-legacy --cp-digest (DoR:1732, :1774, B1-D30): it sets
// legacy_sealed_at and population_digest once the authority's own digest of
// the population equals cpDigest, the digest the CP computed over its
// snapshot. From the next request serve admits enroll_new (D-171) and the
// guard refuses every population write (B1-D32). The invariant it keeps is
// population.go's.
//
// Its first statement locks meta FOR UPDATE, before it reads a row (B1-D30).
// Then:
//   - already sealed under cpDigest: nil, and the re-run still ends on
//     finish, since the first run's commit may be the one a standby has not
//     confirmed (B3-D8);
//   - already sealed under another digest: ErrRefused;
//   - unsealed: the population is digested in account_id order and compared
//     with cpDigest; a mismatch is ErrRefused and seals nothing, so the CP can
//     upload the rows it missed and run the verb again.
//
// cpDigest must be 48 bytes. That is checked first and is ErrRefused, which
// also keeps a nil cpDigest from matching a sealed meta whose digest is NULL.
// Both comparisons are constant-time.
func (o *Owner) SealLegacy(ctx context.Context, cpDigest []byte) error {
	return o.write(ctx, verbSealLegacy, func(ctx context.Context, tx *sql.Tx) error {
		if len(cpDigest) != sha512.Size384 {
			return refused(verbSealLegacy, "the CP digest is 48 bytes, SHA-384")
		}
		var sealedAt sql.NullTime
		var sealed []byte
		err := tx.QueryRowContext(ctx, `SELECT legacy_sealed_at, population_digest FROM authority.meta WHERE id = 1 FOR UPDATE`).
			Scan(&sealedAt, &sealed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return errors.New("authority store: seal-legacy: meta has no row")
		case err != nil:
			return fmt.Errorf("authority store: seal-legacy: meta: %w", err)
		}
		if sealedAt.Valid {
			if subtle.ConstantTimeCompare(sealed, cpDigest) != 1 {
				return refused(verbSealLegacy, "the population is already sealed under a different digest")
			}
			return nil
		}
		got, err := populationDigestOf(ctx, tx)
		if err != nil {
			return fmt.Errorf("authority store: seal-legacy: population digest: %w", err)
		}
		if subtle.ConstantTimeCompare(got, cpDigest) != 1 {
			return refused(verbSealLegacy, "the CP digest does not match the authority's population")
		}
		res, err := tx.ExecContext(ctx, `UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1
			WHERE id = 1`, got)
		if err != nil {
			return fmt.Errorf("authority store: seal-legacy: update: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return errors.New("authority store: seal-legacy: meta has no row")
		}
		return nil
	})
}
