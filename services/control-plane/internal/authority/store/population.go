package store

import (
	"context"
	"crypto/sha512"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// The legacy population (DoR:1380, :1607, D-185) and the seal that freezes it
// (B1-D30, B1-D32).
//
// The invariant: population_digest is the digest of exactly the rows the
// population holds for ever after. It holds because every write to
// legacy_population takes meta FOR SHARE in authority.legacy_population_guard
// and keeps it until it commits, and SealLegacy takes meta FOR UPDATE as its
// first statement, before it reads a row. Holding that lock, the seal knows
// every population write that began before it has committed or rolled back,
// since FOR UPDATE waited for each, and that every later one is parked on FOR
// SHARE until the seal commits and then sees legacy_sealed_at and raises.
// Under READ COMMITTED the seal's row read takes a fresh snapshot after the
// lock, so it digests every committed row and nothing else. Three things
// break it: reading the rows before the lock, a snapshot taken at the lock
// statement rather than after it (REPEATABLE READ, hence the explicit
// isolation in Owner.write), and a population writer that skips the guard.

// sealedRaise is the population guard's refusal once meta.legacy_sealed_at is
// set. P0001 is the SQLSTATE of every plpgsql RAISE EXCEPTION that names none
// of its own, so the code alone identifies no particular refusal: ErrSealed
// needs the code and this exact message.
const sealedRaise = "authority.legacy_population: sealed"

// InsertLegacyPopulation is POST /v1/legacy-population (DoR:1607): it inserts a
// legacy-batch's rows in one transaction, as authority_app. A row whose account
// is already present is skipped (ON CONFLICT DO NOTHING), so the CP's retry of
// a batch whose reply it lost converges; the first digest written for an
// account stands, and the seal's digest comparison is what refuses a
// population that disagrees with the CP's. Once sealed, every batch is
// ErrSealed, a repeat of rows already present included: the guard is a BEFORE
// row trigger and fires before the conflict check. A body that is not a
// legacy-batch is ErrBadAuthz, before any connection is taken.
//
// Its prologue and release are operatorRun's (operator.go): 503 busy,
// not_leader, disk and replication come back as OutcomeError. A commit no
// standby confirmed leaves the rows committed locally, and the CP's retry
// converges.
func (s *Store) InsertLegacyPopulation(ctx context.Context, body []byte) error {
	return operatorRun(ctx, s, "legacy-insert", body, decodePopulation,
		func(ctx context.Context, conn *sql.Conn, p populationRows) error {
			return insertPopulation(ctx, conn, p.ids, p.digests)
		})
}

// populationRows is a decoded legacy-batch as two parallel columns.
type populationRows struct {
	ids     []string
	digests [][]byte
}

// decodePopulation is the batch's rows as two parallel columns. The codec has
// already checked each row's shape (a canonical UUID, a dot, the strict
// base64 of 48 bytes), so the split below cannot fail on a decoded batch; it
// is checked anyway, and fails the same way.
func decodePopulation(body []byte) (populationRows, error) {
	b, err := ac.DecodeLegacyBatch(body)
	if err != nil {
		return populationRows{}, fmt.Errorf("%w: the body is not a legacy-batch", ErrBadAuthz)
	}
	p := populationRows{ids: make([]string, len(b.Rows)), digests: make([][]byte, len(b.Rows))}
	for i, row := range b.Rows {
		id, enc, ok := strings.Cut(row, ".")
		d, err := base64.StdEncoding.DecodeString(enc)
		if !ok || err != nil || len(d) != sha512.Size384 {
			return populationRows{}, fmt.Errorf("%w: a legacy-batch row does not split", ErrBadAuthz)
		}
		p.ids[i], p.digests[i] = id, d
	}
	return p, nil
}

// insertPopulation is the one transaction. Every row's guard takes meta FOR
// SHARE, so the transaction holds that lock from its first row to its commit.
func insertPopulation(ctx context.Context, conn *sql.Conn, ids []string, digests [][]byte) error {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("authority store: legacy-insert: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := setLocalTimeouts(ctx, tx); err != nil {
		return fmt.Errorf("authority store: legacy-insert: timeouts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO authority.legacy_population (account_id, legacy_ek_digest)
		SELECT t.id, t.d FROM unnest($1::uuid[], $2::bytea[]) AS t(id, d)
		ON CONFLICT (account_id) DO NOTHING`, pq.Array(ids), pq.Array(digests)); err != nil {
		if isSealedRaise(err) {
			return ErrSealed
		}
		return fmt.Errorf("authority store: legacy-insert: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("authority store: legacy-insert: commit: %w", err)
	}
	return nil
}

// isSealedRaise reports whether err is the population guard's sealed refusal:
// SQLSTATE P0001 and the guard's exact message, never either alone.
func isSealedRaise(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "P0001" && pqErr.Message == sealedRaise
}

// populationDigestOf is ac.PopulationDigest over every row q can see, read in
// account_id order. SealLegacy calls it only while it holds meta FOR UPDATE.
func populationDigestOf(ctx context.Context, q queryer) ([]byte, error) {
	rows, err := q.QueryContext(ctx, `SELECT account_id::text, legacy_ek_digest FROM authority.legacy_population
		ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var recs []ac.PopulationRecord
	for rows.Next() {
		var r ac.PopulationRecord
		if err := rows.Scan(&r.AccountID, &r.LegacyEKDigest); err != nil {
			return nil, err
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ac.PopulationDigest(recs)
}
