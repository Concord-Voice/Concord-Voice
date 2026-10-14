package authoritycontract

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func init() {
	codecChecks["device-ledger"] = checkDeviceLedgerVector
	codecChecks["p1"] = checkP1Vector
}

// ledgerInputOf is what a device-ledger vector serves to VerifyDeviceLedger.
func ledgerInputOf(t *testing.T, v vector) DeviceLedgerInput {
	t.Helper()
	return DeviceLedgerInput{ChainBundles: listB64(t, v.Bundles), DeviceBundles: listB64(t, v.DeviceBundles),
		Status: mustB64(t, v.BytesB64), DeviceStatus: mustB64(t, v.DeviceStatus)}
}

func checkDeviceLedgerVector(t *testing.T, v vector) {
	tr, r := trustOf(t, v)
	require.Equal(t, ReasonOK, r)
	from, err := ParseDeviceHead(mustB64(t, v.PrevDeviceHead), mustB64(t, v.PrevHead))
	require.NoError(t, err)
	use, ok := ParseUse(v.Use)
	require.True(t, ok)
	ch, dh, _, _, got := VerifyDeviceLedger(tr, hwOf(v.HW), from, ledgerInputOf(t, v), memOf(t, v.Mem), nowOf(t, v.NowMs), use)
	require.Equal(t, v.Expect.Reason, string(got))
	if v.Expect.DeviceHeadB64 != "" {
		d, c := dh.Bytes()
		require.Equal(t, v.Expect.HeadB64, base64.StdEncoding.EncodeToString(ch.Bytes()), "the persisted chain-head (D-269)")
		require.Equal(t, v.Expect.HeadB64, base64.StdEncoding.EncodeToString(c), "the device-head's paired chain-head")
		require.Equal(t, v.Expect.DeviceHeadB64, base64.StdEncoding.EncodeToString(d), "the persisted device-head (D-269)")
	}
}

// checkP1Vector: the status after a private op carries the same message bytes
// as the status before it (P1; LD-32). Both are signed afresh, so only the
// messages compare.
func checkP1Vector(t *testing.T, v vector) {
	require.Equal(t, "status", v.Input.Mode, "%s: no checker for p1 mode %q", v.Name, v.Input.Mode)
	msgOf := func(b64 string) []byte {
		sg, err := DecodeSigned(mustB64(t, b64))
		require.NoError(t, err)
		require.Equal(t, ScopeStatus, sg.Type)
		return sg.Msg
	}
	after, before := msgOf(v.BytesB64), msgOf(v.Input.OtherB64)
	require.Equal(t, before, after, "a private op moved the published status")
	require.Equal(t, v.Expect.ReencodedB64, base64.StdEncoding.EncodeToString(after))
}
