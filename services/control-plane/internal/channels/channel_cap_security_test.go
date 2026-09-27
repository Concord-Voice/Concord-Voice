package channels_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedTextChannels(t *testing.T, ts *testhelpers.TestServer, serverID string, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id := uuid.NewString()
		_, err := ts.DB.Exec(`
			INSERT INTO channels (id, server_id, name, type, position)
			VALUES ($1, $2, $3, 'text', $4)`, id, serverID, fmt.Sprintf("seed-%d", i), i)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	return ids
}

func createTextChannelRequest(t *testing.T, ts *testhelpers.TestServer, owner testhelpers.TestUser, serverID, name string) *httptest.ResponseRecorder {
	t.Helper()
	return ts.DoRequest(http.MethodPost, pathChannels, map[string]interface{}{
		"server_id": serverID,
		"name":      name,
		"type":      "text",
		"wrapped_keys": map[string]string{
			owner.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
}

func TestCreateChannel_499ExistingText_AllowsOneMoreText(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cap-text-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "cap-text-server")
	seedTextChannels(t, ts, serverID, 499)

	w := createTextChannelRequest(t, ts, owner, serverID, "cap-500")
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, 500, rowCount(t, ts, `SELECT COUNT(*) FROM channels WHERE server_id = $1`, serverID))
}

func TestCreateChannel_499ExistingVoicePair_RejectsAtomically(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cap-voice-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "cap-voice-server")
	seedTextChannels(t, ts, serverID, 499)

	w := ts.DoRequest(http.MethodPost, pathChannels, map[string]interface{}{
		"server_id": serverID,
		"name":      "cap-voice",
		"type":      "voice",
		"wrapped_keys": map[string]string{
			owner.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, 499, rowCount(t, ts, `SELECT COUNT(*) FROM channels WHERE server_id = $1`, serverID))
	assert.Equal(t, 0, rowCount(t, ts, `SELECT COUNT(*) FROM channels WHERE server_id = $1 AND name = 'cap-voice'`, serverID))
}

func TestCreateChannel_ConcurrentCreatesCannotExceedServerCap(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cap-race-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "cap-race-server")
	seedTextChannels(t, ts, serverID, 499)

	start := make(chan struct{})
	responses := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			responses <- createTextChannelRequest(t, ts, owner, serverID, fmt.Sprintf("cap-race-%d", i)).Code
		}(i)
	}
	close(start)
	wg.Wait()
	close(responses)

	statuses := []int{<-responses, <-responses}
	assert.ElementsMatch(t, []int{http.StatusCreated, http.StatusConflict}, statuses)
	assert.Equal(t, 500, rowCount(t, ts, `SELECT COUNT(*) FROM channels WHERE server_id = $1`, serverID))
}

func TestCreateChannel_LegacyOverCap_BlocksCreateButAllowsDelete(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "legacy-cap-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "legacy-cap-server")
	ids := seedTextChannels(t, ts, serverID, 501)

	w := createTextChannelRequest(t, ts, owner, serverID, "legacy-blocked")
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, 501, rowCount(t, ts, `SELECT COUNT(*) FROM channels WHERE server_id = $1`, serverID))

	w = ts.DoRequest(http.MethodDelete, pathChannelsPrefix+ids[0], nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 500, rowCount(t, ts, `SELECT COUNT(*) FROM channels WHERE server_id = $1`, serverID))
}
