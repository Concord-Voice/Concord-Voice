package oauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Digest helpers feed the apple_client_secret_minted audit event. They must be
// deterministic, 16 hex chars, and never echo their input (observability rule:
// no raw state tokens or IPs in any log sink).
func TestDigestState_DeterministicTruncatedHex(t *testing.T) {
	a := digestState("state-token-aaaa")
	b := digestState("state-token-aaaa")
	c := digestState("state-token-bbbb")

	assert.Equal(t, a, b, "same input must digest identically")
	assert.NotEqual(t, a, c, "different inputs must differ")
	assert.Len(t, a, 16)
	assert.Regexp(t, "^[0-9a-f]{16}$", a)
	assert.NotContains(t, a, "state-token")
}

func TestDigestIP_KeyedAndTruncated(t *testing.T) {
	k1 := []byte("audit-key-one")
	k2 := []byte("audit-key-two")

	a := digestIP(k1, "203.0.113.7")
	b := digestIP(k1, "203.0.113.7")
	c := digestIP(k2, "203.0.113.7")
	d := digestIP(k1, "203.0.113.8")

	assert.Equal(t, a, b, "deterministic under one key")
	assert.NotEqual(t, a, c, "key must change the digest (defeats unkeyed brute force of low-entropy IPs)")
	assert.NotEqual(t, a, d, "IP must change the digest")
	assert.Len(t, a, 16)
	assert.Regexp(t, "^[0-9a-f]{16}$", a)
}
