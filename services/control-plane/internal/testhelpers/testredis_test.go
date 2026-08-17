package testhelpers

import (
	"os"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The delegation lock (#2680). SetupTestRedis must hand back a client on THIS
// process's allocated index — the same index redistest hands to every other
// caller in the binary — or a package that uses both the helper and its own
// client reads an empty database and gets no error.
func TestSetupTestRedisUsesAllocatedDatabase(t *testing.T) {
	rdb, cleanup := SetupTestRedis(t)
	defer cleanup()

	assert.Equal(t, redistest.DB(t), rdb.Options().DB,
		"the helper must land on this process's allocated index, never a fixed DB")
	assert.NotEqual(t, 0, rdb.Options().DB,
		"DB 0 is reserved: it holds the ticket counter and the dev app's data")
}

// The plan's third behavioural assertion — NotEqual(1, DB) — is deliberately
// NOT written here: index 1 is a legitimate allocation (1 + ((1-1) mod P) == 1),
// so it would fail whenever this process happens to draw the first ticket. The
// property it reached for is that the fixed pin is DELETED rather than made
// conditional, and that is a property of the source, so it is locked as one.
func TestSetupTestRedisKeepsNoFixedDatabasePin(t *testing.T) {
	raw, err := os.ReadFile("testredis.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	source := string(raw)

	// Asserted as booleans rather than with assert.NotContains so a failure
	// prints the reason instead of the whole file.
	assert.False(t, strings.Contains(source, "useDefaultDB"),
		"the useDefaultDB conditional IS the #2680 defect and must not return")
	assert.False(t, strings.Contains(source, "opts.DB ="),
		"the helper must never pin a database index; redistest allocates it")
	// Asserted positively rather than by banning the flush verb's literal: the
	// #2680 pre-commit guard rejects that literal in any _test.go outside
	// redistest, so writing it here to forbid it would trip the guard itself.
	assert.True(t, strings.Contains(source, "redistest.Reset"),
		"the reset must go through redistest.Reset, which fails closed on a foreign DB")
	assert.False(t, strings.Contains(source, "flushTestRedis"),
		"the package-local flush helper is deleted and must not return")
}
