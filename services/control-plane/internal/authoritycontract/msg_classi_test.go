package authoritycontract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyRow builds a deterministic, canonical, sortable row (also used by the
// runtime-only legacy-batch boundary, which is too large for vectors.json).
func legacyRow(i int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-%012x.", i, i) +
		base64.StdEncoding.EncodeToString(Digest([]byte(fmt.Sprint(i))))
}

func TestClassIRoundTrip(t *testing.T) {
	roundTrip(t, Submit{Intent: rnd(700), SigCandidate: rnd(96), CPAuthz: rnd(300), CPAuthzSig: rnd(96),
		CPEvidence: rnd(200), DeviceIntent: rnd(1000), SigNewDevice: rnd(96)})
	roundTrip(t, ImportItem{Bundle: rnd(3000), CPEvidence: rnd(200)})
	roundTrip(t, ImportItem{DeviceBundle: rnd(2000), CPEvidence: rnd(200)})
	roundTrip(t, ImportBatch{Items: [][]byte{rnd(10), rnd(20)}})
	roundTrip(t, LegacyBatch{Rows: []string{legacyRow(2), legacyRow(1)}})
	roundTrip(t, Committed{Result: rnd(1000), ResultSig: rnd(96), DeviceResult: rnd(600), DeviceResultSig: rnd(96)})
	roundTrip(t, Committed{Result: rnd(1000), ResultSig: rnd(96)})
	roundTrip(t, Refused{RequestID: newID(), Code: string(RefusalDeviceTooNew)})
	roundTrip(t, ErrorEnvelope{Code: string(ErrorNotLeader)})
	roundTrip(t, Head{Status: rnd(800), LatestBundle: rnd(3000), DeviceStatus: rnd(500)})
	roundTrip(t, StatusReq{AccountIDs: []string{newID(), newID()}})
	roundTrip(t, HoldReq{Bundles: [][]byte{rnd(3000)}})
	roundTrip(t, PendingHeads{AccountIDs: []string{newID()}, Next: newID()})
	roundTrip(t, PendingHeads{})
	roundTrip(t, Signer{RealmID: newID(), DelegationSerial: 1, Role: "leader", RSKDigest: digestOf("rsk"),
		SubmitLayouts: []string{"submit/1", "submit/2"}, Trust: rnd(4000)})
	roundTrip(t, DeviceSubmit{DeviceIntent: rnd(1000), SigRoot: rnd(96), SigActor: rnd(96), SigNewDevice: rnd(96),
		CPAuthz: rnd(300), CPAuthzSig: rnd(96), CPEvidence: rnd(200)})
	roundTrip(t, DeviceCommitted{DeviceBundle: rnd(3000)})
}

func TestClassITypeColumn(t *testing.T) {
	require.Nil(t, Refused{RequestID: newID(), Code: "device_mismatch"}.Encode(), "LD-32 removed device_mismatch")
	require.Nil(t, ErrorEnvelope{Code: "teapot"}.Encode())
	require.Nil(t, Submit{Intent: rnd(10), CPAuthz: rnd(10), CPEvidence: rnd(10)}.Encode(), "submit cp_authz_sig is never empty")
	require.Nil(t, Signer{RealmID: newID(), DelegationSerial: 1, Role: "primary", RSKDigest: digestOf("r"),
		SubmitLayouts: []string{"s"}, Trust: rnd(1)}.Encode())
	// Valid base64 of the wrong length: a truncated string (the brief's [:100]) fails on
	// the base64 padding instead, so it never reached the length rule.
	require.Nil(t, LegacyBatch{Rows: []string{classiRow(newID(), rnd(47))}}.Encode(), "row digest must be 48 bytes, not 47")
	require.Nil(t, LegacyBatch{Rows: []string{classiRow(newID(), rnd(32))}}.Encode(), "row digest must be 48 bytes, not 32")
	require.Nil(t, StatusReq{}.Encode(), "status-req needs at least one account")
	require.ErrorIs(t, checkLegacyRow("0f8fad5b-d9cb-469f-a165-70867728950E."+base64.StdEncoding.EncodeToString(rnd(48))), ErrMalformed)
}

// Boundaries too large for vectors.json (> 128 KiB), so runtime-only here and
// in the generator's self-check (Task 13).
func TestClassIRuntimeBoundaries(t *testing.T) {
	rows := make([]string, 8192, 8193) // one spare slot for the over-cap append below
	for i := range rows {
		rows[i] = legacyRow(i)
	}
	roundTrip(t, LegacyBatch{Rows: rows})
	require.Nil(t, LegacyBatch{Rows: append(rows, legacyRow(8192))}.Encode())

	s := Signer{RealmID: newID(), DelegationSerial: 1, Role: "standby", RSKDigest: digestOf("r"), SubmitLayouts: []string{"s"},
		Trust: bytes.Repeat([]byte{7}, CapTrust)}
	roundTrip(t, s)
	s.Trust = append(s.Trust, 7)
	require.Nil(t, s.Encode())
}

func TestClassIWorstCases(t *testing.T) {
	require.Equal(t, 14963, layoutMax(&Submit{}), "DoR §2.6 submit")
	require.Equal(t, 7681, layoutMax(&DeviceSubmit{}), "DoR §2.6 device-submit")
	require.Equal(t, 56038, layoutMax(&ImportItem{}), "DoR §2.6 import-item")
	require.LessOrEqual(t, layoutMax(&ImportItem{}), 56320, "import-batch item cap")
	require.LessOrEqual(t, layoutMax(&Submit{}), 32*1024, "POST /v1/transactions body cap")
	require.LessOrEqual(t, layoutMax(&DeviceSubmit{}), 16*1024, "POST /v1/device-transactions body cap")
	// DoR §3.3 endpoint caps for the other bodies a Class I layout carries.
	require.LessOrEqual(t, layoutMax(&StatusReq{}), 32<<10, "POST /v1/status body cap")
	require.LessOrEqual(t, layoutMax(&HoldReq{}), 1<<20, "POST /v1/reconcile/required and /hold body cap")
	require.LessOrEqual(t, layoutMax(&LegacyBatch{}), 1<<20, "POST /v1/legacy-population body cap")
}

// The tests below are the review round's additions to the brief. The brief's own
// tests are all byte-level: they cannot see a spec() table whose pointers are
// consistently swapped (rowbind_test.go closes that package-wide), and they
// pinned the Type column only where a case happened to exercise it.

// classiRow builds a legacy-batch row around an arbitrary digest, for the rows
// legacyRow cannot make (a digest of the wrong length).
func classiRow(id string, digest []byte) string {
	return id + "." + base64.StdEncoding.EncodeToString(digest)
}

// classiIDs returns n distinct canonical UUIDs.
func classiIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
	}
	return ids
}

// classiBlobs returns n distinct small byte strings (list items need not be sorted or unique).
func classiBlobs(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = rnd(8)
	}
	return out
}

func classiB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// classiLowerID is a fixed canonical v4 UUID that contains hex letters, and
// classiUpperID is its upper-cased form. A random v4 UUID with no hex letter
// upper-cases to itself, so using one for an "uppercase" case would pass a
// valid value and fail the test about 2e-6 of the time.
const classiLowerID = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

var classiUpperID = strings.ToUpper(classiLowerID)

// The uppercase cases' premise: the fixture is accepted as written and the
// upper-cased form is a different string that s36 refuses.
func TestClassIUppercaseFixtureDiffersFromItsLowercase(t *testing.T) {
	require.NotEqual(t, classiLowerID, classiUpperID)
	_, ok := canonicalUUID(classiLowerID)
	require.True(t, ok, "the lowercase fixture is a canonical UUID")
	require.True(t, isUUIDv4(classiLowerID), "and a v4 one, so refused.request_id accepts it")
	_, ok = canonicalUUID(classiUpperID)
	require.False(t, ok, "the upper-cased fixture is not canonical")
}

// classiWire decodes a canonical encoding into its JSON array, so a test can read a
// field off the wire without going through the layout's own table.
func classiWire(t *testing.T, b []byte) []any {
	t.Helper()
	require.NotNil(t, b)
	var arr []any
	require.NoError(t, json.Unmarshal(b, &arr))
	require.GreaterOrEqual(t, len(arr), 3)
	return arr
}

// classiRequireWire requires b to be tag's envelope whose positions 3 onward are
// exactly want, in order. Numbers decode as float64.
func classiRequireWire(t *testing.T, tag string, b []byte, want ...any) {
	t.Helper()
	arr := classiWire(t, b)
	require.Equal(t, []any{Prefix, 1.0, tag}, arr[:3])
	require.Equal(t, want, arr[3:], tag)
}

// classiSubmit populates every position with a distinct value (the ones a
// `|""` position admits as empty included).
func classiSubmit(t testing.TB) Submit {
	return Submit{Intent: rnd(700), SigCandidate: rnd(96), SigCurrentRoot: rnd(96), SigOperator: rnd(96),
		SigRecovery: rnd(96), LegacyPopSig: rnd(512), LegacySPKI: testEKSPKI(t), EKBinding: rnd(600),
		EKBindingSig: rnd(96), RecoveryBind: rnd(400), RecoveryBindSig: rnd(96), CPAuthz: rnd(300), CPAuthzSig: rnd(96),
		CPEvidence: rnd(200), DeviceIntent: rnd(1000), SigActor: rnd(96), SigNewDevice: rnd(96)}
}

func classiDeviceSubmit() DeviceSubmit {
	return DeviceSubmit{DeviceIntent: rnd(1000), SigRoot: rnd(96), SigActor: rnd(96), SigNewDevice: rnd(96),
		RecoveryBind: rnd(400), SigRecovery: rnd(96), SigRecoveryPrior: rnd(96), CPAuthz: rnd(300), CPAuthzSig: rnd(96),
		CPEvidence: rnd(200)}
}

// classiSigner populates every Signer position. The plain u positions get distinct
// values. A u01 position can only be 0 or 1, so the seven of them alternate, and
// complement flips the alternation; that gives distinct bytes per position and
// nothing more. roundTrip compares bytes, so it would still pass if two rows were
// bound to each other's struct fields. A consistent pointer swap is caught by
// TestEveryRowBindsItsOwnStructField and TestClassIFieldsSitAtTheirDoRWirePositions.
// Two u01 fields that hold the same value in a sample (reconciled and disk_ready
// in the wire test's, for one pair) can be swapped without changing a byte, so
// for those TestEveryRowBindsItsOwnStructField is the only guard.
func classiSigner(complement uint64) Signer {
	bit := func(i uint64) uint64 { return (i + complement) % 2 }
	return Signer{RealmID: newID(), ProductRootGen: 11, DelegationSerial: 12, RestoreEpoch: 13, Reconciled: bit(0),
		MaxCommitID: 15, HeldCount: 16, LegacySealed: bit(1), PopulationDigest: rnd(48), TickAgeS: 19,
		CancelUnavailableMs: 20, ExtensionCapMs: 21, NCancelMs: 22, VMs: 23, Role: "leader", RSKDigest: rnd(48),
		DiskReady: bit(2), ReplicationRequired: bit(3), ReplicationReady: bit(4), ArchiveOK: bit(5), ClockSynced: bit(6),
		SubmitLayouts: []string{"submit/1", "submit/2"}, Trust: rnd(4000)}
}

func TestClassIRoundTripEveryPosition(t *testing.T) {
	roundTrip(t, classiSubmit(t))
	roundTrip(t, Submit{Intent: rnd(10), CPAuthz: rnd(10), CPAuthzSig: rnd(96), CPEvidence: rnd(10)}) // every `|""` empty
	roundTrip(t, classiDeviceSubmit())
	roundTrip(t, DeviceSubmit{CPAuthz: rnd(10), CPAuthzSig: rnd(96), CPEvidence: rnd(10)}) // every `|""` empty
	roundTrip(t, ImportItem{Bundle: rnd(3000), DeviceBundle: rnd(2000), CPEvidence: rnd(200)})
	roundTrip(t, Committed{Result: rnd(1000), ResultSig: rnd(96), DeviceResult: rnd(600), DeviceResultSig: rnd(96)})
	roundTrip(t, Head{Status: rnd(800), LatestBundle: nil, DeviceStatus: rnd(500)})
	roundTrip(t, Signer{RealmID: newID(), DelegationSerial: 1, Role: "leader", RSKDigest: rnd(48), SubmitLayouts: []string{"s"},
		Trust: rnd(10)}) // population_digest empty
	roundTrip(t, classiSigner(0))
	roundTrip(t, classiSigner(1))
	// u is 0 or [1-9][0-9]*: the top of the range round-trips on every plain u position.
	roundTrip(t, Signer{RealmID: newID(), ProductRootGen: MaxSafeInt, DelegationSerial: MaxSafeInt, RestoreEpoch: MaxSafeInt,
		MaxCommitID: MaxSafeInt, HeldCount: MaxSafeInt, TickAgeS: MaxSafeInt, CancelUnavailableMs: MaxSafeInt,
		ExtensionCapMs: MaxSafeInt, NCancelMs: MaxSafeInt, VMs: MaxSafeInt, Role: "standby", RSKDigest: rnd(48),
		SubmitLayouts: []string{"s"}, Trust: rnd(10)})
}

// A field sits at the wire position the DoR gives it. The expected arrays below
// are written in §2.6's order from the struct's own field names, so they do not
// go through the layout's table.
func TestClassIFieldsSitAtTheirDoRWirePositions(t *testing.T) {
	s := Signer{RealmID: newID(), ProductRootGen: 11, DelegationSerial: 12, RestoreEpoch: 13, Reconciled: 0, MaxCommitID: 15,
		HeldCount: 16, LegacySealed: 1, PopulationDigest: rnd(48), TickAgeS: 19, CancelUnavailableMs: 20, ExtensionCapMs: 21,
		NCancelMs: 22, VMs: 23, Role: "standby", RSKDigest: rnd(48), DiskReady: 0, ReplicationRequired: 1, ReplicationReady: 0,
		ArchiveOK: 1, ClockSynced: 0, SubmitLayouts: []string{"submit/1", "submit/2"}, Trust: rnd(300)}
	classiRequireWire(t, "signer", s.Encode(), s.RealmID, 11.0, 12.0, 13.0, 0.0, 15.0, 16.0, 1.0, classiB64(s.PopulationDigest),
		19.0, 20.0, 21.0, 22.0, 23.0, "standby", classiB64(s.RSKDigest), 0.0, 1.0, 0.0, 1.0, 0.0,
		[]any{"submit/1", "submit/2"}, classiB64(s.Trust))

	sub := classiSubmit(t)
	classiRequireWire(t, "submit", sub.Encode(), classiB64(sub.Intent), classiB64(sub.SigCandidate), classiB64(sub.SigCurrentRoot),
		classiB64(sub.SigOperator), classiB64(sub.SigRecovery), classiB64(sub.LegacyPopSig), classiB64(sub.LegacySPKI),
		classiB64(sub.EKBinding), classiB64(sub.EKBindingSig), classiB64(sub.RecoveryBind), classiB64(sub.RecoveryBindSig),
		classiB64(sub.CPAuthz), classiB64(sub.CPAuthzSig), classiB64(sub.CPEvidence), classiB64(sub.DeviceIntent),
		classiB64(sub.SigActor), classiB64(sub.SigNewDevice))

	ds := classiDeviceSubmit()
	classiRequireWire(t, "device-submit", ds.Encode(), classiB64(ds.DeviceIntent), classiB64(ds.SigRoot), classiB64(ds.SigActor),
		classiB64(ds.SigNewDevice), classiB64(ds.RecoveryBind), classiB64(ds.SigRecovery), classiB64(ds.SigRecoveryPrior),
		classiB64(ds.CPAuthz), classiB64(ds.CPAuthzSig), classiB64(ds.CPEvidence))

	cm := Committed{Result: rnd(1000), ResultSig: rnd(96), DeviceResult: rnd(600), DeviceResultSig: rnd(96)}
	classiRequireWire(t, "committed", cm.Encode(), classiB64(cm.Result), classiB64(cm.ResultSig), classiB64(cm.DeviceResult),
		classiB64(cm.DeviceResultSig))

	ii := ImportItem{Bundle: rnd(3000), DeviceBundle: rnd(2000), CPEvidence: rnd(200)}
	classiRequireWire(t, "import-item", ii.Encode(), classiB64(ii.Bundle), classiB64(ii.DeviceBundle), classiB64(ii.CPEvidence))

	hd := Head{Status: rnd(800), LatestBundle: rnd(3000), DeviceStatus: rnd(500)}
	classiRequireWire(t, "head", hd.Encode(), classiB64(hd.Status), classiB64(hd.LatestBundle), classiB64(hd.DeviceStatus))

	ids := classiIDs(2)
	classiRequireWire(t, "pending-heads", PendingHeads{AccountIDs: ids, Next: ids[1]}.Encode(),
		[]any{ids[0], ids[1]}, ids[1])
	rid := newID()
	classiRequireWire(t, "refused", Refused{RequestID: rid, Code: string(RefusalHeld)}.Encode(), rid, "held")
	classiRequireWire(t, "error", ErrorEnvelope{Code: string(ErrorDisk)}.Encode(), "disk")
}

// The worst cases are reachable on the wire, not only in layoutMax's arithmetic.
func TestClassIMaxedMessagesReachTheirWorstCase(t *testing.T) {
	sub := Submit{Intent: rnd(2048), SigCandidate: rnd(96), SigCurrentRoot: rnd(96), SigOperator: rnd(96), SigRecovery: rnd(96),
		LegacyPopSig: rnd(512), LegacySPKI: testEKSPKI(t), EKBinding: rnd(2048), EKBindingSig: rnd(96), RecoveryBind: rnd(1024),
		RecoveryBindSig: rnd(96), CPAuthz: rnd(1024), CPAuthzSig: rnd(96), CPEvidence: rnd(1024), DeviceIntent: rnd(2048),
		SigActor: rnd(96), SigNewDevice: rnd(96)}
	require.Len(t, roundTrip(t, sub), 14963, "DoR §2.6 submit")

	ds := DeviceSubmit{DeviceIntent: rnd(2048), SigRoot: rnd(96), SigActor: rnd(96), SigNewDevice: rnd(96), RecoveryBind: rnd(1024),
		SigRecovery: rnd(96), SigRecoveryPrior: rnd(96), CPAuthz: rnd(1024), CPAuthzSig: rnd(96), CPEvidence: rnd(1024)}
	require.Len(t, roundTrip(t, ds), 7681, "DoR §2.6 device-submit")

	ii := ImportItem{Bundle: rnd(32768), DeviceBundle: rnd(8192), CPEvidence: rnd(1024)}
	require.Len(t, roundTrip(t, ii), 56038, "DoR §2.6 import-item")

	require.Len(t, roundTrip(t, Committed{Result: rnd(4096), ResultSig: rnd(96), DeviceResult: rnd(1024), DeviceResultSig: rnd(96)}),
		layoutMax(&Committed{}))
	require.Len(t, roundTrip(t, Head{Status: rnd(6144), LatestBundle: rnd(32768), DeviceStatus: rnd(2048)}), layoutMax(&Head{}))
	require.Len(t, roundTrip(t, DeviceCommitted{DeviceBundle: rnd(8192)}), layoutMax(&DeviceCommitted{}))

	hold := make([][]byte, 16)
	for i := range hold {
		hold[i] = rnd(32768)
	}
	got := roundTrip(t, HoldReq{Bundles: hold})
	require.Len(t, got, layoutMax(&HoldReq{}))
	require.InDelta(t, 699_000, len(got), 500, "DoR §2.6 hold-req: about 699 KB")
}

// Array and list bounds, both ends, on the encode path and with the item caps.
func TestClassIArrayAndListBounds(t *testing.T) {
	roundTrip(t, StatusReq{AccountIDs: classiIDs(500)})
	require.Nil(t, StatusReq{AccountIDs: classiIDs(501)}.Encode())
	require.Nil(t, StatusReq{}.Encode(), "status-req needs at least one account")
	id := newID()
	require.Nil(t, StatusReq{AccountIDs: []string{id, id}}.Encode(), "a is strictly ascending, so a duplicate is refused")

	roundTrip(t, PendingHeads{AccountIDs: classiIDs(500)})
	require.Nil(t, PendingHeads{AccountIDs: classiIDs(501)}.Encode())

	roundTrip(t, ImportBatch{Items: classiBlobs(1)})
	roundTrip(t, ImportBatch{Items: classiBlobs(64)})
	require.Nil(t, ImportBatch{}.Encode(), "import-batch needs at least one item")
	require.Nil(t, ImportBatch{Items: classiBlobs(65)}.Encode())
	roundTrip(t, ImportBatch{Items: [][]byte{rnd(56320)}})
	require.Nil(t, ImportBatch{Items: [][]byte{rnd(56321)}}.Encode())

	roundTrip(t, HoldReq{Bundles: classiBlobs(1)})
	roundTrip(t, HoldReq{Bundles: classiBlobs(16)})
	require.Nil(t, HoldReq{}.Encode(), "hold-req needs at least one bundle")
	require.Nil(t, HoldReq{Bundles: classiBlobs(17)}.Encode())
	roundTrip(t, HoldReq{Bundles: [][]byte{rnd(32768)}})
	require.Nil(t, HoldReq{Bundles: [][]byte{rnd(32769)}}.Encode())

	require.Nil(t, LegacyBatch{}.Encode(), "legacy-batch needs at least one row")

	layouts := func(l ...string) []byte {
		m := classiSigner(0)
		m.SubmitLayouts = l
		return m.Encode()
	}
	require.NotNil(t, layouts("a"))
	require.NotNil(t, layouts("a", "b", "c", "d", "e", "f", "g", "h"))
	require.Nil(t, layouts(), "signer.submit_layouts needs at least one entry")
	require.Nil(t, layouts("a", "b", "c", "d", "e", "f", "g", "h", "i"))
	require.NotNil(t, layouts(strings.Repeat("a", 16)))
	require.Nil(t, layouts(strings.Repeat("a", 17)))
	require.Nil(t, layouts("a", "a"), "a duplicate layout name")
	require.Nil(t, layouts("a,b"), "a comma is not an s character")
}

func TestClassIRefusesValuesOutsideTheTypeColumn(t *testing.T) {
	spki := testEKSPKI(t)
	v1 := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	require.NotNil(t, classiSubmit(t).Encode())
	require.NotNil(t, classiDeviceSubmit().Encode())
	require.NotNil(t, classiSigner(0).Encode())

	// Every base is valid, so each case differs from a valid message in one position.
	sub := func(f func(*Submit)) []byte { m := classiSubmit(t); f(&m); return m.Encode() }
	for name, got := range map[string][]byte{
		"submit intent empty":            sub(func(m *Submit) { m.Intent = nil }),
		"submit intent 2049":             sub(func(m *Submit) { m.Intent = rnd(2049) }),
		"submit sig_candidate 95":        sub(func(m *Submit) { m.SigCandidate = rnd(95) }),
		"submit sig_current_root 97":     sub(func(m *Submit) { m.SigCurrentRoot = rnd(97) }),
		"submit sig_operator 95":         sub(func(m *Submit) { m.SigOperator = rnd(95) }),
		"submit sig_recovery 97":         sub(func(m *Submit) { m.SigRecovery = rnd(97) }),
		"submit legacy_pop_sig 511":      sub(func(m *Submit) { m.LegacyPopSig = rnd(511) }),
		"submit legacy_pop_sig 513":      sub(func(m *Submit) { m.LegacyPopSig = rnd(513) }),
		"submit legacy_spki not an SPKI": sub(func(m *Submit) { m.LegacySPKI = rnd(550) }),
		"submit legacy_spki 549":         sub(func(m *Submit) { m.LegacySPKI = spki[:549] }),
		"submit ek_binding 2049":         sub(func(m *Submit) { m.EKBinding = rnd(2049) }),
		"submit ek_binding_sig 95":       sub(func(m *Submit) { m.EKBindingSig = rnd(95) }),
		"submit recovery_bind 1025":      sub(func(m *Submit) { m.RecoveryBind = rnd(1025) }),
		"submit recovery_bind_sig 97":    sub(func(m *Submit) { m.RecoveryBindSig = rnd(97) }),
		"submit cp_authz empty":          sub(func(m *Submit) { m.CPAuthz = nil }),
		"submit cp_authz 1025":           sub(func(m *Submit) { m.CPAuthz = rnd(1025) }),
		"submit cp_authz_sig empty":      sub(func(m *Submit) { m.CPAuthzSig = nil }),
		"submit cp_authz_sig 95":         sub(func(m *Submit) { m.CPAuthzSig = rnd(95) }),
		"submit cp_evidence empty":       sub(func(m *Submit) { m.CPEvidence = nil }),
		"submit cp_evidence 1025":        sub(func(m *Submit) { m.CPEvidence = rnd(1025) }),
		"submit device_intent 2049":      sub(func(m *Submit) { m.DeviceIntent = rnd(2049) }),
		"submit sig_actor 95":            sub(func(m *Submit) { m.SigActor = rnd(95) }),
		"submit sig_new_device 97":       sub(func(m *Submit) { m.SigNewDevice = rnd(97) }),
	} {
		require.Nil(t, got, name)
	}

	ds := func(f func(*DeviceSubmit)) []byte { m := classiDeviceSubmit(); f(&m); return m.Encode() }
	for name, got := range map[string][]byte{
		"device-submit device_intent 2049":    ds(func(m *DeviceSubmit) { m.DeviceIntent = rnd(2049) }),
		"device-submit sig_root 95":           ds(func(m *DeviceSubmit) { m.SigRoot = rnd(95) }),
		"device-submit sig_actor 97":          ds(func(m *DeviceSubmit) { m.SigActor = rnd(97) }),
		"device-submit sig_new_device 95":     ds(func(m *DeviceSubmit) { m.SigNewDevice = rnd(95) }),
		"device-submit recovery_bind 1025":    ds(func(m *DeviceSubmit) { m.RecoveryBind = rnd(1025) }),
		"device-submit sig_recovery 97":       ds(func(m *DeviceSubmit) { m.SigRecovery = rnd(97) }),
		"device-submit sig_recovery_prior 95": ds(func(m *DeviceSubmit) { m.SigRecoveryPrior = rnd(95) }),
		"device-submit cp_authz empty":        ds(func(m *DeviceSubmit) { m.CPAuthz = nil }),
		"device-submit cp_authz 1025":         ds(func(m *DeviceSubmit) { m.CPAuthz = rnd(1025) }),
		"device-submit cp_authz_sig empty":    ds(func(m *DeviceSubmit) { m.CPAuthzSig = nil }),
		"device-submit cp_authz_sig 95":       ds(func(m *DeviceSubmit) { m.CPAuthzSig = rnd(95) }),
		"device-submit cp_evidence empty":     ds(func(m *DeviceSubmit) { m.CPEvidence = nil }),
		"device-submit cp_evidence 1025":      ds(func(m *DeviceSubmit) { m.CPEvidence = rnd(1025) }),
	} {
		require.Nil(t, got, name)
	}

	cm := func(f func(*Committed)) []byte {
		m := Committed{Result: rnd(1000), ResultSig: rnd(96), DeviceResult: rnd(600), DeviceResultSig: rnd(96)}
		f(&m)
		return m.Encode()
	}
	hd := func(f func(*Head)) []byte {
		m := Head{Status: rnd(800), LatestBundle: rnd(3000), DeviceStatus: rnd(500)}
		f(&m)
		return m.Encode()
	}
	ii := func(f func(*ImportItem)) []byte {
		m := ImportItem{Bundle: rnd(3000), DeviceBundle: rnd(2000), CPEvidence: rnd(200)}
		f(&m)
		return m.Encode()
	}
	for name, got := range map[string][]byte{
		"import-item bundle 32769":        ii(func(m *ImportItem) { m.Bundle = rnd(32769) }),
		"import-item device_bundle 8193":  ii(func(m *ImportItem) { m.DeviceBundle = rnd(8193) }),
		"import-item cp_evidence 1025":    ii(func(m *ImportItem) { m.CPEvidence = rnd(1025) }),
		"committed result empty":          cm(func(m *Committed) { m.Result = nil }),
		"committed result 4097":           cm(func(m *Committed) { m.Result = rnd(4097) }),
		"committed result_sig empty":      cm(func(m *Committed) { m.ResultSig = nil }),
		"committed result_sig 95":         cm(func(m *Committed) { m.ResultSig = rnd(95) }),
		"committed device_result 1025":    cm(func(m *Committed) { m.DeviceResult = rnd(1025) }),
		"committed device_result_sig 97":  cm(func(m *Committed) { m.DeviceResultSig = rnd(97) }),
		"head status empty":               hd(func(m *Head) { m.Status = nil }),
		"head status 6145":                hd(func(m *Head) { m.Status = rnd(6145) }),
		"head latest_bundle 32769":        hd(func(m *Head) { m.LatestBundle = rnd(32769) }),
		"head device_status empty":        hd(func(m *Head) { m.DeviceStatus = nil }),
		"head device_status 2049":         hd(func(m *Head) { m.DeviceStatus = rnd(2049) }),
		"device-committed bundle empty":   DeviceCommitted{}.Encode(),
		"device-committed bundle 8193":    DeviceCommitted{DeviceBundle: rnd(8193)}.Encode(),
		"refused request_id empty":        Refused{Code: string(RefusalHeld)}.Encode(),
		"refused request_id v1":           Refused{RequestID: v1, Code: string(RefusalHeld)}.Encode(),
		"refused request_id uppercase":    Refused{RequestID: classiUpperID, Code: string(RefusalHeld)}.Encode(),
		"refused request_id not a uuid":   Refused{RequestID: "request", Code: string(RefusalHeld)}.Encode(),
		"refused code empty":              Refused{RequestID: newID()}.Encode(),
		"refused code from the error set": Refused{RequestID: newID(), Code: string(ErrorMalformed)}.Encode(),
		"error code empty":                ErrorEnvelope{}.Encode(),
		"error code from the refusal set": ErrorEnvelope{Code: string(RefusalHeld)}.Encode(),
		"status-req uppercase id":         StatusReq{AccountIDs: []string{classiUpperID}}.Encode(),
		"status-req not a uuid":           StatusReq{AccountIDs: []string{"account"}}.Encode(),
		"pending-heads uppercase next":    PendingHeads{Next: classiUpperID}.Encode(),
		"pending-heads next not a uuid":   PendingHeads{Next: "next"}.Encode(),
		"pending-heads bad account id":    PendingHeads{AccountIDs: []string{"account"}}.Encode(),
	} {
		require.Nil(t, got, name)
	}

	sg := func(f func(*Signer)) []byte { m := classiSigner(0); f(&m); return m.Encode() }
	for name, got := range map[string][]byte{
		"signer realm_id empty":             sg(func(m *Signer) { m.RealmID = "" }),
		"signer realm_id uppercase":         sg(func(m *Signer) { m.RealmID = classiUpperID }),
		"signer product_root_gen past 2^53": sg(func(m *Signer) { m.ProductRootGen = MaxSafeInt + 1 }),
		"signer delegation_serial 0":        sg(func(m *Signer) { m.DelegationSerial = 0 }),
		"signer reconciled 2":               sg(func(m *Signer) { m.Reconciled = 2 }),
		"signer legacy_sealed 2":            sg(func(m *Signer) { m.LegacySealed = 2 }),
		"signer population_digest 47":       sg(func(m *Signer) { m.PopulationDigest = rnd(47) }),
		"signer population_digest 49":       sg(func(m *Signer) { m.PopulationDigest = rnd(49) }),
		"signer role empty":                 sg(func(m *Signer) { m.Role = "" }),
		"signer role primary":               sg(func(m *Signer) { m.Role = "primary" }),
		"signer rsk_digest empty":           sg(func(m *Signer) { m.RSKDigest = nil }),
		"signer rsk_digest 47":              sg(func(m *Signer) { m.RSKDigest = rnd(47) }),
		"signer disk_ready 2":               sg(func(m *Signer) { m.DiskReady = 2 }),
		"signer replication_required 2":     sg(func(m *Signer) { m.ReplicationRequired = 2 }),
		"signer replication_ready 2":        sg(func(m *Signer) { m.ReplicationReady = 2 }),
		"signer archive_ok 2":               sg(func(m *Signer) { m.ArchiveOK = 2 }),
		"signer clock_synced 2":             sg(func(m *Signer) { m.ClockSynced = 2 }),
		"signer trust empty":                sg(func(m *Signer) { m.Trust = nil }),
	} {
		require.Nil(t, got, name)
	}
}

// legacy-batch's row syntax is "<uuid>.<b64 of a 48-byte digest>", checked on the
// function directly and through the layout.
func TestClassILegacyRowSyntax(t *testing.T) {
	require.NoError(t, checkLegacyRow(legacyRow(1)))
	id := newID()
	require.NoError(t, checkLegacyRow(classiRow(id, rnd(48))))
	for name, row := range map[string]string{
		"no dot":                 id + classiB64(rnd(48)),
		"second dot":             classiRow(id, rnd(48)) + ".x",
		"empty digest":           id + ".",
		"digest of 47 bytes":     classiRow(id, rnd(47)),
		"digest of 49 bytes":     classiRow(id, rnd(49)),
		"digest of 32 bytes":     classiRow(id, rnd(32)),
		"digest of 1 byte":       classiRow(id, rnd(1)),
		"digest not base64":      id + "." + strings.Repeat("!", 64),
		"uppercase id":           classiRow(classiUpperID, rnd(48)),
		"id not a uuid":          classiRow("not-a-uuid", rnd(48)),
		"empty id":               classiRow("", rnd(48)),
		"braced id":              classiRow("{"+id+"}", rnd(48)),
		"id without its hyphens": classiRow(strings.ReplaceAll(id, "-", ""), rnd(48)),
	} {
		require.ErrorIs(t, checkLegacyRow(row), ErrMalformed, name)
	}
	for _, digest := range [][]byte{rnd(47), rnd(49), rnd(32), rnd(1)} {
		require.Nil(t, LegacyBatch{Rows: []string{classiRow(id, digest)}}.Encode(), "%d-byte digest", len(digest))
	}
}

// classiDocKind renders a position's kind in the DoR's §2.3 notation, so a layout
// can be compared with §2.6 as written. u>0 and u≥1 are one kind and render u≥1.
// A trailing * marks a position with a value validator: every b550 is an EK SPKI
// (§2.4) and a legacy-batch row is checked against "<uuid>.<b64 digest>".
func classiDocKind(k kind) string {
	var s string
	switch k.tag {
	case tagStr:
		s = fmt.Sprintf("s≤%d", k.maxLen)
		if k.strChk != nil {
			s += "*"
		}
	case tagUUID:
		s = "s36"
	case tagUUIDv4:
		s = "s36v4"
	case tagUint:
		return classiDocUint(k)
	case tagBytes:
		switch {
		case len(k.sizes) == 1 && k.upTo == 0:
			s = fmt.Sprintf("b%d", k.sizes[0])
		case len(k.sizes) == 0 && k.upTo > 0:
			s = fmt.Sprintf("b≤%d", k.upTo)
		default:
			return fmt.Sprintf("b?%v≤%d", k.sizes, k.upTo)
		}
		if k.binChk != nil {
			s += "*"
		}
	case tagEnum:
		return classiEnum(k.enum...)
	case tagArr:
		return fmt.Sprintf("a<%s>[%d..%d]", classiDocKind(*k.elem), k.minN, k.maxN)
	case tagList:
		return fmt.Sprintf("l<%s>[%d..%d]", classiDocKind(*k.elem), k.minN, k.maxN)
	default:
		return fmt.Sprintf("?%d", k.tag)
	}
	if k.empty {
		s += `|""`
	}
	return s
}

func classiDocUint(k kind) string {
	switch {
	case k.minU == 0 && k.maxU == 1:
		return "u01"
	case k.minU == 1 && k.maxU == MaxSafeInt:
		return "u≥1"
	case k.minU == 0 && k.maxU == MaxSafeInt:
		return "u"
	}
	return fmt.Sprintf("u[%d..%d]", k.minU, k.maxU)
}

// classiEnum renders an e{…} set sorted, so the DoR's own order can be transcribed.
func classiEnum(vals ...string) string {
	vals = slices.Clone(vals)
	slices.Sort(vals)
	for i, v := range vals {
		if v == "" {
			vals[i] = `""`
		}
	}
	return "e{" + strings.Join(vals, ",") + "}"
}

type classiDocRow struct{ name, typ string }

var (
	// DoR §3.3 "Refusal codes (27, recorded)", in the DoR's order.
	classiDocRefusals = []string{"authz_out_of_window", "device_key_bound", "device_limit", "device_too_new", "device_unknown",
		"ek_generation", "ek_mismatch", "ek_reused", "expired", "expiry_out_of_range", "held", "in_population",
		"insufficient_authorization", "legacy_mismatch", "malformed_for_op", "policy_disabled", "predecessor_mismatch",
		"recovery_key_bound", "recovery_key_unbound", "root_bound_elsewhere", "root_revoked", "root_reused",
		"stale_device_seq", "stale_head", "stale_recovery_gen", "stale_seq", "wrong_state"}
	// DoR §3.3 "Error enum", in the DoR's order.
	classiDocErrors = []string{"malformed", "bad_signature", "bad_authz", "request_conflict", "too_large", "store", "signer",
		"clock", "reconcile", "not_sealed", "busy", "not_ready", "not_leader", "replication", "disk"}
)

// classiDocLayouts is DoR §2.6 Class I as written: each layout's element count and
// the (name, Type) of every position from 3 on. The submit rows are "the bundle's
// positions 3–15" with cp_authz_sig typed b96 (never ""), then the three private
// positions. refused.request_id is untyped in §2.6; it echoes the intent's
// request_id, which §2.5 types s36v4.
var classiDocLayouts = []struct {
	m     specer
	count int
	rows  []classiDocRow
}{
	{&Submit{}, 20, []classiDocRow{
		{"intent", "b≤2048"}, {"sig_candidate", `b96|""`}, {"sig_current_root", `b96|""`}, {"sig_operator", `b96|""`},
		{"sig_recovery", `b96|""`}, {"legacy_pop_sig", `b512|""`}, {"legacy_spki", `b550*|""`}, {"ek_binding", `b≤2048|""`},
		{"ek_binding_sig", `b96|""`}, {"recovery_bind", `b≤1024|""`}, {"recovery_bind_sig", `b96|""`}, {"cp_authz", "b≤1024"},
		{"cp_authz_sig", "b96"}, {"cp_evidence", "b≤1024"}, {"device_intent", `b≤2048|""`}, {"sig_actor", `b96|""`},
		{"sig_new_device", `b96|""`},
	}},
	{&ImportItem{}, 6, []classiDocRow{
		{"bundle", `b≤32768|""`}, {"device_bundle", `b≤8192|""`}, {"cp_evidence", `b≤1024|""`},
	}},
	{&ImportBatch{}, 4, []classiDocRow{{"items", "l<b≤56320>[1..64]"}}},
	{&LegacyBatch{}, 4, []classiDocRow{{"rows", "a<s≤101*>[1..8192]"}}},
	{&Committed{}, 7, []classiDocRow{
		{"result", "b≤4096"}, {"result_sig", "b96"}, {"device_result", `b≤1024|""`}, {"device_result_sig", `b96|""`},
	}},
	{&Refused{}, 5, []classiDocRow{{"request_id", "s36v4"}, {"code", classiEnum(classiDocRefusals...)}}},
	{&ErrorEnvelope{}, 4, []classiDocRow{{"code", classiEnum(classiDocErrors...)}}},
	{&Head{}, 6, []classiDocRow{
		{"status", "b≤6144"}, {"latest_bundle", `b≤32768|""`}, {"device_status", "b≤2048"},
	}},
	{&StatusReq{}, 4, []classiDocRow{{"account_ids", "a<s36>[1..500]"}}},
	{&HoldReq{}, 4, []classiDocRow{{"bundles", "l<b≤32768>[1..16]"}}},
	{&PendingHeads{}, 5, []classiDocRow{{"account_ids", "a<s36>[0..500]"}, {"next", `s36|""`}}},
	{&Signer{}, 26, []classiDocRow{
		{"realm_id", "s36"}, {"product_root_gen", "u"}, {"delegation_serial", "u≥1"}, {"restore_epoch", "u"},
		{"reconciled", "u01"}, {"max_commit_id", "u"}, {"held_count", "u"}, {"legacy_sealed", "u01"},
		{"population_digest", `b48|""`}, {"tick_age_s", "u"}, {"cancel_unavailable_ms", "u"}, {"extension_cap_ms", "u"},
		{"n_cancel_ms", "u"}, {"v_ms", "u"}, {"role", classiEnum("leader", "standby")}, {"rsk_digest", "b48"},
		{"disk_ready", "u01"}, {"replication_required", "u01"}, {"replication_ready", "u01"}, {"archive_ok", "u01"},
		{"clock_synced", "u01"}, {"submit_layouts", "a<s≤16>[1..8]"}, {"trust", "b≤2097152"},
	}},
	{&DeviceSubmit{}, 13, []classiDocRow{
		{"device_intent", `b≤2048|""`}, {"sig_root", `b96|""`}, {"sig_actor", `b96|""`}, {"sig_new_device", `b96|""`},
		{"recovery_bind", `b≤1024|""`}, {"sig_recovery", `b96|""`}, {"sig_recovery_prior", `b96|""`}, {"cp_authz", "b≤1024"},
		{"cp_authz_sig", "b96"}, {"cp_evidence", "b≤1024"},
	}},
	{&DeviceCommitted{}, 4, []classiDocRow{{"device_bundle", "b≤8192"}}},
}

func TestClassILayoutsMatchTheDoRTable(t *testing.T) {
	require.Len(t, classiDocLayouts, 14)
	require.Len(t, classiDocRefusals, 27)
	require.Len(t, classiDocErrors, 15)
	seen := map[string]bool{}
	for _, d := range classiDocLayouts {
		tag, fs := d.m.spec()
		require.False(t, seen[tag], "%s listed twice", tag)
		seen[tag] = true
		require.Contains(t, Tags(), tag, "%s must be registered", tag)
		require.Equal(t, d.count, 3+len(fs), "%s element count", tag)
		require.Len(t, fs, len(d.rows), tag)
		for i, f := range fs {
			require.Equal(t, d.rows[i].name, f.name, "%s position %d", tag, 3+i)
			require.Equal(t, d.rows[i].typ, classiDocKind(f.k), "%s.%s (position %d)", tag, f.name, 3+i)
		}
	}
}
