package authoritycontract

import (
	"bytes"
	"slices"
	"strings"
)

// §2.22's binding proof (row 20) and the pending recovery key (row 21).
//
// Invariant. A co-member admits a `recover` only when its B (1) is a bind and
// is the B intent #16 names; (6, realm clause) names the intent's realm and
// account; (2) names a position n below the recover whose seq_index record
// carries B.chain_entry_digest; (3) names as its root the bind root that
// record holds, which appendResult and recordActivation write only when the
// effective state after result n is active, and which root_index resolves to
// a point; (4) carries that root's signature over H(B), while sig_recovery is
// verified under B.recovery_key before R3 (independentSigs, decision A3); and
// (6) sits at or after every activated recover's position with a key none of
// them burned. Check 5 is the row's post-state rule, fed the binding root's
// segment. Checks 3 and 6 read the hypothesis head, whose index already
// records the activation H_act implies (E3-06). Row 21's cancel verifies only
// under the persisted pending_recovery_key (D-269, E3-01).
//
// What breaks it: taking check 3's root from root_index alone (every root the
// chain ever made active would then bind, the pre-activation root of a pending
// result included); judging check 6 against the head before the hypothesis
// recorded its activation; and verifying row 21 under any key but the one the
// pending recover's B named.

func init() {
	rowChecks[20] = checkBindingProof
	rowChecks[21] = checkPendingRecoveryKey
}

// rootRecordOf is the root_index record of digest d with its "<d b64>."
// prefix cut, and whether there is one. root_index is an a<s>, strictly
// ascending bytewise (EN3), and every string carrying the prefix sorts at or
// above it and below any string at or above it that does not, so the first
// record at or above the prefix is the first record of d in index order: the
// one a scan finds. The head must be canonical (indexLen), as every head a
// verifier reads is (ParseChainHead, and every move keeps it so).
func rootRecordOf(h ChainHead, d []byte) (string, bool) {
	prefix := b64s(d) + "."
	i, _ := slices.BinarySearch(h.RootIndex, prefix)
	if i == len(h.RootIndex) {
		return "", false
	}
	rest, ok := strings.CutPrefix(h.RootIndex[i], prefix)
	if !ok {
		return "", false
	}
	return rest, true
}

// rootByDigest returns the 97-byte root point root_index records for d, or
// nil when the chain never made a root with that digest active.
func rootByDigest(h ChainHead, d []byte) []byte {
	rest, ok := rootRecordOf(h, d)
	if !ok {
		return nil
	}
	pt, _, _ := strings.Cut(rest, ".")
	b, err := decodeB64(pt)
	if err != nil {
		return nil
	}
	return b
}

// checkBindingProof is §2.22's checks 1–4 and 6 (check 4's sig_recovery half
// runs in independentSigs); it hands check 5's segment to the row's
// post-state rule. Failures of checks 1–3 and of check 6's position or key
// clause are transition_invalid, check 6's realm clause is realm_mismatch,
// and check 4 is bad_signature (V3-11).
func checkBindingProof(c *bundleCtx) Reason {
	b := c.pb.bind
	if b == nil || len(c.pb.raw.RecoveryBindSig) == 0 {
		return ReasonTransitionInvalid
	}
	if b.Kind != BindKindBind || !bytes.Equal(c.pb.bindDigest, c.pb.intent.RecoveryBindDigest) { // 1
		return ReasonTransitionInvalid
	}
	if b.RealmID != c.pb.intent.RealmID || b.AccountID != c.pb.intent.AccountID { // 6, realm clause
		return ReasonRealmMismatch
	}
	root, r := bindingRoot(c, b)
	if r != ReasonOK {
		return r
	}
	if !VerifyP384(root, sigMsg(RoleCurrentRoot, c.pb.bindDigest), c.pb.raw.RecoveryBindSig) { // 4
		return ReasonBadSignature
	}
	if r := checkRecoverIndex(c.head, b); r != ReasonOK { // 6
		return r
	}
	c.bindingSegment = segmentOf(c.head, root) // 5
	return ReasonOK
}

// bindingRoot is checks 2 and 3: the root point of the effective state after
// result n = B.chain_seq, which exists only for a position below the recover
// whose seq_index record carries B's entry digest and B's binding root.
func bindingRoot(c *bundleCtx, b *RecoveryBind) ([]byte, Reason) {
	n := b.ChainSeq
	if n == 0 || n >= c.pb.res.Seq { // 2, position: B.chain_seq < the recover's seq
		return nil, ReasonTransitionInvalid
	}
	// The recover's seq is len(seq_index) + 1 (headPub, linkCheck), so record
	// n exists; the bound keeps the read safe without relying on the caller.
	if n > uint64(len(c.head.SeqIndex)) {
		return nil, ReasonTransitionInvalid
	}
	entry, bindRoot, _ := strings.Cut(c.head.SeqIndex[n-1], ".")
	if entry != b64s(b.ChainEntryDigest) { // 2, entry digest
		return nil, ReasonTransitionInvalid
	}
	if bindRoot == "" || bindRoot != b64s(b.BindingRootDigest) { // 3
		return nil, ReasonTransitionInvalid
	}
	root := rootByDigest(c.head, b.BindingRootDigest)
	if root == nil { // 3: a recorded bind root is always in root_index (headmove)
		return nil, ReasonTransitionInvalid
	}
	return root, ReasonOK
}

// checkRecoverIndex is check 6's position and key clauses against every
// activated recover the hypothesis head records (E2-3, D-277, E3-06): B is at
// or after each one's seq, and its key is none of theirs.
func checkRecoverIndex(h ChainHead, b *RecoveryBind) Reason {
	key := b64s(Digest(b.RecoveryKey))
	for _, rec := range h.RecoverIndex {
		seqText, k, _ := strings.Cut(rec, ".")
		s, err := parseU(seqText)
		if err != nil || b.ChainSeq < s || k == key {
			return ReasonTransitionInvalid
		}
	}
	return ReasonOK
}

// checkPendingRecoveryKey is row 21: the cancel's sig_recovery verifies under
// the pending recover's B.recovery_key, which the persisted head keeps as
// pending_recovery_key (D-269, E3-01).
func checkPendingRecoveryKey(c *bundleCtx) Reason {
	k := c.head.PendingRecoveryKey
	if len(k) == 0 {
		return ReasonTransitionInvalid
	}
	if !VerifyP384(k, sigMsg(RoleRecoveryKey, c.pb.intentDigest), c.pb.raw.SigRecovery) {
		return ReasonBadSignature
	}
	return ReasonOK
}
