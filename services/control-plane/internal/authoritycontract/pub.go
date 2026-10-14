package authoritycontract

import (
	"slices"
	"sort"
)

// FromCode is a §2.9 from-state code; every code is client-visible.
type FromCode string

// The §2.9 from-state codes (DoR:589–603). In the From column of the
// transition table P excludes Pr (and Pe) unless the row names it.
const (
	FromA0  FromCode = "A0"  // absent, seq 0, no pin
	FromAp  FromCode = "Ap"  // absent, pin present
	FromApr FromCode = "Apr" // absent, legacy_pin_revoked = 1
	FromAc  FromCode = "Ac"  // active
	FromSu  FromCode = "Su"  // suspended, set {user}, root not revoked
	FromSo  FromCode = "So"  // suspended, operator ∈ set, root_revoked = 0
	FromSor FromCode = "Sor" // suspended, root_revoked = 1
	FromP   FromCode = "P"   // pending, prior x (never Pr or Pe)
	FromD   FromCode = "D"   // disputed, prior x
	FromPr  FromCode = "Pr"  // pending recover (LD-34)
	FromPe  FromCode = "Pe"  // pending erase
	FromE   FromCode = "E"   // erased
)

// pub is the client-visible state after a verified result, or at genesis.
// It is derived, never persisted (the persisted form is ChainHead, D-269).
type pub struct {
	Seq         uint64
	Entry       []byte // entry_digest of the head result; nil at seq 0
	State       string
	Prior       string
	Susp        []string
	Root        []byte // the 97-byte active root; nil when there is none
	RootRevoked uint64
	EK          []byte // ek_binding_digest (result #15)
	PendRoot    []byte
	PendEK      []byte
	Pin         []byte // legacy_ek_digest (result #18)
	PinRevoked  uint64
	RG          uint64
	ANB         uint64
	Assurance   string
	Issued      uint64
	Op          string
	EKGen       uint64 // chain-head #8: the active binding's generation
	PendEKGen   uint64 // chain-head #10
}

func genesisPub() pub { return pub{State: StateAbsent} }

func pubOfResult(r Result, rb []byte, ekGen, pendEKGen uint64) pub {
	return pub{Seq: r.Seq, Entry: EntryDigest(rb), State: r.State, Prior: r.PriorState, Susp: r.SuspendedBy,
		Root: r.ActiveRoot, RootRevoked: r.RootRevoked, EK: r.EKBindingDigest, PendRoot: r.PendingRoot,
		PendEK: r.PendingEKBindingDigest, Pin: r.LegacyEKDigest, PinRevoked: r.LegacyPinRevoked, RG: r.RecoveryGen,
		ANB: r.ActivationNotBeforeMs, Assurance: r.Assurance, Issued: r.IssuedAtMs, Op: r.Op,
		EKGen: ekGen, PendEKGen: pendEKGen}
}

// classify is §2.9's from-state code. A Pr head is a pending head whose last
// op is recover; Pe is a pending head with no pending root (result #16).
// A state outside the six result states falls to E, which no row admits.
func classify(p pub) FromCode {
	switch p.State {
	case StateAbsent:
		if len(p.Pin) == 0 {
			return FromA0
		}
		if p.PinRevoked == 1 {
			return FromApr
		}
		return FromAp
	case StateActive:
		return FromAc
	case StateSuspended:
		if p.RootRevoked == 1 {
			return FromSor
		}
		if has(p.Susp, ActorOperator) {
			return FromSo
		}
		return FromSu
	case StatePending:
		if p.Op == OpRecover {
			return FromPr
		}
		if len(p.PendRoot) == 0 {
			return FromPe
		}
		return FromP
	case StateDisputed:
		return FromD
	}
	return FromE
}

// x is the "x" of P(x), D(x), Pe(x) and Pr(x); for any other state, the state.
func (p pub) x() string {
	if p.State == StatePending || p.State == StateDisputed {
		return p.Prior
	}
	return p.State
}

// activate is §2.9's derived activation table (DoR:664–672; no event, no seq
// bump). Only a pending head activates: D(x) never does, and any other state
// has nothing pending, so both are returned unchanged.
func activate(p pub) pub {
	if p.State != StatePending {
		return p
	}
	q := p
	q.Prior, q.ANB, q.PendRoot, q.PendEK = "", 0, nil, nil
	if len(p.PendRoot) == 0 { // Pe(x) → E, the root kept
		q.State, q.Susp = StateErased, nil
		return q
	}
	q.Root, q.EK, q.EKGen, q.PendEKGen, q.RootRevoked = p.PendRoot, p.PendEK, p.PendEKGen, 0, 0
	q.State, q.Susp = StateActive, nil
	if p.Prior == StateSuspended {
		set := slices.Clone(p.Susp)
		if p.RootRevoked == 1 { // operator removed only when it came from Sor; user never removed
			set = withoutSet(set, ActorOperator)
		}
		if len(set) > 0 {
			q.State, q.Susp = StateSuspended, set
		}
	}
	return q
}

func has(set []string, v string) bool { return slices.Contains(set, v) }

// withSet is set ∪ {v}, sorted as the codec's a arrays are; the input is not modified.
func withSet(set []string, v string) []string {
	if has(set, v) {
		return slices.Clone(set)
	}
	out := append(slices.Clone(set), v)
	sort.Strings(out)
	return out
}

// withoutSet is set \ {v}; never nil, because suspended_by is [] when not suspended.
func withoutSet(set []string, v string) []string {
	out := []string{}
	for _, s := range set {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

// sameSet compares two sorted sets; nil and [] are the same set.
func sameSet(a, b []string) bool { return len(a) == len(b) && (len(a) == 0 || slices.Equal(a, b)) }

// digestOrNil is the digest of a key, or nil for an absent one ("" on the wire).
func digestOrNil(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return Digest(b)
}
