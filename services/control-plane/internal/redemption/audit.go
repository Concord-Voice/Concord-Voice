package redemption

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// IssuerContext labels HOW a generation was performed, so a NULL issuer_id
// (CLI/operator-on-the-box, no user identity) is unambiguous in the audit trail.
const (
	IssuerContextCLI       = "cli"
	IssuerContextAdminHTTP = "admin-http"
)

// GenerationAudit is one issuance audit record. It is deliberately PII-free and
// secret-free: WHO (issuer id, may be nil for CLI), WHAT kind, HOW MANY, WHICH
// batch, WHEN — never the code plaintext or hash (spec §7 / observability.md
// "No key material"). Context labels the channel.
type GenerationAudit struct {
	IssuerID  uuid.NullUUID
	Context   string // IssuerContextCLI | IssuerContextAdminHTTP
	BatchID   string
	GrantKind string
	Count     int
	IssuedAt  time.Time
}

// GenerationRecorder records a code-generation event. Implementations MUST write
// within the issuer's transaction (the passed tx) so a code never exists without
// its generation being audited — partial success (codes inserted, audit not) is
// not a tolerated state. Single-method interface named for its method per Go
// convention (S8196).
type GenerationRecorder interface {
	RecordGeneration(ctx context.Context, tx *sql.Tx, a GenerationAudit) error
}

// DBAuditSink persists generation audits to redemption_code_issuance (migration
// 000076).
type DBAuditSink struct{}

// NewDBAuditSink builds the DB-backed audit sink.
func NewDBAuditSink() *DBAuditSink { return &DBAuditSink{} }

// RecordGeneration inserts the audit row inside tx. Parameterized SQL; the
// issuer_id is written NULL when the NullUUID is invalid (CLI path).
func (DBAuditSink) RecordGeneration(ctx context.Context, tx *sql.Tx, a GenerationAudit) error {
	context := a.Context
	if context == "" {
		context = IssuerContextCLI
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO redemption_code_issuance
			(issuer_id, issuer_context, grant_kind, code_count, batch_id, created_at)
		VALUES ($1, $2, $3, $4, NULLIF($5,''), $6)`,
		nullableUUIDValue(a.IssuerID), context, a.GrantKind, a.Count, a.BatchID, a.IssuedAt)
	if err != nil {
		return fmt.Errorf("redemption: insert issuance audit: %w", err)
	}
	return nil
}

// nullableUUIDValue returns the UUID for a valid NullUUID, else nil (SQL NULL).
func nullableUUIDValue(u uuid.NullUUID) any {
	if !u.Valid {
		return nil
	}
	return u.UUID
}

// noopAudit is the fallback sink (records nothing). NewIssuer substitutes it
// only when no sink is supplied; production wiring always passes DBAuditSink.
type noopAudit struct{}

func (noopAudit) RecordGeneration(context.Context, *sql.Tx, GenerationAudit) error { return nil }
