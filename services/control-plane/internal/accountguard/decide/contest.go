package decide

// Contests: T5, and T1b's same-outcome contest set (parent §6.2 T5; child spec §3.0 LEAD-B
// 3-A; plan D20).

// decideContest is T5: contest every immature enrolment the proof outranks. A contest rotates
// the epoch (#40), because a session already signed in with the contested credential would
// otherwise outlive it.
func decideContest(in Input) Outcome {
	set := contestSet(in, false)
	if len(set) == 0 {
		return refuse(RowT5)
	}
	effects, notices := contestEffects(in, set)
	return Outcome{Row: RowT5, Verdict: VerdictApplied, Effects: effects, RotateEpoch: true, Notices: notices}
}

// contestSet returns, in canonical order, the credentials the proof may contest: each is
// uncontested, does not count now (immature or awaiting its notice), has an enrolment
// instant and a maturity instant, and is outranked under T5's rule by the proof's class at that instant, counting
// only credentials mature before the enrolment. T1b passes possessionAndAnchorsOnly, which
// keeps only TOTP, WebAuthn, backup-code sets and anchors: addresses and passwords are left
// to /guard/contest (3-A).
func contestSet(in Input, possessionAndAnchorsOnly bool) []CredRef {
	return credRefsWhere(in.Creds, func(x Cred) bool { return contestable(in, x, possessionAndAnchorsOnly) })
}

func contestable(in Input, x Cred, possessionAndAnchorsOnly bool) bool {
	rank := x.Ref.Kind.Rank()
	switch {
	case rank == RankInvalid, possessionAndAnchorsOnly && rank == Rank1:
		return false
	case !x.ContestedAt.IsZero(), counts(x, in.Now):
		return false
	case x.EnrolledAt.IsZero():
		// D20: no enrolment instant reached Decide, so nothing can have matured before it.
		return false
	case x.MaturesAt.IsZero():
		// Round-2 Q6: a contest is admissible only against an immature enrolment, and a
		// maturity that never reached Decide proves nothing. Reading it as immature would
		// let an equal-class proof kill a possibly mature factor at once, which T3 says must
		// wait Δ_cp. The awaiting-notice sentinel is non-zero and stays contestable.
		return false
	}
	return ClassOf(in.Proof, in.Creds, x.EnrolledAt) >= contestThreshold(x)
}

// contestThreshold is the class a contest of x needs (parent T5). An anchor is contested at
// C2, the class T4 needs to enrol one (#22), whatever class enrolled it. A K or P credential
// needs the class that enrolled it, and never less than its rank (#34); an unrecorded enrol
// class (ClassNone) therefore means the rank floor (D20).
func contestThreshold(x Cred) Class {
	rank := x.Ref.Kind.Rank()
	switch rank {
	case Rank3:
		return C2
	case RankInvalid:
		return C3
	}
	floor := Class(rank)
	if x.EnrolClass > floor {
		return x.EnrolClass
	}
	return floor
}

// contestEffects are one EffectContest and one enrolment_contested per credential, plus the
// restore of the displaced primary when the set holds the current primary. The notice
// carries the contested credential and neither a request nor reservations (architect
// ruling 3).
func contestEffects(in Input, set []CredRef) ([]Effect, []Notice) {
	effects := make([]Effect, 0, len(set)+1)
	notices := make([]Notice, 0, len(set))
	for _, r := range set {
		effects = append(effects, Effect{Kind: EffectContest, Cred: r})
		notices = append(notices, Notice{Kind: NoticeEnrolmentContested, Subject: r})
	}
	if cur, ok := currentPrimary(in); ok && containsSlot(set, cur.Ref) {
		if t, ok := restoreDisplaced(in, cur); ok {
			effects = append(effects, Effect{Kind: EffectRestorePrimary, Cred: t})
		}
	}
	return effects, notices
}

// restoreDisplaced returns the primary a contested current primary displaced (parent T5): of
// the uncontested, still-reserved primaries displaced no later than cur became primary, the
// one displaced last. A tie goes to the smaller ref, so the result never depends on order.
func restoreDisplaced(in Input, cur Primary) (CredRef, bool) {
	var best Primary
	found := false
	for _, p := range in.Primaries {
		if !displacedBy(p, cur) || !reserved(in, p.Ref) {
			continue
		}
		later := p.DisplacedAt.After(best.DisplacedAt)
		if !found || later || (p.DisplacedAt.Equal(best.DisplacedAt) && refLess(p.Ref, best.Ref)) {
			best, found = p, true
		}
	}
	return best.Ref, found
}

func displacedBy(p, cur Primary) bool {
	return !p.DisplacedAt.IsZero() && !p.DisplacedAt.After(cur.BecamePrimaryAt) &&
		p.ContestedAt.IsZero() && !sameSlot(p.Ref, cur.Ref)
}

// reserved reports whether a reservation holds ref's slot.
func reserved(in Input, ref CredRef) bool {
	for _, res := range in.Reservations {
		if sameSlot(res.AddressRef, ref) {
			return true
		}
	}
	return false
}

// currentPrimary returns the one primary with a zero DisplacedAt. None, or two (a malformed
// snapshot), is not found.
func currentPrimary(in Input) (Primary, bool) {
	var cur Primary
	n := 0
	for _, p := range in.Primaries {
		if p.DisplacedAt.IsZero() {
			cur = p
			n++
		}
	}
	return cur, n == 1
}
