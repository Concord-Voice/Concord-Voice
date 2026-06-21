package attestation

import (
	"context"
	"testing"
	"time"

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
//	if !strings.Contains(claims.Workflow, cfg.<Axis>Workflow) → ErrOIDCInvalidWorkflow
//	if claims.Ref != cfg.<Axis>Ref                          → ErrOIDCInvalidRef
//
// The signature + iss + aud + sub-prefix path is identical to the legacy
// validateClaims sub-tests (deleted along with the legacy Verify), and would
// require a live OIDC provider / JWKS fixture to exercise end-to-end — out of
// scope here. These unit tests target the per-axis policy layer that is
// unique to W1, including the cross-axis rejection property.
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
			Audience:       "https://api.example.com",
			SubjectPrefix:  "repo:markdrogersjr/Concord:",
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
	canonicalSPAWorkflowRef = "markdrogersjr/Concord/.github/workflows/main-cd.yml@refs/heads/main"
	// canonicalBinaryWorkflowRef likewise for build-desktop.yml.
	canonicalBinaryWorkflowRef = "markdrogersjr/Concord/.github/workflows/build-desktop.yml@refs/heads/main"
	// canonicalSubject mirrors the real GitHub OIDC `sub` claim shape.
	canonicalSubject = "repo:markdrogersjr/Concord:ref:refs/heads/main"
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
		Workflow: "markdrogersjr/Concord/.github/workflows/build-other.yml@refs/heads/main",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow)
}

func TestVerifySPA_RejectsWrongRef(t *testing.T) {
	err := newTestVerifier().applySPAPolicy(ghOIDCClaims{
		Sub:      "repo:markdrogersjr/Concord:ref:refs/heads/feature/xyz",
		Workflow: "markdrogersjr/Concord/.github/workflows/main-cd.yml@refs/heads/feature/xyz",
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
		Workflow: "markdrogersjr/Concord/.github/workflows/build-other.yml@refs/heads/main",
		Ref:      "refs/heads/main",
	})
	require.ErrorIs(t, err, ErrOIDCInvalidWorkflow)
}

func TestVerifyBinary_RejectsWrongRef(t *testing.T) {
	err := newTestVerifier().applyBinaryPolicy(ghOIDCClaims{
		Sub:      "repo:markdrogersjr/Concord:ref:refs/heads/feature/xyz",
		Workflow: "markdrogersjr/Concord/.github/workflows/build-desktop.yml@refs/heads/feature/xyz",
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
		Workflow: "markdrogersjr/Concord/.github/workflows/attacker-main-cd.yml@refs/heads/main",
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
		Workflow: "markdrogersjr/Concord/.github/workflows/foo-main-cd.yml@refs/heads/main",
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
		Workflow: "markdrogersjr/Concord/.github/workflows/attacker-build-desktop.yml@refs/heads/main",
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
		Workflow: "markdrogersjr/Concord/main-cd.yml@refs/heads/main",
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
		Workflow: "markdrogersjr/Concord/.github/workflows/main-cd.yml",
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
		Audience:       "https://api.example.com",
		SubjectPrefix:  "repo:markdrogersjr/Concord:",
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
		Audience:       "https://api.example.com",
		SubjectPrefix:  "repo:markdrogersjr/Concord:",
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

// ── matchWorkflow unit coverage ─────────────────────────────────────

// TestMatchWorkflow exercises the path-segment-anchored matcher directly
// across the boundary inputs that the per-axis policy tests cover at the
// outer layer. Useful when refactoring matchWorkflow to confirm the
// boundary contract is preserved.
func TestMatchWorkflow(t *testing.T) {
	cases := []struct {
		name       string
		claim      string
		configured string
		expect     bool
	}{
		{"canonical match", "owner/repo/.github/workflows/main-cd.yml@refs/heads/main", "main-cd.yml", true},
		{"empty claim", "", "main-cd.yml", false},
		{"empty configured", "owner/repo/.github/workflows/main-cd.yml@refs/heads/main", "", false},
		{"missing @", "owner/repo/.github/workflows/main-cd.yml", "main-cd.yml", false},
		{"attacker substring", "owner/repo/.github/workflows/attacker-main-cd.yml@refs/heads/main", "main-cd.yml", false},
		{"attacker suffix", "owner/repo/.github/workflows/foo-main-cd.yml@refs/heads/main", "main-cd.yml", false},
		{"missing path segment", "owner/repo/main-cd.yml@refs/heads/main", "main-cd.yml", false},
		{"different workflow", "owner/repo/.github/workflows/build-other.yml@refs/heads/main", "main-cd.yml", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchWorkflow(tc.claim, tc.configured)
			require.Equal(t, tc.expect, got)
		})
	}
}
