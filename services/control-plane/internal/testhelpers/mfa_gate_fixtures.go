package testhelpers

// Fixtures for the #3454 dangerous-action gates: an inline factor for the
// actor, an enforcing server, and a verifier that counts the confirmations a
// gate asked for. mfaenforce's own fixtures_test.go cannot be imported, and it
// seals a real secret for the real verifier; these are for handler tests that
// assert WHETHER a gate verified, not how a code is checked.

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// EnrollInlineTOTP gives userID a TOTP row that policy P1 counts as an inline
// factor: enabled AND confirmed (see stepup.InlineMFAMethods). The secret is
// opaque bytes, so no real verifier can accept a code for it; pair it with
// FakeMFAVerifier.
func EnrollInlineTOTP(t testing.TB, db *sql.DB, userID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed)
		VALUES ($1, '\x00'::bytea, '\x00'::bytea, TRUE, TRUE)`, userID)
	if err != nil {
		t.Fatalf("testhelpers: enroll inline TOTP: %v", err)
	}
}

// SetServerMFAEnforcement sets servers.enforce_mfa_dangerous_actions directly,
// bypassing the toggle endpoint and its confirmation.
func SetServerMFAEnforcement(t testing.TB, db *sql.DB, serverID string, enforcing bool) {
	t.Helper()
	res, err := db.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = $2 WHERE id = $1`, serverID, enforcing)
	if err != nil {
		t.Fatalf("testhelpers: set MFA enforcement: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("testhelpers: set MFA enforcement: server %s not found", serverID)
	}
}

// FakeMFAVerifier is a stepup.MFATxCodeVerifier that accepts exactly
// AcceptCode and records every VerifyCodeTx call. Safe for concurrent use.
//
// A gate that consults the verifier on a server that does not enforce, or more
// than once per attempt, shows up in Calls; a gate confirming under another
// route's purpose shows up in Purposes.
type FakeMFAVerifier struct {
	// AcceptCode is the one code VerifyCodeTx accepts. Empty accepts nothing.
	AcceptCode string
	// Methods answers GetEnabledMethods. Nil answers {"totp"}.
	Methods []string
	// Err, when set, is returned by VerifyCodeTx in place of a verdict.
	Err error

	mu       sync.Mutex
	purposes []stepup.Purpose
}

var _ stepup.MFATxCodeVerifier = (*FakeMFAVerifier)(nil)

// GetEnabledMethods implements stepup.MFAMethodLister.
func (f *FakeMFAVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	if f.Methods == nil {
		return []string{"totp"}, nil
	}
	return f.Methods, nil
}

// VerifyCodeTx implements stepup.MFATxCodeVerifier.
func (f *FakeMFAVerifier) VerifyCodeTx(_ context.Context, _ *sql.Tx, _ string, purpose stepup.Purpose, code string) (bool, error) {
	f.mu.Lock()
	f.purposes = append(f.purposes, purpose)
	f.mu.Unlock()
	if f.Err != nil {
		return false, f.Err
	}
	return f.AcceptCode != "" && code == f.AcceptCode, nil
}

// Calls is the number of VerifyCodeTx calls so far.
func (f *FakeMFAVerifier) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.purposes)
}

// Purposes returns a copy of the purpose each VerifyCodeTx call carried, in
// call order.
func (f *FakeMFAVerifier) Purposes() []stepup.Purpose {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stepup.Purpose(nil), f.purposes...)
}
