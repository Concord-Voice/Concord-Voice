package logger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
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

	// The CodeQL query concord/go/log-injection-unstructured treats a constant-keyed
	// attribute value as sanitized for EVERY slog call, including ones that reach
	// slog's built-in default handler. This subtest locks that assumption.
	t.Run("slog default handler", func(t *testing.T) {
		buf := captureStdlibLog(t)
		require.Equal(t, "*slog.defaultHandler", fmt.Sprintf("%T", slog.Default().Handler()),
			"precondition: another test left a non-default slog handler installed")
		slog.Info("Channel purged", "channel_id", forgedAttr)

		assert.NotContains(t, buf.String(), "\nlevel=ERROR",
			"the default handler let a raw newline through an attribute value")
		assert.Equal(t, 1, strings.Count(strings.TrimRight(buf.String(), "\n"), "\n")+1)
	})
}

// forgedMessage puts the attacker's newline in the MESSAGE. slog's built-in default
// handler writes the message unescaped, which is why cmd/server installs the
// pkg/logger handler with slog.SetDefault.
const forgedMessage = "login failed for abc\n2026/09/24 12:00:00 ERROR FORGED ADMIN ACTION"

func captureStdlibLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevDefault, prevW, prevF := slog.Default(), log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		slog.SetDefault(prevDefault)
		log.SetOutput(prevW)
		log.SetFlags(prevF)
	})
	return &buf
}

func TestSlogDefaultHandlerForgesMessageWithoutSetDefault(t *testing.T) {
	// Positive control: proves the hazard that TestSetDefaultEscapesMessages removes.
	buf := captureStdlibLog(t)
	require.Equal(t, "*slog.defaultHandler", fmt.Sprintf("%T", slog.Default().Handler()))
	slog.Info(forgedMessage)

	assert.Contains(t, buf.String(), "\n2026/09/24 12:00:00 ERROR FORGED",
		"the default handler no longer forges; revisit the SetDefault call in cmd/server and the CodeQL query comment")
}

func TestSetDefaultEscapesMessages(t *testing.T) {
	captureStdlibLog(t) // restores the slog default and stdlib log state afterwards
	var buf bytes.Buffer
	slog.SetDefault(NewWithWriter(&buf).Logger)

	slog.Info(forgedMessage)
	slog.Default().Warn(forgedMessage)
	log.Printf("stdlib bridge: %s", forgedMessage)

	out := strings.TrimRight(buf.String(), "\n")
	assert.Equal(t, 3, strings.Count(out, "\n")+1, "three calls must produce three physical records:\n%s", out)
	assert.NotContains(t, out, "\n2026/09/24 12:00:00 ERROR FORGED")
}

// Two records make two '\n'-terminated lines, each one JSON object, so a
// reader splitting on newlines sees every record whole.
func TestNewJSONWritesOneJSONObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	log := NewJSON(&buf)
	log.Info("authority.exit", "reason", "trust")
	log.Info("authority.tick", "outcome", 3)
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("the last record is not newline-terminated: %q", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines for two records: %q", len(lines), out)
	}
	want := []map[string]any{
		{"msg": "authority.exit", "reason": "trust", "level": "INFO"},
		{"msg": "authority.tick", "outcome": float64(3), "level": "INFO"},
	}
	for i, l := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(l), &got); err != nil {
			t.Fatalf("line %d is not one JSON object: %q: %v", i, l, err)
		}
		for k, v := range want[i] {
			if got[k] != v {
				t.Fatalf("line %d: %s = %v, want %v (%v)", i, k, got[k], v, got)
			}
		}
	}
}

func TestNewJSONDropsDebug(t *testing.T) {
	var buf bytes.Buffer
	NewJSON(&buf).Debug("noise")
	if buf.Len() != 0 {
		t.Fatalf("debug line written: %q", buf.String())
	}
}
