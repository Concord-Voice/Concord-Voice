package presence

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestStatusRedisKey_MatchesLegacyFormat(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	require.Equal(t, "presence:11111111-2222-3333-4444-555555555555", StatusRedisKey(id))
}

func TestStatusRedisKey_NilUUID(t *testing.T) {
	require.Equal(t, "presence:00000000-0000-0000-0000-000000000000", StatusRedisKey(uuid.Nil))
}

func TestEmissionPermittedForStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    string
		permitted bool
	}{
		{"online emits", StatusOnline, true},
		{"dnd emits (product decision, spec 7.2)", StatusDND, true},
		{"invisible suppresses", StatusInvisible, false},
		{"offline suppresses", StatusOffline, false},
		{"empty (missing key / offline) suppresses", "", false},
		{"unknown value suppresses", "bogus", false},
		{"case variant is not a visible status", "ONLINE", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.permitted, EmissionPermittedForStatus(tc.status))
		})
	}
}

// alwaysPermitPresence is the explicit "this test does not exercise the
// base-presence gate" value for NewActivityService's required resolver
// parameter. Passing nil would fail closed and suppress every emission, which
// silently changes what the surrounding tests assert.
type alwaysPermitPresence struct{}

func (alwaysPermitPresence) RichPresenceEmissionPermitted(
	context.Context, uuid.UUID,
) bool {
	return true
}
