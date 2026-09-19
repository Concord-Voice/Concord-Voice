package members

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
)

// grantAdministrator gives a member the administrator bit, so the ban request
// clears every permission gate and reaches the OWNER guard — which is the only
// thing this test is about. Without it a refusal could be an ordinary
// insufficient-permissions 403 and the test would pass while proving nothing.
func grantAdministrator(t *testing.T, db *sql.DB, serverID, userID uuid.UUID) {
	t.Helper()
	roleID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions)
		 VALUES ($1, $2, 'canon-admin', 100, $3)`,
		roleID, serverID, int64(rbac.PermAdministrator))
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, userID, roleID)
	require.NoError(t, err)
}

// #3362 follow-on: the server owner is protected from moderation by BYTE-WISE
// comparisons against an id read from PostgreSQL —
//
//	if targetUserID == ownerID { ... "Cannot ban the server owner" }
//
// and the same shape inside rbac.CheckHierarchy. `ownerID` comes from a `uuid`
// column, so it is canonical; `targetUserID` came straight from
// `c.Param("user_id")`, validated by `uuid.Parse` and then used RAW.
//
// uuid.Parse is a parser, not a canonicality check. PostgreSQL resolves
// braced / UPPERCASE / dash-less spellings to the SAME row, so an administrator
// addressing the owner's id in any of them slipped BOTH guards — the explicit
// owner check and the hierarchy check — while the ban SQL still resolved to the
// owner's row. A server administrator could ban or kick the owner.
//
// This is strictly worse than the admission bypass #3362 itself fixes. That one
// needed a mint path that does not exist; this one needs only a differently
// spelled URL, supplied by the attacker.
//
// The fix canonicalizes at each `c.Param("user_id")` parse guard. This test
// drives the handler directly: on a mutant that reverts any of those, the
// variant subtests return 200-or-other instead of 403 and the owner is banned.
func TestBanMember_OwnerImmunityIsSpellingInvariant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, db := newKeyRotationHandler(t)

	owner := banTestUser(t, db)
	admin := banTestUser(t, db)
	serverID := banTestServer(t, db, owner)

	// Both are members; the admin is given the administrator bit so it clears
	// every permission gate and reaches the owner guard, which is the subject.
	for _, u := range []uuid.UUID{owner, admin} {
		_, err := db.Exec(
			`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, serverID, u)
		require.NoError(t, err)
	}
	grantAdministrator(t, db, serverID, admin)

	canonical := owner.String()
	dashless := strings.ReplaceAll(canonical, "-", "")

	for _, sp := range []struct{ name, target string }{
		{"canonical_control", canonical},
		{"braced", "{" + canonical + "}"},
		{"uppercase", strings.ToUpper(canonical)},
		{"dashless", dashless},
	} {
		t.Run(sp.name, func(t *testing.T) {
			if sp.name != "canonical_control" {
				require.NotEqual(t, canonical, sp.target,
					"a variant row must actually differ from the canonical form")
			}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodDelete,
				fmt.Sprintf("/servers/%s/members/%s/ban", serverID, sp.target), nil)
			c.Params = gin.Params{
				{Key: "id", Value: serverID.String()},
				{Key: "user_id", Value: sp.target},
			}
			c.Set("user_id", admin.String())

			h.BanMember(c)

			require.Equal(t, http.StatusForbidden, rec.Code,
				"the server owner must be unbannable under every spelling of their id; got %d: %s",
				rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "Cannot ban the server owner",
				"the refusal must be the OWNER guard, not an incidental permission failure")

			// The guard refusing is not the same fact as the owner surviving.
			// Assert the row directly: a mutant could refuse for one reason and
			// still have executed the ban.
			var banned bool
			require.NoError(t, db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
				serverID, owner).Scan(&banned))
			assert.False(t, banned, "the owner must not be in server_bans after a refused ban")

			var stillMember bool
			require.NoError(t, db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
				serverID, owner).Scan(&stillMember))
			assert.True(t, stillMember, "the owner must still be a member after a refused ban")
		})
	}
}
