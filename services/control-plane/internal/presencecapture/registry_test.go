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

// The membership families appended by #2447. The additive pair is the sharp
// case: CanRevokeVisibility true would seed plan.viewers and tear down the
// devices of the user who just joined.
func TestAdditiveMembershipFamiliesCannotRevokeVisibility(t *testing.T) {
	for _, family := range []Family{FamilyMemberAdd, FamilyMemberJoin} {
		policy, err := PolicyFor(family)
		require.NoError(t, err, "family %d must be registered", family)
		assert.False(t, policy.CanRevokeVisibility,
			"family %d is additive; revoking would disconnect the joining user", family)
		assert.True(t, policy.CarriesCustomTextTopology,
			"family %d still changes the Custom Status audience", family)
	}
}

func TestRevokingMembershipFamiliesCarryBothAxes(t *testing.T) {
	for _, family := range []Family{FamilyMemberRemove, FamilyMemberBan} {
		policy, err := PolicyFor(family)
		require.NoError(t, err, "family %d must be registered", family)
		assert.True(t, policy.CanRevokeVisibility,
			"family %d removes shared-server visibility", family)
		assert.True(t, policy.CarriesCustomTextTopology,
			"family %d changes the Custom Status audience", family)
	}
}
