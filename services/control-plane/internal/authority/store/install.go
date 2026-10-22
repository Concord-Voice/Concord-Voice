package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// TrustView is the merged trust's high-water delegation, which serve checks
// its own keys against (D-223) and pins its listener to (LD-19), and the RSK
// lists of every other delegation in the trust (D-140, per key): each one a
// retired generation holds at a single (gen, serial), and a file delegation
// that ended uninstalled. Those lists are unexported: only checkRSKReuse,
// D-223's reuse bullet, reads them, inside LoadTrust. Pending means a standby
// accepted file trust that a leader would install (B2-D33).
type TrustView struct {
	HW        ac.Delegation
	otherRSKs [][]byte
	Pending   bool
	// Mark is the installed trust the view was built from, read in
	// LoadTrust's own snapshot, counting what this call installed (Sec-M2).
	Mark TrustMark
	// Ignored counts the trust file's items the merge ignored because they
	// did not verify: forged, or out of the anchors' bounds (T3, I9). serve
	// names the count when D-223's key check refuses.
	Ignored int
}

// TrustMark counts the installed successions and delegations. Both tables
// are insert-only (their guards refuse UPDATE, DELETE and TRUNCATE), so an
// equal mark means the installed trust is the one a view was built from,
// and a moved one means a leader installed more since.
type TrustMark struct{ Successions, Delegations int64 }

// InstalledTrustMark reads the mark now, in one statement. serve compares
// it with its start-time view's when a standby is promoted (Sec-M2).
func (s *Store) InstalledTrustMark(ctx context.Context) (TrustMark, error) {
	var m TrustMark
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM authority.successions),
		(SELECT count(*) FROM authority.delegations)`).Scan(&m.Successions, &m.Delegations)
	return m, err
}

// LoadTrust's refusals (B2-D10, B2-D20). serve maps ErrNoMeta to
// reason=config and the other three, and ErrRealmMismatch, to reason=trust.
var (
	ErrNoMeta           = errors.New("authority store: meta is missing; run migrate --realm")
	ErrTrustNotYetValid = errors.New("authority store: a delegation's window has not begun (D-176)")
	ErrTrustUnusable    = errors.New("authority store: the trust does not verify")
	// ErrRSKReused is D-223's reuse bullet (DoR:252). It is realm-wide, so the
	// store refuses it before the leader commits; serve checks this node's
	// own RSK and TLS key against HW.
	ErrRSKReused = errors.New("authority store: an RSK of the high-water delegation is listed in another delegation (D-223)")
)

// errInstalledUnverified is ErrTrustUnusable's cause when an installed row
// does not verify under the anchors (addInstalled).
var errInstalledUnverified = errors.New("the anchors do not verify an installed row")

// trustSources are the installed trust rows, successions first, in the
// envelope's order (DoR:502). LoadTrust and /v1/signer read the same rows.
var trustSources = []struct{ typ, query string }{
	{"succession", `SELECT bytes, sig_a, sig_b FROM authority.successions ORDER BY prev_root_gen`},
	{"delegation", `SELECT bytes, sig_a, sig_b FROM authority.delegations ORDER BY product_root_gen, serial`},
}

// appendSigned appends each row of query as a `signed` envelope of typ.
func appendSigned(ctx context.Context, q queryer, typ, query string, items [][]byte) ([][]byte, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		sg := ac.Signed{Type: typ}
		if err := rows.Scan(&sg.Msg, &sg.SigA, &sg.SigB); err != nil {
			return nil, err
		}
		items = append(items, sg.Encode())
	}
	return items, rows.Err()
}

// LoadTrust merges the anchors, the installed rows and the trust file, and
// returns the merged high-water view (B2-D20). Every installed row must verify
// under the anchors: one that does not means these anchors did not install it,
// so start refuses. A file delegation not yet installed must cover db_now: one
// whose window has not begun refuses start on every node (D-176), and one
// whose window has ended is left out, because only a covering window installs
// (DoR:1058). On every node the order is then: the merged view, D-223's reuse
// bullet, and the differs check; only after all three does the leader insert
// every accepted succession and delegation not yet installed, AddSigned having
// re-verified both signatures (DoR:1378), and commit, in one transaction. So
// every check runs before anything is written, and a violating install, which
// would be permanent since delegations are never deleted, never commits. A
// standby refuses what its leader refuses, and installs nothing.
//
// The transaction is REPEATABLE READ, not READ COMMITTED, so every read sees
// one snapshot. Under READ COMMITTED each statement takes its own, and a
// standby restarting while the leader commits a succession and a delegation
// of its new generation could read the delegation without the succession and
// refuse start.
func (s *Store) LoadTrust(ctx context.Context, file []byte, install bool) (TrustView, error) {
	env, err := ac.CheckTrustEnvelope(file)
	if err != nil {
		return TrustView{}, errors.Join(ErrTrustUnusable, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: !install})
	if err != nil {
		return TrustView{}, err
	}
	defer func() { _ = tx.Rollback() }()
	m, v, err := s.mergeTrust(ctx, tx, env.Items)
	if err != nil {
		return TrustView{}, err
	}
	if !install {
		v.Pending = len(m.toInsert) > 0
		v.Mark = m.mark(false)
		return v, nil
	}
	if err := m.insertCollected(ctx, tx); err != nil {
		return TrustView{}, err
	}
	if err := tx.Commit(); err != nil {
		return TrustView{}, err
	}
	v.Mark = m.mark(true)
	return v, nil
}

// mark is the TrustMark of the snapshot mergeTrust read, plus the collected
// items when the caller inserted them.
func (m *merge) mark(inserted bool) TrustMark {
	t := TrustMark{Successions: m.count["succession"], Delegations: m.count["delegation"]}
	if !inserted {
		return t
	}
	for _, sg := range m.toInsert {
		if sg.Type == "succession" {
			t.Successions++
		} else {
			t.Delegations++
		}
	}
	return t
}

// mergeTrust is LoadTrust's reads and checks, before anything is written: the
// realm (B2-D10), the merge of the installed rows and the file, then the
// merged view, D-223's reuse bullet and the differs check, in that order. The
// file items to install are collected in m.toInsert.
func (s *Store) mergeTrust(ctx context.Context, tx *sql.Tx, items [][]byte) (*merge, TrustView, error) {
	if err := setLocalTimeouts(ctx, tx); err != nil {
		return nil, TrustView{}, err
	}
	if err := s.checkRealm(ctx, tx); err != nil {
		return nil, TrustView{}, err
	}
	now, err := s.dbNow(ctx, tx)
	if err != nil {
		return nil, TrustView{}, err
	}
	m, err := s.newMerge(ctx, tx)
	if err != nil {
		return nil, TrustView{}, err
	}
	for _, it := range items {
		if err := m.offer(it, now); err != nil {
			return nil, TrustView{}, err
		}
	}
	v, err := m.view()
	if err != nil {
		return nil, TrustView{}, err
	}
	v.Ignored = m.ignored
	if err := checkRSKReuse(v); err != nil {
		return nil, TrustView{}, err
	}
	if err := m.differs(); err != nil {
		return nil, TrustView{}, err
	}
	return m, v, nil
}

// checkRSKReuse is D-223's reuse bullet (DoR:252): no RSK of HW is listed in
// any other delegation in the merged trust.
func checkRSKReuse(v TrustView) error {
	for _, k := range v.HW.RSKPublicKeys {
		if slices.ContainsFunc(v.otherRSKs, func(o []byte) bool { return bytes.Equal(o, k) }) {
			return ErrRSKReused
		}
	}
	return nil
}

// checkRealm is B2-D10: meta exists and names the anchors' realm (DoR:1757).
func (s *Store) checkRealm(ctx context.Context, q queryer) error {
	var realm string
	err := q.QueryRowContext(ctx, `SELECT realm_id::text FROM authority.meta WHERE id = 1`).Scan(&realm)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoMeta
	}
	if err != nil {
		return err
	}
	if realm != s.anchors.RealmID {
		return fmt.Errorf("%w: the anchors name another", ErrRealmMismatch)
	}
	return nil
}

// merge is A2's Trust over the anchors and every admitted row, with the
// delegations it accepted, by (gen, serial). all is the same Trust plus every
// verified file delegation outside its window: tr alone sets HW, and all is
// the whole trust a client holds, so T4 is judged over it (B2-D20).
//
// dels keeps every delegation accepted at a key. T4 halts on two at one
// (gen, serial) only in the current generation, so a retired one can hold
// several, and each one's RSKs count for D-223's reuse bullet.
type merge struct {
	tr           *ac.Trust
	all          *ac.Trust
	dels         map[[2]uint64][]ac.Delegation
	installed    map[string]bool      // msg digests already in the database
	installedDel map[[2]uint64][]byte // each installed delegation's msg digest, by (gen, serial)
	expired      [][]byte             // RSKs of file delegations left out uninstalled (D-223)
	toInsert     []ac.Signed          // accepted file items not yet installed, in file order
	count        map[string]int64     // installed rows read, by trustSources type
	ignored      int                  // file items that did not verify
}

func (s *Store) newMerge(ctx context.Context, q queryer) (*merge, error) {
	tr, err := ac.NewTrust(s.anchors)
	if err != nil {
		return nil, errors.Join(ErrTrustUnusable, err)
	}
	all, err := ac.NewTrust(s.anchors)
	if err != nil {
		return nil, errors.Join(ErrTrustUnusable, err)
	}
	m := &merge{tr: tr, all: all, dels: map[[2]uint64][]ac.Delegation{}, installed: map[string]bool{},
		installedDel: map[[2]uint64][]byte{}, count: map[string]int64{}}
	for _, src := range trustSources {
		if err := m.addInstalled(ctx, q, src.typ, src.query); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// addInstalled admits every installed row of query. Each one must be
// accepted: LoadTrust inserts only what AddSigned accepted, and retention
// depends only on the row and the keys of its own and earlier generations
// (T2, T3), so the same anchors accept it again. A row they ignore means the
// anchors changed, or the row did not come from LoadTrust. Skipping it would
// let substituted anchors and a trust file of their choosing install over
// the realm's trust, so it is unusable, never ignored.
func (m *merge) addInstalled(ctx context.Context, q queryer, typ, query string) error {
	items, err := appendSigned(ctx, q, typ, query, nil)
	if err != nil {
		return err
	}
	m.count[typ] += int64(len(items))
	for _, it := range items {
		sg, err := ac.DecodeSigned(it)
		if err != nil {
			return errors.Join(ErrTrustUnusable, err)
		}
		if err := m.noteInstalled(sg); err != nil {
			return err
		}
		accepted, err := m.add(sg)
		if err != nil {
			return err
		}
		if !accepted {
			return fmt.Errorf("%w: %w: %s", ErrTrustUnusable, errInstalledUnverified, typ)
		}
	}
	return nil
}

// noteInstalled records an installed row: offer skips its msg, and differs
// refuses another delegation at its (gen, serial).
func (m *merge) noteInstalled(sg ac.Signed) error {
	dg := ac.Digest(sg.Msg)
	m.installed[string(dg)] = true
	if sg.Type != "delegation" {
		return nil
	}
	d, err := ac.DecodeDelegation(sg.Msg)
	if err != nil {
		return errors.Join(ErrTrustUnusable, err)
	}
	m.installedDel[[2]uint64{d.ProductRootGen, d.Serial}] = dg
	return nil
}

// add admits sg and reports whether the trust accepted it. Ignored is not an
// error here: a trust file carries every object ever issued (T2, T3), and
// addInstalled refuses an ignored row itself. all admits what tr admits,
// since nothing in T2–T3 depends on another delegation; it may halt where tr
// does not, against a delegation only all holds.
func (m *merge) add(sg ac.Signed) (bool, error) {
	b := sg.Encode()
	err := m.tr.AddSigned(b)
	if errors.Is(err, ac.ErrIgnored) {
		return false, nil
	}
	if err != nil { // EN5 refuses it, or the retained set halts (T4)
		return false, errors.Join(ErrTrustUnusable, err)
	}
	if err := m.all.AddSigned(b); err != nil {
		return false, errors.Join(ErrTrustUnusable, err)
	}
	if sg.Type != "delegation" {
		return true, nil
	}
	d, err := ac.DecodeDelegation(sg.Msg)
	if err != nil {
		return false, errors.Join(ErrTrustUnusable, err)
	}
	k := [2]uint64{d.ProductRootGen, d.Serial}
	m.dels[k] = append(m.dels[k], d)
	return true, nil
}

// outside is T2–T4 for a file delegation outside its window, which tr must
// not admit, so it never raises HW. all judges it. Ignored (forged, or out of
// bounds) reports false, as T3 ignores it (I9). A halt refuses start: it
// contradicts a retained delegation, and every client holding the trust
// halts on it too. The envelope puts every succession before any delegation
// (§2.6), so all already holds the generation it needs.
func (m *merge) outside(sg ac.Signed) (bool, error) {
	err := m.all.AddSigned(sg.Encode())
	if errors.Is(err, ac.ErrIgnored) {
		return false, nil
	}
	if err != nil {
		return false, errors.Join(ErrTrustUnusable, err)
	}
	return true, nil
}

// offer is one trust-file item: skip it if installed, apply D-176 to a new
// delegation, admit it, and collect it for the leader to insert once every
// check has passed. offer writes nothing.
func (m *merge) offer(item []byte, nowMs uint64) error {
	sg, err := ac.DecodeSigned(item)
	if err != nil {
		return errors.Join(ErrTrustUnusable, err)
	}
	if m.installed[string(ac.Digest(sg.Msg))] {
		return nil
	}
	if sg.Type == "delegation" {
		d, err := ac.DecodeDelegation(sg.Msg)
		if err != nil {
			return errors.Join(ErrTrustUnusable, err)
		}
		if d.NotBeforeMs > nowMs || nowMs >= d.NotAfterMs {
			return m.leaveOut(sg, d, nowMs)
		}
	}
	accepted, err := m.add(sg)
	if err != nil {
		return err
	}
	if !accepted {
		m.ignored++
		return nil
	}
	m.toInsert = append(m.toInsert, sg)
	return nil
}

// leaveOut is D-176 for a file delegation outside its window. Both rules
// apply only to a delegation that verifies, and neither once the whole trust
// halts.
func (m *merge) leaveOut(sg ac.Signed, d ac.Delegation, nowMs uint64) error {
	if ok, err := m.outside(sg); !ok {
		if err == nil {
			m.ignored++
		}
		return err
	}
	if d.NotBeforeMs > nowMs {
		return fmt.Errorf("%w: gen %d serial %d", ErrTrustNotYetValid, d.ProductRootGen, d.Serial)
	}
	// It ended before it was installed, so it never installs (D-176). It is
	// still in the trust, so its RSKs count for D-223's reuse bullet (DoR:252).
	m.expired = append(m.expired, d.RSKPublicKeys...)
	return nil
}

// insertCollected is the leader's write, the step after mergeTrust's view,
// reuse and differs checks and before Commit: every collected item, in file
// order, in LoadTrust's transaction. It checks nothing.
func (m *merge) insertCollected(ctx context.Context, tx *sql.Tx) error {
	for _, sg := range m.toInsert {
		if err := insertTrustRow(ctx, tx, sg); err != nil {
			return err
		}
	}
	return nil
}

// keyDigest is the msg digest a delegation key holds, and whether an installed
// row put it there.
type keyDigest struct {
	digest    []byte
	installed bool
}

// differs is the check after the merged view and D-223's reuse bullet, and
// before the leader inserts; it runs on every node, so a standby refuses what
// its leader would. It is one pass over the collected items, with a running
// map from (gen, serial) to msg digest seeded from the installed delegations:
// each collected delegation is checked against the map, then added to it. The
// table holds one row per key, and only a retired generation can hold two
// delegations at one key (T4 halts in the current one), so two there are a
// ceremony error the realm's clients tolerate and the leader cannot install.
// A file cannot carry two at one key (§2.6's strict order); the running map
// refuses them anyway. A succession cannot collide: two for one prev_root_gen
// halt T4 in add, whatever generation is current, so a fork is never collected.
func (m *merge) differs() error {
	at := make(map[[2]uint64]keyDigest, len(m.installedDel)+len(m.toInsert))
	for k, dg := range m.installedDel {
		at[k] = keyDigest{digest: dg, installed: true}
	}
	for _, sg := range m.toInsert {
		if sg.Type != "delegation" {
			continue
		}
		d, err := ac.DecodeDelegation(sg.Msg)
		if err != nil {
			return errors.Join(ErrTrustUnusable, err)
		}
		k, dg := [2]uint64{d.ProductRootGen, d.Serial}, ac.Digest(sg.Msg)
		if err := checkKey(at, k, dg); err != nil {
			return err
		}
		at[k] = keyDigest{digest: dg}
	}
	return nil
}

// checkKey refuses dg at k when the running map holds another digest there.
func checkKey(at map[[2]uint64]keyDigest, k [2]uint64, dg []byte) error {
	prev, ok := at[k]
	switch {
	case !ok || bytes.Equal(prev.digest, dg):
		return nil
	case prev.installed:
		return fmt.Errorf("%w: a delegation at installed (%d, %d) differs", ErrTrustUnusable, k[0], k[1])
	default:
		return fmt.Errorf("%w: two file delegations at (%d, %d) differ", ErrTrustUnusable, k[0], k[1])
	}
}

func insertTrustRow(ctx context.Context, tx *sql.Tx, sg ac.Signed) error {
	if sg.Type == "succession" {
		x, err := ac.DecodeSuccession(sg.Msg)
		if err != nil {
			return errors.Join(ErrTrustUnusable, err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO authority.successions (prev_root_gen, bytes, sig_a, sig_b)
			VALUES ($1, $2, $3, $4)`, i64(x.PrevRootGen), sg.Msg, sg.SigA, sg.SigB)
		return err
	}
	d, err := ac.DecodeDelegation(sg.Msg)
	if err != nil {
		return errors.Join(ErrTrustUnusable, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO authority.delegations (product_root_gen, serial, bytes, sig_a, sig_b, digest)
		VALUES ($1, $2, $3, $4, $5, $6)`, i64(d.ProductRootGen), i64(d.Serial), sg.Msg, sg.SigA, sg.SigB, ac.Digest(sg.Msg))
	return err
}

// view is the merged high-water delegation and every other accepted RSK. A
// halted trust, including one with no valid delegation for its current
// generation, is unusable. HW is in the current generation, where T4 allows
// one delegation per key, so a key holding any other number is unusable too.
func (m *merge) view() (TrustView, error) {
	if h := m.tr.Halted(); h != nil {
		return TrustView{}, errors.Join(ErrTrustUnusable, h)
	}
	hw := m.tr.HighWater()
	key := [2]uint64{hw.Gen, hw.Serial}
	at := m.dels[key]
	if len(at) != 1 {
		return TrustView{}, fmt.Errorf("%w: %d delegations at the high-water mark", ErrTrustUnusable, len(at))
	}
	v := TrustView{HW: at[0], otherRSKs: slices.Clone(m.expired)}
	for _, k := range slices.SortedFunc(maps.Keys(m.dels), func(a, b [2]uint64) int { return slices.Compare(a[:], b[:]) }) {
		if k == key {
			continue
		}
		for _, o := range m.dels[k] {
			v.otherRSKs = append(v.otherRSKs, o.RSKPublicKeys...)
		}
	}
	return v, nil
}
