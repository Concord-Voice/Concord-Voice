package decide

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 10: generator 1 and the global invariants. The properties built on it live in
// enumerate_props_test.go (P2, P4, padding) and in TestEnumerate_P1 below.
//
// Generator 1 crosses generator 2's proof compositions (class_enum_test.go: the 56 multisets of
// size <= 3 over the categories K, paper, non-paper BE=0, non-paper BE=1 and anchor) with five
// maturity states per element, relative to the reference instant ref of the action under test:
//
//	mature     matured an hour before ref (counts at ref and at Now)
//	immature   matures an hour after Now
//	awaiting   the awaiting-notice sentinel
//	contested  matured before ref, but contested
//	mature-now matured after ref and before Now (only when ref is before Now)
//
// Without the fifth state, parent mutation #3 ("drop 'matured before opened'") survives (child
// spec §7). Each case is built in two layouts, "distinct" (every element is its own credential)
// and "shared" (every element of a category is the same credential, so a proof lists one ref
// several times; Review Focus 1).
//
// Sizes (each proof case is crossed with the family's request states and targets):
//
//	reference instant Now (T1b, T2/T3, T6):  4 states -> 2501 distinct + 1181 shared proofs
//	reference instant before Now (T5/7/12):  5 states -> 4776 distinct + 2076 shared proofs
//
// Every property is checked against an independent oracle (classOracle over the composition the
// generator itself knows), never against ClassOf or Decide.

type enState uint8

const (
	enMature enState = iota
	enImmature
	enAwaiting
	enContested
	enMatureNow
)

func (s enState) String() string {
	return [...]string{"mature", "immature", "awaiting", "contested", "mature-now"}[s]
}

// countsAtRef reports whether an element in this state counts at the reference instant. A
// mature-now element counts at Now but not at ref; that is the point of the fifth state.
func (s enState) countsAtRef() bool { return s == enMature }

// countsAtNow reports whether an element in this state counts at Now.
func (s enState) countsAtNow() bool { return s == enMature || s == enMatureNow }

func enStates(refBack bool) []enState {
	if refBack {
		return []enState{enMature, enImmature, enAwaiting, enContested, enMatureNow}
	}
	return []enState{enMature, enImmature, enAwaiting, enContested}
}

// enRefAt is the reference instant of an action: Now for T1b/T2/T3/T6, two hours before Now
// for T5 (EnrolledAt), T7 (OpenedAt) and T12 (DisputedAt).
func enRefAt(refBack bool) time.Time {
	if refBack {
		return t0.Add(-2 * time.Hour)
	}
	return t0
}

// enCred builds the credential for a ref in a maturity state, relative to ref.
func enCred(r CredRef, s enState, ref time.Time) Cred {
	c := matureCred(r)
	switch s {
	case enImmature:
		c.MaturesAt = t0.Add(time.Hour)
	case enAwaiting:
		c.MaturesAt = awaitingInstant
	case enContested:
		c.MaturesAt = ref.Add(-time.Hour)
		c.ContestedAt = t0.Add(-time.Minute) // after ref in the back-ref families: D7, never counts
	case enMatureNow:
		c.MaturesAt = ref.Add(t0.Sub(ref) / 2)
	default:
		c.MaturesAt = ref.Add(-time.Hour)
	}
	return c
}

type enSlot struct {
	cat   gen2Cat
	ref   CredRef
	state enState
}

// enCase is one proof composition with one maturity assignment.
type enCase struct {
	ms      []gen2Cat
	shared  bool
	refBack bool
	ref     time.Time
	slots   []enSlot
	elem    []int // Proof.Used[i] is slots[elem[i]].ref
}

// proof is the proof with its device-bound refs: a generated WebAuthn is device-bound exactly when
// it belongs to the non-paper BE=0 category (F1: DeviceBound is the only source of BE).
func (c enCase) proof() Proof {
	used := c.used()
	return Proof{Used: used, DeviceBound: gen2DeviceBound(used)}
}

// proofCounted is proof with every element that does not count at ref removed (P4).
func (c enCase) proofCounted() Proof {
	used := c.usedCounted()
	return Proof{Used: used, DeviceBound: gen2DeviceBound(used)}
}

func (c enCase) used() []CredRef {
	out := make([]CredRef, len(c.elem))
	for i, e := range c.elem {
		out[i] = c.slots[e].ref
	}
	return out
}

// usedCounted is the proof with every element that does not count at ref removed (P4).
func (c enCase) usedCounted() []CredRef {
	out := make([]CredRef, 0, len(c.elem))
	for _, e := range c.elem {
		if c.slots[e].state.countsAtRef() {
			out = append(out, c.slots[e].ref)
		}
	}
	return out
}

// creds returns one credential per slot, with room for a few more.
func (c enCase) creds() []Cred {
	out := make([]Cred, len(c.slots), len(c.slots)+8)
	for i, s := range c.slots {
		out[i] = enCred(s.ref, s.state, c.ref)
	}
	return out
}

// oracleWith is the independent expected class: the composition of the slots that count
// (counts) and are not skipped, run through classOracle.
func (c enCase) oracleWith(counts func(enState) bool, skip func(CredRef) bool) Class {
	var n [gen2NumCats]int
	for _, s := range c.slots {
		if counts(s.state) && (skip == nil || !skip(s.ref)) {
			n[s.cat]++
		}
	}
	return classOracle(n[catK], n[catPaper], n[catNP0], n[catNP1], n[catA])
}

// oracle is the expected class at ref.
func (c enCase) oracle(skip func(CredRef) bool) Class {
	return c.oracleWith(enState.countsAtRef, skip)
}

// npCounted is the number of non-paper possession slots that count at ref.
func (c enCase) npCounted() int {
	n := 0
	for _, s := range c.slots {
		if s.state.countsAtRef() && (s.cat == catNP0 || s.cat == catNP1) {
			n++
		}
	}
	return n
}

// countedDistinct is the distinct refs that count at ref, in canonical order (ruling 6).
func (c enCase) countedDistinct() []CredRef {
	var out []CredRef
	for _, s := range c.slots {
		if s.state.countsAtRef() {
			out = append(out, s.ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return credRefLess(out[i], out[j]) })
	return out
}

func (c enCase) label() string {
	parts := make([]string, len(c.slots))
	for i, s := range c.slots {
		parts[i] = fmt.Sprintf("%s:%s=%s", s.cat, s.ref.ID, s.state)
	}
	layout := "distinct"
	if c.shared {
		layout = "shared"
	}
	return fmt.Sprintf("%s ms=%v slots=[%s] used=%v", layout, c.ms, strings.Join(parts, " "), c.used())
}

// enForEach visits every case of generator 1 and returns how many it visited.
func enForEach(refBack, shared bool, fn func(c enCase)) int {
	states := enStates(refBack)
	ref := enRefAt(refBack)
	n := 0
	for _, ms := range gen2Multisets() {
		g2slots, elem := gen2Layout(ms, shared)
		total := 1
		for range g2slots {
			total *= len(states)
		}
		for code := 0; code < total; code++ {
			c := enCase{ms: ms, shared: shared, refBack: refBack, ref: ref, elem: elem, slots: make([]enSlot, len(g2slots))}
			rest := code
			for i, s := range g2slots {
				c.slots[i] = enSlot{cat: s.cat, ref: s.ref, state: states[rest%len(states)]}
				rest /= len(states)
			}
			fn(c)
			n++
		}
	}
	return n
}

// TestEnumerate_GeneratorSizes pins the sizes quoted in the header, so a generator slip cannot
// shrink the enumeration silently.
func TestEnumerate_GeneratorSizes(t *testing.T) {
	noop := func(enCase) {}
	assert.Equal(t, 2501, enForEach(false, false, noop), "4 states, distinct")
	assert.Equal(t, 1181, enForEach(false, true, noop), "4 states, shared")
	assert.Equal(t, 4776, enForEach(true, false, noop), "5 states, distinct")
	assert.Equal(t, 2076, enForEach(true, true, noop), "5 states, shared")
}

// TestEnumerate_StatesBehaveAsNamed guards the generator's own credentials with ClassOf (a single
// element, so no oracle is needed): the fifth state counts at Now and not at the earlier ref.
func TestEnumerate_StatesBehaveAsNamed(t *testing.T) {
	ref := enRefAt(true)
	for _, s := range enStates(true) {
		cred := enCred(fxTOTP, s, ref)
		p := Proof{Used: []CredRef{fxTOTP}}
		atRef, atNow := ClassOf(p, []Cred{cred}, ref), ClassOf(p, []Cred{cred}, t0)
		wantRef, wantNow := ClassNone, ClassNone
		if s.countsAtRef() {
			wantRef = C2
		}
		if s.countsAtNow() {
			wantNow = C2
		}
		assert.Equal(t, wantRef, atRef, "%s at ref", s)
		assert.Equal(t, wantNow, atNow, "%s at Now", s)
	}
}

// ---- credentials outside the proof universe --------------------------------------------

var (
	enTgtSSO  = cref(CredSSO, "t-sso")
	enTgtTOTP = cref(CredTOTP, "t-totp")
	enTgtRK   = cref(CredRecoveryKey, "t-rk")
	enOrigin  = cref(CredPassword, "origin-x")
	enCand    = Candidate{Keyed: []byte("k"), Coarse: []byte("c"), KeyID: 1}
)

// enBystanders are credentials no proof names: three mature possession factors (so a T6c reset
// has something to remove) and one immature TOTP.
func enBystanders() []Cred {
	return []Cred{
		matureCred(cref(CredTOTP, "by-totp")), matureCred(cref(CredWebAuthn, "by-wa")),
		matureCred(cref(CredBackupCodeSet, "by-set")), credMaturing(cref(CredTOTP, "by-imm"), t0.Add(time.Hour)),
	}
}

// enPhisher is an immature credential enrolled half an hour before Now at the given class.
func enPhisher(r CredRef, class Class) Cred {
	c := matureCred(r)
	c.MaturesAt = t0.Add(70 * time.Hour)
	c.EnrolledAt = t0.Add(-30 * time.Minute)
	c.EnrolClass = class
	return c
}

// enContestTarget is an immature credential enrolled at ref = t0-2h (the back reference of T5).
func enContestTarget(r CredRef, class Class) Cred {
	c := matureCred(r)
	c.MaturesAt = t0.Add(70 * time.Hour)
	c.EnrolledAt = enRefAt(true)
	c.EnrolClass = class
	return c
}

func enOpenReq(id string, kind ReqKind, status ReqStatus, oc Class, origins, targets []CredRef) Req {
	r := Req{ID: id, Kind: kind, Status: status, OriginClass: oc, Origins: origins, Targets: targets,
		OpenedAt: t0.Add(-2 * time.Hour), CompletesAt: t0.Add(70 * time.Hour)}
	if status == StatusDisputed {
		r.DisputedAt = t0.Add(-time.Hour)
		r.DisputeGroup = "g"
	}
	return r
}

func enRecent(id string, reason CancelReason, canceller Class) Req {
	return Req{ID: id, Kind: ReqBreakGlass, Status: StatusCancelled, OriginClass: C1, Origins: []CredRef{enOrigin},
		OpenedAt: t0.Add(-3 * time.Hour), CompletesAt: t0.Add(69 * time.Hour),
		Fingerprint: []byte("k"), Coarse: []byte("c"), FingerprintKeyID: 1,
		CancelReason: reason, CancellerClass: canceller, CancelledAt: t0.Add(-time.Hour)}
}

// enDisputedPair is r1 and r2 in group g; the earliest DisputedAt (the reference instant of T12)
// is t0-2h.
func enDisputedPair() []Req {
	mk := func(id string, at time.Time, origin CredRef) Req {
		return Req{ID: id, Kind: ReqBreakGlass, Status: StatusDisputed, OriginClass: C1, Origins: []CredRef{origin},
			OpenedAt: at.Add(-time.Hour), CompletesAt: at.Add(71 * time.Hour), DisputedAt: at, DisputeGroup: "g"}
	}
	return []Req{
		mk("r1", t0.Add(-2*time.Hour), cref(CredPassword, "origin-1")),
		mk("r2", t0.Add(-time.Hour), cref(CredPassword, "origin-2")),
	}
}

// enNumVariants is the number of request states of enReqVariant.
const enNumVariants = 8

// enReqVariant builds the request state v (plan Task 10 "Request states"):
//
//	0 none
//	1 a pending credential_remove on the same target (Remove only)
//	2 a pending break-glass
//	3 a disputed pair
//	4 a recent proof cancel with the same fingerprint, canceller C3 (above c unless c is C3)
//	5 a recent proof cancel with the same fingerprint, canceller C1 (at or below c)
//	6 a recent hold_cap cancel with the same fingerprint
//	7 a pending break-glass whose origin set equals the proof's counted refs (a duplicate; A2)
//
// ok is false when the state does not apply. Origins use IDs outside the proof universe.
func enReqVariant(v int, c enCase, tgt CredRef, forRemove bool) (reqs, recents []Req, ok bool) {
	switch v {
	case 0:
		return nil, nil, true
	case 1:
		if !forRemove {
			return nil, nil, false
		}
		return []Req{enOpenReq("p-same", ReqCredentialRemove, StatusPending, C1, []CredRef{enOrigin}, []CredRef{tgt})}, nil, true
	case 2:
		return []Req{enOpenReq("p-bg", ReqBreakGlass, StatusPending, C1, []CredRef{enOrigin}, nil)}, nil, true
	case 3:
		return enDisputedPair(), nil, true
	case 4:
		return nil, []Req{enRecent("old", CancelProof, C3)}, true
	case 5:
		return nil, []Req{enRecent("old", CancelProof, C1)}, true
	case 6:
		return nil, []Req{enRecent("old", CancelHoldCap, C3)}, true
	case 7:
		counted := c.countedDistinct()
		if len(counted) == 0 {
			return nil, nil, false
		}
		return []Req{enOpenReq("p-dup", ReqBreakGlass, StatusPending, C1, counted, nil)}, nil, true
	}
	return nil, nil, false
}

// ---- the global invariants -------------------------------------------------------------

func enHas(o Outcome, k EffectKind) bool {
	for _, e := range o.Effects {
		if e.Kind == k {
			return true
		}
	}
	return false
}

func enFindExact(creds []Cred, r CredRef) (Cred, bool) {
	for _, c := range creds {
		if c.Ref == r {
			return c, true
		}
	}
	return Cred{}, false
}

func enFindReq(reqs []Req, id string) (Req, bool) {
	for _, r := range reqs {
		if r.ID == id {
			return r, true
		}
	}
	return Req{}, false
}

// enCounts is the independent statement of D7: uncontested and matured strictly before ref.
func enCounts(c Cred, ref time.Time) bool { return c.ContestedAt.IsZero() && c.MaturesAt.Before(ref) }

func enExpectedSource(in Input) Source {
	switch a := in.Act.(type) {
	case OpenBreakGlass:
		return SourceBreakGlass
	case Complete:
		if r, ok := enFindReq(in.Reqs, a.ID); ok && r.Kind == ReqBreakGlass {
			return SourceBreakGlass
		}
	}
	return SourceUserSession
}

var (
	enVocabNone  = map[EffectKind]bool{}
	enVocabByAct = map[string]map[EffectKind]bool{
		"SignOutOthers":     enVocabNone,
		"SignOutEverywhere": {EffectContest: true, EffectResyncMFAFlags: true},
		"Remove":            {EffectRemove: true, EffectOpen: true, EffectDispute: true, EffectHold: true, EffectResyncMFAFlags: true},
		"SelfRemoveBackup":  {EffectRemove: true, EffectResyncMFAFlags: true},
		"Enrol":             enVocabNone,
		"Contest":           {EffectContest: true, EffectRestorePrimary: true, EffectResyncMFAFlags: true},
		"OpenBreakGlass": {EffectOpen: true, EffectDispute: true, EffectHold: true, EffectRemove: true,
			EffectRestorePrimary: true, EffectResyncMFAFlags: true},
		"Cancel":  {EffectCancel: true, EffectHold: true},
		"Resolve": {EffectReturnToPending: true, EffectCancel: true},
		"Complete": {EffectComplete: true, EffectRemove: true, EffectCancel: true, EffectHold: true,
			EffectRestorePrimary: true, EffectResyncMFAFlags: true},
		"Expire": {EffectCancel: true, EffectRemove: true, EffectReleaseReservation: true, EffectResyncMFAFlags: true},
	}
)

func enReservedRefs(in Input) []CredRef {
	if len(in.Reservations) == 0 {
		return nil
	}
	out := make([]CredRef, 0, len(in.Reservations))
	for _, r := range in.Reservations {
		out = append(out, r.AddressRef)
	}
	sort.Slice(out, func(i, j int) bool { return credRefLess(out[i], out[j]) })
	dedup := out[:1]
	for _, r := range out[1:] {
		if r != dedup[len(dedup)-1] {
			dedup = append(dedup, r)
		}
	}
	return dedup
}

func enSameRefs(a, b []CredRef) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// invariantViolations returns every global invariant (plan Task 10 "Global", plus the P1
// predicate and the shape rules of architect rulings 3, 4, 6, 9, 11 and 13 and A2) the outcome
// breaks. It calls Decide once more, on the reversed snapshot, for order independence (D18).
func invariantViolations(in Input, out Outcome) []string {
	var v []string
	bad := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }

	if want := enExpectedSource(in); out.Source != want {
		bad("source %q, want %q (D11)", out.Source, want)
	}

	hasContest, hasRemove, hasRestore := enHas(out, EffectContest), enHas(out, EffectRemove), enHas(out, EffectRestorePrimary)
	completedOpen := false
	for _, e := range out.Effects {
		if e.Kind == EffectOpen && e.Open.Status == StatusCompleted {
			completedOpen = true
		}
	}
	bgCompletion := out.Source == SourceBreakGlass && (enHas(out, EffectComplete) || completedOpen)

	// Flags.
	if out.SetsPassword && !out.RotateEpoch {
		bad("SetsPassword without RotateEpoch")
	}
	if hasContest && !out.RotateEpoch {
		bad("a contest without RotateEpoch")
	}
	if bgCompletion && (!out.RotateEpoch || !out.SetsPassword) {
		bad("a break-glass completion without RotateEpoch and SetsPassword (D15)")
	}
	applied := out.Verdict == VerdictApplied
	switch out.Row {
	case RowT1:
		if out.RotateEpoch != applied || out.Continuation != applied {
			bad("T1 flags rotate=%t continuation=%t on verdict %d", out.RotateEpoch, out.Continuation, out.Verdict)
		}
	case RowT1b:
		if out.RotateEpoch != applied || out.Continuation {
			bad("T1b flags rotate=%t continuation=%t on verdict %d", out.RotateEpoch, out.Continuation, out.Verdict)
		}
	case RowT5:
		if out.RotateEpoch != applied {
			bad("T5 rotate=%t on verdict %d (#40)", out.RotateEpoch, out.Verdict)
		}
	case RowT6c:
		if !out.RotateEpoch || !out.SetsPassword {
			bad("T6c without rotate and password")
		}
	case RowT11:
		if out.RotateEpoch != bgCompletion {
			bad("T11 rotate=%t but break-glass completion=%t (D15)", out.RotateEpoch, bgCompletion)
		}
	default:
		if out.RotateEpoch {
			bad("row %d rotates the epoch", out.Row)
		}
	}
	if out.Continuation && out.Row != RowT1 {
		bad("continuation on row %d", out.Row)
	}
	if out.PermGenBump != (hasContest || hasRemove) {
		bad("PermGenBump=%t with contest=%t remove=%t (D17)", out.PermGenBump, hasContest, hasRemove)
	}
	resyncs := 0
	for _, e := range out.Effects {
		if e.Kind == EffectResyncMFAFlags {
			resyncs++
		}
	}
	if wantResync := hasContest || hasRemove || hasRestore; (resyncs == 1) != wantResync || resyncs > 1 {
		bad("%d EffectResyncMFAFlags with contest/remove/restore=%t (D17)", resyncs, wantResync)
	}

	// Verdict and row.
	switch {
	case out.Verdict == VerdictRefused:
		if len(out.Effects) != 0 || len(out.Notices) != 0 || out.RotateEpoch || out.SetsPassword || out.Continuation || out.PermGenBump {
			bad("a refused outcome carries effects, notices or flags")
		}
	case out.Row == RowNone:
		bad("a non-refused outcome with RowNone (ruling 8)")
	}
	if out.Verdict == VerdictOpened && out.Row != RowT3 && out.Row != RowT6 {
		bad("Opened on row %d", out.Row)
	}
	if out.Verdict == VerdictDisputed && out.Row != RowT8 && out.Row != RowT10 {
		bad("Disputed on row %d", out.Row)
	}
	if out.Verdict == VerdictHeld && out.Row != RowT11 {
		bad("Held on row %d", out.Row)
	}
	if out.Row == RowT9 && out.Verdict != VerdictRefused {
		bad("T9 is a refusal, got verdict %d", out.Verdict)
	}

	// Canonical output (D18, ruling 6).
	if out.Effects != nil && len(out.Effects) == 0 || out.Notices != nil && len(out.Notices) == 0 {
		bad("an empty Effects or Notices is not nil")
	}
	for i := 1; i < len(out.Effects); i++ {
		if effectLess(out.Effects[i], out.Effects[i-1]) {
			bad("Effects not sorted at %d", i)
		}
		if reflect.DeepEqual(out.Effects[i], out.Effects[i-1]) {
			bad("duplicate effect %+v", out.Effects[i])
		}
	}
	for i := 1; i < len(out.Notices); i++ {
		if noticeLess(out.Notices[i], out.Notices[i-1]) {
			bad("Notices not sorted at %d", i)
		}
		if reflect.DeepEqual(out.Notices[i], out.Notices[i-1]) {
			bad("duplicate notice %+v", out.Notices[i])
		}
	}

	// Effect vocabulary per action, request ids (ruling 11), canceller class (ruling 13).
	var vocab map[EffectKind]bool
	if in.Act != nil {
		vocab = enVocabByAct[reflect.TypeOf(in.Act).Name()]
	}
	for _, e := range out.Effects {
		if !vocab[e.Kind] {
			bad("effect kind %d is outside the vocabulary of %T", e.Kind, in.Act)
		}
		switch e.Kind {
		case EffectCancel, EffectDispute, EffectReturnToPending, EffectComplete, EffectHold:
			if _, ok := enFindReq(in.Reqs, e.RequestID); !ok {
				bad("effect %d names request %q, which is not in the snapshot", e.Kind, e.RequestID)
			}
		default:
			if e.RequestID != "" {
				bad("effect %d carries request id %q (ruling 11)", e.Kind, e.RequestID)
			}
		}
		if e.Kind == EffectCancel {
			wantClass := e.Reason == CancelProof || e.Reason == CancelDisputeResolved
			if wantClass != (e.CancellerClass != ClassNone) {
				bad("cancel %q reason %d has canceller class %d (ruling 13)", e.RequestID, e.Reason, e.CancellerClass)
			}
		}
		if e.Kind == EffectHold && e.HeldMs < 0 {
			bad("negative HeldMs")
		}
		if e.Kind == EffectOpen {
			v = append(v, enOpeningViolations(in, e.Open)...)
		}
	}

	v = append(v, enEnrolClassViolations(in, out)...)
	v = append(v, enNoticeViolations(in, out)...)
	v = append(v, enP1Violations(in, out)...)

	if rev := Decide(reorderInput(in, 0)); !reflect.DeepEqual(out, rev) {
		bad("the outcome depends on input slice order (D18)")
	}
	return v
}

func enOpeningViolations(in Input, op Opening) []string {
	var v []string
	bad := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }
	if !op.OpenedAt.Equal(in.Now) {
		bad("OpenedAt %v is not Now", op.OpenedAt)
	}
	switch op.Status {
	case StatusPending, StatusDisputed:
		// P1: an opened request waits exactly delta_cp.
		if !op.CompletesAt.Equal(in.Now.Add(DeltaCP)) {
			bad("CompletesAt %v is not Now+DeltaCP", op.CompletesAt)
		}
		if (op.Status == StatusDisputed) != op.DisputedAt.Equal(in.Now) || (op.Status == StatusPending) != op.DisputedAt.IsZero() {
			bad("DisputedAt %v on status %d", op.DisputedAt, op.Status)
		}
		if !op.CompletedAt.IsZero() {
			bad("CompletedAt on an open request")
		}
	case StatusCompleted:
		if !op.CompletesAt.Equal(in.Now) || !op.CompletedAt.Equal(in.Now) || !op.DisputedAt.IsZero() {
			bad("a completed opening must have CompletesAt=CompletedAt=Now and no DisputedAt (ruling 5)")
		}
		if len(op.Candidate.Keyed) != 0 || len(op.Candidate.Coarse) != 0 || op.Candidate.KeyID != 0 {
			bad("a completed opening carries a fingerprint (ruling 15a)")
		}
	default:
		bad("opening status %d", op.Status)
	}
	if op.Kind == ReqBreakGlass && len(op.Targets) != 0 {
		bad("a break-glass opening has Targets %v (ruling 4)", op.Targets)
	}
	if !refsStrictlyIncreasing(op.Origins) || !refsStrictlyIncreasing(op.Targets) {
		bad("Origins %v or Targets %v are not in canonical order (ruling 6)", op.Origins, op.Targets)
	}
	// A2: only refs that match a credential exactly and count at Now.
	for _, o := range op.Origins {
		cr, ok := enFindExact(in.Creds, o)
		inProof := false
		for _, u := range in.Proof.Used {
			if u == o {
				inProof = true
			}
		}
		if !ok || !enCounts(cr, in.Now) || !inProof {
			bad("origin %+v is not a counted proof ref (A2)", o)
		}
	}
	return v
}

// enEnrolClassViolations is the F2 contract (architect rulings-3): Outcome.EnrolClass is the
// class the runner writes with enrolled_at. T1 and T4 write ClassOf(Proof, Creds, Now), T6c writes
// C3, a T11 completion of a break_glass or an address_change (the kinds that enrol or restore a
// credential) writes the request's OriginClass, and every other outcome (a refusal and a
// credential_remove completion included) writes nothing.
func enEnrolClassViolations(in Input, out Outcome) []string {
	var want Class
	switch {
	case out.Verdict == VerdictRefused:
	case out.Row == RowT1, out.Row == RowT4:
		want = ClassOf(in.Proof, in.Creds, in.Now)
	case out.Row == RowT6c:
		want = C3
	case out.Row == RowT11 && enHas(out, EffectComplete):
		if a, ok := in.Act.(Complete); ok {
			if r, found := enFindReq(in.Reqs, a.ID); found && (r.Kind == ReqBreakGlass || r.Kind == ReqAddressChange) {
				want = r.OriginClass
			}
		}
	}
	if out.EnrolClass != want {
		return []string{fmt.Sprintf("EnrolClass %d on row %d verdict %d, want %d (F2)", out.EnrolClass, out.Row, out.Verdict, want)}
	}
	return nil
}

func enNoticeViolations(in Input, out Outcome) []string {
	var v []string
	bad := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }
	reserved := enReservedRefs(in)
	for _, n := range out.Notices {
		if n.Kind == NoticeUnknown {
			bad("a notice of kind NoticeUnknown")
		}
		if n.RequestID != "" {
			if _, ok := enFindReq(in.Reqs, n.RequestID); !ok {
				bad("notice %d names request %q, which is not in the snapshot", n.Kind, n.RequestID)
			}
		}
		isChange := isChangeNotice(n.Kind)
		mustReserve := isChange ||
			((n.Kind == NoticeCredentialRemoved || n.Kind == NoticePasswordChanged) && (out.Row == RowT6c || out.Row == RowT11))
		want := []CredRef(nil)
		if mustReserve {
			want = reserved
		}
		if !enSameRefs(n.ReservedTo, want) {
			bad("notice %d ReservedTo %v, want %v (ruling 3)", n.Kind, n.ReservedTo, want)
		}
		subjectZero := n.Subject == CredRef{}
		switch n.Kind {
		case NoticeCredentialAdded, NoticeCredentialRemoved, NoticeEnrolmentContested:
			if subjectZero {
				bad("notice %d without a Subject", n.Kind)
			}
		default:
			if !subjectZero {
				bad("notice %d carries Subject %+v (ruling 3)", n.Kind, n.Subject)
			}
		}
	}
	return v
}

// enP1Violations is the P1 predicate (parent §7.6): an EffectRemove on a credential that counts
// at Now with rank r needs ClassOf > r, or the completion of a request past delta_cp (or T2b's
// self-removal of a backup address, which names itself). No EffectContest names a counted or an
// already contested credential.
func enP1Violations(in Input, out Outcome) []string {
	var v []string
	bad := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }
	var cls Class
	clsDone := false
	for _, e := range out.Effects {
		switch e.Kind {
		case EffectRemove:
			cr, ok := enFindExact(in.Creds, e.Cred)
			if !ok {
				bad("removes %+v, which is not in the snapshot", e.Cred)
				continue
			}
			if !enCounts(cr, in.Now) {
				continue
			}
			if !clsDone {
				cls, clsDone = ClassOf(in.Proof, in.Creds, in.Now), true
			}
			if uint8(cls) > uint8(e.Cred.Kind.Rank()) {
				continue
			}
			switch a := in.Act.(type) {
			case Complete:
				r, found := enFindReq(in.Reqs, a.ID)
				if found && enHas(out, EffectComplete) && !in.Now.Before(r.OpenedAt.Add(DeltaCP)) {
					continue
				}
				bad("P1: completion removes counted %+v before delta_cp or without EffectComplete", e.Cred)
			case SelfRemoveBackup:
				if out.Row == RowT2b && e.Cred.Kind == CredBackupAddress && e.Cred == a.Target {
					continue
				}
				bad("P1: T2b removes %+v", e.Cred)
			default:
				bad("P1: %T removes counted %+v of rank %d at class %d", in.Act, e.Cred, e.Cred.Kind.Rank(), cls)
			}
		case EffectContest:
			cr, ok := enFindExact(in.Creds, e.Cred)
			if !ok || !cr.ContestedAt.IsZero() || enCounts(cr, in.Now) {
				bad("P1: contest names %+v (found=%t) which is mature or already contested", e.Cred, ok)
			}
		}
	}
	return v
}

// checkInvariants reports every global invariant the outcome breaks (plan Task 10).
func checkInvariants(t *testing.T, in Input, out Outcome) {
	t.Helper()
	for _, msg := range invariantViolations(in, out) {
		t.Errorf("invariant: %s", msg)
	}
}

// ---- the driver -------------------------------------------------------------------------

type enCall struct {
	act      string
	c        enCase
	variant  int
	tgtState enState
	in       Input
	filtered *Input // P4: the same input with every uncounted proof element removed
}

func (k enCall) label() string {
	return fmt.Sprintf("%s variant=%d target=%s %s", k.act, k.variant, k.tgtState, k.c.label())
}

type enStats struct {
	t           *testing.T
	calls       int
	bad         int
	completions int
	rows        map[Row]int
	verdicts    map[Verdict]int
}

func newEnStats(t *testing.T) *enStats {
	return &enStats{t: t, rows: map[Row]int{}, verdicts: map[Verdict]int{}}
}

func (s *enStats) fail(label string, msgs ...string) {
	s.t.Helper()
	s.bad++
	for _, m := range msgs {
		s.t.Errorf("%s: %s", label, m)
	}
	if s.bad >= 12 {
		s.t.Fatalf("stopping after %d failing calls", s.bad)
	}
}

// check runs Decide and the global invariants on a call.
func (s *enStats) check(call enCall) Outcome {
	s.t.Helper()
	s.calls++
	out := Decide(call.in)
	s.rows[out.Row]++
	s.verdicts[out.Verdict]++
	if vs := invariantViolations(call.in, out); len(vs) > 0 {
		s.fail(call.label(), vs...)
	}
	return out
}

// ---- families ---------------------------------------------------------------------------

func enRemoveCall(c enCase, tgt CredRef, ts enState, v int, reqs, recents []Req, p4 bool) enCall {
	creds := append(c.creds(), enCred(tgt, ts, t0))
	in := Input{Now: t0, Act: Remove{Target: tgt, Candidate: enCand}, Proof: c.proof(), Creds: creds,
		Reqs: reqs, RecentCancels: recents, CurrentKeyID: 1}
	call := enCall{act: "remove " + tgt.ID, c: c, variant: v, tgtState: ts, in: in}
	if p4 {
		f := in
		f.Proof = c.proofCounted()
		call.filtered = &f
	}
	return call
}

// enEmitRemove: T2/T3 (and T8/T9/T10 through the request states). Targets are the SSO (rank 1),
// the TOTP (rank 2) and the recovery key (rank 3). P4 keeps the targets mature.
func enEmitRemove(p4 bool, emit func(enCall)) {
	targets := []CredRef{enTgtSSO, enTgtTOTP, enTgtRK}
	enForEach(false, false, func(c enCase) {
		for _, tgt := range targets {
			for v := 0; v < enNumVariants; v++ {
				if reqs, recents, ok := enReqVariant(v, c, tgt, true); ok {
					emit(enRemoveCall(c, tgt, enMature, v, reqs, recents, p4))
				}
			}
		}
	})
	enForEach(false, true, func(c enCase) {
		for _, tgt := range targets {
			for _, v := range []int{0, 7} {
				if reqs, recents, ok := enReqVariant(v, c, tgt, true); ok {
					emit(enRemoveCall(c, tgt, enMature, v, reqs, recents, p4))
				}
			}
		}
	})
	if p4 {
		return
	}
	enForEach(false, false, func(c enCase) {
		for _, tgt := range targets {
			for _, ts := range []enState{enImmature, enAwaiting, enContested} {
				emit(enRemoveCall(c, tgt, ts, 0, nil, nil, false))
			}
		}
	})
}

func enOpenBGCall(c enCase, reset bool, v int, reqs, recents []Req, p4 bool) enCall {
	creds := append(c.creds(), enBystanders()...)
	in := Input{Now: t0, Act: OpenBreakGlass{Reset: reset, Candidate: enCand}, Proof: c.proof(), Creds: creds,
		Reqs: reqs, RecentCancels: recents, CurrentKeyID: 1}
	call := enCall{act: fmt.Sprintf("open_break_glass reset=%t", reset), c: c, variant: v, in: in}
	if p4 {
		f := in
		f.Proof = c.proofCounted()
		call.filtered = &f
	}
	return call
}

// enEmitOpenBG: T6/T6c (and T8/T9/T10).
func enEmitOpenBG(p4 bool, emit func(enCall)) {
	enForEach(false, false, func(c enCase) {
		for _, reset := range []bool{false, true} {
			for v := 0; v < enNumVariants; v++ {
				if reqs, recents, ok := enReqVariant(v, c, CredRef{}, false); ok {
					emit(enOpenBGCall(c, reset, v, reqs, recents, p4))
				}
			}
		}
	})
	enForEach(false, true, func(c enCase) {
		for _, v := range []int{0, 7} {
			if reqs, recents, ok := enReqVariant(v, c, CredRef{}, false); ok {
				emit(enOpenBGCall(c, false, v, reqs, recents, p4))
			}
		}
	})
}

func enCancelCall(c enCase, r Req, variant int, p4 bool) enCall {
	in := Input{Now: t0, Act: Cancel{IDs: []string{r.ID}}, Proof: c.proof(), Creds: c.creds(), Reqs: []Req{r}}
	call := enCall{act: fmt.Sprintf("cancel oc=%d", r.OriginClass), c: c, variant: variant, in: in}
	if p4 {
		f := in
		f.Proof = c.proofCounted()
		call.filtered = &f
	}
	return call
}

// enEmitCancel: T7 against a pending request that removes a foreign SSO (variant 0) or the
// proof universe's address (variant 1, which self-vetoes a proof address).
func enEmitCancel(p4 bool, emit func(enCall)) {
	targets := [][]CredRef{{enTgtSSO}, {fxAddr}}
	enForEach(true, false, func(c enCase) {
		for ti, tg := range targets {
			for _, oc := range []Class{C1, C2, C3} {
				r := enOpenReq("r", ReqCredentialRemove, StatusPending, oc, []CredRef{enOrigin}, tg)
				emit(enCancelCall(c, r, ti, p4))
			}
		}
	})
}

func enContestCall(c enCase, targets []Cred, variant int, p4 bool) enCall {
	in := Input{Now: t0, Act: Contest{}, Proof: c.proof(), Creds: append(c.creds(), targets...)}
	call := enCall{act: "contest", c: c, variant: variant, in: in}
	if p4 {
		f := in
		f.Proof = c.proofCounted()
		call.filtered = &f
	}
	return call
}

// enEmitContest: T5 over immature SSO targets enrolled at C1, C2 and C3 (threshold = the class),
// one per call; the non-P4 enumeration adds a call holding every kind of target at once.
func enEmitContest(p4 bool, emit func(enCall)) {
	enForEach(true, false, func(c enCase) {
		for i, class := range []Class{C1, C2, C3} {
			emit(enContestCall(c, []Cred{enContestTarget(cref(CredSSO, "ct-sso"), class)}, i, p4))
		}
		if !p4 {
			all := []Cred{
				enContestTarget(cref(CredSSO, "ct-sso1"), C1), enContestTarget(cref(CredSSO, "ct-sso2"), C2),
				enContestTarget(cref(CredSSO, "ct-sso3"), C3), enContestTarget(cref(CredWebAuthn, "ct-wa"), C2),
				enContestTarget(cref(CredRecoveryDevice, "ct-rd"), C3), enContestTarget(cref(CredPassword, "ct-pw"), ClassNone),
			}
			emit(enContestCall(c, all, 3, false))
		}
	})
}

func enResolveCall(c enCase, chosen string, p4 bool) enCall {
	in := Input{Now: t0, Act: Resolve{Group: "g", Chosen: chosen}, Proof: c.proof(), Creds: c.creds(),
		Reqs: enDisputedPair()}
	call := enCall{act: "resolve chosen=" + chosen, c: c, in: in}
	if p4 {
		f := in
		f.Proof = c.proofCounted()
		call.filtered = &f
	}
	return call
}

// enEmitResolve: T12 over the disputed pair r1 (DisputedAt t0-2h) and r2 (t0-1h).
func enEmitResolve(p4 bool, emit func(enCall)) {
	enForEach(true, false, func(c enCase) {
		for _, chosen := range []string{"r1", ""} {
			emit(enResolveCall(c, chosen, p4))
		}
	})
}

// enEmitSignOuts: T1 over account shapes (MFA or not, password login or SSO) and T1b with
// phisher credentials in the snapshot for the 3-A contest set. Neither reads maturity of the
// proof for T1, and T1b reads it at Now.
func enEmitSignOuts(emit func(enCall)) {
	mfa := matureCred(cref(CredTOTP, "by-totp"))
	enForEach(false, false, func(c enCase) {
		for _, withMFA := range []bool{false, true} {
			for _, pwLogin := range []bool{false, true} {
				creds := c.creds()
				if withMFA {
					creds = append(creds, mfa)
				}
				emit(enCall{act: fmt.Sprintf("t1 mfa=%t pwlogin=%t", withMFA, pwLogin), c: c,
					in: Input{Now: t0, Act: SignOutOthers{}, Proof: c.proof(), Creds: creds, SSOLogin: !pwLogin}})
			}
		}
	})
	phishers := func() []Cred {
		return []Cred{
			enPhisher(cref(CredWebAuthn, "ph-wa"), C2), enPhisher(cref(CredRecoveryDevice, "ph-rd"), C2),
			enPhisher(cref(CredBackupAddress, "ph-addr"), C1), enPhisher(cref(CredPassword, "ph-pw"), C1),
		}
	}
	for _, shared := range []bool{false, true} {
		enForEach(false, shared, func(c enCase) {
			emit(enCall{act: "t1b", c: c,
				in: Input{Now: t0, Act: SignOutEverywhere{}, Proof: c.proof(), Creds: append(c.creds(), phishers()...)}})
		})
	}
}

// enEmitEnrolAndSelfRemove: T4 over every kind of enrolled ref and T2b over a backup address in
// four states.
func enEmitEnrolAndSelfRemove(emit func(enCall)) {
	refs := []struct {
		ref     CredRef
		inProof bool
	}{
		{cref(CredTOTP, "n-totp"), false}, {cref(CredBackupAddress, "n-bk"), false}, {cref(CredBackupAddress, "n-bk"), true},
		{cref(CredSSO, "n-sso"), true}, {cref(CredRecoveryKey, "n-rk"), false}, {cref(CredRecoveryDevice, "n-rd"), false},
		{cref(CredPassword, "n-pw"), false}, {fxSSO, false},
	}
	enForEach(false, false, func(c enCase) {
		for _, r := range refs {
			used := c.used()
			if r.inProof {
				used = append(used, r.ref)
			}
			emit(enCall{act: fmt.Sprintf("enrol %+v inProof=%t", r.ref, r.inProof), c: c,
				in: Input{Now: t0, Act: Enrol{Ref: r.ref}, Proof: Proof{Used: used, DeviceBound: gen2DeviceBound(used)}, Creds: c.creds()}})
		}
	})
	bk := cref(CredBackupAddress, "bk")
	enForEach(false, false, func(c enCase) {
		if len(c.ms) > 2 {
			return
		}
		type bkState struct {
			present bool
			state   enState
		}
		for _, bs := range []bkState{{false, enMature}, {true, enMature}, {true, enContested}, {true, enImmature}} {
			for _, inProof := range []bool{false, true} {
				creds := c.creds()
				if bs.present {
					creds = append(creds, enCred(bk, bs.state, t0))
				}
				used := c.used()
				if inProof {
					used = append(used, bk)
				}
				emit(enCall{act: fmt.Sprintf("self_remove_backup present=%t state=%s inProof=%t", bs.present, bs.state, inProof), c: c,
					in: Input{Now: t0, Act: SelfRemoveBackup{Target: bk}, Proof: Proof{Used: used, DeviceBound: gen2DeviceBound(used)}, Creds: creds}})
			}
		}
	})
}

// enEmitComplete: T11 over request kind, status, notices, the budget, holds, due-ness and the
// target fence. OpenedAt is CompletesAt-DeltaCP, so the completion exceptions of P1 apply.
func enEmitComplete(emit func(enCall)) {
	kinds := []ReqKind{ReqCredentialRemove, ReqAddressChange, ReqBreakGlass, ReqSocialRecovery, ReqCircleChange, ReqRecoveryKey}
	holds := []struct {
		name  string
		heldM int64
		since time.Time
	}{
		{"none", 0, time.Time{}},
		{"settled 1h", time.Hour.Milliseconds(), time.Time{}},
		{"open 5h", 0, t0.Add(-5 * time.Hour)},
		{"at cap", HoldCap.Milliseconds(), time.Time{}},
	}
	completes := []time.Duration{time.Hour, 0, -10 * time.Hour}
	tgt := enTgtSSO
	older := cref(CredAddress, "old-addr")
	label := 0
	for _, kind := range kinds {
		for _, status := range []ReqStatus{StatusPending, StatusDisputed} {
			for bits := 0; bits < 16; bits++ {
				accepted, settled, budgetClear, covered := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0
				for _, hold := range holds {
					for _, due := range completes {
						for _, bumped := range []bool{false, true} {
							r := Req{ID: "c1", Kind: kind, Status: status, OriginClass: C2, Origins: []CredRef{fxPW, fxTOTP},
								CompletesAt: t0.Add(due), AcceptedAddressNotice: accepted, MatureNoticesSettled: settled,
								HeldMs: hold.heldM, HeldSince: hold.since}
							r.OpenedAt = r.CompletesAt.Add(-DeltaCP)
							if kind != ReqBreakGlass {
								r.Targets = []CredRef{tgt}
							}
							if status == StatusDisputed {
								r.DisputedAt, r.DisputeGroup = t0.Add(-time.Hour), "g"
							}
							tc := credMaturing(tgt, r.OpenedAt.Add(-time.Hour))
							if bumped {
								tc.Ref.Version = 2
							}
							creds := []Cred{
								credMaturing(fxPW, r.OpenedAt.Add(-time.Hour)), credMaturing(fxAddr, r.OpenedAt.Add(-time.Hour)),
								credMaturing(fxTOTP, r.OpenedAt.Add(-time.Hour)), tc,
								credMaturing(cref(CredTOTP, "late"), r.OpenedAt.Add(time.Hour)),
							}
							creds = append(creds, enBystanders()...)
							if covered {
								creds = append(creds, credMaturing(fxWA0, r.OpenedAt.Add(-time.Hour)))
							}
							in := Input{Now: t0, Act: Complete{ID: "c1"}, Creds: creds, Reqs: []Req{r}, GuessableBudgetClear: budgetClear,
								Primaries: []Primary{
									{Ref: older, BecamePrimaryAt: t0.Add(-400 * time.Hour), MaturesAt: t0.Add(-300 * time.Hour), DisplacedAt: t0.Add(-100 * time.Hour)},
									{Ref: fxAddr, BecamePrimaryAt: t0.Add(-100 * time.Hour), MaturesAt: t0.Add(-28 * time.Hour)},
								},
								Reservations: []Reservation{{AddressRef: older, DisplacedAt: t0.Add(-100 * time.Hour), FloorUntil: t0.Add(time.Hour)}}}
							label++
							emit(enCall{act: fmt.Sprintf("complete kind=%d status=%d accepted=%t settled=%t budgetClear=%t covered=%t hold=%s due=%v bumped=%t",
								kind, status, accepted, settled, budgetClear, covered, hold.name, due, bumped), variant: label, in: in})
						}
					}
				}
			}
		}
	}
}

// enEmitExpire: the sweep around each of its five boundaries, one nanosecond either side.
func enEmitExpire(emit func(enCall)) {
	deltas := []time.Duration{-time.Nanosecond, 0, time.Nanosecond}
	for _, disputeOff := range deltas {
		for _, contestOff := range deltas {
			for _, floorOff := range deltas {
				for _, matureOff := range deltas {
					for _, openOff := range deltas {
						for _, kind := range []CredRef{fxWA0, fxPW, fxAddr} {
							cur := Primary{Ref: cref(CredAddress, "cur"), BecamePrimaryAt: t0.Add(-200 * time.Hour), MaturesAt: t0.Add(matureOff)}
							contested := matureCred(kind)
							contested.ContestedAt = t0.Add(-DeltaCP + contestOff)
							bg := enOpenReq("bg", ReqBreakGlass, StatusPending, C1, []CredRef{fxPW}, nil)
							bg.OpenedAt = cur.MaturesAt.Add(openOff)
							disputed := enOpenReq("dsp", ReqBreakGlass, StatusDisputed, C1, []CredRef{fxAddr}, nil)
							disputed.DisputedAt = t0.Add(-NCancelCP + disputeOff)
							in := Input{Now: t0, Act: Expire{}, Creds: []Cred{contested, matureCred(fxSSO)}, Primaries: []Primary{cur},
								Reqs:         []Req{bg, disputed},
								Reservations: []Reservation{{AddressRef: cref(CredAddress, "old"), DisplacedAt: t0.Add(-300 * time.Hour), FloorUntil: t0.Add(floorOff)}}}
							emit(enCall{act: fmt.Sprintf("expire dispute=%v contest=%v floor=%v mature=%v open=%v kind=%v",
								disputeOff, contestOff, floorOff, matureOff, openOff, kind.Kind), in: in})
						}
					}
				}
			}
		}
	}
}

// enProbeCompletion is P1's second half: for each distinct opened pending request, Complete at
// OpenedAt and at OpenedAt+DeltaCP-1ns changes nothing, and at OpenedAt+DeltaCP completes it
// (no holds: notices accepted, budget unlocked).
func enProbeCompletion(s *enStats, call enCall, out Outcome, seen map[string]bool) {
	for _, e := range out.Effects {
		if e.Kind != EffectOpen || e.Open.Status != StatusPending {
			continue
		}
		op := e.Open
		key := fmt.Sprintf("%d|%d|%v|%v|%t|%d", op.Kind, op.OriginClass, op.Origins, op.Targets, op.ResetFactors, len(call.in.Creds))
		if seen[key] {
			continue
		}
		seen[key] = true
		r := Req{ID: "opened", Kind: op.Kind, Status: StatusPending, OriginClass: op.OriginClass, Origins: op.Origins,
			Targets: op.Targets, OpenedAt: op.OpenedAt, CompletesAt: op.CompletesAt, AcceptedAddressNotice: true, MatureNoticesSettled: true,
			ResetFactors: op.ResetFactors}
		for _, probe := range []struct {
			after     time.Duration
			completes bool
		}{{0, false}, {DeltaCP - time.Nanosecond, false}, {DeltaCP, true}} {
			cin := Input{Now: op.OpenedAt.Add(probe.after), Act: Complete{ID: "opened"}, Creds: call.in.Creds,
				Primaries: call.in.Primaries, Reservations: call.in.Reservations, Reqs: []Req{r}, GuessableBudgetClear: true}
			cout := Decide(cin)
			s.completions++
			label := fmt.Sprintf("complete %v after opening [%s]", probe.after, call.label())
			if vs := invariantViolations(cin, cout); len(vs) > 0 {
				s.fail(label, vs...)
			}
			completed := enHas(cout, EffectComplete)
			switch {
			case probe.completes && (!completed || cout.Verdict != VerdictApplied):
				s.fail(label, fmt.Sprintf("did not complete at OpenedAt+DeltaCP: verdict %d", cout.Verdict))
			case !probe.completes && (completed || cout.Verdict != VerdictNoChange || len(cout.Effects) != 0):
				s.fail(label, fmt.Sprintf("changed something before delta_cp: verdict %d, effects %+v", cout.Verdict, cout.Effects))
			}
		}
	}
}

// TestEnumerate_P1 (parent §7.6 P1): no actor of one class completes the removal of a mature
// credential of equal or higher rank without delta_cp, and no contest names a counted
// credential. Every family below runs the global invariants too (they hold P1's predicate).
func TestEnumerate_P1(t *testing.T) {
	t.Parallel()
	st := newEnStats(t)
	seen := map[string]bool{}
	run := func(call enCall) {
		out := st.check(call)
		enProbeCompletion(st, call, out, seen)
	}
	enEmitRemove(false, run)
	enEmitOpenBG(false, run)
	enEmitCancel(false, run)
	enEmitContest(false, run)
	enEmitResolve(false, run)
	enEmitSignOuts(run)
	enEmitEnrolAndSelfRemove(run)
	enEmitComplete(run)
	enEmitExpire(run)
	t.Logf("%d enumerated calls, %d completion probes, %d distinct opened requests", st.calls, st.completions, len(seen))

	// The enumeration must actually reach every row: otherwise a family has gone vacuous.
	for _, row := range []Row{RowT1, RowT1b, RowT2, RowT2b, RowT3, RowT4, RowT5, RowT6, RowT6c, RowT7, RowT8, RowT9, RowT10, RowT11, RowT12, RowSweep} {
		assert.Positive(t, st.rows[row], "no enumerated call reached row %d", row)
	}
	for _, v := range []Verdict{VerdictRefused, VerdictApplied, VerdictOpened, VerdictDisputed, VerdictHeld, VerdictNoChange} {
		assert.Positive(t, st.verdicts[v], "no enumerated call reached verdict %d", v)
	}
	assert.Greater(t, len(seen), 30, "the opened requests probed for completion")
	assert.Greater(t, st.calls, 150000)
}

// TestEnumerate_ReferenceInstants (generator 1 alone): for each action that reads maturity at a
// reference instant, one counted element moved through the five states changes the observable
// class exactly as classOracle predicts. A mature-now element must NOT count for the back
// references (T5, T7, T12) and must count for ClassOf evaluated at Now.
func TestEnumerate_ReferenceInstants(t *testing.T) {
	type obs struct {
		row     Row
		verdict Verdict
	}
	observe := func(in Input) obs {
		o := Decide(in)
		return obs{o.Row, o.Verdict}
	}
	proofIn := func(act Action, c enCase, creds []Cred, reqs ...Req) Input {
		return Input{Now: t0, Act: act, Proof: c.proof(), Creds: creds, Reqs: reqs}
	}
	wantRemove := func(cls Class, rank Rank) obs {
		switch {
		case cls == ClassNone:
			return obs{RowNone, VerdictRefused}
		case uint8(cls) > uint8(rank):
			return obs{RowT2, VerdictApplied}
		}
		return obs{RowT3, VerdictOpened}
	}
	applied := func(row Row, ok bool) obs {
		if ok {
			return obs{row, VerdictApplied}
		}
		return obs{row, VerdictRefused}
	}
	probes := []struct {
		name    string
		refBack bool
		run     func(c enCase) (got, want []obs)
	}{
		{"T2/T3 remove a rank-1, rank-2 and rank-3 target", false, func(c enCase) (got, want []obs) {
			cls := c.oracle(nil)
			for _, tgt := range []CredRef{enTgtSSO, enTgtTOTP, enTgtRK} {
				got = append(got, observe(proofIn(Remove{Target: tgt}, c, append(c.creds(), matureCred(tgt)))))
				want = append(want, wantRemove(cls, tgt.Kind.Rank()))
			}
			return got, want
		}},
		{"T6/T6c open a break-glass", false, func(c enCase) (got, want []obs) {
			got = append(got, observe(proofIn(OpenBreakGlass{}, c, c.creds())))
			switch c.oracle(nil) {
			case ClassNone:
				want = append(want, obs{RowNone, VerdictRefused})
			case C3:
				want = append(want, obs{RowT6c, VerdictApplied})
			default:
				want = append(want, obs{RowT6, VerdictOpened})
			}
			return got, want
		}},
		{"T1b needs a counted non-paper possession factor", false, func(c enCase) (got, want []obs) {
			got = append(got, observe(proofIn(SignOutEverywhere{}, c, c.creds())))
			want = append(want, applied(RowT1b, c.npCounted() > 0))
			return got, want
		}},
		{"T7 cancel at OpenedAt = Now-2h", true, func(c enCase) (got, want []obs) {
			cls := c.oracle(nil)
			for _, oc := range []Class{C1, C2, C3} {
				r := enOpenReq("r", ReqCredentialRemove, StatusPending, oc, []CredRef{enOrigin}, []CredRef{enTgtSSO})
				got = append(got, observe(proofIn(Cancel{IDs: []string{"r"}}, c, c.creds(), r)))
				want = append(want, applied(RowT7, cls >= oc))
			}
			return got, want
		}},
		{"T5 contest at EnrolledAt = Now-2h", true, func(c enCase) (got, want []obs) {
			cls := c.oracle(nil)
			for _, x := range []Class{C1, C2, C3} {
				got = append(got, observe(proofIn(Contest{}, c, append(c.creds(), enContestTarget(cref(CredSSO, "ct-sso"), x)))))
				want = append(want, applied(RowT5, cls >= x))
			}
			return got, want
		}},
		{"T12 resolve at the earliest DisputedAt = Now-2h", true, func(c enCase) (got, want []obs) {
			got = append(got, observe(proofIn(Resolve{Group: "g", Chosen: "r1"}, c, c.creds(), enDisputedPair()...)))
			want = append(want, applied(RowT12, c.oracle(nil) == C3))
			return got, want
		}},
	}

	bases := 0
	for _, ms := range gen2Multisets() {
		if len(ms) <= 2 {
			bases++
		}
	}
	require.Equal(t, 21, bases)

	for _, p := range probes {
		cases := 0
		for _, ms := range gen2Multisets() {
			if len(ms) > 2 {
				continue
			}
			for cat := gen2Cat(0); cat < gen2NumCats; cat++ {
				for _, s := range enStates(p.refBack) {
					ms2 := append(append([]gen2Cat(nil), ms...), cat)
					g2slots, elem := gen2Layout(ms2, false)
					c := enCase{ms: ms2, refBack: p.refBack, ref: enRefAt(p.refBack), elem: elem, slots: make([]enSlot, len(g2slots))}
					for i, g := range g2slots {
						c.slots[i] = enSlot{cat: g.cat, ref: g.ref, state: enMature}
					}
					c.slots[len(c.slots)-1].state = s // the probe: the last element, moved through the states
					got, want := p.run(c)
					cases++
					if !assert.Equal(t, want, got, "%s: %s", p.name, c.label()) {
						return
					}
				}
			}
		}
		t.Logf("%s: %d cases", p.name, cases)
	}

	// ClassOf itself, over the whole of generator 1: at ref the oracle counts only mature
	// elements; at Now it also counts mature-now ones.
	n := 0
	for _, shared := range []bool{false, true} {
		n += enForEach(true, shared, func(c enCase) {
			p := c.proof()
			if got, want := ClassOf(p, c.creds(), c.ref), c.oracle(nil); got != want {
				t.Fatalf("ClassOf at ref: got %d want %d: %s", got, want, c.label())
			}
			if got, want := ClassOf(p, c.creds(), t0), c.oracleWith(enState.countsAtNow, nil); got != want {
				t.Fatalf("ClassOf at Now: got %d want %d: %s", got, want, c.label())
			}
		})
	}
	t.Logf("ClassOf checked on %d generator-1 cases", n)
}
