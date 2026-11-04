package decide

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refLess and the accountguard runner's compareRefs are two copies of one order (kind,
// then ID byte for byte, then version) that must never drift. Both are held to the same
// fixture, which lists refs in the expected order: it covers the kind number, an ID
// order that differs from length order ("a" < "aa" < "b"), a non-ASCII byte, and
// negative versions. accountguard/fingerprint_test.go holds compareRefs to it.
func TestRefLess_MatchesTheSharedOrderFixture(t *testing.T) {
	raw, err := os.ReadFile("../testdata/ref_order.json")
	require.NoError(t, err)
	var want []CredRef
	require.NoError(t, json.Unmarshal(raw, &want))
	require.Greater(t, len(want), 1)

	for i := 1; i < len(want); i++ {
		assert.True(t, refLess(want[i-1], want[i]), "fixture entry %d must sort strictly after entry %d", i, i-1)
		assert.False(t, refLess(want[i], want[i-1]), "fixture entries %d and %d", i-1, i)
	}

	reversed := slices.Clone(want)
	slices.Reverse(reversed)
	rotated := append(slices.Clone(want[3:]), want[:3]...)
	for name, shuffled := range map[string][]CredRef{"reversed": reversed, "rotated": rotated} {
		sort.Slice(shuffled, func(i, j int) bool { return refLess(shuffled[i], shuffled[j]) })
		assert.Equal(t, want, shuffled, name)
	}
}
