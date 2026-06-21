package attestation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// githubWorkflowsPathSegment is the canonical path segment GitHub OIDC
// embeds in the workflow_ref claim before the workflow filename. Used by
// matchWorkflow to anchor exact-segment matching and reject substring
// attacks like "attacker-main-cd.yml@..." against a configured
// "main-cd.yml".
const githubWorkflowsPathSegment = "/.github/workflows/"

// OIDCConfig holds the validation parameters for GitHub Actions OIDC tokens.
//
// W1 (per-axis OIDC config, #677 reconciliation): the SPA publish path and the
// binary publish path are issued from DIFFERENT GitHub Actions workflows
// (main-cd.yml vs build-desktop.yml). Each axis is bound to its own
// (Workflow, Ref) pair so that a token minted by one workflow cannot satisfy
// the other axis's publish handler — axis-bound identity enforced at the OIDC
// layer rather than downstream of authorization.
type OIDCConfig struct {
	Issuer        string
	Audience      string
	SubjectPrefix string

	SPAWorkflow    string // e.g. "main-cd.yml"
	SPARef         string // e.g. "refs/heads/main"
	BinaryWorkflow string // e.g. "build-desktop.yml"
	BinaryRef      string // e.g. "refs/heads/main"
}

// OIDCVerifier validates GitHub Actions OIDC tokens against a fixed configuration.
type OIDCVerifier struct {
	cfg      OIDCConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
}

// NewOIDCVerifier creates a verifier with the provider discovered at cfg.Issuer.
// Network call to /.well-known/openid-configuration happens once at construction.
//
// Empty-field validation runs BEFORE the provider call: an empty Workflow
// field would silently disable the policy gate because strings.Contains and
// matchWorkflow both fail on an empty configured name. The verify_handler
// authorization wiring (#677, ADR-0010) builds the cfg from env vars whose
// defaults are non-empty, but operator override to an empty value would
// otherwise bypass the security check at runtime. Per finding #BLOCK-3 of
// the #1264 review.
func NewOIDCVerifier(ctx context.Context, cfg OIDCConfig) (*OIDCVerifier, error) {
	if err := validateOIDCConfig(cfg); err != nil {
		return nil, err
	}
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc provider: %w", err)
	}
	return &OIDCVerifier{
		cfg:      cfg,
		provider: p,
		verifier: p.Verifier(&oidc.Config{ClientID: cfg.Audience}),
	}, nil
}

// validateOIDCConfig rejects any OIDCConfig with an empty required field.
// Returns a structured error naming the offending field so the operator can
// fix the config without ambiguity. The field ordering here matches the
// declared order on the struct so log lines read in a predictable sequence
// when multiple fields are missing (only the FIRST missing field is named —
// the caller is expected to fix and re-run).
func validateOIDCConfig(cfg OIDCConfig) error {
	required := []struct {
		name  string
		value string
	}{
		{"Issuer", cfg.Issuer},
		{"Audience", cfg.Audience},
		{"SubjectPrefix", cfg.SubjectPrefix},
		{"SPAWorkflow", cfg.SPAWorkflow},
		{"SPARef", cfg.SPARef},
		{"BinaryWorkflow", cfg.BinaryWorkflow},
		{"BinaryRef", cfg.BinaryRef},
	}
	for _, r := range required {
		if r.value == "" {
			return fmt.Errorf("oidc config: field %q is required", r.name)
		}
	}
	return nil
}

// Sentinel errors returned by VerifySPA / VerifyBinary and the underlying
// validators.
var (
	// ErrOIDCInvalidIssuer is returned when the parsed token issuer doesn't match
	// the configured issuer. Defense-in-depth: the library also validates issuer
	// during signature verification, but we double-check at the claims layer.
	ErrOIDCInvalidIssuer = errors.New("oidc: issuer mismatch")
	// ErrOIDCInvalidAudience is reserved for audience mismatches surfaced at the
	// claims layer (the library also enforces this via ClientID config).
	ErrOIDCInvalidAudience = errors.New("oidc: audience mismatch")
	// ErrOIDCInvalidSubject is returned when the token subject does not begin
	// with the configured SubjectPrefix.
	ErrOIDCInvalidSubject = errors.New("oidc: subject prefix mismatch")
	// ErrOIDCInvalidWorkflow is returned when the workflow_ref claim does not
	// contain the configured per-axis Workflow string. Axis attribution comes
	// from the calling handler's log line (axis=spa or axis=binary), not the
	// error — the generic sentinel keeps the verifier API axis-agnostic.
	ErrOIDCInvalidWorkflow = errors.New("oidc: workflow mismatch")
	// ErrOIDCInvalidRef is returned when the ref claim does not exactly match
	// the configured per-axis Ref (e.g., "refs/heads/main"). Axis attribution
	// comes from the calling handler's log line.
	ErrOIDCInvalidRef = errors.New("oidc: ref mismatch")
)

// ghOIDCClaims carries the GitHub OIDC custom claims relevant to attestation
// publish authorization.
type ghOIDCClaims struct {
	Sub      string `json:"sub"`
	Workflow string `json:"workflow_ref"`
	Ref      string `json:"ref"`
}

// VerifySPA validates a raw GitHub Actions OIDC JWT for the SPA publish axis.
// Runs the shared verifyCommon path (signature + iss + aud + sub prefix), then
// matches the workflow_ref claim against cfg.SPAWorkflow and the ref claim
// against cfg.SPARef via applySPAPolicy.
//
// Returns the OIDC `sub` claim on success; used as published_by for the audit
// log.
func (v *OIDCVerifier) VerifySPA(ctx context.Context, raw string) (string, error) {
	c, err := v.verifyCommon(ctx, raw)
	if err != nil {
		return "", err
	}
	if err := v.applySPAPolicy(c); err != nil {
		return "", err
	}
	return c.Sub, nil
}

// VerifyBinary validates a raw GitHub Actions OIDC JWT for the binary publish
// axis. Runs the shared verifyCommon path (signature + iss + aud + sub
// prefix), then matches the workflow_ref claim against cfg.BinaryWorkflow and
// the ref claim against cfg.BinaryRef via applyBinaryPolicy.
//
// Returns the OIDC `sub` claim on success; used as published_by for the audit
// log.
func (v *OIDCVerifier) VerifyBinary(ctx context.Context, raw string) (string, error) {
	c, err := v.verifyCommon(ctx, raw)
	if err != nil {
		return "", err
	}
	if err := v.applyBinaryPolicy(c); err != nil {
		return "", err
	}
	return c.Sub, nil
}

// matchWorkflow returns true if the GitHub OIDC `workflow_ref` claim names
// the configured workflow as the workflows-path basename (exact match), and
// false otherwise.
//
// GitHub's canonical `workflow_ref` claim shape is
//
//	<owner>/<repo>/.github/workflows/<filename>@<ref>
//
// where <ref> is e.g. `refs/heads/main`. matchWorkflow splits on the LAST
// `@` so the basename portion is isolated from the ref suffix (the ref may
// contain its own `/` segments but never an `@`). It then asserts the
// basename ENDS with `/.github/workflows/<configured>` — the leading slash
// anchors the match to a path segment boundary so a malicious workflow named
// `attacker-main-cd.yml` cannot match a configured `main-cd.yml`.
//
// This replaces the prior strings.Contains gate (finding #10 of the #1264
// review), which was overly permissive: any claim with the configured name
// anywhere in its body (e.g., `repo:owner/main-cd.yml-evil/.github/...`)
// would match.
//
// Returns false on:
//   - Empty claim (no `@` present, or empty before `@`)
//   - Configured workflow not at the canonical workflows path segment
//   - Substring match without the leading path-segment anchor
func matchWorkflow(claim, configured string) bool {
	if claim == "" || configured == "" {
		return false
	}
	idx := strings.LastIndex(claim, "@")
	if idx < 0 {
		return false
	}
	beforeAt := claim[:idx]
	suffix := githubWorkflowsPathSegment + configured
	return strings.HasSuffix(beforeAt, suffix)
}

// applySPAPolicy enforces the per-axis workflow + ref policy for the SPA
// publish axis. Pulled out as a package-internal helper so unit tests can
// exercise the production policy code path without needing a live OIDC
// provider / JWKS fixture for the signature + iss + aud check.
//
// W1 (#677 reconciliation) + finding #10 of #1264 review: the workflow check
// uses matchWorkflow (anchored, exact-basename) rather than strings.Contains
// because the canonical claim shape is
// `<owner>/<repo>/.github/workflows/<name>@<ref>`. Strings.Contains accepted
// any claim containing the configured name as a substring, which let a
// malicious workflow named `attacker-main-cd.yml-foo` impersonate
// `main-cd.yml`. The exact path-segment anchor closes that gap.
func (v *OIDCVerifier) applySPAPolicy(c ghOIDCClaims) error {
	if !matchWorkflow(c.Workflow, v.cfg.SPAWorkflow) {
		return ErrOIDCInvalidWorkflow
	}
	if c.Ref != v.cfg.SPARef {
		return ErrOIDCInvalidRef
	}
	return nil
}

// applyBinaryPolicy mirrors applySPAPolicy for the binary publish axis.
// Identical structural shape — the only difference is which (Workflow, Ref)
// pair from cfg is enforced. Keeping the two helpers separate (rather than a
// shared parameterized check) makes the cross-axis rejection property obvious
// from the call site in VerifyBinary.
func (v *OIDCVerifier) applyBinaryPolicy(c ghOIDCClaims) error {
	if !matchWorkflow(c.Workflow, v.cfg.BinaryWorkflow) {
		return ErrOIDCInvalidWorkflow
	}
	if c.Ref != v.cfg.BinaryRef {
		return ErrOIDCInvalidRef
	}
	return nil
}

// verifyCommon runs the axis-agnostic verification:
//  1. Cryptographic signature + standard claims (delegated to coreos/go-oidc).
//  2. Custom GitHub claims that are identical across axes: issuer match,
//     subject prefix.
//
// Returns the parsed claims on success so the per-axis caller can apply the
// workflow + ref check.
func (v *OIDCVerifier) verifyCommon(ctx context.Context, raw string) (ghOIDCClaims, error) {
	var c ghOIDCClaims
	idTok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return c, fmt.Errorf("oidc verify: %w", err)
	}
	if err := idTok.Claims(&c); err != nil {
		return c, fmt.Errorf("oidc claims: %w", err)
	}
	if idTok.Issuer != v.cfg.Issuer {
		return c, ErrOIDCInvalidIssuer
	}
	if !strings.HasPrefix(c.Sub, v.cfg.SubjectPrefix) {
		return c, ErrOIDCInvalidSubject
	}
	return c, nil
}
