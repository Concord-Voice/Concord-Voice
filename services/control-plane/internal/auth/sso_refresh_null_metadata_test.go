package auth_test

// regression for #3290

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSORefresh_NullDeviceMetadata_Returns200 reproduces #3290: an SSO-minted
// refresh token (device_name/ip_address/user_agent all SQL NULL by design —
// see oauth_adapter.go's issueAccessAndRefresh) is rejected by its first
// POST /api/v1/auth/refresh with a 500, because fetchActiveRefreshToken Scans
// those NULL columns into plain string fields without COALESCE.
//
// ORACLE: an SSO-minted refresh token returns 200 from POST
// /api/v1/auth/refresh, rather than 500 with "converting NULL to string is
// unsupported".
func TestSSORefresh_NullDeviceMetadata_Returns200(t *testing.T) {
	ts := setupTS(t)

	user := ts.CreateTestUser(t, "ssorefreshuser")

	log := logger.New("test")
	h := auth.NewHandler(ts.DB, ts.Redis, log, testhelpers.TestJWTSecret, nil)

	// Mint a session through the REAL SSO write path — not a hand-written
	// INSERT and not internal/oauth's fakeAuthAdapter, both of which bypass
	// the exact code path that produces the NULL columns.
	_, refreshToken, sessionID, err := h.IssueAccessAndRefresh(context.Background(), user.ID)
	require.NoError(t, err, "SSO session mint must succeed")
	require.NotEmpty(t, refreshToken)

	// Pin the premise: the minted row really does have NULL device metadata.
	// If this fails, the fixture doesn't reproduce the bug and the rest of
	// the test proves nothing.
	var deviceNameNull, ipAddressNull, userAgentNull bool
	err = ts.DB.QueryRow(
		`SELECT device_name IS NULL, ip_address IS NULL, user_agent IS NULL
		 FROM refresh_tokens WHERE id = $1`,
		sessionID,
	).Scan(&deviceNameNull, &ipAddressNull, &userAgentNull)
	require.NoError(t, err, "must be able to read back the minted refresh_tokens row")
	require.True(t, deviceNameNull, "premise: SSO-minted refresh_tokens.device_name must be NULL")
	require.True(t, ipAddressNull, "premise: SSO-minted refresh_tokens.ip_address must be NULL")
	require.True(t, userAgentNull, "premise: SSO-minted refresh_tokens.user_agent must be NULL")
	machineID := "8f2a7d4e-7f08-4eb2-9f0c-8fef5bf7ab27"

	logs := ts.CaptureLogs(t)

	headers := http.Header{}
	headers.Set("X-Refresh-Token", refreshToken)
	headers.Set("User-Agent", "sso-test-agent")
	headers.Set("X-Machine-Id", machineID)
	w := ts.DoRequest("POST", "/api/v1/auth/refresh", nil, headers)

	if w.Code != http.StatusOK {
		// Distinguish the real defect (NULL-scan failure) from an unrelated
		// 500 (e.g. a setup error), so a failure here is unambiguous evidence.
		require.Contains(t, logs.String(), "converting NULL to string",
			"a 500 here must be caused by the NULL device-metadata scan, not something else")
	}

	require.Equal(t, http.StatusOK, w.Code,
		"an SSO session must survive its first refresh; a 500 here is the NULL metadata scan (fetchActiveRefreshToken Scans NULL device_name/ip_address/user_agent into plain string fields)")

	var response struct {
		RefreshToken      string `json:"refresh_token"`
		PreviousSessionID string `json:"previous_session_id"`
	}
	testhelpers.ParseJSON(t, w, &response)
	require.NotEmpty(t, response.RefreshToken)
	require.Equal(t, sessionID, response.PreviousSessionID)

	var successorID, deviceName, ipAddress, successorMachineID string
	var userAgent sql.NullString
	err = ts.DB.QueryRow(
		`SELECT id, device_name, host(ip_address), user_agent, machine_id
		 FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&successorID, &deviceName, &ipAddress, &userAgent, &successorMachineID)
	require.NoError(t, err)
	require.NotEqual(t, sessionID, successorID)
	assert.NotEmpty(t, ipAddress)
	assert.True(t, userAgent.Valid)
	assert.Empty(t, deviceName, "SSO device name remains absent through rotation")
	assert.Equal(t, machineID, successorMachineID)

	// The rotated pair is usable: a second refresh consumes the exact successor.
	second := ts.DoRequest("POST", "/api/v1/auth/refresh", nil, http.Header{"X-Refresh-Token": []string{response.RefreshToken}, "User-Agent": []string{"sso-test-agent"}, "X-Machine-Id": []string{machineID}})
	assert.Equal(t, http.StatusOK, second.Code)
}
