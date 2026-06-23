package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_AdminWebAuthnVars verifies the three ADMIN_WEBAUTHN_* env vars are
// parsed onto the Config: RP_ID is a plain string, ORIGINS and ALLOWED_AAGUIDS
// are comma-split slices (same parseOrigins helper, trimmed, empties dropped).
// These power the dedicated admin WebAuthn relying-party (#1688), kept fully
// separate from the user-facing WEBAUTHN_RP_* so admin keys and user passkeys
// cannot cross-validate.
func TestLoad_AdminWebAuthnVars(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development") // skip production guards
	t.Setenv("ADMIN_WEBAUTHN_RP_ID", "admin.concordvoice.chat")
	t.Setenv("ADMIN_WEBAUTHN_RP_ORIGINS", "https://admin.concordvoice.chat , https://admin2.concordvoice.chat ")
	t.Setenv("ADMIN_WEBAUTHN_ALLOWED_AAGUIDS", "ee882879-721c-4913-9775-3dfcce97072a,fa2b99dc-9e39-4257-8f92-4a30d23c4118")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "admin.concordvoice.chat", cfg.AdminWebAuthnRPID)
	assert.Equal(t, []string{
		"https://admin.concordvoice.chat",
		"https://admin2.concordvoice.chat",
	}, cfg.AdminWebAuthnRPOrigins)
	assert.Equal(t, []string{
		"ee882879-721c-4913-9775-3dfcce97072a",
		"fa2b99dc-9e39-4257-8f92-4a30d23c4118",
	}, cfg.AdminWebAuthnAllowedAAGUIDs)
}

// TestValidateProductionRejectsMissingAdminWebAuthnRPID asserts that when the
// admin console is ENABLED, an empty ADMIN_WEBAUTHN_RP_ID fatal-fails in prod:
// the console is a browser RP and an empty RP ID silently mis-scopes every
// ceremony. (The guard is conditional on ADMIN_CONSOLE_ENABLED — see the dormant
// test below — because #1688 ships the surface off by default.)
func TestValidateProductionRejectsMissingAdminWebAuthnRPID(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = true
	cfg.AdminWebAuthnRPID = ""

	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ADMIN_WEBAUTHN_RP_ID")
}

// TestValidateProductionRejectsLocalhostAdminWebAuthnRPID asserts that an ENABLED
// console must override the dev "localhost" RP default in production — the dead
// guard this replaces could never catch this (the non-empty default masked it).
func TestValidateProductionRejectsLocalhostAdminWebAuthnRPID(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = true
	cfg.AdminWebAuthnRPID = "localhost"

	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ADMIN_WEBAUTHN_RP_ID")
}

// TestValidateProductionRejectsMissingAdminWebAuthnRPOrigins asserts the guard
// fatal-fails when an ENABLED console resolves an empty origin set.
func TestValidateProductionRejectsMissingAdminWebAuthnRPOrigins(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = true
	cfg.AdminWebAuthnRPOrigins = nil

	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ADMIN_WEBAUTHN_RP_ORIGINS")
}

// TestValidateProductionRejectsEmptyAdminAAGUIDs asserts that an ENABLED console
// requires a non-empty AAGUID allow-list in production: checkAAGUID is fail-closed,
// so an empty list would brick enrollment — the guard turns that silent footgun
// into a loud startup error (Gitar #1703).
func TestValidateProductionRejectsEmptyAdminAAGUIDs(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = true
	cfg.AdminWebAuthnAllowedAAGUIDs = nil

	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ADMIN_WEBAUTHN_ALLOWED_AAGUIDS")
}

// TestValidateProductionDormantConsoleNeedsNoAdminConfig is the prod-safety
// property: when the admin console is DISABLED (the #1688 default), a production
// config with NO admin RP config still validates — the dormant surface must not
// break deploys before #1691/#1692 enable it (the FEEDBACK_PAT #1547 outage class).
func TestValidateProductionDormantConsoleNeedsNoAdminConfig(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = false
	cfg.AdminWebAuthnRPID = ""
	cfg.AdminWebAuthnRPOrigins = nil

	assert.NoError(t, cfg.validate())
}

// TestValidateProductionAcceptsEnabledAdminWebAuthnSet is the positive guard: an
// ENABLED console with a real (non-localhost) RP config passes validate().
func TestValidateProductionAcceptsEnabledAdminWebAuthnSet(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = true
	assert.NoError(t, cfg.validate())
}

func TestLoad_CFAccessVars(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("CF_ACCESS_AUD", "access-aud")
	t.Setenv("CF_ACCESS_TEAM_DOMAIN", "https://team.cloudflareaccess.com")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "access-aud", cfg.CFAccessAUD)
	assert.Equal(t, "https://team.cloudflareaccess.com", cfg.CFAccessTeamDomain)
}

func TestValidateProductionRejectsEnabledAdminWithoutCFAccessAUD(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = true
	cfg.CFAccessAUD = ""

	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CF_ACCESS_AUD")
}

func TestValidateProductionRejectsEnabledAdminWithoutHTTPSCFAccessTeamDomain(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = true
	cfg.CFAccessTeamDomain = "team.cloudflareaccess.com"

	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CF_ACCESS_TEAM_DOMAIN")
}

func TestValidateProductionDormantConsoleNeedsNoCFAccessConfig(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = false
	cfg.CFAccessAUD = ""
	cfg.CFAccessTeamDomain = ""

	assert.NoError(t, cfg.validate())
}

func TestValidateProductionAcceptsEnabledAdminCFAccessSet(t *testing.T) {
	cfg := validProductionConfig()
	cfg.AdminConsoleEnabled = true
	assert.NoError(t, cfg.validate())
}
