package voice

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErasureAudience(t *testing.T) {
	erased, a, b := uuid.New(), uuid.New(), uuid.New()

	audience, ok := erasureAudience([]string{a.String(), b.String()}, erased)
	require.True(t, ok)
	assert.Equal(t, map[uuid.UUID]bool{a: true, b: true}, audience)

	_, ok = erasureAudience([]string{a.String(), "not-a-uuid"}, erased)
	assert.False(t, ok, "a malformed viewer rejects the envelope")

	_, ok = erasureAudience([]string{erased.String()}, erased)
	assert.False(t, ok, "the erased user cannot be their own viewer")

	audience, ok = erasureAudience(nil, erased)
	require.True(t, ok)
	assert.Empty(t, audience)
}
