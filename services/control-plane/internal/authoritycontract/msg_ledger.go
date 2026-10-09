package authoritycontract

import (
	"bytes"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
)

func init() {
	register(
		func() Message { return new(DeviceSet) }, func() Message { return new(DeviceIntent) },
		func() Message { return new(DeviceResult) }, func() Message { return new(DeviceStatus) },
		func() Message { return new(DeviceBundle) }, func() Message { return new(DeviceHistory) },
		func() Message { return new(RecoveryBind) },
	)
}

// DeviceEntry is one parsed `dev` (§2.5, §2.12).
type DeviceEntry struct {
	ID         string
	DSK, DKX   [97]byte
	MatureAtMs uint64
}

// Dev is the entry's `dev` string: device_id.dsk.dkx.mature_at_ms.
func (e DeviceEntry) Dev() string {
	return e.ID + "." + base64.StdEncoding.EncodeToString(e.DSK[:]) + "." +
		base64.StdEncoding.EncodeToString(e.DKX[:]) + "." + strconv.FormatUint(e.MatureAtMs, 10)
}

// ParseDev parses a `dev`: a v4 device_id equal to DeviceID(dsk), two Strict
// P-384 points, and a u maturity; at most 319 characters.
func ParseDev(s string) (DeviceEntry, error) {
	var e DeviceEntry
	if len(s) > 319 { // before Split: this is exported, and Split allocates in proportion to s
		return e, malformed("dev")
	}
	p := strings.Split(s, ".")
	if len(p) != 4 || !isUUIDv4(p[0]) {
		return e, malformed("dev")
	}
	dsk, err := decodeB64(p[1])
	if err != nil || checkPoint(dsk) != nil {
		return e, malformed("dev dsk")
	}
	dkx, err := decodeB64(p[2])
	if err != nil || checkPoint(dkx) != nil {
		return e, malformed("dev dkx")
	}
	mature, err := parseU(p[3])
	if err != nil {
		return e, err
	}
	e.ID, e.MatureAtMs = p[0], mature
	copy(e.DSK[:], dsk)
	copy(e.DKX[:], dkx)
	if DeviceID(e.DSK) != e.ID {
		return DeviceEntry{}, malformed("dev id is not DeviceID(dsk)")
	}
	return e, nil
}

func checkDev(s string) error { _, err := ParseDev(s); return err }

var kDev = kStr(319).withStr(checkDev)

// DeviceSet is `device-set` (12): digested, never signed (D-240).
type DeviceSet struct {
	RealmID, AccountID                                                     string
	Active                                                                 []string
	Pending                                                                string
	RecoveryKey, RecoveryBindDigest, ReplacementKey, ReplacementBindDigest []byte
	ReplacementNbMs                                                        uint64
}

func (m *DeviceSet) spec() (string, []field) {
	return "device-set", []field{
		{"realm_id", kS36, &m.RealmID},                                          // 3
		{"account_id", kS36, &m.AccountID},                                      // 4
		{"active", kA(kDev, 0, MaxDevices), &m.Active},                          // 5
		{"pending", kDev.OrEmpty(), &m.Pending},                                 // 6
		{"recovery_key", kP97.OrEmpty(), &m.RecoveryKey},                        // 7
		{"recovery_bind_digest", kB(48).OrEmpty(), &m.RecoveryBindDigest},       // 8
		{"replacement_key", kP97.OrEmpty(), &m.ReplacementKey},                  // 9
		{"replacement_bind_digest", kB(48).OrEmpty(), &m.ReplacementBindDigest}, // 10
		{"replacement_nb_ms", kU, &m.ReplacementNbMs},                           // 11
	}
}

func (m *DeviceSet) crossCheck() error {
	if err := checkDistinctDeviceIDs(m.Active); err != nil {
		return err
	}
	if m.Pending != "" {
		// §2.5 "pending is not in active" is by device_id: the same device at
		// another maturity or with another dkx is still in active.
		pending := devID(m.Pending)
		if slices.ContainsFunc(m.Active, func(d string) bool { return devID(d) == pending }) {
			return malformed("pending is in active")
		}
	}
	return checkRecoverySlot(m)
}

// devID is the exact 36-character device_id of a validated `dev`: the text
// before its first '.'.
func devID(dev string) string {
	id, _, _ := strings.Cut(dev, ".")
	return id
}

// checkDistinctDeviceIDs is §2.5's "IDs are pairwise distinct (V-10)". The
// strictly ascending order of `active` does not imply it: id.dsk.dkx.5 sorts
// before id.dsk.dkx.6, so one device could be listed twice at two maturities.
// The device_ids are therefore sorted and compared on their own, which also
// catches non-adjacent duplicates in a caller's unsorted input.
func checkDistinctDeviceIDs(active []string) error {
	ids := make([]string, len(active))
	for i, d := range active {
		ids[i] = devID(d)
	}
	slices.Sort(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return malformed("duplicate device id")
		}
	}
	return nil
}

// checkRecoverySlot is D-251's all-or-nothing rule for the recovery positions.
func checkRecoverySlot(m *DeviceSet) error {
	bound := len(m.RecoveryKey) > 0
	slot := len(m.ReplacementBindDigest) > 0
	hasKey := len(m.ReplacementKey) > 0
	switch {
	case bound != (len(m.RecoveryBindDigest) > 0):
		return malformed("recovery key and digest")
	case slot != (m.ReplacementNbMs != 0):
		return malformed("slot digest and nb")
	case !slot && hasKey:
		return malformed("slot key without digest")
	case slot && !hasKey && !bound:
		return malformed("pending unbind without a bound key")
	case hasKey && bytes.Equal(m.ReplacementKey, m.RecoveryKey):
		return malformed("refresh to the bound key")
	}
	return nil
}

// Encode returns the canonical `device-set` bytes of m, or nil if m is not a valid `device-set`.
func (m DeviceSet) Encode() []byte { return encode(&m) }

// DecodeDeviceSet decodes b as the `device-set` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceSet(b []byte) (DeviceSet, error) { return decodeAs[DeviceSet](b, 0) }

// DeviceSetDigest is device_set_digest over a valid device-set's exact bytes.
func DeviceSetDigest(set []byte) ([]byte, error) {
	if _, err := DecodeDeviceSet(set); err != nil {
		return nil, err
	}
	return Digest(set), nil
}

// DeviceIntent is `device-intent` (20), digested as D (D-240).
type DeviceIntent struct {
	RealmID, AccountID, RequestID, Op string
	Salt                              []byte
	ExpiresAtMs, ExpectedSeq          uint64
	ExpectedHeadDigest                []byte
	ExpectedDseq                      uint64
	ExpectedDeviceHead                []byte
	ActorDeviceID, NewDeviceID        string
	NewDeviceDSK, NewDeviceDKX        []byte
	RemovedDeviceIDs                  []string
	RecoveryBindDigest                []byte
	BindWait                          string
}

func (m *DeviceIntent) spec() (string, []field) {
	return "device-intent", []field{
		{"realm_id", kS36, &m.RealmID},                                    // 3
		{"account_id", kS36, &m.AccountID},                                // 4
		{"request_id", kS36v4, &m.RequestID},                              // 5
		{"op", kE(deviceOps...), &m.Op},                                   // 6
		{"salt", kB(32), &m.Salt},                                         // 7
		{"expires_at_ms", kU, &m.ExpiresAtMs},                             // 8
		{"expected_seq", kU, &m.ExpectedSeq},                              // 9
		{"expected_head_digest", kB(48).OrEmpty(), &m.ExpectedHeadDigest}, // 10
		{"expected_dseq", kU, &m.ExpectedDseq},                            // 11
		{"expected_device_head", kB(48).OrEmpty(), &m.ExpectedDeviceHead}, // 12
		{"actor_device_id", kS36v4.OrEmpty(), &m.ActorDeviceID},           // 13
		{"new_device_id", kS36v4.OrEmpty(), &m.NewDeviceID},               // 14
		{"new_device_dsk", kP97.OrEmpty(), &m.NewDeviceDSK},               // 15
		{"new_device_dkx", kP97.OrEmpty(), &m.NewDeviceDKX},               // 16
		{"removed_device_ids", kA(kS36v4, 0, 9), &m.RemovedDeviceIDs},     // 17
		{"recovery_bind_digest", kB(48).OrEmpty(), &m.RecoveryBindDigest}, // 18
		{"bind_wait", kE(bindWaits...), &m.BindWait},                      // 19
	}
}

// Encode returns the canonical `device-intent` bytes of m, or nil if m is not a valid `device-intent`.
func (m DeviceIntent) Encode() []byte { return encode(&m) }

// DecodeDeviceIntent decodes b as the `device-intent` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceIntent(b []byte) (DeviceIntent, error) { return decodeAs[DeviceIntent](b, 0) }

// DeviceResult is `device-result` (17), RSK-signed.
type DeviceResult struct {
	RealmID, AccountID, RequestID               string
	DeviceIntentDigest, CPAuthzDigest           []byte
	Op                                          string
	Dseq                                        uint64
	PrevDeviceEntryDigest                       []byte
	ChainSeq                                    uint64
	ChainEntryDigest, DeviceSetDigest           []byte
	IssuedAtMs, DelegationGen, DelegationSerial uint64
}

func (m *DeviceResult) spec() (string, []field) {
	return "device-result", []field{
		{"realm_id", kS36, &m.RealmID},                                           // 3
		{"account_id", kS36, &m.AccountID},                                       // 4
		{"request_id", kS36v4, &m.RequestID},                                     // 5
		{"device_intent_digest", kB(48), &m.DeviceIntentDigest},                  // 6
		{"cp_authz_digest", kB(48).OrEmpty(), &m.CPAuthzDigest},                  // 7
		{"op", kE(deviceOps...), &m.Op},                                          // 8
		{"dseq", kU1, &m.Dseq},                                                   // 9
		{"prev_device_entry_digest", kB(48).OrEmpty(), &m.PrevDeviceEntryDigest}, // 10
		{"chain_seq", kU1, &m.ChainSeq},                                          // 11
		{"chain_entry_digest", kB(48), &m.ChainEntryDigest},                      // 12
		{"device_set_digest", kB(48), &m.DeviceSetDigest},                        // 13
		{"issued_at_ms", kU, &m.IssuedAtMs},                                      // 14
		{"delegation_gen", kU, &m.DelegationGen},                                 // 15
		{"delegation_serial", kU1, &m.DelegationSerial},                          // 16
	}
}

// Encode returns the canonical `device-result` bytes of m, or nil if m is not a valid `device-result`.
func (m DeviceResult) Encode() []byte { return encode(&m) }

// DecodeDeviceResult decodes b as the `device-result` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceResult(b []byte) (DeviceResult, error) { return decodeAs[DeviceResult](b, 0) }

// DeviceStatus is `device-status` (14), co-signed with every head status (D-241).
type DeviceStatus struct {
	RealmID, AccountID                                    string
	ChainSeq                                              uint64
	ChainHeadDigest                                       []byte
	Dseq                                                  uint64
	DeviceHeadDigest, DeviceSetDigest                     []byte
	AsOfMs, ValidUntilMs, DelegationGen, DelegationSerial uint64
}

func (m *DeviceStatus) spec() (string, []field) {
	return "device-status", []field{
		{"realm_id", kS36, &m.RealmID},                                // 3
		{"account_id", kS36, &m.AccountID},                            // 4
		{"chain_seq", kU, &m.ChainSeq},                                // 5
		{"chain_head_digest", kB(48).OrEmpty(), &m.ChainHeadDigest},   // 6
		{"dseq", kU, &m.Dseq},                                         // 7
		{"device_head_digest", kB(48).OrEmpty(), &m.DeviceHeadDigest}, // 8
		{"device_set_digest", kB(48), &m.DeviceSetDigest},             // 9
		{"as_of_ms", kU, &m.AsOfMs},                                   // 10
		{"valid_until_ms", kU, &m.ValidUntilMs},                       // 11
		{"delegation_gen", kU, &m.DelegationGen},                      // 12
		{"delegation_serial", kU1, &m.DelegationSerial},               // 13
	}
}

// Encode returns the canonical `device-status` bytes of m, or nil if m is not a valid `device-status`.
func (m DeviceStatus) Encode() []byte { return encode(&m) }

// DecodeDeviceStatus decodes b as the `device-status` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceStatus(b []byte) (DeviceStatus, error) { return decodeAs[DeviceStatus](b, 0) }

// DeviceBundle is `device-bundle` (14): one private-ledger entry with its
// private signatures. Nested messages are opaque here.
type DeviceBundle struct {
	DeviceIntent, SigRoot, SigActor, SigNewDevice, RecoveryBind, SigRecovery, SigRecoveryPrior,
	CPAuthz, CPAuthzSig, DeviceResult, DeviceResultSig []byte
}

func (m *DeviceBundle) spec() (string, []field) {
	return "device-bundle", []field{
		{"device_intent", kBUpTo(2048).OrEmpty(), &m.DeviceIntent},    // 3
		{"sig_root", kB(96).OrEmpty(), &m.SigRoot},                    // 4
		{"sig_actor", kB(96).OrEmpty(), &m.SigActor},                  // 5
		{"sig_new_device", kB(96).OrEmpty(), &m.SigNewDevice},         // 6
		{"recovery_bind", kBUpTo(1024).OrEmpty(), &m.RecoveryBind},    // 7
		{"sig_recovery", kB(96).OrEmpty(), &m.SigRecovery},            // 8
		{"sig_recovery_prior", kB(96).OrEmpty(), &m.SigRecoveryPrior}, // 9
		{"cp_authz", kBUpTo(1024).OrEmpty(), &m.CPAuthz},              // 10
		{"cp_authz_sig", kB(96).OrEmpty(), &m.CPAuthzSig},             // 11
		{"device_result", kBUpTo(1024), &m.DeviceResult},              // 12
		{"device_result_sig", kB(96), &m.DeviceResultSig},             // 13
	}
}

// Encode returns the canonical `device-bundle` bytes of m, or nil if m is not a valid `device-bundle`.
func (m DeviceBundle) Encode() []byte { return encode(&m) }

// DecodeDeviceBundle decodes b as the `device-bundle` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceBundle(b []byte) (DeviceBundle, error) { return decodeAs[DeviceBundle](b, 0) }

// DeviceHistory is `device-history` (5).
type DeviceHistory struct {
	AccountID string
	Bundles   [][]byte
}

func (m *DeviceHistory) spec() (string, []field) {
	return "device-history", []field{
		{"account_id", kS36, &m.AccountID},                       // 3
		{"bundles", kL(kBUpTo(8192), 0, DevicePage), &m.Bundles}, // 4
	}
}

// Encode returns the canonical `device-history` bytes of m, or nil if m is not a valid `device-history`.
func (m DeviceHistory) Encode() []byte { return encode(&m) }

// DecodeDeviceHistory decodes b as the `device-history` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceHistory(b []byte) (DeviceHistory, error) { return decodeAs[DeviceHistory](b, 0) }

// RecoveryBind is `recovery-bind` B (14), digested as H(B) (§2.22, LD-53).
type RecoveryBind struct {
	RealmID, AccountID, RequestID, Kind                  string
	ExpiresAtMs                                          uint64
	RecoveryKey, TargetDigest                            []byte
	ChainSeq                                             uint64
	ChainEntryDigest, BindingRootDigest, EscrowPublicKey []byte
}

func (m *RecoveryBind) spec() (string, []field) {
	return "recovery-bind", []field{
		{"realm_id", kS36, &m.RealmID},                                  // 3
		{"account_id", kS36, &m.AccountID},                              // 4
		{"request_id", kS36v4, &m.RequestID},                            // 5
		{"kind", kE(bindKinds...), &m.Kind},                             // 6
		{"expires_at_ms", kU, &m.ExpiresAtMs},                           // 7
		{"recovery_key", kP97.OrEmpty(), &m.RecoveryKey},                // 8
		{"target_digest", kB(48).OrEmpty(), &m.TargetDigest},            // 9
		{"chain_seq", kU, &m.ChainSeq},                                  // 10
		{"chain_entry_digest", kB(48).OrEmpty(), &m.ChainEntryDigest},   // 11
		{"binding_root_digest", kB(48).OrEmpty(), &m.BindingRootDigest}, // 12
		{"escrow_public_key", kP97.OrEmpty(), &m.EscrowPublicKey},       // 13
	}
}

func (m *RecoveryBind) crossCheck() error {
	if len(m.EscrowPublicKey) > 0 && bytes.Equal(m.EscrowPublicKey, m.RecoveryKey) {
		return malformed("escrow key equals the recovery key")
	}
	return nil
}

// Encode returns the canonical `recovery-bind` bytes of m, or nil if m is not a valid `recovery-bind`.
func (m RecoveryBind) Encode() []byte { return encode(&m) }

// DecodeRecoveryBind decodes b as the `recovery-bind` layout under EN5; any failure wraps ErrMalformed.
func DecodeRecoveryBind(b []byte) (RecoveryBind, error) { return decodeAs[RecoveryBind](b, 0) }
