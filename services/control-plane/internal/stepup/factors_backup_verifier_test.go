package stepup_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Spending a backup code writes user_mfa_totp (backup_codes_used and
// updated_at) but must not move the default (picker spec, PR 1 server tests):
// TOTP recency is last_used_step, which only a TOTP acceptance writes. The
// code is spent through the real mfa.Handler.VerifyCode, so a reader that
// switched to updated_at would turn the WebAuthn default into TOTP here.
func TestInlineMFAFactors_BackupCodeAcceptanceKeepsTheDefault(t *testing.T) {
	db := stepup.SubjectTestDB(t)
	ctx := context.Background()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	keyring, err := mfa.ParseKeyring(hex.EncodeToString(key), 1, "")
	require.NoError(t, err)
	sealed, nonce, version, err := keyring.Seal([]byte("JBSWY3DPEHPK3PXP"))
	require.NoError(t, err)
	codes, hashes, err := mfa.GenerateBackupCodes()
	require.NoError(t, err)

	// TOTP last matched, and its row last changed, an hour ago; the key was
	// used a minute ago, so WebAuthn is the default.
	userID := stepup.SubjectTestUser(t, db)
	now := time.Now()
	_, err = db.Exec(`INSERT INTO user_mfa_totp
		(user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed,
		 last_used_step, backup_codes_hash, backup_codes_used, updated_at)
		VALUES ($1, $2, $3, $4, true, true, $5, $6, $7, $8)`,
		userID, sealed, nonce, version, now.Add(-time.Hour).Unix()/30,
		pq.Array(hashes), pq.Array(make([]bool, len(hashes))), now.Add(-time.Hour))
	require.NoError(t, err)
	stepup.FactorsAddKey(t, db, userID, now.Add(-2*time.Hour), now.Add(-time.Minute))

	before, e := stepup.InlineMFAFactors(ctx, db, userID)
	require.Nil(t, e)
	require.Equal(t, stepup.MethodWebAuthn, before.Default())

	h := mfa.NewHandler(db, nil, logger.NewWithWriter(io.Discard), keyring, "", nil, "test")
	ok, err := h.VerifyCode(ctx, userID, stepup.PurposeRecoveryOnlySet, codes[0])
	require.NoError(t, err)
	require.True(t, ok, "control: the backup code is accepted")
	var used []bool
	require.NoError(t, db.QueryRow(`SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, userID).
		Scan(pq.Array(&used)))
	require.True(t, used[0], "control: and spent")

	after, e := stepup.InlineMFAFactors(ctx, db, userID)
	require.Nil(t, e)
	require.Equal(t, before.Default(), after.Default(), "a backup code must not move the default")
}

// backup_code_available must agree with what mfa.VerifyBackupCode will
// actually spend, including on both damaged shapes where the two arrays
// disagree in length (picker spec, PR 1 server tests, rev 3.2 S5). Each case
// asks the real verifier with real codes, so drift between the SQL and the
// verifier fails here rather than advertising a code that cannot be spent.
func TestInlineMFAFactors_BackupAvailabilityAgreesWithVerifier(t *testing.T) {
	db := stepup.SubjectTestDB(t)
	ctx := context.Background()
	codes, hashes, err := mfa.GenerateBackupCodes()
	require.NoError(t, err)

	cases := []struct {
		name      string
		hashes    []string
		used      []bool
		spendable bool // what the verifier is expected to say; checked below
	}{
		{"one unspent", hashes[:2], []bool{true, false}, true},
		{"all spent", hashes[:2], []bool{true, true}, false},
		{"no codes", []string{}, []bool{}, false},
		{"damaged: a hash with no used flag", hashes[:2], []bool{true}, false},
		{"damaged: a used flag with no hash", hashes[:1], []bool{true, false}, false},
		{"damaged, still spendable: an unspent code beside a hash with no flag", hashes[:3], []bool{true, false}, true},
		{"damaged, still spendable: an unspent code beside a flag with no hash", hashes[:2], []bool{false, true, false}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spendable := false
			for _, code := range codes {
				if _, ok := mfa.VerifyBackupCode(code, tc.hashes, tc.used); ok {
					spendable = true
				}
			}
			require.Equal(t, tc.spendable, spendable, "the fixture's premise about the verifier")

			userID := stepup.SubjectTestUser(t, db)
			stepup.SubjectTestTOTP(t, db, userID, true, true)
			stepup.FactorsSetTOTP(t, db, userID, nil, tc.hashes, tc.used)
			f, e := stepup.InlineMFAFactors(ctx, db, userID)
			require.Nil(t, e)
			require.Equal(t, spendable, f.BackupCodeAvailable)
		})
	}
}
