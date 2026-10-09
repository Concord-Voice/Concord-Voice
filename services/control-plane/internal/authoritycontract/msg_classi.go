package authoritycontract

import "strings"

func init() {
	register(
		func() Message { return new(Submit) }, func() Message { return new(ImportItem) },
		func() Message { return new(ImportBatch) }, func() Message { return new(LegacyBatch) },
		func() Message { return new(Committed) }, func() Message { return new(Refused) },
		func() Message { return new(ErrorEnvelope) }, func() Message { return new(Head) },
		func() Message { return new(StatusReq) }, func() Message { return new(HoldReq) },
		func() Message { return new(PendingHeads) }, func() Message { return new(Signer) },
		func() Message { return new(DeviceSubmit) }, func() Message { return new(DeviceCommitted) },
	)
}

// Submit is `submit` (20): the bundle's positions 3–15 plus the private part.
type Submit struct {
	Intent, SigCandidate, SigCurrentRoot, SigOperator, SigRecovery, LegacyPopSig, LegacySPKI,
	EKBinding, EKBindingSig, RecoveryBind, RecoveryBindSig, CPAuthz, CPAuthzSig,
	CPEvidence, DeviceIntent, SigActor, SigNewDevice []byte
}

func (m *Submit) spec() (string, []field) {
	return "submit", []field{
		{"intent", kBUpTo(2048), &m.Intent},                         // 3
		{"sig_candidate", kB(96).OrEmpty(), &m.SigCandidate},        // 4
		{"sig_current_root", kB(96).OrEmpty(), &m.SigCurrentRoot},   // 5
		{"sig_operator", kB(96).OrEmpty(), &m.SigOperator},          // 6
		{"sig_recovery", kB(96).OrEmpty(), &m.SigRecovery},          // 7
		{"legacy_pop_sig", kB(512).OrEmpty(), &m.LegacyPopSig},      // 8
		{"legacy_spki", kEKSPKI.OrEmpty(), &m.LegacySPKI},           // 9
		{"ek_binding", kBUpTo(2048).OrEmpty(), &m.EKBinding},        // 10
		{"ek_binding_sig", kB(96).OrEmpty(), &m.EKBindingSig},       // 11
		{"recovery_bind", kBUpTo(1024).OrEmpty(), &m.RecoveryBind},  // 12
		{"recovery_bind_sig", kB(96).OrEmpty(), &m.RecoveryBindSig}, // 13
		{"cp_authz", kBUpTo(1024), &m.CPAuthz},                      // 14
		{"cp_authz_sig", kB(96), &m.CPAuthzSig},                     // 15
		{"cp_evidence", kBUpTo(1024), &m.CPEvidence},                // 16
		{"device_intent", kBUpTo(2048).OrEmpty(), &m.DeviceIntent},  // 17
		{"sig_actor", kB(96).OrEmpty(), &m.SigActor},                // 18
		{"sig_new_device", kB(96).OrEmpty(), &m.SigNewDevice},       // 19
	}
}

// Encode returns the canonical `submit` bytes of m, or nil if m is not a valid `submit`.
func (m Submit) Encode() []byte { return encode(&m) }

// DecodeSubmit decodes b as the `submit` layout under EN5; any failure wraps ErrMalformed.
func DecodeSubmit(b []byte) (Submit, error) { return decodeAs[Submit](b, 0) }

// ImportItem is `import-item` (6).
type ImportItem struct{ Bundle, DeviceBundle, CPEvidence []byte }

func (m *ImportItem) spec() (string, []field) {
	return "import-item", []field{
		{"bundle", kBUpTo(32768).OrEmpty(), &m.Bundle},             // 3
		{"device_bundle", kBUpTo(8192).OrEmpty(), &m.DeviceBundle}, // 4
		{"cp_evidence", kBUpTo(1024).OrEmpty(), &m.CPEvidence},     // 5
	}
}

// Encode returns the canonical `import-item` bytes of m, or nil if m is not a valid `import-item`.
func (m ImportItem) Encode() []byte { return encode(&m) }

// DecodeImportItem decodes b as the `import-item` layout under EN5; any failure wraps ErrMalformed.
func DecodeImportItem(b []byte) (ImportItem, error) { return decodeAs[ImportItem](b, 0) }

// ImportBatch is `import-batch` (4).
type ImportBatch struct{ Items [][]byte }

func (m *ImportBatch) spec() (string, []field) {
	return "import-batch", []field{
		{"items", kL(kBUpTo(56320), 1, 64), &m.Items}, // 3
	}
}

// Encode returns the canonical `import-batch` bytes of m, or nil if m is not a valid `import-batch`.
func (m ImportBatch) Encode() []byte { return encode(&m) }

// DecodeImportBatch decodes b as the `import-batch` layout under EN5; any failure wraps ErrMalformed.
func DecodeImportBatch(b []byte) (ImportBatch, error) { return decodeAs[ImportBatch](b, 0) }

// checkLegacyRow is legacy-batch's row syntax "<uuid>.<b64 48-byte digest>".
func checkLegacyRow(s string) error {
	id, digest, ok := strings.Cut(s, ".")
	if !ok {
		return malformed("legacy row")
	}
	if _, ok := canonicalUUID(id); !ok {
		return malformed("legacy row id")
	}
	if d, err := decodeB64(digest); err != nil || len(d) != 48 {
		return malformed("legacy row digest")
	}
	return nil
}

// LegacyBatch is `legacy-batch` (4).
type LegacyBatch struct{ Rows []string }

func (m *LegacyBatch) spec() (string, []field) {
	return "legacy-batch", []field{
		{"rows", kA(kStr(101).withStr(checkLegacyRow), 1, 8192), &m.Rows}, // 3
	}
}

// Encode returns the canonical `legacy-batch` bytes of m, or nil if m is not a valid `legacy-batch`.
func (m LegacyBatch) Encode() []byte { return encode(&m) }

// DecodeLegacyBatch decodes b as the `legacy-batch` layout under EN5; any failure wraps ErrMalformed.
func DecodeLegacyBatch(b []byte) (LegacyBatch, error) { return decodeAs[LegacyBatch](b, 0) }

// Committed is `committed` (7).
type Committed struct{ Result, ResultSig, DeviceResult, DeviceResultSig []byte }

func (m *Committed) spec() (string, []field) {
	return "committed", []field{
		{"result", kBUpTo(4096), &m.Result},                         // 3
		{"result_sig", kB(96), &m.ResultSig},                        // 4
		{"device_result", kBUpTo(1024).OrEmpty(), &m.DeviceResult},  // 5
		{"device_result_sig", kB(96).OrEmpty(), &m.DeviceResultSig}, // 6
	}
}

// Encode returns the canonical `committed` bytes of m, or nil if m is not a valid `committed`.
func (m Committed) Encode() []byte { return encode(&m) }

// DecodeCommitted decodes b as the `committed` layout under EN5; any failure wraps ErrMalformed.
func DecodeCommitted(b []byte) (Committed, error) { return decodeAs[Committed](b, 0) }

// Refused is `refused` (5).
type Refused struct{ RequestID, Code string }

func (m *Refused) spec() (string, []field) {
	return "refused", []field{
		{"request_id", kS36v4, &m.RequestID},   // 3
		{"code", kE(refusalCodes...), &m.Code}, // 4
	}
}

// Encode returns the canonical `refused` bytes of m, or nil if m is not a valid `refused`.
func (m Refused) Encode() []byte { return encode(&m) }

// DecodeRefused decodes b as the `refused` layout under EN5; any failure wraps ErrMalformed.
func DecodeRefused(b []byte) (Refused, error) { return decodeAs[Refused](b, 0) }

// ErrorEnvelope is `error` (4).
type ErrorEnvelope struct{ Code string }

func (m *ErrorEnvelope) spec() (string, []field) {
	return "error", []field{
		{"code", kE(errorCodes...), &m.Code}, // 3
	}
}

// Encode returns the canonical `error` bytes of m, or nil if m is not a valid `error`.
func (m ErrorEnvelope) Encode() []byte { return encode(&m) }

// DecodeErrorEnvelope decodes b as the `error` layout under EN5; any failure wraps ErrMalformed.
func DecodeErrorEnvelope(b []byte) (ErrorEnvelope, error) { return decodeAs[ErrorEnvelope](b, 0) }

// Head is `head` (6): a fresh status with its co-signed device-status (D-241).
type Head struct{ Status, LatestBundle, DeviceStatus []byte }

func (m *Head) spec() (string, []field) {
	return "head", []field{
		{"status", kBUpTo(6144), &m.Status},                         // 3
		{"latest_bundle", kBUpTo(32768).OrEmpty(), &m.LatestBundle}, // 4
		{"device_status", kBUpTo(2048), &m.DeviceStatus},            // 5
	}
}

// Encode returns the canonical `head` bytes of m, or nil if m is not a valid `head`.
func (m Head) Encode() []byte { return encode(&m) }

// DecodeHead decodes b as the `head` layout under EN5; any failure wraps ErrMalformed.
func DecodeHead(b []byte) (Head, error) { return decodeAs[Head](b, 0) }

// StatusReq is `status-req` (4).
type StatusReq struct{ AccountIDs []string }

func (m *StatusReq) spec() (string, []field) {
	return "status-req", []field{
		{"account_ids", kA(kS36, 1, StatusBatch), &m.AccountIDs}, // 3
	}
}

// Encode returns the canonical `status-req` bytes of m, or nil if m is not a valid `status-req`.
func (m StatusReq) Encode() []byte { return encode(&m) }

// DecodeStatusReq decodes b as the `status-req` layout under EN5; any failure wraps ErrMalformed.
func DecodeStatusReq(b []byte) (StatusReq, error) { return decodeAs[StatusReq](b, 0) }

// HoldReq is `hold-req` (4) (RT6-7, D-287).
type HoldReq struct{ Bundles [][]byte }

func (m *HoldReq) spec() (string, []field) {
	return "hold-req", []field{
		{"bundles", kL(kBUpTo(32768), 1, 16), &m.Bundles}, // 3
	}
}

// Encode returns the canonical `hold-req` bytes of m, or nil if m is not a valid `hold-req`.
func (m HoldReq) Encode() []byte { return encode(&m) }

// DecodeHoldReq decodes b as the `hold-req` layout under EN5; any failure wraps ErrMalformed.
func DecodeHoldReq(b []byte) (HoldReq, error) { return decodeAs[HoldReq](b, 0) }

// PendingHeads is `pending-heads` (5), unsigned (D-179).
type PendingHeads struct {
	AccountIDs []string
	Next       string
}

func (m *PendingHeads) spec() (string, []field) {
	return "pending-heads", []field{
		{"account_ids", kA(kS36, 0, StatusBatch), &m.AccountIDs}, // 3
		{"next", kS36.OrEmpty(), &m.Next},                        // 4
	}
}

// Encode returns the canonical `pending-heads` bytes of m, or nil if m is not a valid `pending-heads`.
func (m PendingHeads) Encode() []byte { return encode(&m) }

// DecodePendingHeads decodes b as the `pending-heads` layout under EN5; any failure wraps ErrMalformed.
func DecodePendingHeads(b []byte) (PendingHeads, error) { return decodeAs[PendingHeads](b, 0) }

// Signer is `signer` (26): dimension-free infrastructure state (LD-2, LD-25).
type Signer struct {
	RealmID                                                                                          string
	ProductRootGen, DelegationSerial, RestoreEpoch, Reconciled, MaxCommitID, HeldCount, LegacySealed uint64
	PopulationDigest                                                                                 []byte
	TickAgeS, CancelUnavailableMs, ExtensionCapMs, NCancelMs, VMs                                    uint64
	Role                                                                                             string
	RSKDigest                                                                                        []byte
	DiskReady, ReplicationRequired, ReplicationReady, ArchiveOK, ClockSynced                         uint64
	SubmitLayouts                                                                                    []string
	Trust                                                                                            []byte
}

func (m *Signer) spec() (string, []field) {
	return "signer", []field{
		{"realm_id", kS36, &m.RealmID},                               // 3
		{"product_root_gen", kU, &m.ProductRootGen},                  // 4
		{"delegation_serial", kU1, &m.DelegationSerial},              // 5
		{"restore_epoch", kU, &m.RestoreEpoch},                       // 6
		{"reconciled", kU01, &m.Reconciled},                          // 7
		{"max_commit_id", kU, &m.MaxCommitID},                        // 8
		{"held_count", kU, &m.HeldCount},                             // 9
		{"legacy_sealed", kU01, &m.LegacySealed},                     // 10
		{"population_digest", kB(48).OrEmpty(), &m.PopulationDigest}, // 11
		{"tick_age_s", kU, &m.TickAgeS},                              // 12
		{"cancel_unavailable_ms", kU, &m.CancelUnavailableMs},        // 13
		{"extension_cap_ms", kU, &m.ExtensionCapMs},                  // 14
		{"n_cancel_ms", kU, &m.NCancelMs},                            // 15
		{"v_ms", kU, &m.VMs},                                         // 16
		{"role", kE(nodeRoles...), &m.Role},                          // 17
		{"rsk_digest", kB(48), &m.RSKDigest},                         // 18
		{"disk_ready", kU01, &m.DiskReady},                           // 19
		{"replication_required", kU01, &m.ReplicationRequired},       // 20
		{"replication_ready", kU01, &m.ReplicationReady},             // 21
		{"archive_ok", kU01, &m.ArchiveOK},                           // 22
		{"clock_synced", kU01, &m.ClockSynced},                       // 23
		{"submit_layouts", kA(kStr(16), 1, 8), &m.SubmitLayouts},     // 24
		{"trust", kBUpTo(CapTrust), &m.Trust},                        // 25
	}
}

// Encode returns the canonical `signer` bytes of m, or nil if m is not a valid `signer`.
func (m Signer) Encode() []byte { return encode(&m) }

// DecodeSigner decodes b as the `signer` layout under EN5; any failure wraps ErrMalformed.
func DecodeSigner(b []byte) (Signer, error) { return decodeAs[Signer](b, 0) }

// DeviceSubmit is `device-submit` (13), the body of POST /v1/device-transactions.
type DeviceSubmit struct {
	DeviceIntent, SigRoot, SigActor, SigNewDevice, RecoveryBind, SigRecovery, SigRecoveryPrior,
	CPAuthz, CPAuthzSig, CPEvidence []byte
}

func (m *DeviceSubmit) spec() (string, []field) {
	return "device-submit", []field{
		{"device_intent", kBUpTo(2048).OrEmpty(), &m.DeviceIntent},    // 3
		{"sig_root", kB(96).OrEmpty(), &m.SigRoot},                    // 4
		{"sig_actor", kB(96).OrEmpty(), &m.SigActor},                  // 5
		{"sig_new_device", kB(96).OrEmpty(), &m.SigNewDevice},         // 6
		{"recovery_bind", kBUpTo(1024).OrEmpty(), &m.RecoveryBind},    // 7
		{"sig_recovery", kB(96).OrEmpty(), &m.SigRecovery},            // 8
		{"sig_recovery_prior", kB(96).OrEmpty(), &m.SigRecoveryPrior}, // 9
		{"cp_authz", kBUpTo(1024), &m.CPAuthz},                        // 10
		{"cp_authz_sig", kB(96), &m.CPAuthzSig},                       // 11
		{"cp_evidence", kBUpTo(1024), &m.CPEvidence},                  // 12
	}
}

// Encode returns the canonical `device-submit` bytes of m, or nil if m is not a valid `device-submit`.
func (m DeviceSubmit) Encode() []byte { return encode(&m) }

// DecodeDeviceSubmit decodes b as the `device-submit` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceSubmit(b []byte) (DeviceSubmit, error) { return decodeAs[DeviceSubmit](b, 0) }

// DeviceCommitted is `device-committed` (4).
type DeviceCommitted struct{ DeviceBundle []byte }

func (m *DeviceCommitted) spec() (string, []field) {
	return "device-committed", []field{
		{"device_bundle", kBUpTo(8192), &m.DeviceBundle}, // 3
	}
}

// Encode returns the canonical `device-committed` bytes of m, or nil if m is not a valid `device-committed`.
func (m DeviceCommitted) Encode() []byte { return encode(&m) }

// DecodeDeviceCommitted decodes b as the `device-committed` layout under EN5; any failure wraps ErrMalformed.
func DecodeDeviceCommitted(b []byte) (DeviceCommitted, error) { return decodeAs[DeviceCommitted](b, 0) }
