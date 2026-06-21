package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/presence"
)

// customTextCategory is the rich-presence category string for custom text. It
// must match RichPresenceCategorySchema's `custom_text` member in
// client/desktop/src/renderer/types/ws-events.ts (#1233 Task B1/B2).
const customTextCategory = "custom_text"

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
// Concurrency: invoked on an HTTP handler goroutine (a `go` from
// UpdatePresenceSettings), not the hub Run goroutine, so the hub-map reads are
// guarded by h.mu.RLock — the DB audience queries run first, outside the lock.
func (h *Hub) BroadcastCustomText(senderID uuid.UUID, oldTier int, payload *CustomTextPayload) {
	if h.db == nil {
		// No DB (DB-free unit hub): fail closed and skip. Production always has a
		// DB (NewHub requires it).
		return
	}

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
	h.sendToUsers(excluded, clearData)
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
// Called from sendPresenceSnapshot, which already runs on the hub Run goroutine
// during handleRegister, so no additional locking is taken here (consistent with
// the rest of sendPresenceSnapshot).
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

	for _, cand := range candidates {
		if cand.userID == client.UserID {
			continue // self is delivered via the live BroadcastCustomText self-sync, not the snapshot of others
		}
		audience, audErr := presence.ComputeCustomTextAudience(ctx, h.db, cand.userID)
		if audErr != nil {
			// Fail closed for this candidate only; never optimistically include.
			log.Printf("[hub] custom-text snapshot audience failed for sender %s viewer %s: %v",
				sanitizeLogValue(cand.userID.String()), sanitizeLogValue(client.UserID.String()), audErr)
			continue
		}
		if !audience[client.UserID] {
			continue // viewer is NOT in this user's custom-text audience — exclude (privacy lock)
		}

		payload := &CustomTextPayload{Text: cand.text}
		if cand.emoji.Valid {
			payload.Emoji = cand.emoji.String
		}
		data, mErr := marshalCustomTextFrame(cand.userID, payload)
		if mErr != nil {
			log.Printf("[hub] custom-text snapshot marshal failed for sender %s: %v", sanitizeLogValue(cand.userID.String()), mErr)
			continue
		}
		select {
		case client.Send <- data:
		default:
		}
	}
}

// customTextCandidate is one row of the inverse-audience candidate set: a user
// who has custom text set and a tier above Off.
type customTextCandidate struct {
	userID uuid.UUID
	text   string
	emoji  sql.NullString
}

// customTextCandidates returns every user with custom text set AND tier > 0 —
// the bounded set of senders whose status MIGHT be visible to a connecting
// viewer. Per-candidate audience filtering (honoring each candidate's tier)
// happens in sendCustomTextSnapshot; this query only narrows the universe so the
// snapshot is O(candidates), not O(all users). custom_text is non-PII to the
// QUERY layer (it's never logged), but it IS the audience-gated value, so the row
// is only ever emitted after the per-candidate audience check passes.
func (h *Hub) customTextCandidates(ctx context.Context) ([]customTextCandidate, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT user_id, custom_text, custom_text_emoji
		FROM user_presence_settings
		WHERE custom_text_tier > 0 AND custom_text IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []customTextCandidate
	for rows.Next() {
		var c customTextCandidate
		if err := rows.Scan(&c.userID, &c.text, &c.emoji); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
