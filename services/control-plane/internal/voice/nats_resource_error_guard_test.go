package voice

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestServerVoiceParticipantLimitMatchesMediaAdmission(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	voiceDir := filepath.Dir(testFile)
	controlPlaneSource, err := os.ReadFile(filepath.Join(voiceDir, "nats.go")) // #nosec G304 -- runtime.Caller plus fixed repository path
	require.NoError(t, err)
	mediaPlaneSource, err := os.ReadFile(filepath.Join(
		voiceDir, "..", "..", "..", "media-plane", "src", "lib", "roomManager.ts",
	)) // #nosec G304 -- runtime.Caller plus fixed repository path
	require.NoError(t, err)

	controlPlaneLimit := parseCrossPlaneParticipantLimit(
		t, string(controlPlaneSource), `maxServerVoiceParticipantIDs\s*=\s*([0-9_]+)`,
	)
	mediaPlaneLimit := parseCrossPlaneParticipantLimit(
		t, string(mediaPlaneSource), `MAX_SERVER_VOICE_PARTICIPANTS\s*=\s*([0-9_]+)`,
	)
	require.Equal(t, controlPlaneLimit, mediaPlaneLimit,
		"media admission and authoritative heartbeat bounds must remain identical")
}

func parseCrossPlaneParticipantLimit(t *testing.T, source, pattern string) int {
	t.Helper()
	matches := regexp.MustCompile(pattern).FindStringSubmatch(source)
	require.Len(t, matches, 2)
	limit, err := strconv.Atoi(strings.ReplaceAll(matches[1], "_", ""))
	require.NoError(t, err)
	return limit
}

func TestServerVoiceParticipantUnionWithinLimit(t *testing.T) {
	existingID := uuid.New()
	incoming := make([]uuid.UUID, maxServerVoiceParticipantIDs)
	for index := range incoming {
		incoming[index] = uuid.New()
	}
	require.False(t, serverVoiceParticipantUnionWithinLimit(
		[]uuid.UUID{existingID}, incoming,
	))
	incoming[0] = existingID
	require.True(t, serverVoiceParticipantUnionWithinLimit(
		[]uuid.UUID{existingID}, incoming,
	))
}

func TestServerHeartbeatWorkersStopBeforeCanceledWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	workCalls := 0
	forEachServerHeartbeatParticipant(
		ctx,
		make([]uuid.UUID, maxServerVoiceParticipantIDs),
		func(uuid.UUID) { workCalls++ },
	)
	require.Zero(t, workCalls)
}

func TestServerHeartbeatPriorityEventuallyCoversRoomAcrossShortDeadlines(t *testing.T) {
	mediaParticipantIDs := make([]uuid.UUID, maxServerVoiceParticipantIDs)
	for index := range mediaParticipantIDs {
		mediaParticipantIDs[index] = uuid.New()
	}
	base := time.Unix(1_700_000_000, 0).UTC()
	records := make(map[uuid.UUID]time.Time, maxServerVoiceParticipantIDs)
	for _, participantID := range mediaParticipantIDs[:400] {
		records[participantID] = base
	}

	runBudgetedHeartbeat := func(eventAt time.Time) map[uuid.UUID]bool {
		existingRecords := make([]voiceParticipantRecord, 0, len(records))
		for participantID, lifecycleEventAt := range records {
			existingRecords = append(existingRecords, voiceParticipantRecord{
				userID: participantID, lifecycleEventAt: lifecycleEventAt,
			})
		}
		ordered := prioritizeServerHeartbeatParticipants(
			mediaParticipantIDs, existingRecords,
		)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		selected := make(map[uuid.UUID]bool, 100)
		var selectedMu sync.Mutex
		forEachServerHeartbeatParticipant(ctx, ordered, func(participantID uuid.UUID) {
			selectedMu.Lock()
			defer selectedMu.Unlock()
			if len(selected) >= 100 {
				cancel()
				return
			}
			selected[participantID] = true
			if len(selected) == 100 {
				cancel()
			}
		})
		for participantID := range selected {
			records[participantID] = eventAt
		}
		return selected
	}

	selectedDuringMaterialization := make(map[uuid.UUID]bool, maxServerVoiceParticipantIDs)
	deadlineRounds := maxServerVoiceParticipantIDs/100 + serverHeartbeatParticipantWorkers
	for tick := 1; tick <= deadlineRounds; tick++ {
		for participantID := range runBudgetedHeartbeat(base.Add(time.Duration(tick) * time.Second)) {
			selectedDuringMaterialization[participantID] = true
		}
	}
	require.Equal(t, maxServerVoiceParticipantIDs, len(records),
		"missing rows must lead each retry until the whole media set materializes")
	require.Equal(t, maxServerVoiceParticipantIDs, len(selectedDuringMaterialization),
		"old persisted rows must also receive a fair refresh after inserts finish")

	selectedDuringSteadyState := make(map[uuid.UUID]bool, maxServerVoiceParticipantIDs)
	for tick := deadlineRounds + 1; tick <= 2*deadlineRounds; tick++ {
		for participantID := range runBudgetedHeartbeat(base.Add(time.Duration(tick) * time.Second)) {
			selectedDuringSteadyState[participantID] = true
		}
	}
	require.Equal(t, maxServerVoiceParticipantIDs, len(selectedDuringSteadyState),
		"oldest-lifecycle ordering must prevent a stable room tail from starving")
}

func TestServerHeartbeatBulkFanoutStopsWithHub(t *testing.T) {
	hub := websocket.NewHub(nil, nil)
	go hub.Run()
	hub.Shutdown()
	reconnects := 0
	subscriber := &NATSSubscriber{
		hub: hub,
		log: logger.New("test"),
		disconnectAllRichPresenceClientsHook: func() {
			reconnects++
		},
	}
	room := &roomContext{serverID: uuid.NewString(), serverUUID: uuid.New()}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	completed := make(chan struct{})
	go func() {
		defer close(completed)
		for index := 0; index < maxServerVoiceParticipantIDs; index++ {
			subscriber.broadcastServerVoiceParticipantContext(
				ctx, room, uuid.New(), uuid.New(), "left",
			)
		}
		subscriber.broadcastRoomEmpty(ctx, uuid.NewString(), room)
	}()
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("bulk server voice fanout remained blocked after Hub shutdown")
	}
	require.Equal(t, 1, reconnects,
		"a dropped terminal room-empty delta must force conservative reconnect")
}

func TestTempGrantHeartbeatCleanupUsesLifecycleContext(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "nats.go")) // #nosec G304 -- runtime.Caller plus fixed repository path
	require.NoError(t, err)
	start := strings.Index(string(source), "func (s *NATSSubscriber) revokeTempGrantIfHeld(")
	end := strings.Index(string(source), "func (s *NATSSubscriber) broadcastRoomEmpty(")
	require.Greater(t, start, -1)
	require.Greater(t, end, start)
	require.NotContains(t, string(source[start:end]), "context.Background()",
		"stale heartbeat cleanup must remain bounded by its lifecycle context")
}

func ignoredCleanupCalls(filename string, source []byte) ([]string, error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, filename, source, 0)
	if err != nil {
		return nil, err
	}
	var findings []string
	record := func(expression ast.Expr) {
		if cleanup, found := cleanupCall(expression); found {
			findings = append(findings, files.Position(expression.Pos()).String()+": "+cleanup)
		}
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.ExprStmt:
			record(typed.X)
		case *ast.DeferStmt:
			record(typed.Call)
		case *ast.AssignStmt:
			blank := false
			for _, target := range typed.Lhs {
				identifier, ok := target.(*ast.Ident)
				blank = blank || ok && identifier.Name == "_"
			}
			if blank {
				for _, expression := range typed.Rhs {
					record(expression)
				}
			}
		}
		return true
	})
	return findings, nil
}

func cleanupCall(expression ast.Expr) (string, bool) {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if selector.Sel.Name == "Close" || selector.Sel.Name == "Del" {
		return selector.Sel.Name, true
	}
	return cleanupCall(selector.X)
}

func TestNATSLifecycleDoesNotSuppressResourceCleanupErrors(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "nats.go"))
	require.NoError(t, err)

	ignoredCleanup, err := ignoredCleanupCalls("nats.go", source)
	require.NoError(t, err)
	require.Empty(t, ignoredCleanup,
		"resource/replay cleanup errors must be returned or joined")
	require.NotContains(t, string(source), "RETURNING conversation_id",
		"private joins must delete all old scopes with bounded Exec/RowsAffected")
	require.False(t, strings.Contains(string(source), "ARRAY_AGG(channel_id::text ORDER BY channel_id) FROM removed"),
		"legacy server audience collection must use a bounded LIMIT+1 read")
	privateJoinApplyStart := strings.Index(
		string(source), "func (mutation *privateVoiceJoinMutation) apply(",
	)
	privateJoinHandlerStart := strings.Index(
		string(source), "func (s *NATSSubscriber) handlePrivateVoiceJoined(",
	)
	require.NotEqual(t, -1, privateJoinApplyStart)
	require.Greater(t, privateJoinHandlerStart, privateJoinApplyStart)
	privateJoinApply := string(source[privateJoinApplyStart:privateJoinHandlerStart])
	baseBroadcast := strings.Index(privateJoinApply, "broadcastPrivateVoiceJoined(")
	postCommitCleanup := strings.Index(
		privateJoinApply, "deleteCapturedPrivateActivityGenerations(",
	)
	require.NotEqual(t, -1, baseBroadcast)
	require.Greater(t, postCommitCleanup, baseBroadcast,
		"a durable private join must broadcast before fallible post-commit cleanup")

	privateJoinHandlerEnd := strings.Index(
		string(source), "func (s *NATSSubscriber) broadcastPrivateVoiceJoined(",
	)
	require.Greater(t, privateJoinHandlerEnd, privateJoinHandlerStart)
	privateJoinHandler := string(source[privateJoinHandlerStart:privateJoinHandlerEnd])
	failureStart := strings.Index(privateJoinHandler, "if activityErr != nil")
	failureEnd := strings.Index(privateJoinHandler, "if !mutation.applied")
	require.NotEqual(t, -1, failureStart)
	require.Greater(t, failureEnd, failureStart)
	require.Contains(t, privateJoinHandler[failureStart:failureEnd],
		"return mutation.durablyApplied",
		"a post-commit activity failure must preserve the durable base join result")

	joinedStart := strings.Index(string(source), "func (s *NATSSubscriber) handleJoined(")
	joinedEnd := strings.Index(string(source), "func (s *NATSSubscriber) handleLeft(")
	require.NotEqual(t, -1, joinedStart)
	require.Greater(t, joinedEnd, joinedStart)
	require.NotContains(t, string(source[joinedStart:joinedEnd]), "s.refreshDMVoiceCallLease(",
		"the private joined handler must not refresh a lease before its membership-locked transaction")

	heartbeatStart := strings.Index(string(source), "func (s *NATSSubscriber) refreshDMHeartbeat(")
	heartbeatClaim := strings.Index(string(source[heartbeatStart:]), "s.withVoiceLifecycleClaims(")
	require.NotEqual(t, -1, heartbeatStart)
	require.Greater(t, heartbeatClaim, 0)
	require.NotContains(t,
		string(source[heartbeatStart:heartbeatStart+heartbeatClaim]),
		"s.refreshDMVoiceCallLease(",
		"the private heartbeat handler must not refresh a lease before membership rows are locked",
	)
	require.NotContains(t, string(source), "if len(acceptedParticipants) == 0",
		"all-rejected heartbeats must continue into watermark-guarded ghost reconciliation")
}

func TestIgnoredCleanupCallControls(t *testing.T) {
	for _, source := range []string{
		`package voice; func f() { rows.Close() }`,
		`package voice; func f() { defer rows.Close() }`,
		`package voice; func f() { _ = client.Del(ctx, key) }`,
		`package voice; func f() { client.Del(ctx, key).Err() }`,
	} {
		findings, err := ignoredCleanupCalls("control.go", []byte(source))
		require.NoError(t, err)
		require.NotEmpty(t, findings, source)
	}
	findings, err := ignoredCleanupCalls("negative.go", []byte(`package voice
func f() {
	if err := rows.Close(); err != nil { return }
	if err := client.Del(ctx, key).Err(); err != nil { return }
}`))
	require.NoError(t, err)
	require.Empty(t, findings)
}

func TestVoiceRoomResolutionUsesLifecycleContext(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "nats.go"))
	require.NoError(t, err)
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, "nats.go", source, 0)
	require.NoError(t, err)
	methods := make(map[string]*ast.FuncDecl)
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv != nil {
			methods[function.Name.Name] = function
		}
	}

	resolve := methods["resolveRoom"]
	require.NotNil(t, resolve)
	queryUsesContext := false
	ast.Inspect(resolve.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "QueryRowContext" {
			return true
		}
		identifier, ok := call.Args[0].(*ast.Ident)
		queryUsesContext = ok && identifier.Name == "ctx"
		return true
	})
	require.True(t, queryUsesContext, "resolveRoom must bind its query to the lifecycle context")

	for _, methodName := range []string{
		"handleJoined", "handleLeft", "handleRoomEmpty", "handleHeartbeat",
	} {
		method := methods[methodName]
		require.NotNil(t, method)
		var timeoutPosition, resolvePosition token.Pos
		resolveArgumentCount := 0
		ast.Inspect(method.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "WithTimeout":
				if timeoutPosition == token.NoPos {
					timeoutPosition = call.Pos()
				}
			case "resolveRoom":
				if resolvePosition == token.NoPos {
					resolvePosition = call.Pos()
					resolveArgumentCount = len(call.Args)
				}
			}
			return true
		})
		require.NotEqual(t, token.NoPos, timeoutPosition, methodName)
		require.NotEqual(t, token.NoPos, resolvePosition, methodName)
		require.Less(t, timeoutPosition, resolvePosition,
			methodName+" must create its lifecycle timeout before room resolution")
		require.Equal(t, 2, resolveArgumentCount,
			methodName+" must pass the lifecycle context into room resolution")
	}
}
