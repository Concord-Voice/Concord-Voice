package websocket

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const loginFamilySocketSecret = "login-family-socket-test-secret"

type loginFamilySocketRig struct {
	db                *sql.DB
	h                 *Handler
	user, family, sid uuid.UUID
	expiry            time.Time
}

func loginFamilyRig(t *testing.T) loginFamilySocketRig {
	t.Helper()
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit isolated database required; never use default DB")
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)
	user, family, sid := uuid.New(), uuid.New(), uuid.New()
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	_, err := db.Exec(`INSERT INTO users(id,email,username,password_hash,email_verified,age_verified) VALUES($1,$2,$3,'test-hash',true,true)`, user, user.String()+"@family.test", user.String())
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, family, user)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES($1,$2,$3,$4)`, sid, family, user, expiry)
	require.NoError(t, err)
	return loginFamilySocketRig{db: db, h: NewHandler(NewHub(db, rdb), db, rdb, loginFamilySocketSecret, nil, nil, nil), user: user, family: family, sid: sid, expiry: expiry}
}
func (r loginFamilySocketRig) token(t *testing.T, user, sid uuid.UUID, epoch string, expiry time.Time) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": user.String(), "sid": sid.String(), "cred_epoch": epoch, "iss": middleware.AccessTokenIssuer, "email_verified": true, "exp": expiry.Unix(), "iat": time.Now().Unix()}).SignedString([]byte(loginFamilySocketSecret))
	require.NoError(t, err)
	return token
}
func (r loginFamilySocketRig) terminal(t *testing.T) {
	t.Helper()
	_, err := r.db.Exec(`UPDATE login_families SET terminal_at=NOW(),epoch_generation=$2 WHERE family_id=$1`, r.family, uuid.New())
	require.NoError(t, err)
}
func familyAuthContext(path string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, path, nil)
	return c
}

func TestLoginFamilyBearerResolvesExactPersistedIdentity(t *testing.T) {
	r := loginFamilyRig(t)
	valid := r.token(t, r.user, r.sid, "", r.expiry)
	verified, err := r.h.validateAccessToken(context.Background(), valid)
	require.NoError(t, err)
	assert.Equal(t, r.family, verified.FamilyID)
	cases := []struct {
		name      string
		user, sid uuid.UUID
		epoch     string
		expiry    time.Time
	}{
		{"missing membership", r.user, uuid.New(), "", r.expiry},
		{"foreign user", uuid.New(), r.sid, "", r.expiry},
		{"signed expiry differs from membership", r.user, r.sid, "", r.expiry.Add(time.Second)},
		{"credential epoch mismatch", r.user, r.sid, strings.Repeat("1", 32), r.expiry},
		{"expired token", r.user, r.sid, "", time.Now().Add(-time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.h.validateAccessToken(context.Background(), r.token(t, tc.user, tc.sid, tc.epoch, tc.expiry))
			require.Error(t, err)
		})
	}
	r.terminal(t)
	_, err = r.h.validateAccessToken(context.Background(), valid)
	require.Error(t, err, "terminal SQL family must invalidate an otherwise valid bearer")
}

func TestLoginFamilyTicketConsumptionIndependentlyResolvesSQL(t *testing.T) {
	r := loginFamilyRig(t)
	ticket := func() string {
		id := uuid.NewString()
		value := fmt.Sprintf("%s:%s::%d", r.user, r.sid, r.expiry.Unix())
		require.Len(t, strings.Split(value, ":"), 4)
		require.NoError(t, r.h.redis.Set(context.Background(), "ws_ticket:"+id, value, time.Minute).Err())
		return id
	}
	first := ticket()
	user, sid, err := r.h.authenticateWebSocket(familyAuthContext("/ws?ticket=" + first))
	require.NoError(t, err)
	assert.Equal(t, r.user, user)
	assert.Equal(t, r.sid.String(), sid)
	_, _, err = r.h.authenticateWebSocket(familyAuthContext("/ws?ticket=" + first))
	require.Error(t, err, "ticket remains single use")
	last := ticket()
	r.terminal(t)
	_, _, err = r.h.authenticateWebSocket(familyAuthContext("/ws?ticket=" + last))
	require.Error(t, err, "four ticket fields cannot override terminal SQL authority")
}

func TestLoginFamilyPostRunAdmissionRejectsFamilyTerminatedWhilePending(t *testing.T) {
	r := loginFamilyRig(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws", r.h.HandleWebSocket)
	srv := httptest.NewServer(router)
	defer srv.Close()
	header := http.Header{"Authorization": []string{"Bearer " + r.token(t, r.user, r.sid, "", r.expiry)}}
	// The 101 has occurred, but the unbuffered registration cannot finish until Run starts.
	peer, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", header)
	require.NoError(t, err)
	defer func() { _ = peer.Close() }()
	r.terminal(t)
	go r.h.hub.Run()
	t.Cleanup(r.h.hub.Shutdown)
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, payload, err := peer.ReadMessage()
	require.Error(t, err, "pumps must not publish a frame after terminal pending admission: %s", payload)
	var timeout interface{ Timeout() bool }
	if e, ok := err.(interface{ Timeout() bool }); ok {
		timeout = e
	}
	if timeout != nil {
		assert.False(t, timeout.Timeout(), "must observe actual transport close, not merely no frames")
	}
}

type familyRenewalAuthority struct {
	verified     accessTokenAuth
	calls        atomic.Int32
	finalEntered chan struct{}
	finalRelease <-chan struct{}
	finalErr     error
}

func (a *familyRenewalAuthority) validateAccessToken(ctx context.Context, _ string) (accessTokenAuth, error) {
	if a.calls.Add(1) > 1 {
		if a.finalEntered != nil {
			close(a.finalEntered)
		}
		if a.finalRelease != nil {
			select {
			case <-a.finalRelease:
			case <-ctx.Done():
				return accessTokenAuth{}, ctx.Err()
			}
		}
		if a.finalErr != nil {
			return accessTokenAuth{}, a.finalErr
		}
	}
	return a.verified, nil
}
func familyRenewalFixture(t *testing.T) (*Client, *familyRenewalAuthority, *Hub) {
	t.Helper()
	conn, _, _ := familyInstrumentedSocket(t)
	c := familyPublishedClient(uuid.New(), uuid.New(), conn)
	c.CredEpoch = ""
	c.asyncWg.Add(1)
	hub := familyHubWithPublished(t, c)
	t.Cleanup(c.asyncWg.Done)
	a := &familyRenewalAuthority{verified: accessTokenAuth{UserID: c.UserID, FamilyID: c.FamilyID, SessionID: uuid.NewString(), CredEpoch: c.CredEpoch, ExpiresAt: c.TokenExpiresAt.Add(time.Hour)}}
	c.renewAuthority = a
	return c, a, hub
}
func TestLoginFamilyRenewalSameFamilyHasNoRefreshPredecessorDependency(t *testing.T) {
	r := loginFamilyRig(t)
	conn, _, _ := familyInstrumentedSocket(t)
	c := familyPublishedClient(r.user, r.family, conn)
	c.SessionID = r.sid.String()
	c.TokenExpiresAt = r.expiry
	c.renewAuthority = r.h
	c.asyncWg.Add(1)
	hub := r.h.hub
	hub.clients[c.ID] = c
	hub.userClients[c.UserID] = map[uuid.UUID]bool{c.ID: true}
	c.Hub = hub
	go hub.Run()
	t.Cleanup(hub.Shutdown)
	t.Cleanup(c.asyncWg.Done)
	nextSID := uuid.New()
	nextExpiry := r.expiry.Add(time.Hour)
	_, err := r.db.Exec(`INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES($1,$2,$3,$4)`, nextSID, r.family, r.user, nextExpiry)
	require.NoError(t, err)
	var rows int
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE id=$1 OR id=$2`, r.sid, nextSID).Scan(&rows))
	require.Zero(t, rows, "neither refresh predecessor row exists")
	require.True(t, c.renewToken(r.token(t, r.user, nextSID, "", nextExpiry)))
	assert.True(t, c.tokenDeadline().Equal(nextExpiry))
	hub.mu.RLock()
	sid := c.SessionID
	hub.mu.RUnlock()
	assert.Equal(t, nextSID.String(), sid)
	assert.Equal(t, r.family, c.FamilyID)
}
func TestLoginFamilyRenewalDeniesMismatchesAndUpdatesDeadlineLast(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Client, *familyRenewalAuthority)
	}{
		{"different family", func(_ *Client, a *familyRenewalAuthority) { a.verified.FamilyID = uuid.New() }},
		{"different user", func(_ *Client, a *familyRenewalAuthority) { a.verified.UserID = uuid.New() }},
		{"different epoch", func(_ *Client, a *familyRenewalAuthority) { a.verified.CredEpoch = strings.Repeat("1", 32) }},
		{"nonlater expiry", func(c *Client, a *familyRenewalAuthority) { a.verified.ExpiresAt = c.tokenDeadline() }},
		{"expired socket", func(c *Client, _ *familyRenewalAuthority) { c.TokenExpiresAt = time.Now().Add(-time.Minute) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, a, _ := familyRenewalFixture(t)
			tc.mutate(c, a)
			before := c.tokenDeadline()
			require.False(t, c.renewToken("test-family-renew"))
			assert.True(t, before.Equal(c.tokenDeadline()))
		})
	}
	t.Run("expiry stays old until final authority succeeds", func(t *testing.T) {
		c, a, hub := familyRenewalFixture(t)
		before := c.tokenDeadline()
		release := make(chan struct{})
		a.finalEntered = make(chan struct{})
		a.finalRelease = release
		done := make(chan bool, 1)
		go func() { done <- c.renewToken("test-family-renew") }()
		familyAwait(t, a.finalEntered, "final SQL authority recheck after rebind")
		assert.True(t, before.Equal(c.tokenDeadline()))
		hub.mu.RLock()
		sid := c.SessionID
		hub.mu.RUnlock()
		assert.Equal(t, a.verified.SessionID, sid, "Run rebound before final check")
		close(release)
		select {
		case ok := <-done:
			require.True(t, ok)
		case <-time.After(time.Second):
			t.Fatal("renewal did not finish")
		}
		assert.True(t, a.verified.ExpiresAt.Equal(c.tokenDeadline()))
	})
}
