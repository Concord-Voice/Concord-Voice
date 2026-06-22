package redemption

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Sentinel errors returned by Redeem. The HTTP layer collapses ALL of these
// into ONE generic user-facing "code not valid" response (no oracle — the
// caller must never learn which condition failed; spec §4 step 3 / §8). They
// are distinct here only so server-side logging + metrics can categorize
// PII-safely (the category name, never the code).
var (
	// ErrCodeNotValid covers every "this code cannot be redeemed" outcome:
	// bad checksum, not found, revoked, expired, not-yet-valid, exhausted.
	// One sentinel = no oracle by construction.
	ErrCodeNotValid = errors.New("redemption: code not valid")
	// ErrAlreadyRedeemed is the per-user dedup outcome (the UNIQUE(code_id,
	// user_id) ledger constraint fired). Surfaced as its own idempotent
	// response ("already redeemed") — this is NOT an oracle leak: it reveals
	// only that THIS authenticated user already redeemed THIS code, which they
	// performed themselves, not whether the code is otherwise valid.
	ErrAlreadyRedeemed = errors.New("redemption: already redeemed")
)

// Notifier runs the post-commit live-update (cache invalidate + WS push). It is
// the entitlements.OnTierChange convergence point, injected so the redemption
// package doesn't import websocket. May be nil (engine then only invalidates
// cache via the Cache handle, or skips notification entirely in CLI contexts).
type Notifier interface {
	OnTierChange(ctx context.Context, userID uuid.UUID, oldTier, newTier string) error
}

// Engine owns the atomic redeem flow. It holds the DB handle, the grant-effect
// catalog, and an optional post-commit notifier.
type Engine struct {
	db       *sql.DB
	catalog  *Catalog
	notifier Notifier
}

// NewEngine builds the redeem engine.
func NewEngine(db *sql.DB, catalog *Catalog, notifier Notifier) *Engine {
	return &Engine{db: db, catalog: catalog, notifier: notifier}
}

// RedeemOutcome is the success payload returned to the caller — what was
// granted, so the UI can confirm specifically (spec §4 step 7).
type RedeemOutcome struct {
	Description string `json:"description"`
}

// Redeem executes the full atomic redemption for rawInput on behalf of userID.
//
// Security-critical sequence (spec §4), entirely within ONE transaction:
//  1. Checksum pre-filter (NormalizeAndValidate) — reject typos/probes pre-DB.
//  2. SHA-256 → atomic conditional UPDATE … RETURNING. The WHERE clause is the
//     concurrency guard: redemption_count is incremented ONLY if the row is
//     live (not revoked/expired/not-yet-valid) AND under max_redemptions, in a
//     single statement. No check-then-update across a round-trip — N concurrent
//     redeems of a max_redemptions=1 code yield exactly one RETURNING row.
//  3. Insert the ledger row; UNIQUE(code_id,user_id) catches per-user re-redeem.
//  4. Dispatch the grant effect by grant_kind (same tx — all-or-nothing).
//  5. Commit, THEN run the post-commit tier-change notification.
//
// Every failure mode that means "not redeemable" returns ErrCodeNotValid so the
// HTTP layer can present one generic error (no oracle).
func (e *Engine) Redeem(ctx context.Context, userID uuid.UUID, rawInput string) (RedeemOutcome, error) {
	canonical, err := NormalizeAndValidate(rawInput)
	if err != nil {
		// Checksum failure: a typo or random probe. Reject WITHOUT a DB hit and
		// collapse to the generic error (no oracle, anti-enumeration).
		return RedeemOutcome{}, ErrCodeNotValid
	}
	codeHash := HashCode(canonical)

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return RedeemOutcome{}, fmt.Errorf("redemption: begin tx: %w", err)
	}
	// Rollback is a no-op after a successful Commit; safe to always defer.
	defer func() { _ = tx.Rollback() }()

	// ── Step 2: atomic claim ────────────────────────────────────────────
	claim, err := e.claimCode(ctx, tx, codeHash)
	if err != nil {
		return RedeemOutcome{}, err
	}

	// Resolve the grant effect BEFORE mutating the ledger. An unknown grant_kind
	// (binary too old for this code) grants nothing — rollback releases the
	// claim increment too, so the code is not consumed by a binary that can't
	// honor it.
	effect, err := e.catalog.lookup(claim.grantKind)
	if err != nil {
		return RedeemOutcome{}, ErrCodeNotValid
	}

	// ── Step 3: per-user dedup ledger insert ───────────────────────────
	redemptionID, err := insertLedgerRow(ctx, tx, claim.codeID, userID)
	if err != nil {
		return RedeemOutcome{}, err
	}

	// ── Step 4: apply the grant effect + back-fill (same tx) ───────────
	result, err := applyGrant(ctx, tx, effect, userID, claim.grantKind, claim.grantBytes, redemptionID)
	if err != nil {
		return RedeemOutcome{}, err
	}

	// ── Step 5: commit, then notify ────────────────────────────────────
	if err := tx.Commit(); err != nil {
		return RedeemOutcome{}, fmt.Errorf("redemption: commit: %w", err)
	}
	return e.notifyAfterCommit(ctx, userID, result)
}

// claimResult is the row returned by the atomic conditional UPDATE.
type claimResult struct {
	codeID     uuid.UUID
	grantKind  string
	grantBytes []byte
}

// claimCode runs the step-2 atomic conditional UPDATE — the double-spend guard.
// A row is returned ONLY when the code is live AND has a remaining redemption
// slot; the increment and the eligibility check are the SAME statement, so
// concurrent callers serialize on the row lock and exactly one observes the last
// slot. A zero-row match (not found / revoked / expired / not-yet-valid /
// exhausted) collapses to the generic ErrCodeNotValid (no oracle); nothing was
// mutated, so the caller's rollback is clean.
func (e *Engine) claimCode(ctx context.Context, tx *sql.Tx, codeHash string) (claimResult, error) {
	var claim claimResult
	err := tx.QueryRowContext(ctx, `
		UPDATE redemption_codes
		   SET redemption_count = redemption_count + 1
		 WHERE code_hash = $1
		   AND revoked_at IS NULL
		   AND valid_from <= NOW()
		   AND (expires_at IS NULL OR expires_at > NOW())
		   AND (max_redemptions IS NULL OR redemption_count < max_redemptions)
		RETURNING id, grant_kind, grant_params`,
		codeHash).Scan(&claim.codeID, &claim.grantKind, &claim.grantBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return claimResult{}, ErrCodeNotValid
	}
	if err != nil {
		return claimResult{}, fmt.Errorf("redemption: atomic claim: %w", err)
	}
	return claim, nil
}

// insertLedgerRow does the step-3 per-user dedup ledger insert. The
// UNIQUE(code_id,user_id) constraint turns a re-redeem by the SAME user into
// ErrAlreadyRedeemed — the caller's rollback then undoes the count++ so a repeat
// attempt never inflates redemption_count. resulting_subscription_id is
// back-filled by applyGrant for premium grants.
func insertLedgerRow(ctx context.Context, tx *sql.Tx, codeID, userID uuid.UUID) (uuid.UUID, error) {
	var redemptionID uuid.UUID
	err := tx.QueryRowContext(ctx, `
		INSERT INTO code_redemptions (code_id, user_id)
		VALUES ($1, $2)
		RETURNING id`,
		codeID, userID).Scan(&redemptionID)
	if isUniqueViolation(err) {
		return uuid.Nil, ErrAlreadyRedeemed
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("redemption: ledger insert: %w", err)
	}
	return redemptionID, nil
}

// applyGrant does step 4 — decode grant_params, dispatch the grant effect inside
// tx, and back-fill the resulting subscription id on the ledger row. A
// grant-effect error rolls the whole redemption back (no partial grant, no
// consumed code — fail-safe, spec §4); an unknown grant_kind collapses to the
// generic ErrCodeNotValid (no oracle).
func applyGrant(ctx context.Context, tx *sql.Tx, effect GrantEffect, userID uuid.UUID, grantKind string, grantBytes []byte, redemptionID uuid.UUID) (GrantResult, error) {
	params, err := decodeParams(grantBytes)
	if err != nil {
		return GrantResult{}, fmt.Errorf("redemption: decode grant_params: %w", err)
	}
	result, err := effect(ctx, tx, userID, grantKind, params)
	if err != nil {
		if errors.Is(err, errUnknownGrantKind) {
			return GrantResult{}, ErrCodeNotValid
		}
		return GrantResult{}, fmt.Errorf("redemption: apply grant: %w", err)
	}

	// Back-fill the resulting subscription id on the ledger row (premium grants).
	if result.SubscriptionID.Valid {
		if _, err := tx.ExecContext(ctx, `
			UPDATE code_redemptions
			   SET resulting_subscription_id = $1
			 WHERE id = $2`,
			result.SubscriptionID.UUID, redemptionID); err != nil {
			return GrantResult{}, fmt.Errorf("redemption: link subscription: %w", err)
		}
	}
	return result, nil
}

// notifyAfterCommit runs the step-5 post-commit live update. Done AFTER commit so
// the grant is durable before the client is told; a notifier error does NOT undo
// the (committed) grant — it only means the client refreshes on its next poll
// instead of live. On a notify failure the (populated) description is returned
// ALONGSIDE the non-fatal wrapped error, exactly as the inline step-5 did: the
// caller logs it PII-safe and the grant stands.
func (e *Engine) notifyAfterCommit(ctx context.Context, userID uuid.UUID, result GrantResult) (RedeemOutcome, error) {
	if result.TierChanged && e.notifier != nil {
		if err := e.notifier.OnTierChange(ctx, userID, result.OldTier, result.NewTier); err != nil {
			return RedeemOutcome{Description: result.Description},
				fmt.Errorf("redemption: post-commit notify: %w", err)
		}
	}
	return RedeemOutcome{Description: result.Description}, nil
}

// decodeParams unmarshals grant_params JSONB into a map. A NULL/empty column
// (the DEFAULT '{}') decodes to an empty map, never nil-panics downstream.
func decodeParams(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber() // preserve integer fidelity for {"months": N}
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint
// violation (SQLSTATE 23505) — used to detect the per-user ledger dedup hit.
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}
