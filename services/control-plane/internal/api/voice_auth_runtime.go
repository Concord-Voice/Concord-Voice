package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/google/uuid"
)

// Every exact ejection leaf, including immediate postcommit and legacy leaves,
// shares this process-owned budget. Health does not wait for teardown capacity.
var voiceExactRequestSlots = make(chan struct{}, voiceEnforcementSessionEjectionWorkers)

func acquireVoiceExactRequest(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case voiceExactRequestSlots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-voiceExactRequestSlots
			return nil, err
		}
		return func() { <-voiceExactRequestSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type voiceAuthFamilyCloser interface {
	DisconnectFamilyContext(context.Context, uuid.UUID, uuid.UUID) error
}

type voiceAuthRuntime struct {
	db         *sql.DB
	requester  dmBlockVoiceEjectionRequester
	secret     string
	hub        voiceAuthFamilyCloser
	reconciler *dmblock.Reconciler
}

func newVoiceAuthRuntime(db *sql.DB, requester dmBlockVoiceEjectionRequester, secret string, hub voiceAuthFamilyCloser, reconciler *dmblock.Reconciler) *voiceAuthRuntime {
	return &voiceAuthRuntime{db: db, requester: requester, secret: secret, hub: hub, reconciler: reconciler}
}

// VoiceAuthAuthoritySnapshot is CP permission only. Media must independently
// establish local native, ownership and subscription readiness before granting.
type VoiceAuthAuthoritySnapshot struct {
	Activation               string
	SQLAvailable             bool
	OldestUnsettledAt        time.Time
	SecurityRoundCompletedAt time.Time
	SecurityRoundFresh       bool
	ProductiveGrantEligible  bool
}

func voiceAuthSQLFailure(err error) error {
	return errors.Join(dmblock.ErrSecurityAuthorityUnavailable, err)
}

func (r *voiceAuthRuntime) AuthoritySnapshot(ctx context.Context, boot uuid.UUID) (VoiceAuthAuthoritySnapshot, error) {
	snapshot := VoiceAuthAuthoritySnapshot{Activation: "prepared"}
	if r == nil || r.db == nil || boot == uuid.Nil {
		return snapshot, voiceAuthSQLFailure(errors.New("voice auth authority unavailable"))
	}
	var active bool
	var oldest sql.NullTime
	var bootEligible bool
	err := r.db.QueryRowContext(ctx, `SELECT activated_at IS NOT NULL,
 EXISTS(SELECT 1 FROM voice_auth_boot_fences WHERE node_boot_id=$1 AND retired_at IS NULL),
 (SELECT min(created_at) FROM (
 SELECT o.created_at FROM login_family_revocations o
 WHERE o.completed_at IS NULL AND EXISTS(
 SELECT 1 FROM login_family_revocation_targets t JOIN voice_auth_connections c ON c.family_id=t.family_id
 WHERE t.operation_id=o.operation_id AND c.node_boot_id=$1)
 UNION ALL
 SELECT e.created_at FROM credential_epoch_voice_ejections e
 WHERE EXISTS(SELECT 1 FROM login_families f JOIN voice_auth_connections c USING(family_id)
 WHERE f.epoch_generation=e.generation AND c.node_boot_id=$1)
 ) pending) FROM login_family_rollout WHERE id=TRUE`, boot).Scan(&active, &bootEligible, &oldest)
	if err != nil {
		return snapshot, voiceAuthSQLFailure(fmt.Errorf("read voice auth authority: %w", err))
	}
	snapshot.SQLAvailable = true
	if active {
		snapshot.Activation = "active"
	}
	if oldest.Valid {
		snapshot.OldestUnsettledAt = oldest.Time
	}
	snapshot.SecurityRoundCompletedAt = r.reconciler.SecurityRoundCompletedAt()
	now := time.Now()
	snapshot.SecurityRoundFresh = !snapshot.SecurityRoundCompletedAt.IsZero() && now.Sub(snapshot.SecurityRoundCompletedAt) < 30*time.Second
	snapshot.ProductiveGrantEligible = active && bootEligible && snapshot.SecurityRoundFresh && (!oldest.Valid || now.Sub(oldest.Time) < 30*time.Second)
	return snapshot, nil
}

type voiceAuthOwner struct {
	generation              uuid.UUID
	userID                  uuid.UUID
	auth                    bool
	scope, reason, oldEpoch string
	attempts                int
}

func (r *voiceAuthRuntime) deliverAuth(ctx context.Context, o dmblock.AuthRevocation) error {
	owner := voiceAuthOwner{generation: o.OperationID, userID: o.UserID, auth: true}
	if r == nil || r.db == nil {
		return voiceAuthSQLFailure(errors.New("voice auth database unavailable"))
	}
	err := r.db.QueryRowContext(ctx, `SELECT scope,reason,attempts FROM login_family_revocations
 WHERE operation_id=$1 AND user_id=$2 AND sealed_at IS NOT NULL AND completed_at IS NULL`, o.OperationID, o.UserID).Scan(&owner.scope, &owner.reason, &owner.attempts)
	if err != nil {
		return voiceAuthSQLFailure(fmt.Errorf("read sealed auth owner: %w", err))
	}
	return r.deliverOwner(ctx, owner)
}

func (r *voiceAuthRuntime) deliverEpoch(ctx context.Context, userID uuid.UUID, epoch, old string, generation uuid.UUID) error {
	owner := voiceAuthOwner{generation: generation, userID: userID, oldEpoch: old}
	if r == nil || r.db == nil {
		return voiceAuthSQLFailure(errors.New("voice auth database unavailable"))
	}
	err := r.db.QueryRowContext(ctx, `SELECT attempts FROM credential_epoch_voice_ejections
 WHERE generation=$1 AND user_id=$2 AND credential_epoch=$3 AND superseded_credential_epoch=$4`, generation, userID, epoch, old).Scan(&owner.attempts)
	if err != nil {
		return voiceAuthSQLFailure(fmt.Errorf("read epoch owner: %w", err))
	}
	// Existing exact room evidence and retry semantics remain owned by this parent.
	familyResult := make(chan error, 1)
	go func() { familyResult <- r.deliverOwner(ctx, owner) }()
	roomErr := publishDurableCredentialEpochVoiceEjection(ctx, r.db, r.requester, r.secret, credentialEpochVoiceEjection{userID: userID, credentialEpoch: epoch, supersededCredentialEpoch: old}, generation)
	return errors.Join(<-familyResult, roomErr)
}

func (r *voiceAuthRuntime) deliverOwner(ctx context.Context, owner voiceAuthOwner) error {
	// The independent hub receipt must neither consume the media opportunity nor
	// be replaced by it. Both bounded lanes are joined before reporting completion.
	hubResult := make(chan error, 1)
	go func() { hubResult <- r.closeOwnerFamilies(ctx, owner) }()
	mediaErr := r.deliverOwnerMedia(ctx, owner)
	return errors.Join(<-hubResult, mediaErr, ctx.Err())
}

func (r *voiceAuthRuntime) deliverOwnerMedia(ctx context.Context, owner voiceAuthOwner) error {
	bindings, err := r.loadOwnerConnections(ctx, owner)
	if err != nil {
		return err
	}
	deliveryErr := r.deliverConnections(ctx, owner, bindings)
	remaining, err := r.ownerEvidenceRemains(ctx, owner)
	if err != nil {
		return err
	}
	if remaining {
		return errors.Join(errors.New("voice auth owner evidence remains"), deliveryErr)
	}
	// A lost ACK may race a signed durable release. Fresh owner-bound absence
	// proves the media half only; deliverOwner still requires the hub receipt.
	return ctx.Err()
}

const voiceAuthOwnerPredicate = `f.user_id=$2 AND f.terminal_at IS NOT NULL AND
 (($3 AND f.auth_operation_id=$1 AND EXISTS(SELECT 1 FROM login_family_revocation_targets t WHERE t.operation_id=$1 AND t.family_id=f.family_id AND t.user_id=$2))
 OR (NOT $3 AND f.epoch_generation=$1))`

func (r *voiceAuthRuntime) closeOwnerFamilies(ctx context.Context, o voiceAuthOwner) error {
	if r.hub == nil {
		return errors.New("voice auth hub unavailable")
	}
	var outcome error
	for offset := 0; ; offset += voiceEnforcementSessionEjectionMaxFanout {
		families, err := r.loadOwnerFamilyPage(ctx, o, offset)
		if err != nil {
			return errors.Join(outcome, err)
		}
		for _, family := range families {
			if err := ctx.Err(); err != nil {
				return errors.Join(outcome, err)
			}
			outcome = errors.Join(outcome, r.hub.DisconnectFamilyContext(ctx, o.userID, family))
		}
		if len(families) < voiceEnforcementSessionEjectionMaxFanout {
			return outcome
		}
	}
}

func (r *voiceAuthRuntime) loadOwnerFamilyPage(ctx context.Context, o voiceAuthOwner, offset int) (families []uuid.UUID, returnErr error) {
	// Rotate the immutable frontier's first family on every durable attempt.
	// A blocked minimum UUID cannot monopolize subsequent attempts. A successful
	// receipt still traverses the complete circle, in result pages of at most100.
	rows, err := r.db.QueryContext(ctx, `WITH families AS (
 SELECT f.family_id,row_number() OVER(ORDER BY f.family_id)-1 AS ordinal,count(*) OVER() AS total
 FROM login_families f WHERE `+voiceAuthOwnerPredicate+`)
 SELECT family_id FROM families ORDER BY (ordinal-(GREATEST($4::bigint-1,0)%total)+total)%total
 OFFSET $5 LIMIT $6`, o.generation, o.userID, o.auth, o.attempts, offset, voiceEnforcementSessionEjectionMaxFanout)
	if err != nil {
		return nil, voiceAuthSQLFailure(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, voiceAuthSQLFailure(err))
		}
	}()
	for rows.Next() {
		var family uuid.UUID
		if err := rows.Scan(&family); err != nil {
			return nil, voiceAuthSQLFailure(err)
		}
		families = append(families, family)
	}
	if err := rows.Err(); err != nil {
		return nil, voiceAuthSQLFailure(err)
	}
	return families, nil
}

func (r *voiceAuthRuntime) loadOwnerConnections(ctx context.Context, o voiceAuthOwner) (bindings []voiceauthprotocol.Binding, returnErr error) {
	// Rank within each boot before paging; failed boots never form a UUID prefix
	// that hides healthy boots. Advance one target per attempt so even a caller
	// slice shorter than a full request wave eventually starts every target.
	rows, err := r.db.QueryContext(ctx, `WITH candidates AS (
 SELECT c.*,row_number() OVER(PARTITION BY c.node_boot_id ORDER BY c.created_at,c.connection_generation) AS boot_rank
 FROM voice_auth_connections c JOIN login_families f USING(family_id) WHERE `+voiceAuthOwnerPredicate+`
 ), ordered AS (SELECT *,row_number() OVER(ORDER BY boot_rank,node_boot_id,connection_generation)-1 AS ordinal,count(*) OVER() AS total FROM candidates)
 SELECT user_id,family_id,connection_generation,node_boot_id,socket_id,credential_epoch FROM ordered
 ORDER BY (ordinal-(GREATEST($4::bigint-1,0)%total)+total)%total LIMIT $5`, o.generation, o.userID, o.auth, o.attempts, voiceEnforcementSessionEjectionMaxFanout)
	if err != nil {
		return nil, voiceAuthSQLFailure(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			returnErr = errors.Join(returnErr, voiceAuthSQLFailure(err))
		}
	}()
	for rows.Next() {
		var b voiceauthprotocol.Binding
		if err := rows.Scan(&b.UserID, &b.FamilyID, &b.Target.ConnectionGeneration, &b.Target.NodeBootID, &b.Target.SocketID, &b.Target.CredentialEpoch); err != nil {
			return nil, voiceAuthSQLFailure(err)
		}
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		return nil, voiceAuthSQLFailure(err)
	}
	return bindings, nil
}

func (r *voiceAuthRuntime) ownerEvidenceRemains(ctx context.Context, o voiceAuthOwner) (bool, error) {
	var remains bool
	// Connection deletion is guarded by signed closure and absence of live room
	// bindings; retain an explicit room branch so completion states its full scope.
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM login_families f
 JOIN voice_auth_connections c USING(family_id) WHERE `+voiceAuthOwnerPredicate+`)
 OR EXISTS(SELECT 1 FROM login_families f JOIN voice_auth_connections c USING(family_id)
 JOIN voice_auth_room_bindings b USING(connection_generation) JOIN voice_enforcement_sessions s USING(session_generation)
 WHERE `+voiceAuthOwnerPredicate+`)`, o.generation, o.userID, o.auth).Scan(&remains)
	if err != nil {
		return false, voiceAuthSQLFailure(err)
	}
	return remains, nil
}

func (r *voiceAuthRuntime) deliverConnections(ctx context.Context, o voiceAuthOwner, bindings []voiceauthprotocol.Binding) error {
	jobs := make(chan voiceauthprotocol.Binding)
	results := make(chan error, len(bindings))
	var wg sync.WaitGroup
	for range min(len(bindings), voiceEnforcementSessionEjectionWorkers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				results <- r.deliverConnection(ctx, o, b)
			}
		}()
	}
	for _, b := range bindings {
		select {
		case jobs <- b:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	var outcome error
	for err := range results {
		outcome = errors.Join(outcome, err)
	}
	return errors.Join(outcome, ctx.Err())
}

func (r *voiceAuthRuntime) deliverConnection(ctx context.Context, o voiceAuthOwner, b voiceauthprotocol.Binding) error {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("create exact command nonce: %w", err)
	}
	meta := voiceauthprotocol.AttemptMeta{Timestamp: strconv.FormatInt(time.Now().Unix(), 10), Nonce: hex.EncodeToString(nonce)}
	var command voiceauthprotocol.Command
	var err error
	if o.auth {
		command, err = voiceauthprotocol.NewAuthCommand(b, o.generation.String(), o.scope, o.reason, meta)
	} else {
		command, err = voiceauthprotocol.NewEpochCommand(b, o.generation.String(), o.oldEpoch, meta)
	}
	if err != nil {
		return err
	}
	return publishVoiceAuthConnectionEjection(ctx, r.requester, r.secret, command)
}

func publishVoiceAuthConnectionEjection(ctx context.Context, requester dmBlockVoiceEjectionRequester, secret string, command voiceauthprotocol.Command) error {
	requestCtx, cancel := context.WithTimeout(ctx, dmBlockVoiceEjectionRequestTimeout)
	defer cancel()
	release, err := acquireVoiceExactRequest(requestCtx)
	if err != nil {
		return err
	}
	defer release()
	if requester == nil {
		return errors.New("voice auth requester unavailable")
	}
	var boot string
	if auth, ok := command.Auth(); ok {
		boot = auth.Target.NodeBootID
	} else if epoch, ok := command.Epoch(); ok {
		boot = epoch.Target.NodeBootID
	} else {
		return errors.New("invalid exact command")
	}
	raw, err := voiceauthprotocol.SignCommand(command, []byte(secret))
	if err != nil {
		return err
	}
	response, err := requester.RequestWithContext(requestCtx, "voice.enforce.session."+boot, json.RawMessage(raw))
	if err != nil {
		return fmt.Errorf("request exact connection ejection: %w", err)
	}
	if _, err := voiceauthprotocol.VerifyACK(response, command, []byte(secret), time.Now()); err != nil {
		return err
	}
	return requestCtx.Err()
}
