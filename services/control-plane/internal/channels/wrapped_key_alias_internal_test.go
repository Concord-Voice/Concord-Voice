package channels

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalizeWrappedKeyMaps(t *testing.T) {
	recipientID := uuid.NewString()
	alias := "{" + strings.ToUpper(recipientID) + "}"

	t.Run("canonicalizes matching aliases", func(t *testing.T) {
		keys, versions, err := canonicalizeWrappedKeyMaps(
			map[string]string{alias: "wrapped"},
			map[string]int{recipientID: 3},
		)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{recipientID: "wrapped"}, keys)
		assert.Equal(t, map[string]int{recipientID: 3}, versions)
	})

	t.Run("rejects conflicting wrapped-key aliases", func(t *testing.T) {
		_, _, err := canonicalizeWrappedKeyMaps(
			map[string]string{recipientID: "wrapped-a", alias: "wrapped-b"}, nil,
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, errDuplicateWrappedRecipient))
	})

	t.Run("rejects conflicting version aliases", func(t *testing.T) {
		_, _, err := canonicalizeWrappedKeyMaps(
			map[string]string{recipientID: "wrapped"},
			map[string]int{recipientID: 2, alias: 3},
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, errDuplicateWrappedRecipient))
	})
}
