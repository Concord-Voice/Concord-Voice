package decide

import "sort"

// Decide decides one action over one snapshot (child spec §7; parent §6.2). It never panics
// and never writes through its input: a zero Now, a nil or unknown action, a zero enum, an ID
// naming no request, or a snapshot naming one request twice is refused with no effects (plan
// D8). A zero Now is refused first (red-team F3): it is the only clock, no legitimate
// snapshot lacks it, and at a zero instant nothing counts, so every mature credential would
// read as immature and T5 would contest it at equal class.
func Decide(in Input) Outcome {
	if in.Now.IsZero() || !validReqIDs(in.Reqs) {
		return finalize(in, refuse(RowNone))
	}
	return finalize(in, dispatch(in))
}

// dispatch runs the one decision function of the action. Each returns its row's outcome;
// finalize adds what every outcome shares.
func dispatch(in Input) Outcome {
	switch act := in.Act.(type) {
	case SignOutOthers:
		return decideSignOutOthers(in)
	case SignOutEverywhere:
		return decideSignOutEverywhere(in)
	case Remove:
		return decideRemove(in, act)
	case SelfRemoveBackup:
		return decideSelfRemoveBackup(in, act)
	case Enrol:
		return decideEnrol(in, act)
	case Contest:
		return decideContest(in)
	case OpenBreakGlass:
		return decideOpenBreakGlass(in, act)
	case Cancel:
		return decideCancel(in, act)
	case Complete:
		return decideComplete(in, act)
	case Resolve:
		return decideResolve(in, act)
	case Expire:
		return decideExpire(in)
	default:
		return refuse(RowNone)
	}
}

// finalize sets the source (plan D11), strips a refusal down to its row and source, adds
// the MFA follow-ups (D17), and puts Effects and Notices in canonical order, nil when empty
// (D18).
func finalize(in Input, o Outcome) Outcome {
	o.Source = sourceOf(in)
	if o.Verdict == VerdictRefused {
		return Outcome{Row: o.Row, Verdict: VerdictRefused, Source: o.Source}
	}
	o.PermGenBump, o.Effects = mfaFollowUps(o.Effects)
	sort.SliceStable(o.Effects, func(i, j int) bool { return effectBefore(o.Effects[i], o.Effects[j]) })
	sort.SliceStable(o.Notices, func(i, j int) bool { return noticeBefore(o.Notices[i], o.Notices[j]) })
	if len(o.Effects) == 0 {
		o.Effects = nil
	}
	if len(o.Notices) == 0 {
		o.Notices = nil
	}
	return o
}

// sourceOf depends on the action alone (plan D11), so a refusal carries the source its
// action would have and the runner never branches before SourceTx.
func sourceOf(in Input) Source {
	switch act := in.Act.(type) {
	case OpenBreakGlass:
		return SourceBreakGlass
	case Complete:
		if r, ok := findReq(in.Reqs, act.ID); ok && r.Kind == ReqBreakGlass {
			return SourceBreakGlass
		}
	}
	return SourceUserSession
}

// mfaFollowUps reports PermGenBump, true exactly when the effects contain a contest or a
// remove, and appends one EffectResyncMFAFlags whenever they contain a contest, a remove or
// a restore-primary (plan D17).
func mfaFollowUps(effects []Effect) (bool, []Effect) {
	bump, resync := false, false
	for _, e := range effects {
		switch e.Kind {
		case EffectRemove, EffectContest:
			bump, resync = true, true
		case EffectRestorePrimary:
			resync = true
		}
	}
	if resync {
		effects = append(effects, Effect{Kind: EffectResyncMFAFlags})
	}
	return bump, effects
}

// effectBefore is the canonical Effects order (plan D18): kind, request ID, then the
// credential in refLess order.
func effectBefore(a, b Effect) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.RequestID != b.RequestID {
		return a.RequestID < b.RequestID
	}
	return refLess(a.Cred, b.Cred)
}

// noticeBefore is the canonical Notices order: kind, request ID, then the subject.
func noticeBefore(a, b Notice) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.RequestID != b.RequestID {
		return a.RequestID < b.RequestID
	}
	return refLess(a.Subject, b.Subject)
}

// refuse is a refusal at row (architect ruling 8: the row whose condition was evaluated and
// failed, or RowNone when the table has no row for the input).
func refuse(row Row) Outcome { return Outcome{Row: row, Verdict: VerdictRefused} }

// validReqIDs reports whether every request in the snapshot has a non-empty ID that no
// other request shares. An empty ID would read as "the request this outcome opens" (D12).
func validReqIDs(reqs []Req) bool {
	seen := make(map[string]bool, len(reqs))
	for _, r := range reqs {
		if r.ID == "" || seen[r.ID] {
			return false
		}
		seen[r.ID] = true
	}
	return true
}

// findReq returns the request with the given ID.
func findReq(reqs []Req, id string) (Req, bool) {
	for _, r := range reqs {
		if id != "" && r.ID == id {
			return r, true
		}
	}
	return Req{}, false
}

// isGuardKind reports whether a request kind is one T7-T12 act on: a known kind other than
// recovery_key, which the authority decides (parent §6.2).
func isGuardKind(k ReqKind) bool {
	return k >= ReqCredentialRemove && k <= ReqCircleChange
}

// isOpenGuardRow reports whether r is a pending or disputed request T7-T12 act on.
func isOpenGuardRow(r Req) bool {
	return isGuardKind(r.Kind) && (r.Status == StatusPending || r.Status == StatusDisputed)
}

// reservedTo lists every reserved primary in canonical order. Each call returns a fresh
// slice, so no two notices share one.
func reservedTo(in Input) []CredRef {
	refs := make([]CredRef, 0, len(in.Reservations))
	for _, res := range in.Reservations {
		refs = append(refs, res.AddressRef)
	}
	return sortRefs(refs)
}

// requestNotice is a notice that belongs to a request (architect ruling 3): it names the
// request ("" for the one this outcome opens), has no subject, and also goes to every
// reserved primary (parent §6.5, #52).
func requestNotice(in Input, kind NoticeKind, requestID string) Notice {
	return Notice{Kind: kind, RequestID: requestID, ReservedTo: reservedTo(in)}
}

// removalNotice is the credential_removed of a removal that belongs to a request.
func removalNotice(in Input, requestID string, removed CredRef) Notice {
	n := requestNotice(in, NoticeCredentialRemoved, requestID)
	n.Subject = removed
	return n
}

// ---- T1 and T1b ----------------------------------------------------------------------

// decideSignOutOthers is T1: the login floor, maturity ignored. It rotates the epoch, mints
// the actor a continuation session, and changes no request. The rotated password is enrolled
// at the proof's class (F2), which is ClassNone when no mature credential proved it (T5).
func decideSignOutOthers(in Input) Outcome {
	if !LoginFloor(in.Proof, in.Creds, in.SSOLogin) {
		return refuse(RowT1)
	}
	return Outcome{
		Row: RowT1, Verdict: VerdictApplied, RotateEpoch: true, Continuation: true,
		EnrolClass: ClassOf(in.Proof, in.Creds, in.Now),
	}
}

// decideSignOutEverywhere is T1b. It rotates the epoch with no continuation and, in the same
// outcome, contests every immature possession factor and anchor the proof outranks (LEAD-B
// 3-A).
func decideSignOutEverywhere(in Input) Outcome {
	if !t1bQualifies(in) {
		return refuse(RowT1b)
	}
	effects, notices := contestEffects(in, contestSet(in, true))
	notices = append(notices, Notice{Kind: NoticeSignedOutEverywhere})
	return Outcome{Row: RowT1b, Verdict: VerdictApplied, Effects: effects, RotateEpoch: true, Notices: notices}
}

// t1bQualifies reports whether the proof holds a TOTP or WebAuthn that matches exactly, is
// not flagged Paper, and counts at Now (architect ruling 15d). The rest of the proof is
// ignored, so anchor + K and two anchors never qualify (mutation #57).
func t1bQualifies(in Input) bool {
	for _, r := range in.Proof.Used {
		if r.Kind != CredTOTP && r.Kind != CredWebAuthn {
			continue
		}
		if c, ok := findCred(in.Creds, r); ok && !c.Paper && counts(c, in.Now) {
			return true
		}
	}
	return false
}

// ---- T2, T2b and T3 ------------------------------------------------------------------

// decideRemove is T2 (c > rank(X): immediate) or T3 (C1 <= c <= rank(X): a Δ_cp request).
func decideRemove(in Input, act Remove) Outcome {
	t := act.Target
	if !removableTarget(in, t) {
		return refuse(RowNone)
	}
	c := ClassOf(in.Proof, in.Creds, in.Now)
	switch {
	case c == ClassNone:
		return refuse(RowNone)
	case uint8(c) > uint8(t.Kind.Rank()):
		return removeNow(RowT2, t)
	default:
		return openRequest(in, opener{kind: ReqCredentialRemove, targets: []CredRef{t}, class: c, cand: act.Candidate})
	}
}

// removableTarget applies plan D9: the target is a known kind other than the password (T1
// rotates it) and the primary address (C5 changes it, nothing surrenders it), and names a
// present credential exactly that counts now. An immature or contested target has no row;
// PR7a routes it to Contest.
func removableTarget(in Input, t CredRef) bool {
	if t.Kind.Rank() == RankInvalid || t.Kind == CredPassword || t.Kind == CredAddress {
		return false
	}
	c, ok := findCred(in.Creds, t)
	return ok && counts(c, in.Now)
}

// removeNow is an immediate removal (T2, T2b).
func removeNow(row Row, t CredRef) Outcome {
	return Outcome{
		Row: row, Verdict: VerdictApplied,
		Effects: []Effect{{Kind: EffectRemove, Cred: t}},
		Notices: []Notice{{Kind: NoticeCredentialRemoved, Subject: t}},
	}
}

// decideSelfRemoveBackup is T2b: a present, uncontested backup address in the proof removes
// itself, with no maturity requirement.
func decideSelfRemoveBackup(in Input, act SelfRemoveBackup) Outcome {
	t := act.Target
	if t.Kind != CredBackupAddress {
		return refuse(RowNone)
	}
	c, ok := findCred(in.Creds, t)
	if !ok {
		return refuse(RowNone)
	}
	if !c.ContestedAt.IsZero() || !containsRef(in.Proof.Used, t) {
		return refuse(RowT2b)
	}
	return removeNow(RowT2b, t)
}

// ---- T4 ------------------------------------------------------------------------------

// decideEnrol is T4. The enrolling credential is absent from the snapshot (architect ruling
// 7), so an Enrol whose slot is already present is refused. On success the outcome carries
// ClassOf(Proof, Creds, Now) as the enrol class the runner writes (F2, replacing ruling 7's
// runner-computed class).
func decideEnrol(in Input, act Enrol) Outcome {
	r := act.Ref
	if r.Kind.Rank() == RankInvalid || r.Kind == CredPassword || r.Kind == CredAddress || slotTaken(in.Creds, r) {
		return refuse(RowNone)
	}
	if !enrolStepUp(in, r) {
		return refuse(RowT4)
	}
	return Outcome{
		Row: RowT4, Verdict: VerdictApplied, EnrolClass: ClassOf(in.Proof, in.Creds, in.Now),
		Notices: []Notice{{Kind: NoticeCredentialAdded, Subject: r}},
	}
}

// enrolStepUp is T4's step-up for the kind: an address or SSO link needs a proof of itself
// plus the login floor; an anchor needs C2; every other kind needs the login floor.
func enrolStepUp(in Input, r CredRef) bool {
	switch r.Kind {
	case CredBackupAddress, CredSSO:
		return containsRef(in.Proof.Used, r) && LoginFloor(proofWithout(in.Proof, r), in.Creds, in.SSOLogin)
	case CredRecoveryKey, CredRecoveryDevice:
		return ClassOf(in.Proof, in.Creds, in.Now) >= C2
	default:
		return LoginFloor(in.Proof, in.Creds, in.SSOLogin)
	}
}

// slotTaken reports whether a credential already occupies r's slot.
func slotTaken(creds []Cred, r CredRef) bool {
	for _, c := range creds {
		if sameSlot(c.Ref, r) {
			return true
		}
	}
	return false
}

// proofWithout returns a copy of p without r.
func proofWithout(p Proof, r CredRef) Proof {
	used := make([]CredRef, 0, len(p.Used))
	for _, x := range p.Used {
		if !sameCred(x, r) {
			used = append(used, x)
		}
	}
	return Proof{Used: used}
}
