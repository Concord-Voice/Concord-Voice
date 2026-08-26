package activepresence

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// An out-of-range Resolution must be REJECTED, not coerced.
//
// Review finding: Resolution.String() fails closed to "conservative", which is
// correct for a log line and wrong for persistence. Without this check a
// producer defect carrying Resolution(99) passed Validate and was written as a
// conservative plan -- silently manufacturing a clear obligation instead of
// returning ErrInvalidPlan. The category switch already worked this way; the
// resolution check did not.
func TestValidateRejectsAnOutOfRangeResolution(t *testing.T) {
	base := Plan{
		SubjectID:   uuid.New(),
		Category:    presence.CategoryPrivateCall,
		OperationID: uuid.New(),
		EventAt:     time.Now(),
	}

	valid := base
	valid.Resolution = ResolutionConservative
	require.NoError(t, valid.Validate(), "the control must still pass")

	bogus := base
	bogus.Resolution = Resolution(99)
	require.ErrorIs(t, bogus.Validate(), ErrInvalidPlan,
		"an unknown resolution must be rejected rather than coerced to conservative")
}
