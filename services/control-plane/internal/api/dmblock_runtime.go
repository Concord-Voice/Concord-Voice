//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/google/uuid"
)

const dmBlockVoiceEjectionRequestTimeout = 5 * time.Second

// A parent reconciliation claims at most 100 obligations. Bound target fanout
// to the same ceiling: every owner is contacted promptly, while a malformed or
// unusually broad registry query cannot create an unbounded goroutine burst.
const voiceEnforcementSessionEjectionMaxFanout = 100
const voiceEnforcementSessionEjectionWorkers = 64

const (
	voiceEnforcementSessionEjectVersion      = 1
	voiceEnforcementSessionEjectProofVersion = "v1"
	voiceEnforcementSessionEjectRequestProof = "concord/voice-enforcement-session/eject/request/v1"
	voiceEnforcementSessionEjectACKProof     = "concord/voice-enforcement-session/eject/ack/v1"
)

const (
	dmBlockVoiceEjectionACKSubject           = "voice.enforce.disconnect.ack"
	credentialEpochVoiceEjectionACKSubject   = "voice.enforce.credential_epoch.ack"
	credentialEpochVoiceEjectionProofVersion = "v1"
)

const (
	dmBlockVoiceEjectionProofVersion = "v2"
	dmBlockVoiceEjectionRequestProof = "concord/dm-block-voice-ejection/request/v2"
	dmBlockVoiceEjectionACKProof     = "concord/dm-block-voice-ejection/ack/v2"
)

const (
	credentialEpochVoiceEjectionRequestProof = "concord/credential-epoch-voice-ejection/request/v1"
	credentialEpochVoiceEjectionACKProof     = "concord/credential-epoch-voice-ejection/ack/v1"
)

type dmBlockVoiceEjectionRequester interface {
	RequestWithContext(ctx context.Context, subject string, data interface{}) ([]byte, error)
}

type voiceEnforcementSessionRow struct {
	sessionGeneration uuid.UUID
	nodeBootID        uuid.UUID
	roomID            uuid.UUID
	roomKind          string
	userID            uuid.UUID
	credentialEpoch   string
	socketID          string
}

type credentialEpochVoiceEjection struct {
	userID                    uuid.UUID
	credentialEpoch           string
	supersededCredentialEpoch string
}

type voiceEnforcementSessionTarget struct {
	kind            voiceEnforcementSessionTargetKind
	conversationID  string
	userID          uuid.UUID
	credentialEpoch string
}

type voiceEnforcementSessionTargetKind uint8

const (
	voiceEnforcementDMBlockTarget voiceEnforcementSessionTargetKind = iota + 1
	voiceEnforcementCredentialEpochTarget
)

type dmBlockAttachmentRetirer struct{ engine *purge.Engine }

func (r dmBlockAttachmentRetirer) CaptureConversationBlobsTx(
	ctx context.Context, tx *sql.Tx, conversationID string,
) ([]string, []dmblock.AttachmentBlobRef, error) {
	fileIDs, refs, err := r.engine.CaptureConversationBlobsTx(ctx, tx, conversationID)
	if err != nil {
		return nil, nil, err
	}
	converted := make([]dmblock.AttachmentBlobRef, 0, len(refs))
	for _, ref := range refs {
		converted = append(converted, dmblock.AttachmentBlobRef{Key: ref.Key, Backend: ref.Backend})
	}
	return fileIDs, converted, nil
}

func (r dmBlockAttachmentRetirer) EnqueueBlobDeletes(refs []dmblock.AttachmentBlobRef) {
	converted := make([]media.BlobRef, 0, len(refs))
	for _, ref := range refs {
		converted = append(converted, media.BlobRef{Key: ref.Key, Backend: ref.Backend})
	}
	r.engine.EnqueueBlobDeletes(converted)
}

func publishDMBlockVoiceEjection(
	ctx context.Context, requester dmBlockVoiceEjectionRequester, sharedSecret, conversationID string, userID uuid.UUID,
) error {
	requestKey := mediaproof.DeriveKey(sharedSecret, dmBlockVoiceEjectionRequestProof)
	ackKey := mediaproof.DeriveKey(sharedSecret, dmBlockVoiceEjectionACKProof)
	if len(requestKey) == 0 || len(ackKey) == 0 {
		return errors.New("DM block voice ejection proof is unavailable")
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("generate DM block voice ejection nonce: %w", err)
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := hex.EncodeToString(nonceBytes)
	channelID := conversationID
	userIDString := userID.String()
	action := "disconnect"
	proof := mediaproof.Sign(requestKey, dmBlockVoiceEjectionProofVersion, timestamp, channelID, userIDString, action, nonce)
	if proof == "" {
		return errors.New("sign DM block voice ejection request")
	}
	requestCtx, cancel := context.WithTimeout(ctx, dmBlockVoiceEjectionRequestTimeout)
	defer cancel()
	response, err := requester.RequestWithContext(requestCtx, dmBlockVoiceEjectionACKSubject, map[string]interface{}{
		"version":   2,
		"channelId": channelID,
		"userId":    userIDString,
		"action":    action,
		"timestamp": timestamp,
		"nonce":     nonce,
		"proof":     proof,
	})
	if err != nil {
		return fmt.Errorf("request DM block voice ejection acknowledgement: %w", err)
	}
	var acknowledgement struct {
		Version   int    `json:"version"`
		ChannelID string `json:"channelId"`
		UserID    string `json:"userId"`
		Action    string `json:"action"`
		Timestamp string `json:"timestamp"`
		Nonce     string `json:"nonce"`
		OK        bool   `json:"ok"`
		Proof     string `json:"proof"`
	}
	if err := json.Unmarshal(response, &acknowledgement); err != nil ||
		acknowledgement.Version != 2 ||
		acknowledgement.ChannelID != channelID ||
		acknowledgement.UserID != userIDString ||
		acknowledgement.Action != action ||
		acknowledgement.Timestamp != timestamp ||
		acknowledgement.Nonce != nonce ||
		!acknowledgement.OK ||
		!dmBlockVoiceEjectionTimestampIsCurrent(acknowledgement.Timestamp) ||
		!mediaproof.Verify(
			ackKey, acknowledgement.Proof, dmBlockVoiceEjectionProofVersion, acknowledgement.Timestamp,
			acknowledgement.ChannelID, acknowledgement.UserID, acknowledgement.Action, acknowledgement.Nonce,
			strconv.FormatBool(acknowledgement.OK),
		) {
		return errors.New("DM block voice ejection acknowledgement rejected")
	}
	return nil
}

func publishCredentialEpochVoiceEjection(
	ctx context.Context, requester dmBlockVoiceEjectionRequester, secret string, userID uuid.UUID, credentialEpoch, supersededCredentialEpoch string,
) error {
	requestKey := mediaproof.DeriveKey(secret, credentialEpochVoiceEjectionRequestProof)
	ackKey := mediaproof.DeriveKey(secret, credentialEpochVoiceEjectionACKProof)
	if len(requestKey) == 0 || len(ackKey) == 0 {
		return errors.New("credential media ejection proof unavailable")
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("generate credential media ejection nonce: %w", err)
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := hex.EncodeToString(nonceBytes)
	userIDString := userID.String()
	action := "disconnect"
	proof := mediaproof.Sign(requestKey, credentialEpochVoiceEjectionProofVersion, timestamp, userIDString, credentialEpoch, supersededCredentialEpoch, action, nonce)
	if proof == "" {
		return errors.New("sign credential media ejection request")
	}
	requestCtx, cancel := context.WithTimeout(ctx, dmBlockVoiceEjectionRequestTimeout)
	defer cancel()
	response, err := requester.RequestWithContext(requestCtx, credentialEpochVoiceEjectionACKSubject, map[string]interface{}{
		"version":                   1,
		"userId":                    userIDString,
		"credentialEpoch":           credentialEpoch,
		"supersededCredentialEpoch": supersededCredentialEpoch,
		"action":                    action,
		"timestamp":                 timestamp,
		"nonce":                     nonce,
		"proof":                     proof,
	})
	if err != nil {
		return fmt.Errorf("request credential media ejection acknowledgement: %w", err)
	}
	var acknowledgement struct {
		Version                   int    `json:"version"`
		UserID                    string `json:"userId"`
		CredentialEpoch           string `json:"credentialEpoch"`
		SupersededCredentialEpoch string `json:"supersededCredentialEpoch"`
		Action                    string `json:"action"`
		Timestamp                 string `json:"timestamp"`
		Nonce                     string `json:"nonce"`
		OK                        bool   `json:"ok"`
		Proof                     string `json:"proof"`
	}
	if err := json.Unmarshal(response, &acknowledgement); err != nil ||
		acknowledgement.Version != 1 ||
		acknowledgement.UserID != userIDString ||
		acknowledgement.CredentialEpoch != credentialEpoch ||
		acknowledgement.SupersededCredentialEpoch != supersededCredentialEpoch ||
		acknowledgement.Action != action ||
		acknowledgement.Timestamp != timestamp ||
		acknowledgement.Nonce != nonce ||
		!acknowledgement.OK ||
		!dmBlockVoiceEjectionTimestampIsCurrent(acknowledgement.Timestamp) ||
		!mediaproof.Verify(
			ackKey, acknowledgement.Proof, credentialEpochVoiceEjectionProofVersion, acknowledgement.Timestamp,
			acknowledgement.UserID, acknowledgement.CredentialEpoch, acknowledgement.SupersededCredentialEpoch,
			acknowledgement.Action, acknowledgement.Nonce, strconv.FormatBool(acknowledgement.OK),
		) {
		return errors.New("credential media ejection acknowledgement rejected")
	}
	return nil
}

// publishDurableDMBlockVoiceEjection retains the parent until every exact
// registry row has been torn down and released. Generic broadcast replies are
// deliberately never treated as completion: a different node can reply while
// the owner continues forwarding media.
func publishDurableDMBlockVoiceEjection(
	ctx context.Context, db *sql.DB, requester dmBlockVoiceEjectionRequester, secret, conversationID string, userID, parentGeneration uuid.UUID,
) error {
	activated, err := voiceEnforcementRolloutActivated(ctx, db)
	if err != nil {
		return err
	}
	if !activated {
		if err := publishDMBlockVoiceEjection(ctx, requester, secret, conversationID, userID); err != nil {
			return err
		}
		return errors.New("voice enforcement rollout is not activated")
	}
	var attempt int
	if err := db.QueryRowContext(ctx, `SELECT attempts FROM dm_block_voice_ejections WHERE generation = $1`, parentGeneration).Scan(&attempt); err != nil {
		return fmt.Errorf("read voice enforcement parent attempt: %w", err)
	}
	return publishDurableVoiceEnforcementSessions(ctx, db, requester, secret, parentGeneration, attempt,
		voiceEnforcementSessionTarget{
			kind: voiceEnforcementDMBlockTarget, conversationID: conversationID, userID: userID,
		})
}

func publishDurableCredentialEpochVoiceEjection(
	ctx context.Context, db *sql.DB, requester dmBlockVoiceEjectionRequester, secret string, ejection credentialEpochVoiceEjection, parentGeneration uuid.UUID,
) error {
	activated, err := voiceEnforcementRolloutActivated(ctx, db)
	if err != nil {
		return err
	}
	if !activated {
		if err := publishCredentialEpochVoiceEjection(ctx, requester, secret, ejection.userID, ejection.credentialEpoch, ejection.supersededCredentialEpoch); err != nil {
			return err
		}
		return errors.New("voice enforcement rollout is not activated")
	}
	var attempt int
	if err := db.QueryRowContext(ctx, `SELECT attempts FROM credential_epoch_voice_ejections WHERE generation = $1`, parentGeneration).Scan(&attempt); err != nil {
		return fmt.Errorf("read voice enforcement parent attempt: %w", err)
	}
	return publishDurableVoiceEnforcementSessions(ctx, db, requester, secret, parentGeneration, attempt,
		voiceEnforcementSessionTarget{
			kind: voiceEnforcementCredentialEpochTarget, userID: ejection.userID, credentialEpoch: ejection.supersededCredentialEpoch,
		})
}

func voiceEnforcementRolloutActivated(ctx context.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, errors.New("voice enforcement database is unavailable")
	}
	var activated bool
	if err := db.QueryRowContext(ctx,
		`SELECT activated_at IS NOT NULL FROM voice_enforcement_rollout WHERE id = TRUE`).Scan(&activated); err != nil {
		return false, fmt.Errorf("read voice enforcement rollout activation: %w", err)
	}
	return activated, nil
}

func publishDurableVoiceEnforcementSessions(
	ctx context.Context, db *sql.DB, requester dmBlockVoiceEjectionRequester, secret string, parentGeneration uuid.UUID, attempt int, target voiceEnforcementSessionTarget,
) error {
	rows, err := loadVoiceEnforcementSessions(ctx, db, target, attempt)
	if err != nil {
		return err
	}
	deliveryErr := publishVoiceEnforcementSessionRows(ctx, rows, func(session voiceEnforcementSessionRow) error {
		return publishVoiceEnforcementSessionEjection(ctx, requester, secret, parentGeneration, session)
	})
	remaining, err := loadVoiceEnforcementSessions(ctx, db, target, 0)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		if deliveryErr != nil {
			return fmt.Errorf("voice enforcement sessions remain after targeted delivery: %w", deliveryErr)
		}
		return errors.New("voice enforcement sessions remain after targeted delivery")
	}
	return nil
}

func publishVoiceEnforcementSessionRows(ctx context.Context, rows []voiceEnforcementSessionRow, publish func(voiceEnforcementSessionRow) error) error {
	rows = roundRobinVoiceEnforcementSessions(rows)
	resultErrors := make(chan error, len(rows))
	jobs := make(chan voiceEnforcementSessionRow)
	var waitGroup sync.WaitGroup
	workers := min(voiceEnforcementSessionEjectionWorkers, len(rows))
	for range workers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for session := range jobs {
				if err := publish(session); err != nil {
					resultErrors <- err
				}
			}
		}()
	}
	for _, row := range rows {
		jobs <- row
	}
	close(jobs)
	waitGroup.Wait()
	close(resultErrors)
	var deliveryErr error
	for resultErr := range resultErrors {
		deliveryErr = errors.Join(deliveryErr, resultErr)
	}
	return deliveryErr
}

// roundRobinVoiceEnforcementSessions schedules one row from each boot before
// retrying a boot with another row. A dead node therefore cannot monopolize
// the bounded request pool and hide a healthy owner later in UUID order.
func roundRobinVoiceEnforcementSessions(rows []voiceEnforcementSessionRow) []voiceEnforcementSessionRow {
	byNode := make(map[uuid.UUID][]voiceEnforcementSessionRow)
	for _, row := range rows {
		byNode[row.nodeBootID] = append(byNode[row.nodeBootID], row)
	}
	nodes := make([]uuid.UUID, 0, len(byNode))
	for node := range byNode {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].String() < nodes[j].String() })
	ordered := make([]voiceEnforcementSessionRow, 0, len(rows))
	for depth := 0; len(ordered) < len(rows); depth++ {
		for _, node := range nodes {
			if depth < len(byNode[node]) {
				ordered = append(ordered, byNode[node][depth])
			}
		}
	}
	return ordered
}

func loadVoiceEnforcementSessions(ctx context.Context, db *sql.DB, target voiceEnforcementSessionTarget, attempt int) (sessions []voiceEnforcementSessionRow, returnErr error) {
	if db == nil {
		return nil, errors.New("voice enforcement database is unavailable")
	}
	switch target.kind {
	case voiceEnforcementDMBlockTarget:
		return loadDMBlockVoiceEnforcementSessions(ctx, db, target, attempt)
	case voiceEnforcementCredentialEpochTarget:
		return loadCredentialEpochVoiceEnforcementSessions(ctx, db, target, attempt)
	default:
		return nil, errors.New("invalid voice enforcement session target")
	}
}

func loadDMBlockVoiceEnforcementSessions(ctx context.Context, db *sql.DB, target voiceEnforcementSessionTarget, attempt int) ([]voiceEnforcementSessionRow, error) {
	var total int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM voice_enforcement_sessions
		WHERE room_kind = 'dm' AND room_id = $1 AND user_id = $2`, target.conversationID, target.userID).Scan(&total); err != nil {
		return nil, fmt.Errorf("count voice enforcement sessions: %w", err)
	}
	offset := voiceEnforcementSessionOffset(total, attempt)
	rows, err := db.QueryContext(ctx, `
		SELECT session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id
		FROM (
			SELECT session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id,
				row_number() OVER (PARTITION BY node_boot_id ORDER BY created_at, session_generation) AS node_rank
			FROM voice_enforcement_sessions
			WHERE room_kind = 'dm' AND room_id = $1 AND user_id = $2
		) AS voice_enforcement_candidates
		ORDER BY node_rank, node_boot_id, session_generation
		OFFSET $3 LIMIT $4`, target.conversationID, target.userID, offset, voiceEnforcementSessionEjectionMaxFanout)
	if err != nil {
		return nil, fmt.Errorf("list voice enforcement sessions: %w", err)
	}
	return scanVoiceEnforcementSessionRows(rows)
}

func loadCredentialEpochVoiceEnforcementSessions(ctx context.Context, db *sql.DB, target voiceEnforcementSessionTarget, attempt int) ([]voiceEnforcementSessionRow, error) {
	var total int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM voice_enforcement_sessions
		WHERE user_id = $1 AND credential_epoch = $2`, target.userID, target.credentialEpoch).Scan(&total); err != nil {
		return nil, fmt.Errorf("count voice enforcement sessions: %w", err)
	}
	offset := voiceEnforcementSessionOffset(total, attempt)
	rows, err := db.QueryContext(ctx, `
		SELECT session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id
		FROM (
			SELECT session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id,
				row_number() OVER (PARTITION BY node_boot_id ORDER BY created_at, session_generation) AS node_rank
			FROM voice_enforcement_sessions
			WHERE user_id = $1 AND credential_epoch = $2
		) AS voice_enforcement_candidates
		ORDER BY node_rank, node_boot_id, session_generation
		OFFSET $3 LIMIT $4`, target.userID, target.credentialEpoch, offset, voiceEnforcementSessionEjectionMaxFanout)
	if err != nil {
		return nil, fmt.Errorf("list voice enforcement sessions: %w", err)
	}
	return scanVoiceEnforcementSessionRows(rows)
}

func voiceEnforcementSessionOffset(total, attempt int) int {
	if total > 0 {
		return (attempt * voiceEnforcementSessionEjectionMaxFanout) % total
	}
	return 0
}

func scanVoiceEnforcementSessionRows(rows *sql.Rows) (sessions []voiceEnforcementSessionRow, returnErr error) {
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close voice enforcement session rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		var row voiceEnforcementSessionRow
		if err := rows.Scan(&row.sessionGeneration, &row.nodeBootID, &row.roomID, &row.roomKind, &row.userID, &row.credentialEpoch, &row.socketID); err != nil {
			return nil, fmt.Errorf("scan voice enforcement session: %w", err)
		}
		sessions = append(sessions, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate voice enforcement sessions: %w", err)
	}
	return sessions, nil
}

func publishVoiceEnforcementSessionEjection(
	ctx context.Context, requester dmBlockVoiceEjectionRequester, secret string, parentGeneration uuid.UUID, row voiceEnforcementSessionRow,
) error {
	requestKey := mediaproof.DeriveKey(secret, voiceEnforcementSessionEjectRequestProof)
	ackKey := mediaproof.DeriveKey(secret, voiceEnforcementSessionEjectACKProof)
	if len(requestKey) == 0 || len(ackKey) == 0 {
		return errors.New("voice enforcement ejection proof is unavailable")
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("generate voice enforcement ejection nonce: %w", err)
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := hex.EncodeToString(nonceBytes)
	fields := make([]string, 0, 10)
	fields = append(fields, parentGeneration.String(), row.sessionGeneration.String(), row.nodeBootID.String(), row.roomID.String(), row.roomKind, row.userID.String(), row.credentialEpoch, row.socketID, nonce)
	proof := mediaproof.Sign(requestKey, voiceEnforcementSessionEjectProofVersion, timestamp, fields...)
	if proof == "" {
		return errors.New("sign voice enforcement ejection request")
	}
	requestCtx, cancel := context.WithTimeout(ctx, dmBlockVoiceEjectionRequestTimeout)
	defer cancel()
	response, err := requester.RequestWithContext(requestCtx, "voice.enforce.session."+row.nodeBootID.String(), map[string]interface{}{
		"version": voiceEnforcementSessionEjectVersion, "parentGeneration": parentGeneration.String(), "sessionGeneration": row.sessionGeneration.String(), "nodeBootId": row.nodeBootID.String(), "roomId": row.roomID.String(), "roomKind": row.roomKind, "userId": row.userID.String(), "credentialEpoch": row.credentialEpoch, "socketId": row.socketID, "timestamp": timestamp, "nonce": nonce, "proof": proof,
	})
	if err != nil {
		return fmt.Errorf("request targeted voice enforcement ejection: %w", err)
	}
	var acknowledgement struct {
		Version           int    `json:"version"`
		ParentGeneration  string `json:"parentGeneration"`
		SessionGeneration string `json:"sessionGeneration"`
		NodeBootID        string `json:"nodeBootId"`
		RoomID            string `json:"roomId"`
		RoomKind          string `json:"roomKind"`
		UserID            string `json:"userId"`
		CredentialEpoch   string `json:"credentialEpoch"`
		SocketID          string `json:"socketId"`
		Timestamp         string `json:"timestamp"`
		Nonce             string `json:"nonce"`
		OK                bool   `json:"ok"`
		Proof             string `json:"proof"`
	}
	if err := json.Unmarshal(response, &acknowledgement); err != nil || acknowledgement.Version != voiceEnforcementSessionEjectVersion || acknowledgement.ParentGeneration != parentGeneration.String() || acknowledgement.SessionGeneration != row.sessionGeneration.String() || acknowledgement.NodeBootID != row.nodeBootID.String() || acknowledgement.RoomID != row.roomID.String() || acknowledgement.RoomKind != row.roomKind || acknowledgement.UserID != row.userID.String() || acknowledgement.CredentialEpoch != row.credentialEpoch || acknowledgement.SocketID != row.socketID || acknowledgement.Timestamp != timestamp || acknowledgement.Nonce != nonce || !acknowledgement.OK || !dmBlockVoiceEjectionTimestampIsCurrent(timestamp) || !mediaproof.Verify(ackKey, acknowledgement.Proof, voiceEnforcementSessionEjectProofVersion, timestamp, append(fields, strconv.FormatBool(acknowledgement.OK))...) {
		return errors.New("targeted voice enforcement acknowledgement rejected")
	}
	return nil
}

func publishVoiceEnforcementHealth(ctx context.Context, requester dmBlockVoiceEjectionRequester, requestKey, ackKey []byte, nodeBootID uuid.UUID, challenge string) error {
	if requester == nil || len(requestKey) == 0 || len(ackKey) == 0 {
		return errors.New("voice enforcement health requester unavailable")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	proof := mediaproof.Sign(requestKey, "v1", timestamp, nodeBootID.String(), challenge, "health")
	if proof == "" {
		return errors.New("sign voice enforcement health")
	}
	requestCtx, cancel := context.WithTimeout(ctx, dmBlockVoiceEjectionRequestTimeout)
	defer cancel()
	response, err := requester.RequestWithContext(requestCtx, "voice.enforce.session."+nodeBootID.String(), map[string]interface{}{
		"version": 2, "kind": "health", "nodeBootId": nodeBootID.String(), "challenge": challenge, "timestamp": timestamp, "proof": proof,
	})
	if err != nil {
		return fmt.Errorf("request voice enforcement health: %w", err)
	}
	var ack struct {
		Version    int    `json:"version"`
		Kind       string `json:"kind"`
		NodeBootID string `json:"nodeBootId"`
		Challenge  string `json:"challenge"`
		Timestamp  string `json:"timestamp"`
		OK         bool   `json:"ok"`
		Proof      string `json:"proof"`
	}
	if err := json.Unmarshal(response, &ack); err != nil || ack.Version != 2 || ack.Kind != "health" || ack.NodeBootID != nodeBootID.String() || ack.Challenge != challenge || ack.Timestamp != timestamp || !ack.OK || !mediaproof.Verify(ackKey, ack.Proof, "v1", timestamp, nodeBootID.String(), challenge, "health", "true") {
		return errors.New("voice enforcement health acknowledgement rejected")
	}
	return nil
}

func dmBlockVoiceEjectionTimestampIsCurrent(timestamp string) bool {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	skew := time.Since(time.Unix(seconds, 0))
	return skew >= -time.Second && skew <= dmBlockVoiceEjectionRequestTimeout+time.Second
}
