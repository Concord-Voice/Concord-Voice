package attestation_test

import (
	"context"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestRepository_InsertBinaryThenGet(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	err = repo.InsertBinary(ctx, attestation.PublishBinaryPayload{
		Version:  "0.2.7",
		Platform: attestation.PlatformMacOS,
		CertHash: "sha256:abc",
	}, "test-oidc-sub")
	require.NoError(t, err)

	got, err := repo.GetBinary(ctx, "0.2.7", attestation.PlatformMacOS)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "sha256:abc", got.CertHash)
	require.Equal(t, attestation.PlatformMacOS, got.Platform)
	require.Nil(t, got.RevokedAt)
	require.Equal(t, "test-oidc-sub", got.PublishedBy)
}

func TestRepository_InsertSpaThenGet(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	err = repo.InsertSPA(ctx, attestation.PublishSPAPayload{
		SpaVersion: "a1b2c3d",
		HTMLHash:   "sha256:html",
	}, "test-oidc-sub")
	require.NoError(t, err)

	got, err := repo.GetSPA(ctx, "a1b2c3d")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "sha256:html", got.HTMLHash)
	require.Nil(t, got.RevokedAt)
}

func TestRepository_InsertBinary_Idempotent_SameHash(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)
	p := attestation.PublishBinaryPayload{
		Version:  "0.3.0",
		Platform: attestation.PlatformWindows,
		CertHash: "sha256:idempotent",
	}

	require.NoError(t, repo.InsertBinary(ctx, p, "ci"))
	// Second insert with identical (version, platform, cert_hash) → idempotent, no error.
	require.NoError(t, repo.InsertBinary(ctx, p, "ci"))
}

func TestRepository_InsertBinary_Conflict_DifferentHash(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	first := attestation.PublishBinaryPayload{
		Version:  "0.4.0",
		Platform: attestation.PlatformMacOS,
		CertHash: "sha256:first",
	}
	require.NoError(t, repo.InsertBinary(ctx, first, "ci"))

	// Same version + platform but different cert_hash → ErrConflict.
	second := first
	second.CertHash = "sha256:different"
	err = repo.InsertBinary(ctx, second, "ci")
	require.ErrorIs(t, err, attestation.ErrConflict)
}

func TestRepository_RevokeBinary_Marks(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	require.NoError(t, repo.InsertBinary(ctx, attestation.PublishBinaryPayload{
		Version:  "0.5.0",
		Platform: attestation.PlatformMacOS,
		CertHash: "sha256:revoke-me",
	}, "ci"))

	require.NoError(t, repo.RevokeBinary(ctx, "0.5.0", "security: cert rotation", "admin@example.com"))

	got, err := repo.GetBinary(ctx, "0.5.0", attestation.PlatformMacOS)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.RevokedAt, "expected RevokedAt to be set after revocation")
	require.Equal(t, "security: cert rotation", got.RevokedReason)
	require.Equal(t, "admin@example.com", got.RevokedBy)
}

func TestRepository_RevokeBinary_NotFound(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	err = repo.RevokeBinary(ctx, "9.9.9", "no such version", "admin@example.com")
	require.ErrorIs(t, err, attestation.ErrNotFound)
}

func TestRepository_ListActiveBinaries_ExcludesRevoked(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	require.NoError(t, repo.InsertBinary(ctx, attestation.PublishBinaryPayload{
		Version: "0.6.0", Platform: attestation.PlatformMacOS, CertHash: "sha256:active",
	}, "ci"))
	require.NoError(t, repo.InsertBinary(ctx, attestation.PublishBinaryPayload{
		Version: "0.6.1", Platform: attestation.PlatformMacOS, CertHash: "sha256:revoked",
	}, "ci"))
	require.NoError(t, repo.RevokeBinary(ctx, "0.6.1", "test", "admin@example.com"))

	active, err := repo.ListActiveBinaries(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "0.6.0", active[0].Version)
}

func TestRepository_RevokeSPA_Marks(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	require.NoError(t, repo.InsertSPA(ctx, attestation.PublishSPAPayload{
		SpaVersion: "a1b2c3d", HTMLHash: "sha256:html",
	}, "ci"))

	require.NoError(t, repo.RevokeSPA(ctx, "a1b2c3d", "bad deploy", "admin@example.com"))

	got, err := repo.GetSPA(ctx, "a1b2c3d")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.RevokedAt)
	require.Equal(t, "bad deploy", got.RevokedReason)
	require.Equal(t, "admin@example.com", got.RevokedBy)
}

func TestRepository_RevokeSPA_NotFound(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	err = repo.RevokeSPA(ctx, "deadbef", "no such spa", "admin@example.com")
	require.ErrorIs(t, err, attestation.ErrNotFound)
}

func TestRepository_ListActiveSPAs_ExcludesRevoked(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	require.NoError(t, repo.InsertSPA(ctx, attestation.PublishSPAPayload{
		SpaVersion: "aaaa111", HTMLHash: "sha256:html1",
	}, "ci"))
	require.NoError(t, repo.InsertSPA(ctx, attestation.PublishSPAPayload{
		SpaVersion: "bbbb222", HTMLHash: "sha256:html2",
	}, "ci"))
	require.NoError(t, repo.RevokeSPA(ctx, "aaaa111", "test", "admin@example.com"))

	active, err := repo.ListActiveSPAs(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "bbbb222", active[0].SpaVersion)
}

func TestRepository_GetBinary_ReturnsNilWhenAbsent(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	got, err := repo.GetBinary(ctx, "0.0.0-nonexistent", attestation.PlatformMacOS)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRepository_GetSPA_ReturnsNilWhenAbsent(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	got, err := repo.GetSPA(ctx, "nonexis")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRepository_PruneRetention_KeepsCurrentAndPriorMinor(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	// Seed binaries: 0.1.46, 0.1.47, 0.1.48, 0.2.0, 0.2.1, 0.2.2 (all macos).
	versions := []struct {
		ver      string
		certHash string
	}{
		{"0.1.46", "sha256:p146"},
		{"0.1.47", "sha256:p147"},
		{"0.1.48", "sha256:p148"},
		{"0.2.0", "sha256:p200"},
		{"0.2.1", "sha256:p201"},
		{"0.2.2", "sha256:p202"},
	}
	for _, v := range versions {
		require.NoError(t, repo.InsertBinary(ctx, attestation.PublishBinaryPayload{
			Version:  v.ver,
			Platform: attestation.PlatformMacOS,
			CertHash: v.certHash,
		}, "ci"))
	}

	require.NoError(t, repo.PruneRetention(ctx, time.Now()))

	// After pruning:
	// Current MAJOR.MINOR = 0.2 → keep 0.2.0, 0.2.1, 0.2.2
	// Prior MINOR = 0.1 → last 2 patches = 0.1.47, 0.1.48 (max_patch=48, keep >=47)
	// 0.1.46 should be deleted
	remaining, err := repo.ListActiveBinaries(ctx)
	require.NoError(t, err)

	remainingVersions := make(map[string]bool)
	for _, rb := range remaining {
		remainingVersions[rb.Version] = true
	}
	require.True(t, remainingVersions["0.2.0"], "0.2.0 should be kept (current minor)")
	require.True(t, remainingVersions["0.2.1"], "0.2.1 should be kept (current minor)")
	require.True(t, remainingVersions["0.2.2"], "0.2.2 should be kept (current minor)")
	require.True(t, remainingVersions["0.1.47"], "0.1.47 should be kept (prior minor, last 2)")
	require.True(t, remainingVersions["0.1.48"], "0.1.48 should be kept (prior minor, last 2)")
	require.False(t, remainingVersions["0.1.46"], "0.1.46 should be pruned")
}

func TestRepository_PruneRetention_RespectsManualPin(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	// Seed current versions.
	require.NoError(t, repo.InsertBinary(ctx, attestation.PublishBinaryPayload{
		Version: "0.2.0", Platform: attestation.PlatformMacOS, CertHash: "sha256:p200",
	}, "ci"))
	require.NoError(t, repo.InsertBinary(ctx, attestation.PublishBinaryPayload{
		Version: "0.2.1", Platform: attestation.PlatformMacOS, CertHash: "sha256:p201",
	}, "ci"))

	// Seed a very old version with published_by = 'manual-pin' directly via SQL,
	// since InsertBinary accepts publishedBy as a parameter.
	_, err = db.Exec(`
		INSERT INTO release_binaries (version, platform, cert_hash, published_by, published_at)
		VALUES ('0.0.1', 'macos', 'sha256:pinned', 'manual-pin', NOW() - INTERVAL '2 years')
	`)
	require.NoError(t, err)

	require.NoError(t, repo.PruneRetention(ctx, time.Now()))

	// The manual-pin row must survive pruning regardless of age.
	got, err := repo.GetBinary(ctx, "0.0.1", attestation.PlatformMacOS)
	require.NoError(t, err)
	require.NotNil(t, got, "manual-pin row should be preserved after prune")
	require.Equal(t, "sha256:pinned", got.CertHash)
}

// TestRepository_PruneRetention_MinorZero_PriorMajorPruned covers the
// finding #28 edge case: when current minor is 0 (v1.0.x), the CTE's
// `current.minor - 1 = -1` would silently miss the prior-minor lookup. The
// fix gates the prior-minor branch on `current.minor > 0` so v1.0.x doesn't
// accidentally retain unrelated rows AND v0.x.x patches (which are prior
// MAJOR, not prior MINOR) are correctly pruned.
func TestRepository_PruneRetention_MinorZero_PriorMajorPruned(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	// Seed v1.0.0 + v1.0.1 (current MAJOR.MINOR=1.0) and v0.9.x patches
	// (prior MAJOR; should be pruned because retention is keyed off prior
	// MINOR within the same MAJOR, not arbitrary prior major versions).
	versions := []struct {
		ver      string
		certHash string
		// publishedAt is in past order so the LIMIT 1 picks the latest as current.
		publishedAtOffsetDays int
	}{
		// Older v0.9.x patches — should all be pruned.
		{"0.9.5", "sha256:p095", -30},
		{"0.9.6", "sha256:p096", -25},
		// v1.0.x patches — should all be kept (current MAJOR.MINOR).
		{"1.0.0", "sha256:p100", -10},
		{"1.0.1", "sha256:p101", -5},
	}
	for _, v := range versions {
		_, err := db.Exec(`
			INSERT INTO release_binaries (version, platform, cert_hash, published_by, published_at)
			VALUES ($1, 'macos', $2, 'ci', NOW() + ($3 || ' days')::INTERVAL)
		`, v.ver, v.certHash, v.publishedAtOffsetDays)
		require.NoError(t, err)
	}

	require.NoError(t, repo.PruneRetention(ctx, time.Now()))

	remaining, err := repo.ListActiveBinaries(ctx)
	require.NoError(t, err)
	remainingVersions := make(map[string]bool)
	for _, rb := range remaining {
		remainingVersions[rb.Version] = true
	}

	require.True(t, remainingVersions["1.0.0"], "1.0.0 should be kept (current minor)")
	require.True(t, remainingVersions["1.0.1"], "1.0.1 should be kept (current minor)")
	// v0.9.x are prior MAJOR, not prior MINOR — fall outside the retention policy.
	require.False(t, remainingVersions["0.9.5"], "0.9.5 (prior major) should be pruned when current minor=0")
	require.False(t, remainingVersions["0.9.6"], "0.9.6 (prior major) should be pruned when current minor=0")
}

// TestRepository_PruneRetention_MinorZero_DoesNotCrash is a smoke test: the
// CTE must execute without error when the current minor is 0. Even before the
// finding-#28 fix this didn't crash, but the test locks the behavior in.
func TestRepository_PruneRetention_MinorZero_DoesNotCrash(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	// Only v1.0.0 — current.minor = 0; prior minor doesn't exist.
	require.NoError(t, repo.InsertBinary(ctx, attestation.PublishBinaryPayload{
		Version: "1.0.0", Platform: attestation.PlatformMacOS, CertHash: "sha256:p100",
	}, "ci"))

	require.NoError(t, repo.PruneRetention(ctx, time.Now()),
		"PruneRetention must not error when current.minor=0")

	remaining, err := repo.ListActiveBinaries(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, "1.0.0", remaining[0].Version)
}

func TestRepository_PruneRetention_SPAWindow(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	// Insert an old SPA entry (70 days ago) and a recent one (10 days ago).
	now := time.Now()
	_, err = db.Exec(`
		INSERT INTO release_spas (spa_version, html_hash, published_by, published_at)
		VALUES
		  ('aaaa111', 'sha256:old', 'ci', $1),
		  ('bbbb222', 'sha256:new', 'ci', $2)
	`, now.AddDate(0, 0, -70), now.AddDate(0, 0, -10))
	require.NoError(t, err)

	repo := attestation.NewRepository(db)
	require.NoError(t, repo.PruneRetention(ctx, now))

	old, err := repo.GetSPA(ctx, "aaaa111")
	require.NoError(t, err)
	require.Nil(t, old, "SPA older than 60 days should be pruned")

	recent, err := repo.GetSPA(ctx, "bbbb222")
	require.NoError(t, err)
	require.NotNil(t, recent, "SPA within 60 days should be kept")
}

// TestRepository_InsertBinary_CompositePK_PerPlatformIsolation is the structural
// regression test for the BLOCK finding on migration 000066: each release ships
// three platform binaries under the same version, and the composite PRIMARY KEY
// (version, platform) lets them coexist without conflict. Pre-fix this scenario
// failed on the second insert with "duplicate key value violates unique
// constraint" because version was the sole PK.
func TestRepository_InsertBinary_CompositePK_PerPlatformIsolation(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	// Same version, three distinct platforms — each gets its own cert_hash.
	macos := attestation.PublishBinaryPayload{
		Version:  "0.2.7",
		Platform: attestation.PlatformMacOS,
		CertHash: "sha256:macos-cert",
	}
	windows := attestation.PublishBinaryPayload{
		Version:  "0.2.7",
		Platform: attestation.PlatformWindows,
		CertHash: "sha256:windows-cert",
	}
	linux := attestation.PublishBinaryPayload{
		Version:  "0.2.7",
		Platform: attestation.PlatformLinux,
		CertHash: "sha256:linux-cert",
	}

	require.NoError(t, repo.InsertBinary(ctx, macos, "build-desktop"))
	require.NoError(t, repo.InsertBinary(ctx, windows, "build-desktop"),
		"second platform insert must not conflict with first under composite PK")
	require.NoError(t, repo.InsertBinary(ctx, linux, "build-desktop"),
		"third platform insert must not conflict under composite PK")

	// All three rows must persist with distinct cert hashes.
	gotMac, err := repo.GetBinary(ctx, "0.2.7", attestation.PlatformMacOS)
	require.NoError(t, err)
	require.NotNil(t, gotMac)
	require.Equal(t, "sha256:macos-cert", gotMac.CertHash)

	gotWin, err := repo.GetBinary(ctx, "0.2.7", attestation.PlatformWindows)
	require.NoError(t, err)
	require.NotNil(t, gotWin)
	require.Equal(t, "sha256:windows-cert", gotWin.CertHash)

	gotLin, err := repo.GetBinary(ctx, "0.2.7", attestation.PlatformLinux)
	require.NoError(t, err)
	require.NotNil(t, gotLin)
	require.Equal(t, "sha256:linux-cert", gotLin.CertHash)

	// ListActive must return all three.
	active, err := repo.ListActiveBinaries(ctx)
	require.NoError(t, err)
	require.Len(t, active, 3)
}

// TestRepository_InsertBinary_CompositePK_ConflictPerPlatform verifies that the
// composite PK conflict semantics still detect cert-hash drift for a
// (version, platform) pair (i.e., the supply-chain incident signal — same
// version+platform re-published with a different cert_hash — still surfaces as
// ErrConflict).
func TestRepository_InsertBinary_CompositePK_ConflictPerPlatform(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	first := attestation.PublishBinaryPayload{
		Version:  "0.3.0",
		Platform: attestation.PlatformMacOS,
		CertHash: "sha256:original",
	}
	require.NoError(t, repo.InsertBinary(ctx, first, "ci"))

	// Same (version, platform), different hash → ErrConflict.
	conflict := first
	conflict.CertHash = "sha256:rotated"
	err = repo.InsertBinary(ctx, conflict, "ci")
	require.ErrorIs(t, err, attestation.ErrConflict)

	// Same version, DIFFERENT platform with arbitrary hash → succeeds (no conflict).
	windows := first
	windows.Platform = attestation.PlatformWindows
	windows.CertHash = "sha256:windows-cert"
	require.NoError(t, repo.InsertBinary(ctx, windows, "ci"))
}

// TestRepository_RevokeBinary_RevokesAllPlatforms verifies the
// admin-revokes-the-version semantic: when a release version is revoked,
// every platform row for that version is marked revoked. Operators revoke a
// release, not a (version, platform) pair — see ADR-0010 D13.
func TestRepository_RevokeBinary_RevokesAllPlatforms(t *testing.T) {
	ctx := context.Background()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	_, err := db.Exec("TRUNCATE release_binaries, release_spas CASCADE")
	require.NoError(t, err)

	repo := attestation.NewRepository(db)

	for _, plat := range []attestation.Platform{attestation.PlatformMacOS, attestation.PlatformWindows, attestation.PlatformLinux} {
		require.NoError(t, repo.InsertBinary(ctx, attestation.PublishBinaryPayload{
			Version:  "0.4.2",
			Platform: plat,
			CertHash: "sha256:" + string(plat),
		}, "ci"))
	}

	require.NoError(t, repo.RevokeBinary(ctx, "0.4.2", "compromised signing key", "security-team@example.com"))

	// Every platform row must be revoked with the same audit metadata.
	for _, plat := range []attestation.Platform{attestation.PlatformMacOS, attestation.PlatformWindows, attestation.PlatformLinux} {
		got, err := repo.GetBinary(ctx, "0.4.2", plat)
		require.NoError(t, err)
		require.NotNil(t, got, "platform %s row should exist", plat)
		require.NotNil(t, got.RevokedAt, "platform %s should be revoked", plat)
		require.Equal(t, "compromised signing key", got.RevokedReason)
		require.Equal(t, "security-team@example.com", got.RevokedBy)
	}

	// ListActive returns nothing for this version.
	active, err := repo.ListActiveBinaries(ctx)
	require.NoError(t, err)
	for _, rb := range active {
		require.NotEqual(t, "0.4.2", rb.Version, "revoked rows must not appear in ListActive")
	}
}
