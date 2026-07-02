package attestation_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// fakeOIDC is a hand-rolled tokenVerifier for tests. Returning the magic
// "valid" string means accept; any other input is treated as invalid so the
// handler exercises its rejection path. Matches the tokenVerifier interface
// (W1: VerifySPA + VerifyBinary) exposed indirectly via NewHandler's
// parameter type.
//
// The same accept/subject/err shape is used for both axis methods because the
// handler-level publish tests don't exercise per-axis policy — that is
// covered in oidc_test.go (TestVerifySPA_*, TestVerifyBinary_*, and the
// cross-axis rejection tests). Handler tests target HTTP semantics
// (401/400/409/201) gating on a verified bearer.
type fakeOIDC struct {
	acceptToken string
	subject     string
	err         error
}

func (f *fakeOIDC) VerifySPA(_ context.Context, raw string) (string, error) {
	return f.verify(raw)
}

func (f *fakeOIDC) VerifyBinary(_ context.Context, raw string) (string, error) {
	return f.verify(raw)
}

func (f *fakeOIDC) verify(raw string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if raw == f.acceptToken {
		return f.subject, nil
	}
	return "", errors.New("oidc: invalid token")
}

func newPublishHandler(t *testing.T, oidc *fakeOIDC) (*attestation.Handler, func()) {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	repo := attestation.NewRepository(db)
	cache := attestation.NewCache(repo, nil, nil, logger.New("development"))
	h := attestation.NewHandler(repo, cache, oidc, nil, nil, logger.New("development"))
	return h, cleanup
}

// toGinPublishRequest mints a gin.Context targeted at `path` with the given
// body and Authorization header. Use the per-axis path constants below.
func toGinPublishRequest(t *testing.T, path, body, authHeader string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	c.Request = req
	return c, w
}

const (
	publishSPAPath    = "/api/v1/internal/attestation/publish/spa"
	publishBinaryPath = "/api/v1/internal/attestation/publish/binary"

	// Canonical-format hashes for the happy-path tests. Per finding #22 of
	// #1264 review, publish handlers now require sha256:<64 lowercase hex>;
	// the older single-token "sha256:html" / "sha256:abc" placeholders are
	// retained only in tests that exercise the format-rejection path.
	testHashHTML      = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testHashAlt       = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testHashTampered  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testHashOriginal  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testHashBinaryFoo = "sha256:abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabca"
)

func publishSPABody(spaVersion, htmlHash string) string {
	b, _ := json.Marshal(attestation.PublishSPAPayload{
		SpaVersion: spaVersion,
		HTMLHash:   htmlHash,
	})
	return string(b)
}

func publishBinaryBody(version, platform, certHash string) string {
	b, _ := json.Marshal(attestation.PublishBinaryPayload{
		Version:  version,
		Platform: attestation.Platform(platform),
		CertHash: certHash,
	})
	return string(b)
}

// ── PublishSPA ─────────────────────────────────────────────────────

func TestPublishSPAHandler_HappyPath(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "repo:Concord-Voice/Concord-Voice-Alpha:ref:refs/heads/main"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	body := publishSPABody("a1b2c3d", testHashHTML)
	c, w := toGinPublishRequest(t, publishSPAPath, body, "Bearer valid")
	h.PublishSPA(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "a1b2c3d", resp["spa_version"])
}

func TestPublishSPAHandler_MissingBearer_401(t *testing.T) {
	oidc := &fakeOIDC{}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	body := publishSPABody("a1b2c3d", testHashHTML)
	c, w := toGinPublishRequest(t, publishSPAPath, body, "")
	h.PublishSPA(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestPublishSPAHandler_InvalidOIDC_401(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	body := publishSPABody("a1b2c3d", testHashHTML)
	c, w := toGinPublishRequest(t, publishSPAPath, body, "Bearer bogus-token")
	h.PublishSPA(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestPublishSPAHandler_MissingSpaVersion_400(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	// html_hash present, spa_version missing → binding:"required" rejects.
	body := `{"html_hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`
	c, w := toGinPublishRequest(t, publishSPAPath, body, "Bearer valid")
	h.PublishSPA(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPublishSPAHandler_MissingHTMLHash_400(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	// spa_version present, html_hash missing → binding:"required" rejects.
	body := `{"spa_version":"a1b2c3d"}`
	c, w := toGinPublishRequest(t, publishSPAPath, body, "Bearer valid")
	h.PublishSPA(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPublishSPAHandler_Conflict_409(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	// First publish establishes the row.
	first := publishSPABody("a1b2c3d", testHashOriginal)
	c1, w1 := toGinPublishRequest(t, publishSPAPath, first, "Bearer valid")
	h.PublishSPA(c1)
	require.Equal(t, http.StatusCreated, w1.Code, "first publish body: %s", w1.Body.String())

	// Re-publish with same spa_version but DIFFERENT html_hash → conflict.
	conflict := publishSPABody("a1b2c3d", testHashTampered)
	c2, w2 := toGinPublishRequest(t, publishSPAPath, conflict, "Bearer valid")
	h.PublishSPA(c2)
	require.Equal(t, http.StatusConflict, w2.Code, "second publish body: %s", w2.Body.String())
}

func TestPublishSPAHandler_Idempotent_SameHash(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	body := publishSPABody("a1b2c3d", testHashHTML)
	c1, w1 := toGinPublishRequest(t, publishSPAPath, body, "Bearer valid")
	h.PublishSPA(c1)
	require.Equal(t, http.StatusCreated, w1.Code)

	// Re-publish with identical payload should succeed (upsert-with-same-hash).
	c2, w2 := toGinPublishRequest(t, publishSPAPath, body, "Bearer valid")
	h.PublishSPA(c2)
	require.Equal(t, http.StatusCreated, w2.Code, "second publish body: %s", w2.Body.String())
}

// TestPublishSPAHandler_BadHashFormat_400 covers the format-rejection added in
// finding #22 of #1264 review: malformed hashes (wrong length, uppercase hex,
// missing prefix) must be rejected at the handler boundary with HTTP 400.
func TestPublishSPAHandler_BadHashFormat_400(t *testing.T) {
	cases := []struct {
		name string
		hash string
	}{
		// Sequential synthetic hex strings — not real secrets. The detect-secrets
		// hook's entropy check flags them; pragma allowlist applied to each.
		{"missing-prefix", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},      // pragma: allowlist secret
		{"wrong-prefix", "sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, // pragma: allowlist secret
		{"too-short", "sha256:abc"},
		{"too-long", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123"},  // pragma: allowlist secret
		{"uppercase-hex", "sha256:ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}, // pragma: allowlist secret
		{"non-hex", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abxyz!"},       // pragma: allowlist secret
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
			h, cleanup := newPublishHandler(t, oidc)
			defer cleanup()

			body := publishSPABody("a1b2c3d", tc.hash)
			c, w := toGinPublishRequest(t, publishSPAPath, body, "Bearer valid")
			h.PublishSPA(c)

			require.Equal(t, http.StatusBadRequest, w.Code,
				"malformed hash %q must be rejected at handler boundary: body=%s", tc.hash, w.Body.String())
			require.Contains(t, w.Body.String(), "sha256:")
		})
	}
}

// ── PublishBinary ──────────────────────────────────────────────────

func TestPublishBinaryHandler_HappyPath(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "repo:Concord-Voice/Concord-Voice-Alpha:ref:refs/heads/main"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	body := publishBinaryBody("0.2.7", "macos", testHashBinaryFoo)
	c, w := toGinPublishRequest(t, publishBinaryPath, body, "Bearer valid")
	h.PublishBinary(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "0.2.7", resp["version"])
	require.Equal(t, "macos", resp["platform"])
}

func TestPublishBinaryHandler_MissingBearer_401(t *testing.T) {
	oidc := &fakeOIDC{}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	body := publishBinaryBody("0.2.7", "macos", testHashBinaryFoo)
	c, w := toGinPublishRequest(t, publishBinaryPath, body, "")
	h.PublishBinary(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestPublishBinaryHandler_InvalidOIDC_401(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	body := publishBinaryBody("0.2.7", "macos", testHashBinaryFoo)
	c, w := toGinPublishRequest(t, publishBinaryPath, body, "Bearer bogus-token")
	h.PublishBinary(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestPublishBinaryHandler_MissingVersion_400(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	// platform + cert_hash present, version missing → binding:"required" rejects.
	body := `{"platform":"macos","cert_hash":"sha256:abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabca"}`
	c, w := toGinPublishRequest(t, publishBinaryPath, body, "Bearer valid")
	h.PublishBinary(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPublishBinaryHandler_MissingCertHash_400(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	// version + platform present, cert_hash missing → binding:"required" rejects.
	body := `{"version":"0.2.7","platform":"macos"}`
	c, w := toGinPublishRequest(t, publishBinaryPath, body, "Bearer valid")
	h.PublishBinary(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPublishBinaryHandler_MissingPlatform_400(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	// version + cert_hash present, platform missing → binding:"required" rejects.
	body := `{"version":"0.2.7","cert_hash":"sha256:abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabca"}`
	c, w := toGinPublishRequest(t, publishBinaryPath, body, "Bearer valid")
	h.PublishBinary(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPublishBinaryHandler_InvalidPlatform_400(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	body := publishBinaryBody("0.2.7", "haiku-os", testHashBinaryFoo)
	c, w := toGinPublishRequest(t, publishBinaryPath, body, "Bearer valid")
	h.PublishBinary(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestPublishBinaryHandler_BadHashFormat_400 covers the format-rejection added
// in finding #22 of #1264 review for the binary axis.
func TestPublishBinaryHandler_BadHashFormat_400(t *testing.T) {
	cases := []struct {
		name string
		hash string
	}{
		{"missing-prefix", "abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabca"},
		{"wrong-prefix", "md5:abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabca"},
		{"too-short", "sha256:abc"},
		{"too-long", "sha256:abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcaff"},
		{"uppercase-hex", "sha256:ABCABCABCABCABCABCABCABCABCABCABCABCABCABCABCABCABCABCABCABCABCa"},
		{"non-hex", "sha256:!@#$%^&*()$%^&*()abcabcabcabcabcabcabcabcabcabcabcabcabcabcabca"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
			h, cleanup := newPublishHandler(t, oidc)
			defer cleanup()

			body := publishBinaryBody("0.2.7", "macos", tc.hash)
			c, w := toGinPublishRequest(t, publishBinaryPath, body, "Bearer valid")
			h.PublishBinary(c)

			require.Equal(t, http.StatusBadRequest, w.Code,
				"malformed hash %q must be rejected at handler boundary: body=%s", tc.hash, w.Body.String())
			require.Contains(t, w.Body.String(), "sha256:")
		})
	}
}

func TestPublishBinaryHandler_Conflict_409(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	// First publish establishes the row.
	first := publishBinaryBody("0.2.7", "macos", testHashOriginal)
	c1, w1 := toGinPublishRequest(t, publishBinaryPath, first, "Bearer valid")
	h.PublishBinary(c1)
	require.Equal(t, http.StatusCreated, w1.Code, "first publish body: %s", w1.Body.String())

	// Re-publish with same version + platform but DIFFERENT cert_hash → conflict.
	conflict := publishBinaryBody("0.2.7", "macos", testHashTampered)
	c2, w2 := toGinPublishRequest(t, publishBinaryPath, conflict, "Bearer valid")
	h.PublishBinary(c2)
	require.Equal(t, http.StatusConflict, w2.Code, "second publish body: %s", w2.Body.String())
}

func TestPublishBinaryHandler_Idempotent_SameHash(t *testing.T) {
	oidc := &fakeOIDC{acceptToken: "valid", subject: "ci"}
	h, cleanup := newPublishHandler(t, oidc)
	defer cleanup()

	body := publishBinaryBody("0.2.7", "macos", testHashBinaryFoo)
	c1, w1 := toGinPublishRequest(t, publishBinaryPath, body, "Bearer valid")
	h.PublishBinary(c1)
	require.Equal(t, http.StatusCreated, w1.Code)

	// Re-publish with identical payload should succeed (upsert-with-same-hash).
	c2, w2 := toGinPublishRequest(t, publishBinaryPath, body, "Bearer valid")
	h.PublishBinary(c2)
	require.Equal(t, http.StatusCreated, w2.Code, "second publish body: %s", w2.Body.String())
}

// newPublishHandlerNilOIDC constructs a Handler with a true interface-nil
// oidc field, matching the degraded-mode shape api/attestation_wiring.go
// produces when buildOIDCVerifier returns nil and
// REQUIRE_CLIENT_ATTESTATION=false. Passing the literal `nil` to the
// interface-typed oidc parameter is interface-nil (not typed-nil) — exactly
// the state requireOIDC in publish_handler.go is designed to detect.
func newPublishHandlerNilOIDC(t *testing.T) (*attestation.Handler, func()) {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	repo := attestation.NewRepository(db)
	cache := attestation.NewCache(repo, nil, nil, logger.New("development"))
	h := attestation.NewHandler(repo, cache, nil, nil, nil, logger.New("development"))
	return h, cleanup
}

// TestPublishSPA_DegradedMode_503 locks the nil-oidc guard: in degraded mode
// the publish endpoint must refuse cleanly with 503 rather than panicking on
// h.oidc.VerifySPA. Regression for PR #1264 Copilot review (h.oidc nil-deref).
func TestPublishSPA_DegradedMode_503(t *testing.T) {
	h, cleanup := newPublishHandlerNilOIDC(t)
	defer cleanup()

	body := publishSPABody("a1b2c3d", testHashOriginal)
	c, w := toGinPublishRequest(t, publishSPAPath, body, "Bearer anything")
	h.PublishSPA(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code,
		"degraded-mode publish must return 503, not panic: body=%s", w.Body.String())
	require.Contains(t, w.Body.String(), "OIDC verifier unavailable")
}

// TestPublishBinary_DegradedMode_503 — sibling lock for the binary axis.
func TestPublishBinary_DegradedMode_503(t *testing.T) {
	h, cleanup := newPublishHandlerNilOIDC(t)
	defer cleanup()

	body := publishBinaryBody("0.2.7", "macos", testHashOriginal)
	c, w := toGinPublishRequest(t, publishBinaryPath, body, "Bearer anything")
	h.PublishBinary(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code,
		"degraded-mode publish must return 503, not panic: body=%s", w.Body.String())
	require.Contains(t, w.Body.String(), "OIDC verifier unavailable")
}
