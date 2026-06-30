// Package servercapabilities serves the public pre-auth capability descriptor
// (#662). Clients fetch it before login to discover server auth options and to
// clamp their feature surface to what the server advertises (version skew).
// Mirrors internal/clientconfig: public, no auth, config-derived, no secrets.
package servercapabilities

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
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
}

// NewHandler creates a capabilities handler. No logger: the handler is a pure
// config read with no error paths to log.
func NewHandler(cfg *config.Config) *Handler {
	return &Handler{cfg: cfg}
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
	VoiceTiersSupported    bool   `json:"voiceTiersSupported"`
	E2EEEnforcedEverywhere bool   `json:"e2eeEnforcedEverywhere"`
	MaxMembersPerServer    int    `json:"maxMembersPerServer"`
	EntitlementMode        string `json:"entitlementMode"`
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
			VoiceTiersSupported:    instanceType == config.InstanceTypeSaaS,
			E2EEEnforcedEverywhere: true,
			MaxMembersPerServer:    maxMembersPerServer,
			EntitlementMode:        entitlementMode,
		},
		PolicyVersion: policyVersion,
	}

	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, resp)
}
