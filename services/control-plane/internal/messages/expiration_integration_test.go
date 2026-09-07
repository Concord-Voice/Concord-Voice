//go:build integration

package messages_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelMessageExpirationREST_StampAndHistory(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "messageexpiration")
	serverID := ts.CreateTestServer(t, user.ID, "Message Expiration Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]interface{}{
		"channel_id": channelID, "content": testhelpers.ValidCiphertext(), "key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	message := testhelpers.JSONField[map[string]interface{}](t, body, "message")
	assert.Nil(t, message["expires_at"], "NULL policy must preserve a NULL expiry")

	_, err := ts.DB.Exec(`UPDATE channels SET expiration_window_seconds = 3600, expiration_revision = 1 WHERE id = $1`, channelID)
	require.NoError(t, err)
	w = ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]interface{}{
		"channel_id": channelID, "content": testhelpers.ValidCiphertext(), "key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	testhelpers.ParseJSON(t, w, &body)
	message = testhelpers.JSONField[map[string]interface{}](t, body, "message")
	created := testhelpers.JSONField[string](t, message, "created_at")
	expires := testhelpers.JSONField[string](t, message, "expires_at")
	assert.NotEmpty(t, created)
	assert.NotEmpty(t, expires)

	var stored sql.NullTime
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM messages WHERE id = $1`, testhelpers.JSONField[string](t, message, "id")).Scan(&stored))
	assert.True(t, stored.Valid)

	w = ts.DoRequest(http.MethodGet, "/api/v1/channels/"+channelID+"/messages", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	testhelpers.ParseJSON(t, w, &body)
	messages := testhelpers.JSONField[[]interface{}](t, body, "messages")
	require.NotEmpty(t, messages)
	first := testhelpers.JSONAs[map[string]interface{}](t, messages[0], "history message")
	assert.Equal(t, expires, testhelpers.JSONField[string](t, first, "expires_at"))
}

func TestChannelMessageExpirationREST_InvalidAndUnauthorized(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "messageexpirationerrors")
	serverID := ts.CreateTestServer(t, user.ID, "Message Expiration Errors")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]interface{}{
		"channel_id": "not-a-uuid", "content": testhelpers.ValidCiphertext(), "key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w = ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]interface{}{
		"channel_id": channelID, "content": testhelpers.ValidCiphertext(), "key_version": 1,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestChannelMessageSendProgressesUnderSharedChannelLock(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "expirationreadlock")
	serverID := ts.CreateTestServer(t, user.ID, "Expiration Read Lock")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	probe.SetMaxOpenConns(4)
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	require.NoError(t, probe.Ping())
	barrier, err := probe.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := barrier.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("barrier rollback: %v", rollbackErr)
		}
	})
	var txID int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&txID))
	var locked string
	require.NoError(t, barrier.QueryRow(`SELECT id FROM channels WHERE id = $1 FOR SHARE`, channelID).Scan(&locked))

	sendDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		sendDone <- ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]interface{}{
			"channel_id": channelID, "content": testhelpers.ValidCiphertext(), "key_version": 1,
		}, testhelpers.AuthHeaders(user.AccessToken))
	}()
	var sendResponse *httptest.ResponseRecorder
	select {
	case sendResponse = <-sendDone:
	case <-time.After(5 * time.Second):
		t.Fatal("message send did not progress under FOR SHARE")
	}
	require.Equal(t, http.StatusCreated, sendResponse.Code, sendResponse.Body.String())
	var envelope map[string]interface{}
	testhelpers.ParseJSON(t, sendResponse, &envelope)
	message := testhelpers.JSONField[map[string]interface{}](t, envelope, "message")
	messageID := testhelpers.JSONField[string](t, message, "id")
	var storedChannel string
	require.NoError(t, probe.QueryRow(`SELECT channel_id FROM messages WHERE id = $1`, messageID).Scan(&storedChannel))
	assert.Equal(t, channelID, storedChannel)

	setterDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		setterDone <- ts.DoRequest(http.MethodPatch, "/api/v1/channels/"+channelID+"/expiration", map[string]interface{}{
			"mode": "set", "window_seconds": 3600, "retroactive": "new_only",
		}, testhelpers.AuthHeaders(user.AccessToken))
	}()
	dbtest.WaitForRowLockWaiter(t, probe, txID)
	select {
	case setterResponse := <-setterDone:
		t.Fatalf("policy setter completed while FOR SHARE was held: %s", setterResponse.Body.String())
	default:
	}
	require.NoError(t, barrier.Commit())
	var setterResponse *httptest.ResponseRecorder
	select {
	case setterResponse = <-setterDone:
	case <-time.After(5 * time.Second):
		t.Fatal("policy setter did not complete after FOR SHARE release")
	}
	assert.Equal(t, http.StatusOK, setterResponse.Code, setterResponse.Body.String())
}
