package decide

import (
	"sort"
	"time"
)

// Cancel, resolve and the sweep: T7, T12, dispute expiry, reservation release and the
// deletion of contested credentials (parent §6.2; child spec §4 input 1; architect ruling
// A3).

// decideCancel is T7, per named request and independently. A request is cancellable only
// while pending or disputed and of a guard kind. Unknown IDs are ignored; when no ID names a
// cancellable request the table has no row (RowNone), and when the proof cancels none of
// them T7 refused (architect ruling 8).
func decideCancel(in Input, act Cancel) Outcome {
	o := Outcome{Row: RowT7, Verdict: VerdictApplied}
	evaluated := false
	for _, id := range sortedUniqueIDs(act.IDs) {
		r, ok := findReq(in.Reqs, id)
		if !ok || !isOpenGuardRow(r) {
			continue
		}
		evaluated = true
		if cancel, ok := cancelOne(in, r); ok {
			o.Effects = appendFold(in, append(o.Effects, cancel), r)
			o.Notices = append(o.Notices, requestNotice(in, NoticeChangeCancelled, r.ID))
		}
	}
	switch {
	case !evaluated:
		return refuse(RowNone)
	case len(o.Effects) == 0:
		return refuse(RowT7)
	}
	return o
}

// cancelOne is T7's class rule: the proof, less the addresses and SSO links r removes (the K
// self-veto, #2), must reach r's origin class, and at least C1, counting only credentials
// mature before r opened (#3). The password is never excluded: it may cancel a break-glass
// that would replace it.
func cancelOne(in Input, r Req) (Effect, bool) {
	c := ClassOf(selfVetoFiltered(in.Proof, r), in.Creds, r.OpenedAt)
	if c < C1 || c < r.OriginClass {
		return Effect{}, false
	}
	return Effect{Kind: EffectCancel, RequestID: r.ID, Reason: CancelProof, CancellerClass: c}, true
}

// selfVetoFiltered drops from p every address, backup address and SSO ref whose slot r
// targets, and every email-MFA ref when r targets the primary address (F4): in C4 every
// email-MFA code is delivered to the primary (child spec H2), so it proves that mailbox, which
// may not veto its own replacement (parent §6.1, §4). A break-glass's targets are never read
// (architect ruling 4). DeviceBound is kept: it names assertions, and ClassOf reads it only
// for a ref still in Used.
func selfVetoFiltered(p Proof, r Req) Proof {
	if r.Kind == ReqBreakGlass {
		return p
	}
	primaryTargeted := targetsKind(r.Targets, CredAddress)
	used := make([]CredRef, 0, len(p.Used))
	for _, x := range p.Used {
		if selfVetoed(x, r.Targets, primaryTargeted) {
			continue
		}
		used = append(used, x)
	}
	return Proof{Used: used, DeviceBound: p.DeviceBound}
}

// selfVetoed reports whether proof ref x proves a channel the request removes: an address,
// backup address or SSO link in a target slot, or an email-MFA code while the primary is a
// target.
func selfVetoed(x CredRef, targets []CredRef, primaryTargeted bool) bool {
	switch x.Kind {
	case CredAddress, CredBackupAddress, CredSSO:
		return containsSlot(targets, x)
	case CredEmailMFA:
		return primaryTargeted
	default:
		return false
	}
}

// targetsKind reports whether any target is of kind k.
func targetsKind(targets []CredRef, k CredKind) bool {
	for _, t := range targets {
		if t.Kind == k {
			return true
		}
	}
	return false
}

// sortedUniqueIDs returns the IDs sorted, each once.
func sortedUniqueIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	n := 0
	for i := range out {
		if n > 0 && out[i] == out[n-1] {
			continue
		}
		out[n] = out[i]
		n++
	}
	return out[:n]
}

// decideResolve is T12. The group's members are its disputed rows; a group with none, with
// a member that is not a guard kind (recovery_key above all), or a Chosen that is neither ""
// nor a member has no row. The proof must reach C3 counting only credentials mature before
// the group's earliest DisputedAt (D22). The chosen row returns to pending, never completes
// (#32), and every other member is cancelled as dispute_resolved at C3, which T9 refuses to
// reopen (#15). T12 never rotates: the chosen row's T11 does (architect ruling 10).
func decideResolve(in Input, act Resolve) Outcome {
	members, ok := groupMembers(in, act.Group)
	if !ok || (act.Chosen != "" && !hasReqID(members, act.Chosen)) {
		return refuse(RowNone)
	}
	if ClassOf(in.Proof, in.Creds, earliestDisputedAt(members)) < C3 {
		return refuse(RowT12)
	}
	o := Outcome{Row: RowT12, Verdict: VerdictApplied}
	for _, r := range members {
		if r.ID == act.Chosen {
			o.Effects = append(o.Effects, Effect{Kind: EffectReturnToPending, RequestID: r.ID})
			continue
		}
		o.Effects = append(o.Effects, Effect{Kind: EffectCancel, RequestID: r.ID, Reason: CancelDisputeResolved, CancellerClass: C3})
		o.Notices = append(o.Notices, requestNotice(in, NoticeChangeCancelled, r.ID))
	}
	return o
}

// groupMembers returns the disputed rows of group, or false when the group is empty, has no
// member, or has a member T12 may not resolve.
func groupMembers(in Input, group string) ([]Req, bool) {
	if group == "" {
		return nil, false
	}
	var members []Req
	for _, r := range in.Reqs {
		if r.Status != StatusDisputed || r.DisputeGroup != group {
			continue
		}
		if !isGuardKind(r.Kind) {
			return nil, false
		}
		members = append(members, r)
	}
	return members, len(members) > 0
}

func hasReqID(reqs []Req, id string) bool {
	_, ok := findReq(reqs, id)
	return ok
}

// earliestDisputedAt is the group's reference instant (D22). A member with no DisputedAt
// makes it the zero time, at which nothing counts, so T12 fails closed.
func earliestDisputedAt(members []Req) time.Time {
	earliest := members[0].DisputedAt
	for _, r := range members[1:] {
		if r.DisputedAt.Before(earliest) {
			earliest = r.DisputedAt
		}
	}
	return earliest
}

// decideExpire is the sweeper's pass (RowSweep): it expires disputes, releases reservations
// and deletes contested credentials past Δ_cp. It is NoChange, with no effects, when
// nothing applies (architect ruling 15b, 15c).
func decideExpire(in Input) Outcome {
	o := Outcome{Row: RowSweep, Verdict: VerdictApplied}
	o.Effects, o.Notices = expiredDisputes(in)
	for _, ref := range releasedReservations(in) {
		o.Effects = append(o.Effects, Effect{Kind: EffectReleaseReservation, Cred: ref})
	}
	for _, ref := range deletableContested(in) {
		o.Effects = append(o.Effects, Effect{Kind: EffectRemove, Cred: ref})
		o.Notices = append(o.Notices, Notice{Kind: NoticeCredentialRemoved, Subject: ref})
	}
	if len(o.Effects) == 0 {
		return Outcome{Row: RowSweep, Verdict: VerdictNoChange}
	}
	return o
}

// expiredDisputes cancels, with no change, every disputed guard row whose DisputedAt is at
// least DisputeExpiry old (parent T12). CancellerClass stays ClassNone (architect ruling
// 13). A pending row is T11's business, and a row whose DisputedAt never reached Decide is
// left for a snapshot that carries it.
func expiredDisputes(in Input) ([]Effect, []Notice) {
	var effects []Effect
	var notices []Notice
	for _, r := range in.Reqs {
		if r.Status != StatusDisputed || !isGuardKind(r.Kind) || r.DisputedAt.IsZero() || in.Now.Before(r.DisputedAt.Add(DisputeExpiry)) {
			continue
		}
		effects = append(effects, Effect{Kind: EffectCancel, RequestID: r.ID, Reason: CancelDisputeExpired})
		notices = append(notices, requestNotice(in, NoticeChangeCancelled, r.ID))
	}
	return effects, notices
}

// releasedReservations returns, in canonical order, the reservations the sweep releases.
func releasedReservations(in Input) []CredRef {
	refs := make([]CredRef, 0, len(in.Reservations))
	for _, res := range in.Reservations {
		if releasable(in, res) {
			refs = append(refs, res.AddressRef)
		}
	}
	return sortRefs(refs)
}

// releasable is child spec §4 input 1: a reservation is released only when all three hold.
//   - Now has reached its FloorUntil (displacement + Δ_cp).
//   - The one current primary is uncontested and matured strictly before Now.
//   - No pending or disputed break-glass opened at or before the instant the current primary
//     matured (#19, D7), because its completion restores the primary that was mature when it
//     opened. The test is primaryCountsAt, the predicate restoreTarget uses, so the two
//     cannot drift (F5): a primary maturing exactly at OpenedAt did not count then.
//
// A zero FloorUntil or MaturesAt (no value reached Decide) keeps the reservation.
func releasable(in Input, res Reservation) bool {
	if res.FloorUntil.IsZero() || in.Now.Before(res.FloorUntil) {
		return false
	}
	cur, ok := currentPrimary(in)
	if !ok || !cur.ContestedAt.IsZero() || cur.MaturesAt.IsZero() || !cur.MaturesAt.Before(in.Now) {
		return false
	}
	for _, r := range in.Reqs {
		if r.Kind == ReqBreakGlass && isOpenGuardRow(r) && !primaryCountsAt(cur, r.OpenedAt) {
			return false
		}
	}
	return true
}

// deletableContested returns the contested credentials the sweeper deletes (parent T5: "The
// sweeper deletes it after Δ_cp"; architect ruling A3): ContestedAt + DeltaCP has been
// reached. The password and the primary address are never deleted (D9).
func deletableContested(in Input) []CredRef {
	return credRefsWhere(in.Creds, func(c Cred) bool {
		k := c.Ref.Kind
		if c.ContestedAt.IsZero() || k.Rank() == RankInvalid || k == CredPassword || k == CredAddress {
			return false
		}
		return !in.Now.Before(c.ContestedAt.Add(DeltaCP))
	})
}

// ---- held time -------------------------------------------------------------------------

// maxHeldMs saturates held-time arithmetic.
const maxHeldMs = 1<<63 - 1

// accruedMs is r's held time at Now in milliseconds: the settled HeldMs plus, while a hold
// is open, Now - HeldSince (child spec §7). A negative settled value and a hold opened after
// Now count as zero, and the sum saturates rather than wraps.
func accruedMs(in Input, r Req) int64 {
	held := max(r.HeldMs, 0)
	if r.HeldSince.IsZero() {
		return held
	}
	open := in.Now.Sub(r.HeldSince).Milliseconds()
	switch {
	case open <= 0:
		return held
	case held > maxHeldMs-open:
		return maxHeldMs
	}
	return held + open
}

// foldEffect closes r's open hold: HeldMs becomes the accrued time and HeldSince is cleared
// (child spec §6.2). A request with no open hold has nothing to fold.
func foldEffect(in Input, r Req) (Effect, bool) {
	if r.HeldSince.IsZero() {
		return Effect{}, false
	}
	return Effect{Kind: EffectHold, RequestID: r.ID, HeldMs: accruedMs(in, r)}, true
}

// appendFold appends r's fold when r is leaving pending (architect ruling 1's corollary). A
// disputed row accrues nothing, so it has no hold to fold.
func appendFold(in Input, effects []Effect, r Req) []Effect {
	if r.Status != StatusPending {
		return effects
	}
	if fold, ok := foldEffect(in, r); ok {
		effects = append(effects, fold)
	}
	return effects
}
