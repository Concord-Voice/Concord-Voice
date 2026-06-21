package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// verificationRecord is stored in Redis keyed by "email_verify:{pending_id}".
type verificationRecord struct {
	CodeHash string `json:"code_hash"`
	Email    string `json:"email"`
	Attempts int    `json:"attempts"`
}

// generateCode produces a cryptographically random 6-digit numeric code.
func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// hashCode returns the SHA-256 hex digest of a verification code.
func hashCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// redisKey returns the Redis key for a pending registration's verification code.
func redisKey(pendingID string) string {
	return fmt.Sprintf("email_verify:%s", pendingID)
}

func sanitizeVerificationCode(raw string) string {
	return strings.TrimSpace(strings.ReplaceAll(raw, "-", ""))
}

func isValidVerificationCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
