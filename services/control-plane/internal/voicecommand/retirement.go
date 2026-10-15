package voicecommand

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

const retirementConsumerName = "voice-enforcement-v1"

// RetirementState is a coherent SQL snapshot of the retained broker suffix.
type RetirementState struct {
	Allocated      uint64
	Published      uint64
	RetiredThrough uint64
	Pending        *RetirementIntent
}

// RetirementIntent permanently binds one bounded removal to verified evidence.
// Delivery metadata may change; the interval, receipt and consumer proof cannot.
type RetirementIntent struct {
	ID              uuid.UUID
	From            uint64
	Through         uint64
	Published       uint64
	AckFloor        uint64
	Receipt         TransportReceipt
	ConsumerCreated time.Time
	Attempts        int
	Due             bool
	FailureClass    string
}

type storedRetirement struct {
	intent      RetirementIntent
	preparedAt  time.Time
	completedAt sql.NullTime
}

// ReadRetirement holds the sequence lock only for its SQL snapshot. A caller
// never carries this transaction or any of its locks into broker work.
func (r *Repository) ReadRetirement(ctx context.Context) (state RetirementState, returnErr error) {
	if r == nil || r.DB == nil {
		return state, errors.New("voice command repository database unavailable")
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return state, fmt.Errorf("begin outbox retirement transaction: %w", err)
	}
	defer func() {
		returnErr = joinOutboxRollbackError(returnErr, tx.Rollback())
	}()
	state, err = lockRetirementState(ctx, tx)
	if err != nil {
		return state, err
	}
	if err := tx.Commit(); err != nil {
		return state, fmt.Errorf("commit outbox retirement snapshot: %w", err)
	}
	return state, nil
}

func lockRetirementState(ctx context.Context, tx *sql.Tx) (RetirementState, error) {
	var allocated, published, retired int64
	var pending sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT last_allocated_sequence, last_published_sequence,
		retired_through_sequence, pending_retirement_id FROM voice_command_sequence
		WHERE id = TRUE FOR UPDATE`).Scan(&allocated, &published, &retired, &pending)
	if err != nil {
		return RetirementState{}, fmt.Errorf("lock outbox retirement cursor: %w", err)
	}
	if allocated < 0 || published < 0 || retired < 0 {
		return RetirementState{}, ErrBrokerContinuity
	}
	if !validSequenceCounters(allocated, published) || (retired != 0 && retired >= published) {
		return RetirementState{}, ErrBrokerContinuity
	}
	state := RetirementState{Allocated: uint64(allocated), Published: uint64(published), RetiredThrough: uint64(retired)}
	if err := validateRetirementCheckpointTx(ctx, tx, state); err != nil {
		return state, err
	}
	if !pending.Valid {
		return state, nil
	}
	intent, err := readPendingRetirementTx(ctx, tx, pending.String, state)
	if err != nil {
		return state, err
	}
	state.Pending = &intent
	return state, nil
}

func validateRetirementCheckpointTx(ctx context.Context, tx *sql.Tx, state RetirementState) error {
	if state.RetiredThrough == 0 {
		return nil
	}
	stored, err := readRetirementRow(tx.QueryRowContext(ctx, completedRetirementQuery, state.RetiredThrough))
	if err != nil {
		return retirementReadError("read outbox retirement checkpoint proof", err)
	}
	if !stored.completedAt.Valid || stored.intent.Through != state.RetiredThrough || stored.intent.Published > state.Published {
		return ErrBrokerContinuity
	}
	return matchRetirementReceiptTx(ctx, tx, stored.intent.Receipt)
}

func readPendingRetirementTx(ctx context.Context, tx *sql.Tx, pending string, state RetirementState) (RetirementIntent, error) {
	id, err := uuid.Parse(pending)
	if err != nil || id == uuid.Nil || !canonicalUUID.MatchString(pending) {
		return RetirementIntent{}, ErrBrokerContinuity
	}
	stored, err := readRetirementRow(tx.QueryRowContext(ctx, retirementByIDQuery, id))
	if err != nil {
		return RetirementIntent{}, retirementReadError("read pending outbox retirement proof", err)
	}
	intent := stored.intent
	if stored.completedAt.Valid || intent.From != state.RetiredThrough || intent.Published != state.Published {
		return RetirementIntent{}, ErrBrokerContinuity
	}
	if err := matchRetirementReceiptTx(ctx, tx, intent.Receipt); err != nil {
		return RetirementIntent{}, err
	}
	return intent, nil
}

// PrepareRetirement recomputes the acknowledged boundary under the same lock
// as Claim. A live publisher excludes preparation; a prepared intent excludes
// later publishers until its bounded removal and checkpoint are completed.
func (r *Repository) PrepareRetirement(ctx context.Context, receipt TransportReceipt, consumerCreated time.Time, ackFloor uint64) (intent *RetirementIntent, returnErr error) {
	if !validRetirementProofInput(receipt, consumerCreated, ackFloor) {
		return nil, ErrBrokerContinuity
	}
	if r == nil || r.DB == nil {
		return nil, errors.New("voice command repository database unavailable")
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin outbox retirement transaction: %w", err)
	}
	defer func() {
		returnErr = joinOutboxRollbackError(returnErr, tx.Rollback())
	}()
	state, err := lockRetirementState(ctx, tx)
	if err != nil {
		return nil, err
	}
	if state.Pending != nil {
		return state.Pending, nil
	}
	if err := matchRetirementReceiptTx(ctx, tx, receipt); err != nil {
		return nil, err
	}
	through := retirementBoundary(state, ackFloor)
	if through <= state.RetiredThrough {
		return nil, nil
	}
	live, err := livePublicationClaimTx(ctx, tx)
	if err != nil || live {
		return nil, err
	}
	prepared, err := createRetirementIntent(state, receipt, consumerCreated, ackFloor, through)
	if err != nil {
		return nil, err
	}
	if err := insertRetirementIntentTx(ctx, tx, prepared); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return r.confirmPreparedRetirement(ctx, prepared, err)
	}
	return &prepared, nil
}

func validRetirementProofInput(receipt TransportReceipt, consumerCreated time.Time, ackFloor uint64) bool {
	return receipt.Generation != uuid.Nil && receipt.Identity.Valid() &&
		!consumerCreated.IsZero() && ackFloor <= math.MaxInt64
}

func retirementBoundary(state RetirementState, ackFloor uint64) uint64 {
	if state.Published <= 1 {
		return 0
	}
	return min(ackFloor, state.Published-1)
}

func livePublicationClaimTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var live bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM voice_command_outbox
		WHERE published_at IS NULL AND lease_until > clock_timestamp())`).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("check live outbox publication claim: %w", err)
	}
	return live, nil
}

func createRetirementIntent(state RetirementState, receipt TransportReceipt, consumerCreated time.Time, ackFloor, through uint64) (RetirementIntent, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return RetirementIntent{}, fmt.Errorf("create outbox retirement identity: %w", err)
	}
	return RetirementIntent{ID: id, From: state.RetiredThrough, Through: through,
		Published: state.Published, AckFloor: ackFloor, Receipt: receipt,
		ConsumerCreated: consumerCreated.UTC(), Due: true}, nil
}

func insertRetirementIntentTx(ctx context.Context, tx *sql.Tx, intent RetirementIntent) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO voice_command_retirement
		(intent_id, from_sequence, through_sequence, published_sequence, stream_generation,
		stream_created_at, owner_store_created_at, initializing_revision, consumer_stream,
		consumer_name, consumer_durable, consumer_created_at, consumer_profile_version,
		acknowledged_sequence) VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		'VOICE_COMMANDS_V1', 'voice-enforcement-v1', 'voice-enforcement-v1', $9, 1, $10)`,
		intent.ID, intent.From, intent.Through, intent.Published, intent.Receipt.Generation,
		intent.Receipt.Identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano),
		intent.Receipt.Identity.OwnerStoreCreatedAt.UTC().Format(time.RFC3339Nano),
		intent.Receipt.Identity.InitializingRevision, intent.ConsumerCreated.UTC().Format(time.RFC3339Nano), intent.AckFloor)
	if err != nil {
		return fmt.Errorf("prepare outbox retirement proof: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm outbox retirement proof: %w", err)
	}
	result, err = tx.ExecContext(ctx, `UPDATE voice_command_sequence
		SET pending_retirement_id = $1, updated_at = clock_timestamp()
		WHERE id = TRUE AND pending_retirement_id IS NULL
		AND retired_through_sequence = $2 AND last_published_sequence = $3`, intent.ID, intent.From, intent.Published)
	if err != nil {
		return fmt.Errorf("record pending outbox retirement: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm pending outbox retirement: %w", err)
	}
	return nil
}

func (r *Repository) confirmPreparedRetirement(ctx context.Context, intent RetirementIntent, cause error) (*RetirementIntent, error) {
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), receiptConfirmationBudget)
	defer cancel()
	stored, err := readRetirementRow(r.DB.QueryRowContext(confirmCtx, retirementByIDQuery, intent.ID))
	if err == nil && sameRetirementIntent(stored.intent, intent) {
		return &stored.intent, nil
	}
	return nil, errors.Join(fmt.Errorf("commit pending outbox retirement: %w", cause), err)
}

// CompleteRetirement persists a verified broker removal without advancing
// publication. Duplicate resumptions confirm the same immutable proof.
func (r *Repository) CompleteRetirement(ctx context.Context, intent RetirementIntent) (returnErr error) {
	if !validRetirementIntent(intent) {
		return ErrBrokerContinuity
	}
	if r == nil || r.DB == nil {
		return errors.New("voice command repository database unavailable")
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin outbox retirement transaction: %w", err)
	}
	defer func() {
		returnErr = joinOutboxRollbackError(returnErr, tx.Rollback())
	}()
	state, err := lockRetirementState(ctx, tx)
	if err != nil {
		return err
	}
	if state.RetiredThrough >= intent.Through {
		return confirmCompletedRetirementTx(ctx, tx, intent)
	}
	if state.Pending == nil || !sameRetirementIntent(*state.Pending, intent) {
		return ErrClaimLost
	}
	if err := completeRetirementTx(ctx, tx, intent); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return r.confirmCompletedRetirement(ctx, intent, err)
	}
	return nil
}

func completeRetirementTx(ctx context.Context, tx *sql.Tx, intent RetirementIntent) error {
	result, err := tx.ExecContext(ctx, `UPDATE voice_command_retirement
		SET completed_at = clock_timestamp(), failure_class = NULL
		WHERE intent_id = $1 AND completed_at IS NULL`, intent.ID)
	if err != nil {
		return fmt.Errorf("complete outbox retirement proof: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm completed outbox retirement proof: %w", err)
	}
	result, err = tx.ExecContext(ctx, `UPDATE voice_command_sequence
		SET retired_through_sequence = $2, pending_retirement_id = NULL, updated_at = clock_timestamp()
		WHERE id = TRUE AND pending_retirement_id = $1 AND retired_through_sequence = $3
		AND last_published_sequence = $4`, intent.ID, intent.Through, intent.From, intent.Published)
	if err != nil {
		return fmt.Errorf("advance outbox retirement checkpoint: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm outbox retirement checkpoint: %w", err)
	}
	return nil
}

func confirmCompletedRetirementTx(ctx context.Context, tx *sql.Tx, intent RetirementIntent) error {
	stored, err := readRetirementRow(tx.QueryRowContext(ctx, retirementByIDQuery, intent.ID))
	if err != nil {
		return retirementReadError("confirm completed outbox retirement", err)
	}
	if !stored.completedAt.Valid || !sameRetirementIntent(stored.intent, intent) {
		return ErrClaimLost
	}
	return nil
}

func (r *Repository) confirmCompletedRetirement(ctx context.Context, intent RetirementIntent, cause error) error {
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), receiptConfirmationBudget)
	defer cancel()
	state, err := r.ReadRetirement(confirmCtx)
	if err == nil && state.RetiredThrough >= intent.Through {
		stored, readErr := readRetirementRow(r.DB.QueryRowContext(confirmCtx, retirementByIDQuery, intent.ID))
		if readErr == nil && stored.completedAt.Valid && sameRetirementIntent(stored.intent, intent) {
			return nil
		}
		err = readErr
	}
	return errors.Join(fmt.Errorf("commit completed outbox retirement: %w", cause), err)
}

// RetryRetirement preserves the prepared interval and durable bounded backoff.
// Permanent continuity failures keep the publication barrier until repair.
func (r *Repository) RetryRetirement(ctx context.Context, intent RetirementIntent, class string, delay time.Duration) (returnErr error) {
	if !validRetirementIntent(intent) || !validFailureClass(class) || delay < 0 || delay > maximumRetryDelay {
		return errors.New("invalid voice command retirement retry parameters")
	}
	if r == nil || r.DB == nil {
		return errors.New("voice command repository database unavailable")
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin outbox retirement transaction: %w", err)
	}
	defer func() {
		returnErr = joinOutboxRollbackError(returnErr, tx.Rollback())
	}()
	state, err := lockRetirementState(ctx, tx)
	if err != nil {
		return err
	}
	if state.Pending == nil || !sameRetirementIntent(*state.Pending, intent) || permanentFailureClass(state.Pending.FailureClass) {
		return ErrClaimLost
	}
	result, err := tx.ExecContext(ctx, `UPDATE voice_command_retirement
		SET attempts = CASE WHEN attempts < 2147483647 THEN attempts + 1 ELSE attempts END,
		retry_at = clock_timestamp() + $2 * INTERVAL '1 microsecond', failure_class = $3
		WHERE intent_id = $1 AND completed_at IS NULL
		AND (failure_class IS NULL OR failure_class NOT IN ('broker_continuity', 'invalid_command'))`, intent.ID, delay.Microseconds(), class)
	if err != nil {
		return fmt.Errorf("retain pending outbox retirement retry: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm pending outbox retirement retry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pending outbox retirement retry: %w", err)
	}
	return nil
}

func validRetirementIntent(intent RetirementIntent) bool {
	return intent.ID != uuid.Nil && canonicalUUID.MatchString(intent.ID.String()) &&
		validRetirementProofInput(intent.Receipt, intent.ConsumerCreated, intent.AckFloor) &&
		intent.From < intent.Through && intent.Through < intent.Published &&
		intent.Published <= math.MaxInt64 && intent.AckFloor >= intent.Through
}

func sameRetirementIntent(left, right RetirementIntent) bool {
	return left.ID == right.ID && left.From == right.From && left.Through == right.Through &&
		left.Published == right.Published && left.AckFloor == right.AckFloor &&
		sameTransportReceipt(left.Receipt, right.Receipt) && left.ConsumerCreated.Equal(right.ConsumerCreated)
}

func sameTransportReceipt(left, right TransportReceipt) bool {
	return left.Generation == right.Generation &&
		left.Identity.InitializingRevision == right.Identity.InitializingRevision &&
		left.Identity.StreamCreatedAt.Equal(right.Identity.StreamCreatedAt) &&
		left.Identity.OwnerStoreCreatedAt.Equal(right.Identity.OwnerStoreCreatedAt)
}

func matchRetirementReceiptTx(ctx context.Context, tx *sql.Tx, receipt TransportReceipt) error {
	var generation uuid.UUID
	var stream, owner sql.NullString
	var revision sql.NullInt64
	var provisioned bool
	err := tx.QueryRowContext(ctx, `SELECT generation, provisioned_at IS NOT NULL,
		stream_created_at, owner_store_created_at, initializing_revision
		FROM voice_command_transport WHERE id = TRUE`).Scan(&generation, &provisioned, &stream, &owner, &revision)
	if err != nil {
		return fmt.Errorf("read retirement transport receipt: %w", err)
	}
	if !provisioned || !stream.Valid || !owner.Valid || !revision.Valid ||
		generation != receipt.Generation || revision.Int64 <= 0 || uint64(revision.Int64) != receipt.Identity.InitializingRevision ||
		stream.String != receipt.Identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano) ||
		owner.String != receipt.Identity.OwnerStoreCreatedAt.UTC().Format(time.RFC3339Nano) {
		return ErrBrokerContinuity
	}
	return nil
}

func retirementReadError(operation string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errors.Join(ErrBrokerContinuity, fmt.Errorf("%s: %w", operation, err))
	}
	return fmt.Errorf("%s: %w", operation, err)
}

const (
	retirementColumns = `intent_id, from_sequence, through_sequence, published_sequence,
	stream_generation, stream_created_at, owner_store_created_at, initializing_revision,
	consumer_stream, consumer_name, consumer_durable, consumer_created_at, consumer_profile_version,
	acknowledged_sequence, prepared_at, completed_at, attempts, failure_class,
	(retry_at <= clock_timestamp() AND (failure_class IS NULL OR failure_class NOT IN ('broker_continuity', 'invalid_command')))`
	retirementByIDQuery      = `SELECT ` + retirementColumns + ` FROM voice_command_retirement WHERE intent_id = $1`
	completedRetirementQuery = `SELECT ` + retirementColumns + ` FROM voice_command_retirement WHERE through_sequence = $1 AND completed_at IS NOT NULL`
)

func readRetirementRow(row commandRowScanner) (storedRetirement, error) {
	var stored storedRetirement
	var from, through, published, acknowledged, revision int64
	var streamCreated, ownerCreated, consumerCreated, consumerStream, consumerName, durable string
	var profile int
	var failure sql.NullString
	intent := &stored.intent
	err := row.Scan(&intent.ID, &from, &through, &published, &intent.Receipt.Generation,
		&streamCreated, &ownerCreated, &revision, &consumerStream, &consumerName, &durable,
		&consumerCreated, &profile, &acknowledged, &stored.preparedAt, &stored.completedAt,
		&intent.Attempts, &failure, &intent.Due)
	if err != nil {
		return stored, err
	}
	if from < 0 || through <= 0 || published <= 0 || acknowledged < 0 || revision <= 0 {
		return stored, ErrBrokerContinuity
	}
	if !validRetirementMetadata(stored, failure) ||
		consumerStream != StreamName || consumerName != retirementConsumerName || durable != retirementConsumerName || profile != 1 {
		return stored, ErrBrokerContinuity
	}
	intent.From, intent.Through, intent.Published = uint64(from), uint64(through), uint64(published)
	intent.AckFloor, intent.Receipt.Identity.InitializingRevision = uint64(acknowledged), uint64(revision)
	intent.FailureClass = failure.String
	if err := readRetirementBirths(intent, streamCreated, ownerCreated, consumerCreated); err != nil {
		return stored, err
	}
	if !validRetirementIntent(*intent) {
		return stored, ErrBrokerContinuity
	}
	return stored, nil
}

func validRetirementMetadata(stored storedRetirement, failure sql.NullString) bool {
	if stored.preparedAt.IsZero() || stored.intent.Attempts < 0 || stored.intent.Attempts > math.MaxInt32 ||
		(failure.Valid && !validFailureClass(failure.String)) {
		return false
	}
	return !stored.completedAt.Valid || (!stored.completedAt.Time.IsZero() && !failure.Valid)
}

func readRetirementBirths(intent *RetirementIntent, streamCreated, ownerCreated, consumerCreated string) error {
	var err error
	intent.Receipt.Identity.StreamCreatedAt, err = canonicalBrokerTime(streamCreated)
	if err != nil {
		return errors.Join(ErrBrokerContinuity, err)
	}
	intent.Receipt.Identity.OwnerStoreCreatedAt, err = canonicalBrokerTime(ownerCreated)
	if err != nil {
		return errors.Join(ErrBrokerContinuity, err)
	}
	intent.ConsumerCreated, err = canonicalBrokerTime(consumerCreated)
	if err != nil {
		return errors.Join(ErrBrokerContinuity, err)
	}
	return nil
}
