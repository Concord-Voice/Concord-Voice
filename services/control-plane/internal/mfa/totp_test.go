package mfa

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestGenerateSecret(t *testing.T) {
	key, err := GenerateSecret("test@example.com")
	if err != nil {
		t.Fatalf("GenerateSecret failed: %v", err)
	}

	if key.Issuer() != totpIssuer {
		t.Errorf("issuer = %q, want %q", key.Issuer(), totpIssuer)
	}
	if key.AccountName() != "test@example.com" {
		t.Errorf("account = %q, want %q", key.AccountName(), "test@example.com")
	}
	if key.Secret() == "" {
		t.Error("secret is empty")
	}
}

func TestValidateCode(t *testing.T) {
	key, err := GenerateSecret("test@example.com")
	if err != nil {
		t.Fatalf("GenerateSecret failed: %v", err)
	}

	// Generate a valid code
	code, err := totp.GenerateCodeCustom(key.Secret(), time.Now(), totp.ValidateOpts{
		Period:    totpPeriod,
		Digits:    totpDigits,
		Algorithm: totpAlgo,
	})
	if err != nil {
		t.Fatalf("GenerateCode failed: %v", err)
	}

	if !ValidateCode(key.Secret(), code) {
		t.Error("valid code rejected")
	}

	if ValidateCode(key.Secret(), "000000") {
		t.Error("invalid code accepted")
	}
}

func TestEncryptDecryptSecret(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	plaintext := []byte("my-totp-secret-12345")

	ct, nonce, err := EncryptSecret(plaintext, key)
	if err != nil {
		t.Fatalf("EncryptSecret failed: %v", err)
	}

	if len(nonce) == 0 {
		t.Fatal("nonce is empty")
	}

	decrypted, err := DecryptSecret(ct, nonce, key)
	if err != nil {
		t.Fatalf("DecryptSecret failed: %v", err)
	}

	if string(decrypted) != string(plaintext) {
		t.Errorf("decrypted = %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptSecretWrongKey(t *testing.T) {
	key := make([]byte, 32)
	plaintext := []byte("secret")

	ct, nonce, err := EncryptSecret(plaintext, key)
	if err != nil {
		t.Fatalf("EncryptSecret failed: %v", err)
	}

	wrongKey := make([]byte, 32)
	wrongKey[0] = 0xFF

	_, err = DecryptSecret(ct, nonce, wrongKey)
	if err == nil {
		t.Error("expected error decrypting with wrong key")
	}
}

func TestParseEncryptionKey(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "0000000000000000000000000000000000000000000000000000000000000000", false},
		{"too short", "0000000000000000", true},
		{"invalid hex", "zzzz", true},
		{"empty", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := ParseEncryptionKey(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(key) != 32 {
				t.Errorf("key length = %d, want 32", len(key))
			}
		})
	}
}

func TestGenerateBackupCodes(t *testing.T) {
	codes, hashes, err := GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes failed: %v", err)
	}

	if len(codes) != backupCount {
		t.Errorf("got %d codes, want %d", len(codes), backupCount)
	}
	if len(hashes) != backupCount {
		t.Errorf("got %d hashes, want %d", len(hashes), backupCount)
	}

	for i, code := range codes {
		if len(code) != backupLen {
			t.Errorf("code[%d] length = %d, want %d", i, len(code), backupLen)
		}

		// Verify hash matches
		h := sha256.Sum256([]byte(code))
		expected := hex.EncodeToString(h[:])
		if hashes[i] != expected {
			t.Errorf("hash[%d] mismatch", i)
		}
	}

	// Codes should be unique
	seen := make(map[string]bool)
	for _, c := range codes {
		if seen[c] {
			t.Errorf("duplicate code: %s", c)
		}
		seen[c] = true
	}
}

func TestVerifyBackupCode(t *testing.T) {
	codes, hashes, err := GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes failed: %v", err)
	}

	used := make([]bool, len(codes))

	// Valid code matches
	idx, ok := VerifyBackupCode(codes[0], hashes, used)
	if !ok || idx != 0 {
		t.Errorf("valid code: idx=%d, ok=%v, want idx=0, ok=true", idx, ok)
	}

	// Case insensitive
	idx, ok = VerifyBackupCode("  "+codes[1]+"  ", hashes, used)
	if !ok || idx != 1 {
		t.Errorf("trimmed code: idx=%d, ok=%v, want idx=1, ok=true", idx, ok)
	}

	// Used code is skipped
	used[2] = true
	_, ok = VerifyBackupCode(codes[2], hashes, used)
	if ok {
		t.Error("used code should not match")
	}

	// Invalid code
	idx, ok = VerifyBackupCode("XXXXXXXX", hashes, used)
	if ok {
		t.Errorf("invalid code matched at index %d", idx)
	}
}
