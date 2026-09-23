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
		"voiceSub.SetOpsCounters(opsCounters)",
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

func TestNewRouterWiresDMBlockCleanupWithoutNATS(t *testing.T) {
	sourceBytes, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	source := string(sourceBytes)
	purge := strings.Index(source, "dmBlockReconciler.SetPurgeEngine(dmBlockAttachmentRetirer{engine: purgeEngine})")
	notifier := strings.Index(source, "dmBlockReconciler.SetReconciliationNotifier(newDMBlockReconciliationNotifier(hub))")
	cleanup := strings.Index(source, "reconciler.SetDMBlockCleanup(dmBlockReconciler.ReconcileDue)")
	newRouter := strings.Index(source, "func NewRouter(")
	require.NotEqual(t, -1, purge)
	require.NotEqual(t, -1, notifier)
	require.NotEqual(t, -1, cleanup)
	require.NotEqual(t, -1, newRouter)
	newRouterSource := source[newRouter:]
	reconcilerWiring := strings.Index(newRouterSource, "wireDMBlockReconciler(")
	cleanupInHelper := strings.Index(source, "reconciler.SetDMBlockCleanup(dmBlockReconciler.ReconcileDue)")
	require.NotEqual(t, -1, reconcilerWiring)
	require.NotEqual(t, -1, cleanupInHelper)
	natsBranch := strings.Index(newRouterSource, "if natsClient != nil {")
	require.NotEqual(t, -1, natsBranch)
	require.Less(t, purge, cleanup, "attachment retirement must be wired before block cleanup can run")
	require.Less(t, notifier, cleanup, "DM topology notifications must be wired before block cleanup can run")
	require.Less(t, reconcilerWiring, natsBranch, "SQL-only block cleanup must be wired before the NATS-dependent branch")
	require.Contains(t, source, "if !dmBlockCleanupWired(activePlanReconciler) {")
}

func TestNewRouterWiresDMBlockVoiceEjectionRequestReply(t *testing.T) {
	sourceBytes, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	source := string(sourceBytes)
	helperStart := strings.Index(source, "func wireDMBlockReconciler(")
	require.NotEqual(t, -1, helperStart)
	helperEnd := strings.Index(source[helperStart:], "\nfunc ")
	require.Greater(t, helperEnd, 0)
	helper := source[helperStart : helperStart+helperEnd]
	construction := strings.Index(helper, "dmBlockReconciler.SetVoiceEjectV2(func(")
	require.NotEqual(t, -1, construction)
	guard := strings.Index(helper, "if natsClient != nil {")
	require.NotEqual(t, -1, guard)
	segment := helper[construction:]
	require.Contains(t, segment, "func(ctx context.Context, conversationID string, userID, generation uuid.UUID) error {")
	require.Contains(t, segment, "publishDurableDMBlockVoiceEjection(ctx, db, natsClient, jwtSecret, conversationID, userID, generation)")
	require.NotContains(t, segment, "dmBlockReconciler.SetVoiceEject(func(")
	// The durable publisher retains the legacy request/reply path as the
	// compatibility arm until the rollout flag activates the registry-backed
	// protocol.
	runtimeBytes, err := os.ReadFile("dmblock_runtime.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	require.Contains(t, string(runtimeBytes), "publishDMBlockVoiceEjection(ctx, requester, secret, conversationID, userID)")
	require.Less(t, guard, construction, "voice ejection must be enabled only inside the NATS guard")
}
