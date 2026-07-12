package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/presence"
)

// customTextCategory is the rich-presence category string for custom text. It
// must match RichPresenceCategorySchema's `custom_text` member in
// client/desktop/src/renderer/types/ws-events.ts (#1233 Task B1/B2).
const customTextCategory = "custom_text"

const (
	customTextDeliveryLockStripes  = 256
	customTextResetAudienceTimeout = 3 * time.Second
)

type customTextDeliveryCoordinator interface {
	WithSender(uuid.UUID, func())
}

func customTextDeliveryLock(
	locks *[customTextDeliveryLockStripes]sync.Mutex,
	senderID uuid.UUID,
) *sync.Mutex {
	const (
		fnvOffset32 = uint32(2166136261)
		fnvPrime32  = uint32(16777619)
	)
	hash := fnvOffset32
	for _, part := range senderID {
		hash ^= uint32(part)
		hash *= fnvPrime32
	}
	return &locks[int(hash%customTextDeliveryLockStripes)]
}

func (h *Hub) withCustomTextDelivery(senderID uuid.UUID, fn func()) {
	if h.customTextDeliveryCoordinator != nil {
		h.customTextDeliveryCoordinator.WithSender(senderID, fn)
		return
	}
	lock := customTextDeliveryLock(&h.customTextDeliveryLocks, senderID)
	lock.Lock()
	defer lock.Unlock()
	fn()
}

// CustomTextPayload is the wire payload for a custom-text rich-presence update.
// It mirrors CustomTextPresencePayloadSchema in ws-events.ts: `text` is required
// and non-empty (1..140 code points, enforced at the REST write boundary and the
// DB CHECK); `emoji` is optional (omitted when empty so it round-trips to "no
// emoji" on the client, where the zod field is `.optional()`).
//
// A nil *CustomTextPayload at the BroadcastCustomText boundary means CLEAR — the
// user turned custom text off or wrote an empty string — emitted as a
// rich_presence_clear frame, never an update with empty text.
type CustomTextPayload struct {
	Emoji string `json:"emoji,omitempty"`
	Text  string `json:"text"`
}

// BroadcastCustomText fans senderID's custom-text status out to exactly the
// audience permitted to see it — risk: privacy. A non-nil payload is an UPDATE
// (rich_presence_update) delivered to the new tier-audience (computed by
// presence.ComputeCustomTextAudience: 0=Off→∅, 1=Friends→friends+FoF,
// 2=Servers→+shared-server peers; plus the sender's own devices for self-sync). A
// nil payload is a CLEAR. EITHER way, viewers in the PRIOR (oldTier) audience but
// NOT the new one are sent a rich_presence_clear so a stale status never lingers
// on someone who lost permission (#1233, Gitar review). The handler reads oldTier
// before the UPSERT (the DB now holds the new tier). Up to two frames are
// marshaled (a clear, plus an update when payload != nil); each is delivered once
// per viewer.
//
// Fail-closed: if either audience cannot be computed (DB error) NOTHING is sent.
// Only metadata is logged (sanitized sender UUID); the custom text VALUE is never
// logged ([internal]rules/observability.md "No PII").
//
// Concurrency: invoked synchronously on the HTTP handler goroutine while the
// users Handler holds its per-sender Custom Status coordinator, not on the hub
// Run goroutine. Live broadcasts, audience deltas, and reconnect snapshots use
// the same Hub sender-delivery coordinator so a delayed frame cannot overwrite
// newer delivered state. Hub-map reads are guarded by h.mu.RLock; DB audience
// queries run first, outside the hub lock.
func (h *Hub) BroadcastCustomText(senderID uuid.UUID, oldTier int, payload *CustomTextPayload) {
	if h.db == nil {
		// No DB (DB-free unit hub): fail closed and skip. Production always has a
		// DB (NewHub requires it).
		return
	}
	h.withCustomTextDelivery(senderID, func() {
		h.broadcastCustomText(senderID, oldTier, payload)
	})
}

func (h *Hub) broadcastCustomText(senderID uuid.UUID, oldTier int, payload *CustomTextPayload) {
	newAud, oldAud, err := h.customTextAudiences(context.Background(), senderID, oldTier, payload)
	if err != nil {
		log.Printf("[hub] custom-text audience computation failed for %s; suppressing broadcast: %v", sanitizeLogValue(senderID.String()), err)
		return // fail closed
	}

	clearData, err := marshalCustomTextFrame(senderID, nil) // rich_presence_clear frame
	if err != nil {
		log.Printf("[hub] failed to marshal custom-text clear for %s: %v", sanitizeLogValue(senderID.String()), err)
		return
	}
	var updateData []byte
	if payload != nil {
		updateData, err = marshalCustomTextFrame(senderID, payload) // rich_presence_update frame
		if err != nil {
			log.Printf("[hub] failed to marshal custom-text frame for %s: %v", sanitizeLogValue(senderID.String()), err)
			return
		}
	}

	// Viewers who left the audience (oldAud \ newAud) get a clear.
	excluded := make(map[uuid.UUID]bool)
	for viewerID := range oldAud {
		if !newAud[viewerID] {
			excluded[viewerID] = true
		}
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	if payload != nil {
		h.sendToUsers(newAud, updateData)
	}
	h.sendPrivacyClearToUsers(excluded, clearData)
}

// BroadcastCustomTextAudienceDelta applies an already-computed authorization
// change without rebroadcasting to viewers who remain in the audience. Viewers
// in oldAudience but not newAudience receive a clear; viewers newly added receive
// the current update only when payload is non-nil. The sender is never part of
// this delta because their own display is unchanged by recipient exceptions.
func (h *Hub) BroadcastCustomTextAudienceDelta(senderID uuid.UUID, oldAudience, newAudience map[uuid.UUID]bool, payload *CustomTextPayload) {
	h.withCustomTextDelivery(senderID, func() {
		h.broadcastCustomTextAudienceDelta(senderID, oldAudience, newAudience, payload)
	})
}

// ClearCustomTextForPresenceAudience sends a privacy-critical clear only to the
// sender's conservative base presence audience. Destructive key resets force
// the Custom Status tier Off and delete materialized exceptions before fan-out,
// but do not change friendships, opted-in friends-of-friends, or shared-server
// membership.
//
// That base audience therefore remains available after commit without exposing
// the sender's identity or reset timing to unrelated connected clients. Its DB
// read is bounded so a stalled pool cannot prevent the reset-session disconnect.
func (h *Hub) ClearCustomTextForPresenceAudience(senderID uuid.UUID) {
	if h.db == nil {
		return
	}
	h.withCustomTextDelivery(senderID, func() {
		ctx, cancel := context.WithTimeout(context.Background(), customTextResetAudienceTimeout)
		defer cancel()
		audience, err := presence.ComputePresenceAudience(ctx, h.db, senderID)
		if err != nil {
			log.Printf("[hub] reset custom-text audience computation failed for %s; suppressing clear: %v",
				sanitizeLogValue(senderID.String()), err)
			return
		}
		data, err := marshalCustomTextFrame(senderID, nil)
		if err != nil {
			log.Printf("[hub] failed to marshal reset custom-text clear for %s: %v",
				sanitizeLogValue(senderID.String()), err)
			return
		}

		h.mu.RLock()
		defer h.mu.RUnlock()
		h.sendPrivacyClearToUsers(audience, data)
	})
}

func customTextAudienceDifference(senderID uuid.UUID, audience, otherAudience map[uuid.UUID]bool) map[uuid.UUID]bool {
	difference := make(map[uuid.UUID]bool)
	for viewerID := range audience {
		if viewerID != senderID && !otherAudience[viewerID] {
			difference[viewerID] = true
		}
	}
	return difference
}

func (h *Hub) broadcastCustomTextAudienceDelta(senderID uuid.UUID, oldAudience, newAudience map[uuid.UUID]bool, payload *CustomTextPayload) {
	removed := customTextAudienceDifference(senderID, oldAudience, newAudience)
	added := customTextAudienceDifference(senderID, newAudience, oldAudience)

	var clearData []byte
	if len(removed) > 0 {
		var err error
		clearData, err = marshalCustomTextFrame(senderID, nil)
		if err != nil {
			log.Printf("[hub] failed to marshal presence audience-delta clear for %s: %v", sanitizeLogValue(senderID.String()), err)
			return
		}
	}

	var updateData []byte
	if payload != nil && len(added) > 0 {
		var err error
		updateData, err = marshalCustomTextFrame(senderID, payload)
		if err != nil {
			log.Printf("[hub] failed to marshal presence audience-delta update for %s: %v", sanitizeLogValue(senderID.String()), err)
			return
		}
	}

	h.deliverCustomTextAudienceDelta(removed, added, clearData, updateData)
}

func (h *Hub) deliverCustomTextAudienceDelta(removed, added map[uuid.UUID]bool, clearData, updateData []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	// Privacy clears must be enqueued before newly authorized updates so the
	// delta preserves the caller's clear-then-update delivery phase ordering.
	if len(clearData) > 0 {
		h.sendPrivacyClearToUsers(removed, clearData)
	}
	if len(updateData) > 0 {
		h.sendToUsers(added, updateData)
	}
}

// customTextAudiences computes the post-change (newAud) and prior (oldAud)
// custom-text audiences for a settings change, each including the sender's own
// devices (self-sync). newAud is empty on a CLEAR (payload == nil). A non-nil
// error means a DB failure — the caller MUST fail closed (send nothing). Runs
// OUTSIDE the hub lock (DB I/O).
func (h *Hub) customTextAudiences(ctx context.Context, senderID uuid.UUID, oldTier int, payload *CustomTextPayload) (newAud, oldAud map[uuid.UUID]bool, err error) {
	newAud = map[uuid.UUID]bool{}
	if payload != nil {
		newAud, err = presence.ComputeCustomTextAudience(ctx, h.db, senderID)
		if err != nil {
			return nil, nil, err
		}
		newAud[senderID] = true
	}
	oldAud, err = presence.ComputeCustomTextAudienceForTier(ctx, h.db, senderID, oldTier)
	if err != nil {
		return nil, nil, err
	}
	oldAud[senderID] = true
	return newAud, oldAud, nil
}

// sendToUsers delivers data to every connected client of each user in users. The
// caller MUST hold h.mu (R)Lock — it reads h.userClients / h.clients.
func (h *Hub) sendToUsers(users map[uuid.UUID]bool, data []byte) {
	for userID := range users {
		if clientSet, ok := h.userClients[userID]; ok {
			h.sendToUserClients(clientSet, data)
		}
	}
}

// sendPrivacyClearToUsers enqueues authorization-revocation frames only when a
// client queue has immediate capacity. A full queue is never inspected or
// mutated; the client is disconnected so reconnect performs a fresh authorized
// snapshot. The caller holds h.mu.RLock, which keeps each client in the hub maps
// until enqueuePrivacyCritical returns.
func (h *Hub) sendPrivacyClearToUsers(users map[uuid.UUID]bool, data []byte) {
	for userID := range users {
		clientSet, ok := h.userClients[userID]
		if !ok {
			continue
		}
		for clientID := range clientSet {
			if client, ok := h.clients[clientID]; ok {
				enqueuePrivacyCritical(client, data)
			}
		}
	}
}

type privacyCriticalEnqueueOutcome uint8

const (
	privacyCriticalEnqueueSucceeded privacyCriticalEnqueueOutcome = iota
	privacyCriticalEnqueueDisconnectRequired
)

func enqueuePrivacyCritical(client *Client, data []byte) privacyCriticalEnqueueOutcome {
	select {
	case client.Send <- data:
		return privacyCriticalEnqueueSucceeded
	default:
	}

	// Never dequeue from the shared client queue. Other hub paths write to Send
	// directly, so inspecting or replacing a queued frame cannot be made atomic
	// under h.mu. Closing forces reconnect and a fresh authorization snapshot.
	closePrivacyCriticalClient(client)
	return privacyCriticalEnqueueDisconnectRequired
}

func closePrivacyCriticalClient(client *Client) {
	// Production clients always have a socket. DB-free unit clients may omit
	// Conn; the enqueue outcome still records that a disconnect is required.
	if client.Conn != nil {
		if err := client.Conn.Close(); err != nil {
			log.Printf("[hub] privacy-clear slow-client close failed")
		}
	}
}

// marshalCustomTextFrame builds the wire frame for a custom-text change: a
// rich_presence_update when payload is non-nil, else a rich_presence_clear. The
// shapes mirror RichPresenceUpdateSchema / RichPresenceClearSchema in
// ws-events.ts exactly.
func marshalCustomTextFrame(senderID uuid.UUID, payload *CustomTextPayload) ([]byte, error) {
	if payload == nil {
		return json.Marshal(OutgoingMessage{
			Type: "rich_presence_clear",
			Data: map[string]interface{}{
				keyUserID:  senderID.String(),
				"category": customTextCategory,
			},
		})
	}
	return json.Marshal(OutgoingMessage{
		Type: "rich_presence_update",
		Data: map[string]interface{}{
			keyUserID:    senderID.String(),
			"category":   customTextCategory,
			"payload":    payload,
			keyUpdatedAt: time.Now().Unix(),
		},
	})
}

// sendCustomTextSnapshot extends the on-connect presence snapshot (#1233 Task
// B4): the connecting viewer V receives one rich_presence_update for every user
// U who has custom text set AND for whom V is in U's tier-audience.
//
// ACCESS CONTROL (risk: privacy): the inverse-audience problem. Rather than
// trusting the symmetry shortcut that base presence uses, this honors EACH U's
// custom_text_tier independently:
//
//	tier 0 (Off): excluded entirely (the WHERE clause filters tier > 0).
//	tier 1 (Friends): V sees U only if V is U's friend or (when U enabled
//	  dm_friends_of_friends) U's friend-of-friend — NOT merely a shared-server peer.
//	tier 2 (Servers): V also sees U as a shared-server peer.
//
// We resolve this by computing, for each candidate U, U's own custom-text
// audience via presence.ComputeCustomTextAudience and including U only if V is in
// it. The candidate set is bounded by a single query (users with custom text on),
// so this is O(candidates) audience computations, not an N×M fan-out over all
// users. Fail-closed per candidate: a candidate whose audience errors is skipped,
// never optimistically included.
//
// Called from sendPresenceSnapshot on the Hub Run goroutine. Each candidate's
// final authorization read + enqueue is serialized with post-commit audience
// deltas for that sender, so a stale snapshot update cannot follow a revocation.
func (h *Hub) sendCustomTextSnapshot(ctx context.Context, client *Client) {
	if h.db == nil {
		return // DB-free unit hub: nothing to snapshot, fail-safe
	}

	candidates, err := h.customTextCandidates(ctx)
	if err != nil {
		// Fail closed: if the candidate set can't be read, send no custom-text
		// snapshot rather than risk an unfiltered emission. Metadata only.
		log.Printf("[hub] custom-text snapshot candidate query failed for viewer %s: %v", sanitizeLogValue(client.UserID.String()), err)
		return
	}

	for _, senderID := range candidates {
		if senderID == client.UserID {
			continue // self is delivered via the live BroadcastCustomText self-sync, not the snapshot of others
		}
		h.withCustomTextDelivery(senderID, func() {
			h.sendCustomTextSnapshotCandidate(ctx, client, senderID)
		})
	}
}

func (h *Hub) sendCustomTextSnapshotCandidate(ctx context.Context, client *Client, senderID uuid.UUID) {
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		log.Printf("[hub] custom-text snapshot transaction failed for sender %s viewer %s: %v",
			sanitizeLogValue(senderID.String()), sanitizeLogValue(client.UserID.String()), err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	var tier int
	var text, emoji sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings
		WHERE user_id = $1
	`, senderID).Scan(&tier, &text, &emoji)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		log.Printf("[hub] custom-text snapshot current-state query failed for sender %s viewer %s: %v",
			sanitizeLogValue(senderID.String()), sanitizeLogValue(client.UserID.String()), err)
		return
	}
	if h.customTextSnapshotAfterStateRead != nil {
		h.customTextSnapshotAfterStateRead(senderID, client.UserID)
	}
	if tier <= 0 || !text.Valid || text.String == "" {
		return
	}

	audience, audErr := presence.ComputeCustomTextAudience(ctx, tx, senderID)
	if audErr != nil {
		// Fail closed for this candidate only; never optimistically include.
		log.Printf("[hub] custom-text snapshot audience failed for sender %s viewer %s: %v",
			sanitizeLogValue(senderID.String()), sanitizeLogValue(client.UserID.String()), audErr)
		return
	}
	if !audience[client.UserID] {
		return // viewer is NOT in this user's custom-text audience — exclude (privacy lock)
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[hub] custom-text snapshot transaction commit failed for sender %s viewer %s: %v",
			sanitizeLogValue(senderID.String()), sanitizeLogValue(client.UserID.String()), err)
		return
	}

	payload := &CustomTextPayload{Text: text.String}
	if emoji.Valid {
		payload.Emoji = emoji.String
	}
	data, mErr := marshalCustomTextFrame(senderID, payload)
	if mErr != nil {
		log.Printf("[hub] custom-text snapshot marshal failed for sender %s: %v", sanitizeLogValue(senderID.String()), mErr)
		return
	}
	if h.customTextSnapshotBeforeEnqueue != nil {
		h.customTextSnapshotBeforeEnqueue(senderID, client.UserID)
	}
	select {
	case client.Send <- data:
	default:
	}
}

// customTextCandidates returns every user with custom text set AND tier > 0 —
// the bounded sender-ID shortlist whose status MIGHT be visible to a connecting
// viewer. The current tier, text, and emoji are deliberately re-read inside the
// per-sender delivery coordinator before authorization and enqueue; the outer
// query never captures a payload that could become stale while waiting.
func (h *Hub) customTextCandidates(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT user_id
		FROM user_presence_settings
		WHERE custom_text_tier > 0 AND custom_text IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []uuid.UUID
	for rows.Next() {
		var senderID uuid.UUID
		if err := rows.Scan(&senderID); err != nil {
			return nil, err
		}
		out = append(out, senderID)
	}
	return out, rows.Err()
}
