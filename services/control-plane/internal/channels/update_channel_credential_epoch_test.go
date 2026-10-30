package channels_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real family auth admits this request before the epoch rotation commits;
// UpdateChannel's in-transaction fence must reject it and leave the row intact.
func TestUpdateChannelRejectsStaleCredentialEpoch(t *testing.T) {
	boundaryTS, boundary := testhelpers.SetupStaleBoundaryServer(t)
	ts, owner, _, channelID := setupWithChannel(t, boundaryTS)
	var originalName string
	require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channels WHERE id = $1`, channelID).Scan(&originalName))

	require.NoError(t, boundary.Arm(owner.AccessToken))
	boundary.Hook.ArmArg([]string{channelEpochGuardSQL}, owner.ID, nil, nil)
	w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+channelID, map[string]string{
		"name": "must-not-commit",
		"type": "text",
	}, testhelpers.AuthHeaders(owner.AccessToken))

	boundary.RequireRotated(t)
	seen, hookErr := boundary.Hook.Report()
	require.NoError(t, hookErr)
	require.Equal(t, 1, seen, "handler GuardTx must be reached after admitted epoch rotation")
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
	var actualName string
	require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channels WHERE id = $1`, channelID).Scan(&actualName))
	assert.Equal(t, originalName, actualName)
}
