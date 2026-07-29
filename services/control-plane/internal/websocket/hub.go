package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/klipy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const (
	keyChannelID      = "channel_id"
	keyConversationID = "conversation_id"
	keyUserID         = "user_id"
	keyServerID       = "server_id"
	keyMessage        = "message"
	keyContent        = "content"
	keyKeyVersion     = "key_version"
	keyCreatedAt      = "created_at"
	keyUpdatedAt      = "updated_at"
	keyUsername       = "username"
	keyIsTyping       = "is_typing"
	keyStatus         = "status"
	keyCounts         = "counts"
	keyNonce          = "nonce"
	// Aliased from internal/presence so the rich-presence policy and the hub
	// cannot drift on a privacy-critical value.
	statusOnline    = presence.StatusOnline
	statusOffline   = presence.StatusOffline
	statusInvisible = presence.StatusInvisible
	statusDND       = presence.StatusDND
	sessionRevoked  = "session_revoked"

	errMsgFailedSaveMessage    = "Failed to save message"
	errMsgFailedVerifyKeyEpoch = "Failed to verify key epoch"

	clientBootstrapConcurrency = 8
	// Bounds concurrent hidden-sender suppressions so a deploy-time mass
	// disconnect cannot spawn one goroutine per user, each contending for a
	// sender-gate stripe.
	richPresenceSuppressorSlots = 16
	clientBootstrapTimeout      = 5 * time.Second
)

type presenceRecoveryState struct {
	status  string
	pending bool
}

// Hub maintains the set of active clients and broadcasts messages
type Hub struct {
	// Database connection for authorization checks
	db *sql.DB

	// Redis client for presence persistence
	redis *redis.Client

	// Optional aggregate counter sink. Nil keeps metrics-disabled behavior inert.
	opsCounter OpsCounter
	// Optional trusted-side activity observer. It receives only first-client and
	// last-client transitions for each user and must not perform blocking I/O.
	activityObserver UserActivityObserver

	// Mutex protecting maps accessed from outside the Run goroutine
	mu sync.RWMutex

	// Deterministic test seam invoked after a custom-text snapshot authorizes a
	// viewer but before it enqueues the frame. Nil in production.
	customTextSnapshotBeforeEnqueue func(senderID, viewerID uuid.UUID)
	// Deterministic test seam invoked after the bounded candidate query and
	// before any sender gate is acquired. Nil in production.
	customTextSnapshotAfterCandidates func()
	// Deterministic test seam invoked after the snapshot reads the sender's
	// payload but before it computes the audience. Nil in production.
	customTextSnapshotAfterStateRead func(senderID, viewerID uuid.UUID)
	// The same concrete service instance is injected into writers and the Hub.
	// Snapshots use its sender gate; typed delivery never reacquires that gate.
	presenceHistoryService *presencehistory.Service
	// Activity reconnect state is rebuilt outside Run with fresh policy checks.
	activitySnapshotService *presence.ActivitySnapshotService
	// Deterministic test seam; SetActivitySnapshotService installs the concrete
	// service method in production before Run starts.
	activitySnapshot func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error)
	// Finalization reloads exact Redis generations and reauthorizes while the
	// client publication barrier is held.
	activitySnapshotFinalize func(
		context.Context,
		uuid.UUID,
		presence.ActivitySnapshot,
		func(presence.ActivitySnapshot) error,
	) error
	// A Hub-wide semaphore and deadline bound reconnect amplification.
	clientBootstrapSlots   chan struct{}
	clientBootstrapTimeout time.Duration
	// richPresenceHiddenSuppressor clears a sender's active Rich Presence when
	// their base presence transitions to invisible or offline (#2444). Injected
	// so the hub does not depend on internal/presence's service construction,
	// mirroring dmRingCanceller.
	richPresenceHiddenSuppressor      func(uuid.UUID)
	richPresenceHiddenForceSuppressor func(uuid.UUID)
	suppressorMu                      sync.Mutex
	suppressorPending                 map[uuid.UUID]struct{}
	suppressorForced                  map[uuid.UUID]bool
	suppressorRunning                 int
	suppressorWg                      sync.WaitGroup
	// Deterministic test seam invoked after capacity preflight and before the
	// first nonblocking completion write.
	clientBootstrapBeforeFlush func()
	// Deterministic test seam invoked after the snapshot write while sendMu is
	// still held, proving generic writers cannot interleave with replay/live.
	clientBootstrapAfterFirstFrame func()
	// Per-Hub test seams for deterministic delivery failures and cancellation.
	// Nil uses the production JSON marshaler, queue broadcaster, and socket close.
	customTextFrameMarshaler        func(uuid.UUID, *CustomTextPayload) ([]byte, error)
	richPresenceFrameMarshaler      richPresenceFrameMarshaler
	customTextDeliveryBroadcaster   func(context.Context, *Client, []byte) error
	customTextDeliveryBeforeEnqueue func()
	customTextClientDisconnect      func(*Client) error

	// Registered clients (client ID -> Client)
	clients map[uuid.UUID]*Client

	// User ID to client IDs mapping (for multi-device support)
	userClients map[uuid.UUID]map[uuid.UUID]bool

	// Hub-local fail-closed presence for connected users. Values are the status
	// visible to the user themselves (invisible or offline); every other viewer
	// sees offline. Run owns direct reads and write decisions; writes use
	// mutex-owning helpers so off-loop bootstrap readers can hold h.mu.RLock.
	hiddenPresence map[uuid.UUID]string

	// Trusted visible status for connected users and whether fail-closed
	// recovery is pending after a Redis write or read failure. Generic missing
	// keys never set pending and therefore remain fail closed.
	presenceRecovery map[uuid.UUID]presenceRecoveryState

	// Channel subscriptions (channel ID -> set of client IDs)
	channelSubscriptions map[uuid.UUID]map[uuid.UUID]bool

	// Username cache (user ID -> username) for typing indicators
	usernames map[uuid.UUID]string

	// Server subscriptions (server ID -> set of client IDs)
	// Used for lightweight unread notifications without full message delivery
	serverSubscriptions map[uuid.UUID]map[uuid.UUID]bool

	// DM conversation subscriptions (conversation ID -> set of client IDs)
	dmSubscriptions map[uuid.UUID]map[uuid.UUID]bool

	// Register requests from clients
	register chan *Client

	// Unregister requests from clients
	unregister chan *Client

	// Incoming messages from clients
	incoming chan IncomingMessage

	// Broadcast messages to all clients in a channel
	broadcast chan BroadcastMessage

	// Global broadcast messages to all connected clients
	globalBroadcast chan OutgoingMessage

	// User-scoped broadcast messages (sent to all clients of a specific user)
	userBroadcast chan UserBroadcastMessage

	// Server-scoped broadcast messages (sent to all clients subscribed to a server)
	serverBroadcast chan ServerBroadcastMessage

	// Server-scoped eviction broadcasts (member_removed/banned + attached prune).
	// Kept on a dedicated, priority-drained channel so a removed/banned member is
	// evicted from serverSubscriptions BEFORE any other server fanout (e.g.
	// server_updated from handleServerUpdate, key_revocation) can leak to them,
	// regardless of what is queued on serverBroadcast. CV-CAN-027/028.
	evictBroadcast chan ServerBroadcastMessage

	// DM-scoped broadcast messages (sent to all clients subscribed to a DM conversation)
	dmBroadcast chan DMBroadcastMessage

	// Force-disconnect all clients for a user (server-side session termination)
	disconnectUser chan uuid.UUID

	// Force-disconnect a specific session (targeted single-session revocation)
	disconnectSession chan string

	// Signal to recompute and broadcast server voice counts
	voiceCountSignal chan struct{}

	// Revalidate channel subscriptions after RBAC/SBAC changes. These are consumed
	// on the Run goroutine, which owns subscription maps.
	revalidateChannel chan channelRevalidation
	revalidateServer  chan uuid.UUID

	// Results from off-loop channel permission checks. The Run goroutine applies
	// sends and subscription pruning so hub maps remain single-owner.
	channelDeliveryResults chan channelDeliveryResult

	// Results from off-loop voice-count queries issued on subscribe_server. The
	// query runs on a worker goroutine so a burst of subscriptions cannot stall
	// the Run loop on per-subscribe DB round-trips; the Run goroutine applies the
	// scoped send. See dispatchSubscribedVoiceCounts / CV-CAN-030.
	voiceCountCatchupResults chan voiceCountCatchup

	// Coalesces the on-subscribe voice-count catch-up query. The aggregation is
	// global (identical for every subscriber), so a reconnect storm's concurrent
	// dispatches share a single in-flight query instead of each running its own
	// full-table scan. See dispatchSubscribedVoiceCounts / CV-CAN-030.
	voiceCountSF singleflight.Group

	// Shutdown signal
	done chan struct{}

	// Closed when Run() returns, allowing callers to wait for a clean exit
	stopped chan struct{}

	// Debounced online-count broadcasting: instead of hitting DB+Redis on every
	// presence change, we accumulate affected user IDs and flush after a short
	// delay so bursts (e.g., many users reconnecting) collapse into one broadcast.
	onlineCountPending map[uuid.UUID]bool
	onlineCountTimer   *time.Timer

	// Mention permission checker (injected after construction via SetMentionChecker)
	mentionChecker MentionPermissionChecker

	// Channel permission checker (injected after construction via SetChannelPermissionChecker)
	channelPermissionChecker ChannelPermissionChecker

	// DM voice ring canceller (injected after construction via
	// SetDMRingCanceller). When the user's LAST WS connection drops,
	// the hub invokes this to clean up any caller-initiated DM voice
	// rings the user has in flight. Avoids the "callee rings forever
	// until 45s timeout" UX after caller crash. Per #1209 plan task B7
	// Part 2.
	dmRingCanceller DMRingCanceller

	// closeOnce ensures Shutdown is idempotent (safe to call multiple times)
	closeOnce sync.Once
}

// OpsCounter is the optional aggregate counter sink used after committed writes.
type OpsCounter interface {
	Increment(opsmetrics.MetricKey)
}

// UserActivityObserver receives distinct-user connection boundary events.
type UserActivityObserver interface {
	UserConnected(uuid.UUID)
	UserDisconnected(uuid.UUID)
}

// DMRingCanceller is the signature for the DM voice ring cleanup callback
// invoked from handleUnregister when a user's last WS connection drops.
// The dm.Handler.HandleUserDisconnect method satisfies this type.
type DMRingCanceller func(userID uuid.UUID)

type channelRevalidation struct {
	serverID  uuid.UUID
	channelID uuid.UUID
}

// voiceCountCatchup carries an off-loop voice-count query result back to the Run
// goroutine for the on-subscribe catch-up (CV-CAN-030). clientID identifies the
// subscriber to deliver to; the Run loop re-derives that client's subscribed set
// (which it owns) and performs the send, so the DB query stays off the loop.
type voiceCountCatchup struct {
	clientID uuid.UUID
	counts   map[string]int
}

type channelDeliveryKind uint8

const (
	channelDeliveryBroadcast channelDeliveryKind = iota
	channelDeliveryUnread
	channelDeliveryMention
	channelDeliveryPrune
)

type channelDeliveryRecipient struct {
	clientID uuid.UUID
	userID   uuid.UUID
}

type channelDeliveryRequest struct {
	kind       channelDeliveryKind
	serverID   uuid.UUID
	channelID  uuid.UUID
	viewPerm   int64
	data       []byte
	recipients []channelDeliveryRecipient
}

type channelDeliveryDecision struct {
	clientID   uuid.UUID
	userID     uuid.UUID
	allowed    bool
	definitive bool
}

type channelDeliveryResult struct {
	kind      channelDeliveryKind
	serverID  uuid.UUID
	channelID uuid.UUID
	data      []byte
	decisions []channelDeliveryDecision
}

// NewHub creates a new Hub
func NewHub(db *sql.DB, redisClient *redis.Client, opsCounters ...OpsCounter) *Hub {
	hub := &Hub{
		db:                       db,
		redis:                    redisClient,
		clients:                  make(map[uuid.UUID]*Client),
		userClients:              make(map[uuid.UUID]map[uuid.UUID]bool),
		hiddenPresence:           make(map[uuid.UUID]string),
		presenceRecovery:         make(map[uuid.UUID]presenceRecoveryState),
		channelSubscriptions:     make(map[uuid.UUID]map[uuid.UUID]bool),
		usernames:                make(map[uuid.UUID]string),
		serverSubscriptions:      make(map[uuid.UUID]map[uuid.UUID]bool),
		dmSubscriptions:          make(map[uuid.UUID]map[uuid.UUID]bool),
		register:                 make(chan *Client),
		unregister:               make(chan *Client),
		incoming:                 make(chan IncomingMessage, 256),
		broadcast:                make(chan BroadcastMessage, 256),
		globalBroadcast:          make(chan OutgoingMessage, 256),
		userBroadcast:            make(chan UserBroadcastMessage, 256),
		serverBroadcast:          make(chan ServerBroadcastMessage, 256),
		evictBroadcast:           make(chan ServerBroadcastMessage, 16),
		dmBroadcast:              make(chan DMBroadcastMessage, 256),
		disconnectUser:           make(chan uuid.UUID, 16),
		disconnectSession:        make(chan string, 16),
		voiceCountSignal:         make(chan struct{}, 1),
		revalidateChannel:        make(chan channelRevalidation, 256),
		revalidateServer:         make(chan uuid.UUID, 16),
		channelDeliveryResults:   make(chan channelDeliveryResult, 256),
		voiceCountCatchupResults: make(chan voiceCountCatchup, 256),
		done:                     make(chan struct{}),
		stopped:                  make(chan struct{}),
		onlineCountPending:       make(map[uuid.UUID]bool),
		clientBootstrapSlots:     make(chan struct{}, clientBootstrapConcurrency),
		suppressorPending:        make(map[uuid.UUID]struct{}),
		clientBootstrapTimeout:   clientBootstrapTimeout,
	}
	if len(opsCounters) > 0 {
		hub.opsCounter = opsCounters[0]
	}
	return hub
}

// ConnectionCounts returns registered clients and distinct users from one snapshot.
func (h *Hub) ConnectionCounts() (connections, users int) {
	if h == nil {
		return 0, 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients), len(h.userClients)
}

// ConnectionCount returns the current number of registered WebSocket clients.
func (h *Hub) ConnectionCount() int {
	connections, _ := h.ConnectionCounts()
	return connections
}

// ConnectedUserCount returns the number of distinct users with a local client.
func (h *Hub) ConnectedUserCount() int {
	_, users := h.ConnectionCounts()
	return users
}

// SetActivityObserver installs the trusted-side distinct-user activity sink.
// Existing connected users are seeded so startup races cannot lose an interval.
func (h *Hub) SetActivityObserver(observer UserActivityObserver) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.activityObserver = observer
	connectedUsers := make([]uuid.UUID, 0, len(h.userClients))
	if observer != nil {
		for userID := range h.userClients {
			connectedUsers = append(connectedUsers, userID)
		}
	}
	h.mu.Unlock()

	for _, userID := range connectedUsers {
		observer.UserConnected(userID)
	}
}

// SetPresenceHistoryService binds the concrete service shared by Custom Status
// writers and reconnect snapshots. Runtime wiring calls this before Run.
func (h *Hub) SetPresenceHistoryService(service *presencehistory.Service) {
	h.presenceHistoryService = service
}

// SetActivitySnapshotService binds the freshly authorizing reconnect reader.
// Runtime wiring calls this before Run.
func (h *Hub) SetActivitySnapshotService(service *presence.ActivitySnapshotService) {
	h.activitySnapshotService = service
	if service == nil {
		h.activitySnapshot = nil
		h.activitySnapshotFinalize = nil
		return
	}
	h.activitySnapshot = service.Snapshot
	h.activitySnapshotFinalize = service.FinalizeSnapshot
}

// SetMentionChecker injects the RBAC permission checker for mention enforcement.
// Called after Hub and Resolver are both constructed (breaks circular init dependency).
func (h *Hub) SetMentionChecker(checker MentionPermissionChecker) {
	h.mentionChecker = checker
}

// SetChannelPermissionChecker injects the RBAC checker for channel WebSocket authorization.
func (h *Hub) SetChannelPermissionChecker(checker ChannelPermissionChecker) {
	h.channelPermissionChecker = checker
}

// RevalidateChannelSubscriptions queues a visibility recheck for one channel.
func (h *Hub) RevalidateChannelSubscriptions(serverID, channelID uuid.UUID) {
	if h == nil || h.revalidateChannel == nil {
		return
	}
	select {
	case h.revalidateChannel <- channelRevalidation{serverID: serverID, channelID: channelID}:
	default:
		log.Printf("Channel subscription revalidation queue full")
	}
}

// RevalidateServerSubscriptions queues visibility rechecks for subscribed channels in a server.
func (h *Hub) RevalidateServerSubscriptions(serverID uuid.UUID) {
	if h == nil || h.revalidateServer == nil {
		return
	}
	select {
	case h.revalidateServer <- serverID:
	default:
		log.Printf("Server subscription revalidation queue full")
	}
}

// SetDMRingCanceller injects the DM voice ring cleanup callback invoked
// from handleUnregister on a user's last-WS-connection-drop. Called after
// Hub and dm.Handler are both constructed. If never called, last-connection
// drops are a no-op for DM ring cleanup (callees observe the 45s ring
// timeout as the recovery path — degraded but functional).
func (h *Hub) SetDMRingCanceller(c DMRingCanceller) {
	h.dmRingCanceller = c
}

// SetRichPresenceHiddenSuppressor injects the hidden-sender suppression callback
// invoked when a base-presence transition forbids Rich Presence emission (#2444).
// Mirrors SetDMRingCanceller: installed before Run starts.
func (h *Hub) SetRichPresenceHiddenSuppressor(fn func(uuid.UUID), forced ...func(uuid.UUID)) {
	h.richPresenceHiddenSuppressor = fn
	h.richPresenceHiddenForceSuppressor = fn
	if len(forced) > 0 {
		h.richPresenceHiddenForceSuppressor = forced[0]
	}
}

// spawnRichPresenceSuppression runs the suppressor OFF the Run goroutine.
//
// It must never run inline. The callee acquires the process-local sender gate and
// then performs bounded database queries and a delivery fan-out; blocking Run on
// any of that violates the #1654 responsiveness constraint.
//
// It is deliberately NOT joined to client.asyncWg: the unregister edge fires
// after asyncWg.Wait(), so joining there would be a no-op. suppressorWg is
// drained on shutdown instead.
//
// Callers MUST have already detected a transition EDGE. This function cannot tell
// an edge from a level, and the invisible heartbeat re-asserts the level on every
// beat. Distinct edges are coalesced per sender and drained at fixed concurrency,
// so saturation never drops a privacy clear or blocks the Run goroutine.
func (h *Hub) spawnRichPresenceSuppression(userID uuid.UUID, forced ...bool) {
	force := len(forced) > 0 && forced[0]
	h.spawnRichPresenceSuppressionDuringShutdown(userID, false, force)
}

// spawnRichPresenceSuppressionDuringShutdown preserves offline edges that
// Shutdown established after stopping the Run loop from accepting new work.
func (h *Hub) spawnRichPresenceSuppressionDuringShutdown(
	userID uuid.UUID,
	allowStopping bool,
	forced bool,
) {
	if h == nil || h.richPresenceHiddenSuppressor == nil {
		return
	}
	h.suppressorMu.Lock()
	defer h.suppressorMu.Unlock()
	select {
	case <-h.done:
		if !allowStopping {
			return
		}
	default:
	}
	if h.suppressorPending == nil {
		h.suppressorPending = make(map[uuid.UUID]struct{})
	}
	if h.suppressorForced == nil {
		h.suppressorForced = make(map[uuid.UUID]bool)
	}
	if _, queued := h.suppressorPending[userID]; queued {
		h.suppressorForced[userID] = h.suppressorForced[userID] || forced
		return
	}
	h.suppressorPending[userID] = struct{}{}
	h.suppressorForced[userID] = forced
	h.startPendingRichPresenceSuppressions()
}

// startPendingRichPresenceSuppressions must be called with suppressorMu held.
func (h *Hub) startPendingRichPresenceSuppressions() {
	for h.suppressorRunning < richPresenceSuppressorSlots && len(h.suppressorPending) > 0 {
		var userID uuid.UUID
		for userID = range h.suppressorPending {
			break
		}
		forced := h.suppressorForced[userID]
		delete(h.suppressorPending, userID)
		delete(h.suppressorForced, userID)
		fn := h.richPresenceHiddenSuppressor
		if forced {
			fn = h.richPresenceHiddenForceSuppressor
		}
		if fn == nil {
			return
		}
		h.suppressorRunning++
		h.suppressorWg.Add(1)
		go func(userID uuid.UUID, suppress func(uuid.UUID)) {
			defer func() {
				h.suppressorMu.Lock()
				h.suppressorRunning--
				h.startPendingRichPresenceSuppressions()
				h.suppressorMu.Unlock()
				h.suppressorWg.Done()
			}()
			suppress(userID)
		}(userID, fn)
	}
}

// Run starts the hub's main loop
func (h *Hub) Run() {
	defer close(h.stopped)
	for {
		// nil channel is never selected; active only when a debounce timer is pending
		var onlineCountC <-chan time.Time
		if h.onlineCountTimer != nil {
			onlineCountC = h.onlineCountTimer.C
		}

		// Priority drain: process any pending eviction broadcast before other work.
		// A removed/banned member must be evicted from serverSubscriptions before a
		// concurrently-queued server fanout (e.g. server_updated, key_revocation) can
		// leak to them. evictBroadcast is low-traffic, so this cannot starve the loop.
		// CV-CAN-027/028.
		select {
		case message := <-h.evictBroadcast:
			h.handleServerBroadcast(message)
			continue
		default:
		}

		select {
		case message := <-h.evictBroadcast:
			h.handleServerBroadcast(message)

		case client := <-h.register:
			h.handleRegister(client)

		case client := <-h.unregister:
			h.handleUnregister(client)

		case message := <-h.incoming:
			h.handleIncoming(message)

		case message := <-h.broadcast:
			h.handleBroadcast(message)

		case message := <-h.globalBroadcast:
			h.handleGlobalBroadcast(message)

		case message := <-h.userBroadcast:
			h.handleUserBroadcast(message)

		case message := <-h.serverBroadcast:
			h.handleServerBroadcast(message)

		case message := <-h.dmBroadcast:
			h.handleDMBroadcast(message)

		case userID := <-h.disconnectUser:
			h.handleDisconnectUser(userID)

		case sessionID := <-h.disconnectSession:
			h.handleDisconnectSession(sessionID)

		case <-onlineCountC:
			h.flushOnlineCounts()

		case <-h.voiceCountSignal:
			h.broadcastServerVoiceCounts()

		case req := <-h.revalidateChannel:
			h.handleChannelRevalidation(req)

		case serverID := <-h.revalidateServer:
			h.handleServerRevalidation(serverID)

		case result := <-h.channelDeliveryResults:
			h.handleChannelDeliveryResult(result)

		case c := <-h.voiceCountCatchupResults:
			h.applyVoiceCountCatchup(c)

		case <-h.done:
			h.shutdownClients()
			return
		}
	}
}

func (h *Hub) shutdownClients() {
	// Transition each connected sender before unpublishing clients. h.done is
	// already closed, so this path explicitly admits the resulting suppression
	// work and drains it below.
	h.mu.RLock()
	connectedUsers := make([]uuid.UUID, 0, len(h.userClients))
	for userID := range h.userClients {
		connectedUsers = append(connectedUsers, userID)
	}
	h.mu.RUnlock()
	for _, userID := range connectedUsers {
		h.transitionUserOffline(context.Background(), userID, true)
	}

	// Unpublish clients under the same lock held by typed delivery before
	// closing their queues. A delivery already holding RLock completes first;
	// a later delivery sees no connected target.
	h.mu.Lock()
	clients := make([]*Client, 0, len(h.clients))
	connectedUsers = connectedUsers[:0]
	for clientID, client := range h.clients {
		clients = append(clients, client)
		delete(h.clients, clientID)
	}
	for userID := range h.userClients {
		connectedUsers = append(connectedUsers, userID)
	}
	observer := h.activityObserver
	clear(h.userClients)
	h.mu.Unlock()

	if observer != nil {
		for _, userID := range connectedUsers {
			observer.UserDisconnected(userID)
		}
	}

	// Cancel async work, wait for completion, then close connection queues.
	for _, client := range clients {
		client.cancelBootstrap()
		if client.asyncCancel != nil {
			client.asyncCancel()
		}
		client.asyncWg.Wait()
		client.closeOutbound()
	}
	// Hidden-sender suppressions are hub-scoped, not client-scoped (the offline
	// edge fires during unregister, after asyncWg.Wait()), so they are joined
	// here (#2444).
	h.suppressorWg.Wait()
	log.Printf("Hub shut down, closed %d client connections", len(clients))
}

// Shutdown gracefully stops the hub's Run loop, waits for it to exit, and
// closes all client connections. Safe to call multiple times.
func (h *Hub) Shutdown() {
	h.closeOnce.Do(func() {
		close(h.done)
	})
	<-h.stopped
}

// Stopped returns a channel that is closed when the Run loop has exited.
func (h *Hub) Stopped() <-chan struct{} {
	return h.stopped
}

// handleRegister registers a new client
func (h *Hub) handleRegister(client *Client) {
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel stored in client.asyncCancel, called by Run shutdown and handleUnregister
	client.asyncCancel = cancel
	// Start the replacement boundary before publishing the client in Hub maps so
	// no privacy-critical or base-presence delta can overtake its snapshot.
	client.beginBootstrap()
	isFirstConnection := h.registerClient(client)

	if isFirstConnection {
		ctx := context.Background()
		recovery := presenceRecoveryState{status: statusOnline}
		if err := h.redis.Set(ctx, presence.StatusRedisKey(client.UserID), statusOnline, 120*time.Second).Err(); err != nil {
			log.Printf("[hub] failed to persist initial presence for user %s: %v", sanitizeLogValue(client.UserID.String()), err)
			recovery.pending = true
			h.setPresenceRecovery(client.UserID, recovery)
			// E5 (#2444): a first registration that cannot persist presence
			// fails closed to offline, so any activity from a prior session
			// must not keep publishing.
			h.failClosedPresenceHeartbeat(client.UserID)
		} else {
			if err := h.clearOfflinePresenceFence(ctx, client.UserID); err != nil {
				log.Printf("[hub] failed to clear offline presence fence for user %s: %v", sanitizeLogValue(client.UserID.String()), err)
				recovery.pending = true
				h.setPresenceRecovery(client.UserID, recovery)
				h.failClosedPresenceHeartbeat(client.UserID)
			} else {
				h.setPresenceRecovery(client.UserID, recovery)
				h.clearHiddenPresence(client.UserID)
				h.broadcastPresenceToAll(client.UserID, statusOnline, time.Now().Unix())
			}
		}
	}

	log.Printf("Client registered: user=%s client=%s total_clients=%d",
		sanitizeLogValue(client.UserID.String()), sanitizeLogValue(client.ID.String()), len(h.clients))

	h.sendConnectedConfirmation(client)
	seed := h.capturePresenceSnapshotSeed(client.UserID)
	client.asyncWg.Add(1)
	go func() {
		defer client.asyncWg.Done()
		h.runClientBootstrap(ctx, client, seed)
	}()
}

func (h *Hub) registerClient(client *Client) bool {
	h.mu.Lock()

	h.clients[client.ID] = client

	if _, ok := h.userClients[client.UserID]; !ok {
		h.userClients[client.UserID] = make(map[uuid.UUID]bool)
	}
	h.userClients[client.UserID][client.ID] = true

	if _, cached := h.usernames[client.UserID]; !cached {
		var username string
		err := h.db.QueryRow("SELECT username FROM users WHERE id = $1", client.UserID).Scan(&username)
		if err == nil {
			h.usernames[client.UserID] = username
		} else if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("Failed to load username for connected user %s: %v", sanitizeLogValue(client.UserID.String()), err)
		}
	}

	isFirstConnection := len(h.userClients[client.UserID]) == 1
	observer := h.activityObserver
	h.mu.Unlock()

	if isFirstConnection && observer != nil {
		observer.UserConnected(client.UserID)
	}
	return isFirstConnection
}

func (h *Hub) sendConnectedConfirmation(client *Client) {
	confirmMsg := OutgoingMessage{
		Type: "connected",
		Data: map[string]interface{}{
			"client_id": client.ID,
			keyUserID:   client.UserID,
		},
	}
	data, err := json.Marshal(confirmMsg)
	if err != nil {
		log.Printf("Failed to marshal connected confirmation: %v", err)
		return
	}
	client.enqueueOutboundBlockingImmediate(data)
}

// handleConnectionReadyProbe acknowledges a client's subscribe barrier probe.
//
// Because this handler runs on the Hub's single Run() goroutine, any
// subscribe/subscribe_server/subscribe_dm message queued before the probe is
// guaranteed to be committed to the subscriber map before this method
// executes. The emitted connection_ready frame is a definitive signal that
// the client may safely resume message processing.
//
// Backwards compatible: clients that never send the probe never receive the
// response — behaviour is byte-identical to today for v1 clients.
func (h *Hub) handleConnectionReadyProbe(msg IncomingMessage) {
	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Count this client's server subscriptions across the hub-level map.
	// O(N_servers) per probe but probes fire once per reconnect, not per message.
	subscribedServers := 0
	for _, clients := range h.serverSubscriptions {
		if clients[msg.ClientID] {
			subscribedServers++
		}
	}
	subscribedDMs := 0
	for _, clients := range h.dmSubscriptions {
		if clients[msg.ClientID] {
			subscribedDMs++
		}
	}

	readyMsg := OutgoingMessage{
		Type: "connection_ready",
		Data: map[string]interface{}{
			"subscribed_channels": len(client.Channels),
			"subscribed_servers":  subscribedServers,
			"subscribed_dms":      subscribedDMs,
			"protocol_version":    2,
		},
	}
	data, err := json.Marshal(readyMsg)
	if err != nil {
		log.Printf("Failed to marshal connection_ready message: %v", err)
		return
	}
	if !client.enqueueOutbound(data) {
		// Client send buffer full — log so operators can spot slow consumers.
		// The client times out after 5s and proceeds best-effort, so this
		// isn't fatal but is a monitoring signal worth surfacing.
		log.Printf("[hub] connection_ready dropped for client %s: send buffer full; client will proceed best-effort after 5s timeout", sanitizeLogValue(msg.ClientID.String()))
	}
}

type userPresenceInfo struct {
	UserID       string                                          `json:"user_id"`
	Status       string                                          `json:"status"`
	RichPresence map[presence.Category]richPresenceSnapshotEntry `json:"rich_presence,omitempty"`
}

func (h *Hub) resolveVisibleStatus(ctx context.Context, uid, viewerID uuid.UUID) string {
	if selfStatus, hidden := h.hiddenPresence[uid]; hidden {
		if uid == viewerID {
			return selfStatus
		}
		return statusOffline
	}

	status, err := h.redis.Get(ctx, presence.StatusRedisKey(uid)).Result()
	if errors.Is(err, redis.Nil) {
		if uid != viewerID {
			return statusOffline
		}
		status = statusOnline
	} else if err != nil {
		log.Printf("[hub] presence lookup failed for user %s; hiding status from other viewers: %v", sanitizeLogValue(uid.String()), err)
		if uid != viewerID {
			return statusOffline
		}
		status = statusOnline
	}
	switch status {
	case statusOnline, statusDND:
		return status
	case statusInvisible:
		if uid != viewerID {
			return statusOffline
		}
		return statusInvisible
	default:
		log.Printf("[hub] invalid persisted presence status for user %s: %q", sanitizeLogValue(uid.String()), sanitizeLogValue(status))
		if uid != viewerID {
			return statusOffline
		}
		return statusOnline
	}
}

func (h *Hub) sendPresenceSnapshot(client *Client) {
	ctx := context.Background()
	base, err := h.loadBasePresenceSnapshot(ctx, h.capturePresenceSnapshotSeed(client.UserID))
	if err != nil {
		log.Printf("[hub] failed to build base presence snapshot: %T", err)
		return
	}
	data, err := marshalPresenceSnapshot(base, nil)
	if err != nil {
		log.Printf("Failed to marshal presence snapshot: %v", err)
		return
	}
	client.enqueueOutboundBlocking(data)

}

func (h *Hub) sendVoiceCountsSnapshot(ctx context.Context, client *Client) {
	if h.db == nil {
		return
	}
	// CV-CAN-030: scope the initial voice-count snapshot to the servers this
	// client's user is a member of — a client must not learn voice activity for
	// servers it does not belong to. This runs off the Run goroutine (registration
	// goroutine), so it filters via SQL membership rather than the in-memory
	// serverSubscriptions map (which the client has not populated yet at register
	// and which is owned by the Run goroutine).
	rows, err := h.db.QueryContext(ctx, `
		SELECT c.server_id, COUNT(vp.id)
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $1
		GROUP BY c.server_id
	`, client.UserID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("Failed to query voice counts for new client: %v", err)
		return
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close voice-count snapshot rows: %v", closeErr)
		}
	}()

	counts := make(map[string]int)
	for rows.Next() {
		var serverID string
		var count int
		if err := rows.Scan(&serverID, &count); err != nil {
			log.Printf("Failed to scan voice count for new client: %v", err)
			return
		}
		counts[serverID] = count
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("Failed to iterate voice counts for new client: %v", err)
		return
	}

	msg := OutgoingMessage{
		Type: "server_voice_counts",
		Data: map[string]interface{}{
			keyCounts: counts,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal voice-count snapshot: %v", err)
		return
	}
	client.enqueuePostBootstrap(ctx, data)
}

// handleUnregister unregisters a client
func (h *Hub) handleUnregister(client *Client) {
	exists, isLastConnection, remaining := h.unregisterClient(client)
	if !exists {
		return
	}

	client.cancelBootstrap()
	if client.asyncCancel != nil {
		client.asyncCancel()
	}
	client.asyncWg.Wait()
	client.closeOutbound()

	if isLastConnection {
		now := time.Now().Unix()
		ctx := context.Background()
		h.transitionUserOffline(ctx, client.UserID, false)
		if err := h.redis.Set(ctx, fmt.Sprintf("last_seen:%s", client.UserID), fmt.Sprintf("%d", now), 0).Err(); err != nil {
			log.Printf("[hub] failed to persist last_seen for user %s: %v", sanitizeLogValue(client.UserID.String()), err)
		}

		// Cancel any DM voice rings this user initiated (#1209 B7 Part 2).
		// Invoke async to avoid blocking handleUnregister on what is
		// best-effort cleanup. If the callback isn't wired (test/dev),
		// nil-check skips silently.
		if h.dmRingCanceller != nil {
			go h.dmRingCanceller(client.UserID)
		}
	}

	log.Printf("Client unregistered: user=%s client=%s remaining=%d",
		sanitizeLogValue(client.UserID.String()), sanitizeLogValue(client.ID.String()), remaining)
}

func (h *Hub) transitionUserOffline(ctx context.Context, userID uuid.UUID, allowStopping bool) {
	h.clearPresenceRecovery(userID)
	fencePersisted := false
	if err := h.persistOfflinePresenceFence(ctx, userID); err != nil {
		log.Printf("[hub] failed to persist offline presence fence for user %s: %v", sanitizeLogValue(userID.String()), err)
	} else if h.db != nil {
		fencePersisted = true
	}
	offlinePersisted := false
	if h.redis == nil {
		log.Printf("[hub] presence Redis client unavailable while taking user %s offline", sanitizeLogValue(userID.String()))
	} else if err := h.redis.Set(ctx, presence.StatusRedisKey(userID), statusOffline, 120*time.Second).Err(); err != nil {
		log.Printf("[hub] failed to persist offline presence for user %s: %v", sanitizeLogValue(userID.String()), err)
	} else {
		offlinePersisted = true
	}

	h.clearHiddenPresence(userID)
	h.spawnRichPresenceSuppressionDuringShutdown(
		userID, allowStopping, !fencePersisted && !offlinePersisted,
	)
	h.broadcastPresenceToAll(userID, statusOffline, time.Now().Unix())
}

func (h *Hub) persistOfflinePresenceFence(ctx context.Context, userID uuid.UUID) error {
	if h.db == nil {
		return nil
	}
	_, err := h.db.ExecContext(ctx, `
		INSERT INTO presence_offline_fences (user_id, offline_at)
		VALUES ($1, NOW())
		ON CONFLICT (user_id) DO UPDATE
		SET offline_at = EXCLUDED.offline_at
	`, userID)
	return err
}

func (h *Hub) clearOfflinePresenceFence(ctx context.Context, userID uuid.UUID) error {
	if h.db == nil {
		return nil
	}
	_, err := h.db.ExecContext(ctx, `DELETE FROM presence_offline_fences WHERE user_id = $1`, userID)
	return err
}

func (h *Hub) unregisterClient(client *Client) (bool, bool, int) {
	h.mu.Lock()
	_, exists := h.clients[client.ID]
	if !exists {
		remaining := len(h.clients)
		h.mu.Unlock()
		return false, false, remaining
	}

	delete(h.clients, client.ID)
	isLastConnection := h.removeUserClient(client)
	h.removeClientSubscriptions(client)
	remaining := len(h.clients)
	observer := h.activityObserver
	h.mu.Unlock()

	if isLastConnection && observer != nil {
		observer.UserDisconnected(client.UserID)
	}
	return true, isLastConnection, remaining
}

func (h *Hub) removeUserClient(client *Client) bool {
	userClients, ok := h.userClients[client.UserID]
	if !ok {
		return false
	}
	delete(userClients, client.ID)
	if len(userClients) == 0 {
		delete(h.userClients, client.UserID)
		delete(h.usernames, client.UserID)
		return true
	}
	return false
}

func (h *Hub) removeClientSubscriptions(client *Client) {
	for channelID := range client.Channels {
		h.removeFromSubscriptionMap(h.channelSubscriptions, channelID, client.ID)
	}
	for serverID := range h.serverSubscriptions {
		h.removeFromSubscriptionMap(h.serverSubscriptions, serverID, client.ID)
	}
	for convID := range h.dmSubscriptions {
		h.removeFromSubscriptionMap(h.dmSubscriptions, convID, client.ID)
	}
}

func (h *Hub) removeFromSubscriptionMap(m map[uuid.UUID]map[uuid.UUID]bool, key, clientID uuid.UUID) {
	clients, ok := m[key]
	if !ok {
		return
	}
	delete(clients, clientID)
	if len(clients) == 0 {
		delete(m, key)
	}
}

// handleIncoming processes incoming messages from clients
func (h *Hub) handleIncoming(msg IncomingMessage) {
	switch msg.Type {
	case "subscribe":
		h.handleSubscribe(msg)
	case "unsubscribe":
		h.handleUnsubscribe(msg)
	case "subscribe_server":
		h.handleSubscribeServer(msg)
	case "unsubscribe_server":
		h.handleUnsubscribeServer(msg)
	case "message":
		h.handleMessage(msg)
	case "typing":
		h.handleTyping(msg)
	case "profile_update":
		h.handleProfileUpdate(msg)
	case "heartbeat", "set_status":
		h.handlePresenceIncoming(msg)
	case "server_update":
		h.handleServerUpdate(msg)
	case "subscribe_dm":
		h.handleSubscribeDM(msg)
	case "unsubscribe_dm":
		h.handleUnsubscribeDM(msg)
	case "connection_ready_probe":
		h.handleConnectionReadyProbe(msg)
	case "dm_message":
		h.handleDMMessage(msg)
	case "dm_typing":
		h.handleDMTyping(msg)
	default:
		log.Printf("Unknown message type: %s", sanitizeLogValue(msg.Type))
	}
}

func (h *Hub) handlePresenceIncoming(msg IncomingMessage) {
	client, registered := h.clients[msg.ClientID]
	if !registered || client.UserID != msg.UserID {
		return
	}
	if msg.Type == "heartbeat" {
		h.handleHeartbeat(msg)
		return
	}
	h.handleSetStatus(msg)
}

// ChannelPermissionChecker is the interface the Hub needs from the RBAC resolver
// to enforce channel-scoped WebSocket authorization without importing rbac.
type ChannelPermissionChecker interface {
	HasChannelPermission(ctx context.Context, serverID, userID, channelID string, permBit int64) (bool, error)
	HasChannelPermissionsUncached(ctx context.Context, serverID, userID, channelID string, permBits ...int64) (bool, error)
}

const (
	permViewVoiceChannels   int64 = 1 << 9
	permViewTextChannels    int64 = 1 << 10
	permSendMessages        int64 = 1 << 11
	channelAuthCtxTimeout         = 3 * time.Second
	errMsgNotMemberOfServer       = "Not a member of this server"
	errMsgMemberTimedOut          = "Member is timed out"
)

func (h *Hub) rejectActiveMemberTimeout(msg IncomingMessage, serverID, userID uuid.UUID) (bool, time.Time) {
	if h.db == nil {
		return false, time.Time{}
	}

	var timedOutUntil sql.NullTime
	var membershipIncarnation time.Time
	ctx, cancel := context.WithTimeout(context.Background(), channelAuthCtxTimeout)
	defer cancel()
	err := h.db.QueryRowContext(ctx,
		"SELECT timed_out_until, joined_at FROM server_members WHERE server_id = $1 AND user_id = $2",
		serverID, userID,
	).Scan(&timedOutUntil, &membershipIncarnation)
	if errors.Is(err, sql.ErrNoRows) {
		h.sendError(msg.ClientID, errMsgNotMemberOfServer)
		return true, time.Time{}
	}
	if err != nil {
		log.Printf("Failed to check member timeout: %v", err)
		h.sendError(msg.ClientID, "Failed to verify timeout status")
		return true, time.Time{}
	}
	if !timedOutUntil.Valid || !timedOutUntil.Time.After(time.Now().UTC()) {
		return false, membershipIncarnation
	}

	h.sendMemberTimedOut(msg.ClientID, timedOutUntil.Time)
	return true, time.Time{}
}

func (h *Hub) sendMemberTimedOut(clientID uuid.UUID, timedOutUntil time.Time) {
	h.sendErrorWithData(clientID, "member_timed_out", map[string]interface{}{
		keyMessage:        errMsgMemberTimedOut,
		"timed_out_until": timedOutUntil,
	})
}

func (h *Hub) respondPersistMessageError(msg IncomingMessage, persistErr string, timedOutUntil *time.Time) bool {
	if timedOutUntil != nil {
		h.sendMemberTimedOut(msg.ClientID, *timedOutUntil)
		return true
	}
	if persistErr == "" {
		return false
	}
	h.sendError(msg.ClientID, persistErr)
	return true
}

func (h *Hub) authorizeMessageSend(msg IncomingMessage, userID uuid.UUID, chCtx *channelContext, channelUUID uuid.UUID, viewPerm int64) (bool, time.Time) {
	denied := "Not authorized to send messages in this channel"
	rejected, membershipIncarnation := h.rejectActiveMemberTimeout(msg, chCtx.serverUUID, userID)
	if rejected {
		return false, time.Time{}
	}
	if !h.authorizeChannelPermissions(msg, userID, chCtx, channelUUID, []int64{viewPerm, permSendMessages}, denied, true) {
		return false, time.Time{}
	}
	return true, membershipIncarnation
}

func (h *Hub) authorizeChannelPermissions(
	msg IncomingMessage,
	userID uuid.UUID,
	chCtx *channelContext,
	channelUUID uuid.UUID,
	permBits []int64,
	deniedMessage string,
	uncached bool,
) bool {
	if h.channelPermissionChecker == nil {
		log.Printf("Channel permission checker not configured")
		h.sendError(msg.ClientID, "Failed to verify channel access")
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), channelAuthCtxTimeout)
	defer cancel()

	var hasPerm bool
	var err error
	if uncached {
		hasPerm, err = h.channelPermissionChecker.HasChannelPermissionsUncached(
			ctx, chCtx.serverUUID.String(), userID.String(), channelUUID.String(), permBits...,
		)
	} else {
		hasPerm, err = h.channelPermissionChecker.HasChannelPermission(
			ctx, chCtx.serverUUID.String(), userID.String(), channelUUID.String(), permBits[0],
		)
	}
	if err != nil {
		log.Printf("Failed to check channel permission: %v", err)
		h.sendError(msg.ClientID, "Failed to verify channel access")
		return false
	}
	if !hasPerm {
		h.sendError(msg.ClientID, deniedMessage)
		return false
	}
	return true
}

func (h *Hub) clientHasChannelPermission(ctx context.Context, serverID, channelID uuid.UUID, client *Client, perm int64) (allowed, definitive bool) {
	if perm == 0 || serverID == uuid.Nil {
		return true, true
	}
	if h.channelPermissionChecker == nil {
		log.Printf("Channel permission checker not configured")
		return false, false
	}
	hasPerm, err := h.channelPermissionChecker.HasChannelPermission(
		ctx,
		serverID.String(),
		client.UserID.String(),
		channelID.String(),
		perm,
	)
	if err != nil {
		log.Printf("Failed to check channel delivery permission: %v", err)
		return false, false
	}
	return hasPerm, true
}

func (h *Hub) dispatchChannelDelivery(req channelDeliveryRequest) {
	if len(req.recipients) == 0 {
		return
	}
	if req.viewPerm == 0 || req.serverID == uuid.Nil {
		h.handleChannelDeliveryResult(allowAllChannelDelivery(req))
		return
	}

	checker := h.channelPermissionChecker
	if checker == nil {
		log.Printf("Channel permission checker not configured")
		h.handleChannelDeliveryResult(denyAllChannelDelivery(req))
		return
	}

	results := h.channelDeliveryResults
	if results == nil {
		log.Printf("Channel delivery result queue not configured")
		return
	}

	done := h.done
	go func() {
		result := checkChannelDeliveryPermissions(req, checker)
		select {
		case results <- result:
		case <-done:
		}
	}()
}

func allowAllChannelDelivery(req channelDeliveryRequest) channelDeliveryResult {
	result := channelDeliveryResult{
		kind:      req.kind,
		serverID:  req.serverID,
		channelID: req.channelID,
		data:      req.data,
		decisions: make([]channelDeliveryDecision, 0, len(req.recipients)),
	}
	for _, recipient := range req.recipients {
		result.decisions = append(result.decisions, channelDeliveryDecision{
			clientID:   recipient.clientID,
			userID:     recipient.userID,
			allowed:    true,
			definitive: true,
		})
	}
	return result
}

func denyAllChannelDelivery(req channelDeliveryRequest) channelDeliveryResult {
	result := channelDeliveryResult{
		kind:      req.kind,
		serverID:  req.serverID,
		channelID: req.channelID,
		data:      req.data,
		decisions: make([]channelDeliveryDecision, 0, len(req.recipients)),
	}
	for _, recipient := range req.recipients {
		result.decisions = append(result.decisions, channelDeliveryDecision{
			clientID:   recipient.clientID,
			userID:     recipient.userID,
			allowed:    false,
			definitive: false,
		})
	}
	return result
}

func checkChannelDeliveryPermissions(req channelDeliveryRequest, checker ChannelPermissionChecker) channelDeliveryResult {
	result := channelDeliveryResult{
		kind:      req.kind,
		serverID:  req.serverID,
		channelID: req.channelID,
		data:      req.data,
		decisions: make([]channelDeliveryDecision, 0, len(req.recipients)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), channelAuthCtxTimeout)
	defer cancel()

	for _, recipient := range req.recipients {
		hasPerm, err := checker.HasChannelPermission(
			ctx,
			req.serverID.String(),
			recipient.userID.String(),
			req.channelID.String(),
			req.viewPerm,
		)
		if err != nil {
			log.Printf("Failed to check channel delivery permission: %v", err)
			result.decisions = append(result.decisions, channelDeliveryDecision{
				clientID:   recipient.clientID,
				userID:     recipient.userID,
				allowed:    false,
				definitive: false,
			})
			continue
		}
		result.decisions = append(result.decisions, channelDeliveryDecision{
			clientID:   recipient.clientID,
			userID:     recipient.userID,
			allowed:    hasPerm,
			definitive: true,
		})
	}
	return result
}

func (h *Hub) handleChannelDeliveryResult(result channelDeliveryResult) {
	switch result.kind {
	case channelDeliveryBroadcast:
		h.applyBroadcastDeliveryResult(result)
	case channelDeliveryUnread:
		h.applyUnreadDeliveryResult(result)
	case channelDeliveryMention:
		h.applyMentionDeliveryResult(result)
	case channelDeliveryPrune:
		h.applyPruneDeliveryResult(result)
	}
}

func (h *Hub) applyBroadcastDeliveryResult(result channelDeliveryResult) {
	subscribers := h.channelSubscriptions[result.channelID]
	for _, decision := range result.decisions {
		client, ok := h.clients[decision.clientID]
		if !ok || subscribers == nil || !subscribers[decision.clientID] {
			continue
		}
		if !decision.allowed {
			if decision.definitive {
				h.removeChannelSubscription(result.channelID, client)
			}
			continue
		}
		if !client.enqueueOutbound(result.data) {
			h.handleUnregister(client)
		}
	}
}

func (h *Hub) applyUnreadDeliveryResult(result channelDeliveryResult) {
	serverClients := h.serverSubscriptions[result.serverID]
	channelClients := h.channelSubscriptions[result.channelID]
	for _, decision := range result.decisions {
		client, ok := h.clients[decision.clientID]
		if !ok || serverClients == nil || !serverClients[decision.clientID] {
			continue
		}
		if channelClients != nil && channelClients[decision.clientID] {
			continue
		}
		if !decision.allowed {
			continue
		}
		client.enqueueOutbound(result.data)
	}
}

func (h *Hub) applyMentionDeliveryResult(result channelDeliveryResult) {
	channelClients := h.channelSubscriptions[result.channelID]
	for _, decision := range result.decisions {
		client, ok := h.clients[decision.clientID]
		if !ok {
			continue
		}
		userClients := h.userClients[decision.userID]
		if userClients == nil || !userClients[decision.clientID] {
			continue
		}
		if channelClients != nil && channelClients[decision.clientID] {
			continue
		}
		if !decision.allowed {
			continue
		}
		client.enqueueOutbound(result.data)
	}
}

func (h *Hub) applyPruneDeliveryResult(result channelDeliveryResult) {
	for _, decision := range result.decisions {
		if decision.allowed || !decision.definitive {
			continue
		}
		client, ok := h.clients[decision.clientID]
		if !ok {
			continue
		}
		h.removeChannelSubscription(result.channelID, client)
	}
}

func (h *Hub) removeChannelSubscription(channelID uuid.UUID, client *Client) {
	delete(client.Channels, channelID)
	if clients, ok := h.channelSubscriptions[channelID]; ok {
		delete(clients, client.ID)
		if len(clients) == 0 {
			delete(h.channelSubscriptions, channelID)
		}
	}
}

func (ctx *channelContext) viewPermission() (int64, bool) {
	switch ctx.channelType {
	case "text", "bulletin":
		return permViewTextChannels, true
	case "voice":
		return permViewVoiceChannels, true
	default:
		return 0, false
	}
}

// handleSubscribe subscribes a client to a channel after verifying channel visibility.
func (h *Hub) handleSubscribe(msg IncomingMessage) {
	channelID, ok := msg.Data[keyChannelID].(string)
	if !ok {
		h.sendError(msg.ClientID, "Invalid channel_id in subscribe message")
		return
	}

	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		h.sendError(msg.ClientID, "Invalid channel UUID")
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	chCtx := h.fetchChannelContext(msg, channelUUID)
	if chCtx == nil {
		return
	}
	viewPerm, ok := chCtx.viewPermission()
	if !ok {
		h.sendError(msg.ClientID, "Not authorized to subscribe to this channel")
		return
	}
	if !h.authorizeChannelPermissions(msg, client.UserID, chCtx, channelUUID, []int64{viewPerm}, "Not authorized to subscribe to this channel", false) {
		return
	}

	// Add to client's channels
	if client.Channels == nil {
		client.Channels = make(map[uuid.UUID]bool)
	}
	client.Channels[channelUUID] = true

	// Add to channel subscriptions
	if _, ok := h.channelSubscriptions[channelUUID]; !ok {
		h.channelSubscriptions[channelUUID] = make(map[uuid.UUID]bool)
	}
	h.channelSubscriptions[channelUUID][client.ID] = true

	log.Printf("Client %s subscribed to channel %s", sanitizeLogValue(client.ID.String()), sanitizeLogValue(channelUUID.String()))

	// Send confirmation
	confirmMsg := OutgoingMessage{
		Type: "subscribed",
		Data: map[string]interface{}{
			keyChannelID: channelUUID,
		},
	}
	data, err := json.Marshal(confirmMsg)
	if err != nil {
		log.Printf("Failed to marshal channel subscription confirmation: %v", err)
		return
	}
	client.enqueueOutboundBlocking(data)
}

// handleUnsubscribe unsubscribes a client from a channel
func (h *Hub) handleUnsubscribe(msg IncomingMessage) {
	channelID, ok := msg.Data[keyChannelID].(string)
	if !ok {
		return
	}

	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	h.removeChannelSubscription(channelUUID, client)

	log.Printf("Client %s unsubscribed from channel %s", sanitizeLogValue(client.ID.String()), sanitizeLogValue(channelUUID.String()))
}

// handleSubscribeServer subscribes a client to server-level notifications (unread pings)
func (h *Hub) handleSubscribeServer(msg IncomingMessage) {
	serverID, ok := msg.Data[keyServerID].(string)
	if !ok {
		h.sendError(msg.ClientID, "Invalid server_id in subscribe_server message")
		return
	}

	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		h.sendError(msg.ClientID, "Invalid server UUID")
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Verify the user is a member of this server
	var isMember bool
	err = h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverUUID, client.UserID,
	).Scan(&isMember)
	if err != nil {
		log.Printf("Failed to check server membership: %v", err)
		h.sendError(msg.ClientID, "Failed to verify server access")
		return
	}
	if !isMember {
		h.sendError(msg.ClientID, errMsgNotMemberOfServer)
		return
	}

	// Add to server subscriptions
	subs, ok := h.serverSubscriptions[serverUUID]
	if !ok {
		subs = make(map[uuid.UUID]bool)
		h.serverSubscriptions[serverUUID] = subs
	}
	alreadySubscribed := subs[client.ID]
	subs[client.ID] = true

	log.Printf("Client %s subscribed to server %s notifications", sanitizeLogValue(client.ID.String()), sanitizeLogValue(serverUUID.String()))

	// CV-CAN-030: deliver a fresh voice-count snapshot immediately on subscribe.
	// The scoped broadcast only reaches already-subscribed clients, so any voice
	// join/leave that landed between this client's initial snapshot and this
	// subscription would otherwise be missed, leaving a stale count until the
	// next voice event. This catch-up closes that window. The DB query runs off
	// the Run loop so a burst of subscriptions cannot stall the hub.
	//
	// Only dispatch when the client was newly added: a duplicate subscribe_server
	// for a server the client already tracks opens no new staleness window, and
	// firing the catch-up anyway would let an authenticated client spam duplicate
	// frames to force repeated full voice_participants aggregations (singleflight
	// only collapses concurrent in-flight queries, not sequential ones).
	if !alreadySubscribed {
		h.dispatchSubscribedVoiceCounts(client.ID)
	}
}

// voiceCountSFKey is the singleflight key for the on-subscribe catch-up query.
// The aggregation is global, so a single constant key collapses all concurrent
// dispatches onto one in-flight query.
const voiceCountSFKey = "voice-counts"

// dispatchSubscribedVoiceCounts queries current voice counts off the Run
// goroutine and hands the result back via voiceCountCatchupResults, so a burst
// of subscribe_server messages (e.g. a reconnect storm) cannot stall the single
// Run loop on per-subscribe DB round-trips. Mirrors dispatchChannelDelivery. The
// query touches no hub maps (h.db is safe for concurrent use); the scoping to the
// client's subscribed set and the actual send happen on the Run loop in
// applyVoiceCountCatchup, which owns serverSubscriptions and the send lifecycle.
//
// The catch-up aggregation is global and identical for every subscriber, so the
// query is run through singleflight: concurrent dispatches during a reconnect
// storm share one in-flight query instead of each issuing its own full-table
// scan, bounding DB load rather than amplifying it. The shared result is a
// per-server count map that callers only read, so sharing it across callers is
// safe (each caller filters to its own subscribed set on the Run loop).
func (h *Hub) dispatchSubscribedVoiceCounts(clientID uuid.UUID) {
	results := h.voiceCountCatchupResults
	if results == nil {
		return
	}
	done := h.done
	go func() {
		v, err, _ := h.voiceCountSF.Do(voiceCountSFKey, func() (interface{}, error) {
			return h.queryServerVoiceCounts()
		})
		if err != nil {
			log.Printf("Failed to query voice counts on subscribe: %v", err)
			return
		}
		counts, _ := v.(map[string]int)
		select {
		case results <- voiceCountCatchup{clientID: clientID, counts: counts}:
		case <-done:
		}
	}()
}

// applyVoiceCountCatchup delivers the freshly queried voice-count snapshot to the
// subscribing client, scoped to the servers it is currently subscribed to. The
// frontend replaces (does not merge) its voice-count map on each
// server_voice_counts message, so the payload carries the client's complete
// subscribed set, mirroring broadcastServerVoiceCounts. Runs on the Run goroutine,
// which owns serverSubscriptions and the client's Send channel (CV-CAN-030).
func (h *Hub) applyVoiceCountCatchup(c voiceCountCatchup) {
	client, ok := h.clients[c.clientID]
	if !ok {
		return // client disconnected before the query returned
	}

	// Report every subscribed server, defaulting to 0 when it has no voice
	// participants (matches the broadcast contract so a drop-to-zero is explicit).
	counts := make(map[string]int)
	for serverID, subs := range h.serverSubscriptions {
		if subs[c.clientID] {
			counts[serverID.String()] = c.counts[serverID.String()]
		}
	}
	if len(counts) == 0 {
		return
	}

	msg := OutgoingMessage{
		Type: "server_voice_counts",
		Data: map[string]interface{}{keyCounts: counts},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal server voice-count catch-up: %v", err)
		return
	}
	client.enqueueOutbound(data)
}

// handleUnsubscribeServer unsubscribes a client from server-level notifications
func (h *Hub) handleUnsubscribeServer(msg IncomingMessage) {
	serverID, ok := msg.Data[keyServerID].(string)
	if !ok {
		return
	}

	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	if clients, ok := h.serverSubscriptions[serverUUID]; ok {
		delete(clients, client.ID)
		if len(clients) == 0 {
			delete(h.serverSubscriptions, serverUUID)
		}
	}

	log.Printf("Client %s unsubscribed from server %s notifications", sanitizeLogValue(client.ID.String()), sanitizeLogValue(serverUUID.String()))
}

// sendError sends an error message to a specific client
func (h *Hub) sendError(clientID uuid.UUID, message string) {
	client, ok := h.clients[clientID]
	if !ok {
		return
	}
	errMsg := OutgoingMessage{
		Type: "error",
		Data: map[string]interface{}{
			keyMessage: message,
		},
	}
	data, err := json.Marshal(errMsg)
	if err != nil {
		log.Printf("Failed to marshal WebSocket error message: %v", err)
		return
	}
	client.enqueueOutbound(data)
}

// sendErrorWithData sends an error message with additional structured data to a specific client.
func (h *Hub) sendErrorWithData(clientID uuid.UUID, errorCode string, extra map[string]interface{}) {
	client, ok := h.clients[clientID]
	if !ok {
		return
	}
	data := map[string]interface{}{
		keyMessage: errorCode,
		"error":    errorCode,
		"code":     errorCode,
	}
	for k, v := range extra {
		data[k] = v
	}
	errMsg := OutgoingMessage{
		Type: "error",
		Data: data,
	}
	raw, err := json.Marshal(errMsg)
	if err != nil {
		log.Printf("Failed to marshal structured WebSocket error: %v", err)
		return
	}
	client.enqueueOutbound(raw)
}

// messageInput holds the parsed and validated fields from an incoming WebSocket message.
type messageInput struct {
	content         string
	keyVersion      int
	mentionAddendum *MentionAddendum
	attachmentIDs   []string
	gifSlug         *string
}

// errMissingKeyVersion is returned by validateEnvelope when an incoming
// WebSocket message lacks a positive integer key_version field. Under
// E2EE-everywhere (#201), every message envelope MUST carry a key version
// for the receiving client to identify the wrapping epoch.
var errMissingKeyVersion = errors.New("missing or invalid key_version")

// validateEnvelope returns errMissingKeyVersion if the incoming message's
// Data map lacks key_version or carries a non-positive / non-integer
// value. Called from both the channel-send and DM-send paths before any
// DB work. On failure, the caller MUST send close frame 4400
// "missing_or_invalid_key_version" and disconnect.
func validateEnvelope(msg *IncomingMessage) error {
	raw, ok := msg.Data[keyKeyVersion]
	if !ok {
		return errMissingKeyVersion
	}
	switch v := raw.(type) {
	case float64:
		if v < 1 {
			return errMissingKeyVersion
		}
	case int:
		if v < 1 {
			return errMissingKeyVersion
		}
	default:
		return errMissingKeyVersion
	}
	return nil
}

// rejectEnvelope writes the 4400 close frame to the client's WebSocket
// connection and logs the rejection. Used by parseMessageInput and
// parseDMMessageFields when validateEnvelope reports a missing or invalid
// key_version. Per [internal]rules/observability.md, the log emits only
// user_id (UUID, not PII) and the raw key_version value rendered safely
// via %q with a 64-char cap — never key material. The bounded rendering
// and %q quoting defend against log-size DoS and log-injection (newlines
// or control characters in client-supplied bytes).
func (h *Hub) rejectEnvelope(msg IncomingMessage) {
	rawKV := fmt.Sprintf("%v", msg.Data[keyKeyVersion])
	if len(rawKV) > 64 {
		rawKV = rawKV[:64] + "..."
	}
	log.Printf("ws envelope rejected user_id=%s reason=missing_or_invalid_key_version key_version=%q",
		sanitizeLogValue(msg.UserID.String()), rawKV)
	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}
	if client.Conn == nil {
		// Defensive: unit tests construct Clients with a buffered Send channel
		// instead of a real WebSocket connection, so Conn may be nil. In
		// production, Conn is always non-nil (set during the upgrade handshake).
		return
	}
	closeMsg := websocket.FormatCloseMessage(4400, "missing_or_invalid_key_version")
	if err := client.Conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(writeWait)); err != nil {
		log.Printf("Failed to write invalid-envelope close frame for user %s: %v", sanitizeLogValue(msg.UserID.String()), err)
	}
}

// parseMessageInput extracts and validates content, key version, and mention
// addendum from an incoming WebSocket message. Returns nil on validation failure.
func (h *Hub) parseMessageInput(msg IncomingMessage) *messageInput {
	content, ok := msg.Data[keyContent].(string)
	if !ok || content == "" {
		h.sendError(msg.ClientID, "Message content is required")
		return nil
	}

	if err := validateEnvelope(&msg); err != nil {
		h.rejectEnvelope(msg)
		return nil
	}

	keyVersion := 1
	if kv, ok := msg.Data[keyKeyVersion].(float64); ok && kv > 0 {
		keyVersion = int(kv)
	}

	// All channels are encrypted under E2EE-everywhere (#201); allow ciphertext-length payloads.
	if len(content) > 65536 {
		h.sendError(msg.ClientID, "Message content exceeds maximum length")
		return nil
	}

	var mentionAddendum *MentionAddendum
	if metaStr, ok := msg.Data["mention_meta"].(string); ok {
		mentionAddendum = decodeMentionMeta(metaStr)
	}

	attachmentIDs, attachOK := h.parseAttachmentIDs(msg)
	if !attachOK {
		return nil
	}

	gifSlug, gifOK := h.parseGifSlug(msg)
	if !gifOK {
		return nil
	}

	return &messageInput{
		content:         content,
		keyVersion:      keyVersion,
		mentionAddendum: mentionAddendum,
		attachmentIDs:   attachmentIDs,
		gifSlug:         gifSlug,
	}
}

// parseGifSlug extracts and validates gif_slug from an incoming message.
// Delegates to klipy.NormalizeSlug + klipy.ValidateSlug so the rules live in
// exactly one place and empty/whitespace values cleanly round-trip to "no GIF".
// Returns the parsed slug (or nil) and whether to continue.
func (h *Hub) parseGifSlug(msg IncomingMessage) (*string, bool) {
	rawSlug, ok := msg.Data["gif_slug"].(string)
	if !ok {
		return nil, true
	}
	normalized := klipy.NormalizeSlug(&rawSlug)
	if !klipy.ValidateSlug(normalized) {
		h.sendError(msg.ClientID, klipy.SlugValidationError(normalized))
		return nil, false
	}
	return normalized, true
}

// parseAttachmentIDs extracts and validates attachment_ids from an incoming message.
// Returns the parsed IDs and true on success; sends an error and returns false on failure.
func (h *Hub) parseAttachmentIDs(msg IncomingMessage) ([]string, bool) {
	raw, exists := msg.Data["attachment_ids"]
	if !exists {
		return nil, true // key absent — no attachments, valid
	}
	rawIDs, ok := raw.([]interface{})
	if !ok {
		h.sendError(msg.ClientID, "attachment_ids must be an array")
		return nil, false
	}
	if len(rawIDs) > 5 {
		h.sendError(msg.ClientID, "Maximum 5 attachments per message")
		return nil, false
	}
	seen := make(map[string]bool, len(rawIDs))
	ids := make([]string, 0, len(rawIDs))
	for _, item := range rawIDs {
		idStr, ok := item.(string)
		if !ok {
			h.sendError(msg.ClientID, "Invalid attachment ID format")
			return nil, false
		}
		if _, err := uuid.Parse(idStr); err != nil {
			h.sendError(msg.ClientID, "Invalid attachment ID")
			return nil, false
		}
		if seen[idStr] {
			continue // deduplicate
		}
		seen[idStr] = true
		ids = append(ids, idStr)
	}
	return ids, true
}

// channelContext holds server-side channel state needed for message persistence.
type channelContext struct {
	serverAllowEmbeds bool
	serverUUID        uuid.UUID
	channelType       string
}

// fetchChannelContext queries server-side channel state.
// Returns nil on failure (error already sent to client).
// All channels are encrypted under E2EE-everywhere (#201).
func (h *Hub) fetchChannelContext(msg IncomingMessage, channelUUID uuid.UUID) *channelContext {
	var ctx channelContext
	err := h.db.QueryRow(
		`SELECT s.allow_embedded_content, c.server_id, c.type
		 FROM channels c INNER JOIN servers s ON c.server_id = s.id
		 WHERE c.id = $1`, channelUUID,
	).Scan(&ctx.serverAllowEmbeds, &ctx.serverUUID, &ctx.channelType)
	if err != nil {
		log.Printf("Failed to check channel status: %v", err)
		h.sendError(msg.ClientID, "Failed to verify channel status")
		return nil
	}
	return &ctx
}

func (h *Hub) fetchChannelContextForAuth(channelUUID uuid.UUID) (*channelContext, error) {
	if h.db == nil {
		return nil, errors.New("database not configured")
	}
	var ctx channelContext
	err := h.db.QueryRow(
		`SELECT s.allow_embedded_content, c.server_id, c.type
		 FROM channels c INNER JOIN servers s ON c.server_id = s.id
		 WHERE c.id = $1`, channelUUID,
	).Scan(&ctx.serverAllowEmbeds, &ctx.serverUUID, &ctx.channelType)
	if err != nil {
		return nil, err
	}
	return &ctx, nil
}

func (h *Hub) deliveryAuthForChannel(channelID uuid.UUID) (uuid.UUID, int64, bool) {
	if h.db == nil {
		return uuid.Nil, 0, true
	}
	chCtx, err := h.fetchChannelContextForAuth(channelID)
	if err == sql.ErrNoRows {
		h.removeAllChannelSubscriptions(channelID)
		return uuid.Nil, 0, false
	}
	if err != nil {
		log.Printf("Failed to fetch channel delivery auth: %v", err)
		return uuid.Nil, 0, false
	}
	viewPerm, ok := chCtx.viewPermission()
	if !ok {
		h.removeAllChannelSubscriptions(channelID)
		return uuid.Nil, 0, false
	}
	return chCtx.serverUUID, viewPerm, true
}

func (h *Hub) fetchServerChannelDeliveryAuth(serverID uuid.UUID) (map[uuid.UUID]int64, []uuid.UUID, error) {
	if h.db == nil {
		return nil, nil, nil
	}
	rows, err := h.db.Query(
		`SELECT id, type
		 FROM channels
		 WHERE server_id = $1`, serverID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close channel delivery-auth rows: %v", closeErr)
		}
	}()

	authByChannel := make(map[uuid.UUID]int64)
	var invalidChannels []uuid.UUID
	for rows.Next() {
		var channelID uuid.UUID
		var channelType string
		if err := rows.Scan(&channelID, &channelType); err != nil {
			return nil, nil, err
		}
		viewPerm, ok := (&channelContext{channelType: channelType}).viewPermission()
		if !ok {
			invalidChannels = append(invalidChannels, channelID)
			continue
		}
		authByChannel[channelID] = viewPerm
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return authByChannel, invalidChannels, nil
}

// enforceWSEpoch checks that the key epoch is not revoked. Returns false if revoked or on error.
func (h *Hub) enforceWSEpoch(msg IncomingMessage, channelUUID uuid.UUID, channelID string, keyVersion int) bool {
	if keyVersion <= 0 {
		return true
	}
	var epochRevoked bool
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = $2)`,
		channelUUID, keyVersion,
	).Scan(&epochRevoked); err != nil {
		log.Printf("Failed to check epoch revocation: %v", err)
		h.sendError(msg.ClientID, errMsgFailedVerifyKeyEpoch)
		return false
	}
	if epochRevoked {
		currentEpoch := 1
		if err := h.db.QueryRow(
			`SELECT GREATEST(
				COALESCE(MAX(key_version), 1),
				COALESCE((SELECT MAX(successor_epoch) FROM key_revocations WHERE channel_id = $1), 1)
			) FROM channel_keys WHERE channel_id = $1`,
			channelUUID,
		).Scan(&currentEpoch); err != nil {
			log.Printf("Failed to fetch current epoch for channel %s: %v", sanitizeLogValue(channelID), err)
			h.sendError(msg.ClientID, errMsgFailedVerifyKeyEpoch)
			return false
		}
		h.sendErrorWithData(msg.ClientID, "epoch_revoked", map[string]interface{}{
			"current_epoch": currentEpoch,
			keyChannelID:    channelID,
		})
		return false
	}
	return true
}

// messageAck holds the fields for a message_ack response.
type messageAck struct {
	Client      *Client
	Nonce       string
	MessageID   uuid.UUID
	ChannelUUID uuid.UUID
	CreatedAt   interface{}
	UpdatedAt   interface{}
	ReplyToID   *string
	GifSlug     *string
	Attachments []models.AttachmentSummary
}

// sendMessageAck sends a message_ack to the sender with the server-assigned UUID.
func (h *Hub) sendMessageAck(ack messageAck) {
	ackData := map[string]interface{}{
		keyNonce:     ack.Nonce,
		"id":         ack.MessageID.String(),
		keyChannelID: ack.ChannelUUID.String(),
		keyCreatedAt: ack.CreatedAt,
		keyUpdatedAt: ack.UpdatedAt,
	}
	if ack.ReplyToID != nil {
		ackData["reply_to_id"] = *ack.ReplyToID
	}
	if ack.GifSlug != nil {
		ackData["gif_slug"] = *ack.GifSlug
	}
	if len(ack.Attachments) > 0 {
		ackData["attachments"] = ack.Attachments
	}
	ackMsg := OutgoingMessage{Type: "message_ack", Data: ackData}
	data, err := json.Marshal(ackMsg)
	if err != nil {
		log.Printf("Failed to marshal message acknowledgement: %v", err)
		return
	}
	ack.Client.enqueueOutbound(data)
}

// linkChannelAttachments validates and links attachment file_ids to a channel message.
func (h *Hub) linkChannelAttachments(messageID uuid.UUID, userID string, attachmentIDs []string, channelID string) []models.AttachmentSummary {
	return h.linkAttachmentsToTable(attachmentLinkCtx{
		messageID: messageID, userID: userID, channelID: channelID,
		insertSQL: `INSERT INTO message_attachments (message_id, file_id, position) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
	}, attachmentIDs)
}

// linkDMAttachments validates and links attachment file_ids to a DM message.
func (h *Hub) linkDMAttachments(messageID uuid.UUID, userID string, attachmentIDs []string, conversationID string) []models.AttachmentSummary {
	return h.linkAttachmentsToTable(attachmentLinkCtx{
		messageID: messageID, userID: userID, conversationID: conversationID,
		insertSQL: `INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
	}, attachmentIDs)
}

// attachmentLinkCtx holds the context for linking attachments to a message.
type attachmentLinkCtx struct {
	messageID      uuid.UUID
	userID         string
	channelID      string
	conversationID string
	insertSQL      string
}

// linkAttachmentsToTable validates and links attachment file_ids using the provided INSERT query.
func (h *Hub) linkAttachmentsToTable(ctx attachmentLinkCtx, attachmentIDs []string) []models.AttachmentSummary {
	if len(attachmentIDs) == 0 {
		return nil
	}

	var summaries []models.AttachmentSummary
	for i, fileID := range attachmentIDs {
		summary, ok := h.validateAndLinkAttachment(ctx, fileID, i)
		if ok {
			summaries = append(summaries, summary)
		}
	}
	return summaries
}

// validateAndLinkAttachment validates a single attachment and links it to a message.
func (h *Hub) validateAndLinkAttachment(ctx attachmentLinkCtx, fileID string, position int) (models.AttachmentSummary, bool) {
	var summary models.AttachmentSummary
	var uploaderID string
	var fileChannelID, fileConvID *string

	err := h.db.QueryRow(
		`SELECT uploader_id, file_type, mime_type, file_size, channel_id, conversation_id
		 FROM media_files WHERE id = $1 AND deleted_at IS NULL`,
		fileID,
	).Scan(&uploaderID, &summary.FileType, &summary.MimeType, &summary.FileSize, &fileChannelID, &fileConvID)
	if err != nil {
		log.Printf("Attachment %s not found or deleted: %v", sanitizeLogValue(fileID), err)
		return summary, false
	}

	if !h.verifyAttachmentAccess(fileID, uploaderID, ctx, fileChannelID, fileConvID) {
		return summary, false
	}

	_, err = h.db.Exec(ctx.insertSQL, ctx.messageID, fileID, position)
	if err != nil {
		log.Printf("Failed to link attachment %s to message %s: %v", sanitizeLogValue(fileID), sanitizeLogValue(ctx.messageID.String()), err)
		return summary, false
	}

	summary.ID = fileID
	return summary, true
}

// verifyAttachmentAccess checks that the file is owned by the sender and belongs to the correct channel/conversation.
func (h *Hub) verifyAttachmentAccess(fileID, uploaderID string, ctx attachmentLinkCtx, fileChannelID, fileConvID *string) bool {
	if uploaderID != ctx.userID {
		log.Printf("Attachment %s not owned by user %s", sanitizeLogValue(fileID), sanitizeLogValue(ctx.userID))
		return false
	}
	if ctx.channelID != "" && (fileChannelID == nil || *fileChannelID != ctx.channelID) {
		log.Printf("Attachment %s does not belong to channel %s", sanitizeLogValue(fileID), sanitizeLogValue(ctx.channelID))
		return false
	}
	if ctx.conversationID != "" && (fileConvID == nil || *fileConvID != ctx.conversationID) {
		log.Printf("Attachment %s does not belong to conversation %s", sanitizeLogValue(fileID), sanitizeLogValue(ctx.conversationID))
		return false
	}
	return true
}

// persistMessage inserts a message into the database, returning the generated ID and timestamps.
// persistMessageParams holds the fields needed to insert a channel message.
type persistMessageParams struct {
	channelUUID           uuid.UUID
	userID                uuid.UUID
	credEpoch             string
	membershipIncarnation time.Time
	content               string
	keyVersion            int
	embedsSuppressed      bool
	replyToID             *string
	gifSlug               *string
}

func lockMessageMembership(ctx context.Context, tx *sql.Tx, p persistMessageParams) (*time.Time, error) {
	var timedOutUntil sql.NullTime
	var timedOut bool
	err := tx.QueryRowContext(ctx,
		`SELECT sm.timed_out_until,
		        sm.timed_out_until IS NOT NULL AND sm.timed_out_until > clock_timestamp()
		 FROM channels ch
		 INNER JOIN server_members sm ON sm.server_id = ch.server_id
		 WHERE ch.id = $1 AND sm.user_id = $2 AND sm.joined_at = $3
		 FOR SHARE OF sm`,
		p.channelUUID, p.userID, p.membershipIncarnation,
	).Scan(&timedOutUntil, &timedOut)
	if err != nil || !timedOut {
		return nil, err
	}
	return &timedOutUntil.Time, nil
}

func messageCredentialGuardError(ctx context.Context, tx *sql.Tx, p persistMessageParams) string {
	guardErr := credepoch.GuardTx(ctx, tx, p.userID.String(), p.credEpoch)
	if guardErr == nil {
		return ""
	}
	if errors.Is(guardErr, credepoch.ErrEpochMismatch) || errors.Is(guardErr, credepoch.ErrBlocked) {
		return "Authentication required"
	}
	log.Printf("Message epoch guard read failed: %v", guardErr)
	return errMsgFailedSaveMessage
}

// persistMessage returns a specific error message on failure and, for a timeout
// rechecked under the membership lock, its locked deadline.
func (h *Hub) persistMessage(p persistMessageParams) (uuid.UUID, time.Time, time.Time, string, *time.Time) {
	messageID := uuid.New()
	var createdAt, updatedAt time.Time
	ctx, cancel := context.WithTimeout(context.Background(), channelAuthCtxTimeout)
	defer cancel()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Failed to begin message tx: %v", err)
		return messageID, createdAt, updatedAt, errMsgFailedSaveMessage, nil
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			log.Printf("Failed to rollback message transaction: %v", rbErr)
		}
	}()
	// #2201 (Codex #2397 review): a WS message frame read before a destructive
	// reset's DisconnectUser lands must not commit ciphertext under the
	// superseded epoch. GuardTx (users-row FOR SHARE) rechecks the sender's
	// connect-time epoch against the current DB epoch inside the write tx.
	if guardError := messageCredentialGuardError(ctx, tx, p); guardError != "" {
		return messageID, createdAt, updatedAt, guardError, nil
	}
	// Lock the preflight membership row inside the write transaction. Its immutable
	// joined_at value rejects a same-key rejoin after a kick/ban finishes purging,
	// while the fresh timeout read prevents a moderation update during RBAC checks
	// from admitting the frame.
	timedOutUntil, err := lockMessageMembership(ctx, tx, p)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return messageID, createdAt, updatedAt, errMsgNotMemberOfServer, nil
		}
		log.Printf("Failed to lock message membership: %v", err)
		return messageID, createdAt, updatedAt, errMsgFailedSaveMessage, nil
	}
	if timedOutUntil != nil {
		return messageID, createdAt, updatedAt, errMsgMemberTimedOut, timedOutUntil
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, reply_to_id, gif_slug, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
		 RETURNING created_at, updated_at`,
		messageID, p.channelUUID, p.userID, p.content, p.keyVersion, p.embedsSuppressed, p.replyToID, p.gifSlug,
	).Scan(&createdAt, &updatedAt)
	if err != nil && isFKViolation(err) {
		return messageID, createdAt, updatedAt, "Reply target message not found", nil
	}
	if err != nil {
		log.Printf("Failed to persist message: %v", err)
		return messageID, createdAt, updatedAt, errMsgFailedSaveMessage, nil
	}
	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit message: %v", err)
		return messageID, createdAt, updatedAt, errMsgFailedSaveMessage, nil
	}
	return messageID, createdAt, updatedAt, "", nil
}

// isFKViolation returns true if the error is a PostgreSQL foreign key violation (23503).
func isFKViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23503"
}

// validateReplyToID checks that reply_to_id is a valid UUID referencing a message
// in the given channel. Returns the validated ID (or nil) and whether to continue.
func (h *Hub) validateReplyToID(msg IncomingMessage, channelID string) (*string, bool) {
	rtID, ok := msg.Data["reply_to_id"].(string)
	if !ok || rtID == "" {
		return nil, true
	}
	if _, parseErr := uuid.Parse(rtID); parseErr != nil {
		h.sendError(msg.ClientID, "Invalid reply_to_id")
		return nil, false
	}
	var replyChannelUUID uuid.UUID
	err := h.db.QueryRow(`SELECT channel_id FROM messages WHERE id = $1`, rtID).Scan(&replyChannelUUID)
	if err == sql.ErrNoRows {
		h.sendError(msg.ClientID, "Reply target message not found")
		return nil, false
	}
	if err != nil {
		log.Printf("Failed to validate reply_to_id %s: %v", sanitizeLogValue(rtID), err)
		h.sendError(msg.ClientID, "Failed to validate reply target")
		return nil, false
	}
	parsedChannelID, parseErr := uuid.Parse(channelID)
	if parseErr != nil {
		log.Printf("Failed to parse validated reply channel ID: %v", parseErr)
		h.sendError(msg.ClientID, "Invalid channel ID")
		return nil, false
	}
	if replyChannelUUID != parsedChannelID {
		h.sendError(msg.ClientID, "Reply target must be in the same channel")
		return nil, false
	}
	return &rtID, true
}

// fetchRepliedToSummary queries the replied-to message for broadcast enrichment.
func (h *Hub) fetchRepliedToSummary(replyToID string) map[string]interface{} {
	var id, userID, username, content string
	var displayName *string
	var keyVer int
	err := h.db.QueryRow(`
		SELECT m.id, m.user_id, u.username, u.display_name, m.content, COALESCE(m.key_version, 1)
		FROM messages m INNER JOIN users u ON m.user_id = u.id WHERE m.id = $1
	`, replyToID).Scan(&id, &userID, &username, &displayName, &content, &keyVer)
	if err != nil {
		return nil
	}
	return map[string]interface{}{
		"id":           id,
		"user_id":      userID,
		"username":     username,
		"display_name": displayName,
		"content":      content,
		"key_version":  keyVer,
	}
}

// messageBroadcastCtx holds the data needed to build a channel message broadcast payload.
type messageBroadcastCtx struct {
	messageID        uuid.UUID
	channelUUID      uuid.UUID
	userID           uuid.UUID
	client           *Client
	input            *messageInput
	embedsSuppressed bool
	createdAt        interface{}
	updatedAt        interface{}
	replyToID        *string
	gifSlug          *string
	attachments      []models.AttachmentSummary
}

// buildMessageBroadcast constructs the broadcast data map for a channel message.
func (h *Hub) buildMessageBroadcast(ctx messageBroadcastCtx) map[string]interface{} {
	data := map[string]interface{}{
		"id":                ctx.messageID,
		keyChannelID:        ctx.channelUUID,
		keyUserID:           ctx.userID,
		keyUsername:         ctx.client.Username,
		"display_name":      ctx.client.DisplayName,
		"avatar_url":        ctx.client.AvatarURL,
		keyContent:          ctx.input.content,
		keyKeyVersion:       ctx.input.keyVersion,
		"embeds_suppressed": ctx.embedsSuppressed,
		keyCreatedAt:        ctx.createdAt,
		keyUpdatedAt:        ctx.updatedAt,
	}
	if ctx.replyToID != nil {
		data["reply_to_id"] = *ctx.replyToID
		if summary := h.fetchRepliedToSummary(*ctx.replyToID); summary != nil {
			data["replied_to"] = summary
		}
	}
	if ctx.gifSlug != nil {
		data["gif_slug"] = *ctx.gifSlug
	}
	if len(ctx.attachments) > 0 {
		data["attachments"] = ctx.attachments
	}
	return data
}

// handleMessage handles a chat message: validates, persists to DB, then broadcasts
func (h *Hub) handleMessage(msg IncomingMessage) {
	channelID, ok := msg.Data[keyChannelID].(string)
	if !ok {
		return
	}
	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}

	// Verify subscription + rate limit
	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}
	if !client.Channels[channelUUID] {
		h.sendError(msg.ClientID, "Not subscribed to this channel")
		return
	}
	if !client.rateLimitAllow() {
		h.sendError(msg.ClientID, "Rate limit exceeded, please slow down")
		return
	}

	// Parse and validate input fields before DB-backed channel checks. The
	// envelope validator intentionally rejects malformed encrypted payloads
	// with a WebSocket close frame before any database work.
	input := h.parseMessageInput(msg)
	if input == nil {
		return
	}
	if input.mentionAddendum != nil {
		defer input.mentionAddendum.Wipe()
	}

	chCtx := h.fetchChannelContext(msg, channelUUID)
	if chCtx == nil {
		return
	}
	viewPerm, ok := chCtx.viewPermission()
	if !ok {
		return
	}
	authorized, membershipIncarnation := h.authorizeMessageSend(msg, client.UserID, chCtx, channelUUID, viewPerm)
	if !authorized {
		return
	}

	// Embed policy + epoch enforcement (all channels are encrypted under #201)
	if !h.enforceWSEpoch(msg, channelUUID, channelID, input.keyVersion) {
		return
	}
	embedsSuppressed := !chCtx.serverAllowEmbeds

	// Validate optional reply_to_id (same-channel constraint)
	replyToID, replyOK := h.validateReplyToID(msg, channelID)
	if !replyOK {
		return
	}

	// Persist message
	messageID, createdAt, updatedAt, persistErr, timedOutUntil := h.persistMessage(persistMessageParams{
		channelUUID: channelUUID, userID: msg.UserID, credEpoch: msg.CredEpoch, membershipIncarnation: membershipIncarnation, content: input.content,
		keyVersion:       input.keyVersion,
		embedsSuppressed: embedsSuppressed, replyToID: replyToID, gifSlug: input.gifSlug,
	})
	if h.respondPersistMessageError(msg, persistErr, timedOutUntil) {
		return
	}
	if h.opsCounter != nil {
		h.opsCounter.Increment(opsmetrics.MetricChannelMessagesTotal)
	}

	// Link attachments to message (if any)
	attachmentSummaries := h.linkChannelAttachments(messageID, msg.UserID.String(), input.attachmentIDs, channelID)

	// Ack to sender
	nonce, _ := msg.Data[keyNonce].(string)
	h.sendMessageAck(messageAck{
		Client:      client,
		Nonce:       nonce,
		MessageID:   messageID,
		ChannelUUID: channelUUID,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		ReplyToID:   replyToID,
		GifSlug:     input.gifSlug,
		Attachments: attachmentSummaries,
	})

	// Broadcast to channel subscribers (excluding sender)
	broadcastData := h.buildMessageBroadcast(messageBroadcastCtx{
		messageID: messageID, channelUUID: channelUUID, userID: msg.UserID,
		client: client, input: input, embedsSuppressed: embedsSuppressed,
		createdAt: createdAt, updatedAt: updatedAt,
		replyToID: replyToID, gifSlug: input.gifSlug, attachments: attachmentSummaries,
	})
	h.broadcast <- BroadcastMessage{
		ChannelID:      channelUUID,
		ServerID:       chCtx.serverUUID,
		ViewPermission: viewPerm,
		ExcludeUser:    &msg.UserID,
		Data:           OutgoingMessage{Type: "message", Data: broadcastData},
	}

	// Send lightweight unread_notify to server subscribers NOT subscribed to this channel.
	h.sendUnreadNotify(chCtx.serverUUID, channelUUID, msg.UserID, viewPerm)

	// Mention routing: enforce RBAC, resolve targets, send enhanced notifications, wipe.
	if input.mentionAddendum != nil {
		h.enforceMentionPermissions(
			chCtx.serverUUID.String(), msg.UserID.String(), channelUUID.String(),
			input.mentionAddendum,
		)
		h.routeMentionNotifications(chCtx.serverUUID, channelUUID, msg.UserID, input.mentionAddendum, viewPerm)
	}
}

// sendUnreadNotify sends a lightweight notification to server subscribers
// who are not subscribed to the given channel (they already get the full message).
func (h *Hub) sendUnreadNotify(serverID, channelID, senderUserID uuid.UUID, viewPerm int64) {
	serverClients, ok := h.serverSubscriptions[serverID]
	if !ok {
		return
	}

	channelClients := h.channelSubscriptions[channelID] // may be nil, that's ok

	notifyMsg := OutgoingMessage{
		Type: "unread_notify",
		Data: map[string]interface{}{
			keyChannelID: channelID.String(),
			keyServerID:  serverID.String(),
		},
	}
	data, err := json.Marshal(notifyMsg)
	if err != nil {
		return
	}

	recipients := make([]channelDeliveryRecipient, 0, len(serverClients))
	for clientID := range serverClients {
		// Skip clients already subscribed to the channel — they got the full message
		if channelClients != nil && channelClients[clientID] {
			continue
		}

		client, ok := h.clients[clientID]
		if !ok {
			continue
		}

		// Skip the sender
		if client.UserID == senderUserID {
			continue
		}
		recipients = append(recipients, channelDeliveryRecipient{clientID: clientID, userID: client.UserID})
	}
	h.dispatchChannelDelivery(channelDeliveryRequest{
		kind:       channelDeliveryUnread,
		serverID:   serverID,
		channelID:  channelID,
		viewPerm:   viewPerm,
		data:       data,
		recipients: recipients,
	})
}

func (h *Hub) handleChannelRevalidation(req channelRevalidation) {
	chCtx, err := h.fetchChannelContextForAuth(req.channelID)
	if err == sql.ErrNoRows {
		h.removeAllChannelSubscriptions(req.channelID)
		return
	}
	if err != nil {
		log.Printf("Failed to revalidate channel subscriptions: %v", err)
		return
	}
	if req.serverID != uuid.Nil && chCtx.serverUUID != req.serverID {
		return
	}
	viewPerm, ok := chCtx.viewPermission()
	if !ok {
		h.removeAllChannelSubscriptions(req.channelID)
		return
	}
	h.pruneUnauthorizedChannelSubscribers(chCtx.serverUUID, req.channelID, viewPerm)
}

func (h *Hub) handleServerRevalidation(serverID uuid.UUID) {
	authByChannel, invalidChannels, err := h.fetchServerChannelDeliveryAuth(serverID)
	if err != nil {
		log.Printf("Failed to revalidate server subscriptions: %v", err)
		return
	}
	for _, channelID := range invalidChannels {
		h.removeAllChannelSubscriptions(channelID)
	}
	for channelID, viewPerm := range authByChannel {
		h.pruneUnauthorizedChannelSubscribers(serverID, channelID, viewPerm)
	}
}

func (h *Hub) pruneUnauthorizedChannelSubscribers(serverID, channelID uuid.UUID, viewPerm int64) {
	subscribers := h.channelSubscriptions[channelID]
	if len(subscribers) == 0 {
		return
	}
	recipients := make([]channelDeliveryRecipient, 0, len(subscribers))
	for clientID := range subscribers {
		client, ok := h.clients[clientID]
		if !ok {
			delete(subscribers, clientID)
			continue
		}
		recipients = append(recipients, channelDeliveryRecipient{clientID: clientID, userID: client.UserID})
	}
	h.dispatchChannelDelivery(channelDeliveryRequest{
		kind:       channelDeliveryPrune,
		serverID:   serverID,
		channelID:  channelID,
		viewPerm:   viewPerm,
		recipients: recipients,
	})
}

func (h *Hub) removeAllChannelSubscriptions(channelID uuid.UUID) {
	for clientID := range h.channelSubscriptions[channelID] {
		if client, ok := h.clients[clientID]; ok {
			delete(client.Channels, channelID)
		}
	}
	delete(h.channelSubscriptions, channelID)
}

// handleTyping handles typing indicator
func (h *Hub) handleTyping(msg IncomingMessage) {
	channelID, ok := msg.Data[keyChannelID].(string)
	if !ok {
		return
	}

	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}

	// Verify the client is subscribed to this channel
	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}
	if !client.Channels[channelUUID] {
		return
	}

	serverID, viewPerm, ok := h.deliveryAuthForChannel(channelUUID)
	if !ok {
		return
	}

	// Broadcast typing indicator to all subscribers except sender
	broadcastMsg := BroadcastMessage{
		ChannelID:      channelUUID,
		ServerID:       serverID,
		ViewPermission: viewPerm,
		ExcludeUser:    &msg.UserID,
		Data: OutgoingMessage{
			Type: "typing",
			Data: map[string]interface{}{
				keyChannelID: channelUUID,
				keyUserID:    msg.UserID,
				keyUsername:  client.Username,
				keyIsTyping:  msg.Data[keyIsTyping],
			},
		},
	}

	h.broadcast <- broadcastMsg
}

// handleHeartbeat refreshes the Redis presence TTL for the user
func (h *Hub) handleHeartbeat(msg IncomingMessage) {
	ctx := context.Background()
	key := presence.StatusRedisKey(msg.UserID)
	// Refresh TTL for any user-set status without letting missing or stale Redis
	// state expose a user whose Invisible write or prior refresh failed.
	val, err := h.redis.Get(ctx, key).Result()
	if err == nil {
		h.handlePersistedPresenceHeartbeat(ctx, key, msg.UserID, val)
		return
	}
	if errors.Is(err, redis.Nil) {
		h.handleMissingPresenceHeartbeat(ctx, key, msg.UserID)
		return
	}
	log.Printf("[hub] failed to read presence heartbeat state for user %s: %v", sanitizeLogValue(msg.UserID.String()), err)
	if _, hidden := h.hiddenPresence[msg.UserID]; !hidden {
		if recovery, known := h.presenceRecovery[msg.UserID]; known && isVisibleStatus(recovery.status) {
			recovery.pending = true
			h.setPresenceRecovery(msg.UserID, recovery)
		}
	}
	h.failClosedPresenceHeartbeat(msg.UserID)
}

func (h *Hub) handlePersistedPresenceHeartbeat(ctx context.Context, key string, userID uuid.UUID, status string) {
	switch status {
	case statusInvisible:
		h.clearPresenceRecovery(userID)
		h.setHiddenPresence(userID, statusInvisible)
		h.refreshPresenceTTL(ctx, key, userID)
	case statusOnline, statusDND:
		h.handleVisiblePresenceHeartbeat(ctx, key, userID, status)
	default:
		h.clearPresenceRecovery(userID)
		h.failClosedPresenceHeartbeat(userID)
	}
}

func (h *Hub) handleVisiblePresenceHeartbeat(ctx context.Context, key string, userID uuid.UUID, status string) {
	selfStatus, hidden := h.hiddenPresence[userID]
	if !hidden {
		recovery := presenceRecoveryState{status: status}
		recovery.pending = !h.refreshPresenceTTL(ctx, key, userID)
		h.setPresenceRecovery(userID, recovery)
		return
	}
	if selfStatus == statusInvisible {
		h.clearPresenceRecovery(userID)
		if err := h.redis.Set(ctx, key, statusInvisible, 120*time.Second).Err(); err != nil {
			log.Printf("[hub] failed to repair invisible presence for user %s: %v", sanitizeLogValue(userID.String()), err)
		}
		return
	}

	// Offline is a transient fail-closed marker, not a user-selected status.
	// A valid persisted value proves Redis recovered, so make the user visible again.
	recovery := presenceRecoveryState{status: status}
	recovery.pending = !h.refreshPresenceTTL(ctx, key, userID)
	h.setPresenceRecovery(userID, recovery)
	if recovery.pending {
		return
	}
	if err := h.clearOfflinePresenceFence(ctx, userID); err != nil {
		log.Printf("[hub] failed to clear offline presence fence for user %s: %v", sanitizeLogValue(userID.String()), err)
		return
	}
	h.clearHiddenPresence(userID)
	h.broadcastPresenceToAll(userID, status, time.Now().Unix())
}

func (h *Hub) handleMissingPresenceHeartbeat(ctx context.Context, key string, userID uuid.UUID) {
	if _, connected := h.userClients[userID]; !connected {
		return
	}
	selfStatus, hidden := h.hiddenPresence[userID]
	recovery, known := h.presenceRecovery[userID]
	canRestore := known && recovery.pending && isVisibleStatus(recovery.status)
	if !hidden && !canRestore {
		h.failClosedPresenceHeartbeat(userID)
		return
	}
	if hidden && selfStatus == statusInvisible {
		h.clearPresenceRecovery(userID)
		if err := h.redis.Set(ctx, key, statusInvisible, 120*time.Second).Err(); err != nil {
			log.Printf("[hub] failed to restore invisible presence for user %s: %v", sanitizeLogValue(userID.String()), err)
		}
		return
	}
	if !canRestore {
		return
	}
	if err := h.redis.Set(ctx, key, recovery.status, 120*time.Second).Err(); err != nil {
		log.Printf("[hub] failed to restore visible presence for user %s: %v", sanitizeLogValue(userID.String()), err)
		if !hidden {
			h.failClosedPresenceHeartbeat(userID)
		}
		return
	}
	recovery.pending = false
	h.setPresenceRecovery(userID, recovery)
	if err := h.clearOfflinePresenceFence(ctx, userID); err != nil {
		log.Printf("[hub] failed to clear offline presence fence for user %s: %v", sanitizeLogValue(userID.String()), err)
		return
	}
	h.clearHiddenPresence(userID)
	h.broadcastPresenceToAll(userID, recovery.status, time.Now().Unix())
}

func (h *Hub) refreshPresenceTTL(ctx context.Context, key string, userID uuid.UUID) bool {
	refreshed, err := h.redis.Expire(ctx, key, 120*time.Second).Result()
	if err != nil {
		log.Printf("[hub] failed to refresh presence TTL for user %s: %v", sanitizeLogValue(userID.String()), err)
		return false
	}
	if !refreshed {
		log.Printf("[hub] presence key missing during TTL refresh for user %s", sanitizeLogValue(userID.String()))
		return false
	}
	return true
}

func (h *Hub) failClosedPresenceHeartbeat(userID uuid.UUID) {
	h.mu.Lock()
	_, hidden := h.hiddenPresence[userID]
	newEdge := !hidden
	if newEdge {
		if h.hiddenPresence == nil {
			h.hiddenPresence = make(map[uuid.UUID]string)
		}
		h.hiddenPresence[userID] = statusOffline
	}
	h.mu.Unlock()

	fencePersisted := false
	if err := h.persistOfflinePresenceFence(context.Background(), userID); err != nil {
		log.Printf("[hub] failed to persist offline presence fence for user %s: %v", sanitizeLogValue(userID.String()), err)
	} else if h.db != nil {
		fencePersisted = true
	}
	if !newEdge {
		return
	}
	h.broadcastPresenceToAll(userID, statusOffline, time.Now().Unix())
	// E4 (#2444): the early return above already makes this an edge.
	h.spawnRichPresenceSuppression(userID, !fencePersisted)
}

// handleSetStatus allows users to manually set their status (online/dnd/invisible)
func (h *Hub) handleSetStatus(msg IncomingMessage) {
	status, ok := msg.Data[keyStatus].(string)
	if !ok {
		return
	}

	// Validate status
	switch status {
	case statusOnline, statusDND, statusInvisible:
		// valid
	default:
		return
	}

	ctx := context.Background()
	key := presence.StatusRedisKey(msg.UserID)
	// E1 (#2444): capture the EDGE before the successful Redis write mutates the map. A
	// repeated set_status to invisible is a level and must not re-fire.
	wasHidden := true
	if status == statusInvisible {
		_, wasHidden = h.hiddenPresence[msg.UserID]
	}
	if err := h.redis.Set(ctx, key, status, 120*time.Second).Err(); err != nil {
		h.handleSetStatusWriteFailure(msg, status, err)
		return
	}

	// For invisible, broadcast as offline to other users (but store real status in Redis)
	broadcastStatus := status
	if status == statusInvisible {
		h.clearPresenceRecovery(msg.UserID)
		h.setHiddenPresence(msg.UserID, statusInvisible)
		broadcastStatus = statusOffline
		if !wasHidden {
			// Only here, after the Redis write succeeded, so the suppressor's own
			// read cannot observe the pre-transition status. The failure path
			// above returns early on purpose: with the persisted status still
			// visible, the level arm would immediately re-publish whatever this
			// cleared, so clearing would only flap.
			h.spawnRichPresenceSuppression(msg.UserID)
		}
	} else {
		if err := h.clearOfflinePresenceFence(ctx, msg.UserID); err != nil {
			log.Printf("[hub] failed to clear offline presence fence for user %s: %v", sanitizeLogValue(msg.UserID.String()), err)
			h.failClosedPresenceHeartbeat(msg.UserID)
			h.sendErrorWithData(msg.ClientID, "presence_status_unavailable", nil)
			return
		}
		h.setPresenceRecovery(msg.UserID, presenceRecoveryState{status: status})
		h.clearHiddenPresence(msg.UserID)
	}
	h.broadcastPresenceToAll(msg.UserID, broadcastStatus, time.Now().Unix())
}

func (h *Hub) handleSetStatusWriteFailure(msg IncomingMessage, status string, err error) {
	log.Printf("[hub] failed to persist presence status for user %s: %v", sanitizeLogValue(msg.UserID.String()), err)
	h.sendErrorWithData(msg.ClientID, "presence_status_unavailable", nil)
	if status == statusInvisible {
		return
	}
	if recovery, known := h.presenceRecovery[msg.UserID]; known && isVisibleStatus(recovery.status) {
		recovery.pending = true
		h.setPresenceRecovery(msg.UserID, recovery)
	}
}

// handleBroadcast broadcasts a message to all subscribers of a channel
func (h *Hub) handleBroadcast(msg BroadcastMessage) {
	subscribers, ok := h.channelSubscriptions[msg.ChannelID]
	if !ok {
		return
	}

	// Resolve the channel's server + view permission in the run loop for
	// authorized broadcasts (CV-CAN-021..026). deliveryAuthForChannel touches the
	// subscription maps, so this MUST run here and not in the HTTP-handler-called
	// BroadcastToChannelAuthorized. A gone channel / unresolvable view perm prunes
	// subscriptions and drops the broadcast.
	if msg.RequireViewAuth {
		serverID, viewPerm, authOK := h.deliveryAuthForChannel(msg.ChannelID)
		if !authOK {
			return
		}
		msg.ServerID = serverID
		msg.ViewPermission = viewPerm
	}

	messageData, err := json.Marshal(msg.Data)
	if err != nil {
		log.Printf("Failed to marshal broadcast message: %v", err)
		return
	}
	recipients := make([]channelDeliveryRecipient, 0, len(subscribers))
	for clientID := range subscribers {
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}

		// Skip if this is the excluded user
		if msg.ExcludeUser != nil && client.UserID == *msg.ExcludeUser {
			continue
		}
		recipients = append(recipients, channelDeliveryRecipient{clientID: clientID, userID: client.UserID})
	}
	h.dispatchChannelDelivery(channelDeliveryRequest{
		kind:       channelDeliveryBroadcast,
		serverID:   msg.ServerID,
		channelID:  msg.ChannelID,
		viewPerm:   msg.ViewPermission,
		data:       messageData,
		recipients: recipients,
	})
}

// BroadcastToChannel broadcasts a message to all subscribers of a channel.
//
// It leaves ServerID and ViewPermission zero, which dispatchChannelDelivery
// treats as allow-all (no per-recipient view recheck). Use it only for events
// that are safe for every current subscriber, or for DM channels (where server
// view permission does not apply). For REST-triggered server-channel mutation
// events (edit / delete / embed-suppress / reaction / pin / unpin), use
// BroadcastToChannelAuthorized so a stale subscriber that has lost channel view
// access is filtered out.
func (h *Hub) BroadcastToChannel(channelID uuid.UUID, message OutgoingMessage) {
	h.broadcast <- BroadcastMessage{
		ChannelID: channelID,
		Data:      message,
	}
}

// BroadcastToChannelAuthorized broadcasts a channel-scoped message with a
// per-recipient view-permission recheck: the hub resolves the channel's server
// and required view permission in the run loop and filters each recipient by it,
// so a subscriber that lost channel view access since subscribing does not
// receive the message (CV-CAN-021..026). Callers set only the RequireViewAuth
// flag; ServerID/ViewPermission are resolved in handleBroadcast. This mirrors
// the authorization the send-message and typing broadcasts already perform.
func (h *Hub) BroadcastToChannelAuthorized(channelID uuid.UUID, message OutgoingMessage) {
	h.broadcast <- BroadcastMessage{
		ChannelID:       channelID,
		Data:            message,
		RequireViewAuth: true,
	}
}

// GetConnectedUsers returns a map of user IDs currently connected (thread-safe).
func (h *Hub) GetConnectedUsers() map[uuid.UUID]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	users := make(map[uuid.UUID]bool, len(h.userClients))
	for userID := range h.userClients {
		users[userID] = true
	}
	return users
}

// GetUserClientCount returns the number of connected clients for a user (thread-safe).
func (h *Hub) GetUserClientCount(userID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if clients, ok := h.userClients[userID]; ok {
		return len(clients)
	}
	return 0
}

// IsUserOnline reports whether the user has at least one live WS client on
// this (single-replica) control-plane. Authoritative per spec §5.4.
func (h *Hub) IsUserOnline(userID uuid.UUID) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.userClients[userID]) > 0
}

// MarkUserOnlineForTest registers a synthetic client so unit/integration
// tests in OTHER packages (e.g. internal/dm) can make a user appear online
// to IsUserOnline without a real WS upgrade. Test-only; not used in prod.
func (h *Hub) MarkUserOnlineForTest(userID uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.userClients[userID] == nil {
		h.userClients[userID] = make(map[uuid.UUID]bool)
	}
	h.userClients[userID][uuid.New()] = true
}

// handleGlobalBroadcast sends a message to all connected clients
func (h *Hub) handleGlobalBroadcast(msg OutgoingMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal global broadcast: %v", err)
		return
	}
	for _, client := range h.clients {
		client.enqueueOutbound(data)
	}
}

// BroadcastToAll sends a message to all connected clients (thread-safe).
func (h *Hub) BroadcastToAll(msg OutgoingMessage) {
	h.globalBroadcast <- msg
}

// handleServerBroadcast sends a message to all clients subscribed to a server.
func (h *Hub) handleServerBroadcast(msg ServerBroadcastMessage) {
	// A prune attached to this broadcast (CV-CAN-027/028) must run AFTER delivery and
	// regardless of the delivery outcome, so the security eviction is never skipped by
	// an early return. Deferring keeps it in this same serialized hub operation, which
	// guarantees it runs before any later server broadcast (e.g. key_revocation).
	if msg.PruneUserAfter != nil {
		defer h.evictServerSubscriber(msg.ServerID, *msg.PruneUserAfter)
	}

	subscribers, ok := h.serverSubscriptions[msg.ServerID]
	if !ok {
		return
	}

	data, err := json.Marshal(msg.Data)
	if err != nil {
		log.Printf("Failed to marshal server broadcast: %v", err)
		return
	}

	for clientID := range subscribers {
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}
		client.enqueueOutbound(data)
	}
}

// BroadcastToServer sends a message to all clients subscribed to a server
// (thread-safe). Once the Hub has stopped, a saturated queue must not strand a
// caller forever.
func (h *Hub) BroadcastToServer(serverID uuid.UUID, msg OutgoingMessage) {
	h.BroadcastToServerContext(context.Background(), serverID, msg)
}

// BroadcastToServerContext is the deadline-aware form used by bounded lifecycle
// callbacks. It returns false when the caller is canceled, the Hub is stopped,
// or a full queue cannot be drained before either condition occurs.
func (h *Hub) BroadcastToServerContext(
	ctx context.Context,
	serverID uuid.UUID,
	msg OutgoingMessage,
) bool {
	if ctx == nil {
		return false
	}
	select {
	case <-h.done:
		return false
	default:
	}
	if ctx.Err() != nil {
		return false
	}
	serverMessage := ServerBroadcastMessage{
		ServerID: serverID,
		Data:     msg,
	}
	select {
	case h.serverBroadcast <- serverMessage:
		return true
	case <-ctx.Done():
		return false
	case <-h.done:
		return false
	}
}

// BroadcastToServerAndPrune sends msg to all of serverID's current subscribers and
// then, in the SAME serialized hub operation, removes pruneUserID's clients from the
// server subscription set. Delivery and eviction happen in one handleServerBroadcast
// call on the Run goroutine, so a removed/banned member still receives their own
// member_removed event and is then unsubscribed before any subsequent server fanout.
//
// The message is queued on the dedicated evictBroadcast channel, which the Run loop
// drains at higher priority than serverBroadcast. This guarantees the eviction runs
// before server broadcasts that originate on OTHER code paths (e.g. server_updated
// via handleServerUpdate, or unread_notify), not just those queued behind it on
// serverBroadcast. This closes the window where such a fanout could leak to a member
// who has already been removed. The enqueue is attempted non-blockingly first so that
// whenever the buffer has room the prune is always queued (never silently dropped), and
// the caller is an HTTP handler (not the Run goroutine), so it cannot self-deadlock.
// Only when the buffer (16) is full does it fall back to a blocking select that bails on
// h.done: once the hub has finished shutting down the Run loop no longer drains
// evictBroadcast, so without that guard a full buffer would hang the caller forever.
// The control-plane entrypoint drains accepted HTTP handlers before closing h.done
// (#2202); the bail remains defense in depth for direct Hub.Shutdown callers. Trying the
// non-blocking send first also avoids Go's random select choice dropping a queueable
// prune if an enqueue races such a direct shutdown. This is the ordered, guaranteed-
// delivery mechanism for evicting removed/banned members. CV-CAN-027/028.
func (h *Hub) BroadcastToServerAndPrune(serverID uuid.UUID, msg OutgoingMessage, pruneUserID uuid.UUID) {
	uid := pruneUserID
	sbm := ServerBroadcastMessage{
		ServerID:       serverID,
		Data:           msg,
		PruneUserAfter: &uid,
	}
	// Prefer a non-blocking enqueue. If a direct Hub.Shutdown races this call, a
	// single select over the send and <-h.done could choose the done arm even when
	// the buffer has room. The Run loop's priority drain still applies any queued
	// prune ahead of shutdown. The control-plane drains accepted HTTP handlers
	// before closing h.done (#2202); this guard covers other callers defensively.
	// Only fall back to the done bail when the buffer is genuinely full.
	select {
	case h.evictBroadcast <- sbm:
		return
	default:
	}
	select {
	case h.evictBroadcast <- sbm:
	case <-h.done:
		// Buffer is full and the hub is shutting down: the Run loop may stop
		// draining evictBroadcast at any time, so another drain is not guaranteed.
		// Every client connection is closed on shutdown anyway, so no fanout can
		// leak; bail rather than hang the caller.
	}
}

// handleUserBroadcast sends a message to all clients of a specific user.
func (h *Hub) handleUserBroadcast(msg UserBroadcastMessage) {
	clients, ok := h.userClients[msg.UserID]
	if !ok {
		return
	}

	data, err := json.Marshal(msg.Data)
	if err != nil {
		log.Printf("Failed to marshal user broadcast: %v", err)
		return
	}

	for clientID := range clients {
		if msg.ExcludeClientID != nil && clientID == *msg.ExcludeClientID {
			continue
		}
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}
		client.enqueueOutbound(data)
	}
}

// BroadcastToUser sends a message to all connected clients of a specific user (thread-safe).
func (h *Hub) BroadcastToUser(userID uuid.UUID, msg OutgoingMessage) {
	h.userBroadcast <- UserBroadcastMessage{
		UserID: userID,
		Data:   msg,
	}
}

// --- DM subscription and message handlers ---

// handleSubscribeDM subscribes a client to a DM conversation after verifying participation.
func (h *Hub) handleSubscribeDM(msg IncomingMessage) {
	convID, ok := msg.Data[keyConversationID].(string)
	if !ok {
		h.sendError(msg.ClientID, "Invalid conversation_id in subscribe_dm message")
		return
	}

	convUUID, err := uuid.Parse(convID)
	if err != nil {
		h.sendError(msg.ClientID, "Invalid conversation UUID")
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Verify the user is a participant of this DM conversation
	var isParticipant bool
	err = h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`,
		convUUID, client.UserID,
	).Scan(&isParticipant)
	if err != nil {
		log.Printf("Failed to check DM participation: %v", err)
		h.sendError(msg.ClientID, "Failed to verify DM access")
		return
	}
	if !isParticipant {
		h.sendError(msg.ClientID, "Not a participant of this conversation")
		return
	}

	// Add to DM subscriptions
	if _, ok := h.dmSubscriptions[convUUID]; !ok {
		h.dmSubscriptions[convUUID] = make(map[uuid.UUID]bool)
	}
	h.dmSubscriptions[convUUID][client.ID] = true

	log.Printf("Client %s subscribed to DM %s", sanitizeLogValue(client.ID.String()), sanitizeLogValue(convUUID.String()))

	// Send confirmation
	confirmMsg := OutgoingMessage{
		Type: "dm_subscribed",
		Data: map[string]interface{}{
			keyConversationID: convUUID,
		},
	}
	data, err := json.Marshal(confirmMsg)
	if err != nil {
		log.Printf("Failed to marshal DM subscription confirmation: %v", err)
		return
	}
	client.enqueueOutboundBlocking(data)
}

// handleUnsubscribeDM unsubscribes a client from a DM conversation.
func (h *Hub) handleUnsubscribeDM(msg IncomingMessage) {
	convID, ok := msg.Data[keyConversationID].(string)
	if !ok {
		return
	}

	convUUID, err := uuid.Parse(convID)
	if err != nil {
		return
	}

	if clients, ok := h.dmSubscriptions[convUUID]; ok {
		delete(clients, msg.ClientID)
		if len(clients) == 0 {
			delete(h.dmSubscriptions, convUUID)
		}
	}

	log.Printf("Client %s unsubscribed from DM %s", sanitizeLogValue(msg.ClientID.String()), sanitizeLogValue(convUUID.String()))
}

// dmMessageInput holds the parsed and validated fields from an incoming DM WebSocket message.
type dmMessageInput struct {
	content         string
	keyVersion      int
	msgType         string
	mentionAddendum *MentionAddendum
	attachmentIDs   []string
	gifSlug         *string
}

// dmUnreadLastMessage holds last-message metadata included in dm_unread_notify
// so that clients can update conversation previews and ordering in real-time.
type dmUnreadLastMessage struct {
	content   string
	userID    string
	username  string
	createdAt time.Time
}

func (h *Hub) validateDMMessage(msg IncomingMessage) (*Client, uuid.UUID, *dmMessageInput) {
	convID, ok := msg.Data[keyConversationID].(string)
	if !ok {
		return nil, uuid.Nil, nil
	}
	convUUID, err := uuid.Parse(convID)
	if err != nil {
		return nil, uuid.Nil, nil
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return nil, uuid.Nil, nil
	}
	subscribers, hasSubs := h.dmSubscriptions[convUUID]
	if !hasSubs || !subscribers[client.ID] {
		h.sendError(msg.ClientID, "Not subscribed to this DM conversation")
		return nil, uuid.Nil, nil
	}
	if !client.rateLimitAllow() {
		h.sendError(msg.ClientID, "Rate limit exceeded, please slow down")
		return nil, uuid.Nil, nil
	}

	input, valid := h.parseDMMessageFields(msg)
	if !valid {
		return nil, uuid.Nil, nil
	}

	return client, convUUID, input
}

// parseDMMessageFields extracts and validates content, type,
// attachments, and mention metadata from a DM message payload.
func (h *Hub) parseDMMessageFields(msg IncomingMessage) (*dmMessageInput, bool) {
	content, ok := msg.Data[keyContent].(string)
	if !ok || content == "" {
		h.sendError(msg.ClientID, "Message content is required")
		return nil, false
	}

	if err := validateEnvelope(&msg); err != nil {
		h.rejectEnvelope(msg)
		return nil, false
	}

	msgType := "user"
	if t, ok := msg.Data["type"].(string); ok && t == "system" {
		msgType = "system"
	}

	keyVersion := 1
	if kv, ok := msg.Data[keyKeyVersion].(float64); ok && kv > 0 {
		keyVersion = int(kv)
	}

	if !h.validateContentLength(msg.ClientID, content) {
		return nil, false
	}

	attachmentIDs, attachOK := h.parseAttachmentIDs(msg)
	if !attachOK {
		return nil, false
	}

	gifSlug, gifOK := h.parseGifSlug(msg)
	if !gifOK {
		return nil, false
	}

	mentionAddendum := h.parseDMMentionMeta(msg)

	return &dmMessageInput{
		content:         content,
		keyVersion:      keyVersion,
		msgType:         msgType,
		mentionAddendum: mentionAddendum,
		attachmentIDs:   attachmentIDs,
		gifSlug:         gifSlug,
	}, true
}

// validateContentLength checks that the message content does not exceed the
// allowed length. All messages are encrypted under E2EE-everywhere (#201),
// so the 65536-byte (64 KiB) ciphertext cap applies. Sized for the future
// 10,240-char paid-tier message under worst-case CJK UTF-8 + AES-GCM envelope,
// with ~60% envelope evolution headroom.
func (h *Hub) validateContentLength(clientID uuid.UUID, content string) bool {
	if len(content) > 65536 {
		h.sendError(clientID, "Message content exceeds maximum length")
		return false
	}
	return true
}

// parseDMMentionMeta extracts and sanitizes mention metadata for DM messages,
// clearing server-only fields (Everyone, Roles).
func (h *Hub) parseDMMentionMeta(msg IncomingMessage) *MentionAddendum {
	metaStr, ok := msg.Data["mention_meta"].(string)
	if !ok {
		return nil
	}
	mentionAddendum := decodeMentionMeta(metaStr)
	if mentionAddendum != nil {
		mentionAddendum.Everyone = false
		mentionAddendum.Roles = nil
	}
	return mentionAddendum
}

// enforceDMEncryption fetches conversation metadata and verifies epoch validity.
// All DMs are encrypted under E2EE-everywhere (#201).
func (h *Hub) enforceDMEncryption(msg IncomingMessage, convUUID uuid.UUID, keyVersion int) (convPersonal bool, ok bool) {
	encErr := h.db.QueryRow(`SELECT is_personal FROM dm_conversations WHERE id = $1`, convUUID).Scan(&convPersonal)
	if encErr != nil {
		log.Printf("Failed to check DM conversation status: %v", encErr)
		h.sendError(msg.ClientID, "Failed to verify conversation status")
		return false, false
	}

	if !h.enforceDMEpoch(msg, convUUID, keyVersion) {
		return false, false
	}
	return convPersonal, true
}

func (h *Hub) enforceDMEpoch(msg IncomingMessage, convUUID uuid.UUID, keyVersion int) bool {
	if keyVersion <= 0 {
		return true
	}
	var epochRevoked bool
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM dm_key_revocations WHERE conversation_id = $1 AND revoked_epoch = $2)`,
		convUUID, keyVersion,
	).Scan(&epochRevoked); err != nil {
		log.Printf("Failed to check DM epoch revocation: %v", err)
		h.sendError(msg.ClientID, errMsgFailedVerifyKeyEpoch)
		return false
	}
	if !epochRevoked {
		return true
	}
	currentEpoch := 1
	if err := h.db.QueryRow(
		`SELECT COALESCE(MAX(key_version), 1) FROM dm_channel_keys WHERE conversation_id = $1`,
		convUUID,
	).Scan(&currentEpoch); err != nil {
		log.Printf("Failed to fetch current epoch for DM %s: %v", sanitizeLogValue(convUUID.String()), err)
		h.sendError(msg.ClientID, errMsgFailedVerifyKeyEpoch)
		return false
	}
	convID := convUUID.String()
	h.sendErrorWithData(msg.ClientID, "epoch_revoked", map[string]interface{}{
		"current_epoch":   currentEpoch,
		keyConversationID: convID,
		keyChannelID:      convID,
	})
	return false
}

func (h *Hub) persistDMMessage(convUUID uuid.UUID, userID uuid.UUID, credEpoch string, input *dmMessageInput) (uuid.UUID, time.Time, time.Time, error) {
	messageID := uuid.New()
	var createdAt, updatedAt time.Time
	ctx := context.Background()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return messageID, createdAt, updatedAt, err
	}
	defer func() { _ = tx.Rollback() }()
	// #2201 (Codex #2397 review): fence the WS DM ciphertext write against a
	// destructive reset that advanced the sender's epoch after connect.
	if guardErr := credepoch.GuardTx(ctx, tx, userID.String(), credEpoch); guardErr != nil {
		return messageID, createdAt, updatedAt, guardErr
	}
	if err = tx.QueryRowContext(ctx,
		`INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version, type, gif_slug, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		 RETURNING created_at, updated_at`,
		messageID, convUUID, userID, input.content, input.keyVersion, input.msgType, input.gifSlug,
	).Scan(&createdAt, &updatedAt); err != nil {
		return messageID, createdAt, updatedAt, err
	}
	return messageID, createdAt, updatedAt, tx.Commit()
}

// dmMessageAckParams holds the fields for a dm_message_ack response.
type dmMessageAckParams struct {
	client      *Client
	nonce       string
	messageID   uuid.UUID
	convUUID    uuid.UUID
	createdAt   time.Time
	updatedAt   time.Time
	gifSlug     *string
	attachments []models.AttachmentSummary
}

func (h *Hub) sendDMMessageAck(p dmMessageAckParams) {
	dmAckData := map[string]interface{}{
		keyNonce:          p.nonce,
		"id":              p.messageID.String(),
		"conversation_id": p.convUUID.String(),
		keyCreatedAt:      p.createdAt,
		keyUpdatedAt:      p.updatedAt,
	}
	if p.gifSlug != nil {
		dmAckData["gif_slug"] = *p.gifSlug
	}
	if len(p.attachments) > 0 {
		dmAckData["attachments"] = p.attachments
	}
	ackMsg := OutgoingMessage{Type: "dm_message_ack", Data: dmAckData}
	if ackData, ackErr := json.Marshal(ackMsg); ackErr == nil {
		p.client.enqueueOutbound(ackData)
	}
}

// dmBroadcastCtx holds the data needed to build a DM message broadcast payload.
type dmBroadcastCtx struct {
	messageID    uuid.UUID
	convUUID     uuid.UUID
	senderUserID uuid.UUID
	client       *Client
	input        *dmMessageInput
	createdAt    time.Time
	updatedAt    time.Time
	attachments  []models.AttachmentSummary
	convPersonal bool
}

func (h *Hub) broadcastDMMessage(ctx dmBroadcastCtx) {
	dmBroadcastData := map[string]interface{}{
		"id":              ctx.messageID,
		keyConversationID: ctx.convUUID,
		keyUserID:         ctx.senderUserID,
		keyUsername:       ctx.client.Username,
		"display_name":    ctx.client.DisplayName,
		"avatar_url":      ctx.client.AvatarURL,
		keyContent:        ctx.input.content,
		keyKeyVersion:     ctx.input.keyVersion,
		"type":            ctx.input.msgType,
		keyCreatedAt:      ctx.createdAt,
		keyUpdatedAt:      ctx.updatedAt,
	}
	if ctx.input.gifSlug != nil {
		dmBroadcastData["gif_slug"] = *ctx.input.gifSlug
	}
	if len(ctx.attachments) > 0 {
		dmBroadcastData["attachments"] = ctx.attachments
	}

	var excludeUser *uuid.UUID
	if !ctx.convPersonal {
		excludeUser = &ctx.senderUserID
	}
	h.dmBroadcast <- DMBroadcastMessage{
		ConversationID: ctx.convUUID,
		ExcludeUser:    excludeUser,
		Data:           OutgoingMessage{Type: "dm_message", Data: dmBroadcastData},
	}
}

// handleDMMessage handles a DM chat message: validates, persists, then broadcasts.
func (h *Hub) handleDMMessage(msg IncomingMessage) {
	client, convUUID, input := h.validateDMMessage(msg)
	if input == nil {
		return
	}
	if input.mentionAddendum != nil {
		defer input.mentionAddendum.Wipe()
	}

	convPersonal, ok := h.enforceDMEncryption(msg, convUUID, input.keyVersion)
	if !ok {
		return
	}

	messageID, createdAt, updatedAt, err := h.persistDMMessage(convUUID, msg.UserID, msg.CredEpoch, input)
	if err != nil {
		// Any guard/store failure logs and returns the retryable save error (a
		// fenced epoch mismatch is caught by the client's next authoritative API
		// call). The channel path additionally distinguishes an auth-shaped signal
		// for a proven mismatch; the DM caller keeps the simpler safe default.
		log.Printf("Failed to persist DM message: %v", err)
		h.sendError(msg.ClientID, errMsgFailedSaveMessage)
		return
	}
	if input.msgType == "user" && h.opsCounter != nil {
		h.opsCounter.Increment(opsmetrics.MetricDMMessagesTotal)
	}

	convID := convUUID.String()
	attachments := h.linkDMAttachments(messageID, msg.UserID.String(), input.attachmentIDs, convID)

	nonce, _ := msg.Data[keyNonce].(string)
	h.sendDMMessageAck(dmMessageAckParams{
		client: client, nonce: nonce, messageID: messageID, convUUID: convUUID,
		createdAt: createdAt, updatedAt: updatedAt, gifSlug: input.gifSlug, attachments: attachments,
	})
	h.broadcastDMMessage(dmBroadcastCtx{
		messageID: messageID, convUUID: convUUID, senderUserID: msg.UserID,
		client: client, input: input, createdAt: createdAt, updatedAt: updatedAt,
		attachments: attachments, convPersonal: convPersonal,
	})
	h.sendDMUnreadNotify(convUUID, msg.UserID, dmUnreadLastMessage{
		content:   input.content,
		userID:    msg.UserID.String(),
		username:  client.Username,
		createdAt: createdAt,
	})

	if input.mentionAddendum != nil {
		h.routeDMMentionNotifications(convUUID, msg.UserID, input.mentionAddendum)
	}
}

// sendDMUnreadNotify sends a notification to DM participants who are not
// subscribed to the conversation (they don't have it active). Includes
// last-message metadata so clients can update previews and ordering.
func (h *Hub) sendDMUnreadNotify(conversationID, senderUserID uuid.UUID, lastMsg dmUnreadLastMessage) {
	participants, err := h.dmUnreadParticipants(conversationID)
	if err != nil {
		return
	}

	notifyMsg := OutgoingMessage{
		Type: "dm_unread_notify",
		Data: map[string]interface{}{
			keyConversationID: conversationID.String(),
			"last_message": map[string]interface{}{
				keyContent:   lastMsg.content,
				keyUserID:    lastMsg.userID,
				keyUsername:  lastMsg.username,
				keyCreatedAt: lastMsg.createdAt,
			},
		},
	}
	data, marshalErr := json.Marshal(notifyMsg)
	if marshalErr != nil {
		return
	}

	dmClients := h.dmSubscriptions[conversationID] // may be nil

	for _, uid := range participants {
		if uid == senderUserID {
			continue
		}
		h.notifyUnsubscribedUser(uid, dmClients, data)
	}
}

// dmUnreadParticipants queries all participant UUIDs for a DM conversation.
func (h *Hub) dmUnreadParticipants(conversationID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := h.db.Query(`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close DM participant rows: %v", closeErr)
		}
	}()

	var participants []uuid.UUID
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("scan DM participant: %w", err)
		}
		parsed, err := uuid.Parse(uid)
		if err != nil {
			return nil, fmt.Errorf("parse DM participant %q: %w", sanitizeLogValue(uid), err)
		}
		participants = append(participants, parsed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate DM participants: %w", err)
	}
	return participants, nil
}

// userHasDMSubscription checks whether any of a user's clients are subscribed
// to the given DM conversation's client set.
func (h *Hub) userHasDMSubscription(userClientIDs, dmClients map[uuid.UUID]bool) bool {
	if dmClients == nil {
		return false
	}
	for clientID := range userClientIDs {
		if dmClients[clientID] {
			return true
		}
	}
	return false
}

// notifyUnsubscribedUser sends data to all of a user's connected clients,
// but only if none of them are already subscribed to the DM conversation.
func (h *Hub) notifyUnsubscribedUser(uid uuid.UUID, dmClients map[uuid.UUID]bool, data []byte) {
	userClientIDs, ok := h.userClients[uid]
	if !ok {
		return // user not connected
	}
	if h.userHasDMSubscription(userClientIDs, dmClients) {
		return
	}
	h.sendToUserClients(userClientIDs, data)
}

// sendToUserClients sends raw message data to all of a user's connected clients.
func (h *Hub) sendToUserClients(userClientIDs map[uuid.UUID]bool, data []byte) {
	for clientID := range userClientIDs {
		if c, ok := h.clients[clientID]; ok {
			c.enqueueOutbound(data)
		}
	}
}

// handleDMTyping handles typing indicators for DM conversations.
func (h *Hub) handleDMTyping(msg IncomingMessage) {
	convID, ok := msg.Data[keyConversationID].(string)
	if !ok {
		return
	}

	convUUID, err := uuid.Parse(convID)
	if err != nil {
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Verify subscription
	subscribers, hasSubs := h.dmSubscriptions[convUUID]
	if !hasSubs || !subscribers[client.ID] {
		return
	}

	h.dmBroadcast <- DMBroadcastMessage{
		ConversationID: convUUID,
		ExcludeUser:    &msg.UserID,
		Data: OutgoingMessage{
			Type: "dm_typing",
			Data: map[string]interface{}{
				keyConversationID: convUUID,
				keyUserID:         msg.UserID,
				keyUsername:       client.Username,
				keyIsTyping:       msg.Data[keyIsTyping],
			},
		},
	}
}

// handleDMBroadcast sends a message to all subscribers of a DM conversation.
func (h *Hub) handleDMBroadcast(msg DMBroadcastMessage) {
	subscribers, ok := h.dmSubscriptions[msg.ConversationID]
	if !ok {
		return
	}

	messageData, err := json.Marshal(msg.Data)
	if err != nil {
		log.Printf("Failed to marshal DM broadcast message: %v", err)
		return
	}

	for clientID := range subscribers {
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}

		if msg.ExcludeUser != nil && client.UserID == *msg.ExcludeUser {
			continue
		}
		if !h.isCurrentDMParticipant(msg.ConversationID, client.UserID) {
			delete(subscribers, clientID)
			continue
		}

		if !client.enqueueOutbound(messageData) {
			h.handleUnregister(client)
		}
	}
}

func (h *Hub) isCurrentDMParticipant(conversationID, userID uuid.UUID) bool {
	if h.db == nil {
		return true
	}

	var ok bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`,
		conversationID.String(), userID.String(),
	).Scan(&ok)
	if err != nil {
		log.Printf("Failed to verify DM broadcast participant: %v", err)
		return false
	}
	return ok
}

// BroadcastToDM sends a message to all clients subscribed to a DM conversation (thread-safe).
func (h *Hub) BroadcastToDM(conversationID uuid.UUID, msg OutgoingMessage) {
	h.dmBroadcast <- DMBroadcastMessage{
		ConversationID: conversationID,
		Data:           msg,
	}
}

// BroadcastToDMParticipants sends to every connected client owned by a current
// participant, regardless of which DM that client has selected. Terminal call
// events use this path so background conversations update their previews and
// ringing state. If participant resolution is unavailable, retain the legacy
// subscription-scoped broadcast as a graceful fallback.
func (h *Hub) BroadcastToDMParticipants(conversationID uuid.UUID, msg OutgoingMessage) {
	if h.db == nil {
		h.BroadcastToDM(conversationID, msg)
		return
	}
	participants := h.resolveDMParticipants(conversationID)
	if len(participants) == 0 {
		h.BroadcastToDM(conversationID, msg)
		return
	}
	for userID := range participants {
		h.BroadcastToUser(userID, msg)
	}
}

// --- Force-disconnect handlers (server-side session termination) ---

// handleDisconnectUser disconnects ALL WebSocket clients for a user.
// Sends a courtesy "session_revoked" message before severing each TCP connection.
// The real enforcement is handleUnregister which closes the Send channel and connection.
// A rogue client that ignores session_revoked still gets killed at the TCP level.
func (h *Hub) handleDisconnectUser(userID uuid.UUID) {
	clientIDs, ok := h.userClients[userID]
	if !ok {
		return
	}

	// Collect clients before modifying the map (handleUnregister mutates it)
	clients := make([]*Client, 0, len(clientIDs))
	for clientID := range clientIDs {
		if client, ok := h.clients[clientID]; ok {
			clients = append(clients, client)
		}
	}

	// Courtesy notification + forceful disconnect
	revokedMsg, err := json.Marshal(OutgoingMessage{
		Type: sessionRevoked,
		Data: map[string]interface{}{"reason": "session_terminated"},
	})
	if err != nil {
		log.Printf("Failed to marshal user session-revoked message: %v", err)
		revokedMsg = nil
	}
	for _, client := range clients {
		// Best-effort courtesy message (non-blocking)
		if revokedMsg != nil {
			client.enqueueOutbound(revokedMsg)
		}
		// Real enforcement: sever TCP connection
		h.handleUnregister(client)
	}

	if len(clients) > 0 {
		log.Printf("Force-disconnected %d client(s) for user %s", len(clients), sanitizeLogValue(userID.String()))
	}
}

// handleDisconnectSession disconnects WebSocket clients matching a specific session ID.
// Used for targeted single-session revocation from the sessions management UI.
func (h *Hub) handleDisconnectSession(sessionID string) {
	if sessionID == "" {
		return
	}

	revokedMsg, err := json.Marshal(OutgoingMessage{
		Type: sessionRevoked,
		Data: map[string]interface{}{"reason": sessionRevoked},
	})
	if err != nil {
		log.Printf("Failed to marshal targeted session-revoked message: %v", err)
		revokedMsg = nil
	}

	var count int
	for _, client := range h.clients {
		if client.SessionID == sessionID {
			if revokedMsg != nil {
				client.enqueueOutbound(revokedMsg)
			}
			h.handleUnregister(client)
			count++
		}
	}

	if count > 0 {
		log.Printf("Force-disconnected %d client(s) for session %s", count, sanitizeLogValue(sessionID))
	}
}

// DisconnectUser forces all WebSocket connections for a user to close (thread-safe).
// Called from HTTP handlers (logout, credential change, session revocation) when
// a token is blacklisted or session is revoked. The server does not trust the
// client to disconnect itself — this is the authoritative enforcement.
func (h *Hub) DisconnectUser(userID uuid.UUID) {
	h.disconnectUser <- userID
}

// evictServerSubscriber removes every client of userID from serverID's server-level
// subscription set, so a member removed from the server (kicked, self-removed, or
// banned) stops receiving that server's WebSocket fanout (member events,
// key_revocation, server updates). It does NOT disconnect the client, since the user
// may still belong to other servers. Must run on the Run goroutine, which owns
// serverSubscriptions; callers reach it via BroadcastToServerAndPrune so the eviction
// is ordered relative to server broadcasts. CV-CAN-027/028.
func (h *Hub) evictServerSubscriber(serverID, userID uuid.UUID) {
	subscribers, ok := h.serverSubscriptions[serverID]
	if !ok {
		return
	}
	clientIDs, ok := h.userClients[userID]
	if !ok {
		return
	}
	for clientID := range clientIDs {
		delete(subscribers, clientID)
	}
	if len(subscribers) == 0 {
		delete(h.serverSubscriptions, serverID)
	}
}

// DisconnectSession forces a specific session's WebSocket connections to close (thread-safe).
// Called from the single-session revocation handler for targeted disconnect.
func (h *Hub) DisconnectSession(sessionID string) {
	h.disconnectSession <- sessionID
}

// handleProfileUpdate refreshes cached user info on the hub's Client objects.
// The actual "profile_updated" broadcast is sent by the HTTP handler (UpdateMe)
// to ensure reliable delivery without depending on the client-driven WS round-trip.
func (h *Hub) handleProfileUpdate(msg IncomingMessage) {
	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Re-query user info from DB
	var username string
	var displayName *string
	var avatarURL *string
	err := h.db.QueryRow(
		"SELECT username, display_name, avatar_url FROM users WHERE id = $1",
		msg.UserID,
	).Scan(&username, &displayName, &avatarURL)
	if err != nil {
		log.Printf("Failed to refresh user info for %s: %v", sanitizeLogValue(msg.UserID.String()), err)
		return
	}

	// Update cached fields on all of this user's clients
	if userClients, ok := h.userClients[client.UserID]; ok {
		for clientID := range userClients {
			if c, ok := h.clients[clientID]; ok {
				c.Username = username
				c.DisplayName = displayName
				c.AvatarURL = avatarURL
			}
		}
	}
}

// handleServerUpdate re-queries server info from DB and broadcasts to all server subscribers.
func (h *Hub) handleServerUpdate(msg IncomingMessage) {
	serverIDStr, ok := msg.Data[keyServerID].(string)
	if !ok {
		return
	}
	serverID, err := uuid.Parse(serverIDStr)
	if err != nil {
		return
	}

	// Re-query server info from DB
	var name string
	var iconURL *string
	var bannerURL *string
	err = h.db.QueryRow(
		"SELECT name, icon_url, banner_url FROM servers WHERE id = $1",
		serverID,
	).Scan(&name, &iconURL, &bannerURL)
	if err != nil {
		log.Printf("Failed to refresh server info for %s: %v", sanitizeLogValue(serverID.String()), err)
		return
	}

	// Broadcast to all subscribers of this server
	broadcastMsg := OutgoingMessage{
		Type: "server_updated",
		Data: map[string]interface{}{
			keyServerID:  serverID.String(),
			"name":       name,
			"icon_url":   iconURL,
			"banner_url": bannerURL,
		},
	}
	data, err := json.Marshal(broadcastMsg)
	if err != nil {
		return
	}

	subscribers, ok := h.serverSubscriptions[serverID]
	if !ok {
		return
	}
	for clientID := range subscribers {
		if c, ok := h.clients[clientID]; ok {
			c.enqueueOutbound(data)
		}
	}
}

// broadcastPresenceToAll sends userID's presence to that user's audience only —
// accepted friends, optional friends-of-friends, and shared-server peers
// (internal/presence.ComputePresenceAudience) — plus the sender's own connected
// devices (self / multi-device sync). Base presence is NEVER fanned to
// non-audience clients (#47: closes the base online-status leak).
//
// Non-self recipients receive one status (invisible is resolved to "offline"
// by callers); the sender's own devices retain their hidden status for an
// acknowledged self-state update.
//
// Concurrency: this runs on the hub Run goroutine and performs the (bounded,
// indexed) audience query synchronously — consistent with the spec's on-demand
// audience model and keeping all hub-map access race-free on the Run goroutine.
// If presence churn makes this query a Run-loop latency concern, move the
// computation off-goroutine with a connected-clients snapshot (follow-up).
func (h *Hub) broadcastPresenceToAll(userID uuid.UUID, status string, timestamp int64) {
	if h.db == nil {
		// No DB (e.g. a unit-test hub exercising client-map cleanup only): the
		// audience cannot be computed, so fail closed and skip the broadcast.
		// Production always has a DB (NewHub requires it).
		h.scheduleOnlineCountBroadcast(userID)
		return
	}
	audience, err := presence.ComputePresenceAudience(context.Background(), h.db, userID)
	if err != nil {
		// Fail closed: never fan out base presence when the audience cannot be
		// computed — no leak to unauthorized viewers.
		log.Printf("[hub] presence audience computation failed for %s; suppressing broadcast: %v", sanitizeLogValue(userID.String()), err)
		h.scheduleOnlineCountBroadcast(userID)
		return
	}
	audience[userID] = true // the sender's own devices always receive (not a leak)

	data, err := marshalPresenceFrame(userID, status, timestamp)
	if err != nil {
		log.Printf("Failed to marshal presence message: %v", err)
		return
	}
	selfData := data
	if selfStatus, hidden := h.hiddenPresence[userID]; hidden {
		selfData, err = marshalPresenceFrame(userID, selfStatus, timestamp)
		if err != nil {
			log.Printf("Failed to marshal self presence message: %v", err)
			return
		}
	}
	h.enqueuePresenceForAudience(audience, userID, data, selfData)

	// Schedule a debounced recomputation of online counts for all servers
	// the affected user belongs to (batches rapid presence changes).
	h.scheduleOnlineCountBroadcast(userID)
}

func marshalPresenceFrame(userID uuid.UUID, status string, timestamp int64) ([]byte, error) {
	return json.Marshal(OutgoingMessage{
		Type: "presence",
		Data: map[string]interface{}{
			keyUserID:   userID.String(),
			keyStatus:   status,
			"timestamp": timestamp,
		},
	})
}

func (h *Hub) enqueuePresenceForAudience(audience map[uuid.UUID]bool, userID uuid.UUID, data, selfData []byte) {
	h.mu.RLock()
	for viewerID := range audience {
		if clientSet, ok := h.userClients[viewerID]; ok {
			for clientID := range clientSet {
				if client, connected := h.clients[clientID]; connected {
					message := data
					if viewerID == userID {
						message = selfData
					}
					h.enqueueBasePresence(client, message)
				}
			}
		}
	}
	h.mu.RUnlock()
}

// scheduleOnlineCountBroadcast adds userID to the pending set and starts a
// 500ms debounce timer (if not already running). When the timer fires, all
// accumulated user IDs are flushed in a single batched DB+Redis query.
//
// Called from within the hub's Run goroutine, so no locking is needed.
func (h *Hub) scheduleOnlineCountBroadcast(userID uuid.UUID) {
	h.onlineCountPending[userID] = true
	if h.onlineCountTimer == nil {
		h.onlineCountTimer = time.NewTimer(500 * time.Millisecond)
	}
}

// flushOnlineCounts recomputes the visible online member count for every server
// that any pending user belongs to, then broadcasts the updated counts to all
// connected clients. A member is "visible online" if they have an active
// WebSocket connection and their status is not "invisible".
//
// Called from within the hub's Run goroutine when the debounce timer fires.
func (h *Hub) flushOnlineCounts() {
	h.onlineCountTimer = nil

	if len(h.onlineCountPending) == 0 {
		return
	}

	params := h.collectPendingUsers()

	serverMembers, allMemberIDs, err := h.queryServerMemberships(params)
	if err != nil || len(serverMembers) == 0 {
		return
	}

	visibleOnline := h.resolveVisibleOnline(allMemberIDs)

	counts := h.computeServerCounts(serverMembers, visibleOnline)

	h.handleGlobalBroadcast(OutgoingMessage{
		Type: "server_online_counts",
		Data: map[string]interface{}{
			keyCounts: counts,
		},
	})
}

// collectPendingUsers drains onlineCountPending and returns the user IDs as query params.
func (h *Hub) collectPendingUsers() []interface{} {
	params := make([]interface{}, 0, len(h.onlineCountPending))
	for uid := range h.onlineCountPending {
		params = append(params, uid)
	}
	clear(h.onlineCountPending)
	return params
}

// buildPlaceholders creates a "$1,$2,..." parameterized IN clause for the given params.
func buildPlaceholders(n int) string {
	placeholders := make([]byte, 0, n*4)
	for i := range n {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, []byte(fmt.Sprintf("$%d", i+1))...)
	}
	return string(placeholders)
}

// queryServerMemberships returns all server->member mappings for servers that any of
// the given users belong to, plus a set of all unique member IDs.
func (h *Hub) queryServerMemberships(params []interface{}) (map[string][]uuid.UUID, map[uuid.UUID]bool, error) {
	// Placeholders are safe — generated as $1,$2,… from loop index, not user input.
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf — placeholders are $1,$2,… generated from loop index; values are parameterized
	query := fmt.Sprintf( //nolint:gosec // G201
		`SELECT sm.server_id, sm.user_id
		FROM server_members sm
		WHERE sm.server_id IN (
			SELECT DISTINCT server_id FROM server_members WHERE user_id IN (%s)
		)
	`, buildPlaceholders(len(params)))

	rows, err := h.db.Query(query, params...)
	if err != nil {
		log.Printf("Failed to query memberships for online counts: %v", err)
		return nil, nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close membership rows for online counts: %v", closeErr)
		}
	}()

	serverMembers := make(map[string][]uuid.UUID)
	allMemberIDs := make(map[uuid.UUID]bool)
	for rows.Next() {
		var sid, uid uuid.UUID
		if err := rows.Scan(&sid, &uid); err != nil {
			return nil, nil, fmt.Errorf("scan membership for online counts: %w", err)
		}
		sidStr := sid.String()
		serverMembers[sidStr] = append(serverMembers[sidStr], uid)
		allMemberIDs[uid] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate memberships for online counts: %w", err)
	}
	return serverMembers, allMemberIDs, nil
}

// resolveVisibleOnline determines which member IDs are connected and not invisible,
// using a single Redis MGET round-trip for status checks.
func (h *Hub) resolveVisibleOnline(allMemberIDs map[uuid.UUID]bool) map[uuid.UUID]bool {
	connectedUIDs := make([]uuid.UUID, 0, len(allMemberIDs))
	redisKeys := make([]string, 0, len(allMemberIDs))
	for uid := range allMemberIDs {
		_, hidden := h.hiddenPresence[uid]
		if _, connected := h.userClients[uid]; connected && !hidden {
			connectedUIDs = append(connectedUIDs, uid)
			redisKeys = append(redisKeys, presence.StatusRedisKey(uid))
		}
	}

	visibleOnline := make(map[uuid.UUID]bool, len(connectedUIDs))
	if len(redisKeys) == 0 {
		return visibleOnline
	}

	statuses, err := h.redis.MGet(context.Background(), redisKeys...).Result()
	if err != nil {
		// Status is privacy-sensitive: on Redis failure, do not expose connected
		// users whose persisted state might be invisible.
		log.Printf("[hub] presence status batch lookup failed; failing closed: %v", err)
		return visibleOnline
	}

	for i, val := range statuses {
		if isVisibleStatus(val) {
			visibleOnline[connectedUIDs[i]] = true
		}
	}
	return visibleOnline
}

// isVisibleStatus returns true if a Redis presence value explicitly indicates
// the user should be counted as visibly online. A missing value fails closed
// because an invisible-status write or TTL refresh may have failed.
func isVisibleStatus(val interface{}) bool {
	if val == nil {
		return false
	}
	s, ok := val.(string)
	return ok && (s == statusOnline || s == statusDND)
}

func (h *Hub) setHiddenPresence(userID uuid.UUID, selfStatus string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hiddenPresence == nil {
		h.hiddenPresence = make(map[uuid.UUID]string)
	}
	h.hiddenPresence[userID] = selfStatus
}

func (h *Hub) clearHiddenPresence(userID uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.hiddenPresence, userID)
}

func (h *Hub) setPresenceRecovery(userID uuid.UUID, recovery presenceRecoveryState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.presenceRecovery == nil {
		h.presenceRecovery = make(map[uuid.UUID]presenceRecoveryState)
	}
	h.presenceRecovery[userID] = recovery
}

func (h *Hub) clearPresenceRecovery(userID uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.presenceRecovery, userID)
}

// richPresenceEmissionPermitted is the process-local half of the sender gate.
// A shared visible value is not authoritative unless this replica also has a
// connected sender whose visible transition completed without pending recovery.
func (h *Hub) richPresenceEmissionPermitted(userID uuid.UUID) bool {
	if h == nil {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.userClients[userID]) == 0 {
		return false
	}
	if _, hidden := h.hiddenPresence[userID]; hidden {
		return false
	}
	recovery, known := h.presenceRecovery[userID]
	return known && !recovery.pending && isVisibleStatus(recovery.status)
}

// computeServerCounts tallies the number of visibly-online members per server.
func (h *Hub) computeServerCounts(serverMembers map[string][]uuid.UUID, visibleOnline map[uuid.UUID]bool) map[string]int {
	counts := make(map[string]int, len(serverMembers))
	for sid, members := range serverMembers {
		count := 0
		for _, uid := range members {
			if visibleOnline[uid] {
				count++
			}
		}
		counts[sid] = count
	}
	return counts
}

// queryServerVoiceCounts returns the current voice participant count per server,
// keyed by server ID string. Shared by the periodic broadcast and the on-subscribe
// catch-up so both compute counts identically.
func (h *Hub) queryServerVoiceCounts() (map[string]int, error) {
	rows, err := h.db.Query(`
		SELECT c.server_id, COUNT(vp.id)
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		GROUP BY c.server_id
	`)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close server voice-count rows: %v", closeErr)
		}
	}()

	counts := make(map[string]int)
	for rows.Next() {
		var serverID string
		var count int
		if err := rows.Scan(&serverID, &count); err != nil {
			return nil, fmt.Errorf("scan server voice count: %w", err)
		}
		counts[serverID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate server voice counts: %w", err)
	}
	return counts, nil
}

// broadcastServerVoiceCounts queries voice_participants to compute per-server
// voice user counts and broadcasts them to all connected clients.
func (h *Hub) broadcastServerVoiceCounts() {
	counts, err := h.queryServerVoiceCounts()
	if err != nil {
		log.Printf("Failed to query server voice counts: %v", err)
		return
	}

	// CV-CAN-030: send each client only the counts for servers it is subscribed
	// to (a subset of its memberships), instead of the full cross-server map to
	// every client. Runs on the Run goroutine, which owns serverSubscriptions.
	// Each subscribed server is reported with its current count (0 when it has no
	// voice participants) so a drop-to-zero still reaches the client.
	perClient := make(map[uuid.UUID]map[string]int)
	for serverUUID, subs := range h.serverSubscriptions {
		count := counts[serverUUID.String()]
		for clientID := range subs {
			m := perClient[clientID]
			if m == nil {
				m = make(map[string]int)
				perClient[clientID] = m
			}
			m[serverUUID.String()] = count
		}
	}
	// Scoping forces a distinct payload per subscribed-server set rather than one
	// shared marshal for everyone, but many clients share the same set (e.g. every
	// client viewing the same server), yielding byte-identical payloads. Marshal
	// once per distinct set and reuse the bytes, so this hot path on the single Run
	// goroutine does not re-marshal the same map for every client under voice
	// churn. Sharing an immutable []byte across Send channels is the same pattern
	// handleGlobalBroadcast/handleServerBroadcast already use.
	payloadCache := make(map[string][]byte, len(perClient))
	for clientID, filtered := range perClient {
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}
		key := voiceCountPayloadKey(filtered)
		data, cached := payloadCache[key]
		if !cached {
			var err error
			data, err = json.Marshal(OutgoingMessage{
				Type: "server_voice_counts",
				Data: map[string]interface{}{keyCounts: filtered},
			})
			if err != nil {
				continue
			}
			payloadCache[key] = data
		}
		client.enqueueOutbound(data)
	}
}

// voiceCountPayloadKey builds a canonical, collision-free signature of a per-client
// voice-count map so broadcastServerVoiceCounts can memoize identical marshaled
// payloads within a single broadcast. Server IDs are UUIDs (no '=' or ';'), so the
// sorted "id=count;" encoding is injective. json.Marshal sorts map keys too, so any
// two maps with this signature marshal to identical bytes.
func voiceCountPayloadKey(counts map[string]int) string {
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	buf := make([]byte, 0, len(ids)*40)
	for _, id := range ids {
		buf = append(buf, id...)
		buf = append(buf, '=')
		buf = strconv.AppendInt(buf, int64(counts[id]), 10)
		buf = append(buf, ';')
	}
	return string(buf)
}

// BroadcastServerVoiceCounts triggers a recompute and broadcast of per-server
// voice participant counts. Safe to call from any goroutine.
func (h *Hub) BroadcastServerVoiceCounts() {
	select {
	case h.voiceCountSignal <- struct{}{}:
	default:
		// Already pending — coalesces rapid bursts
	}
}
