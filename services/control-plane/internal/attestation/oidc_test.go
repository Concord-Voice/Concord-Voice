package attestation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

// contextWithTimeout returns a context with a short timeout, used so the
// OIDC-provider discovery call in NewOIDCVerifier doesn't block tests on
// unreachable hosts. seconds is bounded by the test runner's overall
// timeout so we don't need to be precise.
func contextWithTimeout(seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
}

// W1 per-axis claim-policy tests (#677 reconciliation).
//
// VerifySPA and VerifyBinary share the same on-the-wire policy shape:
//
//	if claims.Ref != cfg.<Axis>Ref                                     → ErrOIDCInvalidRef
//	if !matchWorkflowRef(claims.Workflow, cfg.<Axis>Workflow, ref)     → ErrOIDCInvalidWorkflow
//
// (Ref is checked first so the two sentinels stay distinguishable — see
// applySPAPolicy. The workflow check was strings.Contains originally, then
// matchWorkflow, and is now matchWorkflowRef; this pseudocode has been wrong
// twice, so keep it in step with the production helpers.)
//
// The signature + iss + aud + sub-prefix path is identical to the legacy
// validateClaims sub-tests (deleted along with the legacy Verify). It DOES now
// have end-to-end coverage: see the SupportedSigningAlgs pin section at the end
// of this file, which stands up a live OIDC issuer fixture (discovery document
// + JWKS) and drives VerifySPA through real signature verification. The tests
// in THIS section deliberately stay at the per-axis policy layer that is unique
// to W1, including the cross-axis rejection property.
//
// Per #1264 Phase 4: tests call applySPAPolicy / applyBinaryPolicy directly
// on a constructed OIDCVerifier so we exercise the production code path (the
// helpers VerifySPA / VerifyBinary delegate to after verifyCommon succeeds).
// The prior pattern declared parallel local helpers (checkSPAPolicy /
// checkBinaryPolicy) that duplicated the logic — SonarQube reported oidc.go
// as 0% covered as a result. Moving the policy logic into the production
// file and pointing tests at it closes that coverage gap.

// newTestVerifier returns an OIDCVerifier wired with the per-axis OIDC config
// used by the policy tests. The provider + verifier fields are left nil
// because applySPAPolicy / applyBinaryPolicy only read v.cfg — no live
// JWKS / network is required at this layer.
func newTestVerifier() *OIDCVerifier {
	return &OIDCVerifier{
		cfg: OIDCConfig{
			Issuer:         "https://token.actions.githubusercontent.com",
			Audience:       "https://api.concordvoice.chat",
			SubjectPrefix:  "repo:Concord-Voice/Concord-Voice-Alpha:",
			SPAWorkflow:    "main-cd.yml",
			SPARef:         "refs/heads/main",
			BinaryWorkflow: "build-desktop.yml",
			BinaryRef:      "refs/heads/main",
		},
	}
}

// Canonical workflow_ref claims use the GitHub OIDC shape
// `<owner>/<repo>/.github/workflows/<filename>@<ref>` (see
// https://docs.github.com/en/actions/deployment/security-hardening-your-deployments/about-security-hardening-with-openid-connect).
// matchWorkflow asserts the basename ENDS WITH `/.github/workflows/<configured>`,
// so the leading `/` between the repo segment and `.github` is load-bearing —
// it anchors the match to a path-segment boundary so a malicious workflow
// `attacker-main-cd.yml-foo` cannot impersonate `main-cd.yml`. Fixtures here
// reflect that canonical shape rather than the prior simplified
// `.github/workflows/<filename>@<ref>` form that worked under the old
// strings.Contains policy.

const (
	// canonicalSPAWorkflowRef matches the form GitHub emits in workflow_ref
	// for a token minted by main-cd.yml on refs/heads/main.
	canonicalSPAWorkflowRef = "Concord-Voice/Concord-Voice-Alpha/.github/workflows/main-cd.yml@refs/heads/main"
	// canonicalBinaryWorkflowRef likewise for build-desktop.yml.
	canonicalBinaryWorkflowRef = "Concord-Voice/Concord-Voice-Alpha/.github/workflows/build-desktop.yml@refs/heads/main"
	// canonicalSubject mirrors the real GitHub OIDC `sub` claim shape.
	canonicalSubject = "repo:Concord-Voice/Concord-Voice-Alpha:ref:refs/heads/main"
)

// ── SPA axis ───────────────────────────────────────────────────────

func TestVerifySPA_HappyPath(t *testing.T) {
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: canonicalSPAWorkflowRef,
		Ref:      "refs/heads/main",
	})
	require.NoError(t, err)
}

func TestVerifySPA_RejectsWrongWorkflow(t *testing.T) {
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: "Concord-Voice/Concord-Voice-Alpha/.github/workflows/build-other.yml@refs/heads/main",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow)
}

func TestVerifySPA_RejectsWrongRef(t *testing.T) {
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      "repo:Concord-Voice/Concord-Voice-Alpha:ref:refs/heads/feature/xyz",
		Workflow: "Concord-Voice/Concord-Voice-Alpha/.github/workflows/main-cd.yml@refs/heads/feature/xyz",
		Ref:      "refs/heads/feature/xyz",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidRef)
}

func TestVerifySPA_RejectsEmptyWorkflowClaim(t *testing.T) {
	// workflow_ref absent — should reject as workflow mismatch because
	// matchWorkflow returns false for an empty claim.
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: "",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow)
}

// ── Binary axis ────────────────────────────────────────────────────

func TestVerifyBinary_HappyPath(t *testing.T) {
	err := newTestVerifier().applyBinaryPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: canonicalBinaryWorkflowRef,
		Ref:      "refs/heads/main",
	})
	require.NoError(t, err)
}

func TestVerifyBinary_RejectsWrongWorkflow(t *testing.T) {
	err := newTestVerifier().applyBinaryPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: "Concord-Voice/Concord-Voice-Alpha/.github/workflows/build-other.yml@refs/heads/main",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow)
}

func TestVerifyBinary_RejectsWrongRef(t *testing.T) {
	err := newTestVerifier().applyBinaryPolicy(ghOIDCClaims{
		Sub:      "repo:Concord-Voice/Concord-Voice-Alpha:ref:refs/heads/feature/xyz",
		Workflow: "Concord-Voice/Concord-Voice-Alpha/.github/workflows/build-desktop.yml@refs/heads/feature/xyz",
		Ref:      "refs/heads/feature/xyz",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidRef)
}

func TestVerifyBinary_RejectsEmptyWorkflowClaim(t *testing.T) {
	// workflow_ref absent — should reject as workflow mismatch.
	err := newTestVerifier().applyBinaryPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: "",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow)
}

// ── Cross-axis rejection (load-bearing W1 security tests) ──────────

// TestVerifyBinary_RejectsSPAWorkflow asserts that a token whose workflow_ref
// names main-cd.yml (the SPA-publishing workflow) is rejected when presented
// to the binary axis. This is the W1 security property: axis-bound identity
// at the OIDC layer — a compromised main-cd.yml runner cannot mint a token
// that publishes binary hashes, even if the bearer reaches the binary
// endpoint.
func TestVerifyBinary_RejectsSPAWorkflow(t *testing.T) {
	err := newTestVerifier().applyBinaryPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: canonicalSPAWorkflowRef,
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow)
}

// TestVerifySPA_RejectsBinaryWorkflow asserts the symmetric W1 property:
// a token minted by build-desktop.yml cannot satisfy the SPA publish handler.
func TestVerifySPA_RejectsBinaryWorkflow(t *testing.T) {
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: canonicalBinaryWorkflowRef,
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow)
}

// ── Substring attack rejection (finding #10 of the #1264 review) ────

// TestVerifySPA_RejectsAttackerSubstringWorkflow asserts that a workflow_ref
// whose basename CONTAINS the configured name as a substring but does NOT
// match it exactly is rejected. This is the property that the prior
// strings.Contains gate violated: an attacker workflow named
// `attacker-main-cd.yml` (the suffix `main-cd.yml` is a substring of the
// basename `attacker-main-cd.yml`) would have matched under
// strings.Contains(claim, "main-cd.yml") but is rejected by the
// matchWorkflow exact-suffix check.
//
// Per finding #10, the matchWorkflow contract requires the basename to END
// WITH `/.github/workflows/<configured>` — the leading slash anchors the
// match to a path-segment boundary, closing the substring foot-gun.
func TestVerifySPA_RejectsAttackerSubstringWorkflow(t *testing.T) {
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: "Concord-Voice/Concord-Voice-Alpha/.github/workflows/attacker-main-cd.yml@refs/heads/main",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow,
		"workflow with configured name as substring (not basename) must be rejected")
}

// TestVerifySPA_RejectsAttackerSuffixWorkflow asserts the related substring
// attack where the configured workflow appears as a SUFFIX of the attacker
// basename but not as a complete path segment. E.g., a workflow_ref
// `.../workflows/foo-main-cd.yml@...` (basename ends with `main-cd.yml` but
// the basename itself isn't `main-cd.yml`) must reject.
func TestVerifySPA_RejectsAttackerSuffixWorkflow(t *testing.T) {
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: "Concord-Voice/Concord-Voice-Alpha/.github/workflows/foo-main-cd.yml@refs/heads/main",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow,
		"workflow whose basename ends with configured name but isn't equal must be rejected")
}

// TestVerifyBinary_RejectsAttackerSubstringWorkflow exercises the same
// substring-attack defense on the binary axis.
func TestVerifyBinary_RejectsAttackerSubstringWorkflow(t *testing.T) {
	err := newTestVerifier().applyBinaryPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: "Concord-Voice/Concord-Voice-Alpha/.github/workflows/attacker-build-desktop.yml@refs/heads/main",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow)
}

// TestVerifySPA_RejectsWorkflowMissingPathSegment asserts that a claim
// containing the configured filename but NOT under the canonical
// /.github/workflows/ path segment is rejected. Defense in depth against a
// future GitHub shape change that omitted the path prefix.
func TestVerifySPA_RejectsWorkflowMissingPathSegment(t *testing.T) {
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: "Concord-Voice/Concord-Voice-Alpha/main-cd.yml@refs/heads/main",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow,
		"workflow without canonical /.github/workflows/ path segment must be rejected")
}

// TestVerifySPA_RejectsClaimWithoutAtSeparator asserts that a workflow_ref
// missing the `@<ref>` suffix is rejected. The canonical GitHub OIDC claim
// always carries `@`; absence is a malformed claim and matchWorkflow
// returns false.
func TestVerifySPA_RejectsClaimWithoutAtSeparator(t *testing.T) {
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      canonicalSubject,
		Workflow: "Concord-Voice/Concord-Voice-Alpha/.github/workflows/main-cd.yml",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow,
		"workflow_ref without @<ref> separator must be rejected")
}

// ── NewOIDCVerifier construction validation ─────────────────────────

// TestNewOIDCVerifier_BadIssuer ensures the constructor surfaces a wrapped
// discovery error when the issuer URL is unreachable.
//
// Per finding #BLOCK-3 the constructor now rejects empty-required-field
// configs BEFORE calling the OIDC provider. To exercise the
// "oidc provider:" wrap path (which would only fire if validation passed
// but discovery failed) we supply a populated config whose Issuer is a
// reachable scheme but resolves to a path that yields a discovery error.
// The simplest such config: a syntactically-valid but unreachable
// HTTPS URL, with all other fields populated. The provider attempts
// .well-known discovery and fails with a network error which we wrap as
// "oidc provider:".
func TestNewOIDCVerifier_BadIssuer(t *testing.T) {
	cfg := OIDCConfig{
		Issuer:         "https://0.0.0.0:1/unreachable",
		Audience:       "https://api.concordvoice.chat",
		SubjectPrefix:  "repo:Concord-Voice/Concord-Voice-Alpha:",
		SPAWorkflow:    "main-cd.yml",
		SPARef:         "refs/heads/main",
		BinaryWorkflow: "build-desktop.yml",
		BinaryRef:      "refs/heads/main",
	}
	ctx, cancel := contextWithTimeout(2)
	defer cancel()
	_, err := NewOIDCVerifier(ctx, cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "oidc provider:",
		"unreachable issuer must surface as wrapped 'oidc provider:' error")
}

// ── NewOIDCVerifier empty-field rejection (finding #BLOCK-3) ────────

// TestNewOIDCVerifier_EmptyFields validates that each OIDCConfig required
// field is individually checked: an empty value for any one of them causes
// constructor failure with a structured error naming the offending field.
// Per finding #BLOCK-3 of the #1264 review: strings.Contains and the new
// matchWorkflow both fail closed on an empty configured name, but the
// earlier code would still construct a verifier that ALWAYS rejected (or
// in the substring-policy case, ALWAYS matched). Loud failure at
// construction is the correct posture.
func TestNewOIDCVerifier_EmptyFields(t *testing.T) {
	base := OIDCConfig{
		Issuer:         "https://token.actions.githubusercontent.com",
		Audience:       "https://api.concordvoice.chat",
		SubjectPrefix:  "repo:Concord-Voice/Concord-Voice-Alpha:",
		SPAWorkflow:    "main-cd.yml",
		SPARef:         "refs/heads/main",
		BinaryWorkflow: "build-desktop.yml",
		BinaryRef:      "refs/heads/main",
	}
	cases := []struct {
		name       string
		mutate     func(*OIDCConfig)
		fieldLabel string
	}{
		{"empty Issuer", func(c *OIDCConfig) { c.Issuer = "" }, "Issuer"},
		{"empty Audience", func(c *OIDCConfig) { c.Audience = "" }, "Audience"},
		{"empty SubjectPrefix", func(c *OIDCConfig) { c.SubjectPrefix = "" }, "SubjectPrefix"},
		{"empty SPAWorkflow", func(c *OIDCConfig) { c.SPAWorkflow = "" }, "SPAWorkflow"},
		{"empty SPARef", func(c *OIDCConfig) { c.SPARef = "" }, "SPARef"},
		{"empty BinaryWorkflow", func(c *OIDCConfig) { c.BinaryWorkflow = "" }, "BinaryWorkflow"},
		{"empty BinaryRef", func(c *OIDCConfig) { c.BinaryRef = "" }, "BinaryRef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			ctx, cancel := contextWithTimeout(1)
			defer cancel()
			_, err := NewOIDCVerifier(ctx, cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), "oidc config: field",
				"empty-field error must use the 'oidc config: field' prefix")
			require.Contains(t, err.Error(), tc.fieldLabel,
				"empty-field error must name the offending field")
		})
	}
}

// ── matchWorkflowRef unit coverage ──────────────────────────────────

// TestMatchWorkflowRef exercises the ref-anchored matcher directly across the
// boundary inputs the per-axis policy tests cover at the outer layer. Useful
// when refactoring matchWorkflowRef to confirm the boundary contract holds.
//
// The `last-@ split bypass` case is a REGRESSION TEST, not a hypothetical: the
// predecessor matchWorkflow split on the LAST `@` and returned true for that
// claim while the running workflow was evil.yml. Surfaced by @red-team on
// PR #3207 with an executable PoC. It was never exploitable end-to-end (git
// forbids a path component starting with `.`, so the ref it needs is not
// mintable, and the separate ref-equality check held), but the guard was safe
// only by an unstated external invariant. Comparing instead of parsing removes
// the class; this row proves it stays removed.
func TestMatchWorkflowRef(t *testing.T) {
	const ref = "refs/heads/main"
	cases := []struct {
		name       string
		claim      string
		configured string
		ref        string
		expect     bool
	}{
		{"canonical match", "owner/repo/.github/workflows/main-cd.yml@" + ref, "main-cd.yml", ref, true},
		{"empty claim", "", "main-cd.yml", ref, false},
		{"empty configured", "owner/repo/.github/workflows/main-cd.yml@" + ref, "", ref, false},
		{"empty ref", "owner/repo/.github/workflows/main-cd.yml@" + ref, "main-cd.yml", "", false},
		{"missing @", "owner/repo/.github/workflows/main-cd.yml", "main-cd.yml", ref, false},
		{"attacker substring", "owner/repo/.github/workflows/attacker-main-cd.yml@" + ref, "main-cd.yml", ref, false},
		{"attacker suffix", "owner/repo/.github/workflows/foo-main-cd.yml@" + ref, "main-cd.yml", ref, false},
		{"missing path segment", "owner/repo/main-cd.yml@" + ref, "main-cd.yml", ref, false},
		{"different workflow", "owner/repo/.github/workflows/build-other.yml@" + ref, "main-cd.yml", ref, false},
		{"wrong ref", "owner/repo/.github/workflows/main-cd.yml@refs/heads/other", "main-cd.yml", ref, false},
		{
			"last-@ split bypass (regression, #3207 red-team)",
			"owner/repo/.github/workflows/evil.yml@refs/heads/x/.github/workflows/main-cd.yml@y",
			"main-cd.yml", ref, false,
		},
		{
			"workflow_ref embeds a ref other than the claim's ref",
			"owner/repo/.github/workflows/main-cd.yml@refs/heads/attacker",
			"main-cd.yml", ref, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expect, matchWorkflowRef(tc.claim, tc.configured, tc.ref))
		})
	}
}

// ── SupportedSigningAlgs pin (RS256) — live fixture tests ───────────
//
// NewOIDCVerifier pins verifier: p.Verifier(&oidc.Config{SupportedSigningAlgs:
// []string{oidc.RS256}}). The tests above exercise the pure claim-policy layer
// with provider/verifier left nil, which cannot see this pin at all — it lives
// entirely inside go-oidc's cryptographic Verify step. These three tests spin
// up a real httptest.Server acting as an OIDC issuer (discovery document +
// JWKS) so NewOIDCVerifier does its normal discovery + verifier construction,
// then exercise the pin end-to-end.
//
// The fixture's discovery document MUST advertise BOTH "RS256" and "ES256" in
// id_token_signing_alg_values_supported. go-oidc's Provider.Verifier backfills
// an empty SupportedSigningAlgs from the discovery document's algorithm list
// (go-oidc v3.21.0 verify.go newVerifier) — so an UNPINNED verifier build
// against a discovery doc that only ever advertised RS256 would reject ES256
// for the same reason a pinned one does, and TestOIDCVerifier_RejectsNonRS256Signature
// would pass for a reason that has nothing to do with the pin under test.
// Advertising ES256 is what makes the pinned rejection meaningful.

// oidcAlgPinKeys holds the two signing keypairs served by newOIDCAlgPinFixture,
// each under its own JWKS kid.
type oidcAlgPinKeys struct {
	rsaPriv *rsa.PrivateKey
	rsaKid  string
	ecPriv  *ecdsa.PrivateKey
	ecKid   string
}

// newOIDCAlgPinFixture starts an httptest.Server serving both a discovery
// document and a JWKS endpoint from ONE mux, so both are reachable through
// the same srv.URL — required because the discovery doc's "issuer" field must
// equal srv.URL exactly (go-oidc's NewProvider rejects a mismatch, and
// verifyCommon re-checks idTok.Issuer != cfg.Issuer) and its "jwks_uri" is
// derived from that same URL. The discovery handler is re-servable: each
// NewOIDCVerifier call in these tests re-fetches it independently.
func newOIDCAlgPinFixture(t *testing.T) (*httptest.Server, oidcAlgPinKeys) {
	t.Helper()

	// 2048 is deliberate and must NOT be raised to satisfy [internal]'s
	// RSA-4096 minimum: that rule governs Concord's OWN key material, and this
	// fixture emulates GitHub's JWKS, whose live keys are 2048-bit (verified
	// 2026-09-08). Raising it makes the fixture less faithful to the issuer it
	// stands in for, and costs seconds per run.
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	keys := oidcAlgPinKeys{
		rsaPriv: rsaPriv,
		rsaKid:  "rsa-signing-key",
		ecPriv:  ecPriv,
		ecKid:   "ec-signing-key",
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			// Only these three fields do any work for the code under test.
			// go-oidc v3.21.0 never reads subject_types_supported or
			// response_types_supported at all, and stores the two endpoints
			// solely for Provider.Endpoint(), which nothing here calls —
			// carrying them would imply they matter to the pin.
			"issuer":                                srv.URL,
			"jwks_uri":                              srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		})
	})

	// ecdsa.PublicKey.Bytes returns the SEC 1 uncompressed point
	// 0x04 ‖ X ‖ Y with both coordinates already fixed-width, so the JWK's
	// x and y fall out as slices. Reading PublicKey.X / .Y instead is
	// deprecated as of Go 1.26 AND would need left-padding by hand, because
	// big.Int.Bytes() strips leading zeros — an unpadded coordinate is an
	// intermittent fixture failure (~1 run in 256 per coordinate), not a
	// theoretical one.
	ecPub, err := ecPriv.PublicKey.Bytes()
	require.NoError(t, err)
	const p256Coord = 32 // ecPub is 0x04 ‖ X(32) ‖ Y(32)
	// p256Coord is coupled to elliptic.P256() above by convention only; assert
	// it so switching curves is a loud failure rather than a malformed JWK.
	require.Len(t, ecPub, 1+2*p256Coord)

	// Marshalled once here rather than per request: the keys never change, and
	// building it on the test goroutine keeps require.* off the server's
	// goroutine, where a t.Fatalf would be undefined behaviour.
	jwksBody, err := json.Marshal(map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": keys.rsaKid,
				"n": base64.RawURLEncoding.EncodeToString(rsaPriv.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
			},
			{
				"kty": "EC", "alg": "ES256", "use": "sig", "crv": "P-256", "kid": keys.ecKid,
				"x": base64.RawURLEncoding.EncodeToString(ecPub[1 : 1+p256Coord]),
				"y": base64.RawURLEncoding.EncodeToString(ecPub[1+p256Coord:]),
			},
		},
	})
	require.NoError(t, err)

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBody)
	})

	return srv, keys
}

// oidcAlgPinConfig mirrors newTestVerifier's config values but with Issuer
// pointed at the fixture server, as required by validateOIDCConfig (all seven
// fields non-empty, SubjectPrefix matching ^repo:<owner>/<repo>:).
func oidcAlgPinConfig(issuer string) OIDCConfig {
	return OIDCConfig{
		Issuer:         issuer,
		Audience:       "https://api.concordvoice.chat",
		SubjectPrefix:  "repo:Concord-Voice/Concord-Voice-Alpha:",
		SPAWorkflow:    "main-cd.yml",
		SPARef:         "refs/heads/main",
		BinaryWorkflow: "build-desktop.yml",
		BinaryRef:      "refs/heads/main",
	}
}

// oidcAlgPinClaims builds the claim set that satisfies VerifySPA against
// oidcAlgPinConfig: canonical subject, canonical SPA workflow_ref, ref
// matching cfg.SPARef, and an issuer matching the fixture server.
func oidcAlgPinClaims(issuer string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":          issuer,
		"aud":          "https://api.concordvoice.chat",
		"sub":          canonicalSubject,
		"workflow_ref": canonicalSPAWorkflowRef,
		"ref":          "refs/heads/main",
		"iat":          now.Unix(),
		"exp":          now.Add(time.Hour).Unix(),
	}
}

// signOIDCPinToken signs claims with the given method/key, stamping kid into
// the JWT header so the verifier's JWKS lookup can find the matching key.
// Mirrors the in-repo pattern at
// internal/oauth/google_internal_test.go:19-41 (jwksServerForKey /
// signGoogleToken).
func signOIDCPinToken(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	require.NoError(t, err)
	return signed
}

// TestOIDCVerifier_RejectsNonRS256Signature proves the SupportedSigningAlgs
// pin: a token signed ES256 — an algorithm the fixture's discovery document
// advertises support for — is rejected by a verifier built through the
// production NewOIDCVerifier constructor. go-oidc's jose.ParseSigned gate
// wraps a disallowed algorithm as "oidc: malformed jwt", distinct from
// "failed to verify signature" (a key-lookup miss) — asserting the former
// (and explicitly not the latter) proves the algorithm gate fired rather than
// some unrelated fixture defect.
func TestOIDCVerifier_RejectsNonRS256Signature(t *testing.T) {
	srv, keys := newOIDCAlgPinFixture(t)
	ctx, cancel := contextWithTimeout(10)
	defer cancel()

	v, err := NewOIDCVerifier(ctx, oidcAlgPinConfig(srv.URL))
	require.NoError(t, err)

	raw := signOIDCPinToken(t, jwt.SigningMethodES256, keys.ecPriv, keys.ecKid, oidcAlgPinClaims(srv.URL))

	_, err = v.VerifySPA(ctx, raw)
	require.Error(t, err)

	// Assert the SPECIFIC go-jose message, not the "malformed jwt" wrapper.
	// go-oidc wraps EVERY jose.ParseSigned failure with that prefix
	// (verify.go), so a future fixture defect producing a structurally invalid
	// token — a bad kid, a claims-marshalling change, a golang-jwt header
	// change — would keep this test green while the algorithm gate was never
	// reached. False-pass is the dangerous direction for a security regression
	// test.
	//
	// Pinning the rendered expected-set also makes this the assertion that
	// holds the real invariant. Against "malformed jwt" alone the test proved
	// only "ES256 is refused"; adding RS512 or ES384 to SupportedSigningAlgs
	// left it green. Here a widened pin renders `expected ["RS256" "ES256"]`,
	// which does not contain this substring (the `]` must follow the closing
	// quote), so ONE assertion is falsified both by REVERTING the pin and by
	// WIDENING it.
	//
	// This is a library-internal string and go-jose exports a typed
	// *jose.ErrUnexpectedSignatureAlgorithm — but go-oidc wraps with %v, not
	// %w, so errors.As returns false through this path and the string is the
	// only handle that exists. A go-oidc/go-jose bump fails here loudly, which
	// is the safe direction; that is the upgrade trigger.
	require.Contains(t, err.Error(), `unexpected signature algorithm "ES256"; expected ["RS256"]`,
		"rejection must come from the algorithm gate with the accepted set pinned to RS256 alone")
	require.NotContains(t, err.Error(), "failed to verify signature",
		"rejection must be the alg gate, not a key-lookup miss (a broken JWKS surfaces here)")
}

// TestOIDCVerifier_AcceptsRS256Signature proves the pin does not break the
// happy path: the same fixture, signed RS256 with the RSA key, verifies
// successfully through NewOIDCVerifier and returns the canonical subject.
func TestOIDCVerifier_AcceptsRS256Signature(t *testing.T) {
	srv, keys := newOIDCAlgPinFixture(t)
	ctx, cancel := contextWithTimeout(10)
	defer cancel()

	v, err := NewOIDCVerifier(ctx, oidcAlgPinConfig(srv.URL))
	require.NoError(t, err)

	raw := signOIDCPinToken(t, jwt.SigningMethodRS256, keys.rsaPriv, keys.rsaKid, oidcAlgPinClaims(srv.URL))

	sub, err := v.VerifySPA(ctx, raw)
	require.NoError(t, err)
	require.Equal(t, canonicalSubject, sub)
}

// TestOIDCVerifier_UnpinnedVerifierAcceptsES256 is the vacuity control for
// TestOIDCVerifier_RejectsNonRS256Signature and must not be omitted: it
// proves the ES256 rejection above is caused by the pin, not by a broken
// fixture, a wrong kid, or a malformed token. It builds a verifier the OLD
// way by hand — oidc.NewProvider + Verifier(&oidc.Config{ClientID: ...}) with
// NO SupportedSigningAlgs — against the identical fixture and identical
// claims used above, signed ES256. That unpinned verifier MUST accept the
// token. If it didn't, TestOIDCVerifier_RejectsNonRS256Signature's failure
// could not be attributed to the pin at all: the same fixture and token would
// be failing for an unrelated reason, and the pin would be untested.
func TestOIDCVerifier_UnpinnedVerifierAcceptsES256(t *testing.T) {
	srv, keys := newOIDCAlgPinFixture(t)
	ctx, cancel := contextWithTimeout(10)
	defer cancel()

	p, err := oidc.NewProvider(ctx, srv.URL)
	require.NoError(t, err)
	unpinned := p.Verifier(&oidc.Config{ClientID: "https://api.concordvoice.chat"})

	raw := signOIDCPinToken(t, jwt.SigningMethodES256, keys.ecPriv, keys.ecKid, oidcAlgPinClaims(srv.URL))

	_, err = unpinned.Verify(ctx, raw)
	require.NoError(t, err,
		"unpinned verifier must accept ES256 — this is the control proving the pinned rejection is non-vacuous")
}
