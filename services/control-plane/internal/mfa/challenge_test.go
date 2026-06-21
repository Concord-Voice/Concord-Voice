package mfa

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testJWTSecret = "test-secret-for-mfa-challenges" // #nosec G101

func TestGenerateChallengeToken(t *testing.T) {
	token, jti, err := GenerateChallengeToken("user-123", PurposeLogin, testJWTSecret)
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	if token == "" {
		t.Error("token is empty")
	}
	if jti == "" {
		t.Error("jti is empty")
	}
}

func TestValidateChallengeToken(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeLogin, testJWTSecret)
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	claims, err := ValidateChallengeToken(token, testJWTSecret, PurposeLogin)
	if err != nil {
		t.Fatalf("ValidateChallengeToken failed: %v", err)
	}

	if claims.UserID != "user-123" {
		t.Errorf("UserID = %q, want %q", claims.UserID, "user-123")
	}
	if claims.Purpose != PurposeLogin {
		t.Errorf("Purpose = %q, want %q", claims.Purpose, PurposeLogin)
	}
	if claims.Issuer != "concordvoice-mfa" {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, "concordvoice-mfa")
	}
}

func TestValidateChallengeTokenWrongPurpose(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeLogin, testJWTSecret)
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	_, err = ValidateChallengeToken(token, testJWTSecret, PurposeSuspiciousRefresh)
	if err == nil {
		t.Error("expected error for wrong purpose")
	}
}

func TestValidateChallengeTokenWrongSecret(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeLogin, testJWTSecret)
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	_, err = ValidateChallengeToken(token, "wrong-secret", PurposeLogin)
	if err == nil {
		t.Error("expected error for wrong secret")
	}
}

func TestGenerateRecoveryToken(t *testing.T) {
	token, jti, err := GenerateRecoveryToken("user-456", testJWTSecret)
	if err != nil {
		t.Fatalf("GenerateRecoveryToken failed: %v", err)
	}
	if token == "" {
		t.Error("token is empty")
	}
	if jti == "" {
		t.Error("jti is empty")
	}

	// Validate with correct purpose
	claims, err := ValidateChallengeToken(token, testJWTSecret, PurposeRecovery)
	if err != nil {
		t.Fatalf("ValidateChallengeToken failed for recovery token: %v", err)
	}
	if claims.UserID != "user-456" {
		t.Errorf("UserID = %q, want %q", claims.UserID, "user-456")
	}
	if claims.Purpose != PurposeRecovery {
		t.Errorf("Purpose = %q, want %q", claims.Purpose, PurposeRecovery)
	}

	// Verify ~25 hour TTL (should expire between 24h59m and 25h10s from now)
	expiresIn := time.Until(claims.ExpiresAt.Time)
	if expiresIn < 24*time.Hour+59*time.Minute || expiresIn > 25*time.Hour+10*time.Second {
		t.Errorf("recovery token TTL = %v, want ~25 hours", expiresIn)
	}
}

func TestRecoveryTokenRejectsWrongPurpose(t *testing.T) {
	token, _, err := GenerateRecoveryToken("user-456", testJWTSecret)
	if err != nil {
		t.Fatalf("GenerateRecoveryToken failed: %v", err)
	}

	// Login purpose should reject a recovery token
	_, err = ValidateChallengeToken(token, testJWTSecret, PurposeLogin)
	if err == nil {
		t.Error("expected error when validating recovery token with login purpose")
	}

	// MFA upgrade purpose should also reject
	_, err = ValidateChallengeToken(token, testJWTSecret, PurposeMFAUpgrade)
	if err == nil {
		t.Error("expected error when validating recovery token with mfa_upgrade purpose")
	}
}

func TestValidateChallengeTokenExpired(t *testing.T) {
	// Create a token that's already expired
	claims := ChallengeClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "test-jti",
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-10 * time.Minute)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-5 * time.Minute)),
			NotBefore: jwt.NewNumericDate(time.Now().Add(-10 * time.Minute)),
			Issuer:    "concordvoice-mfa",
		},
		UserID:  "user-123",
		Purpose: PurposeLogin,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key — test fixture, not a real credential
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	_, err = ValidateChallengeToken(signed, testJWTSecret, PurposeLogin)
	if err == nil {
		t.Error("expected error for expired token")
	}
}
