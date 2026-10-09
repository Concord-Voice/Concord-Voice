package authoritycontract

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func formR() RelayRecover {
	return RelayRecover{Intent: rnd(700), SigCandidate: rnd(96), SigRecovery: rnd(96), EKBinding: rnd(900),
		EKBindingSig: rnd(96), RecoveryBind: rnd(600), RecoveryBindSig: rnd(96), DeviceIntent: rnd(1100), SigNewDevice: rnd(96)}
}

func TestWrapFamilyRoundTrip(t *testing.T) {
	roundTrip(t, DeviceWrap{RealmID: newID(), AccountID: newID(), RecipientDeviceID: newID(),
		DeviceEntryDigest: digestOf("e"), EPK: testPoint(t), Nonce: rnd(12), CT: rnd(2000)})
	roundTrip(t, DeviceSecrets{RootPKCS8: rnd(185), EKPKCS8: rnd(2373)})
	roundTrip(t, DeviceSecrets{EKPKCS8: rnd(2373)})
	roundTrip(t, formR())
	roundTrip(t, RelayRecover{Intent: rnd(700), SigRecovery: rnd(96)})       // form C
	roundTrip(t, RelayRecover{RecoveryBind: rnd(600), SigRecovery: rnd(96)}) // form B
}

func TestRelayRecoverForms(t *testing.T) {
	for name, m := range map[string]RelayRecover{
		"sig_recovery alone":       {SigRecovery: rnd(96)},
		"C plus ek_binding":        {Intent: rnd(700), SigRecovery: rnd(96), EKBinding: rnd(900)},
		"B plus intent":            {Intent: rnd(700), RecoveryBind: rnd(600), SigRecovery: rnd(96)},
		"R without device_intent":  func() RelayRecover { r := formR(); r.DeviceIntent = nil; return r }(),
		"B plus recovery_bind_sig": {RecoveryBind: rnd(600), RecoveryBindSig: rnd(96), SigRecovery: rnd(96)},
	} {
		require.Nil(t, m.Encode(), name)
	}
	require.Nil(t, RelayRecover{Intent: rnd(700)}.Encode(), "sig_recovery is never empty")
}

func TestWrapFamilyWorstCases(t *testing.T) {
	require.Equal(t, 3866, layoutMax(&DeviceSecrets{}), "DoR §2.3 device-secrets")
	require.Equal(t, CapRelayRecover, layoutMax(&RelayRecover{}), "DoR §2.3: the cap is the sum of the position caps")
	require.LessOrEqual(t, layoutMax(&DeviceWrap{}), CapDeviceWrap)
	w := DeviceWrap{RealmID: newID(), AccountID: newID(), RecipientDeviceID: newID(), DeviceEntryDigest: digestOf("e"),
		EPK: hybridOf(testPoint(t)), Nonce: rnd(12), CT: rnd(16)}
	require.Nil(t, w.Encode(), "a 0x06/0x07 epk is refused")
}

// wrapWire hand-builds the canonical wire text of a layout whose positions
// are all `b` kinds, independently of the codec's writer.
func wrapWire(tag string, positions ...[]byte) string {
	parts := make([]string, 0, len(positions))
	for _, p := range positions {
		parts = append(parts, `"`+base64.StdEncoding.EncodeToString(p)+`"`)
	}
	return fmt.Sprintf(`[%q,%d,%q,`, Prefix, Version, tag) + strings.Join(parts, ",") + "]"
}

// wrapRelayWire is wrapWire in relay-recover's wire order (positions 3 to 11).
func wrapRelayWire(m RelayRecover) string {
	return wrapWire("relay-recover", m.Intent, m.SigCandidate, m.SigRecovery, m.EKBinding, m.EKBindingSig,
		m.RecoveryBind, m.RecoveryBindSig, m.DeviceIntent, m.SigNewDevice)
}

// wrapRelayMask presents the eight optional positions whose bit is set in
// mask (bit 0 intent, 1 sig_candidate, 2 ek_binding, 3 ek_binding_sig,
// 4 recovery_bind, 5 recovery_bind_sig, 6 device_intent, 7 sig_new_device).
// sig_recovery is always present.
func wrapRelayMask(mask int) RelayRecover {
	f := formR()
	pick := func(bit int, b []byte) []byte {
		if mask&(1<<bit) != 0 {
			return b
		}
		return nil
	}
	return RelayRecover{
		Intent: pick(0, f.Intent), SigCandidate: pick(1, f.SigCandidate), SigRecovery: f.SigRecovery,
		EKBinding: pick(2, f.EKBinding), EKBindingSig: pick(3, f.EKBindingSig),
		RecoveryBind: pick(4, f.RecoveryBind), RecoveryBindSig: pick(5, f.RecoveryBindSig),
		DeviceIntent: pick(6, f.DeviceIntent), SigNewDevice: pick(7, f.SigNewDevice),
	}
}

// TestRelayRecoverEveryPresencePattern pins §2.6's "exactly one of three
// forms" over all 2^8 presence patterns of the eight optional positions:
// R (all eight), C (intent only) and B (recovery_bind only) are accepted and
// the other 253 are malformed, on both the encode and the decode side.
func TestRelayRecoverEveryPresencePattern(t *testing.T) {
	const maskR, maskC, maskB = 0xFF, 1 << 0, 1 << 4
	accepted := 0
	for mask := 0; mask < 1<<8; mask++ {
		m := wrapRelayMask(mask)
		wire := wrapRelayWire(m)
		name := fmt.Sprintf("mask %08b", mask)
		got, err := DecodeRelayRecover([]byte(wire))
		if mask == maskR || mask == maskC || mask == maskB {
			accepted++
			enc := m.Encode()
			require.NotNil(t, enc, name)
			require.Equal(t, wire, string(enc), name+": hand-built wire is the canonical encoding")
			require.NoError(t, err, name)
			require.Equal(t, m, got, name)
			continue
		}
		require.Nil(t, m.Encode(), name)
		require.ErrorIs(t, err, ErrMalformed, name)
		require.ErrorContains(t, err, "relay-recover form", name+": refused by the form rule, not a wire fault")
	}
	require.Equal(t, 3, accepted)
}

// TestDeviceSecretsBounds pins the position caps of device-secrets on both
// the encode and the decode side, with the boundary values as controls.
func TestDeviceSecretsBounds(t *testing.T) {
	roundTrip(t, DeviceSecrets{RootPKCS8: rnd(256), EKPKCS8: rnd(2600)})
	for name, m := range map[string]DeviceSecrets{
		"empty ek_pkcs8":          {RootPKCS8: rnd(185)},
		"ek_pkcs8 of 2601 bytes":  {EKPKCS8: rnd(2601)},
		"root_pkcs8 of 257 bytes": {RootPKCS8: rnd(257), EKPKCS8: rnd(2373)},
	} {
		require.Nil(t, m.Encode(), name)
		_, err := DecodeDeviceSecrets([]byte(wrapWire("device-secrets", m.RootPKCS8, m.EKPKCS8)))
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}
