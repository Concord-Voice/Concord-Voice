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

// The 2026-09-02 voice outage, through the PRODUCTION router.
//
// Every other test of this fix builds its own middleware chain by hand, so all
// of them stay green if router.go's wiring changes. This one goes through
// bindRouter, so it exercises the real middleware order, the real route
// patterns, and the real handler — the three things the outage was actually
// about. The wiring test pins the source-text ordering; this pins the behaviour
// that ordering produces.
//
// It is also the only test in the repo that runs with CLIENT_MIN_VERSION set.
// SetupTestServer leaves it empty by default, so EnforceClientVersion returns
// true before doing anything and the entire integration suite runs with the
// gate disabled — which is why the defect was invisible to it.
func TestVoiceJoinUnderClientVersionGate(t *testing.T) {
	t.Setenv("TEST_CLIENT_MIN_VERSION", "0.2.44")
	ts := testhelpers.SetupTestServer(t)

	owner := ts.CreateTestUser(t, "hopowner")
	serverID := ts.CreateTestServer(t, owner.ID, "hop-server")
	channelID := ts.CreateTestChannel(t, serverID, "hop-voice")
	_, err := ts.DB.Exec(`UPDATE channels SET type = 'voice' WHERE id = $1`, channelID)
	require.NoError(t, err)

	path := "/api/v1/channels/" + channelID + "/voice/join"

	hopHeaders := func(token string) http.Header {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		proof := mediaproof.Sign(
			mediaproof.DeriveKey(testhelpers.TestJWTSecret, "concord/media-plane-service-hop/v1"),
			"v1", timestamp, http.MethodPost, path, mediaproof.TokenDigest(token),
		)
		require.NotEmpty(t, proof)
		h := testhelpers.AuthHeaders(token)
		h.Set("X-Concord-Service-Timestamp", timestamp)
		h.Set("X-Concord-Service-Proof", proof)
		return h
	}

	t.Run("the outage: a header-less hop with no proof is refused", func(t *testing.T) {
		w := ts.DoRequest("POST", path, nil, testhelpers.AuthHeaders(owner.AccessToken))

		assert.Equal(t, http.StatusForbidden, w.Code,
			"this is what 403'd every voice join on 2026-09-02")
	})

	t.Run("a proven hop joins with no client-version header", func(t *testing.T) {
		w := ts.DoRequest("POST", path, nil, hopHeaders(owner.AccessToken))

		assert.Equal(t, http.StatusOK, w.Code,
			"the service-hop proof is what restores voice, with no client change")
	})

	t.Run("an honest client with a current version still joins", func(t *testing.T) {
		headers := testhelpers.AuthHeaders(owner.AccessToken)
		headers.Set("X-Concord-Client-Version", "0.2.44")

		w := ts.DoRequest("POST", path, nil, headers)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("a genuinely old client is still refused — the gate still works", func(t *testing.T) {
		headers := testhelpers.AuthHeaders(owner.AccessToken)
		headers.Set("X-Concord-Client-Version", "0.2.43")

		w := ts.DoRequest("POST", path, nil, headers)

		assert.Equal(t, http.StatusForbidden, w.Code,
			"exempting the hop must not disarm the gate for real clients")
	})

	t.Run("a forged proof earns nothing", func(t *testing.T) {
		headers := hopHeaders(owner.AccessToken)
		// Derive the mutation from the original so the forgery ALWAYS differs.
		// Overwriting with a fixed prefix silently no-ops whenever the genuine
		// proof already starts with it — a ~1/256 spurious pass that would look
		// like a real one.
		valid := headers.Get("X-Concord-Service-Proof")
		flipped := "f"
		if valid[0] == 'f' {
			flipped = "0"
		}
		headers.Set("X-Concord-Service-Proof", flipped+valid[1:])
		require.NotEqual(t, valid, headers.Get("X-Concord-Service-Proof"))

		w := ts.DoRequest("POST", path, nil, headers)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}
