package voicecommand

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ErrQueueFull refuses new obligations before the authoritative mutation commits.
var ErrQueueFull = errors.New("voice command queue full")

const (
	maximumOutstandingCommands = 5000
	maximumActorCommands       = 1000
)

// CommandSpec contains only the authority and immutable targets supplied by a writer.
type CommandSpec struct {
	Scope, Kind, Action           string
	ChannelID, ServerID           *string
	UserID                        string
	Target                        *ExactTarget
	Reason, AuthorizationRevision string
	ServerMuted, ServerDeafened   *bool
}

// RegisteredTarget preserves registry evidence, including a staged session.
type RegisteredTarget struct {
	ChannelID string
	Target    ExactTarget
}

// EnqueueTx retains the complete batch inside the authoritative transaction.
// The caller acquires all authority/target locks first and owns commit or rollback.
// Its final counter lock serializes allocation through commit: rollback restores
// both rows and counter, and another batch cannot commit ahead of this one.
func EnqueueTx(ctx context.Context, tx *sql.Tx, specs []CommandSpec, actors ...uuid.UUID) ([]VoiceCommand, error) {
	if !validEnqueueParameters(tx, specs, actors) {
		return nil, ErrInvalidCommand
	}
	owned := copyCommandSpecs(specs)
	var generation uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM voice_command_transport
		WHERE id = TRUE`).Scan(&generation); err != nil {
		return nil, fmt.Errorf("read outbox transport generation: %w", err)
	}
	if generation == uuid.Nil {
		return nil, ErrInvalidCommand
	}
	allocated, published, err := lockCommandSequence(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !validSequenceCounters(allocated, published) || int64(len(owned)) > math.MaxInt64-allocated {
		return nil, ErrInvalidCommand
	}
	if int64(len(owned)) > maximumOutstandingCommands-(allocated-published) {
		return nil, ErrQueueFull
	}
	commands, err := buildCommandBatch(owned, generation, allocated)
	if err != nil {
		return nil, err
	}
	charges := commandActorCharges(commands, actors)
	if err := checkActorAdmissionTx(ctx, tx, charges); err != nil {
		return nil, err
	}
	if err := insertCommandBatchTx(ctx, tx, commands, actors); err != nil {
		return nil, err
	}
	if err := advanceAllocationTx(ctx, tx, allocated, published, len(commands)); err != nil {
		return nil, err
	}
	return commands, nil
}

func validEnqueueParameters(tx *sql.Tx, specs []CommandSpec, actors []uuid.UUID) bool {
	return tx != nil && len(specs) != 0 && len(actors) <= 1 && (len(actors) == 0 || actors[0] != uuid.Nil)
}

func insertCommandBatchTx(ctx context.Context, tx *sql.Tx, commands []VoiceCommand, actors []uuid.UUID) error {
	for _, command := range commands {
		if err := insertCommandTx(ctx, tx, command); err != nil {
			return err
		}
		if err := insertCommandAdmissionTx(ctx, tx, command, commandActor(command, actors)); err != nil {
			return err
		}
	}
	return nil
}

func advanceAllocationTx(ctx context.Context, tx *sql.Tx, allocated, published int64, batchSize int) error {
	result, err := tx.ExecContext(ctx, `UPDATE voice_command_sequence
		SET last_allocated_sequence = $1, updated_at = clock_timestamp()
		WHERE id = TRUE AND last_allocated_sequence = $2 AND last_published_sequence = $3`,
		allocated+int64(batchSize), allocated, published)
	if err != nil {
		return fmt.Errorf("allocate outbox command batch: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm outbox batch allocation: %w", err)
	}
	return nil
}

func buildCommandBatch(specs []CommandSpec, generation uuid.UUID, allocated int64) ([]VoiceCommand, error) {
	commands := make([]VoiceCommand, len(specs))
	for index, spec := range specs {
		commandID, err := uuid.NewRandom()
		if err != nil {
			return nil, fmt.Errorf("create outbox command identity: %w", err)
		}
		command := VoiceCommand{
			Schema: Schema, Version: Version, StreamGeneration: generation.String(),
			CommandID: commandID.String(), Sequence: strconv.FormatInt(allocated+int64(index)+1, 10),
			Scope: spec.Scope, Kind: spec.Kind, Action: spec.Action, ChannelID: spec.ChannelID,
			ServerID: spec.ServerID, UserID: spec.UserID, Target: spec.Target, Reason: spec.Reason,
			AuthorizationRevision: spec.AuthorizationRevision,
			ServerMuted:           spec.ServerMuted, ServerDeafened: spec.ServerDeafened,
		}
		commands[index], err = validateStoredCommand(command)
		if err != nil {
			return nil, fmt.Errorf("validate outbox command: %w", err)
		}
	}
	return commands, nil
}

func commandActor(command VoiceCommand, actors []uuid.UUID) string {
	if len(actors) == 1 {
		return actors[0].String()
	}
	return command.UserID
}

func commandActorCharges(commands []VoiceCommand, actors []uuid.UUID) map[string]int64 {
	charges := make(map[string]int64)
	for _, command := range commands {
		charges[commandActor(command, actors)]++
	}
	return charges
}

func checkActorAdmissionTx(ctx context.Context, tx *sql.Tx, charges map[string]int64) (returnErr error) {
	actorIDs := make([]string, 0, len(charges))
	for actor, charge := range charges {
		if charge > maximumActorCommands {
			return ErrQueueFull
		}
		actorIDs = append(actorIDs, actor)
	}
	sort.Strings(actorIDs)
	// Every value is a validated command UUID or a supplied UUID's canonical
	// String. The array remains one parameter; the SQL text is never formatted.
	rows, err := tx.QueryContext(ctx, `SELECT actor_id::text, COUNT(*)
		FROM voice_command_admission WHERE actor_id = ANY($1::uuid[])
		GROUP BY actor_id`, "{"+strings.Join(actorIDs, ",")+"}")
	if err != nil {
		return fmt.Errorf("read outbox actor admission: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close outbox actor admission: %w", err))
		}
	}()
	for rows.Next() {
		var actor string
		var outstanding int64
		if err := rows.Scan(&actor, &outstanding); err != nil {
			return fmt.Errorf("read outbox actor quota: %w", err)
		}
		if outstanding < 0 || charges[actor] > maximumActorCommands-outstanding {
			return ErrQueueFull
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate outbox actor admission: %w", err)
	}
	return nil
}

func insertCommandAdmissionTx(ctx context.Context, tx *sql.Tx, command VoiceCommand, actor string) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO voice_command_admission (sequence, actor_id)
		VALUES ($1, $2)`, command.Sequence, actor)
	if err != nil {
		return fmt.Errorf("reserve outbox command admission: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm outbox command admission: %w", err)
	}
	return nil
}

func copyCommandSpecs(specs []CommandSpec) []CommandSpec {
	owned := make([]CommandSpec, len(specs))
	for index, spec := range specs {
		owned[index] = spec
		owned[index].ChannelID = copyPointer(spec.ChannelID)
		owned[index].ServerID = copyPointer(spec.ServerID)
		owned[index].Target = copyPointer(spec.Target)
		owned[index].ServerMuted = copyPointer(spec.ServerMuted)
		owned[index].ServerDeafened = copyPointer(spec.ServerDeafened)
	}
	return owned
}

func copyPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	owned := *value
	return &owned
}

func validateStoredCommand(command VoiceCommand) (VoiceCommand, error) {
	// Marshal replaces invalid UTF-8. Refuse it before it can alter a socket ID.
	if command.Target != nil && !utf8.ValidString(command.Target.SocketID) {
		return VoiceCommand{}, ErrInvalidCommand
	}
	generation, err := uuid.Parse(command.StreamGeneration)
	if err != nil {
		return VoiceCommand{}, ErrInvalidCommand
	}
	data, err := json.Marshal(command)
	if err != nil {
		return VoiceCommand{}, fmt.Errorf("encode outbox command: %w", err)
	}
	return DecodeVoiceCommand(data, generation)
}

func insertCommandTx(ctx context.Context, tx *sql.Tx, command VoiceCommand) error {
	sequence, err := strconv.ParseInt(command.Sequence, 10, 64)
	if err != nil {
		return fmt.Errorf("parse outbox command sequence: %w", err)
	}
	var session, boot, socket, epoch any
	if command.Target != nil {
		session, boot = command.Target.SessionGeneration, command.Target.NodeBootID
		socket, epoch = command.Target.SocketID, command.Target.CredentialEpoch
	}
	var reason, revision any
	if command.Reason != "" {
		reason = command.Reason
	}
	if command.AuthorizationRevision != "" {
		parsed, err := strconv.ParseInt(command.AuthorizationRevision, 10, 64)
		if err != nil {
			return fmt.Errorf("parse outbox authorization revision: %w", err)
		}
		revision = parsed
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO voice_command_outbox
		(sequence, command_id, stream_generation, envelope_version, scope, kind, action,
		channel_id, server_id, user_id, session_generation, node_boot_id, socket_id,
		credential_epoch, reason, authorization_revision, server_muted, server_deafened)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		sequence, command.CommandID, command.StreamGeneration, command.Version,
		command.Scope, command.Kind, command.Action, command.ChannelID, command.ServerID,
		command.UserID, session, boot, socket, epoch, reason, revision,
		command.ServerMuted, command.ServerDeafened)
	if err != nil {
		return fmt.Errorf("insert typed outbox command: %w", err)
	}
	if err := requireOneChanged(result); err != nil {
		return fmt.Errorf("confirm typed outbox insert: %w", err)
	}
	return nil
}

// CaptureTargetsTx discovers immutable registry targets after caller authority
// fences. Server filtering always joins the channel's server; DM is exact-room.
func CaptureTargetsTx(ctx context.Context, tx *sql.Tx, scope, userID, channelID, serverID string) (targets []RegisteredTarget, returnErr error) {
	if tx == nil || !canonicalUUID.MatchString(userID) ||
		(channelID != "" && !canonicalUUID.MatchString(channelID)) ||
		(serverID != "" && !canonicalUUID.MatchString(serverID)) {
		return nil, ErrInvalidCommand
	}
	var rows *sql.Rows
	var err error
	switch scope {
	case "channel":
		if serverID == "" {
			return nil, ErrInvalidCommand
		}
		rows, err = tx.QueryContext(ctx, `SELECT s.room_id, s.session_generation,
			s.node_boot_id, s.socket_id, s.credential_epoch
			FROM voice_enforcement_sessions s JOIN channels c ON c.id = s.room_id
			WHERE s.room_kind = 'channel' AND s.user_id = $1 AND c.server_id = $2
			AND ($3::uuid IS NULL OR s.room_id = $3::uuid)
			ORDER BY s.room_id, s.session_generation`, userID, serverID, nullableID(channelID))
	case "dm":
		if channelID == "" || serverID != "" {
			return nil, ErrInvalidCommand
		}
		rows, err = tx.QueryContext(ctx, `SELECT room_id, session_generation,
			node_boot_id, socket_id, credential_epoch FROM voice_enforcement_sessions
			WHERE room_kind = 'dm' AND user_id = $1 AND room_id = $2
			ORDER BY room_id, session_generation`, userID, channelID)
	default:
		return nil, ErrInvalidCommand
	}
	if err != nil {
		return nil, fmt.Errorf("capture outbox registered targets: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close captured outbox targets: %w", err))
		}
	}()
	for rows.Next() {
		var registered RegisteredTarget
		if err := rows.Scan(&registered.ChannelID, &registered.Target.SessionGeneration,
			&registered.Target.NodeBootID, &registered.Target.SocketID, &registered.Target.CredentialEpoch); err != nil {
			return nil, fmt.Errorf("read captured outbox target: %w", err)
		}
		targets = append(targets, registered)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate captured outbox targets: %w", err)
	}
	return targets, nil
}

func nullableID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func lockCommandSequence(ctx context.Context, tx *sql.Tx) (int64, int64, error) {
	var allocated, published int64
	if err := tx.QueryRowContext(ctx, `SELECT last_allocated_sequence, last_published_sequence
		FROM voice_command_sequence WHERE id = TRUE FOR UPDATE`).Scan(&allocated, &published); err != nil {
		return 0, 0, fmt.Errorf("lock outbox command cursor: %w", err)
	}
	return allocated, published, nil
}

func validSequenceCounters(allocated, published int64) bool {
	return allocated >= 0 && published >= 0 && published <= allocated
}

func requireOneChanged(result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect outbox write count: %w", err)
	}
	if changed != 1 {
		return errors.New("outbox write did not affect one row")
	}
	return nil
}
