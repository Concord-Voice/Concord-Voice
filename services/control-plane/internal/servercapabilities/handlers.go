// Package servercapabilities serves the public pre-auth capability descriptor
// (#662). Clients fetch it before login to discover server auth options and to
// clamp their feature surface to what the server advertises (version skew).
// Mirrors internal/clientconfig: public, no auth, config-derived, no secrets.
package servercapabilities

import (
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/gin-gonic/gin"
)

const (
	entitlementSelfHosted = "self-hosted-unlocked"
	defaultServerVersion  = "dev"
	policyVersion         = "2026-06-01"
	maxMembersPerServer   = 500
)

// Handler serves GET /api/v1/server/capabilities.
type Handler struct {
	cfg *config.Config
	// Set by the router to mirror the exact condition under which the chunked
	// upload session routes are registered. See SetChunkedAttachmentUpload.
	chunkedAttachmentUpload bool
}

// NewHandler creates a capabilities handler. No logger: the handler is a pure
// config read with no error paths to log.
func NewHandler(cfg *config.Config) *Handler {
	return &Handler{cfg: cfg}
}

// SetChunkedAttachmentUpload records whether the chunked upload session routes
// are actually reachable on this deployment. It is a setter rather than a
// constructor parameter for the same reason SetSessionRedis is: the wiring is
// known in the router, not in config, and a new parameter would touch every
// call site to say nothing.
//
// The zero value is FALSE, and that is the load-bearing part. This capability
// is not a description of the build -- the routes are compiled in
// unconditionally -- it is a description of whether THIS deployment can serve
// them, and it can fail either of two ways. Without object storage the media
// handler is nil, the routes are never registered, and the client meets a 404.
// With storage but no Redis the routes ARE registered and every one of them
// answers 503. Defaulting to true would advertise both, and the client would
// take it at its word.
func (h *Handler) SetChunkedAttachmentUpload(v bool) {
	h.chunkedAttachmentUpload = v
}

// ServerInfo identifies the server and its deployment type.
type ServerInfo struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	InstanceType string `json:"instanceType"`
}

// AuthInfo advertises auth options so the client renders the right login form.
type AuthInfo struct {
	EmailVerificationRequired bool     `json:"emailVerificationRequired"`
	MFAEnabled                bool     `json:"mfaEnabled"`
	MFAMethods                []string `json:"mfaMethods"`
	OAuthProviders            []string `json:"oauthProviders"`
	LDAPEnabled               bool     `json:"ldapEnabled"`
	SAMLEnabled               bool     `json:"samlEnabled"`
}

// FeaturesInfo advertises the feature surface the client clamps to.
type FeaturesInfo struct {
	VoiceTiersSupported      bool   `json:"voiceTiersSupported"`
	E2EEEnforcedEverywhere   bool   `json:"e2eeEnforcedEverywhere"`
	MaxMembersPerServer      int    `json:"maxMembersPerServer"`
	EntitlementMode          string `json:"entitlementMode"`
	ActivityHistorySupported *bool  `json:"activityHistorySupported,omitempty"`
	// Whether the chunked attachment upload session (#2157 PR 2) is reachable.
	// NOT omitempty: an explicit false is the useful answer for a deployment
	// without object storage, and a client that fails closed on a missing field
	// should never have to guess which of the two it is looking at.
	ChunkedAttachmentUpload bool `json:"chunkedAttachmentUpload"`
}

// Response is the payload for GET /api/v1/server/capabilities. The schema is
// additively-evolvable: new fields are optional; old clients ignore unknown
// fields, new clients tolerate missing ones.
type Response struct {
	Server        ServerInfo   `json:"server"`
	Auth          AuthInfo     `json:"auth"`
	Features      FeaturesInfo `json:"features"`
	PolicyVersion string       `json:"policyVersion"`
}

// GetCapabilities returns the capability descriptor. Public — no auth.
func (h *Handler) GetCapabilities(c *gin.Context) {
	instanceType := config.NormalizeInstanceType(h.cfg.InstanceType)

	mfaMethods := []string{"totp"}
	if h.cfg.WebAuthnRPID != "" {
		mfaMethods = append(mfaMethods, "webauthn")
	}

	oauthProviders := []string{}
	if h.cfg.GoogleSSO.Enabled {
		oauthProviders = append(oauthProviders, "google")
	}
	if h.cfg.AppleSSO.Enabled {
		oauthProviders = append(oauthProviders, "apple")
	}

	entitlementMode := config.InstanceTypeSaaS
	if instanceType == config.InstanceTypeSelfHosted {
		entitlementMode = entitlementSelfHosted
	}

	// Guard the zero-value Config{} test path; Load() already defaults
	// SERVER_VERSION to "dev".
	serverVersion := h.cfg.ServerVersion
	if serverVersion == "" {
		serverVersion = defaultServerVersion
	}

	resp := Response{
		Server: ServerInfo{
			Name:         "Concord Voice",
			Version:      serverVersion,
			InstanceType: instanceType,
		},
		Auth: AuthInfo{
			// Email verification is structurally required for password
			// registration on every deployment — SMTP only changes the delivery
			// channel (real email vs the dev stdout/Redis code path), not whether
			// verification is enforced (see the internal/auth registration flow:
			// pending users are not promoted until ConfirmRegistration). So this
			// is a constant true, not a function of SMTP being configured.
			EmailVerificationRequired: true,
			MFAEnabled:                true,
			MFAMethods:                mfaMethods,
			OAuthProviders:            oauthProviders,
			LDAPEnabled:               false,
			SAMLEnabled:               false,
		},
		Features: FeaturesInfo{
			VoiceTiersSupported:      instanceType == config.InstanceTypeSaaS,
			E2EEEnforcedEverywhere:   true,
			MaxMembersPerServer:      maxMembersPerServer,
			EntitlementMode:          entitlementMode,
			ActivityHistorySupported: activityHistorySupported(h.cfg),
			ChunkedAttachmentUpload:  h.chunkedAttachmentUpload,
		},
		PolicyVersion: policyVersion,
	}

	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, resp)
}

func activityHistorySupported(cfg *config.Config) *bool {
	if cfg == nil || !cfg.ActivityHistoryClusterEnabled ||
		!cfg.ControlPlaneReplicaCountExplicit || cfg.ControlPlaneReplicaCount != 1 {
		return nil
	}
	supported := true
	return &supported
}
