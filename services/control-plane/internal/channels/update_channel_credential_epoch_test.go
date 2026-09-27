package channels_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reset-first stale-cache window admits this request at HTTP middleware;
// UpdateChannel's in-transaction fence must reject it and leave the row intact.
func TestUpdateChannelRejectsStaleCredentialEpoch(t *testing.T) {
	ts, owner, _, channelID := setupWithChannel(t)
	var originalName string
	require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channels WHERE id = $1`, channelID).Scan(&originalName))

	stale := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+channelID, map[string]string{
		"name": "must-not-commit",
		"type": "text",
	}, testhelpers.AuthHeaders(stale))

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
	var actualName string
	require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channels WHERE id = $1`, channelID).Scan(&actualName))
	assert.Equal(t, originalName, actualName)
}
