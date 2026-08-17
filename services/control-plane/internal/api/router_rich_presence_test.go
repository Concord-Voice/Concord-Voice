//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMVoiceCallLeaseVerifierMatchesOnlyTheCurrentExactLease(t *testing.T) {
	// The DB index is allocated per process by redistest (#2680); the hand-pinned
	// DB 15 this used to carry was shared with every other concurrent test binary.
	redisClient := redistest.Client(t)
	t.Cleanup(func() {
		// Reported, not discarded — a swallowed cleanup failure leaves lease keys
		// for the next test in this package.
		assert.NoError(t, redistest.Reset(context.Background(), redisClient))
	})
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	conversationID := uuid.New()
	callID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(),
		redisClient,
		dm.VoiceCallLease{
			ConversationID: conversationID,
			CallID:         callID,
			CallerUserID:   uuid.New(),
		},
		dm.DMVoiceCallLeaseTTL,
		true,
	))

	verifier := dmVoiceCallLeaseVerifier{redis: redisClient}
	matches, err := verifier.Matches(context.Background(), conversationID, callID)
	require.NoError(t, err)
	require.True(t, matches)
	matches, err = verifier.Matches(context.Background(), conversationID, uuid.New())
	require.NoError(t, err)
	require.False(t, matches)

	matches, err = (dmVoiceCallLeaseVerifier{}).Matches(
		context.Background(), conversationID, callID,
	)
	require.Error(t, err)
	require.False(t, matches)
}

func TestNewRouterWiresOneAuthoritativeRichPresenceBridgeIntoVoiceNATS(t *testing.T) {
	sourceBytes, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	source := string(sourceBytes)
	needles := []string{
		"activityStore := presence.NewActivityStore(redis)",
		"activityBuilder := presence.NewActivityBuilder(",
		// One shared base-presence gate, constructed before both consumers so
		// they cannot disagree about whether a sender may publish (#2444).
		"senderPresence := websocket.NewSenderPresenceResolver(redis, db, hub)",
		"activityService := presence.NewActivityService(",
		"activitySnapshotService := presence.NewActivitySnapshotService(",
		"hub.SetActivitySnapshotService(activitySnapshotService)",
		"hub.SetRichPresenceHiddenSuppressor(",
		"usersHandler.SetActivitySettingsSuppressor(activityService)",
		"voice.NewNATSSubscriber(db, log, hub, natsClient, redis, rbacResolver, activityService)",
	}
	prior := -1
	for _, needle := range needles {
		require.Equal(t, 1, strings.Count(source, needle), needle)
		position := strings.Index(source, needle)
		require.Greater(t, position, prior, "wiring order for %s", needle)
		prior = position
	}
	require.Contains(t, source, `activityBuilder := presence.NewActivityBuilder(
		db, dmVoiceCallLeaseVerifier{redis: redis}, activityStore,
	)`)
	require.Contains(t, source, `activitySnapshotService := presence.NewActivitySnapshotService(
		db,
		activityBuilder,
		activityStore,
		rbacResolver,
		presenceHistoryService,
		senderPresence,
	)`)
}
