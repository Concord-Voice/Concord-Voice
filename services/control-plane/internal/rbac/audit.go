package rbac

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
)

// AuditWriter logs permission-related administrative actions to the audit_log table
type AuditWriter struct {
	db               *sql.DB
	log              *logger.Logger
	securityEventsMu sync.RWMutex
	securityEvents   securityevent.Emitter
}

// NewAuditWriter creates a new audit log writer
func NewAuditWriter(db *sql.DB, log *logger.Logger) *AuditWriter {
	return &AuditWriter{
		db:             db,
		log:            log,
		securityEvents: securityevent.Discard,
	}
}

// SetSecurityEvents injects bounded Nightwatch telemetry without changing the
// high-fanout audit-writer constructor.
func (a *AuditWriter) SetSecurityEvents(events securityevent.Emitter) {
	if events == nil {
		events = securityevent.Discard
	}
	a.securityEventsMu.Lock()
	a.securityEvents = events
	a.securityEventsMu.Unlock()
}

func (a *AuditWriter) securityEventEmitter() securityevent.Emitter {
	a.securityEventsMu.RLock()
	events := a.securityEvents
	a.securityEventsMu.RUnlock()
	return events
}

// Log writes an audit log entry
// - serverID: the server where the action occurred
// - actorID: the user who performed the action (nil for system actions)
// - action: the type of action (e.g., "role_created", "permission_granted")
// - targetType: the type of resource affected ("role", "member", "channel", "permission")
// - targetID: the ID of the affected resource (nil if not applicable)
// - metadata: additional context as key-value pairs (marshaled to JSONB)
func (a *AuditWriter) Log(ctx context.Context, serverID string, actorID *string, action, targetType string, targetID *string, metadata map[string]interface{}) error {
	metadataJSON, err := json.Marshal(auditMetadataForStorage(metadata))
	if err != nil {
		a.log.Error("Failed to marshal audit metadata", "error", err)
		a.emitAuditWriteFailure(ctx)
		return err
	}

	query := `
		INSERT INTO audit_log (id, server_id, actor_id, action, target_type, target_id, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
	`

	id := uuid.New().String()
	_, err = a.db.ExecContext(ctx, query, id, serverID, actorID, action, targetType, targetID, metadataJSON)
	if err != nil {
		a.log.Error("Failed to write audit log", "error", err, "action", action, "server_id", serverID)
		a.emitAuditWriteFailure(ctx)
		return err
	}

	a.emitAuditCommitted(ctx, action, id)
	a.log.Info("Audit log entry created", "action", action, "server_id", serverID, "actor_id", actorID, "target_type", targetType)
	return nil
}

func (a *AuditWriter) emitAuditCommitted(ctx context.Context, action, evidenceRef string) {
	switch action {
	case "role_created", "role_updated", "role_deleted", "roles_reordered", "role_assigned", "role_unassigned",
		"channel_override_created", "channel_override_updated", "channel_override_deleted",
		"category_override_created", "category_override_updated", "category_override_deleted", "channel_sync_updated",
		"member_timed_out", "member_timeout_removed", "member_banned", "member_updated", "member_removed", "member_left", "member_unbanned",
		"ownership_transfer_initiated", "ownership_transfer_cancelled", "ownership_transfer_reversed", "ownership_transferred", "voice_member_moved":
		a.securityEventEmitter().Emit(ctx, securityevent.Event{
			EventType: securityevent.EventAudit, Outcome: securityevent.OutcomeSuccess,
			Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditCommitted, EvidenceRef: evidenceRef,
		})
	}
}

func (a *AuditWriter) emitAuditWriteFailure(ctx context.Context) {
	a.securityEventEmitter().Emit(ctx, securityevent.Event{
		EventType: securityevent.EventAudit, Outcome: securityevent.OutcomeFailure,
		Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditWriteFailed,
	})
}

// AuditEntry represents a single audit log entry (for API responses)
type AuditEntry struct {
	ID         string                 `json:"id"`
	ServerID   string                 `json:"server_id"`
	ActorID    *string                `json:"actor_id,omitempty"`
	Action     string                 `json:"action"`
	TargetType string                 `json:"target_type"`
	TargetID   *string                `json:"target_id,omitempty"`
	Metadata   map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt  string                 `json:"created_at"`
}

// GetAuditLog retrieves audit log entries for a server (paginated)
func (a *AuditWriter) GetAuditLog(ctx context.Context, serverID string, limit, offset int) ([]AuditEntry, error) {
	query := `
		SELECT id, server_id, actor_id, action, target_type, target_id, metadata, created_at
		FROM audit_log
		WHERE server_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := a.db.QueryContext(ctx, query, serverID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	entries := []AuditEntry{}
	for rows.Next() {
		var entry AuditEntry
		var metadataJSON []byte

		if err := rows.Scan(
			&entry.ID, &entry.ServerID, &entry.ActorID, &entry.Action,
			&entry.TargetType, &entry.TargetID, &metadataJSON, &entry.CreatedAt,
		); err != nil {
			a.log.Error("Failed to scan audit log entry", "error", err)
			continue
		}

		// Unmarshal metadata JSONB (bitfield keys re-emitted exactly, #3406)
		if len(metadataJSON) > 0 {
			meta, err := decodeAuditMetadata(entry.Action, metadataJSON)
			if err != nil {
				a.log.Error("Failed to unmarshal audit metadata", "error", err)
				meta = map[string]interface{}{"_error": "failed to parse metadata"}
			}
			entry.Metadata = meta
		}

		entries = append(entries, entry)
	}

	return entries, rows.Err()
}

// auditMetadataForStorage returns a copy of metadata in which every
// Permission value is its exact decimal string, because a JSON number above
// 2^53 loses its low bits in any float64 reader (#3406). It keys on the
// Permission type, not on int64: AuditWriter also records non-permission
// events, and a member timeout's duration_seconds must stay a number. A writer
// that records a bitfield passes it as Permission, which keeps a new audit
// action from needing an auditBitfieldKeys entry to stay exact.
func auditMetadataForStorage(metadata map[string]interface{}) map[string]interface{} {
	if metadata == nil {
		return nil
	}
	out := make(map[string]interface{}, len(metadata))
	for k, v := range metadata {
		if p, ok := v.(Permission); ok {
			out[k] = strconv.FormatInt(int64(p), 10)
			continue
		}
		out[k] = v
	}
	return out
}

// auditBitfieldKeys names, per audit action, the metadata keys that carry a
// permission bitfield in rows written before #3406, when writers stored them
// as JSON numbers. JSONB keeps those exactly as numeric, and stored rows are
// never rewritten, so GetAuditLog re-emits exactly these keys as decimal
// strings. Rows written since store the writers' Permission values as strings
// already (auditMetadataForStorage) and pass through unchanged.
var auditBitfieldKeys = map[string][]string{
	"role_created":              {"permissions"},
	"role_updated":              {"new_permissions"},
	"channel_override_created":  {"allow", "deny"},
	"channel_override_updated":  {"allow", "deny"},
	"category_override_created": {"allow", "deny"},
	"category_override_updated": {"allow", "deny"},
}

// errAuditMetadataTrailingData refuses a second value after the metadata
// document, which json.Unmarshal refused before #3406.
var errAuditMetadataTrailingData = errors.New("trailing data after audit metadata document")

// decodeAuditMetadata decodes a stored metadata document without losing
// numeric precision: UseNumber keeps every number as its exact digits, so a
// value above 2^53 re-marshals unchanged. The bitfield keys registered for
// the action then become decimal strings. The conversion is keyed by action,
// never by key name alone, and never clamps: a negative value written before
// #2869 is audit evidence and is re-emitted as it was stored.
func decodeAuditMetadata(action string, raw []byte) (map[string]interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var meta map[string]interface{}
	if err := dec.Decode(&meta); err != nil {
		return nil, err
	}
	// json.Unmarshal, which this reader used before #3406, refuses a second
	// value after the document; a streaming Decoder does not, so check for it.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errAuditMetadataTrailingData
	}
	for _, key := range auditBitfieldKeys[action] {
		if n, ok := meta[key].(json.Number); ok {
			meta[key] = n.String()
		}
	}
	return meta, nil
}
