//go:build integration

package rbac_test

// #3454 T8: the dangerous-action gates on UpsertChannelOverride and
// UpsertCategoryOverride, RS5 at both upserts' denial sites, the category
// forced-retry pin (A-1, §14.9) and the flip test. Driven over HTTP through
// the real router and the real MFA verifier, except the forced-retry pin,
// which needs the handler's test seam. Reuses role_gates_test.go's helpers.
// Each test names the mutant that kills it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// ogFixture is rgFixture plus an unsynced channel for the channel route and a
// category with one synced child, childID, for the category route. The
// override target is rgFixture's role.
type ogFixture struct {
	rgFixture
	channelID, categoryID, childID string
}

func newOGFixture(t *testing.T, ts *testhelpers.TestServer, enforcing, enrolled bool) ogFixture {
	t.Helper()
	f := ogFixture{rgFixture: newRGFixture(t, ts, enforcing, enrolled)}
	f.channelID = ts.CreateTestChannel(t, f.serverID, "gated-channel")
	f.categoryID = createTestCategory(t, ts, f.serverID, "gated-category")
	f.childID = ts.CreateTestChannel(t, f.serverID, "synced-child")
	assignChannelToCategory(t, ts, f.childID, f.categoryID, true)
	return f
}

// ogParent names an override table and the column keying its rows to their
// parent, the channel or the category.
type ogParent struct{ table, column string }

var (
	ogChannelParent  = ogParent{table: "channel_permission_overrides", column: "channel_id"}
	ogCategoryParent = ogParent{table: "category_permission_overrides", column: "category_id"}
)

// overrideGateRoute is one gated override upsert.
type overrideGateRoute struct {
	name                  string
	purpose, otherPurpose stepup.Purpose
	path                  func(f ogFixture) string
	// table and parentColumn locate the route's override rows.
	table, parentColumn string
	parent              func(f ogFixture) string
	// synced marks the category route, whose write mirrors its rows onto the
	// fixture's synced child.
	synced bool
}

func overrideGateRoutes() []overrideGateRoute {
	return []overrideGateRoute{
		{
			name: "UpsertChannelOverride", purpose: stepup.PurposeChannelOverrideUpsert,
			otherPurpose: stepup.PurposeCategoryOverrideUpsert,
			path:         func(f ogFixture) string { return channelOverridesPath(f.channelID) },
			table:        ogChannelParent.table, parentColumn: ogChannelParent.column,
			parent: func(f ogFixture) string { return f.channelID },
		},
		{
			name: "UpsertCategoryOverride", purpose: stepup.PurposeCategoryOverrideUpsert,
			otherPurpose: stepup.PurposeChannelOverrideUpsert,
			path:         func(f ogFixture) string { return categoryOverridesPath(f.categoryID) },
			table:        ogCategoryParent.table, parentColumn: ogCategoryParent.column,
			parent: func(f ogFixture) string { return f.categoryID },
			synced: true,
		},
	}
}

func ogBody(targetID string, allow, deny int64, code string) map[string]any {
	b := map[string]any{"target_type": "role", "target_id": targetID, "allow": bitsJSON(allow), "deny": bitsJSON(deny)}
	if code != "" {
		b["mfa_code"] = code
	}
	return b
}

func (r overrideGateRoute) put(ts *testhelpers.TestServer, token string, f ogFixture, allow, deny int64, code string) *httptest.ResponseRecorder {
	return ts.DoRequest(http.MethodPut, r.path(f), ogBody(f.roleID, allow, deny, code), testhelpers.AuthHeaders(token))
}

// do sends the gated write: a new dangerous allow on the fixture's role.
func (r overrideGateRoute) do(ts *testhelpers.TestServer, token string, f ogFixture, code string) *httptest.ResponseRecorder {
	return r.put(ts, token, f, rgDangerous, 0, code)
}

// rows counts the route's override rows for the fixture's role with allow.
func (r overrideGateRoute) rows(t *testing.T, ts *testhelpers.TestServer, f ogFixture, allow int64) int {
	t.Helper()
	return rgCount(t, ts.DB, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = $1 AND target_id = $2 AND allow = $3`,
		r.table, r.parentColumn), r.parent(f), f.roleID, allow)
}

// total counts every override row under the fixture's parent.
func (r overrideGateRoute) total(t *testing.T, ts *testhelpers.TestServer, f ogFixture) int {
	t.Helper()
	return rgCount(t, ts.DB, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = $1`, r.table, r.parentColumn), r.parent(f))
}

func (r overrideGateRoute) applied(t *testing.T, ts *testhelpers.TestServer, f ogFixture) bool {
	t.Helper()
	return r.rows(t, ts, f, rgDangerous) == 1
}

// seed writes the route's override row for the fixture's role directly. A
// category row is mirrored onto the synced child, as the category write's own
// sync copy leaves it, so the copy arm sees a child that already holds it.
func (r overrideGateRoute) seed(t *testing.T, ts *testhelpers.TestServer, f ogFixture, allow, deny int64) {
	t.Helper()
	ogInsertOverride(t, ts, ogParent{table: r.table, column: r.parentColumn}, r.parent(f), f.roleID, allow, deny)
	if r.synced {
		ogInsertOverride(t, ts, ogChannelParent, f.childID, f.roleID, allow, deny)
	}
}

func ogInsertOverride(t *testing.T, ts *testhelpers.TestServer, parent ogParent, parentID, roleID string, allow, deny int64) {
	t.Helper()
	_, err := ts.DB.Exec(fmt.Sprintf(`INSERT INTO %s (id, %s, target_type, target_id, allow, deny)
		VALUES (gen_random_uuid(), $1, 'role', $2, $3, $4)`, parent.table, parent.column), parentID, roleID, allow, deny)
	require.NoError(t, err)
}

// ogClassify classifies an override answer; both upserts answer 200.
func ogClassify(w *httptest.ResponseRecorder) string {
	return classifyRoleGate(roleGateRoute{okStatus: http.StatusOK}, w)
}

// The per-route table: setting x enrollment x submission, acting as the owner
// (I-ID). A refusal writes nothing; a server that does not enforce verifies
// nothing; a token minted for the sibling upsert's purpose is refused and
// stays unspent. An unenrolled owner of an enforcing server is masked, so RS5
// at the pooled pre-check answers before any transaction.
// Kills: requireOverrideGate removed from upsertChannelOverrideTx or
// upsertCategoryOverrideTx (enforcing + enrolled + no code answers ok); the
// two purposes swapped (the own-purpose token is refused).
func TestOverrideGate_PerRouteTable(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, r := range overrideGateRoutes() {
		for _, enforcing := range []bool{false, true} {
			for _, enrolled := range []bool{false, true} {
				for _, submission := range []string{rgNoCode, rgWrong, rgValid, rgOtherWebAuthn} {
					t.Run(fmt.Sprintf("%s/enforcing=%t/enrolled=%t/%s", r.name, enforcing, enrolled, submission), func(t *testing.T) {
						runOverrideGateCase(t, ts, r, enforcing, enrolled, submission)
					})
				}
			}
		}
	}
}

func runOverrideGateCase(t *testing.T, ts *testhelpers.TestServer, r overrideGateRoute, enforcing, enrolled bool, submission string) {
	f := newOGFixture(t, ts, enforcing, enrolled)
	code := map[string]string{rgNoCode: "", rgWrong: rgWrongCode}[submission]
	switch submission {
	case rgValid:
		code = rgMintToken(t, ts.DB, f.owner.ID, r.purpose)
	case rgOtherWebAuthn:
		code = rgMintToken(t, ts.DB, f.owner.ID, r.otherPurpose)
	}
	tokensBefore := rgLiveTokens(t, ts.DB, f.owner.ID)

	w := r.do(ts, f.owner.AccessToken, f, code)
	want := wantRoleGate(enforcing, enrolled, submission)
	require.Equal(t, want, ogClassify(w), w.Body.String())
	assert.Equal(t, want == rgOK, r.applied(t, ts, f), "the write lands exactly when the gate admits")

	wantTokens := tokensBefore
	if enforcing && enrolled && submission == rgValid {
		wantTokens--
	}
	assert.Equal(t, wantTokens, rgLiveTokens(t, ts.DB, f.owner.ID),
		"a token is spent only under its own purpose, on an enforcing server, by a committed write")
}

// The keys the override answer carried before #3454, for each route.
var ogOverrideKeys = map[string][]string{
	"UpsertChannelOverride":  {"allow", "channel_id", "created_at", "deny", "id", "target_id", "target_type", "updated_at"},
	"UpsertCategoryOverride": {"allow", "category_id", "created_at", "deny", "id", "target_id", "target_type", "updated_at"},
}

// With the setting off, a request that carries no code is answered as before
// #3454: one "override" key holding exactly the pre-#3454 fields, the
// decimal-string bitfields, no budget charge and no grace.
// Kills: Charge run without a code; a grace granted on an unverified success;
// mfa_code echoed into the answer.
func TestOverrideGate_SettingOffAndNoCodeIsUnchanged(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, r := range overrideGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newOGFixture(t, ts, false, true)
			w := r.do(ts, f.owner.AccessToken, f, "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var body map[string]map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Len(t, body, 1, w.Body.String())
			keys := make([]string, 0, len(body["override"]))
			for k := range body["override"] {
				keys = append(keys, k)
			}
			assert.ElementsMatch(t, ogOverrideKeys[r.name], keys)
			assert.Equal(t, bitsJSON(rgDangerous), body["override"]["allow"], "the decimal-string wire form is unchanged")
			assert.True(t, r.applied(t, ts, f))
			assert.Equal(t, 0, rgBudget(t, ts.Redis, f.owner.ID), "a request with no code is never charged")
			graces, err := ts.Redis.Keys(context.Background(), "stepup:grace:"+f.owner.ID+":*").Result()
			require.NoError(t, err)
			assert.Empty(t, graces, "an unverified success grants nothing")
		})
	}
}

// The budget is charged before the transaction only when a code is sent,
// stands on a refusal and on an unverified success, and is cleared only after
// a verified commit.
// Kills: Charge dropped or run on every request; settleRoleGate skipped, or
// run before the commit or on a refusal.
func TestOverrideGate_BudgetIsChargedOnlyWithACode(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, r := range overrideGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			off := newOGFixture(t, ts, false, true)
			require.Equal(t, rgOK, ogClassify(r.do(ts, off.owner.AccessToken, off, rgWrongCode)))
			assert.Equal(t, 1, rgBudget(t, ts.Redis, off.owner.ID),
				"a code is charged whatever the setting, and an unverified success clears nothing")

			on := newOGFixture(t, ts, true, true)
			require.Equal(t, rgRequired, ogClassify(r.do(ts, on.owner.AccessToken, on, "")))
			assert.Equal(t, 0, rgBudget(t, ts.Redis, on.owner.ID), "a refusal with no code is not charged")
			require.Equal(t, rgInvalid, ogClassify(r.do(ts, on.owner.AccessToken, on, rgWrongCode)))
			assert.Equal(t, 1, rgBudget(t, ts.Redis, on.owner.ID), "a refused code stays charged")
			token := rgMintToken(t, ts.DB, on.owner.ID, r.purpose)
			require.Equal(t, rgOK, ogClassify(r.do(ts, on.owner.AccessToken, on, token)))
			assert.Equal(t, 0, rgBudget(t, ts.Redis, on.owner.ID), "a verified commit clears the budget")
		})
	}
}

// The gate fires only when the allow NEWLY confers a dangerous bit, read
// against the row's prior allow: re-sending an existing dangerous allow,
// adding a deny (even a dangerous one), removing a deny, and adding a non-D
// allow are all ungated on an enforcing server, with no code.
// Kills: the prior read dropped, or fires computed on the requested allow
// alone (grantsDangerous(0, allow)): re-sending the existing grant would be
// prompted.
func TestOverrideGate_FiresOnlyOnANewDangerousAllow(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	send := int64(rbac.PermSendMessages)
	for _, r := range overrideGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newOGFixture(t, ts, true, true)
			r.seed(t, ts, f, rgDangerous, 0)
			token := f.owner.AccessToken
			steps := []struct {
				what        string
				allow, deny int64
			}{
				{"re-sending the existing dangerous allow", rgDangerous, 0},
				{"adding a deny", rgDangerous, send},
				{"adding a dangerous deny", rgDangerous, send | int64(rbac.PermManageDevResources)},
				{"removing the denies", rgDangerous, 0},
				{"adding a non-dangerous allow", rgDangerous | send, 0},
			}
			for _, s := range steps {
				w := r.put(ts, token, f, s.allow, s.deny, "")
				assert.Equal(t, http.StatusOK, w.Code, "%s is ungated: %s", s.what, w.Body.String())
			}
			w := r.put(ts, token, f, rgDangerous|send|int64(rbac.PermManageDevResources), 0, "")
			assert.Equal(t, rgRequired, ogClassify(w), "a newly added dangerous allow fires")
			assert.Equal(t, 1, r.rows(t, ts, f, rgDangerous|send), "the refusal wrote nothing")
		})
	}
}

// RS5 at the pooled pre-check (§14.1): on an enforcing server, a member who
// would hold ManageChannels without the mask, through the bit or raw
// Administrator, and has no inline factor, is told to enroll; a member
// without the bit, enrolled or not, gets the unchanged generic 403. Nothing
// is written.
// Kills: EnrollmentDenial removed from authorizeOverrideUpsert.
func TestOverrideGate_RS5AtThePooledCheck(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	f := newOGFixture(t, ts, true, true)
	manage := ts.CreateTestRole(t, f.serverID, "manage-channels", 5, int64(rbac.PermManageChannels))
	admin := ts.CreateTestRole(t, f.serverID, "admin", 6, int64(rbac.PermAdministrator))
	other := ts.CreateTestRole(t, f.serverID, "other", 4, int64(rbac.PermSendMessages))
	restricted := map[string]testhelpers.TestUser{
		"unenrolled ManageChannels holder": rgPersona(t, ts, f.serverID, manage, "osm", false),
		"unenrolled raw Administrator":     rgPersona(t, ts, f.serverID, admin, "osa", false),
	}
	generic := map[string]testhelpers.TestUser{
		"unenrolled member without the bit": rgPersona(t, ts, f.serverID, other, "osu", false),
		"enrolled member without the bit":   rgPersona(t, ts, f.serverID, other, "ose", true),
	}
	for _, r := range overrideGateRoutes() {
		for name, u := range restricted {
			assert.Equal(t, rgEnrollment, ogClassify(r.do(ts, u.AccessToken, f, "")), "%s %s", r.name, name)
		}
		for name, u := range generic {
			w := r.do(ts, u.AccessToken, f, rgWrongCode)
			assert.Equal(t, http.StatusForbidden, w.Code, "%s %s", r.name, name)
			assert.Equal(t, rgForbiddenBody, w.Body.String(), "%s %s", r.name, name)
		}
		assert.Zero(t, r.total(t, ts, f), "%s: no refusal writes", r.name)
	}
}

// RS5 inside the transactions: a planted permission-cache entry lets a member
// past the cached pre-check, as a flag flipped on after that entry was
// published would. The in-transaction ManageChannels denial is then the
// masked one for an unenrolled raw holder, answered with the enrollment body
// on the transaction, and the generic 403 for a member without the bit.
// Kills: EnrollmentDenial removed from authorizeOverrideUpsertTx.
func TestOverrideGate_RS5InTheTransaction(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, r := range overrideGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newOGFixture(t, ts, true, true)
			holder := rgPersona(t, ts, f.serverID,
				ts.CreateTestRole(t, f.serverID, "manage-channels", 5, int64(rbac.PermManageChannels)), "osp", false)
			without := rgPersona(t, ts, f.serverID,
				ts.CreateTestRole(t, f.serverID, "other", 4, int64(rbac.PermSendMessages)), "osn", false)
			for _, u := range []testhelpers.TestUser{holder, without} {
				testhelpers.PublishPermissionCache(t, ts.Redis, f.serverID, u.ID, "", rbac.PermManageChannels)
			}
			w := r.put(ts, holder.AccessToken, f, 0, 0, "")
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.JSONEq(t, rgEnrollmentBody(), w.Body.String())
			w = r.put(ts, without.AccessToken, f, 0, 0, "")
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.Equal(t, rgForbiddenBody, w.Body.String())
			assert.Zero(t, r.total(t, ts, f), "no refusal writes")
		})
	}
}

// The category forced-retry pin (§14.9, A-1). A synced child added between
// withStableSyncedCategoryAuthority's preflight and its locked re-read forces
// errCategorySyncSetChanged on attempt 1, which returns before write, so only
// attempt 2 reaches the gate. The gated upsert still succeeds; the real
// verifier runs once, for the one attempt that reached the gate; and the
// WebAuthn token is gone after the commit.
// Kills: the set comparison moved after write in
// withStableSyncedCategoryAuthority (the verifier runs twice); Charge or the
// gate moved outside write.
func TestOverrideGate_CategoryForcedRetryVerifiesOncePerGateAttempt(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	f := newOGFixture(t, ts, true, true)

	ring, err := mfa.ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	log := logger.New("test")
	verifier := &rgCountingVerifier{inner: mfa.NewHandler(ts.DB, ts.Redis, log, ring, testhelpers.TestJWTSecret, nil, "test")}
	cache := rbac.NewPermissionCache(ts.Redis)
	h := rbac.NewHandler(ts.DB, log, ts.Redis, websocket.NewHub(ts.DB, ts.Redis), rbac.NewResolver(ts.DB, cache, log), cache, nil)
	h.SetMFAVerifier(verifier)
	attempts := 0
	rbac.SetSyncedCategoryPreflightForTest(h, func() {
		attempts++
		if attempts == 1 {
			assignChannelToCategory(t, ts, ts.CreateTestChannel(t, f.serverID, "forces-the-retry"), f.categoryID, true)
		}
	})

	token := rgMintToken(t, ts.DB, f.owner.ID, stepup.PurposeCategoryOverrideUpsert)
	route := overrideGateRoutes()[1]
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: f.categoryID}}
	c.Set("user_id", f.owner.ID)
	body, err := json.Marshal(ogBody(f.roleID, rgDangerous, 0, token))
	require.NoError(t, err)
	c.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	h.UpsertCategoryOverride(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 2, attempts, "the forced change must make the wrapper retry")
	assert.Equal(t, []stepup.Purpose{stepup.PurposeCategoryOverrideUpsert}, verifier.purposes,
		"the verifier runs once per attempt that reached the gate: attempt 1 returned before write")
	assert.Zero(t, rgLiveTokens(t, ts.DB, f.owner.ID), "the token is spent by the committed attempt")
	assert.True(t, route.applied(t, ts, f))
	assert.Equal(t, 0, rgBudget(t, ts.Redis, f.owner.ID), "the verified commit cleared the budget")
}

// The flip test: each upsert is held at its write, after its gate read the
// flag OFF, and the toggle's UPDATE must wait on the servers row the gate
// re-took, so the flag cannot change between the gate's read and the commit
// it governs.
// Kills: lockRoleGateTx removed from authorizeOverrideUpsertTx (the hook
// never sees the gate's flag read, so seen stays below 2).
func TestOverrideGate_FlipWaitsForTheGate(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	firstWrite := map[string]string{
		"UpsertChannelOverride":  `(id, channel_id, target_type, target_id, allow, deny, is_temporary, temporary_reason, granted_at)`,
		"UpsertCategoryOverride": `INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)`,
	}
	for _, r := range overrideGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newOGFixture(t, ts, false, true)
			var flip <-chan error
			hook.Arm([]string{
				`enforce_mfa_dangerous_actions, current_setting('transaction_isolation')`,
				firstWrite[r.name],
			}, func() error {
				flip = rgStartFlip(ts.DB, f.serverID)
				return rgWaitForFlipLockWait(ts.DB, flip)
			}, nil)
			w := r.do(ts, f.owner.AccessToken, f, "")
			seen, betweenErr := hook.Report()
			require.Equal(t, 2, seen, "the gate's servers read, then the write")
			require.NoError(t, betweenErr)
			require.Equal(t, http.StatusOK, w.Code, "the gate read the flag OFF and the write committed: %s", w.Body.String())
			select {
			case err := <-flip:
				require.NoError(t, err, "the toggle commits once the gate transaction ends")
			case <-time.After(10 * time.Second):
				t.Fatal("the toggle never resumed")
			}
		})
	}
}

// ogWaitForPriorReadLockWait reports whether a backend is blocked on a row
// lock while running the gate's prior-allow read against table.
func ogWaitForPriorReadLockWait(t *testing.T, ts *testhelpers.TestServer, table string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%SELECT allow FROM ' || $1 || '%'`, table).Scan(&n))
		if n > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// The prior-allow read under a concurrent delete. The prior row already
// carries the dangerous allow, so re-sending it with no code is ungated by
// design; a second transaction deletes that row and holds the delete open
// while the upsert runs. The read must wait on the delete's row lock and,
// once it commits, read prior 0, so the re-send is a new grant and is refused
// with nothing written. The category route's seed also holds the allow on the
// synced child, so its copy arm cannot fire in the read's place.
// Kills: the prior read's FOR NO KEY UPDATE dropped (it reads the pre-delete
// row without waiting, the gate does not fire, and a dangerous row lands with
// no factor).
func TestOverrideGate_PriorAllowReadWaitsOnAConcurrentDelete(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, r := range overrideGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newOGFixture(t, ts, true, true)
			r.seed(t, ts, f, rgDangerous, 0)

			del, err := ts.DB.Begin()
			require.NoError(t, err)
			defer func() { _ = del.Rollback() }()
			res, err := del.Exec(fmt.Sprintf(`DELETE FROM %s WHERE %s = $1 AND target_type = 'role' AND target_id = $2`,
				r.table, r.parentColumn), r.parent(f), f.roleID)
			require.NoError(t, err)
			n, err := res.RowsAffected()
			require.NoError(t, err)
			require.EqualValues(t, 1, n, "the prior row must exist before the race")

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- r.do(ts, f.owner.AccessToken, f, "") }()

			waited := ogWaitForPriorReadLockWait(t, ts, r.table)
			require.NoError(t, del.Commit())

			var w *httptest.ResponseRecorder
			select {
			case w = <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the upsert did not finish")
			}
			require.True(t, waited, "the prior read must wait on the concurrent delete")
			require.Equal(t, rgRequired, ogClassify(w), w.Body.String())
			require.Equal(t, 0, r.rows(t, ts, f, rgDangerous), "the refusal wrote nothing")
		})
	}
}

// ogPlantCopied writes roleID's override directly on the category with
// catAllow and on its synced child with childAllow. A childAllow lacking a bit
// catAllow carries is a child that diverged from its category: the state a
// ManageChannels holder reaches by removing an allow on the child, which is
// ungated and does not unsync it.
func ogPlantCopied(t *testing.T, ts *testhelpers.TestServer, f ogFixture, roleID string, catAllow, childAllow int64) {
	t.Helper()
	ogInsertOverride(t, ts, ogCategoryParent, f.categoryID, roleID, catAllow, 0)
	ogInsertOverride(t, ts, ogChannelParent, f.childID, roleID, childAllow, 0)
}

// ogChildAllow is roleID's allow on the synced child; the row must exist.
func ogChildAllow(t *testing.T, ts *testhelpers.TestServer, f ogFixture, roleID string) int64 {
	t.Helper()
	var allow int64
	require.NoError(t, ts.DB.QueryRow(`SELECT allow FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'role' AND target_id = $2`, f.childID, roleID).Scan(&allow))
	return allow
}

// ogOverrideSnapshot is every override row on the category and its synced
// child, ids included, so a rewrite of either is visible even when it writes
// the same values back.
func ogOverrideSnapshot(t *testing.T, ts *testhelpers.TestServer, f ogFixture) []string {
	t.Helper()
	rows, err := ts.DB.Query(`
		SELECT 'category', id, target_id, allow, deny FROM category_permission_overrides WHERE category_id = $1
		UNION ALL
		SELECT 'child', id, target_id, allow, deny FROM channel_permission_overrides WHERE channel_id = $2
		ORDER BY 1, 2`, f.categoryID, f.childID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var where, id, target string
		var allow, deny int64
		require.NoError(t, rows.Scan(&where, &id, &target, &allow, &deny))
		out = append(out, fmt.Sprintf("%s %s %s %d %d", where, id, target, allow, deny))
	}
	require.NoError(t, rows.Err())
	return out
}

// The category upsert's sync copy rewrites every synced child from every
// category row, so on an enforcing server the upsert is gated when that copy
// would newly confer a dangerous allow on a child for ANY target, not only for
// the request's own row. Here the child diverged from the category by losing
// a target's dangerous allow; a category edit whose own row confers nothing
// would restore it. Without a code the edit is refused and writes nothing,
// neither the category row nor the child; with a valid code it lands and the
// copy restores the allow.
// Kills: the copy arm removed from requireOverrideGate (both cases answer ok
// with no code, and the copy re-confers the allow).
func TestOverrideGate_CategoryCopyNewlyConferringADangerousAllowIsGated(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	route := overrideGateRoutes()[1]
	cases := []struct {
		name     string
		own      bool  // the diverged target is the request's own
		reqAllow int64 // the request's allow, for the fixture's role
	}{
		{"another target's dangerous allow", false, 0},
		{"the request's own unchanged dangerous allow", true, rgDangerous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOGFixture(t, ts, true, true)
			target := f.roleID
			if !tc.own {
				target = ts.CreateTestRole(t, f.serverID, "copied-target", 1, 0)
			}
			ogPlantCopied(t, ts, f, target, rgDangerous, 0)
			before := ogOverrideSnapshot(t, ts, f)

			w := route.put(ts, f.owner.AccessToken, f, tc.reqAllow, 0, "")
			require.Equal(t, rgRequired, ogClassify(w), w.Body.String())
			assert.Equal(t, before, ogOverrideSnapshot(t, ts, f), "the refusal wrote neither the category nor the child")

			token := rgMintToken(t, ts.DB, f.owner.ID, stepup.PurposeCategoryOverrideUpsert)
			w = route.put(ts, f.owner.AccessToken, f, tc.reqAllow, 0, token)
			require.Equal(t, rgOK, ogClassify(w), w.Body.String())
			assert.Equal(t, rgDangerous, ogChildAllow(t, ts, f, target), "the confirmed copy restores the allow")
			assert.Equal(t, 1, route.rows(t, ts, f, tc.reqAllow), "the confirmed edit lands")
		})
	}
}

// What the copy arm leaves ungated: a category edit whose sync copy confers no
// new dangerous allow on the child, judged on the post-write category rows,
// and any edit when the setting is off. Each answers ok with no code and the
// copy lands.
// Kills: the copy arm read without the child's allow (the child already
// holds it), on the pre-write category rows (removing the request's own
// diverged allow fires), or on a server that does not enforce.
func TestOverrideGate_CategoryCopyConferringNothingNewIsUngated(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	route := overrideGateRoutes()[1]
	send := int64(rbac.PermSendMessages)
	cases := []struct {
		name                           string
		enforcing, own                 bool
		catAllow, childAllow, reqAllow int64
	}{
		{"the child already holds the copied dangerous allow", true, false, rgDangerous, rgDangerous, 0},
		{"no copied row carries a dangerous bit", true, false, send, 0, send},
		{"the copy only removes a dangerous allow", true, false, 0, rgDangerous, 0},
		{"the request removes its own diverged dangerous allow", true, true, rgDangerous, 0, 0},
		{"the setting is off", false, false, rgDangerous, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOGFixture(t, ts, tc.enforcing, true)
			target, want := f.roleID, tc.reqAllow
			if !tc.own {
				target, want = ts.CreateTestRole(t, f.serverID, "copied-target", 1, 0), tc.catAllow
			}
			ogPlantCopied(t, ts, f, target, tc.catAllow, tc.childAllow)

			w := route.put(ts, f.owner.AccessToken, f, tc.reqAllow, 0, "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, want, ogChildAllow(t, ts, f, target), "the copy landed")
		})
	}
}
