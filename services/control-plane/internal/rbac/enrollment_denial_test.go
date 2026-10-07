package rbac_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// genericDenialBody stands in for a route's existing 403. EnrollmentDenial's
// nil must leave a caller writing exactly this.
var genericDenialBody = gin.H{"error": "Insufficient permissions"}

// recordingQuerier records every statement, so a test can compare the work
// done on two denials, and can fail the statement at index failAt.
type recordingQuerier struct {
	inner interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	}
	statements []string
	failAt     int
}

func (q *recordingQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.statements = append(q.statements, query)
	if len(q.statements)-1 == q.failAt {
		query = `SELECT 1/0`
	}
	return q.inner.QueryRowContext(ctx, query, args...)
}

// writeDenial is a route's denial site: the RS5 body when EnrollmentDenial
// returns one, the route's generic 403 otherwise.
func writeDenial(e *stepup.Error) (int, string) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if e != nil {
		e.Write(c)
	} else {
		c.JSON(http.StatusForbidden, genericDenialBody)
	}
	return w.Code, w.Body.String()
}

// rs5Fixture is one fresh server per case: its owner, two of its channels,
// and a channel of a second server.
type rs5Fixture struct {
	ts                                     *testhelpers.TestServer
	serverID, ownerID                      string
	channelID, otherChannelID, foreignChID string
}

func newRS5Fixture(t *testing.T, ts *testhelpers.TestServer, enforcing bool) rs5Fixture {
	t.Helper()
	f := rs5Fixture{ts: ts, ownerID: ts.CreateTestUser(t, "rs5o"+uuid.NewString()[:8]).ID}
	f.serverID = ts.CreateTestServer(t, f.ownerID, "RS5 fixture")
	f.channelID = ts.CreateTestChannel(t, f.serverID, "scoped")
	f.otherChannelID = ts.CreateTestChannel(t, f.serverID, "other")
	foreignServer := ts.CreateTestServer(t, f.ownerID, "RS5 foreign")
	f.foreignChID = ts.CreateTestChannel(t, foreignServer, "foreign")
	if enforcing {
		enforceMFA(t, ts, f.serverID)
	}
	return f
}

// member adds a fresh member holding roleBits through one role (none when 0).
func (f rs5Fixture) member(t *testing.T, roleBits rbac.Permission) (userID, roleID string) {
	t.Helper()
	userID = f.ts.CreateTestUser(t, "rs5m"+uuid.NewString()[:8]).ID
	f.ts.AddMemberToServer(t, f.serverID, userID, "member")
	if roleBits != 0 {
		roleID = f.ts.CreateTestRole(t, f.serverID, "RS5 role", 5, int64(roleBits))
		f.ts.AssignRoleToUser(t, f.serverID, userID, roleID)
	}
	return userID, roleID
}

type rs5Actor func(t *testing.T, f rs5Fixture) string

func ownerActor(enrolled bool) rs5Actor {
	return func(t *testing.T, f rs5Fixture) string {
		if enrolled {
			enrollWebAuthn(t, f.ts, f.ownerID)
		}
		return f.ownerID
	}
}

func memberActor(bits rbac.Permission, enrolled bool) rs5Actor {
	return func(t *testing.T, f rs5Fixture) string {
		id, _ := f.member(t, bits)
		if enrolled {
			enrollWebAuthn(t, f.ts, id)
		}
		return id
	}
}

func nonMemberActor(t *testing.T, f rs5Fixture) string {
	return f.ts.CreateTestUser(t, "rs5n"+uuid.NewString()[:8]).ID
}

// channelAllowActor holds ManageAllMessages ONLY through a user ALLOW on the
// fixture's scoped channel (X18's member).
func channelAllowActor(enrolled bool) rs5Actor {
	return func(t *testing.T, f rs5Fixture) string {
		id := memberActor(0, enrolled)(t, f)
		f.ts.CreateChannelOverride(t, f.channelID, "user", id, int64(rbac.PermManageAllMessages), 0)
		return id
	}
}

// roleDenyActor holds ManageAllMessages through a role, which a role DENY on
// the scoped channel removes there.
func roleDenyActor(t *testing.T, f rs5Fixture) string {
	id, roleID := f.member(t, rbac.PermManageAllMessages)
	f.ts.CreateChannelOverride(t, f.channelID, "role", roleID, 0, int64(rbac.PermManageAllMessages))
	return id
}

// adminDenyActor is a raw Administrator whose user DENY on the scoped channel
// names the bit AND bit 62. Raw bit 62 bypasses overrides, so neither applies.
func adminDenyActor(t *testing.T, f rs5Fixture) string {
	id, _ := f.member(t, rbac.PermAdministrator)
	f.ts.CreateChannelOverride(t, f.channelID, "user", id, 0,
		int64(rbac.PermManageAllMessages|rbac.PermAdministrator))
	return id
}

// ownerDenyActor is the owner with a user DENY on the scoped channel, which
// the owner bypass ignores.
func ownerDenyActor(t *testing.T, f rs5Fixture) string {
	f.ts.CreateChannelOverride(t, f.channelID, "user", f.ownerID, 0, int64(rbac.PermManageAllMessages))
	return f.ownerID
}

// The denial's scope.
type rs5Scope func(f rs5Fixture) string

func serverScope(rs5Fixture) string      { return "" }
func scopedChannel(f rs5Fixture) string  { return f.channelID }
func otherChannel(f rs5Fixture) string   { return f.otherChannelID }
func foreignChannel(f rs5Fixture) string { return f.foreignChID }

type rs5Case struct {
	name        string
	enforcing   bool
	actor       rs5Actor
	scope       rs5Scope
	requiredBit rbac.Permission
	// vanished deletes the server after setup, so the denial names a server
	// that no longer exists.
	vanished bool
	want     bool
}

var serverScopeCases = []rs5Case{
	// The predicate's true arms: owner, raw bit, raw Administrator.
	{"unenrolled owner, enforcing", true, ownerActor(false), serverScope, rbac.PermManageChannels, false, true},
	{"unenrolled raw holder, enforcing", true, memberActor(rbac.PermManageChannels, false), serverScope, rbac.PermManageChannels, false, true},
	{"unenrolled raw Administrator, enforcing", true, memberActor(rbac.PermAdministrator, false), serverScope, rbac.PermBan, false, true},

	// Each conjunct false in turn.
	{"unenrolled owner, not enforcing", false, ownerActor(false), serverScope, rbac.PermManageChannels, false, false},
	{"unenrolled raw holder, not enforcing", false, memberActor(rbac.PermManageChannels, false), serverScope, rbac.PermManageChannels, false, false},
	{"enrolled owner, enforcing", true, ownerActor(true), serverScope, rbac.PermManageChannels, false, false},
	{"enrolled raw holder, enforcing", true, memberActor(rbac.PermManageChannels, true), serverScope, rbac.PermManageChannels, false, false},
	{"enrolled raw Administrator, enforcing", true, memberActor(rbac.PermAdministrator, true), serverScope, rbac.PermBan, false, false},
	{"unenrolled holder of another dangerous bit, enforcing", true, memberActor(rbac.PermManageChannels, false), serverScope, rbac.PermBan, false, false},
	{"unenrolled plain member, enforcing", true, memberActor(rbac.PermSendMessages, false), serverScope, rbac.PermManageChannels, false, false},
	{"non-member, enforcing", true, nonMemberActor, serverScope, rbac.PermManageChannels, false, false},
	{"vanished server", true, memberActor(rbac.PermManageChannels, false), serverScope, rbac.PermManageChannels, true, false},

	// X18 at server scope: a channel ALLOW grants nothing here.
	{"channel ALLOW only, at server scope", true, channelAllowActor(false), serverScope, rbac.PermManageAllMessages, false, false},
	// The role grant roleDenyActor's channel DENY removes is real.
	{"role grant denied on a channel, at server scope", true, roleDenyActor, serverScope, rbac.PermManageAllMessages, false, true},
}

var channelScopeCases = []rs5Case{
	// X18: a dangerous grant that exists only as a channel ALLOW.
	{"channel ALLOW only, at that channel", true, channelAllowActor(false), scopedChannel, rbac.PermManageAllMessages, false, true},
	{"channel ALLOW only, at another channel", true, channelAllowActor(false), otherChannel, rbac.PermManageAllMessages, false, false},
	{"channel ALLOW only, not enforcing", false, channelAllowActor(false), scopedChannel, rbac.PermManageAllMessages, false, false},
	{"channel ALLOW only, enrolled", true, channelAllowActor(true), scopedChannel, rbac.PermManageAllMessages, false, false},

	// A channel DENY removes a role grant at that channel only.
	{"role grant denied on that channel", true, roleDenyActor, scopedChannel, rbac.PermManageAllMessages, false, false},
	{"role grant denied on a channel, at another channel", true, roleDenyActor, otherChannel, rbac.PermManageAllMessages, false, true},

	// The bypasses read raw state, as the resolver's do.
	{"raw Administrator, channel DENY of the bit and of bit 62", true, adminDenyActor, scopedChannel, rbac.PermManageAllMessages, false, true},
	{"owner, channel DENY", true, ownerDenyActor, scopedChannel, rbac.PermManageAllMessages, false, true},

	// A channel of another server is a non-member denial no factor lifts.
	{"raw holder, channel of another server", true, memberActor(rbac.PermManageAllMessages, false), foreignChannel, rbac.PermManageAllMessages, false, false},
	{"non-member, at a channel", true, nonMemberActor, scopedChannel, rbac.PermManageAllMessages, false, false},
	{"vanished server, at a channel", true, channelAllowActor(false), scopedChannel, rbac.PermManageAllMessages, true, false},
}

func runRS5Case(t *testing.T, ts *testhelpers.TestServer, tc rs5Case) (*stepup.Error, []string) {
	t.Helper()
	f := newRS5Fixture(t, ts, tc.enforcing)
	actorID := tc.actor(t, f)
	if tc.vanished {
		_, err := ts.DB.Exec(`DELETE FROM servers WHERE id = $1`, f.serverID)
		require.NoError(t, err)
	}
	// On the caller's transaction, as an in-transaction denial site runs it.
	// Ended here rather than at test cleanup: a test running many cases
	// would otherwise pin one pooled connection per case and starve the pool.
	tx, err := ts.DB.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	q := &recordingQuerier{inner: tx, failAt: -1}
	e := rbac.EnrollmentDenial(context.Background(), q, f.serverID, tc.scope(f), actorID, tc.requiredBit)
	return e, q.statements
}

// Every arm of the predicate at both scopes, and the parity the false arms
// owe: a member the mask is not restricting gets the route's generic 403,
// byte for byte, on an enforcing server and on one that does not enforce.
//
// Kills: each conjunct dropped (enforcing, unenrolled, in scope, the owner
// arm, the raw bit, raw Administrator); a raw read that ignores the required
// bit; channel scope reading the server-scope bits (X18's ALLOW and the
// channel DENY); overrides applied to raw Administrator.
func TestEnrollmentDenial_Verdicts(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	genericCode, genericBody := writeDenial(nil)
	enrollCode, enrollBody := writeDenial(stepup.EnrollmentRequired())

	for _, tc := range slices.Concat(serverScopeCases, channelScopeCases) {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := runRS5Case(t, ts, tc)
			code, body := writeDenial(e)
			if tc.want {
				require.NotNil(t, e)
				assert.Equal(t, enrollCode, code)
				assert.JSONEq(t, enrollBody, body)
				return
			}
			assert.Nil(t, e, "a member the mask is not restricting must keep the route's own denial")
			assert.Equal(t, genericCode, code)
			assert.JSONEq(t, genericBody, body)
		})
	}
}

// Constant shape (A.7): a dangerous required bit runs the same statements on
// every denial of one scope, so the work cannot tell an enforcing server from
// one that is not, an owner from a member, an enrolled actor from an
// unenrolled one, or an Administrator from a member whose overrides apply.
//
// Kills: an early return on !enforcing, on the owner, on enrollment, on raw
// Administrator, or on a channel outside the server.
func TestEnrollmentDenial_ConstantShape(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, scope := range []struct {
		name  string
		cases []rs5Case
		// statements is what one denial of that scope reads.
		statements int
	}{
		{"server scope", serverScopeCases, 3},
		{"channel scope", channelScopeCases, 5},
	} {
		t.Run(scope.name, func(t *testing.T) {
			var reference []string
			for _, tc := range scope.cases {
				_, statements := runRS5Case(t, ts, tc)
				if reference == nil {
					reference = statements
					require.Len(t, reference, scope.statements)
					continue
				}
				assert.Equal(t, reference, statements, "%s: statement trace differs", tc.name)
			}
		})
	}
}

// A bit the mask never withholds cannot be a denial the mask caused, so it is
// answered without a statement, even for an unenrolled holder on an enforcing
// server. The branch depends only on the caller's constant.
//
// Kills: the dangerous-bit guard removed (this case answers enrollment).
func TestEnrollmentDenial_NonDangerousBitKeepsGenericDenial(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, scope := range []rs5Scope{serverScope, scopedChannel} {
		tc := rs5Case{
			name: "unenrolled raw holder of a safe bit", enforcing: true, scope: scope,
			actor: memberActor(rbac.PermSendMessages, false), requiredBit: rbac.PermSendMessages,
		}
		e, statements := runRS5Case(t, ts, tc)
		assert.Nil(t, e)
		assert.Empty(t, statements)
	}
}

// A pooled querier works too, for a denial made before any transaction.
func TestEnrollmentDenial_PooledQuerier(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	f := newRS5Fixture(t, ts, true)
	for _, channelID := range []string{"", f.channelID} {
		e := rbac.EnrollmentDenial(context.Background(), ts.DB, f.serverID, channelID, f.ownerID, rbac.PermManageServer)
		require.NotNil(t, e)
		assert.Equal(t, true, e.Body["mfa_enrollment_required"])
	}
}

// A read failure at ANY statement is a 500 with Cause for the caller's error
// writer to log, never the enrollment body and never a silent nil. The actor
// is one the verdict would otherwise answer with enrollment, so a failure
// swallowed into "no overrides" or "not enforcing" would show as a 403.
//
// Kills: any read error degraded to a value (including a channel-scope read
// treated as "no override" or "not in the server").
func TestEnrollmentDenial_ReadFailureIs500(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	f := newRS5Fixture(t, ts, true)
	actorID := channelAllowActor(false)(t, f)

	assertReadFailure := func(t *testing.T, e *stepup.Error) {
		t.Helper()
		require.NotNil(t, e)
		assert.Equal(t, http.StatusInternalServerError, e.Status)
		assert.Equal(t, stepup.ErrMsgVerificationFailed, e.Body["error"])
		assert.Error(t, e.Cause)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertReadFailure(t, rbac.EnrollmentDenial(ctx, ts.DB, f.serverID, f.channelID, actorID, rbac.PermManageAllMessages))

	for failAt := range 5 {
		q := &recordingQuerier{inner: ts.DB, failAt: failAt}
		e := rbac.EnrollmentDenial(context.Background(), q, f.serverID, f.channelID, actorID, rbac.PermManageAllMessages)
		assertReadFailure(t, e)
		assert.Len(t, q.statements, failAt+1, "statement %d: a failed read must stop the denial", failAt)
	}
}
