package decide

import "time"

// Completion: T11, its holds, the target fence and the restore target (parent §6.2 T11 and
// Holds; child spec §4 input 1; architect ruling 1).

// decideComplete is the sweeper's T11 check of one request, in the order of architect
// ruling 1 as amended by round-2 Q1 and Q2:
//  1. the request must be pending, of a guard kind and well formed, or the table has no row;
//  2. held time at or past HoldCap cancels it as hold_cap, before any due check (#6);
//  3. the request is due once Now reaches CompletesAt plus its held time, held or not;
//  4. a request that is not held closes any open hold, and the fold is kept whatever follows;
//  5. a request that is not due changes nothing more;
//  6. a held request opens a hold at Now if none is open;
//  7. otherwise it is cancelled as superseded when a target moved (every kind but
//     break-glass, #33, #38, #39), or it completes.
func decideComplete(in Input, act Complete) Outcome {
	r, ok := findReq(in.Reqs, act.ID)
	if !ok || !completable(r) {
		return refuse(RowNone)
	}
	accrued := accruedMs(in, r)
	if accrued >= HoldCap.Milliseconds() {
		return holdCapCancel(in, r)
	}
	// accrued is below HoldCap, so the due instant cannot overflow.
	due := !in.Now.Before(r.CompletesAt.Add(time.Duration(accrued) * time.Millisecond))
	if isHeld(in, r) {
		return holdIfDue(in, r, due)
	}
	return completeIfDue(in, r, due)
}

// completable reports whether r is a pending request of a guard kind with its instants set.
// CompletesAt must be at least OpenedAt + DeltaCP (H3): every guard row opens at Now + Δ_cp
// and a T12 return keeps it, so an earlier CompletesAt is a mapping bug that would complete
// the request early. "At least" lets a future kind with a longer delay pass. A
// credential_remove must name at least one target, and never the password or the primary
// address, which no T3 opens (D9).
func completable(r Req) bool {
	if r.Status != StatusPending || !isGuardKind(r.Kind) || r.OpenedAt.IsZero() || r.CompletesAt.IsZero() {
		return false
	}
	if r.CompletesAt.Before(r.OpenedAt.Add(DeltaCP)) {
		return false
	}
	if r.Kind != ReqCredentialRemove {
		return true
	}
	for _, t := range r.Targets {
		if t.Kind.Rank() == RankInvalid || t.Kind == CredPassword || t.Kind == CredAddress {
			return false
		}
	}
	return len(r.Targets) > 0
}

// isHeld is parent §6.2 Holds: a notice to a mature address not yet settled (hold a), no
// accepted address notice yet (#9), or a guessable budget not read as clear that no mature
// non-guessable credential covers (hold b). Both flags hold at their zero value (H1), so a
// runner that never loaded them holds the request rather than completing it.
func isHeld(in Input, r Req) bool {
	return !r.MatureNoticesSettled || !r.AcceptedAddressNotice || (!in.GuessableBudgetClear && !covered(in, r))
}

// covered is hold (b)'s per-request test, by T7's class rule: every WebAuthn and anchor
// credential of the account, counted at OpenedAt, reaches the request's origin class. Every
// such WebAuthn is taken as UV-capable, because Cred carries no UV bit (D21). None is taken
// as device-bound: nobody asserted these credentials, so the proof built here has an empty
// DeviceBound and a WebAuthn never pairs with an anchor into C3 (red-team F1). Hold (b) is
// therefore strict only in the holding direction.
func covered(in Input, r Req) bool {
	var used []CredRef
	for _, c := range in.Creds {
		if c.Ref.Kind == CredWebAuthn || c.Ref.Kind.Rank() == Rank3 {
			used = append(used, c.Ref)
		}
	}
	cls := ClassOf(Proof{Used: used}, in.Creds, r.OpenedAt)
	return cls >= C1 && cls >= r.OriginClass
}

// holdCapCancel cancels r with no change once its held time reaches HoldCap, folding an open
// hold (architect ruling 1's corollary).
func holdCapCancel(in Input, r Req) Outcome {
	effects := appendFold(in, []Effect{{Kind: EffectCancel, RequestID: r.ID, Reason: CancelHoldCap}}, r)
	return Outcome{
		Row: RowT11, Verdict: VerdictApplied, Effects: effects,
		Notices: []Notice{requestNotice(in, NoticeChangeCancelled, r.ID)},
	}
}

// holdIfDue handles a held request. A hold opens only once the request is due (architect
// ruling 1), so a held request that is not due changes nothing: opening a hold earlier
// would spend HoldCap on time when the request could not have completed anyway (round-2
// Q1). A due request with no open hold opens one at Now; one whose hold is already open
// stays held with no effect.
func holdIfDue(in Input, r Req, due bool) Outcome {
	switch {
	case !due:
		return Outcome{Row: RowT11, Verdict: VerdictNoChange}
	case r.HeldSince.IsZero():
		hold := Effect{Kind: EffectHold, RequestID: r.ID, HeldMs: max(r.HeldMs, 0), HeldSince: in.Now}
		return Outcome{Row: RowT11, Verdict: VerdictHeld, Effects: []Effect{hold}}
	default:
		return Outcome{Row: RowT11, Verdict: VerdictHeld}
	}
}

// completeIfDue handles a request that is not held. Its open hold, if any, is folded first
// and the fold is kept whatever follows (round-2 Q2): an open hold left on a row that is no
// longer held would keep accruing, so its due instant would recede with Now.
func completeIfDue(in Input, r Req, due bool) Outcome {
	var effects []Effect
	if fold, ok := foldEffect(in, r); ok {
		effects = append(effects, fold)
	}
	if !due {
		return Outcome{Row: RowT11, Verdict: VerdictNoChange, Effects: effects}
	}
	if r.Kind != ReqBreakGlass && !targetsCurrent(in, r) {
		effects = append(effects, Effect{Kind: EffectCancel, RequestID: r.ID, Reason: CancelSuperseded})
		return Outcome{
			Row: RowT11, Verdict: VerdictApplied, Effects: effects,
			Notices: []Notice{requestNotice(in, NoticeChangeCancelled, r.ID)},
		}
	}
	return completion(in, r, effects)
}

// targetsCurrent is the target fence: every target's slot still holds exactly one
// credential, at the version the request recorded. A break-glass is exempt (F4, #38): its
// targets are the password and the factors it resets, which an incumbent's T1 rotates at
// will.
func targetsCurrent(in Input, r Req) bool {
	for _, t := range r.Targets {
		c, ok := findSlot(in.Creds, t)
		if !ok || c.Ref.Version != t.Version {
			return false
		}
	}
	return true
}

// completion completes r (T11). A credential_remove removes its targets; an address_change,
// social_recovery or circle_change completes with no credential effect here; a break-glass
// adds its credential effects and the restore target, rotates the epoch and sets the pending
// password (D15). Every completion emits change_completed (architect ruling 3).
//
// EnrolClass (F2) is the request's origin class on the two kinds whose completion writes a
// credential: the break-glass's password, and the address_change's new address (C5). A
// removal writes none, so a credential_remove, social_recovery or circle_change completion
// carries ClassNone.
func completion(in Input, r Req, effects []Effect) Outcome {
	effects = append(effects, Effect{Kind: EffectComplete, RequestID: r.ID})
	switch r.Kind {
	case ReqBreakGlass:
		effects = append(effects, breakGlassEffects(in, r.OpenedAt, r.Origins, r.ResetFactors)...)
		return Outcome{
			Row: RowT11, Verdict: VerdictApplied, Effects: effects, EnrolClass: r.OriginClass,
			RotateEpoch: true, SetsPassword: true, Notices: breakGlassNotices(in, r.ID, effects),
		}
	case ReqAddressChange:
		return Outcome{
			Row: RowT11, Verdict: VerdictApplied, Effects: effects, EnrolClass: r.OriginClass,
			Notices: completionNotices(in, r.ID, effects),
		}
	case ReqCredentialRemove:
		for _, t := range sortRefs(r.Targets) {
			effects = append(effects, Effect{Kind: EffectRemove, Cred: t})
		}
	}
	return Outcome{Row: RowT11, Verdict: VerdictApplied, Effects: effects, Notices: completionNotices(in, r.ID, effects)}
}

// restoreTarget is the primary a break-glass completion restores (child spec §4 input 1,
// amending parent T11's "current at opened_at"): the newest primary, by BecamePrimaryAt,
// that was mature and uncontested at openedAt, among the reserved primaries and the current
// one. Nothing is restored when that primary is the current one, so a primary that was
// already mature when the break-glass opened is never rolled back (#21).
func restoreTarget(in Input, openedAt time.Time) (CredRef, bool) {
	cur, hasCur := currentPrimary(in)
	var best Primary
	found := false
	for _, p := range in.Primaries {
		isCur := hasCur && p.DisplacedAt.IsZero()
		if !primaryCountsAt(p, openedAt) || (!isCur && !reserved(in, p.Ref)) {
			continue
		}
		if !found || newerPrimary(p, best) {
			best, found = p, true
		}
	}
	if !found || (hasCur && sameSlot(best.Ref, cur.Ref)) {
		return CredRef{}, false
	}
	return best.Ref, true
}

func primaryCountsAt(p Primary, at time.Time) bool {
	return p.ContestedAt.IsZero() && !p.MaturesAt.IsZero() && p.MaturesAt.Before(at)
}

// newerPrimary orders primaries by BecamePrimaryAt, newest first, then by ref.
func newerPrimary(a, b Primary) bool {
	if !a.BecamePrimaryAt.Equal(b.BecamePrimaryAt) {
		return a.BecamePrimaryAt.After(b.BecamePrimaryAt)
	}
	return refLess(a.Ref, b.Ref)
}
