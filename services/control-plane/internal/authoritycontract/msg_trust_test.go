package authoritycontract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func pointsN(t testing.TB, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = testPoint(t)
	}
	return out
}

func sampleDelegation(t testing.TB) Delegation {
	pins := make([][]byte, 8)
	for i := range pins {
		pins[i] = rnd(48)
	}
	return Delegation{RealmID: newID(), Serial: 3, MinValidSerial: 2, ProductRootGen: 0,
		RSKPublicKeys: pointsN(t, 4), CAKPublicKeys: pointsN(t, 4), OperatorPublicKeys: nil,
		AuthorityTLSKeys: pointsN(t, 8), CPTLSPins: pins, NotBeforeMs: 1_700_000_000_000, NotAfterMs: 1_715_552_000_000,
		Scope: append([]string(nil), scopeValues...), AcceptedVersions: []string{"1"}, TauMs: 600_000,
		DeltaMs: 259_200_000, GMs: 900_000, CutoverAtMs: 0}
}

func sampleSigned(t testing.TB, typ string, msg []byte, dual bool) Signed {
	t.Helper()
	s := Signed{Type: typ, Msg: msg, SigA: rnd(96)}
	if dual {
		s.SigB = rnd(96)
	}
	return s
}

func TestTrustFamilyRoundTrip(t *testing.T) {
	d := sampleDelegation(t)
	db := roundTrip(t, d)
	roundTrip(t, Succession{RealmID: newID(), PrevRootGen: 0, NewRootGen: 1, NewPrimaryA: testPoint(t),
		NewPrimaryB: testPoint(t), NewRecoveryA: testPoint(t), NewRecoveryB: testPoint(t)})
	roundTrip(t, Anchors{RealmID: newID(), PrimaryA: testPoint(t), PrimaryB: testPoint(t), RecoveryA: testPoint(t), RecoveryB: testPoint(t)})
	sd := roundTrip(t, sampleSigned(t, "delegation", db, true))
	ss := roundTrip(t, sampleSigned(t, "status", sampleStatus().Encode(), false))
	roundTrip(t, Statuses{Items: [][]byte{ss}})
	roundTrip(t, Statuses{})
	roundTrip(t, TrustEnvelope{Items: [][]byte{sd}})
}

// LD-3 dual control at decode (D-194, D-223).
func TestSignedSecondSignatureRule(t *testing.T) {
	db := sampleDelegation(t).Encode()
	sb := sampleStatus().Encode()
	for name, s := range map[string]Signed{
		"delegation without sig_b": sampleSigned(t, "delegation", db, false),
		"succession without sig_b": sampleSigned(t, "succession", []byte(`["concord-account-authority",1,"succession",`), false),
		"status with sig_b":        sampleSigned(t, "status", sb, true),
		"device-status with sig_b": sampleSigned(t, "device-status", []byte(`["concord-account-authority",1,"device-status",`), true),
		"type is not msg[2]":       sampleSigned(t, "status", db, false),
		"status vs statuses":       sampleSigned(t, "status", []byte(`["concord-account-authority",1,"statuses",[]]`), false),
		"type outside the set":     sampleSigned(t, "result", sampleResult(t).Encode(), false),
	} {
		require.Nil(t, s.Encode(), name)
	}
	// The same rules at decode time, from bytes a writer could forge.
	good := sampleSigned(t, "delegation", db, true).Encode()
	require.NotNil(t, good)
	i := bytes.LastIndex(good, []byte(`,"`))
	noSigB := append(append([]byte(nil), good[:i]...), []byte(`,""]`)...)
	_, err := DecodeSigned(noSigB)
	require.ErrorIs(t, err, ErrMalformed, "a missing signature decodes as malformed and nothing else")
}

func TestDelegationTypeColumn(t *testing.T) {
	for name, mut := range map[string]func(*Delegation){
		"serial 0":           func(d *Delegation) { d.Serial = 0 },
		"no rsk":             func(d *Delegation) { d.RSKPublicKeys = nil },
		"five rsk":           func(d *Delegation) { d.RSKPublicKeys = pointsN(t, 5) },
		"version 0":          func(d *Delegation) { d.AcceptedVersions = []string{"0"} },
		"version 10000":      func(d *Delegation) { d.AcceptedVersions = []string{"10000"} },
		"unknown scope":      func(d *Delegation) { d.Scope = []string{"device-status"} },
		"hybrid cak":         func(d *Delegation) { d.CAKPublicKeys = [][]byte{hybridOf(testPoint(t))} },
		"duplicate operator": func(d *Delegation) { p := testPoint(t); d.OperatorPublicKeys = [][]byte{p, p} },
	} {
		d := sampleDelegation(t)
		mut(&d)
		require.Nil(t, d.Encode(), name)
	}
	// Unsorted bytes refused at decode.
	d := sampleDelegation(t)
	d.AcceptedVersions = []string{"1", "2"}
	b := string(d.Encode())
	_, err := DecodeDelegation([]byte(strings.Replace(b, `["1","2"]`, `["2","1"]`, 1)))
	require.ErrorIs(t, err, ErrMalformed)
}

func TestTrustFamilyCaps(t *testing.T) {
	require.LessOrEqual(t, layoutMax(&Delegation{}), 4096) // signed.msg
	require.LessOrEqual(t, layoutMax(&Succession{}), 4096)
	require.LessOrEqual(t, layoutMax(&Anchors{}), CapAnchors)
	require.LessOrEqual(t, layoutMax(&Signed{}), CapSigned)
	_, err := DecodeTrustEnvelope(bytes.Repeat([]byte("A"), CapTrust+1))
	require.ErrorIs(t, err, ErrMalformed, "trust standalone cap (runtime only: 2 MiB is too large for vectors.json)")
	require.ErrorContains(t, err, "length", "junk over the cap is refused on length, before it is parsed")
	_, err = DecodeAnchors(bytes.Repeat([]byte("A"), CapAnchors+1))
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "length", "junk over the cap is refused on length, before it is parsed")
}

// trustOverCapWire writes a canonical `trust` by hand, so that its items are
// the only thing that decides whether it is over the cap.
func trustOverCapWire(items [][]byte) []byte {
	var w bytes.Buffer
	w.WriteString(encodedPrefix + `"trust",[`)
	for i, it := range items {
		if i > 0 {
			w.WriteByte(',')
		}
		w.WriteString(`"` + base64.StdEncoding.EncodeToString(it) + `"`)
	}
	w.WriteString(`]]`)
	return w.Bytes()
}

// Runs of "A" are refused by the JSON parse whether or not a length check
// exists, so they cannot show that the cap is enforced. This is the input that
// can: a canonical `trust` of 256 items of 6144 bytes, which every position
// cap admits, 2 097 961 bytes long against a 2 097 152 cap.
func TestTrustOverCapStructurallyValid(t *testing.T) {
	items := make([][]byte, TrustItems)
	for i := range items {
		items[i] = rnd(6144)
	}
	wire := trustOverCapWire(items)
	require.Len(t, wire, layoutMax(&TrustEnvelope{}), "the product of the position caps")
	require.Greater(t, len(wire), CapTrust)

	// Control: the same construction one item shorter is under the cap and
	// decodes, so what refuses the full one is its size and nothing else.
	under := trustOverCapWire(items[:TrustItems-1])
	require.LessOrEqual(t, len(under), CapTrust)
	got, err := DecodeTrustEnvelope(under)
	require.NoError(t, err)
	require.Equal(t, items[:TrustItems-1], got.Items)
	require.Equal(t, under, got.Encode())

	_, err = DecodeTrustEnvelope(wire)
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "length")
	_, err = Decode("trust", wire)
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "length")
	require.Nil(t, TrustEnvelope{Items: items}.Encode(), "the encoder refuses what the decoder refuses")
	_, err = Marshal(&TrustEnvelope{Items: items})
	require.ErrorContains(t, err, "length")
}

// trustSignedText writes a `signed` envelope by hand, bypassing Encode's own
// rules, so the decode-time rules run on bytes a writer could forge.
func trustSignedText(typ string, msg, sigA, sigB []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString
	return []byte(encodedPrefix + `"signed","` + typ + `","` + b64(msg) + `","` + b64(sigA) + `","` + b64(sigB) + `"]`)
}

// trustRewrite parses canonical bytes, lets edit change the top-level array
// and re-serialises it (tests only: json.Marshal leaves every byte of EN2's
// alphabet alone, so an unedited array re-serialises to the same bytes).
func trustRewrite(t testing.TB, b []byte, edit func(arr []any)) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var arr []any
	require.NoError(t, dec.Decode(&arr))
	edit(arr)
	out, err := json.Marshal(arr)
	require.NoError(t, err)
	return out
}

// trustWorstDelegation is a delegation with every array at its maximum, every
// u at 2^53 − 1 and four-digit versions.
func trustWorstDelegation(t testing.TB) Delegation {
	t.Helper()
	d := sampleDelegation(t)
	d.OperatorPublicKeys = pointsN(t, MaxOperatorKeys)
	d.AcceptedVersions = []string{"1000", "1001", "1002", "1003", "1004", "1005", "1006", "1007"}
	d.Serial, d.MinValidSerial, d.ProductRootGen = MaxSafeInt, MaxSafeInt, MaxSafeInt
	d.NotBeforeMs, d.NotAfterMs, d.TauMs, d.DeltaMs, d.GMs, d.CutoverAtMs = MaxSafeInt, MaxSafeInt, MaxSafeInt, MaxSafeInt, MaxSafeInt, MaxSafeInt
	return d
}

// §2.6 at decode: sig_b "" on a delegation or succession, or sig_b present on a
// status or device-status, is malformed, whatever Encode would have refused.
func TestSignedDecodeRulesFromForgedBytes(t *testing.T) {
	sigA, sigB := rnd(96), rnd(96)
	msgOf := func(typ string) []byte { return []byte(encodedPrefix + `"` + typ + `",[]]`) }
	for i, typ := range signedTypes {
		dual := typ == "delegation" || typ == "succession"
		right, wrong := sigB, []byte(nil)
		if !dual {
			right, wrong = nil, sigB
		}
		other := signedTypes[(i+1)%len(signedTypes)]
		t.Run(typ+"/the second signature the type calls for", func(t *testing.T) {
			got, err := DecodeSigned(trustSignedText(typ, msgOf(typ), sigA, right))
			require.NoError(t, err)
			require.Equal(t, typ, got.Type)
			require.Equal(t, sigA, got.SigA)
			require.Equal(t, len(right), len(got.SigB))
		})
		t.Run(typ+"/the opposite second signature", func(t *testing.T) {
			_, err := DecodeSigned(trustSignedText(typ, msgOf(typ), sigA, wrong))
			require.ErrorIs(t, err, ErrMalformed)
		})
		t.Run(typ+"/msg of another type", func(t *testing.T) {
			_, err := DecodeSigned(trustSignedText(typ, msgOf(other), sigA, right))
			require.ErrorIs(t, err, ErrMalformed)
		})
	}
	t.Run("status is not statuses", func(t *testing.T) {
		_, err := DecodeSigned(trustSignedText("status", msgOf("statuses"), sigA, nil))
		require.ErrorIs(t, err, ErrMalformed)
	})
	t.Run("msg is not a message at all", func(t *testing.T) {
		_, err := DecodeSigned(trustSignedText("status", []byte("not a message"), sigA, nil))
		require.ErrorIs(t, err, ErrMalformed)
	})
	t.Run("msg is opaque beyond its prefix", func(t *testing.T) {
		// Only the canonical prefix bytes are read; the rest is PR-A2's to decode.
		prefixOnly := []byte(encodedPrefix + `"status",`)
		got, err := DecodeSigned(trustSignedText("status", prefixOnly, sigA, nil))
		require.NoError(t, err)
		require.Equal(t, prefixOnly, got.Msg)
	})
	t.Run("type outside the set", func(t *testing.T) {
		_, err := DecodeSigned(trustSignedText("result", msgOf("result"), sigA, nil))
		require.ErrorIs(t, err, ErrMalformed)
	})
}

func TestSignedFieldBounds(t *testing.T) {
	prefix := []byte(encodedPrefix + `"status",`)
	padded := func(n int) []byte { return append(slices.Clone(prefix), bytes.Repeat([]byte("A"), n-len(prefix))...) }
	b := roundTrip(t, sampleSigned(t, "status", padded(4096), false))
	require.LessOrEqual(t, len(b), CapSigned, "a signed at msg's maximum fits its standalone cap")
	require.Nil(t, sampleSigned(t, "status", padded(4097), false).Encode(), "msg over 4096 bytes")
	require.Nil(t, sampleSigned(t, "status", nil, false).Encode(), "empty msg")
	// sig_a is b96 and never empty; sig_b is b96 or "", so 95 and 97 are its size bounds.
	for _, n := range []int{0, 95, 97} {
		s := sampleSigned(t, "status", prefix, false)
		s.SigA = rnd(n)
		_, err := Marshal(&s)
		require.ErrorIs(t, err, ErrMalformed, "sig_a of %d bytes", n)
		require.ErrorContains(t, err, "sig_a", "sig_a of %d bytes", n)
	}
	for _, n := range []int{95, 97} {
		d := sampleSigned(t, "delegation", []byte(encodedPrefix+`"delegation",`), true)
		d.SigB = rnd(n)
		_, err := Marshal(&d)
		require.ErrorIs(t, err, ErrMalformed, "sig_b of %d bytes", n)
		require.ErrorContains(t, err, "b size", "sig_b of %d bytes is refused by its kind, not by the dual-control rule", n)
	}
	// An empty sig_b is a valid b96|"" value; it is the dual-control rule that refuses it on a delegation.
	d := sampleSigned(t, "delegation", []byte(encodedPrefix+`"delegation",`), false)
	_, err := Marshal(&d)
	require.ErrorIs(t, err, ErrMalformed, "a delegation without sig_b")
	require.ErrorContains(t, err, "missing second signature", "an absent sig_b on a delegation is the dual-control rule")
	for _, typ := range []string{"delegation", "device-status", "status", "succession"} {
		require.NotNil(t, sampleSigned(t, typ, []byte(encodedPrefix+`"`+typ+`",`), typ == "delegation" || typ == "succession").Encode(), typ)
	}
}

func TestSignedRefusalNamesTheRuleOnly(t *testing.T) {
	db := sampleDelegation(t).Encode()
	for name, s := range map[string]Signed{
		"type is not msg[2]":    sampleSigned(t, "status", db, false),
		"missing second sig":    sampleSigned(t, "delegation", db, false),
		"unexpected second sig": sampleSigned(t, "status", []byte(encodedPrefix+`"status",`), true),
	} {
		_, err := Marshal(&s)
		require.ErrorIs(t, err, ErrMalformed, name)
		for _, leak := range []string{base64.StdEncoding.EncodeToString(s.Msg)[:32], base64.StdEncoding.EncodeToString(s.SigA)[:16], "delegation\"", "status\""} {
			require.NotContains(t, err.Error(), leak, name)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, s := range []string{"1", "9", "10", "99", "1234", "9999"} {
		require.NoError(t, checkVersion(s), s)
	}
	for _, s := range []string{"", "0", "01", "10000", "a", "1a", "1.0", "-1", "1 ", " 1", "0001"} {
		require.ErrorIs(t, checkVersion(s), ErrMalformed, "%q", s)
	}
}

func TestDelegationArrayBounds(t *testing.T) {
	versions := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = strconv.Itoa(i + 1)
		}
		return out
	}
	pins := func(n, size int) [][]byte {
		out := make([][]byte, n)
		for i := range out {
			out[i] = rnd(size)
		}
		return out
	}
	for name, mut := range map[string]func(*Delegation){
		"one rsk":        func(d *Delegation) { d.RSKPublicKeys = pointsN(t, 1) },
		"one cak":        func(d *Delegation) { d.CAKPublicKeys = pointsN(t, 1) },
		"four operator":  func(d *Delegation) { d.OperatorPublicKeys = pointsN(t, MaxOperatorKeys) },
		"one tls key":    func(d *Delegation) { d.AuthorityTLSKeys = pointsN(t, 1) },
		"one pin":        func(d *Delegation) { d.CPTLSPins = pins(1, 48) },
		"one scope":      func(d *Delegation) { d.Scope = []string{ScopeStatus} },
		"one version":    func(d *Delegation) { d.AcceptedVersions = versions(1) },
		"eight versions": func(d *Delegation) { d.AcceptedVersions = versions(8) },
		"u at 2^53 - 1":  func(d *Delegation) { d.CutoverAtMs = MaxSafeInt },
	} {
		d := sampleDelegation(t)
		mut(&d)
		require.NotNil(t, d.Encode(), name)
	}
	for name, mut := range map[string]func(*Delegation){
		"no cak":            func(d *Delegation) { d.CAKPublicKeys = nil },
		"five cak":          func(d *Delegation) { d.CAKPublicKeys = pointsN(t, 5) },
		"five operator":     func(d *Delegation) { d.OperatorPublicKeys = pointsN(t, MaxOperatorKeys+1) },
		"no tls key":        func(d *Delegation) { d.AuthorityTLSKeys = nil },
		"nine tls keys":     func(d *Delegation) { d.AuthorityTLSKeys = pointsN(t, MaxAuthorityTLSKeys+1) },
		"no pin":            func(d *Delegation) { d.CPTLSPins = nil },
		"nine pins":         func(d *Delegation) { d.CPTLSPins = pins(MaxCPTLSPins+1, 48) },
		"pin of 47 bytes":   func(d *Delegation) { d.CPTLSPins = pins(1, 47) },
		"pin of 49 bytes":   func(d *Delegation) { d.CPTLSPins = pins(1, 49) },
		"no scope":          func(d *Delegation) { d.Scope = nil },
		"duplicate scope":   func(d *Delegation) { d.Scope = []string{ScopeStatus, ScopeStatus} },
		"scope result":      func(d *Delegation) { d.Scope = []string{"result"} },
		"no version":        func(d *Delegation) { d.AcceptedVersions = nil },
		"nine versions":     func(d *Delegation) { d.AcceptedVersions = versions(9) },
		"version 01":        func(d *Delegation) { d.AcceptedVersions = []string{"01"} },
		"empty version":     func(d *Delegation) { d.AcceptedVersions = []string{""} },
		"duplicate version": func(d *Delegation) { d.AcceptedVersions = []string{"1", "1"} },
		"u above 2^53 - 1":  func(d *Delegation) { d.NotAfterMs = MaxSafeInt + 1 },
		"uppercase realm":   func(d *Delegation) { d.RealmID = strings.ToUpper(d.RealmID) },
		"short rsk point":   func(d *Delegation) { d.RSKPublicKeys = [][]byte{d.RSKPublicKeys[0][:96]} },
		"hybrid tls key":    func(d *Delegation) { d.AuthorityTLSKeys = [][]byte{hybridOf(testPoint(t))} },
		"hybrid rsk":        func(d *Delegation) { d.RSKPublicKeys = [][]byte{hybridOf(testPoint(t))} },
		"hybrid operator":   func(d *Delegation) { d.OperatorPublicKeys = [][]byte{hybridOf(testPoint(t))} },
	} {
		d := sampleDelegation(t)
		mut(&d)
		require.Nil(t, d.Encode(), name)
	}
}

// DoR §2.5: "Measured worst case at every array maximum: about 3.70 KB ...
// under signed.msg b≤4096; PR-A carries a positive vector at that size."
func TestDelegationWorstCase(t *testing.T) {
	b := roundTrip(t, trustWorstDelegation(t))
	t.Logf("delegation at every array maximum: %d bytes (layoutMax %d)", len(b), layoutMax(&Delegation{}))
	require.LessOrEqual(t, len(b), 4096, "fits signed.msg")
	require.LessOrEqual(t, len(b), layoutMax(&Delegation{}))
	signed := roundTrip(t, sampleSigned(t, "delegation", b, true))
	require.LessOrEqual(t, len(signed), CapSigned, "and the signed envelope around it fits the standalone cap")
}

func TestDelegationDecodeRefusesUnsortedAndDuplicateArrays(t *testing.T) {
	b := sampleDelegation(t).Encode()
	require.NotNil(t, b)
	require.Equal(t, b, trustRewrite(t, b, func([]any) {}), "control: the rewrite helper preserves canonical bytes")
	_, err := DecodeDelegation(b)
	require.NoError(t, err, "control: the unedited bytes decode")
	for pos, name := range map[int]string{7: "rsk_public_keys", 8: "cak_public_keys", 10: "authority_tls_keys", 11: "cp_tls_pins", 14: "scope"} {
		swapped := trustRewrite(t, b, func(arr []any) {
			a, ok := arr[pos].([]any)
			require.True(t, ok, name)
			a[0], a[1] = a[1], a[0]
		})
		_, err := DecodeDelegation(swapped)
		require.ErrorIs(t, err, ErrMalformed, name+" out of order")
		dup := trustRewrite(t, b, func(arr []any) {
			a, ok := arr[pos].([]any)
			require.True(t, ok, name)
			a[1] = a[0]
		})
		_, err = DecodeDelegation(dup)
		require.ErrorIs(t, err, ErrMalformed, name+" with a duplicate")
	}
}

func TestSuccessionAndAnchorsTypeColumn(t *testing.T) {
	offCurve := append([]byte{0x04}, make([]byte, 96)...)
	succ := func() Succession {
		return Succession{RealmID: newID(), PrevRootGen: 0, NewRootGen: 1, NewPrimaryA: testPoint(t),
			NewPrimaryB: testPoint(t), NewRecoveryA: testPoint(t), NewRecoveryB: testPoint(t)}
	}
	anch := func() Anchors {
		return Anchors{RealmID: newID(), PrimaryA: testPoint(t), PrimaryB: testPoint(t), RecoveryA: testPoint(t), RecoveryB: testPoint(t)}
	}
	for name, mut := range map[string]func(*Succession){
		"hybrid new_primary_a":  func(m *Succession) { m.NewPrimaryA = hybridOf(m.NewPrimaryA) },
		"short new_primary_b":   func(m *Succession) { m.NewPrimaryB = m.NewPrimaryB[:96] },
		"off-curve recovery_a":  func(m *Succession) { m.NewRecoveryA = offCurve },
		"empty new_recovery_b":  func(m *Succession) { m.NewRecoveryB = nil },
		"uppercase realm":       func(m *Succession) { m.RealmID = strings.ToUpper(m.RealmID) },
		"new_root_gen > 2^53-1": func(m *Succession) { m.NewRootGen = MaxSafeInt + 1 },
	} {
		m := succ()
		mut(&m)
		require.Nil(t, m.Encode(), "succession: "+name)
	}
	for name, mut := range map[string]func(*Anchors){
		"hybrid primary_a":     func(m *Anchors) { m.PrimaryA = hybridOf(m.PrimaryA) },
		"short primary_b":      func(m *Anchors) { m.PrimaryB = m.PrimaryB[:96] },
		"off-curve recovery_a": func(m *Anchors) { m.RecoveryA = offCurve },
		"empty recovery_b":     func(m *Anchors) { m.RecoveryB = nil },
		"uppercase realm":      func(m *Anchors) { m.RealmID = strings.ToUpper(m.RealmID) },
	} {
		m := anch()
		mut(&m)
		require.Nil(t, m.Encode(), "anchors: "+name)
	}
	require.LessOrEqual(t, len(roundTrip(t, anch())), CapAnchors)
}

// The Rule columns of §2.5 and §2.6, key separation (§2.4), T1 and T2 belong to
// PR-A2's verifiers. Pinned as NOT enforced here, so a codec change cannot fork
// the verifier's inputs from the vectors.
func TestTrustRulesBelongToTheVerifier(t *testing.T) {
	for name, mut := range map[string]func(*Delegation){
		"min_valid_serial 0":              func(d *Delegation) { d.MinValidSerial = 0 },
		"min_valid_serial above serial":   func(d *Delegation) { d.MinValidSerial = d.Serial + 1 },
		"not_after before not_before":     func(d *Delegation) { d.NotAfterMs = d.NotBeforeMs - 1 },
		"lifetime over 400 days":          func(d *Delegation) { d.NotAfterMs = d.NotBeforeMs + DelegationLifetimeMaxMs + 1 },
		"tau_ms 0":                        func(d *Delegation) { d.TauMs = 0 },
		"tau_ms above V_MAX":              func(d *Delegation) { d.TauMs = VMaxMs + 1 },
		"delta_ms 0":                      func(d *Delegation) { d.DeltaMs = 0 },
		"delta_ms above DELTA_MAX":        func(d *Delegation) { d.DeltaMs = DeltaMaxMs + 1 },
		"g_ms above delta_ms":             func(d *Delegation) { d.GMs = d.DeltaMs + 1 },
		"g_ms 0":                          func(d *Delegation) { d.GMs = 0 },
		"accepted_versions without \"1\"": func(d *Delegation) { d.AcceptedVersions = []string{"2"} },
		"a cak that is also an rsk":       func(d *Delegation) { d.CAKPublicKeys = [][]byte{d.RSKPublicKeys[0]} },
	} {
		d := sampleDelegation(t)
		mut(&d)
		require.NotNil(t, d.Encode(), name)
	}
	k := testPoint(t)
	require.NotNil(t, Anchors{RealmID: newID(), PrimaryA: k, PrimaryB: k, RecoveryA: k, RecoveryB: k}.Encode(), "anchors: pairwise distinct is T1")
	require.NotNil(t, Succession{RealmID: newID(), PrevRootGen: 4, NewRootGen: 9, NewPrimaryA: k, NewPrimaryB: k,
		NewRecoveryA: k, NewRecoveryB: k}.Encode(), "succession: new = prev + 1 and distinct keys are T2")
	one := sampleSigned(t, "delegation", sampleDelegation(t).Encode(), true).Encode()
	require.NotNil(t, TrustEnvelope{Items: [][]byte{one, one}}.Encode(), "trust: a duplicate is A2's")
	require.NotNil(t, TrustEnvelope{Items: [][]byte{{9}, {1}}}.Encode(), "trust: ordering and item content are A2's")
}

func TestStatusesAndTrustItemBounds(t *testing.T) {
	many := func(n int) [][]byte {
		out := make([][]byte, n)
		for i := range out {
			out[i] = []byte{1}
		}
		return out
	}
	require.NotNil(t, Statuses{Items: many(StatusBatch)}.Encode())
	require.Nil(t, Statuses{Items: many(StatusBatch + 1)}.Encode(), "statuses over 500")
	require.NotNil(t, TrustEnvelope{Items: many(TrustItems)}.Encode())
	require.Nil(t, TrustEnvelope{Items: many(TrustItems + 1)}.Encode(), "trust over 256")
	require.Nil(t, TrustEnvelope{}.Encode(), "trust needs at least one item")
	require.Nil(t, Statuses{Items: [][]byte{nil}}.Encode(), "an empty item")
	require.Nil(t, TrustEnvelope{Items: [][]byte{nil}}.Encode(), "an empty item")
	require.NotNil(t, Statuses{Items: [][]byte{rnd(6144)}}.Encode())
	require.Nil(t, Statuses{Items: [][]byte{rnd(6145)}}.Encode(), "an item over 6144 bytes")
	require.NotNil(t, TrustEnvelope{Items: [][]byte{rnd(6144)}}.Encode())
	require.Nil(t, TrustEnvelope{Items: [][]byte{rnd(6145)}}.Encode(), "an item over 6144 bytes")
	// l keeps the writer's order; only a keeps bytewise order.
	in := [][]byte{{9}, {1}, {5}}
	got, err := DecodeStatuses(Statuses{Items: in}.Encode())
	require.NoError(t, err)
	require.Equal(t, in, got.Items)
	got2, err := DecodeTrustEnvelope(TrustEnvelope{Items: in}.Encode())
	require.NoError(t, err)
	require.Equal(t, in, got2.Items)
	// Standalone cap, through the registry as well.
	_, err = Decode("statuses", bytes.Repeat([]byte("A"), layoutMax(&Statuses{})+1))
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "length", "junk over the cap is refused on length, before it is parsed")
	_, err = Decode("trust", bytes.Repeat([]byte("A"), CapTrust+1))
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "length", "junk over the cap is refused on length, before it is parsed")
}
