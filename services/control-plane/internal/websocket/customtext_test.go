package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- #1233 custom-text fan-out + on-connect snapshot integration tests ---
//
// These tests are the risk: privacy regression locks for the Custom Text Status
// feature. The load-bearing assertions are the NON-AUDIENCE EXCLUSIONS: a viewer
// who is not in the sender's tier-audience must NEVER receive the sender's custom
// text — neither via BroadcastCustomText (B3) nor via the on-connect snapshot
// (B4).

const customTextHash = "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec // dev test hash // pragma: allowlist secret

// insertCTUser inserts a minimal user row and returns its UUID.
func insertCTUser(t *testing.T, db *sql.DB, username string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	email := fmt.Sprintf("%s@test.concord.chat", username)
	// nosemgrep: go.lang.security.audit.sqli.gosql-sqli.gosql-sqli -- fully parameterized ($1..$4); email/username are test-controlled values passed as bound params, not interpolated SQL
	_, err := db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		id.String(), email, username, customTextHash)
	require.NoError(t, err)
	return id
}

// makeFriends inserts an accepted friendship between a and b.
func makeFriends(t *testing.T, db *sql.DB, a, b uuid.UUID) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`,
		a.String(), b.String())
	require.NoError(t, err)
}

// shareServer puts users a and b in the same newly-created server.
func shareServer(t *testing.T, db *sql.DB, owner uuid.UUID, members ...uuid.UUID) {
	t.Helper()
	serverID := uuid.New()
	name := fmt.Sprintf("ct-server-%s", serverID.String()[:8])
	// nosemgrep: go.lang.security.audit.sqli.gosql-sqli.gosql-sqli -- fully parameterized ($1..$3); name is a test-controlled UUID-derived value passed as a bound param, not interpolated SQL
	_, err := db.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, $2, $3)`,
		serverID.String(), name, owner.String())
	require.NoError(t, err)
	for _, m := range members {
		_, err = db.Exec(
			`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`,
			serverID.String(), m.String())
		require.NoError(t, err)
	}
}

// setCustomText writes a user's presence settings (tier + text + optional emoji).
func setCustomText(t *testing.T, db *sql.DB, userID uuid.UUID, tier int, text, emoji string) {
	t.Helper()
	var emojiArg interface{}
	if emoji == "" {
		emojiArg = nil
	} else {
		emojiArg = emoji
	}
	_, err := db.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text, custom_text_emoji)
		 VALUES ($1, $2, $3, $4)`,
		userID.String(), tier, text, emojiArg)
	require.NoError(t, err)
}

// connectClient registers a synthetic client for userID in the hub and returns it.
func connectClient(hub *Hub, userID uuid.UUID) *Client {
	clientID := uuid.New()
	client := &Client{
		ID:       clientID,
		UserID:   userID,
		Send:     make(chan []byte, 16),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[clientID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{clientID: true}
	return client
}

// assertNoMessage asserts no message arrives on the client within a short window.
// This is the privacy-exclusion lock: a non-audience viewer receives nothing.
func assertNoMessage(t *testing.T, client *Client) {
	t.Helper()
	select {
	case data := <-client.Send:
		t.Fatalf("expected NO custom-text message for non-audience viewer, got: %s", string(data))
	case <-time.After(150 * time.Millisecond):
		// good — nothing delivered
	}
}

func setupCustomTextHub(t *testing.T) (*Hub, *sql.DB) {
	t.Helper()
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	return NewHub(db, redisClient), db
}

// TestBroadcastCustomText_FriendsTier_AudienceVsNonAudience is the core B3
// privacy lock. Sender at Friends tier (1): a friend RECEIVES the update; a
// shared-server-only peer (NOT a friend) does NOT (Friends tier excludes
// server-only peers); a stranger does NOT.
func TestBroadcastCustomText_FriendsTier_AudienceVsNonAudience(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	sender := insertCTUser(t, db, "ctsender")
	friend := insertCTUser(t, db, "ctfriend")
	serverPeer := insertCTUser(t, db, "ctserverpeer")
	stranger := insertCTUser(t, db, "ctstranger")

	// sender is friends with `friend`, shares a server with `serverPeer`, has no
	// relation to `stranger`. Tier 1 (Friends) ⇒ only friend (+ FoF) is audience.
	makeFriends(t, db, sender, friend)
	shareServer(t, db, sender, sender, serverPeer)
	setCustomText(t, db, sender, 1, "focusing", "🎯")

	friendClient := connectClient(hub, friend)
	serverPeerClient := connectClient(hub, serverPeer)
	strangerClient := connectClient(hub, stranger)

	hub.BroadcastCustomText(sender, 1, &CustomTextPayload{Text: "focusing", Emoji: "🎯"})

	// Audience member (friend) RECEIVES the update.
	msg := readClientMsg(t, friendClient)
	assert.Equal(t, "rich_presence_update", msg["type"])
	data := msg["data"].(map[string]interface{})
	assert.Equal(t, sender.String(), data["user_id"])
	assert.Equal(t, "custom_text", data["category"])
	payload := data["payload"].(map[string]interface{})
	assert.Equal(t, "focusing", payload["text"])
	assert.Equal(t, "🎯", payload["emoji"])

	// PRIVACY LOCK: a shared-server-only peer is NOT in a Friends-tier audience.
	assertNoMessage(t, serverPeerClient)
	// PRIVACY LOCK: a stranger is NOT in the audience.
	assertNoMessage(t, strangerClient)
}

// TestBroadcastCustomText_ServersTier_IncludesServerPeer verifies that at tier 2
// (Servers) a shared-server peer DOES receive the update — proving the tier is
// honored, not hardcoded to Friends. A stranger still does NOT.
func TestBroadcastCustomText_ServersTier_IncludesServerPeer(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	sender := insertCTUser(t, db, "ctsender2")
	serverPeer := insertCTUser(t, db, "ctserverpeer2")
	stranger := insertCTUser(t, db, "ctstranger2")

	shareServer(t, db, sender, sender, serverPeer)
	setCustomText(t, db, sender, 2, "in a meeting", "")

	serverPeerClient := connectClient(hub, serverPeer)
	strangerClient := connectClient(hub, stranger)

	hub.BroadcastCustomText(sender, 2, &CustomTextPayload{Text: "in a meeting"})

	msg := readClientMsg(t, serverPeerClient)
	assert.Equal(t, "rich_presence_update", msg["type"])
	data := msg["data"].(map[string]interface{})
	payload := data["payload"].(map[string]interface{})
	assert.Equal(t, "in a meeting", payload["text"])
	_, hasEmoji := payload["emoji"]
	assert.False(t, hasEmoji, "emoji omitted when empty (matches optional() zod field)")

	// PRIVACY LOCK: stranger still excluded even at Servers tier.
	assertNoMessage(t, strangerClient)
}

// TestBroadcastCustomText_Clear emits rich_presence_clear to the audience when
// the payload is nil (user turned custom text off / cleared the text).
func TestBroadcastCustomText_Clear(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	sender := insertCTUser(t, db, "ctclearsender")
	friend := insertCTUser(t, db, "ctclearfriend")
	stranger := insertCTUser(t, db, "ctclearstranger")

	makeFriends(t, db, sender, friend)
	// Tier 1 keeps the friend in the audience for the clear frame.
	setCustomText(t, db, sender, 1, "was here", "")

	friendClient := connectClient(hub, friend)
	strangerClient := connectClient(hub, stranger)

	hub.BroadcastCustomText(sender, 1, nil) // nil ⇒ clear (sender was at tier 1)

	msg := readClientMsg(t, friendClient)
	assert.Equal(t, "rich_presence_clear", msg["type"])
	data := msg["data"].(map[string]interface{})
	assert.Equal(t, sender.String(), data["user_id"])
	assert.Equal(t, "custom_text", data["category"])
	_, hasPayload := data["payload"]
	assert.False(t, hasPayload, "clear frame carries no payload")

	// PRIVACY LOCK: a non-audience user gets no clear frame either.
	assertNoMessage(t, strangerClient)
}

// TestBroadcastCustomText_SenderSelfReceives confirms the sender's own connected
// devices receive their status (self-sync), like broadcastPresenceToAll.
func TestBroadcastCustomText_SenderSelfReceives(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	sender := insertCTUser(t, db, "ctselfsender")
	setCustomText(t, db, sender, 1, "solo status", "")

	selfClient := connectClient(hub, sender)

	hub.BroadcastCustomText(sender, 1, &CustomTextPayload{Text: "solo status"})

	msg := readClientMsg(t, selfClient)
	assert.Equal(t, "rich_presence_update", msg["type"])
	data := msg["data"].(map[string]interface{})
	assert.Equal(t, sender.String(), data["user_id"])
}

// TestSendCustomTextSnapshot_AudienceVsNonAudience is the core B4 privacy lock.
// Two senders have custom text at Friends tier. The connecting viewer is a friend
// of senderA only. The snapshot MUST contain senderA's text and MUST NOT contain
// senderB's (the viewer is not in senderB's audience).
func TestSendCustomTextSnapshot_AudienceVsNonAudience(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctsnapviewer")
	senderA := insertCTUser(t, db, "ctsnapsenderA")
	senderB := insertCTUser(t, db, "ctsnapsenderB")

	// viewer is a friend of senderA (in A's Friends-tier audience).
	makeFriends(t, db, senderA, viewer)
	setCustomText(t, db, senderA, 1, "A is coding", "💻")

	// viewer is NOT a friend of senderB and shares no server with B — and even if
	// they shared a server, B's Friends tier (1) would still exclude a server-only
	// peer. Here they have NO relation at all.
	setCustomText(t, db, senderB, 1, "B is secret", "🤫")

	viewerClient := connectClient(hub, viewer)

	hub.sendCustomTextSnapshot(context.Background(), viewerClient)

	// Drain all snapshot frames and collect which senders appear.
	seen := map[string]map[string]interface{}{}
	for {
		select {
		case raw := <-viewerClient.Send:
			var msg map[string]interface{}
			require.NoError(t, json.Unmarshal(raw, &msg))
			require.Equal(t, "rich_presence_update", msg["type"])
			data := msg["data"].(map[string]interface{})
			seen[data["user_id"].(string)] = data
		case <-time.After(150 * time.Millisecond):
			goto done
		}
	}
done:
	// Audience member: senderA's text IS in the snapshot.
	require.Contains(t, seen, senderA.String(), "viewer is in senderA's audience and must receive A's custom text")
	payloadA := seen[senderA.String()]["payload"].(map[string]interface{})
	assert.Equal(t, "A is coding", payloadA["text"])
	assert.Equal(t, "💻", payloadA["emoji"])

	// PRIVACY LOCK: senderB's text MUST NOT appear — viewer is not in B's audience.
	assert.NotContains(t, seen, senderB.String(), "viewer is NOT in senderB's audience and must NOT receive B's custom text")
}

// TestSendCustomTextSnapshot_ServerPeerExcludedAtFriendsTier is a focused privacy
// lock: a sender at Friends tier (1) whose ONLY relation to the viewer is a shared
// server must NOT appear in the viewer's snapshot. (At tier 2 they would.)
func TestSendCustomTextSnapshot_ServerPeerExcludedAtFriendsTier(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctsnapviewer2")
	sender := insertCTUser(t, db, "ctsnapserveronly")

	// Shared server only, no friendship. Sender is Friends-tier.
	shareServer(t, db, sender, sender, viewer)
	setCustomText(t, db, sender, 1, "friends only status", "")

	viewerClient := connectClient(hub, viewer)
	hub.sendCustomTextSnapshot(context.Background(), viewerClient)

	// PRIVACY LOCK: nothing delivered — a Friends-tier sender excludes a
	// server-only peer from their custom-text audience.
	assertNoMessage(t, viewerClient)
}

// TestSendCustomTextSnapshot_ServerPeerIncludedAtServersTier confirms the tier is
// honored in the snapshot direction too: the same server-only peer DOES appear
// when the sender is at Servers tier (2).
func TestSendCustomTextSnapshot_ServerPeerIncludedAtServersTier(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctsnapviewer3")
	sender := insertCTUser(t, db, "ctsnapserverstier")

	shareServer(t, db, sender, sender, viewer)
	setCustomText(t, db, sender, 2, "servers can see me", "")

	viewerClient := connectClient(hub, viewer)
	hub.sendCustomTextSnapshot(context.Background(), viewerClient)

	msg := readClientMsg(t, viewerClient)
	assert.Equal(t, "rich_presence_update", msg["type"])
	data := msg["data"].(map[string]interface{})
	assert.Equal(t, sender.String(), data["user_id"])
	payload := data["payload"].(map[string]interface{})
	assert.Equal(t, "servers can see me", payload["text"])
}

// TestSendCustomTextSnapshot_TierOffExcluded confirms tier 0 (Off) senders are
// never snapshotted even to a friend.
func TestSendCustomTextSnapshot_TierOffExcluded(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctsnapviewer4")
	sender := insertCTUser(t, db, "ctsnapoff")

	makeFriends(t, db, sender, viewer)
	// Tier 0 (Off) with text present — the candidate query filters tier > 0.
	setCustomText(t, db, sender, 0, "should be hidden", "")

	viewerClient := connectClient(hub, viewer)
	hub.sendCustomTextSnapshot(context.Background(), viewerClient)

	// PRIVACY LOCK: tier Off ⇒ no audience ⇒ nothing delivered even to a friend.
	assertNoMessage(t, viewerClient)
}

// --- #1233 fail-closed / guard-branch coverage ---
//
// The tests below lock the defensive branches that the audience-path tests above
// never exercise: the DB-free hub guards, the fail-closed DB-error paths, the
// snapshot self-skip, and the non-blocking send. These are the risk: privacy
// "never leak on error" guarantees — a DB error must mean "send nothing", never
// "send to everyone".

// dbFreeHub builds a zero-value hub with only the client maps initialized and a
// nil *sql.DB, mirroring the DB-free unit-hub pattern in hub_coverage_test.go.
// It is sufficient for the `h.db == nil` guard branches: those return before any
// DB or lock access, but BroadcastCustomText's deferred RUnlock still needs a
// usable mutex (zero-value sync.RWMutex is ready to use) and the loop needs a
// non-nil userClients map only on the non-guard path.
func dbFreeHub() *Hub {
	return &Hub{
		clients:     make(map[uuid.UUID]*Client),
		userClients: make(map[uuid.UUID]map[uuid.UUID]bool),
	}
}

// closedDBHub returns a real hub whose underlying *sql.DB has been closed, so
// every subsequent query fails — the most practical trigger for the fail-closed
// error branches (mirrors the closed-pool pattern in
// internal/users/handlers_dberror_test.go). The pool is opened INLINE here (not
// via setupHubTestDB) on purpose: setupHubTestDB registers a t.Cleanup that runs
// a TRUNCATE, which would error against an already-closed pool. No data is
// seeded because every query fails before reading any rows. Returns a fresh
// sender/viewer pair only so callers have stable IDs to drive the fail path.
func closedDBHub(t *testing.T) (*Hub, uuid.UUID, uuid.UUID) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://concord:" + hubTestDBPassword + "@localhost:5432/concord?sslmode=disable" //nolint:gosec
	}
	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	// Close immediately so every subsequent query/exec returns
	// "sql: database is closed".
	require.NoError(t, db.Close())

	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	return hub, uuid.New(), uuid.New()
}

// TestBroadcastCustomText_NilDB_NoPanicNoSend covers the `h.db == nil` guard in
// BroadcastCustomText (lines 53-58): a DB-free hub must return without panicking
// and deliver nothing.
func TestBroadcastCustomText_NilDB_NoPanicNoSend(t *testing.T) {
	hub := dbFreeHub()
	sender := uuid.New()
	selfClient := connectClient(hub, sender)

	// Must not panic; must send nothing (audience uncomputable ⇒ fail closed).
	assert.NotPanics(t, func() {
		hub.BroadcastCustomText(sender, 1, &CustomTextPayload{Text: "no db"})
	})
	assertNoMessage(t, selfClient)
}

// TestBroadcastCustomText_ClosedDB_FailsClosed covers the audience-computation
// error branch (lines 61-67): when ComputeCustomTextAudience errors (closed DB),
// the frame is fanned out to no one. No panic, no leak.
func TestBroadcastCustomText_ClosedDB_FailsClosed(t *testing.T) {
	hub, sender, viewer := closedDBHub(t)

	viewerClient := connectClient(hub, viewer)
	selfClient := connectClient(hub, sender)

	assert.NotPanics(t, func() {
		hub.BroadcastCustomText(sender, 2, &CustomTextPayload{Text: "should not leak"})
	})

	// PRIVACY LOCK: audience error ⇒ nobody receives, not even the sender's self.
	assertNoMessage(t, viewerClient)
	assertNoMessage(t, selfClient)
}

// TestSendCustomTextSnapshot_NilDB_NoPanicNoSend covers the `h.db == nil` guard
// in sendCustomTextSnapshot (lines 134-136).
func TestSendCustomTextSnapshot_NilDB_NoPanicNoSend(t *testing.T) {
	hub := dbFreeHub()
	viewer := uuid.New()
	viewerClient := connectClient(hub, viewer)

	assert.NotPanics(t, func() {
		hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	})
	assertNoMessage(t, viewerClient)
}

// TestSendCustomTextSnapshot_ClosedDB_FailsClosed covers the candidate-query
// error branch in sendCustomTextSnapshot (lines 139-144) AND the query-error
// branch in customTextCandidates (lines 198-200): a closed DB means the
// candidate query errors, so the snapshot emits nothing.
func TestSendCustomTextSnapshot_ClosedDB_FailsClosed(t *testing.T) {
	hub, _, viewer := closedDBHub(t)

	viewerClient := connectClient(hub, viewer)

	assert.NotPanics(t, func() {
		hub.sendCustomTextSnapshot(context.Background(), viewerClient)
	})
	assertNoMessage(t, viewerClient)
}

// TestCustomTextCandidates_ClosedDB_ReturnsError directly asserts the
// customTextCandidates query-error path (lines 198-200): a closed DB yields a
// non-nil error and a nil slice (caller fails closed on it).
func TestCustomTextCandidates_ClosedDB_ReturnsError(t *testing.T) {
	hub, _, _ := closedDBHub(t)

	out, err := hub.customTextCandidates(context.Background())
	require.Error(t, err)
	assert.Nil(t, out)
}

// TestSendCustomTextSnapshot_SelfCandidateSkipped covers the self-skip branch
// (lines 147-148): when the connecting viewer is themselves a custom-text
// candidate, their own row is skipped (self is delivered via live self-sync, not
// the snapshot of others).
func TestSendCustomTextSnapshot_SelfCandidateSkipped(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctselfcand")
	// The viewer has custom text set, so they ARE a candidate row — but the
	// snapshot of OTHERS must skip self.
	setCustomText(t, db, viewer, 2, "my own status", "🙂")

	viewerClient := connectClient(hub, viewer)
	hub.sendCustomTextSnapshot(context.Background(), viewerClient)

	// Self row skipped ⇒ no frame delivered to the viewer about themselves.
	assertNoMessage(t, viewerClient)
}

// TestSendCustomTextSnapshot_SendBufferFull covers the non-blocking-send
// `default:` branch (line 172): when the viewer's Send channel is full, the
// snapshot drops the frame rather than blocking the hub goroutine.
func TestSendCustomTextSnapshot_SendBufferFull(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	viewer := insertCTUser(t, db, "ctbufviewer")
	sender := insertCTUser(t, db, "ctbufsender")

	// viewer is in sender's Friends-tier audience, so the snapshot WOULD send.
	makeFriends(t, db, sender, viewer)
	setCustomText(t, db, sender, 1, "would be sent", "")

	// Connect the viewer with a zero-capacity Send channel and pre-fill so the
	// send blocks and hits the `default:` drop path.
	clientID := uuid.New()
	viewerClient := &Client{
		ID:       clientID,
		UserID:   viewer,
		Send:     make(chan []byte), // unbuffered + no reader ⇒ send never ready
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[clientID] = viewerClient
	hub.userClients[viewer] = map[uuid.UUID]bool{clientID: true}

	// Must not block / panic — the default branch drops the frame.
	done := make(chan struct{})
	go func() {
		hub.sendCustomTextSnapshot(context.Background(), viewerClient)
		close(done)
	}()
	select {
	case <-done:
		// good — returned without blocking on the unbuffered Send channel.
	case <-time.After(2 * time.Second):
		t.Fatal("sendCustomTextSnapshot blocked instead of taking the default drop branch")
	}
}

// TestBroadcastCustomText_TierNarrowed_ClearsExcluded is the #1233/Gitar privacy
// regression lock: narrowing Servers(2)->Friends(1) must send rich_presence_clear
// to a viewer who was only a shared-server peer (in the OLD audience, not the
// new), while the friend (in both) gets the update. Without the oldTier delta the
// stale status would linger on the excluded peer until reconnect.
func TestBroadcastCustomText_TierNarrowed_ClearsExcluded(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	sender := insertCTUser(t, db, "ctnarrowsender")
	friend := insertCTUser(t, db, "ctnarrowfriend")
	serverPeer := insertCTUser(t, db, "ctnarrowpeer")

	makeFriends(t, db, sender, friend)
	shareServer(t, db, sender, sender, serverPeer)
	// DB holds the NEW tier (Friends=1) + text; oldTier passed as 2 (Servers).
	setCustomText(t, db, sender, 1, "narrowed", "")

	friendClient := connectClient(hub, friend)
	serverPeerClient := connectClient(hub, serverPeer)

	hub.BroadcastCustomText(sender, 2, &CustomTextPayload{Text: "narrowed"})

	// Friend (in both old and new audience) receives the update.
	fm := readClientMsg(t, friendClient)
	assert.Equal(t, "rich_presence_update", fm["type"])

	// PRIVACY LOCK: the server-only peer left the audience (Servers->Friends) and
	// MUST be cleared, not left showing the stale status.
	pm := readClientMsg(t, serverPeerClient)
	assert.Equal(t, "rich_presence_clear", pm["type"])
	pdata := pm["data"].(map[string]interface{})
	assert.Equal(t, sender.String(), pdata["user_id"])
	assert.Equal(t, "custom_text", pdata["category"])
}

// TestBroadcastCustomText_TurnedOff_ClearsPriorAudience: turning custom text off
// (new tier 0 => nil payload) must clear the PRIOR (oldTier) audience — #1233/Gitar.
func TestBroadcastCustomText_TurnedOff_ClearsPriorAudience(t *testing.T) {
	hub, db := setupCustomTextHub(t)

	sender := insertCTUser(t, db, "ctoffsender")
	friend := insertCTUser(t, db, "ctofffriend")

	makeFriends(t, db, sender, friend)
	// DB now holds tier 0 (off); oldTier passed as 1 (Friends).
	setCustomText(t, db, sender, 0, "", "")

	friendClient := connectClient(hub, friend)

	hub.BroadcastCustomText(sender, 1, nil) // turned off => clear the prior audience

	fm := readClientMsg(t, friendClient)
	assert.Equal(t, "rich_presence_clear", fm["type"])
	fdata := fm["data"].(map[string]interface{})
	assert.Equal(t, sender.String(), fdata["user_id"])
}
