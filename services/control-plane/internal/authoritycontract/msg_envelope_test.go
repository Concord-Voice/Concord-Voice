package authoritycontract

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func sampleBundle(t testing.TB) Bundle {
	return Bundle{Intent: sampleIntent(t).Encode(), SigCandidate: rnd(96), SigCurrentRoot: rnd(96),
		LegacyPopSig: rnd(512), LegacySPKI: testEKSPKI(t), CPAuthz: rnd(300), CPAuthzSig: rnd(96),
		Result: sampleResult(t).Encode(), ResultSig: rnd(96)}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	bb := roundTrip(t, sampleBundle(t))
	roundTrip(t, History{AccountID: newID(), Bundles: [][]byte{bb, bb}})
	roundTrip(t, History{AccountID: newID()})
	roundTrip(t, Relay{Intent: rnd(700), SigCurrentRoot: rnd(96)})
	roundTrip(t, QueryReq{Query: rnd(400), QuerySig: rnd(96)})
	roundTrip(t, QueryReq{Query: rnd(400), QuerySig: rnd(512), LegacySPKI: testEKSPKI(t)})
	roundTrip(t, QueryResp{Status: rnd(700), Bundles: [][]byte{bb}, DeviceStatus: rnd(500), DeviceBundles: [][]byte{rnd(900)}})
}

func TestQueryRespFailureForm(t *testing.T) {
	require.Equal(t, `["concord-account-authority",1,"query-resp","",[],"",[]]`, string(QueryRespFailure))
	require.Equal(t, QueryRespFailure, QueryResp{}.Encode())
	_, err := DecodeQueryResp(QueryRespFailure)
	require.NoError(t, err)
}

func TestEnvelopeTypeColumn(t *testing.T) {
	b := sampleBundle(t)
	b.ResultSig = rnd(95)
	require.Nil(t, b.Encode(), "95-byte P1363")
	b = sampleBundle(t)
	b.ResultSig = rnd(97)
	require.Nil(t, b.Encode(), "97-byte P1363")
	b = sampleBundle(t)
	b.LegacyPopSig = rnd(511)
	require.Nil(t, b.Encode(), "511-byte PSS")
	q := QueryReq{Query: rnd(10), QuerySig: rnd(97)}
	require.Nil(t, q.Encode())
	h := History{AccountID: newID(), Bundles: make([][]byte, HistoryPage+1)}
	for i := range h.Bundles {
		h.Bundles[i] = []byte{1}
	}
	require.Nil(t, h.Encode(), "HISTORY_PAGE + 1")
}

func TestEnvelopeWorstCases(t *testing.T) {
	require.Equal(t, 24388, layoutMax(&Bundle{}), "DoR §2.6 bundle worst case")
	require.LessOrEqual(t, layoutMax(&Bundle{}), 32768, "history/hold-req/head item cap")
	_, fs := (&Bundle{}).spec()
	for _, f := range fs {
		require.NotContains(t, f.name, "device", "no device signature is published (D-242)")
		require.NotContains(t, f.name, "actor")
	}
}

// envelopeQ is the hand-built wire form of one b position: standard padded
// base64 in quotes. An empty slice gives the empty string position.
func envelopeQ(b []byte) string { return `"` + base64.StdEncoding.EncodeToString(b) + `"` }

// envelopeList is the hand-built wire form of one l position.
func envelopeList(items ...[]byte) string {
	parts := make([]string, len(items))
	for i, b := range items {
		parts[i] = envelopeQ(b)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// envelopeWire hand-builds an envelope from already-formatted position
// fragments, so a wire fault cannot hide behind the codec's own writer.
func envelopeWire(tag string, positions ...string) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, `["concord-account-authority",1,%q`, tag)
	for _, p := range positions {
		sb.WriteString("," + p)
	}
	sb.WriteString("]")
	return []byte(sb.String())
}

// envelopeBundleNames is the bundle's positions 3..18 in wire order, written
// out here rather than read from the layout under test.
var envelopeBundleNames = []string{"intent", "sig_candidate", "sig_current_root", "sig_operator", "sig_recovery",
	"legacy_pop_sig", "legacy_spki", "ek_binding", "ek_binding_sig", "recovery_bind", "recovery_bind_sig",
	"cp_authz", "cp_authz_sig", "result", "result_sig", "held_status"}

// envelopeBundleFields names every bundle position's backing field.
func envelopeBundleFields(b *Bundle) map[string]*[]byte {
	return map[string]*[]byte{
		"intent": &b.Intent, "sig_candidate": &b.SigCandidate, "sig_current_root": &b.SigCurrentRoot,
		"sig_operator": &b.SigOperator, "sig_recovery": &b.SigRecovery, "legacy_pop_sig": &b.LegacyPopSig,
		"legacy_spki": &b.LegacySPKI, "ek_binding": &b.EKBinding, "ek_binding_sig": &b.EKBindingSig,
		"recovery_bind": &b.RecoveryBind, "recovery_bind_sig": &b.RecoveryBindSig, "cp_authz": &b.CPAuthz,
		"cp_authz_sig": &b.CPAuthzSig, "result": &b.Result, "result_sig": &b.ResultSig, "held_status": &b.HeldStatus,
	}
}

// envelopeBundleWire is the hand-built wire of b: one fragment per position,
// in envelopeBundleNames order.
func envelopeBundleWire(b *Bundle) []byte {
	fs := envelopeBundleFields(b)
	pos := make([]string, len(envelopeBundleNames))
	for i, n := range envelopeBundleNames {
		pos[i] = envelopeQ(*fs[n])
	}
	return envelopeWire("bundle", pos...)
}

// envelopeBundleWith returns sampleBundle with the named position set to v,
// and the hand-built wire of those very bytes.
func envelopeBundleWith(t testing.TB, name string, v []byte) (Bundle, []byte) {
	t.Helper()
	b := sampleBundle(t)
	p, ok := envelopeBundleFields(&b)[name]
	require.True(t, ok, "unknown bundle position %s", name)
	*p = v
	return b, envelopeBundleWire(&b)
}

// envelopeRefused requires err to be the codec's malformed refusal and to
// come from the named position, so a fault elsewhere in a hand-built wire
// cannot pass for the rule under test.
func envelopeRefused(t *testing.T, err error, field, why string) {
	t.Helper()
	require.ErrorIs(t, err, ErrMalformed, why)
	require.True(t, strings.HasPrefix(err.Error(), field+": "), "%s: want a refusal at %q, got %v", why, field, err)
}

// TestEnvelopeTypeColumnRefusedOnWire is TestEnvelopeTypeColumn's every
// refusal again, at the decoder, on wire built by hand.
func TestEnvelopeTypeColumnRefusedOnWire(t *testing.T) {
	// Controls first: a valid hand-built wire decodes, so a wire fault cannot
	// pass for the rule.
	b, wire := envelopeBundleWith(t, "result_sig", rnd(96))
	require.Equal(t, b.Encode(), wire, "hand-built bundle is the canonical one")
	_, err := DecodeBundle(wire)
	require.NoError(t, err, "bundle control")
	_, err = DecodeQueryReq(envelopeWire("query-req", envelopeQ(rnd(10)), envelopeQ(rnd(96)), `""`))
	require.NoError(t, err, "query-req control")
	id := newID()
	_, err = DecodeHistory(envelopeWire("history", `"`+id+`"`, envelopeList(envelopeItems(HistoryPage)...)))
	require.NoError(t, err, "history control")

	for _, c := range []struct {
		name, field string
		size        int
	}{
		{"95-byte P1363", "result_sig", 95},
		{"97-byte P1363", "result_sig", 97},
		{"511-byte PSS", "legacy_pop_sig", 511},
	} {
		_, w := envelopeBundleWith(t, c.field, rnd(c.size))
		_, err := DecodeBundle(w)
		envelopeRefused(t, err, c.field, c.name)
	}

	_, err = DecodeQueryReq(envelopeWire("query-req", envelopeQ(rnd(10)), envelopeQ(rnd(97)), `""`))
	envelopeRefused(t, err, "query_sig", "97-byte query_sig")

	_, err = DecodeHistory(envelopeWire("history", `"`+id+`"`, envelopeList(envelopeItems(HistoryPage+1)...)))
	envelopeRefused(t, err, "bundles", "HISTORY_PAGE + 1")
}

// envelopeBoundCase is one capped position of a published layout: field is
// the position a refusal must name, and limit is its cap in bytes (b) or
// items (l). try builds a value with that position at size n and returns
// what the encoder makes of it, the hand-built wire of the same bytes, and
// what the decoder makes of that wire.
type envelopeBoundCase struct {
	name, field string
	limit       int
	try         func(t *testing.T, n int) (encoded, wire []byte, decodeErr error)
}

func envelopeBundleBound(field string, limit int) envelopeBoundCase {
	return envelopeBoundCase{name: "bundle " + field, field: field, limit: limit,
		try: func(t *testing.T, n int) ([]byte, []byte, error) {
			b, wire := envelopeBundleWith(t, field, rnd(n))
			_, err := DecodeBundle(wire)
			return b.Encode(), wire, err
		}}
}

func envelopeItems(n int) [][]byte {
	items := make([][]byte, n)
	for i := range items {
		items[i] = []byte{1}
	}
	return items
}

func envelopeHistoryCase(name string, limit int, sized bool) envelopeBoundCase {
	return envelopeBoundCase{name: name, field: "bundles", limit: limit,
		try: func(_ *testing.T, n int) ([]byte, []byte, error) {
			items := envelopeItems(n)
			if sized {
				items = [][]byte{rnd(n)}
			}
			h := History{AccountID: newID(), Bundles: items}
			wire := envelopeWire("history", `"`+h.AccountID+`"`, envelopeList(items...))
			_, err := DecodeHistory(wire)
			return h.Encode(), wire, err
		}}
}

// envelopeQueryRespCase varies one query-resp position: the other three stay
// at their empty form.
func envelopeQueryRespCase(name, field string, limit int, build func(n int) (QueryResp, []string)) envelopeBoundCase {
	return envelopeBoundCase{name: name, field: field, limit: limit,
		try: func(_ *testing.T, n int) ([]byte, []byte, error) {
			m, pos := build(n)
			wire := envelopeWire("query-resp", pos...)
			_, err := DecodeQueryResp(wire)
			return m.Encode(), wire, err
		}}
}

func envelopeBoundCases() []envelopeBoundCase {
	return []envelopeBoundCase{
		envelopeBundleBound("held_status", 6144),
		envelopeBundleBound("intent", 2048),
		envelopeBundleBound("ek_binding", 2048),
		envelopeBundleBound("recovery_bind", 1024),
		envelopeBundleBound("cp_authz", 1024),
		envelopeBundleBound("result", 4096),
		envelopeHistoryCase("history bundles count", HistoryPage, false),
		envelopeHistoryCase("history bundle item", 32768, true),
		{name: "relay intent", field: "intent", limit: 2048,
			try: func(_ *testing.T, n int) ([]byte, []byte, error) {
				m := Relay{Intent: rnd(n), SigCurrentRoot: rnd(96)}
				wire := envelopeWire("relay", envelopeQ(m.Intent), envelopeQ(m.SigCurrentRoot), `""`, `""`)
				_, err := DecodeRelay(wire)
				return m.Encode(), wire, err
			}},
		{name: "query-req query", field: "query", limit: 1024,
			try: func(_ *testing.T, n int) ([]byte, []byte, error) {
				m := QueryReq{Query: rnd(n), QuerySig: rnd(96)}
				wire := envelopeWire("query-req", envelopeQ(m.Query), envelopeQ(m.QuerySig), `""`)
				_, err := DecodeQueryReq(wire)
				return m.Encode(), wire, err
			}},
		envelopeQueryRespCase("query-resp status", "status", 6144, func(n int) (QueryResp, []string) {
			m := QueryResp{Status: rnd(n)}
			return m, []string{envelopeQ(m.Status), "[]", `""`, "[]"}
		}),
		envelopeQueryRespCase("query-resp bundles count", "bundles", QueryRespBundles, func(n int) (QueryResp, []string) {
			m := QueryResp{Bundles: envelopeItems(n)}
			return m, []string{`""`, envelopeList(m.Bundles...), `""`, "[]"}
		}),
		envelopeQueryRespCase("query-resp bundle item", "bundles", 32768, func(n int) (QueryResp, []string) {
			m := QueryResp{Bundles: [][]byte{rnd(n)}}
			return m, []string{`""`, envelopeList(m.Bundles...), `""`, "[]"}
		}),
		envelopeQueryRespCase("query-resp device_status", "device_status", 2048, func(n int) (QueryResp, []string) {
			m := QueryResp{DeviceStatus: rnd(n)}
			return m, []string{`""`, "[]", envelopeQ(m.DeviceStatus), "[]"}
		}),
		envelopeQueryRespCase("query-resp device_bundles count", "device_bundles", DevicePage, func(n int) (QueryResp, []string) {
			m := QueryResp{DeviceBundles: envelopeItems(n)}
			return m, []string{`""`, "[]", `""`, envelopeList(m.DeviceBundles...)}
		}),
		envelopeQueryRespCase("query-resp device_bundles item", "device_bundles", 8192, func(n int) (QueryResp, []string) {
			m := QueryResp{DeviceBundles: [][]byte{rnd(n)}}
			return m, []string{`""`, "[]", `""`, envelopeList(m.DeviceBundles...)}
		}),
	}
}

// TestEnvelopeBounds pins every position cap from both sides: at the cap the
// encoder and the decoder accept the same bytes; one past it both refuse.
func TestEnvelopeBounds(t *testing.T) {
	for _, c := range envelopeBoundCases() {
		t.Run(c.name, func(t *testing.T) {
			enc, wire, err := c.try(t, c.limit)
			require.NoError(t, err, "at the cap, decode")
			require.NotNil(t, enc, "at the cap, encode")
			require.Equal(t, enc, wire, "the hand-built wire is the canonical encoding")

			enc, _, err = c.try(t, c.limit+1)
			require.Nil(t, enc, "cap + 1, encode")
			envelopeRefused(t, err, c.field, "cap + 1, decode")
		})
	}
}

// TestEnvelopeRequiredPositionsRefuseEmpty: none of these positions is
// b..|"", so the empty string is refused by encoder and decoder alike.
func TestEnvelopeRequiredPositionsRefuseEmpty(t *testing.T) {
	for _, field := range []string{"cp_authz", "result", "result_sig", "intent"} {
		t.Run("bundle "+field, func(t *testing.T) {
			valid := map[string]int{"cp_authz": 300, "result": 900, "result_sig": 96, "intent": 700}[field]
			b, wire := envelopeBundleWith(t, field, rnd(valid))
			require.NotNil(t, b.Encode(), "control, encode")
			_, err := DecodeBundle(wire)
			require.NoError(t, err, "control, decode")

			b, wire = envelopeBundleWith(t, field, nil)
			require.Nil(t, b.Encode(), "empty, encode")
			_, err = DecodeBundle(wire)
			envelopeRefused(t, err, field, "empty, decode")
		})
	}

	t.Run("relay intent", func(t *testing.T) {
		_, err := DecodeRelay(envelopeWire("relay", envelopeQ(rnd(10)), envelopeQ(rnd(96)), `""`, `""`))
		require.NoError(t, err, "control")
		require.Nil(t, Relay{SigCurrentRoot: rnd(96)}.Encode())
		_, err = DecodeRelay(envelopeWire("relay", `""`, envelopeQ(rnd(96)), `""`, `""`))
		envelopeRefused(t, err, "intent", "empty")
	})

	t.Run("query-req query and query_sig", func(t *testing.T) {
		_, err := DecodeQueryReq(envelopeWire("query-req", envelopeQ(rnd(10)), envelopeQ(rnd(96)), `""`))
		require.NoError(t, err, "control")
		require.Nil(t, QueryReq{Query: rnd(10)}.Encode(), "empty query_sig")
		_, err = DecodeQueryReq(envelopeWire("query-req", envelopeQ(rnd(10)), `""`, `""`))
		envelopeRefused(t, err, "query_sig", "empty query_sig")
		require.Nil(t, QueryReq{QuerySig: rnd(96)}.Encode(), "empty query")
		_, err = DecodeQueryReq(envelopeWire("query-req", `""`, envelopeQ(rnd(96)), `""`))
		envelopeRefused(t, err, "query", "empty query")
	})
}

// TestQueryRespFailureDecodes: the failure form is not only what the zero
// value encodes but what the decoder reads back, byte for byte.
func TestQueryRespFailureDecodes(t *testing.T) {
	got, err := DecodeQueryResp(QueryRespFailure)
	require.NoError(t, err)
	require.Equal(t, QueryRespFailure, got.Encode(), "decode then encode is byte-identical")
	require.Equal(t, QueryResp{}, got, "the failure form carries nothing")

	fresh := QueryResp{}.Encode()
	fresh[0] = 'X'
	require.Equal(t, byte('['), QueryRespFailure[0], "Encode returns a fresh copy, never the shared variable")
}

// envelopeBundleCaps is each bundle position's cap in bytes (DoR §2.6),
// written out here rather than read from the layout under test.
var envelopeBundleCaps = map[string]int{
	"intent": 2048, "sig_candidate": 96, "sig_current_root": 96, "sig_operator": 96, "sig_recovery": 96,
	"legacy_pop_sig": 512, "legacy_spki": 550, "ek_binding": 2048, "ek_binding_sig": 96,
	"recovery_bind": 1024, "recovery_bind_sig": 96, "cp_authz": 1024, "cp_authz_sig": 96,
	"result": 4096, "result_sig": 96, "held_status": 6144,
}

// envelopeMaximalBundle fills every bundle position to its cap, with a real
// SPKI in legacy_spki.
func envelopeMaximalBundle(t testing.TB) Bundle {
	t.Helper()
	var b Bundle
	for name, p := range envelopeBundleFields(&b) {
		*p = rnd(envelopeBundleCaps[name])
	}
	b.LegacySPKI = testEKSPKI(t)
	return b
}

// TestEnvelopeMaximalBundle proves the DoR §2.6 worst case on a real
// encoding rather than on the kinds table that TestEnvelopeWorstCases reads.
func TestEnvelopeMaximalBundle(t *testing.T) {
	b := envelopeMaximalBundle(t)
	fs := envelopeBundleFields(&b)
	require.Len(t, envelopeBundleCaps, len(envelopeBundleNames), "a cap for every position")
	for _, name := range envelopeBundleNames {
		limit, ok := envelopeBundleCaps[name]
		require.True(t, ok, "no cap written for %s", name)
		require.Len(t, *fs[name], limit, "%s is at its cap", name)
	}
	enc := b.Encode()
	require.Len(t, enc, 24388, "DoR §2.6 bundle worst case")
	require.Equal(t, envelopeBundleWire(&b), enc, "the hand-built wire is the canonical encoding")

	got, err := DecodeBundle(enc)
	require.NoError(t, err)
	require.Equal(t, b, got, "every position survives")
	require.Equal(t, enc, got.Encode(), "decode then encode is byte-identical")
}

// envelopeEmptyCase is one b..|"" position of a published layout. try builds
// a value with every position populated, blanks the one under test when
// blank is set, and returns what the encoder makes of it, the hand-built
// wire of the same bytes, and what the decoder makes of that wire.
type envelopeEmptyCase struct {
	name string
	try  func(t *testing.T, blank bool) (encoded, wire []byte, decodeErr error)
}

func envelopeBundleEmptyCase(field string) envelopeEmptyCase {
	return envelopeEmptyCase{name: "bundle " + field,
		try: func(t *testing.T, blank bool) ([]byte, []byte, error) {
			b := envelopeMaximalBundle(t)
			if blank {
				*envelopeBundleFields(&b)[field] = nil
			}
			wire := envelopeBundleWire(&b)
			_, err := DecodeBundle(wire)
			return b.Encode(), wire, err
		}}
}

func envelopeRelayEmptyCase(field string) envelopeEmptyCase {
	return envelopeEmptyCase{name: "relay " + field,
		try: func(t *testing.T, blank bool) ([]byte, []byte, error) {
			m := Relay{Intent: rnd(700), SigCurrentRoot: rnd(96), LegacyPopSig: rnd(512), LegacySPKI: testEKSPKI(t)}
			if blank {
				*map[string]*[]byte{"sig_current_root": &m.SigCurrentRoot, "legacy_pop_sig": &m.LegacyPopSig,
					"legacy_spki": &m.LegacySPKI}[field] = nil
			}
			wire := envelopeWire("relay", envelopeQ(m.Intent), envelopeQ(m.SigCurrentRoot),
				envelopeQ(m.LegacyPopSig), envelopeQ(m.LegacySPKI))
			_, err := DecodeRelay(wire)
			return m.Encode(), wire, err
		}}
}

func envelopeQueryRespEmptyCase(field string) envelopeEmptyCase {
	return envelopeEmptyCase{name: "query-resp " + field,
		try: func(_ *testing.T, blank bool) ([]byte, []byte, error) {
			m := QueryResp{Status: rnd(700), Bundles: [][]byte{rnd(900)}, DeviceStatus: rnd(500),
				DeviceBundles: [][]byte{rnd(900)}}
			if blank {
				*map[string]*[]byte{"status": &m.Status, "device_status": &m.DeviceStatus}[field] = nil
			}
			wire := envelopeWire("query-resp", envelopeQ(m.Status), envelopeList(m.Bundles...),
				envelopeQ(m.DeviceStatus), envelopeList(m.DeviceBundles...))
			_, err := DecodeQueryResp(wire)
			return m.Encode(), wire, err
		}}
}

func envelopeEmptyCases() []envelopeEmptyCase {
	var cases []envelopeEmptyCase
	// bundle positions 4-13, 15 and 18; 3, 14, 16 and 17 are required.
	for _, f := range []string{"sig_candidate", "sig_current_root", "sig_operator", "sig_recovery", "legacy_pop_sig",
		"legacy_spki", "ek_binding", "ek_binding_sig", "recovery_bind", "recovery_bind_sig", "cp_authz_sig", "held_status"} {
		cases = append(cases, envelopeBundleEmptyCase(f))
	}
	for _, f := range []string{"sig_current_root", "legacy_pop_sig", "legacy_spki"} {
		cases = append(cases, envelopeRelayEmptyCase(f))
	}
	cases = append(cases, envelopeEmptyCase{name: "query-req legacy_spki",
		try: func(t *testing.T, blank bool) ([]byte, []byte, error) {
			m := QueryReq{Query: rnd(400), QuerySig: rnd(512), LegacySPKI: testEKSPKI(t)}
			if blank {
				m.LegacySPKI = nil
			}
			wire := envelopeWire("query-req", envelopeQ(m.Query), envelopeQ(m.QuerySig), envelopeQ(m.LegacySPKI))
			_, err := DecodeQueryReq(wire)
			return m.Encode(), wire, err
		}})
	return append(cases, envelopeQueryRespEmptyCase("status"), envelopeQueryRespEmptyCase("device_status"))
}

// TestEnvelopeEmptyAdmittedPositions: every |"" position admits the empty
// string, on the encoder and on the decoder. cp_authz_sig = "" is the
// restore-cancel form (DoR §2.6, E3-09), so refusing it would be a defect.
func TestEnvelopeEmptyAdmittedPositions(t *testing.T) {
	for _, c := range envelopeEmptyCases() {
		t.Run(c.name, func(t *testing.T) {
			enc, wire, err := c.try(t, false)
			require.NoError(t, err, "populated control, decode")
			require.NotNil(t, enc, "populated control, encode")
			require.Equal(t, enc, wire, "populated control, canonical wire")

			enc, wire, err = c.try(t, true)
			require.NoError(t, err, "empty, decode")
			require.NotNil(t, enc, "empty, encode")
			require.Equal(t, enc, wire, "empty, canonical wire")
		})
	}
}

// envelopeSPKICase is one layout's legacy_spki position. try builds an
// otherwise valid value with that position set to spki, and returns what the
// encoder makes of it, the hand-built wire of the same bytes, and what the
// decoder makes of that wire.
type envelopeSPKICase struct {
	name string
	try  func(t *testing.T, spki []byte) (encoded, wire []byte, decodeErr error)
}

// TestEnvelopeLegacySPKIRefused: legacy_spki is b550 and a real EK SPKI
// wherever it appears. A wrong length, or the right length with no SPKI in
// it, is refused at the position.
func TestEnvelopeLegacySPKIRefused(t *testing.T) {
	cases := []envelopeSPKICase{
		{"bundle", func(t *testing.T, spki []byte) ([]byte, []byte, error) {
			b, wire := envelopeBundleWith(t, "legacy_spki", spki)
			_, err := DecodeBundle(wire)
			return b.Encode(), wire, err
		}},
		{"relay", func(_ *testing.T, spki []byte) ([]byte, []byte, error) {
			m := Relay{Intent: rnd(700), LegacyPopSig: rnd(512), LegacySPKI: spki}
			wire := envelopeWire("relay", envelopeQ(m.Intent), `""`, envelopeQ(m.LegacyPopSig), envelopeQ(m.LegacySPKI))
			_, err := DecodeRelay(wire)
			return m.Encode(), wire, err
		}},
		{"query-req", func(_ *testing.T, spki []byte) ([]byte, []byte, error) {
			m := QueryReq{Query: rnd(400), QuerySig: rnd(512), LegacySPKI: spki}
			wire := envelopeWire("query-req", envelopeQ(m.Query), envelopeQ(m.QuerySig), envelopeQ(m.LegacySPKI))
			_, err := DecodeQueryReq(wire)
			return m.Encode(), wire, err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc, wire, err := c.try(t, testEKSPKI(t))
			require.NoError(t, err, "valid SPKI control, decode")
			require.NotNil(t, enc, "valid SPKI control, encode")
			require.Equal(t, enc, wire, "valid SPKI control, canonical wire")

			for _, bad := range []struct {
				name string
				spki []byte
			}{
				{"549 bytes", rnd(549)},
				{"551 bytes", rnd(551)},
				{"550 bytes that are not an SPKI", rnd(550)},
			} {
				enc, _, err := c.try(t, bad.spki)
				require.Nil(t, enc, bad.name+", encode")
				envelopeRefused(t, err, "legacy_spki", bad.name+", decode")
			}
		})
	}
}
