package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
)

type oidcFixture struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	now    time.Time
	raw    []byte
	claims map[string]any
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	now := time.Unix(1_700_000_000, 0)
	payload := map[string]any{
		"caller_run_id":      12345,
		"caller_run_attempt": 2,
		"pr_number":          2949,
		"expected_head_sha":  strings.Repeat("a", 40),
		"expected_base_sha":  strings.Repeat("b", 40),
		"pr_ci_result":       "success",
		"selection":          map[string]any{"selected": map[string]any{"sonar": true}},
		"jobs": map[string]any{
			"ioc": "success", "build": "success", "semgrep": "success",
			"eslint": "success", "govulncheck": "success",
		},
		"coverage": map[string]any{
			"desktop": true, "control_plane": true, "media_plane": true, "admin": true,
		},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	digest := sha256.Sum256(raw)
	fixture := &oidcFixture{key: key, now: now, raw: raw}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			_, err := fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q,"id_token_signing_alg_values_supported":["RS256"]}`,
				fixture.server.URL, fixture.server.URL+"/keys")
			if err != nil {
				t.Errorf("write discovery: %v", err)
			}
		case "/keys":
			w.Header().Set("Content-Type", "application/json")
			err := json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
				Key: &fixture.key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig",
			}}})
			if err != nil {
				t.Errorf("write JWKS: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)
	fixture.claims = map[string]any{
		"iss": fixture.server.URL, "aud": audienceBase + hex.EncodeToString(digest[:]),
		"sub": "repo:Concord-Voice/Concord-Voice-Alpha:pull_request",
		// GitHub's live shape: nbf five minutes before iat, five-minute lifetime.
		"iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-6 * time.Minute).Unix(),
		"exp":           now.Add(4 * time.Minute).Unix(),
		"repository_id": repositoryID, "repository": repository,
		"event_name": "pull_request", "run_id": "12345", "run_attempt": "2",
		"workflow_ref": workflowRef, "workflow_sha": strings.Repeat("c", 40),
	}
	return fixture
}

func (f *oidcFixture) proof(t *testing.T, key *rsa.PrivateKey, claims map[string]any, payload []byte) []byte {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	require.NoError(t, err)
	body, err := json.Marshal(claims)
	require.NoError(t, err)
	signed, err := signer.Sign(body)
	require.NoError(t, err)
	token, err := signed.CompactSerialize()
	require.NoError(t, err)
	proof, err := json.Marshal(proofFile{
		PayloadB64: base64.StdEncoding.EncodeToString(payload),
		OIDCJWT:    token,
	})
	require.NoError(t, err)
	return proof
}

func TestVerifyProofAcceptsSignedCanonicalPayload(t *testing.T) {
	f := newOIDCFixture(t)
	raw := f.proof(t, f.key, f.claims, f.raw)
	out, err := verifyProof(context.Background(), raw, f.server.URL, func() time.Time { return f.now })
	require.NoError(t, err)
	require.NotContains(t, string(out), "oidc_jwt")
	var got verifiedOutput
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, f.raw, []byte(got.Payload))
	require.Equal(t, repositoryID, got.Claims.RepositoryID)
	require.Equal(t, "12345", got.Claims.RunID)
	require.Equal(t, workflowRef, got.Claims.WorkflowRef)
	require.Equal(t, strings.Repeat("c", 40), got.Claims.WorkflowSHA)
}

func TestVerifyProofRejectsBadProofs(t *testing.T) {
	f := newOIDCFixture(t)
	tests := []struct {
		name   string
		mutate func(*testing.T, map[string]any, *[]byte) *rsa.PrivateKey
	}{
		{"signature", func(t *testing.T, _ map[string]any, _ *[]byte) *rsa.PrivateKey {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			require.NoError(t, err)
			return key
		}},
		{"issuer", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["iss"] = "https://other.example"
			return f.key
		}},
		{"audience", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["aud"] = audienceBase + strings.Repeat("0", 64)
			return f.key
		}},
		{"additional audience", func(t *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			aud, ok := c["aud"].(string)
			require.True(t, ok)
			c["aud"] = []string{aud, "other"}
			return f.key
		}},
		{"expired", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["exp"] = f.now.Add(-time.Minute).Unix()
			return f.key
		}},
		{"future not-before", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["nbf"] = f.now.Add(time.Minute).Unix()
			return f.key
		}},
		{"missing not-before", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			delete(c, "nbf")
			return f.key
		}},
		{"not-before after issued-at", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["nbf"] = f.now.Add(-30 * time.Second).Unix()
			return f.key
		}},
		{"not-before too early", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["nbf"] = f.now.Add(-12 * time.Minute).Unix()
			return f.key
		}},
		{"long lifetime", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["iat"] = f.now.Add(-20 * time.Minute).Unix()
			return f.key
		}},
		{"repository id", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["repository_id"] = "999"
			return f.key
		}},
		{"repository name", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["repository"] = "other/repo"
			return f.key
		}},
		{"event", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["event_name"] = "push"
			return f.key
		}},
		{"run id", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["run_id"] = "12346"
			return f.key
		}},
		{"run attempt", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["run_attempt"] = "3"
			return f.key
		}},
		{"workflow ref", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["workflow_ref"] = repository + "/.github/workflows/pr-ci.yml@refs/heads/main"
			return f.key
		}},
		{"branch-form workflow ref", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["workflow_ref"] = repository + "/.github/workflows/required-pr-build.yml@refs/heads/main"
			return f.key
		}},
		{"pull-request workflow ref", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["workflow_ref"] = repository + "/.github/workflows/required-pr-build.yml@refs/pull/7/merge"
			return f.key
		}},
		{"workflow sha", func(_ *testing.T, c map[string]any, _ *[]byte) *rsa.PrivateKey {
			c["workflow_sha"] = strings.Repeat("z", 40)
			return f.key
		}},
		{"payload substitution", func(_ *testing.T, _ map[string]any, raw *[]byte) *rsa.PrivateKey {
			*raw = bytes.Replace(*raw, []byte(`"pr_number":2949`), []byte(`"pr_number":2950`), 1)
			return f.key
		}},
		{"noncanonical payload", func(_ *testing.T, c map[string]any, raw *[]byte) *rsa.PrivateKey {
			*raw = append([]byte(" "), *raw...)
			digest := sha256.Sum256(*raw)
			c["aud"] = audienceBase + hex.EncodeToString(digest[:])
			return f.key
		}},
		{"nonboolean coverage", func(_ *testing.T, c map[string]any, raw *[]byte) *rsa.PrivateKey {
			*raw = bytes.Replace(*raw, []byte(`"admin":true`), []byte(`"admin":null`), 1)
			digest := sha256.Sum256(*raw)
			c["aud"] = audienceBase + hex.EncodeToString(digest[:])
			return f.key
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := make(map[string]any, len(f.claims))
			for key, value := range f.claims {
				claims[key] = value
			}
			payload := bytes.Clone(f.raw)
			key := tc.mutate(t, claims, &payload)
			proof := f.proof(t, key, claims, payload)
			_, err := verifyProof(context.Background(), proof, f.server.URL, func() time.Time { return f.now })
			require.Error(t, err)
			require.NotContains(t, err.Error(), "eyJ")
		})
	}
	_, err := verifyProof(context.Background(), f.proof(t, f.key, f.claims, f.raw), "://invalid", func() time.Time { return f.now })
	require.ErrorContains(t, err, "OIDC discovery failed")
}

func TestParsePayloadRejectsInvalidCallerVerdictsAndReports(t *testing.T) {
	f := newOIDCFixture(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"zero run", func(p map[string]any) { p["caller_run_id"] = 0 }, "invalid payload identifiers"},
		{"invalid head", func(p map[string]any) { p["expected_head_sha"] = "not-a-sha" }, "invalid payload identifiers"},
		{"failed caller", func(p map[string]any) { p["pr_ci_result"] = "failure" }, "invalid payload identifiers"},
		{"missing selection", func(p map[string]any) { p["selection"] = nil }, "invalid payload identifiers"},
		{"missing job", func(p map[string]any) { p["jobs"] = map[string]any{"build": "success"} }, "invalid job verdict"},
		{"invalid job", func(p map[string]any) {
			p["jobs"] = map[string]any{"ioc": "unknown", "build": "success", "semgrep": "success", "eslint": "success", "govulncheck": "success"}
		}, "invalid job verdict"},
		{"failed build", func(p map[string]any) {
			p["jobs"] = map[string]any{"ioc": "success", "build": "failure", "semgrep": "success", "eslint": "success", "govulncheck": "success"}
		}, "invalid report shape"},
		{"missing coverage", func(p map[string]any) { p["coverage"] = map[string]any{"desktop": true} }, "invalid report shape"},
		{"invalid coverage", func(p map[string]any) {
			p["coverage"] = map[string]any{"desktop": true, "control_plane": true, "media_plane": true, "admin": nil}
		}, "invalid coverage shape"},
		{"unknown field", func(p map[string]any) { p["surprise"] = true }, "invalid payload"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var candidate map[string]any
			require.NoError(t, json.Unmarshal(f.raw, &candidate))
			tc.mutate(candidate)
			raw, err := json.Marshal(candidate)
			require.NoError(t, err)
			_, err = parsePayload(raw)
			require.ErrorContains(t, err, tc.want)
		})
	}
	_, err := parsePayload(append(bytes.Clone(f.raw), []byte(`{}`)...))
	require.ErrorContains(t, err, "invalid payload")
}

func TestRunWritesBoundedVerifiedFileOnlyOnSuccess(t *testing.T) {
	f := newOIDCFixture(t)
	dir := t.TempDir()
	proofPath := filepath.Join(dir, "proof.json")
	outPath := filepath.Join(dir, "verified.json")
	// run uses the real clock; issue a fixture at current time for this boundary.
	f.now = time.Now()
	f.claims["iat"] = f.now.Add(-time.Minute).Unix()
	f.claims["nbf"] = f.now.Add(-6 * time.Minute).Unix()
	f.claims["exp"] = f.now.Add(4 * time.Minute).Unix()
	require.NoError(t, os.WriteFile(proofPath, f.proof(t, f.key, f.claims, f.raw), 0600))
	require.NoError(t, run(context.Background(), proofPath, outPath, f.server.URL))
	info, err := os.Stat(outPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.Less(t, info.Size(), int64(maxOutput))
	require.NoError(t, os.Remove(outPath))
	require.ErrorContains(t, run(context.Background(), filepath.Join(dir, "missing.json"), outPath, f.server.URL), "cannot open proof")
	require.ErrorContains(t, run(context.Background(), proofPath, filepath.Join(dir, "missing", "verified.json"), f.server.URL), "cannot write verified claims")
	require.ErrorContains(t, run(context.Background(), proofPath, dir, f.server.URL), "cannot write verified claims")
	require.NoError(t, os.WriteFile(proofPath, bytes.Repeat([]byte("x"), maxProofSize+1), 0600))
	require.ErrorContains(t, run(context.Background(), proofPath, outPath, f.server.URL), "invalid proof size")
	require.NoError(t, os.WriteFile(proofPath, []byte(`{"payload_b64":"invalid","oidc_jwt":"invalid"}`), 0600))
	require.Error(t, run(context.Background(), proofPath, outPath, f.server.URL))
	_, err = os.Stat(outPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}
