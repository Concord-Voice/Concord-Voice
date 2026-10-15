package store

import (
	"bytes"
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func pendingHead(base, g, accBase uint64) *headRow {
	set := ac.EmptyDeviceSet(testRealm, testAcct)
	sd, _ := ac.DeviceSetDigest(set)
	return &headRow{AccountID: testAcct, State: ac.StatePending, Prior: ac.StateActive, Seq: 2, Entry: d48(2),
		Root: p97(1), EK: d48(3), EKGen: 1, PendRoot: p97(2), PendEK: d48(4), PendEKGen: 2, RG: 1,
		ActBase: u64p(base), ActG: u64p(g), ActUnavailBase: u64p(accBase), DeviceSet: set, DeviceSetDigest: sd,
		Segment: ac.AssuranceInitial, SegmentRoot: p97(1)}
}

func TestActivationIsVisibleAtBasePlusGNotBefore(t *testing.T) {
	h := pendingHead(1000, 100, 0)
	if n, _ := normalize(h, 1099, 0, ac.AssuranceRecovery); n.Activated || h.State != ac.StatePending {
		t.Fatal("activated at visible − 1 (effective, not effective + G)")
	}
	n, err := normalize(h, 1100, 0, ac.AssuranceRecovery)
	if err != nil || !n.Activated || !n.NewRoot || h.State != ac.StateActive || !bytes.Equal(h.Root, p97(2)) ||
		h.EKGen != 2 || h.Segment != ac.AssuranceRecovery || !bytes.Equal(h.SegmentRoot, p97(2)) || h.ActBase != nil {
		t.Fatalf("at visible: %+v, head %+v", n, *h)
	}
}

func TestActivationExtensionAndCap(t *testing.T) {
	h := pendingHead(1000, 100, 50)
	if n, _ := normalize(h, 1100+29, 80, ""); n.Activated { // acc grew by 30 → visible 1130
		t.Fatal("the accumulator extension was not applied")
	}
	h = pendingHead(1000, 100, 0)
	big := ExtensionCapMs + 5
	if n, _ := normalize(h, 1100+ExtensionCapMs, big, ""); !n.Activated {
		t.Fatal("the extension was not capped at EXTENSION_CAP_MS")
	}
}

func TestActivationFloorIsGPrime(t *testing.T) {
	h := pendingHead(1000, 100, 0)
	h.ActFloor = u64p(5000)
	if n, _ := normalize(h, 4999, 0, ""); n.Activated {
		t.Fatal("activated below activation_floor_ms (G′)")
	}
	if n, _ := normalize(h, 5000, 0, ""); !n.Activated {
		t.Fatal("did not activate at the floor")
	}
}

func TestHeldNeverActivatesAndNeverCompletesTheSlot(t *testing.T) {
	h := pendingHead(1000, 100, 0)
	h.Held = true
	if n, _ := normalize(h, 1<<50, 0, ""); n.Activated || n.SlotCompleted {
		t.Fatal("a held head activated on the clock")
	}
}

func TestPeActivatesToErasedWithTheRootKept(t *testing.T) {
	h := pendingHead(1000, 100, 0)
	h.PendRoot, h.PendEK, h.PendEKGen, h.PendErase = nil, nil, 0, true
	n, _ := normalize(h, 1100, 0, "")
	if !n.Activated || !n.Erased || h.State != ac.StateErased || !bytes.Equal(h.Root, p97(1)) || h.ErasedAtMs != 1100 || h.PendErase {
		t.Fatalf("Pe at visible: %+v", *h)
	}
}

func TestActivationFromSorRemovesOperatorOnlyAndClearsForceUnverified(t *testing.T) {
	h := pendingHead(1000, 100, 0)
	h.Prior, h.SuspOp, h.SuspUser, h.RootRevoked, h.ForceUnverified = ac.StateSuspended, true, true, true, true
	n, _ := normalize(h, 1100, 0, ac.AssuranceUnverified)
	if !n.Activated || h.State != ac.StateSuspended || h.SuspOp || !h.SuspUser || h.RootRevoked || h.ForceUnverified {
		t.Fatalf("P(suspended) from Sor: %+v", *h)
	}
}

func slotHead(t *testing.T, nb, g, accBase uint64) *headRow {
	h := pendingHead(1000, 100, 0)
	h.State, h.Prior, h.PendRoot, h.PendEK, h.PendEKGen, h.ActBase, h.ActG, h.ActUnavailBase = ac.StateActive, "", nil, nil, 0, nil, nil, nil
	set := ac.DeviceSet{RealmID: testRealm, AccountID: testAcct, Active: []string{}, ReplacementKey: p97(9),
		ReplacementBindDigest: d48(9), ReplacementNbMs: nb}.Encode()
	if set == nil {
		t.Fatal("slot set does not encode")
	}
	h.DeviceSet, h.ReplUnavailBase, h.ReplG, h.ReplacementBindAssurance = set, u64p(accBase), u64p(g), ac.AssuranceInitial
	return h
}

func TestSlotCompletesAtNbPlusGPlusGrowth(t *testing.T) {
	h := slotHead(t, 2000, 100, 10)
	if n, _ := normalize(h, 2129, 40, ""); n.SlotCompleted { // growth 30 → 2130
		t.Fatal("the slot completed before nb + g + growth (RT6-9)")
	}
	n, _ := normalize(h, 2130, 40, "")
	set, _ := ac.DecodeDeviceSet(h.DeviceSet)
	if !n.SlotCompleted || !bytes.Equal(set.RecoveryKey, p97(9)) || h.RecoveryBindAssurance != ac.AssuranceInitial ||
		h.ReplacementBindAssurance != "" || h.ReplUnavailBase != nil || h.ReplG != nil {
		t.Fatalf("slot completion: %+v, set %+v", *h, set)
	}
}

func TestCompletionAndActivationTieCompletesFirst(t *testing.T) {
	h := slotHead(t, 1000, 100, 0)
	h.State, h.Prior, h.PendRoot, h.PendEK, h.PendEKGen = ac.StatePending, ac.StateActive, p97(2), d48(4), 2
	h.ActBase, h.ActG, h.ActUnavailBase = u64p(1000), u64p(100), u64p(0)
	n, _ := normalize(h, 1100, 0, ac.AssuranceRecovery)
	set, _ := ac.DecodeDeviceSet(h.DeviceSet)
	if !n.SlotCompleted || !n.Activated || !bytes.Equal(set.RecoveryKey, p97(9)) {
		t.Fatalf("C4-4 tie: completed %v activated %v, bound key %x (completion applies first)", n.SlotCompleted, n.Activated, set.RecoveryKey)
	}
}
