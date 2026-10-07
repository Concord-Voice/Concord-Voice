package permgen

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestKeys pins the spelling both consumers depend on: rbac writes these keys
// and stepup's grace store reads them, so a change here moves both at once.
func TestKeys(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	require.Equal(t, "permgen:u:"+id, UserKey(id))
	require.Equal(t, "permgen:s:"+id, ServerKey(id))
	require.Equal(t, "permgen:u:"+id, UserKey("{0F8FAD5BD9CB469FA16570867728950E}"), "every uuid spelling is one key")
}

// TestCanonicalID: every spelling PostgreSQL resolves to one uuid folds to its
// canonical form; anything that is not a uuid is left exactly as given.
func TestCanonicalID(t *testing.T) {
	const canonical = "0f8fad5b-d9cb-469f-a165-70867728950e"
	for _, in := range []string{
		canonical,
		"0F8FAD5B-D9CB-469F-A165-70867728950E",
		"0f8fad5bd9cb469fa16570867728950e", // pragma: allowlist secret
		"{0f8fad5b-d9cb-469f-a165-70867728950e}",
		"0f8f-ad5b-d9cb-469f-a165-7086-7728-950e",
	} {
		require.Equal(t, canonical, CanonicalID(in), in)
	}
	for _, in := range []string{"", "not-a-uuid", "0f8fad5bd9cb469fa16570867728950", "0f8fad5bd9cb469fa16570867728950e0", "0f8fad5b d9cb"} { // pragma: allowlist secret
		require.Equal(t, in, CanonicalID(in), "%q is not a uuid and must pass through", in)
	}
}
