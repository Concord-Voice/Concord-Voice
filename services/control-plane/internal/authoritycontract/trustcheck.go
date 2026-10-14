package authoritycontract

import (
	"maps"
	"slices"
)

// The client forms here (T6, T7, T8, T12) are existential over every valid
// delegation retained at a (gen, serial). Two different ones can share a key
// only in a retired generation: T4 halts on a contradiction in current_gen and
// ignores one in a retired gen, and T3 retains both "for T7/T8 lookups".
// Reading "the valid D named" as "any valid D retained under that name" is the
// weakest form the DoR permits (plan A6; T8 says "there exists a valid D′"),
// keeps the authority's own delegation, which is always one of them, accepted
// (AC9), and does not depend on arrival order. It grants nothing a forger
// lacked: whoever can dual-sign a second delegation for a retired (gen, serial)
// can sign one for an unused serial of that generation, and every result under
// a retired generation is superseded, so it is accepted only under a
// current-generation status (T7). Refusing an ambiguous key instead would let
// that same forger withhold every honest result issued under it.

// covers is §2.10's window: not_before_ms ≤ x < not_after_ms.
func covers(d Delegation, x uint64) bool { return d.NotBeforeMs <= x && x < d.NotAfterMs }

// rskVerifies tries at most MAX_RSKS keys of D (LD-2).
func rskVerifies(d Delegation, msg, sig []byte) bool {
	for i, k := range d.RSKPublicKeys {
		if i >= MaxRSKs {
			break
		}
		if VerifyP384(k, msg, sig) {
			return true
		}
	}
	return false
}

// bestOf returns the furthest Reason any of ds reaches under rule, ranked by
// order (first = least far, last = ReasonOK). A search over several retained
// delegations therefore reports one Reason whatever order they arrived in. A
// Reason absent from order ranks as order[0], a failure.
func bestOf(ds []Delegation, order []Reason, rule func(Delegation) Reason) Reason {
	best := 0
	for _, d := range ds {
		best = max(best, slices.Index(order, rule(d)))
	}
	return order[best]
}

// rskRule is the per-delegation part of T6 and T7, in their check order: an
// RSK of d signed msg, d's window covers x, and scope ∈ d.scope.
func rskRule(d Delegation, msg, sig []byte, x uint64, scope string) Reason {
	switch {
	case !rskVerifies(d, msg, sig):
		return ReasonBadSignature
	case !covers(d, x):
		return ReasonDelegationWindow
	case !slices.Contains(d.Scope, scope):
		return ReasonOutOfScope
	}
	return ReasonOK
}

func rskOrder() []Reason {
	return []Reason{ReasonBadSignature, ReasonDelegationWindow, ReasonOutOfScope, ReasonOK}
}

// checkT6 verifies a status or device-status signature and delegation and
// returns the effective HW (never decreasing). D must be the HW delegation
// itself: gen = current_gen, serial ≥ HW.min_valid, and not below HW.
func (t *Trust) checkT6(hw HighWater, msg, sig []byte, gen, serial, asOf uint64) (HighWater, Reason) {
	if r := t.haltReason(); r != ReasonOK {
		return hw, r
	}
	ds := t.delegations(gen, serial)
	if len(ds) == 0 {
		return hw, ReasonDelegationUnknown
	}
	r := bestOf(ds, rskOrder(), func(d Delegation) Reason { return rskRule(d, msg, sig, asOf, ScopeStatus) })
	if r == ReasonBadSignature {
		return hw, r
	}
	eff := maxHW(hw, t.HighWater())
	if gen != t.CurrentGen() || serial < eff.MinValid || gen != eff.Gen || serial != eff.Serial {
		return hw, ReasonStatusSuperseded
	}
	if r != ReasonOK {
		return hw, r
	}
	return eff, ReasonOK
}

// checkT7 verifies a result's (or device-result's) RSK signature and
// delegation; superseded entries need a covering status (VerifyChain). A
// caller that reads D's parameters afterwards (R1, R2) must take them from a
// delegation at (gen, serial) whose rskRule is OK, never from lookup, which
// refuses an ambiguous key. Callers must have checked haltReason: checkT7
// does not consult the halt (VerifyBundle, VerifyChain and VerifyDeviceLedger
// refuse a halted trust first). A nil Trust is trust_halt, as haltReason has
// it.
func (t *Trust) checkT7(hw HighWater, gen, serial, issued uint64, op string, msg, sig []byte) (bool, Reason) {
	if t == nil {
		return false, ReasonTrustHalt
	}
	ds := t.delegations(gen, serial)
	if len(ds) == 0 {
		return false, ReasonDelegationUnknown
	}
	if r := bestOf(ds, rskOrder(), func(d Delegation) Reason { return rskRule(d, msg, sig, issued, op) }); r != ReasonOK {
		return false, r
	}
	eff := maxHW(hw, t.HighWater())
	// A persisted HW of a later generation than this Trust also supersedes
	// (T6 then refuses every status, so the entry fails closed).
	return gen < max(t.CurrentGen(), eff.Gen) || (gen == eff.Gen && serial < eff.MinValid), ReasonOK
}

// sortedKeys returns the retained (gen, serial) keys, highest first.
func (t *Trust) sortedKeys() [][2]uint64 {
	return slices.SortedFunc(maps.Keys(t.dels), func(a, b [2]uint64) int { return slices.Compare(b[:], a[:]) })
}

// cakRule is T8's per-delegation part: a CAK of d signed cpAuthz, and d's
// window covers assertedAt.
func cakRule(d Delegation, cpAuthz, sig []byte, assertedAt uint64) Reason {
	if !slices.ContainsFunc(d.CAKPublicKeys, func(k []byte) bool { return VerifyP384(k, cpAuthz, sig) }) {
		return ReasonCAKUnlisted
	}
	if !covers(d, assertedAt) {
		return ReasonDelegationWindow
	}
	return ReasonOK
}

// matchCAK is T8's search: the highest serial ≤ maxSerial of gen at which a
// retained D′ admits the CAK signature. Otherwise it reports
// delegation_window when a CAK verified under a D′ whose window misses
// assertedAt, and cak_unlisted when none verified.
func (t *Trust) matchCAK(cpAuthz, sig []byte, gen, maxSerial, assertedAt uint64) (uint64, Reason) {
	r := ReasonCAKUnlisted
	order := []Reason{ReasonCAKUnlisted, ReasonDelegationWindow, ReasonOK}
	rule := func(d Delegation) Reason { return cakRule(d, cpAuthz, sig, assertedAt) }
	for _, k := range t.sortedKeys() {
		if k[0] != gen || k[1] > maxSerial {
			continue
		}
		got := bestOf(t.delegations(k[0], k[1]), order, rule)
		if got == ReasonOK {
			return k[1], ReasonOK
		}
		if got == ReasonDelegationWindow {
			r = got
		}
	}
	return 0, r
}

// checkT8 is T8, the CAK's client form. Callers must have checked
// haltReason: checkT8 does not consult the halt. A nil Trust is trust_halt,
// as haltReason has it.
func (t *Trust) checkT8(cpAuthz, sig []byte, gen, serial, assertedAt uint64) Reason {
	if t == nil {
		return ReasonTrustHalt
	}
	_, r := t.matchCAK(cpAuthz, sig, gen, serial, assertedAt)
	return r
}

// CAKAuthority is T9's T8 part against the authority's current delegation
// (before BEGIN); it returns the newest D′ whose CAK signed, whose drain
// clause the transaction then checks with DrainClause. It refuses on a halted
// trust and when the authority does not hold exactly one current delegation:
// the authority form may be stricter than T8, never weaker (AC9).
func (t *Trust) CAKAuthority(cpAuthz, sig []byte, curGen, curSerial, assertedAtMs uint64) (gen, serial uint64, ok bool) {
	if t.haltReason() != ReasonOK {
		return curGen, 0, false
	}
	if _, held := t.lookup(curGen, curSerial); !held {
		return curGen, 0, false
	}
	serial, r := t.matchCAK(cpAuthz, sig, curGen, curSerial, assertedAtMs)
	return curGen, serial, r == ReasonOK
}

// DrainClause is T9's in-transaction part: D′ not superseded, or
// db_now < installed_at(superseding) + drain_grace_ms (D-164, D-171).
func DrainClause(superseded bool, installedAtSupersedingMs, drainGraceMs, dbNowMs uint64) bool {
	if !superseded {
		return true
	}
	end, ok := addMs(installedAtSupersedingMs, drainGraceMs)
	return ok && dbNowMs < end
}

// operatorRule is T12's per-delegation part: an operator key of d signed msg,
// and d's window covers assertedAt.
func operatorRule(d Delegation, msg, sig []byte, assertedAt uint64) Reason {
	if !slices.ContainsFunc(d.OperatorPublicKeys, func(k []byte) bool { return VerifyP384(k, msg, sig) }) {
		return ReasonBadSignature
	}
	if !covers(d, assertedAt) {
		return ReasonDelegationWindow
	}
	return ReasonOK
}

// checkT12 is T12's client form over the delegation the result names, with
// no lookback to an earlier serial (D-231). Callers must have checked
// haltReason: checkT12 does not consult the halt (OperatorAuthority,
// VerifyBundle, VerifyChain and VerifyDeviceLedger check it first). A nil
// Trust is trust_halt, as haltReason has it.
func (t *Trust) checkT12(intentDigest, sig []byte, gen, serial, assertedAt uint64) Reason {
	if t == nil {
		return ReasonTrustHalt
	}
	ds := t.delegations(gen, serial)
	if len(ds) == 0 {
		return ReasonDelegationUnknown
	}
	msg := sigMsg(RoleOperator, intentDigest)
	return bestOf(ds, []Reason{ReasonBadSignature, ReasonDelegationWindow, ReasonOK},
		func(d Delegation) Reason { return operatorRule(d, msg, sig, assertedAt) })
}

// DelegationRef names one signing delegation by product-root generation and
// serial.
type DelegationRef struct{ Gen, Serial uint64 }

// OperatorAuthority is T12's authority form: the client form for the claimed
// delegation, plus the key in cur, the current signing delegation, and not
// revoked (AC9: stricter). revoked is
// required: a nil predicate refuses rather than reading as "none revoked", and
// so do a halted trust and a current delegation the authority does not hold.
func (t *Trust) OperatorAuthority(intentDigest, sig []byte, claimed DelegationRef, assertedAtMs uint64, cur DelegationRef,
	revoked func(key []byte) bool) bool {
	if revoked == nil || t.haltReason() != ReasonOK ||
		t.checkT12(intentDigest, sig, claimed.Gen, claimed.Serial, assertedAtMs) != ReasonOK {
		return false
	}
	d, ok := t.lookup(cur.Gen, cur.Serial)
	if !ok {
		return false
	}
	msg := sigMsg(RoleOperator, intentDigest)
	i := slices.IndexFunc(d.OperatorPublicKeys, func(k []byte) bool { return VerifyP384(k, msg, sig) })
	return i >= 0 && !revoked(d.OperatorPublicKeys[i])
}
