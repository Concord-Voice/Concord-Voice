// services/control-plane/internal/users/delete_account.go

package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
)

// ErrUserNotFound is returned when DeleteAccount targets a user row that no
// longer exists. Callers should map this to HTTP 404; the endpoint remains
// idempotent (retry after a successful delete is harmless) while still
// distinguishing "already gone" from "just deleted" in operational logs.
var ErrUserNotFound = errors.New("user not found")

// ErrErasureCandidateSetDrifted means the private-call audience changed after
// it was gated and before the erasure transaction locked its evidence.
var ErrErasureCandidateSetDrifted = errors.New(
	"account erasure active-call candidate set changed under lock",
)

const maxErasurePrivateCallSubjects = 16

type erasureCandidateQuerier interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

// AccountDeleter is the narrow interface the privacy handler depends on.
// Declared here so privacy can import it without pulling the concrete
// service (and its database handle) into the handler's test surface.
type AccountDeleter interface {
	DeleteAccount(ctx context.Context, userID string) error
}

// ChannelDeletedBroadcaster notifies connected server members after account
// erasure removes an incomplete channel.
type ChannelDeletedBroadcaster func(serverID, channelID string)

// ErasedMediaReclaimer discharges the OBJECT-STORAGE half of an account
// erasure, given the blob references captured before the cascade destroyed the
// rows that named them (#2759 follow-on).
//
// A func type rather than an interface, mirroring ChannelDeletedBroadcaster:
// the two halves live in different packages (media for tier 1, purge for tier
// 2) and the router already holds both, so a closure there is the whole wiring.
//
// nil means unwired, and unwired means the pre-existing leak. That is a
// deliberate degrade rather than a boot refusal, because an erasure that
// completes with residue is strictly better than one that cannot run at all --
// but production must wire it, and the orphan reaper only covers tier 2.
type ErasedMediaReclaimer func(ctx context.Context, tier1, tier2 []media.BlobRef)

// erasedMedia is the object-storage half of one erasure, captured under the
// user-row lock BEFORE `DELETE FROM users` fires.
//
// Split by tier because the two halves discharge through different rails for
// different reasons. Tier 2 goes to the purge reaper's queue, whose contract
// requires per-upload-unique keys -- `attachments/<fileID>` satisfies it. Tier 1
// cannot: its keys are DETERMINISTIC per subject, which is exactly the input
// EnqueueBlobDeletes refuses, so it goes to media.ReclaimErasedTier1 instead.
type erasedMedia struct {
	tier1 []media.BlobRef
	tier2 []media.BlobRef
}

// erasedMediaReclaimTimeout bounds the detached reclamation. It covers a
// handful of object DELETEs, so exceeding it means the backend is unhealthy,
// not that the budget was tight.
const erasedMediaReclaimTimeout = 30 * time.Second

// erasedMediaQuery captures the objects an erasure strands, under the user-row
// lock and before the cascade removes the rows.
//
// THE `storage_key = ANY($2)` ARM IS THE SAFETY ARGUMENT and it is in the SQL
// deliberately, where it is visible next to the uploader_id predicate it
// narrows. $2 is media.ErasableTier1Keys -- the two tier-1 keys whose subject
// IS this user. A bare `WHERE uploader_id = $1` would additionally sweep in
// `server-icons/`, `server-banners/` and `dm-icons/` rows, whose objects belong
// to servers and conversations that survive this user; see ErasableTier1Keys
// for why uploader_id does not mean what it looks like it means there.
//
// THE `deleted_at` FILTER IS ON THE TIER-2 ARM ONLY, and that asymmetry is the
// whole point -- an earlier revision applied it to both and stranded tier-1
// plaintext (caught in review of PR #3019).
//
// On the TIER-2 arm it rides idx_media_files_uploader (partial on exactly that
// predicate) instead of sequentially scanning media_files under the erasure's
// user-row lock, and the rows it skips are genuinely covered: a soft-deleted
// attachment row is either already reaped or is a straggler-sweep candidate,
// and once the cascade destroys it the orphan reaper (media.OrphanReaper)
// reclaims the object from the bucket. Two mechanisms, one deliberate handoff.
//
// On the TIER-1 arm that reasoning does NOT hold, and applying the filter there
// leaks. media.CleanupObject soft-deletes the row even when its DeleteObject
// FAILED and even when no store is configured, so a soft-deleted
// `avatars/<userID>` row can legitimately still have a live object. Nothing
// else would ever find it: the straggler sweep is `media_tier = 2`, and so is
// the orphan reaper. The residue would be PLAINTEXT (processTier1Image decodes
// and re-encodes server-side) and would be created by erasures AFTER this
// change, not merely inherited from before it.
//
// Widening is safe rather than merely better: both keys are subject-scoped to
// the user being erased, so no shared subject can be reached; the tier-1 arm is
// an equality probe against idx_media_files_storage_key_all (migration 000115),
// so it costs an index lookup rather than a scan; and re-deleting an object
// that was already reaped is a success no-op.
//
// DISTINCT because tier-1 keys are deterministic: an old avatar row and the
// live one legitimately share `avatars/<userID>`, and the object only needs
// deleting once.
const erasedMediaQuery = `SELECT DISTINCT media_tier, storage_key, storage_backend
	         FROM media_files
	         WHERE uploader_id = $1
	           AND ((media_tier = 2 AND deleted_at IS NULL)
	                OR storage_key = ANY($2::text[]))`

// ActivePlanDrain is the #2448 durable active-category rail's erasure seam,
// implemented by *activepresence.Rail.
//
// Declared HERE, at the consumer, so this package depends on the two methods it
// uses rather than on the whole rail -- the same shape dm.ActivePlanRail uses,
// and what keeps internal/users free of an internal/activepresence import.
//
// Both methods are the ...AlreadyGated flavour by necessity, not preference.
// DeleteAccount already holds this user's presencehistory sender gate; the
// gated flavours (Rail.WithGatedTx) would take the same buffered-1 channel a
// second time and block forever, with no timeout and no deadlock detector,
// because half the cycle is a Go channel rather than a database lock.
type ActivePlanDrain interface {
	// DrainAlreadyGated removes every outstanding obligation for one subject
	// inside the caller's transaction and reports which categories it removed.
	DrainAlreadyGated(
		ctx context.Context, tx *sql.Tx, subjectID uuid.UUID,
	) ([]presence.Category, error)

	// ClearDrained transfers the drained obligation to one proportional clear
	// frame per category. It returns nothing DELIBERATELY: it runs after the
	// erasure has committed, and a presence delivery failure must never fail an
	// erasure that already happened.
	ClearDrained(ctx context.Context, subjectID uuid.UUID, categories []presence.Category)

	CapturePrivateCallPlansAlreadyGated(
		ctx context.Context, tx *sql.Tx, subjects []uuid.UUID,
	) error
	CompletePrivateCallPlansAlreadyGated(
		ctx context.Context, subjects []uuid.UUID,
	) error
}

// drainedObligation is deleteAccountTx's transferable output: the subject whose
// plans were drained, and the categories it now owes a clear frame for.
//
// It carries the PARSED subject rather than leaving deleteAccount to re-parse
// the caller's string post-commit, where a parse failure would have nowhere to
// go but a log line on a path that has already succeeded.
type drainedObligation struct {
	subject    uuid.UUID
	categories []presence.Category
}

type committedAccountDeletion struct {
	channelIDs       pq.StringArray
	serverIDs        pq.StringArray
	drained          drainedObligation
	stranded         erasedMedia
	survivorSubjects []uuid.UUID
}

// AccountService is the concrete AccountDeleter backed by the primary
// Postgres pool. Its erasure transaction starts only after sender-gated
// activity cleanup has completed.
type AccountService struct {
	db              *sql.DB
	log             *logger.Logger
	activityCleanup *Handler
	channelDeleted  ChannelDeletedBroadcaster

	// reclaimMedia discharges the object-storage half of the erasure. nil
	// leaves the pre-existing behaviour: the cascade hard-deletes every
	// media_files row this user uploaded and the objects stay on the backend
	// with nothing able to find them again. See ErasedMediaReclaimer.
	reclaimMedia ErasedMediaReclaimer

	// graphPresence is the #2447 erasure presence capture. nil means unwired.
	graphPresence presencecapture.GraphPresenceCapture

	// activePlans is the #2448 durable active-category drain. nil means
	// unwired, in which case an erasure of a user holding an outstanding plan
	// fails on migration 000111's ON DELETE RESTRICT with an undiagnosable
	// 23503 -- which is why the boot guard refuses to start without it.
	activePlans ActivePlanDrain

	// audienceFence brackets the erasure transaction so an in-flight presence
	// audience computed against the pre-erasure graph cannot be delivered
	// (#2992). nil means unwired: the erasure still runs and still dispatches
	// its post-commit signal, but that signal is bounded by dispatchQueueDepth
	// times dispatchTimeout rather than ordered against the write. The boot
	// guard in router.go refuses to start unwired.
	//
	// This path cannot use graphpresence's choke point: it already holds the
	// same sender-gate stripe (see the comment on the erasure capture below) and
	// re-entering WithGatedTx would self-deadlock.
	audienceFence *websocket.Hub

	// nats fans the erasure clear out to every replica (#2447). The Hub closes
	// only LOCAL clients, so without this a viewer on another replica keeps the
	// erased principal's Custom Status indefinitely -- it carries no TTL and is
	// not republished on a heartbeat. nil leaves that gap open, so the boot path
	// must wire it.
	nats *natsclient.Client
}

// NewAccountService constructs an AccountService. The logger is optional;
// a nil value is tolerated so tests that do not exercise the failure path
// can construct a service without one. Production callers must always
// pass a non-nil logger.
func NewAccountService(db *sql.DB, log *logger.Logger) *AccountService {
	return &AccountService{db: db, log: log}
}

// SetActivitySettingsCleanupHandler binds the same sender coordinator and
// suppression service used by presence-settings writers.
func (s *AccountService) SetActivitySettingsCleanupHandler(handler *Handler) {
	s.activityCleanup = handler
}

// SetChannelDeletedBroadcaster binds the post-commit incomplete-channel event.
func (s *AccountService) SetChannelDeletedBroadcaster(broadcaster ChannelDeletedBroadcaster) {
	s.channelDeleted = broadcaster
}

// SetErasedMediaReclaimer binds the post-commit object-storage reclamation.
func (s *AccountService) SetErasedMediaReclaimer(reclaimer ErasedMediaReclaimer) {
	s.reclaimMedia = reclaimer
}

// SetAudienceFence binds the presence-audience revocation fence (#2992).
func (s *AccountService) SetAudienceFence(hub *websocket.Hub) {
	s.audienceFence = hub
}

// AudienceFenceWired reports whether the fence is bound, so the router's boot
// guard can interrogate the service itself rather than trusting that the setter
// call in buildPrivacyHandler still exists.
func (s *AccountService) AudienceFenceWired() bool {
	return s != nil && s.audienceFence != nil
}

// DeleteAccount first resumes any durable activity-policy cleanup while holding
// the sender gate, then performs the database erasure inside one transaction:
//  1. Lock the user and delete any incomplete E2EE channels they created, so
//     the post-commit caller can notify their server members.
//  2. DELETE FROM users WHERE id = $1 — cascades through every remaining
//     user_id-FK table configured with ON DELETE CASCADE.
//  3. INSERT an audit row into account_deletions with user_id = NULL. We
//     cannot reference the just-deleted user_id at this point — the FK
//     check would fire on insert and fail — but a NULL is the intended
//     post-commit state anyway (the schema's ON DELETE SET NULL would have
//     nulled it automatically had we ordered INSERT before DELETE). The
//     audit row captures the deletion event and timestamp.
//  4. COMMIT.
//
// The DELETE-then-INSERT ordering makes the retry-after-success path behave
// correctly: a second call gets RowsAffected = 0, returns ErrUserNotFound,
// and leaves no audit side effect. Ordering INSERT-first would make the
// FK reject the retry's audit INSERT with 23503 even though semantically
// nothing needs to happen.
//
// On failure at any stage the deferred rollback restores the pre-call
// state; callers may retry safely (idempotent).
func (s *AccountService) DeleteAccount(
	ctx context.Context,
	userID string,
) error {
	creatorID, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("delete account: invalid user id: %w", err)
	}
	candidates, overflow, err := readErasurePrivateCallCandidates(ctx, s.db, creatorID)
	if err != nil {
		return fmt.Errorf("delete account: read private-call candidates: %w", err)
	}
	if overflow {
		return fmt.Errorf("private-call candidate fan-out exceeds %d subjects", maxErasurePrivateCallSubjects)
	}
	if len(candidates) > 0 && (s.activityCleanup == nil || s.activePlans == nil) {
		return errors.New("delete account: private-call erasure wiring unavailable")
	}
	survivorSubjects := append([]uuid.UUID(nil), candidates...)
	committed, operationErr := s.deleteAccountWithActivityCleanup(
		ctx, userID, creatorID, candidates, &survivorSubjects,
	)
	if committed != nil {
		s.runPostCommitObligations(ctx, userID, committed.channelIDs,
			committed.serverIDs, committed.drained, committed.stranded)
	}
	return operationErr
}

func (s *AccountService) deleteAccountWithActivityCleanup(
	ctx context.Context,
	userID string,
	creatorID uuid.UUID,
	candidates []uuid.UUID,
	survivorSubjects *[]uuid.UUID,
) (*committedAccountDeletion, error) {
	var committed *committedAccountDeletion
	work := func() error {
		// The transaction and its post-commit private-call completion form one
		// audience revocation interval. The sender gate remains held outside this
		// closure until the fence has closed.
		defer s.audienceFence.BeginAudienceRevocation()()
		var err error
		committed, err = s.deleteAccount(ctx, userID, candidates, survivorSubjects)
		return s.completeErasurePrivateCallPlans(ctx, committed, err)
	}
	if s.activityCleanup == nil {
		err := work()
		return committed, err
	}
	if s.activityCleanup.presenceHistory == nil {
		return nil, errors.New("delete account: activity cleanup coordinator unavailable")
	}
	if s.activityCleanup.activitySuppressor == nil {
		return nil, errors.New("delete account: full activity suppression unavailable")
	}
	err := s.activityCleanup.presenceHistory.WithSenders(ctx,
		append([]uuid.UUID{creatorID}, candidates...), func() error {
			if _, err := s.activityCleanup.resumePendingActivitySettingsCleanup(ctx, creatorID); err != nil {
				return fmt.Errorf("delete account: resume activity cleanup: %w", err)
			}
			if err := s.activityCleanup.activitySuppressor.SuppressAllActivityAlreadyGated(ctx, creatorID); err != nil {
				return fmt.Errorf("delete account: suppress active activity: %w", err)
			}
			return work()
		})
	return committed, err
}

func (s *AccountService) completeErasurePrivateCallPlans(
	ctx context.Context,
	committed *committedAccountDeletion,
	operationErr error,
) error {
	if committed == nil || s.activePlans == nil || len(committed.survivorSubjects) == 0 {
		return operationErr
	}
	if err := s.activePlans.CompletePrivateCallPlansAlreadyGated(ctx, committed.survivorSubjects); err != nil {
		deliveryErr := fmt.Errorf("%w: %w", presencecapture.ErrPostCommitDelivery, err)
		if operationErr == nil {
			return deliveryErr
		}
		return errors.Join(operationErr, deliveryErr)
	}
	return operationErr
}

func readErasurePrivateCallCandidates(
	ctx context.Context, querier erasureCandidateQuerier, creatorID uuid.UUID,
) (candidates []uuid.UUID, overflow bool, returnErr error) {
	rows, err := querier.QueryContext(ctx, `
		SELECT DISTINCT dvp.user_id
		FROM dm_conversations c
		JOIN dm_voice_participants dvp ON dvp.conversation_id = c.id
		WHERE c.created_by = $1 AND dvp.user_id <> $1
		ORDER BY dvp.user_id
		LIMIT $2`, creatorID, maxErasurePrivateCallSubjects+1)
	if err != nil {
		return nil, false, fmt.Errorf("query candidates: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close candidate rows: %w", closeErr))
		}
	}()
	candidates = make([]uuid.UUID, 0, maxErasurePrivateCallSubjects)
	for rows.Next() {
		var subject uuid.UUID
		if err := rows.Scan(&subject); err != nil {
			return nil, false, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, subject)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate candidates: %w", err)
	}
	return candidates, len(candidates) > maxErasurePrivateCallSubjects, nil
}

func (s *AccountService) deleteAccount(
	ctx context.Context, userID string, candidates []uuid.UUID, survivorSubjects *[]uuid.UUID,
) (*committedAccountDeletion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("delete account: begin tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			if s.log != nil {
				s.log.Error("delete account: rollback failed", "error", rbErr)
			}
		}
	}()
	channelIDs, serverIDs, plan, drained, stranded, err := s.deleteAccountTx(ctx, tx, userID, candidates, survivorSubjects)
	if err != nil {
		// CauseWriteFailed and friends prove no commit happened, so Abandon does
		// NOT disconnect anyone on those causes -- the deferred rollback discards
		// the plan along with the transaction.
		presencehook.Abandon(s.graphPresence, plan, presencecapture.CauseWriteFailed)
		return nil, err
	}
	// Complete owns the commit. A bare tx.Commit() here would leave the captured
	// plan undispatched, so the surviving senders' audiences would never be
	// reconciled even though the erasure landed.
	//
	// ErrPostCommitDelivery is a DURABLE outcome: the users row is gone and only
	// presence delivery failed. Returning early on it would skip the
	// cross-replica clear and the channel-deleted fan-out below — leaving the
	// erased principal's Custom Status resident on every other replica, which is
	// the precise gap publishErasureCleared exists to close. So record it and
	// fall through (rbac review, PR #2840).
	var deliveryErr error
	if err := presencehook.Complete(ctx, s.graphPresence, tx, plan); err != nil {
		if !errors.Is(err, presencecapture.ErrPostCommitDelivery) {
			return nil, fmt.Errorf("delete account: commit: %w", err)
		}
		deliveryErr = err
	}
	committed := &committedAccountDeletion{
		channelIDs: channelIDs, serverIDs: serverIDs, drained: drained,
		stranded:         stranded,
		survivorSubjects: append([]uuid.UUID(nil), (*survivorSubjects)...),
	}
	if deliveryErr != nil {
		return committed, fmt.Errorf("delete account: presence delivery: %w", deliveryErr)
	}
	return committed, nil
}

// runPostCommitObligations discharges everything the erasure owes AFTER its
// commit. Every member is best effort by design: the users row is gone, so
// failing the request would invite a retry against state that no longer exists.
func (s *AccountService) runPostCommitObligations(
	ctx context.Context,
	userID string,
	channelIDs, serverIDs pq.StringArray,
	drained drainedObligation,
	stranded erasedMedia,
) {
	// Cross-replica Custom Status clear (#2447). AFTER the commit: publishing
	// first would tell the fleet to clear an account that may still exist.
	s.publishErasureCleared(userID)
	s.transferDrainedActivePlans(ctx, drained)
	s.reclaimErasedMedia(ctx, stranded)
	if s.channelDeleted == nil {
		return
	}
	for index, channelID := range channelIDs {
		s.channelDeleted(serverIDs[index], channelID)
	}
}

// reclaimErasedMedia is the post-commit half of the media capture: the objects
// are deleted only once the erasure that stranded them has definitely landed.
//
// Post-commit and not pre-, for the same reason transferDrainedActivePlans is:
// an object delete is not rollback-able, so performing it inside the
// transaction would let a rollback restore an account whose avatar is already
// gone from the bucket and whose row still points at it.
//
// THE REQUEST CONTEXT IS DETACHED, and this MATCHES the file rather than
// departing from it. Every other post-commit obligation here is already immune
// to request cancellation: publishErasureCleared takes no context at all,
// ClearDrained takes one and discards it (`_ context.Context`), and
// channelDeleted is a hub broadcast. This was the only one that passed a LIVE
// request context into work that can fail on cancellation -- an S3 DELETE.
//
// The window is real, not theoretical. DeleteAccount is handed
// c.Request.Context() (privacy/handler.go), which cancels the moment the client
// disconnects, and account deletion is exactly the flow where someone confirms
// and then closes the app while the erasure is still doing its sender-gated
// suppression, presence capture and cascade. The users row is already gone by
// the time we get here, so a cancelled context would strand the objects with
// nobody able to retry: tier 2 would wait for the next orphan sweep, and TIER 1
// has no sweep at all -- that residue is plaintext and permanent.
//
// Bounded rather than unbounded: this runs on the request goroutine, so an
// unresponsive object store must not hold it open indefinitely. The timeout is
// generous because the work is a handful of DELETEs, and exceeding it means the
// backend is unhealthy rather than that we were impatient.
func (s *AccountService) reclaimErasedMedia(ctx context.Context, stranded erasedMedia) {
	if len(stranded.tier1) == 0 && len(stranded.tier2) == 0 {
		return
	}
	if s.reclaimMedia == nil {
		// Unwired. Say so rather than returning silently: this is the
		// pre-existing leak, and on the tier-1 leg it is PLAINTEXT residue
		// surviving a GDPR Article 17 erasure with no sweep able to find it.
		if s.log != nil {
			s.log.Error("delete account: no media reclaimer is wired; erased account's objects remain on the backend",
				"tier1_objects", len(stranded.tier1), "tier2_objects", len(stranded.tier2))
		}
		return
	}
	detached, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), erasedMediaReclaimTimeout)
	defer cancel()
	s.reclaimMedia(detached, stranded.tier1, stranded.tier2)
}

// transferDrainedActivePlans is the post-commit half of the erasure drain
// (#2448): one proportional clear frame per category the drain removed, at most
// two for one user.
//
// The obligation is TRANSFERRED, not discarded. Deleting the plan row without
// this would discharge a durable privacy repair on paper and leave the erased
// principal's Server Voice / Private Call activity resident on every viewer
// that already received it -- the exact residue the rail exists to retract.
//
// It must run after the commit, never inside deleteAccountTx: the reconciler's
// terminal is a WebSocket fan-out, and emitting it from inside the transaction
// would tell viewers about an erasure that can still roll back.
func (s *AccountService) transferDrainedActivePlans(
	ctx context.Context,
	drained drainedObligation,
) {
	if s.activePlans == nil || len(drained.categories) == 0 {
		return
	}
	s.activePlans.ClearDrained(ctx, drained.subject, drained.categories)
}

func (s *AccountService) deleteAccountTx(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
	candidates []uuid.UUID,
	survivorSubjects *[]uuid.UUID,
) (pq.StringArray, pq.StringArray, presencecapture.Plan, drainedObligation, erasedMedia, error) {
	creatorID, lockedUserID, err := lockErasureSubjectsAndConversations(ctx, tx, userID, candidates)
	if err != nil {
		return nil, nil, nil, drainedObligation{}, erasedMedia{}, err
	}
	currentCandidates, err := readCurrentErasurePrivateCallCandidates(ctx, tx, creatorID, candidates)
	if err != nil {
		return nil, nil, nil, drainedObligation{}, erasedMedia{}, err
	}
	*survivorSubjects = append((*survivorSubjects)[:0], currentCandidates...)

	// Capture-before-cascade, under the user-row lock, with nothing between it
	// and the DELETE below that could change the audience. user_presence_settings
	// and presence_settings_pending_operations are both ON DELETE CASCADE, so the
	// rows defining this audience are destroyed by that DELETE.
	//
	// This reconciles the SURVIVING senders whose audiences shrink. The erased
	// principal's own state is handled separately and synchronously, by
	// SuppressAllActivityAlreadyGated before this transaction opens.
	plan, captureErr := s.captureErasureAlreadyGated(ctx, tx, lockedUserID)
	if captureErr != nil {
		return nil, nil, nil, drainedObligation{}, erasedMedia{}, fmt.Errorf(
			"delete account: capture presence: %w", captureErr)
	}

	// Drain the durable active-category obligation under the user-row lock, and
	// BEFORE the DELETE below.
	//
	// presence_active_pending_plans is ON DELETE RESTRICT (migration 000111),
	// so without this the DELETE raises an opaque 23503 and a GDPR erasure
	// fails on a presence marker with no diagnosable operator symptom. Draining
	// first is migration 000110's fail-closed-with-a-diagnosable-error
	// precedent: if the drain itself fails, this returns a wrapped error naming
	// the drain rather than letting Postgres report a constraint nobody
	// interpreting the 500 would recognise.
	drained, drainErr := s.drainActivePlansAlreadyGated(ctx, tx, lockedUserID)
	if drainErr != nil {
		return nil, nil, plan, drainedObligation{}, erasedMedia{}, drainErr
	}
	if err := s.captureErasurePrivateCallPlans(ctx, tx, currentCandidates); err != nil {
		return nil, nil, plan, drainedObligation{}, erasedMedia{}, err
	}

	// Capture the object-storage half under the user-row lock, and BEFORE the
	// two deletes below -- both of which destroy rows that name objects.
	//
	// Ahead of deleteIncompleteChannelsTx, not merely ahead of the users
	// DELETE: media_files.channel_id is ON DELETE CASCADE (migration 000042),
	// so dropping this user's incomplete E2EE channels already removes the
	// attachment rows inside them. Capturing after that call would miss exactly
	// the rows it destroyed.
	//
	// A capture failure FAILS THE ERASURE, and it has no choice. A failed
	// statement poisons a PostgreSQL transaction (25P02), so "log it and carry
	// on" would not carry on -- the DELETE below would fail anyway, with an
	// opaque 25P02 in place of the real cause. The alternative is a SAVEPOINT
	// (graphpresence's FailConservativeDegrade shape), which buys a degrade this
	// path does not want: the erasure is retryable and idempotent, so a caller
	// that retries gets both the erasure and its reclamation, where a degrade
	// would silently leak tier-1 plaintext with nothing to sweep it. Wrapping
	// the error names the capture, per migration 000110's precedent.
	stranded, mediaErr := s.captureErasedMedia(ctx, tx, lockedUserID)
	if mediaErr != nil {
		return nil, nil, plan, drainedObligation{}, erasedMedia{},
			fmt.Errorf("delete account: capture media: %w", mediaErr)
	}

	channelIDs, serverIDs, err := deleteIncompleteChannelsTx(ctx, tx, lockedUserID)
	if err != nil {
		return nil, nil, plan, drainedObligation{}, erasedMedia{}, err
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM users WHERE id = $1`,
		lockedUserID,
	)
	if err != nil {
		return nil, nil, plan, drainedObligation{}, erasedMedia{}, fmt.Errorf("delete account: delete user: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, nil, plan, drainedObligation{}, erasedMedia{}, fmt.Errorf("delete account: rows affected: %w", err)
	}
	if affected == 0 {
		return nil, nil, plan, drainedObligation{}, erasedMedia{}, ErrUserNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_deletions (user_id) VALUES (NULL)`,
	); err != nil {
		return nil, nil, plan, drainedObligation{}, erasedMedia{}, fmt.Errorf("delete account: insert audit: %w", err)
	}
	return channelIDs, serverIDs, plan, drained, stranded, nil
}

func lockErasureSubjectsAndConversations(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
	candidates []uuid.UUID,
) (creatorID uuid.UUID, lockedUserID string, returnErr error) {
	var err error
	creatorID, err = uuid.Parse(userID)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("delete account: parse user: %w", err)
	}
	lockedSubjects := append([]uuid.UUID{creatorID}, candidates...)
	if err := dm.LockPrivateVoiceScopesTx(ctx, tx, lockedSubjects); err != nil {
		return uuid.Nil, "", fmt.Errorf("delete account: lock private voice scopes: %w", err)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`,
		pq.Array(lockedSubjects))
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("delete account: lock users: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("delete account: close locked users: %w", closeErr))
		}
	}()
	var foundCreator bool
	for rows.Next() {
		var subject uuid.UUID
		if err := rows.Scan(&subject); err != nil {
			return uuid.Nil, "", fmt.Errorf("delete account: scan locked user: %w", err)
		}
		if subject == creatorID {
			foundCreator = true
		}
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, "", fmt.Errorf("delete account: iterate locked users: %w", err)
	}
	if !foundCreator {
		return uuid.Nil, "", ErrUserNotFound
	}
	if err := lockErasureConversations(ctx, tx, creatorID); err != nil {
		return uuid.Nil, "", err
	}
	return creatorID, creatorID.String(), nil
}

func lockErasureConversations(ctx context.Context, tx *sql.Tx, creatorID uuid.UUID) (returnErr error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM dm_conversations WHERE created_by = $1 ORDER BY id FOR UPDATE`, creatorID)
	if err != nil {
		return fmt.Errorf("delete account: lock conversations: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("delete account: close locked conversations: %w", closeErr))
		}
	}()
	for rows.Next() {
		var conversationID uuid.UUID
		if err := rows.Scan(&conversationID); err != nil {
			return fmt.Errorf("delete account: scan locked conversation: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("delete account: iterate locked conversations: %w", err)
	}
	return nil
}

func readCurrentErasurePrivateCallCandidates(
	ctx context.Context,
	tx *sql.Tx,
	creatorID uuid.UUID,
	candidates []uuid.UUID,
) ([]uuid.UUID, error) {
	currentCandidates, overflow, err := readErasurePrivateCallCandidates(ctx, tx, creatorID)
	if err != nil {
		return nil, fmt.Errorf("delete account: reread private-call candidates: %w", err)
	}
	if overflow {
		return nil, ErrErasureCandidateSetDrifted
	}
	preGatedCandidates := make(map[uuid.UUID]struct{}, len(candidates))
	for _, candidate := range candidates {
		preGatedCandidates[candidate] = struct{}{}
	}
	for _, candidate := range currentCandidates {
		if _, ok := preGatedCandidates[candidate]; !ok {
			return nil, ErrErasureCandidateSetDrifted
		}
	}
	return currentCandidates, nil
}

func (s *AccountService) captureErasurePrivateCallPlans(
	ctx context.Context,
	tx *sql.Tx,
	candidates []uuid.UUID,
) error {
	if len(candidates) == 0 {
		return nil
	}
	if s.activePlans == nil {
		return errors.New("delete account: private-call erasure rail unavailable")
	}
	if err := s.activePlans.CapturePrivateCallPlansAlreadyGated(ctx, tx, candidates); err != nil {
		return fmt.Errorf("delete account: capture private-call plans: %w", err)
	}
	return nil
}

// captureErasedMedia reads the objects this erasure is about to strand, split
// by tier. See erasedMediaQuery for what the narrowing admits and why.
func (s *AccountService) captureErasedMedia(
	ctx context.Context,
	tx *sql.Tx,
	lockedUserID string,
) (erasedMedia, error) {
	var out erasedMedia
	rows, err := tx.QueryContext(ctx, erasedMediaQuery,
		lockedUserID, pq.Array(media.ErasableTier1Keys(lockedUserID)))
	if err != nil {
		return erasedMedia{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var tier int
		var key string
		var backend sql.NullString
		if err := rows.Scan(&tier, &key, &backend); err != nil {
			return erasedMedia{}, err
		}
		ref := media.NewBlobRef(key, backend)
		// The query admits only tiers 1 and 2, so anything not tier 1 is tier 2
		// by construction.
		if tier == media.MediaTierAuthenticated {
			out.tier1 = append(out.tier1, ref)
			continue
		}
		out.tier2 = append(out.tier2, ref)
	}
	if err := rows.Err(); err != nil {
		return erasedMedia{}, err
	}
	return out, nil
}

// deleteIncompleteChannelsTx removes the E2EE channels this user created but
// never finished distributing keys for, and reports them so the post-commit
// caller can notify their server members.
func deleteIncompleteChannelsTx(
	ctx context.Context,
	tx *sql.Tx,
	lockedUserID string,
) (pq.StringArray, pq.StringArray, error) {
	var channelIDs, serverIDs pq.StringArray
	if err := tx.QueryRowContext(ctx, `
		WITH incomplete AS (
			SELECT channel_id
			FROM channel_initial_key_distributions
			WHERE creator_id = $1
		)
		SELECT COALESCE(array_agg(c.id::text ORDER BY c.id), ARRAY[]::text[]),
		       COALESCE(array_agg(c.server_id::text ORDER BY c.id), ARRAY[]::text[])
		FROM channels c
		WHERE c.id IN (SELECT channel_id FROM incomplete)
		   OR c.linked_voice_channel_id IN (SELECT channel_id FROM incomplete)`, lockedUserID,
	).Scan(&channelIDs, &serverIDs); err != nil {
		return nil, nil, fmt.Errorf("delete account: list incomplete channels: %w", err)
	}
	if len(channelIDs) == 0 {
		return channelIDs, serverIDs, nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channels WHERE id = ANY($1::uuid[])`, pq.Array(channelIDs),
	); err != nil {
		return nil, nil, fmt.Errorf("delete account: delete incomplete channels: %w", err)
	}
	return channelIDs, serverIDs, nil
}

// drainActivePlansAlreadyGated removes this user's outstanding #2448
// active-category obligations on a transaction whose sender gate the caller
// ALREADY holds.
//
// DrainAlreadyGated, never the rail's WithGatedTx, for the reason
// captureErasureAlreadyGated documents below: DeleteAccount already runs inside
// presenceHistory.WithSender, and the rail shares that same buffered-1 gate
// array, so the gated flavour would block on a stripe this goroutine holds --
// forever, with no timeout and no detector.
//
// An unwired drain returns no obligation rather than an error. That is a
// deliberate degrade to pre-#2448 behaviour for a replica whose wiring line was
// deleted, and it is safe ONLY because RESTRICT still fails the erasure loudly
// if such a replica meets a user who owes a plan; the boot guard is what stops
// it happening in production.
func (s *AccountService) drainActivePlansAlreadyGated(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
) (drainedObligation, error) {
	if s.activePlans == nil {
		return drainedObligation{}, nil
	}
	parsed, err := uuid.Parse(userID)
	if err != nil {
		// Fail closed: draining uuid.Nil would delete nothing, report nothing,
		// and let the DELETE below fail on the 23503 this exists to prevent.
		return drainedObligation{}, fmt.Errorf(
			"delete account: drain presence plans: parse subject: %w", err)
	}
	categories, err := s.activePlans.DrainAlreadyGated(ctx, tx, parsed)
	if err != nil {
		return drainedObligation{}, fmt.Errorf(
			"delete account: drain presence plans: %w", err)
	}
	return drainedObligation{subject: parsed, categories: categories}, nil
}

// SetActivePlanRail wires the #2448 durable active-category rail so erasure can
// drain an outstanding obligation instead of hitting the FK RESTRICT.
func (s *AccountService) SetActivePlanRail(rail ActivePlanDrain) { s.activePlans = rail }

// HasActivePlanDrain reports whether the drain is wired. The router's boot guard
// interrogates the SERVICE through this, never the constructed rail, for the
// same reason HasGraphPresenceCapture does: activepresence.NewRail never returns
// nil, so a check on that value is a tautology that still boots with the
// SetActivePlanRail line deleted. Unwired, an erasure of a user with an
// outstanding plan fails with an undiagnosable 23503.
func (s *AccountService) HasActivePlanDrain() bool { return s.activePlans != nil }

// SetGraphPresenceCapture wires the #2447 erasure presence capture.
func (s *AccountService) SetGraphPresenceCapture(c presencecapture.GraphPresenceCapture) {
	s.graphPresence = c
}

// HasGraphPresenceCapture reports whether the capture was wired. The router's
// boot guard interrogates the SERVICE through this, never the constructed
// reconciler value, for the same reason the handler guards do.
func (s *AccountService) HasGraphPresenceCapture() bool { return s.graphPresence != nil }

// HasErasureClearPublisher reports whether the erasure-clear publisher is wired.
//
// The boot guard interrogates this because the publish is not a cross-replica
// nicety — it is the ONLY mechanism that retracts an erased user's already
// delivered Custom Status, including on the publishing replica. The durable C2
// rail provably cannot cover this sender: its marker path locks the sender's
// users row first, and after the erasure commits that row is gone, so delivery
// is skipped with no error at all.
//
// FAILING CLOSED HERE IS STILL RIGHT. #2854 finding A did not change that; it
// changed what a nil MEANS. Before `RetryOnFailedConnect` (pkg/nats.Connect) a
// nil could be a transient bus outage, and fataling on that took auth and
// health down with it and crash-looped self-hosted and dev deployments. It can
// no longer: an outage now yields a reconnecting client, so a nil arriving here
// is a DELETED WIRING LINE or an unparseable NATS_URL — both deterministic
// deploy defects that never fix themselves, which is exactly what a boot guard
// is for.
//
// DO NOT weaken this to a "was SetNATS called" flag to make some future
// outage-shaped crash-loop go away. That was built and rejected in #2854 stage
// A, for three reasons found by three separate reviewers: it boots on a
// misconfiguration too; a nil client silently disables the voice permission
// enforcer's dispatch (internal/voice/permission_enforcer.go), so a banned or
// permission-revoked member keeps talking in a live room; and it removes the
// only observer of `s.nats`'s assignment, after which deleting `s.nats = c`
// kills the erasure clear on every replica with every test still green. Fix the
// connection, not the guard.
func (s *AccountService) HasErasureClearPublisher() bool { return s.nats != nil }

// SetNATS wires the cross-replica erasure-clear publisher.
func (s *AccountService) SetNATS(c *natsclient.Client) { s.nats = c }

// NATSSubjectPresenceErasureCleared fans an account erasure out to every replica
// so viewers still holding the erased principal's Custom Status drop it.
//
// It is exported and lives here, beside its only publisher, because the
// subscriber is in another package: a second unexported copy of the literal
// would let the two drift and publish into a subject nothing listens on.
//
// Core NATS, so at-most-once. See publishErasureCleared for the residual.
const NATSSubjectPresenceErasureCleared = "presence.erasure.cleared"

// captureErasureAlreadyGated captures the erased principal's pre-cascade
// audience on a transaction whose sender gate the caller ALREADY holds.
//
// Deliberately NOT presencehook.WithGatedTx. DeleteAccount already runs inside
// presenceHistory.WithSender, which takes a buffered-1 channel stripe keyed on
// this user; WithGatedTx would reach rail.WithSenders, hash to the same stripe
// and block on it forever. That deadlock has no timeout and no detector, because
// half the cycle is a Go channel rather than a database lock. The AlreadyGated
// naming convention exists for exactly this hazard.
//
// The family is FamilyMemberRemove: erasure revokes shared-server visibility and
// carries the Custom Status topology, which is precisely that family's policy. A
// dedicated erasure family would say the same two things while adding a fifth
// value to a positionally dense enum that two slices are already appending to.
func (s *AccountService) captureErasureAlreadyGated(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
) (presencecapture.Plan, error) {
	if s.graphPresence == nil {
		return nil, nil
	}
	parsed, err := uuid.Parse(userID)
	if err != nil {
		// Fail closed: capturing against uuid.Nil would drop the principal from
		// the focal set silently and reconcile nothing.
		return nil, fmt.Errorf("parse erasure principal: %w", err)
	}
	return s.graphPresence.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyMemberRemove,
		FailPosture: presencecapture.FailClosedBlockWrite,
		Principal:   parsed,
	})
}

// publishErasureCleared tells every replica to drop the erased principal's
// already-delivered Custom Status.
//
// Why this exists at all: SuppressAllActivityAlreadyGated deletes only
// CategoryServerVoice and CategoryPrivateCall and never touches Custom Status,
// and the disconnect it falls back on is Hub.DisconnectAllRichPresenceClients,
// which closes every LOCAL client. The Hub holds no NATS connection and no Redis
// pub/sub, so without this a viewer on another replica keeps the erased user's
// status text indefinitely -- Custom Status carries no TTL and is not
// republished on a heartbeat.
//
// A push rather than a fence. presence_offline_fences cannot help here: its
// user_id is REFERENCES users(id) ON DELETE CASCADE, so a fence row for an
// erased user is destroyed by the very DELETE it would need to outlive. A fence
// is also the wrong shape -- it denies re-authorization on the emission path and
// never retracts what was already delivered.
//
// Best-effort by design: the error is logged, not returned. The account IS
// erased by this point, so failing the request would invite a retry against a
// user row that no longer exists.
//
// It takes NO context, deliberately. An earlier version guarded on
// ctx.Err() using the inbound REQUEST context, which meant a client that
// disconnected between the commit and this publish silently skipped the clear
// entirely (Gitar review, PR #2840). This runs AFTER the erasure has committed,
// so it is a compensating action: cancellation of the originating request must
// never be a reason to skip it. There is nothing to cancel anyway --
// Client.Publish is a non-blocking local buffer write with no context parameter.
//
// ponytail: core NATS, at-most-once. Named residual -- a replica that misses
// this publish AND holds a viewer connected continuously never re-derives. Every
// other path self-heals, because after erasure there is nothing left to
// re-derive against. internal/attestation accepts the same semantics for
// registry revocation. Upgrade path if that residual ever matters: JetStream, or
// a durable FK-free clear ledger the replicas replay on reconnect.
func (s *AccountService) publishErasureCleared(userID string) {
	if s.nats == nil {
		return
	}
	if err := s.nats.Publish(NATSSubjectPresenceErasureCleared, map[string]interface{}{
		"user_id": userID,
	}); err != nil && s.log != nil {
		s.log.Error("Cross-replica presence clear failed", "failure_class", "delivery")
	}
}
