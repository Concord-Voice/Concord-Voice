package media_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
)

const erasureTestUser = "11111111-2222-3333-4444-555555555555"

// TestErasableTier1KeysExcludesSharedSubjects guards the narrowing at its
// source. Every key this returns is deleted when an account is erased, so a
// shared-subject key appearing here destroys a live server's icon.
func TestErasableTier1KeysExcludesSharedSubjects(t *testing.T) {
	keys := media.ErasableTier1Keys(erasureTestUser)

	assert.ElementsMatch(t, []string{
		"avatars/" + erasureTestUser,
		"banners/" + erasureTestUser,
	}, keys)

	// Stated as a prefix rule as well as an exact set, so a future purpose added
	// to tier1StorageKey fails here rather than silently widening the erasure.
	for _, key := range keys {
		assert.True(t,
			strings.HasPrefix(key, "avatars/") || strings.HasPrefix(key, "banners/"),
			"%s is not scoped to a user subject; server-icons/, server-banners/ and "+
				"dm-icons/ belong to subjects that outlive their uploader", key)
	}
}
