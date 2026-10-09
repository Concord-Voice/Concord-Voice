package authoritycontract

func init() {
	register(
		func() Message { return new(Intent) }, func() Message { return new(Sig) },
		func() Message { return new(LegacyEKPoP) }, func() Message { return new(EKBinding) },
		func() Message { return new(CPAuthz) }, func() Message { return new(CPEvidence) },
		func() Message { return new(Result) }, func() Message { return new(Status) },
		func() Message { return new(Query) },
	)
}

// Intent is `intent` (17), covered by its digest.
type Intent struct {
	RealmID, AccountID, RequestID, Op             string
	ExpiresAtMs, ExpectedSeq, ExpectedRecoveryGen uint64
	ExpectedHeadDigest, PredecessorRootDigest, CandidateRoot, EKBindingDigest,
	LegacyEKDigest, DeviceIntentDigest, RecoveryBindDigest []byte
}

func (m *Intent) spec() (string, []field) {
	return "intent", []field{
		{"realm_id", kS36, &m.RealmID},                                          // 3
		{"account_id", kS36, &m.AccountID},                                      // 4
		{"request_id", kS36v4, &m.RequestID},                                    // 5
		{"op", kE(publishedOps...), &m.Op},                                      // 6
		{"expires_at_ms", kU1, &m.ExpiresAtMs},                                  // 7
		{"expected_seq", kU, &m.ExpectedSeq},                                    // 8
		{"expected_recovery_gen", kU, &m.ExpectedRecoveryGen},                   // 9
		{"expected_head_digest", kB(48).OrEmpty(), &m.ExpectedHeadDigest},       // 10
		{"predecessor_root_digest", kB(48).OrEmpty(), &m.PredecessorRootDigest}, // 11
		{"candidate_root", kP97.OrEmpty(), &m.CandidateRoot},                    // 12
		{"ek_binding_digest", kB(48).OrEmpty(), &m.EKBindingDigest},             // 13
		{"legacy_ek_digest", kB(48).OrEmpty(), &m.LegacyEKDigest},               // 14
		{"device_intent_digest", kB(48).OrEmpty(), &m.DeviceIntentDigest},       // 15
		{"recovery_bind_digest", kB(48).OrEmpty(), &m.RecoveryBindDigest},       // 16
	}
}

// Encode returns the canonical `intent` bytes of m, or nil if m is not a valid `intent`.
func (m Intent) Encode() []byte { return encode(&m) }

// DecodeIntent decodes b as the `intent` layout under EN5; any failure wraps ErrMalformed.
func DecodeIntent(b []byte) (Intent, error) { return decodeAs[Intent](b, 0) }

// Sig is `sig` (5). Envelopes carry only its signature; verifiers rebuild it.
type Sig struct {
	Role   string
	Digest []byte
}

func (m *Sig) spec() (string, []field) {
	return "sig", []field{
		{"role", kE(sigRoles...), &m.Role}, // 3
		{"digest", kB(48), &m.Digest},      // 4
	}
}

// Encode returns the canonical `sig` bytes of m, or nil if m is not a valid `sig`.
func (m Sig) Encode() []byte { return encode(&m) }

// DecodeSig decodes b as the `sig` layout under EN5; any failure wraps ErrMalformed.
func DecodeSig(b []byte) (Sig, error) { return decodeAs[Sig](b, 0) }

// LegacyEKPoP is `legacy-ek-pop` (7), signed by the legacy key (PSS).
type LegacyEKPoP struct {
	RealmID, AccountID           string
	IntentDigest, LegacyEKDigest []byte
}

func (m *LegacyEKPoP) spec() (string, []field) {
	return "legacy-ek-pop", []field{
		{"realm_id", kS36, &m.RealmID},                  // 3
		{"account_id", kS36, &m.AccountID},              // 4
		{"intent_digest", kB(48), &m.IntentDigest},      // 5
		{"legacy_ek_digest", kB(48), &m.LegacyEKDigest}, // 6
	}
}

// Encode returns the canonical `legacy-ek-pop` bytes of m, or nil if m is not a valid `legacy-ek-pop`.
func (m LegacyEKPoP) Encode() []byte { return encode(&m) }

// DecodeLegacyEKPoP decodes b as the `legacy-ek-pop` layout under EN5; any failure wraps ErrMalformed.
func DecodeLegacyEKPoP(b []byte) (LegacyEKPoP, error) { return decodeAs[LegacyEKPoP](b, 0) }

// EKBinding is `ek-binding` (9), signed by the root it names.
type EKBinding struct {
	RealmID, AccountID string
	RootDigest         []byte
	EKGeneration       uint64
	EKAlg              string
	EKPublicKey        []byte
}

func (m *EKBinding) spec() (string, []field) {
	return "ek-binding", []field{
		{"realm_id", kS36, &m.RealmID},             // 3
		{"account_id", kS36, &m.AccountID},         // 4
		{"root_digest", kB(48), &m.RootDigest},     // 5
		{"ek_generation", kU1, &m.EKGeneration},    // 6
		{"ek_alg", kE(ekAlgs...), &m.EKAlg},        // 7
		{"ek_public_key", kEKSPKI, &m.EKPublicKey}, // 8
	}
}

// Encode returns the canonical `ek-binding` bytes of m, or nil if m is not a valid `ek-binding`.
func (m EKBinding) Encode() []byte { return encode(&m) }

// DecodeEKBinding decodes b as the `ek-binding` layout under EN5; any failure wraps ErrMalformed.
func DecodeEKBinding(b []byte) (EKBinding, error) { return decodeAs[EKBinding](b, 0) }

// CPAuthz is `cp-authz` (10), CAK-signed except for actor restore (row 22).
type CPAuthz struct {
	RealmID, AccountID, RequestID string
	IntentDigest                  []byte
	Actor                         string
	AssertedAtMs                  uint64
	EvidenceDigest                []byte
}

func (m *CPAuthz) spec() (string, []field) {
	return "cp-authz", []field{
		{"realm_id", kS36, &m.RealmID},                 // 3
		{"account_id", kS36, &m.AccountID},             // 4
		{"request_id", kS36v4, &m.RequestID},           // 5
		{"intent_digest", kB(48), &m.IntentDigest},     // 6
		{"actor", kE(actorValues...), &m.Actor},        // 7
		{"asserted_at_ms", kU1, &m.AssertedAtMs},       // 8
		{"evidence_digest", kB(48), &m.EvidenceDigest}, // 9
	}
}

// Encode returns the canonical `cp-authz` bytes of m, or nil if m is not a valid `cp-authz`.
func (m CPAuthz) Encode() []byte { return encode(&m) }

// DecodeCPAuthz decodes b as the `cp-authz` layout under EN5; any failure wraps ErrMalformed.
func DecodeCPAuthz(b []byte) (CPAuthz, error) { return decodeAs[CPAuthz](b, 0) }

// CPEvidence is `cp-evidence` (11): private, never published, bound through
// cp-authz.evidence_digest.
type CPEvidence struct {
	RequestID                                                         string
	Factors                                                           []string
	MFAEnrolled, PasswordLogin, RecoveryWindowClear, OperatorAssisted uint64
	CredentialEpochDigest, EvidenceSalt                               []byte
}

func (m *CPEvidence) spec() (string, []field) {
	return "cp-evidence", []field{
		{"request_id", kS36v4, &m.RequestID},                                    // 3
		{"factors", kA(kE(factorNames...), 0, 14), &m.Factors},                  // 4
		{"mfa_enrolled", kU01, &m.MFAEnrolled},                                  // 5
		{"password_login", kU01, &m.PasswordLogin},                              // 6
		{"recovery_window_clear", kU01, &m.RecoveryWindowClear},                 // 7
		{"operator_assisted", kU01, &m.OperatorAssisted},                        // 8
		{"credential_epoch_digest", kB(48).OrEmpty(), &m.CredentialEpochDigest}, // 9
		{"evidence_salt", kB(32), &m.EvidenceSalt},                              // 10
	}
}

// Encode returns the canonical `cp-evidence` bytes of m, or nil if m is not a valid `cp-evidence`.
func (m CPEvidence) Encode() []byte { return encode(&m) }

// DecodeCPEvidence decodes b as the `cp-evidence` layout under EN5; any failure wraps ErrMalformed.
func DecodeCPEvidence(b []byte) (CPEvidence, error) { return decodeAs[CPEvidence](b, 0) }

// Result is `result` (27), RSK-signed; no device field (LD-32).
type Result struct {
	RealmID, AccountID, RequestID                                        string
	IntentDigest, CPAuthzDigest                                          []byte
	Op                                                                   string
	Seq                                                                  uint64
	State, PriorState                                                    string
	SuspendedBy                                                          []string
	ActiveRoot                                                           []byte
	RootRevoked                                                          uint64
	EKBindingDigest, PendingRoot, PendingEKBindingDigest, LegacyEKDigest []byte
	LegacyPinRevoked                                                     uint64
	PrevEntryDigest                                                      []byte
	RecoveryGen, ActivationNotBeforeMs                                   uint64
	Assurance                                                            string
	IssuedAtMs, DelegationGen, DelegationSerial                          uint64
}

func (m *Result) spec() (string, []field) {
	return "result", []field{
		{"realm_id", kS36, &m.RealmID},                                             // 3
		{"account_id", kS36, &m.AccountID},                                         // 4
		{"request_id", kS36v4, &m.RequestID},                                       // 5
		{"intent_digest", kB(48), &m.IntentDigest},                                 // 6
		{"cp_authz_digest", kB(48), &m.CPAuthzDigest},                              // 7
		{"op", kE(publishedOps...), &m.Op},                                         // 8
		{"seq", kU1, &m.Seq},                                                       // 9
		{"state", kE(stateValues...), &m.State},                                    // 10
		{"prior_state", kE(priorStateValues...), &m.PriorState},                    // 11
		{"suspended_by", kA(kE(suspenders...), 0, 2), &m.SuspendedBy},              // 12
		{"active_root", kP97.OrEmpty(), &m.ActiveRoot},                             // 13
		{"root_revoked", kU01, &m.RootRevoked},                                     // 14
		{"ek_binding_digest", kB(48).OrEmpty(), &m.EKBindingDigest},                // 15
		{"pending_root", kP97.OrEmpty(), &m.PendingRoot},                           // 16
		{"pending_ek_binding_digest", kB(48).OrEmpty(), &m.PendingEKBindingDigest}, // 17
		{"legacy_ek_digest", kB(48).OrEmpty(), &m.LegacyEKDigest},                  // 18
		{"legacy_pin_revoked", kU01, &m.LegacyPinRevoked},                          // 19
		{"prev_entry_digest", kB(48).OrEmpty(), &m.PrevEntryDigest},                // 20
		{"recovery_gen", kU, &m.RecoveryGen},                                       // 21
		{"activation_not_before_ms", kU, &m.ActivationNotBeforeMs},                 // 22
		{"assurance", kE(assuranceValues...), &m.Assurance},                        // 23
		{"issued_at_ms", kU, &m.IssuedAtMs},                                        // 24
		{"delegation_gen", kU, &m.DelegationGen},                                   // 25
		{"delegation_serial", kU1, &m.DelegationSerial},                            // 26
	}
}

// Encode returns the canonical `result` bytes of m, or nil if m is not a valid `result`.
func (m Result) Encode() []byte { return encode(&m) }

// DecodeResult decodes b as the `result` layout under EN5; any failure wraps ErrMalformed.
func DecodeResult(b []byte) (Result, error) { return decodeAs[Result](b, 0) }

// Status is `status` (23), RSK-signed inside `signed`; no device field (LD-32).
type Status struct {
	RealmID, AccountID                                                                                   string
	Seq                                                                                                  uint64
	HeadEntryDigest                                                                                      []byte
	State, PriorState                                                                                    string
	SuspendedBy                                                                                          []string
	ActiveRootDigest                                                                                     []byte
	RootRevoked                                                                                          uint64
	EKBindingDigest, PendingRootDigest, PendingEKBindingDigest, LegacyEKDigest                           []byte
	LegacyPinRevoked, ActivationNotBeforeMs, Held, AsOfMs, ValidUntilMs, DelegationGen, DelegationSerial uint64
}

func (m *Status) spec() (string, []field) {
	return "status", []field{
		{"realm_id", kS36, &m.RealmID},                                             // 3
		{"account_id", kS36, &m.AccountID},                                         // 4
		{"seq", kU, &m.Seq},                                                        // 5
		{"head_entry_digest", kB(48).OrEmpty(), &m.HeadEntryDigest},                // 6
		{"state", kE(stateValues...), &m.State},                                    // 7
		{"prior_state", kE(priorStateValues...), &m.PriorState},                    // 8
		{"suspended_by", kA(kE(suspenders...), 0, 2), &m.SuspendedBy},              // 9
		{"active_root_digest", kB(48).OrEmpty(), &m.ActiveRootDigest},              // 10
		{"root_revoked", kU01, &m.RootRevoked},                                     // 11
		{"ek_binding_digest", kB(48).OrEmpty(), &m.EKBindingDigest},                // 12
		{"pending_root_digest", kB(48).OrEmpty(), &m.PendingRootDigest},            // 13
		{"pending_ek_binding_digest", kB(48).OrEmpty(), &m.PendingEKBindingDigest}, // 14
		{"legacy_ek_digest", kB(48).OrEmpty(), &m.LegacyEKDigest},                  // 15
		{"legacy_pin_revoked", kU01, &m.LegacyPinRevoked},                          // 16
		{"activation_not_before_ms", kU, &m.ActivationNotBeforeMs},                 // 17
		{"held", kU01, &m.Held},                                                    // 18
		{"as_of_ms", kU, &m.AsOfMs},                                                // 19
		{"valid_until_ms", kU, &m.ValidUntilMs},                                    // 20
		{"delegation_gen", kU, &m.DelegationGen},                                   // 21
		{"delegation_serial", kU1, &m.DelegationSerial},                            // 22
	}
}

// Encode returns the canonical `status` bytes of m, or nil if m is not a valid `status`.
func (m Status) Encode() []byte { return encode(&m) }

// DecodeStatus decodes b as the `status` layout under EN5; any failure wraps ErrMalformed.
func DecodeStatus(b []byte) (Status, error) { return decodeAs[Status](b, 0) }

// Query is `query` (9).
type Query struct {
	RealmID, AccountID string
	SignerDigest       []byte
	IssuedAtMs         uint64
	Nonce              []byte
	AfterDseq          uint64
}

func (m *Query) spec() (string, []field) {
	return "query", []field{
		{"realm_id", kS36, &m.RealmID},             // 3
		{"account_id", kS36, &m.AccountID},         // 4
		{"signer_digest", kB(48), &m.SignerDigest}, // 5
		{"issued_at_ms", kU, &m.IssuedAtMs},        // 6
		{"nonce", kB(16), &m.Nonce},                // 7
		{"after_dseq", kU, &m.AfterDseq},           // 8
	}
}

// Encode returns the canonical `query` bytes of m, or nil if m is not a valid `query`.
func (m Query) Encode() []byte { return encode(&m) }

// DecodeQuery decodes b as the `query` layout under EN5; any failure wraps ErrMalformed.
func DecodeQuery(b []byte) (Query, error) { return decodeAs[Query](b, 0) }
