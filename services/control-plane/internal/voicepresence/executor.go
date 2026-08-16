package voicepresence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
)

const (
	// presenceRecheckTimeout bounds one sender's post-commit recheck, mirroring
	// the enforcer's recheckTimeout.
	presenceRecheckTimeout = 10 * time.Second
	// presenceCaptureMaxChannels bounds category-cascade fan-out. Exceeding it
	// fails the capture CLOSED (rollback) — it is a safety bound, not a tunable.
	presenceCaptureMaxChannels = 64
	// presenceDispatchQueue is the executor's bounded hand-off. A full queue is
	// an unreachable dispatch and fails closed via Abandon (failure class 5).
	presenceDispatchQueue = 256
)

// ErrCaptureChannelLimit reports a capture whose channel fan-out exceeds the
// safety bound. It rolls the mutation back.
//
// It wraps rbac.ErrPresenceCaptureLimited so withAuthorityCapture can classify
// this failure in its logs without internal/rbac importing this package — the
// zero-presence-imports invariant. The wrap direction is the only one available:
// this package already imports internal/rbac, and the reverse edge is forbidden.
var ErrCaptureChannelLimit = fmt.Errorf(
	"rich-presence capture channel limit exceeded: %w", rbac.ErrPresenceCaptureLimited,
)

// ActivityRefresher is the presence entry point this executor drives.
type ActivityRefresher interface {
	RefreshServerVoiceRecheck(
		ctx context.Context, senderID uuid.UUID, scope presence.Scope,
		recheckViewers map[uuid.UUID]bool,
	) error
}

// ChannelVisibilityFilterer is the transaction-bound visibility filter
// (implemented by *rbac.Resolver).
type ChannelVisibilityFilterer interface {
	FilterVisibleUserIDsForChannelTx(
		ctx context.Context, tx *sql.Tx, serverID, channelID string,
		candidateUserIDs []string,
	) ([]string, error)
}

// Disconnector is the fail-closed sink (implemented by *websocket.Hub).
type Disconnector interface {
	DisconnectRichPresenceClients(ctx context.Context, recipients map[uuid.UUID]bool) error
	DisconnectAllRichPresenceClients(ctx context.Context) error
}

// Executor implements rbac.PresenceRecheck.
type Executor struct {
	db             *sql.DB
	activity       ActivityRefresher
	visibility     ChannelVisibilityFilterer
	senderPresence presence.SenderPresenceResolver
	disconnector   Disconnector
	log            *logger.Logger

	queue     chan *Plan
	closeOnce sync.Once
	done      chan struct{}
}

var _ rbac.PresenceRecheck = (*Executor)(nil)

// NewExecutor builds the executor and starts its single lifecycle worker.
// Dispatch is sequential by design: every recheck enters the presence sender
// gate, so a cascade must not fan out concurrent gate acquisitions.
func NewExecutor(
	db *sql.DB,
	activity ActivityRefresher,
	visibility ChannelVisibilityFilterer,
	senderPresence presence.SenderPresenceResolver,
	disconnector Disconnector,
	log *logger.Logger,
) *Executor {
	e := &Executor{
		db: db, activity: activity, visibility: visibility,
		senderPresence: senderPresence, disconnector: disconnector, log: log,
		queue: make(chan *Plan, presenceDispatchQueue),
		done:  make(chan struct{}),
	}
	go e.run()
	return e
}

// Close stops the worker. Plans submitted afterwards fail closed via Abandon.
func (e *Executor) Close() {
	e.closeOnce.Do(func() { close(e.done) })
}

// PrepareCapture is PHASE 1 — pre-transaction, outside the advisory lock.
//
// It enumerates the active Server Voice senders in scope and resolves each
// one's candidate half. All of that reads settings / friends / FoF / server
// membership / base presence, which NO hooked RBAC write mutates, so it is
// sound outside the transaction — and running it here is what keeps the
// per-sender round trips off the advisory lock entirely. A channel admits up to
// 1000 participants; doing this work between the lock and the write would hold
// a lock that serializes every RBAC mutation on the server for ~1000 sequential
// round trips.
//
// The channel bound is checked HERE, before any transaction opens, so exceeding
// it returns 5xx without ever taking the lock.
func (e *Executor) PrepareCapture(
	ctx context.Context,
	serverID string,
	channelIDs []string,
	onlyUserID *string,
) (rbac.PresenceRecheckPlan, error) {
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return nil, fmt.Errorf("parse capture server: %w", err)
	}
	if channelIDs == nil {
		// nil means "every active voice channel in the server".
		enumerated, enumErr := e.activeVoiceChannelsForServer(ctx, serverID)
		if enumErr != nil {
			return nil, enumErr
		}
		channelIDs = enumerated
	}
	if len(channelIDs) > presenceCaptureMaxChannels {
		return nil, fmt.Errorf("%w: %d channels", ErrCaptureChannelLimit, len(channelIDs))
	}
	// ONE server_members read for this whole capture (#2681). Every sender on
	// the server resolves the same membership, and the read was previously
	// re-issued per sender because the sender exclusion was applied in SQL.
	//
	// Scoped to this call and discarded when it returns: reusing it across
	// captures would compute an audience from pre-mutation membership, the
	// error #2445 exists to prevent. It loads lazily, so a capture whose
	// senders all have presence disabled still issues zero reads.
	memberLoader := presence.NewServerMemberLoader(e.db, serverUUID)
	builder := newPlanBuilder(serverID, onlyUserID)
	for _, channelID := range channelIDs {
		senders, sendersErr := e.activeSenders(ctx, channelID)
		if sendersErr != nil {
			return nil, sendersErr
		}
		for _, sender := range senders {
			candidates, candidateErr := presence.CaptureServerVoiceCandidatesWithMembers(
				ctx, e.db, e.senderPresence, sender.SenderID, serverUUID, memberLoader,
			)
			if candidateErr != nil {
				return nil, fmt.Errorf("resolve rich-presence capture candidates: %w", candidateErr)
			}
			builder.add(sender.SenderID, channelID, sender.Scope, candidates)
		}
	}
	return builder.build(), nil
}

// CaptureVisibility is PHASE 2 — in-transaction, after the per-server advisory
// lock and before the permission write.
//
// It runs exactly ONE FilterVisibleUserIDsForChannelTx per affected channel
// over the union of that channel's senders' candidate sets, so advisory-lock
// hold time is O(#affected channels). It then intersects the result into each
// sender's OldAudience — the exact pre-write authorized audience.
func (e *Executor) CaptureVisibility(
	ctx context.Context,
	tx *sql.Tx,
	plan rbac.PresenceRecheckPlan,
) error {
	typed, ok := plan.(*Plan)
	if !ok || !typed.hasCandidates() {
		// No active senders, or every sender's base presence is off. The
		// statement sequence is identical either way: zero visibility queries,
		// and the filter returns immediately on an empty candidate list anyway
		// (#1794).
		return nil
	}
	byChannel := candidateSendersByChannel(typed)
	for _, channelID := range sortedChannelIDs(byChannel) {
		if err := e.captureChannelVisibility(
			ctx, tx, typed, channelID, byChannel[channelID],
		); err != nil {
			return err
		}
	}
	return nil
}

// captureChannelVisibility runs the ONE visibility query for a single affected
// channel and intersects its result into every sender in that channel. Keeping
// it to one query per channel is what bounds advisory-lock hold time.
func (e *Executor) captureChannelVisibility(
	ctx context.Context,
	tx *sql.Tx,
	plan *Plan,
	channelID string,
	indexes []int,
) error {
	filterInput := candidateIDStrings(unionCandidates(plan, indexes), plan.OnlyUserID)
	if len(filterInput) == 0 {
		return nil
	}
	visibleIDs, err := e.visibility.FilterVisibleUserIDsForChannelTx(
		ctx, tx, plan.ServerID, channelID, filterInput,
	)
	if err != nil {
		return fmt.Errorf("capture rich-presence channel visibility: %w", err)
	}
	intersectIntoOldAudience(plan, indexes, parseVisibleIDs(visibleIDs))
	return nil
}

// candidateSendersByChannel groups the plan's candidate-bearing senders by
// channel. A sender with no candidates contributes nothing to filter, so it is
// excluded and its channel never becomes an affected channel on its account.
func candidateSendersByChannel(plan *Plan) map[string][]int {
	byChannel := make(map[string][]int, len(plan.Senders))
	for index, sender := range plan.Senders {
		if len(sender.Candidates) == 0 {
			continue
		}
		byChannel[sender.ChannelID] = append(byChannel[sender.ChannelID], index)
	}
	return byChannel
}

// sortedChannelIDs fixes a deterministic query order under the advisory lock.
func sortedChannelIDs(byChannel map[string][]int) []string {
	channelIDs := make([]string, 0, len(byChannel))
	for channelID := range byChannel {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Strings(channelIDs)
	return channelIDs
}

// unionCandidates merges the candidate sets of every sender sharing one
// channel, so they share a single visibility query.
func unionCandidates(plan *Plan, indexes []int) map[uuid.UUID]bool {
	union := make(map[uuid.UUID]bool)
	for _, index := range indexes {
		for candidateID := range plan.Senders[index].Candidates {
			union[candidateID] = true
		}
	}
	return union
}

// parseVisibleIDs drops an unparseable id rather than discarding the rest of
// the filter result.
func parseVisibleIDs(visibleIDs []string) map[uuid.UUID]bool {
	visible := make(map[uuid.UUID]bool, len(visibleIDs))
	for _, visibleID := range visibleIDs {
		if parsed, parseErr := uuid.Parse(visibleID); parseErr == nil {
			visible[parsed] = true
		}
	}
	return visible
}

// intersectIntoOldAudience narrows each sender's candidate half by the shared
// channel visibility result, yielding the exact pre-write authorized audience.
func intersectIntoOldAudience(
	plan *Plan,
	indexes []int,
	visible map[uuid.UUID]bool,
) {
	for _, index := range indexes {
		for candidateID := range plan.Senders[index].Candidates {
			if visible[candidateID] {
				plan.Senders[index].OldAudience[candidateID] = true
			}
		}
	}
}

// candidateIDStrings bounds the visibility-filter input. Candidate SETS are
// never pruned by mutation shape; only assign/unassign may bound the FILTER
// input to the one affected user, because only that user's permission inputs
// changed.
func candidateIDStrings(
	candidates map[uuid.UUID]bool,
	onlyUserID *string,
) []string {
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
	return out
}

type activeSender struct {
	SenderID uuid.UUID
	Scope    presence.Scope
}

// activeSenders is PHASE 1 work: it runs on e.db, outside the transaction and
// outside the advisory lock. Consequence, accepted: a sender who joins between
// PrepareCapture and the write is not captured — see the deviations note.
func (e *Executor) activeSenders(
	ctx context.Context,
	channelID string,
) ([]activeSender, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT vp.user_id, vp.channel_id, vp.lifecycle_event_at
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE vp.channel_id = $1 AND c.type = 'voice'
		ORDER BY vp.user_id
	`, channelID)
	if err != nil {
		return nil, fmt.Errorf("enumerate active voice senders: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only scan; Err() is checked below

	var senders []activeSender
	for rows.Next() {
		var (
			senderID    uuid.UUID
			roomID      uuid.UUID
			lifecycleAt time.Time
		)
		if scanErr := rows.Scan(&senderID, &roomID, &lifecycleAt); scanErr != nil {
			return nil, fmt.Errorf("scan active voice sender: %w", scanErr)
		}
		senders = append(senders, activeSender{
			SenderID: senderID,
			Scope: presence.Scope{
				Category:    presence.CategoryServerVoice,
				RoomID:      roomID,
				LifecycleID: roomID,
				EventAt:     lifecycleAt,
			},
		})
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate active voice senders: %w", rowsErr)
	}
	return senders, nil
}

// activeVoiceChannelsForServer is PHASE 1 work: it runs on e.db, outside the
// transaction and outside the advisory lock.
func (e *Executor) activeVoiceChannelsForServer(
	ctx context.Context,
	serverID string,
) ([]string, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT DISTINCT c.id
		FROM channels c
		JOIN voice_participants vp ON vp.channel_id = c.id
		WHERE c.server_id = $1 AND c.type = 'voice'
		ORDER BY c.id
	`, serverID)
	if err != nil {
		return nil, fmt.Errorf("enumerate server voice channels: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only scan; Err() is checked below

	var channelIDs []string
	for rows.Next() {
		var channelID string
		if scanErr := rows.Scan(&channelID); scanErr != nil {
			return nil, fmt.Errorf("scan server voice channel: %w", scanErr)
		}
		channelIDs = append(channelIDs, channelID)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate server voice channels: %w", rowsErr)
	}
	return channelIDs, nil
}

// Execute enqueues the plan on the executor's own lifecycle worker. A closed or
// saturated queue is an unreachable dispatch and fails closed (class 5).
func (e *Executor) Execute(plan rbac.PresenceRecheckPlan) {
	typed, ok := plan.(*Plan)
	if !ok || !typed.HasWork() {
		return
	}
	// The closed check is its OWN select and runs FIRST. Folding it in beside
	// the enqueue would make both cases ready once the worker has stopped, and
	// select picks a ready case UNIFORMLY AT RANDOM — so a post-Close plan would
	// usually be enqueued to a worker that has already returned and silently
	// dropped, i.e. a captured audience that is never cleared and never
	// disconnected. That is fail-OPEN, so the ordering is load-bearing.
	select {
	case <-e.done:
		e.Abandon(plan, "dispatch_unavailable")
		return
	default:
	}
	select {
	case e.queue <- typed:
	default:
		// Saturated queue, or a Close that landed after the check above; the
		// worker's shutdown drain is the backstop for the latter.
		e.Abandon(plan, "dispatch_unavailable")
	}
}

// Abandon disconnects the captured audience fail-closed. A disconnect discloses
// nothing and reconnect rebuilds from committed state.
func (e *Executor) Abandon(plan rbac.PresenceRecheckPlan, cause string) {
	typed, ok := plan.(*Plan)
	if !ok || !typed.HasWork() {
		return
	}
	e.disconnect(typed.CapturedAudience(), cause)
}

func (e *Executor) run() {
	for {
		select {
		case <-e.done:
			e.drainOnShutdown()
			return
		case plan := <-e.queue:
			e.dispatch(plan)
		}
	}
}

// drainOnShutdown fails the already-queued plans closed rather than dropping
// them: Close can land between Execute's closed check and its enqueue, and a
// dropped plan is a captured audience that is never cleared. It drains only
// what is currently buffered, so a caller still enqueueing during shutdown
// cannot keep the worker alive.
func (e *Executor) drainOnShutdown() {
	for {
		select {
		case plan := <-e.queue:
			e.Abandon(plan, "dispatch_unavailable")
		default:
			return
		}
	}
}

func (e *Executor) dispatch(plan *Plan) {
	for _, sender := range plan.Senders {
		if len(sender.OldAudience) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), presenceRecheckTimeout)
		err := e.activity.RefreshServerVoiceRecheck(
			ctx, sender.SenderID, sender.Scope, sender.OldAudience,
		)
		cancel()
		if err == nil {
			continue
		}
		if errors.Is(err, presence.ErrRecheckSenderNotCurrent) {
			// Viewer-scoped, never global: the sender's own leave path computes
			// the post-mutation audience and would never clear this viewer.
			e.disconnect(sender.OldAudience, "sender_not_current")
			continue
		}
		// Fail closed. A terminal raised INSIDE refreshAlreadyGated has already
		// disconnected, but two classes return before it ever runs and leave the
		// captured audience holding state:
		//   - validateActivityServiceCall (ErrInvalidActivityScope, a nil service
		//     component, ctx.Err()) rejects ahead of the sender gate;
		//   - coordinator.WithSender returns ctx.Err() from its gate-acquisition
		//     select, so work() is never invoked at all. The gate is striped and
		//     shared with the voice-lifecycle writers, so a busy stripe plus the
		//     presenceRecheckTimeout budget reaches this branch under load.
		// Disconnecting an already-disconnected audience is idempotent and
		// discloses nothing; NOT disconnecting is the #2445 disclosure itself.
		// Spec §Q5 reserves log-and-continue for the empty-plan terminal only.
		e.log.Error("rich-presence recheck failed",
			"failure_class", "recheck_refresh", "error", err.Error())
		e.disconnect(sender.OldAudience, "recheck_refresh")
	}
}

func (e *Executor) disconnect(recipients map[uuid.UUID]bool, cause string) {
	if len(recipients) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), presenceRecheckTimeout)
	defer cancel()
	if err := e.disconnector.DisconnectRichPresenceClients(ctx, recipients); err != nil {
		e.log.Error("rich-presence capture disconnect failed",
			"failure_class", "capture_disconnect", "cause", cause)
		if e.disconnector.DisconnectAllRichPresenceClients(ctx) != nil {
			e.log.Error("rich-presence global disconnect failed",
				"failure_class", "capture_disconnect_all", "cause", cause)
		}
	}
}
