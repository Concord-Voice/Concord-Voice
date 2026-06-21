package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

var (
	// ErrInvalidHash indicates the password hash format is invalid.
	ErrInvalidHash = errors.New("invalid hash format")
	// ErrIncompatibleVersion indicates the Argon2 version is incompatible.
	ErrIncompatibleVersion = errors.New("incompatible argon2 version")
)

// Argon2Params contains Argon2id parameters that exceed OWASP 2023 recommendations.
type Argon2Params struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultParams returns enhanced parameters for Argon2id
// These settings exceed OWASP recommendations, optimized for modern multi-core systems
func DefaultParams() *Argon2Params {
	return &Argon2Params{
		Memory:      64 * 1024, // 64 MB
		Iterations:  3,
		Parallelism: 4, // Increased from 2 for better resistance on modern CPUs
		SaltLength:  16,
		KeyLength:   32,
	}
}

// HashPassword generates an Argon2id hash of the password
func HashPassword(password string) (string, error) {
	return HashPasswordWithParams(password, DefaultParams())
}

// HashPasswordWithParams generates an Argon2id hash with custom parameters
func HashPasswordWithParams(password string, p *Argon2Params) (string, error) {
	// Generate random salt
	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	// Generate hash
	hash := argon2.IDKey(
		[]byte(password),
		salt,
		p.Iterations,
		p.Memory,
		p.Parallelism,
		p.KeyLength,
	)

	// Encode to string: $argon2id$v=19$m=65536,t=3,p=4$salt$hash
	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)

	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		p.Memory,
		p.Iterations,
		p.Parallelism,
		b64Salt,
		b64Hash,
	)

	return encoded, nil
}

// VerifyPassword checks if the password matches the hash
func VerifyPassword(password, encodedHash string) (bool, error) {
	// Extract parameters and salt from encoded hash
	p, salt, hash, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}

	// Generate hash from password
	otherHash := argon2.IDKey(
		[]byte(password),
		salt,
		p.Iterations,
		p.Memory,
		p.Parallelism,
		p.KeyLength,
	)

	// Constant-time comparison to prevent timing attacks
	if subtle.ConstantTimeCompare(hash, otherHash) == 1 {
		return true, nil
	}

	return false, nil
}

// decodeHash parses the encoded hash string
func decodeHash(encodedHash string) (*Argon2Params, []byte, []byte, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 {
		return nil, nil, nil, ErrInvalidHash
	}

	// Check algorithm
	if parts[1] != "argon2id" {
		return nil, nil, nil, ErrInvalidHash
	}

	// Check version
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, nil, err
	}
	if version != argon2.Version {
		return nil, nil, nil, ErrIncompatibleVersion
	}

	// Parse parameters
	p := &Argon2Params{}
	if _, err := fmt.Sscanf(
		parts[3],
		"m=%d,t=%d,p=%d",
		&p.Memory,
		&p.Iterations,
		&p.Parallelism,
	); err != nil {
		return nil, nil, nil, err
	}

	// Decode salt
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, nil, err
	}
	saltLen := len(salt)
	if saltLen < 0 || saltLen > 0xFFFFFFFF {
		return nil, nil, nil, ErrInvalidHash
	}
	p.SaltLength = uint32(saltLen)

	// Decode hash
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, nil, err
	}
	hashLen := len(hash)
	if hashLen < 0 || hashLen > 0xFFFFFFFF {
		return nil, nil, nil, ErrInvalidHash
	}
	p.KeyLength = uint32(hashLen)

	return p, salt, hash, nil
}

func hasUpperCase(password string) bool {
	for _, ch := range password {
		if ch >= 'A' && ch <= 'Z' {
			return true
		}
	}
	return false
}

func hasLowerCase(password string) bool {
	for _, ch := range password {
		if ch >= 'a' && ch <= 'z' {
			return true
		}
	}
	return false
}

func hasDigit(password string) bool {
	for _, ch := range password {
		if ch >= '0' && ch <= '9' {
			return true
		}
	}
	return false
}

func hasSpecialChar(password string) bool {
	for _, ch := range password {
		if ch >= '!' && ch <= '/' || ch >= ':' && ch <= '@' || ch >= '[' && ch <= '`' || ch >= '{' && ch <= '~' {
			return true
		}
	}
	return false
}

func countCharTypes(password string) int {
	types := 0
	if hasUpperCase(password) {
		types++
	}
	if hasLowerCase(password) {
		types++
	}
	if hasDigit(password) {
		types++
	}
	if hasSpecialChar(password) {
		types++
	}
	return types
}

// ValidatePasswordStrength checks if password meets minimum requirements
func ValidatePasswordStrength(password string) error {
	if len(password) < 12 {
		return errors.New("password must be at least 12 characters long")
	}
	if len(password) > 128 {
		return errors.New("password must be no more than 128 characters")
	}
	if countCharTypes(password) < 3 {
		return errors.New("password must contain at least 3 of the following: uppercase letters, lowercase letters, numbers, special characters")
	}
	return nil
}
