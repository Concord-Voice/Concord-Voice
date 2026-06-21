// Package oauth provides federated identity provider integration.
// Concrete providers (Google, Apple) implement the Provider interface;
// callers consult the Registry by provider name.
package oauth

// Provider abstracts an OAuth/OIDC identity provider.
type Provider interface {
	// Name returns the provider key stored in user_sso_identities.provider
	// (e.g., "google", "apple"). Stable across releases; never renamed.
	Name() string

	// AuthorizationURL builds the URL the client opens in the system browser.
	// state, nonce, codeChallenge are caller-generated CSRF/replay/PKCE values.
	AuthorizationURL(state, nonce, codeChallenge string) string

	// Exchange removed (#975): both providers are now client-driven; the
	// desktop main process performs the code exchange and id_token verification
	// locally, then POSTs the verified id_token to POST /auth/sso/:provider/session.
	// id_token verification happens at /session via the concrete provider type.
}

// UserInfo is the normalized identity returned by all providers.
type UserInfo struct {
	Provider       string // matches Provider.Name()
	ProviderUserID string // OIDC sub claim
	Email          string
	EmailVerified  bool
	Name           string // may be empty (Apple subsequent auths)
	AvatarURL      string // may be empty (Apple, or Google users without picture)
	// IsRelayEmail is true if Email is a provider-issued forwarding alias
	// (e.g., Apple privaterelay.appleid.com). Auto-link flows MUST refuse to
	// link relay addresses to existing accounts. Always false for Google.
	IsRelayEmail bool
}
