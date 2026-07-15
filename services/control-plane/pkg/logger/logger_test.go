package logger

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewProductionMode(t *testing.T) {
	log := New("production")
	require.NotNil(t, log)
	require.NotNil(t, log.Logger)
}

func TestNewDevelopmentMode(t *testing.T) {
	log := New("development")
	require.NotNil(t, log)
	require.NotNil(t, log.Logger)
}

func TestNewUnknownEnvironment(t *testing.T) {
	// Unknown envs fall through to the else (text/debug) branch
	log := New("staging")
	require.NotNil(t, log)
}

func TestNewEmptyEnvironment(t *testing.T) {
	log := New("")
	require.NotNil(t, log)
}

func TestWithReturnsNewLogger(t *testing.T) {
	base := New("development")
	child := base.With("key", "value")

	require.NotNil(t, child)
	assert.NotSame(t, base, child, "With() must return a distinct Logger instance")
}

func TestWithChainedCalls(t *testing.T) {
	base := New("development")
	child1 := base.With("k1", "v1")
	child2 := child1.With("k2", "v2")

	require.NotNil(t, child2)
	assert.NotSame(t, child1, child2)
}

func TestWithMultipleArgs(t *testing.T) {
	base := New("production")
	child := base.With("service", "control-plane", "version", "0.2.0")
	require.NotNil(t, child)
}

func TestNewWithWriterRoutesOutputToBuffer(t *testing.T) {
	var buf bytes.Buffer
	log := NewWithWriter(&buf)
	require.NotNil(t, log)
	require.NotNil(t, log.Logger)

	log.Info("test-message", "structured_key", "structured_value")

	out := buf.String()
	assert.Contains(t, out, "test-message", "message must appear in captured output")
	assert.Contains(t, out, "structured_key=structured_value",
		"structured key/value must appear in captured output")
}

func TestNewWithWriterIncludesDebugLevel(t *testing.T) {
	// The constructor sets slog.LevelDebug, so a Debug-level message must be
	// captured. Regression-lock for the level choice so future changes do not
	// silently drop debug lines in tests.
	var buf bytes.Buffer
	log := NewWithWriter(&buf)
	log.Debug("visible-debug")

	assert.True(t, strings.Contains(buf.String(), "visible-debug"),
		"Debug-level messages must be captured; levels below Debug would break test log-capture assertions")
}

// forgedAttr is an attacker-controlled attribute value attempting CWE-117 log
// forging: a newline followed by a well-formed second record.
const forgedAttr = "abc\nlevel=ERROR msg=\"FORGED ADMIN ACTION\""

// TestHandlersEscapeControlCharsInAttrValues locks the premise behind the slog
// exemption in [internal]rules/observability.md #5: handler-level output encoding
// is what makes it safe for internal/** handlers to log user-derived values
// (channel_id, user_id, …) WITHOUT sanitizeLogValue. That helper's mandate is
// scoped to the unescaped stdlib log.Printf sink.
//
// This is not a test of stdlib behavior for its own sake — it locks OUR handler
// CHOICE. Swapping in a handler that does not escape (a custom text formatter,
// say) would silently invalidate the exemption and turn every unsanitized
// handler log site into a genuine log-forging vector. Then this test fails.
//
// Both branches of New() are covered: TextHandler via the NewWithWriter seam
// (mirrors New("development")), and a JSONHandler constructed exactly as
// New("production") does — keep that construction in lockstep with logger.go.
func TestHandlersEscapeControlCharsInAttrValues(t *testing.T) {
	t.Run("TextHandler (development)", func(t *testing.T) {
		var buf bytes.Buffer
		NewWithWriter(&buf).Info("Channel purged", "channel_id", forgedAttr)

		assert.NotContains(t, buf.String(), "\nlevel=ERROR",
			"a raw newline reached the sink — log forging is possible and the observability.md #5 slog exemption is invalid")
		assert.Contains(t, buf.String(), `\n`,
			"the newline must survive as the literal two-char escape, which cannot line-split")
	})

	t.Run("JSONHandler (production)", func(t *testing.T) {
		var buf bytes.Buffer
		log := &Logger{Logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
		log.Info("Channel purged", "channel_id", forgedAttr)

		assert.NotContains(t, buf.String(), "\nlevel=ERROR",
			"a raw newline reached the sink — log forging is possible in production")
		assert.Equal(t, 1, strings.Count(strings.TrimRight(buf.String(), "\n"), "\n")+1,
			"the forged record must stay on ONE physical line")
	})
}
