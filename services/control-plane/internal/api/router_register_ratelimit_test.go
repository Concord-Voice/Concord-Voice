//nolint:revive // var-naming false positive on "api" in v2.10.1 (relaxed in v2.12+); matches router_test.go
package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// These tests lock the #1274 fix: the auth-flow rate-limit cap (POST /register,
// /register/confirm, /login) is relaxed ONLY under CONCORD_ENV=test, leaving
// production caps unchanged. The Playwright e2e suite registers a fresh user AND
// logs in per backend spec (login is what initializes e2eeService); from a single
// CI IP this exceeds the 5/15min /register cap, the 10/15min /register/confirm
// cap, and the 10/15min /login cap. The test gate keeps the suite from tripping
// the limiter without weakening production. See
// [internal]0011-playwright-e2e-rate-limit-test-gate.md.

func TestIsE2ETestEnv(t *testing.T) {
	t.Run("true when CONCORD_ENV=test", func(t *testing.T) {
		t.Setenv("CONCORD_ENV", "test")
		assert.True(t, isE2ETestEnv())
	})

	t.Run("false in production", func(t *testing.T) {
		t.Setenv("CONCORD_ENV", "production")
		assert.False(t, isE2ETestEnv())
	})

	t.Run("false when unset/empty", func(t *testing.T) {
		t.Setenv("CONCORD_ENV", "")
		assert.False(t, isE2ETestEnv())
	})
}

func TestAuthFlowTestRateLimit(t *testing.T) {
	t.Run("returns production cap unchanged when not test env", func(t *testing.T) {
		t.Setenv("CONCORD_ENV", "production")
		assert.Equal(t, 5, authFlowTestRateLimit(5), "/register production cap must be unchanged")
		assert.Equal(t, 10, authFlowTestRateLimit(10), "/register/confirm + /login production cap must be unchanged")
	})

	t.Run("relaxes to test cap under CONCORD_ENV=test", func(t *testing.T) {
		t.Setenv("CONCORD_ENV", "test")
		assert.Equal(t, testEnvAuthFlowCap, authFlowTestRateLimit(5))
		assert.Equal(t, testEnvAuthFlowCap, authFlowTestRateLimit(10))
	})

	t.Run("relaxed cap exceeds full e2e suite auth volume", func(t *testing.T) {
		// The suite performs ~18 registrations + an equal number of confirms +
		// a login per channels/messaging spec, plus Playwright CI retries. The
		// relaxed cap must comfortably exceed that so a full run never 429s.
		t.Setenv("CONCORD_ENV", "test")
		assert.Greater(t, authFlowTestRateLimit(10), 50,
			"relaxed cap must exceed the suite's auth-call volume incl. retries")
	})
}
