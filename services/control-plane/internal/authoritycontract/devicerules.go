package authoritycontract

import (
	"bytes"
	"slices"
)

// EmptyDeviceSet is the genesis private set: no device and no recovery state.
func EmptyDeviceSet(realm, account string) []byte {
	return DeviceSet{RealmID: realm, AccountID: account, Active: []string{}}.Encode()
}

type devSet struct {
	DeviceSet
	act  []DeviceEntry
	pend *DeviceEntry
}

func parseSet(b []byte) (devSet, error) {
	ds, err := DecodeDeviceSet(b)
	if err != nil {
		return devSet{}, err
	}
	s := devSet{DeviceSet: ds}
	for _, d := range ds.Active {
		e, err := ParseDev(d)
		if err != nil {
			return devSet{}, err
		}
		s.act = append(s.act, e)
	}
	if ds.Pending != "" {
		e, err := ParseDev(ds.Pending)
		if err != nil {
			return devSet{}, err
		}
		s.pend = &e
	}
	return s, nil
}

func (s devSet) encode() []byte {
	ds := s.DeviceSet
	ds.Active = make([]string, len(s.act))
	for i, e := range s.act {
		ds.Active[i] = e.Dev()
	}
	ds.Pending = ""
	if s.pend != nil {
		ds.Pending = s.pend.Dev()
	}
	return ds.Encode()
}

func (s *devSet) activeIdx(id string) int {
	return slices.IndexFunc(s.act, func(e DeviceEntry) bool { return e.ID == id })
}

func (s *devSet) clearSlot() {
	s.ReplacementKey, s.ReplacementBindDigest, s.ReplacementNbMs = nil, nil, 0
}
func (s *devSet) unbindKey() { s.RecoveryKey, s.RecoveryBindDigest = nil, nil }

func waitMs(w string, deltaMs uint64) (uint64, bool) {
	switch w {
	case BindWaitNow, "":
		return 0, true
	case BindWaitDelta:
		return deltaMs, true
	case BindWaitReplace:
		return RecoveryReplaceMs, true
	}
	return 0, false
}

// usesActor is DR1/DR4's set: ops with an actor device (the sheet-form
// cancel has none).
func usesActor(in DeviceOpInput) bool {
	if in.Op == OpCancelRecoveryBind {
		return in.RootForm
	}
	return actorOpName(in.Op)
}

// ApplyDeviceOp is the shared DR1, DR2, DR4–DR6 and DR9 (a), (b), (e) code
// (D-270): the authority's step 8a and VerifyDeviceLedger call it. DR3's
// signatures are the caller's, checked between DR1 and DR4. set is never
// nil: the genesis set is EmptyDeviceSet.
func ApplyDeviceOp(set []byte, in DeviceOpInput, issuedAtMs, deltaMs uint64) ([]byte, Refusal) {
	s, err := parseSet(set)
	if err != nil {
		return nil, RefusalWrongState
	}
	b, r := preCheck(in)
	if r != "" {
		return nil, r
	}
	if r := headGate(in); r != "" {
		return nil, r
	}
	if r := s.dr1(in); r != "" {
		return nil, r
	}
	if r := s.dr4(in, issuedAtMs); r != "" {
		return nil, r
	}
	if in.Op == OpAddDevice && len(s.act) >= MaxDevices {
		return nil, RefusalDeviceLimit
	}
	if r := s.body(in, b, issuedAtMs, deltaMs); r != "" {
		return nil, r
	}
	out := s.encode()
	if out == nil {
		return nil, RefusalWrongState
	}
	return out, ""
}

// preCheck is step 6's malformed_for_op for the recovery ops: B's per-kind
// positions and the bind_wait pattern (DR9 (a)).
func preCheck(in DeviceOpInput) (RecoveryBind, Refusal) {
	if in.Op != OpBindRecovery && in.Op != OpCancelRecoveryBind && in.Op != OpRecover {
		return RecoveryBind{}, ""
	}
	b, err := DecodeRecoveryBind(in.Bind)
	if err != nil {
		return b, RefusalWrongState
	}
	if RecoveryBindForKind(b) != "" {
		return b, RefusalMalformedForOp
	}
	if in.Op == OpBindRecovery && in.PriorSig == (in.BindWait != "") {
		return b, RefusalMalformedForOp
	}
	return b, ""
}

func headGate(in DeviceOpInput) Refusal {
	switch in.Op {
	case OpAddDevice, OpBindRecovery:
		if in.Head.State != StateActive || in.Head.ForceUnverified {
			return RefusalWrongState
		}
	case OpCancelRecoveryBind:
		if in.Head.State == StateErased {
			return RefusalWrongState
		}
	}
	return ""
}

func (s *devSet) dr1(in DeviceOpInput) Refusal {
	if usesActor(in) {
		if s.activeIdx(in.Actor) < 0 {
			return RefusalDeviceUnknown
		}
		for _, id := range in.Removed {
			if id == in.Actor || s.activeIdx(id) < 0 {
				return RefusalDeviceUnknown
			}
		}
	}
	if in.New != nil {
		if s.activeIdx(in.New.ID) >= 0 || (s.pend != nil && s.pend.ID == in.New.ID) || DeviceID(in.New.DSK) != in.New.ID {
			return RefusalDeviceUnknown
		}
	}
	return ""
}

func (s *devSet) dr4(in DeviceOpInput, issued uint64) Refusal {
	if !usesActor(in) {
		return ""
	}
	// dr1 runs first and refuses an actor outside active; this guard keeps dr4
	// fail-closed on its own rather than indexing on that ordering.
	i := s.activeIdx(in.Actor)
	if i < 0 {
		return RefusalDeviceUnknown
	}
	if s.act[i].MatureAtMs > issued {
		return RefusalDeviceTooNew
	}
	return ""
}

func (s *devSet) body(in DeviceOpInput, b RecoveryBind, issued, delta uint64) Refusal {
	switch in.Op {
	case OpEnrollNew:
		if in.New == nil {
			return RefusalMalformedForOp
		}
		n := *in.New
		n.MatureAtMs = issued
		s.act, s.pend = []DeviceEntry{n}, nil
	case OpEnrollExisting, OpReplaceRoot, OpRecover:
		return s.propose(in)
	case OpRotateRoot, OpRotateEK:
		s.act = slices.DeleteFunc(s.act, func(e DeviceEntry) bool { return slices.Contains(in.Removed, e.ID) })
		if in.Op == OpRotateRoot {
			s.clearSlot()
		}
	case OpUnsuspend:
	case OpAddDevice:
		m, ok := addMs(issued, delta)
		if !ok {
			return RefusalWrongState
		}
		if in.New == nil {
			return RefusalMalformedForOp
		}
		n := *in.New
		n.MatureAtMs = m
		s.act = append(s.act, n)
	case OpBindRecovery:
		return s.bind(in, b, issued, delta)
	case OpCancelRecoveryBind:
		return s.cancel(in, b)
	default:
		return RefusalWrongState
	}
	return ""
}

func (s *devSet) propose(in DeviceOpInput) Refusal {
	if in.New == nil { // #14–#16 are set on every proposing op (§2.5)
		return RefusalMalformedForOp
	}
	if in.Op == OpRecover {
		if len(s.RecoveryBindDigest) == 0 || !bytes.Equal(Digest(in.Bind), s.RecoveryBindDigest) {
			return RefusalRecoveryKeyUnbound
		}
		if in.Outcome != "P" {
			return RefusalWrongState
		}
	}
	switch in.Outcome {
	case "P":
		n := *in.New
		n.MatureAtMs = in.ProposingAnbMs
		s.pend = &n
		if in.Op == OpRecover {
			s.clearSlot()
		}
	case "D":
		s.pend = nil
	default:
		return RefusalWrongState
	}
	return ""
}

func (s *devSet) bind(in DeviceOpInput, b RecoveryBind, issued, delta uint64) Refusal {
	if b.Kind == BindKindCancel {
		return RefusalWrongState
	}
	if b.ChainSeq != in.Head.Seq {
		return RefusalStaleSeq
	}
	if !bytes.Equal(b.ChainEntryDigest, in.Head.EntryDigest) {
		return RefusalStaleHead
	}
	if !bytes.Equal(b.BindingRootDigest, in.Head.RootDigest) {
		return RefusalWrongState
	}
	wait, ok := waitMs(in.BindWait, delta)
	if !ok {
		return RefusalWrongState
	}
	nb, ok := addMs(issued, wait)
	if !ok {
		return RefusalWrongState
	}
	h := Digest(in.Bind)
	bound, slot, now := len(s.RecoveryKey) > 0, len(s.ReplacementBindDigest) > 0, in.BindWait == BindWaitNow
	switch {
	case b.Kind == BindKindUnbind && !bound:
		return RefusalWrongState
	case b.Kind == BindKindUnbind && in.PriorSig:
		s.unbindKey()
		s.clearSlot()
	case b.Kind == BindKindUnbind && slot:
		return RefusalWrongState
	case b.Kind == BindKindUnbind && now:
		s.unbindKey()
	case b.Kind == BindKindUnbind:
		s.ReplacementKey, s.ReplacementBindDigest, s.ReplacementNbMs = nil, h, nb
	case in.PriorSig && !bound:
		return RefusalWrongState
	case in.PriorSig:
		s.RecoveryKey, s.RecoveryBindDigest = b.RecoveryKey, h
		s.clearSlot()
	case slot:
		return RefusalWrongState
	case now:
		s.RecoveryKey, s.RecoveryBindDigest = b.RecoveryKey, h
	default:
		s.ReplacementKey, s.ReplacementBindDigest, s.ReplacementNbMs = b.RecoveryKey, h, nb
	}
	return ""
}

func (s *devSet) cancel(in DeviceOpInput, b RecoveryBind) Refusal {
	slot := len(s.ReplacementBindDigest) > 0
	if b.Kind != BindKindCancel || !slot || (len(b.TargetDigest) > 0 && !bytes.Equal(b.TargetDigest, s.ReplacementBindDigest)) {
		return RefusalWrongState
	}
	bound := len(s.RecoveryKey) > 0
	if in.RootForm {
		if bound || !bytes.Equal(b.RecoveryKey, s.ReplacementKey) {
			return RefusalWrongState
		}
	} else if !bound || !bytes.Equal(b.RecoveryKey, s.RecoveryKey) {
		return RefusalRecoveryKeyUnbound
	}
	s.clearSlot()
	return ""
}

// NormalizeDeviceSet is DR8 and DR9 (c): entry-less changes, applied in the
// order clearPending, emptySlot, completeSlot, activate, burnRecovery.
// It returns nil for a set that does not decode.
//
// activate is passed only for a new-root activation (a derived activation that
// installs a new root); a Pe activation, which keeps the root, never passes it.
//
// The order is fixed: with both completeSlot and activate set, the slot always
// completes before the activation burns it. That is C4-4's answer only when
// the slot's completion comes first (CompletionFirst, a tie included). When the
// activation comes first the caller must pass completeSlot false, and the
// activation empties the slot (LD-52); passing both lets a slot the activation
// should have burned complete.
func NormalizeDeviceSet(set []byte, activate, burnRecovery, completeSlot, clearPending, emptySlot bool) []byte {
	s, err := parseSet(set)
	if err != nil {
		return nil
	}
	if clearPending {
		s.pend = nil
	}
	if emptySlot {
		s.clearSlot()
	}
	if completeSlot && len(s.ReplacementBindDigest) > 0 {
		if len(s.ReplacementKey) > 0 {
			s.RecoveryKey, s.RecoveryBindDigest = s.ReplacementKey, s.ReplacementBindDigest
		} else {
			s.unbindKey()
		}
		s.clearSlot()
	}
	if activate {
		if s.pend != nil {
			s.act, s.pend = []DeviceEntry{*s.pend}, nil
		}
		s.clearSlot()
	}
	if burnRecovery {
		s.unbindKey()
	}
	return s.encode()
}

// VisibleInstant is §3.2.4 step 7's max(base + min(extCap, growth) + g, floor):
// a pending head's visible instant (base = asserted + Δ) and a pending
// slot's completion instant (base = replacement_nb_ms). ok is false on
// overflow. EXTENSION_CAP_MS is the authority's (B1) and arrives as extCapMs.
func VisibleInstant(baseMs, gMs, growthMs, extCapMs, floorMs uint64) (uint64, bool) {
	v, ok := addMs(baseMs, min(growthMs, extCapMs), gMs)
	if !ok {
		return 0, false
	}
	return max(v, floorMs), true
}

// CompletionFirst is C4-4: the earlier instant applies first; a tie
// completes first (decision A10).
func CompletionFirst(slotCompletesAtMs, activationVisibleMs uint64) bool {
	return slotCompletesAtMs <= activationVisibleMs
}
