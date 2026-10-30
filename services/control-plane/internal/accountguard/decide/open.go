package decide

import (
	"bytes"
	"sort"
	"time"
)

// Opening requests: T3, T6, T6c, T8, T9 and T10 (parent §6.2, §6.3).
//
// The checks run in a fixed order (plan D14, architect ruling A1): T9 first (a refused
// reopen), then the duplicate-origin refusal (D13), then T10 and T8 together (a disputed
// opening), and only then the split between a pending opening (T3, T6) and a break-glass
// completed at once (T6c). A C3 break-glass that meets T10 or T8 is disputed, never
// completed.

// opener is what an opening action asks for.
type opener struct {
	kind    ReqKind
	targets []CredRef
	class   Class
	cand    Candidate
	reset   bool
}

// decideOpenBreakGlass is T6 (C1 or C2) or T6c (C3). A proof of ClassNone has no row.
func decideOpenBreakGlass(in Input, act OpenBreakGlass) Outcome {
	c := ClassOf(in.Proof, in.Creds, in.Now)
	if c == ClassNone {
		return refuse(RowNone)
	}
	return openRequest(in, opener{kind: ReqBreakGlass, class: c, cand: act.Candidate, reset: act.Reset})
}

// openRequest decides an opening of class op.class.
func openRequest(in Input, op opener) Outcome {
	refused, reopen := reopenCheck(in, op.cand, op.class)
	if refused {
		return refuse(RowT9)
	}
	origins := countedOrigins(in)
	conflicts := conflictingRows(in, op, origins)
	for _, r := range conflicts {
		if sameRefSet(sortRefs(r.Origins), origins) {
			// D13: an opener whose origin set equals a conflicting row's is a duplicate, and
			// a duplicate never disputes itself.
			return refuse(RowNone)
		}
	}
	opening := Opening{
		Kind: op.kind, Status: StatusPending, Targets: sortRefs(op.targets), Origins: origins,
		OriginClass: op.class, ResetFactors: op.reset,
		OpenedAt: in.Now, CompletesAt: in.Now.Add(DeltaCP), Candidate: copyCandidate(op.cand),
	}
	switch {
	case reopen || len(conflicts) > 0:
		return disputedOpening(in, opening, conflicts, reopen)
	case op.kind == ReqBreakGlass && op.class == C3:
		return completedOpening(in, opening)
	}
	row := RowT3
	if op.kind == ReqBreakGlass {
		row = RowT6
	}
	return Outcome{
		Row: row, Verdict: VerdictOpened,
		Effects: []Effect{{Kind: EffectOpen, Open: opening}},
		Notices: []Notice{requestNotice(in, NoticeChangeRequested, "")},
	}
}

// countedOrigins is the opening's origin set: the distinct proof refs that name a
// credential exactly and count now (architect ruling A2), in canonical order (ruling 6). An
// immature ref therefore cannot turn a duplicate into a dispute (P4).
func countedOrigins(in Input) []CredRef {
	var refs []CredRef
	for _, r := range in.Proof.Used {
		if c, ok := findCred(in.Creds, r); ok && counts(c, in.Now) {
			refs = append(refs, r)
		}
	}
	return sortRefs(refs)
}

// reopenCheck applies T9 and T10 against every recent proof or dispute_resolved cancel of a
// guard kind within NCancelCP whose fingerprint matches. A match refuses (T9) when the
// cancel was T12's or its canceller outranked c, and otherwise disputes (T10). A hold_cap or
// expiry cancel never matches (mutation #7).
func reopenCheck(in Input, cand Candidate, c Class) (refused, reopen bool) {
	for _, r := range in.RecentCancels {
		if !reopenWindow(in.Now, r) || !fingerprintMatch(in, r, cand) {
			continue
		}
		if r.CancelReason == CancelDisputeResolved || r.CancellerClass > c {
			refused = true
		} else {
			reopen = true
		}
	}
	return refused, reopen
}

// reopenWindow reports whether r is a cancel T9 and T10 read: a proof or dispute_resolved
// cancel of a kind other than recovery_key, less than NCancelCP old. A cancel exactly
// NCancelCP old is outside the window. A zero CancelledAt is inside it (H1): RecentCancels
// holds only cancels younger than NCancelCP, so a dropped timestamp must not exempt the
// row, and a match refuses or disputes, the protective side.
func reopenWindow(now time.Time, r Req) bool {
	if r.CancelReason != CancelProof && r.CancelReason != CancelDisputeResolved {
		return false
	}
	return r.Kind != ReqRecoveryKey && (r.CancelledAt.IsZero() || now.Sub(r.CancelledAt) < NCancelCP)
}

// fingerprintMatch applies plan D19 and architect ruling 2. The keyed fingerprints are
// compared only when the cancel's key id, the candidate's key id and the current key id are
// all equal and both keyed values are present; otherwise the coarse fingerprints decide,
// which matches more, never less (LEAD-D 11-A). An empty fingerprint never matches, so a
// redacted cancel and an unfilled candidate are not equal (Review Focus 2).
func fingerprintMatch(in Input, r Req, cand Candidate) bool {
	if r.FingerprintKeyID == cand.KeyID && cand.KeyID == in.CurrentKeyID && len(r.Fingerprint) > 0 && len(cand.Keyed) > 0 {
		return bytes.Equal(r.Fingerprint, cand.Keyed)
	}
	return len(r.Coarse) > 0 && len(cand.Coarse) > 0 && bytes.Equal(r.Coarse, cand.Coarse)
}

// conflictingRows returns, by ID, the open guard rows the opening conflicts with (T8): either
// is a break-glass, their targets intersect, or one's target is in the other's origin set
// (all compared exactly).
func conflictingRows(in Input, op opener, origins []CredRef) []Req {
	var out []Req
	for _, r := range in.Reqs {
		if isOpenGuardRow(r) && conflicts(op, origins, r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func conflicts(op opener, origins []CredRef, r Req) bool {
	if op.kind == ReqBreakGlass || r.Kind == ReqBreakGlass {
		return true
	}
	return anyShared(op.targets, r.Targets) || anyShared(op.targets, r.Origins) || anyShared(r.Targets, origins)
}

// anyShared reports whether a and b hold a common ref exactly.
func anyShared(a, b []CredRef) bool {
	for _, x := range a {
		if containsRef(b, x) {
			return true
		}
	}
	return false
}

// sameRefSet compares two canonical (sorted, de-duplicated) ref lists.
func sameRefSet(a, b []CredRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameCred(a[i], b[i]) {
			return false
		}
	}
	return true
}

// disputedOpening is T8 or T10: the opening and every conflicting row become disputed under
// one group (architect rulings 9, 12 and A1).
//
// The group g is the smallest group already held by a conflicting row, or "" for a new
// one. Every member of every group a conflicting row holds moves to g with them (group
// union), so a dispute never splits a group. Only a row whose status or group changes gets
// an EffectDispute. The opener and each pending row that moves to disputed get one
// change_disputed; a row already disputed that only changes group gets none. A pending row
// with an open hold leaving pending also folds it (ruling 1's corollary).
func disputedOpening(in Input, opening Opening, conflicting []Req, reopen bool) Outcome {
	g := smallestGroup(conflicting)
	opening.Status = StatusDisputed
	opening.DisputedAt = in.Now
	effects := []Effect{{Kind: EffectOpen, Open: opening, Group: g}}
	notices := []Notice{requestNotice(in, NoticeChangeDisputed, "")}
	for _, r := range regroupRows(in, conflicting, g) {
		effects = append(effects, Effect{Kind: EffectDispute, RequestID: r.ID, Group: g})
		if r.Status == StatusPending {
			notices = append(notices, requestNotice(in, NoticeChangeDisputed, r.ID))
			effects = appendFold(in, effects, r)
		}
	}
	row := RowT8
	if reopen {
		row = RowT10
	}
	return Outcome{Row: row, Verdict: VerdictDisputed, Effects: effects, Notices: notices}
}

// smallestGroup returns the byte-wise smallest dispute group held by a disputed row, or "".
func smallestGroup(rows []Req) string {
	g := ""
	for _, r := range rows {
		if r.Status == StatusDisputed && r.DisputeGroup != "" && (g == "" || r.DisputeGroup < g) {
			g = r.DisputeGroup
		}
	}
	return g
}

// regroupRows returns, by ID, the rows a disputed opening moves into group g: the
// conflicting rows and every disputed member of a group a conflicting row holds, less the
// rows already disputed in g.
func regroupRows(in Input, conflicting []Req, g string) []Req {
	touched := map[string]bool{}
	direct := map[string]bool{}
	for _, r := range conflicting {
		direct[r.ID] = true
		if r.Status == StatusDisputed && r.DisputeGroup != "" {
			touched[r.DisputeGroup] = true
		}
	}
	out := make([]Req, 0, len(in.Reqs))
	for _, r := range in.Reqs {
		member := r.Status == StatusDisputed && touched[r.DisputeGroup]
		switch {
		case !isOpenGuardRow(r), !member && !direct[r.ID]:
			continue
		case r.Status == StatusDisputed && g != "" && r.DisputeGroup == g:
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// completedOpening is T6c: a C3 break-glass with nothing in its way completes at once
// (parent T6c: "Nothing waits"). The row is written completed with OpenedAt, CompletesAt and
// CompletedAt all Now (architect ruling 5) and no fingerprint (ruling 15a), plus T11's
// break-glass effects measured at Now. The new password is enrolled at C3 (F2).
func completedOpening(in Input, opening Opening) Outcome {
	opening.Status = StatusCompleted
	opening.CompletesAt = in.Now
	opening.CompletedAt = in.Now
	opening.Candidate = Candidate{}
	effects := append([]Effect{{Kind: EffectOpen, Open: opening}}, breakGlassEffects(in, in.Now, opening.Origins, opening.ResetFactors)...)
	return Outcome{
		Row: RowT6c, Verdict: VerdictApplied, Effects: effects, EnrolClass: C3,
		RotateEpoch: true, SetsPassword: true, Notices: breakGlassNotices(in, "", effects),
	}
}

// breakGlassEffects are a break-glass completion's credential effects (parent T6 and T11;
// plan D16), for T6c and T11 alike:
//   - remove every credential that did not count at openedAt, enrolled after it included;
//   - when reset, also remove every TOTP, WebAuthn and backup-code set whose slot is not in
//     the origin set, so a factor that proved the break-glass is never reset by it (#35);
//   - never remove the password (the pending hash replaces it) or the primary address
//     (only restored), and never reset an anchor;
//   - restore the primary from restoreTarget (#21).
func breakGlassEffects(in Input, openedAt time.Time, origins []CredRef, reset bool) []Effect {
	removed := credRefsWhere(in.Creds, func(c Cred) bool { return breakGlassRemoves(c, openedAt, origins, reset) })
	effects := make([]Effect, 0, len(removed)+1)
	for _, r := range removed {
		effects = append(effects, Effect{Kind: EffectRemove, Cred: r})
	}
	if t, ok := restoreTarget(in, openedAt); ok {
		effects = append(effects, Effect{Kind: EffectRestorePrimary, Cred: t})
	}
	return effects
}

func breakGlassRemoves(c Cred, openedAt time.Time, origins []CredRef, reset bool) bool {
	k := c.Ref.Kind
	if k.Rank() == RankInvalid || k == CredPassword || k == CredAddress {
		return false
	}
	if !counts(c, openedAt) {
		return true
	}
	resettable := k == CredTOTP || k == CredWebAuthn || k == CredBackupCodeSet
	return reset && resettable && !containsSlot(origins, c.Ref)
}

// breakGlassNotices are a break-glass completion's notices: change_completed,
// password_changed, and one credential_removed per removal (architect ruling 3), each naming
// the request ("" for T6c's own row) and every reserved primary.
func breakGlassNotices(in Input, requestID string, effects []Effect) []Notice {
	return append(completionNotices(in, requestID, effects), requestNotice(in, NoticePasswordChanged, requestID))
}

// completionNotices are change_completed plus one credential_removed per EffectRemove.
func completionNotices(in Input, requestID string, effects []Effect) []Notice {
	notices := []Notice{requestNotice(in, NoticeChangeCompleted, requestID)}
	for _, e := range effects {
		if e.Kind == EffectRemove {
			notices = append(notices, removalNotice(in, requestID, e.Cred))
		}
	}
	return notices
}

// copyCandidate copies a candidate's fingerprints, so the outcome shares no memory with
// the input.
func copyCandidate(c Candidate) Candidate {
	return Candidate{Keyed: copyBytes(c.Keyed), Coarse: copyBytes(c.Coarse), KeyID: c.KeyID}
}

func copyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}
