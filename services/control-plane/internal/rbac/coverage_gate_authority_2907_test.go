package rbac

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAuthorityLifecycleHelpersRejectInvalidAndDeduplicate(t *testing.T) {
	user := uuid.NewString()
	users, lifecycle := splitAuthorityLifecyclePrincipals([]string{
		user,
		authorityLifecyclePrincipal(user),
		authorityLifecyclePrincipal(user),
		"",
	})
	require.Equal(t, []string{user, ""}, users)
	require.Equal(t, []string{user, user}, lifecycle)

	require.Error(t, lockAuthorityLifecyclePrincipalsTx(context.Background(), nil, []string{"not-a-uuid"}))
	require.NoError(t, lockAuthorityLifecyclePrincipalsTx(context.Background(), nil, nil))
}

func TestServerVoiceLifecycleAdvisoryKeyRejectsNil(t *testing.T) {
	_, err := ServerVoiceLifecycleAdvisoryKey(uuid.Nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid voice lifecycle lock sender")
}

func TestSameAuthorityChannelTargetRequiresBothSetsToMatch(t *testing.T) {
	left := channelAuthorityTarget{all: []string{"a"}, voice: []string{"a"}}
	require.True(t, sameAuthorityChannelTarget(left, left))
	require.False(t, sameAuthorityChannelTarget(left, channelAuthorityTarget{all: []string{"a"}, voice: nil}))
}
