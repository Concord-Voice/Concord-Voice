package mfa

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: crypto/cipher's GCM Open PANICS on a nonce whose length is not
// gcm.NonceSize() rather than returning an error. The nonce is read from a
// stored TOTP row, so corrupt or hand-written data must fail DecryptSecret
// with an error, never a panic.
func TestDecryptSecret_WrongNonceLength_ReturnsError(t *testing.T) {
	key := make([]byte, 32)
	plaintext := []byte("my-totp-secret-12345")

	ct, goodNonce, err := EncryptSecret(plaintext, key)
	require.NoError(t, err)

	// Positive control: the fixture is valid and reaches gcm.Open, so the
	// cases below differ from a working decrypt in nonce length alone.
	decrypted, err := DecryptSecret(ct, goodNonce, key)
	require.NoError(t, err)
	require.Equal(t, plaintext, decrypted)

	tests := []struct {
		name  string
		nonce []byte
	}{
		{"nil nonce", nil},
		{"9-byte nonce", goodNonce[:9]},
		{"11-byte nonce, one short", goodNonce[:11]},
		{"13-byte nonce, one long", append(append([]byte{}, goodNonce...), 0x00)},
		{"16-byte nonce", append(append([]byte{}, goodNonce...), 0x00, 0x00, 0x00, 0x00)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []byte
			var decErr error
			require.NotPanics(t, func() {
				got, decErr = DecryptSecret(ct, tt.nonce, key)
			}, "DecryptSecret panicked on a %d-byte nonce; want an error", len(tt.nonce))

			require.ErrorContains(t, decErr, "invalid nonce length", "DecryptSecret accepted a %d-byte nonce", len(tt.nonce))
			assert.Nil(t, got, "DecryptSecret returned plaintext alongside an error")
		})
	}
}
