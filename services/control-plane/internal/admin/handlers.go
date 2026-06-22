package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/mfa"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// loginChallengeTTL bounds the window between the password step (which returns a
// challenge handle) and the WebAuthn step (which redeems it). Short — the
// operator already has the hardware key in hand; a 5-minute cap limits the
// replay window of a leaked handle.
const loginChallengeTTL = 5 * time.Minute

// loginChallengePrefix namespaces the inter-step WebAuthn challenge in Redis. The
// stored value is the serialized webauthn.SessionData plus the resolved adminID;
// the handle is an opaque 256-bit CSPRNG token (never the username).
const loginChallengePrefix = "admin_login_challenge:"

// Shared error-response message strings, extracted to satisfy go:S1192 (these
// recur across the handler's JSON error responses).
const (
	msgInvalidRequestBody = "invalid request body"
	msgInternalError      = "internal error"
)

// adminWebAuthnUserHandle is the stable WebAuthn user handle for the admin RP.
// The admin RP authenticates a single logical "operator" identity per browser
// ceremony; the binding to a concrete admin row is carried server-side in the
// challenge handle, not in the WebAuthn user id. It mirrors the value the
// enrollment ceremony registers under.
var adminWebAuthnUserHandle = []byte("admin-user-handle")

// Handler bundles the admin auth surface (#1688): password + WebAuthn login,
// logout, enrollment, and admin-create. It is constructed once by the wiring
// layer and mounted by RegisterRoutes. The `log` field name is load-bearing —
// the log_emissions AST test inspects `<chain>.log.<Method>` calls.
//
//nolint:revive // Admin* package; Handler is the conventional name (no stutter — admin.Handler).
type Handler struct {
	repo     *AdminRepo
	audit    *AuditLog
	sessions *SessionStore
	lockout  *Lockout
	enroll   *EnrollmentStore
	webauthn *mfa.WebAuthnService
	redis    *redis.Client
	cfg      *config.Config
	log      *logger.Logger

	// dummyPasswordHash is a throwaway Argon2id hash verified against when the
	// requested username does not exist, so the password step takes the same time
	// and returns the same generic error whether or not the account exists
	// (username-enumeration defense, #1688 §7).
	dummyPasswordHash string

	// allowedAAGUIDs is the resolved ADMIN_WEBAUTHN_ALLOWED_AAGUIDS list checked at
	// enrollment.
	allowedAAGUIDs []string
}

// NewHandler builds the admin Handler, wiring every Task 4–9 component from the
// shared DB/Redis/config. It computes a throwaway dummy password hash at
// construction (never a hard-coded literal — that would trip detect-secrets and
// be a static fingerprint) for the enumeration-defense path.
func NewHandler(db *sql.DB, rdb *redis.Client, log *logger.Logger, cfg *config.Config) (*Handler, error) {
	webauthnSvc, err := NewAdminWebAuthn(cfg)
	if err != nil {
		return nil, fmt.Errorf("admin handler: build webauthn: %w", err)
	}

	dummyHash, err := newDummyPasswordHash()
	if err != nil {
		return nil, fmt.Errorf("admin handler: seed enumeration-defense hash: %w", err)
	}

	return &Handler{
		repo:              NewAdminRepo(db),
		audit:             NewAuditLog(db),
		sessions:          NewSessionStore(rdb, nil),
		lockout:           NewLockout(rdb, nil),
		enroll:            NewEnrollmentStore(rdb),
		webauthn:          webauthnSvc,
		redis:             rdb,
		cfg:               cfg,
		log:               log,
		dummyPasswordHash: dummyHash,
		allowedAAGUIDs:    cfg.AdminWebAuthnAllowedAAGUIDs,
	}, nil
}

// newDummyPasswordHash hashes a random throwaway password so VerifyPassword has a
// real Argon2id hash to burn cycles against when an admin username is unknown.
func newDummyPasswordHash() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate dummy password: %w", err)
	}
	hash, err := auth.HashPassword(hex.EncodeToString(raw))
	if err != nil {
		return "", fmt.Errorf("hash dummy password: %w", err)
	}
	return hash, nil
}

// passwordRequest is the POST /admin/api/v1/auth/password body.
type passwordRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// passwordResponse is returned on a successful password step: an opaque challenge
// handle plus the WebAuthn assertion options the browser feeds navigator
// .credentials.get(). The session cookie is NOT set here — only the WebAuthn step
// mints a session.
type passwordResponse struct {
	Handle    string                        `json:"handle"`
	PublicKey *protocol.CredentialAssertion `json:"publicKey"`
}

// loginChallenge is the value stored under the opaque login handle between the
// password and WebAuthn steps.
type loginChallenge struct {
	AdminID  string               `json:"admin_id"`
	Username string               `json:"username"`
	Session  webauthn.SessionData `json:"session"`
}

// genericAuthError is the single response shape for every password-step failure
// (unknown user, wrong password, locked out). Keeping it uniform prevents an
// attacker from distinguishing "no such admin" from "wrong password".
var genericAuthError = gin.H{"error": "authentication failed"}

// PasswordLogin handles step 1 of admin login: verify the password (constant
// time, enumeration-safe), and on success begin a WebAuthn assertion ceremony
// whose SessionData is stashed under an opaque handle. Lockout is checked FIRST.
func (h *Handler) PasswordLogin(c *gin.Context) {
	ctx := c.Request.Context()
	ip := c.ClientIP()

	var req passwordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}

	// Lockout gate FIRST — a locked account/IP never reaches password verify.
	locked, _, err := h.lockout.IsLocked(ctx, req.Username, ip)
	if err != nil {
		h.log.Error("admin lockout check failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}
	if locked {
		h.auditDenied(ctx, req.Username, EventLoginFailure, "locked_out")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts, try again later"})
		return
	}

	admin, lookupErr := h.repo.GetByUsername(ctx, req.Username)

	// Enumeration defense: when the username is unknown, still run a constant-time
	// verify against the dummy hash so timing/response are uniform.
	verifyHash := h.dummyPasswordHash
	if lookupErr == nil && admin != nil {
		verifyHash = admin.PasswordHash
	}
	ok, verifyErr := auth.VerifyPassword(req.Password, verifyHash)

	// Treat unknown user, disabled admin, verify error, or mismatch identically.
	if lookupErr != nil || admin == nil || verifyErr != nil || !ok || admin.Status != StatusActive {
		h.failLogin(ctx, req.Username, ip)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	// Password verified — begin the WebAuthn assertion ceremony for this admin's
	// registered credentials.
	user, err := h.webAuthnUserForAdmin(ctx, admin.ID)
	if err != nil {
		h.log.Error("admin webauthn user build failed", "error", err)
		// A configured admin with no/unreadable credentials cannot 2FA; fail closed
		// without leaking the cause.
		h.failLogin(ctx, req.Username, ip)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	assertion, session, err := h.webauthn.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		h.log.Error("admin begin login failed", "error", err)
		h.failLogin(ctx, req.Username, ip)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	handle, err := h.storeLoginChallenge(ctx, loginChallenge{
		AdminID:  admin.ID,
		Username: admin.Username,
		Session:  *session,
	})
	if err != nil {
		h.log.Error("admin store login challenge failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	c.JSON(http.StatusOK, passwordResponse{Handle: handle, PublicKey: assertion})
}

// webauthnRequest is the POST /admin/api/v1/auth/webauthn body: the handle from
// step 1 plus the raw assertion JSON the authenticator produced.
type webauthnRequest struct {
	Handle    string          `json:"handle"`
	Assertion json.RawMessage `json:"assertion"`
}

// WebAuthnLogin handles step 2 of admin login: redeem the opaque handle, finish
// the WebAuthn assertion (UV required), and — only on success — mint a session +
// set the __Host- cookie + reset lockout + audit EventLoginSuccess.
func (h *Handler) WebAuthnLogin(c *gin.Context) {
	ctx := c.Request.Context()
	ip := c.ClientIP()

	var req webauthnRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}

	challenge, err := h.consumeLoginChallenge(ctx, req.Handle)
	if err != nil {
		// Unknown/expired/replayed handle.
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	// Defense-in-depth: re-check lockout in the WebAuthn step (#1688 / Gitar). The
	// handle is single-use, but an account locked AFTER its handle was issued could
	// otherwise still redeem it once. Mirrors PasswordLogin's gate.
	locked, _, lerr := h.lockout.IsLocked(ctx, challenge.Username, ip)
	if lerr != nil {
		h.log.Error("admin lockout check failed", "error", lerr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}
	if locked {
		h.auditDenied(ctx, challenge.Username, EventLoginFailure, "locked_out")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts, try again later"})
		return
	}

	user, err := h.webAuthnUserForAdmin(ctx, challenge.AdminID)
	if err != nil {
		h.log.Error("admin webauthn user build failed", "error", err)
		h.failLogin(ctx, challenge.Username, ip)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	cred, err := h.webauthn.FinishLoginWithBytes(user, challenge.Session, []byte(req.Assertion))
	if err != nil {
		h.log.Warn("admin webauthn assertion rejected", "error", err)
		h.failLogin(ctx, challenge.Username, ip)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	// Persist the authenticator's advanced sign counter so clone-detection keeps
	// working (#1688 / Gitar). Non-fatal: the assertion already validated; a failed
	// write leaves the counter stale, not the login compromised.
	if scErr := h.repo.UpdateCredentialSignCount(ctx, cred.ID, int64(cred.Authenticator.SignCount)); scErr != nil {
		h.log.Warn("admin sign-count persist failed", "error", scErr)
	}

	sid, err := h.sessions.Mint(ctx, challenge.AdminID)
	if err != nil {
		h.log.Error("admin session mint failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}
	SetAdminSessionCookie(c, sid)

	if err := h.lockout.Reset(ctx, challenge.Username, ip); err != nil {
		// Non-fatal: the login succeeded; a stale lockout counter self-expires.
		h.log.Warn("admin lockout reset failed", "error", err)
	}

	adminID := challenge.AdminID
	h.auditOK(ctx, &adminID, challenge.Username, EventLoginSuccess)
	c.JSON(http.StatusOK, gin.H{"status": "authenticated"})
}

// Logout revokes the current admin session and clears the cookie. It is mounted
// pre-auth (a stale/invalid cookie should still be clearable) and is idempotent.
func (h *Handler) Logout(c *gin.Context) {
	ctx := c.Request.Context()
	sid, err := c.Cookie(adminCookieName)
	if err == nil && sid != "" {
		var adminID *string
		if sess, gerr := h.sessions.Get(ctx, sid); gerr == nil {
			id := sess.AdminID
			adminID = &id
		}
		if rerr := h.sessions.Revoke(ctx, sid); rerr != nil {
			h.log.Warn("admin session revoke failed", "error", rerr)
		}
		h.auditOK(ctx, adminID, "", EventLogout)
	}
	ClearAdminSessionCookie(c)
	c.JSON(http.StatusOK, gin.H{"status": "logged out"})
}

// failLogin records a lockout failure and writes the EventLoginFailure audit row.
// Errors from the side effects are logged, never surfaced to the client (which
// always gets the uniform generic error).
func (h *Handler) failLogin(ctx context.Context, username, ip string) {
	if err := h.lockout.RecordFailure(ctx, username, ip); err != nil {
		h.log.Warn("admin lockout record failed", "error", err)
	}
	h.auditFailure(ctx, username, EventLoginFailure)
}

// webAuthnUserForAdmin reconstructs the WebAuthnUser (with all registered
// credentials) for an admin so BeginLogin/FinishLogin can operate on the stored
// keys. It returns an error when the admin has no credentials (cannot 2FA).
func (h *Handler) webAuthnUserForAdmin(ctx context.Context, adminID string) (*mfa.WebAuthnUser, error) {
	stored, err := h.repo.ListCredentials(ctx, adminID)
	if err != nil {
		return nil, fmt.Errorf("list admin credentials: %w", err)
	}
	if len(stored) == 0 {
		return nil, errors.New("admin has no registered credentials")
	}
	creds := make([]webauthn.Credential, 0, len(stored))
	for _, s := range stored {
		cred := webauthn.Credential{
			ID:        s.CredentialID,
			PublicKey: s.PublicKey,
			Authenticator: webauthn.Authenticator{
				AAGUID:    s.AAGUID,
				SignCount: uint32(s.SignCount), //nolint:gosec // sign count is a small monotonic counter, no overflow
			},
		}
		for _, t := range s.Transports {
			cred.Transport = append(cred.Transport, protocol.AuthenticatorTransport(t))
		}
		creds = append(creds, cred)
	}
	return &mfa.WebAuthnUser{
		ID:          adminWebAuthnUserHandle,
		Name:        "operator",
		DisplayName: "Operator",
		Credentials: creds,
	}, nil
}

// storeLoginChallenge stashes the inter-step SessionData under a fresh opaque
// handle and returns the handle.
func (h *Handler) storeLoginChallenge(ctx context.Context, ch loginChallenge) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate login handle: %w", err)
	}
	handle := hex.EncodeToString(raw)
	payload, err := json.Marshal(ch)
	if err != nil {
		return "", fmt.Errorf("marshal login challenge: %w", err)
	}
	if err := h.redis.Set(ctx, loginChallengePrefix+handle, payload, loginChallengeTTL).Err(); err != nil {
		return "", fmt.Errorf("store login challenge: %w", err)
	}
	return handle, nil
}

// consumeLoginChallenge atomically reads-and-deletes the challenge bound to a
// handle (single-use via GETDEL). An unknown/expired/replayed handle errors.
func (h *Handler) consumeLoginChallenge(ctx context.Context, handle string) (loginChallenge, error) {
	if handle == "" {
		return loginChallenge{}, errors.New("empty login handle")
	}
	raw, err := h.redis.GetDel(ctx, loginChallengePrefix+handle).Result()
	if errors.Is(err, redis.Nil) {
		return loginChallenge{}, errors.New("unknown or expired login handle")
	}
	if err != nil {
		return loginChallenge{}, fmt.Errorf("load login challenge: %w", err)
	}
	var ch loginChallenge
	if err := json.Unmarshal([]byte(raw), &ch); err != nil {
		return loginChallenge{}, fmt.Errorf("decode login challenge: %w", err)
	}
	return ch, nil
}

// --- audit convenience wrappers (best-effort: a failed audit is logged, not surfaced) ---

func (h *Handler) auditOK(ctx context.Context, adminID *string, actor, event string) {
	h.writeAudit(ctx, AuditEvent{AdminID: adminID, Actor: actor, EventType: event, Result: AuditSuccess})
}

func (h *Handler) auditFailure(ctx context.Context, actor, event string) {
	h.writeAudit(ctx, AuditEvent{Actor: actor, EventType: event, Result: AuditFailure})
}

func (h *Handler) auditDenied(ctx context.Context, actor, event, reason string) {
	h.writeAudit(ctx, AuditEvent{
		Actor: actor, EventType: event, Result: AuditDenied,
		Detail: map[string]any{"reason": reason},
	})
}

func (h *Handler) writeAudit(ctx context.Context, ev AuditEvent) {
	if err := h.audit.Write(ctx, ev); err != nil {
		h.log.Warn("admin audit write failed", "error", err)
	}
}

// -----------------------------------------------------------------------------
// Enrollment (#1688 §5): a pending admin authenticates username+password+token,
// registers a hardware key (AAGUID-checked), and is flipped to active. WebAuthn
// registration is structurally a two-step ceremony — begin (server issues the
// creation challenge) then finish (client posts the attestation) — so it is
// surfaced as /enroll/begin + /enroll/finish, both pre-auth (the token + password
// ARE the authentication). The single-use enrollment token is consumed at begin.
// -----------------------------------------------------------------------------

// enrollChallengePrefix namespaces the inter-step registration SessionData.
const enrollChallengePrefix = "admin_enroll_challenge:"

// enrollChallengeTTL bounds the begin→finish window for the registration ceremony.
const enrollChallengeTTL = 5 * time.Minute

// enrollBeginRequest is the POST /admin/api/v1/enroll/begin body.
type enrollBeginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

// enrollBeginResponse returns the registration options + an opaque handle.
type enrollBeginResponse struct {
	Handle    string                       `json:"handle"`
	PublicKey *protocol.CredentialCreation `json:"publicKey"`
}

// enrollChallenge is the value stored under the begin handle.
type enrollChallenge struct {
	AdminID  string               `json:"admin_id"`
	Username string               `json:"username"`
	Session  webauthn.SessionData `json:"session"`
}

// EnrollBegin verifies password + single-use token, then begins a hardware-key
// registration ceremony bound to the pending admin. The token is consumed here
// (single-use); a failed finish requires a fresh token via reset-enrollment.
func (h *Handler) EnrollBegin(c *gin.Context) {
	ctx := c.Request.Context()

	var req enrollBeginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}

	adminUser, lookupErr := h.repo.GetByUsername(ctx, req.Username)
	verifyHash := h.dummyPasswordHash
	if lookupErr == nil && adminUser != nil {
		verifyHash = adminUser.PasswordHash
	}
	ok, verifyErr := auth.VerifyPassword(req.Password, verifyHash)
	if lookupErr != nil || adminUser == nil || verifyErr != nil || !ok {
		h.auditFailure(ctx, req.Username, EventEnrollComplete)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	// Consume the single-use enrollment token and verify it binds to THIS admin.
	boundAdminID, err := h.enroll.ConsumeEnrollmentToken(ctx, req.Token)
	if err != nil || boundAdminID != adminUser.ID {
		h.auditFailure(ctx, req.Username, EventEnrollComplete)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	creation, session, err := h.webauthn.BeginRegistration(adminRegistrationUser(), adminRegistrationOptions()...)
	if err != nil {
		h.log.Error("admin begin registration failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	handle, err := h.storeEnrollChallenge(ctx, enrollChallenge{
		AdminID:  adminUser.ID,
		Username: adminUser.Username,
		Session:  *session,
	})
	if err != nil {
		h.log.Error("admin store enroll challenge failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	c.JSON(http.StatusOK, enrollBeginResponse{Handle: handle, PublicKey: creation})
}

// enrollFinishRequest is the POST /admin/api/v1/enroll/finish body.
type enrollFinishRequest struct {
	Handle         string          `json:"handle"`
	Attestation    json.RawMessage `json:"attestation"`
	CredentialName string          `json:"credential_name"`
}

// EnrollFinish redeems the begin handle, completes the registration (AAGUID
// allow-list enforced inside FinishAdminRegistration), persists the credential,
// and flips the admin to active. Audits enroll_complete.
func (h *Handler) EnrollFinish(c *gin.Context) {
	ctx := c.Request.Context()

	var req enrollFinishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}

	challenge, err := h.consumeEnrollChallenge(ctx, req.Handle)
	if err != nil {
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	user := adminRegistrationUser()
	if _, err := FinishAdminRegistration(ctx, h.webauthn, h.repo, AdminRegistrationInput{
		User:           user,
		Session:        challenge.Session,
		Request:        httpRequestWithBody(c, req.Attestation),
		AdminID:        challenge.AdminID,
		AllowedAAGUIDs: h.allowedAAGUIDs,
		CredentialName: req.CredentialName,
	}); err != nil {
		h.log.Warn("admin finish registration rejected", "error", err)
		h.auditFailure(ctx, challenge.Username, EventEnrollComplete)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	if err := h.repo.SetStatus(ctx, challenge.AdminID, StatusActive); err != nil {
		h.log.Error("admin activate failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	adminID := challenge.AdminID
	h.auditOK(ctx, &adminID, challenge.Username, EventEnrollComplete)
	c.JSON(http.StatusOK, gin.H{"status": "enrolled"})
}

// adminRegistrationUser is the WebAuthnUser used for the admin registration
// ceremony (same stable handle as login).
func adminRegistrationUser() *mfa.WebAuthnUser {
	return &mfa.WebAuthnUser{
		ID:          adminWebAuthnUserHandle,
		Name:        "operator",
		DisplayName: "Operator",
	}
}

// adminRegistrationOptions is the admin registration policy: direct attestation
// (so the AAGUID is conveyed for the allow-list check), resident key required,
// cross-platform authenticator, user verification required.
func adminRegistrationOptions() []webauthn.RegistrationOption {
	rk := true
	return []webauthn.RegistrationOption{
		webauthn.WithConveyancePreference(protocol.PreferDirectAttestation),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.CrossPlatform,
			RequireResidentKey:      &rk,
			ResidentKey:             protocol.ResidentKeyRequirementRequired,
			UserVerification:        protocol.VerificationRequired,
		}),
	}
}

func (h *Handler) storeEnrollChallenge(ctx context.Context, ch enrollChallenge) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate enroll handle: %w", err)
	}
	handle := hex.EncodeToString(raw)
	payload, err := json.Marshal(ch)
	if err != nil {
		return "", fmt.Errorf("marshal enroll challenge: %w", err)
	}
	if err := h.redis.Set(ctx, enrollChallengePrefix+handle, payload, enrollChallengeTTL).Err(); err != nil {
		return "", fmt.Errorf("store enroll challenge: %w", err)
	}
	return handle, nil
}

func (h *Handler) consumeEnrollChallenge(ctx context.Context, handle string) (enrollChallenge, error) {
	if handle == "" {
		return enrollChallenge{}, errors.New("empty enroll handle")
	}
	raw, err := h.redis.GetDel(ctx, enrollChallengePrefix+handle).Result()
	if errors.Is(err, redis.Nil) {
		return enrollChallenge{}, errors.New("unknown or expired enroll handle")
	}
	if err != nil {
		return enrollChallenge{}, fmt.Errorf("load enroll challenge: %w", err)
	}
	var ch enrollChallenge
	if err := json.Unmarshal([]byte(raw), &ch); err != nil {
		return enrollChallenge{}, fmt.Errorf("decode enroll challenge: %w", err)
	}
	return ch, nil
}

// httpRequestWithBody wraps a JSON body in the *http.Request shape FinishRegistration
// parses (it reads request.Body). The gin context's request is reused for its
// context; only the Body is swapped to the supplied attestation JSON.
func httpRequestWithBody(c *gin.Context, body []byte) *http.Request {
	r := c.Request.Clone(c.Request.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	return r
}

// -----------------------------------------------------------------------------
// Admin-create (#1688 §5): an authenticated active admin provisions a new pending
// admin and receives a one-time enrollment token in the response body (shown
// once, handed over out-of-band). Behind AdminAuthRequired.
// -----------------------------------------------------------------------------

// createAdminRequest is the POST /admin/api/v1/admins body.
type createAdminRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// CreateAdmin provisions a pending admin and mints a one-time enrollment token,
// returned ONCE in the response body. The acting admin id comes from the session
// (set by AdminAuthRequired). The password set here is a temporary credential the
// new operator changes is out-of-scope; it is hashed with Argon2id and never
// echoed back.
func (h *Handler) CreateAdmin(c *gin.Context) {
	ctx := c.Request.Context()
	actingAdminID := c.GetString(adminIDContextKey)

	var req createAdminRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}
	if req.Username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username is required"})
		return
	}
	if err := auth.ValidatePasswordStrength(req.Password); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password too weak"})
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		h.log.Error("admin hash password failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	created, err := h.repo.CreatePending(ctx, req.Username, hash)
	if err != nil {
		if errors.Is(err, ErrDuplicateUsername) {
			c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
			return
		}
		h.log.Error("admin create pending failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	token, err := h.enroll.MintEnrollmentToken(ctx, created.ID)
	if err != nil {
		h.log.Error("admin mint enrollment token failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	acting := actingAdminID
	h.auditOK(ctx, &acting, req.Username, EventBootstrap)

	c.JSON(http.StatusCreated, gin.H{
		"username": created.Username,
		"status":   string(created.Status),
		// The token is surfaced ONCE here; it is never stored in plaintext and
		// never logged.
		"enrollment_token": token,
	})
}
