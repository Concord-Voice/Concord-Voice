package admin_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/mfa"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

// -----------------------------------------------------------------------------
// Virtual authenticator harness.
//
// The go-webauthn library ships only static spec test vectors (recorded hex),
// which cannot be re-signed for an arbitrary RP origin / challenge. Verifying
// "reject wrong-RP-origin / stale-challenge / bad-signature / UV-unsatisfied"
// against OUR admin RP config requires a PROGRAMMATIC authenticator that emits
// fresh attestation + assertion objects on demand. This minimal ES256 (ECDSA
// P-256) authenticator builds the exact byte layout the library parses:
//
//	authData = rpIDHash[32] || flags[1] || signCount[4] ||
//	           AAGUID[16] || credIDLen[2] || credID || COSE_pubkey   (attested)
//	authData = rpIDHash[32] || flags[1] || signCount[4]              (assertion)
//
// Registration uses the "none" attestation format (attStmt = {}), which the
// library accepts and stores. Login signs (authData || sha256(clientDataJSON))
// with the ECDSA private key, ASN.1-DER encoded, exactly as a real key would.
// -----------------------------------------------------------------------------

const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagBE = 0x08 // backup eligible
	flagBS = 0x10 // backup state
	flagAT = 0x40 // attested credential data present
)

// virtualAuthenticator is a software FIDO2 authenticator for tests.
type virtualAuthenticator struct {
	aaguid    uuid.UUID
	credID    []byte
	priv      *ecdsa.PrivateKey
	signCount uint32
}

// newVirtualAuthenticator generates a fresh ES256 key + random credential id.
func newVirtualAuthenticator(t *testing.T, aaguid uuid.UUID) *virtualAuthenticator {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	credID := make([]byte, 32)
	_, err = rand.Read(credID)
	require.NoError(t, err)
	return &virtualAuthenticator{aaguid: aaguid, credID: credID, priv: priv, signCount: 1}
}

// coseKey returns the CBOR-encoded COSE_Key (EC2, P-256, ES256) public key.
func (va *virtualAuthenticator) coseKey(t *testing.T) []byte {
	t.Helper()
	// PublicKey.Bytes() (Go 1.25+) returns the SEC1 uncompressed encoding
	// 0x04 || X(32) || Y(32); slice out the fixed-width coordinates. This avoids
	// the deprecated big.Int coordinate fields.
	pub, err := va.priv.PublicKey.Bytes()
	require.NoError(t, err)
	require.Len(t, pub, 65)
	xb := pub[1:33]
	yb := pub[33:65]
	// COSE_Key map keyed by integer labels:
	//   1 (kty) = 2 (EC2); 3 (alg) = -7 (ES256); -1 (crv) = 1 (P-256);
	//   -2 (x); -3 (y). Encode deterministically (canonical) so the library
	//   parses it identically.
	key := map[int]any{
		1:  2,
		3:  -7,
		-1: 1,
		-2: xb,
		-3: yb,
	}
	em, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	b, err := em.Marshal(key)
	require.NoError(t, err)
	return b
}

// authData builds authenticator data. When attested is true, the attested
// credential data block (AAGUID + credID + COSE pubkey) is appended and the AT
// flag is set.
func (va *virtualAuthenticator) authData(t *testing.T, rpID string, flags byte, attested bool) []byte {
	t.Helper()
	rpHash := sha256.Sum256([]byte(rpID))
	var buf bytes.Buffer
	buf.Write(rpHash[:])
	if attested {
		flags |= flagAT
	}
	buf.WriteByte(flags)
	var counter [4]byte
	binary.BigEndian.PutUint32(counter[:], va.signCount)
	buf.Write(counter[:])
	if attested {
		aaguidBytes, err := va.aaguid.MarshalBinary()
		require.NoError(t, err)
		buf.Write(aaguidBytes)
		// credID is a fixed 32-byte value (well under the 1023-byte WebAuthn cap);
		// the require keeps the uint16 conversion provably in-range for gosec.
		require.LessOrEqual(t, len(va.credID), 1023)
		var credLen [2]byte
		binary.BigEndian.PutUint16(credLen[:], uint16(len(va.credID))) //nolint:gosec // bounded by the require above (≤1023, fits uint16)
		buf.Write(credLen[:])
		buf.Write(va.credID)
		buf.Write(va.coseKey(t))
	}
	return buf.Bytes()
}

// clientDataJSON builds the collected client data for a ceremony.
func clientDataJSON(t *testing.T, ceremony, challengeB64URL, origin string) []byte {
	t.Helper()
	cd := map[string]any{
		"type":        ceremony,
		"challenge":   challengeB64URL,
		"origin":      origin,
		"crossOrigin": false,
	}
	b, err := json.Marshal(cd)
	require.NoError(t, err)
	return b
}

// attestationResponse assembles a registration credential response body (the
// JSON the client POSTs back), using the "none" attestation format.
func (va *virtualAuthenticator) attestationResponse(t *testing.T, rpID, challengeB64URL, origin string, flags byte) []byte {
	t.Helper()
	authData := va.authData(t, rpID, flags, true)
	attObj := map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	}
	em, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	attCBOR, err := em.Marshal(attObj)
	require.NoError(t, err)

	cdj := clientDataJSON(t, "webauthn.create", challengeB64URL, origin)
	idB64 := base64.RawURLEncoding.EncodeToString(va.credID)
	resp := map[string]any{
		"id":    idB64,
		"rawId": idB64,
		"type":  "public-key",
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attCBOR),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(cdj),
		},
	}
	body, err := json.Marshal(resp)
	require.NoError(t, err)
	return body
}

// assertionResponse assembles an authentication credential response body. If
// tamperSig is true, the signature is corrupted (to drive the bad-signature
// rejection path).
func (va *virtualAuthenticator) assertionResponse(t *testing.T, rpID, challengeB64URL, origin string, flags byte, tamperSig bool) []byte {
	t.Helper()
	authData := va.authData(t, rpID, flags, false)
	cdj := clientDataJSON(t, "webauthn.get", challengeB64URL, origin)
	cdjHash := sha256.Sum256(cdj)

	signed := append(append([]byte{}, authData...), cdjHash[:]...)
	digest := sha256.Sum256(signed)
	r, s, err := ecdsa.Sign(rand.Reader, va.priv, digest[:])
	require.NoError(t, err)
	sig, err := asn1.Marshal(ecdsaSig{R: r, S: s})
	require.NoError(t, err)
	if tamperSig {
		sig[len(sig)-1] ^= 0xff
	}

	idB64 := base64.RawURLEncoding.EncodeToString(va.credID)
	resp := map[string]any{
		"id":    idB64,
		"rawId": idB64,
		"type":  "public-key",
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(cdj),
			"signature":         base64.RawURLEncoding.EncodeToString(sig),
			"userHandle":        base64.RawURLEncoding.EncodeToString([]byte("admin-user-handle")),
		},
	}
	body, err := json.Marshal(resp)
	require.NoError(t, err)
	return body
}

type ecdsaSig struct {
	R, S *big.Int
}

// -----------------------------------------------------------------------------
// Test config + helpers
// -----------------------------------------------------------------------------

const (
	testAdminRPID    = "admin.example.org"
	testAdminOrigin  = "https://admin.example.org"
	testAllowedAAGID = "ee882879-721c-4913-9775-3dfcce97072a"
)

func testAdminConfig() *config.Config {
	return &config.Config{
		AdminWebAuthnRPID:           testAdminRPID,
		AdminWebAuthnRPOrigins:      []string{testAdminOrigin},
		AdminWebAuthnAllowedAAGUIDs: []string{testAllowedAAGID},
	}
}

// adminWebAuthnUser returns the WebAuthnUser the admin RP registers/authenticates.
func adminWebAuthnUser(creds ...webauthn.Credential) *mfa.WebAuthnUser {
	return &mfa.WebAuthnUser{
		ID:          []byte("admin-user-handle"),
		Name:        "operator",
		DisplayName: "Operator",
		Credentials: creds,
	}
}

// beginAdminRegistration drives BeginRegistration with the admin policy (UV
// required, resident key required, cross-platform, direct attestation).
func beginAdminRegistration(t *testing.T, svc *mfa.WebAuthnService, user *mfa.WebAuthnUser) (*protocol.CredentialCreation, *webauthn.SessionData) {
	t.Helper()
	tv := true
	creation, session, err := svc.BeginRegistration(user,
		webauthn.WithConveyancePreference(protocol.PreferDirectAttestation),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.CrossPlatform,
			RequireResidentKey:      &tv,
			ResidentKey:             protocol.ResidentKeyRequirementRequired,
			UserVerification:        protocol.VerificationRequired,
		}),
	)
	require.NoError(t, err)
	require.NotNil(t, creation)
	require.NotNil(t, session)
	return creation, session
}

// httpReq wraps a JSON body in a minimal *http.Request for Finish* calls.
func httpReq(body []byte) *http.Request {
	return &http.Request{Body: io.NopCloser(bytes.NewReader(body))}
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

func TestNewAdminWebAuthn_UsesAdminRP(t *testing.T) {
	cfg := testAdminConfig()
	svc, err := admin.NewAdminWebAuthn(cfg)
	require.NoError(t, err)
	require.NotNil(t, svc)

	creation, _ := beginAdminRegistration(t, svc, adminWebAuthnUser())
	assert.Equal(t, testAdminRPID, creation.Response.RelyingParty.ID)
	assert.Equal(t, "Concord Admin", creation.Response.RelyingParty.Name)
}

func TestAdminWebAuthn_RegisterThenLogin_HappyPath(t *testing.T) {
	svc, err := admin.NewAdminWebAuthn(testAdminConfig())
	require.NoError(t, err)

	aaguid := uuid.MustParse(testAllowedAAGID)
	va := newVirtualAuthenticator(t, aaguid)

	// --- Registration ---
	user := adminWebAuthnUser()
	creation, session := beginAdminRegistration(t, svc, user)
	regBody := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV|flagBE|flagBS)

	cred, err := svc.FinishRegistration(user, *session, httpReq(regBody))
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, va.credID, cred.ID)

	// AAGUID round-trips and is allow-listed.
	gotAAGUID, err := uuid.FromBytes(cred.Authenticator.AAGUID)
	require.NoError(t, err)
	assert.Equal(t, aaguid, gotAAGUID)

	// --- Login (assert) ---
	loginUser := adminWebAuthnUser(*cred)
	assertion, loginSession, err := svc.BeginLogin(loginUser, webauthn.WithUserVerification(protocol.VerificationRequired))
	require.NoError(t, err)
	require.NotNil(t, assertion)

	va.signCount++ // a real authenticator increments per assertion
	loginBody := va.assertionResponse(t, testAdminRPID, assertion.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV|flagBE|flagBS, false)

	matched, err := svc.FinishLogin(loginUser, *loginSession, httpReq(loginBody))
	require.NoError(t, err)
	require.NotNil(t, matched)
	assert.Equal(t, va.credID, matched.ID)
}

func TestAdminWebAuthn_Register_RejectsWrongRPOrigin(t *testing.T) {
	svc, err := admin.NewAdminWebAuthn(testAdminConfig())
	require.NoError(t, err)

	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	user := adminWebAuthnUser()
	creation, session := beginAdminRegistration(t, svc, user)

	// Origin the authenticator signs over does NOT match the admin RP origin.
	regBody := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), "https://evil.example.com", flagUP|flagUV)

	cred, err := svc.FinishRegistration(user, *session, httpReq(regBody))
	require.Error(t, err)
	assert.Nil(t, cred)
}

func TestAdminWebAuthn_Login_RejectsStaleChallenge(t *testing.T) {
	svc, err := admin.NewAdminWebAuthn(testAdminConfig())
	require.NoError(t, err)

	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	user := adminWebAuthnUser()
	creation, session := beginAdminRegistration(t, svc, user)
	regBody := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)
	cred, err := svc.FinishRegistration(user, *session, httpReq(regBody))
	require.NoError(t, err)

	loginUser := adminWebAuthnUser(*cred)
	_, loginSession, err := svc.BeginLogin(loginUser, webauthn.WithUserVerification(protocol.VerificationRequired))
	require.NoError(t, err)

	// Sign over a DIFFERENT (stale/replayed) challenge than the session expects.
	staleChallenge := base64.RawURLEncoding.EncodeToString([]byte("a-totally-different-challenge!!"))
	loginBody := va.assertionResponse(t, testAdminRPID, staleChallenge, testAdminOrigin, flagUP|flagUV, false)

	matched, err := svc.FinishLogin(loginUser, *loginSession, httpReq(loginBody))
	require.Error(t, err)
	assert.Nil(t, matched)
}

func TestAdminWebAuthn_Login_RejectsBadSignature(t *testing.T) {
	svc, err := admin.NewAdminWebAuthn(testAdminConfig())
	require.NoError(t, err)

	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	user := adminWebAuthnUser()
	creation, session := beginAdminRegistration(t, svc, user)
	regBody := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)
	cred, err := svc.FinishRegistration(user, *session, httpReq(regBody))
	require.NoError(t, err)

	loginUser := adminWebAuthnUser(*cred)
	assertion, loginSession, err := svc.BeginLogin(loginUser, webauthn.WithUserVerification(protocol.VerificationRequired))
	require.NoError(t, err)

	// Corrupt the ECDSA signature.
	loginBody := va.assertionResponse(t, testAdminRPID, assertion.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, true)

	matched, err := svc.FinishLogin(loginUser, *loginSession, httpReq(loginBody))
	require.Error(t, err)
	assert.Nil(t, matched)
}

func TestAdminWebAuthn_Login_RejectsUserVerificationNotSatisfied(t *testing.T) {
	svc, err := admin.NewAdminWebAuthn(testAdminConfig())
	require.NoError(t, err)

	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	user := adminWebAuthnUser()
	creation, session := beginAdminRegistration(t, svc, user)
	regBody := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)
	cred, err := svc.FinishRegistration(user, *session, httpReq(regBody))
	require.NoError(t, err)

	loginUser := adminWebAuthnUser(*cred)
	assertion, loginSession, err := svc.BeginLogin(loginUser, webauthn.WithUserVerification(protocol.VerificationRequired))
	require.NoError(t, err)

	// UV flag NOT set (user present, but not verified) while the session requires UV.
	va.signCount++
	loginBody := va.assertionResponse(t, testAdminRPID, assertion.Response.Challenge.String(), testAdminOrigin, flagUP, false)

	matched, err := svc.FinishLogin(loginUser, *loginSession, httpReq(loginBody))
	require.Error(t, err)
	assert.Nil(t, matched)
}

func TestCheckAAGUID(t *testing.T) {
	allowed := []string{testAllowedAAGID, "00000000-0000-0000-0000-00000000000a"}

	t.Run("accepts an allow-listed AAGUID", func(t *testing.T) {
		b, err := uuid.MustParse(testAllowedAAGID).MarshalBinary()
		require.NoError(t, err)
		assert.NoError(t, admin.CheckAAGUIDForTest(b, allowed))
	})

	t.Run("accepts regardless of allow-list entry casing", func(t *testing.T) {
		b, err := uuid.MustParse("00000000-0000-0000-0000-00000000000A").MarshalBinary()
		require.NoError(t, err)
		assert.NoError(t, admin.CheckAAGUIDForTest(b, []string{"00000000-0000-0000-0000-00000000000A"}))
	})

	t.Run("rejects a non-allow-listed AAGUID", func(t *testing.T) {
		b, err := uuid.MustParse("99999999-9999-9999-9999-999999999999").MarshalBinary()
		require.NoError(t, err)
		err = admin.CheckAAGUIDForTest(b, allowed)
		require.Error(t, err)
		assert.ErrorIs(t, err, admin.ErrAAGUIDNotAllowed)
	})

	t.Run("rejects everything against an empty allow-list (fail-closed)", func(t *testing.T) {
		b, err := uuid.MustParse(testAllowedAAGID).MarshalBinary()
		require.NoError(t, err)
		err = admin.CheckAAGUIDForTest(b, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, admin.ErrAAGUIDNotAllowed)
	})

	t.Run("rejects a malformed AAGUID", func(t *testing.T) {
		err := admin.CheckAAGUIDForTest([]byte{0x01, 0x02, 0x03}, allowed)
		require.Error(t, err)
		assert.ErrorIs(t, err, admin.ErrInvalidAAGUID)
	})
}

// FinishAdminRegistration persists an allow-listed credential and rejects (never
// persisting) a non-allow-listed one. These are integration tests (real PG).
func TestFinishAdminRegistration_PersistsAllowListedCredential(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	svc, err := admin.NewAdminWebAuthn(testAdminConfig())
	require.NoError(t, err)
	repo := admin.NewAdminRepo(db)

	adminUser, err := repo.CreatePending(ctx, uniqueAdminUsername("wa-ok"), "h")
	require.NoError(t, err)
	registerAdminCleanup(t, db, adminUser.ID)

	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	user := adminWebAuthnUser()
	creation, session := beginAdminRegistration(t, svc, user)
	regBody := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)

	stored, err := admin.FinishAdminRegistration(ctx, svc, repo, admin.AdminRegistrationInput{ // #nosec G101 -- test fixture: WebAuthn credential label, not a secret
		User: user, Session: *session, Request: httpReq(regBody),
		AdminID: adminUser.ID, AllowedAAGUIDs: []string{testAllowedAAGID}, CredentialName: "yubikey-primary",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, stored.ID)
	assert.Equal(t, va.credID, stored.CredentialID)

	list, err := repo.ListCredentials(ctx, adminUser.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, va.credID, list[0].CredentialID)
}

func TestFinishAdminRegistration_RejectsNonAllowListedBeforePersist(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	svc, err := admin.NewAdminWebAuthn(testAdminConfig())
	require.NoError(t, err)
	repo := admin.NewAdminRepo(db)

	adminUser, err := repo.CreatePending(ctx, uniqueAdminUsername("wa-deny"), "h")
	require.NoError(t, err)
	registerAdminCleanup(t, db, adminUser.ID)

	// Authenticator reports an AAGUID NOT on the allow-list.
	va := newVirtualAuthenticator(t, uuid.MustParse("99999999-9999-9999-9999-999999999999"))
	user := adminWebAuthnUser()
	creation, session := beginAdminRegistration(t, svc, user)
	regBody := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)

	stored, err := admin.FinishAdminRegistration(ctx, svc, repo, admin.AdminRegistrationInput{
		User: user, Session: *session, Request: httpReq(regBody),
		AdminID: adminUser.ID, AllowedAAGUIDs: []string{testAllowedAAGID}, CredentialName: "rogue-key",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, admin.ErrAAGUIDNotAllowed)
	assert.Empty(t, stored.ID)

	// Nothing was persisted.
	list, err := repo.ListCredentials(ctx, adminUser.ID)
	require.NoError(t, err)
	assert.Empty(t, list)
}
