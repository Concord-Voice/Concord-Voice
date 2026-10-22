package joinpause

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const testUser = "11111111-2222-4333-8444-555555555555"

func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

func refuse(t *testing.T, rdb *redis.Client, userID string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return recorder, Refuse(c, rdb, nil, userID)
}

func TestReadParsesEveryKeyShape(t *testing.T) {
	server, rdb := newRedis(t)
	cases := []struct {
		value string
		want  State
	}{
		{"", State{}},
		{"1700000000:1700007200", State{Active: true, Deadline: 1700007200}},
		{"1700000000:1700007200:" + testUser, State{Active: true, Deadline: 1700007200, allowUser: testUser}},
		{"garbage", State{Active: true}},
		{"1700000000:1700007200:NOT-A-UUID", State{Active: true}},
	}
	for _, tc := range cases {
		server.Del(Key)
		if tc.value != "" {
			require.NoError(t, server.Set(Key, tc.value))
		}
		got, err := Read(t.Context(), rdb)
		require.NoError(t, err, tc.value)
		require.Equal(t, tc.want, got, tc.value)
	}
}

func TestReadNilClientIsInactiveAndRedisErrorIsReturned(t *testing.T) {
	got, err := Read(t.Context(), nil)
	require.NoError(t, err)
	require.False(t, got.Active)

	_, rdb := newRedis(t)
	require.NoError(t, rdb.Close())
	_, err = Read(t.Context(), rdb)
	require.Error(t, err)
}

func TestRefuseWrites503OnlyWhilePaused(t *testing.T) {
	server, rdb := newRedis(t)

	recorder, refused := refuse(t, rdb, testUser)
	require.False(t, refused)
	require.Equal(t, http.StatusOK, recorder.Code) // nothing written

	require.NoError(t, server.Set(Key, "1700000000:1700007200"))
	recorder, refused = refuse(t, rdb, testUser)
	require.True(t, refused)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, Code, body["code"])
}

func TestRefuseAdmitsOnlyTheAllowedTestUser(t *testing.T) {
	server, rdb := newRedis(t)
	require.NoError(t, server.Set(Key, "1700000000:1700007200:"+testUser))

	_, refused := refuse(t, rdb, testUser)
	require.False(t, refused)
	_, refused = refuse(t, rdb, "99999999-2222-4333-8444-555555555555")
	require.True(t, refused)
}

func TestRefuseFailsOpenWhenRedisIsUnreadable(t *testing.T) {
	lastDeadline.Store(0)
	server, rdb := newRedis(t)
	require.NoError(t, server.Set(Key, "1700000000:1700007200"))
	require.NoError(t, rdb.Close())
	_, refused := refuse(t, rdb, testUser)
	require.False(t, refused)
}

func TestRefuseKeepsRefusingThroughARedisBlipDuringAPause(t *testing.T) {
	t.Cleanup(func() { lastDeadline.Store(0) })
	server, rdb := newRedis(t)
	now := time.Now().Unix()
	require.NoError(t, server.Set(Key, fmt.Sprintf("%d:%d", now, now+7200)))
	_, refused := refuse(t, rdb, testUser)
	require.True(t, refused)

	server.SetError("blip")
	recorder, refused := refuse(t, rdb, testUser)
	require.True(t, refused)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	// A pause already past its deadline no longer holds through an error.
	lastDeadline.Store(now - 1)
	_, refused = refuse(t, rdb, testUser)
	require.False(t, refused)

	// A lifted pause clears the memory.
	server.SetError("")
	server.Del(Key)
	_, refused = refuse(t, rdb, testUser)
	require.False(t, refused)
	require.Zero(t, lastDeadline.Load())
}

func TestHandlerReportsStateAndUnavailableRedis(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server, rdb := newRedis(t)
	require.NoError(t, server.Set(Key, "1700000000:1700007200:"+testUser))
	router := gin.New()
	router.GET("/voice-join-pause", Handler(rdb))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/voice-join-pause", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"voice_join_pause":{"active":true,"deadline":1700007200}}`, recorder.Body.String())

	require.NoError(t, rdb.Close())
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/voice-join-pause", nil))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}
