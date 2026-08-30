package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type preboundActivityHistoryDelivery struct{}

func (preboundActivityHistoryDelivery) DeliverCustomText(
	context.Context,
	presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	return presencehistory.DeliveryAck{}, nil
}

func TestNewRouterRequiresOneUnboundActivityHistoryService(t *testing.T) {
	cfg := &config.Config{Environment: "test"}
	log := logger.NewWithWriter(io.Discard)

	_, _, _, _, _, _, _, _, _, err := api.NewRouter(nil, nil, nil, cfg, nil, log, api.RouterDependencies{})
	require.Error(t, err)

	service := presencehistory.NewService(nil, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(preboundActivityHistoryDelivery{}))
	_, _, _, _, _, _, _, _, _, err = api.NewRouter(
		nil,
		nil,
		nil,
		cfg,
		nil,
		log,
		api.RouterDependencies{PresenceHistory: service},
	)
	require.Error(t, err)
}

func TestNewRouterActivityHistoryWiringOrderIsSingleAndFinal(t *testing.T) {
	sourceBytes, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	source := string(sourceBytes)
	needles := []string{
		"hub := websocket.NewHub",
		"bindPresenceHistoryRuntime(hub, presenceHistoryService)",
		"authHandler.SetPresenceHistory(presenceHistoryService)",
		"usersHandler.SetPresenceHistory(presenceHistoryService)",
		"presenceHistoryHandler := presencehistory.NewHandler(presenceHistoryService)",
		"presenceHistoryHandler.RegisterRoutes(",
		"opsRuntime := wireOpsMetricsRuntime(",
		"go hub.Run()",
		// #2445 added a 7th return value (the shared presence-recheck executor);
		// #2738 added an 8th, the closer that drains BOTH presence dispatch
		// workers at shutdown — nothing called either Close before it. #2448
		// added a 9th: the durable active-category reconciler, carried out so
		// its startup pass and ticker join cmd/server's Activity History runtime
		// instead of a second one under no guard.
		// #2666 added a 10th: the ownership expiry callback, carried out so
		// scheduled ownership writes use the same capture-bound transaction.
		"return router, hub, natsClient, opsRuntime, voicePermEnforcer, " +
			"presenceRecheckExecutor, closePresenceWorkers, activePlanReconciler, " +
			"ownershipHandler.CompleteExpiredTransfers, nil",
	}
	prior := -1
	for _, needle := range needles {
		require.Equal(t, 1, strings.Count(source, needle), needle)
		position := strings.Index(source, needle)
		require.Greater(t, position, prior, "wiring order for %s", needle)
		prior = position
	}

	helperStart := strings.Index(source, "func bindPresenceHistoryRuntime(")
	require.GreaterOrEqual(t, helperStart, 0)
	helperEnd := strings.Index(source[helperStart:], "\n}\n")
	require.Greater(t, helperEnd, 0)
	helperSource := source[helperStart : helperStart+helperEnd]
	setPosition := strings.Index(helperSource, "hub.SetPresenceHistoryService(presenceHistoryService)")
	bindPosition := strings.Index(helperSource, "presenceHistoryService.BindDelivery(hub)")
	require.GreaterOrEqual(t, setPosition, 0)
	require.Greater(t, bindPosition, setPosition)
	require.Equal(t, 1, strings.Count(source, "hub.SetPresenceHistoryService(presenceHistoryService)"))
	require.Equal(t, 1, strings.Count(source, "presenceHistoryService.BindDelivery(hub)"))
}

func TestActivityHistoryRoutesAreAuthenticatedSelfOnlyAndUseConcreteLimits(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	paths := []struct {
		method string
		path   string
		body   map[string]interface{}
		limit  string
	}{
		{method: http.MethodGet, path: "/api/v1/users/me/presence-history/settings", limit: "30"},
		{method: http.MethodPatch, path: "/api/v1/users/me/presence-history/settings", body: map[string]interface{}{}, limit: "10"},
		{method: http.MethodGet, path: "/api/v1/users/me/presence-history", limit: "30"},
		{method: http.MethodDelete, path: "/api/v1/users/me/presence-history", limit: "10"},
	}

	for _, route := range paths {
		response := ts.DoRequest(route.method, route.path, route.body, nil)
		require.Equal(t, http.StatusUnauthorized, response.Code, "%s %s", route.method, route.path)
	}

	user := ts.CreateTestUser(t, "history_router_paths")
	headers := testhelpers.AuthHeaders(user.AccessToken)
	for _, route := range paths {
		response := ts.DoRequest(route.method, route.path, route.body, headers)
		require.Equal(t, route.limit, response.Header().Get("X-RateLimit-Limit"),
			"%s %s", route.method, route.path)
	}
	for _, path := range []string{
		"/api/v1/users/users/me/presence-history/settings",
		"/api/v1/users/" + uuid.NewString() + "/presence-history/settings",
	} {
		response := ts.DoRequest(http.MethodGet, path, nil, headers)
		require.Equal(t, http.StatusNotFound, response.Code, path)
	}

	readUser := ts.CreateTestUser(t, "history_router_read_limit")
	readHeaders := testhelpers.AuthHeaders(readUser.AccessToken)
	for request := 1; request <= 31; request++ {
		response := ts.DoRequest(http.MethodGet,
			"/api/v1/users/me/presence-history/settings", nil, readHeaders)
		if request <= 30 {
			require.Equal(t, http.StatusOK, response.Code, "read request %d", request)
		} else {
			require.Equal(t, http.StatusTooManyRequests, response.Code)
		}
	}

	mutationUser := ts.CreateTestUser(t, "history_router_mutation_limit")
	mutationHeaders := testhelpers.AuthHeaders(mutationUser.AccessToken)
	for request := 1; request <= 11; request++ {
		response := ts.DoRequest(http.MethodPatch,
			"/api/v1/users/me/presence-history/settings", map[string]interface{}{}, mutationHeaders)
		if request <= 10 {
			require.Equal(t, http.StatusBadRequest, response.Code, "mutation request %d", request)
		} else {
			require.Equal(t, http.StatusTooManyRequests, response.Code)
		}
	}
}

func TestRouterUsesTestServerPresenceHistoryGateForWriterAndReconnectSnapshot(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sender := ts.CreateTestUser(t, "history_shared_gate_sender")
	viewer := ts.CreateTestUser(t, "history_shared_gate_viewer")
	senderID := uuid.MustParse(sender.ID)
	_, err := ts.DB.Exec(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'accepted')
	`, sender.ID, viewer.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		VALUES ($1, 1, 'before gate')
	`, sender.ID)
	require.NoError(t, err)

	gateHeld := make(chan struct{})
	releaseGate := make(chan struct{})
	holderDone := make(chan error, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseGate) }) }
	t.Cleanup(release)
	go func() {
		holderDone <- ts.PresenceHistory.WithSender(context.Background(), senderID, func() error {
			close(gateHeld)
			<-releaseGate
			return nil
		})
	}()
	select {
	case <-gateHeld:
	case <-time.After(3 * time.Second):
		t.Fatal("test holder did not acquire shared sender gate")
	}

	writerBody, err := json.Marshal(map[string]interface{}{
		"custom_text_tier": 1,
		"custom_text":      "after gate",
	})
	require.NoError(t, err)
	writerRequest := httptest.NewRequest(http.MethodPatch,
		"/api/v1/users/me/presence-settings", bytes.NewReader(writerBody))
	for key, values := range testhelpers.AuthHeaders(sender.AccessToken) {
		writerRequest.Header[key] = values
	}
	writerResponse := httptest.NewRecorder()
	writerDone := make(chan struct{})
	go func() {
		ts.Router.ServeHTTP(writerResponse, writerRequest)
		close(writerDone)
	}()
	select {
	case <-writerDone:
		t.Fatalf("HTTP writer escaped shared Service gate with status %d", writerResponse.Code)
	case <-time.After(100 * time.Millisecond):
	}

	httpServer := httptest.NewServer(ts.Router)
	defer httpServer.Close()
	socketURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		"/api/v1/ws?token=" + url.QueryEscape(viewer.AccessToken)
	connection, _, err := websocket.DefaultDialer.Dial(socketURL, nil)
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()

	replacementFrames := make(chan string, 3)
	readError := make(chan error, 1)
	go func() {
		for {
			_, payload, readErr := connection.ReadMessage()
			if readErr != nil {
				readError <- readErr
				return
			}
			var envelope struct {
				Type string `json:"type"`
				Data struct {
					UserID   string `json:"user_id"`
					Category string `json:"category"`
				} `json:"data"`
			}
			if json.Unmarshal(payload, &envelope) != nil {
				continue
			}
			switch envelope.Type {
			case "presence_snapshot":
				replacementFrames <- envelope.Type
			case "rich_presence_update":
				if envelope.Data.Category != "custom_text" || envelope.Data.UserID != sender.ID {
					continue
				}
				replacementFrames <- "custom_text"
			}
		}
	}()
	select {
	case frame := <-replacementFrames:
		t.Fatalf("replacement frame %q escaped before all sender-gated Custom work completed", frame)
	case err := <-readError:
		t.Fatalf("WebSocket read while gate held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case <-writerDone:
		require.Equal(t, http.StatusOK, writerResponse.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP writer did not resume after gate release")
	}
	select {
	case err := <-holderDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("gate holder did not finish")
	}
	for index, expected := range []string{"presence_snapshot", "custom_text", "custom_text"} {
		select {
		case actual := <-replacementFrames:
			require.Equal(t, expected, actual, "replacement frame %d", index+1)
		case err := <-readError:
			t.Fatalf("WebSocket read after gate release: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatalf("received %d/3 ordered replacement frames after gate release", index)
		}
	}
}
