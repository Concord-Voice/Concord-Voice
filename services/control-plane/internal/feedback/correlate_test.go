package feedback

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeriveCorrelationToken(t *testing.T) {
	key := []byte("test-correlation-key-000000000000") // pragma: allowlist secret -- test fixture
	const uid = "550e8400-e29b-41d4-a716-446655440000"

	tok := DeriveCorrelationToken(key, uid)

	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{16}$`), tok, "16 lowercase hex chars")
	assert.NotContains(t, tok, uid, "token must not embed the raw UUID")
	assert.Equal(t, tok, DeriveCorrelationToken(key, uid), "deterministic")
	assert.NotEqual(t, tok, DeriveCorrelationToken(key, "other-user"), "different user → different token")
	assert.NotEqual(t, tok, DeriveCorrelationToken([]byte("different-key-00000000000000000000"), uid), "different key → different token")
}
