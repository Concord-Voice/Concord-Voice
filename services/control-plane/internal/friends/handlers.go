// Package friends provides handlers for friend relationship management.
package friends

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/invites"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	errMsgFailedClaimFriendCode = "Failed to claim friend code"
	// errMsgInvalidFriendCode is returned for BOTH the pre-transaction preview
	// miss and the locked read's miss. Deliberately identical: a caller must not
	// be able to tell the two apart, or the difference times how far a guessed
	// code got through the claim.
	errMsgInvalidFriendCode       = "Invalid friend code"
	errMsgFailedSendFriendRequest = "Failed to send friend request"
	errMsgFailedBlockUser         = "Failed to block user"
	errMsgFailedAcceptRequest     = "Failed to accept friend request"
	errMsgFailedRemoveFriend      = "Failed to remove friend"
)

// Handler handles friend-related requests.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
	hub UserBroadcaster
	// graphPresence reconciles already-delivered Rich Presence with the friend
	// graph these handlers mutate (#2446). Nil until the router wires it, and a
	// nil capture leaves every handler behaving exactly as it did before.
	graphPresence presencecapture.GraphPresenceCapture
}

// UserBroadcaster is the single hub method this package needs. It is an
// interface rather than *websocket.Hub so a test can observe what a handler
// actually put on the wire: several handlers here derive a broadcast field from
// a value computed deep inside a transaction, and with a concrete hub the only
// way to assert on that derivation was to not have one, which asserts nothing.
type UserBroadcaster interface {
	BroadcastToUser(userID uuid.UUID, msg websocket.OutgoingMessage)
}

// NewHandler creates a new friends handler.
//
// The typed-nil normalization is load-bearing, not defensive. Every broadcast
// site below guards on `h.hub == nil`, and a nil *websocket.Hub boxed into an
// interface is NOT nil -- so without this, an unwired caller would pass every
// one of those guards and panic in BroadcastToUser, which dereferences its
// receiver. #2446 hit this exact trap on the TopologyRail interface, where a
// typed-nil service satisfied the boot guard while being unusable.
func NewHandler(db *sql.DB, log *logger.Logger, hub UserBroadcaster) *Handler {
	if concrete, isHub := hub.(*websocket.Hub); isHub && concrete == nil {
		hub = nil
	}
	return &Handler{
		db:  db,
		log: log,
		hub: hub,
	}
}

// SetGraphPresenceCapture wires the #2446 pre-mutation presence capture. A nil
// capture leaves every handler behaving exactly as it did before the hook.
func (h *Handler) SetGraphPresenceCapture(c presencecapture.GraphPresenceCapture) {
	h.graphPresence = c
}

// HasGraphPresenceCapture reports whether the capture was wired. The router's
// boot guard interrogates the HANDLER through this, never the constructed
// reconciler value: graphpresence.New always returns a non-nil pointer, so a
// check on that value is a tautology that still boots with the wiring line
// deleted — the exact fail-open the guard exists to catch.
func (h *Handler) HasGraphPresenceCapture() bool { return h.graphPresence != nil }

// blockCaptureSpec declares the block site's capture. Block is the ONLY #2446
// site using FailConservativeDegrade: refusing a block because the capture read
// failed would let a large friend graph deny a safety affordance, which is a
// worse outcome than a conservative disconnect.
func blockCaptureSpec(blockerID, blockedID string) presencehook.Spec {
	return presencehook.Spec{
		Family:        presencecapture.FamilyBlock,
		Posture:       presencecapture.FailConservativeDegrade,
		PrincipalID:   blockerID,
		CounterpartID: blockedID,
	}
}

// errFriendRequestNotAcceptable and errFriendshipNotFound are IN-CLOSURE
// signals, never a client-visible failure. A hooked handler's work runs inside
// presencehook.WithGatedTx, so the only way to abort a transaction from inside
// it is to return an error; these two say "abort and roll back, but respond
// 404" rather than "something failed".
//
// Returning one is positive PROOF that nothing was written, which is why
// neither site abandons: the captured plan's viewer set contains BOTH
// principals and Abandon closes every local device for them, so abandoning on a
// proven no-change would let any authenticated caller force a full websocket
// teardown of a stranger, repeatably, having mutated nothing.
var (
	errFriendRequestNotAcceptable = errors.New("friend request is no longer acceptable")
	errFriendshipNotFound         = errors.New("accepted friendship not found")
)

// respondPresenceTerminal writes the HTTP shape a hooked transaction's terminal
// error takes. Every call site guards it with `if err != nil`; it is never the
// success path.
//
// No route shape changes: the body keeps each site's existing message and the
// classification goes to the structured log's fixed error_class field. It logs
// the error itself alongside the class because the 500 arm collapses every
// distinct cause (BeginTx failure, capture read failure, savepoint restore
// failure, ErrCaptureBound, a joined discard error, the handler's own write
// error) to error_class "internal" with a fixed body string, and no other sink
// records which one occurred. The wrapped errors these sites produce carry
// UUIDs and driver text, never user-authored content.
//
// presencecapture.ErrCaptureBound landing on that 500 arm is a DECISION, not an
// unmapped gap — §3.6's failure table has no row for it, so this is where the
// answer lives. Its only producer is graphpresence.checkFocalBound, which
// returns it when len(focal) exceeds maxFocalSenders. Its two call sites differ,
// and an earlier version of this comment flattened them: from WithGatedTx it
// returns before any gate or transaction is taken, while from CaptureInTx a
// transaction is already open and what it precedes is the savepoint and the
// write. Neither depends on the declared posture. Either way the write provably
// did not land, which excludes
// the ErrPostCommitDelivery 503 whose whole meaning is that the mutation IS
// durable. And an oversized focal set is a defect in the focal-set derivation
// rather than load, so it does not clear on its own and must not carry a retry
// promise, which excludes the pending 503. The subtest in
// presence_capture_test.go pins that shape; do not re-open it with a new arm.
//
// A 503 for a held marker additionally carries Retry-After, which is a header,
// not a body field. presencehook.Failure.RetryAfterHeader is the only sanctioned
// source for it: it yields a value for the pending terminal alone, so no other
// class can promise a retry cadence for a failure that does not self-resolve.
func (h *Handler) respondPresenceTerminal(c *gin.Context, message string, err error) {
	failure := presencehook.Classify(err)
	if retry, ok := failure.RetryAfterHeader(); ok {
		c.Header("Retry-After", retry)
	}
	// The LOG keeps the site's own message; the BODY does not. A post-commit
	// terminal committed, so telling the client "Failed to X" would invite the
	// duplicate retry the 503 exists to prevent -- see Failure.Body.
	h.log.Error(message, "error_class", failure.Code, "error", err)
	c.JSON(failure.Status, gin.H{"error": failure.Body(message)})
}

// friendResponse represents a friend in API responses.
type friendResponse struct {
	ID          string  `json:"id"`
	UserID      string  `json:"user_id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
	AvatarURL   *string `json:"avatar_url,omitempty"`
	ColorScheme *string `json:"color_scheme,omitempty"`
	CreatedAt   string  `json:"created_at"`
}

// friendRequestResponse represents a friend request in API responses.
type friendRequestResponse struct {
	ID              string  `json:"id"`
	FromUserID      string  `json:"from_user_id"`
	FromUsername    string  `json:"from_username"`
	FromDisplayName *string `json:"from_display_name,omitempty"`
	FromAvatarURL   *string `json:"from_avatar_url,omitempty"`
	ToUserID        string  `json:"to_user_id"`
	ToUsername      string  `json:"to_username"`
	ToDisplayName   *string `json:"to_display_name,omitempty"`
	ToAvatarURL     *string `json:"to_avatar_url,omitempty"`
	Direction       string  `json:"direction"`
	CreatedAt       string  `json:"created_at"`
}

// ListFriends returns the caller's accepted friendships with user details.
// GET /friends
func (h *Handler) ListFriends(c *gin.Context) {
	userID := c.GetString("user_id")

	query := `
		SELECT f.id, u.id, u.username, u.display_name, u.avatar_url, u.color_scheme, f.created_at
		FROM friendships f
		INNER JOIN users u ON u.id = CASE
			WHEN f.requester_id = $1 THEN f.addressee_id
			ELSE f.requester_id
		END
		WHERE (f.requester_id = $1 OR f.addressee_id = $1)
		  AND f.status = 'accepted'
		ORDER BY u.username ASC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query friends", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friends"})
		return
	}
	defer func() { _ = rows.Close() }()

	friends := []friendResponse{}
	for rows.Next() {
		var f friendResponse
		if err := rows.Scan(&f.ID, &f.UserID, &f.Username, &f.DisplayName, &f.AvatarURL, &f.ColorScheme, &f.CreatedAt); err != nil {
			h.log.Error("Failed to scan friend", "error", err)
			continue
		}
		friends = append(friends, f)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating friends", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friends"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"friends": friends})
}

// ListRequests returns pending friend requests (sent and received).
// GET /friends/requests
func (h *Handler) ListRequests(c *gin.Context) {
	userID := c.GetString("user_id")

	query := `
		SELECT f.id,
		       req.id, req.username, req.display_name, req.avatar_url,
		       addr.id, addr.username, addr.display_name, addr.avatar_url,
		       CASE WHEN f.requester_id = $1 THEN 'sent' ELSE 'received' END AS direction,
		       f.created_at
		FROM friendships f
		INNER JOIN users req ON req.id = f.requester_id
		INNER JOIN users addr ON addr.id = f.addressee_id
		WHERE (f.requester_id = $1 OR f.addressee_id = $1)
		  AND f.status = 'pending'
		ORDER BY f.created_at DESC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query friend requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend requests"})
		return
	}
	defer func() { _ = rows.Close() }()

	requests := []friendRequestResponse{}
	for rows.Next() {
		var r friendRequestResponse
		if err := rows.Scan(
			&r.ID,
			&r.FromUserID, &r.FromUsername, &r.FromDisplayName, &r.FromAvatarURL,
			&r.ToUserID, &r.ToUsername, &r.ToDisplayName, &r.ToAvatarURL,
			&r.Direction, &r.CreatedAt,
		); err != nil {
			h.log.Error("Failed to scan friend request", "error", err)
			continue
		}
		requests = append(requests, r)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating friend requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend requests"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"requests": requests})
}

// SendRequestBody represents the request to send a friend request.
type SendRequestBody struct {
	UserID   *string `json:"user_id"`
	Username *string `json:"username"`
}

type resolveResult struct {
	targetUserID string
	status       int
	errMsg       string
}

func (h *Handler) resolveTargetUserID(req SendRequestBody) resolveResult {
	if req.UserID != nil {
		if _, err := uuid.Parse(*req.UserID); err != nil {
			return resolveResult{status: http.StatusBadRequest, errMsg: "Invalid user_id"}
		}
		return resolveResult{targetUserID: *req.UserID}
	}
	if req.Username != nil {
		var targetUserID string
		// LOWER(username): identity is case-insensitive; legacy SSO rows may be
		// stored mixed-case (pre-#1931), so match case-insensitively.
		err := h.db.QueryRow(`SELECT id FROM users WHERE LOWER(username) = $1`, strings.ToLower(strings.TrimSpace(*req.Username))).Scan(&targetUserID)
		if err == sql.ErrNoRows {
			return resolveResult{status: http.StatusNotFound, errMsg: "User not found"}
		}
		if err != nil {
			h.log.Error("Failed to look up user by username", "error", err)
			return resolveResult{status: http.StatusInternalServerError, errMsg: errMsgFailedSendFriendRequest}
		}
		return resolveResult{targetUserID: targetUserID}
	}
	return resolveResult{status: http.StatusBadRequest, errMsg: "user_id or username is required"}
}

func checkExistingFriendship(querier interface {
	QueryRow(string, ...interface{}) *sql.Row
}, userID, targetUserID string) (string, error) {
	var existingStatus string
	err := querier.QueryRow(`
		SELECT status FROM friendships
		WHERE (requester_id = $1 AND addressee_id = $2)
		   OR (requester_id = $2 AND addressee_id = $1)
	`, userID, targetUserID).Scan(&existingStatus)
	return existingStatus, err
}

func friendshipConflictResponse(status string) (int, string) {
	switch status {
	case "accepted":
		return http.StatusConflict, "Already friends"
	case "pending":
		return http.StatusConflict, "Friend request already pending"
	case "blocked":
		return http.StatusForbidden, "Cannot send friend request"
	default:
		return 0, ""
	}
}

func (h *Handler) notifyFriendRequestReceived(targetUserID, friendshipID, userID, createdAt string) {
	if h.hub == nil {
		return
	}
	addresseeUUID, parseErr := uuid.Parse(targetUserID)
	if parseErr != nil {
		return
	}
	var sender userProfile
	if err := h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, userID).
		Scan(&sender.username, &sender.displayName, &sender.avatarURL); err != nil {
		h.log.Error("notifyFriendRequestReceived: failed to load sender profile", "error", err)
	}

	// Addressee (recipient) profile. The client's friend_request_received handler
	// bails without to_user_id/to_username (#981), and because that check is a
	// truthiness test (useWebSocketMessages.ts), an empty to_username is dropped
	// too. If the addressee profile can't be loaded, skip the broadcast rather than
	// emit a payload the client is guaranteed to discard. (to_user_id is the parsed
	// targetUserID and is always present; to_username is the field that can be empty.)
	var addressee userProfile
	if err := h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, targetUserID).
		Scan(&addressee.username, &addressee.displayName, &addressee.avatarURL); err != nil {
		h.log.Error("notifyFriendRequestReceived: failed to load addressee profile", "error", err)
		return
	}

	h.hub.BroadcastToUser(addresseeUUID, websocket.OutgoingMessage{
		Type: "friend_request_received",
		Data: friendRequestReceivedData(friendshipID, userID, sender, targetUserID, addressee, createdAt),
	})
}

// SendRequest sends a friend request to another user.
// POST /friends/request
func (h *Handler) SendRequest(c *gin.Context) {
	userID := c.GetString("user_id")

	var req SendRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	resolved := h.resolveTargetUserID(req)
	if resolved.errMsg != "" {
		c.JSON(resolved.status, gin.H{"error": resolved.errMsg})
		return
	}
	targetUserID := resolved.targetUserID

	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot send a friend request to yourself"})
		return
	}

	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, targetUserID).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	existingStatus, err := checkExistingFriendship(h.db, userID, targetUserID)
	if err == nil {
		code, msg := friendshipConflictResponse(existingStatus)
		if code != 0 {
			c.JSON(code, gin.H{"error": msg})
			return
		}
	} else if err != sql.ErrNoRows {
		h.log.Error("Failed to check existing friendship", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendFriendRequest})
		return
	}

	var friendshipID string
	var createdAt string
	err = h.db.QueryRow(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'pending')
		RETURNING id, created_at
	`, userID, targetUserID).Scan(&friendshipID, &createdAt)
	if err != nil {
		h.log.Error("Failed to create friend request", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendFriendRequest})
		return
	}

	h.log.Info("Friend request sent", "from", userID, "to", targetUserID)
	h.notifyFriendRequestReceived(targetUserID, friendshipID, userID, createdAt)

	c.JSON(http.StatusCreated, gin.H{
		"id":         friendshipID,
		"status":     "pending",
		"created_at": createdAt,
	})
}

// RespondRequestBody represents a response to a friend request.
type RespondRequestBody struct {
	Action string `json:"action" binding:"required"` // accept, decline
}

func (h *Handler) acceptFriendRequest(c *gin.Context, requestID, userID, requesterID string) {
	ctx := c.Request.Context()

	// ONE spec for the gates, the capture and the focal set, so the three
	// cannot drift apart: WithGatedTx gates focalSenders(subject) while
	// CaptureInTx derives its marker set from the subject it is handed, and
	// nothing checks that two separately-built subjects agree.
	spec := presencehook.Spec{
		Family:        presencecapture.FamilyFriendshipAccept,
		Posture:       presencecapture.FailClosedBlockWrite,
		PrincipalID:   userID,
		CounterpartID: requesterID,
	}

	// WithGatedTx acquires the sender gates BEFORE opening the transaction and
	// owns the deferred rollback. The write widens the friend graph the capture
	// reads, so the two must share a transaction: an autocommit write leaves no
	// window in which the pre-mutation audience is still readable.
	err := presencehook.WithGatedTx(ctx, h.graphPresence, h.db, h.log, spec, func(tx *sql.Tx) error {
		// Capture strictly precedes the write, with nothing between them. No
		// direction branch — accepting widens, but the capture and refresh are
		// unconditional and the clear set is always captured minus fresh.
		plan, captureErr := presencehook.Capture(ctx, h.graphPresence, tx, spec)
		if captureErr != nil {
			return fmt.Errorf("capture accept presence: %w", captureErr)
		}

		// The predicate is repeated here on purpose, inside the transaction.
		// RespondRequest's eligibility read runs on h.db OUTSIDE this tx, so
		// matching on id alone left a TOCTOU window: if the counterpart called
		// BlockUser in between, executeBlockTx set the row to 'blocked' and this
		// UPDATE silently flipped it back to 'accepted' — an authorization
		// regression that undoes a block (PR #2738 review, @code-reviewer).
		// Re-asserting addressee_id and status='pending' makes the write refuse
		// any row the caller is no longer entitled to accept.
		res, execErr := tx.ExecContext(ctx, `
			UPDATE friendships SET status = 'accepted', updated_at = NOW()
			WHERE id = $1 AND addressee_id = $2 AND status = 'pending'
		`, requestID, userID)
		if execErr != nil {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
			return fmt.Errorf("accept friend request: %w", execErr)
		}
		accepted, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseRowsAffected)
			return fmt.Errorf("read accept rows affected: %w", rowsErr)
		}
		if accepted == 0 {
			// The request stopped being acceptable between the eligibility read
			// and this write — blocked, withdrawn, or already answered. Nothing
			// was written, so the same rule as RemoveFriend's rowsAffected == 0
			// branch follows: drop the plan without disconnecting anyone. The
			// rollback also discards the topology markers, which is what keeps a
			// no-op accept from suppressing anyone's Custom Status snapshot for
			// the whole grace window.
			return errFriendRequestNotAcceptable
		}

		return presencehook.Complete(ctx, h.graphPresence, tx, plan)
	})
	if errors.Is(err, errFriendRequestNotAcceptable) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friend request not found"})
		return
	}
	if err != nil {
		h.respondPresenceTerminal(c, errMsgFailedAcceptRequest, err)
		return
	}

	h.log.Info("Friend request accepted", "request_id", requestID, "user_id", userID)
	h.notifyFriendRequestAccepted(requesterID, requestID, userID)
	c.JSON(http.StatusOK, gin.H{"message": "Friend request accepted"})
}

func (h *Handler) notifyFriendRequestAccepted(requesterID, requestID, userID string) {
	if h.hub == nil {
		return
	}
	requesterUUID, parseErr := uuid.Parse(requesterID)
	if parseErr != nil {
		return
	}
	var acceptorUsername string
	var acceptorDisplayName *string
	var acceptorAvatarURL *string
	if err := h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, userID).
		Scan(&acceptorUsername, &acceptorDisplayName, &acceptorAvatarURL); err != nil {
		h.log.Error("notifyFriendRequestAccepted: failed to load acceptor profile", "error", err)
	}

	h.hub.BroadcastToUser(requesterUUID, websocket.OutgoingMessage{
		Type: "friend_request_accepted",
		Data: map[string]interface{}{
			"id":           requestID,
			"user_id":      userID,
			"username":     acceptorUsername,
			"display_name": acceptorDisplayName,
			"avatar_url":   acceptorAvatarURL,
		},
	})
}

// RespondRequest accepts or declines a friend request.
// PATCH /friends/request/:id
func (h *Handler) RespondRequest(c *gin.Context) {
	userID := c.GetString("user_id")
	requestID := c.Param("id")

	if _, err := uuid.Parse(requestID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request ID"})
		return
	}

	var req RespondRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if req.Action != "accept" && req.Action != "decline" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Action must be 'accept' or 'decline'"})
		return
	}

	var requesterID string
	err := h.db.QueryRow(`
		SELECT requester_id FROM friendships
		WHERE id = $1 AND addressee_id = $2 AND status = 'pending'
	`, requestID, userID).Scan(&requesterID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friend request not found"})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch friend request", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to respond to friend request"})
		return
	}

	if req.Action == "accept" {
		h.acceptFriendRequest(c, requestID, userID, requesterID)
		return
	}

	_, err = h.db.Exec(`DELETE FROM friendships WHERE id = $1`, requestID)
	if err != nil {
		h.log.Error("Failed to decline friend request", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decline friend request"})
		return
	}

	h.log.Info("Friend request declined", "request_id", requestID, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Friend request declined"})
}

// RemoveFriend removes a friendship.
// DELETE /friends/:user_id
func (h *Handler) RemoveFriend(c *gin.Context) {
	userID := c.GetString("user_id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	ctx := c.Request.Context()

	// ONE spec for the gates, the capture and the focal set — see the note in
	// acceptFriendRequest on why two separately-built subjects are a silent
	// hazard.
	spec := presencehook.Spec{
		Family:        presencecapture.FamilyFriendshipRemove,
		Posture:       presencecapture.FailClosedBlockWrite,
		PrincipalID:   userID,
		CounterpartID: targetUserID,
	}

	err := presencehook.WithGatedTx(ctx, h.graphPresence, h.db, h.log, spec, func(tx *sql.Tx) error {
		// Capture strictly precedes the write, with nothing between them: the
		// removal destroys the friend graph the capture reads.
		plan, captureErr := presencehook.Capture(ctx, h.graphPresence, tx, spec)
		if captureErr != nil {
			return fmt.Errorf("capture removal presence: %w", captureErr)
		}

		result, execErr := tx.ExecContext(ctx, `
			DELETE FROM friendships
			WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
			  AND status = 'accepted'
		`, userID, targetUserID)
		if execErr != nil {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
			return fmt.Errorf("remove friend: %w", execErr)
		}

		// The previous `rowsAffected, _ :=` discarded a driver error and
		// reported "Friendship not found" for a genuine failure
		// ([internal]rules/backend.md).
		rowsAffected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseRowsAffected)
			return fmt.Errorf("read remove rows affected: %w", rowsErr)
		}
		if rowsAffected == 0 {
			// Deliberately NO Abandon here. `rowsAffected == 0` is positive
			// PROOF that no accepted friendship existed and nothing was written,
			// so there is no stale audience to reconcile and nobody to
			// disconnect. Returning the sentinel makes WithGatedTx's deferred
			// rollback discard the transaction, which also discards any topology
			// markers the capture wrote, and the in-memory plan is dropped.
			//
			// Abandoning here was a security defect (#2738 review): the captured
			// plan's viewer set always contains BOTH principals, and Abandon
			// calls DisconnectRichPresenceClients, which closes every local
			// device for those users. Any authenticated caller could therefore
			// invoke RemoveFriend against a stranger and force a full websocket
			// disconnect of that stranger, repeatably, having mutated nothing —
			// a cheap DoS amplification vector.
			//
			// Contrast the `rows_affected` branch above, which DOES abandon: a
			// driver error there leaves the write's outcome genuinely unknown,
			// and unknown state must fail closed. Proven no-change must not.
			return errFriendshipNotFound
		}

		return presencehook.Complete(ctx, h.graphPresence, tx, plan)
	})
	if errors.Is(err, errFriendshipNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friendship not found"})
		return
	}
	if err != nil {
		h.respondPresenceTerminal(c, errMsgFailedRemoveFriend, err)
		return
	}

	// Counterpart omitted: this handler DELETES the row, so keeping the edge in
	// logs would outlive the record the user just removed.
	h.log.Info("Friend removed", "user_id", userID)

	// Notify the other user
	if h.hub != nil {
		if targetUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
			h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
				Type: "friend_removed",
				Data: map[string]interface{}{
					"user_id": userID,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Friend removed"})
}

// executeBlockTx captures the pre-mutation audience and applies the block, both
// inside tx. It deliberately does NOT commit: presencehook.Complete owns the commit
// so the durable rail can replace the terminal without touching this site.
//
// The capture is unconditional. The site sees all three prior friendship states
// (accepted, pending, absent) and must not branch on them: the delta is simply
// empty where nothing changed.
func (h *Handler) executeBlockTx(
	ctx context.Context, tx *sql.Tx, userID, targetUserID string,
) (presencecapture.Plan, error) {
	plan, err := presencehook.Capture(ctx, h.graphPresence, tx, blockCaptureSpec(userID, targetUserID))
	if err != nil {
		return nil, fmt.Errorf("capture block presence: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE friendships SET status = 'blocked', updated_at = NOW()
		WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)
	`, userID, targetUserID)
	if err != nil {
		return plan, fmt.Errorf("update friendship to blocked: %w", err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return plan, fmt.Errorf("read block rows affected: %w", err)
	}
	if rowsAffected == 0 {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO friendships (requester_id, addressee_id, status)
			VALUES ($1, $2, 'blocked')
		`, userID, targetUserID)
		if err != nil {
			return plan, fmt.Errorf("insert block row: %w", err)
		}
	}
	return plan, nil
}

type convEpoch struct {
	convID   string
	maxEpoch int
}

// findDMRevocations discovers the shared DM conversations whose key material a
// block must revoke. It returns an error so a failed DISCOVERY fails the block.
//
// Every failure here was previously reported as "no revocations": a query error
// logged and returned nil, a scan error was `continue`d — conflated with the
// benign maxEpoch == 0 case — and rows.Err() only logged. The caller then wrote
// no `dm_key_revocations` row, deleted the keys, and COMMITTED, so the DM key
// endpoint kept accepting stale key material because its revocation check reads
// exactly that row. A scan error is client-side and does not poison the
// transaction, so nothing downstream failed either — the block looked healthy
// (PR #2738 review, CodeRabbit; CWE-703).
func (h *Handler) findDMRevocations(
	ctx context.Context, tx *sql.Tx, userID, targetUserID string,
) ([]convEpoch, error) {
	revokeRows, revokeErr := tx.QueryContext(ctx, `
		SELECT dp1.conversation_id, COALESCE(MAX(dck.key_version), 0)
		FROM dm_participants dp1
		INNER JOIN dm_participants dp2 ON dp1.conversation_id = dp2.conversation_id
		LEFT JOIN dm_channel_keys dck ON dck.conversation_id = dp1.conversation_id
		WHERE dp1.user_id = $1 AND dp2.user_id = $2
		GROUP BY dp1.conversation_id
	`, userID, targetUserID)
	if revokeErr != nil {
		return nil, fmt.Errorf("query shared dm conversations for revocation: %w", revokeErr)
	}
	defer func() { _ = revokeRows.Close() }()

	var revocations []convEpoch
	for revokeRows.Next() {
		var ce convEpoch
		if err := revokeRows.Scan(&ce.convID, &ce.maxEpoch); err != nil {
			return nil, fmt.Errorf("scan dm revocation row: %w", err)
		}
		if ce.maxEpoch == 0 {
			// Genuinely nothing to revoke for this conversation — no key has
			// ever been distributed. Distinct from a scan failure, which the
			// old code conflated with this.
			continue
		}
		revocations = append(revocations, ce)
	}
	if err := revokeRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dm revocation rows: %w", err)
	}
	return revocations, nil
}

// revokeBlockedDMKeys revokes the blocked user's DM key material inside the
// block transaction. It returns an error so a failed revocation FAILS THE BLOCK
// rather than committing one.
//
// Every write here was previously discarded (`_, _ = tx.Exec`, and a
// log-and-continue on the revocation insert), so a failed revocation of KEY
// MATERIAL still committed the block and returned 200 — the caller believed the
// blocked user had lost their keys when they had not. That is the incident class
// `[internal]rules/backend.md` cites for #1154, in the same subsystem. Surfaced by
// `@code-reviewer` and `@e2ee-reviewer` on PR #2738.
func (h *Handler) revokeBlockedDMKeys(
	ctx context.Context, tx *sql.Tx, userID, targetUserID string,
) error {
	revocations, err := h.findDMRevocations(ctx, tx, userID, targetUserID)
	if err != nil {
		return fmt.Errorf("find dm revocations: %w", err)
	}
	for _, ce := range revocations {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by)
			VALUES ($1, $2, $3, 'user_blocked', $4)
			ON CONFLICT (conversation_id, revoked_epoch) DO NOTHING
		`, ce.convID, ce.maxEpoch, ce.maxEpoch+1, userID); err != nil {
			return fmt.Errorf("record dm key revocation: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM dm_channel_keys
		WHERE user_id = $2
		  AND conversation_id IN (
			SELECT dp1.conversation_id FROM dm_participants dp1
			INNER JOIN dm_participants dp2 ON dp1.conversation_id = dp2.conversation_id
			WHERE dp1.user_id = $1 AND dp2.user_id = $2
		  )
		  AND key_version = (
			SELECT MAX(key_version) FROM dm_channel_keys dck2
			WHERE dck2.conversation_id = dm_channel_keys.conversation_id
		  )
	`, userID, targetUserID); err != nil {
		return fmt.Errorf("delete blocked dm keys: %w", err)
	}
	return nil
}

func (h *Handler) notifyBlock(userID, targetUserID string) {
	if h.hub == nil {
		return
	}
	if targetUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
		h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
			Type: "friend_removed",
			Data: map[string]interface{}{
				"user_id": userID,
			},
		})
	}
	if blockerUUID, parseErr := uuid.Parse(userID); parseErr == nil {
		h.hub.BroadcastToUser(blockerUUID, websocket.OutgoingMessage{
			Type: "key_revocation",
			Data: map[string]interface{}{
				"blocked_user_id": targetUserID,
			},
		})
	}
}

// BlockUser blocks another user. If a friendship exists, it is updated to 'blocked'.
// If a DM conversation exists, the blocked user's current epoch key is revoked.
// POST /friends/:user_id/block
func (h *Handler) BlockUser(c *gin.Context) {
	userID := c.GetString("user_id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot block yourself"})
		return
	}

	ctx := c.Request.Context()

	// blockCaptureSpec is the single source for the gates, the capture and the
	// focal set; executeBlockTx calls it again for its own Capture, so both
	// subjects come from one constructor rather than two hand-built literals.
	spec := blockCaptureSpec(userID, targetUserID)

	// This site declares FailConservativeDegrade so a large friend graph cannot
	// deny a safety affordance. That posture governs the C1 legs ONLY. The
	// durable C2 leg fails CLOSED here as everywhere else: Custom Status has no
	// heartbeat and no TTL, so degrading it would leave a blocked user holding
	// the blocker's status indefinitely. The consequence is stated rather than
	// hidden — a topology-rail fault does 500 a block.
	err := presencehook.WithGatedTx(ctx, h.graphPresence, h.db, h.log, spec, func(tx *sql.Tx) error {
		plan, blockErr := h.executeBlockTx(ctx, tx, userID, targetUserID)
		if blockErr != nil {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
			return blockErr
		}

		if revokeErr := h.revokeBlockedDMKeys(ctx, tx, userID, targetUserID); revokeErr != nil {
			// A failed key revocation must not commit a block that claims to
			// have revoked. The rollback discards the friendship write too, so
			// nothing landed — hence CauseWriteFailed, which proves no commit
			// and therefore disconnects nobody.
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
			return fmt.Errorf("revoke blocked DM keys: %w", revokeErr)
		}

		return presencehook.Complete(ctx, h.graphPresence, tx, plan)
	})
	if err != nil {
		h.respondPresenceTerminal(c, errMsgFailedBlockUser, err)
		return
	}

	h.log.Info("User blocked", "user_id", userID, "blocked", targetUserID)
	h.notifyBlock(userID, targetUserID)
	c.JSON(http.StatusOK, gin.H{"message": "User blocked"})
}

// --- Friend Code types ---

// friendCodeResponse represents a friend code in API responses.
type friendCodeResponse struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Code       string     `json:"code"`
	MaxUses    *int       `json:"max_uses"`
	UseCount   int        `json:"use_count"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	IsRevoked  bool       `json:"is_revoked"`
	AutoAccept bool       `json:"auto_accept"`
	CreatedAt  string     `json:"created_at"`
}

type createFriendCodeRequest struct {
	MaxUses    *int  `json:"max_uses"`
	ExpiresIn  *int  `json:"expires_in"` // seconds; nil → default 3600 (1h)
	AutoAccept *bool `json:"auto_accept"`
}

func resolveMaxUses(input *int) *int {
	if input == nil {
		defaultMax := 1
		return &defaultMax
	}
	if *input <= 0 {
		return nil
	}
	maxUses := *input
	if maxUses > 10 {
		maxUses = 10
	}
	return &maxUses
}

func resolveExpiresIn(input *int) int {
	if input == nil {
		return 3600
	}
	sec := *input
	if sec < 300 {
		return 300
	}
	if sec > 86400 {
		return 86400
	}
	return sec
}

// CreateFriendCode generates a new friend code for the caller.
// POST /friends/codes
func (h *Handler) CreateFriendCode(c *gin.Context) {
	userID := c.GetString("user_id")

	var req createFriendCodeRequest
	_ = c.ShouldBindJSON(&req)

	maxUsesPtr := resolveMaxUses(req.MaxUses)
	expiresAt := time.Now().UTC().Add(time.Duration(resolveExpiresIn(req.ExpiresIn)) * time.Second)

	autoAccept := false
	if req.AutoAccept != nil {
		autoAccept = *req.AutoAccept
	}

	for attempts := 0; attempts < 5; attempts++ {
		code, err := invites.GenerateCode()
		if err != nil {
			h.log.Error("Failed to generate friend code", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create friend code"})
			return
		}

		var fc friendCodeResponse
		insertErr := h.db.QueryRow(`
			INSERT INTO friend_codes (user_id, code, max_uses, expires_at, auto_accept)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, user_id, code, max_uses, use_count, expires_at, is_revoked, auto_accept, created_at
		`, userID, code, maxUsesPtr, expiresAt, autoAccept).Scan(
			&fc.ID, &fc.UserID, &fc.Code, &fc.MaxUses, &fc.UseCount,
			&fc.ExpiresAt, &fc.IsRevoked, &fc.AutoAccept, &fc.CreatedAt,
		)
		if insertErr != nil {
			continue
		}

		// The code value is omitted deliberately: anyone holding it can claim a
		// friendship with the owner, so it is bearer material, and logs are a
		// weaker trust boundary than the table it lives in (#2738 review).
		h.log.Info("Friend code created", "user_id", userID)
		c.JSON(http.StatusCreated, gin.H{"friend_code": fc})
		return
	}

	h.log.Error("Failed to create unique friend code after retries")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create friend code"})
}

// ListFriendCodes returns the caller's non-revoked friend codes.
// GET /friends/codes
func (h *Handler) ListFriendCodes(c *gin.Context) {
	userID := c.GetString("user_id")

	rows, err := h.db.Query(`
		SELECT id, user_id, code, max_uses, use_count, expires_at, is_revoked, auto_accept, created_at
		FROM friend_codes
		WHERE user_id = $1 AND is_revoked = FALSE
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		h.log.Error("Failed to query friend codes", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend codes"})
		return
	}
	defer func() { _ = rows.Close() }()

	codes := []friendCodeResponse{}
	for rows.Next() {
		var fc friendCodeResponse
		if err := rows.Scan(
			&fc.ID, &fc.UserID, &fc.Code, &fc.MaxUses, &fc.UseCount,
			&fc.ExpiresAt, &fc.IsRevoked, &fc.AutoAccept, &fc.CreatedAt,
		); err != nil {
			h.log.Error("Failed to scan friend code", "error", err)
			continue
		}
		codes = append(codes, fc)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating friend codes", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend codes"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"friend_codes": codes})
}

// RevokeFriendCode soft-revokes a friend code owned by the caller.
// DELETE /friends/codes/:id
func (h *Handler) RevokeFriendCode(c *gin.Context) {
	userID := c.GetString("user_id")
	codeID := c.Param("id")

	if _, err := uuid.Parse(codeID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid code ID"})
		return
	}

	result, err := h.db.Exec(`
		UPDATE friend_codes SET is_revoked = TRUE
		WHERE id = $1 AND user_id = $2 AND is_revoked = FALSE
	`, codeID, userID)
	if err != nil {
		h.log.Error("Failed to revoke friend code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke friend code"})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friend code not found or already revoked"})
		return
	}

	h.log.Info("Friend code revoked", "code_id", codeID, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Friend code revoked"})
}

// PreviewFriendCode validates a friend code and returns the owner's profile.
// Does NOT consume a use.
// GET /friends/codes/:code
func (h *Handler) PreviewFriendCode(c *gin.Context) {
	code := c.Param("code")

	if len(code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid friend code format"})
		return
	}

	var (
		ownerID     string
		username    string
		displayName *string
		avatarURL   *string
		expiresAt   *time.Time
		isRevoked   bool
		maxUses     *int
		useCount    int
	)

	err := h.db.QueryRow(`
		SELECT fc.user_id, u.username, u.display_name, u.avatar_url,
		       fc.expires_at, fc.is_revoked, fc.max_uses, fc.use_count
		FROM friend_codes fc
		INNER JOIN users u ON fc.user_id = u.id
		WHERE fc.code = $1
	`, code).Scan(
		&ownerID, &username, &displayName, &avatarURL,
		&expiresAt, &isRevoked, &maxUses, &useCount,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgInvalidFriendCode})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch friend code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend code"})
		return
	}

	valid := !isRevoked &&
		(expiresAt == nil || expiresAt.After(time.Now().UTC())) &&
		(maxUses == nil || *maxUses == 0 || useCount < *maxUses)

	c.JSON(http.StatusOK, gin.H{
		"user_id":      ownerID,
		"username":     username,
		"display_name": displayName,
		"avatar_url":   avatarURL,
		"valid":        valid,
	})
}

type friendCodeRow struct {
	codeID     string
	ownerID    string
	maxUses    *int
	useCount   int
	expiresAt  *time.Time
	isRevoked  bool
	autoAccept bool
}

func lookupFriendCode(tx *sql.Tx, code string) (friendCodeRow, error) {
	var fc friendCodeRow
	err := tx.QueryRow(`
		SELECT id, user_id, max_uses, use_count, expires_at, is_revoked, auto_accept
		FROM friend_codes
		WHERE code = $1
		FOR UPDATE
	`, code).Scan(&fc.codeID, &fc.ownerID, &fc.maxUses, &fc.useCount, &fc.expiresAt, &fc.isRevoked, &fc.autoAccept)
	return fc, err
}

// friendCodePreview is the pre-transaction half of a friend-code claim: just
// enough to name the focal senders and decide whether a capture is needed.
type friendCodePreview struct {
	ownerID    string
	autoAccept bool
}

// lookupFriendCodeOwner resolves the code's owner BEFORE the transaction opens.
//
// WithGatedTx keys the process-local sender gates on the focal senders, and for
// this site the counterpart is the code's OWNER — which the handler cannot know
// until it has read the code. The gates must be held from before BeginTx, so the
// read is hoisted out here and re-asserted inside the transaction under
// FOR UPDATE. That is the same shape acceptFriendRequest already uses for its
// TOCTOU re-assertion, and it costs one round trip.
//
// It takes NO row lock: a lock held outside the transaction that uses the value
// would be meaningless, and the in-transaction lookupFriendCode is what actually
// serializes the claim.
func lookupFriendCodeOwner(
	ctx context.Context, db *sql.DB, code string,
) (friendCodePreview, error) {
	var preview friendCodePreview
	err := db.QueryRowContext(ctx, `
		SELECT user_id, auto_accept
		FROM friend_codes
		WHERE code = $1
	`, code).Scan(&preview.ownerID, &preview.autoAccept)
	return preview, err
}

// claimRejection is an in-closure signal carrying a pre-write rejection's own
// status and message, so the existing route shapes survive the move into a
// transaction closure. Like the two 404 sentinels above it is never a failure:
// returning it aborts and rolls back a transaction that has written nothing.
type claimRejection struct {
	status  int
	message string
}

func (e *claimRejection) Error() string { return e.message }

func validateFriendCodeClaim(fc friendCodeRow, userID string) (int, string) {
	if fc.isRevoked {
		return http.StatusGone, "This friend code has been revoked"
	}
	if fc.expiresAt != nil && fc.expiresAt.Before(time.Now().UTC()) {
		return http.StatusGone, "This friend code has expired"
	}
	if fc.maxUses != nil && *fc.maxUses > 0 && fc.useCount >= *fc.maxUses {
		return http.StatusGone, "This friend code has reached its maximum uses"
	}
	if fc.ownerID == userID {
		return http.StatusBadRequest, "Cannot claim your own friend code"
	}
	return 0, ""
}

func claimFriendshipConflictResponse(status string) (int, string) {
	switch status {
	case "accepted":
		return http.StatusConflict, "Already friends with this user"
	case "pending":
		return http.StatusConflict, "Friend request already pending with this user"
	case "blocked":
		return http.StatusForbidden, "Cannot add this user as a friend"
	default:
		return 0, ""
	}
}

// claimResult carries the claim's outcome plus the presence plan its caller
// must carry to the terminal. The plan is a return value rather than handler
// state because ClaimFriendCode owns the transaction and therefore the commit.
type claimResult struct {
	friendshipID string
	createdAt    string
	status       string
	// codeID is the id read under FOR UPDATE inside the transaction. The
	// notification tail needs it and the hoisted preview does not carry one:
	// only the locked read establishes which row the claim actually consumed.
	codeID string
	plan   presencecapture.Plan
}

// executeFriendCodeClaim inserts the friendship and consumes one use of the
// code inside the caller's transaction. It deliberately does NOT commit:
// presencehook.Complete owns the commit on both the wired and unwired paths.
//
// It takes the capture as a PARAMETER rather than reading h.graphPresence,
// because ClaimFriendCode decides per request whether this claim captures at
// all: a non-auto-accepting code creates a 'pending' row, which confers no
// friend-of-friends visibility, so that request takes no gate. Reading the
// handler field here would capture — and abandon — for a claim whose gates were
// never acquired.
func (h *Handler) executeFriendCodeClaim(
	ctx context.Context,
	capture presencecapture.GraphPresenceCapture,
	tx *sql.Tx,
	userID, ownerID, codeID string,
	autoAccept bool,
) (claimResult, error) {
	status := "pending"
	if autoAccept {
		status = "accepted"
	}

	// Only an auto-accepting claim widens the friend graph. A 'pending' row
	// confers no friend-of-friends visibility, which is why SendRequest — the
	// other pending-friendship writer — carries no capture either. Capturing
	// here would block a plain friend request on a capture failure for no
	// reconciliation.
	var plan presencecapture.Plan
	if status == "accepted" {
		var err error
		plan, err = presencehook.Capture(ctx, capture, tx, presencehook.Spec{
			Family:        presencecapture.FamilyFriendshipAccept,
			Posture:       presencecapture.FailClosedBlockWrite,
			PrincipalID:   userID,
			CounterpartID: ownerID,
		})
		if err != nil {
			return claimResult{}, fmt.Errorf("presence capture: %w", err)
		}
	}

	var friendshipID, createdAt string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, $3)
		RETURNING id, created_at
	`, userID, ownerID, status).Scan(&friendshipID, &createdAt)
	if err != nil {
		presencehook.Abandon(capture, plan, presencecapture.CauseWriteFailed)
		return claimResult{}, fmt.Errorf("insert friendship from friend code: %w", err)
	}

	if _, err = tx.ExecContext(ctx, `UPDATE friend_codes SET use_count = use_count + 1 WHERE id = $1`, codeID); err != nil {
		presencehook.Abandon(capture, plan, presencecapture.CauseWriteFailed)
		return claimResult{}, fmt.Errorf("increment friend code use count: %w", err)
	}

	return claimResult{
		friendshipID: friendshipID,
		createdAt:    createdAt,
		status:       status,
		codeID:       codeID,
		plan:         plan,
	}, nil
}

type userProfile struct {
	username    string
	displayName *string
	avatarURL   *string
}

func (h *Handler) fetchUserProfile(userID string) userProfile {
	var p userProfile
	// Per [internal]rules/backend.md (#1142/#1154): never silently discard a query
	// error. A failed lookup leaves p.username == "", which the friend_request_received
	// emitters treat as a skip signal (the client drops an empty to_username, #981).
	// Log the error only — never profile values (observability.md).
	if err := h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, userID).
		Scan(&p.username, &p.displayName, &p.avatarURL); err != nil {
		h.log.Error("fetchUserProfile: failed to load profile", "error", err)
	}
	return p
}

// friendRequestReceivedData builds the friend_request_received WS payload shared by
// both emit sites (SendRequest and the friend-code-claim path). It is pure (no DB,
// no hub) so the from_*/to_* wire shape can be unit-tested directly — that WS↔REST
// shape contract is exactly what drifted in #981, and the unit test guards it from
// regressing again. The client requires to_user_id and to_username.
func friendRequestReceivedData(friendshipID, fromUserID string, from userProfile, toUserID string, to userProfile, createdAt string) map[string]interface{} {
	return map[string]interface{}{
		"id":                friendshipID,
		"from_user_id":      fromUserID,
		"from_username":     from.username,
		"from_display_name": from.displayName,
		"from_avatar_url":   from.avatarURL,
		"to_user_id":        toUserID,
		"to_username":       to.username,
		"to_display_name":   to.displayName,
		"to_avatar_url":     to.avatarURL,
		"created_at":        createdAt,
	}
}

type claimNotification struct {
	ownerID      string
	userID       string
	friendshipID string
	codeID       string
	status       string
	createdAt    string
	autoAccept   bool
	claimer      userProfile
	owner        userProfile
}

func (h *Handler) notifyFriendCodeClaimed(n claimNotification) {
	if h.hub == nil {
		return
	}
	ownerUUID, parseErr := uuid.Parse(n.ownerID)
	if parseErr != nil {
		return
	}

	h.hub.BroadcastToUser(ownerUUID, websocket.OutgoingMessage{
		Type: "friend_code_claimed",
		Data: map[string]interface{}{
			"friendship_id": n.friendshipID,
			"code_id":       n.codeID,
			"status":        n.status,
			"user_id":       n.userID,
			"username":      n.claimer.username,
			"display_name":  n.claimer.displayName,
			"avatar_url":    n.claimer.avatarURL,
			"created_at":    n.createdAt,
		},
	})

	if n.autoAccept {
		if claimerUUID, parseErr2 := uuid.Parse(n.userID); parseErr2 == nil {
			h.hub.BroadcastToUser(claimerUUID, websocket.OutgoingMessage{
				Type: "friend_request_accepted",
				Data: map[string]interface{}{
					"id":           n.friendshipID,
					"user_id":      n.ownerID,
					"username":     n.owner.username,
					"display_name": n.owner.displayName,
					"avatar_url":   n.owner.avatarURL,
				},
			})
		}
		return
	}

	// owner profile is fetched upstream via fetchUserProfile; an empty username means
	// that lookup failed (username is NOT NULL). The client drops a friend_request_received
	// with an empty to_username (#981), so skip rather than emit a payload it will discard.
	if n.owner.username == "" {
		h.log.Warn("notifyFriendCodeClaimed: empty owner username; skipping friend_request_received broadcast")
		return
	}
	h.hub.BroadcastToUser(ownerUUID, websocket.OutgoingMessage{
		Type: "friend_request_received",
		Data: friendRequestReceivedData(n.friendshipID, n.userID, n.claimer, n.ownerID, n.owner, n.createdAt),
	})
}

// claimFriendCodeInTx is the locked half of ClaimFriendCode, extracted verbatim
// so the handler stays under the cognitive-complexity bound (SonarQube S3776).
// Nothing was reordered: the locked read, the preview re-assertion, validation,
// the conflict check, the write and the terminal run in exactly the sequence
// they did inline, and presencehook.Complete is still the ONLY path that
// returns nil.
//
// It runs on the transaction WithGatedTx opened, under the gates WithGatedTx
// acquired for preview.ownerID -- which is why the re-assertion below is
// load-bearing rather than defensive.
func (h *Handler) claimFriendCodeInTx(
	ctx context.Context,
	gatedCapture presencecapture.GraphPresenceCapture,
	tx *sql.Tx,
	userID string,
	code string,
	preview friendCodePreview,
) (claimResult, error) {
	var claim claimResult

	fc, lookupErr := lookupFriendCode(tx, code)
	if errors.Is(lookupErr, sql.ErrNoRows) {
		return claim, &claimRejection{status: http.StatusNotFound, message: errMsgInvalidFriendCode}
	}
	if lookupErr != nil {
		return claim, fmt.Errorf("query friend code: %w", lookupErr)
	}

	// Re-assert the hoisted read. The gates were acquired for preview.ownerID
	// and the capture is keyed on preview.autoAccept; if the locked row names a
	// different owner or a different auto_accept, the gates and the capture
	// cover the wrong sender, so fail closed rather than write under the wrong
	// lock. The 500 keeps the existing route shape — this path was previously
	// unreachable and must not introduce a new status.
	if fc.ownerID != preview.ownerID || fc.autoAccept != preview.autoAccept {
		return claim, errors.New("friend code changed between the preview and the locked read")
	}

	if errCode, errMsg := validateFriendCodeClaim(fc, userID); errCode != 0 {
		return claim, &claimRejection{status: errCode, message: errMsg}
	}

	existingStatus, existingErr := checkExistingFriendship(tx, userID, fc.ownerID)
	if existingErr == nil {
		if respCode, msg := claimFriendshipConflictResponse(existingStatus); respCode != 0 {
			return claim, &claimRejection{status: respCode, message: msg}
		}
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return claim, fmt.Errorf("check existing friendship: %w", existingErr)
	}

	claim, claimErr := h.executeFriendCodeClaim(
		ctx, gatedCapture, tx, userID, fc.ownerID, fc.codeID, fc.autoAccept,
	)
	if claimErr != nil {
		return claim, claimErr
	}

	// presencehook.Complete owns the commit on both the wired and unwired
	// paths, so this handler never calls tx.Commit() itself. Every branch above
	// returns an error, so this is the ONLY way a nil error is returned — an
	// early nil would let WithGatedTx report success for a transaction its
	// deferred rollback then discards.
	return claim, presencehook.Complete(ctx, gatedCapture, tx, claim.plan)
}

// ClaimFriendCode redeems a friend code to create a friendship.
// If auto_accept is set on the code, the friendship is created as 'accepted' directly.
// Otherwise, a pending friend request is created from the claimer to the code owner.
// POST /friends/codes/:code/claim
func (h *Handler) ClaimFriendCode(c *gin.Context) {
	userID := c.GetString("user_id")
	code := c.Param("code")

	if len(code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid friend code format"})
		return
	}

	ctx := c.Request.Context()

	// The counterpart of this claim is the code's OWNER, which is unknown until
	// the code has been read — and the sender gates must be held from before
	// BeginTx. So the owner is resolved here, unlocked, and re-asserted against
	// the FOR UPDATE row inside the transaction.
	preview, err := lookupFriendCodeOwner(ctx, h.db, code)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgInvalidFriendCode})
		return
	}
	if err != nil {
		h.log.Error("Failed to preview friend code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedClaimFriendCode})
		return
	}

	// Only an auto-accepting claim widens the friend graph. A 'pending' row
	// confers no friend-of-friends visibility, so a plain friend request takes
	// no gate, writes no marker, and behaves exactly as it did before the hook.
	// The same value goes to WithGatedTx, executeFriendCodeClaim and Complete,
	// so the gate, the capture and the terminal cannot disagree about whether
	// this request is hooked.
	gatedCapture := h.graphPresence
	if !preview.autoAccept {
		gatedCapture = nil
	}
	spec := presencehook.Spec{
		Family:        presencecapture.FamilyFriendshipAccept,
		Posture:       presencecapture.FailClosedBlockWrite,
		PrincipalID:   userID,
		CounterpartID: preview.ownerID,
	}

	var claim claimResult
	err = presencehook.WithGatedTx(ctx, gatedCapture, h.db, h.log, spec, func(tx *sql.Tx) error {
		var txErr error
		claim, txErr = h.claimFriendCodeInTx(ctx, gatedCapture, tx, userID, code, preview)
		return txErr
	})
	var rejection *claimRejection
	if errors.As(err, &rejection) {
		c.JSON(rejection.status, gin.H{"error": rejection.message})
		return
	}
	if err != nil {
		h.respondPresenceTerminal(c, errMsgFailedClaimFriendCode, err)
		return
	}
	friendshipID, createdAt, status := claim.friendshipID, claim.createdAt, claim.status

	// preview.ownerID and preview.autoAccept rather than the closure-scoped fc:
	// the closure asserted the two agree, so this is a rename, not a change.
	// Code value omitted — bearer material, see the note in CreateFriendCode.
	h.log.Info("Friend code claimed", "claimer", userID, "owner", preview.ownerID, "status", status)

	claimer := h.fetchUserProfile(userID)
	owner := h.fetchUserProfile(preview.ownerID)
	h.notifyFriendCodeClaimed(claimNotification{
		ownerID:      preview.ownerID,
		userID:       userID,
		friendshipID: friendshipID,
		codeID:       claim.codeID,
		status:       status,
		createdAt:    createdAt,
		autoAccept:   preview.autoAccept,
		claimer:      claimer,
		owner:        owner,
	})

	c.JSON(http.StatusOK, gin.H{
		"status":        status,
		"friendship_id": friendshipID,
		"user": map[string]interface{}{
			"user_id":      preview.ownerID,
			"username":     owner.username,
			"display_name": owner.displayName,
			"avatar_url":   owner.avatarURL,
		},
	})
}
