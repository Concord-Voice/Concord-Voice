package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// DoR §3.1 "Bind tiers (LD-41, LD-43, LD-47, D-264; BindTier, Go only)".
func TestBindTierVectors(t *testing.T) {
	const (
		now     = BindWait(BindWaitNow)
		delta   = BindWait(BindWaitDelta)
		replace = BindWait(BindWaitReplace)
	)
	cases := []struct {
		factors []string
		mfa     bool
		want    BindWait
	}{
		{[]string{FactorPassword}, false, replace},
		{[]string{FactorPassword, FactorTOTP}, true, delta},
		{[]string{FactorPassword, FactorEmail}, false, delta},
		{[]string{FactorPassword, FactorBackupCode}, true, delta},
		{[]string{FactorPassword, FactorEmail, FactorTOTP}, true, now},
		{[]string{FactorPassword, FactorEmail, FactorEmailBackup}, false, now},
		{[]string{FactorPassword, FactorEmail, FactorPasskey, FactorBackupCode}, true, now},
		{[]string{FactorPassword, FactorEmail, FactorBackupCode}, true, delta},                    // a backup code never counts toward now
		{[]string{FactorPassword, FactorEmail, FactorBackupCode}, false, delta},                   // LD-47 on its own: no LD-50 guard to lean on
		{[]string{FactorPassword, FactorEmail, FactorEmailBackup, FactorBackupCode}, true, delta}, // LD-50, RB3-4, RT6-5
		{[]string{FactorPassword, FactorEmail, FactorEmailBackup, FactorTOTP}, true, now},
		{[]string{FactorPassword, FactorEmailBackup}, false, delta},                        // V3-12
		{[]string{FactorPassword, FactorTOTP, FactorWebAuthn, FactorPasskey}, true, delta}, // the MFA factor counts once
		{[]string{FactorEmail, FactorBackupCode}, true, delta},
		{[]string{FactorTOTP, FactorBackupCode}, true, replace}, // a backup code stands in only when no MFA method is present
		{[]string{FactorSSO}, false, replace},                   // LD-48
		{[]string{FactorSSO, FactorEmail}, false, delta},        // an SSO-only account with an address
		{[]string{FactorSSO, FactorEmail, FactorTOTP}, true, now},
		{[]string{FactorPassword, FactorSSO}, false, replace}, // the login counts once
		{[]string{FactorPassword, FactorSSO, FactorEmail}, false, delta},
		{[]string{FactorSession}, false, ""}, // insufficient_authorization
		{[]string{FactorRecoveryKey, FactorDeviceKey, FactorRegistration, FactorAdminWebAuthn, FactorErasureList, FactorSession}, false, ""},
		{[]string{FactorPassword, FactorEmail, FactorEmailBackup}, true, delta}, // MFA enrolled but none asserted
	}
	for _, c := range cases {
		require.Equal(t, c.want, BindTier(c.factors, c.mfa), "%v mfa=%v", c.factors, c.mfa)
	}
}

// "a bind_wait shorter than the tier refused (insufficient_authorization)
// and a longer one accepted with the longer wait".
func TestBindWaitAdmitted(t *testing.T) {
	now, delta, replace := BindWait(BindWaitNow), BindWait(BindWaitDelta), BindWait(BindWaitReplace)
	require.True(t, BindWaitAdmitted(now, now))
	require.True(t, BindWaitAdmitted(delta, now))
	require.True(t, BindWaitAdmitted(replace, delta))
	require.False(t, BindWaitAdmitted(now, delta))
	require.False(t, BindWaitAdmitted(delta, replace))
	require.False(t, BindWaitAdmitted(replace, ""), "no tier admits nothing")
	require.False(t, BindWaitAdmitted("", now), "an empty bind_wait is never a wait")
}
