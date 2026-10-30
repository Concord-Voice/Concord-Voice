package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HealthBootstrap records capability only after the exact protocol-4 transport
// challenge succeeds. Prepared transport needs neither clients nor native drain.
// No SQL lock is retained across the broker request. The final boot lock orders
// this result against retirement, including retirement of an absent identity.
func (h *voiceEnforcementSessionHandler) HealthBootstrap(c *gin.Context) {
	if h == nil || h.db == nil || h.requester == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	request, _, err := h.readVoiceAuth(c, protocol.EndpointHealthBootstrap)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	value, ok := voiceAuthPayload(c, request.HealthBootstrap)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	snapshot, err := h.bootAuthority(ctx, uuid.MustParse(value.NodeBootID))
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	ack, err := h.challengeBoot(ctx, value.NodeBootID, snapshot)
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	response, err := h.runVoiceAuth(ctx, request, func(tx *sql.Tx) (protocol.ResponsePayload, error) {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT activated_at IS NOT NULL FROM login_family_rollout WHERE id=TRUE FOR SHARE`).Scan(&active); err != nil {
			return nil, err
		}
		if err := voiceAuthGeneration(ctx, tx, "boot", value.NodeBootID); err != nil {
			return nil, err
		}
		if err := h.bootstrapBootTx(ctx, tx, request, value.NodeBootID); err != nil {
			return nil, err
		}
		activation := "prepared"
		if active {
			activation = "active"
		}
		granted := active && ack.AdmissionGranted && voiceAuthSnapshotEligible(snapshot, time.Now())
		return protocol.HealthBootstrapResult{Version: 1, VoiceEnforcementProtocol: 4, NodeBootID: value.NodeBootID, Activation: activation, TransportOK: true, AdmissionGranted: granted}, nil
	})
	writeVoiceAuth(c, response, err)
}

func voiceAuthSnapshotEligible(snapshot VoiceAuthAuthoritySnapshot, now time.Time) bool {
	return snapshot.Activation == "active" && snapshot.SQLAvailable && snapshot.ProductiveGrantEligible && snapshot.SecurityRoundFresh &&
		!snapshot.SecurityRoundCompletedAt.IsZero() && !snapshot.SecurityRoundCompletedAt.After(now) && now.Sub(snapshot.SecurityRoundCompletedAt) < 30*time.Second &&
		(snapshot.OldestUnsettledAt.IsZero() || now.Sub(snapshot.OldestUnsettledAt) < 30*time.Second)
}

func (h *voiceEnforcementSessionHandler) bootAuthority(ctx context.Context, boot uuid.UUID) (VoiceAuthAuthoritySnapshot, error) {
	if h.authoritySnapshot != nil {
		return h.authoritySnapshot(ctx, boot)
	}
	// Constructor compatibility never invents process freshness. Production is
	// wired to the same reconciler that performs the bounded security rounds.
	snapshot := VoiceAuthAuthoritySnapshot{Activation: "prepared"}
	var active bool
	err := h.db.QueryRowContext(ctx, `SELECT activated_at IS NOT NULL FROM login_family_rollout WHERE id=TRUE`).Scan(&active)
	if err == nil {
		snapshot.SQLAvailable = true
		if active {
			snapshot.Activation = "active"
		}
	}
	return snapshot, err
}

func (h *voiceEnforcementSessionHandler) challengeBoot(ctx context.Context, boot string, snapshot VoiceAuthAuthoritySnapshot) (protocol.HealthAck4, error) {
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return protocol.HealthAck4{}, err
	}
	command, err := protocol.NewHealthCommand(boot, hex.EncodeToString(challenge), snapshot.Activation, voiceAuthSnapshotEligible(snapshot, time.Now()), strconv.FormatInt(time.Now().Unix(), 10))
	if err != nil {
		return protocol.HealthAck4{}, err
	}
	raw, err := protocol.SignCommand(command, h.authSecret)
	if err != nil {
		return protocol.HealthAck4{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := h.requester.RequestWithContext(requestCtx, "voice.enforce.session."+boot, json.RawMessage(raw))
	if err != nil {
		return protocol.HealthAck4{}, err
	}
	verified, err := protocol.VerifyACK(response, command, h.authSecret, time.Now())
	if err != nil {
		return protocol.HealthAck4{}, err
	}
	ack, ok := verified.Health()
	if !ok {
		return protocol.HealthAck4{}, voiceAuthFail(http.StatusForbidden)
	}
	return ack, nil
}

func (h *voiceEnforcementSessionHandler) bootstrapBootTx(ctx context.Context, tx *sql.Tx, request protocol.Request, boot string) error {
	var retired sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT retired_at FROM voice_auth_boot_fences WHERE node_boot_id=$1 FOR UPDATE`, boot).Scan(&retired)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if retired.Valid {
		return voiceAuthFail(http.StatusForbidden)
	}
	if freshErr := protocol.VerifyHTTPRequest(request, h.authSecret, time.Now()); freshErr != nil {
		return freshErr
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, boot)
	}
	return err
}

// RetireBoot accepts the authenticated shutdown owner's whole-boot witness.
// That private sender joins native workers, registrations, renewals, release
// retries and health work before sending. A MAC authenticates that trusted
// assertion; SQL absence alone never establishes native cessation.
func (h *voiceEnforcementSessionHandler) RetireBoot(c *gin.Context) {
	if h == nil || h.db == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	request, _, err := h.readVoiceAuth(c, protocol.EndpointBootRetire)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	value, ok := voiceAuthPayload(c, request.BootRetire)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	response, err := h.runVoiceAuth(ctx, request, func(tx *sql.Tx) (protocol.ResponsePayload, error) {
		fresh := func() error { return protocol.VerifyHTTPRequest(request, h.authSecret, time.Now()) }
		return nil, retireVoiceAuthBootTx(ctx, tx, value.NodeBootID, voiceAuthServiceRetirement, fresh)
	})
	writeVoiceAuth(c, response, err)
}

type voiceAuthRetirementAuthority uint8

const (
	voiceAuthServiceRetirement voiceAuthRetirementAuthority = iota
	voiceAuthOperatorRetirement
	voiceAuthObserveRetirement
)

// RetireStoppedVoiceAuthBoot is the privileged local operator entrypoint. Its
// caller must verify the exact boot/runtime association and complete physical
// process containment exit before calling. It cannot be selected by HTTP input.
// A nil result attests definite SQL commit, not a cryptographic host witness.
func RetireStoppedVoiceAuthBoot(ctx context.Context, db *sql.DB, boot uuid.UUID) (retErr error) {
	if db == nil || boot == uuid.Nil {
		return voiceAuthFail(http.StatusBadRequest)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			retErr = errors.Join(retErr, err)
		}
	}()
	if err = retireVoiceAuthBootTx(ctx, tx, boot.String(), voiceAuthOperatorRetirement, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// VerifyRetiredVoiceAuthBoot observes an existing permanent fence and fresh
// zero-live evidence under the same serialization as retirement. It never writes
// a fence or substitutes SQL evidence for a shutdown witness.
func VerifyRetiredVoiceAuthBoot(ctx context.Context, db *sql.DB, boot uuid.UUID) (verified bool, retErr error) {
	if db == nil || boot == uuid.Nil {
		return false, voiceAuthFail(http.StatusBadRequest)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			verified = false
			retErr = errors.Join(retErr, err)
		}
	}()
	err = retireVoiceAuthBootTx(ctx, tx, boot.String(), voiceAuthObserveRetirement, nil)
	if err != nil {
		if voiceAuthStatus(err) == http.StatusConflict {
			return false, nil
		}
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func retireVoiceAuthBootTx(ctx context.Context, tx *sql.Tx, boot string, authority voiceAuthRetirementAuthority, fresh func() error) error {
	if err := voiceAuthGeneration(ctx, tx, "boot", boot); err != nil {
		return err
	}
	var retired sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT retired_at FROM voice_auth_boot_fences WHERE node_boot_id=$1 FOR UPDATE`, boot).Scan(&retired)
	absent := errors.Is(err, sql.ErrNoRows)
	if err != nil && !absent {
		return err
	}
	if fresh != nil {
		if err := fresh(); err != nil {
			return err
		}
	}
	if (absent && authority != voiceAuthOperatorRetirement) || (authority == voiceAuthObserveRetirement && !retired.Valid) {
		return voiceAuthFail(http.StatusConflict)
	}
	var bootClear bool
	if err := tx.QueryRowContext(ctx, voiceAuthBootRetirementClearSQL, boot).Scan(&bootClear); err != nil {
		return err
	}
	if !bootClear {
		return voiceAuthFail(http.StatusConflict)
	}
	if fresh != nil {
		if err := fresh(); err != nil {
			return err
		}
	}
	if retired.Valid {
		return nil
	}
	if absent {
		_, err = tx.ExecContext(ctx, `INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,clock_timestamp())`, boot)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE voice_auth_boot_fences SET retired_at=clock_timestamp(),updated_at=clock_timestamp() WHERE node_boot_id=$1 AND retired_at IS NULL`, boot)
	}
	return err
}

// Creators retain boot SHARE through commit. Retirement only fences creation;
// pending durable deliveries retain their closure evidence independently.
const voiceAuthBootRetirementClearSQL = `SELECT NOT EXISTS(SELECT 1 FROM voice_auth_connections WHERE node_boot_id=$1)
 AND NOT EXISTS(SELECT 1 FROM voice_enforcement_sessions WHERE node_boot_id=$1)
 AND NOT EXISTS(SELECT 1 FROM voice_auth_room_bindings b WHERE
 EXISTS(SELECT 1 FROM voice_auth_connections c WHERE c.connection_generation=b.connection_generation AND c.node_boot_id=$1)
 OR EXISTS(SELECT 1 FROM voice_enforcement_sessions r WHERE r.session_generation=b.session_generation AND r.node_boot_id=$1)
 OR EXISTS(SELECT 1 FROM voice_auth_connection_closures c WHERE c.connection_generation=b.connection_generation AND c.node_boot_id=$1)
 OR EXISTS(SELECT 1 FROM voice_auth_room_closures r WHERE r.session_generation=b.session_generation AND r.node_boot_id=$1))`
