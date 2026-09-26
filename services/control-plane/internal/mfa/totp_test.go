package mfa

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// stepFixtureSecret is a fixed seed, so the matcher tests never depend on a
// random secret or on when they run.
const stepFixtureSecret = "JBSWY3DPEHPK3PXP" //nolint:gosec // test-only TOTP seed // pragma: allowlist secret

// stepCode generates the TOTP code for one time step with the parameters
// MatchCodeStep checks against.
func stepCode(t *testing.T, secret string, step int64) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriod, 0), totp.ValidateOpts{
		Period:    totpPeriod,
		Digits:    totpDigits,
		Algorithm: totpAlgo,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom failed: %v", err)
	}
	return code
}

// TestMatchCodeStep pins the step matcher: each of the three candidate steps in
// the skew window reports its own step, and a code outside the window, a wrong
// code, or a bad secret reports no match.
func TestMatchCodeStep(t *testing.T) {
	// A fixed secret and a fixed instant 7 s into its step, so no candidate
	// sits on a boundary and the result never depends on when the test runs.
	const secret = stepFixtureSecret // pragma: allowlist secret -- test-only TOTP seed
	const current = int64(59_000_000)
	now := time.Unix(current*totpPeriod+7, 0)

	// Codes for steps current-2 .. current+2. They must be distinct, or a
	// single-match case below would really be a double match.
	codes := map[int64]string{}
	seen := map[string]bool{}
	for offset := int64(-2); offset <= 2; offset++ {
		code := stepCode(t, secret, current+offset)
		if seen[code] {
			t.Fatalf("fixture broken: step %d repeats an earlier code", current+offset)
		}
		seen[code] = true
		codes[offset] = code
	}

	for _, offset := range []int64{-1, 0, 1} {
		want := current + offset
		if got, ok := matchCodeStepAt(secret, codes[offset], now); !ok || got != want {
			t.Errorf("offset %d: matchCodeStepAt = (%d, %v), want (%d, true)", offset, got, ok, want)
		}
		// Surrounding whitespace is trimmed, as the previous matcher did.
		if got, ok := matchCodeStepAt(secret, " "+codes[offset]+"\n", now); !ok || got != want {
			t.Errorf("offset %d: padded code = (%d, %v), want (%d, true)", offset, got, ok, want)
		}
	}

	for _, offset := range []int64{-2, 2} {
		if got, ok := matchCodeStepAt(secret, codes[offset], now); ok {
			t.Errorf("offset %d is outside the skew window but matched step %d", offset, got)
		}
	}

	// A wrong code, chosen deterministically so it is none of the window's.
	wrong := 0
	for seen[fmt.Sprintf("%06d", wrong)] {
		wrong++
	}
	for _, code := range []string{fmt.Sprintf("%06d", wrong), "", "12345", "1234567", "abcdef"} {
		if got, ok := matchCodeStepAt(secret, code, now); ok {
			t.Errorf("code %q matched step %d, want no match", code, got)
		}
	}

	if _, ok := matchCodeStepAt("not base32!", codes[0], now); ok {
		t.Error("an undecodable secret must never match")
	}
}

// TestMatchCodeStepDoubleMatchReportsHighest pins that a code matching two
// candidates reports the later step, so recording it burns the whole
// collision. The fixture was found by search: for this secret, steps
// 59607044 and 59607045 share one code.
func TestMatchCodeStepDoubleMatchReportsHighest(t *testing.T) {
	const secret = stepFixtureSecret // pragma: allowlist secret -- test-only TOTP seed
	const lo, hi = int64(59607044), int64(59607045)
	code := stepCode(t, secret, lo)
	if stepCode(t, secret, hi) != code {
		t.Fatalf("fixture broken: steps %d and %d no longer share a code", lo, hi)
	}

	cases := []struct {
		name    string
		current int64
		want    int64
	}{
		{"both in window, current is the lower", lo, hi},
		{"both in window, current is the higher", hi, hi},
		{"lower out of window", hi + 1, hi},
		{"higher out of window", lo - 1, lo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchCodeStepAt(secret, code, time.Unix(tc.current*totpPeriod+7, 0))
			if !ok || got != tc.want {
				t.Errorf("matchCodeStepAt = (%d, %v), want (%d, true)", got, ok, tc.want)
			}
		})
	}
}

// TestMatchCodeStepReadsTheClock pins that the exported matcher reads the real
// clock: a code generated for now matches the step now falls in.
func TestMatchCodeStepReadsTheClock(t *testing.T) {
	key, err := GenerateSecret("test@example.com")
	if err != nil {
		t.Fatalf("GenerateSecret failed: %v", err)
	}
	before := time.Now().Unix() / totpPeriod
	code := stepCode(t, key.Secret(), before)
	got, ok := MatchCodeStep(key.Secret(), code)
	// A step boundary may pass between the two reads; the code then matches as
	// the previous step, which still names before.
	if !ok || got < before {
		t.Errorf("MatchCodeStep = (%d, %v), want (>= %d, true)", got, ok, before)
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

// aes.NewCipher accepts 16- and 24-byte keys, so without their own length
// checks EncryptSecret would seal under AES-128/192 and DecryptSecret would
// open it. Other lengths already fail inside aes.NewCipher and pin nothing.
func TestEncryptDecryptSecretRejectShortAESKeys(t *testing.T) {
	for _, n := range []int{16, 24} {
		key := make([]byte, n)
		if _, _, err := EncryptSecret([]byte("secret"), key); err == nil {
			t.Errorf("EncryptSecret accepted a %d-byte key", n)
		}

		// Sealed under this same short key, so only the length check can refuse it.
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatalf("aes.NewCipher(%d bytes): %v", n, err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			t.Fatalf("cipher.NewGCM: %v", err)
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		ct := gcm.Seal(nil, nonce, []byte("secret"), nil)
		if got, err := DecryptSecret(ct, nonce, key); err == nil {
			t.Errorf("DecryptSecret opened a %d-byte-key ciphertext: %q", n, got)
		}
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

// The hash and used arrays are written together, so a used array shorter
// than the hashes is a damaged row. A code with no used flag must not match:
// the caller indexes used[i] to spend it.
func TestVerifyBackupCodeShortUsedArrayFailsClosed(t *testing.T) {
	codes, hashes, err := GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes failed: %v", err)
	}
	used := make([]bool, 2)

	if idx, ok := VerifyBackupCode(codes[0], hashes, used); !ok || idx != 0 {
		t.Fatalf("code with a used flag: idx=%d, ok=%v, want idx=0, ok=true", idx, ok)
	}
	if idx, ok := VerifyBackupCode(codes[5], hashes, used); ok {
		t.Errorf("code %d has no used flag but matched", idx)
	}
	if idx, ok := VerifyBackupCode(codes[0], hashes, nil); ok {
		t.Errorf("nil used array: code %d matched", idx)
	}
}
