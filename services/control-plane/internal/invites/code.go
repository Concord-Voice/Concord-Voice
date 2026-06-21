// Package invites provides handlers for managing server invite codes.
package invites

import (
	"crypto/rand"
	"math/big"
)

// charset excludes ambiguous characters (I, l, O, 0, 1) for readability
const charset = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"

// GenerateCode creates a cryptographically random 8-character invite code.
func GenerateCode() (string, error) {
	code := make([]byte, 8)
	for i := range code {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		code[i] = charset[n.Int64()]
	}
	return string(code), nil
}
