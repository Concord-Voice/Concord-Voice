package authoritycontract

func init() {
	register(
		func() Message { return new(DeviceWrap) }, func() Message { return new(DeviceSecrets) },
		func() Message { return new(RelayRecover) },
	)
}

// DeviceWrap is `device-wrap` (10): the authority never sees it.
type DeviceWrap struct {
	RealmID, AccountID, RecipientDeviceID string
	DeviceEntryDigest, EPK, Nonce, CT     []byte
}

func (m *DeviceWrap) spec() (string, []field) {
	return "device-wrap", []field{
		{"realm_id", kS36, &m.RealmID},                        // 3
		{"account_id", kS36, &m.AccountID},                    // 4
		{"recipient_device_id", kS36v4, &m.RecipientDeviceID}, // 5
		{"device_entry_digest", kB(48), &m.DeviceEntryDigest}, // 6
		{"epk", kP97, &m.EPK},                                 // 7
		{"nonce", kB(12), &m.Nonce},                           // 8
		{"ct", kBUpTo(4096), &m.CT},                           // 9
	}
}

// Encode returns the canonical `device-wrap` bytes of m, or nil if m is not a valid `device-wrap`.
func (m DeviceWrap) Encode() []byte { return encode(&m) }

// DecodeDeviceWrap decodes b as the `device-wrap` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceWrap(b []byte) (DeviceWrap, error) { return decodeAs[DeviceWrap](b, 0) }

// DeviceSecrets is `device-secrets` (5): a wrap's plaintext (§2.12's type).
type DeviceSecrets struct{ RootPKCS8, EKPKCS8 []byte }

func (m *DeviceSecrets) spec() (string, []field) {
	return "device-secrets", []field{
		{"root_pkcs8", kBUpTo(256).OrEmpty(), &m.RootPKCS8}, // 3
		{"ek_pkcs8", kBUpTo(2600), &m.EKPKCS8},              // 4
	}
}

// Encode returns the canonical `device-secrets` bytes of m, or nil if m is not a valid `device-secrets`.
func (m DeviceSecrets) Encode() []byte { return encode(&m) }

// DecodeDeviceSecrets decodes b as the `device-secrets` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceSecrets(b []byte) (DeviceSecrets, error) { return decodeAs[DeviceSecrets](b, 0) }

// RelayRecover is `relay-recover` (12): a sheet holder's recover, recovery-key
// cancel_pending, or sheet-form cancel_recovery_bind (LD-34, D-257).
type RelayRecover struct {
	Intent, SigCandidate, SigRecovery, EKBinding, EKBindingSig, RecoveryBind, RecoveryBindSig,
	DeviceIntent, SigNewDevice []byte
}

func (m *RelayRecover) spec() (string, []field) {
	return "relay-recover", []field{
		{"intent", kBUpTo(2048).OrEmpty(), &m.Intent},               // 3
		{"sig_candidate", kB(96).OrEmpty(), &m.SigCandidate},        // 4
		{"sig_recovery", kB(96), &m.SigRecovery},                    // 5
		{"ek_binding", kBUpTo(2048).OrEmpty(), &m.EKBinding},        // 6
		{"ek_binding_sig", kB(96).OrEmpty(), &m.EKBindingSig},       // 7
		{"recovery_bind", kBUpTo(1024).OrEmpty(), &m.RecoveryBind},  // 8
		{"recovery_bind_sig", kB(96).OrEmpty(), &m.RecoveryBindSig}, // 9
		{"device_intent", kBUpTo(2048).OrEmpty(), &m.DeviceIntent},  // 10
		{"sig_new_device", kB(96).OrEmpty(), &m.SigNewDevice},       // 11
	}
}

// crossCheck admits exactly forms R, C and B by their non-empty pattern (§2.6).
func (m *RelayRecover) crossCheck() error {
	has := func(b []byte) bool { return len(b) > 0 }
	rest := has(m.SigCandidate) || has(m.EKBinding) || has(m.EKBindingSig) || has(m.RecoveryBindSig) ||
		has(m.DeviceIntent) || has(m.SigNewDevice)
	isR := has(m.Intent) && has(m.SigCandidate) && has(m.EKBinding) && has(m.EKBindingSig) &&
		has(m.RecoveryBind) && has(m.RecoveryBindSig) && has(m.DeviceIntent) && has(m.SigNewDevice)
	isC := has(m.Intent) && !has(m.RecoveryBind) && !rest
	isB := !has(m.Intent) && has(m.RecoveryBind) && !rest
	if !isR && !isC && !isB {
		return malformed("relay-recover form")
	}
	return nil
}

// Encode returns the canonical `relay-recover` bytes of m, or nil if m is not a valid `relay-recover`.
func (m RelayRecover) Encode() []byte { return encode(&m) }

// DecodeRelayRecover decodes b as the `relay-recover` layout under EN5; any failure wraps ErrMalformed.
func DecodeRelayRecover(b []byte) (RelayRecover, error) { return decodeAs[RelayRecover](b, 0) }
