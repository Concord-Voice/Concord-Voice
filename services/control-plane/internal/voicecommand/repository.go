package voicecommand

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// ErrClaimLost distinguishes an expired or superseded worker from invalid evidence.
var ErrClaimLost = errors.New("voice command claim lost")

const (
	failureTransportUnavailable = "transport_unavailable"
	failureBrokerUnavailable    = "broker_unavailable"
	failureBrokerContinuity     = "broker_continuity"
	failureInvalidCommand       = "invalid_command"
	failureReceiptUnavailable   = "receipt_unavailable"
	claimLease                  = 20 * time.Second
	receiptConfirmationBudget   = 2 * time.Second
	maximumRetryDelay           = 30 * time.Second
)

// ClaimedCommand binds an immutable command to one bounded SQL ownership token.
type ClaimedCommand struct {
	Command        VoiceCommand
	Token          uuid.UUID
	Attempts       int
	RetiredThrough uint64
}

// Repository persists head ownership and permanent publication evidence.
type Repository struct {
	DB *sql.DB
}

// NewRepository binds head ownership and publication evidence to the SQL pool.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{DB: db}
}

// Claim serializes on the cursor before the head. A busy, delayed or blocked
// head stops the queue; no later sequence can be selected in its place.
func (r *Repository) Claim(ctx context.Context, token uuid.UUID, leaseDuration time.Duration) (claim *ClaimedCommand, returnErr error) {
	if r == nil || r.DB == nil {
		return nil, errors.New("voice command repository database unavailable")
	}
	if !validClaimParameters(token, leaseDuration) {
		return nil, errors.New("invalid voice command claim parameters")
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin outbox claim: %w", err)
	}
	defer func() {
		returnErr = joinOutboxRollbackError(returnErr, tx.Rollback())
	}()
	state, err := lockRetirementState(ctx, tx)
	if err != nil {
		return nil, err
	}
	if state.Pending != nil || state.Allocated == state.Published {
		return nil, nil
	}
	if state.Published > math.MaxInt64-1 {
		return nil, ErrBrokerContinuity
	}
	sequence := int64(state.Published) + 1
	stored, readErr := readCommandRow(tx.QueryRowContext(ctx, lockedCommandQuery, sequence))
	if errors.Is(readErr, sql.ErrNoRows) {
		return nil, ErrBrokerContinuity
	}
	if readErr != nil && !errors.Is(readErr, ErrInvalidCommand) {
		return nil, fmt.Errorf("read outbox claim head: %w", readErr)
	}
	if stored.publishedAt.Valid || stored.brokerSequence.Valid {
		return nil, ErrBrokerContinuity
	}
	if permanentCommandFailure(stored) {
		return nil, nil
	}
	if errors.Is(readErr, ErrInvalidCommand) || !validDeliveryMetadata(stored) {
		return nil, poisonHeadTx(ctx, tx, sequence)
	}
	var attempts int
	err = tx.QueryRowContext(ctx, `UPDATE voice_command_outbox
		SET lease_token = $2, lease_until = clock_timestamp() + $3 * INTERVAL '1 microsecond',
		attempts = CASE WHEN attempts < 2147483647 THEN attempts + 1 ELSE attempts END,
		failure_class = NULL
		WHERE sequence = $1 AND published_at IS NULL AND broker_sequence IS NULL
		AND retry_at <= clock_timestamp()
		AND (lease_until IS NULL OR lease_until <= clock_timestamp())
		AND (failure_class IS NULL OR failure_class NOT IN ('broker_continuity', 'invalid_command'))
		RETURNING attempts`, sequence, token, leaseDuration.Microseconds()).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lease outbox command head: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit outbox command claim: %w", err)
	}
	return &ClaimedCommand{Command: stored.command, Token: token, Attempts: attempts, RetiredThrough: state.RetiredThrough}, nil
}

func validClaimParameters(token uuid.UUID, leaseDuration time.Duration) bool {
	return token != uuid.Nil && leaseDuration >= time.Microsecond && leaseDuration <= claimLease
}

func poisonHeadTx(ctx context.Context, tx *sql.Tx, sequence int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE voice_command_outbox
		SET failure_class = 'invalid_command', lease_token = NULL, lease_until = NULL
		WHERE sequence = $1 AND published_at IS NULL AND broker_sequence IS NULL`, sequence)
	if err != nil {
		return errors.Join(ErrInvalidCommand, fmt.Errorf("retain invalid outbox command head: %w", err))
	}
	if err := requireOneChanged(result); err != nil {
		return errors.Join(ErrInvalidCommand, err)
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(ErrInvalidCommand, fmt.Errorf("commit invalid outbox head classification: %w", err))
	}
	return ErrInvalidCommand
}

// MarkPublished advances the row receipt and cursor atomically, only while the
// exact claim is live. A fresh permanent receipt can confirm a lost commit reply.
func (r *Repository) MarkPublished(ctx context.Context, claim ClaimedCommand, receipt PublishReceipt) (returnErr error) {
	if r == nil || r.DB == nil {
		return errors.New("voice command repository database unavailable")
	}
	command, sequence, err := validatedClaim(claim)
	if err != nil {
		return err
	}
	if receipt.Stream != StreamName || receipt.Sequence == 0 || receipt.Sequence > math.MaxInt64 ||
		strconv.FormatUint(receipt.Sequence, 10) != command.Sequence {
		return ErrBrokerContinuity
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin outbox publication receipt: %w", err)
	}
	defer func() {
		returnErr = joinOutboxRollbackError(returnErr, tx.Rollback())
	}()
	if err := lockOwnedHead(ctx, tx, claim, command, sequence); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE voice_command_outbox
		SET published_at = clock_timestamp(), broker_sequence = $3,
		lease_token = NULL, lease_until = NULL, failure_class = NULL
		WHERE sequence = $1 AND lease_token = $2 AND lease_until > clock_timestamp()
		AND published_at IS NULL AND broker_sequence IS NULL`, sequence, claim.Token, sequence)
	if err != nil {
		return fmt.Errorf("record outbox publication receipt: %w", err)
	}
	if err := requireOwnedWrite(result); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE voice_command_sequence
		SET last_published_sequence = $1, updated_at = clock_timestamp()
		WHERE id = TRUE AND last_published_sequence = $2 AND last_allocated_sequence >= $1`, sequence, sequence-1)
	if err != nil {
		return fmt.Errorf("advance outbox publication cursor: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm outbox publication cursor: %w", err)
	}
	if err := releaseCommandAdmissionTx(ctx, tx, sequence); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		commitErr := fmt.Errorf("commit outbox publication receipt: %w", err)
		confirmCtx, cancel := context.WithTimeout(ctx, receiptConfirmationBudget)
		defer cancel()
		confirmed, readErr := r.Published(confirmCtx, sequence)
		if readErr == nil && sameVoiceCommand(confirmed, command) {
			return nil
		}
		return errors.Join(commitErr, readErr)
	}
	return nil
}

// Retry releases only a live current token and retains the obligation. The two
// permanent classes block this head indefinitely; ordinary failures remain due.
func (r *Repository) Retry(ctx context.Context, claim ClaimedCommand, class string, retryDelay time.Duration) (returnErr error) {
	if r == nil || r.DB == nil {
		return errors.New("voice command repository database unavailable")
	}
	if !validFailureClass(class) || retryDelay < 0 || retryDelay > maximumRetryDelay {
		return errors.New("invalid voice command retry parameters")
	}
	command, sequence, err := validatedClaim(claim)
	if err != nil {
		return err
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin outbox retry: %w", err)
	}
	defer func() {
		returnErr = joinOutboxRollbackError(returnErr, tx.Rollback())
	}()
	if err := lockOwnedHead(ctx, tx, claim, command, sequence); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE voice_command_outbox
		SET lease_token = NULL, lease_until = NULL,
		retry_at = clock_timestamp() + $3 * INTERVAL '1 microsecond', failure_class = $4
		WHERE sequence = $1 AND lease_token = $2 AND lease_until > clock_timestamp()
		AND published_at IS NULL AND broker_sequence IS NULL`, sequence, claim.Token, retryDelay.Microseconds(), class)
	if err != nil {
		return fmt.Errorf("retain outbox command retry: %w", err)
	}
	if err := requireOwnedWrite(result); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit outbox command retry: %w", err)
	}
	return nil
}

// Published returns immutable proof only when both the permanent row receipt
// and the committed cursor confirm the requested sequence.
func (r *Repository) Published(ctx context.Context, sequence int64) (VoiceCommand, error) {
	if r == nil || r.DB == nil {
		return VoiceCommand{}, errors.New("voice command repository database unavailable")
	}
	if sequence <= 0 {
		return VoiceCommand{}, sql.ErrNoRows
	}
	stored, err := readCommandRow(r.DB.QueryRowContext(ctx, publishedCommandQuery, sequence))
	if err != nil {
		return VoiceCommand{}, fmt.Errorf("read published outbox command: %w", err)
	}
	if !validDeliveryMetadata(stored) || !stored.publishedAt.Valid || !stored.brokerSequence.Valid ||
		stored.brokerSequence.Int64 != sequence || stored.leaseToken.Valid || stored.failureClass.Valid {
		return VoiceCommand{}, ErrBrokerContinuity
	}
	return stored.command, nil
}

func validatedClaim(claim ClaimedCommand) (VoiceCommand, int64, error) {
	if claim.Token == uuid.Nil {
		return VoiceCommand{}, 0, ErrClaimLost
	}
	command, err := validateStoredCommand(claim.Command)
	if err != nil {
		return VoiceCommand{}, 0, err
	}
	sequence, err := strconv.ParseInt(command.Sequence, 10, 64)
	if err != nil {
		return VoiceCommand{}, 0, ErrInvalidCommand
	}
	return command, sequence, nil
}

func lockOwnedHead(ctx context.Context, tx *sql.Tx, claim ClaimedCommand, command VoiceCommand, sequence int64) error {
	if sequence <= 0 {
		return ErrInvalidCommand
	}
	ownedSequence := uint64(sequence)
	state, err := lockRetirementState(ctx, tx)
	if err != nil {
		return err
	}
	if state.Pending != nil || state.Published != ownedSequence-1 || state.Allocated < ownedSequence || state.RetiredThrough != claim.RetiredThrough {
		return ErrClaimLost
	}
	stored, err := readCommandRow(tx.QueryRowContext(ctx, lockedCommandQuery, sequence))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClaimLost
	}
	if err != nil {
		return fmt.Errorf("read owned outbox head: %w", err)
	}
	if !validDeliveryMetadata(stored) {
		return ErrBrokerContinuity
	}
	if stored.publishedAt.Valid || !stored.leaseToken.Valid || stored.leaseToken.String != claim.Token.String() ||
		permanentCommandFailure(stored) ||
		!sameVoiceCommand(stored.command, command) {
		return ErrClaimLost
	}
	return nil
}

func releaseCommandAdmissionTx(ctx context.Context, tx *sql.Tx, sequence int64) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM voice_command_admission WHERE sequence = $1`, sequence)
	if err != nil {
		return fmt.Errorf("release published outbox admission: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm published outbox admission release: %w", err)
	}
	return nil
}

func requireOwnedWrite(result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect owned outbox write: %w", err)
	}
	if changed == 0 {
		return ErrClaimLost
	}
	if changed != 1 {
		return errors.New("owned outbox write affected multiple rows")
	}
	return nil
}

func joinOutboxRollbackError(returnErr, rollbackErr error) error {
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return returnErr
	}
	return errors.Join(returnErr, fmt.Errorf("rollback outbox transaction: %w", rollbackErr))
}

func validFailureClass(class string) bool {
	switch class {
	case failureTransportUnavailable, failureBrokerUnavailable, failureBrokerContinuity,
		failureInvalidCommand, failureReceiptUnavailable:
		return true
	default:
		return false
	}
}

func permanentFailureClass(class string) bool {
	return class == failureBrokerContinuity || class == failureInvalidCommand
}

func permanentCommandFailure(stored storedCommand) bool {
	return stored.failureClass.Valid && permanentFailureClass(stored.failureClass.String)
}

func sameVoiceCommand(left, right VoiceCommand) bool {
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

const (
	storedCommandColumns = `sequence, command_id, stream_generation, envelope_version,
	scope, kind, action, channel_id, server_id, user_id, session_generation, node_boot_id,
	socket_id, credential_epoch, reason, authorization_revision, server_muted, server_deafened,
	lease_token, lease_until, attempts, retry_at, failure_class, published_at, broker_sequence`
	lockedCommandQuery = `SELECT ` + storedCommandColumns + `
		FROM voice_command_outbox WHERE sequence = $1 FOR UPDATE`
	publishedCommandQuery = `SELECT ` + storedCommandColumns + `
		FROM voice_command_outbox WHERE sequence = $1
		AND published_at IS NOT NULL AND broker_sequence = sequence
		AND EXISTS (SELECT 1 FROM voice_command_sequence WHERE id = TRUE
		AND last_published_sequence >= $1 AND last_allocated_sequence >= last_published_sequence)`
)

type storedCommand struct {
	command        VoiceCommand
	leaseToken     sql.NullString
	leaseUntil     sql.NullTime
	attempts       int
	retryAt        time.Time
	failureClass   sql.NullString
	publishedAt    sql.NullTime
	brokerSequence sql.NullInt64
}

type commandRowScanner interface {
	Scan(...any) error
}

func readCommandRow(row commandRowScanner) (storedCommand, error) {
	var stored storedCommand
	var sequence int64
	var channelID, serverID, session, boot, socket, epoch, reason sql.NullString
	var revision sql.NullInt64
	var muted, deafened sql.NullBool
	command := &stored.command
	err := row.Scan(&sequence, &command.CommandID, &command.StreamGeneration, &command.Version,
		&command.Scope, &command.Kind, &command.Action, &channelID, &serverID, &command.UserID,
		&session, &boot, &socket, &epoch, &reason, &revision, &muted, &deafened,
		&stored.leaseToken, &stored.leaseUntil, &stored.attempts, &stored.retryAt, &stored.failureClass,
		&stored.publishedAt, &stored.brokerSequence)
	if err != nil {
		return stored, err
	}
	command.Schema, command.Sequence = Schema, strconv.FormatInt(sequence, 10)
	command.ChannelID, command.ServerID = optionalString(channelID), optionalString(serverID)
	command.ServerMuted, command.ServerDeafened = optionalBool(muted), optionalBool(deafened)
	command.Reason = reason.String
	if revision.Valid {
		command.AuthorizationRevision = strconv.FormatInt(revision.Int64, 10)
	}
	if session.Valid || boot.Valid || socket.Valid || epoch.Valid {
		if !session.Valid || !boot.Valid || !socket.Valid || !epoch.Valid {
			return stored, ErrInvalidCommand
		}
		command.Target = &ExactTarget{
			SessionGeneration: session.String, NodeBootID: boot.String,
			SocketID: socket.String, CredentialEpoch: epoch.String,
		}
	}
	if reason.Valid && reason.String == "" {
		return stored, ErrInvalidCommand
	}
	validated, err := validateStoredCommand(*command)
	if err != nil {
		return stored, err
	}
	stored.command = validated
	return stored, nil
}

func optionalString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func optionalBool(value sql.NullBool) *bool {
	if !value.Valid {
		return nil
	}
	return &value.Bool
}

func validDeliveryMetadata(stored storedCommand) bool {
	if stored.attempts < 0 || stored.attempts > math.MaxInt32 || stored.retryAt.IsZero() ||
		stored.leaseToken.Valid != stored.leaseUntil.Valid ||
		stored.publishedAt.Valid != stored.brokerSequence.Valid ||
		(stored.failureClass.Valid && !validFailureClass(stored.failureClass.String)) {
		return false
	}
	if stored.leaseToken.Valid {
		token, err := uuid.Parse(stored.leaseToken.String)
		if err != nil || token == uuid.Nil || !canonicalUUID.MatchString(stored.leaseToken.String) || stored.leaseUntil.Time.IsZero() {
			return false
		}
	}
	if stored.publishedAt.Valid && (stored.publishedAt.Time.IsZero() || stored.brokerSequence.Int64 <= 0) {
		return false
	}
	return true
}
