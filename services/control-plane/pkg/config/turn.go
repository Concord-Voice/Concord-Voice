package config

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- SHA1 is required by the TURN REST API credential spec (RFC 5389)
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"syscall"
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

// GenerateTURNCredentials creates legacy static-key credentials for coturn's
// use-auth-secret mode. Managed admissions use ICEServersChecked instead. The
// username encodes the expiry timestamp and user ID; the credential is an
// HMAC-SHA1 of the username using the shared secret.
// Credentials are valid for the specified TTL (typically 24h — generous because
// coturn checks at relay-allocation time, not mid-stream).
func (c *Config) GenerateTURNCredentials(userID string, ttl time.Duration) TURNCredentials {
	return generateTURNCredentials(c.TURNSecret, userID, ttl)
}

func generateTURNCredentials(secret, userID string, ttl time.Duration) TURNCredentials {
	expiry := time.Now().Add(ttl).Unix()
	username := fmt.Sprintf("%d:%s", expiry, userID)

	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return TURNCredentials{
		Username:   username,
		Credential: credential,
	}
}

// ICEServers is the legacy static-key path. Voice join handlers use
// ICEServersChecked so managed custody failures cannot fall back to STUN.
func (c *Config) ICEServers(userID string) []ICEServer {
	return c.iceServersForSecret(userID, c.TURNSecret)
}

// ICEServersChecked reads the managed active key for each admission. An absent
// snapshot stops admission instead of silently returning a STUN-only response.
func (c *Config) ICEServersChecked(userID string) ([]ICEServer, error) {
	if c.TURNSecretFile == "" {
		return c.ICEServers(userID), nil
	}
	if c.TURNSecret != "" || c.TURNServerHost == "" {
		return nil, fmt.Errorf("managed TURN configuration is incomplete or conflicting")
	}
	// O_NONBLOCK so a FIFO (direct or via symlink) cannot park the admission
	// handler in open(2) waiting for a writer; the IsRegular check then rejects it.
	f, err := os.OpenFile(c.TURNSecretFile, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("managed TURN active key unavailable: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("managed TURN active key metadata unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 || info.Size() < 32 || info.Size() > 128 {
		return nil, fmt.Errorf("managed TURN active key has invalid metadata")
	}
	key, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil {
		return nil, fmt.Errorf("managed TURN active key read failed: %w", err)
	}
	if int64(len(key)) != info.Size() || len(key) < 32 || len(key) > 128 {
		return nil, fmt.Errorf("managed TURN active key changed during read")
	}
	for _, ch := range key {
		if ch < '!' || ch > '~' {
			return nil, fmt.Errorf("managed TURN active key has invalid content")
		}
	}
	return c.iceServersForSecret(userID, string(key)), nil
}

func (c *Config) iceServersForSecret(userID, secret string) []ICEServer {
	// TURN not configured — dev/test fallback with just STUN
	if c.TURNServerHost == "" || secret == "" {
		host := c.TURNServerHost
		if host == "" {
			host = "localhost"
		}
		return []ICEServer{
			{URLs: fmt.Sprintf("stun:%s:3478", host)},
		}
	}

	creds := generateTURNCredentials(secret, userID, 24*time.Hour)

	return []ICEServer{
		{URLs: fmt.Sprintf("stun:%s:3478", c.TURNServerHost)},
		{URLs: fmt.Sprintf("turn:%s:3478", c.TURNServerHost), Username: creds.Username, Credential: creds.Credential},
		{URLs: fmt.Sprintf("turn:%s:3478?transport=tcp", c.TURNServerHost), Username: creds.Username, Credential: creds.Credential},
		{URLs: fmt.Sprintf("turns:%s:5349", c.TURNServerHost), Username: creds.Username, Credential: creds.Credential},
	}
}
