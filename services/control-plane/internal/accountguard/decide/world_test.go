package decide

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Plan Task 11: the test-only world model. It plays the runner's part (PR3): it hands Decide a
// snapshot, then maps the outcome's effects back onto the snapshot. It exists for the depth-3
// sequences of P3 (sequence_test.go) and the section 6.12 scenarios (scenarios_test.go).
//
// The world never reads an epoch: Input has none, and that structural fact is what P3 rests on
// (TestP3_InputCarriesNoEpoch). A password rotation is therefore a plain credential change, made
// by the test through rotatePassword, exactly as the runner makes it after a T1.

type world struct {
	now          time.Time
	ssoLogin     bool
	currentKeyID int16
	budgetLocked bool // zero: the budget is clear
	creds        []Cred
	primaries    []Primary
	reservations []Reservation
	reqs, recent []Req // reqs: every request row but the cancelled ones; recent: cancelled rows
	next         int   // the next request number: opens allocate "r<next>"
	nextGroup    int
}

func newWorld(creds ...Cred) *world {
	return &world{now: t0, currentKeyID: 1, creds: append([]Cred(nil), creds...)}
}

// input is the snapshot of the world, with the given action and proof. The slices are copies, so
// a later apply cannot reach back into an Input a test kept.
func (w *world) input(act Action, used ...CredRef) Input {
	return Input{
		Now: w.now, Act: act, Proof: Proof{Used: append([]CredRef(nil), used...)},
		Creds:                append([]Cred(nil), w.creds...),
		Primaries:            append([]Primary(nil), w.primaries...),
		Reservations:         append([]Reservation(nil), w.reservations...),
		Reqs:                 append([]Req(nil), w.reqs...),
		RecentCancels:        append([]Req(nil), w.recent...),
		GuessableBudgetClear: !w.budgetLocked, CurrentKeyID: w.currentKeyID, SSOLogin: w.ssoLogin,
	}
}

// decide runs Decide on the world's snapshot and the global invariants on its outcome. It does
// not apply the outcome.
func (w *world) decide(t *testing.T, act Action, used ...CredRef) Outcome {
	t.Helper()
	in := w.input(act, used...)
	var out Outcome
	require.NotPanics(t, func() { out = Decide(in) })
	checkInvariants(t, in, out)
	return out
}

// passwordRef is the current reference of the account's password.
func (w *world) passwordRef() CredRef {
	for _, c := range w.creds {
		if c.Ref.Kind == CredPassword {
			return c.Ref
		}
	}
	return CredRef{}
}

func (w *world) credIndex(r CredRef) int {
	for i, c := range w.creds {
		if c.Ref.Kind == r.Kind && c.Ref.ID == r.ID {
			return i
		}
	}
	return -1
}

// reqPtr finds a request among reqs, then among recent; nil when the world holds none.
func (w *world) reqPtr(id string) *Req {
	for i := range w.reqs {
		if w.reqs[i].ID == id {
			return &w.reqs[i]
		}
	}
	for i := range w.recent {
		if w.recent[i].ID == id {
			return &w.recent[i]
		}
	}
	return nil
}

// apply maps an outcome onto the world, as the runner would. A refused outcome changes nothing.
// It fails the test if an effect names something the world does not hold.
func (w *world) apply(t *testing.T, out Outcome) {
	t.Helper()
	group := ""
	groupFor := func(e Effect) string {
		if e.Group != "" {
			return e.Group
		}
		if group == "" { // D12: one new group, shared by every effect of this outcome
			w.nextGroup++
			group = fmt.Sprintf("g%d", w.nextGroup)
		}
		return group
	}
	for _, e := range out.Effects {
		switch e.Kind {
		case EffectOpen:
			w.next++
			op := e.Open
			r := Req{
				ID: fmt.Sprintf("r%d", w.next), Kind: op.Kind, Status: op.Status, OriginClass: op.OriginClass,
				Origins: op.Origins, Targets: op.Targets, OpenedAt: op.OpenedAt, CompletesAt: op.CompletesAt,
				DisputedAt: op.DisputedAt, ResetFactors: op.ResetFactors,
				Fingerprint: op.Candidate.Keyed, Coarse: op.Candidate.Coarse, FingerprintKeyID: op.Candidate.KeyID,
			}
			if op.Status == StatusDisputed {
				r.DisputeGroup = groupFor(e)
			}
			w.reqs = append(w.reqs, r)
		case EffectCancel:
			found := false
			for i := range w.reqs {
				if w.reqs[i].ID != e.RequestID {
					continue
				}
				r := w.reqs[i]
				r.Status, r.CancelReason, r.CancellerClass, r.CancelledAt = StatusCancelled, e.Reason, e.CancellerClass, w.now
				w.reqs = append(w.reqs[:i], w.reqs[i+1:]...)
				w.recent = append(w.recent, r)
				found = true
				break
			}
			require.True(t, found, "cancel names request %q, which is not an open row", e.RequestID)
		case EffectDispute:
			r := w.reqPtr(e.RequestID)
			require.NotNil(t, r, "dispute names unknown request %q", e.RequestID)
			if r.Status != StatusDisputed {
				r.DisputedAt = w.now // a regrouped row keeps its DisputedAt (ruling 12)
			}
			r.Status, r.DisputeGroup = StatusDisputed, groupFor(e)
		case EffectReturnToPending:
			r := w.reqPtr(e.RequestID)
			require.NotNil(t, r, "return_to_pending names unknown request %q", e.RequestID)
			r.Status, r.DisputedAt, r.DisputeGroup = StatusPending, time.Time{}, ""
		case EffectComplete:
			r := w.reqPtr(e.RequestID)
			require.NotNil(t, r, "complete names unknown request %q", e.RequestID)
			r.Status = StatusCompleted
		case EffectHold:
			r := w.reqPtr(e.RequestID)
			require.NotNil(t, r, "hold names unknown request %q", e.RequestID)
			r.HeldMs, r.HeldSince = e.HeldMs, e.HeldSince
		case EffectRemove:
			i := w.credIndex(e.Cred)
			require.GreaterOrEqual(t, i, 0, "remove names unknown credential %+v", e.Cred)
			w.creds = append(w.creds[:i], w.creds[i+1:]...)
		case EffectContest:
			i := w.credIndex(e.Cred)
			require.GreaterOrEqual(t, i, 0, "contest names unknown credential %+v", e.Cred)
			w.creds[i].ContestedAt = w.now
		case EffectRestorePrimary:
			found := false
			for i := range w.primaries {
				if w.primaries[i].DisplacedAt.IsZero() {
					w.primaries[i].DisplacedAt = w.now
				}
			}
			for i := range w.primaries {
				if w.primaries[i].Ref.Kind == e.Cred.Kind && w.primaries[i].Ref.ID == e.Cred.ID {
					w.primaries[i].DisplacedAt = time.Time{}
					found = true
				}
			}
			require.True(t, found, "restore_primary names unknown primary %+v", e.Cred)
			w.dropReservation(e.Cred)
		case EffectReleaseReservation:
			w.dropReservation(e.Cred)
		case EffectResyncMFAFlags:
			// A derived-flag resync: nothing the snapshot holds.
		default:
			require.Fail(t, "unknown effect kind", "%d", e.Kind)
		}
	}
	if out.SetsPassword {
		w.rotatePassword()
	}
}

func (w *world) dropReservation(r CredRef) {
	for i := range w.reservations {
		if a := w.reservations[i].AddressRef; a.Kind == r.Kind && a.ID == r.ID {
			w.reservations = append(w.reservations[:i], w.reservations[i+1:]...)
			return
		}
	}
}

// rotatePassword is the runner's password change after a T1: the version moves on, and the new
// password matures only after delta_cp.
func (w *world) rotatePassword() {
	for i := range w.creds {
		if w.creds[i].Ref.Kind == CredPassword {
			w.creds[i].Ref.Version++
			w.creds[i].MaturesAt = w.now.Add(DeltaCP)
			w.creds[i].EnrolledAt = w.now
			return
		}
	}
}

// enrol adds a credential enrolled now at the given class; it matures after delta_cp.
func (w *world) enrol(c Cred, class Class) {
	c.EnrolledAt = w.now
	c.MaturesAt = w.now.Add(DeltaCP)
	c.EnrolClass = class
	w.creds = append(w.creds, c)
}

func (w *world) advance(d time.Duration) { w.now = w.now.Add(d) }

// accept records that a notice of the request was accepted, so the request is no longer held on it.
func (w *world) accept(id string) {
	if r := w.reqPtr(id); r != nil {
		r.AcceptedAddressNotice = true
		r.MatureNoticesSettled = true
	}
}

// reqFacts are the fields of a request P3 says no sign-out or rotation may move.
type reqFacts struct {
	Status       ReqStatus
	CompletesAt  time.Time
	HeldMs       int64
	HeldSince    time.Time
	DisputeGroup string
}

func (w *world) facts() map[string]reqFacts {
	out := map[string]reqFacts{}
	for _, list := range [][]Req{w.reqs, w.recent} {
		for _, r := range list {
			out[r.ID] = reqFacts{r.Status, r.CompletesAt, r.HeldMs, r.HeldSince, r.DisputeGroup}
		}
	}
	return out
}
