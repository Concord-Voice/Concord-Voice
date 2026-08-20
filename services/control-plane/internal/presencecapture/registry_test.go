package presencecapture

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFamilyRegistryCoversEveryDeclaredFamily is the whole point of the
// registry: an appended family lands below familyCount and, absent a registry
// entry, shows up here and in the boot guard instead of silently inheriting
// another family's policy.
func TestFamilyRegistryCoversEveryDeclaredFamily(t *testing.T) {
	assert.Empty(t, UnregisteredFamilies())
}

func TestPolicyForReturnsDeclaredPolicies(t *testing.T) {
	t.Run("accept is additive and carries the custom status leg", func(t *testing.T) {
		policy, err := PolicyFor(FamilyFriendshipAccept)
		require.NoError(t, err)
		assert.False(t, policy.CanRevokeVisibility)
		assert.True(t, policy.CarriesCustomTextTopology)
	})

	t.Run("block revokes and carries the custom status leg", func(t *testing.T) {
		policy, err := PolicyFor(FamilyBlock)
		require.NoError(t, err)
		assert.True(t, policy.CanRevokeVisibility)
		assert.True(t, policy.CarriesCustomTextTopology)
	})
}

func TestPolicyForRejectsAnUnregisteredFamily(t *testing.T) {
	_, err := PolicyFor(familyCount)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFamilyUnregistered))
}

// CanRevokeVisibility keeps its pre-registry fail-closed answer: an uncleared
// viewer is a disclosure, an unnecessary disconnect is a reconnect.
func TestCanRevokeVisibilityFailsClosedForAnUnregisteredFamily(t *testing.T) {
	assert.True(t, familyCount.CanRevokeVisibility())
}

func TestUnregisteredFamiliesReportsAGap(t *testing.T) {
	original := familyRegistry
	t.Cleanup(func() { familyRegistry = original })

	reduced := make(map[Family]FamilyPolicy, len(original))
	for family, policy := range original {
		if family == FamilyBlock {
			continue
		}
		reduced[family] = policy
	}
	familyRegistry = reduced

	assert.Equal(t, []Family{FamilyBlock}, UnregisteredFamilies())
}
