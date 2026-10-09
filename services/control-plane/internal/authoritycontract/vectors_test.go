package authoritycontract

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type vectorFile struct {
	GeneratorSHA384 string   `json:"generator_sha384"`
	VectorsSHA384   string   `json:"vectors_sha384"`
	Vectors         []vector `json:"vectors"`
}

type vector struct {
	Name     string       `json:"name"`
	Type     string       `json:"type"`
	BytesB64 string       `json:"bytes_b64"`
	Input    vectorInput  `json:"input"`
	Expect   vectorExpect `json:"expect"`
}

type vectorExpect struct {
	Reason       string `json:"reason"`
	Verdict      string `json:"verdict"`
	ReencodedB64 string `json:"reencoded_b64"`
	TextB64      string `json:"text_b64"`
	DigestB64    string `json:"digest_b64"`
	DeviceID     string `json:"device_id"`
	SafetyNumber string `json:"safety_number"`
}

type vectorRecord struct {
	AccountID         string `json:"account_id"`
	LegacyEKDigestB64 string `json:"legacy_ek_digest_b64"`
}

type vectorInput struct {
	PointB64               string         `json:"point_b64"`
	MsgB64                 string         `json:"msg_b64"`
	SigB64                 string         `json:"sig_b64"`
	SPKIB64                string         `json:"spki_b64"`
	DSKB64                 string         `json:"dsk_b64"`
	PinDigestB64           string         `json:"pin_digest_b64"`
	ChainHeadB64           string         `json:"chain_head_b64"`
	AccountID              string         `json:"account_id"`
	UserID                 string         `json:"user_id"`
	RealmID                string         `json:"realm_id"`
	DeviceID               string         `json:"device_id"`
	SessionID              string         `json:"session_id"`
	Challenge              string         `json:"challenge"`
	Nonce                  string         `json:"nonce"`
	ClientVersion          string         `json:"client_version"`
	Epoch                  string         `json:"epoch"`
	EpochValid             bool           `json:"epoch_valid"`
	ValidAge               bool           `json:"valid_age"`
	NSFWAuth               bool           `json:"nsfw_auth"`
	JurisdictionObligation int            `json:"jurisdiction_obligation"`
	Timestamp              int64          `json:"timestamp"`
	IssuedAtMs             uint64         `json:"issued_at_ms"`
	Records                []vectorRecord `json:"records"`
	Layout                 string         `json:"layout"`
	Fields                 []string       `json:"fields"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	require.NoError(t, err)
	var f vectorFile
	require.NoError(t, json.Unmarshal(raw, &f))
	require.NotEmpty(t, f.Vectors)
	return f
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	require.NoError(t, err)
	return b
}

func wantOK(v vector) bool { return v.Expect.Reason == "ok" }

// §2.12's Reason → Verdict table, for the reasons A1's vectors use.
var verdictOf = map[string]string{"ok": "ok", "malformed": "withhold", "bad_signature": "withhold"}

var codecChecks = map[string]func(*testing.T, vector){
	"p384-point": checkPointVector, "p384-sig": checkP384SigVector, "pss-sig": checkPSSVector,
	"ek-spki": checkEKSPKIVector, "device-id": checkDeviceIDVector, "safety-number": checkSafetyNumberVector,
	"age-claim-v2": checkAgeClaimVector, "session-bind-v1": checkSessionBindVector,
	"credential-epoch-digest": checkEpochVector, "population-digest": checkPopulationVector,
	"tls-pin": checkTLSPinVector, "device-head": checkDeviceHeadVector, "layout-fields": checkLayoutFieldsVector,
}

// TestVectors is DoR §3.1's cross-language check: Go verifies the committed
// Node vectors and byte-equals its own re-encode.
func TestVectors(t *testing.T) {
	for _, v := range loadVectors(t).Vectors {
		t.Run(v.Name, func(t *testing.T) {
			want, known := verdictOf[v.Expect.Reason]
			require.True(t, known, "reason %q is not in §2.12's table for A1's vectors", v.Expect.Reason)
			require.Equal(t, want, v.Expect.Verdict, "§2.12 verdict for %q", v.Expect.Reason)
			check, ok := codecChecks[v.Type]
			if !ok {
				check = checkLayoutVector
			}
			check(t, v)
		})
	}
}

func checkLayoutVector(t *testing.T, v vector) {
	_, known := registry[v.Type]
	require.True(t, known, "unknown vector type %q", v.Type)
	m, err := Decode(v.Type, mustB64(t, v.BytesB64))
	if !wantOK(v) {
		require.ErrorIs(t, err, ErrMalformed)
		return
	}
	require.NoError(t, err)
	require.Equal(t, v.Expect.ReencodedB64, base64.StdEncoding.EncodeToString(m.Encode()))
}

func checkPointVector(t *testing.T, v vector) {
	_, err := ParsePoint(mustB64(t, v.Input.PointB64))
	if !wantOK(v) {
		require.ErrorIs(t, err, ErrMalformed)
		return
	}
	require.NoError(t, err)
}

func checkP384SigVector(t *testing.T, v vector) {
	got := VerifyP384(mustB64(t, v.Input.PointB64), mustB64(t, v.Input.MsgB64), mustB64(t, v.Input.SigB64))
	require.Equal(t, wantOK(v), got)
}

func checkPSSVector(t *testing.T, v vector) {
	got := VerifyLegacyPSS(mustB64(t, v.Input.SPKIB64), mustB64(t, v.Input.MsgB64), mustB64(t, v.Input.SigB64))
	require.Equal(t, wantOK(v), got)
}

func checkEKSPKIVector(t *testing.T, v vector) {
	_, err := ValidateEKSPKI(mustB64(t, v.Input.SPKIB64))
	if !wantOK(v) {
		require.ErrorIs(t, err, ErrMalformed)
		return
	}
	require.NoError(t, err)
}

func checkDeviceIDVector(t *testing.T, v vector) {
	require.True(t, wantOK(v), "DeviceID takes a [97]byte and cannot refuse: a negative device-id vector needs its own handler")
	b := mustB64(t, v.Input.DSKB64)
	require.Len(t, b, 97)
	var dsk [97]byte
	copy(dsk[:], b)
	require.Equal(t, v.Expect.DeviceID, DeviceID(dsk))
}

func checkSafetyNumberVector(t *testing.T, v vector) {
	require.Equal(t, v.Expect.SafetyNumber, SafetyNumber(v.Input.AccountID, mustB64(t, v.Input.PinDigestB64)))
}

func checkAgeClaimVector(t *testing.T, v vector) {
	in := v.Input
	got := AgeClaimV2Bytes(AgeClaimV2{UserID: in.UserID, ValidAge: in.ValidAge, NSFWAuth: in.NSFWAuth,
		JurisdictionObligation: in.JurisdictionObligation, Nonce: in.Nonce, Timestamp: in.Timestamp,
		DeviceID: in.DeviceID, ClientVersion: in.ClientVersion})
	if !wantOK(v) {
		require.Nil(t, got)
		return
	}
	require.Equal(t, v.Expect.TextB64, base64.StdEncoding.EncodeToString(got))
}

func checkSessionBindVector(t *testing.T, v vector) {
	in := v.Input
	got := SessionBindBytes(SessionBind{RealmID: in.RealmID, UserID: in.UserID, DeviceID: in.DeviceID,
		SessionID: in.SessionID, Challenge: in.Challenge, IssuedAtMs: in.IssuedAtMs})
	if !wantOK(v) {
		require.Nil(t, got)
		return
	}
	require.Equal(t, v.Expect.TextB64, base64.StdEncoding.EncodeToString(got))
}

func checkEpochVector(t *testing.T, v vector) {
	require.True(t, wantOK(v), "CredentialEpochDigest cannot refuse: a negative credential-epoch-digest vector needs its own handler")
	got := CredentialEpochDigest(v.Input.Epoch, v.Input.EpochValid)
	require.Equal(t, v.Expect.DigestB64, base64.StdEncoding.EncodeToString(got))
}

func checkPopulationVector(t *testing.T, v vector) {
	recs := make([]PopulationRecord, len(v.Input.Records))
	for i, r := range v.Input.Records {
		recs[i] = PopulationRecord{AccountID: r.AccountID, LegacyEKDigest: mustB64(t, r.LegacyEKDigestB64)}
	}
	got, err := PopulationDigest(recs)
	if !wantOK(v) {
		require.ErrorIs(t, err, ErrMalformed)
		return
	}
	require.NoError(t, err)
	require.Equal(t, v.Expect.DigestB64, base64.StdEncoding.EncodeToString(got))
}

func checkTLSPinVector(t *testing.T, v vector) {
	got, err := TLSPin(mustB64(t, v.Input.PointB64))
	if !wantOK(v) {
		require.ErrorIs(t, err, ErrMalformed)
		return
	}
	require.NoError(t, err)
	require.Equal(t, v.Expect.DigestB64, base64.StdEncoding.EncodeToString(got))
}

func checkDeviceHeadVector(t *testing.T, v vector) {
	dh, ch := mustB64(t, v.BytesB64), mustB64(t, v.Input.ChainHeadB64)
	h, err := ParseDeviceHead(dh, ch)
	if !wantOK(v) {
		require.ErrorIs(t, err, ErrMalformed)
		return
	}
	require.NoError(t, err)
	d, c := h.Bytes()
	require.Equal(t, dh, d)
	require.Equal(t, ch, c)
	require.Equal(t, v.Expect.ReencodedB64, base64.StdEncoding.EncodeToString(d))
}

// checkLayoutFieldsVector pins the position order of one layout against Node's
// LAYOUTS table. TestVectors decodes and re-encodes through the same spec(), so
// two same-kind rows swapped in spec() (primary_a/primary_b, realm_id/account_id)
// round-trip every other vector unchanged while writing different wire bytes; the
// names in position order are what such a swap cannot hide.
func checkLayoutFieldsVector(t *testing.T, v vector) {
	require.True(t, wantOK(v), "a layout-fields vector is never negative")
	ctor, known := registry[v.Input.Layout]
	require.True(t, known, "unknown layout %q", v.Input.Layout)
	tag, fields := ctor().spec()
	require.Equal(t, v.Input.Layout, tag)
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = f.name
	}
	require.Equal(t, v.Input.Fields, names, "%s: position names in wire order", v.Input.Layout)
}

// Every registered layout has exactly one layout-fields vector, and no vector
// names a layout the registry lacks.
func TestLayoutFieldsVectorForEveryTag(t *testing.T) {
	seen := map[string]int{}
	for _, v := range loadVectors(t).Vectors {
		if v.Type != "layout-fields" {
			continue
		}
		require.Equal(t, "prim/layout-fields/"+v.Input.Layout, v.Name)
		seen[v.Input.Layout]++
	}
	for _, tag := range Tags() {
		require.Equal(t, 1, seen[tag], "%s needs exactly one layout-fields vector", tag)
		delete(seen, tag)
	}
	require.Empty(t, seen, "layout-fields vectors for layouts the registry does not have")
}

// AC7: every layout has a positive and a negative vector.
func TestVectorsCoverEveryLayout(t *testing.T) {
	pos, neg := map[string]bool{}, map[string]bool{}
	for _, v := range loadVectors(t).Vectors {
		if wantOK(v) {
			pos[v.Type] = true
		} else {
			neg[v.Type] = true
		}
	}
	for _, tag := range Tags() {
		require.True(t, pos[tag], "%s has no positive vector", tag)
		require.True(t, neg[tag], "%s has no negative vector", tag)
	}
}

// The registry holds exactly the 30 Class F, 14 Class I and 2 persisted layouts.
func TestRegistryIsComplete(t *testing.T) {
	require.Equal(t, []string{"anchors", "bundle", "chain-head", "committed", "cp-authz", "cp-evidence", "delegation",
		"device-bundle", "device-committed", "device-head", "device-history", "device-intent", "device-result",
		"device-secrets", "device-set", "device-status", "device-submit", "device-wrap", "ek-binding", "error", "head",
		"history", "hold-req", "import-batch", "import-item", "intent", "legacy-batch", "legacy-ek-pop", "pending-heads",
		"query", "query-req", "query-resp", "recovery-bind", "refused", "relay", "relay-recover", "result", "sig",
		"signed", "signer", "status", "status-req", "statuses", "submit", "succession", "trust"}, Tags())
}

// The drift check's CP-job leg (DoR §3.1). gen.mjs is private (scripts/ is not
// mirrored); Go tests never run in the public mirror (public-ci-go.yml runs
// build and vet only), so a missing generator here is a failure, not a skip.
func TestVectorsGeneratorDigest(t *testing.T) {
	gen, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "scripts", "authority-vectors", "gen.mjs"))
	require.NoError(t, err)
	h := sha512.Sum384(gen)
	require.Equal(t, hex.EncodeToString(h[:]), loadVectors(t).GeneratorSHA384, "run: node scripts/authority-vectors/gen.mjs")
}

// The file's content leg: vectors_sha384 is SHA-384 over every byte after the
// header line, so an edit made without running gen.mjs fails here. It catches
// drift, not tampering; whoever edits the file can recompute it.
func TestVectorsContentDigest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	require.NoError(t, err)
	newline := bytes.IndexByte(raw, '\n')
	require.GreaterOrEqual(t, newline, 0, "vectors.json has a header line")
	// The digest covers only what follows the header line, so the line must be
	// exactly what gen.mjs writes: a vector placed on it would be uncommitted.
	require.Regexp(t, `^\{"generator_sha384":"[0-9a-f]{96}","vectors_sha384":"[0-9a-f]{96}","vectors":\[$`, string(raw[:newline]))
	h := sha512.Sum384(raw[newline+1:])
	require.Equal(t, hex.EncodeToString(h[:]), loadVectors(t).VectorsSHA384, "run: node scripts/authority-vectors/gen.mjs")
}

// Nested worst cases fit the positions that carry them (§2.6).
func TestEnvelopeNesting(t *testing.T) {
	d48 := digestOf("x")
	ds := DeviceStatus{RealmID: newID(), AccountID: newID(), ChainSeq: MaxSafeInt, ChainHeadDigest: d48, Dseq: MaxSafeInt,
		DeviceHeadDigest: d48, DeviceSetDigest: d48, AsOfMs: MaxSafeInt, ValidUntilMs: MaxSafeInt,
		DelegationGen: MaxSafeInt, DelegationSerial: MaxSafeInt}
	signedDS := Signed{Type: "device-status", Msg: ds.Encode(), SigA: rnd(96)}.Encode()
	require.NotNil(t, signedDS)
	require.LessOrEqual(t, len(signedDS), 2048, "head.device_status and query-resp.device_status are b≤2048")
	require.LessOrEqual(t, layoutMax(&Signed{}), 6144, "head.status, statuses and trust items are b≤6144")
}
