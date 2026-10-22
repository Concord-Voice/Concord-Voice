package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"slices"

	"github.com/google/uuid"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

const legacyPage = 8192

// canonicalID is A1's s36 rule (kind.go canonicalUUID), which POST /v1/status
// applies through DecodeStatusReq. The path reads apply it before any store
// access (red-team R-1): another spelling of a real id would take its own
// D-241 as_of fence key, and a 503 store for a caller's malformed id would set
// the D-147 leg for every account.
func canonicalID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// readSession takes a read slot and a connection, checks leadership, applies
// the hard disk floor, and returns the conn for the gate. The caller defers
// release() and conn.Close(). Leadership comes first, as step 1a precedes 2b
// on a submit: a standby answers every read with not_leader, which moves the
// CP to its next URL, even below the floor (DoR §3.3, Codex round 2). The
// slot and the connection are released by a deferred guard unless handed to
// the caller, so a panic that serve's run() recovers holds neither
// (CWE-772).
func (s *Store) readSession(ctx context.Context) (*sql.Conn, func(), Outcome, bool) {
	rel, ok := s.adm.acquireRead()
	if !ok {
		return nil, nil, errorOutcome(503, ac.ErrorBusy), false
	}
	var conn *sql.Conn
	handed := false
	defer func() {
		if handed {
			return
		}
		if conn != nil {
			_ = conn.Close()
		}
		rel()
	}()
	var err error
	if conn, err = s.db.Conn(ctx); err != nil {
		return nil, nil, s.storeError(), false
	}
	out, ok := s.leaderCheck(ctx, conn)
	if ok {
		out, ok = s.hardFloorCheck() // D-232: every request, reads included (review round 2)
	}
	if !ok {
		return nil, nil, out, false
	}
	handed = true
	return conn, rel, Outcome{}, true
}

type readCtx struct {
	tx    *sql.Tx
	now   uint64
	realm string
	acc   uint64
	sig   *sigDelegation
}

// readSnapshot opens a read-only transaction, reads db_now and meta, and
// applies /v1/status's readiness rule when ready is true (unreconciled or a
// tick older than 90 s → 503 not_ready). It needs no signer, so PendingHeads,
// which returns no signed bytes, uses it alone (review round 2).
func (s *Store) readSnapshot(ctx context.Context, conn *sql.Conn, ready bool) (*readCtx, Outcome, bool) {
	// B1-D29: one snapshot, taken by the db_now statement, so no row this read
	// signs was committed after its as_of_ms.
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, s.storeError(), false
	}
	rc := &readCtx{tx: tx}
	if rc.now, err = s.dbNow(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, s.storeError(), false
	}
	var reconciled bool
	var tick sql.NullTime
	var acc int64
	if err := tx.QueryRowContext(ctx, `SELECT realm_id::text, reconciled, last_tick_at, cancel_unavailable_ms
		FROM authority.meta WHERE id = 1`).Scan(&rc.realm, &reconciled, &tick, &acc); err != nil {
		_ = tx.Rollback()
		return nil, s.storeError(), false
	}
	rc.acc = u64(acc)
	if ready && (!reconciled || msOfNull(tick)+TickGapThresholdMs < rc.now) {
		_ = tx.Rollback()
		return nil, errorOutcome(503, ac.ErrorNotReady), false
	}
	return rc, Outcome{}, true
}

// readTx is readSnapshot plus the signing delegation, refused unless it lists
// this node's RSK (T10, as step 9 checks it for writes), and D-241's fence:
// every account the read signs a status for reserves rc.now. A failed
// reservation takes a new snapshot rather than re-reading db_now inside this
// one, which would sign a snapshot older than its as_of_ms (B1-D29, B1-D26).
func (s *Store) readTx(ctx context.Context, conn *sql.Conn, ready bool, accounts []string) (*readCtx, Outcome, bool) {
	for try := 0; ; try++ {
		rc, out, ok := s.readSnapshot(ctx, conn, ready)
		if !ok {
			return nil, out, false
		}
		if out, ok := s.readSigner(ctx, rc, accounts); !ok {
			return nil, out, false
		}
		if s.reserveAsOf(accounts, rc.now) {
			return rc, Outcome{}, true
		}
		_ = rc.tx.Rollback()
		if try == asOfRetries {
			s.nonClient503.Store(true) // B1-D31: the database clock is behind this leader's last as_of_ms
			return nil, errorOutcome(503, ac.ErrorClock), false
		}
		s.pause()
	}
}

// readSigner loads rc's signing delegation and checks that it may sign this
// read: it lists this node's RSK (T10, as step 9 checks it for writes) and,
// when the read signs statuses, its scope holds `status` (T6, DoR:347, :720;
// review round 3). A refusal rolls rc.tx back; a database error reading the
// trust is 503 store.
func (s *Store) readSigner(ctx context.Context, rc *readCtx, accounts []string) (Outcome, bool) {
	tr, accepted, err := s.trustFor(ctx, rc.tx)
	if err == nil {
		rc.sig, err = s.signingDelegation(ctx, rc.tx, tr.CurrentGen(), rc.now, accepted)
	}
	if err == nil && !s.rskListed(rc.sig) {
		err = errSigner
	}
	if err != nil {
		_ = rc.tx.Rollback()
		return s.signerOrStore(err), false
	}
	if len(accounts) > 0 && !slices.Contains(rc.sig.D.Scope, ac.ScopeStatus) {
		_ = rc.tx.Rollback()
		return s.signerError(), false
	}
	return Outcome{}, true
}

// loadHeadRO reads a head without locking it; a missing head is genesis
// (absent). A seq-0 head without a stored pin takes the population pin if any,
// as lockHead does (D-149): a genesis head that a recorded refusal committed
// stores none.
func loadHeadRO(ctx context.Context, rc *readCtx, account string) (*headRow, error) {
	h, err := scanHead(rc.tx.QueryRowContext(ctx, selectHead, account))
	if errors.Is(err, sql.ErrNoRows) {
		h, err = &headRow{AccountID: account, State: ac.StateAbsent, RealmID: rc.realm}, nil
	}
	if err != nil || h.Seq != 0 || len(h.Pin) != 0 {
		return h, err
	}
	perr := rc.tx.QueryRowContext(ctx, `SELECT legacy_ek_digest FROM authority.legacy_population WHERE account_id = $1`, account).Scan(&h.Pin)
	if perr != nil && !errors.Is(perr, sql.ErrNoRows) {
		return nil, perr
	}
	return h, nil
}

func (s *Store) statusFor(ctx context.Context, rc *readCtx, account string) ([]byte, *headRow, error) {
	h, err := loadHeadRO(ctx, rc, account)
	if err != nil {
		return nil, nil, err
	}
	n, err := normalize(h, rc.now, rc.acc, "")
	if err != nil {
		return nil, nil, err
	}
	b, err := s.signedOf("status", statusOf(rc.realm, h, n, rc.now, rc.sig).Encode())
	return b, h, err
}

// Head is GET /v1/accounts/{id}/head (§3.3): the signed status, the latest
// bundle and the device-status; 503 not_ready while unreconciled or on a stale
// tick (B1-D24).
func (s *Store) Head(ctx context.Context, accountID string) Outcome {
	if !canonicalID(accountID) {
		return errorOutcome(400, ac.ErrorMalformed)
	}
	ctx, cancel := context.WithTimeout(ctx, s.readDL)
	defer cancel()
	conn, rel, out, ok := s.readSession(ctx)
	if !ok {
		return out
	}
	defer rel()
	defer func() { _ = conn.Close() }()
	rc, out, ok := s.readTx(ctx, conn, true, []string{accountID}) // B1-D24
	if !ok {
		return out
	}
	defer func() { _ = rc.tx.Rollback() }()
	status, h, err := s.statusFor(ctx, rc, accountID)
	if err != nil {
		return s.signerOrStore(err)
	}
	if h.DeviceSetDigest == nil {
		if err := h.setDeviceSet(ac.EmptyDeviceSet(rc.realm, accountID)); err != nil {
			return s.storeError()
		}
	}
	dstatus, err := s.signedOf("device-status", deviceStatusOf(rc.realm, h, rc.now, rc.sig).Encode())
	if err != nil {
		return s.signerOrStore(err)
	}
	var latest []byte
	if h.Seq > 0 {
		if err := rc.tx.QueryRowContext(ctx, `SELECT bundle FROM authority.account_events WHERE account_id = $1 AND seq = $2`,
			accountID, i64(h.Seq)).Scan(&latest); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return s.storeError()
		}
	}
	body := ac.Head{Status: status, LatestBundle: latest, DeviceStatus: dstatus}.Encode()
	_ = rc.tx.Commit()
	return s.release(ctx, conn, Outcome{HTTP: 200, Kind: KindOK, Body: body})
}

// Statuses is POST /v1/status (§3.2.5): a signed status per requested
// account, refused while unreconciled or on a stale tick.
func (s *Store) Statuses(ctx context.Context, body []byte) Outcome {
	ctx, cancel := context.WithTimeout(ctx, s.readDL)
	defer cancel()
	if len(body) > BodyCapTx {
		return errorOutcome(413, ac.ErrorTooLarge)
	}
	req, err := ac.DecodeStatusReq(body)
	if err != nil {
		return errorOutcome(400, ac.ErrorMalformed)
	}
	conn, rel, out, ok := s.readSession(ctx)
	if !ok {
		return out
	}
	defer rel()
	defer func() { _ = conn.Close() }()
	rc, out, ok := s.readTx(ctx, conn, true, req.AccountIDs)
	if !ok {
		return out
	}
	defer func() { _ = rc.tx.Rollback() }()
	items := make([][]byte, 0, len(req.AccountIDs))
	for _, id := range req.AccountIDs {
		b, _, err := s.statusFor(ctx, rc, id)
		if err != nil {
			return s.signerOrStore(err)
		}
		items = append(items, b)
	}
	_ = rc.tx.Commit()
	return s.release(ctx, conn, Outcome{HTTP: 200, Kind: KindOK, Body: ac.Statuses{Items: items}.Encode()})
}

// History is GET /v1/accounts/{id}/history (§3.3): published bundles after
// afterSeq, at most ac.HistoryPage a page, every page through the release gate (D-225).
func (s *Store) History(ctx context.Context, accountID string, afterSeq uint64) Outcome {
	return s.pageRead(ctx, `SELECT bundle FROM authority.account_events WHERE account_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`,
		accountID, afterSeq, ac.HistoryPage, func(b [][]byte) []byte { return ac.History{AccountID: accountID, Bundles: b}.Encode() })
}

// Devices is GET /v1/accounts/{id}/devices (§3.3): private device bundles
// after afterDseq, at most ac.DevicePage a page.
func (s *Store) Devices(ctx context.Context, accountID string, afterDseq uint64) Outcome {
	return s.pageRead(ctx, `SELECT bundle FROM authority.device_events WHERE account_id = $1 AND dseq > $2 ORDER BY dseq LIMIT $3`,
		accountID, afterDseq, ac.DevicePage, func(b [][]byte) []byte { return ac.DeviceHistory{AccountID: accountID, Bundles: b}.Encode() })
}

// pageRead serves one gated page (D-225: every page passes the release gate).
func (s *Store) pageRead(ctx context.Context, q, account string, after uint64, limit int, enc func([][]byte) []byte) Outcome {
	if !canonicalID(account) {
		return errorOutcome(400, ac.ErrorMalformed)
	}
	ctx, cancel := context.WithTimeout(ctx, s.readDL)
	defer cancel()
	conn, rel, out, ok := s.readSession(ctx)
	if !ok {
		return out
	}
	defer rel()
	defer func() { _ = conn.Close() }()
	if after > ac.MaxSafeInt {
		return errorOutcome(400, ac.ErrorMalformed)
	}
	rows, err := conn.QueryContext(ctx, q, account, int64(after), limit)
	if err != nil {
		return s.storeError()
	}
	defer func() { _ = rows.Close() }()
	bundles := [][]byte{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return s.storeError()
		}
		bundles = append(bundles, b)
	}
	if err := rows.Err(); err != nil {
		return s.storeError()
	}
	body := enc(bundles)
	if body == nil {
		return s.storeError()
	}
	return s.release(ctx, conn, Outcome{HTTP: 200, Kind: KindOK, Body: body})
}

// PendingHeads lists effectively pending or disputed heads in ascending
// account_id after `after` (D-179). Class I: no signed bytes, no gate.
func (s *Store) PendingHeads(ctx context.Context, after string) Outcome {
	ctx, cancel := context.WithTimeout(ctx, s.readDL)
	defer cancel()
	conn, rel, out, ok := s.readSession(ctx)
	if !ok {
		return out
	}
	defer rel()
	defer func() { _ = conn.Close() }()
	rc, out, ok := s.readSnapshot(ctx, conn, true) // no signed bytes, so no signer (review round 2)
	if !ok {
		return out
	}
	defer func() { _ = rc.tx.Rollback() }()
	rows, err := rc.tx.QueryContext(ctx, selectPendingHeads, after, ac.StatusBatch)
	if err != nil {
		return s.storeError()
	}
	defer func() { _ = rows.Close() }()
	ids, seen, last := []string{}, 0, ""
	for rows.Next() {
		h, err := scanHead(rows)
		if err != nil {
			return s.storeError()
		}
		seen, last = seen+1, h.AccountID
		n, err := normalize(h, rc.now, rc.acc, "")
		if err != nil { // fail the page as scanHead does: a head must never drop out of the protective set unseen
			return s.storeError()
		}
		if !n.Activated {
			ids = append(ids, h.AccountID)
		}
	}
	if err := rows.Err(); err != nil {
		return s.storeError()
	}
	next := ""
	if seen == ac.StatusBatch {
		next = last
	}
	return Outcome{HTTP: 200, Kind: KindOK, Body: ac.PendingHeads{AccountIDs: ids, Next: next}.Encode()}
}

// LegacyPopulation is the monitor's population source (D-185): rows
// "uuid.base64(digest)" ascending after `after`, ≤ 8192; 204 when none.
func (s *Store) LegacyPopulation(ctx context.Context, after string) Outcome {
	ctx, cancel := context.WithTimeout(ctx, s.readDL)
	defer cancel()
	conn, rel, out, ok := s.readSession(ctx)
	if !ok {
		return out
	}
	defer rel()
	defer func() { _ = conn.Close() }()
	rows, err := conn.QueryContext(ctx, `SELECT account_id::text, legacy_ek_digest FROM authority.legacy_population
		WHERE account_id::text > $1 ORDER BY account_id LIMIT $2`, after, legacyPage)
	if err != nil {
		return s.storeError()
	}
	defer func() { _ = rows.Close() }()
	var out2 []string
	for rows.Next() {
		var id string
		var d []byte
		if err := rows.Scan(&id, &d); err != nil {
			return s.storeError()
		}
		out2 = append(out2, id+"."+base64.StdEncoding.EncodeToString(d))
	}
	if err := rows.Err(); err != nil {
		return s.storeError()
	}
	if len(out2) == 0 {
		return Outcome{HTTP: 204, Kind: KindOK}
	}
	return Outcome{HTTP: 200, Kind: KindOK, Body: ac.LegacyBatch{Rows: out2}.Encode()}
}
