package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// Sentinel errors for the admin repository.
var (
	// ErrAdminNotFound is returned when no admin matches the lookup.
	ErrAdminNotFound = errors.New("admin: admin not found")
	// ErrDuplicateUsername is returned when CreatePending hits the UNIQUE(username) constraint.
	ErrDuplicateUsername = errors.New("admin: username already exists")
)

// pgUniqueViolation is the Postgres SQLSTATE for a unique-constraint violation.
const pgUniqueViolation = "23505"

// AdminRepo provides parameterized CRUD over admin_users and
// admin_webauthn_credentials (#1688). It uses database/sql (*sql.DB) consistent
// with the rest of the control-plane.
//
//nolint:revive // Admin* prefix is the #1688 cross-task naming contract (see types.go header).
type AdminRepo struct {
	db *sql.DB
}

// NewAdminRepo wires an AdminRepo against the given DB.
func NewAdminRepo(db *sql.DB) *AdminRepo {
	return &AdminRepo{db: db}
}

// CreatePending inserts a new admin in the pending_enrollment state. It returns
// ErrDuplicateUsername if the username is already taken.
func (r *AdminRepo) CreatePending(ctx context.Context, username, passwordHash string) (AdminUser, error) {
	const q = `
		INSERT INTO admin_users (username, password_hash, status)
		VALUES ($1, $2, $3)
		RETURNING id, username, password_hash, status, created_at, updated_at, disabled_at
	`
	var u AdminUser
	var status string
	var disabledAt sql.NullTime
	err := r.db.QueryRowContext(ctx, q, username, passwordHash, string(StatusPending)).Scan(
		&u.ID, &u.Username, &u.PasswordHash, &status, &u.CreatedAt, &u.UpdatedAt, &disabledAt,
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && string(pqErr.Code) == pgUniqueViolation {
			return AdminUser{}, ErrDuplicateUsername
		}
		return AdminUser{}, fmt.Errorf("create pending admin: %w", err)
	}
	u.Status = AdminStatus(status)
	if disabledAt.Valid {
		u.DisabledAt = &disabledAt.Time
	}
	return u, nil
}

// GetByUsername returns the admin with the given username, or ErrAdminNotFound.
func (r *AdminRepo) GetByUsername(ctx context.Context, username string) (*AdminUser, error) {
	const q = `
		SELECT id, username, password_hash, status, created_at, updated_at, disabled_at
		FROM admin_users WHERE username = $1
	`
	var u AdminUser
	var status string
	var disabledAt sql.NullTime
	err := r.db.QueryRowContext(ctx, q, username).Scan(
		&u.ID, &u.Username, &u.PasswordHash, &status, &u.CreatedAt, &u.UpdatedAt, &disabledAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAdminNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get admin by username: %w", err)
	}
	u.Status = AdminStatus(status)
	if disabledAt.Valid {
		u.DisabledAt = &disabledAt.Time
	}
	return &u, nil
}

// SetStatus updates an admin's status (and touches updated_at). Returns
// ErrAdminNotFound when no row matches the id.
func (r *AdminRepo) SetStatus(ctx context.Context, adminID string, status AdminStatus) error {
	const q = `UPDATE admin_users SET status = $2, updated_at = now() WHERE id = $1`
	res, err := r.db.ExecContext(ctx, q, adminID, string(status))
	if err != nil {
		return fmt.Errorf("set admin status: %w", err)
	}
	return requireOneRow(res)
}

// Disable marks an admin disabled and records disabled_at. Returns
// ErrAdminNotFound when no row matches the id.
func (r *AdminRepo) Disable(ctx context.Context, adminID string) error {
	const q = `
		UPDATE admin_users
		SET status = $2, disabled_at = now(), updated_at = now()
		WHERE id = $1
	`
	res, err := r.db.ExecContext(ctx, q, adminID, string(StatusDisabled))
	if err != nil {
		return fmt.Errorf("disable admin: %w", err)
	}
	return requireOneRow(res)
}

// AddCredential persists a registered WebAuthn credential for an admin and
// returns the stored row (with its generated id + created_at).
func (r *AdminRepo) AddCredential(ctx context.Context, c AdminCredential) (AdminCredential, error) {
	const q = `
		INSERT INTO admin_webauthn_credentials
			(admin_id, credential_id, public_key, aaguid, sign_count, credential_name, transports)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at
	`
	var name sql.NullString
	if c.CredentialName != "" {
		name = sql.NullString{String: c.CredentialName, Valid: true}
	}
	err := r.db.QueryRowContext(ctx, q,
		c.AdminID, c.CredentialID, c.PublicKey, c.AAGUID, c.SignCount, name, pq.Array(c.Transports),
	).Scan(&c.ID, &c.CreatedAt)
	if err != nil {
		return AdminCredential{}, fmt.Errorf("add admin credential: %w", err)
	}
	return c, nil
}

// UpdateCredentialSignCount persists the authenticator's advanced sign counter
// after a successful assertion, and bumps last_used_at. The sign count is
// WebAuthn's clone-detection signal: go-webauthn validates each assertion's
// counter against the stored value, so the stored value MUST advance — otherwise
// a cloned authenticator replaying a stale counter is never detected (#1688,
// Gitar finding). Keyed by the unique credential_id.
func (r *AdminRepo) UpdateCredentialSignCount(ctx context.Context, credentialID []byte, signCount int64) error {
	const q = `UPDATE admin_webauthn_credentials SET sign_count = $1, last_used_at = now() WHERE credential_id = $2`
	res, err := r.db.ExecContext(ctx, q, signCount, credentialID)
	if err != nil {
		return fmt.Errorf("update credential sign count: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update credential sign count rows: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("update credential sign count: expected 1 row, got %d", n)
	}
	return nil
}

// ListCredentials returns all WebAuthn credentials bound to an admin, ordered by
// creation time. The slice is empty (not nil error) when the admin has none.
func (r *AdminRepo) ListCredentials(ctx context.Context, adminID string) ([]AdminCredential, error) {
	const q = `
		SELECT id, admin_id, credential_id, public_key, aaguid, sign_count,
		       credential_name, transports, created_at, last_used_at
		FROM admin_webauthn_credentials
		WHERE admin_id = $1
		ORDER BY created_at ASC
	`
	rows, err := r.db.QueryContext(ctx, q, adminID)
	if err != nil {
		return nil, fmt.Errorf("list admin credentials: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is not actionable (matches repo-wide pattern)

	var creds []AdminCredential
	for rows.Next() {
		var c AdminCredential
		var name sql.NullString
		var lastUsed sql.NullTime
		if err := rows.Scan(
			&c.ID, &c.AdminID, &c.CredentialID, &c.PublicKey, &c.AAGUID, &c.SignCount,
			&name, pq.Array(&c.Transports), &c.CreatedAt, &lastUsed,
		); err != nil {
			return nil, fmt.Errorf("scan admin credential: %w", err)
		}
		if name.Valid {
			c.CredentialName = name.String
		}
		if lastUsed.Valid {
			c.LastUsedAt = &lastUsed.Time
		}
		creds = append(creds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin credentials: %w", err)
	}
	return creds, nil
}

// DeleteCredentials removes ALL WebAuthn credentials bound to an admin. It is
// the break-glass revocation primitive used by `adminctl reset-enrollment`
// (#1688 §9): a lost or compromised hardware key set is cleared so the admin
// must re-enrol a fresh key. Returns the number of rows removed (0 when the
// admin already had no credentials — not an error).
func (r *AdminRepo) DeleteCredentials(ctx context.Context, adminID string) error {
	const q = `DELETE FROM admin_webauthn_credentials WHERE admin_id = $1`
	if _, err := r.db.ExecContext(ctx, q, adminID); err != nil {
		return fmt.Errorf("delete admin credentials: %w", err)
	}
	return nil
}

// CountActiveCredentials returns how many WebAuthn credentials an admin holds.
// Used by revocation flows to refuse removing an admin's last key.
func (r *AdminRepo) CountActiveCredentials(ctx context.Context, adminID string) (int, error) {
	const q = `SELECT COUNT(*) FROM admin_webauthn_credentials WHERE admin_id = $1`
	var n int
	if err := r.db.QueryRowContext(ctx, q, adminID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count admin credentials: %w", err)
	}
	return n, nil
}

// requireOneRow maps a zero-rows-affected result to ErrAdminNotFound.
func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrAdminNotFound
	}
	return nil
}
