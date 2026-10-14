package authoritycontract

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// genKeys is one product-root generation (§2.2, LD-3): primaries A and B sign
// its delegations, recoveries A′ and B′ sign the succession to the next.
type genKeys struct{ PA, PB, RA, RB []byte }

func (g genKeys) all() [][]byte { return [][]byte{g.PA, g.PB, g.RA, g.RB} }

// delRec is one retained delegation: T4 compares Msg (D-222), never SigA.
type delRec struct {
	D         Delegation
	Msg, SigA []byte
}

// Trust is a client's realm trust (T1–T5). Build it with NewTrust and
// AddSigned or AddTrust; it is not safe for concurrent mutation. Every method
// needs a Trust from NewTrust except the two halt readers, Halted and
// haltReason, which report a halt for a nil Trust so a verifier holding none
// refuses instead of panicking.
//
// Dual control is positional (§2.6 `signed`): a delegation is retained only
// when sig_a verifies under primary A and sig_b under primary B of its
// generation, a succession only when sig_a verifies under recovery A′ and
// sig_b under recovery B′ of prev_root_gen. No signature is tried under the
// other key, so one key signing both positions is one signer, and no pair
// signs the other pair's object.
//
// Retention (T2, T3) depends only on the object and the keys of generations
// 0 through its own: a succession from prev is judged against generations
// 0..prev, a delegation of gen g against generations 0..g (D-223, A14). Those
// keys are fixed once the generation exists, so retention never depends on
// which other objects arrived first. Two cases sit outside that rule: an
// object of a generation this Trust has not reached is ignored and needs
// delivering again (a trust envelope carries successions first), and in a
// succession fork the branch that becomes prev+1 is the first to arrive, but
// a fork halts permanently (T4). The halt is recomputed from the retained set
// after every admission, so it does not depend on arrival order either: a
// delegation contradiction stops halting once a succession retires its
// generation, and a succession contradiction never does.
type Trust struct {
	realm string
	gens  []genKeys
	succ  map[uint64][][]byte    // prev_root_gen → distinct validly dual-signed msgs
	dels  map[[2]uint64][]delRec // (gen, serial) → distinct valid delegations
	seen  map[string]bool        // SHA-384(msg) of every accepted object (D-222)
	halt  *HaltError
}

func pairwiseDistinct(keys [][]byte) bool {
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if bytes.Equal(keys[i], keys[j]) {
				return false
			}
		}
	}
	return true
}

// NewTrust is T1: the anchors' four keys MUST be pairwise distinct (LD-3).
// The keys are copied, so the caller's slices cannot change the Trust.
func NewTrust(a Anchors) (*Trust, error) {
	if a.Encode() == nil {
		return nil, malformed("anchors")
	}
	g := genKeys{slices.Clone(a.PrimaryA), slices.Clone(a.PrimaryB), slices.Clone(a.RecoveryA), slices.Clone(a.RecoveryB)}
	if !pairwiseDistinct(g.all()) {
		return nil, malformed("anchors keys not pairwise distinct")
	}
	return &Trust{realm: a.RealmID, gens: []genKeys{g},
		succ: map[uint64][][]byte{}, dels: map[[2]uint64][]delRec{}, seen: map[string]bool{}}, nil
}

// keysThrough is the anchor and succession keys of generations 0..last. It
// refuses a generation this Trust has not reached, so no caller can judge a
// key against a set that silently stops short.
func (t *Trust) keysThrough(last uint64) ([][]byte, bool) {
	if last >= uint64(len(t.gens)) {
		return nil, false
	}
	out := make([][]byte, 0, 4*(last+1))
	for _, g := range t.gens[:last+1] {
		out = append(out, g.all()...)
	}
	return out, true
}

// AddSigned admits one `signed` succession or delegation. It returns
// ErrMalformed (EN5, §2.6, or a non-trust type), ErrIgnored (T2/T3, or a
// msg already accepted), or *HaltError when the retained set halts after
// the object is admitted (T4); nil when admitted and not halted. A nil
// Trust admits nothing: ErrMalformed (NewTrust is the only constructor).
func (t *Trust) AddSigned(b []byte) error {
	if t == nil {
		return malformed("nil trust")
	}
	s, err := DecodeSigned(b)
	if err != nil {
		return err
	}
	var add func(Signed) error
	switch s.Type {
	case "succession":
		add = t.addSuccession
	case "delegation":
		add = t.addDelegation
	default:
		return malformed("signed type is not a trust object")
	}
	key := string(Digest(s.Msg))
	if t.seen[key] {
		return ErrIgnored
	}
	if err := add(s); err != nil {
		return err
	}
	t.seen[key] = true
	t.evalHalt()
	if t.halt != nil {
		return t.halt
	}
	return nil
}

// addSuccession is T2.
func (t *Trust) addSuccession(s Signed) error {
	m, err := DecodeSuccession(s.Msg)
	if err != nil {
		return err
	}
	if m.RealmID != t.realm || m.PrevRootGen >= uint64(len(t.gens)) || m.NewRootGen != m.PrevRootGen+1 {
		return ErrIgnored
	}
	g := t.gens[m.PrevRootGen]
	if !VerifyP384(g.RA, s.Msg, s.SigA) || !VerifyP384(g.RB, s.Msg, s.SigB) {
		return ErrIgnored
	}
	next := genKeys{m.NewPrimaryA, m.NewPrimaryB, m.NewRecoveryA, m.NewRecoveryB}
	if !t.freshSuccessor(next, m.PrevRootGen) {
		return ErrIgnored
	}
	t.succ[m.PrevRootGen] = append(t.succ[m.PrevRootGen], s.Msg)
	if m.PrevRootGen == t.CurrentGen() && len(t.succ[m.PrevRootGen]) == 1 {
		t.gens = append(t.gens, next)
	}
	return nil
}

// freshSuccessor is T2's key rule: the four new keys are pairwise distinct and
// equal no key of an earlier generation, which for a succession from prev is
// generations 0..prev. A second succession for prev is judged against that
// same set: judged against every generation this Trust knows, a fork reusing
// one key of the first branch's generation prev+1 would be ignored instead of
// halting (T4), and which branch became prev+1 would depend on arrival order.
func (t *Trust) freshSuccessor(next genKeys, prev uint64) bool {
	nk := next.all()
	if !pairwiseDistinct(nk) {
		return false
	}
	earlier, ok := t.keysThrough(prev)
	if !ok {
		return false
	}
	for _, old := range earlier {
		if slices.ContainsFunc(nk, func(k []byte) bool { return bytes.Equal(k, old) }) {
			return false
		}
	}
	return true
}

// addDelegation is T3.
func (t *Trust) addDelegation(s Signed) error {
	d, err := DecodeDelegation(s.Msg)
	if err != nil {
		return err
	}
	gen := d.ProductRootGen
	if d.RealmID != t.realm || gen >= uint64(len(t.gens)) {
		return ErrIgnored
	}
	g := t.gens[gen]
	if !VerifyP384(g.PA, s.Msg, s.SigA) || !VerifyP384(g.PB, s.Msg, s.SigB) {
		return ErrIgnored
	}
	if !delegationBoundsOK(d) || !t.keySeparationOK(d) {
		return ErrIgnored
	}
	k := [2]uint64{gen, d.Serial}
	t.dels[k] = append(t.dels[k], delRec{D: d, Msg: s.Msg, SigA: s.SigA})
	return nil
}

// delegationBoundsOK is T3's §2.5 bounds (the 400-d lifetime and delta_ms
// bounds included).
func delegationBoundsOK(d Delegation) bool {
	return d.MinValidSerial >= 1 && d.MinValidSerial <= d.Serial &&
		d.NotBeforeMs < d.NotAfterMs && d.NotAfterMs-d.NotBeforeMs <= DelegationLifetimeMaxMs &&
		d.TauMs >= 1 && d.TauMs <= VMaxMs &&
		d.DeltaMs > IntentWindowMs+EpsilonMs && d.DeltaMs <= DeltaMaxMs &&
		d.GMs >= 1 && d.GMs <= d.DeltaMs &&
		slices.Contains(d.AcceptedVersions, "1")
}

// keySeparationOK is §2.4's first two bullets for this delegation alone
// (D-223): its four key arrays are pairwise disjoint and hold no anchor or
// succession key of generations 0..product_root_gen (A14), and no point's
// P-384 SPKI digest is in its cp_tls_pins. The third and fourth bullets (this
// node's keys; no RSK reused across delegations) are never T3.
func (t *Trust) keySeparationOK(d Delegation) bool {
	keys, ok := disjointKeys(d.RSKPublicKeys, d.CAKPublicKeys, d.OperatorPublicKeys, d.AuthorityTLSKeys)
	if !ok {
		return false
	}
	// D-223, A14: generations 0..product_root_gen only, never a later one this
	// Trust happens to hold. Those keys are fixed once the delegation's
	// generation exists, so retention, lookup and HW (T5) are the same in every
	// arrival order. Judged against every known generation, a delegation would
	// be kept before a succession reusing one of its keys and ignored after it.
	anchored, ok := t.keysThrough(d.ProductRootGen)
	if !ok {
		return false
	}
	for _, k := range anchored {
		if keys[string(k)] {
			return false
		}
	}
	return pinsExclude(keys, d.CPTLSPins)
}

// disjointKeys collects the points of arrs, refusing any point seen twice.
func disjointKeys(arrs ...[][]byte) (map[string]bool, bool) {
	seen := map[string]bool{}
	for _, arr := range arrs {
		for _, k := range arr {
			if seen[string(k)] {
				return nil, false
			}
			seen[string(k)] = true
		}
	}
	return seen, true
}

// pinsExclude reports that no point's TLS pin is listed. A point whose pin
// cannot be computed fails closed.
func pinsExclude(points map[string]bool, pins [][]byte) bool {
	listed := make(map[string]bool, len(pins))
	for _, p := range pins {
		listed[string(p)] = true
	}
	for k := range points {
		pin, err := TLSPin([]byte(k))
		if err != nil || listed[string(pin)] {
			return false
		}
	}
	return true
}

func distinctMsgs(recs []delRec) int {
	n := 0
	for i := range recs {
		dup := false
		for j := 0; j < i; j++ {
			if bytes.Equal(recs[i].Msg, recs[j].Msg) {
				dup = true
				break
			}
		}
		if !dup {
			n++
		}
	}
	return n
}

// evalHalt is T4, recomputed from scratch over the retained set so it is
// independent of arrival order. "No valid delegation for current_gen" is
// evaluated at verification time (haltReason), never stored here.
func (t *Trust) evalHalt() {
	t.halt = t.successionHalt()
	if t.halt == nil {
		t.halt = t.delegationHalt()
	}
}

// successionHalt: two successions with different msg for one prev_root_gen,
// whatever generation is current.
func (t *Trust) successionHalt() *HaltError {
	for _, prev := range slices.Sorted(maps.Keys(t.succ)) {
		if len(t.succ[prev]) > 1 {
			return &HaltError{Rule: fmt.Sprintf("T4: two successions for generation %d", prev)}
		}
	}
	return nil
}

// delegationHalt: for gen = current_gen only, two delegations with one
// (gen, serial) and different msg, or a higher serial whose cutover is lower
// than an earlier non-zero cutover. Retired generations never halt.
func (t *Trust) delegationHalt() *HaltError {
	cg := t.CurrentGen()
	keys := slices.SortedFunc(maps.Keys(t.dels), func(a, b [2]uint64) int { return slices.Compare(a[:], b[:]) })
	var cur []Delegation
	for _, k := range keys {
		if k[0] != cg {
			continue
		}
		if distinctMsgs(t.dels[k]) > 1 {
			return &HaltError{Rule: fmt.Sprintf("T4: two delegations for (%d, %d)", k[0], k[1])}
		}
		// [0] is safe here: distinctMsgs above returned ≤ 1, so every retained
		// record at this key is the same delegation and arrival order is moot.
		cur = append(cur, t.dels[k][0].D)
	}
	return cutoverHalt(cur)
}

// cutoverHalt reads cur in ascending serial order.
func cutoverHalt(cur []Delegation) *HaltError {
	for i, lo := range cur {
		if lo.CutoverAtMs == 0 {
			continue
		}
		for _, hi := range cur[i+1:] {
			if hi.CutoverAtMs < lo.CutoverAtMs {
				return &HaltError{Rule: "T4: a higher serial lowers the cutover"}
			}
		}
	}
	return nil
}

// HighWater is T5 over the valid delegations this Trust holds.
//
// The recs[0] read below depends on arrival order only when a (gen, serial)
// holds two different records. In the current generation that is a T4 halt,
// and every verifier checks the halt before it reads the high-water mark. In
// a retired generation it can occur (retired contradictions are ignored), but
// an unhalted trust always holds a delegation for the current generation (see
// haltReason), whose higher generation outranks any retired key in maxHW.
// A nil Trust holds no delegation, so its high-water mark is the zero value.
func (t *Trust) HighWater() HighWater {
	var hw HighWater
	if t == nil {
		return hw
	}
	for k, recs := range t.dels {
		hw = maxHW(hw, HighWater{Gen: k[0], Serial: k[1], MinValid: recs[0].D.MinValidSerial})
	}
	return hw
}

// CurrentGen is the top of the contiguous succession chain (T2). A nil Trust
// reports generation 0; it is halted (Halted), so nothing verifies under it.
func (t *Trust) CurrentGen() uint64 {
	if t == nil || len(t.gens) == 0 {
		return 0
	}
	return uint64(len(t.gens)) - 1
}

// delegations returns every distinct valid delegation retained at (gen,
// serial), ordered by msg bytes so no caller depends on arrival order. More
// than one exists only in a retired generation: T4 halts on a contradiction
// in current_gen. The client-form trust predicates are existential over the
// result (trustcheck.go).
func (t *Trust) delegations(gen, serial uint64) []Delegation {
	recs := slices.Clone(t.dels[[2]uint64{gen, serial}])
	slices.SortFunc(recs, func(a, b delRec) int { return bytes.Compare(a.Msg, b.Msg) })
	recs = slices.CompactFunc(recs, func(a, b delRec) bool { return bytes.Equal(a.Msg, b.Msg) })
	out := make([]Delegation, len(recs))
	for i, r := range recs {
		out[i] = r.D
	}
	return out
}

// lookup returns the delegation at (gen, serial) only when exactly one is
// retained. An ambiguous key reports false rather than whichever record
// arrived first: a caller that needs one of several must select it by the
// signature it verifies (delegations). The Delegation shares its slices with
// the retained record: callers must not mutate it.
func (t *Trust) lookup(gen, serial uint64) (Delegation, bool) {
	ds := t.delegations(gen, serial)
	if len(ds) != 1 {
		return Delegation{}, false
	}
	return ds[0], true
}

// Halted is T4's state, including "no valid delegation for current_gen",
// which is evaluated at verification time, never in AddSigned. A nil Trust
// is halted.
func (t *Trust) Halted() *HaltError {
	if t == nil {
		return &HaltError{Rule: "T4: no trust"}
	}
	if t.halt != nil {
		return t.halt
	}
	if t.haltReason() != ReasonOK {
		return &HaltError{Rule: "T4: no valid delegation for the current generation"}
	}
	return nil
}

func (t *Trust) haltReason() Reason {
	if t == nil || t.halt != nil {
		return ReasonTrustHalt
	}
	cg := t.CurrentGen()
	for k := range t.dels {
		if k[0] == cg {
			return ReasonOK
		}
	}
	return ReasonTrustHalt
}

// trustOrder is §2.6's `trust` ordering: successions by new_root_gen strictly
// ascending, then delegations by (gen, serial) strictly ascending.
type trustOrder struct {
	lastSucc          uint64
	lastDel           [2]uint64
	seenSucc, seenDel bool
}

func (o *trustOrder) next(item []byte) error {
	s, err := DecodeSigned(item)
	if err != nil {
		return err
	}
	switch s.Type {
	case "succession":
		return o.succession(s.Msg)
	case "delegation":
		return o.delegation(s.Msg)
	}
	return malformed("trust item type")
}

func (o *trustOrder) succession(msg []byte) error {
	m, err := DecodeSuccession(msg)
	if err != nil {
		return err
	}
	if o.seenDel || (o.seenSucc && m.NewRootGen <= o.lastSucc) {
		return malformed("trust succession order")
	}
	o.lastSucc, o.seenSucc = m.NewRootGen, true
	return nil
}

func (o *trustOrder) delegation(msg []byte) error {
	d, err := DecodeDelegation(msg)
	if err != nil {
		return err
	}
	k := [2]uint64{d.ProductRootGen, d.Serial}
	if o.seenDel && slices.Compare(k[:], o.lastDel[:]) <= 0 {
		return malformed("trust delegation order")
	}
	o.lastDel, o.seenDel = k, true
	return nil
}

// CheckTrustEnvelope is §2.6's `trust` rule: successions by new_root_gen
// strictly ascending, then delegations by (gen, serial) strictly ascending;
// a duplicate is malformed. On error the envelope is the zero value.
func CheckTrustEnvelope(b []byte) (TrustEnvelope, error) {
	env, err := DecodeTrustEnvelope(b)
	if err != nil {
		return TrustEnvelope{}, err
	}
	var o trustOrder
	for _, it := range env.Items {
		if err := o.next(it); err != nil {
			return TrustEnvelope{}, err
		}
	}
	return env, nil
}

// AddTrust checks a `trust` envelope and admits every item in order. Ignored
// items are expected (a trust file carries every delegation ever issued). An
// intermediate halt does not stop admission, because T4 is a property of the
// retained set: the result is the halt state after the last item. A nil
// Trust admits nothing: ErrMalformed, before the envelope is read (its own
// rule name, so the guard is told apart from AddSigned's).
func (t *Trust) AddTrust(envelope []byte) error {
	if t == nil {
		return malformed("nil trust: envelope not read")
	}
	env, err := CheckTrustEnvelope(envelope)
	if err != nil {
		return err
	}
	for _, it := range env.Items {
		err := t.AddSigned(it)
		var h *HaltError
		if err != nil && !errors.Is(err, ErrIgnored) && !errors.As(err, &h) {
			return err
		}
	}
	if t.halt != nil {
		return t.halt
	}
	return nil
}
