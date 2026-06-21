package attestation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Repository encapsulates Postgres access for the attestation registry.
type Repository struct {
	db *sql.DB
}

// NewRepository wires a Repository against the given DB.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// Sentinel errors.
var (
	// ErrConflict is returned when a publish conflicts with a different hash for the same version.
	ErrConflict = errors.New("attestation: publish conflict (different hash for same version)")
	// ErrNotFound is returned when no matching registry entry exists.
	ErrNotFound = errors.New("attestation: registry entry not found")
)

// InsertBinary upserts a release_binaries row.
// Idempotent if (version, platform, cert_hash) matches the existing row;
// returns ErrConflict if (version, platform) exists with a different cert_hash.
//
// release_binaries has a composite PRIMARY KEY (version, platform), so each
// release ships three rows (macos / windows / linux) under the same version
// — conflict is per-(version, platform).
func (r *Repository) InsertBinary(ctx context.Context, p PublishBinaryPayload, publishedBy string) error {
	// DO UPDATE on a no-op self-assignment is required so RETURNING returns the
	// existing row when the hash matches (idempotent path). When the hash
	// differs, the WHERE clause filters the update out → no row returned →
	// sql.ErrNoRows → ErrConflict.
	const q = `
		INSERT INTO release_binaries (version, platform, cert_hash, published_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (version, platform) DO UPDATE
			SET cert_hash = release_binaries.cert_hash
			WHERE release_binaries.cert_hash = EXCLUDED.cert_hash
		RETURNING version
	`
	var got string
	err := r.db.QueryRowContext(ctx, q, p.Version, string(p.Platform), p.CertHash, publishedBy).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("insert binary: %w", err)
	}
	return nil
}

// GetBinary returns the row matching (version, platform) or nil if absent.
// Lookup is by the composite primary key (version, platform).
func (r *Repository) GetBinary(ctx context.Context, version string, platform Platform) (*ReleaseBinary, error) {
	const q = `
		SELECT version, platform, cert_hash, published_at, published_by, revoked_at, revoked_reason, revoked_by
		FROM release_binaries WHERE version = $1 AND platform = $2
	`
	rb := &ReleaseBinary{}
	var revokedAt sql.NullTime
	var revokedReason sql.NullString
	var revokedBy sql.NullString
	err := r.db.QueryRowContext(ctx, q, version, string(platform)).Scan(
		&rb.Version, &rb.Platform, &rb.CertHash, &rb.PublishedAt, &rb.PublishedBy,
		&revokedAt, &revokedReason, &revokedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get binary: %w", err)
	}
	if revokedAt.Valid {
		rb.RevokedAt = &revokedAt.Time
	}
	if revokedReason.Valid {
		rb.RevokedReason = revokedReason.String
	}
	if revokedBy.Valid {
		rb.RevokedBy = revokedBy.String
	}
	return rb, nil
}

// ListActiveBinaries returns all non-revoked binaries (for cache hydration).
func (r *Repository) ListActiveBinaries(ctx context.Context) ([]ReleaseBinary, error) {
	const q = `
		SELECT version, platform, cert_hash, published_at, published_by, revoked_at, revoked_reason, revoked_by
		FROM release_binaries WHERE revoked_at IS NULL
	`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list active binaries: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []ReleaseBinary
	for rows.Next() {
		var rb ReleaseBinary
		var revokedAt sql.NullTime
		var revokedReason sql.NullString
		var revokedBy sql.NullString
		if err := rows.Scan(
			&rb.Version, &rb.Platform, &rb.CertHash, &rb.PublishedAt, &rb.PublishedBy,
			&revokedAt, &revokedReason, &revokedBy,
		); err != nil {
			return nil, fmt.Errorf("scan binary: %w", err)
		}
		if revokedAt.Valid {
			rb.RevokedAt = &revokedAt.Time
		}
		if revokedReason.Valid {
			rb.RevokedReason = revokedReason.String
		}
		if revokedBy.Valid {
			rb.RevokedBy = revokedBy.String
		}
		out = append(out, rb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate binaries: %w", err)
	}
	return out, nil
}

// RevokeBinary marks every release_binaries row for the given version as
// revoked, regardless of platform. Releases ship multiple platform rows under
// the same version (macos / windows / linux), and the admin-facing revoke
// surface is platform-agnostic: revoking a version revokes the whole release.
// Returns ErrNotFound if no row exists for the version or every matching row
// is already revoked.
//
// revokedBy is the admin identity recorded for forensic audit (typically the
// admin user from the request context).
func (r *Repository) RevokeBinary(ctx context.Context, version string, reason string, revokedBy string) error {
	const q = `
		UPDATE release_binaries
		SET revoked_at = NOW(), revoked_reason = $2, revoked_by = $3
		WHERE version = $1 AND revoked_at IS NULL
	`
	res, err := r.db.ExecContext(ctx, q, version, reason, revokedBy)
	if err != nil {
		return fmt.Errorf("revoke binary: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke binary rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// InsertSPA upserts a release_spas row. Same idempotency semantics as InsertBinary:
// idempotent if the hash matches; returns ErrConflict if spa_version exists with a different hash.
func (r *Repository) InsertSPA(ctx context.Context, p PublishSPAPayload, publishedBy string) error {
	// DO UPDATE on a no-op self-assignment is required so RETURNING returns the
	// existing row when the hash matches (idempotent path). When the hash
	// differs, the WHERE clause filters the update out → no row returned →
	// sql.ErrNoRows → ErrConflict.
	const q = `
		INSERT INTO release_spas (spa_version, html_hash, published_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (spa_version) DO UPDATE
			SET html_hash = release_spas.html_hash
			WHERE release_spas.html_hash = EXCLUDED.html_hash
		RETURNING spa_version
	`
	var got string
	err := r.db.QueryRowContext(ctx, q, p.SpaVersion, p.HTMLHash, publishedBy).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("insert spa: %w", err)
	}
	return nil
}

// GetSPA returns the row matching spa_version or nil if absent.
func (r *Repository) GetSPA(ctx context.Context, spaVersion string) (*ReleaseSPA, error) {
	const q = `
		SELECT spa_version, html_hash, published_at, published_by, revoked_at, revoked_reason, revoked_by
		FROM release_spas WHERE spa_version = $1
	`
	rs := &ReleaseSPA{}
	var revokedAt sql.NullTime
	var revokedReason sql.NullString
	var revokedBy sql.NullString
	err := r.db.QueryRowContext(ctx, q, spaVersion).Scan(
		&rs.SpaVersion, &rs.HTMLHash, &rs.PublishedAt, &rs.PublishedBy,
		&revokedAt, &revokedReason, &revokedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get spa: %w", err)
	}
	if revokedAt.Valid {
		rs.RevokedAt = &revokedAt.Time
	}
	if revokedReason.Valid {
		rs.RevokedReason = revokedReason.String
	}
	if revokedBy.Valid {
		rs.RevokedBy = revokedBy.String
	}
	return rs, nil
}

// ListActiveSPAs returns all non-revoked SPA entries.
func (r *Repository) ListActiveSPAs(ctx context.Context) ([]ReleaseSPA, error) {
	const q = `
		SELECT spa_version, html_hash, published_at, published_by, revoked_at, revoked_reason, revoked_by
		FROM release_spas WHERE revoked_at IS NULL
	`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list active spas: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []ReleaseSPA
	for rows.Next() {
		var rs ReleaseSPA
		var revokedAt sql.NullTime
		var revokedReason sql.NullString
		var revokedBy sql.NullString
		if err := rows.Scan(
			&rs.SpaVersion, &rs.HTMLHash, &rs.PublishedAt, &rs.PublishedBy,
			&revokedAt, &revokedReason, &revokedBy,
		); err != nil {
			return nil, fmt.Errorf("scan spa: %w", err)
		}
		if revokedAt.Valid {
			rs.RevokedAt = &revokedAt.Time
		}
		if revokedReason.Valid {
			rs.RevokedReason = revokedReason.String
		}
		if revokedBy.Valid {
			rs.RevokedBy = revokedBy.String
		}
		out = append(out, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spas: %w", err)
	}
	return out, nil
}

// RevokeSPA marks a SPA version as revoked. Returns ErrNotFound if the version
// does not exist or is already revoked.
//
// revokedBy is the admin identity recorded for forensic audit (typically the
// admin user from the request context).
func (r *Repository) RevokeSPA(ctx context.Context, spaVersion string, reason string, revokedBy string) error {
	const q = `
		UPDATE release_spas
		SET revoked_at = NOW(), revoked_reason = $2, revoked_by = $3
		WHERE spa_version = $1 AND revoked_at IS NULL
	`
	res, err := r.db.ExecContext(ctx, q, spaVersion, reason, revokedBy)
	if err != nil {
		return fmt.Errorf("revoke spa: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke spa rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PruneRetention applies the retention policy from ADR-0010 D9:
//   - Binary: keep all patches of current MAJOR.MINOR + last 2 patches of prior MINOR.
//   - SPA: 60-day time window.
//   - Both: rows with published_by = 'manual-pin' are exempt from pruning.
//
// The now parameter is the reference clock for SPA age calculations.
//
// Finding #28 of #1264 review: when the current minor is 0 (e.g., v1.0.x),
// `current.minor - 1 = -1` doesn't match any real version. The original CTE
// silently dropped the prior-minor branch in that case (matching no rows is
// indistinguishable from "no prior minor exists"), but the implicit behavior
// was fragile — a future SPLIT_PART optimization or schema change could cause
// PostgreSQL to evaluate the subquery differently. The CASE WHEN guard
// below makes the intent explicit: skip the prior-minor keepers when no
// prior minor exists.
func (r *Repository) PruneRetention(ctx context.Context, now time.Time) error {
	// Binary pruning: keep current MAJOR.MINOR (all patches) and last 2 patches of prior MINOR.
	// Rows with published_by = 'manual-pin' are always kept.
	const pruneBinary = `
		WITH current AS (
			SELECT
				SPLIT_PART(version, '.', 1)::int AS major,
				SPLIT_PART(version, '.', 2)::int AS minor
			FROM release_binaries
			WHERE revoked_at IS NULL AND published_by != 'manual-pin'
			ORDER BY published_at DESC LIMIT 1
		),
		prior_max_patch AS (
			-- Skip the lookup entirely when current.minor = 0 (no prior minor
			-- within the same major). Returns a single NULL row so the
			-- downstream JOIN doesn't produce an empty CTE (PostgreSQL CROSS
			-- JOIN against an empty set yields zero rows).
			SELECT
				CASE
					WHEN current.minor = 0 THEN NULL
					ELSE MAX(SPLIT_PART(rb.version, '.', 3)::int)
				END AS max_patch
			FROM current
			LEFT JOIN release_binaries rb
				ON SPLIT_PART(rb.version, '.', 1)::int = current.major
				AND SPLIT_PART(rb.version, '.', 2)::int = current.minor - 1
			GROUP BY current.minor
		),
		keepers AS (
			SELECT rb.version FROM release_binaries rb, current
			WHERE rb.published_by = 'manual-pin'
			UNION
			SELECT rb.version FROM release_binaries rb, current
			WHERE SPLIT_PART(rb.version, '.', 1)::int = current.major
			  AND SPLIT_PART(rb.version, '.', 2)::int = current.minor
			UNION
			-- Prior-minor patches: gated on current.minor > 0 so v*.0.x
			-- doesn't accidentally pick up unrelated rows. When minor = 0,
			-- prior_max_patch.max_patch is NULL and the >= predicate fails,
			-- but the explicit gate makes the no-prior-minor intent visible.
			SELECT rb.version FROM release_binaries rb, current, prior_max_patch
			WHERE current.minor > 0
			  AND SPLIT_PART(rb.version, '.', 1)::int = current.major
			  AND SPLIT_PART(rb.version, '.', 2)::int = current.minor - 1
			  AND SPLIT_PART(rb.version, '.', 3)::int >= COALESCE(prior_max_patch.max_patch - 1, 0)
		)
		DELETE FROM release_binaries WHERE version NOT IN (SELECT version FROM keepers)
	`
	if _, err := r.db.ExecContext(ctx, pruneBinary); err != nil {
		return fmt.Errorf("prune binaries: %w", err)
	}

	// SPA pruning: delete non-pinned entries older than 60 days.
	const pruneSPA = `
		DELETE FROM release_spas
		WHERE published_at < $1
		  AND published_by != 'manual-pin'
	`
	if _, err := r.db.ExecContext(ctx, pruneSPA, now.AddDate(0, 0, -60)); err != nil {
		return fmt.Errorf("prune spas: %w", err)
	}
	return nil
}
