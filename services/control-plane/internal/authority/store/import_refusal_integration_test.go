package store

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// The refusals of the published import replay (import.go) that hold an
// account: an item that authenticates and links, and that the commit core's own
// decision still refuses or does not reproduce. Each row asserts the exact
// reason the importer holds the account for (replayWhy), that only that account
// is held, and that no event row is written.

// flipAttached is the item with mutate applied to its attached device entry.
func flipAttached(t *testing.T, it ac.ImportItem, mutate func(*ac.DeviceBundle)) ac.ImportItem {
	t.Helper()
	db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&db)
	if it.DeviceBundle = db.Encode(); it.DeviceBundle == nil {
		t.Fatal("forge: the attached entry does not encode")
	}
	return it
}

func TestImportReplayRefusals(t *testing.T) {
	suspend := func(t *testing.T, e *env, a *authoritytest.Account) ac.ImportItem {
		e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
		return retained(t, e, a.ID, 2)
	}
	rotate := func(t *testing.T, e *env, a *authoritytest.Account) ac.ImportItem {
		e.commitOK(a, a.RotateRoot(t).Bytes)
		it := retained(t, e, a.ID, 2)
		if len(it.DeviceBundle) == 0 {
			t.Fatal("fixture: the rotate_root item carries no attached device entry")
		}
		return it
	}
	for _, tc := range []struct {
		name  string
		why   string
		base  func(*testing.T, *env, *authoritytest.Account) ac.ImportItem
		forge func(*testing.T, *env, *authoritytest.Account, ac.ImportItem) ac.ImportItem
	}{
		{"cp_evidence that does not decode", "the item does not rebuild a submit", suspend,
			func(_ *testing.T, _ *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				it.CPEvidence = []byte("not an evidence")
				return it
			}},
		{"an attached entry whose device result is not the commit core's", "the attached entry is not the commit core's", rotate,
			func(t *testing.T, e *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				// Signed by the realm's RSK over a device result with another set digest.
				out := forgeDeviceResultOf(t, e, it, func(dr *ac.DeviceResult) { dr.DeviceSetDigest = flipped(dr.DeviceSetDigest) })
				out.Bundle = it.Bundle
				return out
			}},
		{"an attached entry whose signature does not verify", "the attached entry's signature does not verify", rotate,
			func(t *testing.T, _ *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return flipAttached(t, it, func(db *ac.DeviceBundle) { db.DeviceResultSig = flipped(db.DeviceResultSig) })
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			a, _ := e.enrolledWith()
			snapshotAll(t, e, "imp")
			it := tc.forge(t, e, a, tc.base(t, e, a))
			rewind(t, e, "imp", a)
			beginImport(t, e, restorePoint{})
			events, devices := eventsView(t, e, a.ID), deviceEventsView(t, e, a.ID)

			if why := replayWhy(t, e, it); why != tc.why {
				t.Fatalf("the replay refused for %q, want %q", why, tc.why)
			}
			importOK(t, e, importBatch(t, it))
			requireHeld(t, e, a.ID)
			if got := eventsView(t, e, a.ID); got != events {
				t.Fatalf("published events after the refusal = %q, want %q", got, events)
			}
			if got := deviceEventsView(t, e, a.ID); got != devices {
				t.Fatalf("device entries after the refusal = %q, want %q", got, devices)
			}
		})
	}
}
