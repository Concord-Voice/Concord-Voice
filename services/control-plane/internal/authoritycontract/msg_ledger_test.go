package authoritycontract

import (
	"encoding/base64"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testDev(t testing.TB, mature uint64) DeviceEntry {
	var e DeviceEntry
	copy(e.DSK[:], testPoint(t))
	copy(e.DKX[:], testPoint(t))
	e.ID = DeviceID(e.DSK)
	e.MatureAtMs = mature
	return e
}

func sampleDeviceSet(t testing.TB) DeviceSet {
	return DeviceSet{RealmID: newID(), AccountID: newID(), Active: []string{testDev(t, 1).Dev(), testDev(t, 2).Dev()}}
}

func sampleBind(t testing.TB) RecoveryBind {
	return RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: newID(), Kind: BindKindBind,
		RecoveryKey: testPoint(t), ChainSeq: 3, ChainEntryDigest: digestOf("e"), BindingRootDigest: digestOf("r"),
		EscrowPublicKey: testPoint(t)}
}

func TestLedgerRoundTrip(t *testing.T) {
	set := sampleDeviceSet(t)
	roundTrip(t, set)
	refresh := sampleDeviceSet(t)
	refresh.RecoveryKey, refresh.RecoveryBindDigest = testPoint(t), digestOf("b1")
	refresh.ReplacementKey, refresh.ReplacementBindDigest, refresh.ReplacementNbMs = testPoint(t), digestOf("b2"), 1_700_000_000_000
	roundTrip(t, refresh)
	unbind := refresh
	unbind.ReplacementKey = nil
	roundTrip(t, unbind)
	firstBind := sampleDeviceSet(t)
	firstBind.ReplacementKey, firstBind.ReplacementBindDigest, firstBind.ReplacementNbMs = testPoint(t), digestOf("b"), 5
	firstBind.Pending = testDev(t, 9).Dev()
	roundTrip(t, firstBind)

	n := testDev(t, 0)
	roundTrip(t, DeviceIntent{RealmID: newID(), AccountID: newID(), RequestID: newID(), Op: OpAddDevice, Salt: rnd(32),
		ExpiresAtMs: 1, ExpectedSeq: 2, ExpectedHeadDigest: digestOf("h"), ExpectedDseq: 3, ExpectedDeviceHead: digestOf("d"),
		ActorDeviceID: testDev(t, 0).ID, NewDeviceID: n.ID, NewDeviceDSK: n.DSK[:], NewDeviceDKX: n.DKX[:],
		RemovedDeviceIDs: []string{newID(), newID()}, BindWait: BindWaitDelta})
	roundTrip(t, DeviceResult{RealmID: newID(), AccountID: newID(), RequestID: newID(), DeviceIntentDigest: digestOf("D"),
		CPAuthzDigest: digestOf("c"), Op: OpAddDevice, Dseq: 4, PrevDeviceEntryDigest: digestOf("p"), ChainSeq: 2,
		ChainEntryDigest: digestOf("ce"), DeviceSetDigest: digestOf("s"), IssuedAtMs: 1, DelegationSerial: 1})
	roundTrip(t, DeviceStatus{RealmID: newID(), AccountID: newID(), ChainSeq: 2, ChainHeadDigest: digestOf("h"), Dseq: 4,
		DeviceHeadDigest: digestOf("d"), DeviceSetDigest: digestOf("s"), AsOfMs: 1, ValidUntilMs: 2, DelegationSerial: 1})
	db := roundTrip(t, DeviceBundle{DeviceIntent: rnd(900), SigRoot: rnd(96), SigActor: rnd(96), SigNewDevice: rnd(96),
		CPAuthz: rnd(400), CPAuthzSig: rnd(96), DeviceResult: rnd(600), DeviceResultSig: rnd(96)})
	roundTrip(t, DeviceHistory{AccountID: newID(), Bundles: [][]byte{db}})
	roundTrip(t, sampleBind(t))
	cancel := RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: newID(), Kind: BindKindCancel,
		ExpiresAtMs: 9, RecoveryKey: testPoint(t)}
	roundTrip(t, cancel)
	unb := RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: newID(), Kind: BindKindUnbind,
		ChainSeq: 1, ChainEntryDigest: digestOf("e"), BindingRootDigest: digestOf("r")}
	roundTrip(t, unb)
}

func TestDevBoundaries(t *testing.T) {
	at319 := testDev(t, MaxSafeInt)
	require.Len(t, at319.Dev(), 319)
	got, err := ParseDev(at319.Dev())
	require.NoError(t, err)
	require.Equal(t, at319, got)
	require.NoError(t, kDev.checkString(at319.Dev()))

	e := testDev(t, 0)
	at320 := strings.TrimSuffix(e.Dev(), ".0") + "." + strconv.FormatUint(10_000_000_000_000_000, 10)
	require.Len(t, at320, 320)
	require.ErrorIs(t, kDev.checkString(at320), ErrMalformed)
	_, err = ParseDev(at320)
	require.ErrorIs(t, err, ErrMalformed)

	wrongID := strings.Replace(e.Dev(), e.ID, newID(), 1)
	_, err = ParseDev(wrongID)
	require.ErrorIs(t, err, ErrMalformed, "device_id must equal DeviceID(dsk)")
	hyb := e
	copy(hyb.DKX[:], hybridOf(e.DKX[:]))
	_, err = ParseDev(hyb.Dev())
	require.ErrorIs(t, err, ErrMalformed, "dkx is a point too")
	_, err = ParseDev(e.ID + ".x.y")
	require.ErrorIs(t, err, ErrMalformed)
}

func TestDeviceSetCrossRules(t *testing.T) {
	for name, mut := range map[string]func(*DeviceSet){
		"key without digest":       func(s *DeviceSet) { s.RecoveryKey = testPoint(t) },
		"digest without key":       func(s *DeviceSet) { s.RecoveryBindDigest = digestOf("b") },
		"slot digest without nb":   func(s *DeviceSet) { s.ReplacementKey, s.ReplacementBindDigest = testPoint(t), digestOf("b") },
		"nb without slot digest":   func(s *DeviceSet) { s.ReplacementNbMs = 5 },
		"slot key without digest":  func(s *DeviceSet) { s.ReplacementKey = testPoint(t) },
		"unbind with no bound key": func(s *DeviceSet) { s.ReplacementBindDigest, s.ReplacementNbMs = digestOf("b"), 5 },
		"refresh to the bound key": func(s *DeviceSet) {
			k := testPoint(t)
			s.RecoveryKey, s.RecoveryBindDigest = k, digestOf("a")
			s.ReplacementKey, s.ReplacementBindDigest, s.ReplacementNbMs = k, digestOf("b"), 5
		},
		"pending in active": func(s *DeviceSet) { s.Pending = s.Active[0] },
		"eleven devices": func(s *DeviceSet) {
			for len(s.Active) < MaxDevices+1 {
				s.Active = append(s.Active, testDev(t, 0).Dev())
			}
		},
	} {
		s := sampleDeviceSet(t)
		mut(&s)
		require.Nil(t, s.Encode(), name)
	}
	s := sampleDeviceSet(t)
	b := string(s.Encode())
	d, err := DecodeDeviceSet([]byte(b))
	require.NoError(t, err)
	swapped := strings.Replace(b, `"`+d.Active[0]+`","`+d.Active[1]+`"`, `"`+d.Active[1]+`","`+d.Active[0]+`"`, 1)
	_, err = DecodeDeviceSet([]byte(swapped))
	require.ErrorIs(t, err, ErrMalformed, "active strictly ascending")
}

// TestLedgerDeviceSetDuplicateIDs is DoR §2.5's "IDs are pairwise distinct
// (V-10)". Strict ascending order does not imply it: the same device with
// mature_at_ms 5 and 6 is two strictly ascending entries on one device_id.
func TestLedgerDeviceSetDuplicateIDs(t *testing.T) {
	five := testDev(t, 5)
	six := five
	six.MatureAtMs = 6
	require.Less(t, five.Dev(), six.Dev(), "the counter-example is strictly ascending")
	other := testDev(t, 1)

	_, err := Marshal(&DeviceSet{RealmID: newID(), AccountID: newID(), Active: []string{five.Dev(), six.Dev()}})
	require.ErrorIs(t, err, ErrMalformed, "adjacent duplicate id")
	_, err = Marshal(&DeviceSet{RealmID: newID(), AccountID: newID(), Active: []string{six.Dev(), other.Dev(), five.Dev()}})
	require.ErrorIs(t, err, ErrMalformed, "duplicate id in a caller's unsorted input")
	require.Nil(t, DeviceSet{RealmID: newID(), AccountID: newID(), Active: []string{five.Dev(), other.Dev(), six.Dev()}}.Encode())

	wire := `["` + Prefix + `",1,"device-set","` + newID() + `","` + newID() + `",["` + five.Dev() + `","` + six.Dev() + `"],"","","","","",0]`
	_, err = DecodeDeviceSet([]byte(wire))
	require.ErrorIs(t, err, ErrMalformed, "a strictly ascending wire array with one id twice")

	ok := DeviceSet{RealmID: newID(), AccountID: newID(), Active: []string{five.Dev(), other.Dev()}}
	roundTrip(t, ok)
}

// ledgerSetWire writes s as a device-set wire array WITHOUT validating it, so a
// refusal can be asserted on decode as well as on Encode. active is sorted: the
// wire is canonical, so a decode refusal is the rule under test and not the
// ordering. For a valid s it equals s.Encode(), which the callers assert.
func ledgerSetWire(s DeviceSet) []byte {
	q := func(b []byte) string { return `"` + base64.StdEncoding.EncodeToString(b) + `"` }
	active := make([]string, 0, len(s.Active))
	for _, d := range slices.Sorted(slices.Values(s.Active)) {
		active = append(active, `"`+d+`"`)
	}
	return []byte(`["` + Prefix + `",1,"device-set","` + s.RealmID + `","` + s.AccountID + `",[` +
		strings.Join(active, ",") + `],"` + s.Pending + `",` + q(s.RecoveryKey) + `,` + q(s.RecoveryBindDigest) + `,` +
		q(s.ReplacementKey) + `,` + q(s.ReplacementBindDigest) + `,` + strconv.FormatUint(s.ReplacementNbMs, 10) + `]`)
}

// TestLedgerDeviceSetPendingByID pins the semantics of "pending is not in
// active" (§2.5): by device_id. The same device at another maturity, or with
// another (valid) dkx, is a different `dev` string but the same device, so it
// is refused as pending on Encode and on decode of hand-built wire.
func TestLedgerDeviceSetPendingByID(t *testing.T) {
	a, b := testDev(t, 1), testDev(t, 2)
	pendingOf := func(e DeviceEntry) DeviceSet {
		return DeviceSet{RealmID: newID(), AccountID: newID(), Active: []string{a.Dev(), b.Dev()}, Pending: e.Dev()}
	}

	// control: a third device is a legal pending, and the wire builder writes the encoder's bytes
	fresh := pendingOf(testDev(t, 3))
	require.Equal(t, fresh.Encode(), ledgerSetWire(fresh))
	roundTrip(t, fresh)
	_, err := DecodeDeviceSet(ledgerSetWire(fresh))
	require.NoError(t, err)

	for name, e := range map[string]DeviceEntry{"first": a, "second": b} {
		later := e
		later.MatureAtMs = e.MatureAtMs + 10
		otherDKX := e
		copy(otherDKX.DKX[:], testPoint(t))
		require.NotEqual(t, e.Dev(), otherDKX.Dev())
		require.Equal(t, e.ID, otherDKX.ID, "a different dkx does not change the device_id")
		require.NoError(t, kDev.checkString(otherDKX.Dev()), "the other dkx is a valid dev")

		for variant, pending := range map[string]DeviceEntry{
			"the same entry": e, "same id, another maturity": later, "same id, another dkx": otherDKX,
		} {
			s := pendingOf(pending)
			label := name + " active entry: " + variant
			require.Nil(t, s.Encode(), label)
			_, err := Marshal(&s)
			require.ErrorIs(t, err, ErrMalformed, label)
			_, err = DecodeDeviceSet(ledgerSetWire(s))
			require.ErrorIs(t, err, ErrMalformed, label)
		}
	}
}

// TestLedgerRecoveryStates enumerates all 32 present/absent combinations of the
// five recovery positions and requires that exactly the five states D-251
// permits are accepted (and round-trip byte for byte); every other
// combination is refused on Encode and on decode of hand-built wire.
func TestLedgerRecoveryStates(t *testing.T) {
	const (
		pKey        = 1 << iota // recovery_key
		pKeyDigest              // recovery_bind_digest
		pSlotKey                // replacement_key
		pSlotDigest             // replacement_bind_digest
		pSlotNb                 // replacement_nb_ms != 0
	)
	bound, replacement := testPoint(t), testPoint(t)
	steps := []func(*DeviceSet){ // in the order of the bits above
		func(s *DeviceSet) { s.RecoveryKey = bound },
		func(s *DeviceSet) { s.RecoveryBindDigest = digestOf("b1") },
		func(s *DeviceSet) { s.ReplacementKey = replacement },
		func(s *DeviceSet) { s.ReplacementBindDigest = digestOf("b2") },
		func(s *DeviceSet) { s.ReplacementNbMs = 5 },
	}
	permitted := map[int]string{
		0:                                "no recovery state",
		pKey | pKeyDigest:                "bound key, empty slot",
		pSlotKey | pSlotDigest | pSlotNb: "pending first bind",
		pKey | pKeyDigest | pSlotDigest | pSlotNb:            "pending unbind",
		pKey | pKeyDigest | pSlotKey | pSlotDigest | pSlotNb: "pending refresh",
	}
	require.Len(t, permitted, 5, "D-251 permits five states")
	require.Len(t, steps, 5)

	for mask := range 1 << len(steps) {
		s := sampleDeviceSet(t)
		for i, set := range steps {
			if mask&(1<<i) != 0 {
				set(&s)
			}
		}
		name := "mask " + strconv.FormatInt(int64(mask), 2)
		if state, ok := permitted[mask]; ok {
			enc := roundTrip(t, s)
			require.Equal(t, enc, ledgerSetWire(s), state)
			continue
		}
		require.Nil(t, s.Encode(), name)
		_, err := DecodeDeviceSet(ledgerSetWire(s))
		require.ErrorIs(t, err, ErrMalformed, name)
	}

	// a refresh to the bound key is not a refresh
	same := sampleDeviceSet(t)
	for _, set := range steps {
		set(&same)
	}
	same.ReplacementKey = same.RecoveryKey
	require.Nil(t, same.Encode(), "refresh to the bound key")
	_, err := DecodeDeviceSet(ledgerSetWire(same))
	require.ErrorIs(t, err, ErrMalformed, "refresh to the bound key")
}

func TestRecoveryBindEscrowKey(t *testing.T) {
	b := sampleBind(t)
	b.EscrowPublicKey = b.RecoveryKey
	require.Nil(t, b.Encode(), "escrow key equal to the recovery key (LD-53)")
	b = sampleBind(t)
	b.EscrowPublicKey = hybridOf(b.EscrowPublicKey)
	require.Nil(t, b.Encode(), "escrow key not an uncompressed point")
	b = sampleBind(t)
	b.Kind = "rebind"
	require.Nil(t, b.Encode())
}

func TestDeviceSetDigest(t *testing.T) {
	b := sampleDeviceSet(t).Encode()
	got, err := DeviceSetDigest(b)
	require.NoError(t, err)
	require.Equal(t, Digest(b), got)
	_, err = DeviceSetDigest(append(append([]byte(nil), b...), ' '))
	require.ErrorIs(t, err, ErrMalformed)
}

func TestLedgerWorstCases(t *testing.T) {
	require.Equal(t, 4087, layoutMax(&DeviceSet{}), "DoR §2.5 device-set")
	require.Equal(t, 7812, layoutMax(&DeviceBundle{}), "DoR §2.6 device-bundle")
	require.Equal(t, 174921, layoutMax(&DeviceHistory{}), "DoR §2.3 device-history")
	require.Equal(t, 678, layoutMax(&RecoveryBind{}), "DoR §2.22 recovery-bind (LD-53)")
	require.LessOrEqual(t, layoutMax(&DeviceIntent{}), CapDeviceIntent)
	require.LessOrEqual(t, layoutMax(&DeviceResult{}), CapDeviceResult)
	require.LessOrEqual(t, layoutMax(&DeviceStatus{}), 2048)
	require.Equal(t, 14, 3+len(func() []field { _, fs := (&RecoveryBind{}).spec(); return fs }()), "LD-53: 14 elements")
}

// TestLedgerParseDevRefusals pins each part of a `dev`: the id, both points
// and the maturity, and that a fifth part is not one.
func TestLedgerParseDevRefusals(t *testing.T) {
	e := testDev(t, 3)
	p := strings.Split(e.Dev(), ".")
	require.Len(t, p, 4)
	join := func(id, dsk, dkx, mature string) string { return id + "." + dsk + "." + dkx + "." + mature }
	hybrid := base64.StdEncoding.EncodeToString(hybridOf(e.DSK[:]))
	require.Equal(t, e.Dev(), join(p[0], p[1], p[2], p[3]))
	for name, s := range map[string]string{
		"dsk is a hybrid point":     join(p[0], hybrid, p[2], p[3]),
		"dsk is not base64":         join(p[0], "!!", p[2], p[3]),
		"dkx is not base64":         join(p[0], p[1], "!!", p[3]),
		"id is not a v4 uuid":       join("00000000-0000-0000-0000-000000000000", p[1], p[2], p[3]),
		"maturity with a leading 0": join(p[0], p[1], p[2], "03"),
		"maturity is not a number":  join(p[0], p[1], p[2], "x"),
		"maturity past 2^53-1":      join(p[0], p[1], p[2], strconv.FormatUint(MaxSafeInt+1, 10)),
		"empty maturity":            join(p[0], p[1], p[2], ""),
		"five parts":                e.Dev() + ".1",
		"empty":                     "",
	} {
		_, err := ParseDev(s)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

// TestLedgerParseDevBoundsWorkBeforeSplit pins that the 319-character bound is
// checked before strings.Split: ParseDev is exported, and splitting a long
// string of separators would allocate about 16 MiB for it.
func TestLedgerParseDevBoundsWorkBeforeSplit(t *testing.T) {
	long := strings.Repeat(".", 1<<20)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := ParseDev(long)
	runtime.ReadMemStats(&after)
	require.ErrorIs(t, err, ErrMalformed)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<16), "ParseDev allocated in proportion to its input")
}

// ledgerDecodes requires the typed decoder to return want from enc, and to
// refuse the same bytes with a trailing byte, returning the zero value.
func ledgerDecodes[T any](t *testing.T, want T, enc []byte, decode func([]byte) (T, error)) {
	t.Helper()
	got, err := decode(enc)
	require.NoError(t, err)
	require.Equal(t, want, got)
	zero, err := decode(append(slices.Clone(enc), ' '))
	require.ErrorIs(t, err, ErrMalformed)
	var none T
	require.Equal(t, none, zero, "a failed decode returns the zero value")
}

// TestLedgerTypedDecoders drives every exported DecodeT of the family.
func TestLedgerTypedDecoders(t *testing.T) {
	set := sampleDeviceSet(t)
	set.Active = slices.Sorted(slices.Values(set.Active))
	ledgerDecodes(t, set, set.Encode(), DecodeDeviceSet)

	n := testDev(t, 0)
	intent := DeviceIntent{RealmID: newID(), AccountID: newID(), RequestID: newID(), Op: OpAddDevice, Salt: rnd(32),
		ExpiresAtMs: 1, ExpectedSeq: 2, ExpectedHeadDigest: digestOf("h"), ExpectedDseq: 3, ExpectedDeviceHead: digestOf("d"),
		ActorDeviceID: testDev(t, 0).ID, NewDeviceID: n.ID, NewDeviceDSK: n.DSK[:], NewDeviceDKX: n.DKX[:],
		RemovedDeviceIDs: []string{newID()}, BindWait: BindWaitNow}
	ledgerDecodes(t, intent, intent.Encode(), DecodeDeviceIntent)

	result := DeviceResult{RealmID: newID(), AccountID: newID(), RequestID: newID(), DeviceIntentDigest: digestOf("D"),
		Op: OpRotateRoot, Dseq: 4, PrevDeviceEntryDigest: digestOf("p"), ChainSeq: 2, ChainEntryDigest: digestOf("ce"),
		DeviceSetDigest: digestOf("s"), IssuedAtMs: 1, DelegationGen: 7, DelegationSerial: 1}
	ledgerDecodes(t, result, result.Encode(), DecodeDeviceResult)

	status := DeviceStatus{RealmID: newID(), AccountID: newID(), ChainSeq: 2, ChainHeadDigest: digestOf("h"), Dseq: 4,
		DeviceHeadDigest: digestOf("d"), DeviceSetDigest: digestOf("s"), AsOfMs: 1, ValidUntilMs: 2, DelegationGen: 3,
		DelegationSerial: 1}
	ledgerDecodes(t, status, status.Encode(), DecodeDeviceStatus)

	bundle := DeviceBundle{DeviceIntent: rnd(900), SigRoot: rnd(96), RecoveryBind: rnd(300), SigRecovery: rnd(96),
		DeviceResult: rnd(600), DeviceResultSig: rnd(96)}
	ledgerDecodes(t, bundle, bundle.Encode(), DecodeDeviceBundle)

	history := DeviceHistory{AccountID: newID(), Bundles: [][]byte{bundle.Encode(), rnd(5)}}
	ledgerDecodes(t, history, history.Encode(), DecodeDeviceHistory)

	bind := sampleBind(t)
	ledgerDecodes(t, bind, bind.Encode(), DecodeRecoveryBind)
}

// TestLedgerWorstCaseMessages reaches each DoR-exact worst case with a real,
// valid message, so the figures are not only layoutMax arithmetic.
func TestLedgerWorstCaseMessages(t *testing.T) {
	active := make([]string, 0, MaxDevices)
	for range MaxDevices {
		active = append(active, testDev(t, MaxSafeInt).Dev())
	}
	set := DeviceSet{RealmID: newID(), AccountID: newID(), Active: active, Pending: testDev(t, MaxSafeInt).Dev(),
		RecoveryKey: testPoint(t), RecoveryBindDigest: digestOf("b1"), ReplacementKey: testPoint(t),
		ReplacementBindDigest: digestOf("b2"), ReplacementNbMs: MaxSafeInt}
	require.Len(t, roundTrip(t, set), 4087, "device-set at every maximum")

	bundle := DeviceBundle{DeviceIntent: rnd(2048), SigRoot: rnd(96), SigActor: rnd(96), SigNewDevice: rnd(96),
		RecoveryBind: rnd(1024), SigRecovery: rnd(96), SigRecoveryPrior: rnd(96), CPAuthz: rnd(1024),
		CPAuthzSig: rnd(96), DeviceResult: rnd(1024), DeviceResultSig: rnd(96)}
	require.Len(t, roundTrip(t, bundle), 7812, "device-bundle at every maximum")

	bundles := make([][]byte, 0, DevicePage)
	for range DevicePage {
		bundles = append(bundles, rnd(8192))
	}
	require.Len(t, roundTrip(t, DeviceHistory{AccountID: newID(), Bundles: bundles}), 174921, "device-history at every maximum")

	bind := RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: newID(), Kind: BindKindCancel,
		ExpiresAtMs: MaxSafeInt, RecoveryKey: testPoint(t), TargetDigest: digestOf("t"), ChainSeq: MaxSafeInt,
		ChainEntryDigest: digestOf("e"), BindingRootDigest: digestOf("r"), EscrowPublicKey: testPoint(t)}
	require.Len(t, roundTrip(t, bind), 678, "recovery-bind at every maximum")
}

// TestLedgerTypeColumns pins the Type column of the private-ledger layouts:
// u minima, enum sets, array bounds, fixed sizes and the opaque nested caps.
func TestLedgerTypeColumns(t *testing.T) {
	intent := func(mut func(*DeviceIntent)) DeviceIntent {
		m := DeviceIntent{RealmID: newID(), AccountID: newID(), RequestID: newID(), Op: OpRotateEK, Salt: rnd(32)}
		mut(&m)
		return m
	}
	result := func(mut func(*DeviceResult)) DeviceResult {
		m := DeviceResult{RealmID: newID(), AccountID: newID(), RequestID: newID(), DeviceIntentDigest: digestOf("D"),
			Op: OpAddDevice, Dseq: 1, ChainSeq: 1, ChainEntryDigest: digestOf("c"), DeviceSetDigest: digestOf("s"),
			DelegationSerial: 1}
		mut(&m)
		return m
	}
	status := func(mut func(*DeviceStatus)) DeviceStatus {
		m := DeviceStatus{RealmID: newID(), AccountID: newID(), DeviceSetDigest: digestOf("s"), DelegationSerial: 1}
		mut(&m)
		return m
	}
	bundle := func(mut func(*DeviceBundle)) DeviceBundle {
		m := DeviceBundle{DeviceResult: rnd(600), DeviceResultSig: rnd(96)}
		mut(&m)
		return m
	}
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = newID()
		}
		return out
	}

	// the minimal valid message of each layout, so every refusal below is the one mutation
	roundTrip(t, intent(func(*DeviceIntent) {}))
	roundTrip(t, result(func(*DeviceResult) {}))
	roundTrip(t, status(func(*DeviceStatus) {}))
	roundTrip(t, bundle(func(*DeviceBundle) {}))
	roundTrip(t, intent(func(m *DeviceIntent) { m.RemovedDeviceIDs = ids(9) }))
	roundTrip(t, bundle(func(m *DeviceBundle) { m.DeviceIntent, m.RecoveryBind, m.CPAuthz = rnd(2048), rnd(1024), rnd(1024) }))

	for name, enc := range map[string][]byte{
		"intent: a published-only op":           intent(func(m *DeviceIntent) { m.Op = "suspend" }).Encode(),
		"intent: empty op":                      intent(func(m *DeviceIntent) { m.Op = "" }).Encode(),
		"intent: short salt":                    intent(func(m *DeviceIntent) { m.Salt = rnd(31) }).Encode(),
		"intent: no salt":                       intent(func(m *DeviceIntent) { m.Salt = nil }).Encode(),
		"intent: ten removed ids":               intent(func(m *DeviceIntent) { m.RemovedDeviceIDs = ids(10) }).Encode(),
		"intent: unknown bind_wait":             intent(func(m *DeviceIntent) { m.BindWait = "later" }).Encode(),
		"intent: short head digest":             intent(func(m *DeviceIntent) { m.ExpectedHeadDigest = rnd(47) }).Encode(),
		"intent: actor id is not a v4 uuid":     intent(func(m *DeviceIntent) { m.ActorDeviceID = "00000000-0000-0000-0000-000000000000" }).Encode(),
		"intent: dsk is not a point":            intent(func(m *DeviceIntent) { m.NewDeviceDSK = rnd(97) }).Encode(),
		"intent: dkx is a hybrid point":         intent(func(m *DeviceIntent) { m.NewDeviceDKX = hybridOf(testPoint(t)) }).Encode(),
		"result: dseq 0":                        result(func(m *DeviceResult) { m.Dseq = 0 }).Encode(),
		"result: chain_seq 0":                   result(func(m *DeviceResult) { m.ChainSeq = 0 }).Encode(),
		"result: delegation_serial 0":           result(func(m *DeviceResult) { m.DelegationSerial = 0 }).Encode(),
		"result: no device_intent_digest":       result(func(m *DeviceResult) { m.DeviceIntentDigest = nil }).Encode(),
		"result: no chain_entry_digest":         result(func(m *DeviceResult) { m.ChainEntryDigest = nil }).Encode(),
		"result: no device_set_digest":          result(func(m *DeviceResult) { m.DeviceSetDigest = nil }).Encode(),
		"result: a published-only op":           result(func(m *DeviceResult) { m.Op = "erase" }).Encode(),
		"result: request_id is not a v4 uuid":   result(func(m *DeviceResult) { m.RequestID = "00000000-0000-0000-0000-000000000000" }).Encode(),
		"status: delegation_serial 0":           status(func(m *DeviceStatus) { m.DelegationSerial = 0 }).Encode(),
		"status: no device_set_digest":          status(func(m *DeviceStatus) { m.DeviceSetDigest = nil }).Encode(),
		"status: u past 2^53-1":                 status(func(m *DeviceStatus) { m.AsOfMs = MaxSafeInt + 1 }).Encode(),
		"bundle: no device_result":              bundle(func(m *DeviceBundle) { m.DeviceResult = nil }).Encode(),
		"bundle: no device_result_sig":          bundle(func(m *DeviceBundle) { m.DeviceResultSig = nil }).Encode(),
		"bundle: short device_result_sig":       bundle(func(m *DeviceBundle) { m.DeviceResultSig = rnd(95) }).Encode(),
		"bundle: sig_root is not 96 bytes":      bundle(func(m *DeviceBundle) { m.SigRoot = rnd(97) }).Encode(),
		"bundle: device_intent over 2048 bytes": bundle(func(m *DeviceBundle) { m.DeviceIntent = rnd(2049) }).Encode(),
		"bundle: recovery_bind over 1024 bytes": bundle(func(m *DeviceBundle) { m.RecoveryBind = rnd(1025) }).Encode(),
		"bundle: cp_authz over 1024 bytes":      bundle(func(m *DeviceBundle) { m.CPAuthz = rnd(1025) }).Encode(),
		"bundle: device_result over 1024 bytes": bundle(func(m *DeviceBundle) { m.DeviceResult = rnd(1025) }).Encode(),
		"history: seventeen bundles":            DeviceHistory{AccountID: newID(), Bundles: [][]byte{rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9), rnd(9)}}.Encode(),
		"history: an empty bundle":              DeviceHistory{AccountID: newID(), Bundles: [][]byte{nil}}.Encode(),
		"history: a bundle over 8192 bytes":     DeviceHistory{AccountID: newID(), Bundles: [][]byte{rnd(8193)}}.Encode(),
		"bind: unknown kind":                    RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: newID(), Kind: ""}.Encode(),
		"bind: recovery_key is a hybrid point":  RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: newID(), Kind: BindKindBind, RecoveryKey: hybridOf(testPoint(t))}.Encode(),
		"bind: request_id is not a v4 uuid":     RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: "00000000-0000-0000-0000-000000000000", Kind: BindKindBind}.Encode(),
	} {
		require.Nil(t, enc, name)
	}

	// the empty history is valid: the page is [0..16]
	roundTrip(t, DeviceHistory{AccountID: newID()})
}
