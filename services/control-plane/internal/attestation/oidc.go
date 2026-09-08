package attestation

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// subjectPrefixShape requires a fully-qualified "repo:<owner>/<repo>:"
// SubjectPrefix. Defence-in-depth: pkg/config enforces the same shape at
// startup, but the verifier must refuse to construct with an unbindable
// prefix even when built outside that path. The prefix is the sole
// repository binding — matchWorkflow deliberately ignores the owner/repo
// segment — so an under-qualified value silently admits any repo's token
// (#2021).
var subjectPrefixShape = regexp.MustCompile(`^repo:[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9][A-Za-z0-9._-]*:`)

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
		// SupportedSigningAlgs is pinned explicitly. Leaving it nil does NOT
		// select go-oidc's RS256 default on this path: Provider.Verifier
		// backfills an empty list from p.algorithms — the
		// id_token_signing_alg_values_supported array parsed out of the
		// issuer's /.well-known/openid-configuration by the NewProvider call
		// above. The library's hardcoded RS256 fallback fires only when config
		// AND discovery are both empty, so it is unreachable here.
		//
		// Be precise about what this closes. Unpinned, the accepted set is that
		// advertised list INTERSECTED with go-oidc's own supportedAlgorithms
		// map, which already excludes HS256 and "none" — so this is not an
		// alg:none or HMAC-confusion hole. What it closes is a
		// key-reuse-across-algorithm downgrade: EdDSA, PS256 and RS512 are the
		// cases where this pin is the sole blocker, each still requiring a
		// private key present in the issuer's JWKS. p.algorithms is captured
		// once at NewProvider, so a widening took effect on the next process
		// start rather than instantly.
		//
		// RS256 is the correct pin because the GitHub Actions JWKS publishes
		// RSA keys exclusively. CWE-757, algorithm downgrade during
		// negotiation: preventative, bound at Concord's own trust boundary
		// rather than inherited from the issuer. Verified against go-oidc
		// v3.21.0 (verify.go, newVerifier) and GitHub's live discovery document
		// on 2026-09-08; re-check on a dependency bump. The TestOIDCVerifier_*
		// fixture tests are the enforcement.
		verifier: p.Verifier(&oidc.Config{
			ClientID:             cfg.Audience,
			SupportedSigningAlgs: []string{oidc.RS256},
		}),
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
	if !subjectPrefixShape.MatchString(cfg.SubjectPrefix) {
		return fmt.Errorf("oidc config: SubjectPrefix %q is not a fully-qualified %q prefix", cfg.SubjectPrefix, "repo:<owner>/<repo>:")
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

// matchWorkflowRef reports whether the GitHub OIDC `workflow_ref` claim names
// the configured workflow running on the configured ref.
//
// GitHub's canonical `workflow_ref` shape is
//
//	<owner>/<repo>/.github/workflows/<filename>@<ref>
//
// so the check is a single exact-suffix comparison against
// `/.github/workflows/<configured>@<ref>`. It performs NO split.
//
// The predecessor, matchWorkflow, split on the LAST `@` and justified that in
// its own doc comment with the premise that a ref "may contain its own `/`
// segments but never an `@`". That premise is FALSE — `git check-ref-format
// refs/heads/feature/a@b` exits 0. Under the split, a claim whose ref portion
// itself contains `/.github/workflows/<configured>@` relocates the split point
// and the guard passes for a claim naming a different workflow:
//
//	.../.github/workflows/evil.yml@refs/heads/x/.github/workflows/main-cd.yml@y
//
// That was never exploitable end-to-end — git forbids a path component
// beginning with `.`, so no mintable ref carries the anchor, and the separate
// exact-`ref` equality check held independently. But it was safe only by an
// external invariant it neither stated nor enforced, one of whose legs is an
// operator-settable env var. Comparing instead of parsing eliminates the class.
//
// Folding the ref into the same literal also closes a second gap: the previous
// pair of checks never cross-checked each other, so a claim whose workflow_ref
// embedded one ref while the `ref` claim named another satisfied both.
//
// `configured` and `ref` are operator config, never wire data; `claim` is the
// only attacker-influenced input, and it is compared rather than parsed.
// Surfaced by @red-team on PR #3207 with an executable PoC.
//
// Returns false on any empty argument, and on a claim that does not end in the
// exact configured workflow-and-ref literal.
func matchWorkflowRef(claim, configured, ref string) bool {
	if claim == "" || configured == "" || ref == "" {
		return false
	}
	return strings.HasSuffix(claim, githubWorkflowsPathSegment+configured+"@"+ref)
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
	// Ref is checked FIRST: matchWorkflowRef folds the ref into the workflow
	// literal, so a wrong ref would otherwise surface as ErrOIDCInvalidWorkflow
	// and lose the diagnostic distinction between the two sentinels.
	if c.Ref != v.cfg.SPARef {
		return ErrOIDCInvalidRef
	}
	if !matchWorkflowRef(c.Workflow, v.cfg.SPAWorkflow, v.cfg.SPARef) {
		return ErrOIDCInvalidWorkflow
	}
	return nil
}

// applyBinaryPolicy mirrors applySPAPolicy for the binary publish axis.
// Identical structural shape — the only difference is which (Workflow, Ref)
// pair from cfg is enforced. Keeping the two helpers separate (rather than a
// shared parameterized check) makes the cross-axis rejection property obvious
// from the call site in VerifyBinary.
func (v *OIDCVerifier) applyBinaryPolicy(c ghOIDCClaims) error {
	if c.Ref != v.cfg.BinaryRef {
		return ErrOIDCInvalidRef
	}
	if !matchWorkflowRef(c.Workflow, v.cfg.BinaryWorkflow, v.cfg.BinaryRef) {
		return ErrOIDCInvalidWorkflow
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
