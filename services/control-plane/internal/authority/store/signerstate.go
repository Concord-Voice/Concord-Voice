package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Signer is GET /v1/signer's database half (§2.6, B2-D4), read in one
// read-only snapshot; fill sets the node's own fields (role, disk_ready,
// archive_ok, clock_synced, submit_layouts). It takes no read slot and skips
// the disk floor, because /v1/signer is the only channel out of a node
// (DoR:1786). With no installed delegation it is 503 signer, as is a body
// that does not encode. Like every read it is bounded by the read deadline,
// whatever deadline its caller's context carries.
func (s *Store) Signer(ctx context.Context, fill func(*ac.Signer)) Outcome {
	ctx, cancel := context.WithTimeout(ctx, s.readDL)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return s.storeError()
	}
	defer func() { _ = tx.Rollback() }()
	m, err := s.signerState(ctx, tx)
	if err != nil {
		return s.signerOrStore(err)
	}
	fill(&m)
	body := m.Encode()
	if body == nil {
		return s.signerError()
	}
	return Outcome{HTTP: 200, Kind: KindOK, Body: body}
}

func (s *Store) signerState(ctx context.Context, tx *sql.Tx) (ac.Signer, error) {
	if err := setLocalTimeouts(ctx, tx); err != nil {
		return ac.Signer{}, err
	}
	now, err := s.dbNow(ctx, tx)
	if err != nil {
		return ac.Signer{}, err
	}
	var m ac.Signer
	var restoreEpoch, cancelMs, maxCommit, held int64
	var reconciled, required bool
	var sealed sql.NullTime
	var lastTick time.Time
	var syncs int
	if err := tx.QueryRowContext(ctx, `SELECT m.realm_id::text, m.restore_epoch, m.reconciled, m.legacy_sealed_at,
		m.population_digest, m.last_tick_at, m.cancel_unavailable_ms, m.replication_required,
		COALESCE(pg_sequence_last_value(pg_get_serial_sequence('authority.account_events', 'commit_id')::regclass), 0),
		(SELECT count(*) FROM authority.account_heads WHERE held), authority.replication_state()
		FROM authority.meta m WHERE m.id = 1`).Scan(&m.RealmID, &restoreEpoch, &reconciled, &sealed,
		&m.PopulationDigest, &lastTick, &cancelMs, &required, &maxCommit, &held, &syncs); err != nil {
		return ac.Signer{}, err
	}
	tr, _, err := s.trustFor(ctx, tx)
	if err != nil {
		return ac.Signer{}, err
	}
	hw := tr.HighWater()
	if hw.Serial == 0 {
		return ac.Signer{}, errNoDelegation
	}
	if m.Trust, err = installedTrust(ctx, tx); err != nil {
		return ac.Signer{}, err
	}
	m.ProductRootGen, m.DelegationSerial = hw.Gen, hw.Serial
	m.RestoreEpoch, m.Reconciled = u64(restoreEpoch), b01(reconciled)
	m.MaxCommitID, m.HeldCount, m.LegacySealed = u64(maxCommit), u64(held), b01(sealed.Valid)
	m.TickAgeS = tickAgeS(now, u64(lastTick.UnixMilli()))
	m.CancelUnavailableMs = u64(cancelMs)
	m.ExtensionCapMs, m.NCancelMs, m.VMs = ExtensionCapMs, NCancelMs, VMs
	m.RSKDigest = ac.KeyDigest(s.sig.PublicKey())
	m.ReplicationRequired, m.ReplicationReady = b01(required), b01(required && syncs >= 1) // DoR:1453
	return m, nil
}

// installedTrust re-emits every installed succession, then every installed
// delegation, each with both signatures (DoR:502, :1378). More than
// TrustItems items cannot encode, which is 503 signer, never an empty trust.
func installedTrust(ctx context.Context, q queryer) ([]byte, error) {
	var items [][]byte
	for _, src := range trustSources {
		var err error
		if items, err = appendSigned(ctx, q, src.typ, src.query, items); err != nil {
			return nil, err
		}
	}
	b := ac.TrustEnvelope{Items: items}.Encode()
	if b == nil {
		return nil, errors.Join(errSigner, errors.New("the installed trust does not encode"))
	}
	return b, nil
}

func tickAgeS(nowMs, lastMs uint64) uint64 {
	if nowMs <= lastMs {
		return 0
	}
	return (nowMs - lastMs) / 1000
}
