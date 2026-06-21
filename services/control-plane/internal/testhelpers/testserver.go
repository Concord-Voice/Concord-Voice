package testhelpers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/api"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	// headerContentType is the HTTP Content-Type header name, extracted so the
	// literal is defined once and referenced at every test-helper call site.
	headerContentType = "Content-Type"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// SyncBuffer is a goroutine-safe wrapper around bytes.Buffer. The structured
// logger (*slog.Logger) can be used concurrently from handler goroutines, so
// the writer it points at must serialize writes. Reads are also guarded so
// CaptureLogs callers don't race against an in-flight request log.
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer. Safe to call concurrently.
func (s *SyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// String returns the captured log output. Safe to call concurrently with Write.
func (s *SyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Reset discards any captured log output. Useful for scoping a test to only
// the log lines emitted after a particular setup step.
func (s *SyncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

// TestServer wraps a fully-wired Gin router with a real DB and Redis for integration testing.
type TestServer struct {
	Router *gin.Engine
	Hub    *websocket.Hub
	DB     *sql.DB
	Redis  *redis.Client

	// logBuf captures all structured-log output emitted by handlers during the
	// test. It is written alongside os.Stdout via an io.MultiWriter so human
	// readers of `go test -v` still see log lines. Tests that need to assert
	// on log contents should use CaptureLogs.
	logBuf *SyncBuffer
}

// SetupTestServer creates a test server backed by a real database and Redis.
func SetupTestServer(t *testing.T) *TestServer {
	t.Helper()

	// Ensure handlers that gate test-only behaviour (e.g. writing the plaintext
	// verification code to Redis) activate correctly in integration tests.
	t.Setenv("CONCORD_ENV", "test")

	db, dbCleanup := SetupTestDB(t)
	redisClient, redisCleanup := SetupTestRedis(t)

	cfg := &config.Config{
		Environment:       "test",
		Port:              "0",
		JWTSecret:         TestJWTSecret,
		AllowedOrigins:    []string{"*"},
		MFAEncryptionKey:  "0000000000000000000000000000000000000000000000000000000000000000",
		WebAuthnRPID:      "localhost",
		WebAuthnRPOrigins: []string{"http://localhost:3001"},
	}

	// Route logs through a MultiWriter: humans running `go test -v` still see
	// output on stdout, while tests that need to audit log contents can reach
	// the in-memory buffer via CaptureLogs.
	logBuf := &SyncBuffer{}
	log := logger.NewWithWriter(io.MultiWriter(os.Stdout, logBuf))

	router, hub, natsClient := api.NewRouter(db, redisClient, nil, cfg, nil, log)
	if natsClient != nil {
		t.Cleanup(func() { natsClient.Close() })
	}

	t.Cleanup(func() {
		redisCleanup()
		dbCleanup()
	})

	// Shut down the hub goroutine and wait for Run() to exit BEFORE Redis/DB
	// are torn down. t.Cleanup runs in LIFO order, so registered-last runs first.
	t.Cleanup(func() { hub.Shutdown() })

	return &TestServer{
		Router: router,
		Hub:    hub,
		DB:     db,
		Redis:  redisClient,
		logBuf: logBuf,
	}
}

// CaptureLogs clears the in-memory log buffer and returns a handle tests can
// read from via .String() after driving the code under test. Log output is
// ALSO written to os.Stdout so `go test -v` remains useful for humans.
//
// Typical use:
//
//	logs := ts.CaptureLogs(t)
//	ts.DoRequest(...)
//	require.NotContains(t, logs.String(), "wrapped_key")
//
// Intended for tests that assert sensitive material (wrapped keys, tokens,
// crypto outputs) does NOT reach logs — see [internal]rules/e2ee.md.
func (ts *TestServer) CaptureLogs(t *testing.T) *SyncBuffer {
	t.Helper()
	if ts.logBuf == nil {
		t.Fatal("testhelpers: CaptureLogs called on a TestServer that was not built by SetupTestServer")
	}
	// Reset so only log output from this point forward is captured.
	ts.logBuf.Reset()
	return ts.logBuf
}

// AuthHeaders returns an Authorization header map for the given user.
func AuthHeaders(accessToken string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set(headerContentType, "application/json")
	return h
}

// insertE2EEKeys inserts E2EE key material for a test user.
func (ts *TestServer) insertE2EEKeys(t *testing.T, userID string) {
	t.Helper()
	pubKey, wrappedKey, saltB64 := E2EETestKeys()
	pubKeyBytes, _ := base64.StdEncoding.DecodeString(pubKey)
	wrappedKeyBytes, _ := base64.StdEncoding.DecodeString(wrappedKey)
	saltBytes, _ := base64.StdEncoding.DecodeString(saltB64)

	_, err := ts.DB.Exec(
		`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt) VALUES ($1, $2, $3)`,
		userID, wrappedKeyBytes, saltBytes,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create user keys: %v", err)
	}

	keyID := uuid.New().String()
	_, err = ts.DB.Exec(
		`INSERT INTO public_keys (id, user_id, public_key, key_version) VALUES ($1, $2, $3, 1)`,
		keyID, userID, pubKeyBytes,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create public key: %v", err)
	}
}

// createTestUserWithVerification inserts a user and returns a TestUser.
// Shared implementation for CreateTestUser and CreateTestUserUnverified.
func (ts *TestServer) createTestUserWithVerification(t *testing.T, username string, emailVerified bool) TestUser {
	t.Helper()

	userID := uuid.New().String()
	email := username + "@test.concord.chat"
	normalizedUsername := auth.NormalizeUsername(username)

	_, err := ts.DB.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, $4, true, $5)`,
		userID, email, normalizedUsername, TestAuthHash, emailVerified,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create test user: %v", err)
	}

	ts.insertE2EEKeys(t, userID)

	accessToken, err := auth.GenerateAccessToken(userID, TestJWTSecret, emailVerified)
	if err != nil {
		t.Fatalf("testhelpers: failed to generate access token: %v", err)
	}

	return TestUser{
		ID:          userID,
		Email:       email,
		Username:    normalizedUsername,
		Password:    TestAuthPlaintext,
		AccessToken: accessToken,
	}
}

// CreateTestUser inserts a user directly into the database and returns a TestUser
// with a valid JWT access token. Uses a pre-computed password hash for speed.
func (ts *TestServer) CreateTestUser(t *testing.T, username string) TestUser {
	t.Helper()
	return ts.createTestUserWithVerification(t, username, true)
}

// VerifyUserEmail marks a user's email as verified in the DB and returns a new
// access token with email_verified=true. Use this after API-based registration
// in tests that need to hit verified-only endpoints.
func (ts *TestServer) VerifyUserEmail(t *testing.T, userID string) string {
	t.Helper()

	_, err := ts.DB.Exec(`UPDATE users SET email_verified = true WHERE id = $1`, userID)
	if err != nil {
		t.Fatalf("testhelpers: failed to verify user email: %v", err)
	}

	token, err := auth.GenerateAccessToken(userID, TestJWTSecret, true)
	if err != nil {
		t.Fatalf("testhelpers: failed to generate verified token: %v", err)
	}
	return token
}

// CreateTestServer creates a server owned by the given user and returns its ID.
func (ts *TestServer) CreateTestServer(t *testing.T, ownerID, name string) string {
	t.Helper()

	serverID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, $2, $3)`,
		serverID, name, ownerID,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create test server: %v", err)
	}

	_, err = ts.DB.Exec(
		`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`,
		serverID, ownerID,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to add owner to server: %v", err)
	}

	// Create @all default role with base permissions (RBAC)
	allRoleID := uuid.New().String()
	_, err = ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		 VALUES ($1, $2, '@all', 0, $3, TRUE, TRUE)`,
		allRoleID, serverID, int64(rbac.BasePermissions),
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create @all role: %v", err)
	}

	// Assign @all role to the owner
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, ownerID, allRoleID,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to assign @all role: %v", err)
	}

	return serverID
}

// CreateTestChannel creates a channel in the given server and returns its ID.
// All channels are encrypted under E2EE-everywhere (#201).
func (ts *TestServer) CreateTestChannel(t *testing.T, serverID, name string) string {
	t.Helper()

	channelID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'text')`,
		channelID, serverID, name,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create test channel: %v", err)
	}

	return channelID
}

// AddMemberToServer adds a user to a server with the given role.
func (ts *TestServer) AddMemberToServer(t *testing.T, serverID, userID, role string) {
	t.Helper()

	_, err := ts.DB.Exec(
		`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, $3)`,
		serverID, userID, role,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to add member to server: %v", err)
	}

	// Assign all default roles (including @all) to the new member
	_, err = ts.DB.Exec(`
		INSERT INTO member_roles (server_id, user_id, role_id)
		SELECT $1, $2, id FROM roles
		WHERE server_id = $1 AND is_default = TRUE
		ON CONFLICT DO NOTHING
	`, serverID, userID)
	if err != nil {
		t.Fatalf("testhelpers: failed to assign default roles: %v", err)
	}

	// Admins need an RBAC role with AdminPermissions (resolver uses roles table, not legacy role column)
	if role == "admin" {
		adminRoleID := uuid.New().String()
		_, err = ts.DB.Exec(
			`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
			 VALUES ($1, $2, 'admin', 10, $3, FALSE, TRUE)
			 ON CONFLICT DO NOTHING`,
			adminRoleID, serverID, int64(rbac.AdminPermissions),
		)
		if err != nil {
			t.Fatalf("testhelpers: failed to create admin role: %v", err)
		}
		_, err = ts.DB.Exec(
			`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			serverID, userID, adminRoleID,
		)
		if err != nil {
			t.Fatalf("testhelpers: failed to assign admin role: %v", err)
		}
	}
}

// DoRequest performs an HTTP request against the test router and returns the response.
func (ts *TestServer) DoRequest(method, path string, body interface{}, headers http.Header) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		jsonBytes, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(jsonBytes)
	}

	req := httptest.NewRequest(method, path, bodyReader)
	if headers != nil {
		req.Header = headers
	}
	if body != nil && req.Header.Get(headerContentType) == "" {
		req.Header.Set(headerContentType, "application/json")
	}

	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	return w
}

// ParseJSON parses the response body into the given target.
func ParseJSON(t *testing.T, w *httptest.ResponseRecorder, target interface{}) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(target); err != nil {
		t.Fatalf("testhelpers: failed to parse JSON response: %v", err)
	}
}

// CreateTestUserUnverified inserts a user with email_verified=false and returns a
// TestUser with a JWT that has the email_verified=false claim.
func (ts *TestServer) CreateTestUserUnverified(t *testing.T, username string) TestUser {
	t.Helper()
	return ts.createTestUserWithVerification(t, username, false)
}

// CreateFriendship inserts a friendship row between two users with the given status.
func (ts *TestServer) CreateFriendship(t *testing.T, user1ID, user2ID, status string) {
	t.Helper()

	_, err := ts.DB.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, $3)`,
		user1ID, user2ID, status,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create friendship: %v", err)
	}
}

// CreateDMConversation creates a DM conversation between two users and returns its ID.
// All DMs are encrypted under E2EE-everywhere (#201).
func (ts *TestServer) CreateDMConversation(t *testing.T, user1ID, user2ID string) string {
	t.Helper()

	convID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO dm_conversations (id, is_group, is_personal, created_by)
		 VALUES ($1, false, false, $2)`,
		convID, user1ID,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create DM conversation: %v", err)
	}

	_, err = ts.DB.Exec(
		`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`,
		convID, user1ID, user2ID,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to add DM participants: %v", err)
	}

	return convID
}

// CreateGroupDMConversation creates an is_group=TRUE DM conversation with the
// given members (first = owner/creator) and returns the conversation ID.
// All DMs are encrypted under E2EE-everywhere (#201).
func (ts *TestServer) CreateGroupDMConversation(t *testing.T, memberIDs ...string) string {
	t.Helper()

	if len(memberIDs) == 0 {
		t.Fatalf("testhelpers: CreateGroupDMConversation requires at least one member")
	}

	convID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO dm_conversations (id, is_group, is_personal, created_by)
		 VALUES ($1, true, false, $2)`,
		convID, memberIDs[0],
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create group DM conversation: %v", err)
	}

	for _, m := range memberIDs {
		_, err = ts.DB.Exec(
			`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`,
			convID, m,
		)
		if err != nil {
			t.Fatalf("testhelpers: failed to add group DM participant %s: %v", m, err)
		}
	}

	return convID
}

// SeedDMKey inserts a DM channel key for a user in a conversation at the given version.
func (ts *TestServer) SeedDMKey(t *testing.T, conversationID, userID string, keyVersion int) {
	t.Helper()

	wrappedKey := []byte("test-wrapped-key-" + conversationID)
	_, err := ts.DB.Exec(
		`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, $3, $4)`,
		conversationID, userID, wrappedKey, keyVersion,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to seed DM key: %v", err)
	}
}

// SeedDMKeyRevocation inserts a dm_key_revocations row for testing the
// REVOKED_EPOCH code path in GetUnifiedKeys. The caller supplies the revoked
// epoch and successor epoch; reason defaults to "test".
func (ts *TestServer) SeedDMKeyRevocation(t *testing.T, conversationID string, revokedEpoch, successorEpoch int) {
	t.Helper()

	_, err := ts.DB.Exec(
		`INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason)
		 VALUES ($1, $2, $3, 'test')`,
		conversationID, revokedEpoch, successorEpoch,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to seed DM key revocation: %v", err)
	}
}

// CreateTestRole creates a custom role in a server and returns the role ID.
func (ts *TestServer) CreateTestRole(t *testing.T, serverID, name string, position int, permissions int64) string {
	t.Helper()

	roleID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		 VALUES ($1, $2, $3, $4, $5, FALSE, FALSE)`,
		roleID, serverID, name, position, permissions,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create test role: %v", err)
	}
	return roleID
}

// AssignRoleToUser assigns an existing role to a user in a server.
func (ts *TestServer) AssignRoleToUser(t *testing.T, serverID, userID, roleID string) {
	t.Helper()

	_, err := ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		serverID, userID, roleID,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to assign role to user: %v", err)
	}
}

// CreateChannelOverride creates an SBAC channel permission override.
func (ts *TestServer) CreateChannelOverride(t *testing.T, channelID, targetType, targetID string, allow, deny int64) {
	t.Helper()

	overrideID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		overrideID, channelID, targetType, targetID, allow, deny,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create channel override: %v", err)
	}
}

// CreateTestMessage inserts a message row directly via SQL and returns the message ID.
// Bypasses the SendMessage API (which enforces ciphertext shape under E2EE-everywhere
// #201) so callers can supply arbitrary plaintext content for assertion purposes.
func (ts *TestServer) CreateTestMessage(t *testing.T, channelID string, user TestUser, content string) string {
	t.Helper()

	messageID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 1, FALSE, NOW(), NOW())`,
		messageID, channelID, user.ID, content,
	)
	if err != nil {
		t.Fatalf("testhelpers: CreateTestMessage failed to insert message: %v", err)
	}

	return messageID
}

// FetchVerificationCode retrieves the plaintext verification code stored in Redis
// under "test_only:<pendingID>" by the registration handler (only written when
// CONCORD_ENV=test). Fails the test immediately if the key is absent.
func FetchVerificationCode(t *testing.T, ts *TestServer, pendingID string) string {
	t.Helper()
	code, err := ts.Redis.Get(context.Background(), "test_only:"+pendingID).Result()
	if err != nil {
		t.Fatalf("testhelpers: FetchVerificationCode: key test_only:%s not found in Redis: %v", pendingID, err)
	}
	return code
}

// CreateVoiceChannel creates a voice channel in the given server and returns its ID.
// All channels are encrypted under E2EE-everywhere (#201).
func (ts *TestServer) CreateVoiceChannel(t *testing.T, serverID, name string) string {
	t.Helper()

	channelID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'voice')`,
		channelID, serverID, name,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create voice channel: %v", err)
	}
	return channelID
}
