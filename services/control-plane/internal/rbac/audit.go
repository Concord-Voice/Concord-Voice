package rbac

import (
	"context"
	"database/sql"
	"encoding/json"
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
	metadataJSON, err := json.Marshal(metadata)
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

		// Unmarshal metadata JSONB
		if len(metadataJSON) > 0 {
			if err := json.Unmarshal(metadataJSON, &entry.Metadata); err != nil {
				a.log.Error("Failed to unmarshal audit metadata", "error", err)
				entry.Metadata = map[string]interface{}{"_error": "failed to parse metadata"}
			}
		}

		entries = append(entries, entry)
	}

	return entries, rows.Err()
}
