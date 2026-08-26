package activepresence

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

func validPlan() Plan {
	return Plan{
		SubjectID:   uuid.New(),
		Category:    presence.CategoryPrivateCall,
		OperationID: uuid.New(),
		Resolution:  ResolutionConservative,
		EventAt:     time.Now(),
	}
}

func TestPlanValidateRejectsInconsistentPlans(t *testing.T) {
	cases := map[string]func(p *Plan){
		"nil subject":             func(p *Plan) { p.SubjectID = uuid.Nil },
		"nil operation":           func(p *Plan) { p.OperationID = uuid.Nil },
		"zero event time":         func(p *Plan) { p.EventAt = time.Time{} },
		"unknown category":        func(p *Plan) { p.Category = presence.Category("screen_share") },
		"exact without lifecycle": func(p *Plan) { p.Resolution = ResolutionExact; p.LifecycleID = uuid.Nil },
		"negative attempts":       func(p *Plan) { p.Attempts = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := validPlan()
			mutate(&p)
			require.ErrorIs(t, p.Validate(), ErrInvalidPlan)
		})
	}
}

func TestPlanValidateAcceptsBothArms(t *testing.T) {
	require.NoError(t, validPlan().Validate())

	exact := validPlan()
	exact.Resolution = ResolutionExact
	exact.LifecycleID = uuid.New()
	require.NoError(t, exact.Validate())
}

// The enums are stringified into log lines and into the failure_class column,
// so an unknown value must never render as a number that looks like a real
// class. It renders as the fail-closed member instead.
func TestClosedVocabulariesStringifyExactly(t *testing.T) {
	require.Equal(t, "exact", ResolutionExact.String())
	require.Equal(t, "conservative", ResolutionConservative.String())
	require.Equal(t, "conservative", Resolution(200).String())

	require.Equal(t, "state_unexpiring", FailureStateUnexpiring.String())
	require.Equal(t, "plan_invalid", FailurePlanInvalid.String())
	require.Equal(t, "plan_invalid", FailureClass(200).String())

	require.Equal(t, "superseded", OutcomeSuperseded.String())
	require.Equal(t, "retained", Outcome(200).String())
}

func TestParseHelpersRejectUnknownValues(t *testing.T) {
	category, err := ParseCategory("private_call")
	require.NoError(t, err)
	require.Equal(t, presence.CategoryPrivateCall, category)

	_, err = ParseCategory("screen_share")
	require.ErrorIs(t, err, ErrInvalidPlan)

	_, err = ParseResolution("approximate")
	require.ErrorIs(t, err, ErrInvalidPlan)
}

// The bounds are consts, never configuration: a deployment that needs another
// value has a design problem. Pinning them makes widening one a visible test
// failure instead of a silent relaxation, and keeps the package honest about
// which bounds it actually owns.
func TestBoundsArePinned(t *testing.T) {
	require.Equal(t, 16, maxActiveSubjects)
	require.Equal(t, 1000, maxPlanBatch)
	require.Equal(t, 5, maxPlanAttempts)
	require.Equal(t, 5*time.Second, reconcileInterval)
	require.Equal(t, 10*time.Minute, quarantineInterval)
}

// Every member of a closed vocabulary is pinned, not just the interesting ones.
// failure_class and resolution are written into CHECK-constrained columns, so a
// member whose string drifts is a runtime constraint violation, not a cosmetic
// log defect.
func TestClosedVocabulariesPinEveryMember(t *testing.T) {
	// FailureNone renders as "plan_invalid" because the vocabulary has no
	// "none" member. A store MUST persist NULL for FailureNone rather than
	// calling String(), or a success would be recorded as a failure class.
	require.Equal(t, "plan_invalid", FailureNone.String())

	failures := map[FailureClass]string{
		FailureStateRead:        "state_read",
		FailureStateUnexpiring:  "state_unexpiring",
		FailureStateMalformed:   "state_malformed",
		FailureGenerationDelete: "generation_delete",
		FailureDelivery:         "delivery",
		FailurePlanInvalid:      "plan_invalid",
	}
	for class, want := range failures {
		require.Equal(t, want, class.String())
	}

	outcomes := map[Outcome]string{
		OutcomeStateAbsent:  "state_absent",
		OutcomeSuperseded:   "superseded",
		OutcomeCleared:      "cleared",
		OutcomeDisconnected: "disconnected",
		OutcomeRetained:     "retained",
		OutcomeQuarantined:  "quarantined",
	}
	for outcome, want := range outcomes {
		require.Equal(t, want, outcome.String())
	}
}

// A value that survives a write/read cycle must come back as itself. This is
// what makes String() and the Parse helpers a matched pair rather than two
// independently drifting tables.
func TestParseHelpersRoundTripEveryMember(t *testing.T) {
	for _, resolution := range []Resolution{ResolutionExact, ResolutionConservative} {
		parsed, err := ParseResolution(resolution.String())
		require.NoError(t, err)
		require.Equal(t, resolution, parsed)
	}

	for _, category := range []presence.Category{presence.CategoryServerVoice, presence.CategoryPrivateCall} {
		parsed, err := ParseCategory(string(category))
		require.NoError(t, err)
		require.Equal(t, category, parsed)
	}

	// The empty string is the zero value a scan of a NULL column produces. It
	// must be rejected, never silently resolved to a real category.
	_, err := ParseCategory("")
	require.ErrorIs(t, err, ErrInvalidPlan)
}

// An unknown resolution still returns the conservative arm alongside its error,
// so a caller that mishandles the error degrades in the safe direction.
func TestParseResolutionFailsToTheConservativeArm(t *testing.T) {
	resolution, err := ParseResolution("exact ")
	require.Error(t, err)
	require.Equal(t, ResolutionConservative, resolution)
}
