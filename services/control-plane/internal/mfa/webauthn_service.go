package mfa

import (
	"fmt"
	"net/http"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// WebAuthnService wraps the go-webauthn library for registration and authentication.
type WebAuthnService struct {
	wan *webauthn.WebAuthn
}

// NewWebAuthnService creates a WebAuthn relying party configuration.
func NewWebAuthnService(rpID, rpDisplayName string, rpOrigins []string) (*WebAuthnService, error) {
	cfg := &webauthn.Config{
		RPID:          rpID,
		RPDisplayName: rpDisplayName,
		RPOrigins:     rpOrigins,
	}

	wan, err := webauthn.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("create webauthn: %w", err)
	}

	return &WebAuthnService{wan: wan}, nil
}

// WebAuthnUser implements the webauthn.User interface for credential operations.
type WebAuthnUser struct {
	ID          []byte
	Name        string
	DisplayName string
	Credentials []webauthn.Credential
}

// WebAuthnID implements webauthn.User.
func (u *WebAuthnUser) WebAuthnID() []byte { return u.ID }

// WebAuthnName implements webauthn.User.
func (u *WebAuthnUser) WebAuthnName() string { return u.Name }

// WebAuthnDisplayName implements webauthn.User.
func (u *WebAuthnUser) WebAuthnDisplayName() string { return u.DisplayName }

// WebAuthnCredentials implements webauthn.User.
func (u *WebAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

// BeginRegistration starts the WebAuthn credential creation ceremony.
// Returns the creation options (to send to the client) and session data (to store server-side).
func (s *WebAuthnService) BeginRegistration(user *WebAuthnUser, opts ...webauthn.RegistrationOption) (*protocol.CredentialCreation, *webauthn.SessionData, error) {
	return s.wan.BeginRegistration(user, opts...)
}

// FinishRegistration completes the WebAuthn credential creation ceremony.
// Validates the attestation response from the client and returns the new credential.
func (s *WebAuthnService) FinishRegistration(user *WebAuthnUser, sessionData webauthn.SessionData, request *http.Request) (*webauthn.Credential, error) {
	return s.wan.FinishRegistration(user, sessionData, request)
}

// BeginLogin starts the WebAuthn authentication ceremony.
// Returns the assertion options (to send to the client) and session data (to store server-side).
func (s *WebAuthnService) BeginLogin(user *WebAuthnUser, opts ...webauthn.LoginOption) (*protocol.CredentialAssertion, *webauthn.SessionData, error) {
	return s.wan.BeginLogin(user, opts...)
}

// FinishLogin completes the WebAuthn authentication ceremony.
// Validates the assertion response and returns the matched credential (with updated sign count).
func (s *WebAuthnService) FinishLogin(user *WebAuthnUser, sessionData webauthn.SessionData, request *http.Request) (*webauthn.Credential, error) {
	return s.wan.FinishLogin(user, sessionData, request)
}

// FinishLoginWithBytes completes the WebAuthn authentication ceremony using raw assertion bytes.
// Use this when the request body has already been consumed (e.g., by JSON binding).
func (s *WebAuthnService) FinishLoginWithBytes(user *WebAuthnUser, sessionData webauthn.SessionData, assertionBytes []byte) (*webauthn.Credential, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertionBytes)
	if err != nil {
		return nil, err
	}
	return s.wan.ValidateLogin(user, sessionData, parsed)
}
