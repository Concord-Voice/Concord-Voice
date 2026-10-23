package managedturn_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

func TestParseKey_Lengths(t *testing.T) {
	cases := []struct {
		name string
		n    int
		ok   bool
	}{
		{"42 bytes is one short", 42, false},
		{"43 bytes is the minimum", 43, true},
		{"44 bytes", 44, true},
		{"127 bytes is the maximum", 127, true},
		{"128 bytes is one too long", 128, false},
		{"empty", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Repeat("A", tc.n)
			k, err := managedturn.ParseKey([]byte(raw))
			if tc.ok {
				require.NoError(t, err)
				assert.Len(t, k.ID(), 64)
				return
			}
			requireRejected(t, err, managedturn.ErrInvalidKey)
		})
	}
}

func TestParseKey_RejectsNonPrintableAndSpace(t *testing.T) {
	good := newKeyString(t) // 43 printable chars
	base := good[:40]
	cases := []struct {
		name string
		raw  string
	}{
		{"embedded space", base + " ab"},
		{"leading space", " " + good},
		{"trailing space", good + " "},
		{"embedded newline", base + "\nab"},
		{"trailing newline", good + "\n"},
		{"carriage return", base + "\rab"},
		{"tab", base + "\tab"},
		{"NUL", base + "\x00ab"},
		{"control 0x01", base + "\x01ab"},
		{"DEL 0x7f", base + "\x7fab"},
		{"high byte 0x80", base + "\x80ab"},
		{"multi-byte UTF-8", base + "éab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.GreaterOrEqual(t, len(tc.raw), managedturn.MinKeyLen, "fixture must fail only on its character class")
			require.LessOrEqual(t, len(tc.raw), managedturn.MaxKeyLen, "fixture must fail only on its character class")
			_, err := managedturn.ParseKey([]byte(tc.raw))
			requireRejected(t, err, managedturn.ErrInvalidKey)
		})
	}
}

func TestParseKey_ErrorDoesNotEchoInput(t *testing.T) {
	raw := newKeyString(t)[:30] + " bad" // too short and contains a space
	_, err := managedturn.ParseKey([]byte(raw))
	requireRejected(t, err, managedturn.ErrInvalidKey)
	assert.NotContains(t, err.Error(), raw)
	assert.NotContains(t, err.Error(), raw[:20])
}

func TestParseKey_AcceptsFullPrintableRange(t *testing.T) {
	var sb strings.Builder
	for c := byte(0x21); c <= 0x7e; c++ {
		sb.WriteByte(c)
	}
	raw := sb.String() // 94 printable non-space ASCII characters
	k, err := managedturn.ParseKey([]byte(raw))
	require.NoError(t, err)
	assert.Len(t, k.ID(), 64)
}

func TestParseKey_CopiesInput(t *testing.T) {
	raw := []byte(newKeyString(t))
	k, err := managedturn.ParseKey(raw)
	require.NoError(t, err)
	before := k.ID()
	for i := range raw {
		raw[i] = 'Z'
	}
	assert.Equal(t, before, k.ID(), "mutating the caller's buffer must not change the key")
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestKeyID_Shape(t *testing.T) {
	k, raw := newKey(t)
	id := k.ID()

	assert.Regexp(t, hex64, id, "ID is 64 lowercase hex characters")
	assert.NotContains(t, id, raw)
	assert.NotContains(t, id, raw[:8], "ID must not leak a key prefix")
}

func TestKeyID_Deterministic(t *testing.T) {
	raw := newKeyString(t)
	a, err := managedturn.ParseKey([]byte(raw))
	require.NoError(t, err)
	b, err := managedturn.ParseKey([]byte(raw))
	require.NoError(t, err)

	assert.Equal(t, a.ID(), b.ID())
	assert.Equal(t, a.ID(), a.ID())
}

func TestKeyID_DiffersPerKey(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		k, _ := newKey(t)
		id := k.ID()
		require.Regexp(t, hex64, id)
		require.False(t, seen[id], "two distinct keys produced the same ID")
		seen[id] = true
	}
}

func TestKeyID_IsDomainSeparated(t *testing.T) {
	k, raw := newKey(t)
	plain := sha256.Sum256([]byte(raw))
	assert.NotEqual(t, hex.EncodeToString(plain[:]), k.ID(),
		"ID must be SHA-256 over a domain separator plus the key, not over the bare key")
	assert.Regexp(t, hex64, k.ID())
}

func TestKey_DoesNotFormatItsValue(t *testing.T) {
	k, raw := newKey(t)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		out := fmt.Sprintf(verb, k)
		assert.NotContains(t, out, raw, "verb %s leaked the key", verb)
	}
	ptr := fmt.Sprintf("%v %+v %#v", &k, &k, &k)
	assert.NotContains(t, ptr, raw, "formatting a *Key leaked the key")
}
