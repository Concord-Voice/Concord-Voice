package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Policy constants for pending registrations.
const (
	PendingRegistrationTTL = 15 * time.Minute
	PendingCleanupInterval = 2 * time.Minute
	MaxResends             = 4
	ResendCooldown         = 2 * time.Minute
	VerifyCodeTTLNew       = 2 * time.Minute
	MaxCodeAttempts        = 4
)

// Sentinel errors for pending registration operations.
var (
	ErrPendingNotFound      = errors.New("pending registration not found")
	ErrPendingExpired       = errors.New("pending registration expired")
	ErrResendCooldown       = errors.New("resend cooldown active")
	ErrResendsExhausted     = errors.New("resend limit reached")
	ErrEmailAlreadyRegister = errors.New("email already registered")
	ErrEmailPending         = errors.New("email pending verification")
	ErrUsernameTaken        = errors.New("username taken")
)

// PendingRegistration holds an in-progress registration awaiting email verification.
type PendingRegistration struct {
	ID                string
	Email             string
	Username          string
	PasswordHash      string
	WrappedPrivateKey []byte
	KeyDerivationSalt []byte
	KeyDerivationAlg  string
	PublicKey         []byte
	AgeVerified       bool
	ResendCount       int
	LastResendAt      *time.Time
	ExpiresAt         time.Time
	CreatedAt         time.Time
}

// PendingRepo provides database operations for pending registrations.
type PendingRepo struct {
	db *sql.DB
}

// NewPendingRepo constructs a PendingRepo backed by the given database.
func NewPendingRepo(db *sql.DB) *PendingRepo { return &PendingRepo{db: db} }

// InsertParams holds the fields needed to create a new pending registration.
type InsertParams struct {
	Email             string
	Username          string
	PasswordHash      string
	WrappedPrivateKey []byte
	KeyDerivationSalt []byte
	KeyDerivationAlg  string
	PublicKey         []byte
}

// locateEmailTakeover checks if a pending row exists for the given email. If found,
// it verifies the password and returns the row ID (without deleting). Returns the
// row ID (empty string if none found), or ErrEmailPending on password mismatch.
func locateEmailTakeover(ctx context.Context, tx *sql.Tx, lowerEmail, rawPassword string) (candidateID string, err error) {
	var existingID, existingHash string
	scanErr := tx.QueryRowContext(ctx,
		`SELECT id, password_hash FROM pending_registrations WHERE LOWER(email) = $1 FOR UPDATE`,
		lowerEmail,
	).Scan(&existingID, &existingHash)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return "", nil
	}
	if scanErr != nil {
		return "", scanErr
	}
	ok, verifyErr := VerifyPassword(rawPassword, existingHash)
	if verifyErr != nil {
		return "", verifyErr
	}
	if !ok {
		return "", ErrEmailPending
	}
	return existingID, nil
}

// locateUsernameTakeover checks if a pending row exists for the given username. If found,
// it verifies the password and returns the row ID (without deleting). Returns the
// row ID (empty string if none found), or ErrUsernameTaken on password mismatch.
func locateUsernameTakeover(ctx context.Context, tx *sql.Tx, lowerUsername, rawPassword string) (candidateID string, err error) {
	var existingID, existingHash string
	scanErr := tx.QueryRowContext(ctx,
		`SELECT id, password_hash FROM pending_registrations WHERE LOWER(username) = $1 FOR UPDATE`,
		lowerUsername,
	).Scan(&existingID, &existingHash)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return "", nil
	}
	if scanErr != nil {
		return "", scanErr
	}
	ok, verifyErr := VerifyPassword(rawPassword, existingHash)
	if verifyErr != nil {
		return "", verifyErr
	}
	if !ok {
		return "", ErrUsernameTaken
	}
	return existingID, nil
}

// checkPendingUniqueness returns an error if the submitted email or username is
// already held by a different pending registration. Pass excludeID == "" when
// there is no takeover in progress; the WHERE clause treats an empty string as
// "don't exclude anything" (a UUID column never equals "").
func checkPendingUniqueness(
	ctx context.Context,
	tx *sql.Tx,
	lowerEmail, lowerUsername, excludeID string,
) error {
	var usernameTaken bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pending_registrations
			 WHERE LOWER(username) = $1
			   AND ($2 = '' OR id::text <> $2)
		 )`,
		lowerUsername, excludeID,
	).Scan(&usernameTaken); err != nil {
		return err
	}
	if usernameTaken {
		return ErrUsernameTaken
	}

	var emailTaken bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pending_registrations
			 WHERE LOWER(email) = $1
			   AND ($2 = '' OR id::text <> $2)
		 )`,
		lowerEmail, excludeID,
	).Scan(&emailTaken); err != nil {
		return err
	}
	if emailTaken {
		return ErrEmailPending
	}
	return nil
}

// checkUsersUniqueness verifies that neither the email nor username is already
// registered in the users table. Returns ErrEmailAlreadyRegister or ErrUsernameTaken.
func checkUsersUniqueness(ctx context.Context, tx *sql.Tx, lowerEmail, lowerUsername string) error {
	var emailExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE LOWER(email) = $1)`, lowerEmail,
	).Scan(&emailExists); err != nil {
		return err
	}
	if emailExists {
		return ErrEmailAlreadyRegister
	}

	var usernameExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE LOWER(username) = $1)`, lowerUsername,
	).Scan(&usernameExists); err != nil {
		return err
	}
	if usernameExists {
		return ErrUsernameTaken
	}
	return nil
}

// InsertOrTakeover creates a new pending registration, or replaces an existing one
// if the caller can prove ownership via the correct password. It returns the new
// pending ID, its expiry, and (if a previous row was replaced) the old pending ID.
func (r *PendingRepo) InsertOrTakeover(
	ctx context.Context,
	params InsertParams,
	rawPassword string,
) (id string, expiresAt time.Time, deletedPendingID string, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, "", err
	}
	defer func() { _ = tx.Rollback() }()

	lowerEmail := strings.ToLower(params.Email)
	lowerUsername := strings.ToLower(params.Username)

	// --- Phase 1: locate takeover candidate (read + lock, do NOT delete yet) ---
	// We must perform all collision checks before any destructive operation so that
	// a later collision rejection cannot leave the user's previous pending row deleted.
	var takeoverID string
	takeoverID, err = locateEmailTakeover(ctx, tx, lowerEmail, rawPassword)
	if err != nil {
		return "", time.Time{}, "", err
	}

	if takeoverID == "" {
		// No email match — try username match.
		takeoverID, err = locateUsernameTakeover(ctx, tx, lowerUsername, rawPassword)
		if err != nil {
			return "", time.Time{}, "", err
		}
	}

	// --- Phase 2: cross-table uniqueness checks ---
	if err = checkUsersUniqueness(ctx, tx, lowerEmail, lowerUsername); err != nil {
		return "", time.Time{}, "", err
	}

	// --- Phase 2b: pending-table collision checks, excluding the takeover target ---
	// A *different* pending row holding the submitted email/username would cause a
	// unique-constraint 500 on INSERT. Return the proper sentinel (409) instead.
	// takeoverID is "" when there is no takeover; checkPendingUniqueness handles both cases.
	if err = checkPendingUniqueness(ctx, tx, lowerEmail, lowerUsername, takeoverID); err != nil {
		return "", time.Time{}, "", err
	}

	// --- Phase 3: commit — delete the old row (if any) then insert the new one ---
	if takeoverID != "" {
		if _, err = tx.ExecContext(ctx,
			`DELETE FROM pending_registrations WHERE id = $1`, takeoverID,
		); err != nil {
			return "", time.Time{}, "", err
		}
	}

	kdAlg := params.KeyDerivationAlg
	if kdAlg == "" {
		kdAlg = "argon2id"
	}

	newID := uuid.New().String()
	if err = tx.QueryRowContext(ctx,
		`INSERT INTO pending_registrations
			(id, email, username, password_hash, wrapped_private_key, key_derivation_salt,
			 key_derivation_alg, public_key, age_verified, resend_count, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, 0, $9)
		RETURNING expires_at`,
		newID,
		params.Email,
		params.Username,
		params.PasswordHash,
		params.WrappedPrivateKey,
		params.KeyDerivationSalt,
		kdAlg,
		params.PublicKey,
		time.Now().Add(PendingRegistrationTTL),
	).Scan(&expiresAt); err != nil {
		return "", time.Time{}, "", err
	}

	if err = tx.Commit(); err != nil {
		return "", time.Time{}, "", err
	}

	return newID, expiresAt, takeoverID, nil
}

// GetByID retrieves a pending registration by ID. Returns ErrPendingNotFound if
// none exists, or ErrPendingExpired (and deletes the row) if past its TTL.
func (r *PendingRepo) GetByID(ctx context.Context, id string) (*PendingRegistration, error) {
	p := &PendingRegistration{}
	var lastResendAt sql.NullTime

	err := r.db.QueryRowContext(ctx,
		`SELECT id, email, username, password_hash, wrapped_private_key, key_derivation_salt,
		        key_derivation_alg, public_key, age_verified,
		        resend_count, last_resend_at, expires_at, created_at
		   FROM pending_registrations
		  WHERE id = $1`,
		id,
	).Scan(
		&p.ID, &p.Email, &p.Username, &p.PasswordHash,
		&p.WrappedPrivateKey, &p.KeyDerivationSalt, &p.KeyDerivationAlg, &p.PublicKey,
		&p.AgeVerified,
		&p.ResendCount, &lastResendAt, &p.ExpiresAt, &p.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPendingNotFound
		}
		return nil, err
	}

	if lastResendAt.Valid {
		t := lastResendAt.Time
		p.LastResendAt = &t
	}

	if time.Now().After(p.ExpiresAt) {
		_, _ = r.db.ExecContext(ctx, `DELETE FROM pending_registrations WHERE id = $1`, id)
		return nil, ErrPendingExpired
	}

	return p, nil
}

// IncrementResend increments the resend counter for a pending registration, enforcing
// cooldown and max-resend limits. Returns the new resend count.
func (r *PendingRepo) IncrementResend(ctx context.Context, id string) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	var lastResendAt sql.NullTime
	var expiresAt time.Time

	if err = tx.QueryRowContext(ctx,
		`SELECT resend_count, last_resend_at, expires_at
		   FROM pending_registrations
		  WHERE id = $1
		  FOR UPDATE`,
		id,
	).Scan(&count, &lastResendAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrPendingNotFound
		}
		return 0, err
	}

	if time.Now().After(expiresAt) {
		return 0, ErrPendingExpired
	}

	if count >= MaxResends {
		return 0, ErrResendsExhausted
	}

	if lastResendAt.Valid && time.Since(lastResendAt.Time) < ResendCooldown {
		return 0, ErrResendCooldown
	}

	if _, err = tx.ExecContext(ctx,
		`UPDATE pending_registrations
		    SET resend_count = resend_count + 1, last_resend_at = NOW()
		  WHERE id = $1`,
		id,
	); err != nil {
		return 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}

	return count + 1, nil
}

// RevertResend decrements the resend counter by one (minimum 0) and clears the
// last_resend_at timestamp. Used to undo an increment when a send fails.
func (r *PendingRepo) RevertResend(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE pending_registrations
		    SET resend_count = GREATEST(resend_count - 1, 0), last_resend_at = NULL
		  WHERE id = $1`,
		id,
	)
	return err
}

// UpdateEmail replaces the email on a pending registration, resets the resend
// counter, and enforces uniqueness against both the pending table and users.
func (r *PendingRepo) UpdateEmail(ctx context.Context, id, newEmail string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Step 1: Lock the pending row and check expiry.
	var expiresAt time.Time
	if err = tx.QueryRowContext(ctx,
		`SELECT expires_at FROM pending_registrations WHERE id = $1 FOR UPDATE`,
		id,
	).Scan(&expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPendingNotFound
		}
		return err
	}
	if time.Now().After(expiresAt) {
		return ErrPendingExpired
	}

	lowerEmail := strings.ToLower(strings.TrimSpace(newEmail))

	// Uniqueness check: pending table (excluding self)
	var pendingExists bool
	if err = tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pending_registrations WHERE LOWER(email) = $1 AND id <> $2)`,
		lowerEmail, id,
	).Scan(&pendingExists); err != nil {
		return err
	}
	if pendingExists {
		return ErrEmailPending
	}

	// Uniqueness check: users table
	var userExists bool
	if err = tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE LOWER(email) = $1)`,
		lowerEmail,
	).Scan(&userExists); err != nil {
		return err
	}
	if userExists {
		return ErrEmailAlreadyRegister
	}

	if _, err = tx.ExecContext(ctx,
		`UPDATE pending_registrations
		    SET email = $1, resend_count = 0, last_resend_at = NULL
		  WHERE id = $2`,
		lowerEmail, id,
	); err != nil {
		return err
	}

	return tx.Commit()
}

// Promote atomically converts a pending registration into a full user account,
// inserting rows into users, user_keys, and public_keys, then deleting the
// pending row. Returns the new user ID.
func (r *PendingRepo) Promote(ctx context.Context, id string) (userID string, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	// Step 1: lock the pending row
	p := &PendingRegistration{}
	var lastResendAt sql.NullTime
	if err = tx.QueryRowContext(ctx,
		`SELECT id, email, username, password_hash, wrapped_private_key, key_derivation_salt,
		        key_derivation_alg, public_key, age_verified,
		        resend_count, last_resend_at, expires_at, created_at
		   FROM pending_registrations
		  WHERE id = $1
		  FOR UPDATE`,
		id,
	).Scan(
		&p.ID, &p.Email, &p.Username, &p.PasswordHash,
		&p.WrappedPrivateKey, &p.KeyDerivationSalt, &p.KeyDerivationAlg, &p.PublicKey,
		&p.AgeVerified,
		&p.ResendCount, &lastResendAt, &p.ExpiresAt, &p.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrPendingNotFound
		}
		return "", err
	}

	if time.Now().After(p.ExpiresAt) {
		return "", ErrPendingExpired
	}

	// Step 2: insert into users
	newUserID := uuid.New().String()
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO users (id, email, username, password_hash, email_verified, age_verified)
		 VALUES ($1, $2, $3, $4, TRUE, TRUE)`,
		newUserID, p.Email, p.Username, p.PasswordHash,
	); err != nil {
		return "", err
	}

	// Step 3: insert into user_keys — forward key_derivation_alg from pending row.
	kdAlg := p.KeyDerivationAlg
	if kdAlg == "" {
		kdAlg = "argon2id"
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt, key_derivation_alg, key_version)
		 VALUES ($1, $2, $3, $4, 1)`,
		newUserID, p.WrappedPrivateKey, p.KeyDerivationSalt, kdAlg,
	); err != nil {
		return "", err
	}

	// Step 4: insert into public_keys
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO public_keys (user_id, public_key, key_version)
		 VALUES ($1, $2, 1)`,
		newUserID, p.PublicKey,
	); err != nil {
		return "", err
	}

	// Step 5: delete the pending row
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM pending_registrations WHERE id = $1`, id,
	); err != nil {
		return "", err
	}

	if err = tx.Commit(); err != nil {
		return "", err
	}

	return newUserID, nil
}

// Delete removes a pending registration by ID. Returns true if a row was deleted.
func (r *PendingRepo) Delete(ctx context.Context, id string) (bool, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM pending_registrations WHERE id = $1`, id,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// DeleteExpired removes all pending registrations past their expiry. Returns the
// number of rows deleted.
func (r *PendingRepo) DeleteExpired(ctx context.Context) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM pending_registrations WHERE expires_at < NOW()`,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
