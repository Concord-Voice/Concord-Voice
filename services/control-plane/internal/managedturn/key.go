package managedturn

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// keyIDDomain separates key IDs from any other SHA-256 use of the same bytes.
const keyIDDomain = "concord-voice/managed-turn/key-id/v1\x00"

// redactedKey is the only rendering a Key ever has.
const redactedKey = "managedturn.Key{redacted}"

var errKeyNotSerialisable = errors.New("managedturn: key is not serialisable")

// Key is one validated TURN shared key held in memory only. Its value is
// unexported and every formatting path renders it redacted.
type Key struct{ value string }

// ParseKey validates 43–127 printable, non-space ASCII bytes. The returned key
// copies raw, so the caller may clear its buffer.
func ParseKey(raw []byte) (Key, error) {
	if len(raw) < MinKeyLen || len(raw) > MaxKeyLen {
		return Key{}, ErrInvalidKey
	}
	for _, b := range raw {
		if b < 0x21 || b > 0x7e {
			return Key{}, ErrInvalidKey
		}
	}
	return Key{value: string(raw)}, nil
}

// valid reports whether k came from ParseKey; the zero Key is never valid.
func (k Key) valid() bool { return k.value != "" }

// ID returns the non-secret identifier: hex SHA-256 over a fixed domain
// separator and the key bytes. The zero Key has no ID.
func (k Key) ID() string {
	if !k.valid() {
		return ""
	}
	h := sha256.New() // hash.Hash.Write never returns an error
	_, _ = io.WriteString(h, keyIDDomain)
	_, _ = io.WriteString(h, k.value)
	return hex.EncodeToString(h.Sum(nil))
}

// String implements fmt.Stringer without revealing the key.
func (Key) String() string { return redactedKey }

// GoString implements fmt.GoStringer without revealing the key.
func (Key) GoString() string { return redactedKey }

// Format implements fmt.Formatter so that no verb (%x, %q, %+v, ...) can
// reach the unexported value through reflection. Formatter has no error
// return; fmt itself ignores the writer's error the same way.
func (Key) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redactedKey) }

// MarshalJSON refuses: a key never belongs in a serialised record.
func (Key) MarshalJSON() ([]byte, error) { return nil, errKeyNotSerialisable }

// MarshalText refuses for the same reason (it also covers map keys).
func (Key) MarshalText() ([]byte, error) { return nil, errKeyNotSerialisable }

// ReadKeys reads at most n keys, one per line, from r and requires EOF. The
// total read is bounded to n*(MaxKeyLen+1) bytes. A single trailing newline is
// accepted; blank lines, CR and every other non-printable byte are refused.
// n must be between 1 and MaxKeys.
func ReadKeys(r io.Reader, n int) ([]Key, error) {
	if n < 1 || n > MaxKeys {
		return nil, fmt.Errorf("%w: key count %d outside 1..%d", ErrInvalidConfig, n, MaxKeys)
	}
	bound := n * (MaxKeyLen + 1)
	// One byte past the bound distinguishes "exactly at the bound, then EOF"
	// from "more input follows"; nothing beyond it is ever read.
	data, err := io.ReadAll(io.LimitReader(r, int64(bound)+1))
	defer clear(data)
	if err != nil {
		return nil, fmt.Errorf("managedturn: read keys: %w", err)
	}
	if len(data) > bound {
		return nil, ErrInputTooLarge
	}
	if len(data) == 0 {
		return nil, ErrInvalidKey
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	if len(lines) > n {
		return nil, ErrInputTooLarge
	}
	keys := make([]Key, 0, len(lines))
	for i, line := range lines {
		k, perr := ParseKey(line)
		if perr != nil {
			return nil, fmt.Errorf("%w (line %d)", perr, i+1)
		}
		keys = append(keys, k)
	}
	return keys, nil
}
