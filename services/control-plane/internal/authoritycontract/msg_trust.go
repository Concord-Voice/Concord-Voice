package authoritycontract

import "bytes"

func init() {
	register(
		func() Message { return new(Delegation) }, func() Message { return new(Succession) },
		func() Message { return new(Anchors) }, func() Message { return new(Signed) },
		func() Message { return new(Statuses) }, func() Message { return new(TrustEnvelope) },
	)
}

// checkVersion is accepted_versions' item syntax [1-9][0-9]{0,3}.
func checkVersion(s string) error {
	if len(s) == 0 || len(s) > 4 || s[0] < '1' || s[0] > '9' {
		return malformed("version")
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return malformed("version")
		}
	}
	return nil
}

// Delegation is `delegation` (20), dual-signed by primaries A and B (LD-3).
type Delegation struct {
	RealmID                                                                       string
	Serial, MinValidSerial, ProductRootGen                                        uint64
	RSKPublicKeys, CAKPublicKeys, OperatorPublicKeys, AuthorityTLSKeys, CPTLSPins [][]byte
	NotBeforeMs, NotAfterMs                                                       uint64
	Scope, AcceptedVersions                                                       []string
	TauMs, DeltaMs, GMs, CutoverAtMs                                              uint64
}

func (m *Delegation) spec() (string, []field) {
	return "delegation", []field{
		{"realm_id", kS36, &m.RealmID},                                                      // 3
		{"serial", kU1, &m.Serial},                                                          // 4
		{"min_valid_serial", kU, &m.MinValidSerial},                                         // 5
		{"product_root_gen", kU, &m.ProductRootGen},                                         // 6
		{"rsk_public_keys", kA(kP97, 1, MaxRSKs), &m.RSKPublicKeys},                         // 7
		{"cak_public_keys", kA(kP97, 1, MaxCAKs), &m.CAKPublicKeys},                         // 8
		{"operator_public_keys", kA(kP97, 0, MaxOperatorKeys), &m.OperatorPublicKeys},       // 9
		{"authority_tls_keys", kA(kP97, 1, MaxAuthorityTLSKeys), &m.AuthorityTLSKeys},       // 10
		{"cp_tls_pins", kA(kB(48), 1, MaxCPTLSPins), &m.CPTLSPins},                          // 11
		{"not_before_ms", kU, &m.NotBeforeMs},                                               // 12
		{"not_after_ms", kU, &m.NotAfterMs},                                                 // 13
		{"scope", kA(kE(scopeValues...), 1, 15), &m.Scope},                                  // 14
		{"accepted_versions", kA(kStr(4).withStr(checkVersion), 1, 8), &m.AcceptedVersions}, // 15
		{"tau_ms", kU, &m.TauMs},                                                            // 16
		{"delta_ms", kU, &m.DeltaMs},                                                        // 17
		{"g_ms", kU, &m.GMs},                                                                // 18
		{"cutover_at_ms", kU, &m.CutoverAtMs},                                               // 19
	}
}

// Encode returns the canonical `delegation` bytes of m, or nil if m is not a valid `delegation`.
func (m Delegation) Encode() []byte { return encode(&m) }

// DecodeDelegation decodes b as the `delegation` layout under EN5; any failure wraps ErrMalformed.
func DecodeDelegation(b []byte) (Delegation, error) { return decodeAs[Delegation](b, 0) }

// Succession is `succession` (10), dual-signed by recovery A′ and B′.
type Succession struct {
	RealmID                                              string
	PrevRootGen, NewRootGen                              uint64
	NewPrimaryA, NewPrimaryB, NewRecoveryA, NewRecoveryB []byte
}

func (m *Succession) spec() (string, []field) {
	return "succession", []field{
		{"realm_id", kS36, &m.RealmID},            // 3
		{"prev_root_gen", kU, &m.PrevRootGen},     // 4
		{"new_root_gen", kU, &m.NewRootGen},       // 5
		{"new_primary_a", kP97, &m.NewPrimaryA},   // 6
		{"new_primary_b", kP97, &m.NewPrimaryB},   // 7
		{"new_recovery_a", kP97, &m.NewRecoveryA}, // 8
		{"new_recovery_b", kP97, &m.NewRecoveryB}, // 9
	}
}

// Encode returns the canonical `succession` bytes of m, or nil if m is not a valid `succession`.
func (m Succession) Encode() []byte { return encode(&m) }

// DecodeSuccession decodes b as the `succession` layout under EN5; any failure wraps ErrMalformed.
func DecodeSuccession(b []byte) (Succession, error) { return decodeAs[Succession](b, 0) }

// Anchors is `anchors` (8): unsigned, trusted by provenance (#3594).
type Anchors struct {
	RealmID                                  string
	PrimaryA, PrimaryB, RecoveryA, RecoveryB []byte
}

func (m *Anchors) spec() (string, []field) {
	return "anchors", []field{
		{"realm_id", kS36, &m.RealmID},     // 3
		{"primary_a", kP97, &m.PrimaryA},   // 4
		{"primary_b", kP97, &m.PrimaryB},   // 5
		{"recovery_a", kP97, &m.RecoveryA}, // 6
		{"recovery_b", kP97, &m.RecoveryB}, // 7
	}
}

// Encode returns the canonical `anchors` bytes of m, or nil if m is not a valid `anchors`.
func (m Anchors) Encode() []byte { return encode(&m) }

// DecodeAnchors decodes b as the `anchors` layout under EN5; any failure wraps ErrMalformed.
func DecodeAnchors(b []byte) (Anchors, error) { return decodeAs[Anchors](b, 0) }

// Signed is the `signed` envelope (7). Both signatures are over msg.
type Signed struct {
	Type       string
	Msg        []byte
	SigA, SigB []byte
}

func (m *Signed) spec() (string, []field) {
	return "signed", []field{
		{"type", kE(signedTypes...), &m.Type}, // 3
		{"msg", kBUpTo(4096), &m.Msg},         // 4
		{"sig_a", kB(96), &m.SigA},            // 5
		{"sig_b", kB(96).OrEmpty(), &m.SigB},  // 6
	}
}

// crossCheck applies §2.6's decode-time rules. type = msg[2] is read from the
// canonical prefix bytes of msg; msg itself is decoded by PR-A2's verifiers.
func (m *Signed) crossCheck() error {
	if !bytes.HasPrefix(m.Msg, []byte(encodedPrefix+`"`+m.Type+`",`)) {
		return malformed("signed type is not msg[2]")
	}
	dual := m.Type == "delegation" || m.Type == "succession"
	if dual && len(m.SigB) == 0 {
		return malformed("missing second signature")
	}
	if !dual && len(m.SigB) != 0 {
		return malformed("unexpected second signature")
	}
	return nil
}

// Encode returns the canonical `signed` bytes of m, or nil if m is not a valid `signed`.
func (m Signed) Encode() []byte { return encode(&m) }

// DecodeSigned decodes b as the `signed` layout under EN5; any failure wraps ErrMalformed.
func DecodeSigned(b []byte) (Signed, error) { return decodeAs[Signed](b, 0) }

// Statuses is `statuses` (4): signed statuses in request order.
type Statuses struct{ Items [][]byte }

func (m *Statuses) spec() (string, []field) {
	return "statuses", []field{
		{"items", kL(kBUpTo(6144), 0, StatusBatch), &m.Items}, // 3
	}
}

// Encode returns the canonical `statuses` bytes of m, or nil if m is not a valid `statuses`.
func (m Statuses) Encode() []byte { return encode(&m) }

// DecodeStatuses decodes b as the `statuses` layout under EN5; any failure wraps ErrMalformed.
func DecodeStatuses(b []byte) (Statuses, error) { return decodeAs[Statuses](b, 0) }

// TrustEnvelope is the `trust` envelope (4): every succession and delegation
// the realm has issued. Its ordering rules are PR-A2's (they need a nested decode).
type TrustEnvelope struct{ Items [][]byte }

func (m *TrustEnvelope) spec() (string, []field) {
	return "trust", []field{
		{"items", kL(kBUpTo(6144), 1, TrustItems), &m.Items}, // 3
	}
}

// Encode returns the canonical `trust` bytes of m, or nil if m is not a valid `trust`.
func (m TrustEnvelope) Encode() []byte { return encode(&m) }

// DecodeTrustEnvelope decodes b as the `trust` layout under EN5; any failure wraps ErrMalformed.
func DecodeTrustEnvelope(b []byte) (TrustEnvelope, error) { return decodeAs[TrustEnvelope](b, 0) }
