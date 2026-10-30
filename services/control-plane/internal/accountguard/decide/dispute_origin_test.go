package decide

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Architect ruling A-20 (plan D13): an opener is refused as a duplicate (RowNone) only when its
// origin set equals the origin set of a row it CONFLICTS with. A row that joins the opener's
// group only through the group union (ruling 12) is not compared, so a merged dispute group may
// hold several rows with one origin set. These tests pin that rule from three vantages:
//
//   - S1 to S3, three world-model sequences that each reach a group of same-origin rows by a
//     legal path (S2 is the reason the whole-group rule was rejected);
//   - two properties over every sequence of up to three steps from three start states, each
//     with positive controls that fail the property if the enumeration stops reaching the shape;
//   - four vectors in testdata/vectors.json (the t8/ union-member, duplicate-of-a-conflicting-row
//     and merge cases, and the t12/ resolve-by-id case).
//
// Mutations the suite was proven against (see the commit message):
//   - compare the opener's origin set against the whole merged group;
//   - refuse a merge that carries same-origin rows;
//   - remove D13 altogether;
//   - merge only the directly conflicting rows into the opener's group.

// fxBK is a backup address, the rank-1 channel an attacker holds beside the mailbox.
var fxBK = cref(CredBackupAddress, "bk")

// daWorld is the account of the A-20 cases: pw, the primary mailbox, a backup address, an SSO
// link and a TOTP (C5), plus any extra long-mature credentials.
func daWorld(extra ...CredRef) *world {
	creds := make([]Cred, 0, 5+len(extra))
	for _, r := range []CredRef{fxPW, fxAddr, fxBK, fxSSO, fxTOTP} {
		creds = append(creds, matureCred(r))
	}
	for _, r := range extra {
		creds = append(creds, matureCred(r))
	}
	return scWorld(creds...)
}

// daOnly returns the single effect of kind k, failing the test when there is not exactly one.
func daOnly(t *testing.T, out Outcome, k EffectKind) Effect {
	t.Helper()
	es := effectsOfKind(out, k)
	require.Len(t, es, 1, "effects of kind %d in %+v", k, out.Effects)
	return es[0]
}

// daGroup returns the origin set of every row of the dispute group, by request ID.
func daGroup(w *world, group string) map[string][]CredRef {
	out := map[string][]CredRef{}
	for _, r := range w.reqs {
		if r.Status == StatusDisputed && r.DisputeGroup == group {
			out[r.ID] = r.Origins
		}
	}
	return out
}

// S1 is the reproduction from the Phase-8 review. Row r2 joins r1's group because it conflicts
// with r1 (both remove totp). r3 then conflicts with r1 only (it removes the {bk} origin), and
// its own origin set {sso} equals r2's, a row that sits in the group it joins but that r3 does
// not conflict with. D13 compares r1 alone, so r3 is admitted into the group.
func TestT8_UnionMemberWithOpenersOriginSetJoins(t *testing.T) {
	t.Run("an opener sharing an origin set with a union member, not with a conflicting row, is disputed into the group", func(t *testing.T) {
		w := daWorld()

		out := w.decide(t, Remove{Target: fxTOTP, Candidate: scCand("bk")}, fxBK)
		scRequire(t, out, RowT3, VerdictOpened, SourceUserSession)
		w.apply(t, out)

		out = w.decide(t, Remove{Target: fxTOTP, Candidate: scCand("sso")}, fxSSO)
		scRequire(t, out, RowT8, VerdictDisputed, SourceUserSession)
		w.apply(t, out)
		require.Len(t, w.reqs, 2)
		g := w.reqs[0].DisputeGroup
		require.NotEmpty(t, g)
		require.Equal(t, g, w.reqs[1].DisputeGroup, "r1 and r2 share the group")

		out = w.decide(t, Remove{Target: fxBK, Candidate: scCand("sso-bk")}, fxSSO)
		scRequire(t, out, RowT8, VerdictDisputed, SourceUserSession)
		open := daOnly(t, out, EffectOpen)
		require.Len(t, out.Effects, 1, "r1 and r2 are already in g: the only effect is the open")
		assert.Equal(t, g, open.Group)
		assert.Equal(t, StatusDisputed, open.Open.Status)
		assert.Equal(t, []CredRef{fxSSO}, open.Open.Origins)
		assert.Equal(t, []CredRef{fxBK}, open.Open.Targets)
		require.Len(t, out.Notices, 1)
		assert.Equal(t, NoticeChangeDisputed, out.Notices[0].Kind)
		w.apply(t, out)

		assert.Equal(t, map[string][]CredRef{
			"r1": {fxBK}, "r2": {fxSSO}, "r3": {fxSSO},
		}, daGroup(w, g), "the group holds two rows with the origin set {sso}")
	})
}

// S2 is why the whole-group rule was rejected. An attacker's dispute of the owner's removal pulls
// the owner's own counter-move into the group the owner is already in; comparing against the
// whole group would refuse the owner's counter-move for having the owner's own origin set.
func TestT8_OwnersCounterMoveJoinsAGroupHoldingItsOwnRow(t *testing.T) {
	t.Run("the owner's second request joins the group that already holds the owner's first", func(t *testing.T) {
		w := daWorld(fxWA1)

		out := w.decide(t, Remove{Target: fxWA1, Candidate: scCand("owner-wa1")}, fxPW)
		scRequire(t, out, RowT3, VerdictOpened, SourceUserSession)
		w.apply(t, out)

		out = w.decide(t, Remove{Target: fxWA1, Candidate: scCand("attacker-wa1")}, fxBK)
		scRequire(t, out, RowT8, VerdictDisputed, SourceUserSession)
		w.apply(t, out)
		require.Len(t, w.reqs, 2)
		g := w.reqs[0].DisputeGroup
		require.NotEmpty(t, g)
		require.Equal(t, g, w.reqs[1].DisputeGroup)

		out = w.decide(t, Remove{Target: fxBK, Candidate: scCand("owner-bk")}, fxPW)
		scRequire(t, out, RowT8, VerdictDisputed, SourceUserSession)
		require.Len(t, out.Effects, 1, "the owner's first row is already in g: the only effect is the open")
		assert.Equal(t, g, daOnly(t, out, EffectOpen).Group)
		w.apply(t, out)

		assert.Equal(t, map[string][]CredRef{
			"r1": {fxPW}, "r2": {fxBK}, "r3": {fxPW},
		}, daGroup(w, g), "r1 and r3 carry the owner's origin set {pw}")
	})
}

// S3 shows the rule is what lets an owner break a one-credential attacker's blocking: the
// attacker's two requests each end alone in a group of their own, the owner's break-glass
// conflicts with both and merges them, and the group then holds two rows with one origin set.
// A rule that refused that merge would leave the break-glass undisputable and the attacker's
// rows standing.
func TestT8_OneCredentialAttackerCannotBlockTheBreakGlass(t *testing.T) {
	t.Run("the break-glass merges the attacker's two same-origin groups and T12 resolves them", func(t *testing.T) {
		w := daWorld(fxWA1, fxRK)
		candA, candB := scCand("attacker-totp"), scCand("attacker-wa1")

		// Round A: the attacker asks to remove totp; the owner cancels; the reopen is disputed (T10).
		out := w.decide(t, Remove{Target: fxTOTP, Candidate: candA}, fxSSO)
		scRequire(t, out, RowT3, VerdictOpened, SourceUserSession)
		w.apply(t, out)
		out = w.decide(t, Cancel{IDs: []string{"r1"}}, fxPW)
		scRequire(t, out, RowT7, VerdictApplied, SourceUserSession)
		assert.Equal(t, C1, daOnly(t, out, EffectCancel).CancellerClass)
		w.apply(t, out)
		out = w.decide(t, Remove{Target: fxTOTP, Candidate: candA}, fxSSO)
		scRequire(t, out, RowT10, VerdictDisputed, SourceUserSession)
		w.apply(t, out)

		// Round B: the same for wa1; r2 targets totp, so r3 does not conflict with it.
		out = w.decide(t, Remove{Target: fxWA1, Candidate: candB}, fxSSO)
		scRequire(t, out, RowT3, VerdictOpened, SourceUserSession)
		w.apply(t, out)
		out = w.decide(t, Cancel{IDs: []string{"r3"}}, fxPW)
		scRequire(t, out, RowT7, VerdictApplied, SourceUserSession)
		w.apply(t, out)
		out = w.decide(t, Remove{Target: fxWA1, Candidate: candB}, fxSSO)
		scRequire(t, out, RowT10, VerdictDisputed, SourceUserSession)
		w.apply(t, out)

		g1, g2 := w.reqPtr("r2").DisputeGroup, w.reqPtr("r4").DisputeGroup
		require.NotEmpty(t, g1)
		require.NotEmpty(t, g2)
		require.NotEqual(t, g1, g2, "each reopen opened a group of its own")
		require.Len(t, daGroup(w, g1), 1)
		require.Len(t, daGroup(w, g2), 1)

		// The owner's break-glass conflicts with both rows and merges them into the smaller group.
		out = w.decide(t, OpenBreakGlass{Candidate: scCand("owner")}, fxPW, fxTOTP)
		scRequire(t, out, RowT8, VerdictDisputed, SourceBreakGlass)
		assert.Equal(t, g1, daOnly(t, out, EffectOpen).Group)
		assert.Equal(t, "r4", daOnly(t, out, EffectDispute).RequestID)
		assert.Equal(t, g1, daOnly(t, out, EffectDispute).Group)
		require.Len(t, out.Effects, 2, "r2 is already in g1")
		w.apply(t, out)
		assert.Equal(t, map[string][]CredRef{
			"r2": {fxSSO}, "r4": {fxSSO}, "r5": {fxPW, fxTOTP},
		}, daGroup(w, g1), "one group holds both {sso} rows and the break-glass")

		// T12 with the recovery key and the totp: the break-glass is the chosen row.
		out = w.decide(t, Resolve{Group: g1, Chosen: "r5"}, fxRK, fxTOTP)
		scRequire(t, out, RowT12, VerdictApplied, SourceUserSession)
		cancels := effectsOfKind(out, EffectCancel)
		require.Len(t, cancels, 2)
		assert.ElementsMatch(t, []string{"r2", "r4"}, []string{cancels[0].RequestID, cancels[1].RequestID})
		for _, c := range cancels {
			assert.Equal(t, CancelDisputeResolved, c.Reason)
			assert.Equal(t, C3, c.CancellerClass)
		}
		assert.Equal(t, "r5", daOnly(t, out, EffectReturnToPending).RequestID)
		w.apply(t, out)
		require.Equal(t, StatusPending, w.reqPtr("r5").Status)

		// The attacker's reopen is refused for good (T9): a dispute_resolved cancel is final.
		out = w.decide(t, Remove{Target: fxTOTP, Candidate: candA}, fxSSO)
		scRequire(t, out, RowT9, VerdictRefused, SourceUserSession)
		assert.Empty(t, out.Effects)
	})
}

// ---- properties -----------------------------------------------------------------------------

// daStep is one action the properties enumerate, with the proof it is made under.
type daStep struct {
	name  string
	act   Action
	proof []CredRef
}

// daSteps are the world's moves: removal of each of totp, wa1, bk and sso, and a break-glass,
// each under four proofs, plus a cancel of each of r1 to r4 by the password. A step naming a
// row the world does not hold is a legal no-op (RowNone), so the same steps serve every state.
//
// Resolve is cut from the step set (the specification allows it): a T12 only removes rows, so
// it adds no admission and no group, and its fan-out would multiply the three-step depth by
// (24+k)/24 cubed.
func daSteps() []daStep {
	proofs := map[string][]CredRef{"bk": {fxBK}, "sso": {fxSSO}, "pw": {fxPW}, "pw+totp": {fxPW, fxTOTP}}
	order := []string{"bk", "sso", "pw", "pw+totp"}
	steps := make([]daStep, 0, 24)
	for _, tgt := range []CredRef{fxTOTP, fxWA1, fxBK, fxSSO} {
		for _, p := range order {
			steps = append(steps, daStep{
				name:  fmt.Sprintf("remove %v/%s by %s", tgt.Kind, tgt.ID, p),
				act:   Remove{Target: tgt},
				proof: proofs[p],
			})
		}
	}
	for _, p := range order {
		steps = append(steps, daStep{name: "open_break_glass by " + p, act: OpenBreakGlass{}, proof: proofs[p]})
	}
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		steps = append(steps, daStep{name: "cancel " + id + " by pw", act: Cancel{IDs: []string{id}}, proof: []CredRef{fxPW}})
	}
	return steps
}

// daClone copies a world so a branch of the enumeration cannot reach another's state. The rows
// are copied by value; their ref slices are never written in place, by apply or by anything else.
func daClone(w *world) *world {
	c := *w
	c.creds = append([]Cred(nil), w.creds...)
	c.primaries = append([]Primary(nil), w.primaries...)
	c.reservations = append([]Reservation(nil), w.reservations...)
	c.reqs = append([]Req(nil), w.reqs...)
	c.recent = append([]Req(nil), w.recent...)
	return &c
}

// daStarts are the three start states: the empty account, the first two steps of S1 (a
// conflicting pair in one group), and a four-step setup that leaves two groups of two rows:
// {bk} and {sso} both remove totp (group one), then {pw} and {bk} both remove wa1 (group two).
func daStarts(t *testing.T) map[string]*world {
	t.Helper()
	empty := daWorld(fxWA1, fxRK)

	pair := daWorld(fxWA1, fxRK)
	for _, s := range []struct {
		act  Action
		used CredRef
		row  Row
	}{
		{Remove{Target: fxTOTP}, fxBK, RowT3},
		{Remove{Target: fxTOTP}, fxSSO, RowT8},
	} {
		out := pair.decide(t, s.act, s.used)
		require.Equal(t, s.row, out.Row)
		pair.apply(t, out)
	}

	two := daWorld(fxWA1, fxRK)
	for _, s := range []struct {
		act  Action
		used CredRef
		row  Row
	}{
		{Remove{Target: fxTOTP}, fxBK, RowT3},
		{Remove{Target: fxTOTP}, fxSSO, RowT8},
		{Remove{Target: fxWA1}, fxPW, RowT3},
		{Remove{Target: fxWA1}, fxBK, RowT8},
	} {
		out := two.decide(t, s.act, s.used)
		require.Equal(t, s.row, out.Row)
		two.apply(t, out)
	}
	require.Len(t, two.reqs, 4)
	require.NotEqual(t, two.reqs[0].DisputeGroup, two.reqs[2].DisputeGroup, "two groups")

	return map[string]*world{"empty": empty, "conflicting pair": pair, "two groups": two}
}

// daConflict is T8's conflict test, written out independently of conflictingRows: either row is
// a break-glass, their targets intersect, or one's target is in the other's origin set.
func daConflict(a, b Req) bool {
	if a.Kind == ReqBreakGlass || b.Kind == ReqBreakGlass {
		return true
	}
	return daShares(a.Targets, b.Targets) || daShares(a.Targets, b.Origins) || daShares(b.Targets, a.Origins)
}

func daShares(a, b []CredRef) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// daSameSet reports whether two origin sets hold the same refs.
func daSameSet(a, b []CredRef) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !daShares([]CredRef{x}, b) {
			return false
		}
	}
	return true
}

// daWalk visits start and every state reachable from it in up to depth steps. visit is called
// for each state with the step that reached it (nil for start); admit is called for each step
// taken, with the snapshot Decide saw and its outcome.
func daWalk(t *testing.T, w *world, depth int, steps []daStep, visit func(*world), admit func(Input, Outcome)) {
	t.Helper()
	visit(w)
	if depth == 0 {
		return
	}
	for _, s := range steps {
		next := daClone(w)
		in := next.input(s.act, s.proof...)
		out := next.decide(t, s.act, s.proof...)
		admit(in, out)
		next.apply(t, out)
		daWalk(t, next, depth-1, steps, visit, admit)
	}
}

// daAdmission reports whether out is a T8 or T10 dispute whose opener joins an existing group
// holding a row that has the opener's origin set and that the opener does not conflict with.
func daAdmission(in Input, out Outcome) bool {
	if out.Verdict != VerdictDisputed {
		return false
	}
	var open Effect
	for _, e := range out.Effects {
		if e.Kind == EffectOpen {
			open = e
		}
	}
	if open.Group == "" {
		return false
	}
	opener := Req{Kind: open.Open.Kind, Origins: open.Open.Origins, Targets: open.Open.Targets}
	for _, r := range in.Reqs {
		if isOpenGuardRow(r) && r.Status == StatusDisputed && r.DisputeGroup == open.Group &&
			daSameSet(r.Origins, opener.Origins) && !daConflict(opener, r) {
			return true
		}
	}
	return false
}

// daOpenRows are the world's pending and disputed guard rows.
func daOpenRows(w *world) []Req {
	var rows []Req
	for _, r := range w.reqs {
		if isOpenGuardRow(r) {
			rows = append(rows, r)
		}
	}
	return rows
}

// daEnumerate is the enumeration both properties share: every sequence of up to three steps
// from each start state. check is called on each reached state's open rows.
type daStats struct {
	states, equalConflicting, conflictPairs int
	sameOriginInGroup, admissions           int
}

func daEnumerate(t *testing.T, check func(rows []Req, stats *daStats)) daStats {
	t.Helper()
	steps := daSteps()
	require.Len(t, steps, 24, "4 removals x 4 proofs, 4 break-glasses, 4 cancels")
	var stats daStats
	for name, start := range daStarts(t) {
		before := stats.states
		daWalk(t, start, 3, steps,
			func(w *world) {
				stats.states++
				rows := daOpenRows(w)
				for i := range rows {
					for j := i + 1; j < len(rows); j++ {
						a, b := rows[i], rows[j]
						if daSameSet(a.Origins, b.Origins) && a.Status == StatusDisputed && b.Status == StatusDisputed &&
							a.DisputeGroup != "" && a.DisputeGroup == b.DisputeGroup {
							stats.sameOriginInGroup++
							break
						}
					}
				}
				check(rows, &stats)
			},
			func(in Input, out Outcome) {
				if daAdmission(in, out) {
					stats.admissions++
				}
			})
		n := len(steps)
		require.Equal(t, 1+n+n*n+n*n*n, stats.states-before, "start %q: every state to depth 3 is visited", name)
	}
	return stats
}

// daReport fails the test with the first few violations; a flood of identical lines
// would bury the first.
func daReport(t *testing.T, violations []string) {
	t.Helper()
	if len(violations) > 5 {
		violations = violations[:5]
	}
	assert.Empty(t, violations)
}

// Property one: no two open rows with one origin set conflict. D13 refuses the second opener at
// the door; A-20 narrows what it compares, and must not let a conflicting equal-origin pair in.
func TestProperty_EqualOriginOpenRowsNeverConflict(t *testing.T) {
	var violations []string
	stats := daEnumerate(t, func(rows []Req, stats *daStats) {
		for i := range rows {
			for j := i + 1; j < len(rows); j++ {
				if !daSameSet(rows[i].Origins, rows[j].Origins) {
					continue
				}
				stats.equalConflicting++
				if daConflict(rows[i], rows[j]) {
					violations = append(violations, fmt.Sprintf("%s %v and %s %v conflict with one origin set",
						rows[i].ID, rows[i].Origins, rows[j].ID, rows[j].Origins))
				}
			}
		}
	})
	daReport(t, violations)

	t.Logf("states=%d with-same-origin-rows-in-a-group=%d admissions=%d equal-origin-pairs=%d",
		stats.states, stats.sameOriginInGroup, stats.admissions, stats.equalConflicting)
	// Positive controls: the enumeration reaches the shapes A-20 is about. Without them a
	// walk that never produced a same-origin group would pass for the wrong reason.
	assert.Positive(t, stats.sameOriginInGroup, "no state holds two rows of one origin set in one group")
	assert.Positive(t, stats.admissions, "no T8 admits an opener that shares its origin set with a group member it does not conflict with")
	assert.Positive(t, stats.equalConflicting, "no state holds two open rows of one origin set")
}

// Property two: any two open rows that conflict are both disputed, in one group (ruling 12).
func TestProperty_ConflictingOpenRowsShareOneGroup(t *testing.T) {
	var violations []string
	stats := daEnumerate(t, func(rows []Req, stats *daStats) {
		for i := range rows {
			for j := i + 1; j < len(rows); j++ {
				if !daConflict(rows[i], rows[j]) {
					continue
				}
				stats.conflictPairs++
				a, b := rows[i], rows[j]
				if a.Status != StatusDisputed || b.Status != StatusDisputed || a.DisputeGroup == "" || a.DisputeGroup != b.DisputeGroup {
					violations = append(violations, fmt.Sprintf("%s (%v, group %q) and %s (%v, group %q) conflict outside one group",
						a.ID, a.Status, a.DisputeGroup, b.ID, b.Status, b.DisputeGroup))
				}
			}
		}
	})
	daReport(t, violations)

	t.Logf("states=%d conflicting-pairs=%d with-same-origin-rows-in-a-group=%d admissions=%d",
		stats.states, stats.conflictPairs, stats.sameOriginInGroup, stats.admissions)
	assert.Positive(t, stats.conflictPairs, "no state holds two conflicting open rows")
	assert.Positive(t, stats.sameOriginInGroup, "no state holds two rows of one origin set in one group")
	assert.Positive(t, stats.admissions, "no T8 admits an opener that shares its origin set with a group member it does not conflict with")
}
