package api_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
)

func TestVoiceJoinRateLimitCountsVerifiedServiceHopsAsLogicalJoins(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	createVoiceChannel := func(username string) (testhelpers.TestUser, string) {
		t.Helper()
		owner := ts.CreateTestUser(t, username)
		serverID := ts.CreateTestServer(t, owner.ID, username+" server")
		channelID := ts.CreateTestChannel(t, serverID, username+" voice")
		_, err := ts.DB.Exec(`UPDATE channels SET type = 'voice' WHERE id = $1`, channelID)
		require.NoError(t, err)
		return owner, "/api/v1/channels/" + channelID + "/voice/join"
	}

	hopHeaders := func(token, path string) http.Header {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		proof := mediaproof.Sign(
			mediaproof.DeriveKey(testhelpers.TestJWTSecret, "concord/media-plane-service-hop/v1"),
			"v1", timestamp, http.MethodPost, path, mediaproof.TokenDigest(token),
		)
		require.NotEmpty(t, proof)
		headers := testhelpers.AuthHeaders(token)
		headers.Set("X-Concord-Service-Timestamp", timestamp)
		headers.Set("X-Concord-Service-Proof", proof)
		return headers
	}

	t.Run("twenty verified service hops fit and the twenty-first is rejected", func(t *testing.T) {
		owner, path := createVoiceChannel("voice_join_service_limit")
		for attempt := 0; attempt < 10; attempt++ {
			admission := map[string]interface{}{
				"admission_id": "24e27f2b-1d45-4fcf-90c3-3f0ff028a910",
				"socket_id":    "rate-limit-socket",
				"activate":     false,
			}
			first := ts.DoRequest(http.MethodPost, path, admission, hopHeaders(owner.AccessToken, path))
			require.Equal(t, http.StatusOK, first.Code, "verified service hop %d", 2*attempt+1)
			assert.Equal(t, "20", first.Header().Get("X-RateLimit-Limit"))

			second := ts.DoRequest(http.MethodPost, path, admission, hopHeaders(owner.AccessToken, path))
			require.Equal(t, http.StatusOK, second.Code, "verified service hop %d", 2*attempt+2)
		}

		twentyFirst := ts.DoRequest(http.MethodPost, path, nil, hopHeaders(owner.AccessToken, path))
		assert.Equal(t, http.StatusTooManyRequests, twentyFirst.Code)
	})

	t.Run("invalid service proof remains in the direct-client budget", func(t *testing.T) {
		owner, path := createVoiceChannel("voice_join_invalid_hop_limit")
		for request := 0; request < 9; request++ {
			response := ts.DoRequest(http.MethodPost, path, nil, testhelpers.AuthHeaders(owner.AccessToken))
			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, "10", response.Header().Get("X-RateLimit-Limit"))
		}

		forgedHeaders := hopHeaders(owner.AccessToken, path)
		proof := forgedHeaders.Get("X-Concord-Service-Proof")
		require.NotEmpty(t, proof)
		prefix := "0"
		if proof[0] == '0' {
			prefix = "1"
		}
		forgedHeaders.Set("X-Concord-Service-Proof", prefix+proof[1:])

		tenth := ts.DoRequest(http.MethodPost, path, nil, forgedHeaders)
		assert.Equal(t, http.StatusOK, tenth.Code)
		assert.Equal(t, "10", tenth.Header().Get("X-RateLimit-Limit"))

		eleventh := ts.DoRequest(http.MethodPost, path, nil, forgedHeaders)
		assert.Equal(t, http.StatusTooManyRequests, eleventh.Code)
	})
}
