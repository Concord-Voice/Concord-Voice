package redemption

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
)

// GrantResult is what a grant-effect produces: a human-facing description the
// UI echoes back ("Premium unlocked for 12 months") plus the optional
// subscription row id created/extended by a premium grant (recorded in the
// code_redemptions ledger). oldTier/newTier feed the post-commit
// OnTierChange notification (cache invalidate + entitlements_changed push) so
// the client updates live without a re-fetch.
type GrantResult struct {
	// Description is a non-secret, user-safe summary of what was granted.
	Description string
	// SubscriptionID is set only by premium:subscription (else uuid.Nil) and is
	// stored as code_redemptions.resulting_subscription_id.
	SubscriptionID uuid.NullUUID
	// TierChanged is true when the user's tier moved (premium grant). When set,
	// the engine runs entitlements.OnTierChange AFTER the transaction commits.
	TierChanged      bool
	OldTier, NewTier string
}

// GrantEffect applies one grant_kind's side effect INSIDE the redeem
// transaction (tx). It must be deterministic, parameterized-SQL only, and must
// NOT itself commit/rollback — the engine owns the transaction boundary so the
// code claim + ledger insert + grant effect are all-or-nothing (spec §4
// "Fail-safe: any ambiguity → grant nothing").
//
// grantKind is the full key (e.g. "feature:custom_themes"); params is the
// decoded grant_params JSONB. userID is the redeeming user.
type GrantEffect func(ctx context.Context, tx *sql.Tx, userID uuid.UUID, grantKind string, params map[string]any) (GrantResult, error)

// errUnknownGrantKind is returned when no catalog entry matches. The engine
// maps this to a generic failure AND rolls back — a code whose grant_kind the
// running binary doesn't understand grants nothing (fail-safe), never a partial
// or silently-ignored grant.
var errUnknownGrantKind = errors.New("redemption: unknown grant_kind")

// Catalog is the extensible grant-effect registry (spec §6). Adding a reward
// type is a new entry + an effect function — NO change to the redeem flow.
// Keyed by an exact grant_kind for fixed kinds, OR by a "<namespace>:" prefix
// for parameterized families (feature:*, cosmetic:*) via prefixEffects.
type Catalog struct {
	exact    map[string]GrantEffect
	prefixes []prefixEntry
}

type prefixEntry struct {
	prefix string
	effect GrantEffect
}

// Grant kind keys / namespaces.
const (
	GrantPremiumSubscription = "premium:subscription"
	GrantFeaturePrefix       = "feature:"  // feature:<capability>
	GrantCosmeticPrefix      = "cosmetic:" // cosmetic:<id>
)

// NewCatalog builds the default catalog wired to the live grant effects. The
// premium effect needs no extra deps beyond the tx; tier-change notification is
// run by the engine post-commit via the injected notifier (see engine.go).
func NewCatalog() *Catalog {
	c := &Catalog{exact: make(map[string]GrantEffect)}
	c.exact[GrantPremiumSubscription] = grantPremiumSubscription
	c.RegisterPrefix(GrantFeaturePrefix, grantFeatureFlag)
	c.RegisterPrefix(GrantCosmeticPrefix, grantCosmetic)
	return c
}

// Register adds (or overrides) an exact-match grant effect. Test/extension seam.
func (c *Catalog) Register(grantKind string, effect GrantEffect) {
	c.exact[grantKind] = effect
}

// RegisterPrefix adds a namespace effect matched by leading prefix.
func (c *Catalog) RegisterPrefix(prefix string, effect GrantEffect) {
	c.prefixes = append(c.prefixes, prefixEntry{prefix: prefix, effect: effect})
}

// lookup resolves the effect for a grant_kind: exact match first, then the
// first matching prefix. Returns errUnknownGrantKind when nothing matches.
func (c *Catalog) lookup(grantKind string) (GrantEffect, error) {
	if e, ok := c.exact[grantKind]; ok {
		return e, nil
	}
	for _, p := range c.prefixes {
		if strings.HasPrefix(grantKind, p.prefix) && len(grantKind) > len(p.prefix) {
			return p.effect, nil
		}
	}
	return nil, errUnknownGrantKind
}

// Supports reports whether the catalog can apply a grant_kind. Used by the
// issuer to reject generating a code for an effect the binary can't honor
// (fail-fast at issue time rather than silent failure at redeem time).
func (c *Catalog) Supports(grantKind string) bool {
	_, err := c.lookup(grantKind)
	return err == nil
}

// ── Grant effects ─────────────────────────────────────────────────────────

const defaultPremiumMonths = 1

// grantPremiumSubscription creates or extends the user's active subscription
// row to premium, source='code'. grant_params {"months": N} extends
// current_period_end by N months (default 1). It UPSERTs against the partial
// unique index idx_subscriptions_user_active (one active sub per user): if an
// active row exists it extends from the greater of NOW() and the existing
// period end (so stacking codes accumulate); otherwise it inserts a fresh row.
//
// All SQL is parameterized. The tier move (free→premium, or premium→premium
// extension) is reported via GrantResult so the engine fires OnTierChange after
// commit.
func grantPremiumSubscription(ctx context.Context, tx *sql.Tx, userID uuid.UUID, _ string, params map[string]any) (GrantResult, error) {
	months := monthsFromParams(params)

	// Serialize same-user premium grants on the USER row. Two premium codes
	// redeemed by the same user concurrently with NO prior subscription would
	// otherwise BOTH see "no existing sub" (a FOR UPDATE on the subscriptions
	// SELECT locks nothing when zero rows match) and BOTH attempt an INSERT,
	// racing the partial unique index idx_subscriptions_user_active. Locking the
	// always-present users row first gives a real lock target, so the second
	// transaction blocks until the first commits and then observes the inserted
	// subscription via the extend path. The lock order is consistent
	// (users row → subscriptions row), so it cannot deadlock with the engine's
	// redemption_codes-row claim (a disjoint table touched earlier).
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		return GrantResult{}, fmt.Errorf("redemption: lock user for premium grant: %w", err)
	}

	// Read the user's current active subscription (if any).
	var (
		existingID  uuid.UUID
		periodEnd   sql.NullTime
		hasExisting bool
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, current_period_end
		  FROM subscriptions
		 WHERE user_id = $1
		   AND status IN ('active', 'trialing', 'past_due')`, userID).Scan(&existingID, &periodEnd)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		hasExisting = false
	case err != nil:
		return GrantResult{}, fmt.Errorf("redemption: read existing subscription: %w", err)
	default:
		hasExisting = true
	}

	// Extend from the later of NOW() and the existing period end so an already-
	// active premium stacks rather than truncates.
	base := time.Now()
	if hasExisting && periodEnd.Valid && periodEnd.Time.After(base) {
		base = periodEnd.Time
	}
	newEnd := base.AddDate(0, months, 0)

	var subID uuid.UUID
	if hasExisting {
		subID = existingID
		// Preserve an externally-managed source (e.g. 'stripe') — redeeming a
		// code must never silently rewrite the billing source of record. Today
		// every active sub is source='code' (no Stripe integration yet) so the
		// CASE is a no-op; it is forward-safe for when Stripe (C1/C2, #1305/#1306)
		// lands. TODO(#1305/#1306): define full reconciliation when a paying
		// Stripe subscriber redeems a code (period stacking vs. the Stripe
		// billing period) — for now we extend the period and keep the source.
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions
			   SET tier = $1, status = 'active',
			       source = CASE WHEN source = 'stripe' THEN source ELSE 'code' END,
			       current_period_end = $2, updated_at = NOW()
			 WHERE id = $3`,
			entitlements.TierPremium, newEnd, subID); err != nil {
			return GrantResult{}, fmt.Errorf("redemption: extend subscription: %w", err)
		}
	} else {
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO subscriptions (user_id, tier, status, source, current_period_end)
			VALUES ($1, $2, 'active', 'code', $3)
			RETURNING id`,
			userID, entitlements.TierPremium, newEnd).Scan(&subID); err != nil {
			return GrantResult{}, fmt.Errorf("redemption: create subscription: %w", err)
		}
	}

	// oldTier: premium if the user already had an active sub, else free. The
	// engine uses this to decide whether OnTierChange is a no-op (both premium)
	// or a real upgrade (free→premium). Either way OnTierChange invalidates the
	// cache and pushes the live update.
	oldTier := entitlements.TierFree
	if hasExisting {
		oldTier = entitlements.TierPremium
	}

	plural := "month"
	if months != 1 {
		plural = "months"
	}
	return GrantResult{
		Description:    fmt.Sprintf("Premium unlocked for %d %s", months, plural),
		SubscriptionID: uuid.NullUUID{UUID: subID, Valid: true},
		TierChanged:    true,
		OldTier:        oldTier,
		NewTier:        entitlements.TierPremium,
	}, nil
}

// grantFeatureFlag is the feature:<capability> effect. Beta scope: feature/
// cosmetic grants are recorded in the ledger (the code_redemptions row IS the
// grant of record) and surfaced to the client via the redeem response; a
// dedicated per-user feature/cosmetic store is a follow-on (the schema for it
// does not exist in the Beta foundation). The effect is therefore a no-op DB
// write here — the durable record is the ledger row the engine inserts. It
// validates the capability segment is non-empty so a malformed "feature:" code
// can never have been issued (the issuer also rejects it).
func grantFeatureFlag(_ context.Context, _ *sql.Tx, _ uuid.UUID, grantKind string, _ map[string]any) (GrantResult, error) {
	capability := strings.TrimPrefix(grantKind, GrantFeaturePrefix)
	if capability == "" {
		return GrantResult{}, errUnknownGrantKind
	}
	return GrantResult{Description: fmt.Sprintf("Feature unlocked: %s", capability)}, nil
}

// grantCosmetic is the cosmetic:<id> effect (badges, profile rewards). Same
// Beta posture as grantFeatureFlag: the ledger row is the grant of record.
func grantCosmetic(_ context.Context, _ *sql.Tx, _ uuid.UUID, grantKind string, _ map[string]any) (GrantResult, error) {
	id := strings.TrimPrefix(grantKind, GrantCosmeticPrefix)
	if id == "" {
		return GrantResult{}, errUnknownGrantKind
	}
	return GrantResult{Description: fmt.Sprintf("Reward unlocked: %s", id)}, nil
}

// monthsFromParams reads {"months": N} from grant_params, clamping to a sane
// range. JSON numbers decode as float64; a missing/zero/negative value defaults
// to 1 month (never grants ≤0). An absurd value is capped at 1200 months
// (100 years) so a typo in an issued code can't mint an effectively-infinite
// subscription.
func monthsFromParams(params map[string]any) int {
	const maxMonths = 1200
	v, ok := params["months"]
	if !ok {
		return defaultPremiumMonths
	}
	var n int
	switch t := v.(type) {
	case float64:
		n = int(t)
	case json.Number:
		i, err := t.Int64()
		if err != nil {
			return defaultPremiumMonths
		}
		n = int(i)
	case int:
		n = t
	default:
		return defaultPremiumMonths
	}
	if n < 1 {
		return defaultPremiumMonths
	}
	if n > maxMonths {
		return maxMonths
	}
	return n
}
