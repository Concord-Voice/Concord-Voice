package members_test

// Codex on #3508: getServerOwnerID read the servers row with a contextless
// h.db.QueryRow. That covers the preflight owner reads and, since 26e3125, the
// re-read before a moderation refusal stands. A request whose client had gone
// still waited on the database for a connection, which pool saturation makes
// indefinite, and the re-read runs on exactly the refusal and error paths.
//
// Each case sends its request under a context carrying a marker and hooks one
// owner read; the read must run under that context.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

type requestMarker struct{}

func doRequestUnder(ctx context.Context, ts *testhelpers.TestServer, method, path string, body any, token string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader).WithContext(ctx)
	req.Header = testhelpers.AuthHeaders(token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	return w
}

func TestModerationOwnerReads_RunUnderTheRequestContext(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	member := func(f moderationFixture) string {
		return "/api/v1/servers/" + f.serverID + "/members/" + f.target.ID
	}
	moderator := func(_ *testing.T, f moderationFixture) string { return f.actor.AccessToken }
	cases := []struct {
		name     string
		sequence []string
		arrange  func(t *testing.T, f moderationFixture) string
		method   string
		path     func(f moderationFixture) string
		body     any
	}{
		{name: "UpdateMember preflight", sequence: []string{pooledOwnerRead}, arrange: moderator,
			method: http.MethodPatch, path: member, body: map[string]string{"role": "admin"}},
		{name: "TimeoutMember preflight", sequence: []string{pooledOwnerRead}, arrange: moderator,
			method: http.MethodPost, path: func(f moderationFixture) string { return member(f) + "/timeout" },
			body: map[string]int64{"duration_seconds": 600}},
		{name: "RemoveMember preflight", sequence: []string{pooledOwnerRead}, arrange: moderator,
			method: http.MethodDelete, path: member},
		{name: "BanMember preflight", sequence: []string{pooledOwnerRead}, arrange: moderator,
			method: http.MethodPost, path: func(f moderationFixture) string {
				return "/api/v1/servers/" + f.serverID + "/bans/" + f.target.ID
			}},
		{
			// Positive control: CheckHierarchy's own owner read already runs under
			// the request's context, so the marker survives the middleware chain.
			name: "control: CheckHierarchy's owner read", sequence: []string{pooledOwnerRead, hierarchyOwnerRead},
			arrange: moderator,
			method:  http.MethodPost, path: func(f moderationFixture) string { return member(f) + "/timeout" },
			body: map[string]int64{"duration_seconds": 600},
		},
		{name: "RemoveMember re-read after a refusal", sequence: []string{pooledOwnerRead, pooledOwnerRead},
			arrange: func(t *testing.T, f moderationFixture) string {
				bystander := ts.CreateTestUser(t, "modcb"+uuid.NewString()[:8])
				ts.AddMemberToServer(t, f.serverID, bystander.ID, "member")
				return bystander.AccessToken
			},
			method: http.MethodDelete, path: member},
		{name: "TimeoutMember re-read after a violation", sequence: []string{pooledOwnerRead, hierarchyOwnerRead, pooledOwnerRead},
			arrange: func(t *testing.T, f moderationFixture) string {
				senior := ts.CreateTestRole(t, f.serverID, "senior", 20, 0)
				ts.AssignRoleToUser(t, f.serverID, f.target.ID, senior)
				return f.actor.AccessToken
			},
			method: http.MethodPost, path: func(f moderationFixture) string { return member(f) + "/timeout" },
			body: map[string]int64{"duration_seconds": 600}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newModerationFixture(t, ts)
			token := tc.arrange(t, f)
			ctx := context.WithValue(context.Background(), requestMarker{}, tc.name)
			hook.Arm(tc.sequence, nil, nil)
			var marker any
			hook.Observe(func(fired context.Context) { marker = fired.Value(requestMarker{}) })

			doRequestUnder(ctx, ts, tc.method, tc.path(f), tc.body, token)

			seen, _ := hook.Report()
			require.Equal(t, len(tc.sequence), seen, "the hook must reach the owner read")
			assert.Equal(t, tc.name, marker,
				"the owner read must run under the request's context, so a client that has gone stops it")
		})
	}
}
