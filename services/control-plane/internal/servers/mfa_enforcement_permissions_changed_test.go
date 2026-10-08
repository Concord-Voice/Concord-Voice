package servers_test

// #3456: the permission-refresh events, driven over a real WebSocket through
// the real router. Each test names the mutant that kills it.
//
//   - server_permissions_changed: PUT /servers/:id/mfa-enforcement, through
//     refreshMFAEnforcementViews, to the server's subscribers.
//   - permissions_changed: a committed inline-factor change, through the
//     notifier internal/api wires into the MFA handler, to the actor only.
//
// The Go sides of the ordering and the exact bytes are pinned here and in
// internal/api/permission_change_notifier_test.go; the TypeScript side parses
// the same literals in tests/unit/types/ws-events.test.ts.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lib/pq"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

const (
	serverPermsChangedType = "server_permissions_changed"
	userPermsChangedType   = "permissions_changed"
	// frameQuiet is how long a connection must stay silent before a drain
	// concludes that no further frame is coming. Every emission under test is
	// admitted to the hub before the PUT answers, so this only bounds the hub's
	// own dispatch latency; a missing frame is never what waits on it.
	frameQuiet = 400 * time.Millisecond
	// frameWait bounds the wait for a frame that MUST arrive.
	frameWait = 5 * time.Second
)

// permFrames reads a subscribed WebSocket on a goroutine and hands the raw
// text of every frame to the test, so a test can count frames and compare their
// exact bytes without racing the read deadline.
type permFrames struct {
	frames chan string
}

// dialPermFrames connects as u, subscribes to each server and returns once the
// subscriptions are in place (the malformed-subscribe barrier of
// TestMFAEnforcement_ValueAppearsInNoServerPayload: the hub handles one
// client's frames in order, so the error reply proves the earlier subscribes
// already ran). Frames already queued are discarded.
func dialPermFrames(t *testing.T, ts *testhelpers.TestServer, u testhelpers.TestUser, serverIDs ...string) *permFrames {
	t.Helper()
	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+u.AccessToken)
	client, _, err := websocket.DefaultDialer.Dial("ws"+wsServer.URL[len("http"):]+"/api/v1/ws", headers)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	require.NoError(t, client.SetReadDeadline(time.Now().Add(frameWait)))
	for _, id := range serverIDs {
		require.NoError(t, client.WriteJSON(map[string]any{
			"type": "subscribe_server", "data": map[string]any{"server_id": id},
		}))
	}
	require.NoError(t, client.WriteJSON(map[string]any{
		"type": "subscribe_server", "data": map[string]any{"server_id": "not-a-uuid"},
	}))
	for {
		var frame map[string]any
		require.NoError(t, client.ReadJSON(&frame), "the subscription barrier must answer")
		if frame["type"] == "error" {
			break
		}
	}
	require.NoError(t, client.SetReadDeadline(time.Time{}))

	pf := &permFrames{frames: make(chan string, 64)}
	go func() {
		for {
			_, raw, err := client.ReadMessage()
			if err != nil {
				close(pf.frames)
				return
			}
			select {
			case pf.frames <- string(raw):
			default: // a runaway sender trips the count assertions instead of blocking
			}
		}
	}()
	return pf
}

// frameType returns the "type" of a raw frame, or "" if it is not an object.
func frameType(raw string) string {
	var f struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(raw), &f)
	return f.Type
}

// drain returns the raw text of every frame of the given type that arrives
// before the connection has been quiet for frameQuiet.
func (p *permFrames) drain(typ string) []string {
	var out []string
	timer := time.NewTimer(frameQuiet)
	defer timer.Stop()
	for {
		select {
		case raw, ok := <-p.frames:
			if !ok {
				return out
			}
			if frameType(raw) == typ {
				out = append(out, raw)
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(frameQuiet)
		case <-timer.C:
			return out
		}
	}
}

// next waits for one frame of the given type and returns it.
func (p *permFrames) next(t *testing.T, typ string) string {
	t.Helper()
	deadline := time.After(frameWait)
	for {
		select {
		case raw, ok := <-p.frames:
			require.True(t, ok, "the connection closed before a %s frame arrived", typ)
			if frameType(raw) == typ {
				return raw
			}
		case <-deadline:
			require.FailNow(t, "no frame arrived", "type %s", typ)
		}
	}
}

// expectOne asserts that exactly one frame of the given type arrives and that
// its bytes are want. The frame MUST arrive, so it is awaited for frameWait
// rather than inferred from silence: a drain alone would return after frameQuiet
// and fail a correct but late frame on a loaded runner. Only the absence of a
// SECOND frame is judged by the quiet window.
func (p *permFrames) expectOne(t *testing.T, typ, want, msg string) {
	t.Helper()
	assert.Equal(t, want, p.next(t, typ), msg)
	assert.Empty(t, p.drain(typ), "%s: no second frame may follow", msg)
}

// serverFrame is the exact wire text of server_permissions_changed.
func serverFrame(serverID string) string {
	return `{"type":"server_permissions_changed","data":{"server_id":"` + serverID + `"}}`
}

// The frame's bytes are the contract with ServerPermissionsChangedSchema, and
// the set of changed PUTs is the contract with the desktop's refresh: one frame
// for each committed change, in either direction, and the two directions are
// byte-identical (the frame says THAT the setting may have changed, never which
// way).
//
// Kills: the announcement removed from refreshMFAEnforcementViews (no frame),
// sent twice (two frames), a key added to Data (a direction or an enrollment
// flag), the type renamed, and the server id spelled other than canonically.
func TestServerPermissionsChanged_OneFramePerChangedPut(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "spc1", false)
	enrollMFATOTP(t, env, f.owner.ID)
	frames := dialPermFrames(t, env.ts, f.member, f.serverID)
	want := serverFrame(f.serverID)

	on := putMFA(env, f.owner, f.serverID, bodyOn())
	require.Equal(t, http.StatusOK, on.Code, on.Body.String())
	frames.expectOne(t, serverPermsChangedType, want, "an ON flip emits exactly one frame")

	off := putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	require.Equal(t, http.StatusOK, off.Code, off.Body.String())
	frames.expectOne(t, serverPermsChangedType, want,
		"an OFF flip emits exactly one frame, byte-identical to the ON frame")

	// The request spells the id in capitals; the frame must not. The URL
	// spelling is not the wire's: the server re-renders the parsed id.
	upper := strings.ToUpper(f.serverID)
	require.NotEqual(t, f.serverID, upper)
	again := putMFA(env, f.owner, upper, bodyOn())
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	frames.expectOne(t, serverPermsChangedType, want,
		"the frame carries the lowercase canonical uuid whatever spelling the URL used")
}

// A request that changes nothing, or is refused, must not make every member
// re-read: each such frame is a cold permission resolve per member.
//
// Kills: the announcement moved out of the changed-only path (the no-op or any
// refusal then emits), and the announcement moved above the authorization or
// the step-up check (the refusals emit). The control at the end proves the
// harness would have seen a frame.
func TestServerPermissionsChanged_NoFrameForNoOpOrRefusal(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "spc2", false)
	frames := dialPermFrames(t, env.ts, f.member, f.serverID)

	// The owner is not enrolled yet: turning ON is refused.
	w := putMFA(env, f.owner, f.serverID, bodyOn())
	require.Equal(t, http.StatusForbidden, w.Code, "an unenrolled owner is refused: %s", w.Body.String())
	// A plain member is not permitted to manage the setting.
	w = putMFA(env, f.member, f.serverID, bodyOn())
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	// Malformed body.
	w = putMFARaw(env, f.owner, f.serverID, `{"enabled":"yes"}`)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// Enrol the owner on an enforcing server: ON again changes nothing, and
	// OFF is refused without a code and with a wrong one.
	enrollMFATOTP(t, env, f.owner.ID)
	setMFAFlag(t, env, f.serverID, true)
	w = putMFA(env, f.owner, f.serverID, bodyOn())
	require.Equal(t, http.StatusOK, w.Code, "ON while ON is a no-op, not an error: %s", w.Body.String())
	w = putMFA(env, f.owner, f.serverID, bodyOffNoCode())
	require.NotEqual(t, http.StatusOK, w.Code, "OFF without a code is refused: %s", w.Body.String())
	w = putMFA(env, f.owner, f.serverID, bodyOff(mfaWrongCode))
	require.NotEqual(t, http.StatusOK, w.Code, "OFF with a wrong code is refused: %s", w.Body.String())
	require.True(t, readMFAFlag(t, env, f.serverID), "setup: nothing above changed the setting")

	assert.Empty(t, frames.drain(serverPermsChangedType), "no frame for a no-op or a refusal")

	// Control: a real change on the same connection is seen.
	w = putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	frames.expectOne(t, serverPermsChangedType, serverFrame(f.serverID), "exactly one frame")
}

// A Commit() error does not prove a rollback (spec correction C-3), so a PUT
// whose UPDATE ran refreshes every member's view whatever Commit returned: the
// change may have applied, and the members' caches are stale if it did. The
// frame, like the bump, is sent; the Nightwatch success event is not (covered
// by TestMFAEnforcement_LostCommitAckStillRefreshesPermissions).
//
// Kills: the announcement skipped on the commit-error path (the lost-ack arm
// sees no frame), sent from both the error path and the clean path (two
// frames), and made unconditional (the no-UPDATE arm sees a frame).
func TestServerPermissionsChanged_CommitErrorPaths(t *testing.T) {
	t.Run("a lost commit acknowledgement still announces once", func(t *testing.T) {
		env := setupMFAEnforcementEnv(t)
		f := newMFAFixture(t, env, "spc3", false)
		enrollMFATOTP(t, env, f.owner.ID)
		frames := dialPermFrames(t, env.ts, f.member, f.serverID)
		servers.SetMFAEnforcementCommitForTest(t, func(tx *sql.Tx) error {
			if err := tx.Commit(); err != nil {
				return err
			}
			return errors.New("simulated lost commit acknowledgement")
		})

		w := putMFA(env, f.owner, f.serverID, bodyOn())
		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		require.True(t, readMFAFlag(t, env, f.serverID), "setup: the commit applied")
		frames.expectOne(t, serverPermsChangedType, serverFrame(f.serverID), "exactly one frame")
	})

	t.Run("a commit that really rolled back also announces once", func(t *testing.T) {
		env := setupMFAEnforcementEnv(t)
		f := newMFAFixture(t, env, "spc4", false)
		enrollMFATOTP(t, env, f.owner.ID)
		frames := dialPermFrames(t, env.ts, f.member, f.serverID)
		servers.SetMFAEnforcementCommitForTest(t, func(tx *sql.Tx) error {
			_, poisonErr := tx.Exec(`SELECT 1/0`)
			require.Error(t, poisonErr)
			return tx.Commit()
		})

		w := putMFA(env, f.owner, f.serverID, bodyOn())
		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		require.False(t, readMFAFlag(t, env, f.serverID), "setup: the commit rolled back")
		frames.expectOne(t, serverPermsChangedType, serverFrame(f.serverID),
			"the handler cannot tell a rollback from a lost ack, so it announces in both")
	})

	t.Run("a commit error after no UPDATE announces nothing", func(t *testing.T) {
		env := setupMFAEnforcementEnv(t)
		f := newMFAFixture(t, env, "spc5", true)
		enrollMFATOTP(t, env, f.owner.ID)
		frames := dialPermFrames(t, env.ts, f.member, f.serverID)
		servers.SetMFAEnforcementCommitForTest(t, func(tx *sql.Tx) error {
			if err := tx.Commit(); err != nil {
				return err
			}
			return errors.New("simulated lost commit acknowledgement")
		})

		w := putMFA(env, f.owner, f.serverID, bodyOn()) // already ON: no UPDATE
		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.Empty(t, frames.drain(serverPermsChangedType), "no UPDATE ran, so nothing may have changed")
	})
}

// The bump runs before the announcement: a member that re-read on the frame
// before the bump could be served, or could recompute and cache, the value the
// bump is about to retire.
//
// The test stalls the bump deterministically. The commit seam commits for
// real and then pauses every Redis write for a fixed window, so the bump's
// write cannot complete until the window ends. If the bump precedes the
// announcement, no frame can reach the member before then; if it follows, the
// frame arrives within milliseconds. The assertion is a lower bound only, so
// it cannot flake on a slow machine.
//
// Kills: the announcement moved above the bump in refreshMFAEnforcementViews.
func TestServerPermissionsChanged_FollowsTheBump(t *testing.T) {
	const pause = 800 * time.Millisecond
	const lowerBound = 600 * time.Millisecond

	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "spc6", false)
	enrollMFATOTP(t, env, f.owner.ID)
	frames := dialPermFrames(t, env.ts, f.member, f.serverID)

	var committedAt time.Time
	servers.SetMFAEnforcementCommitForTest(t, func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		committedAt = time.Now()
		return env.ts.Redis.Do(context.Background(), "CLIENT", "PAUSE", pause.Milliseconds(), "WRITE").Err()
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- putMFA(env, f.owner, f.serverID, bodyOn()) }()

	got := frames.next(t, serverPermsChangedType)
	arrived := time.Now()
	assert.Equal(t, serverFrame(f.serverID), got)
	require.False(t, committedAt.IsZero(), "the commit seam must have run before the frame")
	assert.GreaterOrEqual(t, arrived.Sub(committedAt), lowerBound,
		"the frame reached the member while the bump was still blocked: the announcement ran before the bump")

	select {
	case w := <-done:
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	case <-time.After(frameWait):
		require.FailNow(t, "the PUT never answered")
	}
}

var uuidInLog = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// A frame the hub refuses (here: a hub that has shut down) is logged ONCE as a
// failure class with nothing else on the line (I7): no server id, no actor id,
// no direction. The PUT itself still succeeds, because the change committed.
//
// Kills: the false return ignored (no line), the line logged twice, the message
// or the class renamed, and any field added to the line (the server id, the actor, the
// direction, the error text).
func TestServerPermissionsChanged_RefusedBroadcastLogsOneAnonymousLine(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "spc7", false)
	enrollMFATOTP(t, env, f.owner.ID)
	env.ts.Hub.Shutdown()
	env.logs.Reset()

	w := putMFA(env, f.owner, f.serverID, bodyOn())
	require.Equal(t, http.StatusOK, w.Code,
		"the committed change must not fail on a refused broadcast: %s", w.Body.String())
	require.True(t, readMFAFlag(t, env, f.serverID))

	var lines []string
	for _, line := range strings.Split(env.logs.String(), "\n") {
		if strings.Contains(line, "failure_class=perm_change_broadcast") {
			lines = append(lines, line)
		}
	}
	require.Len(t, lines, 1, "exactly one failure line: %q", env.logs.String())
	assert.Contains(t, lines[0], `msg="Failed to announce a permission change"`,
		"the message is byte-identical to the user event's in internal/api: %q", lines[0])
	assert.NotContains(t, lines[0], f.serverID)
	assert.NotContains(t, lines[0], f.owner.ID)
	assert.False(t, uuidInLog.MatchString(lines[0]), "the line must carry no identifier: %q", lines[0])
	fields := regexp.MustCompile(`\b[a-z_]+=`).FindAllString(lines[0], -1)
	assert.ElementsMatch(t, []string{"time=", "level=", "msg=", "failure_class="}, fields,
		"the line may carry the class and nothing else: %q", lines[0])
}

// The user event reaches the actor's own connections through the router's real
// wiring, and nobody else's. The frame's bytes are the contract with
// PermissionsChangedSchema.
//
// Kills: SetPermissionChangeNotifier dropped from the router (the actor sees no
// frame), the frame sent to the server's subscribers instead of the actor (the
// bystander sees it), sent twice, "data":null instead of {}, and the event
// sent for a verification that did not commit.
func TestPermissionsChanged_ReachesOnlyTheActor(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "pch1", false)
	actor := dialPermFrames(t, env.ts, f.owner, f.serverID)
	bystander := dialPermFrames(t, env.ts, f.member, f.serverID)

	secret := seedPendingTOTP(t, env, f.owner.ID)
	verify := func(code string) *httptest.ResponseRecorder {
		return env.ts.DoRequest(http.MethodPost, "/api/v1/mfa/totp/verify-setup",
			map[string]any{"code": code}, testhelpers.AuthHeaders(f.owner.AccessToken))
	}

	// A wrong code commits nothing and announces nothing.
	wrong := "000000"
	if valid, err := totp.GenerateCode(secret, time.Now()); err == nil && valid == wrong {
		wrong = "111111"
	}
	w := verify(wrong)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Empty(t, actor.drain(userPermsChangedType), "a refused verification must not announce")

	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	w = verify(code)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	actor.expectOne(t, userPermsChangedType, `{"type":"permissions_changed","data":{}}`,
		"the actor gets exactly one frame, with an empty object body")
	assert.Empty(t, bystander.drain(userPermsChangedType), "no one else may be told")
}

// seedPendingTOTP inserts the row TOTPSetup leaves behind (enabled and
// confirmed FALSE, awaiting verify-setup), sealed under the router's keyring,
// and returns its secret.
func seedPendingTOTP(t *testing.T, env *mfaEnv, userID string) string {
	t.Helper()
	ring, err := mfa.ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	key, err := mfa.GenerateSecret(userID + "@permissions-changed.test")
	require.NoError(t, err)
	enc, nonce, version, err := ring.Seal([]byte(key.Secret()))
	require.NoError(t, err)
	_, err = env.ts.DB.Exec(`INSERT INTO user_mfa_totp
		(user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed, backup_codes_hash, backup_codes_used)
		VALUES ($1, $2, $3, $4, FALSE, FALSE, $5, $6)`,
		userID, enc, nonce, version, pq.Array([]string{}), pq.Array([]bool{}))
	require.NoError(t, err)
	return key.Secret()
}
