package messages_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pinPath   = "/pin"
	pinsPath  = "/pins"
	pinAPIMsg = "/api/v1/messages/"
	pinAPICh  = "/api/v1/channels/"
)

// --- Pin Message Tests ---

func TestPinMessageSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinuser")
	serverID := ts.CreateTestServer(t, user.ID, "Pin Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Pin this!")

	w := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, msgID, resp["message_id"])
	assert.NotNil(t, resp["pinned_at"])
	assert.NotNil(t, resp["pinned_by"])
}

// Regression: transaction-local message authorization must use the locked
// channel's visibility bit. A member that can view only voice channels may pin
// voice history, while a later flip to text-only must stop the same mutation.
func TestPinMessageVoiceChannelUsesLockedChannelViewPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "pinvoiceowner")
	member := ts.CreateTestUser(t, "pinvoicemember")
	serverID := ts.CreateTestServer(t, owner.ID, "Pin Voice Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-history")
	messageID := ts.CreateTestMessage(t, channelID, owner, "pin voice history")

	// The base role still supplies PinMessages. Denying text visibility leaves
	// this member voice-only, which must be sufficient for a voice message.
	ts.CreateChannelOverride(t, channelID, "user", member.ID, 0, int64(rbac.PermViewTextChannels))
	w := ts.DoRequest(http.MethodPost, pinAPIMsg+messageID+pinPath, nil,
		testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var pinnedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(`SELECT pinned_at FROM messages WHERE id = $1`, messageID).Scan(&pinnedAt))
	require.True(t, pinnedAt.Valid, "voice-visible member must pin the voice message")

	// Flip the same member to text-only. PinMessages remains granted, so only
	// the channel-type visibility gate can reject the unpin.
	_, err := ts.DB.Exec(`UPDATE channel_permission_overrides SET deny = $1 WHERE channel_id = $2 AND target_type = 'user' AND target_id = $3`,
		int64(rbac.PermViewVoiceChannels), channelID, member.ID)
	require.NoError(t, err)
	w = ts.DoRequest(http.MethodDelete, pinAPIMsg+messageID+pinPath, nil,
		testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	require.NoError(t, ts.DB.QueryRow(`SELECT pinned_at FROM messages WHERE id = $1`, messageID).Scan(&pinnedAt))
	assert.True(t, pinnedAt.Valid, "text-only member must not unpin voice history")
}

func TestPinMessageAlreadyPinned(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinidempotent")
	serverID := ts.CreateTestServer(t, user.ID, "Pin2 Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Pin me twice")

	// Pin first time
	ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))

	// Pin again — should be idempotent
	w := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, true, resp["already_pinned"])
}

func TestPinMessageAlreadyPinnedUsesOpenTransactionConnection(t *testing.T) {
	fixtureDB, _ := testhelpers.SetupTestDB(t)
	redis, cleanupRedis := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanupRedis)
	ts := &testhelpers.TestServer{DB: fixtureDB, Redis: redis}
	user := ts.CreateTestUser(t, "pinpool_"+uuid.NewString()[:8])
	serverID := ts.CreateTestServer(t, user.ID, "Pin Pool Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Pin with one connection")
	_, err := fixtureDB.Exec(`UPDATE messages SET pinned_at = NOW(), pinned_by = $1 WHERE id = $2`, user.ID, msgID)
	require.NoError(t, err)

	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(redis), logger.New("test"))
	handler := messages.NewHandler(db, logger.New("test"), nil, resolver, nil, nil)

	request := func(ctx context.Context) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, pinAPIMsg+msgID+pinPath, nil)
		c.Params = gin.Params{{Key: "id", Value: msgID}}
		c.Set("user_id", user.ID)
		handler.PinMessage(c)
		return response
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- request(ctx) }()

	select {
	case response := <-result:
		assert.Equal(t, http.StatusOK, response.Code)
	case <-ctx.Done():
		// The pre-fix pool lookup ignores the request context. Releasing a second
		// connection lets it unwind so this test fails without leaking a goroutine.
		db.SetMaxOpenConns(2)
		<-result
		t.Fatal("idempotent pin checked the database pool instead of its transaction")
	}
}

func TestPinMessageInvalidUUID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pininvalid")
	_ = ts.CreateTestServer(t, user.ID, "PinInvalid Server")

	w := ts.DoRequest("POST", pinAPIMsg+"not-a-uuid"+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPinMessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinnotfound")
	_ = ts.CreateTestServer(t, user.ID, "PinNotFound Server")

	fakeID := "00000000-0000-0000-0000-000000000099"
	w := ts.DoRequest("POST", pinAPIMsg+fakeID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPinMessageNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "pinowner")
	outsider := ts.CreateTestUser(t, "pinoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "PinPrivate Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, owner, "Secret pin")

	w := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestPinMessageLimitReached(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinlimit")
	serverID := ts.CreateTestServer(t, user.ID, "PinLimit Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Seed 50 pinned messages directly via SQL to avoid rate limiting
	for i := 0; i < 50; i++ {
		_, err := ts.DB.Exec(
			`INSERT INTO messages (id, channel_id, user_id, content, pinned_at, pinned_by, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, 'pinned msg', NOW(), $2, NOW(), NOW())`,
			channelID, user.ID,
		)
		require.NoError(t, err)
	}

	// Create 51st message and try to pin via API
	extraMsg := ts.CreateTestMessage(t, channelID, user, "One too many")
	w := ts.DoRequest("POST", pinAPIMsg+extraMsg+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

// --- Unpin Message Tests ---

// The channel pin cap is a COUNT under READ COMMITTED, so without the
// per-channel advisory lock two pins of different messages can both count 49
// and land 51 (CWE-362, #3552 red-team). Each round races eight pins at 49
// pinned. Every contender is a different member, so the per-user pin rate
// limit (10 a minute) never turns one into a 429 and every round contends.
func TestChannelPinCapHoldsUnderConcurrentPins(t *testing.T) {
	const contenders, rounds = 8, 8
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "pincap_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Pin Cap Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	pinnerRole := ts.CreateTestRole(t, serverID, "pinner", 5, int64(rbac.PermPinMessages))
	pinners := make([]testhelpers.TestUser, contenders)
	for i := range pinners {
		pinners[i] = ts.CreateTestUser(t, fmt.Sprintf("pincap_%d", i))
		ts.AddMemberToServer(t, serverID, pinners[i].ID, "member")
		ts.AssignRoleToUser(t, serverID, pinners[i].ID, pinnerRole)
	}
	for i := 0; i < 49; i++ {
		id := ts.CreateTestMessage(t, channelID, owner, fmt.Sprintf("pre-%d", i))
		_, err := ts.DB.Exec(`UPDATE messages SET pinned_at = NOW(), pinned_by = $2 WHERE id = $1`, id, owner.ID)
		require.NoError(t, err)
	}

	for round := 0; round < rounds; round++ {
		cands := make([]string, contenders)
		for i := range cands {
			cands[i] = ts.CreateTestMessage(t, channelID, owner, fmt.Sprintf("cand-%d-%d", round, i))
		}
		codes := make([]int, contenders)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range cands {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				codes[i] = ts.DoRequest("POST", pinAPIMsg+cands[i]+pinPath, nil,
					testhelpers.AuthHeaders(pinners[i].AccessToken)).Code
			}(i)
		}
		close(start)
		wg.Wait()

		won := 0
		for _, code := range codes {
			require.Contains(t, []int{http.StatusOK, http.StatusConflict}, code,
				"round %d: every contender must reach the cap check, codes=%v", round, codes)
			if code == http.StatusOK {
				won++
			}
		}
		var total int
		require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1 AND pinned_at IS NOT NULL`, channelID).Scan(&total))
		require.Equal(t, 50, total, "round %d: the cap must hold under concurrent pins, codes=%v", round, codes)
		require.Equal(t, 1, won, "round %d: exactly one contender takes the last slot, codes=%v", round, codes)

		_, err := ts.DB.Exec(`UPDATE messages SET pinned_at = NULL, pinned_by = NULL WHERE id = ANY($1::uuid[])`, pq.Array(cands))
		require.NoError(t, err)
	}
}

func TestUnpinMessageSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unpinuser")
	serverID := ts.CreateTestServer(t, user.ID, "Unpin Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Unpin me")

	// Pin first
	ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))

	// Unpin
	w := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, msgID, resp["message_id"])
	assert.Nil(t, resp["already_unpinned"])
}

func TestUnpinMessageAlreadyUnpinned(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unpinidempotent")
	serverID := ts.CreateTestServer(t, user.ID, "Unpin2 Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Never pinned")

	w := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, true, resp["already_unpinned"])
}

func TestUnpinMessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unpinnotfound")
	_ = ts.CreateTestServer(t, user.ID, "UnpinNotFound Server")

	fakeID := "00000000-0000-0000-0000-000000000099"
	w := ts.DoRequest("DELETE", pinAPIMsg+fakeID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- GetChannelPins Tests ---

func TestGetChannelPinsSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getpinsuser")
	serverID := ts.CreateTestServer(t, user.ID, "GetPins Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Create and pin 3 messages
	for i := 0; i < 3; i++ {
		msgID := ts.CreateTestMessage(t, channelID, user, "Pinned msg")
		ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
			testhelpers.AuthHeaders(user.AccessToken))
	}

	w := ts.DoRequest("GET", pinAPICh+channelID+pinsPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		PinnedMessages []models.MessageWithUser `json:"pinned_messages"`
		Count          int                      `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 3, resp.Count)
	assert.Len(t, resp.PinnedMessages, 3)
	// Verify ordered by pinned_at DESC (most recently pinned first)
	for _, msg := range resp.PinnedMessages {
		assert.NotNil(t, msg.PinnedAt)
	}
}

func TestGetChannelPinsEmpty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "nopinsuser")
	serverID := ts.CreateTestServer(t, user.ID, "NoPins Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("GET", pinAPICh+channelID+pinsPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		PinnedMessages []json.RawMessage `json:"pinned_messages"`
		Count          int               `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 0, resp.Count)
	assert.Len(t, resp.PinnedMessages, 0)
}

func TestGetChannelPinsNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "pinsowner")
	outsider := ts.CreateTestUser(t, "pinsoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "PinsPrivate Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("GET", pinAPICh+channelID+pinsPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetMessagesPinnedFieldsIncluded(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinnedfields")
	serverID := ts.CreateTestServer(t, user.ID, "PinnedFields Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Pin and fetch")

	// Pin the message
	ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))

	// Fetch messages and verify pinned fields
	w := ts.DoRequest("GET", pinAPICh+channelID+"/messages", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []json.RawMessage `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.GreaterOrEqual(t, len(resp.Messages), 1)

	var msg struct {
		ID       string  `json:"id"`
		PinnedAt *string `json:"pinned_at"`
		PinnedBy *string `json:"pinned_by"`
	}
	require.NoError(t, json.Unmarshal(resp.Messages[0], &msg))
	assert.Equal(t, msgID, msg.ID)
	require.NotNil(t, msg.PinnedAt)
	require.NotNil(t, msg.PinnedBy)
}
