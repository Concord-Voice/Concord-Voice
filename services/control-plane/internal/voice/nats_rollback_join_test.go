package voice

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJoinRollbackErr(t *testing.T) {
	opErr := errors.New("operation failed")
	rbErr := errors.New("rollback failed")

	t.Run("nil rollback error returns returnErr untouched", func(t *testing.T) {
		gotNil := joinRollbackErr(nil, nil, "rollback context")
		assert.NoError(t, gotNil)

		gotOp := joinRollbackErr(opErr, nil, "rollback context")
		assert.Same(t, opErr, gotOp)
	})

	t.Run("sql.ErrTxDone is benign and returns returnErr untouched", func(t *testing.T) {
		gotNil := joinRollbackErr(nil, sql.ErrTxDone, "rollback context")
		assert.NoError(t, gotNil)

		gotOp := joinRollbackErr(opErr, sql.ErrTxDone, "rollback context")
		assert.Same(t, opErr, gotOp)
	})

	t.Run("wrapped sql.ErrTxDone is recognized via errors.Is and returns returnErr untouched", func(t *testing.T) {
		wrappedTxDone := fmt.Errorf("driver wrapping: %w", sql.ErrTxDone)
		gotNil := joinRollbackErr(nil, wrappedTxDone, "rollback context")
		assert.NoError(t, gotNil)

		gotOp := joinRollbackErr(opErr, wrappedTxDone, "rollback context")
		assert.Same(t, opErr, gotOp)
	})

	t.Run("real rollback error alone without returnErr wraps with message prefix", func(t *testing.T) {
		msg := "rollback voice lifecycle mutation"
		got := joinRollbackErr(nil, rbErr, msg)
		// CWE-460: a real rollback failure must never collapse to nil.
		require.Error(t, got)
		assert.True(t, errors.Is(got, rbErr), "expected joined error to wrap rollback error")
		assert.Contains(t, got.Error(), msg)
	})

	t.Run("real rollback error joined with existing operation error preserves both causes", func(t *testing.T) {
		msg := "rollback voice lifecycle mutation"
		got := joinRollbackErr(opErr, rbErr, msg)
		require.Error(t, got)
		assert.True(t, errors.Is(got, opErr), "expected joined error to preserve operation error cause")
		assert.True(t, errors.Is(got, rbErr), "expected joined error to preserve rollback error cause")
		assert.Contains(t, got.Error(), msg)
	})
}
