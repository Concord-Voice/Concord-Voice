package managedredis

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var credentialTestNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func credentialTestScope() SecretScope {
	return SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
}

func credentialTestRaw(t *testing.T, scope SecretScope, generation uint64, username, password, rotationID string, validFrom, validUntil time.Time) []byte {
	t.Helper()
	value := map[string]any{
		"version": 1,
		"scope": map[string]string{
			"projectId": scope.Project, "environment": scope.Environment,
			"path": scope.Path, "name": scope.Name,
		},
		"rotationId": rotationID,
		"service":    "control-plane",
		"generation": generation,
		"username":   username,
		"password":   password,
		"validFrom":  validFrom.Format(time.RFC3339),
		"validUntil": validUntil.Format(time.RFC3339),
	}
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func TestParseCredentialAcceptsExactScopedCredential(t *testing.T) {
	scope := credentialTestScope()
	raw := credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", credentialTestNow.Add(-time.Minute), credentialTestNow.Add(time.Hour))

	got, err := ParseCredential(raw, scope, "control-plane", credentialTestNow)

	require.NoError(t, err)
	require.Equal(t, uint64(12), got.Generation)
	require.Equal(t, "rotation-12", got.RotationID)
	require.Equal(t, "cv_cp_12", got.Username)
	require.Equal(t, "synthetic-password", got.Password)
}

func TestParseCredentialRejectsDuplicateKeys(t *testing.T) {
	scope := credentialTestScope()
	raw := []byte(`{"version":1,"version":1,"scope":{"projectId":"project-test","environment":"prod","path":"/redis","name":"REDIS_CONTROL_PLANE_CREDENTIAL"},"rotationId":"rotation-12","service":"control-plane","generation":12,"username":"cv_cp_12","password":"synthetic-password","validFrom":"2026-10-17T11:59:00Z","validUntil":"2026-10-17T13:00:00Z"}`)

	_, err := ParseCredential(raw, scope, "control-plane", credentialTestNow)

	require.Error(t, err)
}

func TestParseCredentialRejectsUnknownFields(t *testing.T) {
	scope := credentialTestScope()
	var value map[string]any
	require.NoError(t, json.Unmarshal(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", credentialTestNow.Add(-time.Minute), credentialTestNow.Add(time.Hour)), &value))
	value["unexpected"] = true
	raw, err := json.Marshal(value)
	require.NoError(t, err)

	_, err = ParseCredential(raw, scope, "control-plane", credentialTestNow)

	require.Error(t, err)
}

func TestParseCredentialRejectsWrongService(t *testing.T) {
	scope := credentialTestScope()
	raw := credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", credentialTestNow.Add(-time.Minute), credentialTestNow.Add(time.Hour))

	_, err := ParseCredential(raw, scope, "media-plane", credentialTestNow)

	require.Error(t, err)
}

func TestParseCredentialRejectsEachScopeMismatch(t *testing.T) {
	base := credentialTestScope()
	for _, tc := range []struct {
		name   string
		mutate func(*SecretScope)
	}{
		{name: "project", mutate: func(s *SecretScope) { s.Project = "other-project" }},
		{name: "environment", mutate: func(s *SecretScope) { s.Environment = "staging" }},
		{name: "path", mutate: func(s *SecretScope) { s.Path = "/other" }},
		{name: "name", mutate: func(s *SecretScope) { s.Name = "OTHER_SECRET" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payloadScope := base
			tc.mutate(&payloadScope)
			raw := credentialTestRaw(t, payloadScope, 12, "cv_cp_12", "synthetic-password", "rotation-12", credentialTestNow.Add(-time.Minute), credentialTestNow.Add(time.Hour))

			_, err := ParseCredential(raw, base, "control-plane", credentialTestNow)

			require.Error(t, err)
		})
	}
}

func TestParseCredentialRejectsInvalidTimeBounds(t *testing.T) {
	scope := credentialTestScope()
	for _, tc := range []struct {
		name  string
		from  time.Time
		until time.Time
	}{
		{name: "not yet valid", from: credentialTestNow.Add(time.Second), until: credentialTestNow.Add(time.Hour)},
		{name: "already expired", from: credentialTestNow.Add(-time.Hour), until: credentialTestNow},
		{name: "nonpositive interval", from: credentialTestNow.Add(-time.Minute), until: credentialTestNow.Add(-time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", tc.from, tc.until)
			_, err := ParseCredential(raw, scope, "control-plane", credentialTestNow)
			require.Error(t, err)
		})
	}
}

func TestParseCredentialRejectsUnsafeUsername(t *testing.T) {
	scope := credentialTestScope()
	for _, username := range []string{"", "cv cp 12", "cv_cp_12 +@all", "cv_cp_12\nuser default on", "cv_cp_13", "cv_mp_12", "cv_controller_next"} {
		t.Run(username, func(t *testing.T) {
			raw := credentialTestRaw(t, scope, 12, username, "synthetic-password", "rotation-12", credentialTestNow.Add(-time.Minute), credentialTestNow.Add(time.Hour))
			_, err := ParseCredential(raw, scope, "control-plane", credentialTestNow)
			require.Error(t, err)
		})
	}
}

func TestParseCredentialRejectsInvalidRotationID(t *testing.T) {
	scope := credentialTestScope()
	raw := credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation/12", credentialTestNow.Add(-time.Minute), credentialTestNow.Add(time.Hour))

	_, err := ParseCredential(raw, scope, "control-plane", credentialTestNow)

	require.Error(t, err)
}

func TestParseCredentialRejectsGenerationBeyondMediaSafeInteger(t *testing.T) {
	scope := credentialTestScope()
	raw := credentialTestRaw(t, scope, maxJSONSafeGeneration+1, "cv_cp_12", "synthetic-password", "rotation-12", credentialTestNow.Add(-time.Minute), credentialTestNow.Add(time.Hour))
	_, err := ParseCredential(raw, scope, "control-plane", credentialTestNow)
	require.Error(t, err)
}
