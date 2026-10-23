package store

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// An entry at or below the head's dseq is present when the stored entry at that
// dseq has its digest (skipped, nothing held), and a fork otherwise: another
// entry at that dseq, or none stored there at all.
func TestImportDevicePresentOrFork(t *testing.T) {
	for _, tc := range []struct {
		name string
		why  string
		held bool
		make func(*testing.T, *env, ac.ImportItem) ac.ImportItem
	}{
		{"the stored entry", "", false, func(_ *testing.T, _ *env, it ac.ImportItem) ac.ImportItem { return it }},
		{"another entry at that dseq", "a fork: another entry at that seq", true,
			func(t *testing.T, e *env, it ac.ImportItem) ac.ImportItem {
				return forgeDeviceEvidence(t, e, it, func(*ac.CPEvidence) {})
			}},
		{"no stored entry at that dseq", "a fork: no stored event at that seq", true,
			func(t *testing.T, e *env, it ac.ImportItem) ac.ImportItem {
				// The head says dseq 2 and the entry is gone: a replica-role removal
				// skips the append-only guard, as restoreAccounts does.
				mustExec(t, e.f.Super, `BEGIN; SET LOCAL session_replication_role = replica;
					DELETE FROM authority.device_events WHERE dseq = 2; COMMIT`)
				return it
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			a, _ := e.enrolledWith()
			e.deviceOK(a, a.AddDevice(t, authoritytest.NewDevice(t)).Bytes) // d2, the head's
			it := tc.make(t, e, retainedPrivate(t, e, a.ID, 2))
			beginImport(t, e, restorePoint{})
			events, devices := eventsView(t, e, a.ID), deviceEventsView(t, e, a.ID)

			if why := replayWhy(t, e, it); why != tc.why {
				t.Fatalf("the replay answered %q, want %q", why, tc.why)
			}
			importOK(t, e, importBatch(t, it))
			if tc.held {
				requireHeld(t, e, a.ID)
			} else {
				requireHeld(t, e)
			}
			if got := eventsView(t, e, a.ID); got != events {
				t.Fatalf("published events = %q, want %q", got, events)
			}
			if got := deviceEventsView(t, e, a.ID); got != devices {
				t.Fatalf("device entries = %q, want %q", got, devices)
			}
		})
	}
}
