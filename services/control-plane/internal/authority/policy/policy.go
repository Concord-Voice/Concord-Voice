// Package policy applies §2.9's authority-only columns over the pure contract
// functions (#3587 PR-B1, DoR §3.0, §3.2.4 step 8). It is pure: the store
// reads the database, normalizes the head and calls Admit.
package policy

import (
	"bytes"
	"slices"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// NCancelMs is N_CANCEL_MS (§2.3 authority-only, Tier 2 row 4).
const NCancelMs uint64 = 2_592_000_000

// Head is the normalized head the store hands to Admit: the published columns
// and the authority-only ones.
type Head struct {
	Pub                                 ac.PublishedHead
	Held, ForceUnverified               bool
	LastCancelAtMs, LastEraseCancelAtMs uint64
	PendingEpochDigest                  []byte
	Segment                             ac.Assurance
	RecoveryBindAssurance               ac.Assurance
	RecoveryKeyBound                    bool
	InPopulation                        bool
}

// Input is one published submit as Admit sees it.
type Input struct {
	Head           Head
	Bundle         ac.Bundle
	Intent         ac.Intent
	Actor          string
	Evidence       ac.Evidence
	EKBinding      *ac.EKBinding
	EKBindingBytes []byte
	EpochDigest    []byte
	Scope          []string
	DBNowMs        uint64
}

// Decision is Admit's verdict: a refusal, or the selected row with its
// outcome, post-state and assurance.
type Decision struct {
	Refusal   ac.Refusal
	Row       ac.Row
	OutcomeP  bool
	Post      ac.Post
	Assurance ac.Assurance
}

func refuse(r ac.Refusal) Decision { return Decision{Refusal: r} }

// operatorOp: an operator actor exists only on rows 8, 10, 15–17 and 19.
func operatorOp(op string) bool {
	return op == ac.OpSuspend || op == ac.OpUnsuspend || op == ac.OpRevokeRoot || op == ac.OpErase
}

func within(atMs, nowMs uint64) bool { return atMs != 0 && nowMs < atMs+NCancelMs }

// Admit is step 8 in the order of decision B1-D8.
func Admit(in Input) Decision {
	op, h := in.Intent.Op, in.Head
	if in.Actor == ac.ActorOperator && !operatorOp(op) { // 1
		return refuse(ac.RefusalInsufficientAuthorization)
	}
	if !slices.Contains(in.Scope, op) { // 2
		return refuse(ac.RefusalPolicyDisabled)
	}
	if op == ac.OpEnrollNew && h.InPopulation { // 3
		return refuse(ac.RefusalInPopulation)
	}
	row, r := ac.SelectRow(h.Pub, in.Bundle) // 4
	if r != ac.ReasonOK {
		if op == ac.OpCancelPending && h.Pub.RootRevoked == 1 && len(in.Bundle.SigCurrentRoot) > 0 &&
			h.Pub.FromCode() != ac.FromPr {
			return refuse(ac.RefusalRootRevoked)
		}
		return refuse(ac.RefusalWrongState)
	}
	if code := rowChecks(in, row); code != "" { // 5–9
		return refuse(code)
	}
	outcomeP := row.NCancel != ac.NCancelDisputed || !within(h.LastCancelAtMs, in.DBNowMs) // 10
	if competitor(row) {
		// Rows 4 and 12 have only a D outcome (toDispBurn). The client derives the
		// flag as res.State == pending (A2 chain.go), so the self-check gets the same.
		outcomeP = false
	}
	return Decision{Row: row, OutcomeP: outcomeP, Post: ac.ExpectedPost(row, h.Pub, in.Intent, outcomeP),
		Assurance: assurance(in, row, outcomeP)}
}

// rowChecks is steps 5–9, the ones that need the selected row, in Admit's order.
func rowChecks(in Input, row ac.Row) ac.Refusal {
	op, h := in.Intent.Op, in.Head
	if h.ForceUnverified && row.ForceUnverified == ac.FURefuse { // 5
		return ac.RefusalWrongState
	}
	if code := intentValues(in, row); code != "" { // 6
		return code
	}
	if competitor(row) && len(in.EpochDigest) > 0 && bytes.Equal(in.EpochDigest, h.PendingEpochDigest) { // 7
		return ac.RefusalWrongState
	}
	if !ac.Floor(ac.Op(op), ac.Actor(in.Actor), row.N, in.Evidence) { // 8
		return ac.RefusalInsufficientAuthorization
	}
	if row.NCancel == ac.NCancelWrongState && within(h.LastEraseCancelAtMs, in.DBNowMs) { // 9
		return ac.RefusalWrongState
	}
	return ""
}

// competitor: rows 4 and 12 contest a pending candidate (same-epoch rule).
func competitor(row ac.Row) bool { return row.N == 4 || row.N == 12 }

// intentValues is decision B1-D7.
func intentValues(in Input, _ ac.Row) ac.Refusal {
	it, p := in.Intent, in.Head.Pub
	switch {
	case it.ExpectedSeq != p.Seq:
		return ac.RefusalStaleSeq
	case !bytes.Equal(it.ExpectedHeadDigest, p.Entry):
		return ac.RefusalStaleHead
	case it.ExpectedRecoveryGen != p.RG:
		return ac.RefusalStaleRecoveryGen
	case len(it.PredecessorRootDigest) > 0 && (len(p.Root) == 0 || !bytes.Equal(it.PredecessorRootDigest, ac.KeyDigest(p.Root))):
		return ac.RefusalPredecessorMismatch
	case len(it.LegacyEKDigest) > 0 && (!bytes.Equal(it.LegacyEKDigest, p.Pin) ||
		!bytes.Equal(ac.KeyDigest(in.Bundle.LegacySPKI), it.LegacyEKDigest)):
		return ac.RefusalLegacyMismatch
	}
	return ekChecks(in)
}

func ekChecks(in Input) ac.Refusal {
	it, p := in.Intent, in.Head.Pub
	if len(it.EKBindingDigest) == 0 {
		return ""
	}
	b := in.EKBinding
	if b == nil || !bytes.Equal(ac.Digest(in.EKBindingBytes), it.EKBindingDigest) ||
		b.RealmID != it.RealmID || b.AccountID != it.AccountID {
		return ac.RefusalEKMismatch
	}
	signer := it.CandidateRoot
	if it.Op == ac.OpRotateEK {
		signer = p.Root
	}
	if len(signer) == 0 || !bytes.Equal(b.RootDigest, ac.KeyDigest(signer)) {
		return ac.RefusalEKMismatch
	}
	if b.EKGeneration != p.EKGen+1 {
		return ac.RefusalEKGeneration
	}
	return ""
}

// assurance is §2.8's DeriveAssurance, "" on a D outcome of rows 2, 3, 11.
func assurance(in Input, row ac.Row, outcomeP bool) ac.Assurance {
	if !outcomeP && (row.N == 2 || row.N == 3 || row.N == 11) {
		return ""
	}
	h := in.Head
	return ac.DeriveAssurance(ac.AssuranceInput{Op: ac.Op(in.Intent.Op), ValidPoP: in.Evidence.PoP,
		PinLive: h.Pub.PinRevoked == 0, RootLive: h.Pub.RootRevoked == 0, ForceUnverified: h.ForceUnverified,
		HasRoot: len(h.Pub.Root) > 0, Ev: in.Evidence, Row: row.N, Segment: h.Segment,
		BindingSegment: h.RecoveryBindAssurance})
}

// HeldRefuses is step 5: recorded `held` except rows 16, 17, 18 (commits a Pe
// that cannot activate), 19 and 22.
func HeldRefuses(code ac.FromCode, op, actor string) bool {
	switch {
	case actor == ac.ActorRestore:
		return false // row 22: admitted only while held
	case op == ac.OpErase:
		return false // rows 18 and 19
	case op == ac.OpRevokeRoot && actor == ac.ActorOperator:
		return code != ac.FromP && code != ac.FromD && code != ac.FromAp // rows 16, 17
	}
	return true
}
