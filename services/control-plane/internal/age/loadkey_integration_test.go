package age

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadCurrentKey_CorruptSPKI_InvalidSignature: a stored public_key that is
// not valid DER cannot verify anything, so LoadCurrentKey maps the x509 parse
// failure to the single ErrInvalidSignature sentinel (no parse detail leaks).
func TestLoadCurrentKey_CorruptSPKI_InvalidSignature(t *testing.T) {
	env, cleanup := setupAgeIT(t, false)
	defer cleanup()

	_, err := env.db.Exec(
		`INSERT INTO public_keys (user_id, public_key, key_version) VALUES ($1, $2, 1)`,
		env.userID, []byte{0x30, 0x01, 0x02}) // malformed DER
	require.NoError(t, err)

	_, err = LoadCurrentKey(t.Context(), env.db, env.userID, 1)
	assert.ErrorIs(t, err, ErrInvalidSignature)
}

// TestLoadCurrentKey_NonRSAKey_InvalidSignature: a structurally-valid SPKI that
// decodes to a non-RSA key (ECDSA P-256 here) is rejected — the age-claim scheme
// is RSA-PSS only, so a non-*rsa.PublicKey verifies nothing.
func TestLoadCurrentKey_NonRSAKey_InvalidSignature(t *testing.T) {
	env, cleanup := setupAgeIT(t, false)
	defer cleanup()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	spki, err := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	require.NoError(t, err)

	_, err = env.db.Exec(
		`INSERT INTO public_keys (user_id, public_key, key_version) VALUES ($1, $2, 1)`,
		env.userID, spki)
	require.NoError(t, err)

	_, err = LoadCurrentKey(t.Context(), env.db, env.userID, 1)
	assert.ErrorIs(t, err, ErrInvalidSignature)
}

// TestAgeIT_InvalidObligation_400: a well-formed JSON body that BINDS but fails
// Claim.Validate() (jurisdiction_obligation outside the 0..2 range) is rejected
// at the validation step with 400 malformed — before any signature work. The
// body is hand-built with a dummy signature because validation precedes verify.
func TestAgeIT_InvalidObligation_400(t *testing.T) {
	env, cleanup := setupAgeIT(t, true)
	defer cleanup()

	c := env.freshClaim() // supplies a 64-hex nonce + current timestamp
	body := fmt.Sprintf(
		`{"canonical_version":1,"valid_age":true,"nsfw_auth":false,`+
			`"jurisdiction_obligation":5,"nonce":%q,"timestamp":%d,`+
			`"key_version":1,"client_version":"0.2.0","signature":"AA=="}`,
		c.Nonce, c.Timestamp)

	w := env.submit(t, body)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, "malformed", env.errorCode(t, w))
}
