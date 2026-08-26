package friends

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// errMsgFailedEligibility is the only body the eligibility endpoint emits on a
// database error. It never names the target, the mode, or the failing query.
const errMsgFailedEligibility = "Failed to check friend request eligibility"

// sameUser reports whether two user-id strings denote the same user, comparing
// canonical UUID values rather than raw text.
//
// The path param is canonicalised before it reaches here, but the JWT claim is
// not: `GenerateAccessToken` takes a plain string and no auth-middleware step
// parses it, so the claim is canonical only because every caller happens to
// hand it a DB-rendered uuid. Comparing a canonicalised value against a raw one
// makes the self-check depend on that convention holding forever. It is not
// exploitable today — the token is signed, so a requester cannot choose their
// own claim — but a future mint path that passes a differently-cased id would
// silently turn a self-probe into an ordinary gate evaluation, which answers
// `true` for anyone in at least one server.
//
// Falls back to an exact string compare when either side is not a UUID, which
// preserves today's behaviour for non-UUID ids rather than inventing a match.
func sameUser(a, b string) bool {
	ua, errA := uuid.Parse(a)
	ub, errB := uuid.Parse(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ua == ub
}

// canReceiveFriendRequestFrom reports whether targetID accepts a friend request from requesterID.
//
// SECURITY — the shared-server EXISTS is the CASE *condition*, not a CASE *arm*. Postgres evaluates
// CASE conditions in order, so the join runs on every path; the enum subexpression appears in both
// arms, so it too runs on every path. All three enum values therefore perform identical database
// work for a fixed (requester, target) pair. Moving the EXISTS into an arm, or rewriting this with
// AND/OR (whose operand evaluation order Postgres does not define), reopens the timing channel this
// helper exists to close. Do not "simplify" it.
//
// Wraps sql.ErrNoRows when targetID does not exist; both call sites use errors.Is. The enum value itself never becomes a Go
// value here, so it cannot be logged ([internal]rules/observability.md).
func (h *Handler) canReceiveFriendRequestFrom(ctx context.Context, targetID, requesterID string) (bool, error) {
	return h.canReceiveFriendRequestFromQ(ctx, h.db, targetID, requesterID)
}

// rowQuerier is satisfied by both *sql.DB and *sql.Tx. RED-TEAM FIX RT-1: the
// friend-code claim must evaluate the same gate, and it must do so on ITS
// transaction — the claim already holds the friend_codes row FOR UPDATE, and a
// second pool connection would read outside that serialization.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// canReceiveFriendRequestFromQ is canReceiveFriendRequestFrom against an
// explicit querier. The statement is byte-identical, so all three enum values
// keep performing identical database work on every call site.
func (h *Handler) canReceiveFriendRequestFromQ(
	ctx context.Context, q rowQuerier, targetID, requesterID string,
) (bool, error) {
	var eligible bool
	err := q.QueryRowContext(ctx, `
		SELECT CASE
		         WHEN EXISTS (
		             SELECT 1
		             FROM server_members sm1
		             JOIN server_members sm2 ON sm1.server_id = sm2.server_id
		             WHERE sm1.user_id = $1 AND sm2.user_id = $2
		         )
		         THEN COALESCE(ps.allow_friend_requests_from, 'everyone') <> 'nobody'
		         ELSE COALESCE(ps.allow_friend_requests_from, 'everyone') =  'everyone'
		       END AS eligible
		FROM users u
		LEFT JOIN privacy_settings ps ON ps.user_id = u.id
		WHERE u.id = $1
	`, targetID, requesterID).Scan(&eligible)
	if err != nil {
		return false, fmt.Errorf("check friend request eligibility: %w", err)
	}
	return eligible, nil
}

// GetFriendRequestEligibility answers whether the authenticated user could send
// a friend request to :user_id. It returns exactly one bit and never reveals
// which of the three modes produced a false.
//
// GET /users/:user_id/friend-request-eligibility
func (h *Handler) GetFriendRequestEligibility(c *gin.Context) {
	userID := c.GetString("user_id")
	targetID := c.Param("user_id")

	parsedTarget, err := uuid.Parse(targetID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id"})
		return
	}
	// RED-TEAM FIX RT-3: uuid.Parse accepts "urn:uuid:…", braces and un-hyphenated
	// hex; Postgres's uuid input rejects the urn form outright and folds hex case.
	// Forwarding the RAW string therefore (a) 500s on a validated input and (b)
	// makes the self-compare below case-sensitive. Canonicalise once, here.
	targetID = parsedTarget.String()

	// Self is answered before the query, and not for cost: the shared-server
	// self-join matches trivially for any user in at least one server, so a
	// self-probe would otherwise report true. The endpoint answers "would a
	// request be accepted", and for self it would not. (Deliberately asymmetric
	// with SendRequest, which 400s a self-target earlier.)
	if sameUser(targetID, userID) {
		c.JSON(http.StatusOK, gin.H{"eligible": false})
		return
	}

	eligible, err := h.canReceiveFriendRequestFrom(c.Request.Context(), targetID, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}
		h.log.Error(errMsgFailedEligibility, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedEligibility})
		return
	}

	// RED-TEAM FIX RT-2: the privacy statement above consults NO friendship
	// state, so before this the endpoint answered `true` for a target who had
	// BLOCKED the caller — while POST /friends/request answered 403. Composing
	// the two therefore separated privacy-blocked from user-blocked, the exact
	// pair spec §2 declares indistinguishable, and falsified this route's own
	// OpenAPI claim that POST "enforces the same gate".
	//
	// Unconditional, and ANDed rather than short-circuited, so the bit still
	// costs the same two statements for every enum value and every relationship.
	existingStatus, statusErr := checkExistingFriendship(h.db, userID, targetID)
	switch {
	case statusErr == nil:
		if code, _ := friendshipConflictResponse(existingStatus); code != 0 {
			eligible = false
		}
	case errors.Is(statusErr, sql.ErrNoRows):
		// No relationship: the privacy verdict stands.
	default:
		h.log.Error(errMsgFailedEligibility, "error", statusErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedEligibility})
		return
	}

	c.JSON(http.StatusOK, gin.H{"eligible": eligible})
}
