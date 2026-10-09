package authoritycontract

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func sampleIntent(t testing.TB) Intent {
	return Intent{RealmID: newID(), AccountID: newID(), RequestID: newID(), Op: OpRotateRoot,
		ExpiresAtMs: 1_700_000_660_000, ExpectedSeq: 4, ExpectedRecoveryGen: 1, ExpectedHeadDigest: digestOf("h"),
		PredecessorRootDigest: digestOf("p"), CandidateRoot: testPoint(t), EKBindingDigest: digestOf("e"),
		DeviceIntentDigest: digestOf("d")}
}

func sampleCPEvidence() CPEvidence {
	return CPEvidence{RequestID: newID(), Factors: []string{FactorTOTP, FactorPassword}, MFAEnrolled: 1,
		PasswordLogin: 1, RecoveryWindowClear: 1, CredentialEpochDigest: CredentialEpochDigest("e1", true), EvidenceSalt: rnd(32)}
}

func sampleResult(t testing.TB) Result {
	return Result{RealmID: newID(), AccountID: newID(), RequestID: newID(), IntentDigest: digestOf("i"),
		CPAuthzDigest: digestOf("c"), Op: OpSuspend, Seq: 5, State: StateSuspended, SuspendedBy: []string{ActorUser, ActorOperator},
		ActiveRoot: testPoint(t), EKBindingDigest: digestOf("e"), LegacyEKDigest: digestOf("l"), PrevEntryDigest: digestOf("p"),
		RecoveryGen: 1, Assurance: "", IssuedAtMs: 1_700_000_000_000, DelegationGen: 0, DelegationSerial: 3}
}

func sampleStatus() Status {
	return Status{RealmID: newID(), AccountID: newID(), Seq: 5, HeadEntryDigest: digestOf("h"), State: StatePending,
		PriorState: StateActive, ActiveRootDigest: digestOf("r"), EKBindingDigest: digestOf("e"),
		PendingRootDigest: digestOf("pr"), PendingEKBindingDigest: digestOf("pe"), ActivationNotBeforeMs: ANBNever,
		Held: 1, AsOfMs: 1_700_000_000_000, ValidUntilMs: 1_700_259_200_000, DelegationSerial: 1}
}

func TestPublishedRoundTrip(t *testing.T) {
	roundTrip(t, sampleIntent(t))
	roundTrip(t, Sig{Role: RoleCurrentRoot, Digest: digestOf("i")})
	roundTrip(t, LegacyEKPoP{RealmID: newID(), AccountID: newID(), IntentDigest: digestOf("i"), LegacyEKDigest: digestOf("l")})
	roundTrip(t, EKBinding{RealmID: newID(), AccountID: newID(), RootDigest: digestOf("r"), EKGeneration: 1,
		EKAlg: "rsa-oaep-4096-sha256", EKPublicKey: testEKSPKI(t)})
	roundTrip(t, CPAuthz{RealmID: newID(), AccountID: newID(), RequestID: newID(), IntentDigest: digestOf("i"),
		Actor: ActorUser, AssertedAtMs: 1_700_000_000_000, EvidenceDigest: digestOf("ev")})
	roundTrip(t, sampleCPEvidence())
	roundTrip(t, sampleResult(t))
	roundTrip(t, sampleStatus())
	roundTrip(t, Query{RealmID: newID(), AccountID: newID(), SignerDigest: digestOf("s"), IssuedAtMs: 1, Nonce: rnd(16), AfterDseq: 0})
}

func TestIntentTypeColumn(t *testing.T) {
	for name, mut := range map[string]func(*Intent){
		"private op":       func(m *Intent) { m.Op = OpAddDevice },
		"v1 request_id":    func(m *Intent) { m.RequestID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8" },
		"expires_at_ms 0":  func(m *Intent) { m.ExpiresAtMs = 0 },
		"hybrid candidate": func(m *Intent) { m.CandidateRoot = hybridOf(m.CandidateRoot) },
		"short digest":     func(m *Intent) { m.ExpectedHeadDigest = m.ExpectedHeadDigest[:47] },
		"uppercase realm":  func(m *Intent) { m.RealmID = strings.ToUpper(m.RealmID) },
	} {
		m := sampleIntent(t)
		mut(&m)
		require.Nil(t, m.Encode(), name)
	}
}

func TestCPEvidenceFactorVocabulary(t *testing.T) {
	all := sampleCPEvidence()
	all.Factors = append([]string(nil), factorNames...)
	roundTrip(t, all) // the 14-name vocabulary at its cap (LD-41)

	bad := sampleCPEvidence()
	bad.Factors = []string{"backup-email-recovery"}
	require.Nil(t, bad.Encode(), "LD-33 dropped backup-email-recovery")

	b := sampleCPEvidence().Encode()
	dup := strings.Replace(string(b), `["password","totp"]`, `["password","password"]`, 1)
	_, err := DecodeCPEvidence([]byte(dup))
	require.ErrorIs(t, err, ErrMalformed, "a duplicate name is malformed at EN5")
	unsorted := strings.Replace(string(b), `["password","totp"]`, `["totp","password"]`, 1)
	_, err = DecodeCPEvidence([]byte(unsorted))
	require.ErrorIs(t, err, ErrMalformed)
}

func TestEKBindingRefusesANonCanonicalSPKI(t *testing.T) {
	spki := append([]byte(nil), testEKSPKI(t)...)
	spki[549] = 0x03 // e = 65539
	m := EKBinding{RealmID: newID(), AccountID: newID(), RootDigest: digestOf("r"), EKGeneration: 1,
		EKAlg: "rsa-oaep-4096-sha256", EKPublicKey: spki}
	require.Nil(t, m.Encode())
}

// P1 at the layout level (LD-32): no published result or status position
// names a device. The vectors P1-a–P1-e are PR-A2's.
func TestPublishedLayoutsCarryNoDeviceField(t *testing.T) {
	for _, m := range []specer{&Result{}, &Status{}} {
		tag, fs := m.spec()
		for _, f := range fs {
			require.NotContains(t, f.name, "device", "%s.%s", tag, f.name)
		}
	}
}

// Every worst case fits each envelope position that carries it (§2.6).
func TestPublishedFitTheirEnvelopePositions(t *testing.T) {
	require.LessOrEqual(t, layoutMax(&Intent{}), 2048)     // bundle.intent, relay.intent
	require.LessOrEqual(t, layoutMax(&EKBinding{}), 2048)  // bundle.ek_binding
	require.LessOrEqual(t, layoutMax(&CPAuthz{}), 1024)    // bundle.cp_authz
	require.LessOrEqual(t, layoutMax(&CPEvidence{}), 1024) // submit.cp_evidence
	require.LessOrEqual(t, layoutMax(&Result{}), 4096)     // bundle.result
	require.LessOrEqual(t, layoutMax(&Status{}), 4096)     // signed.msg
	require.LessOrEqual(t, layoutMax(&Query{}), 1024)      // query-req.query
	require.Equal(t, 17, 3+len(func() []field { _, fs := (&Intent{}).spec(); return fs }()))
	require.Equal(t, 27, 3+len(func() []field { _, fs := (&Result{}).spec(); return fs }()))
	require.Equal(t, 23, 3+len(func() []field { _, fs := (&Status{}).spec(); return fs }()))
}

// The tests below are Task 5's additions to the brief. A byte-level round trip
// cannot see a table whose pointers are consistently swapped (realm_id writing
// into AccountID encodes and decodes to the same bytes), and the Type column is
// otherwise pinned only where a case above happens to exercise it.

// publishedDocKind renders a position's kind in the DoR's §2.3 notation, so a layout can
// be compared with §2.5 as written. u>0 and u≥1 are one kind and render u≥1. A
// trailing * marks a bytes position with a value validator (every b97 is a
// P-384 point and b550 an EK SPKI, §2.4).
func publishedDocKind(k kind) string {
	switch k.tag {
	case tagUUID:
		return "s36"
	case tagUUIDv4:
		return "s36v4"
	case tagUint:
		switch {
		case k.minU == 0 && k.maxU == 1:
			return "u01"
		case k.minU == 1 && k.maxU == MaxSafeInt:
			return "u≥1"
		case k.minU == 0 && k.maxU == MaxSafeInt:
			return "u"
		}
		return fmt.Sprintf("u[%d..%d]", k.minU, k.maxU)
	case tagBytes:
		if len(k.sizes) != 1 || k.upTo != 0 {
			return fmt.Sprintf("b?%v≤%d", k.sizes, k.upTo)
		}
		s := fmt.Sprintf("b%d", k.sizes[0])
		if k.binChk != nil {
			s += "*"
		}
		if k.empty {
			s += `|""`
		}
		return s
	case tagEnum:
		vals := slices.Clone(k.enum)
		slices.Sort(vals)
		for i, v := range vals {
			if v == "" {
				vals[i] = `""`
			}
		}
		return "e{" + strings.Join(vals, ",") + "}"
	case tagArr:
		return fmt.Sprintf("a<%s>[%d..%d]", publishedDocKind(*k.elem), k.minN, k.maxN)
	}
	return fmt.Sprintf("?%d", k.tag)
}

type publishedDocRow struct{ name, typ string }

const (
	publishedDocOps     = "e{cancel_pending,enroll_existing,enroll_new,erase,recover,replace_root,revoke_root,rotate_ek,rotate_root,suspend,unsuspend}"
	publishedDocState   = "e{absent,active,disputed,erased,pending,suspended}"
	publishedDocPrior   = `e{"",absent,active,suspended}`
	publishedDocSuspBy  = "a<e{operator,user}>[0..2]"
	publishedDocFactors = "a<e{admin-webauthn,backup-code,device-key,email,email-backup,erasure-list,passkey,password,recovery-key,registration,session,sso,totp,webauthn}>[0..14]"
)

// publishedDocLayouts is DoR §2.5 as written: each published layout's element count and
// the (name, Type) of every position from 3 on.
var publishedDocLayouts = []struct {
	m     specer
	count int
	rows  []publishedDocRow
}{
	{&Intent{}, 17, []publishedDocRow{
		{"realm_id", "s36"}, {"account_id", "s36"}, {"request_id", "s36v4"}, {"op", publishedDocOps},
		{"expires_at_ms", "u≥1"}, {"expected_seq", "u"}, {"expected_recovery_gen", "u"},
		{"expected_head_digest", `b48|""`}, {"predecessor_root_digest", `b48|""`}, {"candidate_root", `b97*|""`},
		{"ek_binding_digest", `b48|""`}, {"legacy_ek_digest", `b48|""`}, {"device_intent_digest", `b48|""`},
		{"recovery_bind_digest", `b48|""`},
	}},
	{&Sig{}, 5, []publishedDocRow{
		{"role", "e{actor-device,candidate,current-root,new-device,operator,recovery-key}"}, {"digest", "b48"},
	}},
	{&LegacyEKPoP{}, 7, []publishedDocRow{
		{"realm_id", "s36"}, {"account_id", "s36"}, {"intent_digest", "b48"}, {"legacy_ek_digest", "b48"},
	}},
	{&EKBinding{}, 9, []publishedDocRow{
		{"realm_id", "s36"}, {"account_id", "s36"}, {"root_digest", "b48"}, {"ek_generation", "u≥1"},
		{"ek_alg", "e{rsa-oaep-4096-sha256}"}, {"ek_public_key", "b550*"},
	}},
	{&CPAuthz{}, 10, []publishedDocRow{
		{"realm_id", "s36"}, {"account_id", "s36"}, {"request_id", "s36v4"}, {"intent_digest", "b48"},
		{"actor", "e{operator,restore,user}"}, {"asserted_at_ms", "u≥1"}, {"evidence_digest", "b48"},
	}},
	{&CPEvidence{}, 11, []publishedDocRow{
		{"request_id", "s36v4"}, {"factors", publishedDocFactors}, {"mfa_enrolled", "u01"}, {"password_login", "u01"},
		{"recovery_window_clear", "u01"}, {"operator_assisted", "u01"}, {"credential_epoch_digest", `b48|""`},
		{"evidence_salt", "b32"},
	}},
	{&Result{}, 27, []publishedDocRow{
		{"realm_id", "s36"}, {"account_id", "s36"}, {"request_id", "s36v4"}, {"intent_digest", "b48"},
		{"cp_authz_digest", "b48"}, {"op", publishedDocOps}, {"seq", "u≥1"}, {"state", publishedDocState}, {"prior_state", publishedDocPrior},
		{"suspended_by", publishedDocSuspBy}, {"active_root", `b97*|""`}, {"root_revoked", "u01"},
		{"ek_binding_digest", `b48|""`}, {"pending_root", `b97*|""`}, {"pending_ek_binding_digest", `b48|""`},
		{"legacy_ek_digest", `b48|""`}, {"legacy_pin_revoked", "u01"}, {"prev_entry_digest", `b48|""`},
		{"recovery_gen", "u"}, {"activation_not_before_ms", "u"},
		{"assurance", `e{"",continuity,initial,recovery,unverified}`}, {"issued_at_ms", "u"},
		{"delegation_gen", "u"}, {"delegation_serial", "u≥1"},
	}},
	{&Status{}, 23, []publishedDocRow{
		{"realm_id", "s36"}, {"account_id", "s36"}, {"seq", "u"}, {"head_entry_digest", `b48|""`},
		{"state", publishedDocState}, {"prior_state", publishedDocPrior}, {"suspended_by", publishedDocSuspBy},
		{"active_root_digest", `b48|""`}, {"root_revoked", "u01"}, {"ek_binding_digest", `b48|""`},
		{"pending_root_digest", `b48|""`}, {"pending_ek_binding_digest", `b48|""`}, {"legacy_ek_digest", `b48|""`},
		{"legacy_pin_revoked", "u01"}, {"activation_not_before_ms", "u"}, {"held", "u01"}, {"as_of_ms", "u"},
		{"valid_until_ms", "u"}, {"delegation_gen", "u"}, {"delegation_serial", "u≥1"},
	}},
	{&Query{}, 9, []publishedDocRow{
		{"realm_id", "s36"}, {"account_id", "s36"}, {"signer_digest", "b48"}, {"issued_at_ms", "u"},
		{"nonce", "b16"}, {"after_dseq", "u"},
	}},
}

func TestPublishedLayoutsMatchTheDoRTable(t *testing.T) {
	require.Len(t, publishedDocLayouts, 9)
	for _, d := range publishedDocLayouts {
		tag, fs := d.m.spec()
		require.Equal(t, d.count, 3+len(fs), "%s element count", tag)
		require.Len(t, fs, len(d.rows), tag)
		for i, f := range fs {
			require.Equal(t, d.rows[i].name, f.name, "%s position %d", tag, 3+i)
			require.Equal(t, d.rows[i].typ, publishedDocKind(f.k), "%s.%s (position %d)", tag, f.name, 3+i)
		}
	}
}

// Every row must point at the struct field that carries its name, and every
// struct field must be carried by exactly one row.
func TestPublishedRowsBindTheirOwnStructField(t *testing.T) {
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }
	for _, d := range publishedDocLayouts {
		tag, fs := d.m.spec()
		v := reflect.ValueOf(d.m).Elem()
		bound := map[int]int{}
		for _, f := range fs {
			got := -1
			for i := 0; i < v.NumField(); i++ {
				if v.Field(i).Addr().Interface() == f.ptr {
					got = i
				}
			}
			require.GreaterOrEqual(t, got, 0, "%s.%s points at no field of its struct", tag, f.name)
			require.Equal(t, norm(f.name), norm(v.Type().Field(got).Name),
				"%s.%s is bound to the wrong struct field", tag, f.name)
			bound[got]++
		}
		for i := 0; i < v.NumField(); i++ {
			require.Equal(t, 1, bound[i], "%s struct field %s", tag, v.Type().Field(i).Name)
		}
	}
}

func TestPublishedRefusesValuesOutsideTheTypeColumn(t *testing.T) {
	ek := EKBinding{RealmID: newID(), AccountID: newID(), RootDigest: digestOf("r"), EKGeneration: 1,
		EKAlg: "rsa-oaep-4096-sha256", EKPublicKey: testEKSPKI(t)}
	authz := CPAuthz{RealmID: newID(), AccountID: newID(), RequestID: newID(), IntentDigest: digestOf("i"),
		Actor: ActorRestore, AssertedAtMs: 1, EvidenceDigest: digestOf("ev")}
	noEvents := Status{RealmID: newID(), AccountID: newID(), State: StateAbsent, ValidUntilMs: 2, DelegationSerial: 1}
	hybrid := hybridOf(testPoint(t))

	// Every base is valid, so each case below differs from a valid message in
	// exactly one position.
	for name, b := range map[string][]byte{
		"ek-binding": ek.Encode(), "cp-authz restore": authz.Encode(), "status with no events": noEvents.Encode(),
		"sig": Sig{Role: RoleRecoveryKey, Digest: digestOf("i")}.Encode(),
		"legacy-ek-pop": LegacyEKPoP{RealmID: newID(), AccountID: newID(), IntentDigest: digestOf("i"),
			LegacyEKDigest: digestOf("l")}.Encode(),
		"query": Query{RealmID: newID(), AccountID: newID(), SignerDigest: digestOf("s"), Nonce: rnd(16)}.Encode(),
	} {
		require.NotNil(t, b, name)
	}
	roundTrip(t, noEvents)

	ekMut := func(f func(*EKBinding)) []byte { m := ek; f(&m); return m.Encode() }
	authzMut := func(f func(*CPAuthz)) []byte { m := authz; f(&m); return m.Encode() }
	evMut := func(f func(*CPEvidence)) []byte { m := sampleCPEvidence(); f(&m); return m.Encode() }
	resMut := func(f func(*Result)) []byte { m := sampleResult(t); f(&m); return m.Encode() }
	stMut := func(f func(*Status)) []byte { m := sampleStatus(); f(&m); return m.Encode() }

	for name, got := range map[string][]byte{
		"ek-binding generation 0":   ekMut(func(m *EKBinding) { m.EKGeneration = 0 }),
		"ek-binding other alg":      ekMut(func(m *EKBinding) { m.EKAlg = "rsa-oaep-2048-sha256" }),
		"ek-binding root digest 47": ekMut(func(m *EKBinding) { m.RootDigest = m.RootDigest[:47] }),

		"cp-authz asserted_at_ms 0":       authzMut(func(m *CPAuthz) { m.AssertedAtMs = 0 }),
		"cp-authz unknown actor":          authzMut(func(m *CPAuthz) { m.Actor = "admin" }),
		"cp-authz v1 request_id":          authzMut(func(m *CPAuthz) { m.RequestID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8" }),
		"cp-authz empty evidence":         authzMut(func(m *CPAuthz) { m.EvidenceDigest = nil }),
		"cp-evidence mfa_enrolled 2":      evMut(func(m *CPEvidence) { m.MFAEnrolled = 2 }),
		"cp-evidence operator_assisted 2": evMut(func(m *CPEvidence) { m.OperatorAssisted = 2 }),
		"cp-evidence salt 31":             evMut(func(m *CPEvidence) { m.EvidenceSalt = m.EvidenceSalt[:31] }),
		"cp-evidence epoch digest 47":     evMut(func(m *CPEvidence) { m.CredentialEpochDigest = m.CredentialEpochDigest[:47] }),
		"cp-evidence v1 request_id":       evMut(func(m *CPEvidence) { m.RequestID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8" }),

		"result seq 0":                    resMut(func(m *Result) { m.Seq = 0 }),
		"result delegation_serial 0":      resMut(func(m *Result) { m.DelegationSerial = 0 }),
		"result unknown state":            resMut(func(m *Result) { m.State = "locked" }),
		"result prior_state disputed":     resMut(func(m *Result) { m.PriorState = StateDisputed }),
		"result unknown assurance":        resMut(func(m *Result) { m.Assurance = "high" }),
		"result private op":               resMut(func(m *Result) { m.Op = OpBindRecovery }),
		"result suspended_by unknown":     resMut(func(m *Result) { m.SuspendedBy = []string{"admin"} }),
		"result suspended_by duplicate":   resMut(func(m *Result) { m.SuspendedBy = []string{ActorUser, ActorUser} }),
		"result suspended_by restore":     resMut(func(m *Result) { m.SuspendedBy = []string{ActorRestore} }),
		"result hybrid active_root":       resMut(func(m *Result) { m.ActiveRoot = hybrid }),
		"result hybrid pending_root":      resMut(func(m *Result) { m.PendingRoot = hybrid }),
		"result short active_root":        resMut(func(m *Result) { m.ActiveRoot = m.ActiveRoot[:96] }),
		"result root_revoked 2":           resMut(func(m *Result) { m.RootRevoked = 2 }),
		"result legacy_pin_revoked 2":     resMut(func(m *Result) { m.LegacyPinRevoked = 2 }),
		"result issued_at_ms past 2^53-1": resMut(func(m *Result) { m.IssuedAtMs = MaxSafeInt + 1 }),

		"status delegation_serial 0":    stMut(func(m *Status) { m.DelegationSerial = 0 }),
		"status unknown state":          stMut(func(m *Status) { m.State = "locked" }),
		"status prior_state erased":     stMut(func(m *Status) { m.PriorState = StateErased }),
		"status held 2":                 stMut(func(m *Status) { m.Held = 2 }),
		"status root_revoked 2":         stMut(func(m *Status) { m.RootRevoked = 2 }),
		"status legacy_pin_revoked 2":   stMut(func(m *Status) { m.LegacyPinRevoked = 2 }),
		"status suspended_by duplicate": stMut(func(m *Status) { m.SuspendedBy = []string{ActorUser, ActorUser} }),
		"status as_of_ms past 2^53-1":   stMut(func(m *Status) { m.AsOfMs = MaxSafeInt + 1 }),
		"status head digest 47":         stMut(func(m *Status) { m.HeadEntryDigest = m.HeadEntryDigest[:47] }),

		"sig unknown role":             Sig{Role: "root", Digest: digestOf("i")}.Encode(),
		"sig digest 47":                Sig{Role: RoleCandidate, Digest: digestOf("i")[:47]}.Encode(),
		"sig empty digest":             Sig{Role: RoleCandidate}.Encode(),
		"legacy-ek-pop digest 47":      LegacyEKPoP{RealmID: newID(), AccountID: newID(), IntentDigest: digestOf("i")[:47], LegacyEKDigest: digestOf("l")}.Encode(),
		"legacy-ek-pop empty pin":      LegacyEKPoP{RealmID: newID(), AccountID: newID(), IntentDigest: digestOf("i")}.Encode(),
		"query nonce 15":               Query{RealmID: newID(), AccountID: newID(), SignerDigest: digestOf("s"), Nonce: rnd(15)}.Encode(),
		"query signer digest 49":       Query{RealmID: newID(), AccountID: newID(), SignerDigest: append(digestOf("s"), 0), Nonce: rnd(16)}.Encode(),
		"query after_dseq past 2^53-1": Query{RealmID: newID(), AccountID: newID(), SignerDigest: digestOf("s"), Nonce: rnd(16), AfterDseq: MaxSafeInt + 1}.Encode(),
	} {
		require.Nil(t, got, name)
	}
}

// u is 0 or [1-9][0-9]*, so the zero a position admits must round-trip, and so
// must the top of the range (ANB_NEVER, §2.5 status #17).
func TestPublishedUnboundedPositionsAdmitZeroAndTheTop(t *testing.T) {
	r := sampleResult(t)
	r.RecoveryGen, r.ActivationNotBeforeMs, r.IssuedAtMs, r.DelegationGen = 0, 0, 0, 0
	roundTrip(t, r)
	s := sampleStatus()
	s.Seq, s.AsOfMs, s.DelegationGen = 0, 0, 0
	s.HeadEntryDigest = nil
	s.ValidUntilMs = MaxSafeInt
	require.Equal(t, ANBNever, s.ActivationNotBeforeMs)
	roundTrip(t, s)
	roundTrip(t, Query{RealmID: newID(), AccountID: newID(), SignerDigest: digestOf("s"), IssuedAtMs: MaxSafeInt,
		Nonce: rnd(16), AfterDseq: MaxSafeInt})
}
