package authoritycontract

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenesisHeads(t *testing.T) {
	realm, account := newID(), newID()
	g := GenesisHead(realm, account)
	want := `["concord-account-authority",1,"chain-head","` + realm + `","` + account + `","","","",0,"",0,"",[],[],[],[]]`
	require.Equal(t, want, string(g.Bytes()))
	got, err := ParseChainHead(g.Bytes())
	require.NoError(t, err)
	require.Equal(t, g.Bytes(), got.Bytes())

	for _, self := range []string{"", newID()} {
		dh, ch := GenesisDeviceHead(realm, account, self).Bytes()
		require.NotNil(t, dh)
		require.Equal(t, g.Bytes(), ch)
		parsed, err := ParseDeviceHead(dh, ch)
		require.NoError(t, err)
		dh2, ch2 := parsed.Bytes()
		require.Equal(t, dh, dh2)
		require.Equal(t, ch, ch2)
	}
}

func headsAtCaps(t *testing.T) (ChainHead, DeviceHead) {
	root, ek := testPoint(t), testEKSPKI(t)
	seq := SeqRecord(digestOf("e"), digestOf("r"))
	rootRec := RootRecord(digestOf("r"), root, AssuranceContinuity)
	rec := RecoverRecord(MaxSafeInt, digestOf("k"))
	require.Len(t, seq, 129)
	require.Len(t, rootRec, 208)
	require.Len(t, rec, 81)
	ch := ChainHead{RealmID: newID(), AccountID: newID(), HeadResult: rnd(1000), Fix: rnd(800),
		ActiveEKSPKI: ek, ActiveEKGeneration: 3, PendingEKSPKI: ek, PendingEKGeneration: 4, PendingRecoveryKey: testPoint(t),
		SeqIndex: []string{SeqRecord(digestOf("e1"), nil), seq}, RootIndex: []string{rootRec}, EKIndex: [][]byte{digestOf("ek")},
		RecoverIndex: []string{rec, RecoverRecord(10, digestOf("k2"))}}
	adm := AdmittedRecord(newID(), newID())
	sec := SecretsRecord(digestOf("de"), OpRotateRoot, root, ek)
	require.Len(t, adm, 73)
	require.Len(t, sec, 946)
	dh := DeviceHead{RealmID: ch.RealmID, AccountID: ch.AccountID, SelfDeviceID: newID(), LastDeviceResult: rnd(600),
		DeviceSet: rnd(1200), AdmittedBy: []string{adm, AdmittedRecord(newID(), "")},
		Secrets: []string{sec, SecretsRecord(digestOf("d2"), OpRotateEK, nil, ek)}, Chain: ch}
	return ch, dh
}

func TestHeadRecordsAtTheirCaps(t *testing.T) {
	ch, dh := headsAtCaps(t)
	b := ch.Bytes()
	require.NotNil(t, b)
	got, err := ParseChainHead(b)
	require.NoError(t, err)
	require.Equal(t, b, got.Bytes())
	d, c := dh.Bytes()
	parsed, err := ParseDeviceHead(d, c)
	require.NoError(t, err)
	d2, _ := parsed.Bytes()
	require.Equal(t, d, d2)
}

// One character over each record cap (E3-07) refuses, at encode and at decode.
func TestHeadRecordsOneOver(t *testing.T) {
	ch, dh := headsAtCaps(t)
	good := string(ch.Bytes())
	for name, rec := range map[string]string{"seq_index": ch.SeqIndex[1], "root_index": ch.RootIndex[0], "recover_index": ch.RecoverIndex[0]} {
		_, err := ParseChainHead([]byte(strings.Replace(good, `"`+rec+`"`, `"`+rec+`A"`, 1)))
		require.ErrorIs(t, err, ErrMalformed, name)
	}
	over := ch
	over.SeqIndex = []string{ch.SeqIndex[1] + "A"}
	require.Nil(t, over.Bytes())

	d, c := dh.Bytes()
	for name, rec := range map[string]string{"admitted_by": dh.AdmittedBy[0], "secrets": dh.Secrets[0]} {
		_, err := ParseDeviceHead([]byte(strings.Replace(string(d), `"`+rec+`"`, `"`+rec+`A"`, 1)), c)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

func TestDeviceHeadPairing(t *testing.T) {
	_, dh := headsAtCaps(t)
	d, _ := dh.Bytes()
	other := GenesisHead(dh.RealmID, dh.AccountID).Bytes()
	_, err := ParseDeviceHead(d, other)
	require.ErrorIs(t, err, ErrMalformed, "chain_head_digest must match the paired chain-head")
}

func TestRecordShapes(t *testing.T) {
	for name, c := range map[string]struct {
		check func(string) error
		s     string
	}{
		"seq bad digest":     {checkSeqRecord, "AAAA."},
		"root bad assurance": {checkRootRecord, strings.TrimSuffix(RootRecord(digestOf("r"), testPoint(t), AssuranceRecovery), "recovery") + "trusted"},
		"root hybrid point":  {checkRootRecord, RootRecord(digestOf("r"), hybridOf(testPoint(t)), AssuranceRecovery)},
		"recover seq 2^53":   {checkRecoverRecord, "9007199254740992." + b64Of(digestOf("k"))},
		"admitted v1 id":     {checkAdmittedRecord, "6ba7b810-9dad-11d1-80b4-00c04fd430c8."},
		"secrets op":         {checkSecretsRecord, SecretsRecord(digestOf("d"), OpEnrollNew, nil, testEKSPKI(t))},
		"secrets parts":      {checkSecretsRecord, "a.b.c"},
	} {
		require.ErrorIs(t, c.check(c.s), ErrMalformed, name)
	}
}

// HEAD_BLOB_MAX is runtime-only (16 MiB cannot go into vectors.json).
func TestHeadBlobMax(t *testing.T) {
	_, err := ParseChainHead(make([]byte, HeadBlobMax+1))
	require.ErrorIs(t, err, ErrMalformed)
	h := GenesisHead(newID(), newID())
	rec := SeqRecord(digestOf("e"), digestOf("r"))
	h.SeqIndex = make([]string, HeadBlobMax/132+1)
	for i := range h.SeqIndex {
		h.SeqIndex[i] = rec
	}
	require.Nil(t, h.Bytes(), "a head whose encoding exceeds HEAD_BLOB_MAX is never produced")
}

// A seq_index record admits an empty bind root; an invalid entry digest is
// refused whether or not the bind root is empty (both arms of the check).
func TestHeadSeqRecordArms(t *testing.T) {
	entry, root := digestOf("e"), digestOf("r")
	for name, c := range map[string]struct {
		rec     string
		wantErr bool
	}{
		"entry and bind root":        {SeqRecord(entry, root), false},
		"entry and empty bind root":  {SeqRecord(entry, nil), false},
		"bad entry, empty bind root": {"AAAA.", true},
		"bad entry, bind root":       {"AAAA." + b64Of(root), true},
		"empty entry, bind root":     {"." + b64Of(root), true},
		"entry and bad bind root":    {b64Of(entry) + ".AAAA", true},
		"entry only":                 {b64Of(entry), true},
	} {
		err := checkSeqRecord(c.rec)
		if c.wantErr {
			require.ErrorIs(t, err, ErrMalformed, name)
		} else {
			require.NoError(t, err, name)
		}
	}
}

// Both persisted layouts are registered, and the Message encoder is Bytes.
func TestHeadsRegisteredAndEncodeIsBytes(t *testing.T) {
	ch, dh := headsAtCaps(t)
	b := ch.Bytes()
	require.NotNil(t, b)
	require.Equal(t, b, ch.Encode())
	m, err := Decode("chain-head", b)
	require.NoError(t, err)
	require.Equal(t, b, m.Encode())

	d, _ := dh.Bytes()
	require.NotNil(t, d)
	w, err := Decode("device-head", d)
	require.NoError(t, err)
	require.Equal(t, d, w.Encode())

	bad := ch
	bad.RealmID = "not-a-uuid"
	require.Nil(t, bad.Bytes())
	require.Nil(t, bad.Encode())
}
