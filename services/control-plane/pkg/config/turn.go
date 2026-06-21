package config

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- SHA1 is required by the TURN REST API credential spec (RFC 5389)
	"encoding/base64"
	"fmt"
	"time"
)

// TURNCredentials holds time-limited HMAC credentials for coturn.
type TURNCredentials struct {
	Username   string `json:"username"`
	Credential string `json:"credential"`
}

// ICEServer represents a single ICE server entry returned to clients.
type ICEServer struct {
	URLs       string `json:"urls"`
	Username   string `json:"username,omitempty"`
	Credential string `json:"credential,omitempty"`
}

// GenerateTURNCredentials creates ephemeral HMAC credentials for coturn's
// use-auth-secret mode. The username encodes the expiry timestamp and user ID;
// the credential is an HMAC-SHA1 of the username using the shared secret.
// Credentials are valid for the specified TTL (typically 24h — generous because
// coturn checks at relay-allocation time, not mid-stream).
func (c *Config) GenerateTURNCredentials(userID string, ttl time.Duration) TURNCredentials {
	expiry := time.Now().Add(ttl).Unix()
	username := fmt.Sprintf("%d:%s", expiry, userID)

	mac := hmac.New(sha1.New, []byte(c.TURNSecret))
	mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return TURNCredentials{
		Username:   username,
		Credential: credential,
	}
}

// ICEServers returns the full list of ICE server entries for a voice join
// response. If TURN is configured, it includes STUN + TURN + TURNS endpoints
// on the coturn server with ephemeral credentials. Otherwise falls back to
// STUN-only from the coturn host (or localhost for dev).
func (c *Config) ICEServers(userID string) []ICEServer {
	// TURN not configured — dev/test fallback with just STUN
	if c.TURNServerHost == "" || c.TURNSecret == "" {
		host := c.TURNServerHost
		if host == "" {
			host = "localhost"
		}
		return []ICEServer{
			{URLs: fmt.Sprintf("stun:%s:3478", host)},
		}
	}

	creds := c.GenerateTURNCredentials(userID, 24*time.Hour)

	return []ICEServer{
		{URLs: fmt.Sprintf("stun:%s:3478", c.TURNServerHost)},
		{URLs: fmt.Sprintf("turn:%s:3478", c.TURNServerHost), Username: creds.Username, Credential: creds.Credential},
		{URLs: fmt.Sprintf("turn:%s:3478?transport=tcp", c.TURNServerHost), Username: creds.Username, Credential: creds.Credential},
		{URLs: fmt.Sprintf("turns:%s:5349", c.TURNServerHost), Username: creds.Username, Credential: creds.Credential},
	}
}
