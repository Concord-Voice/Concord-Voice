package authoritycontract

func init() {
	register(
		func() Message { return new(Bundle) }, func() Message { return new(History) },
		func() Message { return new(Relay) }, func() Message { return new(QueryReq) },
		func() Message { return new(QueryResp) },
	)
}

// Bundle is the published `bundle` (19). Nested messages are opaque here.
type Bundle struct {
	Intent, SigCandidate, SigCurrentRoot, SigOperator, SigRecovery, LegacyPopSig, LegacySPKI,
	EKBinding, EKBindingSig, RecoveryBind, RecoveryBindSig, CPAuthz, CPAuthzSig, Result, ResultSig, HeldStatus []byte
}

func (m *Bundle) spec() (string, []field) {
	return "bundle", []field{
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
		{"cp_authz_sig", kB(96).OrEmpty(), &m.CPAuthzSig},           // 15
		{"result", kBUpTo(4096), &m.Result},                         // 16
		{"result_sig", kB(96), &m.ResultSig},                        // 17
		{"held_status", kBUpTo(6144).OrEmpty(), &m.HeldStatus},      // 18
	}
}

// Encode returns the canonical `bundle` bytes of m, or nil if m is not a valid `bundle`.
func (m Bundle) Encode() []byte { return encode(&m) }

// DecodeBundle decodes b as the `bundle` layout under EN5; any failure wraps ErrMalformed.
func DecodeBundle(b []byte) (Bundle, error) { return decodeAs[Bundle](b, 0) }

// History is `history` (5).
type History struct {
	AccountID string
	Bundles   [][]byte
}

func (m *History) spec() (string, []field) {
	return "history", []field{
		{"account_id", kS36, &m.AccountID},                         // 3
		{"bundles", kL(kBUpTo(32768), 0, HistoryPage), &m.Bundles}, // 4
	}
}

// Encode returns the canonical `history` bytes of m, or nil if m is not a valid `history`.
func (m History) Encode() []byte { return encode(&m) }

// DecodeHistory decodes b as the `history` layout under EN5; any failure wraps ErrMalformed.
func DecodeHistory(b []byte) (History, error) { return decodeAs[History](b, 0) }

// Relay is `relay` (7): a root or PoP cancel_pending.
type Relay struct{ Intent, SigCurrentRoot, LegacyPopSig, LegacySPKI []byte }

func (m *Relay) spec() (string, []field) {
	return "relay", []field{
		{"intent", kBUpTo(2048), &m.Intent},                       // 3
		{"sig_current_root", kB(96).OrEmpty(), &m.SigCurrentRoot}, // 4
		{"legacy_pop_sig", kB(512).OrEmpty(), &m.LegacyPopSig},    // 5
		{"legacy_spki", kEKSPKI.OrEmpty(), &m.LegacySPKI},         // 6
	}
}

// Encode returns the canonical `relay` bytes of m, or nil if m is not a valid `relay`.
func (m Relay) Encode() []byte { return encode(&m) }

// DecodeRelay decodes b as the `relay` layout under EN5; any failure wraps ErrMalformed.
func DecodeRelay(b []byte) (Relay, error) { return decodeAs[Relay](b, 0) }

// QueryReq is `query-req` (6).
type QueryReq struct{ Query, QuerySig, LegacySPKI []byte }

func (m *QueryReq) spec() (string, []field) {
	return "query-req", []field{
		{"query", kBUpTo(1024), &m.Query},                 // 3
		{"query_sig", kBOneOf(96, 512), &m.QuerySig},      // 4
		{"legacy_spki", kEKSPKI.OrEmpty(), &m.LegacySPKI}, // 5
	}
}

// Encode returns the canonical `query-req` bytes of m, or nil if m is not a valid `query-req`.
func (m QueryReq) Encode() []byte { return encode(&m) }

// DecodeQueryReq decodes b as the `query-req` layout under EN5; any failure wraps ErrMalformed.
func DecodeQueryReq(b []byte) (QueryReq, error) { return decodeAs[QueryReq](b, 0) }

// QueryResp is `query-resp` (7). The zero value encodes the failure form.
type QueryResp struct {
	Status        []byte
	Bundles       [][]byte
	DeviceStatus  []byte
	DeviceBundles [][]byte
}

// QueryRespFailure is the single byte-identical answer to every failed query.
// Callers must not modify it; QueryResp{}.Encode() returns a fresh copy.
var QueryRespFailure = []byte(`["` + Prefix + `",1,"query-resp","",[],"",[]]`)

func (m *QueryResp) spec() (string, []field) {
	return "query-resp", []field{
		{"status", kBUpTo(6144).OrEmpty(), &m.Status},                         // 3
		{"bundles", kL(kBUpTo(32768), 0, QueryRespBundles), &m.Bundles},       // 4
		{"device_status", kBUpTo(2048).OrEmpty(), &m.DeviceStatus},            // 5
		{"device_bundles", kL(kBUpTo(8192), 0, DevicePage), &m.DeviceBundles}, // 6
	}
}

// Encode returns the canonical `query-resp` bytes of m, or nil if m is not a valid `query-resp`.
func (m QueryResp) Encode() []byte { return encode(&m) }

// DecodeQueryResp decodes b as the `query-resp` layout under EN5; any failure wraps ErrMalformed.
func DecodeQueryResp(b []byte) (QueryResp, error) { return decodeAs[QueryResp](b, 0) }
