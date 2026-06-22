package admin

import "time"

// The Admin* type prefix below is the plan-mandated naming contract for #1688,
// referenced consistently across the admin package's tasks (AdminRepo, AdminUser,
// AdminCredential, AdminStatus). The slight admin.AdminX stutter that revive's
// `exported` rule flags is accepted to keep those cross-task names stable; each
// type therefore carries a targeted //nolint:revive directive.

// AdminStatus is the lifecycle state of an admin_users row (#1688). An admin is
// created `pending_enrollment` (password set, no hardware key yet), flips to
// `active` once a WebAuthn key is registered, and is `disabled` on revocation.
// The string values mirror the CHECK constraint in migration 000077.
//
//nolint:revive // Admin* prefix is the #1688 cross-task naming contract (see file header).
type AdminStatus string

const (
	// StatusPending is a freshly bootstrapped admin awaiting WebAuthn enrollment.
	StatusPending AdminStatus = "pending_enrollment"
	// StatusActive is a fully enrolled admin (password + ≥1 hardware key).
	StatusActive AdminStatus = "active"
	// StatusDisabled is a deactivated admin; no login is possible.
	StatusDisabled AdminStatus = "disabled"
)

// AdminUser is a platform-admin identity, fully isolated from end-user accounts.
// Username is an operator handle (NOT an email — no PII). PasswordHash is an
// Argon2id encoded hash produced by internal/auth.HashPassword.
//
//nolint:revive // Admin* prefix is the #1688 cross-task naming contract (see file header).
type AdminUser struct {
	ID           string
	Username     string
	PasswordHash string
	Status       AdminStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DisabledAt   *time.Time
}

// AdminCredential is a registered WebAuthn/FIDO2 authenticator bound to an admin.
// CredentialID, PublicKey, and AAGUID are raw bytes (the AAGUID is allow-list
// checked at enrollment). An admin may hold ≥1 credential (backup keys are
// encouraged to avoid lockout).
//
//nolint:revive // Admin* prefix is the #1688 cross-task naming contract (see file header).
type AdminCredential struct {
	ID             string
	AdminID        string
	CredentialID   []byte
	PublicKey      []byte
	AAGUID         []byte
	SignCount      int64
	CredentialName string
	Transports     []string
	CreatedAt      time.Time
	LastUsedAt     *time.Time
}

// Audit event_type constants written to admin_audit_log (#1688). They name the
// security-relevant admin lifecycle events; the audit row also carries a
// success/failure/denied result and a sanitized actor handle (never secrets).
const (
	// EventLoginSuccess marks a completed 2-factor admin login.
	EventLoginSuccess = "login_success"
	// EventLoginFailure marks a rejected password or WebAuthn step.
	EventLoginFailure = "login_failure"
	// EventLogout marks an explicit admin logout (session revoked).
	EventLogout = "logout"
	// EventLockout marks an account/IP entering exponential-backoff lockout.
	EventLockout = "lockout"
	// EventEnrollComplete marks a pending admin flipping to active on WebAuthn enrollment.
	EventEnrollComplete = "enroll_complete"
	// EventCredentialRevoked marks a hardware key being disabled. Not a credential value.
	EventCredentialRevoked = "credential_revoked" // #nosec G101 -- event-type label, not a secret
	// EventBootstrap marks the out-of-band creation of an admin (adminctl / in-console).
	EventBootstrap = "bootstrap"
)
