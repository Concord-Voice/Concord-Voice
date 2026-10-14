package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type setB struct {
	realm, acct string
	active      []DeviceEntry
	pending     *DeviceEntry
	key, dig    []byte
	rkey, rdig  []byte
	nb          uint64
}

func (s setB) bytes(t *testing.T) []byte {
	devs := make([]string, len(s.active))
	for i, e := range s.active {
		devs[i] = e.Dev()
	}
	p := ""
	if s.pending != nil {
		p = s.pending.Dev()
	}
	b := DeviceSet{RealmID: s.realm, AccountID: s.acct, Active: devs, Pending: p, RecoveryKey: s.key,
		RecoveryBindDigest: s.dig, ReplacementKey: s.rkey, ReplacementBindDigest: s.rdig, ReplacementNbMs: s.nb}.Encode()
	require.NotNil(t, b)
	return b
}

func decodeSet(t *testing.T, b []byte) DeviceSet {
	require.NotNil(t, b)
	s, err := DecodeDeviceSet(b)
	require.NoError(t, err)
	return s
}

const tDelta = uint64(259_200_000)

func activeHead() HeadInput {
	return HeadInput{State: StateActive, Seq: 7, EntryDigest: digestOf("e7"), RootDigest: digestOf("root")}
}

func bindB(t *testing.T, s setB, kind string, key []byte) []byte {
	b := RecoveryBind{RealmID: s.realm, AccountID: s.acct, RequestID: newID(), Kind: kind, RecoveryKey: key,
		ChainSeq: 7, ChainEntryDigest: digestOf("e7"), BindingRootDigest: digestOf("root")}
	if kind == BindKindBind {
		b.EscrowPublicKey = testPoint(t)
	}
	out := b.Encode()
	require.NotNil(t, out)
	return out
}

func cancelB(t *testing.T, s setB, key, target []byte) []byte {
	out := RecoveryBind{RealmID: s.realm, AccountID: s.acct, RequestID: newID(), Kind: BindKindCancel, ExpiresAtMs: 9,
		RecoveryKey: key, TargetDigest: target}.Encode()
	require.NotNil(t, out)
	return out
}

func TestDR1DR2DR4(t *testing.T) {
	a, b := testDev(t, 100), testDev(t, 100)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a, b}}
	rot := func(actor string, removed []string, issued uint64) Refusal {
		_, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpRotateEK, Actor: actor, Removed: removed, Head: activeHead()}, issued, tDelta)
		return r
	}
	require.Equal(t, Refusal(""), rot(a.ID, []string{b.ID}, 100))
	require.Equal(t, RefusalDeviceUnknown, rot(newID(), nil, 100), "the actor not in the set")
	require.Equal(t, RefusalDeviceUnknown, rot(a.ID, []string{a.ID}, 100), "the actor in removed")
	require.Equal(t, RefusalDeviceUnknown, rot(a.ID, []string{newID()}, 100), "an unknown removed ID")
	require.Equal(t, RefusalDeviceTooNew, rot(a.ID, nil, 99), "issued_at = mature_at − 1")
	require.Equal(t, Refusal(""), rot(a.ID, nil, 100), "issued_at = mature_at")

	full := s
	full.active = nil
	for i := 0; i < MaxDevices; i++ {
		full.active = append(full.active, testDev(t, 0))
	}
	nd := testDev(t, 0)
	_, r := ApplyDeviceOp(full.bytes(t), DeviceOpInput{Op: OpAddDevice, Actor: full.active[0].ID, New: &nd, Head: activeHead()}, 5, tDelta)
	require.Equal(t, RefusalDeviceLimit, r, "an eleventh device")
	bad := testDev(t, 0)
	bad.ID = newID()
	_, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpAddDevice, Actor: a.ID, New: &bad, Head: activeHead()}, 200, tDelta)
	require.Equal(t, RefusalDeviceUnknown, r, "device_id ≠ DeviceID(dsk)")
	_, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpAddDevice, Actor: a.ID, New: &b, Head: activeHead()}, 200, tDelta)
	require.Equal(t, RefusalDeviceUnknown, r, "the new device already active")
}

func TestDR5DR6(t *testing.T) {
	realm, acct := newID(), newID()
	n := testDev(t, 0)
	out, r := ApplyDeviceOp(EmptyDeviceSet(realm, acct), DeviceOpInput{Op: OpEnrollNew, New: &n,
		Head: HeadInput{State: StateAbsent}}, 1_000, tDelta)
	require.Equal(t, Refusal(""), r)
	set := decodeSet(t, out)
	e, err := ParseDev(set.Active[0])
	require.NoError(t, err)
	require.Equal(t, uint64(1_000), e.MatureAtMs, "the genesis device is mature at once")

	a := testDev(t, 0)
	key := testPoint(t)
	s := setB{realm: realm, acct: acct, active: []DeviceEntry{a}, key: key, dig: digestOf("B0"),
		rkey: testPoint(t), rdig: digestOf("B1"), nb: 50}
	np := testDev(t, 0)
	out, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpReplaceRoot, New: &np, Outcome: "P", ProposingAnbMs: 7_777}, 10, tDelta)
	require.Equal(t, Refusal(""), r)
	set = decodeSet(t, out)
	pe, _ := ParseDev(set.Pending)
	require.Equal(t, uint64(7_777), pe.MatureAtMs, "pending mature at the proposing anb")
	require.NotEmpty(t, set.ReplacementBindDigest, "a pending replace_root keeps the slot (LD-52)")
	out, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpReplaceRoot, New: &np, Outcome: "D"}, 10, tDelta)
	require.Equal(t, Refusal(""), r)
	require.Empty(t, decodeSet(t, out).Pending, "DR6: a D outcome records no pending device")

	rec := s
	bound := bindB(t, s, BindKindBind, key)
	rec.dig = Digest(bound)
	out, r = ApplyDeviceOp(rec.bytes(t), DeviceOpInput{Op: OpRecover, New: &np, Bind: bound, Outcome: "P", ProposingAnbMs: 9}, 10, tDelta)
	require.Equal(t, Refusal(""), r)
	set = decodeSet(t, out)
	require.Empty(t, set.ReplacementBindDigest, "a recover commit empties the slot")
	require.Equal(t, key, set.RecoveryKey, "and keeps the bound key")
	_, r = ApplyDeviceOp(rec.bytes(t), DeviceOpInput{Op: OpRecover, New: &np, Bind: bindB(t, s, BindKindBind, testPoint(t)),
		Outcome: "P"}, 10, tDelta)
	require.Equal(t, RefusalRecoveryKeyUnbound, r, "DR9 (e)")

	out, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpRotateRoot, Actor: a.ID, Head: activeHead()}, 10, tDelta)
	require.Equal(t, Refusal(""), r)
	set = decodeSet(t, out)
	require.Empty(t, set.ReplacementBindDigest, "rotate_root empties the slot (LD-52)")
	require.Equal(t, key, set.RecoveryKey)
	out, _ = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpRotateEK, Actor: a.ID, Head: activeHead()}, 10, tDelta)
	require.NotEmpty(t, decodeSet(t, out).ReplacementBindDigest, "rotate_ek keeps the slot")

	nd := testDev(t, 0)
	out, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpAddDevice, Actor: a.ID, New: &nd, Head: activeHead()}, 10, tDelta)
	require.Equal(t, Refusal(""), r)
	for _, d := range decodeSet(t, out).Active {
		e, _ := ParseDev(d)
		if e.ID == nd.ID {
			require.Equal(t, 10+tDelta, e.MatureAtMs, "add_device matures Δ later")
		}
	}
	_, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpAddDevice, Actor: a.ID, New: &nd, Head: activeHead()}, MaxSafeInt-1, tDelta)
	require.Equal(t, RefusalWrongState, r, "an add_device maturity sum above 2^53−1")
	fu := activeHead()
	fu.ForceUnverified = true
	_, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpAddDevice, Actor: a.ID, New: &nd, Head: fu}, 10, tDelta)
	require.Equal(t, RefusalWrongState, r, "add_device under force_unverified")
}

func TestDR9Bind(t *testing.T) {
	a := testDev(t, 0)
	empty := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}}
	k1, k2 := testPoint(t), testPoint(t)
	apply := func(s setB, b []byte, wait string, prior bool, h HeadInput, issued uint64) (DeviceSet, Refusal) {
		out, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpBindRecovery, Actor: a.ID, Bind: b, BindWait: wait,
			PriorSig: prior, Head: h}, issued, tDelta)
		if r != "" {
			return DeviceSet{}, r
		}
		return decodeSet(t, out), r
	}
	b1 := bindB(t, empty, BindKindBind, k1)
	set, r := apply(empty, b1, BindWaitNow, false, activeHead(), 10)
	require.Equal(t, Refusal(""), r)
	require.Equal(t, k1, set.RecoveryKey, "first bind, now: bound at once")
	set, _ = apply(empty, b1, BindWaitDelta, false, activeHead(), 10)
	require.Equal(t, k1, set.ReplacementKey)
	require.Equal(t, 10+tDelta, set.ReplacementNbMs, "pending until issued + delta_ms")
	set, _ = apply(empty, b1, BindWaitReplace, false, activeHead(), 10)
	require.Equal(t, 10+RecoveryReplaceMs, set.ReplacementNbMs)
	_, r = apply(empty, b1, "", false, activeHead(), 10)
	require.Equal(t, RefusalMalformedForOp, r, "bind_wait \"\" on a first bind")
	_, r = apply(empty, b1, BindWaitDelta, false, activeHead(), MaxSafeInt)
	require.Equal(t, RefusalWrongState, r, "a bind_wait sum above 2^53−1")

	pendingFirst := empty
	pendingFirst.rkey, pendingFirst.rdig, pendingFirst.nb = k1, Digest(b1), 99
	_, r = apply(pendingFirst, bindB(t, empty, BindKindBind, k2), BindWaitDelta, false, activeHead(), 10)
	require.Equal(t, RefusalWrongState, r, "a second first bind while one is pending")

	boundSet := empty
	boundSet.key, boundSet.dig = k1, Digest(b1)
	b2 := bindB(t, empty, BindKindBind, k2)
	set, _ = apply(boundSet, b2, "", true, activeHead(), 10)
	require.Equal(t, k2, set.RecoveryKey, "with the old key: bound at once")
	_, r = apply(boundSet, b2, BindWaitNow, true, activeHead(), 10)
	require.Equal(t, RefusalMalformedForOp, r, "bind_wait on the old-key form")
	set, _ = apply(boundSet, b2, BindWaitDelta, false, activeHead(), 10)
	require.Equal(t, k1, set.RecoveryKey, "root-only refresh: the bound key stays")
	require.Equal(t, k2, set.ReplacementKey)

	ub := bindB(t, empty, BindKindUnbind, nil)
	set, _ = apply(boundSet, ub, "", true, activeHead(), 10)
	require.Empty(t, set.RecoveryKey, "unbind with the old key: none bound")
	set, _ = apply(boundSet, ub, BindWaitNow, false, activeHead(), 10)
	require.Empty(t, set.RecoveryKey, "root-only unbind at now")
	set, _ = apply(boundSet, ub, BindWaitDelta, false, activeHead(), 10)
	require.Equal(t, k1, set.RecoveryKey)
	require.Empty(t, set.ReplacementKey, "a pending unbind")
	require.Equal(t, Digest(ub), set.ReplacementBindDigest)
	_, r = apply(empty, ub, BindWaitNow, false, activeHead(), 10)
	require.Equal(t, RefusalWrongState, r, "an unbind with no key bound")
	refreshing := boundSet
	refreshing.rkey, refreshing.rdig, refreshing.nb = k2, Digest(b2), 50
	_, r = apply(refreshing, ub, BindWaitNow, false, activeHead(), 10)
	require.Equal(t, RefusalWrongState, r, "a root-only unbind while the slot is full")
	_, r = apply(boundSet, ub, "", false, activeHead(), 10)
	require.Equal(t, RefusalMalformedForOp, r, "bind_wait \"\" on a root-only unbind")

	pr := activeHead()
	pr.State = StatePending
	_, r = apply(empty, b1, BindWaitNow, false, pr, 10)
	require.Equal(t, RefusalWrongState, r, "every form refused while a recover is pending")
	_, r = apply(boundSet, ub, BindWaitNow, false, pr, 10)
	require.Equal(t, RefusalWrongState, r, "an unbind is refused while a recover is pending (LD-49)")
	stale := activeHead()
	stale.Seq = 8
	_, r = apply(empty, b1, BindWaitNow, false, stale, 10)
	require.Equal(t, RefusalStaleSeq, r)
	stale = activeHead()
	stale.EntryDigest = digestOf("e8")
	_, r = apply(empty, b1, BindWaitNow, false, stale, 10)
	require.Equal(t, RefusalStaleHead, r, "a B whose chain position is not the head")
	young := empty
	young.active = []DeviceEntry{testDev(t, 11)}
	_, r = ApplyDeviceOp(young.bytes(t), DeviceOpInput{Op: OpBindRecovery, Actor: young.active[0].ID, Bind: b1,
		BindWait: BindWaitNow, Head: activeHead()}, 10, tDelta)
	require.Equal(t, RefusalDeviceTooNew, r, "DR4 on bind_recovery at mature_at − 1")
}

func TestDR9Cancel(t *testing.T) {
	a := testDev(t, 0)
	base := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}}
	k1, k2 := testPoint(t), testPoint(t)
	first := base
	first.rkey, first.rdig, first.nb = k1, digestOf("B1"), 50
	refresh := base
	refresh.key, refresh.dig, refresh.rkey, refresh.rdig, refresh.nb = k1, digestOf("B0"), k2, digestOf("B2"), 50
	cancel := func(s setB, b []byte, root bool, actor string, issued uint64) (DeviceSet, Refusal) {
		out, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpCancelRecoveryBind, Actor: actor, Bind: b, RootForm: root,
			Head: activeHead()}, issued, tDelta)
		if r != "" {
			return DeviceSet{}, r
		}
		return decodeSet(t, out), r
	}
	set, r := cancel(first, cancelB(t, base, k1, nil), true, a.ID, 10)
	require.Equal(t, Refusal(""), r, "the root form cancels a pending first bind")
	require.Empty(t, set.ReplacementBindDigest)
	young := first
	young.active = []DeviceEntry{testDev(t, 11)}
	_, r = cancel(young, cancelB(t, base, k1, nil), true, young.active[0].ID, 10)
	require.Equal(t, RefusalDeviceTooNew, r, "from a device at mature_at − 1")
	_, r = cancel(first, cancelB(t, base, k1, nil), true, newID(), 10)
	require.Equal(t, RefusalDeviceUnknown, r, "from a removed device")
	_, r = cancel(refresh, cancelB(t, base, k2, nil), true, a.ID, 10)
	require.Equal(t, RefusalWrongState, r, "the root form against a pending refresh")
	_, r = cancel(base, cancelB(t, base, k1, nil), true, a.ID, 10)
	require.Equal(t, RefusalWrongState, r, "against an empty slot")
	_, r = cancel(first, cancelB(t, base, k2, nil), true, a.ID, 10)
	require.Equal(t, RefusalWrongState, r, "B.recovery_key ≠ the slot's key")
	_, r = cancel(first, cancelB(t, base, k1, nil), false, "", 10)
	require.Equal(t, RefusalRecoveryKeyUnbound, r, "the sheet form against a pending first bind")
	set, r = cancel(refresh, cancelB(t, base, k1, nil), false, "", 10)
	require.Equal(t, Refusal(""), r, "target \"\" with a refresh pending")
	require.Empty(t, set.ReplacementKey)
	require.Equal(t, k1, set.RecoveryKey)
	_, r = cancel(refresh, cancelB(t, base, k1, digestOf("other")), false, "", 10)
	require.Equal(t, RefusalWrongState, r, "a wrong non-empty digest")
	_, r = cancel(refresh, cancelB(t, base, k2, nil), false, "", 10)
	require.Equal(t, RefusalRecoveryKeyUnbound, r, "a sheet cancel signed by a key that is not bound")
}

func TestNormalizeDeviceSet(t *testing.T) {
	a, p := testDev(t, 0), testDev(t, 500)
	k1, k2 := testPoint(t), testPoint(t)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}, pending: &p, key: k1, dig: digestOf("B0"),
		rkey: k2, rdig: digestOf("B1"), nb: 50}
	set := decodeSet(t, NormalizeDeviceSet(s.bytes(t), true, false, false, false, false))
	require.Equal(t, []string{p.Dev()}, set.Active, "activation drops every old device, keeping mature_at")
	require.Empty(t, set.Pending)
	require.Empty(t, set.ReplacementBindDigest, "activation empties the slot (LD-52)")
	require.Equal(t, k1, set.RecoveryKey, "a replace_root activation keeps the bound key")
	set = decodeSet(t, NormalizeDeviceSet(s.bytes(t), true, true, false, false, false))
	require.Empty(t, set.RecoveryKey, "a Pr activation burns the bound key")
	set = decodeSet(t, NormalizeDeviceSet(s.bytes(t), false, false, true, false, false))
	require.Equal(t, k2, set.RecoveryKey, "a refresh completes")
	require.Equal(t, digestOf("B1"), set.RecoveryBindDigest)
	unb := s
	unb.rkey = nil
	set = decodeSet(t, NormalizeDeviceSet(unb.bytes(t), false, false, true, false, false))
	require.Empty(t, set.RecoveryKey, "a pending unbind completes to no key")
	require.Empty(t, set.RecoveryBindDigest, "and its H(B) is not copied (E3-03)")
	set = decodeSet(t, NormalizeDeviceSet(s.bytes(t), false, false, false, true, true))
	require.Empty(t, set.Pending, "row 22: pending cleared")
	require.Empty(t, set.ReplacementBindDigest, "row 22: the slot emptied")
	require.Equal(t, k1, set.RecoveryKey, "row 22: the bound key kept")
	set = decodeSet(t, NormalizeDeviceSet(s.bytes(t), false, false, false, true, false))
	require.NotEmpty(t, set.ReplacementBindDigest, "a cancel of a replace_root leaves the refresh pending")
}

// DR1: the new device is in neither active nor pending. The pending device's
// own ID is a burned key, so naming it as the new device is device_unknown.
func TestDR1RefusesANewDeviceEqualToThePending(t *testing.T) {
	a, p := testDev(t, 0), testDev(t, 500)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}, pending: &p}
	dup := p
	_, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpAddDevice, Actor: a.ID, New: &dup, Head: activeHead()}, 5, tDelta)
	require.Equal(t, RefusalDeviceUnknown, r, "the new device is the pending device")
	fresh := testDev(t, 0)
	_, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpAddDevice, Actor: a.ID, New: &fresh, Head: activeHead()}, 5, tDelta)
	require.Equal(t, Refusal(""), r, "control: a device in neither set")
}

// DR1 for user unsuspend: the actor must be in the set.
func TestDR1UnsuspendNeedsAnActorInTheSet(t *testing.T) {
	a := testDev(t, 0)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}}
	_, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpUnsuspend, Actor: newID(), Head: activeHead()}, 10, tDelta)
	require.Equal(t, RefusalDeviceUnknown, r)
	_, r = ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpUnsuspend, Actor: a.ID, Head: activeHead()}, 10, tDelta)
	require.Equal(t, Refusal(""), r, "control: a device in the set")
}

// DR4 (DoR:653): the actor's mature_at_ms ≤ issued_at_ms for user unsuspend
// and add_device (LD-40, D-262), as for the rotations: one millisecond short
// is device_too_new, equality is admitted.
// dr4 is fail-closed on its own: called without dr1 for an actor outside the
// active set it refuses device_unknown rather than indexing out of range.
func TestDR4RefusesAnActorOutsideActiveWithoutDR1(t *testing.T) {
	member := testDev(t, 0)
	s := devSet{act: []DeviceEntry{member}}
	for name, c := range map[string]struct {
		s  devSet
		in DeviceOpInput
	}{
		"unsuspend by a stranger":        {s, DeviceOpInput{Op: OpUnsuspend, Actor: newID()}},
		"root-form cancel by a stranger": {s, DeviceOpInput{Op: OpCancelRecoveryBind, RootForm: true, Actor: newID()}},
		"empty active set":               {devSet{}, DeviceOpInput{Op: OpAddDevice, Actor: member.ID}},
	} {
		require.NotPanics(t, func() { require.Equal(t, RefusalDeviceUnknown, c.s.dr4(c.in, 5), name) })
	}
	require.Equal(t, Refusal(""), s.dr4(DeviceOpInput{Op: OpUnsuspend, Actor: member.ID}, 5), "control: a mature member")
}

func TestDR4MaturityOnUnsuspendAndAddDevice(t *testing.T) {
	young := testDev(t, 100)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{young}}
	nd := testDev(t, 0)
	for name, in := range map[string]DeviceOpInput{
		"unsuspend":  {Op: OpUnsuspend, Actor: young.ID, Head: activeHead()},
		"add_device": {Op: OpAddDevice, Actor: young.ID, New: &nd, Head: activeHead()},
	} {
		_, r := ApplyDeviceOp(s.bytes(t), in, 99, tDelta)
		require.Equal(t, RefusalDeviceTooNew, r, name+" at mature_at − 1")
		out, r := ApplyDeviceOp(s.bytes(t), in, 100, tDelta)
		require.Equal(t, Refusal(""), r, name+" at mature_at")
		require.NotNil(t, out, name)
	}
}

// DR2: add_device never takes |active| above MAX_DEVICES, and the tenth
// device is admitted from nine.
func TestDR2AddDeviceAtMaxDevicesMinusOne(t *testing.T) {
	s := setB{realm: newID(), acct: newID()}
	for i := 0; i < MaxDevices-1; i++ {
		s.active = append(s.active, testDev(t, 0))
	}
	nd := testDev(t, 0)
	out, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpAddDevice, Actor: s.active[0].ID, New: &nd, Head: activeHead()}, 5, tDelta)
	require.Equal(t, Refusal(""), r, "the tenth device")
	require.Len(t, decodeSet(t, out).Active, MaxDevices)
}

// DR9 (a): refusals of bind_recovery that no other test names.
func TestDR9BindRefusals(t *testing.T) {
	a := testDev(t, 0)
	empty := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}}
	k1 := testPoint(t)
	b1 := bindB(t, empty, BindKindBind, k1)
	apply := func(s setB, b []byte, wait string, prior bool, h HeadInput) Refusal {
		_, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpBindRecovery, Actor: a.ID, Bind: b, BindWait: wait,
			PriorSig: prior, Head: h}, 10, tDelta)
		return r
	}
	require.Equal(t, Refusal(""), apply(empty, b1, BindWaitNow, false, activeHead()), "control")

	other := activeHead()
	other.RootDigest = digestOf("another root")
	require.Equal(t, RefusalWrongState, apply(empty, b1, BindWaitNow, false, other), "B.binding_root_digest ≠ the head's root")

	cancel := cancelB(t, empty, k1, nil)
	require.Equal(t, RefusalWrongState, apply(empty, cancel, BindWaitNow, false, activeHead()), "a cancel B on bind_recovery")

	require.Equal(t, RefusalWrongState, apply(empty, b1, "", true, activeHead()), "the old-key form with no key bound")
	ub := bindB(t, empty, BindKindUnbind, nil)
	require.Equal(t, RefusalWrongState, apply(empty, ub, "", true, activeHead()), "the old-key unbind with no key bound")
}

// DR9 (a), LD-52: the old-key bind and the old-key unbind empty a full
// pending slot, whatever it held.
func TestDR9OldKeyFormsEmptyAFullSlot(t *testing.T) {
	a := testDev(t, 0)
	k1, k2, k3 := testPoint(t), testPoint(t), testPoint(t)
	full := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}, key: k1, dig: digestOf("B0"),
		rkey: k2, rdig: digestOf("B1"), nb: 50}
	apply := func(b []byte) DeviceSet {
		out, r := ApplyDeviceOp(full.bytes(t), DeviceOpInput{Op: OpBindRecovery, Actor: a.ID, Bind: b, PriorSig: true,
			Head: activeHead()}, 10, tDelta)
		require.Equal(t, Refusal(""), r)
		return decodeSet(t, out)
	}
	set := apply(bindB(t, full, BindKindBind, k3))
	require.Equal(t, k3, set.RecoveryKey, "the new key is bound at once")
	require.Empty(t, set.ReplacementBindDigest, "the slot is emptied")
	require.Empty(t, set.ReplacementKey)
	require.Zero(t, set.ReplacementNbMs)
	set = apply(bindB(t, full, BindKindUnbind, nil))
	require.Empty(t, set.RecoveryKey, "no key bound")
	require.Empty(t, set.ReplacementBindDigest, "the slot is emptied")
	require.Empty(t, set.ReplacementKey)
	require.Zero(t, set.ReplacementNbMs)
}

// DR9 (b): refusals of cancel_recovery_bind beyond the slot and key rules.
func TestDR9CancelRefusals(t *testing.T) {
	a := testDev(t, 0)
	base := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}}
	k1, k2 := testPoint(t), testPoint(t)
	first := base
	first.rkey, first.rdig, first.nb = k1, digestOf("B1"), 50
	refresh := base
	refresh.key, refresh.dig, refresh.rkey, refresh.rdig, refresh.nb = k1, digestOf("B0"), k2, digestOf("B2"), 50
	run := func(s setB, b []byte, root bool, h HeadInput) Refusal {
		actor := ""
		if root {
			actor = a.ID
		}
		_, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpCancelRecoveryBind, Actor: actor, Bind: b, RootForm: root,
			Head: h}, 10, tDelta)
		return r
	}
	require.Equal(t, Refusal(""), run(first, cancelB(t, base, k1, nil), true, activeHead()), "control: root form")
	require.Equal(t, Refusal(""), run(refresh, cancelB(t, base, k1, nil), false, activeHead()), "control: sheet form")

	require.Equal(t, RefusalWrongState, run(first, bindB(t, base, BindKindBind, k1), true, activeHead()), "a bind B, root form")
	require.Equal(t, RefusalWrongState, run(refresh, bindB(t, base, BindKindBind, k1), false, activeHead()), "a bind B, sheet form")

	erased := activeHead()
	erased.State = StateErased
	require.Equal(t, RefusalWrongState, run(first, cancelB(t, base, k1, nil), true, erased), "root form on an erased head")
	require.Equal(t, RefusalWrongState, run(refresh, cancelB(t, base, k1, nil), false, erased), "sheet form on an erased head")
}

// DR9 (e) beside DR6: a recover whose outcome is D is refused, not recorded
// as a D outcome (row 20 has only a P or Pr result).
func TestDR9RecoverRefusesADOutcome(t *testing.T) {
	a := testDev(t, 0)
	key := testPoint(t)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}, key: key}
	bound := bindB(t, s, BindKindBind, key)
	s.dig = Digest(bound)
	np := testDev(t, 0)
	_, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpRecover, New: &np, Bind: bound, Outcome: "P", ProposingAnbMs: 9}, 10, tDelta)
	require.Equal(t, Refusal(""), r, "control: a P outcome")
	out, r := ApplyDeviceOp(s.bytes(t), DeviceOpInput{Op: OpRecover, New: &np, Bind: bound, Outcome: "D"}, 10, tDelta)
	require.Equal(t, RefusalWrongState, r, "a D outcome")
	require.Nil(t, out)
}

// A10 / C4-4: when a completion and an activation are both due, the slot
// completes first, so the replacement key is bound before the activation
// empties the slot.
func TestNormalizeDeviceSetCompletesTheSlotBeforeActivating(t *testing.T) {
	a, p := testDev(t, 0), testDev(t, 500)
	k1, k2 := testPoint(t), testPoint(t)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}, pending: &p, key: k1, dig: digestOf("B0"),
		rkey: k2, rdig: digestOf("B1"), nb: 50}
	set := decodeSet(t, NormalizeDeviceSet(s.bytes(t), true, false, true, false, false))
	require.Equal(t, k2, set.RecoveryKey, "the refresh completed first")
	require.Equal(t, digestOf("B1"), set.RecoveryBindDigest)
	require.Empty(t, set.ReplacementBindDigest, "the slot is empty")
	require.Equal(t, []string{p.Dev()}, set.Active, "and the activation applied")
	require.Empty(t, set.Pending)
	set = decodeSet(t, NormalizeDeviceSet(s.bytes(t), true, true, true, false, false))
	require.Empty(t, set.RecoveryKey, "a Pr activation burns the key the completion bound")
}

// The authority's completion instant (D-274, RT6-9, D-286) and C4-4.
func TestVisibleInstant(t *testing.T) {
	const capMs, g, nb = 259_200_000, 900_000, 1_000_000
	v, ok := VisibleInstant(nb, g, 0, capMs, 0)
	require.True(t, ok)
	require.Equal(t, uint64(nb+g), v, "no unavailability: nb + g")
	v, _ = VisibleInstant(nb, g, 5_000, capMs, 0)
	require.Equal(t, uint64(nb+g+5_000), v, "nb + g + k, and not before")
	v, _ = VisibleInstant(nb, g, capMs+1, capMs, 0)
	require.Equal(t, uint64(nb+g+capMs), v, "the extension is capped")
	v, _ = VisibleInstant(nb, g, 0, capMs, nb+g+10)
	require.Equal(t, uint64(nb+g+10), v, "not before the floor")
	_, ok = VisibleInstant(MaxSafeInt, g, 0, capMs, 0)
	require.False(t, ok)
	require.True(t, CompletionFirst(10, 10), "a tie completes first (decision A10)")
	require.True(t, CompletionFirst(9, 10))
	require.False(t, CompletionFirst(11, 10))
}

// A device-intent always names the new device of a proposing or admitting op
// (§2.5 #14–#16); an input without one is refused, never dereferenced.
func TestApplyDeviceOpRefusesAMissingNewDevice(t *testing.T) {
	a := testDev(t, 0)
	key := testPoint(t)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}, key: key}
	bound := bindB(t, s, BindKindBind, key)
	s.dig = Digest(bound)
	for name, in := range map[string]DeviceOpInput{
		"enroll_new":        {Op: OpEnrollNew, Head: HeadInput{State: StateAbsent}},
		"add_device":        {Op: OpAddDevice, Actor: a.ID, Head: activeHead()},
		"enroll_existing P": {Op: OpEnrollExisting, Outcome: "P", ProposingAnbMs: 9},
		"replace_root P":    {Op: OpReplaceRoot, Outcome: "P", ProposingAnbMs: 9},
		"replace_root D":    {Op: OpReplaceRoot, Outcome: "D"},
		"recover":           {Op: OpRecover, Bind: bound, Outcome: "P", ProposingAnbMs: 9},
	} {
		set := s.bytes(t)
		if in.Op == OpEnrollNew {
			set = EmptyDeviceSet(s.realm, s.acct)
		}
		out, r := ApplyDeviceOp(set, in, 10, tDelta)
		require.Equal(t, RefusalMalformedForOp, r, name)
		require.Nil(t, out, name)
	}
}

// TestDR9BindPositionsAreMalformedForOp pins step 6's malformed_for_op for a B
// that decodes but breaks its kind's positions (DoR §2.22): a bind B with no
// escrow key. Nothing later in the bind path checks the escrow key.
func TestDR9BindPositionsAreMalformedForOp(t *testing.T) {
	a := testDev(t, 0)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}}
	in := func(b []byte) DeviceOpInput {
		return DeviceOpInput{Op: OpBindRecovery, Actor: a.ID, Bind: b, BindWait: BindWaitNow, Head: activeHead()}
	}
	_, r := ApplyDeviceOp(s.bytes(t), in(bindB(t, s, BindKindBind, testPoint(t))), 10, tDelta)
	require.Equal(t, Refusal(""), r, "control: a well-formed first bind")
	noEscrow := RecoveryBind{RealmID: s.realm, AccountID: s.acct, RequestID: newID(), Kind: BindKindBind,
		RecoveryKey: testPoint(t), ChainSeq: 7, ChainEntryDigest: digestOf("e7"), BindingRootDigest: digestOf("root")}.Encode()
	require.NotNil(t, noEscrow)
	out, r := ApplyDeviceOp(s.bytes(t), in(noEscrow), 10, tDelta)
	require.Equal(t, RefusalMalformedForOp, r, "a bind B with no escrow key")
	require.Nil(t, out)
}

// TestDR9BindRecoveryRefusedUnderForceUnverified pins the head gate for
// bind_recovery (DR9 (a): an Ac head, not force_unverified), which is
// separate from add_device's.
func TestDR9BindRecoveryRefusedUnderForceUnverified(t *testing.T) {
	a := testDev(t, 0)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a}}
	b := bindB(t, s, BindKindBind, testPoint(t))
	in := DeviceOpInput{Op: OpBindRecovery, Actor: a.ID, Bind: b, BindWait: BindWaitNow, Head: activeHead()}
	_, r := ApplyDeviceOp(s.bytes(t), in, 10, tDelta)
	require.Equal(t, Refusal(""), r, "control: an Ac head")
	in.Head.ForceUnverified = true
	_, r = ApplyDeviceOp(s.bytes(t), in, 10, tDelta)
	require.Equal(t, RefusalWrongState, r, "bind_recovery under force_unverified")
}
