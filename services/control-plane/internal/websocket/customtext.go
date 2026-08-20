package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"syscall"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
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
	// ErrPrivacyCriticalDeliveryBroadcaster identifies queue or disconnect failures.
	ErrPrivacyCriticalDeliveryBroadcaster = errors.New("privacy-critical delivery broadcaster failed")
	// ErrCustomTextDeliveryBroadcaster preserves the original error identity for callers.
	ErrCustomTextDeliveryBroadcaster = ErrPrivacyCriticalDeliveryBroadcaster
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
	if privacyCriticalRecipientCount(plan.UpdateRecipients) > 0 && plan.Payload == nil {
		return fmt.Errorf("%w: update recipients require payload", ErrCustomTextDeliveryPlan)
	}

	var clearData []byte
	if privacyCriticalRecipientCount(plan.ClearRecipients) > 0 {
		var err error
		clearData, err = h.marshalCustomTextDeliveryFrame(plan.SenderID, nil)
		if err != nil {
			return err
		}
	}

	var updateData []byte
	if privacyCriticalRecipientCount(plan.UpdateRecipients) > 0 {
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
		if err := h.deliverPrivacyCriticalToUsers(ctx, plan.ClearRecipients, clearData, disconnected); err != nil {
			return err
		}
	}
	if len(updateData) > 0 {
		if err := h.deliverPrivacyCriticalToUsers(ctx, plan.UpdateRecipients, updateData, disconnected); err != nil {
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
		return h.disconnectAllPrivacyCriticalClients(ctx)
	}
	recipients := privacyCriticalRecipientUnion(plan.ClearRecipients, plan.UpdateRecipients)
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
	if err := h.deliverPrivacyCriticalToUsers(ctx, recipients, clearData, disconnected); err != nil {
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
	return h.deliverPrivacyCriticalToUsers(
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

func privacyCriticalRecipientCount(recipients map[uuid.UUID]bool) int {
	count := 0
	for _, included := range recipients {
		if included {
			count++
		}
	}
	return count
}

func privacyCriticalRecipientUnion(recipientSets ...map[uuid.UUID]bool) map[uuid.UUID]bool {
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

func (h *Hub) deliverPrivacyCriticalToUsers(
	ctx context.Context,
	recipients map[uuid.UUID]bool,
	data []byte,
	disconnected map[*Client]bool,
) error {
	return h.deliverPrivacyCriticalToUsersWhere(ctx, recipients, data, disconnected, nil)
}

func (h *Hub) deliverPrivacyCriticalToUsersWhere(
	ctx context.Context,
	recipients map[uuid.UUID]bool,
	data []byte,
	disconnected map[*Client]bool,
	includeClient func(*Client) bool,
) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for userID, included := range recipients {
		if !included {
			continue
		}
		if err := h.deliverPrivacyCriticalToUser(
			ctx, userID, data, disconnected, includeClient,
		); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hub) deliverPrivacyCriticalToUser(
	ctx context.Context,
	userID uuid.UUID,
	data []byte,
	disconnected map[*Client]bool,
	includeClient func(*Client) bool,
) error {
	for clientID := range h.userClients[userID] {
		client, connected := h.clients[clientID]
		if !connected || disconnected[client] ||
			(includeClient != nil && !includeClient(client)) {
			continue
		}
		wasDisconnected, err := h.deliverPrivacyCriticalToClient(ctx, client, data)
		if err != nil {
			return err
		}
		if wasDisconnected {
			disconnected[client] = true
		}
	}
	return nil
}

func (h *Hub) deliverPrivacyCriticalToClient(
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
	switch client.bufferBootstrapLive(data) {
	case bootstrapBufferEnqueued:
		return false, nil
	case bootstrapBufferOverflow:
		if err := h.disconnectPrivacyCriticalClient(client); err != nil {
			return false, err
		}
		return true, nil
	case bootstrapBufferCanceled:
		return true, nil
	case bootstrapBufferInactive:
		// The replacement has completed; use normal immediate delivery.
	}
	if h.customTextDeliveryBroadcaster != nil {
		if err := h.customTextDeliveryBroadcaster(ctx, client, data); err != nil {
			return false, fmt.Errorf("%w: %w", ErrPrivacyCriticalDeliveryBroadcaster, err)
		}
		return false, nil
	}
	if client.enqueueOutbound(data) {
		return false, nil
	}
	if err := h.disconnectPrivacyCriticalClient(client); err != nil {
		return false, err
	}
	return true, nil
}

// alreadyDisconnected reports whether a close error means the socket was
// already gone rather than that the disconnect failed.
//
// The distinction is load-bearing. A disconnect's postcondition is "this client
// is not receiving any more frames", and an already-closed socket satisfies it
// — the recipient WAS reached. Reporting it as a failure is what let a benign
// double-close escalate into a whole-node teardown: the graphpresence
// reconciler treats any non-nil error from DisconnectRichPresenceClients as
// "targeted disconnect failed" and escalates to
// DisconnectAllRichPresenceClients (PR #2738 review, @security-reviewer).
//
// writePump closes Conn on any write error and never unregisters, so a client
// stays in h.clients with a closed Conn until readPump's deadline expires —
// the window is seconds wide, not microseconds. And graphpresence dispatch
// disconnects overlapping sets back to back (leg.captured, then p.viewers), so
// a shared-server counterpart is closed twice on the ordinary path.
func alreadyDisconnected(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, websocket.ErrCloseSent)
}

func (h *Hub) disconnectPrivacyCriticalClient(client *Client) error {
	disconnect := h.customTextClientDisconnect
	if disconnect != nil {
		if err := disconnect(client); err != nil {
			if alreadyDisconnected(err) {
				return nil
			}
			return fmt.Errorf("%w: %w", ErrPrivacyCriticalDeliveryBroadcaster, err)
		}
		return nil
	}
	if client.Conn == nil {
		return nil
	}
	if err := client.Conn.Close(); err != nil {
		if alreadyDisconnected(err) {
			return nil
		}
		return fmt.Errorf("%w: %w", ErrPrivacyCriticalDeliveryBroadcaster, err)
	}
	return nil
}

func (h *Hub) disconnectAllPrivacyCriticalClients(ctx context.Context) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var disconnectErr error
	for _, client := range h.clients {
		if err := h.disconnectPrivacyCriticalClient(client); err != nil {
			disconnectErr = errors.Join(disconnectErr, err)
		}
	}
	return errors.Join(disconnectErr, ctx.Err())
}

// ClearErasedSenderCustomText broadcasts a Custom Status clear for ONE sender to
// every local Rich Presence client, without computing an audience and without
// disconnecting anyone.
//
// It exists for account erasure, where neither of the other two options works.
// ClearCustomTextForPresenceAudience cannot: it recomputes the audience from the
// database, and after erasure the sender's rows have cascaded away, so it would
// resolve an empty set and clear nobody. A fleet-wide DISCONNECT can, but makes
// the triggering message a denial-of-service primitive — the red-team PoCs on
// PR #2840 proved a forged publish carrying any random UUID, an unbounded replay
// of a genuine one, and a lookup error all reached that sink.
//
// A clear frame makes the action PROPORTIONAL to the claim. A client that never
// held this sender's status ignores the frame, so a forged or replayed message
// for an arbitrary id is inert rather than destructive, and the honest cost is
// one small frame per connected client instead of a fleet-wide reconnect storm.
//
// Delivery is best-effort per client and DROPS rather than disconnecting —
// deliberately unlike sendPrivacyClearToUsers, which closes on a full queue.
// There the frame IS the authorization revocation, so a client that misses it
// must reconnect for a fresh authorized snapshot. Here it is not: this frame
// only retracts a display string for a sender who no longer exists.
//
// The cost of a dropped frame, stated precisely because the obvious bound is
// wrong. Custom Status carries NO TTL and is not republished on a heartbeat
// (delete_account.go's publishErasureCleared says so explicitly), and
// ActivityStateTTL bounds activity categories, not this. A viewer that already
// held the erased sender's status keeps rendering it until its next
// presence_snapshot, which fires only on reconnect or sign-out. That can be an
// entire session — not 90 seconds. It is in-memory only (the renderer store has
// no persist middleware) and the erased user has cascaded out of the viewer's
// friend and member lists, so it is a stale string rather than live disclosure.
//
// Closing on overflow instead made the fan-out a denial-of-service primitive:
// the bus is unauthenticated and the fan-out unconditional, so 257 forged
// messages disconnected every Rich Presence client on the replica and left each
// one permanently in the disconnect branch.
//
// What dropping does NOT buy, so no later reader assumes it: a forged clear is
// not inert. The fan-out is still unconditional, so a burst still costs one
// frame per connected client, and an attacker who saturates a viewer's queue can
// make a GENUINE clear be dropped, leaving the erased status resident. Those are
// accepted here and closed by stage B1's rate limit at the NATS subscriber
// dispatch boundary, which stops a burst before it reaches any client. B2 is not
// safe to sit on indefinitely without B1. #2854 stage B2.
func (h *Hub) ClearErasedSenderCustomText(senderID uuid.UUID) {
	// Fail closed on a nil receiver rather than panicking, matching the
	// TopologyRail methods: a hub-less replica has no local clients to clear, and
	// a panic inside a NATS callback would take the subscriber down.
	if h == nil {
		return
	}
	data, err := marshalCustomTextFrame(senderID, nil)
	if err != nil {
		log.Printf("[hub] failed to marshal erased-sender custom-text clear: %T", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	dropped := 0
	for _, client := range h.clients {
		if !activityRichPresenceClient(client) {
			continue
		}
		// enqueueOutboundBootstrapSafe never closes the client and never touches
		// the reconnect-replacement buffer.
		//
		// Plain enqueueOutbound is NOT used here. Its first act is
		// bufferBootstrapLive, so for a client inside its replacement window a
		// forged burst latches bootstrapFailed (or inflates completeClientBootstrap's
		// capacity preflight) and the bootstrap path disconnects the client on its
		// own goroutine — the same fleet-wide disconnect, moved off the fan-out and
		// out of sight of a fan-out-only assertion. That variant is CHEAPER than the
		// one this stage removed (256 vs 257), independent of Send capacity, and
		// self-sustaining, because each disconnect returns the victim to a
		// replacement window. Proven by PoC before it shipped.
		//
		// enqueuePrivacyCritical is NOT used here either — its close-on-overflow arm
		// is correct only for sendPrivacyClearToUsers, where the frame carries an
		// authorization revocation.
		if !client.enqueueOutboundBootstrapSafe(data) {
			dropped++
		}
	}
	if dropped > 0 {
		// ONE aggregate line per fan-out, never one per client: per-client
		// logging would hand a forged flood O(clients) writes per message,
		// re-creating in the log the amplification just removed from the socket.
		//
		// Worded "erasure-clear", not "erased-sender clear", deliberately. The
		// AST guard in customtext_log_emissions_test.go rejects a sensitive field
		// label within 32 characters of a format verb, and "sender" next to %d
		// reads as a sender identifier even though this operand is an aggregate
		// count. Keep any future edit free of those labels rather than widening
		// the guard.
		log.Printf("[hub] erasure-clear frames dropped for %d client(s); resource_limit", dropped)
	}
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
	switch client.bufferBootstrapLive(data) {
	case bootstrapBufferEnqueued:
		return privacyCriticalEnqueueSucceeded
	case bootstrapBufferOverflow:
		closePrivacyCriticalClient(client)
		return privacyCriticalEnqueueDisconnectRequired
	case bootstrapBufferCanceled:
		return privacyCriticalEnqueueDisconnectRequired
	case bootstrapBufferInactive:
		// The replacement has completed; use normal immediate delivery.
	}
	if client.enqueueOutbound(data) {
		return privacyCriticalEnqueueSucceeded
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
// We resolve this with one viewer-correlated, bounded shortlist, then re-read
// each candidate under its sender gate and run a single-viewer authorization
// query. Final authorization returns one boolean rather than materializing U's
// potentially unbounded audience. A candidate read/authorization failure aborts
// the unpublished replacement so reconnect can retry; it is never optimistically
// included.
//
// Called from the client's tracked async registration lifecycle, never the Hub
// Run goroutine. The read-only transaction commits before its frame is appended
// while the local sender gate remains held. A cross-process change that commits
// after that read is delivered into the live buffer and therefore replays after
// this committed snapshot frame.
func (h *Hub) sendCustomTextSnapshot(ctx context.Context, client *Client) error {
	if h.db == nil || h.presenceHistoryService == nil {
		return nil // DB-free unit hub: nothing to snapshot, fail-safe
	}

	candidates, err := h.customTextCandidates(ctx, client.UserID)
	if err != nil {
		h.logCustomTextSnapshotError(ctx, "candidate query", err)
		return err
	}
	if h.customTextSnapshotAfterCandidates != nil {
		h.customTextSnapshotAfterCandidates()
	}

	for _, senderID := range candidates {
		if err := h.sendCustomTextSnapshotForSender(ctx, client, senderID); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hub) sendCustomTextSnapshotForSender(
	ctx context.Context,
	client *Client,
	senderID uuid.UUID,
) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if senderID == client.UserID {
		return nil // self is delivered via acknowledged writer self-sync, not the snapshot of others
	}
	err := h.presenceHistoryService.WithSender(ctx, senderID, func() error {
		return h.sendCustomTextSnapshotCandidate(ctx, client, senderID)
	})
	if err == nil {
		return nil
	}
	h.logCustomTextSnapshotError(ctx, "candidate", err)
	return err
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
	var masterEnabled bool
	var tier int
	var text, emoji sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT settings.master_enabled,
		       settings.custom_text_tier,
		       settings.custom_text,
		       settings.custom_text_emoji
		FROM user_presence_settings AS settings
		WHERE settings.user_id = $1
		  AND NOT EXISTS (
		      SELECT 1
		      FROM presence_settings_pending_operations AS pending
		      WHERE pending.user_id = settings.user_id
		  )
		FOR SHARE OF settings
	`, senderID).Scan(&masterEnabled, &tier, &text, &emoji)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read Custom Status snapshot state: %w", err)
	}
	if !masterEnabled {
		return nil, false, nil
	}
	if h.customTextSnapshotAfterStateRead != nil {
		h.customTextSnapshotAfterStateRead(senderID, viewerID)
	}
	if (tier != 1 && tier != 2) || !text.Valid || text.String == "" || senderID == viewerID {
		return nil, false, nil
	}

	var authorized bool
	audErr := tx.QueryRowContext(ctx, `
		SELECT NOT EXISTS (
		         SELECT 1
		         FROM user_presence_overrides excluded
		         WHERE excluded.sender_id = $1
		           AND excluded.category = 'custom_text'
		           AND excluded.target_user_id = $2
		       )
		       AND (
		         EXISTS (
		           SELECT 1
		           FROM friendships direct
		           WHERE direct.status = 'accepted'
		             AND (
		               (direct.requester_id = $1 AND direct.addressee_id = $2)
		               OR (direct.addressee_id = $1 AND direct.requester_id = $2)
		             )
		         )
		         OR EXISTS (
		           SELECT 1
		           FROM privacy_settings sender_privacy
		           JOIN friendships sender_friend
		             ON sender_friend.status = 'accepted'
		            AND (sender_friend.requester_id = $1 OR sender_friend.addressee_id = $1)
		           WHERE sender_privacy.user_id = $1
		             AND sender_privacy.dm_friends_of_friends
		             AND EXISTS (
		               SELECT 1
		               FROM friendships friend_viewer
		               WHERE friend_viewer.status = 'accepted'
		                 AND (
		                   (friend_viewer.requester_id = CASE
		                      WHEN sender_friend.requester_id = $1
		                      THEN sender_friend.addressee_id
		                      ELSE sender_friend.requester_id END
		                    AND friend_viewer.addressee_id = $2)
		                   OR
		                   (friend_viewer.addressee_id = CASE
		                      WHEN sender_friend.requester_id = $1
		                      THEN sender_friend.addressee_id
		                      ELSE sender_friend.requester_id END
		                    AND friend_viewer.requester_id = $2)
		                 )
		             )
		         )
		         OR ($3 = 2 AND EXISTS (
		           SELECT 1
		           FROM server_members sender_member
		           JOIN server_members viewer_member
		             ON viewer_member.server_id = sender_member.server_id
		            AND viewer_member.user_id = $2
		           WHERE sender_member.user_id = $1
		         ))
		       )
	`, senderID, viewerID, tier).Scan(&authorized)
	if audErr != nil {
		return nil, false, fmt.Errorf("authorize Custom Status snapshot: %w", audErr)
	}
	if !authorized {
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
	if err := client.appendBootstrapReplay(data); err == nil {
		return nil
	} else if !errors.Is(err, errClientBootstrapInactive) {
		if errors.Is(err, errClientBootstrapOverflow) {
			if disconnectErr := h.disconnectPrivacyCriticalClient(client); disconnectErr != nil {
				return errors.Join(err, disconnectErr)
			}
		}
		return err
	}
	if client.enqueueOutbound(data) {
		return nil
	}
	if err := h.disconnectPrivacyCriticalClient(client); err != nil {
		return errors.Join(errClientBootstrapOverflow, err)
	}
	return errClientBootstrapOverflow
}

// customTextCandidates returns the bounded inverse-audience shortlist for one
// connecting viewer. The current payload and authorization are deliberately
// re-read inside the per-sender delivery coordinator before enqueue.
func (h *Hub) customTextCandidates(
	ctx context.Context,
	viewerID uuid.UUID,
) (out []uuid.UUID, returnErr error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT settings.user_id
		FROM user_presence_settings AS settings
		WHERE settings.user_id <> $1
		  AND settings.master_enabled
		  AND settings.custom_text_tier > 0
		  AND settings.custom_text IS NOT NULL
		  AND settings.custom_text <> ''
		  AND NOT EXISTS (
		      SELECT 1
		      FROM presence_settings_pending_operations AS pending
		      WHERE pending.user_id = settings.user_id
		  )
		  AND NOT EXISTS (
		      SELECT 1
		      FROM user_presence_overrides AS excluded
		      WHERE excluded.sender_id = settings.user_id
		        AND excluded.category = 'custom_text'
		        AND excluded.target_user_id = $1
		  )
		  AND (
		      EXISTS (
		          SELECT 1 FROM friendships direct
		          WHERE direct.status = 'accepted'
		            AND (
		                (direct.requester_id = settings.user_id AND direct.addressee_id = $1)
		                OR
		                (direct.addressee_id = settings.user_id AND direct.requester_id = $1)
		            )
		      )
		      OR EXISTS (
		          SELECT 1
		          FROM privacy_settings sender_privacy
		          JOIN friendships sender_friend
		            ON sender_friend.status = 'accepted'
		           AND (
		               sender_friend.requester_id = settings.user_id
		               OR sender_friend.addressee_id = settings.user_id
		           )
		          WHERE sender_privacy.user_id = settings.user_id
		            AND sender_privacy.dm_friends_of_friends
		            AND EXISTS (
		                SELECT 1
		                FROM friendships friend_viewer
		                WHERE friend_viewer.status = 'accepted'
		                  AND (
		                      (
		                          friend_viewer.requester_id = CASE
		                              WHEN sender_friend.requester_id = settings.user_id
		                              THEN sender_friend.addressee_id
		                              ELSE sender_friend.requester_id
		                          END
		                          AND friend_viewer.addressee_id = $1
		                      )
		                      OR (
		                          friend_viewer.addressee_id = CASE
		                              WHEN sender_friend.requester_id = settings.user_id
		                              THEN sender_friend.addressee_id
		                              ELSE sender_friend.requester_id
		                          END
		                          AND friend_viewer.requester_id = $1
		                      )
		                  )
		            )
		      )
		      OR (
		          settings.custom_text_tier = 2
		          AND EXISTS (
		              SELECT 1
		              FROM server_members sender_member
		              JOIN server_members viewer_member
		                ON viewer_member.server_id = sender_member.server_id
		               AND viewer_member.user_id = $1
		              WHERE sender_member.user_id = settings.user_id
		          )
		      )
		  )
		ORDER BY settings.user_id
		LIMIT $2
	`, viewerID, clientBootstrapBufferedFrameLimit+1)
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
	if len(out) > clientBootstrapBufferedFrameLimit {
		return nil, errClientBootstrapOverflow
	}
	return out, nil
}
