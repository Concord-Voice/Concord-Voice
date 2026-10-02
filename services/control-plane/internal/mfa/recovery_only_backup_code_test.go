package mfa

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Backup codes belong to TOTP. When TOTP is recovery-only, sign-in refuses its
// backup codes too, and refuses them without judging them, so a refused
// attempt spends neither a backup code nor a TOTP step. Before this fix the
// restriction compared only the matched method name, and "backup_code" is
// never on the restricted list. Found as C50 of the MFA factor picker design
// ([internal]specs/2026-09-26-mfa-factor-picker-design.md, PR #3540).
func TestVerifyRefusesTOTPFamilyCodesWhenTOTPIsRecoveryOnly(t *testing.T) {
	codes, hashes, err := GenerateBackupCodes()
	require.NoError(t, err)
	used := "{" + strings.TrimSuffix(strings.Repeat("f,", len(hashes)), ",") + "}"

	for _, tc := range []struct {
		name         string
		method       string
		backup       bool
		empty        bool
		mfaMethods   string
		recoveryOnly string
		want         int
	}{
		{name: "backup code, totp restricted", method: "backup_code", backup: true, recoveryOnly: "{totp}", want: http.StatusForbidden},
		{name: "backup code under the totp label, totp restricted", method: "totp", backup: true, recoveryOnly: "{totp}", want: http.StatusForbidden},
		{name: "totp code, totp restricted", method: "totp", recoveryOnly: "{totp}", want: http.StatusForbidden},
		// Positive control: the same fixture, unrestricted, signs in with the
		// backup code and spends it, so the harness reaches that path.
		{name: "backup code, nothing restricted", method: "backup_code", backup: true, want: http.StatusOK},
		// The effective list governs backup codes too: with TOTP the only
		// method, the restriction lapses and its backup codes sign in.
		{name: "backup code, restriction lapses", method: "backup_code", backup: true, mfaMethods: "{totp}", recoveryOnly: "{totp}", want: http.StatusOK},
		{name: "totp code, restriction lapses", method: "totp", mfaMethods: "{totp}", recoveryOnly: "{totp}", want: http.StatusOK},
		// Only a TOTP restriction covers backup codes: restricting email
		// leaves them signing in.
		{name: "backup code, only email restricted", method: "backup_code", backup: true, recoveryOnly: "{email}", want: http.StatusOK},
		// The guard leaves an empty code to verifyTOTPOrBackup's 400, so a
		// missing code still reads as missing, not as wrong.
		{name: "empty code, totp restricted", method: "totp", empty: true, recoveryOnly: "{totp}", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerifyFixture(t)
			f.state.mfaMethods = tc.mfaMethods
			f.state.recoveryOnly = tc.recoveryOnly
			f.state.backupHashes = "{" + strings.Join(hashes, ",") + "}"
			f.state.backupUsed = used
			code := f.right
			if tc.backup {
				code = codes[0]
			}
			if tc.empty {
				code = ""
			}

			response := verifyWith(f.h, `{"mfa_challenge_token":"`+f.token+`","method":"`+tc.method+`","code":"`+code+`"}`)

			require.Equal(t, tc.want, response.Code, response.Body.String())
			require.Equal(t, tc.want == http.StatusOK, f.completer.called, "only an accepted code signs in")
			switch {
			case tc.want == http.StatusBadRequest:
				require.Contains(t, response.Body.String(), errMsgCodeRequired)
				require.Zero(t, f.judged())
			case tc.want == http.StatusForbidden:
				require.Contains(t, response.Body.String(), "Invalid MFA code", "the refusal must not confirm the code was right")
				require.Zero(t, f.judged(), "a restricted TOTP-family code must not be judged")
				require.Zero(t, backupCodesSpent(f.state), "the refused backup code must stay unspent")
			case tc.backup:
				require.Equal(t, 1, backupCodesSpent(f.state), "the control must spend the backup code it signed in with")
			default:
				require.Equal(t, 1, f.judged(), "once the restriction lapses, the TOTP code is judged")
			}
		})
	}
}

// A refused restricted code is a failed attempt like any wrong code: it counts
// toward the lockout, so restricted backup codes cannot be tried without limit.
func TestVerifyRestrictedBackupCodesArmTheLockout(t *testing.T) {
	codes, hashes, err := GenerateBackupCodes()
	require.NoError(t, err)
	f := newVerifyFixture(t)
	f.state.recoveryOnly = "{totp}"
	f.state.backupHashes = "{" + strings.Join(hashes, ",") + "}"
	f.state.backupUsed = "{" + strings.TrimSuffix(strings.Repeat("f,", len(hashes)), ",") + "}"

	for i := 0; i < failedAttemptLimit; i++ {
		response := verifyWith(f.h, `{"mfa_challenge_token":"`+f.token+`","method":"backup_code","code":"`+codes[i]+`"}`)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	}

	require.True(t, f.exists(t, "mfa_verify_lockout:"+enrollUser), "restricted codes must arm the lockout as wrong codes do")
	require.Zero(t, backupCodesSpent(f.state))
}

// backupCodesSpent counts the backup-code consumption writes state recorded.
func backupCodesSpent(state *mfaEventDB) int {
	state.mu.Lock()
	defer state.mu.Unlock()
	n := 0
	for _, statement := range state.statements {
		if strings.Contains(statement, "SET backup_codes_used") {
			n++
		}
	}
	return n
}
