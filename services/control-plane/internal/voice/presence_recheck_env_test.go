//go:build integration

package voice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	_ "github.com/lib/pq" // registers the PostgreSQL driver used by this fixture
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// FIDELITY NOTE — mirrors internal/rbac/presence_recheck_integration_test.go.
//
// This harness does not use *voicepresence.Executor: internal/voicepresence
// imports internal/rbac and internal/presence and is the wiring layer, while
// this file must be `package voice` to reach the unexported tempGrantManager
// and its revokeTemporaryChannelAccess. What is substituted is ONLY the
// executor's dispatch queue and failure classification (covered directly by
// internal/voicepresence/executor_test.go). Capture, visibility filtering, the
// captured-minus-fresh delta and DeliveryPlan marshalling are all real
// production code.
// ─────────────────────────────────────────────────────────────────────────────

// ── recording sinks ──────────────────────────────────────────────────────────

type tempGrantDelivery struct {
	mu            sync.Mutex
	plans         []presence.DeliveryPlan
	disconnected  map[uuid.UUID]bool
	disconnectAll int
}

func newTempGrantDelivery() *tempGrantDelivery {
	return &tempGrantDelivery{disconnected: make(map[uuid.UUID]bool)}
}

func (d *tempGrantDelivery) sawPlanFor(senderID uuid.UUID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.plans {
		if d.plans[i].SenderID == senderID {
			return true
		}
	}
	return false
}

// reset drops everything recorded while seeding, so assertions read only the
// plans a revoke produced rather than the initial publish's own plan.
func (d *tempGrantDelivery) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.plans = nil
	d.disconnected = make(map[uuid.UUID]bool)
	d.disconnectAll = 0
}

func (d *tempGrantDelivery) DeliverRichPresence(_ context.Context, plan presence.DeliveryPlan) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.plans = append(d.plans, plan)
	return nil
}

func (d *tempGrantDelivery) DisconnectRichPresenceClients(
	_ context.Context, recipients map[uuid.UUID]bool,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for viewerID, included := range recipients {
		if included {
			d.disconnected[viewerID] = true
		}
	}
	return nil
}

func (d *tempGrantDelivery) DisconnectAllRichPresenceClients(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnectAll++
	return nil
}

func (d *tempGrantDelivery) snapshot() []presence.DeliveryPlan {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]presence.DeliveryPlan, len(d.plans))
	copy(out, d.plans)
	return out
}

type tempGrantSenderGate struct{ mu sync.Mutex }

func (g *tempGrantSenderGate) WithSender(_ context.Context, _ uuid.UUID, fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn()
}

type tempGrantPresencePermitted struct{}

func (tempGrantPresencePermitted) RichPresenceEmissionPermitted(context.Context, uuid.UUID) bool {
	return true
}

type tempGrantNoLeases struct{}

func (tempGrantNoLeases) Matches(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return false, nil
}

// ── the rbac.PresenceRecheck under test ──────────────────────────────────────

type tempGrantSender struct {
	senderID    uuid.UUID
	channelID   string
	scope       presence.Scope
	candidates  map[uuid.UUID]bool
	oldAudience map[uuid.UUID]bool
}

type tempGrantPlan struct {
	serverID   string
	onlyUserID *string
	senders    []tempGrantSender
}

func (p *tempGrantPlan) HasWork() bool {
	if p == nil {
		return false
	}
	for i := range p.senders {
		if len(p.senders[i].oldAudience) > 0 {
			return true
		}
	}
	return false
}

func (p *tempGrantPlan) capturedAudience() map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool)
	for i := range p.senders {
		for viewerID := range p.senders[i].oldAudience {
			out[viewerID] = true
		}
	}
	return out
}

type tempGrantRecheck struct {
	db       *sql.DB
	resolver *rbac.Resolver
	activity *presence.ActivityService
	delivery *tempGrantDelivery

	mu        sync.Mutex
	refreshes []uuid.UUID
}

func (r *tempGrantRecheck) PrepareCapture(
	ctx context.Context, serverID string, channelIDs []string, onlyUserID *string,
) (rbac.PresenceRecheckPlan, error) {
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return nil, fmt.Errorf("parse capture server: %w", err)
	}
	if channelIDs == nil {
		channelIDs, err = r.activeVoiceChannels(ctx, serverID)
		if err != nil {
			return nil, err
		}
	}
	plan := &tempGrantPlan{serverID: serverID, onlyUserID: onlyUserID}
	for _, channelID := range channelIDs {
		senders, sendersErr := r.activeSenders(ctx, channelID)
		if sendersErr != nil {
			return nil, sendersErr
		}
		for _, sender := range senders {
			candidates, candErr := presence.CaptureServerVoiceCandidates(
				ctx, r.db, tempGrantPresencePermitted{}, sender.senderID, serverUUID,
			)
			if candErr != nil {
				return nil, candErr
			}
			sender.candidates = candidates
			sender.oldAudience = make(map[uuid.UUID]bool)
			plan.senders = append(plan.senders, sender)
		}
	}
	sort.Slice(plan.senders, func(i, j int) bool {
		return plan.senders[i].senderID.String() < plan.senders[j].senderID.String()
	})
	return plan, nil
}

func (r *tempGrantRecheck) CaptureVisibility(
	ctx context.Context, tx *sql.Tx, plan rbac.PresenceRecheckPlan,
) error {
	typed, ok := plan.(*tempGrantPlan)
	if !ok {
		return nil
	}
	for index := range typed.senders {
		sender := &typed.senders[index]
		input := tempGrantCandidateStrings(sender.candidates, typed.onlyUserID)
		if len(input) == 0 {
			continue
		}
		visible, err := r.resolver.FilterVisibleUserIDsForChannelTx(
			ctx, tx, typed.serverID, sender.channelID, input,
		)
		if err != nil {
			return err
		}
		for _, visibleID := range visible {
			if parsed, parseErr := uuid.Parse(visibleID); parseErr == nil {
				sender.oldAudience[parsed] = true
			}
		}
	}
	return nil
}

func tempGrantCandidateStrings(candidates map[uuid.UUID]bool, onlyUserID *string) []string {
	if onlyUserID != nil {
		parsed, err := uuid.Parse(*onlyUserID)
		if err != nil || !candidates[parsed] {
			return nil
		}
		return []string{parsed.String()}
	}
	out := make([]string, 0, len(candidates))
	for candidateID := range candidates {
		out = append(out, candidateID.String())
	}
	sort.Strings(out)
	return out
}

// Execute dispatches SYNCHRONOUSLY through the real presence entry point, so
// waitForDispatch is a barrier with no sleep and no polling.
func (r *tempGrantRecheck) Execute(plan rbac.PresenceRecheckPlan) {
	typed, ok := plan.(*tempGrantPlan)
	if !ok || !typed.HasWork() {
		return
	}
	for i := range typed.senders {
		sender := typed.senders[i]
		if len(sender.oldAudience) == 0 {
			continue
		}
		r.mu.Lock()
		r.refreshes = append(r.refreshes, sender.senderID)
		r.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := r.activity.RefreshServerVoiceRecheck(ctx, sender.senderID, sender.scope, sender.oldAudience)
		cancel()
		if err != nil && errors.Is(err, presence.ErrRecheckSenderNotCurrent) {
			disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), 10*time.Second)
			// discard: recording sink cannot fail; the real Hub error branch is
			// owned by *voicepresence.Executor.
			_ = r.delivery.DisconnectRichPresenceClients(disconnectCtx, sender.oldAudience)
			disconnectCancel()
		}
	}
}

func (r *tempGrantRecheck) Abandon(plan rbac.PresenceRecheckPlan, _ string) {
	typed, ok := plan.(*tempGrantPlan)
	if !ok || !typed.HasWork() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// discard: recording sink; see Execute.
	_ = r.delivery.DisconnectRichPresenceClients(ctx, typed.capturedAudience())
}

func (r *tempGrantRecheck) activeSenders(ctx context.Context, channelID string) ([]tempGrantSender, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT vp.user_id, vp.channel_id, vp.lifecycle_event_at
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE vp.channel_id = $1 AND c.type = 'voice'
		ORDER BY vp.user_id
	`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only scan; Err() is checked below

	var out []tempGrantSender
	for rows.Next() {
		var (
			senderID    uuid.UUID
			roomID      uuid.UUID
			lifecycleAt time.Time
		)
		if scanErr := rows.Scan(&senderID, &roomID, &lifecycleAt); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, tempGrantSender{
			senderID:  senderID,
			channelID: channelID,
			scope: presence.Scope{
				Category:    presence.CategoryServerVoice,
				RoomID:      roomID,
				LifecycleID: roomID,
				EventAt:     lifecycleAt,
			},
		})
	}
	return out, rows.Err()
}

func (r *tempGrantRecheck) activeVoiceChannels(ctx context.Context, serverID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT c.id
		FROM channels c
		JOIN voice_participants vp ON vp.channel_id = c.id
		WHERE c.server_id = $1 AND c.type = 'voice'
		ORDER BY c.id
	`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only scan; Err() is checked below

	var out []string
	for rows.Next() {
		var channelID string
		if scanErr := rows.Scan(&channelID); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, channelID)
	}
	return out, rows.Err()
}

// ── tempGrantPresenceEnv ─────────────────────────────────────────────────────

type tempGrantPresenceEnv struct {
	t        *testing.T
	db       *sql.DB
	redis    *redis.Client
	log      *logger.Logger
	hub      *websocket.Hub
	resolver *rbac.Resolver
	recheck  *tempGrantRecheck
	delivery *tempGrantDelivery

	manager   *tempGrantManager
	serverID  string
	channelID string
	ownerID   string
	viewRole  string

	cleanupServers []string
	cleanupUsers   []string
}

// tempGrantRedisURL mirrors internal/testhelpers.SetupTestRedis. The password is
// load-bearing: without it every call returns NOAUTH and the suite silently
// SKIPS rather than failing. DB 1 keeps fixtures off dev data.
func tempGrantRedisURL() string {
	if fromEnv := os.Getenv("REDIS_URL"); fromEnv != "" {
		return fromEnv
	}
	return "redis://:" + tempGrantRedisPassword + "@localhost:6379/1"
}

// The literal is the docker-compose dev default, not a credential: it is already
// in the committed compose file, it reaches only a local container, and REDIS_URL
// overrides it wherever the environment differs.
var tempGrantRedisPassword = "concord_dev_redis" //nolint:gosec // pragma: allowlist secret

func newTempGrantPresenceEnv(t *testing.T) *tempGrantPresenceEnv {
	t.Helper()

	db, cleanupDB := dbtest.SetupTestDB(t)
	t.Cleanup(cleanupDB)

	opts, err := redis.ParseURL(tempGrantRedisURL())
	if err != nil {
		t.Skipf("temp-grant presence integration needs Redis: %v", err)
	}
	rdb := redis.NewClient(opts)
	if pingErr := rdb.Ping(context.Background()).Err(); pingErr != nil {
		// discard: best-effort close on a client that never connected.
		_ = rdb.Close()
		t.Skipf("temp-grant presence integration needs Redis: %v", pingErr)
	}

	log := logger.New("test")
	cache := rbac.NewPermissionCache(rdb)
	resolver := rbac.NewResolver(db, cache, log)
	hub := websocket.NewHub(db, rdb)
	delivery := newTempGrantDelivery()

	activity := presence.NewActivityService(
		&tempGrantSenderGate{},
		presence.NewActivityBuilder(db, tempGrantNoLeases{}),
		presence.NewActivityStore(rdb),
		db,
		resolver, // *rbac.Resolver implements presence.ChannelVisibilityResolver
		delivery,
		tempGrantPresencePermitted{},
	)

	env := &tempGrantPresenceEnv{
		t: t, db: db, redis: rdb, log: log, hub: hub,
		resolver: resolver, delivery: delivery,
	}
	env.recheck = &tempGrantRecheck{
		db: db, resolver: resolver, activity: activity, delivery: delivery,
	}
	env.manager = newTempGrantManager(db, log, hub, resolver, nil)
	env.manager.SetPresenceRecheck(env.recheck)

	env.bootstrap()
	return env
}

func (e *tempGrantPresenceEnv) bootstrap() {
	e.ownerID = e.createUser("tgowner")
	serverID := uuid.New().String()
	e.exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, $2, $3)`,
		serverID, "tempgrant-"+serverID[:8], e.ownerID)
	e.serverID = serverID
	e.cleanupServers = append(e.cleanupServers, serverID)
	e.exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`,
		serverID, e.ownerID)

	e.viewRole = uuid.New().String()
	e.exec(`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, $3, 1, $4)`,
		e.viewRole, serverID, "tgviewers-"+e.viewRole[:8], int64(rbac.BasePermissions))

	channelID := uuid.New().String()
	e.exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'voice')`,
		channelID, serverID, "tgvc-"+channelID[:8])
	e.channelID = channelID
}

func (e *tempGrantPresenceEnv) createUser(prefix string) string {
	userID := uuid.New().String()
	e.exec(`INSERT INTO users (id, email, username, password_hash, email_verified)
	        VALUES ($1, $2, $3, 'x', TRUE)`,
		userID, prefix+"-"+userID+"@example.test",
		strings.ToLower(prefix)+strings.ReplaceAll(userID[:12], "-", ""))
	e.cleanupUsers = append(e.cleanupUsers, userID)
	return userID
}

func (e *tempGrantPresenceEnv) exec(query string, args ...any) {
	e.t.Helper()
	_, err := e.db.Exec(query, args...)
	require.NoError(e.t, err, "fixture statement failed: %s", query)
}

// ── fixture builders ─────────────────────────────────────────────────────────

// joinVoice creates a Server Voice sender whose presence is on at TierServers
// with details shown, so every server member is a candidate viewer.
func (e *tempGrantPresenceEnv) joinVoice(t *testing.T, channelID string) string {
	t.Helper()
	senderID := e.createUser("tgsender")
	e.exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`,
		e.serverID, senderID)
	e.exec(`INSERT INTO member_roles (id, server_id, user_id, role_id) VALUES ($1, $2, $3, $4)`,
		uuid.New().String(), e.serverID, senderID, e.viewRole)
	e.exec(`INSERT INTO user_presence_settings (user_id, master_enabled, server_voice_tier, server_voice_show_details)
	        VALUES ($1, TRUE, $2, TRUE)
	        ON CONFLICT (user_id) DO UPDATE
	        SET master_enabled = TRUE, server_voice_tier = EXCLUDED.server_voice_tier`,
		senderID, int(presence.TierServers))
	e.exec(`INSERT INTO voice_participants (id, channel_id, user_id, lifecycle_event_at)
	        VALUES ($1, $2, $3, NOW())`,
		uuid.New().String(), channelID, senderID)

	// Seed the Redis lifecycle watermark a real voice.joined publishes, then the
	// initial generation. Inserting rows only makes the sender look joined to
	// whatever READS the database; CompareAndSetActive's Lua returns 0 when the
	// lifecycle key is absent, so the publish would take
	// disconnectAfterGenerationMiss -- a global disconnect that returns NIL -- and
	// nothing would ever be stored or delivered. See the equivalent note in
	// internal/rbac/presence_recheck_integration_test.go.
	senderUUID := uuid.MustParse(senderID)
	scope, ok, scopeErr := presence.CurrentServerVoiceScope(context.Background(), e.db, senderUUID)
	require.NoError(t, scopeErr, "resolve seeded sender scope")
	require.True(t, ok, "a seeded sender must have a current server-voice scope")
	e.seedVoiceLifecycle(t, senderUUID, scope)
	require.NoError(t,
		e.recheck.activity.RefreshServerVoice(context.Background(), senderUUID, scope, nil),
		"seeding publish must succeed")
	require.True(t, e.delivery.sawPlanFor(senderUUID),
		"the seeding publish must actually store a generation, or the suite asserts on nothing")
	e.delivery.reset()
	return senderID
}

// seedVoiceLifecycle mirrors internal/voice/nats.go claimVoiceLifecycle: three
// fields exactly, lowercase dashed token, positive version with no leading zero,
// active '1', TTL inside ActivityStateTTL. The CAS Lua validates all of it.
func (e *tempGrantPresenceEnv) seedVoiceLifecycle(
	t *testing.T, senderID uuid.UUID, scope presence.Scope,
) {
	t.Helper()
	key, err := presence.VoiceLifecycleKey(senderID, presence.CategoryServerVoice)
	require.NoError(t, err, "derive voice lifecycle key")
	ctx := context.Background()
	require.NoError(t, e.redis.Del(ctx, key).Err())
	require.NoError(t, e.redis.HSet(ctx, key, map[string]interface{}{
		"token":   scope.LifecycleID.String(),
		"version": strconv.FormatInt(scope.EventAt.UnixMicro(), 10),
		"active":  "1",
	}).Err())
	require.NoError(t, e.redis.PExpire(ctx, key, presence.ActivityStateTTL).Err())
}

func (e *tempGrantPresenceEnv) addMember(prefix string) string {
	memberID := e.createUser(prefix)
	e.exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`,
		e.serverID, memberID)
	return memberID
}

// grantTemporaryAccess mints the temporary SBAC override the revoke path
// deletes. granted_at is backdated past the sweeper's 60-second grace so the
// same fixture is usable by the orphan-sweep path test.
func (e *tempGrantPresenceEnv) grantTemporaryAccess(t *testing.T) string {
	t.Helper()
	viewerID := e.addMember("tgtemp")
	e.exec(`INSERT INTO channel_permission_overrides
	          (id, channel_id, target_type, target_id, allow, deny, is_temporary, granted_at)
	        VALUES ($1, $2, 'user', $3, $4, 0, TRUE, NOW() - INTERVAL '5 minutes')`,
		uuid.New().String(), e.channelID, viewerID, int64(rbac.PermViewVoiceChannels))
	return viewerID
}

// addPermanentViewer holds sight through a PERMANENT user override, which the
// is_temporary guard must never remove.
func (e *tempGrantPresenceEnv) addPermanentViewer(t *testing.T) string {
	t.Helper()
	viewerID := e.addMember("tgperm")
	e.exec(`INSERT INTO channel_permission_overrides
	          (id, channel_id, target_type, target_id, allow, deny, is_temporary)
	        VALUES ($1, $2, 'user', $3, $4, 0, FALSE)`,
		uuid.New().String(), e.channelID, viewerID, int64(rbac.PermViewVoiceChannels))
	return viewerID
}

func (e *tempGrantPresenceEnv) permanentOverrideStillPresent(t *testing.T, viewerID string) bool {
	t.Helper()
	var count int
	require.NoError(t, e.db.QueryRow(`
		SELECT COUNT(*) FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND NOT is_temporary
	`, e.channelID, viewerID).Scan(&count))
	return count > 0
}

// ── observation ──────────────────────────────────────────────────────────────

// waitForDispatch is a named barrier, not a sleep: tempGrantRecheck.Execute
// dispatches synchronously, so every DeliveryPlan is already recorded when
// revokeTemporaryChannelAccess returns.
func (e *tempGrantPresenceEnv) waitForDispatch(t *testing.T) { t.Helper() }

func (e *tempGrantPresenceEnv) plansFor(senderID string) []presence.DeliveryPlan {
	out := make([]presence.DeliveryPlan, 0)
	for _, plan := range e.delivery.snapshot() {
		if plan.SenderID.String() == senderID {
			out = append(out, plan)
		}
	}
	return out
}

func (e *tempGrantPresenceEnv) wasCleared(senderID, viewerID string) bool {
	parsed, err := uuid.Parse(viewerID)
	if err != nil {
		return false
	}
	for _, plan := range e.plansFor(senderID) {
		if plan.ClearRecipients[parsed] {
			return true
		}
	}
	return false
}

func (e *tempGrantPresenceEnv) refreshCount(senderID string) int {
	e.recheck.mu.Lock()
	defer e.recheck.mu.Unlock()
	count := 0
	for _, id := range e.recheck.refreshes {
		if id.String() == senderID {
			count++
		}
	}
	return count
}

func (e *tempGrantPresenceEnv) disconnectedViewers() []string {
	e.delivery.mu.Lock()
	defer e.delivery.mu.Unlock()
	out := make([]string, 0, len(e.delivery.disconnected))
	for viewerID := range e.delivery.disconnected {
		out = append(out, viewerID.String())
	}
	sort.Strings(out)
	return out
}

func (e *tempGrantPresenceEnv) Close() {
	for _, serverID := range e.cleanupServers {
		// discard: best-effort fixture teardown on a SHARED database.
		_, _ = e.db.Exec(`DELETE FROM servers WHERE id = $1`, serverID)
	}
	for _, userID := range e.cleanupUsers {
		_, _ = e.db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	}
	_ = e.redis.Close()
}
