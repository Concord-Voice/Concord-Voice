package rbac

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// discardOutcome is the one place CreateRole, withAuthorityCapture and
// applyRolePositions decide what a deferred Rollback does to their outcome.
func TestDiscardOutcome(t *testing.T) {
	rbErr := errors.New("connection already closed")

	t.Run("a clean discard keeps the work's outcome", func(t *testing.T) {
		assert.NoError(t, discardOutcome(nil, "x", nil))
		assert.ErrorIs(t, discardOutcome(nil, "x", ErrNotMember), ErrNotMember)
	})
	t.Run("ErrTxDone after Commit keeps the work's outcome", func(t *testing.T) {
		assert.NoError(t, discardOutcome(sql.ErrTxDone, "x", nil))
		assert.ErrorIs(t, discardOutcome(sql.ErrTxDone, "x", errAmbiguousAuthorityCommit), errAmbiguousAuthorityCommit)
	})
	t.Run("a failed discard replaces a denial with the fault", func(t *testing.T) {
		got := discardOutcome(rbErr, "x", ErrNotMember)
		assert.ErrorIs(t, got, rbErr)
		assert.NotErrorIs(t, got, ErrNotMember, "the denial sentinel must not survive a failed discard")
		assert.Contains(t, got.Error(), ErrNotMember.Error(), "the work's error survives as text")
	})
	t.Run("a failed discard after no error is still reported", func(t *testing.T) {
		assert.ErrorIs(t, discardOutcome(rbErr, "x", nil), rbErr)
	})
}
