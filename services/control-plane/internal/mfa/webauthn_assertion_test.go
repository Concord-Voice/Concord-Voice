package mfa

// Tests for WebAuthnService.ValidateAssertion (#3680 PR3 Task 7): a helper that
// reports, for THIS assertion only, whether the authenticator is device-bound
// (BE=0) and whether it verified the user (UV=1).
//
// Every assertion here is signed by vaAuthenticator, a minimal in-test software
// authenticator (P-256 key, hand-built authenticatorData, real clientDataJSON),
// so go-webauthn's ValidateLogin runs unmodified against it.
//
// Three library facts shape the fixtures (go-webauthn v0.18.2, login.go):
//   - ValidateLogin refuses an assertion whose BE differs from the STORED
//     credential's Flags.BackupEligible, and refuses BE=0 with BS=1.
//   - The credential it returns carries flags merged by CredentialFlags.Update:
//     UV is the STORED value (latched), not the assertion's. So UV must be read
//     from the parsed assertion, and a stored UV=true must not leak into the
//     result.
//   - Concord's production loader (handlers.go) builds credentials without
//     Flags, so a stored credential is zero-Flags today. BE=0 cases therefore
//     store zero Flags; the BE=1 case stores BackupEligible=true, which is the
//     shape once registration persists BE.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vaRPID   = "webauthn.io"
	vaOrigin = "https://webauthn.io"

	// vaSignCount is the counter every valid fixture assertion presents. It is
	// above the stored counter (zero) so the replay check cannot be the reason
	// an assertion is accepted or refused.
	vaSignCount uint32 = 7

	vaFlagsUPUV     = protocol.FlagUserPresent | protocol.FlagUserVerified
	vaFlagsUPUVBE   = vaFlagsUPUV | protocol.FlagBackupEligible
	vaFlagsUPOnly   = protocol.FlagUserPresent
	vaFlagsUPUVBSNo = vaFlagsUPUV | protocol.FlagBackupState
)

// vaAuthenticator is a software authenticator holding one credential.
type vaAuthenticator struct {
	signer *ecdsa.PrivateKey
	credID []byte
	cose   []byte
}

func vaNewSigner(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	signer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return signer
}

func vaNewAuthenticator(t *testing.T) *vaAuthenticator {
	t.Helper()
	signer := vaNewSigner(t)
	pub, err := signer.PublicKey.ECDH()
	require.NoError(t, err)
	point := pub.Bytes() // uncompressed point: marker || X || Y
	require.Len(t, point, 65)
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         int64(webauthncose.P256),
		XCoord:        point[1:33],
		YCoord:        point[33:65],
	})
	require.NoError(t, err)
	return &vaAuthenticator{signer: signer, credID: []byte("va-credential-id"), cose: cose}
}

// vaAssertionSpec describes one assertion the authenticator produces.
type vaAssertionSpec struct {
	flags   protocol.AuthenticatorFlags
	counter uint32
	// signWith overrides the signing key; nil signs with the credential's own.
	signWith *ecdsa.PrivateKey
	// tamper, when set, mutates the authenticatorData AFTER it was signed.
	tamper func(authData []byte)
}

// assertion builds the JSON a client posts after navigator.credentials.get().
func (a *vaAuthenticator) assertion(t *testing.T, challenge string, spec vaAssertionSpec) []byte {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{
		"type": "webauthn.get", "challenge": challenge, "origin": vaOrigin, "crossOrigin": false,
	})
	require.NoError(t, err)
	rpIDHash := sha256.Sum256([]byte(vaRPID))
	authData := append([]byte{}, rpIDHash[:]...)
	authData = append(authData, byte(spec.flags))
	authData = binary.BigEndian.AppendUint32(authData, spec.counter)
	clientHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientHash[:]...))
	signer := spec.signWith
	if signer == nil {
		signer = a.signer
	}
	sig, err := ecdsa.SignASN1(rand.Reader, signer, digest[:])
	require.NoError(t, err)
	if spec.tamper != nil {
		spec.tamper(authData)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	body, err := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response": map[string]any{
			"authenticatorData": b64(authData),
			"clientDataJSON":    b64(clientData),
			"signature":         b64(sig),
		},
	})
	require.NoError(t, err)
	return body
}

// vaFixture is a service, a user holding the authenticator's credential, and a
// login session with a fresh challenge.
type vaFixture struct {
	svc       *WebAuthnService
	auth      *vaAuthenticator
	user      *WebAuthnUser
	session   webauthn.SessionData
	challenge string
}

// vaNewFixture stores the credential with the given Flags. The session carries
// the default UserVerification (not required), so UP-only assertions pass.
func vaNewFixture(t *testing.T, stored webauthn.CredentialFlags) *vaFixture {
	t.Helper()
	svc, err := NewWebAuthnService(vaRPID, "test", []string{vaOrigin})
	require.NoError(t, err)
	auth := vaNewAuthenticator(t)
	raw := make([]byte, 32)
	_, err = rand.Read(raw)
	require.NoError(t, err)
	challenge := base64.RawURLEncoding.EncodeToString(raw)
	userHandle := []byte("va-user-handle")
	user := &WebAuthnUser{
		ID:          userHandle,
		Name:        "va-user",
		DisplayName: "VA User",
		Credentials: []webauthn.Credential{{
			ID:        auth.credID,
			PublicKey: auth.cose,
			Flags:     stored,
		}},
	}
	return &vaFixture{
		svc:  svc,
		auth: auth,
		user: user,
		session: webauthn.SessionData{
			Challenge:            challenge,
			UserID:               userHandle,
			AllowedCredentialIDs: [][]byte{auth.credID},
		},
		challenge: challenge,
	}
}

func (f *vaFixture) assertion(t *testing.T, spec vaAssertionSpec) []byte {
	t.Helper()
	return f.auth.assertion(t, f.challenge, spec)
}

// requireZeroResult pins the error contract: any error returns the zero
// AssertionResult, with no credential and no flags.
func requireZeroResult(t *testing.T, res AssertionResult) {
	t.Helper()
	assert.Nil(t, res.Credential, "an error must not return a credential")
	assert.False(t, res.UserVerified, "an error must not report user verification")
	assert.False(t, res.DeviceBound, "an error must not report a device-bound credential")
	assert.Equal(t, AssertionResult{}, res, "an error must return the zero AssertionResult")
}

func TestValidateAssertion_BEZeroIsDeviceBound(t *testing.T) {
	f := vaNewFixture(t, webauthn.CredentialFlags{})
	body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPUV, counter: vaSignCount})

	res, err := f.svc.ValidateAssertion(f.user, f.session, body)

	require.NoError(t, err)
	require.NotNil(t, res.Credential)
	assert.Equal(t, f.auth.credID, res.Credential.ID)
	assert.Equal(t, vaSignCount, res.Credential.Authenticator.SignCount, "the presented counter must be recorded")
	assert.True(t, res.UserVerified, "UV=1 in this assertion must be reported")
	assert.True(t, res.DeviceBound, "BE=0 in this assertion is device-bound")
}

func TestValidateAssertion_BEOneIsNotDeviceBound(t *testing.T) {
	// The stored credential carries BackupEligible=true: this is the shape once
	// registration persists BE. The production loader does not produce it today
	// (it builds credentials without Flags); see
	// TestValidateAssertion_StoredZeroFlagsRefusesBackupEligible.
	f := vaNewFixture(t, webauthn.CredentialFlags{BackupEligible: true})
	body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPUVBE, counter: vaSignCount})

	res, err := f.svc.ValidateAssertion(f.user, f.session, body)

	require.NoError(t, err)
	require.NotNil(t, res.Credential)
	assert.Equal(t, f.auth.credID, res.Credential.ID)
	assert.Equal(t, vaSignCount, res.Credential.Authenticator.SignCount)
	assert.True(t, res.UserVerified, "UV=1 in this assertion must be reported")
	assert.False(t, res.DeviceBound, "BE=1 (a synced passkey) is not device-bound")
}

func TestValidateAssertion_NoUVReported(t *testing.T) {
	// UP only, default (not required) UserVerification: the library accepts it,
	// and the result must say the user was NOT verified.
	f := vaNewFixture(t, webauthn.CredentialFlags{})
	body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPOnly, counter: vaSignCount})

	res, err := f.svc.ValidateAssertion(f.user, f.session, body)

	require.NoError(t, err)
	require.NotNil(t, res.Credential)
	assert.Equal(t, f.auth.credID, res.Credential.ID)
	assert.Equal(t, vaSignCount, res.Credential.Authenticator.SignCount)
	assert.False(t, res.UserVerified, "UV=0 in this assertion must be reported as not verified")
	assert.True(t, res.DeviceBound, "BE=0 is device-bound regardless of UV")
}

// The returned credential's UV is the STORED, latched value. A stored UV=true
// must not make a UP-only assertion report a verified user: UV is read from
// this assertion's authenticator data.
func TestValidateAssertion_UserVerifiedComesFromAssertionNotStoredCredential(t *testing.T) {
	f := vaNewFixture(t, webauthn.NewCredentialFlags(vaFlagsUPUV))
	body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPOnly, counter: vaSignCount})

	res, err := f.svc.ValidateAssertion(f.user, f.session, body)

	require.NoError(t, err)
	require.NotNil(t, res.Credential)
	assert.Equal(t, f.auth.credID, res.Credential.ID)
	assert.Equal(t, vaSignCount, res.Credential.Authenticator.SignCount)
	// Control: the library really does carry the stored UV into the credential
	// it returns, so reading UV from there would have reported true.
	assert.True(t, res.Credential.Flags.UserVerified, "premise: go-webauthn latches the stored UV in the returned credential")
	assert.False(t, res.UserVerified, "this assertion did not verify the user")
	assert.True(t, res.DeviceBound)
}

func TestValidateAssertion_InvalidSignatureErrors(t *testing.T) {
	f := vaNewFixture(t, webauthn.CredentialFlags{})
	// Well-formed in every respect except the signer: flags UP|UV, BE=0.
	body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPUV, counter: vaSignCount, signWith: vaNewSigner(t)})

	res, err := f.svc.ValidateAssertion(f.user, f.session, body)

	require.Error(t, err)
	var perr *protocol.Error
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, protocol.ErrAssertionSignature.Type, perr.Type, "the refusal must be the signature check, not some other failure")
	assert.Contains(t, perr.Details, "Error validating the assertion signature", "a public-key parse failure shares the Type and must not pass")
	requireZeroResult(t, res)
}

// The UV flag is covered by the signature: setting it after signing must fail
// signature verification, so UserVerified cannot be forged by editing the
// authenticatorData of a UP-only assertion.
func TestValidateAssertion_TamperedFlagsFailSignature(t *testing.T) {
	f := vaNewFixture(t, webauthn.CredentialFlags{})
	body := f.assertion(t, vaAssertionSpec{
		flags:   vaFlagsUPOnly,
		counter: vaSignCount,
		tamper: func(authData []byte) {
			authData[sha256.Size] |= byte(protocol.FlagUserVerified)
		},
	})

	res, err := f.svc.ValidateAssertion(f.user, f.session, body)

	require.Error(t, err)
	var perr *protocol.Error
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, protocol.ErrAssertionSignature.Type, perr.Type)
	assert.Contains(t, perr.Details, "Error validating the assertion signature")
	requireZeroResult(t, res)
}

func TestValidateAssertion_MalformedBytesErrors(t *testing.T) {
	f := vaNewFixture(t, webauthn.CredentialFlags{})

	res, err := f.svc.ValidateAssertion(f.user, f.session, []byte(`{"id": not json`))

	require.Error(t, err)
	requireZeroResult(t, res)
}

// Production shape: the loader stores zero Flags, so a BE=1 assertion fails
// ValidateLogin's BE-consistency check. This pins the fail-closed property the
// device-bound claim relies on (no path reports DeviceBound=true for a BE=1
// assertion) and records the pre-existing defect that a synced passkey cannot
// complete login until registration persists BE.
func TestValidateAssertion_StoredZeroFlagsRefusesBackupEligible(t *testing.T) {
	f := vaNewFixture(t, webauthn.CredentialFlags{})
	body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPUVBE, counter: vaSignCount})

	res, err := f.svc.ValidateAssertion(f.user, f.session, body)

	require.Error(t, err)
	assert.ErrorContains(t, err, "Backup Eligible flag inconsistency", "the refusal must be the BE-consistency check")
	requireZeroResult(t, res)
}

// BE=0 with BS=1 is an impossible combination the library refuses.
func TestValidateAssertion_BackupStateWithoutEligibilityRefused(t *testing.T) {
	f := vaNewFixture(t, webauthn.CredentialFlags{})
	body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPUVBSNo, counter: vaSignCount})

	res, err := f.svc.ValidateAssertion(f.user, f.session, body)

	require.Error(t, err)
	assert.ErrorContains(t, err, "Backup State Flag is true but Backup Eligible flag is false", "the refusal must be the BE=0/BS=1 check")
	requireZeroResult(t, res)
}

// FinishLoginWithBytes keeps its behaviour: same credential for valid input,
// same error class on a bad signature, same refusals as before.
func TestFinishLoginWithBytes_Unchanged(t *testing.T) {
	t.Run("valid assertion returns the matched credential", func(t *testing.T) {
		f := vaNewFixture(t, webauthn.CredentialFlags{})
		body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPUV, counter: vaSignCount})

		cred, err := f.svc.FinishLoginWithBytes(f.user, f.session, body)

		require.NoError(t, err)
		require.NotNil(t, cred)
		assert.Equal(t, f.auth.credID, cred.ID)
		assert.Equal(t, vaSignCount, cred.Authenticator.SignCount)
	})

	t.Run("bad signature is a signature error and no credential", func(t *testing.T) {
		f := vaNewFixture(t, webauthn.CredentialFlags{})
		body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPUV, counter: vaSignCount, signWith: vaNewSigner(t)})

		cred, err := f.svc.FinishLoginWithBytes(f.user, f.session, body)

		require.Error(t, err)
		var perr *protocol.Error
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, protocol.ErrAssertionSignature.Type, perr.Type)
		assert.Contains(t, perr.Details, "Error validating the assertion signature")
		assert.Nil(t, cred)
	})

	t.Run("malformed bytes are refused with no credential", func(t *testing.T) {
		f := vaNewFixture(t, webauthn.CredentialFlags{})

		cred, err := f.svc.FinishLoginWithBytes(f.user, f.session, []byte(`{"id": not json`))

		require.Error(t, err)
		assert.Nil(t, cred)
	})

	t.Run("BE=1 against zero stored Flags is still refused", func(t *testing.T) {
		f := vaNewFixture(t, webauthn.CredentialFlags{})
		body := f.assertion(t, vaAssertionSpec{flags: vaFlagsUPUVBE, counter: vaSignCount})

		cred, err := f.svc.FinishLoginWithBytes(f.user, f.session, body)

		require.Error(t, err)
		assert.ErrorContains(t, err, "Backup Eligible flag inconsistency")
		assert.Nil(t, cred)
	})
}
