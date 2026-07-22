package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

type presenceSnapshotSeed struct {
	viewerID     uuid.UUID
	connectedIDs []uuid.UUID
	hidden       map[uuid.UUID]string
}

// presenceSnapshotConnectedLimit bounds the only Run-loop work performed for
// a reconnect replacement. Additional connected users are omitted fail-closed
// from base presence; Rich Presence has its own freshly authorized shortlist.
const presenceSnapshotConnectedLimit = 512

type richPresenceSnapshotEntry struct {
	Minimized bool            `json:"minimized"`
	Payload   json.RawMessage `json:"payload"`
	UpdatedAt int64           `json:"updated_at"`
}

// marshalPresenceSnapshot combines base visibility with the independently
// authorized activity projection. Activity-only senders stay base-offline: an
// activity grant must never imply permission to observe connection status.
func marshalPresenceSnapshot(
	base map[uuid.UUID]string,
	activity presence.ActivitySnapshot,
) ([]byte, error) {
	orderedUserIDs, err := presenceSnapshotUserIDs(base, activity)
	if err != nil {
		return nil, err
	}
	users, onlineUserIDs := buildPresenceSnapshotUsers(orderedUserIDs, base, activity)

	return json.Marshal(OutgoingMessage{
		Type: "presence_snapshot",
		Data: map[string]interface{}{
			"online_user_ids": onlineUserIDs,
			"users":           users,
		},
	})
}

func presenceSnapshotUserIDs(
	base map[uuid.UUID]string,
	activity presence.ActivitySnapshot,
) ([]uuid.UUID, error) {
	allUserIDs := make(map[uuid.UUID]bool, len(base)+len(activity))
	for userID := range base {
		allUserIDs[userID] = true
	}
	for userID, entries := range activity {
		if len(entries) == 0 {
			continue
		}
		if err := validatePresenceSnapshotActivity(userID, entries); err != nil {
			return nil, err
		}
		allUserIDs[userID] = true
	}
	orderedUserIDs := make([]uuid.UUID, 0, len(allUserIDs))
	for userID := range allUserIDs {
		orderedUserIDs = append(orderedUserIDs, userID)
	}
	sort.Slice(orderedUserIDs, func(left, right int) bool {
		return orderedUserIDs[left].String() < orderedUserIDs[right].String()
	})
	return orderedUserIDs, nil
}

func validatePresenceSnapshotActivity(
	userID uuid.UUID,
	entries map[presence.Category]presence.ActivitySnapshotEntry,
) error {
	if userID == uuid.Nil {
		return presence.ErrInvalidActivitySnapshot
	}
	for category, entry := range entries {
		if !validPresenceSnapshotCategory(category) || !validPresenceSnapshotEntry(entry) {
			return presence.ErrInvalidActivitySnapshot
		}
	}
	return nil
}

func validPresenceSnapshotCategory(category presence.Category) bool {
	return category == presence.CategoryServerVoice || category == presence.CategoryPrivateCall
}

func validPresenceSnapshotEntry(entry presence.ActivitySnapshotEntry) bool {
	payload := bytes.TrimSpace(entry.Payload)
	return entry.UpdatedAt > 0 && entry.UpdatedAt <= presence.MaxActivityUnixSeconds &&
		len(payload) >= 2 && payload[0] == '{' && payload[len(payload)-1] == '}' &&
		json.Valid(payload)
}

func buildPresenceSnapshotUsers(
	orderedUserIDs []uuid.UUID,
	base map[uuid.UUID]string,
	activity presence.ActivitySnapshot,
) ([]userPresenceInfo, []string) {
	users := make([]userPresenceInfo, 0, len(orderedUserIDs))
	onlineUserIDs := make([]string, 0, len(base))
	for _, userID := range orderedUserIDs {
		status, baseVisible := base[userID]
		if !baseVisible {
			status = statusOffline
		} else if status != statusOffline {
			onlineUserIDs = append(onlineUserIDs, userID.String())
		}
		users = append(users, presenceSnapshotUser(userID, status, activity[userID]))
	}
	return users, onlineUserIDs
}

func presenceSnapshotUser(
	userID uuid.UUID,
	status string,
	entries map[presence.Category]presence.ActivitySnapshotEntry,
) userPresenceInfo {
	user := userPresenceInfo{UserID: userID.String(), Status: status}
	if len(entries) == 0 {
		return user
	}
	user.RichPresence = make(map[presence.Category]richPresenceSnapshotEntry, len(entries))
	for category, entry := range entries {
		user.RichPresence[category] = richPresenceSnapshotEntry{
			Minimized: entry.Minimized,
			Payload:   append(json.RawMessage(nil), entry.Payload...),
			UpdatedAt: entry.UpdatedAt,
		}
	}
	return user
}

// capturePresenceSnapshotSeed copies Run-owned state synchronously. Slow
// database and Redis work operates only on this immutable seed off the loop;
// subsequent transitions are buffered on the client and replayed afterward.
func (h *Hub) capturePresenceSnapshotSeed(viewerID uuid.UUID) presenceSnapshotSeed {
	h.mu.RLock()
	defer h.mu.RUnlock()
	capacity := len(h.userClients)
	if capacity > presenceSnapshotConnectedLimit {
		capacity = presenceSnapshotConnectedLimit
	}
	seed := presenceSnapshotSeed{
		viewerID:     viewerID,
		connectedIDs: make([]uuid.UUID, 0, capacity),
		hidden:       make(map[uuid.UUID]string, capacity),
	}
	appendConnected := func(userID uuid.UUID) {
		seed.connectedIDs = append(seed.connectedIDs, userID)
		if status, hidden := h.hiddenPresence[userID]; hidden {
			seed.hidden[userID] = status
		}
	}
	if _, connected := h.userClients[viewerID]; connected {
		appendConnected(viewerID)
	}
	for userID := range h.userClients {
		if userID == viewerID {
			continue
		}
		if len(seed.connectedIDs) == presenceSnapshotConnectedLimit {
			break
		}
		appendConnected(userID)
	}
	return seed
}

func (h *Hub) loadBasePresenceSnapshot(
	ctx context.Context,
	seed presenceSnapshotSeed,
) (map[uuid.UUID]string, error) {
	visible, err := h.authorizeBasePresenceCandidates(ctx, seed)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("authorize base presence snapshot: %w", err)
	}

	base := make(map[uuid.UUID]string)
	for _, userID := range seed.connectedIDs {
		if !visible[userID] {
			continue
		}
		status, err := h.resolveSnapshotVisibleStatus(ctx, seed, userID)
		if err != nil {
			return nil, err
		}
		base[userID] = status
	}
	return base, nil
}

func (h *Hub) authorizeBasePresenceCandidates(
	ctx context.Context,
	seed presenceSnapshotSeed,
) (visible map[uuid.UUID]bool, returnErr error) {
	if seed.viewerID == uuid.Nil || len(seed.connectedIDs) > presenceSnapshotConnectedLimit {
		return nil, errors.New("invalid base presence snapshot seed")
	}
	visible = map[uuid.UUID]bool{seed.viewerID: true}
	if h.db == nil || len(seed.connectedIDs) == 0 {
		return visible, nil
	}
	candidateIDs := make([]string, 0, len(seed.connectedIDs))
	for _, userID := range seed.connectedIDs {
		if userID == uuid.Nil {
			return nil, errors.New("invalid base presence snapshot candidate")
		}
		candidateIDs = append(candidateIDs, userID.String())
	}
	rows, err := h.db.QueryContext(ctx, `
		WITH candidates AS (
			SELECT unnest($2::uuid[]) AS sender_id
		)
		SELECT DISTINCT candidate.sender_id
		FROM candidates candidate
		WHERE candidate.sender_id = $1
		   OR EXISTS (
		       SELECT 1
		       FROM friendships direct
		       WHERE direct.status = 'accepted'
		         AND (
		             (direct.requester_id = candidate.sender_id AND direct.addressee_id = $1)
		             OR
		             (direct.addressee_id = candidate.sender_id AND direct.requester_id = $1)
		         )
		   )
		   OR EXISTS (
		       SELECT 1
		       FROM privacy_settings sender_privacy
		       WHERE sender_privacy.user_id = candidate.sender_id
		         AND sender_privacy.dm_friends_of_friends
		         AND EXISTS (
		             SELECT 1
		             FROM friendships sender_friend
		             WHERE sender_friend.status = 'accepted'
		               AND (
		                   sender_friend.requester_id = candidate.sender_id
		                   OR sender_friend.addressee_id = candidate.sender_id
		               )
		               AND EXISTS (
		                   SELECT 1
		                   FROM friendships friend_viewer
		                   WHERE friend_viewer.status = 'accepted'
		                     AND (
		                         (
		                             friend_viewer.requester_id = CASE
		                                 WHEN sender_friend.requester_id = candidate.sender_id
		                                 THEN sender_friend.addressee_id
		                                 ELSE sender_friend.requester_id
		                             END
		                             AND friend_viewer.addressee_id = $1
		                         )
		                         OR (
		                             friend_viewer.addressee_id = CASE
		                                 WHEN sender_friend.requester_id = candidate.sender_id
		                                 THEN sender_friend.addressee_id
		                                 ELSE sender_friend.requester_id
		                             END
		                             AND friend_viewer.requester_id = $1
		                         )
		                     )
		               )
		         )
		   )
		   OR EXISTS (
		       SELECT 1
		       FROM server_members sender_member
		       JOIN server_members viewer_member
		         ON viewer_member.server_id = sender_member.server_id
		        AND viewer_member.user_id = $1
		       WHERE sender_member.user_id = candidate.sender_id
		   )
		ORDER BY candidate.sender_id
	`, seed.viewerID, pq.Array(candidateIDs))
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, rows.Close()) }()
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		visible[userID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return visible, nil
}

func (h *Hub) resolveSnapshotVisibleStatus(
	ctx context.Context,
	seed presenceSnapshotSeed,
	userID uuid.UUID,
) (string, error) {
	if selfStatus, hidden := seed.hidden[userID]; hidden {
		return snapshotStatusForViewer(userID == seed.viewerID, selfStatus), nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if h.redis == nil {
		return snapshotStatusForViewer(userID == seed.viewerID, statusOnline), nil
	}

	status, err := h.redis.Get(ctx, fmt.Sprintf(presenceKeyFmt, userID)).Result()
	if errors.Is(err, redis.Nil) {
		return snapshotStatusForViewer(userID == seed.viewerID, statusOnline), nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("load base presence status: %w", err)
	}

	return normalizeSnapshotVisibleStatus(userID == seed.viewerID, status), nil
}

func snapshotStatusForViewer(self bool, selfStatus string) string {
	if self {
		return selfStatus
	}
	return statusOffline
}

func normalizeSnapshotVisibleStatus(self bool, status string) string {
	switch status {
	case statusOnline, statusDND:
		return status
	case statusInvisible:
		return snapshotStatusForViewer(self, statusInvisible)
	default:
		log.Printf("[hub] snapshot encountered invalid persisted presence status")
		return snapshotStatusForViewer(self, statusOnline)
	}
}

func (h *Hub) runClientBootstrap(
	parentCtx context.Context,
	client *Client,
	seed presenceSnapshotSeed,
) {
	timeout := h.clientBootstrapTimeout
	if timeout <= 0 {
		timeout = clientBootstrapTimeout
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	releaseSlot, err := h.acquireClientBootstrapSlot(ctx)
	if err != nil {
		h.failClientBootstrap(parentCtx, client, err)
		return
	}
	defer releaseSlot()

	base, err := h.loadBasePresenceSnapshot(ctx, seed)
	if err != nil {
		h.failClientBootstrap(parentCtx, client, err)
		return
	}

	activity, err := h.loadClientBootstrapActivity(ctx, client.UserID)
	if err != nil {
		h.failClientBootstrap(parentCtx, client, err)
		return
	}
	if err := ctx.Err(); err != nil {
		h.failClientBootstrap(parentCtx, client, err)
		return
	}
	completed, err := h.completeClientPresenceBootstrap(ctx, client, seed, base, activity)
	if err != nil {
		h.failClientBootstrap(parentCtx, client, err)
		return
	}
	if !completed {
		return
	}

	// Voice counts are informational and must never overtake the replacement.
	h.sendVoiceCountsSnapshot(ctx, client)
}

func (h *Hub) loadClientBootstrapActivity(
	ctx context.Context,
	viewerID uuid.UUID,
) (presence.ActivitySnapshot, error) {
	if h.activitySnapshot == nil {
		return make(presence.ActivitySnapshot), nil
	}
	activity, err := h.activitySnapshot(ctx, viewerID)
	if errors.Is(err, presence.ErrActivitySnapshotCandidateLimit) {
		// The replacement frame is authoritative, so an empty activity map safely
		// clears any client cache. Keep the socket usable when a viewer shares more
		// bounded candidates than this bootstrap can freshly authorize in one pass.
		return make(presence.ActivitySnapshot), nil
	}
	return activity, err
}

func (h *Hub) completeClientPresenceBootstrap(
	ctx context.Context,
	client *Client,
	seed presenceSnapshotSeed,
	base map[uuid.UUID]string,
	activity presence.ActivitySnapshot,
) (bool, error) {
	return h.completePreparedClientBootstrap(
		client,
		func(publish func([]byte) error) error {
			return h.finalizeClientActivitySnapshot(ctx, client.UserID, seed, base, activity, publish)
		},
		func() error { return h.sendCustomTextSnapshot(ctx, client) },
	)
}

func (h *Hub) finalizeClientActivitySnapshot(
	ctx context.Context,
	viewerID uuid.UUID,
	seed presenceSnapshotSeed,
	base map[uuid.UUID]string,
	activity presence.ActivitySnapshot,
	publish func([]byte) error,
) error {
	publishActivity := func(finalized presence.ActivitySnapshot) error {
		return h.publishClientPresenceSnapshot(ctx, seed, base, finalized, publish)
	}
	if h.activitySnapshotFinalize == nil {
		return publishActivity(activity)
	}
	return h.activitySnapshotFinalize(ctx, viewerID, activity, publishActivity)
}

func (h *Hub) publishClientPresenceSnapshot(
	ctx context.Context,
	seed presenceSnapshotSeed,
	base map[uuid.UUID]string,
	activity presence.ActivitySnapshot,
	publish func([]byte) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	visible, err := h.authorizeBasePresenceCandidates(ctx, seed)
	if err != nil {
		return err
	}
	refreshedBase, err := h.refreshBasePresenceForPublication(ctx, seed, base, visible)
	if err != nil {
		return err
	}
	snapshot, err := marshalPresenceSnapshot(refreshedBase, activity)
	if err != nil {
		return err
	}
	return publish(snapshot)
}

func (h *Hub) refreshBasePresenceForPublication(
	ctx context.Context,
	seed presenceSnapshotSeed,
	base map[uuid.UUID]string,
	visible map[uuid.UUID]bool,
) (map[uuid.UUID]string, error) {
	currentSeed := presenceSnapshotSeed{
		viewerID: seed.viewerID,
		hidden:   make(map[uuid.UUID]string),
	}
	connected := make(map[uuid.UUID]bool, len(base))
	h.mu.RLock()
	_, viewerRegistered := h.userClients[seed.viewerID]
	for userID := range base {
		connectedNow, hiddenStatus, hidden := h.currentBasePresenceCandidateLocked(
			seed, visible, viewerRegistered, userID,
		)
		if !connectedNow {
			continue
		}
		connected[userID] = true
		if hidden {
			currentSeed.hidden[userID] = hiddenStatus
		}
	}
	h.mu.RUnlock()

	refreshed := make(map[uuid.UUID]string, len(connected))
	for userID := range connected {
		status, err := h.resolveSnapshotVisibleStatus(ctx, currentSeed, userID)
		if err != nil {
			return nil, err
		}
		refreshed[userID] = status
	}
	return refreshed, nil
}

// currentBasePresenceCandidateLocked classifies one reconnect candidate while
// the caller holds h.mu.RLock. Redis-backed status resolution remains outside
// the Hub lock.
func (h *Hub) currentBasePresenceCandidateLocked(
	seed presenceSnapshotSeed,
	visible map[uuid.UUID]bool,
	viewerRegistered bool,
	userID uuid.UUID,
) (bool, string, bool) {
	if !visible[userID] {
		return false, "", false
	}
	if viewerRegistered {
		if _, present := h.userClients[userID]; !present {
			return false, "", false
		}
	}
	if status, hidden := h.hiddenPresence[userID]; hidden {
		return true, status, true
	}
	if !viewerRegistered {
		status, hidden := seed.hidden[userID]
		return true, status, hidden
	}
	return true, "", false
}

func noOpClientBootstrapRelease() {
	// No semaphore slot was acquired, so release intentionally does nothing.
}

func (h *Hub) acquireClientBootstrapSlot(ctx context.Context) (func(), error) {
	if h.clientBootstrapSlots == nil {
		return noOpClientBootstrapRelease, nil
	}
	select {
	case h.clientBootstrapSlots <- struct{}{}:
		return func() { <-h.clientBootstrapSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *Hub) failClientBootstrap(
	parentCtx context.Context,
	client *Client,
	err error,
) {
	client.cancelBootstrap()
	if parentCtx.Err() != nil {
		return
	}
	log.Printf("[hub] reconnect replacement failed; disconnecting client: %T", err)
	if disconnectErr := h.disconnectPrivacyCriticalClient(client); disconnectErr != nil {
		log.Printf("[hub] reconnect replacement disconnect failed: %T", disconnectErr)
	}
}

func (h *Hub) enqueueBasePresence(client *Client, data []byte) {
	switch client.bufferBootstrapLive(data) {
	case bootstrapBufferEnqueued, bootstrapBufferCanceled:
		return
	case bootstrapBufferOverflow:
		if err := h.disconnectPrivacyCriticalClient(client); err != nil {
			log.Printf("[hub] reconnect base-presence overflow disconnect failed: %T", err)
		}
		return
	case bootstrapBufferInactive:
	}
	client.enqueueOutbound(data)
}

// completeClientBootstrap atomically orders an already prepared replacement
// before its already buffered replay and live deltas.
func (h *Hub) completeClientBootstrap(client *Client, snapshot []byte) bool {
	client.sendMu.Lock()
	client.bootstrapMu.Lock()
	if client.bootstrapFailed {
		return h.failClientBootstrapFlushLocked(client)
	}
	if client.bootstrapCanceled || !client.bootstrapActive {
		client.bootstrapActive = false
		client.bootstrapReplay = nil
		client.bootstrapLive = nil
		client.bootstrapMu.Unlock()
		client.sendMu.Unlock()
		return false
	}

	frameCount := 1 + len(client.bootstrapReplay) + len(client.bootstrapLive)
	if client.sendClosed || client.Send == nil || cap(client.Send)-len(client.Send) < frameCount {
		return h.failClientBootstrapFlushLocked(client)
	}
	if h.clientBootstrapBeforeFlush != nil {
		h.clientBootstrapBeforeFlush()
	}

	if !enqueueBootstrapFrame(client.Send, snapshot) {
		return h.failClientBootstrapFlushLocked(client)
	}
	if h.clientBootstrapAfterFirstFrame != nil {
		h.clientBootstrapAfterFirstFrame()
	}
	for _, frame := range client.bootstrapReplay {
		if !enqueueBootstrapFrame(client.Send, frame) {
			return h.failClientBootstrapFlushLocked(client)
		}
	}
	for _, frame := range client.bootstrapLive {
		if !enqueueBootstrapFrame(client.Send, frame) {
			return h.failClientBootstrapFlushLocked(client)
		}
	}
	client.bootstrapActive = false
	client.bootstrapReplay = nil
	client.bootstrapLive = nil
	client.bootstrapMu.Unlock()
	client.sendMu.Unlock()
	return true
}

// completePreparedClientBootstrap prepares Custom Status replay before activity
// finalization so the latter can acquire all sender gates without nesting a
// per-sender gate. prepareSnapshot owns final authorization and calls publish
// while those gates remain held. publish delegates to completeClientBootstrap,
// whose brief sendMu/bootstrapMu section performs capacity preflight and the
// atomic snapshot/replay/live flush without blocking Hub.Run during slow work.
func (h *Hub) completePreparedClientBootstrap(
	client *Client,
	prepareSnapshot func(func([]byte) error) error,
	prepareReplay func() error,
) (bool, error) {
	if !h.clientBootstrapCanPrepare(client) {
		return false, nil
	}

	if prepareReplay != nil {
		if err := prepareReplay(); err != nil {
			return false, err
		}
		if !h.clientBootstrapCanPrepare(client) {
			return false, nil
		}
	}

	completed := false
	published := false
	err := prepareSnapshot(func(snapshot []byte) error {
		published = true
		completed = h.completeClientBootstrap(client, snapshot)
		return nil
	})
	if err != nil {
		return false, err
	}
	if !published {
		return false, errClientBootstrapPublicationMissing
	}
	return completed, nil
}

func (h *Hub) clientBootstrapCanPrepare(client *Client) bool {
	client.bootstrapMu.Lock()
	failed := client.bootstrapFailed
	if client.bootstrapCanceled || !client.bootstrapActive {
		client.bootstrapActive = false
		client.bootstrapReplay = nil
		client.bootstrapLive = nil
		client.bootstrapMu.Unlock()
		return false
	}
	client.bootstrapMu.Unlock()
	if failed {
		h.completeClientBootstrap(client, nil)
		return false
	}
	return true
}

func enqueueBootstrapFrame(send chan []byte, frame []byte) bool {
	select {
	case send <- frame:
		return true
	default:
		return false
	}
}

// failClientBootstrapFlushLocked releases bootstrapMu and sendMu before
// closing the socket. Callers must hold both locks.
func (h *Hub) failClientBootstrapFlushLocked(client *Client) bool {
	client.bootstrapActive = false
	client.bootstrapCanceled = true
	client.bootstrapFailed = true
	client.bootstrapReplay = nil
	client.bootstrapLive = nil
	client.bootstrapMu.Unlock()
	client.sendMu.Unlock()
	if err := h.disconnectPrivacyCriticalClient(client); err != nil {
		log.Printf("[hub] reconnect bootstrap queue disconnect failed: %T", err)
	}
	return false
}
