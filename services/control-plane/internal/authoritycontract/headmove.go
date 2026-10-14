package authoritycontract

import (
	"bytes"
	"slices"
	"strings"
)

// §2.12 "When the index moves" (V3-10, E3-06). The persisted chain-head is
// the only state a verifier keeps between calls, so these functions are the
// whole of what one verified result changes.
//
// Invariant. After result n+1 is verified against hypothesis h:
//
//   - if h is H_act of a pending result n, n's activation is recorded first
//     (recordActivation): seq_index record n gains its bind root when the
//     activation yields active, root_index gains n's pending root with n's
//     assurance (A9), the pending EK becomes the active EK, and a recover's
//     key enters recover_index;
//   - then n+1 is appended (appendResult): its seq_index record, its root in
//     root_index when it is active, its EK in ek_index and in the active or
//     pending slot, the pending recovery key of a pending recover, the head
//     result; the fix is dropped (it named the previous head).
//
// The pending EK slot mirrors result #17: it is emptied exactly when the new
// head's pending_ek_binding_digest is "" — every non-pending result and a
// pending erase, which burns a pending candidate (DoR:865, "likewise for
// #17"). Emptying it on "state ≠ pending" instead would keep a burned
// candidate's EK in the slot of a Pe head.
//
// What breaks it: recording an activation on a status fix alone (the index
// moves only on a verified result); recording the activated root with the
// result's own label rather than the segment's; inserting an a<…> element out
// of EN3 order, which makes the head unencodable; and keeping a pending slot
// whose result position is "".
//
// Moving a head without re-encoding it (CWE-400). A chain verification moves
// one head through every result from seq 1 on first contact (D-120), and a
// head may reach HEAD_BLOB_MAX, so no step re-encodes the whole head and no
// step's head outlives the step (chainStep keeps EK slots only). What a step
// still pays is one copy of each index it inserts into, or of seq_index when
// an activation rebinds a record: O(index) per step, O(N²) over a chain.
// Measured at the HEAD_BLOB_MAX ceiling (about 125 000 EK inserts): 110 s
// and 236 GB of short-lived allocation, against about 350 s of P-384
// signature checks for the same chain; #3588's fetch budget bounds N first.
// The upgrade path, if that ratio matters: an append-only overlay per index
// with a digest-keyed map, sorted once when the head is returned (11 s
// measured for in-place inserts).
//
// Aliasing. No step writes an element any head existing before it can see:
// a record is inserted or replaced in a NEW array (insertStr, insertB48,
// replaceAt), never in place, so the persisted head, the H_pend head, a
// refused attempt and every earlier step's head keep their records. The one
// in-place write is appendResult's append to seq_index, which lands beyond
// the end of the head it extends; the caller's head is clipped on entry
// (clipIndexes), so that append reallocates rather than write a caller's
// array, and H_act's seq_index is clipped or private (formHypothesis), so
// H_pend and H_act never extend one array. A refused attempt's head is
// dropped before the next attempt reuses its slot.
//
// E3-08 without re-encoding. A head whose every index record the codec admits,
// with every a<…> strictly ascending (indexLen: "canonical"), stays canonical
// under these edits, because each added record is judged by the codec's own
// writer as it is added and insertStr/insertB48 keep the order and refuse a
// duplicate. headMove accumulates the exact bytes each edit adds, so prev's
// index length plus that sum IS after's index length, and fits answers what
// after.Bytes() != nil answers by re-encoding only the positions outside the
// indexes. What breaks it: an edit that bypasses headMove (its bytes go
// uncounted, so a head over HEAD_BLOB_MAX could be produced), and judging a
// head that was never measured as canonical by the incremental rule (an
// unsorted a<…> lets insertStr miss a duplicate).

// Index position names, as the chain-head layout names them.
const (
	posSeqIndex     = "seq_index"
	posRootIndex    = "root_index"
	posEKIndex      = "ek_index"
	posRecoverIndex = "recover_index"
)

func isIndexPos(name string) bool {
	return name == posSeqIndex || name == posRootIndex || name == posEKIndex || name == posRecoverIndex
}

// indexKind is the kind of the chain-head index position name; an unknown
// name is the zero kind, which every writer refuses.
func indexKind(name string) kind {
	var h ChainHead
	_, fs := h.spec()
	for _, f := range fs {
		if f.name == name {
			return f.k
		}
	}
	return kind{}
}

// headMove is one step's edits to a head's index positions as they are made:
// the bytes they add to the head's encoding and whether any added record is
// one the codec refuses (see the invariant above).
type headMove struct {
	grew int
	bad  bool
}

// itemLen is the encoded length of one record of position name as the
// codec's own writer emits it (`"…"`), or ok false if the codec refuses it.
func itemLen(name string, s string, b []byte) (int, bool) {
	var buf bytes.Buffer
	var err error
	if b != nil {
		err = writeBins(&buf, indexKind(name), [][]byte{b})
	} else {
		err = writeStrings(&buf, indexKind(name), []string{s})
	}
	if err != nil {
		return 0, false
	}
	return buf.Len() - len("[]"), true
}

// added accounts one record added to position name, which held n records:
// the record and, after the first, its separator.
func (m *headMove) added(name string, n int, s string, b []byte) {
	l, ok := itemLen(name, s, b)
	if !ok {
		m.bad = true
		return
	}
	if n > 0 {
		l += len(",")
	}
	m.grew += l
}

// replaced accounts record old replaced by s in position name.
func (m *headMove) replaced(name, old, s string) {
	l, ok := itemLen(name, s, nil)
	if !ok {
		m.bad = true
		return
	}
	m.grew += l - (len(old) + len(`""`))
}

// insertStr inserts s into position name's a<s> and accounts it.
func (m *headMove) insertStr(name string, xs []string, s string) []string {
	out := insertStr(xs, s)
	if len(out) > len(xs) {
		m.added(name, len(xs), s, nil)
	}
	return out
}

// insertB48 inserts b into ek_index and accounts it.
func (m *headMove) insertB48(xs [][]byte, b []byte) [][]byte {
	out := insertB48(xs, b)
	if len(out) > len(xs) {
		m.added(posEKIndex, len(xs), "", b)
	}
	return out
}

// fits answers after.Bytes() != nil for after, a head one step past a
// canonical head whose index positions encode to prevIdx bytes, m being the
// step's edits; it also returns after's index length. It re-encodes only the
// positions outside the indexes. prevIdx < 0 (prev not measured) falls back
// to encoding after whole, and after stays unmeasured. A move that met a
// record it could not read or write (bad) never fits, on either path.
func (m headMove) fits(after ChainHead, prevIdx int) (int, bool) {
	if m.bad {
		return 0, false
	}
	if prevIdx < 0 {
		return -1, after.Bytes() != nil
	}
	rest := after
	rest.SeqIndex, rest.RootIndex, rest.EKIndex, rest.RecoverIndex = nil, nil, nil, nil
	b := rest.Bytes()
	empty, _ := indexLen(rest)
	idx := prevIdx + m.grew
	if b == nil || len(b)-empty+idx > HeadBlobMax {
		return 0, false
	}
	return idx, true
}

// indexLen is the length of h's four index positions within h's encoding,
// and whether h is canonical there: every record one the codec admits and
// every a<…> strictly ascending in the order writeItems emits, so that
// insertStr and insertB48 keep it so. A head ParseChainHead returned is
// canonical, and so is every head a step moves from one.
func indexLen(h ChainHead) (int, bool) {
	if !ascending(h.RootIndex) || !ascending(h.RecoverIndex) || !ascendingB64(h.EKIndex) {
		return 0, false
	}
	n := 0
	_, fs := h.spec()
	for _, f := range fs {
		if !isIndexPos(f.name) {
			continue
		}
		var buf bytes.Buffer
		if writeField(&buf, f) != nil {
			return 0, false
		}
		n += buf.Len()
	}
	return n, true
}

func ascending(xs []string) bool {
	for i := 1; i < len(xs); i++ {
		if xs[i-1] >= xs[i] {
			return false
		}
	}
	return true
}

// ascendingB64 is ascending over the base64 text (EN3).
func ascendingB64(xs [][]byte) bool {
	prev := ""
	for i, x := range xs {
		s := b64s(x)
		if i > 0 && prev >= s {
			return false
		}
		prev = s
	}
	return true
}

// clipIndexes caps h's index slices at their length, so that no edit a
// verification makes can write the backing arrays of the head it was given.
func clipIndexes(h ChainHead) ChainHead {
	h.SeqIndex, h.RootIndex = slices.Clip(h.SeqIndex), slices.Clip(h.RootIndex)
	h.EKIndex, h.RecoverIndex = slices.Clip(h.EKIndex), slices.Clip(h.RecoverIndex)
	return h
}

// insertStr keeps an a<s> strictly ascending bytewise (EN3); a duplicate is a
// no-op. It never writes xs's array: an insertion returns a new one.
func insertStr(xs []string, s string) []string {
	i, found := slices.BinarySearch(xs, s)
	if found {
		return xs
	}
	return slices.Insert(slices.Clip(xs), i, s)
}

// insertB48 keeps ek_index strictly ascending over its base64 text (EN3: the
// order is over the encoded string, not the raw bytes). Like insertStr it
// never writes xs's array.
func insertB48(xs [][]byte, b []byte) [][]byte {
	i, found := slices.BinarySearchFunc(xs, b64s(b), func(x []byte, s string) int { return strings.Compare(b64s(x), s) })
	if found {
		return xs
	}
	return slices.Insert(slices.Clip(xs), i, b)
}

// replaceAt is xs with xs[i] = s in a new array, with room for the one
// append appendResult makes next; xs's array is not written.
func replaceAt(xs []string, i uint64, s string) []string {
	out := make([]string, len(xs), len(xs)+1)
	copy(out, xs)
	out[i] = s
	return out
}

// addRoot records a root the chain made active, once (root_index is an a<s>).
func (m *headMove) addRoot(h *ChainHead, root []byte, seg string) {
	if _, found := rootRecordOf(*h, Digest(root)); found {
		return
	}
	h.RootIndex = m.insertStr(posRootIndex, h.RootIndex, RootRecord(Digest(root), root, seg))
}

// recordActivation is §2.12's "When the index moves" for a pending result n
// whose successor matched H_act: seq_index record n gains its bind root when
// the activation yields active, root_index gains #16 (with the result's
// assurance, decision A9), the pending EK becomes active, and a recover's
// key enters recover_index (E3-01, E3-06). A Pe(x) activation installs no
// root and moves nothing but the (already empty) pending slots.
func recordActivation(h *ChainHead, prev Result, act pub) {
	new(headMove).recordActivation(h, prev, act)
}

func (m *headMove) recordActivation(h *ChainHead, prev Result, act pub) {
	if len(prev.PendingRoot) > 0 {
		if act.State == StateActive {
			m.bindRecord(h, prev.Seq, prev.PendingRoot)
		}
		m.addRoot(h, prev.PendingRoot, prev.Assurance)
		h.ActiveEKSPKI, h.ActiveEKGeneration = h.PendingEKSPKI, h.PendingEKGeneration
		if prev.Op == OpRecover {
			h.RecoverIndex = m.insertStr(posRecoverIndex, h.RecoverIndex, RecoverRecord(prev.Seq, Digest(h.PendingRecoveryKey)))
		}
	}
	h.PendingEKSPKI, h.PendingEKGeneration, h.PendingRecoveryKey = nil, 0, nil
}

// bindRecord gives seq_index record n its bind root. headPub pins
// len(seq_index) to the head's seq, and VerifyBundle and VerifyChain refuse a
// head whose records the codec does not admit, so record n exists and holds a
// "."; a head that reached here without that (a bypassed guard) marks the
// move bad, which fits refuses (malformed), rather than panic (Sec-N3).
func (m *headMove) bindRecord(h *ChainHead, n uint64, root []byte) {
	if n == 0 || n > uint64(len(h.SeqIndex)) {
		m.bad = true
		return
	}
	rec := h.SeqIndex[n-1]
	entry, _, ok := strings.Cut(rec, ".")
	if !ok {
		m.bad = true
		return
	}
	next := entry + "." + b64s(Digest(root))
	h.SeqIndex = replaceAt(h.SeqIndex, n-1, next)
	m.replaced(posSeqIndex, rec, next)
}

// appendResult appends a verified result: its seq_index record (bind root
// #13 when active), #13 in root_index when active (a rotated root takes the
// current segment's assurance, A9), its EK in ek_index and the active or
// pending EK slot, the pending recovery key of a pending recover, the head
// result. The pending EK slot follows #17 (see the invariant above).
func appendResult(h *ChainHead, pb parsedBundle, seg Assurance) {
	new(headMove).appendResult(h, pb, seg)
}

func (m *headMove) appendResult(h *ChainHead, pb parsedBundle, seg Assurance) {
	r := pb.res
	var bindRoot []byte
	if r.State == StateActive {
		bindRoot = Digest(r.ActiveRoot)
		s := string(seg)
		if r.Op == OpEnrollNew {
			s = AssuranceInitial
		}
		m.addRoot(h, r.ActiveRoot, s)
	}
	rec := SeqRecord(pb.entry, bindRoot)
	m.added(posSeqIndex, len(h.SeqIndex), rec, nil)
	h.SeqIndex = append(h.SeqIndex, rec)
	if pb.ekb != nil {
		h.EKIndex = m.insertB48(h.EKIndex, Digest(pb.ekb.EKPublicKey))
		switch r.State {
		case StateActive:
			h.ActiveEKSPKI, h.ActiveEKGeneration = pb.ekb.EKPublicKey, pb.ekb.EKGeneration
		case StatePending:
			h.PendingEKSPKI, h.PendingEKGeneration = pb.ekb.EKPublicKey, pb.ekb.EKGeneration
		}
	}
	if len(r.PendingEKBindingDigest) == 0 {
		h.PendingEKSPKI, h.PendingEKGeneration = nil, 0
	}
	h.PendingRecoveryKey = nil
	if r.Op == OpRecover && r.State == StatePending && pb.bind != nil {
		h.PendingRecoveryKey = pb.bind.RecoveryKey
	}
	h.HeadResult, h.Fix = pb.resBytes, nil
}

// formHypothesis is the head hypothesis h reads: prev itself, or for H_act
// prev with the activation of prevRes recorded. H_act's seq_index is clipped
// first, so H_act's append can never land in the slot H_pend's would (the
// aliasing rule above); m is the activation's edits.
func formHypothesis(prev ChainHead, prevRes Result, h hyp) (ChainHead, headMove) {
	var m headMove
	if !h.act {
		return prev, m
	}
	head := prev
	head.SeqIndex = slices.Clip(head.SeqIndex)
	m.recordActivation(&head, prevRes, h.p)
	return head, m
}
