package mfa

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// RecoveryCompleter issues final recovery authority after the challenge is claimed.
type RecoveryCompleter interface {
	CompleteRecovery(c *gin.Context, userID, verifiedFactor string) bool
}

// SetRecoveryCompleter wires final recovery issuance after factor verification.
func (h *Handler) SetRecoveryCompleter(completer RecoveryCompleter) {
	h.recoveryCompleter = completer
}

// GetRecoveryMethods reads the actual factors rather than the denormalized MFA
// flags. Sign-in's effectiveRecoveryOnly lapse must not weaken recovery policy.
// The caller has already proved email ownership; one reserved inline factor is
// additionally required when configured. Backup codes belong to TOTP.
func (h *Handler) GetRecoveryMethods(ctx context.Context, userID string) ([]string, error) {
	var reserved, flags []string
	var hardened, totpEnabled, webauthnEnabled bool
	if err := h.db.QueryRowContext(ctx, `
		SELECT recovery_only_methods, recovery_hardened, mfa_methods,
		 EXISTS (SELECT 1 FROM user_mfa_totp WHERE user_id = users.id AND enabled AND confirmed),
		 EXISTS (SELECT 1 FROM user_mfa_webauthn WHERE user_id = users.id)
		FROM users WHERE id = $1`, userID,
	).Scan(pq.Array(&reserved), &hardened, pq.Array(&flags), &totpEnabled, &webauthnEnabled); err != nil {
		return nil, fmt.Errorf("read recovery policy: %w", err)
	}
	_, smsEnabled, err := h.readEmailSmsEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	return recoveryMethods(reserved, flags, hardened, totpEnabled, webauthnEnabled, smsEnabled)
}

func recoveryMethods(reserved, flags []string, hardened, totpEnabled, webauthnEnabled, smsEnabled bool) ([]string, error) {
	// SMS has no recovery verifier yet. Never downgrade an enrolled dual-channel
	// policy; the default hardened flag remains dormant without enrolled SMS.
	if hardened && (smsEnabled || containsStr(flags, "sms")) {
		return nil, fmt.Errorf("SMS recovery verification is unavailable")
	}
	methods := make([]string, 0, 2)
	for _, method := range reserved {
		switch method {
		case "totp":
			if totpEnabled && !containsStr(methods, method) {
				methods = append(methods, method)
			}
		case "webauthn":
			if webauthnEnabled && !containsStr(methods, method) {
				methods = append(methods, method)
			}
		case "email": // Proved by the recovery email code.
		case "sms":
			return nil, fmt.Errorf("SMS recovery verification is unavailable")
		default:
			return nil, fmt.Errorf("unknown recovery factor")
		}
	}
	return methods, nil
}

// ValidateRecoveryClaimsTx linearizes authority with policy and factor writers.
func (h *Handler) ValidateRecoveryClaimsTx(ctx context.Context, tx *sql.Tx, claims *auth.RecoveryClaims) error {
	if tx == nil || claims == nil || (claims.RecoveryFactor != "email" && claims.RecoveryFactor != "totp" && claims.RecoveryFactor != "webauthn") {
		return auth.ErrRecoveryProofInvalid
	}
	if err := lockUserForMFAWriteTx(ctx, tx, claims.UserID); err != nil {
		return fmt.Errorf("%w: %w", auth.ErrRecoveryPolicyUnavailable, err)
	}
	// Use a fresh statement snapshot after waiting for any factor writer.
	var reserved, flags []string
	var hardened, totpEnabled, webauthnEnabled bool
	if err := tx.QueryRowContext(ctx, `
  SELECT recovery_only_methods, recovery_hardened, mfa_methods,
   EXISTS (SELECT 1 FROM user_mfa_totp WHERE user_id = users.id AND enabled AND confirmed),
   EXISTS (SELECT 1 FROM user_mfa_webauthn WHERE user_id = users.id)
  FROM users WHERE id=$1`, claims.UserID).Scan(pq.Array(&reserved), &hardened, pq.Array(&flags), &totpEnabled, &webauthnEnabled); err != nil {
		return fmt.Errorf("%w: %w", auth.ErrRecoveryPolicyUnavailable, err)
	}
	methods, err := recoveryMethods(reserved, flags, hardened, totpEnabled, webauthnEnabled, false)
	if err != nil {
		return fmt.Errorf("%w: %w", auth.ErrRecoveryPolicyUnavailable, err)
	}
	if len(methods) > 0 && !containsStr(methods, claims.RecoveryFactor) {
		return auth.ErrRecoveryProofInvalid
	}
	return nil
}

// GenerateRecoveryChallenge is called only after the single-use email proof.
// Its distinct purpose cannot authorize any reset, device or social operation.
func (h *Handler) GenerateRecoveryChallenge(_ context.Context, userID string) (string, string, error) {
	return GenerateChallengeToken(userID, PurposeRecoveryChallenge, JWTSecret(h.jwtSecret), "")
}

func recoveryFactor(method string) string {
	if method == "backup_code" {
		return "totp"
	}
	return method
}
