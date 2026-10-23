package managedturn_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

// countingReader yields an endless stream and counts how much was consumed.
type countingReader struct {
	chunk []byte
	read  int
}

// hardCap ends the stream with an error so a reader that ignores the bound
// fails the assertion below instead of hanging or exhausting memory.
const hardCap = 8 << 20

func (r *countingReader) Read(p []byte) (int, error) {
	if r.read > hardCap {
		return 0, errors.New("test reader hard cap exceeded: implementation did not bound its read")
	}
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.chunk)
		n += c
	}
	r.read += n
	return n, nil
}

// bytesBound is the documented total read bound for n keys.
func bytesBound(n int) int { return n * (managedturn.MaxKeyLen + 1) }

func TestReadKeys_ExactlyNKeysThenEOF(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{{"one key", 1}, {"two keys", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			raws := make([]string, tc.n)
			for i := range raws {
				raws[i] = newKeyString(t)
			}
			input := strings.Join(raws, "\n") + "\n"

			keys, err := managedturn.ReadKeys(strings.NewReader(input), tc.n)
			require.NoError(t, err)
			require.Len(t, keys, tc.n)
			for i, raw := range raws {
				want, perr := managedturn.ParseKey([]byte(raw))
				require.NoError(t, perr)
				assert.Equal(t, want.ID(), keys[i].ID(), "keys are returned in input order")
			}
		})
	}
}

func TestReadKeys_TrailingNewline(t *testing.T) {
	raw := newKeyString(t)
	want, err := managedturn.ParseKey([]byte(raw))
	require.NoError(t, err)

	t.Run("single trailing newline accepted", func(t *testing.T) {
		keys, err := managedturn.ReadKeys(strings.NewReader(raw+"\n"), 1)
		require.NoError(t, err)
		require.Len(t, keys, 1)
		assert.Equal(t, want.ID(), keys[0].ID())
	})
	t.Run("no trailing newline accepted", func(t *testing.T) {
		keys, err := managedturn.ReadKeys(strings.NewReader(raw), 1)
		require.NoError(t, err)
		require.Len(t, keys, 1)
		assert.Equal(t, want.ID(), keys[0].ID(), "newline is a separator, never part of the key")
	})
	t.Run("blank line after the key rejected", func(t *testing.T) {
		_, err := managedturn.ReadKeys(strings.NewReader(raw+"\n\n"), 1)
		requireRejected(t, err, managedturn.ErrInvalidKey, managedturn.ErrInputTooLarge)
	})
	t.Run("CRLF rejected because CR is not printable", func(t *testing.T) {
		_, err := managedturn.ReadKeys(strings.NewReader(raw+"\r\n"), 1)
		requireRejected(t, err, managedturn.ErrInvalidKey)
	})
}

func TestReadKeys_FewerThanNIsAllowed(t *testing.T) {
	raw := newKeyString(t)
	keys, err := managedturn.ReadKeys(strings.NewReader(raw+"\n"), 2)
	require.NoError(t, err, "n is a maximum")
	assert.Len(t, keys, 1)
}

func TestReadKeys_RejectsNPlusOneLines(t *testing.T) {
	a, b, c := newKeyString(t), newKeyString(t), newKeyString(t)

	t.Run("two lines for n=1", func(t *testing.T) {
		keys, err := managedturn.ReadKeys(strings.NewReader(a+"\n"+b+"\n"), 1)
		requireRejected(t, err, managedturn.ErrInputTooLarge, managedturn.ErrInvalidKey)
		assert.Empty(t, keys, "no partial result on refusal")
	})
	t.Run("three lines for n=2", func(t *testing.T) {
		keys, err := managedturn.ReadKeys(strings.NewReader(a+"\n"+b+"\n"+c+"\n"), 2)
		requireRejected(t, err, managedturn.ErrInputTooLarge, managedturn.ErrInvalidKey)
		assert.Empty(t, keys)
	})
	t.Run("third line without trailing newline", func(t *testing.T) {
		_, err := managedturn.ReadKeys(strings.NewReader(a+"\n"+b+"\n"+c), 2)
		requireRejected(t, err, managedturn.ErrInputTooLarge, managedturn.ErrInvalidKey)
	})
}

func TestReadKeys_RejectsEmptyInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{{"no bytes", ""}, {"only a newline", "\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := managedturn.ReadKeys(strings.NewReader(tc.input), 1)
			requireRejected(t, err, managedturn.ErrInvalidKey)
			assert.Empty(t, keys)
		})
	}
}

func TestReadKeys_BoundedReadOfInfiniteInput(t *testing.T) {
	// slack covers one buffered chunk; an unbounded reader would blow far past it.
	const slack = 64 * 1024

	t.Run("endless bytes with no newline", func(t *testing.T) {
		r := &countingReader{chunk: []byte("A")}
		_, err := managedturn.ReadKeys(r, 2)
		requireRejected(t, err, managedturn.ErrInputTooLarge)
		assert.LessOrEqual(t, r.read, bytesBound(2)+slack, "must not read unbounded data")
	})
	t.Run("endless valid-looking lines", func(t *testing.T) {
		line := newKeyString(t) + "\n"
		r := &countingReader{chunk: []byte(line)}
		_, err := managedturn.ReadKeys(r, 2)
		requireRejected(t, err, managedturn.ErrInputTooLarge, managedturn.ErrInvalidKey)
		assert.LessOrEqual(t, r.read, bytesBound(2)+slack, "must not read unbounded data")
	})
	t.Run("one endless line for n=1", func(t *testing.T) {
		r := &countingReader{chunk: []byte("x")}
		_, err := managedturn.ReadKeys(r, 1)
		requireRejected(t, err, managedturn.ErrInputTooLarge)
		assert.LessOrEqual(t, r.read, bytesBound(1)+slack)
	})
}

func TestReadKeys_ByteBoundBoundaries(t *testing.T) {
	long := strings.Repeat("K", managedturn.MaxKeyLen) // exactly 127 bytes

	t.Run("two maximum-length keys at exactly the bound", func(t *testing.T) {
		input := long + "\n" + strings.Repeat("L", managedturn.MaxKeyLen) + "\n"
		require.Equal(t, bytesBound(2), len(input))
		keys, err := managedturn.ReadKeys(strings.NewReader(input), 2)
		require.NoError(t, err)
		assert.Len(t, keys, 2)
	})
	t.Run("a 128-byte line is not a key", func(t *testing.T) {
		_, err := managedturn.ReadKeys(strings.NewReader(long+"M\n"), 2)
		requireRejected(t, err, managedturn.ErrInvalidKey, managedturn.ErrInputTooLarge)
	})
	t.Run("a 42-byte line is not a key", func(t *testing.T) {
		_, err := managedturn.ReadKeys(strings.NewReader(strings.Repeat("S", 42)+"\n"), 1)
		requireRejected(t, err, managedturn.ErrInvalidKey)
	})
}

func TestReadKeys_InvalidLineErrorDoesNotEchoInput(t *testing.T) {
	secretish := newKeyString(t)[:35] + " " + "tail" // space makes it invalid
	_, err := managedturn.ReadKeys(strings.NewReader(secretish+"\n"), 1)
	requireRejected(t, err, managedturn.ErrInvalidKey)
	assert.NotContains(t, err.Error(), secretish)
	assert.NotContains(t, err.Error(), secretish[:20])
}

func TestReadKeys_PropagatesReaderError(t *testing.T) {
	boom := errors.New("simulated read failure")
	_, err := managedturn.ReadKeys(iotest.ErrReader(boom), 1)
	requireRejected(t, err)
	assert.ErrorIs(t, err, boom)
}

func TestReadKeys_PropagatesErrorAfterKey(t *testing.T) {
	boom := errors.New("simulated read failure")
	r := io.MultiReader(bytes.NewReader([]byte(newKeyString(t)+"\n")), iotest.ErrReader(boom))
	_, err := managedturn.ReadKeys(r, 1)
	requireRejected(t, err)
	assert.ErrorIs(t, err, boom, "a failed read must not be mistaken for EOF")
}
