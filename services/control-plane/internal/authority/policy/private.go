package policy

import (
	"bytes"
	"slices"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// PrivateInput is one private device submit as AdmitPrivate sees it.
type PrivateInput struct {
	Head                          Head
	Op                            string
	DeviceIntent                  *ac.DeviceIntent
	Bind                          *ac.RecoveryBind
	PriorSig, RootForm, SheetForm bool
	Evidence                      ac.Evidence
	Scope                         []string
}

func headInput(h Head) ac.HeadInput {
	var rd []byte
	if len(h.Pub.Root) > 0 {
		rd = ac.KeyDigest(h.Pub.Root)
	}
	return ac.HeadInput{State: h.Pub.State, Seq: h.Pub.Seq, EntryDigest: h.Pub.Entry, RootDigest: rd,
		ForceUnverified: h.ForceUnverified}
}

// AdmitPrivate is the policy of POST /v1/device-transactions (§3.2.4), in the
// DoR's order: effective Ac and not force_unverified (wrong_state), stale_seq
// and stale_head, the floor (and §2.22's tier), then the scope.
func AdmitPrivate(in PrivateInput) ac.Refusal {
	switch {
	case in.Op == ac.OpCancelRecoveryBind && in.SheetForm:
		return scopeOf(in) // DR9 (b) and the signature are the store's
	case in.Op == ac.OpCancelRecoveryBind:
		if r := ac.CheckDeviceIntentHead(*in.DeviceIntent, headInput(in.Head)); r != "" {
			return r
		}
		return scopeOf(in)
	}
	if in.Head.Pub.FromCode() != ac.FromAc || in.Head.ForceUnverified {
		return ac.RefusalWrongState
	}
	if r := ac.CheckDeviceIntentHead(*in.DeviceIntent, headInput(in.Head)); r != "" {
		return r
	}
	if in.Op == ac.OpBindRecovery {
		if r := bindPosition(in); r != "" {
			return r
		}
	}
	if !ac.Floor(ac.Op(in.Op), ac.Actor(ac.ActorUser), 0, in.Evidence) {
		return ac.RefusalInsufficientAuthorization
	}
	if in.Op == ac.OpBindRecovery && !in.PriorSig {
		tier := ac.BindTier(in.Evidence.Factors, in.Evidence.MFAEnrolled)
		if !ac.BindWaitAdmitted(ac.BindWait(in.DeviceIntent.BindWait), tier) {
			return ac.RefusalInsufficientAuthorization
		}
	}
	return scopeOf(in)
}

// bindPosition: B.chain_seq and B.chain_entry_digest equal the effective head
// (stale_seq, stale_head); B.binding_root_digest equals its active root
// (B1-D15: wrong_state).
func bindPosition(in PrivateInput) ac.Refusal {
	b, p := in.Bind, in.Head.Pub
	switch {
	case b.ChainSeq != p.Seq:
		return ac.RefusalStaleSeq
	case !bytes.Equal(b.ChainEntryDigest, p.Entry):
		return ac.RefusalStaleHead
	case len(p.Root) == 0 || !bytes.Equal(b.BindingRootDigest, ac.KeyDigest(p.Root)):
		return ac.RefusalWrongState
	}
	return ""
}

func scopeOf(in PrivateInput) ac.Refusal {
	if !slices.Contains(in.Scope, in.Op) {
		return ac.RefusalPolicyDisabled
	}
	return ""
}
