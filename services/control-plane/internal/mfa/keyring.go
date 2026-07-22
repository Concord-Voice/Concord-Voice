package mfa

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Keyring holds the versioned AES-256 keys for TOTP-seed encryption at rest
// (#2307). New seals always use the active version; opens select the key by the
// row's stamped key_version, so retired keys stay decrypt-only during a
// rotation window. Error strings never contain key material — only version
// numbers and entry indexes (observability rule #1).
type Keyring struct {
	activeVersion int
	keys          map[int][]byte
}

// ParseKeyring builds a Keyring from the three MFA_ENCRYPTION_KEY* env values.
// Fail-closed: any malformed input is an error (the caller fatal-exits), never
// a partially-usable ring.
func ParseKeyring(activeHex string, activeVersion int, retired string) (*Keyring, error) {
	if !validKeyVersion(activeVersion) {
		return nil, fmt.Errorf("MFA_ENCRYPTION_KEY_VERSION must be in PostgreSQL SMALLINT range 1..%d, got %d", math.MaxInt16, activeVersion)
	}
	active, err := ParseEncryptionKey(activeHex)
	if err != nil {
		return nil, fmt.Errorf("MFA_ENCRYPTION_KEY: %w", err)
	}
	keys := map[int][]byte{activeVersion: active}
	if retired != "" {
		for i, entry := range strings.Split(retired, ",") {
			if err := addRetiredKey(keys, i+1, entry); err != nil {
				return nil, err
			}
		}
	}

	return &Keyring{activeVersion: activeVersion, keys: keys}, nil
}

// addRetiredKey parses one "<version>:<hex64>" retired-key entry and inserts it
// into keys. n is the 1-based entry index (for error messages only). Fail-closed:
// a malformed entry, a version outside PostgreSQL SMALLINT range 1..32767, a
// version already in the ring, or a bad key is an error. Never emits key material.
func addRetiredKey(keys map[int][]byte, n int, entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return fmt.Errorf("MFA_ENCRYPTION_KEYS_RETIRED entry %d: empty", n)
	}
	verStr, hexKey, ok := strings.Cut(entry, ":")
	if !ok {
		return fmt.Errorf("MFA_ENCRYPTION_KEYS_RETIRED entry %d: want <version>:<hex64>", n)
	}
	ver, err := strconv.Atoi(verStr)
	if err != nil || !validKeyVersion(ver) {
		return fmt.Errorf("MFA_ENCRYPTION_KEYS_RETIRED entry %d: version must be an integer in PostgreSQL SMALLINT range 1..%d", n, math.MaxInt16)
	}
	if _, dup := keys[ver]; dup {
		return fmt.Errorf("MFA_ENCRYPTION_KEYS_RETIRED entry %d: version %d already present in keyring", n, ver)
	}
	key, err := ParseEncryptionKey(hexKey)
	if err != nil {
		return fmt.Errorf("MFA_ENCRYPTION_KEYS_RETIRED entry %d (version %d): %w", n, ver, err)
	}
	keys[ver] = key
	return nil
}

func validKeyVersion(version int) bool {
	return version >= 1 && version <= math.MaxInt16
}

// ActiveVersion returns the version new seals are stamped with.
func (k *Keyring) ActiveVersion() int { return k.activeVersion }

// Seal encrypts plaintext under the active key and returns the version stamp
// the caller must persist alongside the ciphertext.
func (k *Keyring) Seal(plaintext []byte) (ciphertext, nonce []byte, version int, err error) {
	ciphertext, nonce, err = EncryptSecret(plaintext, k.keys[k.activeVersion])
	if err != nil {
		return nil, nil, 0, fmt.Errorf("seal under key version %d: %w", k.activeVersion, err)
	}
	return ciphertext, nonce, k.activeVersion, nil
}

// Open decrypts a ciphertext sealed under the given stamped version. A version
// absent from the ring fails closed.
func (k *Keyring) Open(ciphertext, nonce []byte, version int) ([]byte, error) {
	key, ok := k.keys[version]
	if !ok {
		return nil, fmt.Errorf("no key for version %d (active version %d)", version, k.activeVersion)
	}
	plaintext, err := DecryptSecret(ciphertext, nonce, key)
	if err != nil {
		return nil, fmt.Errorf("open under key version %d: %w", version, err)
	}
	return plaintext, nil
}
