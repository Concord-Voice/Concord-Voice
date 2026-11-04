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
		RPID:                              rpID,
		RPDisplayName:                     rpDisplayName,
		RPOrigins:                         rpOrigins,
		ExtensionsUnsolicitedOutputPolicy: protocol.UnsolicitedOutputPolicyReject,
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
// It is ValidateAssertion without the per-assertion flags.
func (s *WebAuthnService) FinishLoginWithBytes(user *WebAuthnUser, sessionData webauthn.SessionData, assertionBytes []byte) (*webauthn.Credential, error) {
	res, err := s.ValidateAssertion(user, sessionData, assertionBytes)
	return res.Credential, err
}

// AssertionResult is a validated WebAuthn assertion and what THIS assertion proved.
//
// UserVerified and DeviceBound are read from the assertion's own authenticator data, which
// the verified signature covers, never from a credential. go-webauthn latches UserVerified
// on the credential it returns (CredentialFlags.Update keeps the stored value), so the
// credential's Flags can report a past verification for an assertion that verified nobody,
// and can equally clear the UV this assertion did report when the stored value is false.
// Reading UV from the credential is wrong in both directions.
type AssertionResult struct {
	// Credential is the matched credential after go-webauthn's counter update: SignCount
	// advanced, or Authenticator.CloneWarning set when the counter did not increase (an
	// authenticator whose counter stays zero gets neither).
	Credential *webauthn.Credential
	// UserVerified is the assertion's UV flag: the authenticator verified the user.
	UserVerified bool
	// DeviceBound is the assertion's BE flag clear: the authenticator reports the credential
	// as not backup-eligible. This is self-reported and not attested (no attestation or MDS
	// policy is configured); it does not prove a hardware-bound key.
	DeviceBound bool
}

// ValidateAssertion completes the WebAuthn authentication ceremony using raw assertion bytes
// and reports the assertion's UV and BE flags. Any error returns the zero AssertionResult.
//
// A BE=1 assertion verifies only if the stored credential records BackupEligible;
// go-webauthn refuses a mismatch.
func (s *WebAuthnService) ValidateAssertion(user *WebAuthnUser, session webauthn.SessionData, assertion []byte) (AssertionResult, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertion)
	if err != nil {
		return AssertionResult{}, err
	}
	cred, err := s.wan.ValidateLogin(user, session, parsed)
	if err != nil {
		return AssertionResult{}, err
	}
	flags := parsed.Response.AuthenticatorData.Flags
	return AssertionResult{
		Credential:   cred,
		UserVerified: flags.HasUserVerified(),
		DeviceBound:  !flags.HasBackupEligible(),
	}, nil
}
