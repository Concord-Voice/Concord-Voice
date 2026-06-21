package auth_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const wsTicketKeyPrefix = "ws_ticket:"

// --- ValidateTicket Unit Tests (no HTTP, direct Redis) ---

func TestValidateTicketSuccess(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Seed a ticket directly in Redis
	ticket := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, "user-123", 30*time.Second).Err()
	require.NoError(t, err)

	userID, sessionID, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-123", userID)
	assert.Empty(t, sessionID)
}

func TestValidateTicketWithSessionID(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "fedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, "user-456:session-abc", 30*time.Second).Err()
	require.NoError(t, err)

	userID, sessionID, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-456", userID)
	assert.Equal(t, "session-abc", sessionID)
}

func TestValidateTicketSingleUse(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "singleuse1234567890singleuse1234567890singleuse1234567890singleu"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, "user-789", 30*time.Second).Err()
	require.NoError(t, err)

	// First use succeeds
	userID, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-789", userID)

	// Second use fails (ticket deleted)
	_, _, err = auth.ValidateTicket(ctx, ts.Redis, ticket)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid or expired")
}

func TestValidateTicketEmpty(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	_, _, err := auth.ValidateTicket(ctx, ts.Redis, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty ticket")
}

func TestValidateTicketWhitespace(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	_, _, err := auth.ValidateTicket(ctx, ts.Redis, "   ")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty ticket")
}

func TestValidateTicketExpired(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Nonexistent ticket (simulates expired)
	_, _, err := auth.ValidateTicket(ctx, ts.Redis, "nonexistent-ticket-value")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid or expired")
}

// --- IssueTicket HTTP Tests ---

func TestIssueTicketSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticket1")

	w := ts.DoRequest("POST", pathWSTicket, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ticket, ok := body["ticket"].(string)
	assert.True(t, ok, "response should contain ticket")
	assert.NotEmpty(t, ticket)

	// Verify ticket is valid in Redis
	ctx := context.Background()
	userID, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID)
}

func TestIssueTicketWithSessionID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticket2")

	headers := testhelpers.AuthHeaders(user.AccessToken)
	headers.Set("X-Session-ID", "test-session-123")

	w := ts.DoRequest("POST", pathWSTicket, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ticket := body["ticket"].(string)

	// Validate and check session ID
	ctx := context.Background()
	userID, sessionID, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID)
	assert.Equal(t, "test-session-123", sessionID)
}

func TestIssueTicketNoAuth(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathWSTicket, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestIssueTicketMultipleTickets(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticket3")

	// Issue two tickets
	w1 := ts.DoRequest("POST", pathWSTicket, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w1.Code)

	w2 := ts.DoRequest("POST", pathWSTicket, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w2.Code)

	var body1, body2 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	testhelpers.ParseJSON(t, w2, &body2)

	// Tickets should be different
	assert.NotEqual(t, body1["ticket"], body2["ticket"])

	// Both should be valid
	ctx := context.Background()
	userID1, _, err := auth.ValidateTicket(ctx, ts.Redis, body1["ticket"].(string))
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID1)

	userID2, _, err := auth.ValidateTicket(ctx, ts.Redis, body2["ticket"].(string))
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID2)
}

func TestValidateTicketSessionIDWithColons(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Session ID with colons (edge case: SplitN with N=2)
	ticket := "colontest12345678901234567890123456789012345678901234567890abcde"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, "user-x:session:with:colons", 30*time.Second).Err()
	require.NoError(t, err)

	userID, sessionID, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-x", userID)
	assert.Equal(t, "session:with:colons", sessionID)
}

func TestIssueTicketStoresWithTTL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticket4")
	ctx := context.Background()

	w := ts.DoRequest("POST", pathWSTicket, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ticket := body["ticket"].(string)

	// Check TTL on the Redis key
	key := fmt.Sprintf("%s%s", wsTicketKeyPrefix, ticket)
	ttl, err := ts.Redis.TTL(ctx, key).Result()
	require.NoError(t, err)
	assert.True(t, ttl > 0, "ticket should have a TTL")
	assert.True(t, ttl <= 30*time.Second, "ticket TTL should not exceed 30 seconds")
}
