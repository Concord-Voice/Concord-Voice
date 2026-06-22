package redemption_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/redemption"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIssue_AuditRecordedNoSecret verifies that issuing a batch (a) writes the
// codes as hashes only (no plaintext column exists) and (b) records exactly one
// issuance audit row carrying issuer/context/grant_kind/count/batch — and NO
// plaintext or hash.
func TestIssue_AuditRecordedNoSecret(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	issuer := uuid.NullUUID{UUID: testhelpers.CreateUser(t, db), Valid: true}
	iss := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink())

	codes, err := iss.Issue(ctx, redemption.IssueSpec{
		GrantKind:   redemption.GrantPremiumSubscription,
		GrantParams: map[string]any{"months": 12},
		Count:       5,
		Prefix:      "KS",
		SingleUse:   true,
		MaxRedeems:  intPtr(1),
		BatchID:     "ks-2026-founder",
		CreatedBy:   issuer,
		Context:     redemption.IssuerContextAdminHTTP,
	})
	require.NoError(t, err)
	require.Len(t, codes, 5)
	for _, c := range codes {
		assert.Contains(t, c.Plaintext, "KS-", "formatted with prefix")
	}

	// 5 code rows, all with the shared batch_id and a non-empty hash.
	var codeRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM redemption_codes WHERE batch_id='ks-2026-founder'`).Scan(&codeRows))
	assert.Equal(t, 5, codeRows)

	// Hashes are 64-hex and distinct (no plaintext stored anywhere).
	rows, err := db.Query(`SELECT code_hash FROM redemption_codes WHERE batch_id='ks-2026-founder'`)
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // test cleanup
	seen := map[string]struct{}{}
	for rows.Next() {
		var h string
		require.NoError(t, rows.Scan(&h))
		assert.Len(t, h, 64)
		seen[h] = struct{}{}
	}
	require.NoError(t, rows.Err())
	assert.Len(t, seen, 5, "all hashes distinct")

	// Exactly one audit row, with the right metadata and NO secret surface.
	var (
		auditIssuer  uuid.NullUUID
		auditCtx     string
		auditKind    string
		auditCount   int
		auditBatch   string
		auditCreated time.Time
	)
	require.NoError(t, db.QueryRow(`
		SELECT issuer_id, issuer_context, grant_kind, code_count, batch_id, created_at
		  FROM redemption_code_issuance WHERE batch_id='ks-2026-founder'`,
	).Scan(&auditIssuer, &auditCtx, &auditKind, &auditCount, &auditBatch, &auditCreated))
	assert.Equal(t, issuer.UUID, auditIssuer.UUID)
	assert.Equal(t, redemption.IssuerContextAdminHTTP, auditCtx)
	assert.Equal(t, redemption.GrantPremiumSubscription, auditKind)
	assert.Equal(t, 5, auditCount)
	assert.Equal(t, "ks-2026-founder", auditBatch)
	assert.WithinDuration(t, time.Now(), auditCreated, time.Minute)
}

// TestIssue_CLINullIssuer: the CLI path issues with a nil issuer_id and
// context='cli' (operator-on-the-box, no user identity).
func TestIssue_CLINullIssuer(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	iss := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink())
	_, err := iss.Issue(ctx, redemption.IssueSpec{
		GrantKind: "feature:custom_themes", Count: 2, SingleUse: true, MaxRedeems: intPtr(1),
		BatchID: "cli-batch", Context: redemption.IssuerContextCLI,
	})
	require.NoError(t, err)

	var issuerID uuid.NullUUID
	var ctxLabel string
	require.NoError(t, db.QueryRow(
		`SELECT issuer_id, issuer_context FROM redemption_code_issuance WHERE batch_id='cli-batch'`,
	).Scan(&issuerID, &ctxLabel))
	assert.False(t, issuerID.Valid, "CLI issuance has NULL issuer_id")
	assert.Equal(t, redemption.IssuerContextCLI, ctxLabel)
}

// TestIssue_RejectsUnsupportedGrantKind: a code is NEVER issued for an effect
// the binary can't honor (fail-fast at issue, not silent failure at redeem).
func TestIssue_RejectsUnsupportedGrantKind(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	iss := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink())

	_, err := iss.Issue(context.Background(), redemption.IssueSpec{
		GrantKind: "totally:unknown", Count: 1, SingleUse: true, MaxRedeems: intPtr(1),
	})
	require.Error(t, err)

	// Nothing was written (no code rows, no audit rows).
	var codeRows, auditRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM redemption_codes`).Scan(&codeRows))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM redemption_code_issuance`).Scan(&auditRows))
	assert.Equal(t, 0, codeRows)
	assert.Equal(t, 0, auditRows)
}

// TestIssue_UnlimitedPromoRequiresExpiry enforces spec §5: an unlimited
// (max_redemptions NULL), non-single-use promo MUST carry an expiry.
func TestIssue_UnlimitedPromoRequiresExpiry(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	iss := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink())

	// No expiry → rejected.
	_, err := iss.Issue(context.Background(), redemption.IssueSpec{
		GrantKind: "feature:custom_themes", Count: 1, SingleUse: false, MaxRedeems: nil,
	})
	require.Error(t, err)

	// With expiry → accepted.
	_, err = iss.Issue(context.Background(), redemption.IssueSpec{
		GrantKind: "feature:custom_themes", Count: 1, SingleUse: false, MaxRedeems: nil,
		ExpiresAt: timePtr(time.Now().Add(30 * 24 * time.Hour)), BatchID: "promo-ok",
	})
	require.NoError(t, err)
}

// TestRevoke_ByID disables a single code so it fails the atomic-claim filter.
func TestRevoke_ByID(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	iss := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink())

	codes, err := iss.Issue(ctx, redemption.IssueSpec{
		GrantKind: "feature:custom_themes", Count: 1, SingleUse: true, MaxRedeems: intPtr(1), BatchID: "revoke-one",
	})
	require.NoError(t, err)

	n, err := iss.Revoke(ctx, codes[0].ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	// Re-revoking is a no-op (already revoked).
	n, err = iss.Revoke(ctx, codes[0].ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// The revoked code now rejects on redeem.
	eng := redemption.NewEngine(db, redemption.NewCatalog(), &recordingNotifier{})
	_, err = eng.Redeem(ctx, testhelpers.CreateUser(t, db), codes[0].Plaintext)
	assert.ErrorIs(t, err, redemption.ErrCodeNotValid)
}
