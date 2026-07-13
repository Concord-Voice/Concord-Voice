package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/presence"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/presencehistory"
)

// customTextCategory is the rich-presence category string for custom text. It
// must match RichPresenceCategorySchema's `custom_text` member in
// client/desktop/src/renderer/types/ws-events.ts (#1233 Task B1/B2).
const customTextCategory = "custom_text"

const (
	customTextResetAudienceTimeout = 3 * time.Second
)

var (
	// ErrCustomTextDeliveryPlan identifies a malformed or unsupported prepared plan.
	ErrCustomTextDeliveryPlan = errors.New("invalid custom status delivery plan")
	// ErrCustomTextDeliveryMarshal identifies frame construction failures.
	ErrCustomTextDeliveryMarshal = errors.New("custom status delivery marshal failed")
	// ErrCustomTextDeliveryBroadcaster identifies queue or disconnect failures.
	ErrCustomTextDeliveryBroadcaster = errors.New("custom status delivery broadcaster failed")
)

// CustomTextPayload is the wire payload for a custom-text rich-presence update.
// It mirrors CustomTextPresencePayloadSchema in ws-events.ts: `text` is required
// and non-empty (1..140 code points, enforced at the REST write boundary and the
// DB CHECK); `emoji` is optional (omitted when empty so it round-trips to "no
// emoji" on the client, where the zod field is `.optional()`).
//
// A nil *CustomTextPayload at a prepared delivery boundary means CLEAR — the
// user turned custom text off or wrote an empty string — emitted as a
// rich_presence_clear frame, never an update with empty text.
type CustomTextPayload struct {
	Emoji string `json:"emoji,omitempty"`
	Text  string `json:"text"`
}

// DeliverCustomText synchronously applies a prepared Custom Status delivery
// plan and acknowledges only after every connected target has been enqueued or
// disconnected. The caller already owns presencehistory.Service.WithSender;
// this adapter performs no database work and never reacquires that gate.
func (h *Hub) DeliverCustomText(
	ctx context.Context,
	plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	if err := ctx.Err(); err != nil {
		return presencehistory.DeliveryAck{}, err
	}
	if plan.OperationID == uuid.Nil {
		return presencehistory.DeliveryAck{}, fmt.Errorf(
			"%w: operation ID is required",
			ErrCustomTextDeliveryPlan,
		)
	}

	disconnected := make(map[*Client]bool)
	if err := h.deliverCustomTextPlan(ctx, plan, disconnected); err != nil {
		return presencehistory.DeliveryAck{}, err
	}
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

func (h *Hub) deliverCustomTextPlan(
	ctx context.Context,
	plan presencehistory.DeliveryPlan,
	disconnected map[*Client]bool,
) error {
	switch plan.Mode {
	case presencehistory.DeliveryExactDelta:
		return h.deliverExactCustomText(ctx, plan, disconnected)
	case presencehistory.DeliveryConservativeReset:
		return h.deliverConservativeCustomText(ctx, plan, disconnected)
	default:
		return fmt.Errorf(
			"%w: unsupported mode",
			ErrCustomTextDeliveryPlan,
		)
	}
}

func (h *Hub) deliverExactCustomText(
	ctx context.Context,
	plan presencehistory.DeliveryPlan,
	disconnected map[*Client]bool,
) error {
	if plan.SenderID == uuid.Nil {
		return fmt.Errorf("%w: exact delivery requires sender", ErrCustomTextDeliveryPlan)
	}
	if customTextRecipientCount(plan.UpdateRecipients) > 0 && plan.Payload == nil {
		return fmt.Errorf("%w: update recipients require payload", ErrCustomTextDeliveryPlan)
	}

	var clearData []byte
	if customTextRecipientCount(plan.ClearRecipients) > 0 {
		var err error
		clearData, err = h.marshalCustomTextDeliveryFrame(plan.SenderID, nil)
		if err != nil {
			return err
		}
	}

	var updateData []byte
	if customTextRecipientCount(plan.UpdateRecipients) > 0 {
		payload := &CustomTextPayload{
			Text:  plan.Payload.Text,
			Emoji: plan.Payload.Emoji,
		}
		var err error
		updateData, err = h.marshalCustomTextDeliveryFrame(plan.SenderID, payload)
		if err != nil {
			return err
		}
	}

	if len(clearData) > 0 {
		if err := h.deliverCustomTextToUsers(ctx, plan.ClearRecipients, clearData, disconnected); err != nil {
			return err
		}
	}
	if len(updateData) > 0 {
		if err := h.deliverCustomTextToUsers(ctx, plan.UpdateRecipients, updateData, disconnected); err != nil {
			return err
		}
	}
	return h.deliverCustomTextOverrideMetadata(ctx, plan, disconnected)
}

func (h *Hub) deliverConservativeCustomText(
	ctx context.Context,
	plan presencehistory.DeliveryPlan,
	disconnected map[*Client]bool,
) error {
	if plan.ClearRecipients == nil && plan.UpdateRecipients == nil {
		return h.disconnectAllCustomTextClients(ctx)
	}
	recipients := customTextRecipientUnion(plan.ClearRecipients, plan.UpdateRecipients)
	if plan.SenderID == uuid.Nil && len(recipients) > 0 {
		return fmt.Errorf("%w: prepared recipients require sender", ErrCustomTextDeliveryPlan)
	}
	if plan.SenderID != uuid.Nil {
		recipients[plan.SenderID] = true
	}
	clearData, err := h.marshalCustomTextDeliveryFrame(plan.SenderID, nil)
	if err != nil {
		return err
	}
	if err := h.deliverCustomTextToUsers(ctx, recipients, clearData, disconnected); err != nil {
		return err
	}
	return h.deliverCustomTextOverrideMetadata(ctx, plan, disconnected)
}

func (h *Hub) deliverCustomTextOverrideMetadata(
	ctx context.Context,
	plan presencehistory.DeliveryPlan,
	disconnected map[*Client]bool,
) error {
	if plan.OverrideVersion == nil {
		return nil
	}
	data, err := json.Marshal(OutgoingMessage{
		Type: "presence_overrides_updated",
		Data: map[string]interface{}{
			"category": customTextCategory,
			"version":  *plan.OverrideVersion,
		},
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCustomTextDeliveryMarshal, err)
	}
	return h.deliverCustomTextToUsers(
		ctx,
		map[uuid.UUID]bool{plan.SenderID: true},
		data,
		disconnected,
	)
}

func (h *Hub) marshalCustomTextDeliveryFrame(
	senderID uuid.UUID,
	payload *CustomTextPayload,
) ([]byte, error) {
	marshal := h.customTextFrameMarshaler
	if marshal == nil {
		marshal = marshalCustomTextFrame
	}
	data, err := marshal(senderID, payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCustomTextDeliveryMarshal, err)
	}
	return data, nil
}

func customTextRecipientCount(recipients map[uuid.UUID]bool) int {
	count := 0
	for _, included := range recipients {
		if included {
			count++
		}
	}
	return count
}

func customTextRecipientUnion(recipientSets ...map[uuid.UUID]bool) map[uuid.UUID]bool {
	union := make(map[uuid.UUID]bool)
	for _, recipients := range recipientSets {
		for userID, included := range recipients {
			if included {
				union[userID] = true
			}
		}
	}
	return union
}

func (h *Hub) deliverCustomTextToUsers(
	ctx context.Context,
	recipients map[uuid.UUID]bool,
	data []byte,
	disconnected map[*Client]bool,
) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for userID, included := range recipients {
		if !included {
			continue
		}
		if err := h.deliverCustomTextToUser(ctx, userID, data, disconnected); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hub) deliverCustomTextToUser(
	ctx context.Context,
	userID uuid.UUID,
	data []byte,
	disconnected map[*Client]bool,
) error {
	for clientID := range h.userClients[userID] {
		client, connected := h.clients[clientID]
		if !connected || disconnected[client] {
			continue
		}
		wasDisconnected, err := h.deliverCustomTextToClient(ctx, client, data)
		if err != nil {
			return err
		}
		if wasDisconnected {
			disconnected[client] = true
		}
	}
	return nil
}

func (h *Hub) deliverCustomTextToClient(
	ctx context.Context,
	client *Client,
	data []byte,
) (bool, error) {
	if h.customTextDeliveryBeforeEnqueue != nil {
		h.customTextDeliveryBeforeEnqueue()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if h.customTextDeliveryBroadcaster != nil {
		if err := h.customTextDeliveryBroadcaster(ctx, client, data); err != nil {
			return false, fmt.Errorf("%w: %w", ErrCustomTextDeliveryBroadcaster, err)
		}
		return false, nil
	}
	select {
	case client.Send <- data:
		return false, nil
	default:
	}
	if err := h.disconnectCustomTextClient(client); err != nil {
		return false, err
	}
	return true, nil
}

func (h *Hub) disconnectCustomTextClient(client *Client) error {
	disconnect := h.customTextClientDisconnect
	if disconnect != nil {
		if err := disconnect(client); err != nil {
			return fmt.Errorf("%w: %w", ErrCustomTextDeliveryBroadcaster, err)
		}
		return nil
	}
	if client.Conn == nil {
		return nil
	}
	if err := client.Conn.Close(); err != nil {
		return fmt.Errorf("%w: %w", ErrCustomTextDeliveryBroadcaster, err)
	}
	return nil
}

func (h *Hub) disconnectAllCustomTextClients(ctx context.Context) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, client := range h.clients {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := h.disconnectCustomTextClient(client); err != nil {
			return err
		}
	}
	return nil
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
	ctx, cancel := context.WithTimeout(context.Background(), customTextResetAudienceTimeout)
	defer cancel()
	audience, err := presence.ComputePresenceAudience(ctx, h.db, senderID)
	if err != nil {
		log.Printf("[hub] reset custom-text audience computation failed; suppressing clear: %T", err)
		return
	}
	data, err := marshalCustomTextFrame(senderID, nil)
	if err != nil {
		log.Printf("[hub] failed to marshal reset custom-text clear: %T", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	h.sendPrivacyClearToUsers(audience, data)
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
// Called from the client's tracked async registration lifecycle, never the Hub
// Run goroutine. Each candidate's final authorization read + enqueue holds the
// same concrete presencehistory.Service sender gate as writers, so a stale
// snapshot update cannot follow a newer committed delivery.
func (h *Hub) sendCustomTextSnapshot(ctx context.Context, client *Client) {
	if h.db == nil || h.presenceHistoryService == nil {
		return // DB-free unit hub: nothing to snapshot, fail-safe
	}

	candidates, err := h.customTextCandidates(ctx)
	if err != nil {
		h.logCustomTextSnapshotError(ctx, "candidate query", err)
		return
	}
	if h.customTextSnapshotAfterCandidates != nil {
		h.customTextSnapshotAfterCandidates()
	}

	for _, senderID := range candidates {
		if !h.sendCustomTextSnapshotForSender(ctx, client, senderID) {
			return
		}
	}
}

func (h *Hub) sendCustomTextSnapshotForSender(
	ctx context.Context,
	client *Client,
	senderID uuid.UUID,
) bool {
	if ctx.Err() != nil {
		return false
	}
	if senderID == client.UserID {
		return true // self is delivered via acknowledged writer self-sync, not the snapshot of others
	}
	err := h.presenceHistoryService.WithSender(ctx, senderID, func() error {
		return h.sendCustomTextSnapshotCandidate(ctx, client, senderID)
	})
	if err == nil {
		return true
	}
	h.logCustomTextSnapshotError(ctx, "candidate", err)
	return ctx.Err() == nil
}

func (h *Hub) logCustomTextSnapshotError(ctx context.Context, operation string, err error) {
	if ctx.Err() == nil {
		// Fail closed: a candidate that cannot be read or authorized is omitted.
		log.Printf("[hub] custom-text snapshot %s failed: %T", operation, err)
	}
}

func (h *Hub) sendCustomTextSnapshotCandidate(
	ctx context.Context,
	client *Client,
	senderID uuid.UUID,
) (returnErr error) {
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return fmt.Errorf("begin Custom Status snapshot transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	defer mergeCustomTextSnapshotRollback(tx, &returnErr)

	payload, authorized, err := h.readCustomTextSnapshotCandidate(ctx, tx, client.UserID, senderID)
	if err != nil {
		return err
	}
	if !authorized {
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Custom Status snapshot transaction: %w", err)
	}
	return h.enqueueCustomTextSnapshot(ctx, client, senderID, payload)
}

func mergeCustomTextSnapshotRollback(tx *sql.Tx, returnErr *error) {
	rollbackErr := tx.Rollback()
	if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		*returnErr = errors.Join(
			*returnErr,
			fmt.Errorf("rollback Custom Status snapshot transaction: %w", rollbackErr),
		)
	}
}

func (h *Hub) readCustomTextSnapshotCandidate(
	ctx context.Context,
	tx *sql.Tx,
	viewerID uuid.UUID,
	senderID uuid.UUID,
) (*CustomTextPayload, bool, error) {
	var tier int
	var text, emoji sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT settings.custom_text_tier,
		       settings.custom_text,
		       settings.custom_text_emoji
		FROM user_presence_settings AS settings
		WHERE settings.user_id = $1
		  AND NOT EXISTS (
		      SELECT 1
		      FROM presence_settings_pending_operations AS pending
		      WHERE pending.user_id = settings.user_id
		  )
	`, senderID).Scan(&tier, &text, &emoji)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read Custom Status snapshot state: %w", err)
	}
	if h.customTextSnapshotAfterStateRead != nil {
		h.customTextSnapshotAfterStateRead(senderID, viewerID)
	}
	if tier <= 0 || !text.Valid || text.String == "" {
		return nil, false, nil
	}

	audience, audErr := presence.ComputeCustomTextAudience(ctx, tx, senderID)
	if audErr != nil {
		return nil, false, fmt.Errorf("authorize Custom Status snapshot: %w", audErr)
	}
	if !audience[viewerID] {
		return nil, false, nil // viewer is NOT in this user's custom-text audience — exclude (privacy lock)
	}

	payload := &CustomTextPayload{Text: text.String}
	if emoji.Valid {
		payload.Emoji = emoji.String
	}
	return payload, true, nil
}

func (h *Hub) enqueueCustomTextSnapshot(
	ctx context.Context,
	client *Client,
	senderID uuid.UUID,
	payload *CustomTextPayload,
) error {
	marshal := h.customTextFrameMarshaler
	if marshal == nil {
		marshal = marshalCustomTextFrame
	}
	data, mErr := marshal(senderID, payload)
	if mErr != nil {
		return fmt.Errorf("marshal Custom Status snapshot frame: %w", mErr)
	}
	if h.customTextSnapshotBeforeEnqueue != nil {
		h.customTextSnapshotBeforeEnqueue(senderID, client.UserID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case client.Send <- data:
	default:
	}
	return nil
}

// customTextCandidates returns every user with custom text set AND tier > 0 —
// the bounded sender-ID shortlist whose status MIGHT be visible to a connecting
// viewer. The current tier, text, and emoji are deliberately re-read inside the
// per-sender delivery coordinator before authorization and enqueue; the outer
// query never captures a payload that could become stale while waiting.
func (h *Hub) customTextCandidates(
	ctx context.Context,
) (out []uuid.UUID, returnErr error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT settings.user_id
		FROM user_presence_settings AS settings
		WHERE settings.custom_text_tier > 0
		  AND settings.custom_text IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1
		      FROM presence_settings_pending_operations AS pending
		      WHERE pending.user_id = settings.user_id
		  )
	`)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close Custom Status snapshot candidates: %w", closeErr))
		}
	}()

	for rows.Next() {
		var senderID uuid.UUID
		if err := rows.Scan(&senderID); err != nil {
			return nil, err
		}
		out = append(out, senderID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
