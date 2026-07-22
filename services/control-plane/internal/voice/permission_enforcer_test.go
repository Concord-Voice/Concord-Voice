package voice_test

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enforcerTestRig wires a PermissionEnforcer against the integration DB and a
// live NATS pair (publisher + observer), mirroring nats_disconnect_test.go.
// Skips the calling test when NATS is unreachable (runs in CI / dev env).
type enforcerTestRig struct {
	ts        *testhelpers.TestServer
	enforcer  *voice.PermissionEnforcer
	resolver  *rbac.Resolver
	publisher *natsclient.Client
	perms     chan map[string]interface{}
	disc      chan map[string]interface{}
}

func setupEnforcerRig(t *testing.T) *enforcerTestRig {
	t.Helper()
	pubClient, err := natsclient.Connect(natsTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live enforcer test (runs in CI)", err)
	}
	t.Cleanup(pubClient.Close)

	obsClient, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(obsClient.Close)

	ts := testhelpers.SetupTestServer(t)
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	enforcer := voice.NewPermissionEnforcer(ts.DB, logger.New("test"), resolver, pubClient)

	perms := make(chan map[string]interface{}, 8)
	permsSub, err := obsClient.Subscribe("voice.enforce.permissions", func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) == nil {
			perms <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = permsSub.Unsubscribe() })

	disc := make(chan map[string]interface{}, 8)
	discSub, err := obsClient.Subscribe("voice.enforce.disconnect", func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) == nil {
			disc <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = discSub.Unsubscribe() })

	// Flush so subscription interest reaches the server before any publish
	// (mirrors the race note in nats_disconnect_test.go).
	require.NoError(t, obsClient.Flush())

	return &enforcerTestRig{
		ts: ts, enforcer: enforcer, resolver: resolver, publisher: pubClient,
		perms: perms, disc: disc,
	}
}

func (r *enforcerTestRig) addVoiceParticipant(t *testing.T, channelID, userID string) {
	t.Helper()
	_, err := r.ts.DB.Exec(
		`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, userID)
	require.NoError(t, err)
}

// waitPayloadFor returns the first payload whose userId is in want, draining
// any non-matching payloads (a prior test's async publish can still be in
// flight on these shared subjects).
func waitPayloadFor(t *testing.T, ch chan map[string]interface{}, what string, want map[string]bool) map[string]interface{} {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case p := <-ch:
			if userID, _ := p["userId"].(string); want[userID] {
				return p
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s message", what)
			return nil
		}
	}
}

func assertNoPayload(t *testing.T, ch chan map[string]interface{}, what string) {
	t.Helper()
	select {
	case p := <-ch:
		t.Fatalf("expected no %s message, got %v", what, p)
	case <-time.After(500 * time.Millisecond):
	}
}

func assertNoEnforcementFor(
	t *testing.T,
	perms, disconnects <-chan map[string]interface{},
	userID string,
) {
	t.Helper()
	deadline := time.NewTimer(750 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case payload := <-perms:
			if payload["userId"] == userID {
				t.Fatalf("expected no permission push for %s, got %v", userID, payload)
			}
		case payload := <-disconnects:
			if payload["userId"] == userID {
				t.Fatalf("expected no disconnect for %s, got %v", userID, payload)
			}
		case <-deadline.C:
			return
		}
	}
}

func openEnforcerMonitorDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, db.PingContext(t.Context()))
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func lockEnforcerTable(t *testing.T, db *sql.DB, table string) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	switch table {
	case "server_members":
		_, err = tx.ExecContext(t.Context(), `LOCK TABLE server_members IN ACCESS EXCLUSIVE MODE`)
	case "servers":
		_, err = tx.ExecContext(t.Context(), `LOCK TABLE servers IN ACCESS EXCLUSIVE MODE`)
	default:
		t.Fatalf("unsupported enforcer test table %q", table)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func waitForEnforcerQuery(
	t *testing.T,
	db *sql.DB,
	state, waitEventType, queryPattern string,
) {
	t.Helper()
	require.Eventually(t, func() bool {
		var found bool
		err := db.QueryRowContext(t.Context(), `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE datname = current_database()
				  AND pid <> pg_backend_pid()
				  AND state = $1
				  AND ($2 = '' OR COALESCE(wait_event_type, '') = $2)
				  AND query LIKE $3
			)
		`, state, waitEventType, "%"+queryPattern+"%").Scan(&found)
		return err == nil && found
	}, 3*time.Second, 10*time.Millisecond)
}

func waitForPermissionWithoutDisconnect(
	t *testing.T,
	perms, disconnects <-chan map[string]interface{},
	userID string,
) map[string]interface{} {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case payload := <-perms:
			if payload["userId"] == userID {
				return payload
			}
		case payload := <-disconnects:
			if payload["userId"] == userID {
				t.Fatalf("received stale disconnect for cleared timeout: %v", payload)
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for permission push for %s", userID)
		}
	}
}

// TestPermissionEnforcer_RecheckUser_PublishesRecomputedBitfield verifies the
// happy path: a voice-connected member's fresh effective bitfield is published
// on voice.enforce.permissions in the decimal-string wire format.
func TestPermissionEnforcer_RecheckUser_PublishesRecomputedBitfield(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peowner1")
	member := r.ts.CreateTestUser(t, "pemember1")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce User")
	r.ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice")
	r.addVoiceParticipant(t, channelID, member.ID)

	r.enforcer.RecheckUser(serverID, member.ID)

	payload := waitPayloadFor(t, r.perms, "voice.enforce.permissions", map[string]bool{member.ID: true})
	assert.Equal(t, channelID, payload["channelId"])
	assert.Equal(t, member.ID, payload["userId"])

	expected, err := r.resolver.GetEffectivePermissions(t.Context(), serverID, member.ID, channelID)
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(int64(expected), 10), payload["permissions"],
		"published bitfield must equal the freshly resolved effective permissions")
}

// TestPermissionEnforcer_RecheckUser_NotMemberPublishesDisconnect verifies the
// membership-loss branch: a voice participant who is no longer a server member
// is evicted via voice.enforce.disconnect, not left with a stale snapshot.
func TestPermissionEnforcer_RecheckUser_NotMemberPublishesDisconnect(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peowner2")
	outsider := r.ts.CreateTestUser(t, "peoutsider2")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce NotMember")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-nm")
	// In the room but NOT a server member (e.g. kicked between join and recheck).
	r.addVoiceParticipant(t, channelID, outsider.ID)

	r.enforcer.RecheckUser(serverID, outsider.ID)

	payload := waitPayloadFor(t, r.disc, "voice.enforce.disconnect", map[string]bool{outsider.ID: true})
	assert.Equal(t, channelID, payload["channelId"])
	assert.Equal(t, outsider.ID, payload["userId"])
	assertNoPayload(t, r.perms, "voice.enforce.permissions")
}

// TestPermissionEnforcer_RecheckChannel_PublishesForEachParticipant verifies
// channel scope fans out to every member currently in that voice channel.
func TestPermissionEnforcer_RecheckChannel_PublishesForEachParticipant(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peowner3")
	m1 := r.ts.CreateTestUser(t, "pemember3a")
	m2 := r.ts.CreateTestUser(t, "pemember3b")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Channel")
	r.ts.AddMemberToServer(t, serverID, m1.ID, "member")
	r.ts.AddMemberToServer(t, serverID, m2.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-ch")
	r.addVoiceParticipant(t, channelID, m1.ID)
	r.addVoiceParticipant(t, channelID, m2.ID)

	r.enforcer.RecheckChannel(serverID, channelID)

	want := map[string]bool{m1.ID: true, m2.ID: true}
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		payload := waitPayloadFor(t, r.perms, "voice.enforce.permissions", want)
		assert.Equal(t, channelID, payload["channelId"])
		userID, _ := payload["userId"].(string)
		got[userID] = true
	}
	assert.True(t, got[m1.ID], "member 1 should receive a recheck")
	assert.True(t, got[m2.ID], "member 2 should receive a recheck")
}

func TestPermissionEnforcer_RecheckParticipants_BatchesAuthoritativeHeartbeatSet(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peheartbeatowner")
	member := r.ts.CreateTestUser(t, "peheartbeatmember")
	timedOut := r.ts.CreateTestUser(t, "peheartbeattimedout")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Heartbeat")
	r.ts.AddMemberToServer(t, serverID, member.ID, "member")
	r.ts.AddMemberToServer(t, serverID, timedOut.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-heartbeat")
	_, err := r.ts.DB.Exec(`
		UPDATE server_members
		SET timed_out_until = NOW() + INTERVAL '1 hour'
		WHERE server_id = $1 AND user_id = $2
	`, serverID, timedOut.ID)
	require.NoError(t, err)

	r.enforcer.RecheckParticipants(serverID, channelID, []uuid.UUID{
		uuid.MustParse(member.ID), uuid.MustParse(timedOut.ID),
	})

	permission := waitPayloadFor(
		t, r.perms, "voice.enforce.permissions", map[string]bool{member.ID: true},
	)
	assert.Equal(t, channelID, permission["channelId"])
	disconnect := waitPayloadFor(
		t, r.disc, "voice.enforce.disconnect", map[string]bool{timedOut.ID: true},
	)
	assert.Equal(t, channelID, disconnect["channelId"])
}

func TestPermissionEnforcer_RecheckParticipants_TimeoutReadErrorPublishesNothing(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peheartbeaterrorowner")
	member := r.ts.CreateTestUser(t, "peheartbeaterrormember")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Heartbeat Error")
	r.ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-heartbeat-error")

	closedDB, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, closedDB.Close())
	brokenTimeoutReader := voice.NewPermissionEnforcer(
		closedDB, logger.New("test"), r.resolver, r.publisher,
	)
	brokenTimeoutReader.RecheckParticipants(
		serverID, channelID, []uuid.UUID{uuid.MustParse(member.ID)},
	)

	assertNoEnforcementFor(t, r.perms, r.disc, member.ID)
}

func TestPermissionEnforcer_RecheckParticipants_RequeuedChannelDoesNotStarvePendingChannel(
	t *testing.T,
) {
	r := setupEnforcerRig(t)
	monitor := openEnforcerMonitorDB(t)
	owner := r.ts.CreateTestUser(t, "peheartbeatfifoowner")
	first := r.ts.CreateTestUser(t, "peheartbeatfifofirst")
	pending := r.ts.CreateTestUser(t, "peheartbeatfifopending")
	replacement := r.ts.CreateTestUser(t, "peheartbeatfiforeplacement")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Heartbeat FIFO")
	for _, member := range []string{first.ID, pending.ID, replacement.ID} {
		r.ts.AddMemberToServer(t, serverID, member, "member")
	}
	channelA := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-heartbeat-fifo-a")
	channelB := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-heartbeat-fifo-b")
	lowChannel, highChannel := channelA, channelB
	if highChannel < lowChannel {
		lowChannel, highChannel = highChannel, lowChannel
	}

	lockTx := lockEnforcerTable(t, r.ts.DB, "server_members")
	r.enforcer.RecheckParticipants(
		serverID, lowChannel, []uuid.UUID{uuid.MustParse(first.ID)},
	)
	waitForEnforcerQuery(
		t, monitor, "active", "Lock", "timed_out_until IS NOT NULL",
	)
	r.enforcer.RecheckParticipants(
		serverID, highChannel, []uuid.UUID{uuid.MustParse(pending.ID)},
	)
	r.enforcer.RecheckParticipants(
		serverID, lowChannel, []uuid.UUID{uuid.MustParse(replacement.ID)},
	)
	require.NoError(t, lockTx.Commit())

	want := map[string]bool{first.ID: true, pending.ID: true, replacement.ID: true}
	got := make([]string, 0, len(want))
	for len(got) < 3 {
		payload := waitPayloadFor(t, r.perms, "FIFO permission recheck", want)
		userID, ok := payload["userId"].(string)
		require.True(t, ok)
		got = append(got, userID)
		delete(want, userID)
	}
	require.Equal(t, []string{first.ID, pending.ID, replacement.ID}, got)
}

func TestPermissionEnforcer_RecheckParticipants_ClearedTimeoutPublishesFreshPermissions(
	t *testing.T,
) {
	r := setupEnforcerRig(t)
	monitor := openEnforcerMonitorDB(t)
	owner := r.ts.CreateTestUser(t, "peheartbeatclearowner")
	blocker := r.ts.CreateTestUser(t, "peheartbeatclearblocker")
	target := r.ts.CreateTestUser(t, "peheartbeatcleartarget")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Heartbeat Clear")
	r.ts.AddMemberToServer(t, serverID, blocker.ID, "member")
	r.ts.AddMemberToServer(t, serverID, target.ID, "member")
	blockerChannel := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-heartbeat-clear-blocker")
	targetChannel := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-heartbeat-clear-target")
	_, err := r.ts.DB.Exec(`
		UPDATE server_members
		SET timed_out_until = NOW() + INTERVAL '1 hour'
		WHERE server_id = $1 AND user_id = $2
	`, serverID, target.ID)
	require.NoError(t, err)

	lockTx := lockEnforcerTable(t, r.ts.DB, "servers")
	r.enforcer.RecheckParticipant(blockerChannel, blocker.ID)
	waitForEnforcerQuery(t, monitor, "active", "Lock", "SELECT owner_id FROM servers")

	r.enforcer.RecheckParticipants(
		serverID, targetChannel, []uuid.UUID{uuid.MustParse(target.ID)},
	)
	waitForEnforcerQuery(
		t, monitor, "idle", "", "user_id = ANY($2::uuid[])",
	)
	_, err = r.ts.DB.Exec(`
		UPDATE server_members
		SET timed_out_until = NULL
		WHERE server_id = $1 AND user_id = $2
	`, serverID, target.ID)
	require.NoError(t, err)
	require.NoError(t, lockTx.Commit())

	permission := waitForPermissionWithoutDisconnect(t, r.perms, r.disc, target.ID)
	require.Equal(t, targetChannel, permission["channelId"])
}

// TestPermissionEnforcer_RecheckServer_CoversAllVoiceChannels verifies server
// scope reaches participants across different voice channels of the server.
func TestPermissionEnforcer_RecheckServer_CoversAllVoiceChannels(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peowner4")
	m1 := r.ts.CreateTestUser(t, "pemember4a")
	m2 := r.ts.CreateTestUser(t, "pemember4b")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Server")
	r.ts.AddMemberToServer(t, serverID, m1.ID, "member")
	r.ts.AddMemberToServer(t, serverID, m2.ID, "member")
	ch1 := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-s1")
	ch2 := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-s2")
	r.addVoiceParticipant(t, ch1, m1.ID)
	r.addVoiceParticipant(t, ch2, m2.ID)

	r.enforcer.RecheckServer(serverID)

	want := map[string]bool{m1.ID: true, m2.ID: true}
	got := map[string]string{}
	for i := 0; i < 2; i++ {
		payload := waitPayloadFor(t, r.perms, "voice.enforce.permissions", want)
		userID, _ := payload["userId"].(string)
		channelID, _ := payload["channelId"].(string)
		got[userID] = channelID
	}
	assert.Equal(t, ch1, got[m1.ID], "member 1 rechecked in its own channel")
	assert.Equal(t, ch2, got[m2.ID], "member 2 rechecked in its own channel")
}

// TestPermissionEnforcer_ResolverErrorDisconnects locks the fail-closed
// direction: after an authorization mutation, a fresh resolve failure must not
// retain a potentially stale join-time permission snapshot.
func TestPermissionEnforcer_ResolverErrorDisconnects(t *testing.T) {
	r := setupEnforcerRig(t)
	pubClient, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(pubClient.Close)
	owner := r.ts.CreateTestUser(t, "peowner5")
	member := r.ts.CreateTestUser(t, "pemember5")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Broken")
	r.ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-br")
	r.addVoiceParticipant(t, channelID, member.ID)

	sentinel := r.ts.CreateTestUser(t, "pesentinel5")
	r.ts.AddMemberToServer(t, serverID, sentinel.ID, "member")
	r.addVoiceParticipant(t, channelID, sentinel.ID)

	// Presence queries use the healthy DB; the resolver's own queries hit a
	// closed DB, so every per-user resolve errors (non-ErrNotMember).
	broken := voice.NewPermissionEnforcer(
		r.ts.DB, logger.New("test"), testhelpers.BrokenResolver(t, r.ts.Redis), pubClient)

	broken.RecheckUser(serverID, member.ID)
	// Healthy sentinel push AFTER the broken recheck: when it arrives, the
	// broken recheck has had a full round-trip's window to publish — so the
	// silence assertions below are not vacuously passing on a dead observer.
	r.enforcer.RecheckUser(serverID, sentinel.ID)

	got := waitPayloadFor(t, r.perms, "voice.enforce.permissions",
		map[string]bool{sentinel.ID: true, member.ID: true})
	assert.Equal(t, sentinel.ID, got["userId"],
		"only the healthy sentinel may receive a permission snapshot")
	disconnect := waitPayloadFor(t, r.disc, "voice.enforce.disconnect",
		map[string]bool{member.ID: true})
	assert.Equal(t, channelID, disconnect["channelId"])
	assertNoPayload(t, r.perms, "voice.enforce.permissions")
}

// TestPermissionEnforcer_RecheckParticipant_PushesOnJoin locks the join-race
// closure: the voice.joined bridge re-pushes fresh permissions for the exact
// (channel, user) whose presence just landed. A channel id with no channels
// row (a DM voice conversation) is a structural no-op.
func TestPermissionEnforcer_RecheckParticipant_PushesOnJoin(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peowner6")
	member := r.ts.CreateTestUser(t, "pemember6")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Join")
	r.ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-join")
	r.addVoiceParticipant(t, channelID, member.ID)

	// DM-shaped id first: must publish nothing for it (no channels row).
	r.enforcer.RecheckParticipant("33333333-3333-3333-3333-333333333333", member.ID)
	r.enforcer.RecheckParticipant(channelID, member.ID)

	payload := waitPayloadFor(t, r.perms, "voice.enforce.permissions", map[string]bool{member.ID: true})
	assert.Equal(t, channelID, payload["channelId"],
		"the push must target the joined channel; the DM-shaped id must have produced nothing")
	assertNoPayload(t, r.perms, "voice.enforce.permissions")
}

// TestPermissionEnforcer_DisconnectUser_PublishesDisconnect verifies the
// timeout path: a still-a-member voice participant is evicted via
// voice.enforce.disconnect unconditionally, NOT re-pushed a permissions
// bitfield. The join gate bars a timed-out member independently of their
// permission bits, so a recheck would leave them live in the room.
func TestPermissionEnforcer_DisconnectUser_PublishesDisconnect(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peowner7")
	member := r.ts.CreateTestUser(t, "pemember7")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Disconnect")
	// Still a full server member (unlike the ErrNotMember eviction test) —
	// only a timeout, not removal, triggers this path.
	r.ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-disc")
	r.addVoiceParticipant(t, channelID, member.ID)

	r.enforcer.DisconnectUser(serverID, member.ID)

	payload := waitPayloadFor(t, r.disc, "voice.enforce.disconnect", map[string]bool{member.ID: true})
	assert.Equal(t, channelID, payload["channelId"])
	assert.Equal(t, member.ID, payload["userId"])
	assertNoPayload(t, r.perms, "voice.enforce.permissions")
}

// TestPermissionEnforcer_RecheckParticipant_EvictsTimedOutMember locks the
// join-vs-timeout backstop: a member whose join-authorize resolved before
// TimeoutMember committed must be disconnected by the voice.joined re-push —
// a timeout changes neither membership nor the bitfield, so without the
// explicit timed_out_until check the fresh resolve would re-push valid
// permissions and leave the member in the room for the timeout duration.
func TestPermissionEnforcer_RecheckParticipant_EvictsTimedOutMember(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peowner7")
	member := r.ts.CreateTestUser(t, "pemember7")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce Timeout")
	r.ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-to")
	r.addVoiceParticipant(t, channelID, member.ID)

	_, err := r.ts.DB.Exec(
		`UPDATE server_members SET timed_out_until = NOW() + INTERVAL '1 hour'
		 WHERE server_id = $1 AND user_id = $2`, serverID, member.ID)
	require.NoError(t, err)

	r.enforcer.RecheckParticipant(channelID, member.ID)

	payload := waitPayloadFor(t, r.disc, "voice.enforce.disconnect", map[string]bool{member.ID: true})
	assert.Equal(t, channelID, payload["channelId"])
	assertNoPayload(t, r.perms, "voice.enforce.permissions")
}

// TestPermissionEnforcer_RecheckParticipant_ExpiredTimeoutPushesNormally locks
// the negative direction: an EXPIRED timeout must not evict — the re-push
// proceeds with the fresh bitfield.
func TestPermissionEnforcer_RecheckParticipant_ExpiredTimeoutPushesNormally(t *testing.T) {
	r := setupEnforcerRig(t)
	owner := r.ts.CreateTestUser(t, "peowner8")
	member := r.ts.CreateTestUser(t, "pemember8")
	serverID := r.ts.CreateTestServer(t, owner.ID, "PermEnforce TimeoutExp")
	r.ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := r.ts.CreateVoiceChannel(t, serverID, "pe-voice-toe")
	r.addVoiceParticipant(t, channelID, member.ID)

	_, err := r.ts.DB.Exec(
		`UPDATE server_members SET timed_out_until = NOW() - INTERVAL '1 minute'
		 WHERE server_id = $1 AND user_id = $2`, serverID, member.ID)
	require.NoError(t, err)

	r.enforcer.RecheckParticipant(channelID, member.ID)

	payload := waitPayloadFor(t, r.perms, "voice.enforce.permissions", map[string]bool{member.ID: true})
	assert.Equal(t, channelID, payload["channelId"])
	assertNoPayload(t, r.disc, "voice.enforce.disconnect")
}

// TestPermissionEnforcer_NilNATSNoop verifies every recheck is a safe no-op
// without a NATS client (dev/test default), mirroring the other publishers.
func TestPermissionEnforcer_NilNATSNoop(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	enforcer := voice.NewPermissionEnforcer(ts.DB, logger.New("test"), resolver, nil)

	assert.NotPanics(t, func() {
		enforcer.RecheckUser("srv", "usr")
		enforcer.RecheckChannel("srv", "chan")
		enforcer.RecheckServer("srv")
		enforcer.DisconnectUser("srv", "usr")
	})
}
