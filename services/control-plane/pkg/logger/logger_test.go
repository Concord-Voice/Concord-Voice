package logger

import (
	"bytes"
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
