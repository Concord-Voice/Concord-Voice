package config

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- SHA1 required by TURN RFC; mirroring the implementation
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	turnHost   = "turn.concordvoice.chat"
	turnSecret = "test-secret"
	turnUser1  = "user-1"
)

// ─── GenerateTURNCredentials ──────────────────────────────────────────────────

func TestGenerateTURNCredentialsUsernameFormat(t *testing.T) {
	cfg := &Config{TURNSecret: turnSecret}
	creds := cfg.GenerateTURNCredentials("user-123", time.Hour)

	parts := strings.SplitN(creds.Username, ":", 2)
	require.Len(t, parts, 2, "username must be 'expiry:userID'")
	assert.NotEmpty(t, parts[0], "expiry timestamp must be non-empty")
	assert.Equal(t, "user-123", parts[1])
}

func TestGenerateTURNCredentialsCredentialIsBase64(t *testing.T) {
	cfg := &Config{TURNSecret: turnSecret}
	creds := cfg.GenerateTURNCredentials(turnUser1, time.Hour)

	decoded, err := base64.StdEncoding.DecodeString(creds.Credential)
	require.NoError(t, err, "credential must be valid base64")
	assert.Len(t, decoded, 20, "HMAC-SHA1 output is 20 bytes")
}

func TestGenerateTURNCredentialsHMACIsCorrect(t *testing.T) {
	cfg := &Config{TURNSecret: "fixed-secret"}
	creds := cfg.GenerateTURNCredentials("testuser", 24*time.Hour)

	// Re-derive the expected credential from the generated username
	mac := hmac.New(sha1.New, []byte("fixed-secret"))
	mac.Write([]byte(creds.Username))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	assert.Equal(t, expected, creds.Credential)
}

func TestGenerateTURNCredentialsDifferentSecrets(t *testing.T) {
	cfg1 := &Config{TURNSecret: "secret-A"}
	cfg2 := &Config{TURNSecret: "secret-B"}

	// Generate near-simultaneously; both usernames embed expiry so may differ,
	// but even if expiry matches, the HMAC key differs → different credential.
	creds1 := cfg1.GenerateTURNCredentials("sameuser", time.Hour)
	creds2 := cfg2.GenerateTURNCredentials("sameuser", time.Hour)

	assert.NotEqual(t, creds1.Credential, creds2.Credential)
}

func TestGenerateTURNCredentialsDifferentUsers(t *testing.T) {
	cfg := &Config{TURNSecret: turnSecret}

	creds1 := cfg.GenerateTURNCredentials("alice", time.Hour)
	creds2 := cfg.GenerateTURNCredentials("bob", time.Hour)

	assert.NotEqual(t, creds1.Username, creds2.Username)
	assert.NotEqual(t, creds1.Credential, creds2.Credential)
}

func TestGenerateTURNCredentialsTTLAffectsUsername(t *testing.T) {
	cfg := &Config{TURNSecret: turnSecret}

	// One second apart — expiry timestamps will differ
	creds1 := cfg.GenerateTURNCredentials("user", 1*time.Hour)
	creds2 := cfg.GenerateTURNCredentials("user", 48*time.Hour)

	// Different TTLs → different expiry → different username
	assert.NotEqual(t, creds1.Username, creds2.Username)
}

// ─── ICEServers ───────────────────────────────────────────────────────────────

func TestICEServersNoConfig(t *testing.T) {
	cfg := &Config{} // both TURNServerHost and TURNSecret empty

	servers := cfg.ICEServers(turnUser1)

	require.Len(t, servers, 1)
	assert.Equal(t, "stun:localhost:3478", servers[0].URLs)
	assert.Empty(t, servers[0].Username)
	assert.Empty(t, servers[0].Credential)
}

func TestICEServersHostSetButNoSecret(t *testing.T) {
	cfg := &Config{TURNServerHost: "turn.example.com"} // no TURNSecret

	servers := cfg.ICEServers(turnUser1)

	require.Len(t, servers, 1, "missing secret should fall back to STUN-only")
	assert.Equal(t, "stun:turn.example.com:3478", servers[0].URLs)
}

func TestICEServersSecretSetButNoHost(t *testing.T) {
	cfg := &Config{TURNSecret: "some-secret"} // no TURNServerHost

	servers := cfg.ICEServers(turnUser1)

	require.Len(t, servers, 1)
	assert.Equal(t, "stun:localhost:3478", servers[0].URLs, "missing host falls back to localhost")
}

func TestICEServersFullConfigReturnsFourServers(t *testing.T) {
	cfg := &Config{
		TURNServerHost: turnHost,
		TURNSecret:     turnSecret,
	}

	servers := cfg.ICEServers("user-99")

	require.Len(t, servers, 4)
}

func TestICEServersFullConfigURIFormats(t *testing.T) {
	cfg := &Config{
		TURNServerHost: turnHost,
		TURNSecret:     turnSecret,
	}

	servers := cfg.ICEServers("user-99")

	assert.Equal(t, "stun:"+turnHost+":3478", servers[0].URLs)
	assert.Equal(t, "turn:"+turnHost+":3478", servers[1].URLs)
	assert.Equal(t, "turn:"+turnHost+":3478?transport=tcp", servers[2].URLs)
	assert.Equal(t, "turns:"+turnHost+":5349", servers[3].URLs)
}

func TestICEServersFullConfigSTUNHasNoCredentials(t *testing.T) {
	cfg := &Config{
		TURNServerHost: turnHost,
		TURNSecret:     turnSecret,
	}

	servers := cfg.ICEServers(turnUser1)

	assert.Empty(t, servers[0].Username, "STUN entry must not carry credentials")
	assert.Empty(t, servers[0].Credential)
}

func TestICEServersFullConfigTURNHasCredentials(t *testing.T) {
	cfg := &Config{
		TURNServerHost: turnHost,
		TURNSecret:     turnSecret,
	}

	servers := cfg.ICEServers(turnUser1)

	for i := 1; i <= 3; i++ {
		assert.NotEmpty(t, servers[i].Username, "TURN server %d must have username", i)
		assert.NotEmpty(t, servers[i].Credential, "TURN server %d must have credential", i)
	}
}

func TestICEServersFullConfigAllTURNShareSameCredentials(t *testing.T) {
	cfg := &Config{
		TURNServerHost: turnHost,
		TURNSecret:     turnSecret,
	}

	servers := cfg.ICEServers("user-42")

	// All TURN entries (indices 1-3) should share a single credential pair.
	assert.Equal(t, servers[1].Username, servers[2].Username)
	assert.Equal(t, servers[1].Credential, servers[2].Credential)
	assert.Equal(t, servers[1].Username, servers[3].Username)
	assert.Equal(t, servers[1].Credential, servers[3].Credential)
}
