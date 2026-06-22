package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/mfa"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

// adminWebAuthnRPDisplayName is the fixed Relying Party display name for the
// admin console. It is NOT the user-facing RP name — the admin RP is a fully
// separate relying party (own RP ID + origins) so an admin hardware key and a
// user passkey can never cross-validate (#1688 spec §2 constraint 2).
const adminWebAuthnRPDisplayName = "Concord Admin"

// ErrAAGUIDNotAllowed is returned when a registered authenticator's AAGUID is
// not present in ADMIN_WEBAUTHN_ALLOWED_AAGUIDS. Only approved hardware-key
// models may become admin credentials (#1688 spec §8).
var ErrAAGUIDNotAllowed = errors.New("admin: authenticator AAGUID is not in the allow-list")

// ErrInvalidAAGUID is returned when a credential's AAGUID bytes cannot be parsed
// as a 16-byte UUID (a malformed authenticator response). It is treated as a
// rejection — an unparseable AAGUID can never match the allow-list.
var ErrInvalidAAGUID = errors.New("admin: authenticator AAGUID is malformed")

// NewAdminWebAuthn builds a WebAuthnService bound to the DEDICATED admin relying
// party (`ADMIN_WEBAUTHN_RP_ID` / `ADMIN_WEBAUTHN_RP_ORIGINS`), never the
// user-facing `WEBAUTHN_RP_*` config. Reuses the vendored go-webauthn wrapper in
// internal/mfa — no new dependency, no custom crypto (#1688 spec §2 / §16).
func NewAdminWebAuthn(cfg *config.Config) (*mfa.WebAuthnService, error) {
	svc, err := mfa.NewWebAuthnService(cfg.AdminWebAuthnRPID, adminWebAuthnRPDisplayName, cfg.AdminWebAuthnRPOrigins)
	if err != nil {
		return nil, fmt.Errorf("new admin webauthn service: %w", err)
	}
	return svc, nil
}

// checkAAGUID reports whether a credential's AAGUID is allow-listed. The AAGUID
// arrives as 16 raw bytes (from the authenticator data); it is formatted as the
// canonical UUID string and matched case-insensitively against `allowed`.
//
//   - An empty allow-list rejects everything (fail-closed): if an operator has
//     not declared any approved authenticator models, no key may enrol. This is
//     the deliberate posture for a high-value admin surface. To avoid SILENTLY
//     bricking enrollment, config.validate() requires a non-empty list when
//     ADMIN_CONSOLE_ENABLED=true — an enabled-but-unconfigured console fails
//     loudly at startup instead (Gitar #1703).
//   - A malformed AAGUID (not 16 bytes) is rejected with ErrInvalidAAGUID.
//   - A well-formed AAGUID absent from the list is rejected with
//     ErrAAGUIDNotAllowed.
func checkAAGUID(aaguid []byte, allowed []string) error {
	id, err := uuid.FromBytes(aaguid)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAAGUID, err)
	}
	want := id.String()
	for _, a := range allowed {
		// uuid.Parse normalizes case/format; compare canonical string forms so
		// braces/upper-case entries in the allow-list still match.
		parsed, perr := uuid.Parse(a)
		if perr != nil {
			// A malformed allow-list entry can never match a valid AAGUID; skip
			// it rather than failing the whole check (an operator typo in one
			// entry should not silently disable an otherwise-correct list).
			continue
		}
		if parsed == id {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrAAGUIDNotAllowed, want)
}

// AdminRegistrationInput bundles the inputs for FinishAdminRegistration, keeping
// the function within the parameter limit (go:S107) while grouping the cohesive
// "this enrollment ceremony" values.
//
//nolint:revive // Admin* prefix is the #1688 cross-task naming contract (see types.go header).
type AdminRegistrationInput struct {
	User           *mfa.WebAuthnUser
	Session        webauthn.SessionData
	Request        *http.Request
	AdminID        string
	AllowedAAGUIDs []string // resolved ADMIN_WEBAUTHN_ALLOWED_AAGUIDS
	CredentialName string   // operator-supplied label (may be empty)
}

// FinishAdminRegistration completes the admin WebAuthn registration ceremony,
// enforces the AAGUID allow-list, and persists the credential (#1688 §8). It is
// the single seam where a freshly registered hardware key is admitted: the
// AAGUID check runs BEFORE AdminRepo.AddCredential, so a non-allow-listed key is
// never written. `in.AllowedAAGUIDs` is the resolved `ADMIN_WEBAUTHN_ALLOWED_AAGUIDS`
// list; `in.CredentialName` is an operator-supplied label (may be empty).
//
// Returns the stored AdminCredential. On AAGUID rejection it returns
// ErrAAGUIDNotAllowed (or ErrInvalidAAGUID) and writes nothing.
func FinishAdminRegistration(ctx context.Context, svc *mfa.WebAuthnService, repo *AdminRepo, in AdminRegistrationInput) (AdminCredential, error) {
	cred, err := svc.FinishRegistration(in.User, in.Session, in.Request)
	if err != nil {
		return AdminCredential{}, fmt.Errorf("finish admin registration: %w", err)
	}

	if err := checkAAGUID(cred.Authenticator.AAGUID, in.AllowedAAGUIDs); err != nil {
		// Fail-closed: a key whose model is not on the allow-list is rejected and
		// never persisted.
		return AdminCredential{}, err
	}

	transports := make([]string, 0, len(cred.Transport))
	for _, tr := range cred.Transport {
		transports = append(transports, string(tr))
	}

	stored, err := repo.AddCredential(ctx, AdminCredential{
		AdminID:        in.AdminID,
		CredentialID:   cred.ID,
		PublicKey:      cred.PublicKey,
		AAGUID:         cred.Authenticator.AAGUID,
		SignCount:      int64(cred.Authenticator.SignCount),
		CredentialName: in.CredentialName,
		Transports:     transports,
	})
	if err != nil {
		return AdminCredential{}, fmt.Errorf("persist admin credential: %w", err)
	}
	return stored, nil
}
