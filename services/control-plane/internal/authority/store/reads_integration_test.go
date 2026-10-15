package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// freshTick stamps last_tick_at at the pinned clock, as a running tick would.
// advance moves only db_now, so after a jump past TickGapThresholdMs every
// ready-gated read is 503 not_ready until this runs (PR review).
func (e *env) freshTick(t *testing.T) {
	t.Helper()
	mustExec(t, e.f.App, `UPDATE authority.meta SET last_tick_at = to_timestamp($1 / 1000.0)`, e.now.Load())
}

func readHead(t *testing.T, e *env, acct string) (ac.Status, ac.DeviceStatus, ac.Head) {
	t.Helper()
	e.freshTick(t) // callers advance the clock past activation; the read must not fail on a stale tick
	out := e.s.Head(context.Background(), acct)
	if out.Kind != KindOK {
		t.Fatalf("Head = %+v", out)
	}
	hd, err := ac.DecodeHead(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := ac.DecodeSigned(hd.Status)
	st, err := ac.DecodeStatus(ss.Msg)
	if err != nil || !ac.VerifyP384(e.r.RSK.PublicKey(), ss.Msg, ss.SigA) || len(ss.SigB) != 0 {
		t.Fatalf("status not RSK-signed with an empty sig_b: %v", err)
	}
	ds, _ := ac.DecodeSigned(hd.DeviceStatus)
	dst, err := ac.DecodeDeviceStatus(ds.Msg)
	if err != nil {
		t.Fatal(err)
	}
	return st, dst, hd
}

func TestHeadStatusIsTheDerivedEffectiveHead(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	st, dst, _ := readHead(t, e, a.ID)
	if st.State != ac.StateActive || st.Seq != 1 || st.AsOfMs != e.now.Load() || st.ValidUntilMs != st.AsOfMs+VMs ||
		st.ActivationNotBeforeMs != 0 || st.Held != 0 {
		t.Fatalf("status of an active head: %+v", st)
	}
	if dst.AsOfMs != st.AsOfMs || dst.ValidUntilMs != st.ValidUntilMs || dst.DelegationSerial != st.DelegationSerial ||
		dst.ChainSeq != st.Seq || !bytes.Equal(dst.ChainHeadDigest, st.HeadEntryDigest) {
		t.Fatalf("device-status does not match its co-signed status (DR7): %+v vs %+v", dst, st)
	}
	asserted := e.now.Load()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	st, _, _ = readHead(t, e, a.ID)
	want := asserted + e.r.Current.D.DeltaMs + e.r.Current.D.GMs
	if st.State != ac.StatePending || st.ActivationNotBeforeMs != want {
		t.Fatalf("pending status anb = %d, want visible %d (B1-D23)", st.ActivationNotBeforeMs, want)
	}
	e.pinHeld(a.ID)
	if st, _, _ = readHead(t, e, a.ID); st.Held != 1 || st.ActivationNotBeforeMs != ac.ANBNever {
		t.Fatalf("held status: %+v, want held = 1 and anb = ANB_NEVER", st)
	}
	e.advance(e.r.Current.D.DeltaMs + e.r.Current.D.GMs)
	if st, _, _ = readHead(t, e, a.ID); st.State != ac.StatePending {
		t.Fatal("a held pending head read as activated")
	}
}

func TestStatusRefusedWhileUnreconciledOrOnAStaleTick(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	req := ac.StatusReq{AccountIDs: []string{a.ID}}.Encode()
	if out := e.s.Statuses(context.Background(), req); out.Kind != KindOK {
		t.Fatalf("a fresh tick, reconciled: %+v", out)
	}
	e.freshTick(t) // SeedMeta stamped last_tick_at with the real clock, after the realm pinned db_now
	e.advance(TickGapThresholdMs + 1)
	if out := e.s.Statuses(context.Background(), req); out.Code != string(ac.ErrorNotReady) {
		t.Fatalf("a tick older than 90 s = %+v, want 503 not_ready", out)
	}
	e.freshTick(t)
	mustExec(t, e.f.Owner, `SELECT authority.enter_unreconciled()`)
	if out := e.s.Head(context.Background(), a.ID); out.Code != string(ac.ErrorNotReady) {
		t.Fatalf("unreconciled head read = %+v, want 503 not_ready", out)
	}
}

// Review Focus 5.
func TestHistoryPagingBoundaries(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	for seq := 2; seq <= 201; seq++ { // synthetic events: History pages bytes, it does not verify them
		mustExec(t, e.f.Super, `INSERT INTO authority.account_events (account_id, seq, restore_epoch, request_id, intent_digest,
			op, bundle, cp_evidence, entry_digest, delegation_gen, delegation_serial, issued_at_ms, asserted_at_ms)
			VALUES ($1, $2, 0, gen_random_uuid(), $3, 'suspend', $4, '\x00', $5, 0, 1, 1, 1)`,
			a.ID, seq, d48(byte(seq)), []byte{byte(seq)}, ac.Digest([]byte{byte(seq), 1}))
	}
	page := func(after uint64) int {
		out := e.s.History(context.Background(), a.ID, after)
		if out.Kind != KindOK {
			t.Fatalf("History(%d) = %+v", after, out)
		}
		h, err := ac.DecodeHistory(out.Body)
		if err != nil {
			t.Fatal(err)
		}
		return len(h.Bundles)
	}
	if n := page(0); n != ac.HistoryPage {
		t.Fatalf("first page %d, want HISTORY_PAGE", n)
	}
	if n := page(200); n != 1 {
		t.Fatalf("after 200: %d, want 1", n)
	}
	if n := page(201); n != 0 {
		t.Fatalf("after the head: %d, want an empty page", n)
	}
	if n := page(10_000); n != 0 {
		t.Fatalf("after beyond the head: %d, want an empty page, not an error", n)
	}
}

// §3.2.5 private ledger: add_device leaves the head status bytes unchanged
// modulo as_of_ms, valid_until_ms and the signature (P1-a at the authority).
func TestAddDeviceLeavesTheStatusUnchangedModuloAsOf(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	before, _, _ := readHead(t, e, a.ID)
	e.deviceOK(a, a.AddDevice(t, authoritytest.NewDevice(t)).Bytes)
	e.advance(1)
	after, dafter, _ := readHead(t, e, a.ID)
	after.AsOfMs, after.ValidUntilMs = before.AsOfMs, before.ValidUntilMs
	if !bytes.Equal(after.Encode(), before.Encode()) {
		t.Fatal("add_device changed the published status (P1-a)")
	}
	if dafter.Dseq != 2 {
		t.Fatalf("device-status dseq = %d, want 2", dafter.Dseq)
	}
}

func TestEveryReadPathPassesTheReleaseGate(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	requireGate(t, e.f, true)
	e.s.readDL = 150 * 1e6
	ctx := context.Background()
	for name, out := range map[string]Outcome{
		"head":    e.s.Head(ctx, a.ID),
		"status":  e.s.Statuses(ctx, ac.StatusReq{AccountIDs: []string{a.ID}}.Encode()),
		"history": e.s.History(ctx, a.ID, 0),
		"devices": e.s.Devices(ctx, a.ID, 0),
	} {
		if out.Code != string(ac.ErrorReplication) {
			t.Fatalf("%s with replication unconfirmed = %+v, want 503 replication (D-225)", name, out)
		}
	}
}

func TestPendingHeadsListsEffectivelyPendingOrDisputed(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	b, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	out := e.s.PendingHeads(context.Background(), "")
	ph, err := ac.DecodePendingHeads(out.Body)
	if err != nil || len(ph.AccountIDs) != 1 || ph.AccountIDs[0] != a.ID {
		t.Fatalf("pending heads = %+v, %v; want only %s (not %s)", ph, err, a.ID, b.ID)
	}
	e.advance(e.r.Current.D.DeltaMs + e.r.Current.D.GMs) // now effectively active
	e.freshTick(t)                                       // otherwise the page is 503 not_ready, whose empty body decodes to no IDs
	out = e.s.PendingHeads(context.Background(), "")
	if ph, err := ac.DecodePendingHeads(out.Body); out.Kind != KindOK || err != nil || len(ph.AccountIDs) != 0 {
		t.Fatalf("after activation = %+v, %+v, %v; want an empty OK page", out, ph, err)
	}
}

// A head that does not normalize fails the page (503 store), as a head that does
// not scan does. It never drops out of the monitor's protective set unseen.
func TestPendingHeadsFailsOnAnUnreadableHead(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	mustExec(t, e.f.Super, `UPDATE authority.account_heads SET device_set = '\x00', device_set_digest = $2 WHERE account_id = $1`,
		a.ID, d48(9))
	if out := e.s.PendingHeads(context.Background(), ""); out.Code != string(ac.ErrorStore) {
		t.Fatalf("pending-heads over an unreadable head = %+v, want 503 store", out)
	}
}

// T10: a read never signs under a delegation that does not list this node's RSK.
func TestHeadStatusRefusesADelegationWithoutThisNodesRSK(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.r.Install(t, e.f.App, e.r.NextDelegation(t, authoritytest.WithRSKs(authoritytest.Point(authoritytest.NewRealm(t).CAK))))
	if out := e.s.Head(context.Background(), a.ID); out.Code != string(ac.ErrorSigner) {
		t.Fatalf("head under a delegation without this node's RSK = %+v, want 503 signer", out)
	}
	// Review round 2: pending-heads signs nothing, so the monitor's seed still answers.
	if out := e.s.PendingHeads(context.Background(), ""); out.Kind != KindOK {
		t.Fatalf("pending-heads under a delegation without this node's RSK = %+v, want 200", out)
	}
}

// D-241 (DoR:478): at most one status per (account, as_of_ms). Two head reads
// in one database millisecond, with only `held` changed between them, never
// share an as_of_ms; a clock that will not pass the last one is 503 clock.
func TestTwoReadsInOneMillisecondNeverShareAnAsOf(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	first, _, _ := readHead(t, e, a.ID)
	e.pinHeld(a.ID)
	second, dsecond, _ := readHead(t, e, a.ID) // the pinned clock has not moved
	if second.Held != 1 || second.AsOfMs <= first.AsOfMs || dsecond.AsOfMs != second.AsOfMs {
		t.Fatalf("as_of %d (held %d) after %d: want a later as_of_ms, shared with its device-status", second.AsOfMs, second.Held, first.AsOfMs)
	}
	e.s.hooks.Pause = func() {} // a database clock that never moves on
	if out := e.s.Head(context.Background(), a.ID); out.HTTP != 503 || out.Code != string(ac.ErrorClock) || !e.s.nonClient503.Load() {
		t.Fatalf("a clock stuck at the last as_of_ms = %+v, want 503 clock and the D-147 leg set", out)
	}
}

// D-232, B1-D31: below DISK_HARD_FLOOR every read family is 503 disk, and the
// refusal sets the tick's D-147 leg.
func TestEveryReadFamilyRefusesBelowTheHardFloor(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.s.hooks.Statfs = func() (uint64, error) { return DiskHardFloorBytes - 1, nil }
	ctx := context.Background()
	for name, out := range map[string]Outcome{
		"head":              e.s.Head(ctx, a.ID),
		"status":            e.s.Statuses(ctx, ac.StatusReq{AccountIDs: []string{a.ID}}.Encode()),
		"history":           e.s.History(ctx, a.ID, 0),
		"devices":           e.s.Devices(ctx, a.ID, 0),
		"pending-heads":     e.s.PendingHeads(ctx, ""),
		"legacy-population": e.s.LegacyPopulation(ctx, ""),
	} {
		if out.HTTP != 503 || out.Code != string(ac.ErrorDisk) {
			t.Fatalf("%s below the hard floor = %+v, want 503 disk", name, out)
		}
	}
	if !e.s.nonClient503.Load() {
		t.Fatal("a read refused at the hard floor did not set the tick's non-client 503 leg")
	}
}

// DoR §3.3 (Codex round 2): a standby answers every read with 503 not_leader,
// below the hard floor too, so the CP moves to its next URL instead of staying
// pinned to a demoted node.
func TestEveryReadFamilyOnAStandbyIsNotLeaderBelowTheHardFloor(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.s.hooks.Statfs = func() (uint64, error) { return DiskHardFloorBytes - 1, nil }
	e.s.hooks.InRecovery = func(context.Context, *sql.Conn) (bool, error) { return true, nil }
	ctx := context.Background()
	for name, out := range map[string]Outcome{
		"head":              e.s.Head(ctx, a.ID),
		"status":            e.s.Statuses(ctx, ac.StatusReq{AccountIDs: []string{a.ID}}.Encode()),
		"history":           e.s.History(ctx, a.ID, 0),
		"devices":           e.s.Devices(ctx, a.ID, 0),
		"pending-heads":     e.s.PendingHeads(ctx, ""),
		"legacy-population": e.s.LegacyPopulation(ctx, ""),
	} {
		if out.HTTP != 503 || out.Code != string(ac.ErrorNotLeader) {
			t.Fatalf("%s on a standby below the hard floor = %+v, want 503 not_leader", name, out)
		}
	}
}

// Codex round 2: a status the RSK fails to sign is 503 signer on the reads, as
// on the commit path, not 503 store.
func TestStatusReadsReportASigningFailureAsSigner(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.s.sig = failingSigner{e.r.RSK}
	ctx := context.Background()
	if out := e.s.Head(ctx, a.ID); out.HTTP != 503 || out.Code != string(ac.ErrorSigner) {
		t.Fatalf("head with a failing signer = %+v, want 503 signer", out)
	}
	if out := e.s.Statuses(ctx, ac.StatusReq{AccountIDs: []string{a.ID}}.Encode()); out.HTTP != 503 || out.Code != string(ac.ErrorSigner) {
		t.Fatalf("statuses with a failing signer = %+v, want 503 signer", out)
	}
}

// Red-team R-1 (Phase 4): the path reads take an account id only in A1's s36
// spelling, as POST /v1/status does through DecodeStatusReq, and refuse any
// other before the store sees it. An uppercase head read matched the row in
// PostgreSQL but took its own D-241 as_of fence key. Every other case was a
// 503 store, which sets the D-147 leg for every account: a non-uuid failed
// the uuid cast, and A1's encoder refused an uppercase page's account id.
func TestPathReadsRefuseANonCanonicalAccountID(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.freshTick(t)
	ctx := context.Background()
	reads := map[string]func(string) Outcome{
		"head":    func(id string) Outcome { return e.s.Head(ctx, id) },
		"history": func(id string) Outcome { return e.s.History(ctx, id, 0) },
		"devices": func(id string) Outcome { return e.s.Devices(ctx, id, 0) },
	}
	for name, read := range reads { // control: the canonical spelling reads
		if out := read(a.ID); out.HTTP != 200 {
			t.Fatalf("%s(%s) = %+v, want 200", name, a.ID, out)
		}
	}
	refused := func(stage string) {
		for name, read := range reads {
			for _, id := range []string{strings.ToUpper(a.ID), "not-a-uuid"} {
				e.s.nonClient503.Store(false)
				if out := read(id); out.HTTP != 400 || out.Code != string(ac.ErrorMalformed) || e.s.nonClient503.Load() {
					t.Errorf("%s: %s(%q) = %d %s, D-147 leg %v; want 400 malformed and the leg unset",
						stage, name, id, out.HTTP, out.Code, e.s.nonClient503.Load())
				}
			}
		}
	}
	refused("ready")
	// Before any store access: below the hard floor every read session is 503 disk.
	e.s.hooks.Statfs = func() (uint64, error) { return DiskHardFloorBytes - 1, nil }
	refused("below the hard floor")
}

// T6 (DoR:347, :720, review round 3): a status is signed only under a delegation
// whose scope holds `status`. Without it, head and statuses are 503 signer
// rather than bytes every client rejects; pending-heads signs nothing.
func TestStatusReadsNeedStatusInScope(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.r.Install(t, e.f.App, e.r.NextDelegation(t, func(d *ac.Delegation) {
		d.Scope = slices.DeleteFunc(slices.Clone(d.Scope), func(s string) bool { return s == ac.ScopeStatus })
	}))
	ctx := context.Background()
	if out := e.s.Head(ctx, a.ID); out.Code != string(ac.ErrorSigner) {
		t.Fatalf("head without status in scope = %+v, want 503 signer", out)
	}
	if out := e.s.Statuses(ctx, ac.StatusReq{AccountIDs: []string{a.ID}}.Encode()); out.Code != string(ac.ErrorSigner) {
		t.Fatalf("statuses without status in scope = %+v, want 503 signer", out)
	}
	if out := e.s.PendingHeads(ctx, ""); out.Kind != KindOK {
		t.Fatalf("pending-heads without status in scope = %+v, want 200", out)
	}
}

// D-241 bounded (review round 3): pruning drops entries older than asOfKeepMs
// and never reopens a coordinate at or below the newest one it pruned.
func TestAsOfFencePrunesWithoutReopening(t *testing.T) {
	s := &Store{}
	if !s.reserveAsOf([]string{"a"}, 1_000) || s.reserveAsOf([]string{"a"}, 1_000) {
		t.Fatal("a second status at the same (account, as_of_ms) was admitted")
	}
	s.pruneAsOf(1_000 + asOfKeepMs + 1)
	if len(s.asOfLast) != 0 {
		t.Fatalf("pruning kept %d entries older than asOfKeepMs", len(s.asOfLast))
	}
	if s.reserveAsOf([]string{"a"}, 1_000) || s.reserveAsOf([]string{"b"}, 999) {
		t.Fatal("pruning reopened a coordinate at or below the newest pruned as_of_ms")
	}
	if !s.reserveAsOf([]string{"b"}, 1_001) {
		t.Fatal("an as_of_ms above the floor was refused")
	}
}

// B1-D29: every signed read runs in one REPEATABLE READ, read-only snapshot.
func TestEveryReadPathSharesOneSnapshot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	conn, err := e.f.App.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	rc, out, ok := e.s.readTx(ctx, conn, false, nil)
	if !ok {
		t.Fatalf("readTx = %+v", out)
	}
	defer func() { _ = rc.tx.Rollback() }()
	var iso, ro string
	if err := rc.tx.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`).
		Scan(&iso, &ro); err != nil {
		t.Fatal(err)
	}
	if iso != "repeatable read" || ro != "on" {
		t.Fatalf("signed reads run at %q, read only %q; want repeatable read, on", iso, ro)
	}
}

// LegacyPopulation (D-185) lists "uuid.base64(digest)" in account order after
// the cursor, and answers 204 once no row remains.
func TestLegacyPopulationPagesThePopulationInAccountOrder(t *testing.T) {
	e, accts := newEnvPop(t, 3)
	ctx := context.Background()
	want := make([]string, 0, len(accts))
	for _, a := range accts {
		want = append(want, a.ID+"."+base64.StdEncoding.EncodeToString(a.Legacy.Digest))
	}
	slices.Sort(want)
	idOf := func(row string) string { return strings.SplitN(row, ".", 2)[0] }
	rows := func(after string) []string {
		t.Helper()
		out := e.s.LegacyPopulation(ctx, after)
		if out.HTTP != 200 || out.Kind != KindOK {
			t.Fatalf("LegacyPopulation(%q) = %+v, want 200", after, out)
		}
		b, err := ac.DecodeLegacyBatch(out.Body)
		if err != nil {
			t.Fatal(err)
		}
		return b.Rows
	}
	if got := rows(""); !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if got := rows(idOf(want[0])); !slices.Equal(got, want[1:]) {
		t.Fatalf("rows after the first = %v, want %v", got, want[1:])
	}
	if out := e.s.LegacyPopulation(ctx, idOf(want[2])); out.HTTP != 204 || out.Kind != KindOK || out.Body != nil {
		t.Fatalf("LegacyPopulation after the last row = %+v, want 204", out)
	}
}

func TestStatusesRefusesAnOversizedOrUndecodableBody(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if out := e.s.Statuses(ctx, make([]byte, BodyCapTx+1)); out.HTTP != 413 || out.Code != string(ac.ErrorTooLarge) {
		t.Fatalf("an oversized body = %+v, want 413 too_large", out)
	}
	if out := e.s.Statuses(ctx, []byte("[")); out.HTTP != 400 || out.Code != string(ac.ErrorMalformed) {
		t.Fatalf("an undecodable body = %+v, want 400 malformed", out)
	}
}

// A page cursor past MaxSafeInt is malformed on both paged reads; the cursor
// at MaxSafeInt itself is an ordinary empty page.
func TestPagedReadsRefuseACursorPastMaxSafeInt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	acct := e.r.NewAccount(t).ID
	for name, read := range map[string]func(uint64) Outcome{
		"history": func(after uint64) Outcome { return e.s.History(ctx, acct, after) },
		"devices": func(after uint64) Outcome { return e.s.Devices(ctx, acct, after) },
	} {
		if out := read(ac.MaxSafeInt); out.HTTP != 200 {
			t.Fatalf("control: %s at MaxSafeInt = %+v, want 200", name, out)
		}
		if out := read(ac.MaxSafeInt + 1); out.HTTP != 400 || out.Code != string(ac.ErrorMalformed) {
			t.Errorf("%s past MaxSafeInt = %+v, want 400 malformed", name, out)
		}
	}
}
