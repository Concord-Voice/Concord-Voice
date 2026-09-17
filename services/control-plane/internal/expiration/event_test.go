package expiration

import (
	"testing"
	"time"
)

func TestEventKindForTransition(t *testing.T) {
	window := 86400
	otherWindow := 3600

	tests := []struct {
		name       string
		request    Request
		transition Transition
		wantKind   EventKind
		wantOK     bool
	}{
		{
			name:       "clear of a window that WAS set reports cleared",
			request:    Request{Mode: "clear", Retroactive: "clear_pending"},
			transition: Transition{Previous: Policy{WindowSeconds: &window}, Current: Policy{WindowSeconds: nil}},
			wantKind:   EventKindCleared,
			wantOK:     true,
		},
		{
			// Inverted deliberately. This asserted the opposite until review proved the
			// old behaviour exploitable: a cleared event carries a nil window, so its row
			// is written with expires_at = NULL and the sweeper can never reap it, and any
			// permitted actor could repeat the request to mint permanent rows at will.
			name:       "clear against an already-null window is NOT an event",
			request:    Request{Mode: "clear", Retroactive: "leave_pending"},
			transition: Transition{Previous: Policy{WindowSeconds: nil}, Current: Policy{WindowSeconds: nil}},
			wantOK:     false,
		},
		{
			name:       "set with no prior window reports set",
			request:    Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"},
			transition: Transition{Previous: Policy{WindowSeconds: nil}, Current: Policy{WindowSeconds: &window}},
			wantKind:   EventKindSet,
			wantOK:     true,
		},
		{
			name:       "set replacing a different prior window reports changed",
			request:    Request{Mode: "set", WindowSeconds: &otherWindow, Retroactive: "apply"},
			transition: Transition{Previous: Policy{WindowSeconds: &window}, Current: Policy{WindowSeconds: &otherWindow}},
			wantKind:   EventKindChanged,
			wantOK:     true,
		},
		{
			// Also inverted. "changed" over a policy that did not move is a false record;
			// the row is reapable here (it carries a window) but it is still a lie.
			name:       "set re-affirming the SAME prior window is NOT an event",
			request:    Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"},
			transition: Transition{Previous: Policy{WindowSeconds: &window}, Current: Policy{WindowSeconds: &window}},
			wantOK:     false,
		},
		{
			name:       "set to a DIFFERENT window from a set window still reports changed",
			request:    Request{Mode: "set", WindowSeconds: &otherWindow, Retroactive: "apply"},
			transition: Transition{Previous: Policy{WindowSeconds: &window}, Current: Policy{WindowSeconds: &otherWindow}},
			wantKind:   EventKindChanged,
			wantOK:     true,
		},
		{
			name:       "resume never produces an event — it is not a policy mutation",
			request:    Request{Mode: "resume", Revision: ptr(int64(3))},
			transition: Transition{Previous: Policy{WindowSeconds: &window}, Current: Policy{WindowSeconds: &window}},
			wantOK:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, ok := EventKindForTransition(tc.request, tc.transition)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}

func TestEventFor(t *testing.T) {
	window := 86400
	previous := 3600
	stamped := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	revision := int64(4)

	t.Run("prefers the policy's own stamp over the wall clock", func(t *testing.T) {
		// The row's displayed time and the policy's accepted time must not
		// diverge: the handler that forgot this fallback would stamp the row
		// from its own clock, and nothing downstream would notice.
		payload, ok := EventFor(
			Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only"},
			Transition{
				Previous: Policy{WindowSeconds: &previous},
				Current:  Policy{WindowSeconds: &window, UpdatedAt: &stamped},
			},
			"user-1",
		)
		if !ok {
			t.Fatal("expected an event for a set transition")
		}
		if !payload.ChangedAt.Equal(stamped) {
			t.Errorf("ChangedAt = %v, want the policy's UpdatedAt %v", payload.ChangedAt, stamped)
		}
		if payload.Kind != EventKindChanged {
			t.Errorf("Kind = %q, want %q", payload.Kind, EventKindChanged)
		}
		if payload.ActorUserID != "user-1" {
			t.Errorf("ActorUserID = %q, want user-1", payload.ActorUserID)
		}
		if payload.WindowSeconds == nil || *payload.WindowSeconds != window {
			t.Errorf("WindowSeconds = %v, want %d", payload.WindowSeconds, window)
		}
		if payload.PreviousWindowSeconds == nil || *payload.PreviousWindowSeconds != previous {
			t.Errorf("PreviousWindowSeconds = %v, want %d", payload.PreviousWindowSeconds, previous)
		}
	})

	t.Run("falls back to now when the policy carries no stamp", func(t *testing.T) {
		before := time.Now().UTC()
		payload, ok := EventFor(
			Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only"},
			Transition{Previous: Policy{}, Current: Policy{WindowSeconds: &window}},
			"user-1",
		)
		if !ok {
			t.Fatal("expected an event for a set transition")
		}
		if payload.ChangedAt.Before(before) || payload.ChangedAt.After(time.Now().UTC()) {
			t.Errorf("ChangedAt = %v, want a stamp taken during this call", payload.ChangedAt)
		}
		if payload.Kind != EventKindSet {
			t.Errorf("Kind = %q, want %q for a first window", payload.Kind, EventKindSet)
		}
	})

	t.Run("a resume produces no event at all", func(t *testing.T) {
		// A backfill continuation changed no window, so a row for it would be a
		// system message announcing nothing.
		payload, ok := EventFor(
			Request{Mode: "resume", Revision: &revision},
			Transition{
				Previous: Policy{WindowSeconds: &window},
				Current:  Policy{WindowSeconds: &window, UpdatedAt: &stamped},
			},
			"user-1",
		)
		if ok {
			t.Fatalf("expected no event for a resume, got %+v", payload)
		}
		if payload != (EventPayload{}) {
			t.Errorf("payload = %+v, want the zero value when ok is false", payload)
		}
	})

	t.Run("a clear reports no current window", func(t *testing.T) {
		payload, ok := EventFor(
			Request{Mode: "clear", Retroactive: "clear_pending"},
			Transition{
				Previous: Policy{WindowSeconds: &previous},
				Current:  Policy{WindowSeconds: nil, UpdatedAt: &stamped},
			},
			"user-1",
		)
		if !ok {
			t.Fatal("expected an event for a clear")
		}
		if payload.Kind != EventKindCleared {
			t.Errorf("Kind = %q, want %q", payload.Kind, EventKindCleared)
		}
		if payload.WindowSeconds != nil {
			t.Errorf("WindowSeconds = %v, want nil after a clear", payload.WindowSeconds)
		}
	})
}
