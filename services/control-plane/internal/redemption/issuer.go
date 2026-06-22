package redemption

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// IssueSpec describes a batch of codes to mint. The same spec produces one-off
// codes (MaxRedemptions=1) or promotional codes (MaxRedemptions=nil/unlimited +
// ExpiresAt), per spec §5 — the columns express the policy, one engine.
type IssueSpec struct {
	GrantKind   string         // catalog key; MUST be Supports()-ed (validated)
	GrantParams map[string]any // e.g. {"months": 12}; nil → {}
	Count       int            // number of codes to mint (≥1)
	Prefix      string         // non-secret support handle ('KS','PROMO'); ≤16 chars
	SingleUse   bool           // true for one-off
	MaxRedeems  *int           // nil = unlimited (promo); pointer distinguishes nil from 0
	ExpiresAt   *time.Time     // hard expiry; required for unlimited promo (validated)
	BatchID     string         // campaign label shared across the batch; ≤64 chars
	// CreatedBy is the issuer's user id, recorded on every row (audit). May be
	// uuid.Nil for the CLI path (operator-on-the-box, no user identity); the
	// audit log still records the operator context.
	CreatedBy uuid.NullUUID
	// Context labels the issuance channel for the audit trail
	// (IssuerContextCLI | IssuerContextAdminHTTP). Defaults to CLI when empty.
	Context string
}

// IssuedCode is one minted code: the plaintext (returned ONCE — never persisted)
// plus its registry id. The hash is stored; the plaintext is the issuer's to
// distribute and is unrecoverable afterward.
type IssuedCode struct {
	ID        uuid.UUID
	Plaintext string // formatted (prefix + grouped); shown once
}

var (
	errIssueCountInvalid   = errors.New("redemption: issue count must be ≥1")
	errIssueCountTooLarge  = fmt.Errorf("redemption: issue count exceeds the maximum batch size of %d", MaxBatchSize)
	errIssueGrantUnknown   = errors.New("redemption: grant_kind not supported by this binary")
	errIssuePromoNoExpiry  = errors.New("redemption: unlimited promotional codes require an expires_at")
	errIssuePrefixTooLong  = errors.New("redemption: code_prefix exceeds 16 chars")
	errIssueBatchIDTooLong = errors.New("redemption: batch_id exceeds 64 chars")
)

// MaxBatchSize is the hard upper bound on the number of codes a single issue
// call may mint. It is both a usability guard (a typo'd Count can't attempt to
// mint a runaway number of rows in one transaction) and a security guard: the
// per-batch count drives an up-front slice allocation (see Issue), so an
// attacker-influenced Count must be bounded BEFORE any allocation to prevent a
// memory-exhaustion DoS (CWE-789, CodeQL go/uncontrolled-allocation-size).
//
// 10,000 is comfortably above the largest realistic issuance — the Kickstarter
// CSV use case in [internal]redemption-code-issuance.md mints hundreds per
// batch — while keeping the worst-case single-call allocation small (~10k
// IssuedCode structs). Larger campaigns issue multiple batches.
const MaxBatchSize = 10_000

// Issuer mints codes into the registry. It validates the spec against the
// catalog (so a code is never issued for an effect the binary can't honor) and
// writes every row + an audit record in ONE transaction.
type Issuer struct {
	db      *sql.DB
	catalog *Catalog
	audit   GenerationRecorder
}

// NewIssuer builds an issuer. audit may be nil (no-op audit) but callers SHOULD
// pass a real sink — generation auditing is a security acceptance criterion.
func NewIssuer(db *sql.DB, catalog *Catalog, audit GenerationRecorder) *Issuer {
	if audit == nil {
		audit = noopAudit{}
	}
	return &Issuer{db: db, catalog: catalog, audit: audit}
}

// validateSpec enforces the spec invariants BEFORE any DB write. Rejecting an
// unsupported grant_kind here is the fail-fast that prevents a dead code (one
// that would always fail at redeem with "unknown grant_kind").
func (i *Issuer) validateSpec(spec IssueSpec) error {
	if spec.Count < 1 {
		return errIssueCountInvalid
	}
	if spec.Count > MaxBatchSize {
		return errIssueCountTooLarge
	}
	if !i.catalog.Supports(spec.GrantKind) {
		return errIssueGrantUnknown
	}
	if len(spec.Prefix) > 16 {
		return errIssuePrefixTooLong
	}
	if len(spec.BatchID) > 64 {
		return errIssueBatchIDTooLong
	}
	// Unlimited promo (MaxRedeems nil AND not single-use) MUST carry a hard
	// expiry (spec §5) — an unlimited, never-expiring code is an unbounded
	// liability.
	unlimited := spec.MaxRedeems == nil
	if unlimited && !spec.SingleUse && spec.ExpiresAt == nil {
		return errIssuePromoNoExpiry
	}
	return nil
}

// Issue mints spec.Count codes atomically and returns their plaintexts (once).
// Every row carries the shared batch_id; an audit record is written in the same
// transaction (so a code never exists without its generation being audited).
//
// The plaintext is generated, hashed, and only the hash is inserted; the
// plaintext is held in memory just long enough to return it to the issuer. SQL
// is fully parameterized.
func (i *Issuer) Issue(ctx context.Context, spec IssueSpec) ([]IssuedCode, error) {
	if err := i.validateSpec(spec); err != nil {
		return nil, err
	}

	paramsJSON, err := marshalParams(spec.GrantParams)
	if err != nil {
		return nil, fmt.Errorf("redemption: marshal grant_params: %w", err)
	}

	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("redemption: begin issue tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-assert the allocation bound immediately before the size-driven
	// allocation. validateSpec already rejects an out-of-range Count, but this
	// local guard makes the bound visible at the allocation site so a future
	// refactor can never route an unbounded, user-influenced Count into make()
	// (CWE-789 memory-exhaustion DoS; closes CodeQL go/uncontrolled-allocation-size).
	if spec.Count < 1 || spec.Count > MaxBatchSize {
		return nil, errIssueCountTooLarge
	}

	issued := make([]IssuedCode, 0, spec.Count)
	for n := 0; n < spec.Count; n++ {
		raw, err := generateRawCode()
		if err != nil {
			return nil, fmt.Errorf("redemption: generate code: %w", err)
		}
		canonical, err := NormalizeAndValidate(formatCode(raw, "")) // round-trip self-check
		if err != nil {
			// A freshly generated code that fails its own checksum is a bug; abort.
			return nil, fmt.Errorf("redemption: generated code failed self-validation: %w", err)
		}
		hash := HashCode(canonical)

		var id uuid.UUID
		err = tx.QueryRowContext(ctx, `
			INSERT INTO redemption_codes
				(code_hash, code_prefix, grant_kind, grant_params,
				 single_use, max_redemptions, expires_at, batch_id, created_by)
			VALUES ($1, NULLIF($2,''), $3, $4, $5, $6, $7, NULLIF($8,''), $9)
			RETURNING id`,
			hash, spec.Prefix, spec.GrantKind, paramsJSON,
			spec.SingleUse, nullableInt(spec.MaxRedeems), nullableTime(spec.ExpiresAt),
			spec.BatchID, nullableUUID(spec.CreatedBy),
		).Scan(&id)
		if err != nil {
			// A hash collision (astronomically improbable at 130 bits) or any
			// insert error aborts the whole batch — no partial issuance.
			return nil, fmt.Errorf("redemption: insert code: %w", err)
		}

		issued = append(issued, IssuedCode{
			ID:        id,
			Plaintext: formatCode(raw, spec.Prefix),
		})
	}

	// Audit the generation INSIDE the tx — issuer identity, batch_id, count,
	// grant_kind. NEVER the plaintext or hash (spec §7 / observability.md).
	if err := i.audit.RecordGeneration(ctx, tx, GenerationAudit{
		IssuerID:  spec.CreatedBy,
		Context:   spec.Context,
		BatchID:   spec.BatchID,
		GrantKind: spec.GrantKind,
		Count:     spec.Count,
		IssuedAt:  time.Now(),
	}); err != nil {
		return nil, fmt.Errorf("redemption: record generation audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("redemption: commit issue: %w", err)
	}
	return issued, nil
}

// csvSafe neutralises spreadsheet formula injection (CWE-1236). A cell whose
// first character is a formula trigger (`=`, `+`, `-`, `@`) — or a leading TAB /
// CR that some importers strip before re-examining the cell — is executed as a
// formula by Excel / Google Sheets / LibreOffice when the CSV is opened.
// Prefixing a single quote forces the importer to treat the cell as literal
// text. `batch_id` and `grant_kind` are operator-supplied free text at issue
// time (the real vector); `code` is generated, but we neutralise uniformly for
// defence in depth.
func csvSafe(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + v
	}
	return v
}

// WriteCSV exports a batch's plaintext codes as CSV for the Kickstarter backer
// survey (spec §7). Columns: code, batch_id, grant_kind. The plaintext appears
// ONLY in this issuer-side export (it is never persisted server-side). Callers
// are responsible for the export's confidentiality. Cells are formula-injection
// neutralised (CWE-1236) via csvSafe.
func WriteCSV(w io.Writer, codes []IssuedCode, batchID, grantKind string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"code", "batch_id", "grant_kind"}); err != nil {
		return fmt.Errorf("redemption: write csv header: %w", err)
	}
	for _, c := range codes {
		if err := cw.Write([]string{csvSafe(c.Plaintext), csvSafe(batchID), csvSafe(grantKind)}); err != nil {
			return fmt.Errorf("redemption: write csv row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// Revoke marks a code revoked by its registry id (operator revocation, spec §7
// runbook). A revoked code fails the atomic-claim WHERE clause, so subsequent
// redeems return the generic ErrCodeNotValid. Returns the number of rows
// affected (0 = unknown id or already revoked).
func (i *Issuer) Revoke(ctx context.Context, codeID uuid.UUID) (int64, error) {
	res, err := i.db.ExecContext(ctx, `
		UPDATE redemption_codes
		   SET revoked_at = NOW()
		 WHERE id = $1 AND revoked_at IS NULL`, codeID)
	if err != nil {
		return 0, fmt.Errorf("redemption: revoke: %w", err)
	}
	return res.RowsAffected()
}

// RevokeBatch revokes every non-revoked code sharing batch_id (campaign-wide
// kill switch). Returns rows affected.
func (i *Issuer) RevokeBatch(ctx context.Context, batchID string) (int64, error) {
	if batchID == "" {
		return 0, errors.New("redemption: batch_id required for batch revoke")
	}
	res, err := i.db.ExecContext(ctx, `
		UPDATE redemption_codes
		   SET revoked_at = NOW()
		 WHERE batch_id = $1 AND revoked_at IS NULL`, batchID)
	if err != nil {
		return 0, fmt.Errorf("redemption: revoke batch: %w", err)
	}
	return res.RowsAffected()
}

// ── helpers ────────────────────────────────────────────────────────────────

func marshalParams(p map[string]any) ([]byte, error) {
	if p == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(p)
}

func nullableInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

func nullableTime(p *time.Time) sql.NullTime {
	if p == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *p, Valid: true}
}

func nullableUUID(u uuid.NullUUID) any {
	if !u.Valid {
		return nil
	}
	return u.UUID
}

// ParseMaxRedeems converts a CLI string flag to the *int the spec uses, where
// an empty string OR "unlimited" → nil (unlimited). Exported for the CLI.
func ParseMaxRedeems(s string) (*int, error) {
	if s == "" || s == "unlimited" {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return nil, fmt.Errorf("redemption: invalid max-redeems %q (use a positive integer or 'unlimited')", s)
	}
	return &n, nil
}
