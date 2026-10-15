package store

import (
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

type normOut struct {
	Activated, NewRoot, SlotCompleted, SlotEmptied, Erased bool
	Visible                                                uint64
}

func floorOf(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

func growth(acc uint64, base *uint64) uint64 {
	if base == nil || acc < *base {
		return 0
	}
	return acc - *base
}

// normalize is step 7, in memory on h. pendingAssurance is the assurance of the
// result that proposed a pending root (it opens the segment at activation).
func normalize(h *headRow, now, acc uint64, pendingAssurance string) (normOut, error) {
	var n normOut
	if h.Held {
		return n, nil // a held head never activates on the clock, and its slot never completes (DR9 c)
	}
	if h.DeviceSet == nil && h.State != ac.StatePending {
		return n, nil // no private set yet and nothing pending: nothing to normalize
	}
	ds, err := privateSet(h)
	if err != nil {
		return n, err
	}
	var completeFirst, actDue bool
	n.Visible, completeFirst, actDue = dueOf(h, ds, now, acc)
	newRoot := actDue && len(h.PendRoot) > 0
	if completeFirst {
		n.SlotCompleted = true
		h.moveSlotToBound(ds)
	}
	if actDue {
		n.Activated, n.NewRoot, n.Erased = true, newRoot, len(h.PendRoot) == 0
		h.activate(n.Visible, pendingAssurance)
	}
	if newRoot && !completeFirst && len(ds.ReplacementBindDigest) > 0 {
		n.SlotEmptied = true
	}
	err = settleSet(h, n, newRoot, completeFirst)
	return n, err
}

// privateSet gives a pending head with no private set an empty one, then decodes it.
func privateSet(h *headRow) (ac.DeviceSet, error) {
	if h.DeviceSet == nil {
		if err := h.setDeviceSet(ac.EmptyDeviceSet(h.RealmID, h.AccountID)); err != nil {
			return ac.DeviceSet{}, err
		}
	}
	return ac.DecodeDeviceSet(h.DeviceSet)
}

// dueOf reads the two clocks: the activation's visible instant, whether the slot
// completes first, and whether the activation is due.
func dueOf(h *headRow, ds ac.DeviceSet, now, acc uint64) (visible uint64, completeFirst, actDue bool) {
	slotAt, slotDue := uint64(0), false
	if h.ReplUnavailBase != nil && len(ds.ReplacementBindDigest) > 0 {
		at, ok := ac.VisibleInstant(ds.ReplacementNbMs, floorOf(h.ReplG), growth(acc, h.ReplUnavailBase), ExtensionCapMs, floorOf(h.ReplFloor))
		slotAt, slotDue = at, ok && now >= at
	}
	if h.State == ac.StatePending {
		v, ok := ac.VisibleInstant(floorOf(h.ActBase), floorOf(h.ActG), growth(acc, h.ActUnavailBase), ExtensionCapMs, floorOf(h.ActFloor))
		visible, actDue = v, ok && now >= v
	}
	completeFirst = slotDue && (!actDue || ac.CompletionFirst(slotAt, visible))
	return visible, completeFirst, actDue
}

// settleSet re-normalizes the private set after a completion or a new root, then
// clears the slot columns once the slot completed or emptied.
func settleSet(h *headRow, n normOut, newRoot, completeFirst bool) error {
	if completeFirst || newRoot {
		next := ac.NormalizeDeviceSet(h.DeviceSet, newRoot, newRoot && h.burnOnActivate, completeFirst, false, false)
		if next == nil {
			return errBadSet
		}
		if err := h.setDeviceSet(next); err != nil {
			return err
		}
	}
	if n.SlotEmptied || n.SlotCompleted {
		h.clearSlotColumns()
	}
	return nil
}
