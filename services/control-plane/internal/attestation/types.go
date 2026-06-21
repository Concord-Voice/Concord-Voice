// Package attestation implements client attestation for example.com (#677).
// See [internal]0010-client-attestation.md for design decisions.
package attestation

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// hashFormatPattern matches sha256:<64 lowercase hex chars>. Used by
// ValidateHash to gate publish payloads at the handler boundary so malformed
// hashes from CI runners produce a 400 with a clear message rather than
// reaching downstream consumers (cache, middleware) where they would surface
// as confusing UNKNOWN_RELEASE rejections at verify time.
//
// Reference format: see captureSpaHash in
// client/desktop/src/main/spaLoader.ts (createHash('sha256').digest('hex'))
// and main-cd.yml SPA hash step (shasum -a 256 ... | awk '{print $1}').
// Per finding #22 of #1264 review.
var hashFormatPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidateHash returns nil if h matches the canonical sha256:<lowercase-hex>
// format, or an error describing the mismatch. The handler short-circuits
// with a 400 on error.
func ValidateHash(h string) error {
	if !hashFormatPattern.MatchString(h) {
		return fmt.Errorf("hash must match format sha256:<64 lowercase hex chars>, got %q", h)
	}
	return nil
}

// Platform identifies the client OS family for attestation purposes.
type Platform string

// Supported platform values for attestation.
const (
	PlatformMacOS   Platform = "macos"
	PlatformWindows Platform = "windows"
	PlatformLinux   Platform = "linux"
	PlatformWeb     Platform = "web"
)

// ErrInvalidPlatform is the sentinel error returned by ParsePlatform when the
// input string does not match a recognised Platform constant. Use errors.Is
// at call sites to distinguish from other validation failures.
var ErrInvalidPlatform = errors.New("attestation: invalid platform")

// ParsePlatform converts a wire-string platform identifier into a typed
// Platform, returning ErrInvalidPlatform for any unrecognised value. Prefer
// this constructor over raw Platform(s) casts at trust boundaries (HTTP
// handlers, NATS payload decoders, registry-row hydration) so the cast and
// the Valid() check happen together in a single, audited step.
//
// Raw Platform(s) casts remain syntactically possible inside the package
// (the type's underlying kind is `string`, not opaque), and are intentionally
// allowed for tests that need to construct invalid values to exercise the
// rejection path. Production code paths should call ParsePlatform.
func ParsePlatform(s string) (Platform, error) {
	p := Platform(s)
	if !p.Valid() {
		return "", ErrInvalidPlatform
	}
	return p, nil
}

// Valid returns true if p is one of the recognised platforms.
func (p Platform) Valid() bool {
	switch p {
	case PlatformMacOS, PlatformWindows, PlatformLinux, PlatformWeb:
		return true
	}
	return false
}

// RequiresCertHash reports whether platform p requires signal 1 (cert hash).
// Linux is exempt until #653 ships; web has no installed binary.
func (p Platform) RequiresCertHash() bool {
	return p == PlatformMacOS || p == PlatformWindows
}

// RequiresMachineID reports whether platform p requires signal 2 (machine ID).
// Web clients cannot fingerprint hardware via web crypto.
func (p Platform) RequiresMachineID() bool {
	return p != PlatformWeb
}

// VerifyPayload is the body of POST /api/v1/attestation/verify.
type VerifyPayload struct {
	Version    string   `json:"version"     binding:"required"`
	Platform   Platform `json:"platform"    binding:"required"`
	CertHash   string   `json:"cert_hash,omitempty"`
	MachineID  string   `json:"machine_id,omitempty"`
	SpaVersion string   `json:"spa_version" binding:"required"`
	SpaHash    string   `json:"spa_hash"    binding:"required"`
}

// VerifyResponse is returned on successful attestation.
type VerifyResponse struct {
	AttestationToken string    `json:"attestation_token"`
	TTLSeconds       int       `json:"ttl_seconds"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// PublishSPAPayload is the body of POST /api/v1/internal/attestation/publish/spa.
// Published by main-cd.yml on every main-push (no code signing happens there,
// so cert_hash is structurally absent from this axis).
type PublishSPAPayload struct {
	SpaVersion string `json:"spa_version" binding:"required"`
	HTMLHash   string `json:"html_hash"   binding:"required"`
}

// PublishBinaryPayload is the body of POST /api/v1/internal/attestation/publish/binary.
// Published by build-desktop.yml post-signing (cert_hash is available only on the
// binary axis after Authenticode/notarytool steps complete).
type PublishBinaryPayload struct {
	Version  string   `json:"version"   binding:"required"`
	Platform Platform `json:"platform"  binding:"required"`
	CertHash string   `json:"cert_hash" binding:"required"`
}

// RevokePayload is the body of POST /api/v1/internal/attestation/revoke.
type RevokePayload struct {
	Version    string `json:"version,omitempty"`     // exactly one of version or spa_version
	SpaVersion string `json:"spa_version,omitempty"` //nolint:tagliatelle
	Reason     string `json:"reason" binding:"required"`
}

// ErrorCode is the structured `code` field returned in 403 attestation rejections.
type ErrorCode string

// Structured error codes returned in 403 attestation rejection responses.
const (
	ErrMissing        ErrorCode = "ATTESTATION_MISSING"
	ErrExpired        ErrorCode = "ATTESTATION_EXPIRED"
	ErrInvalid        ErrorCode = "ATTESTATION_INVALID"
	ErrUnknownRelease ErrorCode = "ATTESTATION_UNKNOWN_RELEASE"
	ErrRevoked        ErrorCode = "ATTESTATION_REVOKED"
	ErrVersionTooOld  ErrorCode = "CLIENT_VERSION_TOO_OLD"
)

// ErrorResponse is the structured body for 403 rejections.
type ErrorResponse struct {
	Error              string    `json:"error"`
	Code               ErrorCode `json:"code"`
	UpdateAvailable    bool      `json:"updateAvailable,omitempty"`
	RequiredMinVersion string    `json:"requiredMinVersion,omitempty"`
	DownloadHelpURL    string    `json:"downloadHelpUrl,omitempty"`
}

// ReleaseBinary mirrors a row from release_binaries.
type ReleaseBinary struct {
	Version       string
	Platform      Platform
	CertHash      string
	PublishedAt   time.Time
	PublishedBy   string
	RevokedAt     *time.Time
	RevokedReason string
	RevokedBy     string
}

// ReleaseSPA mirrors a row from release_spas.
type ReleaseSPA struct {
	SpaVersion    string
	HTMLHash      string
	PublishedAt   time.Time
	PublishedBy   string
	RevokedAt     *time.Time
	RevokedReason string
	RevokedBy     string
}

// TokenRecord is the JSON payload stored in Redis under attestation:<session>:<machine|web>.
//
// Field-by-field semantics:
//
//   - Token: the opaque attestation token returned to the client.
//   - Version: the client's binary version at attestation time. Used by the
//     RequireAttestation middleware for the O(1) binary-revoke check against
//     the Redis revoked_versions SET (ADR-0010 D13).
//   - SpaVersion: the client's SPA version at attestation time. Currently
//     written by the verify handler but NOT read by the middleware. The
//     binary-revoke path in middleware/attestation.go only checks
//     rec.Version against revoked_versions; SPA revocations are enforced
//     instead by the verify handler at re-attest time (the cache rejects
//     attestation against a revoked SPA version). SpaVersion is captured
//     here so a future middleware-time SPA-revoke check can be wired
//     without a TokenRecord schema migration — readiness wiring per
//     ADR-0010 D13's two-phase revocation model.
//   - IssuedAt: token mint timestamp for audit logging.
//
// Forward-looking note: `rec.SpaVersion` is captured against the
// possibility that a middleware-time SPA-revoke gate is added later
// (e.g., hot-revoke for an SPA hot-update without forcing the user
// through a new verify roundtrip). If that gate is wired, plumb
// `rec.SpaVersion` into ensureNotRevoked alongside `rec.Version`. If
// SPA revocations remain a re-attest concern only, this field and the
// corresponding verify-handler assignment can be removed. Tracked by
// the #677 follow-up surface in ADR-0010 D13.
type TokenRecord struct {
	Token      string    `json:"token"`
	Version    string    `json:"version"`
	SpaVersion string    `json:"spa_version"`
	IssuedAt   time.Time `json:"issued_at"`
}

// DownloadHelpURLDefault is the user-facing download page URL surfaced
// in 403 responses. Not the updater feed — the updater uses its pinned URL.
const DownloadHelpURLDefault = "https://concordvoice.com/download"

// RevokedVersionsKey is the canonical Redis SET name populated by the revoke
// handler and consumed by Cache.IsRevoked + the RequireAttestation middleware
// for O(1) revocation lookup. Per finding #29 of #1264 review: this constant
// is the single source of truth — middleware/attestation.go and
// internal/attestation/cache.go both reference it now (previously each had a
// private duplicate, with drift risk if the SET name ever changed).
const RevokedVersionsKey = "attestation:revoked_versions"
