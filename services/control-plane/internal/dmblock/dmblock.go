// Package dmblock owns the durable SQL rail that converges blocked pairs out
// of shared DM conversations. It deliberately has no dependency on friends,
// dm handlers, or websocket delivery so every topology writer can use the same
// lock/guard leaves without importing a handler package.
package dmblock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ErrUnavailable is deliberately non-diagnostic: callers must fail closed
// without disclosing whether a conversation, pair, or obligation exists.
var ErrUnavailable = errors.New("dm block reconciliation unavailable")

// ErrMembershipChanged requires the caller to restart its complete topology
// transaction because the participant snapshot changed while it was locked.
var ErrMembershipChanged = errors.New("dm block conversation membership changed")

// LockMode selects the row lock used for the canonical users-first prefix.
type LockMode uint8

const (
	// LockShare permits concurrent readers while excluding conflicting writers.
	LockShare LockMode = iota
	// LockNoKeyUpdate excludes participant topology writes without key-row locks.
	LockNoKeyUpdate
	// LockUpdate takes the strongest users-row lock for destructive writers.
	LockUpdate
)

const (
	// maxDMSubjects is the reconciliation/lock bound, not the product cap.
	// Group DMs cap at 10 participants, but the legacy AddMember count/insert
	// race could produce 11-member conversations. Keep the operational bound at
	// 16 so those conversations remain removable; larger sets fail closed.
	maxDMSubjects                = 16
	maxReconcileBatch            = 100
	maxPairConversations         = 16
	securityEjectionDrainTimeout = 10 * time.Second
)

// Reconciler is the bounded durable-obligation worker. It is driven by the
// existing active-presence tick; it never owns a goroutine.
type Reconciler struct {
	db                     *sql.DB
	log                    *logger.Logger
	attachmentRetirer      AttachmentRetirer
	notifier               ReconciliationNotifier
	voiceEject             func(context.Context, string, uuid.UUID) error
	credentialEpochEject   func(context.Context, uuid.UUID, string, string) error
	voiceEjectV2           func(context.Context, string, uuid.UUID, uuid.UUID) error
	credentialEpochEjectV2 func(context.Context, uuid.UUID, string, string, uuid.UUID) error
}

// AttachmentRetirer captures the Tier-2 metadata a conversation deletion would
// cascade, then retires its backing blobs only after the deletion commits.
// A boot-time adapter forwards this seam to purge.Engine without creating an
// import cycle because purge already uses dmblock's transaction guards.
type AttachmentRetirer interface {
	CaptureConversationBlobsTx(context.Context, *sql.Tx, string) ([]string, []AttachmentBlobRef, error)
	EnqueueBlobDeletes([]AttachmentBlobRef)
}

// AttachmentBlobRef names one Tier-2 object without importing media, which
// would cycle through the websocket package back into dmblock.
type AttachmentBlobRef struct {
	Key     string
	Backend *string
}

// ReconciliationNotifier publishes the existing DM topology events after a
// blocked-pair transaction has committed. Delivery is a best-effort UI refresh
// hint, so an unavailable notifier must never undo or retry durable removal.
// The adapter lives above this package to avoid importing websocket here.
type ReconciliationNotifier interface {
	ParticipantRemoved(context.Context, string, uuid.UUID) error
	GroupDeleted(context.Context, string, []uuid.UUID) error
	RoleChanged(context.Context, string, uuid.UUID) error
	KeyRevocation(context.Context, string, int, string) error
}

// New constructs the durable blocked-pair reconciliation worker.
func New(db *sql.DB, log *logger.Logger) *Reconciler { return &Reconciler{db: db, log: log} }

// SetVoiceEject installs the media delivery terminal. Each target remains in
// the durable outbox until this callback succeeds; nil retains it for retry.
func (r *Reconciler) SetVoiceEject(eject func(context.Context, string, uuid.UUID) error) {
	if r != nil {
		r.voiceEject = eject
	}
}

// SetCredentialEpochEject installs the durable credential-rotation media terminal.
func (r *Reconciler) SetCredentialEpochEject(eject func(context.Context, uuid.UUID, string, string) error) {
	if r != nil {
		r.credentialEpochEject = eject
	}
}

// SetVoiceEjectV2 receives the immutable parent generation. The legacy setter
// remains for existing callers, but durable session delivery must use this
// form so a late acknowledgement cannot delete a successor obligation.
func (r *Reconciler) SetVoiceEjectV2(eject func(context.Context, string, uuid.UUID, uuid.UUID) error) {
	if r != nil {
		r.voiceEjectV2 = eject
	}
}

// SetCredentialEpochEjectV2 is the generation-fenced counterpart for epoch
// ejections.
func (r *Reconciler) SetCredentialEpochEjectV2(eject func(context.Context, uuid.UUID, string, string, uuid.UUID) error) {
	if r != nil {
		r.credentialEpochEjectV2 = eject
	}
}

// SetPurgeEngine installs the attachment-retirement rail shared with ordinary
// message and group deletion. An empty conversation refuses deletion without
// it so a cascade can never orphan an encrypted attachment object.
func (r *Reconciler) SetPurgeEngine(retirer AttachmentRetirer) {
	if r != nil {
		r.attachmentRetirer = retirer
	}
}

// SetReconciliationNotifier installs the post-commit DM topology notifier.
func (r *Reconciler) SetReconciliationNotifier(notifier ReconciliationNotifier) {
	if r != nil {
		r.notifier = notifier
	}
}

// RecordBlockTx is the handler-facing SQL leaf. A reconciler is not required
// to record an obligation, so synchronous friendship writes do not gain a
// lifecycle dependency merely to persist durable evidence.
func RecordBlockTx(ctx context.Context, tx *sql.Tx, blockerID, blockedID, operationID string) error {
	return recordBlockTx(ctx, tx, blockerID, blockedID, operationID)
}

// LockBlockSubjectsTx is the block-transition fence. It conflicts with the
// participant trigger's FOR SHARE read so a legacy/direct participant insert
// cannot commit across the durable blocked-pair obligation.
func LockBlockSubjectsTx(ctx context.Context, tx *sql.Tx, subjects []uuid.UUID) error {
	return lockSubjectsModeTx(ctx, tx, subjects, LockNoKeyUpdate)
}

func lockSubjectsModeTx(ctx context.Context, tx *sql.Tx, subjects []uuid.UUID, mode LockMode) (returnErr error) {
	if tx == nil || len(subjects) == 0 {
		return ErrUnavailable
	}
	seen := make(map[uuid.UUID]struct{}, len(subjects))
	ids := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		if subject == uuid.Nil {
			return ErrUnavailable
		}
		if _, ok := seen[subject]; !ok {
			seen[subject] = struct{}{}
			ids = append(ids, subject.String())
		}
	}
	if len(ids) > maxDMSubjects {
		return ErrUnavailable
	}
	sort.Strings(ids)
	var query string
	switch mode {
	case LockShare:
		query = `SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR SHARE`
	case LockNoKeyUpdate:
		query = `SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR NO KEY UPDATE`
	case LockUpdate:
		query = `SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`
	default:
		return ErrUnavailable
	}
	rows, err := tx.QueryContext(ctx, query, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("dm block lock subjects: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("dm block close subject locks: %w", closeErr))
		}
	}()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dm block iterate subjects: %w", err)
	}
	if count != len(ids) {
		return ErrUnavailable
	}
	return nil
}

// PrepareNewConversationTx fences proposed DM membership before a conversation
// parent exists. It must run before credential checks or any participant/key
// write, because it owns the complete sorted users-first prefix.
func PrepareNewConversationTx(ctx context.Context, tx *sql.Tx, subjects []uuid.UUID, userMode LockMode) error {
	if tx == nil {
		return ErrUnavailable
	}
	if err := lockSubjectsModeTx(ctx, tx, subjects, userMode); err != nil {
		return err
	}
	return guardBlockedSubjectsTx(ctx, tx, subjects)
}

type conversationPreparationOptions struct {
	extraUsers   []uuid.UUID
	userMode     LockMode
	parentMode   LockMode
	guardBlocked bool
	lockUsers    bool
}

// PrepareConversationTx is the only users-before-parent preparation leaf for
// DM sinks. ExtraUsers includes the actor before its credential guard. A drift
// returns ErrMembershipChanged so callers restart the complete transaction.
func PrepareConversationTx(ctx context.Context, tx *sql.Tx, conversationID string, extraUsers []uuid.UUID, userMode, parentMode LockMode) ([]uuid.UUID, error) {
	return prepareConversationTx(ctx, tx, conversationID, conversationPreparationOptions{
		extraUsers:   extraUsers,
		userMode:     userMode,
		parentMode:   parentMode,
		guardBlocked: true,
		lockUsers:    true,
	})
}

// LockConversationUsersTx takes the complete users-first prefix without
// locking the parent. Callers that also need an advisory lock must take it
// after this function and then complete the transaction with
// PrepareConversationAfterUserLocksTx; that preserves users -> advisory ->
// parent ordering while retaining the blocked-pair fence.
func LockConversationUsersTx(ctx context.Context, tx *sql.Tx, conversationID string, extraUsers []uuid.UUID, userMode LockMode) ([]uuid.UUID, error) {
	if tx == nil {
		return nil, ErrUnavailable
	}
	subjects, err := conversationSubjectsTx(ctx, tx, conversationID)
	if err != nil {
		return nil, err
	}
	lockSet := append(append([]uuid.UUID(nil), subjects...), extraUsers...)
	if err := lockSubjectsModeTx(ctx, tx, lockSet, userMode); err != nil {
		return nil, err
	}
	return subjects, nil
}

// PrepareConversationAfterUserLocksTx locks and revalidates a conversation
// after the caller acquired the exact users-first snapshot. It intentionally
// never takes another user lock: an intervening membership change must restart
// the transaction rather than leave a new participant outside that prefix.
func PrepareConversationAfterUserLocksTx(ctx context.Context, tx *sql.Tx, conversationID string, expectedSubjects []uuid.UUID, parentMode LockMode) ([]uuid.UUID, error) {
	if tx == nil || len(expectedSubjects) == 0 {
		return nil, ErrUnavailable
	}
	var parentQuery string
	switch parentMode {
	case LockShare:
		parentQuery = `SELECT id FROM dm_conversations WHERE id = $1 FOR SHARE`
	case LockNoKeyUpdate:
		parentQuery = `SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`
	case LockUpdate:
		parentQuery = `SELECT id FROM dm_conversations WHERE id = $1 FOR UPDATE`
	default:
		return nil, ErrUnavailable
	}
	var id string
	if err := tx.QueryRowContext(ctx, parentQuery, conversationID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUnavailable
		}
		return nil, fmt.Errorf("dm block lock conversation: %w", err)
	}
	fresh, err := conversationSubjectsTx(ctx, tx, conversationID)
	if err != nil {
		return nil, err
	}
	if !sameSubjects(expectedSubjects, fresh) {
		return nil, ErrMembershipChanged
	}
	if err := guardBlockedConversationTx(ctx, tx, conversationID); err != nil {
		return nil, err
	}
	return fresh, nil
}

// PrepareResolutionConversationTx is solely for a destructive operation that
// removes a participant or the entire group. It takes the same exact
// users-before-parent lock prefix as PrepareConversationTx but deliberately
// does not decide which member a legacy blocked pair should lose. The caller
// must revalidate its authority after this call and, when the conversation
// remains, call ValidateConversationResolvedTx after its delete.
func PrepareResolutionConversationTx(ctx context.Context, tx *sql.Tx, conversationID string, extraUsers []uuid.UUID, userMode, parentMode LockMode) ([]uuid.UUID, error) {
	return prepareConversationTx(ctx, tx, conversationID, conversationPreparationOptions{
		extraUsers: extraUsers,
		userMode:   userMode,
		parentMode: parentMode,
		lockUsers:  true,
	})
}

// ValidateConversationResolvedTx proves a destructive resolution left no
// blocked co-membership. It intentionally does not alter reconciliation
// evidence: legacy direction remains unknowable and is never synthesized.
func ValidateConversationResolvedTx(ctx context.Context, tx *sql.Tx, conversationID string) error {
	if tx == nil {
		return ErrUnavailable
	}
	return guardBlockedConversationTx(ctx, tx, conversationID)
}

func prepareConversationTx(ctx context.Context, tx *sql.Tx, conversationID string, options conversationPreparationOptions) ([]uuid.UUID, error) {
	if tx == nil {
		return nil, ErrUnavailable
	}
	subjects, err := conversationSubjectsTx(ctx, tx, conversationID)
	if err != nil {
		return nil, err
	}
	snapshot := append([]uuid.UUID(nil), subjects...)
	if options.lockUsers {
		lockSet := append(append([]uuid.UUID(nil), snapshot...), options.extraUsers...)
		if err := lockSubjectsModeTx(ctx, tx, lockSet, options.userMode); err != nil {
			return nil, err
		}
	}
	var parentQuery string
	switch options.parentMode {
	case LockShare:
		parentQuery = `SELECT id FROM dm_conversations WHERE id = $1 FOR SHARE`
	case LockNoKeyUpdate:
		parentQuery = `SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`
	case LockUpdate:
		parentQuery = `SELECT id FROM dm_conversations WHERE id = $1 FOR UPDATE`
	default:
		return nil, ErrUnavailable
	}
	var id string
	if err := tx.QueryRowContext(ctx, parentQuery, conversationID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUnavailable
		}
		return nil, fmt.Errorf("dm block lock conversation: %w", err)
	}
	fresh, err := conversationSubjectsTx(ctx, tx, conversationID)
	if err != nil {
		return nil, err
	}
	if !sameSubjects(snapshot, fresh) {
		return nil, ErrMembershipChanged
	}
	if options.guardBlocked {
		if err := guardBlockedConversationTx(ctx, tx, conversationID); err != nil {
			return nil, err
		}
	}
	return fresh, nil
}

func conversationSubjectsTx(ctx context.Context, tx *sql.Tx, conversationID string) (subjects []uuid.UUID, returnErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT user_id FROM dm_participants WHERE conversation_id = $1 ORDER BY user_id LIMIT $2`, conversationID, maxDMSubjects+1)
	if err != nil {
		return nil, fmt.Errorf("dm block list conversation subjects: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("dm block close conversation subjects: %w", closeErr))
		}
	}()
	subjects = make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("dm block scan conversation subject: %w", err)
		}
		subjects = append(subjects, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dm block iterate conversation subjects: %w", err)
	}
	if len(subjects) == 0 || len(subjects) > maxDMSubjects {
		return nil, ErrUnavailable
	}
	return subjects, nil
}

// prepareWorkerConversationTx is deliberately separate from the public guard:
// its own matching obligation is proof cleanup is required, not a reason to
// refuse cleanup. It still validates that exact operation after every lock.
func prepareWorkerConversationTx(ctx context.Context, tx *sql.Tx, conversationID string, obligation claimedObligation) ([]uuid.UUID, error) {
	if tx == nil {
		return nil, ErrUnavailable
	}
	if !obligation.removeA && !obligation.removeB {
		return nil, ErrUnavailable
	}
	snapshot, err := conversationSubjectsTx(ctx, tx, conversationID)
	if err != nil {
		return nil, err
	}
	lockSet := append(append([]uuid.UUID(nil), snapshot...), obligation.a, obligation.b)
	if err := LockBlockSubjectsTx(ctx, tx, lockSet); err != nil {
		return nil, err
	}
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, conversationID).Scan(&id); err != nil {
		return nil, ErrUnavailable
	}
	fresh, err := conversationSubjectsTx(ctx, tx, conversationID)
	if err != nil {
		return nil, err
	}
	if !sameSubjects(snapshot, fresh) {
		return nil, ErrMembershipChanged
	}
	if !slices.Contains(fresh, obligation.a) || !slices.Contains(fresh, obligation.b) {
		return nil, ErrMembershipChanged
	}
	var currentA, currentB bool
	if err := tx.QueryRowContext(ctx, `SELECT remove_a, remove_b FROM dm_block_reconciliations WHERE user_a_id=$1 AND user_b_id=$2 AND operation_id=$3 FOR KEY SHARE`, obligation.a, obligation.b, obligation.operationID).Scan(&currentA, &currentB); err != nil {
		return nil, ErrUnavailable
	}
	if currentA != obligation.removeA || currentB != obligation.removeB {
		return nil, ErrUnavailable
	}
	return fresh, nil
}

func guardBlockedConversationTx(ctx context.Context, tx *sql.Tx, conversationID string) error {
	var blocked bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM dm_participants a JOIN dm_participants b
			  ON a.conversation_id = b.conversation_id AND a.user_id < b.user_id
			JOIN friendships f ON (f.requester_id = a.user_id AND f.addressee_id = b.user_id)
			  OR (f.requester_id = b.user_id AND f.addressee_id = a.user_id)
			WHERE a.conversation_id = $1 AND f.status = 'blocked'
			UNION ALL
			SELECT 1 FROM dm_block_reconciliations r
			JOIN dm_participants a ON a.user_id = r.user_a_id AND a.conversation_id = $1
			JOIN dm_participants b ON b.user_id = r.user_b_id AND b.conversation_id = $1
		)`, conversationID).Scan(&blocked); err != nil {
		return fmt.Errorf("dm block guard conversation: %w", err)
	}
	if blocked {
		return ErrUnavailable
	}
	return nil
}

func guardBlockedSubjectsTx(ctx context.Context, tx *sql.Tx, subjects []uuid.UUID) error {
	ids := make([]string, 0, len(subjects))
	seen := make(map[uuid.UUID]struct{}, len(subjects))
	for _, subject := range subjects {
		if subject == uuid.Nil {
			return ErrUnavailable
		}
		if _, ok := seen[subject]; !ok {
			seen[subject] = struct{}{}
			ids = append(ids, subject.String())
		}
	}
	if len(ids) == 0 || len(ids) > maxDMSubjects {
		return ErrUnavailable
	}
	var blocked bool
	if err := tx.QueryRowContext(ctx, `
		WITH subjects AS (SELECT DISTINCT unnest($1::uuid[]) AS user_id)
		SELECT EXISTS (
			SELECT 1 FROM subjects a JOIN subjects b ON a.user_id < b.user_id
			JOIN friendships f ON (f.requester_id = a.user_id AND f.addressee_id = b.user_id)
				OR (f.requester_id = b.user_id AND f.addressee_id = a.user_id)
			WHERE f.status = 'blocked'
			UNION ALL
			SELECT 1 FROM subjects a JOIN subjects b ON a.user_id < b.user_id
			JOIN dm_block_reconciliations r ON r.user_a_id = a.user_id AND r.user_b_id = b.user_id
		)`, pq.Array(ids)).Scan(&blocked); err != nil {
		return fmt.Errorf("dm block guard subjects: %w", err)
	}
	if blocked {
		return ErrUnavailable
	}
	return nil
}

func sameSubjects(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func recordBlockTx(ctx context.Context, tx *sql.Tx, blockerID, blockedID, operationID string) error {
	if tx == nil {
		return ErrUnavailable
	}
	a, err := uuid.Parse(blockerID)
	if err != nil || a == uuid.Nil {
		return ErrUnavailable
	}
	b, err := uuid.Parse(blockedID)
	if err != nil || b == uuid.Nil || a == b {
		return ErrUnavailable
	}
	op, err := uuid.Parse(operationID)
	if err != nil || op == uuid.Nil {
		return ErrUnavailable
	}
	removeA := a.String() < b.String()
	if !removeA {
		a, b = b, a
	}
	if err := LockBlockSubjectsTx(ctx, tx, []uuid.UUID{a, b}); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, remove_a, remove_b, operation_id, attempts, failure_class, reconcile_after, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 0, NULL, clock_timestamp(), NOW(), NOW())
		ON CONFLICT (user_a_id, user_b_id) DO UPDATE
		SET remove_a = dm_block_reconciliations.remove_a OR EXCLUDED.remove_a,
		    remove_b = dm_block_reconciliations.remove_b OR EXCLUDED.remove_b,
		    operation_id = EXCLUDED.operation_id, attempts = 0, failure_class = NULL,
		    reconcile_after = EXCLUDED.reconcile_after, updated_at = clock_timestamp()`, a, b, removeA, !removeA, op)
	if err != nil {
		return fmt.Errorf("dm block upsert obligation: %w", err)
	}
	return nil
}

type claimedObligation struct {
	a, b        uuid.UUID
	removeA     bool
	removeB     bool
	operationID uuid.UUID
}

// claimedVoiceEjection is an independent durable media outbox entry. It has
// no foreign keys because the conversation and account can be deleted before
// the media plane consumes the disconnect request.
type claimedVoiceEjection struct {
	conversationID string
	userID         uuid.UUID
	generation     uuid.UUID
}

// ReconcileDue claims due evidence briefly, then converges each conversation
// in a separate transaction. A worker crash therefore retains evidence and a
// later worker can continue from the last committed conversation.
func (r *Reconciler) ReconcileDue(ctx context.Context, limit int) (int, error) {
	if r == nil || r.db == nil || limit <= 0 {
		return 0, ErrUnavailable
	}
	if limit > maxReconcileBatch {
		limit = maxReconcileBatch
	}
	credentialCtx, cancelCredential := context.WithTimeout(ctx, securityEjectionDrainTimeout)
	voiceCtx, cancelVoice := context.WithTimeout(ctx, securityEjectionDrainTimeout)
	credentialResults := make(chan error, 1)
	voiceResults := make(chan error, 1)
	go func() { credentialResults <- r.drainDueCredentialEpochEjections(credentialCtx, limit) }()
	go func() { voiceResults <- r.drainDueVoiceEjections(voiceCtx, limit) }()
	credentialErr := <-credentialResults
	voiceErr := <-voiceResults
	cancelCredential()
	cancelVoice()
	if credentialErr != nil || voiceErr != nil {
		var drainErr error
		if credentialErr != nil {
			drainErr = errors.Join(drainErr, fmt.Errorf("dm block drain credential media ejections: %w", credentialErr))
		}
		if voiceErr != nil {
			drainErr = errors.Join(drainErr, fmt.Errorf("dm block drain media ejections: %w", voiceErr))
		}
		return 0, drainErr
	}
	claimed, err := r.claimDue(ctx, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, obligation := range claimed {
		if err := r.reconcilePair(ctx, obligation); err != nil {
			if retryErr := r.retryPair(ctx, obligation); retryErr != nil {
				return count, fmt.Errorf("dm block retry obligation: %w", retryErr)
			}
			if r.log != nil {
				r.log.Error("DM block reconciliation retained", "failure_class", "database")
			}
			continue
		}
		count++
	}
	postPairVoiceCtx, cancelPostPairVoice := context.WithTimeout(ctx, securityEjectionDrainTimeout)
	postPairVoiceErr := r.drainDueVoiceEjections(postPairVoiceCtx, limit)
	cancelPostPairVoice()
	if postPairVoiceErr != nil {
		return count, fmt.Errorf("dm block drain media ejections: %w", postPairVoiceErr)
	}
	return count, nil
}

type claimedCredentialEpochEjection struct {
	userID                                     uuid.UUID
	credentialEpoch, supersededCredentialEpoch string
	generation                                 uuid.UUID
}

// DeliverCredentialEpochVoiceEjection claims one committed, due durable row.
// It is the targeted post-commit hook; no row means rollback or an already ACKed delivery.
func (r *Reconciler) DeliverCredentialEpochVoiceEjection(ctx context.Context, userID, credentialEpoch, supersededCredentialEpoch string) error {
	if r == nil || r.db == nil {
		return ErrUnavailable
	}
	claimed, err := r.claimDueCredentialEpochEjections(ctx, 1, &userID, &credentialEpoch, &supersededCredentialEpoch)
	if err != nil || len(claimed) == 0 {
		return err
	}
	return r.deliverCredentialEpochEjection(ctx, claimed[0])
}

func (r *Reconciler) drainDueCredentialEpochEjections(ctx context.Context, limit int) error {
	claimed, err := r.claimDueCredentialEpochEjections(ctx, limit, nil, nil, nil)
	if err != nil {
		return err
	}
	for _, ejection := range claimed {
		if err := r.deliverCredentialEpochEjection(ctx, ejection); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) deliverCredentialEpochEjection(ctx context.Context, e claimedCredentialEpochEjection) error {
	if r.credentialEpochEjectV2 != nil || r.credentialEpochEject != nil {
		var deliveryErr error
		if r.credentialEpochEjectV2 != nil {
			deliveryErr = r.credentialEpochEjectV2(ctx, e.userID, e.credentialEpoch, e.supersededCredentialEpoch, e.generation)
		} else {
			deliveryErr = r.credentialEpochEject(ctx, e.userID, e.credentialEpoch, e.supersededCredentialEpoch)
		}
		if deliveryErr == nil {
			result, deleteErr := r.db.ExecContext(ctx, `DELETE FROM credential_epoch_voice_ejections WHERE user_id = $1 AND superseded_credential_epoch = $2 AND generation = $3`, e.userID, e.supersededCredentialEpoch, e.generation)
			if deleteErr != nil {
				return fmt.Errorf("acknowledge credential media ejection: %w", deleteErr)
			}
			changed, countErr := result.RowsAffected()
			if countErr != nil {
				return fmt.Errorf("count acknowledged credential media ejection: %w", countErr)
			}
			if changed > 1 {
				return ErrUnavailable
			}
			return nil
		}
	}
	result, err := r.db.ExecContext(ctx, `UPDATE credential_epoch_voice_ejections SET failure_class = 'delivery', reconcile_after = clock_timestamp() + CASE WHEN attempts >= 10 THEN INTERVAL '1 hour' ELSE INTERVAL '1 minute' END, updated_at = clock_timestamp() WHERE user_id = $1 AND superseded_credential_epoch = $2 AND generation = $3`, e.userID, e.supersededCredentialEpoch, e.generation)
	if err != nil {
		return fmt.Errorf("retry credential media ejection: %w", err)
	}
	changed, countErr := result.RowsAffected()
	if countErr != nil {
		return fmt.Errorf("count retried credential media ejection: %w", countErr)
	}
	if changed > 1 {
		return ErrUnavailable
	}
	if r.log != nil {
		r.log.Error("Credential media ejection retained", "failure_class", "delivery")
	}
	return nil
}

func (r *Reconciler) claimDueCredentialEpochEjections(ctx context.Context, limit int, userID, credentialEpoch, old *string) (claimed []claimedCredentialEpochEjection, returnErr error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > maxReconcileBatch {
		limit = maxReconcileBatch
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin credential media ejection claim: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, rollbackErr)
		}
	}()
	query := `SELECT user_id, credential_epoch, superseded_credential_epoch, generation FROM credential_epoch_voice_ejections WHERE reconcile_after <= clock_timestamp()`
	args := []any{limit}
	if userID != nil {
		query += ` AND user_id = $1 AND credential_epoch = $2 AND superseded_credential_epoch = $3 ORDER BY reconcile_after LIMIT $4 FOR UPDATE SKIP LOCKED`
		args = []any{*userID, *credentialEpoch, *old, limit}
	} else {
		query += ` ORDER BY reconcile_after LIMIT $1 FOR UPDATE SKIP LOCKED`
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list due credential media ejections: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := rows.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close due credential media ejections: %w", closeErr))
			}
		}
	}()
	for rows.Next() {
		var e claimedCredentialEpochEjection
		if err := rows.Scan(&e.userID, &e.credentialEpoch, &e.supersededCredentialEpoch, &e.generation); err != nil {
			return nil, fmt.Errorf("scan due credential media ejection: %w", err)
		}
		claimed = append(claimed, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due credential media ejections: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close due credential media ejections: %w", err)
	}
	closed = true
	for _, e := range claimed {
		result, err := tx.ExecContext(ctx, `UPDATE credential_epoch_voice_ejections SET attempts=attempts+1,reconcile_after=clock_timestamp()+INTERVAL '30 seconds',updated_at=clock_timestamp() WHERE user_id=$1 AND superseded_credential_epoch=$2 AND generation=$3`, e.userID, e.supersededCredentialEpoch, e.generation)
		if err != nil {
			return nil, fmt.Errorf("lease credential media ejection: %w", err)
		}
		changed, countErr := result.RowsAffected()
		if countErr != nil {
			return nil, fmt.Errorf("count leased credential media ejection: %w", countErr)
		}
		if changed != 1 {
			return nil, ErrUnavailable
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit credential media ejection claim: %w", err)
	}
	return claimed, nil
}

func (r *Reconciler) claimDue(ctx context.Context, limit int) (claimed []claimedObligation, returnErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("dm block begin claim: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("dm block rollback claim: %w", rollbackErr))
		}
	}()
	rows, err := tx.QueryContext(ctx, `
		SELECT user_a_id, user_b_id, remove_a, remove_b, operation_id
		FROM dm_block_reconciliations
		WHERE reconcile_after <= clock_timestamp()
		ORDER BY reconcile_after, user_a_id, user_b_id
		LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("dm block list due obligations: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := rows.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("dm block close due obligations: %w", closeErr))
			}
		}
	}()
	claimed = make([]claimedObligation, 0, limit)
	for rows.Next() {
		var obligation claimedObligation
		if err := rows.Scan(&obligation.a, &obligation.b, &obligation.removeA, &obligation.removeB, &obligation.operationID); err != nil {
			return nil, fmt.Errorf("dm block scan due obligation: %w", err)
		}
		claimed = append(claimed, obligation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dm block iterate due obligations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("dm block close due obligations: %w", err)
	}
	closed = true
	for _, obligation := range claimed {
		result, err := tx.ExecContext(ctx, `
			UPDATE dm_block_reconciliations
			SET reconcile_after = clock_timestamp() + INTERVAL '30 seconds', updated_at = clock_timestamp()
			WHERE user_a_id = $1 AND user_b_id = $2 AND operation_id = $3`, obligation.a, obligation.b, obligation.operationID)
		if err != nil {
			return nil, fmt.Errorf("dm block lease obligation: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return nil, ErrUnavailable
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("dm block commit claim: %w", err)
	}
	return claimed, nil
}

func (r *Reconciler) reconcilePair(ctx context.Context, obligation claimedObligation) error {
	conversationIDs, err := r.sharedConversationIDs(ctx, obligation.a, obligation.b)
	if err != nil {
		return err
	}
	var reconcileErr error
	for _, conversationID := range conversationIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.reconcileConversation(ctx, conversationID, obligation); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if reconcileErr == nil {
				reconcileErr = err
			}
		}
	}
	if reconcileErr != nil {
		return reconcileErr
	}
	return r.acknowledgeOrReschedule(ctx, obligation)
}

func (r *Reconciler) sharedConversationIDs(ctx context.Context, a, b uuid.UUID) (ids []string, returnErr error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT a.conversation_id FROM dm_participants a
		JOIN dm_participants b ON b.conversation_id = a.conversation_id
		WHERE a.user_id = $1 AND b.user_id = $2 ORDER BY a.conversation_id LIMIT $3`, a, b, maxPairConversations)
	if err != nil {
		return nil, fmt.Errorf("dm block list shared conversations: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("dm block close shared conversations: %w", closeErr))
		}
	}()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("dm block scan shared conversation: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dm block iterate shared conversations: %w", err)
	}
	return ids, nil
}

func (r *Reconciler) reconcileConversation(ctx context.Context, conversationID string, obligation claimedObligation) (returnErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dm block begin conversation: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("dm block rollback conversation: %w", rollbackErr))
		}
	}()
	var refs []AttachmentBlobRef
	var outcome reconciliationOutcome
	err = r.reconcileConversationTx(ctx, tx, conversationID, obligation, &refs, &outcome)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dm block commit conversation: %w", err)
	}
	if len(refs) > 0 {
		r.attachmentRetirer.EnqueueBlobDeletes(refs)
	}
	r.notifyReconciliation(ctx, outcome)
	return nil
}

type reconciliationOutcome struct {
	conversationID     string
	removedUserIDs     []uuid.UUID
	groupDeleted       bool
	newCreatorID       *uuid.UUID
	keyRevocationEpoch int
}

func (r *Reconciler) reconcileConversationTx(ctx context.Context, tx *sql.Tx, conversationID string, obligation claimedObligation, refsOut *[]AttachmentBlobRef, outcomeOut *reconciliationOutcome) error {
	if _, err := prepareWorkerConversationTx(ctx, tx, conversationID, obligation); err != nil {
		return err
	}
	var epoch int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(key_version), 0) FROM dm_channel_keys WHERE conversation_id = $1`, conversationID).Scan(&epoch); err != nil {
		return fmt.Errorf("dm block read key epoch: %w", err)
	}
	var creator string
	if err := tx.QueryRowContext(ctx, `SELECT created_by FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, conversationID).Scan(&creator); err != nil {
		return err
	}
	removeIDs := make([]uuid.UUID, 0, 2)
	var survivor string
	removed := make([]string, 0, 2)
	if obligation.removeA {
		removed = append(removed, obligation.a.String())
		removeIDs = append(removeIDs, obligation.a)
	}
	if obligation.removeB {
		removed = append(removed, obligation.b.String())
		removeIDs = append(removeIDs, obligation.b)
	}
	if err := captureRemovedVoicePlansTx(ctx, tx, conversationID, removeIDs); err != nil {
		return err
	}
	if err := EnqueueVoiceEjectionsTx(ctx, tx, conversationID, removeIDs); err != nil {
		return err
	}
	err := tx.QueryRowContext(ctx, `SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id <> ALL($2::uuid[]) ORDER BY CASE WHEN role = 'admin' THEN 0 ELSE 1 END, joined_at, user_id LIMIT 1`, conversationID, pq.Array(removed)).Scan(&survivor)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		refs, err := r.deleteEmptyConversationTx(ctx, tx, conversationID)
		if err != nil {
			return err
		}
		if refsOut != nil {
			*refsOut = refs
		}
		if outcomeOut != nil {
			*outcomeOut = reconciliationOutcome{
				conversationID: conversationID,
				removedUserIDs: append([]uuid.UUID(nil), removeIDs...),
				groupDeleted:   true,
			}
		}
		return nil
	case err != nil:
		return fmt.Errorf("dm block select successor: %w", err)
	}
	creatorChanged := containsString(removed, creator)
	if err := reconcileNonEmptyConversationTx(ctx, tx, conversationID, survivor, creatorChanged, removeIDs); err != nil {
		return err
	}
	if outcomeOut != nil {
		outcome := reconciliationOutcome{
			conversationID: conversationID,
			removedUserIDs: append([]uuid.UUID(nil), removeIDs...),
		}
		if creatorChanged {
			newCreatorID, err := uuid.Parse(survivor)
			if err != nil {
				return ErrUnavailable
			}
			outcome.newCreatorID = &newCreatorID
		}
		if epoch > 0 {
			outcome.keyRevocationEpoch = epoch
		}
		*outcomeOut = outcome
	}
	return nil
}

func (r *Reconciler) notifyReconciliation(ctx context.Context, outcome reconciliationOutcome) {
	if r == nil || r.notifier == nil || outcome.conversationID == "" || len(outcome.removedUserIDs) == 0 {
		return
	}
	var err error
	if outcome.groupDeleted {
		err = r.notifier.GroupDeleted(ctx, outcome.conversationID, outcome.removedUserIDs)
	} else {
		for _, userID := range outcome.removedUserIDs {
			if notifyErr := r.notifier.ParticipantRemoved(ctx, outcome.conversationID, userID); notifyErr != nil {
				err = errors.Join(err, notifyErr)
			}
		}
		if outcome.newCreatorID != nil {
			if notifyErr := r.notifier.RoleChanged(ctx, outcome.conversationID, *outcome.newCreatorID); notifyErr != nil {
				err = errors.Join(err, notifyErr)
			}
		}
		if outcome.keyRevocationEpoch > 0 {
			if notifyErr := r.notifier.KeyRevocation(ctx, outcome.conversationID, outcome.keyRevocationEpoch, "user_blocked"); notifyErr != nil {
				err = errors.Join(err, notifyErr)
			}
		}
	}
	if err != nil && r.log != nil {
		r.log.Error("DM block topology notification not delivered", "failure_class", "delivery")
	}
}

func reconcileNonEmptyConversationTx(ctx context.Context, tx *sql.Tx, conversationID, survivor string, creatorChanged bool, removeIDs []uuid.UUID) error {
	if creatorChanged {
		if _, err := tx.ExecContext(ctx, `UPDATE dm_conversations SET created_by = $1 WHERE id = $2`, survivor, conversationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, conversationID, survivor); err != nil {
			return err
		}
	}
	for _, userID := range removeIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dm_read_states WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID); err != nil {
			return err
		}
	}
	return nil
}

func captureRemovedVoicePlansTx(ctx context.Context, tx *sql.Tx, conversationID string, removeIDs []uuid.UUID) (returnErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT user_id FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = ANY($2::uuid[])
		ORDER BY user_id FOR UPDATE`, conversationID, pq.Array(removeIDs))
	if err != nil {
		return fmt.Errorf("dm block list removed voice participants: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := rows.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("dm block close removed voice participants: %w", closeErr))
			}
		}
	}()
	var active []uuid.UUID
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			return fmt.Errorf("dm block scan removed voice participant: %w", err)
		}
		active = append(active, userID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dm block iterate removed voice participants: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("dm block close removed voice participants: %w", err)
	}
	closed = true
	now := time.Now()
	for _, userID := range active {
		if err := activepresence.InsertPlanTx(ctx, tx, activepresence.Plan{
			SubjectID: userID, Category: activepresence.CategoryPrivateCall,
			OperationID: uuid.New(), Resolution: activepresence.ResolutionConservative,
			EventAt: now,
		}); err != nil {
			return fmt.Errorf("dm block capture private-call cleanup: %w", err)
		}
	}
	return nil
}

// EnqueueVoiceEjectionsTx records every removed membership, including an
// offline member with no dm_voice_participants row. The active-presence plan
// above is deliberately narrower: it only needs evidence of an active call.
func EnqueueVoiceEjectionsTx(ctx context.Context, tx *sql.Tx, conversationID string, removeIDs []uuid.UUID) error {
	for _, userID := range removeIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dm_block_voice_ejections
				(conversation_id, user_id, generation, attempts, failure_class, reconcile_after, created_at, updated_at)
			VALUES ($1, $2, gen_random_uuid(), 0, NULL, clock_timestamp(), NOW(), NOW())
			ON CONFLICT (conversation_id, user_id) DO UPDATE
			SET generation = gen_random_uuid(), attempts = 0, failure_class = NULL,
				reconcile_after = clock_timestamp(), updated_at = clock_timestamp()`, conversationID, userID); err != nil {
			return fmt.Errorf("dm block enqueue media ejection: %w", err)
		}
	}
	return nil
}

func (r *Reconciler) deleteEmptyConversationTx(ctx context.Context, tx *sql.Tx, conversationID string) ([]AttachmentBlobRef, error) {
	if r == nil || r.attachmentRetirer == nil {
		return nil, errors.New("dm block empty conversation: purge engine is unavailable")
	}
	fileIDs, refs, err := r.attachmentRetirer.CaptureConversationBlobsTx(ctx, tx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("dm block capture empty conversation attachments: %w", err)
	}
	statements := []string{
		`DELETE FROM dm_voice_participants WHERE conversation_id = $1`,
		`DELETE FROM dm_read_states WHERE conversation_id = $1`,
		`DELETE FROM dm_pending_key_requests WHERE conversation_id = $1`,
		`DELETE FROM dm_key_revocations WHERE conversation_id = $1`,
		`DELETE FROM dm_channel_keys WHERE conversation_id = $1`,
		`DELETE FROM dm_messages WHERE conversation_id = $1`,
		`DELETE FROM dm_participants WHERE conversation_id = $1`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, conversationID); err != nil {
			return nil, fmt.Errorf("dm block delete empty conversation children: %w", err)
		}
	}
	if err := ensureNoAttachmentBridges(ctx, tx, fileIDs); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM dm_conversations WHERE id = $1`, conversationID); err != nil {
		return nil, fmt.Errorf("dm block delete empty conversation: %w", err)
	}
	return refs, nil
}

func ensureNoAttachmentBridges(ctx context.Context, tx *sql.Tx, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return nil
	}
	var fileID string
	err := tx.QueryRowContext(ctx, `
		SELECT file_id FROM (
			SELECT file_id FROM message_attachments WHERE file_id = ANY($1::uuid[])
			UNION ALL
			SELECT file_id FROM dm_message_attachments WHERE file_id = ANY($1::uuid[])
		) AS remaining LIMIT 1`, pq.Array(fileIDs)).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dm block check remaining attachment bridges: %w", err)
	}
	return fmt.Errorf("dm block empty conversation: attachment %s remains referenced", fileID)
}

func containsString(ids []string, wanted string) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}

// drainDueVoiceEjections is intentionally independent of pair-marker state:
// deleting a conversation must not erase a media disconnect that was already
// committed. A nil callback is a delivery failure: acknowledging it would
// permanently lose a committed media ejection before a publisher is wired.
func (r *Reconciler) drainDueVoiceEjections(ctx context.Context, limit int) error {
	claimed, err := r.claimDueVoiceEjections(ctx, limit)
	if err != nil {
		return err
	}
	for _, ejection := range claimed {
		if r.voiceEjectV2 == nil && r.voiceEject == nil {
			if retryErr := r.retryVoiceEjection(ctx, ejection); retryErr != nil {
				return fmt.Errorf("retain unwired media ejection: %w", retryErr)
			}
			if r.log != nil {
				r.log.Error("DM block media ejection retained", "failure_class", "delivery")
			}
			continue
		}
		var deliveryErr error
		if r.voiceEjectV2 != nil {
			deliveryErr = r.voiceEjectV2(ctx, ejection.conversationID, ejection.userID, ejection.generation)
		} else {
			deliveryErr = r.voiceEject(ctx, ejection.conversationID, ejection.userID)
		}
		if deliveryErr != nil {
			if retryErr := r.retryVoiceEjection(ctx, ejection); retryErr != nil {
				return fmt.Errorf("retain failed media ejection: %w", retryErr)
			}
			if r.log != nil {
				r.log.Error("DM block media ejection retained", "failure_class", "delivery")
			}
			continue
		}
		if err := r.acknowledgeVoiceEjection(ctx, ejection); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) claimDueVoiceEjections(ctx context.Context, limit int) (claimed []claimedVoiceEjection, returnErr error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > maxReconcileBatch {
		limit = maxReconcileBatch
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin media ejection claim: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("dm block rollback media ejection claim: %w", rollbackErr))
		}
	}()
	rows, err := tx.QueryContext(ctx, `
		SELECT conversation_id, user_id, generation FROM dm_block_voice_ejections
		WHERE reconcile_after <= clock_timestamp()
		ORDER BY reconcile_after, conversation_id, user_id
		LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("list due media ejections: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := rows.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close due media ejections: %w", closeErr))
			}
		}
	}()
	claimed = make([]claimedVoiceEjection, 0, limit)
	for rows.Next() {
		var ejection claimedVoiceEjection
		if err := rows.Scan(&ejection.conversationID, &ejection.userID, &ejection.generation); err != nil {
			return nil, fmt.Errorf("scan due media ejection: %w", err)
		}
		claimed = append(claimed, ejection)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due media ejections: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close due media ejections: %w", err)
	}
	closed = true
	for _, ejection := range claimed {
		result, err := tx.ExecContext(ctx, `
			UPDATE dm_block_voice_ejections
			SET attempts = attempts + 1,
				reconcile_after = clock_timestamp() + INTERVAL '30 seconds', updated_at = clock_timestamp()
			WHERE conversation_id = $1 AND user_id = $2`, ejection.conversationID, ejection.userID)
		if err != nil {
			return nil, fmt.Errorf("lease media ejection: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return nil, ErrUnavailable
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit media ejection claim: %w", err)
	}
	return claimed, nil
}

func (r *Reconciler) acknowledgeVoiceEjection(ctx context.Context, ejection claimedVoiceEjection) error {
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM dm_block_voice_ejections
		WHERE conversation_id = $1 AND user_id = $2 AND generation = $3`, ejection.conversationID, ejection.userID, ejection.generation)
	if err != nil {
		return fmt.Errorf("acknowledge media ejection: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count acknowledged media ejection: %w", err)
	}
	if changed > 1 {
		return ErrUnavailable
	}
	return nil
}

func (r *Reconciler) retryVoiceEjection(ctx context.Context, ejection claimedVoiceEjection) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE dm_block_voice_ejections
		SET failure_class = 'delivery',
			reconcile_after = clock_timestamp() + CASE WHEN attempts >= 10 THEN INTERVAL '1 hour' ELSE INTERVAL '1 minute' END,
			updated_at = clock_timestamp()
		WHERE conversation_id = $1 AND user_id = $2 AND generation = $3`, ejection.conversationID, ejection.userID, ejection.generation)
	if err != nil {
		return fmt.Errorf("retry media ejection: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count retried media ejection: %w", err)
	}
	if changed > 1 {
		return ErrUnavailable
	}
	return nil
}

func (r *Reconciler) acknowledgeOrReschedule(ctx context.Context, obligation claimedObligation) (returnErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dm block begin acknowledgement: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("dm block rollback acknowledgement: %w", rollbackErr))
		}
	}()
	if err := LockBlockSubjectsTx(ctx, tx, []uuid.UUID{obligation.a, obligation.b}); err != nil {
		return err
	}
	var removeA, removeB bool
	if err := tx.QueryRowContext(ctx, `
		SELECT remove_a, remove_b FROM dm_block_reconciliations
		WHERE user_a_id = $1 AND user_b_id = $2 AND operation_id = $3 FOR UPDATE`,
		obligation.a, obligation.b, obligation.operationID).Scan(&removeA, &removeB); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // a newer operation owns this pair now.
		}
		return fmt.Errorf("dm block lock acknowledgement: %w", err)
	}
	if removeA != obligation.removeA || removeB != obligation.removeB {
		return nil
	}
	var remains bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM dm_participants a
			JOIN dm_participants b ON b.conversation_id = a.conversation_id
			WHERE a.user_id = $1 AND b.user_id = $2
		)`, obligation.a, obligation.b).Scan(&remains); err != nil {
		return fmt.Errorf("dm block check acknowledgement: %w", err)
	}
	if remains {
		_, err = tx.ExecContext(ctx, `
			UPDATE dm_block_reconciliations SET reconcile_after = clock_timestamp(), updated_at = clock_timestamp()
			WHERE user_a_id = $1 AND user_b_id = $2 AND operation_id = $3`, obligation.a, obligation.b, obligation.operationID)
	} else {
		_, err = tx.ExecContext(ctx, `
			DELETE FROM dm_block_reconciliations
			WHERE user_a_id = $1 AND user_b_id = $2 AND operation_id = $3`, obligation.a, obligation.b, obligation.operationID)
	}
	if err != nil {
		return fmt.Errorf("dm block acknowledge obligation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dm block commit acknowledgement: %w", err)
	}
	return nil
}

func (r *Reconciler) retryPair(ctx context.Context, obligation claimedObligation) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE dm_block_reconciliations
		SET attempts = attempts + 1, failure_class = 'database',
		    reconcile_after = clock_timestamp() + CASE WHEN attempts >= 9 THEN INTERVAL '1 hour' ELSE INTERVAL '1 minute' END,
		    updated_at = clock_timestamp()
		WHERE user_a_id = $1 AND user_b_id = $2 AND operation_id = $3`,
		obligation.a, obligation.b, obligation.operationID)
	return err
}
