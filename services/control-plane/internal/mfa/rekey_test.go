package mfa_test

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reuses rotationKeyV1/rotationKeyV2 from handlers_test.go (same package).

// seedSealedRow inserts an enabled+confirmed enrollment sealed under the given
// ring's active key and returns the user ID.
func seedSealedRow(t *testing.T, ts *testhelpers.TestServer, ring *mfa.Keyring, name, seed string) string {
	t.Helper()
	user := ts.CreateTestUser(t, name)
	ct, nonce, ver, err := ring.Seal([]byte(seed))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, $4, TRUE, TRUE)`,
		user.ID, ct, nonce, ver)
	require.NoError(t, err)
	return user.ID
}

func rotatedRing(t *testing.T) *mfa.Keyring {
	t.Helper()
	ring, err := mfa.ParseKeyring(rotationKeyV2, 2, "1:"+rotationKeyV1)
	require.NoError(t, err)
	return ring
}

func v1OnlyRing(t *testing.T) *mfa.Keyring {
	t.Helper()
	ring, err := mfa.ParseKeyring(rotationKeyV1, 1, "")
	require.NoError(t, err)
	return ring
}

func TestRekey_ConvergesAllRowsToActive(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	u1 := seedSealedRow(t, ts, v1OnlyRing(t), "rekeyuser1", "JBSWY3DPEHPK3PXP")
	u2 := seedSealedRow(t, ts, v1OnlyRing(t), "rekeyuser2", "KRSXG5CTMVRXEZLU")

	res, err := mfa.Rekey(context.Background(), ts.DB, ring, 1 /* batchSize=1 exercises pagination */, false)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Rekeyed)
	assert.Empty(t, res.Failed)

	for _, uid := range []string{u1, u2} {
		var enc, nonce []byte
		var ver int
		require.NoError(t, ts.DB.QueryRow(
			`SELECT totp_secret_enc, totp_secret_nonce, key_version FROM user_mfa_totp WHERE user_id = $1`, uid,
		).Scan(&enc, &nonce, &ver))
		assert.Equal(t, 2, ver)
		_, err := ring.Open(enc, nonce, ver)
		assert.NoError(t, err, "re-sealed row must decrypt under the active key")
	}
}

func TestRekey_IdempotentSecondRun(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	seedSealedRow(t, ts, v1OnlyRing(t), "rekeyidem", "JBSWY3DPEHPK3PXP")

	_, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, false)
	require.NoError(t, err)
	res2, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, false)
	require.NoError(t, err)
	assert.Equal(t, 0, res2.Rekeyed)
	assert.Empty(t, res2.Failed)
}

func TestRekey_DryRunCountsWithoutMutating(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	uid := seedSealedRow(t, ts, v1OnlyRing(t), "rekeydry", "JBSWY3DPEHPK3PXP")

	res, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, true)
	require.NoError(t, err)
	// The shared test DB may carry pending rows from sibling tests; this
	// user's row must be among the pending count and stay unmutated.
	assert.GreaterOrEqual(t, res.Scanned, 1)

	var ver int
	require.NoError(t, ts.DB.QueryRow(`SELECT key_version FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&ver))
	assert.Equal(t, 1, ver, "dry-run must not mutate")
}

func TestRekey_UndecryptableRowLeftUntouchedAndReported(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	// Sealed under a key that is in NO ring (version 3 stamp, unknown key).
	orphanRing, err := mfa.ParseKeyring("0303030303030303030303030303030303030303030303030303030303030303", 3, "")
	require.NoError(t, err)
	uid := seedSealedRow(t, ts, orphanRing, "rekeyorphan", "JBSWY3DPEHPK3PXP")
	seedSealedRow(t, ts, v1OnlyRing(t), "rekeygood", "KRSXG5CTMVRXEZLU")

	res, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, false)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Rekeyed, 1, "the decryptable row still converges")
	require.NotEmpty(t, res.Failed)
	var found *mfa.RekeyFailure
	for i := range res.Failed {
		if res.Failed[i].UserID == uid {
			found = &res.Failed[i]
		}
	}
	require.NotNil(t, found, "orphan row must be reported")
	assert.Equal(t, 3, found.SealedVersion)

	var ver int
	require.NoError(t, ts.DB.QueryRow(`SELECT key_version FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&ver))
	assert.Equal(t, 3, ver, "failed row must be left untouched")
}

// TestRekey_CASGuardSkipsConcurrentlyRewrittenRow exercises the UPDATE's
// compare-and-swap tail directly: if the stored ciphertext no longer matches
// what the backfill read, the write must affect 0 rows.
func TestRekey_CASGuardSkipsConcurrentlyRewrittenRow(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	uid := seedSealedRow(t, ts, v1OnlyRing(t), "rekeycas", "JBSWY3DPEHPK3PXP")

	var origEnc []byte
	require.NoError(t, ts.DB.QueryRow(`SELECT totp_secret_enc FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&origEnc))

	// Simulate a concurrent re-enrollment: the server rewrote the row under the
	// active version between the backfill's SELECT and UPDATE.
	newCT, newNonce, newVer, err := ring.Seal([]byte("NEWSEEDNEWSEEDNE"))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE user_mfa_totp SET totp_secret_enc=$1, totp_secret_nonce=$2, key_version=$3 WHERE user_id=$4`,
		newCT, newNonce, newVer, uid)
	require.NoError(t, err)

	// Replay the backfill's CAS UPDATE with the STALE prior image.
	staleCT, staleNonce, _, err := v1OnlyRing(t).Seal([]byte("JBSWY3DPEHPK3PXP"))
	require.NoError(t, err)
	tag, err := ts.DB.Exec(`
		UPDATE user_mfa_totp
		   SET totp_secret_enc = $1, totp_secret_nonce = $2, key_version = $3, updated_at = NOW()
		 WHERE user_id = $4 AND key_version = $5 AND totp_secret_enc = $6`,
		staleCT, staleNonce, ring.ActiveVersion(), uid, 1, origEnc)
	require.NoError(t, err)
	n, err := tag.RowsAffected()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "CAS tail must skip a concurrently rewritten row")

	var enc []byte
	require.NoError(t, ts.DB.QueryRow(`SELECT totp_secret_enc FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&enc))
	assert.Equal(t, newCT, enc, "concurrent rewrite must win")
}
