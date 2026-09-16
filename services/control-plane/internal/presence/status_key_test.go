package presence

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
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

func (d alwaysPermitPresence) RichPresenceEmissionState(
	ctx context.Context, senderID uuid.UUID,
) (bool, error) {
	// Test double: always DETERMINED, so it exercises the
	// suppression path rather than the indeterminate one.
	return d.RichPresenceEmissionPermitted(ctx, senderID), nil
}

// TestStatusTTLValueIsPinned pins the constant's VALUE, and nothing more.
//
// Be honest about its reach: it CANNOT catch a re-introduced local
// 120*time.Second in a presence writer, which is the regression the shared
// constant exists to prevent -- the two would simply agree. The "obeyed" half of
// that pair is TestSweeperRenewsAnOfflineMarkerToExactlyTheSharedTTL in
// internal/websocket, which asserts the sweeper's argument against this constant
// rather than against a literal. A source-scanning guard over the writers would
// close what remains; until one exists, do not read this test as covering it.
func TestStatusTTLValueIsPinned(t *testing.T) {
	assert.Equal(t, 120*time.Second, StatusTTL)
}
