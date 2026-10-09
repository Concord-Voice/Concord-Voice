package authoritycontract

import (
	"bytes"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
)

// device-head is registered so the registry holds every layout. Decode on it
// validates the device-head's own positions only; the chain_head_digest
// pairing with its chain-head is checked by ParseDeviceHead alone.
func init() {
	register(func() Message { return new(ChainHead) }, func() Message { return new(deviceHeadWire) })
}

func b64s(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// SeqRecord is a seq_index record; bindRootDigest nil means "no bind root".
func SeqRecord(entryDigest, bindRootDigest []byte) string {
	return b64s(entryDigest) + "." + b64s(bindRootDigest)
}

// RootRecord is a root_index record.
func RootRecord(rootDigest, root []byte, assurance string) string {
	return b64s(rootDigest) + "." + b64s(root) + "." + assurance
}

// RecoverRecord is a recover_index record.
func RecoverRecord(seq uint64, keyDigest []byte) string {
	return strconv.FormatUint(seq, 10) + "." + b64s(keyDigest)
}

// AdmittedRecord is an admitted_by record; admittedBy "" means no actor.
func AdmittedRecord(deviceID, admittedBy string) string { return deviceID + "." + admittedBy }

// SecretsRecord is a secrets record; root nil means the entry carries no root.
func SecretsRecord(entryDigest []byte, op string, root, ekSPKI []byte) string {
	return b64s(entryDigest) + "." + op + "." + b64s(root) + "." + b64s(ekSPKI)
}

func recordParts(s string, n int) ([]string, error) {
	p := strings.Split(s, ".")
	if len(p) != n {
		return nil, malformed("record parts")
	}
	return p, nil
}

func b64Exact(s string, n int) error {
	if b, err := decodeB64(s); err != nil || len(b) != n {
		return malformed("record digest")
	}
	return nil
}

func b64Point(s string) error {
	b, err := decodeB64(s)
	if err != nil {
		return err
	}
	return checkPoint(b)
}

func checkSeqRecord(s string) error {
	p, err := recordParts(s, 2)
	if err != nil {
		return err
	}
	if err := b64Exact(p[0], 48); err != nil {
		return err
	}
	if p[1] == "" {
		return nil // no bind root at this seq (§2.12)
	}
	return b64Exact(p[1], 48)
}

func checkRootRecord(s string) error {
	p, err := recordParts(s, 3)
	if err != nil {
		return err
	}
	if err := b64Exact(p[0], 48); err != nil {
		return err
	}
	if err := b64Point(p[1]); err != nil {
		return err
	}
	if !slices.Contains(segmentAssurances, p[2]) {
		return malformed("root record assurance")
	}
	return nil
}

func checkRecoverRecord(s string) error {
	p, err := recordParts(s, 2)
	if err != nil {
		return err
	}
	if _, err := parseU(p[0]); err != nil {
		return err
	}
	return b64Exact(p[1], 48)
}

func checkAdmittedRecord(s string) error {
	p, err := recordParts(s, 2)
	if err != nil {
		return err
	}
	if !isUUIDv4(p[0]) || (p[1] != "" && !isUUIDv4(p[1])) {
		return malformed("admitted record")
	}
	return nil
}

func checkSecretsRecord(s string) error {
	p, err := recordParts(s, 4)
	if err != nil {
		return err
	}
	if err := b64Exact(p[0], 48); err != nil {
		return err
	}
	if !slices.Contains(secretsOps, p[1]) {
		return malformed("secrets record op")
	}
	if p[2] != "" {
		if err := b64Point(p[2]); err != nil {
			return err
		}
	}
	spki, err := decodeB64(p[3])
	if err != nil {
		return err
	}
	return checkEKSPKI(spki)
}

// ChainHead is the persisted `chain-head` (16, D-269). Callers store and
// compare it only as bytes.
type ChainHead struct {
	RealmID, AccountID            string
	HeadResult, Fix, ActiveEKSPKI []byte
	ActiveEKGeneration            uint64
	PendingEKSPKI                 []byte
	PendingEKGeneration           uint64
	PendingRecoveryKey            []byte
	SeqIndex, RootIndex           []string
	EKIndex                       [][]byte
	RecoverIndex                  []string
}

func (h *ChainHead) spec() (string, []field) {
	return "chain-head", []field{
		{"realm_id", kS36, &h.RealmID},                                                      // 3
		{"account_id", kS36, &h.AccountID},                                                  // 4
		{"head_result", kBUpTo(4096).OrEmpty(), &h.HeadResult},                              // 5
		{"fix", kBUpTo(6144).OrEmpty(), &h.Fix},                                             // 6
		{"active_ek_spki", kEKSPKI.OrEmpty(), &h.ActiveEKSPKI},                              // 7
		{"active_ek_generation", kU, &h.ActiveEKGeneration},                                 // 8
		{"pending_ek_spki", kEKSPKI.OrEmpty(), &h.PendingEKSPKI},                            // 9
		{"pending_ek_generation", kU, &h.PendingEKGeneration},                               // 10
		{"pending_recovery_key", kP97.OrEmpty(), &h.PendingRecoveryKey},                     // 11
		{"seq_index", kL(kStr(129).withStr(checkSeqRecord), 0, -1), &h.SeqIndex},            // 12
		{"root_index", kA(kStr(208).withStr(checkRootRecord), 0, -1), &h.RootIndex},         // 13
		{"ek_index", kA(kB(48), 0, -1), &h.EKIndex},                                         // 14
		{"recover_index", kA(kStr(81).withStr(checkRecoverRecord), 0, -1), &h.RecoverIndex}, // 15
	}
}

// Bytes returns the canonical `chain-head` bytes of h, or nil if h is not a
// valid `chain-head` or its encoding exceeds HEAD_BLOB_MAX.
func (h ChainHead) Bytes() []byte { return encode(&h) }

// Encode is Bytes under the Message interface; the two cannot drift.
func (h ChainHead) Encode() []byte { return h.Bytes() }

// ParseChainHead decodes b as the `chain-head` layout under EN5 and
// HEAD_BLOB_MAX; any failure wraps ErrMalformed.
func ParseChainHead(b []byte) (ChainHead, error) { return decodeAs[ChainHead](b, 0) }

// GenesisHead carries no pin: the seq-0 pin is AccountMemory.pin only (V3-05).
func GenesisHead(realm, account string) ChainHead {
	return ChainHead{RealmID: realm, AccountID: account}
}

// DeviceHead is the persisted `device-head` (11) paired with its ChainHead.
type DeviceHead struct {
	RealmID, AccountID, SelfDeviceID string
	LastDeviceResult, DeviceSet      []byte
	AdmittedBy, Secrets              []string
	Chain                            ChainHead
}

// deviceHeadWire is the `device-head` layout with its derived digest.
type deviceHeadWire struct {
	RealmID, AccountID, SelfDeviceID             string
	LastDeviceResult, DeviceSet, ChainHeadDigest []byte
	AdmittedBy, Secrets                          []string
}

func (w *deviceHeadWire) spec() (string, []field) {
	return "device-head", []field{
		{"realm_id", kS36, &w.RealmID},                                                   // 3
		{"account_id", kS36, &w.AccountID},                                               // 4
		{"self_device_id", kS36v4.OrEmpty(), &w.SelfDeviceID},                            // 5
		{"last_device_result", kBUpTo(1024).OrEmpty(), &w.LastDeviceResult},              // 6
		{"device_set", kBUpTo(CapDeviceSet).OrEmpty(), &w.DeviceSet},                     // 7
		{"chain_head_digest", kB(48), &w.ChainHeadDigest},                                // 8
		{"admitted_by", kA(kStr(73).withStr(checkAdmittedRecord), 0, 11), &w.AdmittedBy}, // 9
		{"secrets", kL(kStr(946).withStr(checkSecretsRecord), 0, -1), &w.Secrets},        // 10
	}
}
func (w deviceHeadWire) Encode() []byte { return encode(&w) }

// Bytes returns the device-head and its paired chain-head, or (nil, nil).
func (h DeviceHead) Bytes() (deviceHead, chainHead []byte) {
	chainHead = h.Chain.Bytes()
	if chainHead == nil {
		return nil, nil
	}
	w := deviceHeadWire{h.RealmID, h.AccountID, h.SelfDeviceID, h.LastDeviceResult, h.DeviceSet,
		Digest(chainHead), h.AdmittedBy, h.Secrets}
	if deviceHead = w.Encode(); deviceHead == nil {
		return nil, nil
	}
	return deviceHead, chainHead
}

// ParseDeviceHead decodes both blobs and refuses a chain_head_digest mismatch.
func ParseDeviceHead(deviceHead, chainHead []byte) (DeviceHead, error) {
	chain, err := ParseChainHead(chainHead)
	if err != nil {
		return DeviceHead{}, err
	}
	w, err := decodeAs[deviceHeadWire](deviceHead, 0)
	if err != nil {
		return DeviceHead{}, err
	}
	if !bytes.Equal(w.ChainHeadDigest, Digest(chainHead)) {
		return DeviceHead{}, malformed("chain_head_digest")
	}
	return DeviceHead{RealmID: w.RealmID, AccountID: w.AccountID, SelfDeviceID: w.SelfDeviceID,
		LastDeviceResult: w.LastDeviceResult, DeviceSet: w.DeviceSet, AdmittedBy: w.AdmittedBy,
		Secrets: w.Secrets, Chain: chain}, nil
}

// GenesisDeviceHead is the empty private head; self is "" for the CP monitor.
func GenesisDeviceHead(realm, account, self string) DeviceHead {
	return DeviceHead{RealmID: realm, AccountID: account, SelfDeviceID: self, Chain: GenesisHead(realm, account)}
}
