package servers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Regression for the #3453 Phase-4 red-team finding. PostgreSQL resolves every
// spelling of a uuid to one row, but the permission cache keyed its entries and
// its server generation on the RAW :id path parameter. A member who primed the
// cache through a non-canonical spelling (uppercase, or without hyphens) got an
// entry tagged with a server generation the enforcement flip never bumps, so
// after enforcement turned ON the unenrolled Administrator kept reading, and
// USING, the dangerous bits through that spelling until the cache TTL expired.
// Reproduced as a rename of an enforcing server via PATCH /servers/<UPPER>.
//
// Each subtest uses its own server, primes through the alternate spelling while
// enforcement is off, flips it on through the canonical spelling, and requires
// that the alternate spelling is masked on the very next read and cannot act.
func TestMFAEnforcement_FlipReachesEveryIDSpelling(t *testing.T) {
	env := setupMFAEnforcementEnv(t)

	spellings := []struct {
		name  string
		spell func(string) string
	}{
		{"uppercase", strings.ToUpper},
		{"no hyphens", func(id string) string { return strings.ReplaceAll(id, "-", "") }},
	}
	for i, sp := range spellings {
		t.Run(sp.name, func(t *testing.T) {
			f := newMFAFixture(t, env, fmt.Sprintf("idsp%d", i), false)
			enrollMFATOTP(t, env, f.owner.ID)
			alt := sp.spell(f.serverID)
			require.NotEqual(t, f.serverID, alt)

			perms := func(serverID string) rbac.Permission {
				t.Helper()
				w := env.ts.DoRequest(http.MethodGet, "/api/v1/servers/"+serverID+"/permissions", nil,
					testhelpers.AuthHeaders(f.admin.AccessToken))
				require.Equal(t, http.StatusOK, w.Code, "the app must accept this spelling: %s", w.Body.String())
				var body struct {
					Permissions int64 `json:"permissions"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				return rbac.Permission(body.Permissions)
			}

			// Prime the cache through the alternate spelling while enforcement is off.
			require.True(t, perms(alt).Has(rbac.PermManageServer), "setup: unmasked before the flip")
			require.True(t, perms(alt).Has(rbac.PermManageServer), "setup: the primed entry is served")

			on := putMFA(env, f.owner, f.serverID, bodyOn())
			require.Equal(t, http.StatusOK, on.Code, on.Body.String())
			require.True(t, readMFAFlag(t, env, f.serverID))

			// The alternate spelling is read FIRST: a canonical read would recompute
			// and overwrite the one shared value entry, hiding a server generation
			// that is still keyed by spelling.
			assert.False(t, perms(alt).Has(rbac.PermManageServer),
				"an unenrolled Administrator still reads ManageServer through %q after enforcement turned ON", alt)
			require.False(t, perms(f.serverID).Has(rbac.PermManageServer), "control: the canonical spelling is masked")

			patch := env.ts.DoRequest(http.MethodPatch, "/api/v1/servers/"+alt,
				map[string]any{"name": "renamed-through-" + sp.name}, testhelpers.AuthHeaders(f.admin.AccessToken))
			assert.Equal(t, http.StatusForbidden, patch.Code,
				"a dangerous action through %q must be refused on an enforcing server: %s", alt, patch.Body.String())
			var name string
			require.NoError(t, env.ts.DB.QueryRow(`SELECT name FROM servers WHERE id = $1`, f.serverID).Scan(&name))
			assert.NotEqual(t, "renamed-through-"+sp.name, name, "the enforcing server must not be renamed")
		})
	}
}
