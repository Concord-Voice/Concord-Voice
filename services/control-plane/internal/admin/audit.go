package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Audit result values — mirror the CHECK constraint on admin_audit_log.result.
const (
	// AuditSuccess marks a successful outcome.
	AuditSuccess = "success"
	// AuditFailure marks a failed outcome (e.g., bad password / assertion).
	AuditFailure = "failure"
	// AuditDenied marks an authorization denial (e.g., locked-out / no session).
	AuditDenied = "denied"
)

// auditRole is the restricted Postgres role adopted per-transaction so the
// audit INSERT runs append-only: the role holds INSERT/SELECT on
// admin_audit_log but NOT UPDATE/DELETE (migration 000077). Adopting it via
// SET LOCAL ROLE makes append-only an ENFORCED Postgres guarantee, not just
// app-layer intent. The constant name is fixed — do NOT parameterize it (it is
// a SQL identifier, not a value; SET ROLE does not accept placeholders).
const auditRole = "concord_admin_rt"

// AuditEvent is one append-only admin_audit_log entry (#1688). It records the
// OUTCOME of a security-relevant admin action — never secrets, assertion bytes,
// raw IPs, or end-user PII. Actor is the attempted operator handle (sanitized);
// SourceRef is an opaque reference (e.g., a CF Access subject), never a raw IP
// or email; Detail is a small scrubbed JSON object.
type AuditEvent struct {
	AdminID   *string        // nullable FK to admin_users.id
	Actor     string         // attempted handle; sanitized before storage
	EventType string         // one of the Event* constants in types.go
	Result    string         // AuditSuccess | AuditFailure | AuditDenied
	SourceRef string         // opaque reference; never a raw IP/email
	Detail    map[string]any // scrubbed JSON; no secrets/PII/assertion
}

// AuditLog writes append-only entries to admin_audit_log.
type AuditLog struct {
	db *sql.DB
}

// NewAuditLog wires an AuditLog against the given DB.
func NewAuditLog(db *sql.DB) *AuditLog {
	return &AuditLog{db: db}
}

// Write inserts exactly one audit row. It opens a transaction, adopts the
// restricted concord_admin_rt role via SET LOCAL ROLE (so the append-only
// guarantee is enforced by Postgres for the lifetime of the transaction), runs
// a parameterized INSERT, and commits. The Actor field is routed through
// sanitizeAuditString (CWE-117) before storage.
func (a *AuditLog) Write(ctx context.Context, ev AuditEvent) error {
	var detailJSON []byte
	if ev.Detail != nil {
		b, err := json.Marshal(ev.Detail)
		if err != nil {
			return fmt.Errorf("marshal audit detail: %w", err)
		}
		detailJSON = b
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin audit tx: %w", err)
	}
	// Roll back on any error path; a successful Commit makes Rollback a no-op.
	defer func() { _ = tx.Rollback() }()

	// Adopt the restricted role for this transaction. SET ROLE takes an
	// identifier, not a bind parameter, so the role name is a fixed constant
	// (never user input).
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+auditRole); err != nil {
		return fmt.Errorf("adopt audit role: %w", err)
	}

	const q = `
		INSERT INTO admin_audit_log (admin_id, actor, event_type, result, source_ref, detail)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
	if _, err := tx.ExecContext(ctx, q,
		toNullString(adminIDValue(ev.AdminID)),
		toNullString(sanitizeAuditString(ev.Actor)),
		ev.EventType,
		ev.Result,
		toNullString(ev.SourceRef),
		detailJSONOrNil(detailJSON),
	); err != nil {
		return fmt.Errorf("insert audit row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit audit row: %w", err)
	}
	return nil
}

// adminIDValue dereferences a nullable admin id pointer to a string ("" => NULL).
func adminIDValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// toNullString maps "" → SQL NULL, else a valid string.
func toNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// detailJSONOrNil returns the JSONB bytes or nil (so the column stores NULL
// rather than the literal text "null").
func detailJSONOrNil(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// sanitizeAuditString strips control characters from a user-derived string
// before it is written to the audit log, preventing CWE-117 log forging (CRLF
// injection of fabricated lines). It mirrors internal/websocket.sanitizeLogValue
// (unexported there, so duplicated here): `\n` and `\r` are removed via
// strings.ReplaceAll — the form CodeQL go/log-injection recognizes — and any
// remaining C0 control characters plus DEL are dropped. See
// [internal]rules/observability.md ("Logging Discipline").
func sanitizeAuditString(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
