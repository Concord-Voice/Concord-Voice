// Package mfa implements Multi-Factor Authentication (TOTP + WebAuthn) for Concord.
package mfa

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"strings"

	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

const (
	totpIssuer  = "ConcordVoice"
	totpDigits  = otp.DigitsSix
	totpPeriod  = 30
	totpAlgo    = otp.AlgorithmSHA1
	totpSkew    = 1 // Allow previous and next period for clock drift
	backupCount = 8
	backupLen   = 8
)

// backupAlphabet is [A-Z0-9] for human-readable backup codes.
const backupAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GenerateSecret creates a new TOTP secret for the given email.
// Returns the otp.Key which contains the secret and otpauth:// URL.
func GenerateSecret(email string) (*otp.Key, error) {
	return totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: email,
		Period:      totpPeriod,
		Digits:      totpDigits,
		Algorithm:   totpAlgo,
	})
}

// ValidateCode checks a TOTP code against a secret with a 1-step skew window.
func ValidateCode(secret, code string) bool {
	valid, _ := totp.ValidateCustom(code, secret, time.Now(), totp.ValidateOpts{
		Period:    totpPeriod,
		Digits:    totpDigits,
		Algorithm: totpAlgo,
		Skew:      totpSkew,
	})
	return valid
}

// EncryptSecret encrypts a TOTP secret using AES-256-GCM.
// Returns ciphertext and nonce. The encKey must be exactly 32 bytes.
func EncryptSecret(plaintext, encKey []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create GCM: %w", err)
	}

	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// DecryptSecret decrypts an AES-256-GCM encrypted TOTP secret.
func DecryptSecret(ciphertext, nonce, encKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}

// ParseEncryptionKey decodes a hex-encoded 32-byte AES key.
func ParseEncryptionKey(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode hex key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

// GenerateBackupCodes creates 8 random backup codes.
// Returns the plaintext codes (to show the user once) and their SHA-256 hashes (to store).
func GenerateBackupCodes() (codes []string, hashes []string, err error) {
	codes = make([]string, backupCount)
	hashes = make([]string, backupCount)
	alphabetLen := big.NewInt(int64(len(backupAlphabet)))

	for i := range backupCount {
		var sb strings.Builder
		for range backupLen {
			idx, randErr := rand.Int(rand.Reader, alphabetLen)
			if randErr != nil {
				return nil, nil, fmt.Errorf("generate random: %w", randErr)
			}
			sb.WriteByte(backupAlphabet[idx.Int64()])
		}
		codes[i] = sb.String()
		hash := sha256.Sum256([]byte(codes[i]))
		hashes[i] = hex.EncodeToString(hash[:])
	}

	return codes, hashes, nil
}

// VerifyBackupCode checks a code against stored hashes using timing-safe comparison.
// Returns the index of the matched code and whether it matched.
// The caller must check that the code hasn't already been used (via the parallel used array).
func VerifyBackupCode(code string, hashes []string, used []bool) (index int, matched bool) {
	codeHash := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	codeHex := hex.EncodeToString(codeHash[:])

	for i, storedHash := range hashes {
		if i < len(used) && used[i] {
			continue // skip already-used codes
		}
		if subtle.ConstantTimeCompare([]byte(codeHex), []byte(storedHash)) == 1 {
			return i, true
		}
	}
	return -1, false
}
