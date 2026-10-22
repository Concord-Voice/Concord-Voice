package store

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
	"github.com/google/uuid"
)

// bareStore opens a store over meta for realm with nothing installed:
// LoadTrust is what installs.
func bareStore(t *testing.T, f *authoritydb.Fixture, r *authoritytest.Realm, metaRealm string) *Store {
	t.Helper()
	if metaRealm != "" {
		f.SeedMeta(t, metaRealm)
	}
	s, err := Open(Config{DB: f.App, Signer: r.RSK, Anchors: r.AnchorBytes,
		Hooks:      Hooks{Statfs: func() (uint64, error) { return 1 << 40, nil }},
		TxDeadline: 2 * time.Second, ReadDeadline: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func trustFile(t *testing.T, ds ...authoritytest.SignedDelegation) []byte {
	t.Helper()
	items := make([][]byte, 0, len(ds))
	for _, d := range ds {
		items = append(items, d.Signed)
	}
	b := ac.TrustEnvelope{Items: items}.Encode()
	if b == nil {
		t.Fatal("the trust file does not encode")
	}
	return b
}

func installedSerials(t *testing.T, f *authoritydb.Fixture) []int64 {
	t.Helper()
	rows, err := f.App.Query(`SELECT serial FROM authority.delegations ORDER BY product_root_gen, serial`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func freshPoint(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return authoritytest.Point(k)
}

func TestLoadTrustInstallsACoveringDelegationOnTheLeader(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := bareStore(t, f, r, r.ID)
	ctx := context.Background()
	v, err := s.LoadTrust(ctx, trustFile(t, r.Current), true)
	if err != nil {
		t.Fatal(err)
	}
	if v.HW.Serial != 1 || !bytes.Equal(v.HW.RSKPublicKeys[0], r.RSK.PublicKey()) {
		t.Fatalf("HW: %+v", v.HW)
	}
	if got := installedSerials(t, f); len(got) != 1 || got[0] != 1 {
		t.Fatalf("installed: %v, want [1]", got)
	}
	if _, err := s.LoadTrust(ctx, trustFile(t, r.Current), true); err != nil {
		t.Fatalf("a restart with the same file: %v", err)
	}
	if got := installedSerials(t, f); len(got) != 1 {
		t.Fatalf("installed twice: %v", got)
	}
	if v, err := s.LoadTrust(ctx, trustFile(t, r.Current), false); err != nil || v.Pending {
		t.Fatalf("a standby with the whole file installed: pending %v, %v", v.Pending, err)
	}
}

func TestLoadTrustOnAStandbyInstallsNothing(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := bareStore(t, f, r, r.ID)
	v, err := s.LoadTrust(context.Background(), trustFile(t, r.Current), false)
	if err != nil {
		t.Fatal(err)
	}
	if v.HW.Serial != 1 || !v.Pending {
		t.Fatalf("HW serial %d, pending %v: want 1 (from the file), pending", v.HW.Serial, v.Pending)
	}
	if got := installedSerials(t, f); len(got) != 0 {
		t.Fatalf("a standby installed %v", got)
	}
}

// D-176: a delegation whose window has not begun refuses start on every node,
// and installs nothing, not even the covering delegation before it in the file.
func TestLoadTrustRefusesADelegationBeforeItsWindow(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	d1 := r.Current
	now := uint64(time.Now().UnixMilli())
	d2 := r.NextDelegation(t, authoritytest.WithWindow(now+3_600_000, now+7_200_000))
	s := bareStore(t, f, r, r.ID)
	for _, install := range []bool{true, false} {
		if _, err := s.LoadTrust(context.Background(), trustFile(t, d1, d2), install); !errors.Is(err, ErrTrustNotYetValid) {
			t.Fatalf("install=%v: %v, want ErrTrustNotYetValid", install, err)
		}
	}
	if got := installedSerials(t, f); len(got) != 0 {
		t.Fatalf("a refused start installed %v", got)
	}
}

// Only a covering window installs (DoR:1058): a delegation whose window ended
// before it was installed is left out and does not become the high-water mark.
// Its RSKs still count against D-223's reuse bullet (DoR:252): d2 lists a
// fresh RSK, which reaches otherRSKs. Listing HW's instead refuses start
// (TestLoadTrustRefusesAnRSKReusedAcrossDelegations). d1 arrives either from
// the file or already installed, which LoadTrust admits first, and each on
// the leader and on a standby (deferred #9); only the leader installs d1.
func TestLoadTrustLeavesOutADelegationThatEndedUninstalled(t *testing.T) {
	for _, c := range []struct {
		d1Installed, install bool
	}{{false, true}, {true, true}, {false, false}, {true, false}} {
		t.Run(fmt.Sprintf("d1 installed %v, install %v", c.d1Installed, c.install), func(t *testing.T) {
			f := authoritydb.Setup(t)
			r := authoritytest.NewRealm(t)
			d1 := r.Current
			now := uint64(time.Now().UnixMilli())
			rsk2 := freshPoint(t)
			d2 := r.NextDelegation(t, authoritytest.WithWindow(now-7_200_000, now-3_600_000), authoritytest.WithRSKs(rsk2))
			f.SeedMeta(t, r.ID)
			if c.d1Installed {
				r.Install(t, f.App, d1)
			}
			v, err := bareStore(t, f, r, "").LoadTrust(context.Background(), trustFile(t, d1, d2), c.install)
			if err != nil {
				t.Fatal(err)
			}
			if v.HW.Serial != 1 {
				t.Fatalf("HW serial %d, want 1", v.HW.Serial)
			}
			want := []int64{1}
			if !c.d1Installed && !c.install {
				want = nil // a standby installs nothing; its leader will install d1
			}
			if got := installedSerials(t, f); !slices.Equal(got, want) {
				t.Fatalf("installed %v, want %v", got, want)
			}
			if v.Pending != (!c.d1Installed && !c.install) {
				t.Fatalf("pending %v", v.Pending)
			}
			if len(v.otherRSKs) != 1 || !bytes.Equal(v.otherRSKs[0], rsk2) {
				t.Fatalf("other RSKs: %d, want d2's", len(v.otherRSKs))
			}
		})
	}
}

// Renewal step 4 (DoR:1056): a standby restarted with D(s+1)'s keys before the
// leader installs D(s+1) sees D(s+1) as HW from its file, and the old RSKs as
// another delegation's.
func TestLoadTrustMergesAFileAheadOfTheDatabase(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	d1 := r.Current
	f.SeedMeta(t, r.ID)
	r.Install(t, f.App, d1)
	newRSK := freshPoint(t)
	d2 := r.NextDelegation(t, authoritytest.WithRSKs(newRSK))
	s := bareStore(t, f, r, "")
	v, err := s.LoadTrust(context.Background(), trustFile(t, d1, d2), false)
	if err != nil {
		t.Fatal(err)
	}
	if v.HW.Serial != 2 || !bytes.Equal(v.HW.RSKPublicKeys[0], newRSK) {
		t.Fatalf("HW: %+v", v.HW)
	}
	if len(v.otherRSKs) != 1 || !bytes.Equal(v.otherRSKs[0], r.RSK.PublicKey()) {
		t.Fatalf("other RSKs: %d", len(v.otherRSKs))
	}
	if got := installedSerials(t, f); len(got) != 1 {
		t.Fatalf("a standby installed: %v", got)
	}
}

// T3 and I9: a forged delegation is ignored whatever its window. One whose
// window has not begun does not refuse start, and one that ended uninstalled
// lends no RSK to D-223's reuse check (d2 keeps the realm's RSK, HW's).
func TestLoadTrustIgnoresAForgedDelegationOutsideItsWindow(t *testing.T) {
	now := uint64(time.Now().UnixMilli())
	for name, window := range map[string][2]uint64{
		"not begun": {now + 3_600_000, now + 7_200_000},
		"ended":     {now - 7_200_000, now - 3_600_000},
		"in window": {now - 3_600_000, now + 3_600_000}, // counted on the merge's other path
	} {
		t.Run(name, func(t *testing.T) {
			f := authoritydb.Setup(t)
			r := authoritytest.NewRealm(t)
			d1 := r.Current
			d2 := r.NextDelegation(t, authoritytest.WithWindow(window[0], window[1]))
			forged := append([]byte(nil), d2.SigB...)
			forged[10] ^= 0xff
			d2.Signed = ac.Signed{Type: "delegation", Msg: d2.Msg, SigA: d2.SigA, SigB: forged}.Encode()
			s := bareStore(t, f, r, r.ID)
			v, err := s.LoadTrust(context.Background(), trustFile(t, d1, d2), true)
			if err != nil {
				t.Fatalf("a forged item refused start: %v", err)
			}
			if v.HW.Serial != 1 || len(v.otherRSKs) != 0 {
				t.Fatalf("HW serial %d, other RSKs %d: want 1 and none", v.HW.Serial, len(v.otherRSKs))
			}
			// The forged item is counted; the control file has none.
			if v.Ignored != 1 {
				t.Fatalf("Ignored %d, want 1", v.Ignored)
			}
			if c, err := s.LoadTrust(context.Background(), trustFile(t, d1), true); err != nil || c.Ignored != 0 {
				t.Fatalf("control: %v, Ignored %d; want 0", err, c.Ignored)
			}
		})
	}
}

// T4 (B2-D20): a verified delegation outside its window still counts toward
// the halt check, since every client holds the whole trust. One at an
// installed delegation's (gen, serial) with a different msg halts them, so
// start refuses on every node. A file cannot carry both (§2.6), but the
// database and the file can.
func TestLoadTrustRefusesAContradictionOutsideItsWindow(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	d1 := r.Current
	f.SeedMeta(t, r.ID)
	r.Install(t, f.App, d1)
	now := uint64(time.Now().UnixMilli())
	twin := d1.D
	twin.NotBeforeMs, twin.NotAfterMs = now-7_200_000, now-3_600_000
	msg := twin.Encode()
	sa, sb := authoritytest.SignRaw(t, r.PrimaryA, msg), authoritytest.SignRaw(t, r.PrimaryB, msg)
	file := trustFile(t, authoritytest.SignedDelegation{D: twin, Msg: msg, SigA: sa, SigB: sb,
		Signed: ac.Signed{Type: "delegation", Msg: msg, SigA: sa, SigB: sb}.Encode()})
	s := bareStore(t, f, r, "")
	for _, install := range []bool{true, false} {
		_, err := s.LoadTrust(context.Background(), file, install)
		var h *ac.HaltError
		if !errors.Is(err, ErrTrustUnusable) || !errors.As(err, &h) {
			t.Fatalf("install=%v: %v, want ErrTrustUnusable carrying the T4 halt", install, err)
		}
	}
}

func TestLoadTrustRefusesAForgedSignature(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	d := r.Current
	forged := append([]byte(nil), d.SigB...)
	forged[10] ^= 0xff
	d.Signed = ac.Signed{Type: "delegation", Msg: d.Msg, SigA: d.SigA, SigB: forged}.Encode()
	s := bareStore(t, f, r, r.ID)
	if _, err := s.LoadTrust(context.Background(), trustFile(t, d), true); !errors.Is(err, ErrTrustUnusable) {
		t.Fatalf("forged sig_b: %v, want ErrTrustUnusable (T3 ignores it, so no delegation remains)", err)
	}
	if got := installedSerials(t, f); len(got) != 0 {
		t.Fatalf("installed a forged delegation: %v", got)
	}
}

func TestLoadTrustChecksTheRealm(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	if _, err := bareStore(t, f, r, "").LoadTrust(context.Background(), trustFile(t, r.Current), true); !errors.Is(err, ErrNoMeta) {
		t.Fatalf("no meta: %v, want ErrNoMeta", err)
	}
	s := bareStore(t, f, r, uuid.NewString())
	if _, err := s.LoadTrust(context.Background(), trustFile(t, r.Current), true); !errors.Is(err, ErrRealmMismatch) {
		t.Fatalf("other realm: %v, want ErrRealmMismatch", err)
	}
}

func TestLoadTrustRefusesAMalformedFile(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := bareStore(t, f, r, r.ID)
	if _, err := s.LoadTrust(context.Background(), []byte(`["concord",1,"trust",[]]`), true); !errors.Is(err, ErrTrustUnusable) {
		t.Fatalf("malformed: %v, want ErrTrustUnusable", err)
	}
}

// trustRowCount is every installed trust row, successions and delegations.
func trustRowCount(t *testing.T, f *authoritydb.Fixture) int {
	t.Helper()
	var n int
	if err := f.Super.QueryRow(`SELECT (SELECT count(*) FROM authority.delegations)
		+ (SELECT count(*) FROM authority.successions)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// envelope is a trust file of items, already in §2.6's order.
func envelope(t *testing.T, items ...[]byte) []byte {
	t.Helper()
	b := ac.TrustEnvelope{Items: items}.Encode()
	if b == nil {
		t.Fatal("the trust file does not encode")
	}
	return b
}

// openWithAnchors is bareStore over other anchors, with meta already seeded.
func openWithAnchors(t *testing.T, f *authoritydb.Fixture, r *authoritytest.Realm, anchors []byte) *Store {
	t.Helper()
	s, err := Open(Config{DB: f.App, Signer: r.RSK, Anchors: anchors,
		Hooks:      Hooks{Statfs: func() (uint64, error) { return 1 << 40, nil }},
		TxDeadline: 2 * time.Second, ReadDeadline: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// signDelegation signs d under primaries pa and pb, for a generation the
// realm fixture does not sign.
func signDelegation(t *testing.T, pa, pb *ecdsa.PrivateKey, d ac.Delegation) authoritytest.SignedDelegation {
	t.Helper()
	msg := d.Encode()
	if msg == nil {
		t.Fatalf("the delegation does not encode: %+v", d)
	}
	sa, sb := authoritytest.SignRaw(t, pa, msg), authoritytest.SignRaw(t, pb, msg)
	return authoritytest.SignedDelegation{D: d, Msg: msg, SigA: sa, SigB: sb,
		Signed: ac.Signed{Type: "delegation", Msg: msg, SigA: sa, SigB: sb}.Encode()}
}

// signedSuccession is r's succession from generation 0 to 1, signed by its
// recovery keys, with generation 1's primaries, which sign its delegations.
func signedSuccession(t *testing.T, r *authoritytest.Realm) (sg ac.Signed, pa, pb *ecdsa.PrivateKey) {
	t.Helper()
	k := make([]*ecdsa.PrivateKey, 4)
	for i := range k {
		var err error
		if k[i], err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader); err != nil {
			t.Fatal(err)
		}
	}
	msg := ac.Succession{RealmID: r.ID, PrevRootGen: 0, NewRootGen: 1,
		NewPrimaryA: authoritytest.Point(k[0]), NewPrimaryB: authoritytest.Point(k[1]),
		NewRecoveryA: authoritytest.Point(k[2]), NewRecoveryB: authoritytest.Point(k[3])}.Encode()
	if msg == nil {
		t.Fatal("the succession does not encode")
	}
	sg = ac.Signed{Type: "succession", Msg: msg,
		SigA: authoritytest.SignRaw(t, r.RecoveryA, msg), SigB: authoritytest.SignRaw(t, r.RecoveryB, msg)}
	return sg, k[0], k[1]
}

// D-223's reuse bullet (DoR:252) runs inside LoadTrust, before the leader
// commits: an RSK of HW listed in another delegation in the merged trust
// refuses start on every node, and nothing is installed. Delegations are
// never deleted, so a violating install that committed would fail D-223 on
// every later start until a new ceremony. NextDelegation keeps the realm's
// RSK, so d2 reuses d1's, whether d2 is HW or left out because it ended.
func TestLoadTrustRefusesAnRSKReusedAcrossDelegations(t *testing.T) {
	now := uint64(time.Now().UnixMilli())
	for _, c := range []struct {
		name       string
		preinstall bool
		opts       []authoritytest.RealmOption
	}{
		{"d2 accepted", false, nil},
		{"d2 accepted, d1 installed", true, nil},
		{"d2 ended uninstalled", false, []authoritytest.RealmOption{authoritytest.WithWindow(now-7_200_000, now-3_600_000)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := authoritydb.Setup(t)
			r := authoritytest.NewRealm(t)
			f.SeedMeta(t, r.ID)
			d1 := r.Current
			if c.preinstall {
				r.Install(t, f.App, d1)
			}
			d2 := r.NextDelegation(t, c.opts...)
			s := bareStore(t, f, r, "")
			before := trustRowCount(t, f)
			for _, install := range []bool{true, false} {
				if _, err := s.LoadTrust(context.Background(), trustFile(t, d1, d2), install); !errors.Is(err, ErrRSKReused) {
					t.Fatalf("install=%v: %v, want ErrRSKReused", install, err)
				}
				if got := trustRowCount(t, f); got != before {
					t.Fatalf("install=%v: %d trust rows after the refusal, want %d", install, got, before)
				}
			}
		})
	}
}

// S3: an installed row the anchors do not verify means these anchors did not
// install it, or it did not come from LoadTrust. Start refuses on every node,
// with that cause (errInstalledUnverified) and the row's type.
// Skipped, it would let a substituted anchors file and trust file install a
// delegation of the substitute's choosing, here one listing the realm's own
// RSK, over the realm's installed trust. A trust-file item that does not
// verify is still ignored (T3, I9): TestLoadTrustIgnoresAForgedDelegationOutsideItsWindow.
func TestLoadTrustRefusesAnInstalledRowTheAnchorsDoNotVerify(t *testing.T) {
	for _, installed := range []string{"delegation", "succession"} {
		t.Run(installed, func(t *testing.T) {
			f := authoritydb.Setup(t)
			r := authoritytest.NewRealm(t)
			f.SeedMeta(t, r.ID)
			if installed == "delegation" {
				r.Install(t, f.App, r.Current)
			} else {
				sg, _, _ := signedSuccession(t, r)
				if _, err := f.App.Exec(`INSERT INTO authority.successions (prev_root_gen, bytes, sig_a, sig_b)
					VALUES (0, $1, $2, $3)`, sg.Msg, sg.SigA, sg.SigB); err != nil {
					t.Fatal(err)
				}
			}
			sub := authoritytest.NewRealm(t, func(d *ac.Delegation) {
				d.RealmID, d.RSKPublicKeys = r.ID, [][]byte{r.RSK.PublicKey()}
			})
			anchors := ac.Anchors{RealmID: r.ID, PrimaryA: authoritytest.Point(sub.PrimaryA),
				PrimaryB: authoritytest.Point(sub.PrimaryB), RecoveryA: authoritytest.Point(sub.RecoveryA),
				RecoveryB: authoritytest.Point(sub.RecoveryB)}.Encode()
			d2 := sub.NextDelegation(t)
			s := openWithAnchors(t, f, r, anchors)
			before := trustRowCount(t, f)
			for _, install := range []bool{true, false} {
				// The cause is pinned, so no other unusable trust satisfies this.
				_, err := s.LoadTrust(context.Background(), trustFile(t, d2), install)
				if !errors.Is(err, ErrTrustUnusable) || !errors.Is(err, errInstalledUnverified) ||
					!strings.HasSuffix(err.Error(), ": "+installed) {
					t.Fatalf("install=%v: %v, want ErrTrustUnusable from errInstalledUnverified for the installed %s",
						install, err, installed)
				}
				if got := trustRowCount(t, f); got != before {
					t.Fatalf("install=%v: %d trust rows after the refusal, want %d", install, got, before)
				}
			}
		})
	}
}

// retiredCase places two delegations at (0, 2) that differ only in their RSK.
// With reuse, the second lists HW's RSK. One of them is installed, the first
// unless reuserDB, and the other is in the file; with bothFiled, both are in
// the file and neither is installed.
type retiredCase struct {
	name                       string
	reuse, reuserDB, bothFiled bool
}

// retiredTrust signs E2's objects: the succession that retires generation 0,
// HW at (1, 1) under generation 1's primaries, and two delegations at (0, 2)
// that differ only in their RSK. With reuse, the second lists HW's RSK.
func retiredTrust(t *testing.T, r *authoritytest.Realm, reuse bool) (ac.Signed, authoritytest.SignedDelegation, [2]authoritytest.SignedDelegation) {
	t.Helper()
	succ, pa, pb := signedSuccession(t, r)
	g1 := r.Current.D
	g1.ProductRootGen, g1.Serial, g1.MinValidSerial, g1.RSKPublicKeys = 1, 1, 1, [][]byte{freshPoint(t)}
	hw := signDelegation(t, pa, pb, g1)
	rsks := [][]byte{freshPoint(t), freshPoint(t)}
	if reuse {
		rsks[1] = g1.RSKPublicKeys[0]
	}
	var at02 [2]authoritytest.SignedDelegation
	for i, rsk := range rsks {
		d := r.Current.D
		d.Serial, d.MinValidSerial, d.RSKPublicKeys = 2, 2, [][]byte{rsk}
		at02[i] = signDelegation(t, r.PrimaryA, r.PrimaryB, d)
	}
	return succ, hw, at02
}

// retiredSerial is the trust for c. Installed: d1, and one (0, 2) delegation
// unless c.bothFiled. The file: the succession, d1, the (0, 2) delegations not
// installed, and HW at (1, 1).
func retiredSerial(t *testing.T, f *authoritydb.Fixture, r *authoritytest.Realm, c retiredCase) []byte {
	t.Helper()
	d1 := r.Current
	succ, hw, at02 := retiredTrust(t, r, c.reuse)
	if c.reuserDB {
		at02[0], at02[1] = at02[1], at02[0]
	}
	r.Install(t, f.App, d1)
	items := [][]byte{succ.Encode(), d1.Signed}
	if c.bothFiled {
		items = append(items, at02[0].Signed, at02[1].Signed)
	} else {
		r.Install(t, f.App, at02[0])
		items = append(items, at02[1].Signed)
	}
	return envelope(t, append(items, hw.Signed)...)
}

// retiredSerialCases are E2 and C1 at one retired (gen, serial). A retired
// generation can hold two delegations at one key (T4 halts only in the current
// one), so the merged view counts both, and the reuse check sees HW's RSK in
// whichever arrives second: the installed row is admitted before the file's,
// so swapping which one is installed feeds them in both orders. With no reuse,
// the file's (0, 2) differs from the installed row, which the table cannot
// hold beside it, so start refuses; the reuse check runs first. A file cannot
// carry both: §2.6 orders its delegations strictly by (gen, serial), so the
// envelope is malformed before anything is merged.
var retiredSerialCases = []struct {
	retiredCase
	want []error // every one must match
	text string
}{
	{retiredCase{"no reuse", false, false, false}, []error{ErrTrustUnusable}, "a delegation at installed (0, 2) differs"},
	{retiredCase{"no reuse, the other one installed", false, true, false}, []error{ErrTrustUnusable}, "a delegation at installed (0, 2) differs"},
	{retiredCase{"reuser in the file", true, false, false}, []error{ErrRSKReused}, ErrRSKReused.Error()},
	{retiredCase{"reuser installed", true, true, false}, []error{ErrRSKReused}, ErrRSKReused.Error()},
	{retiredCase{"both in the file", false, false, true}, []error{ac.ErrMalformed, ErrTrustUnusable}, "trust delegation order"},
}

// runRetiredSerial runs every retiredSerialCases case on one node. Every
// refusal is decided before anything is written, so the leader and a standby
// refuse alike and nothing is installed.
func runRetiredSerial(t *testing.T, install bool) {
	for _, c := range retiredSerialCases {
		t.Run(c.name, func(t *testing.T) {
			f := authoritydb.Setup(t)
			r := authoritytest.NewRealm(t)
			f.SeedMeta(t, r.ID)
			file := retiredSerial(t, f, r, c.retiredCase)
			before := trustRowCount(t, f)
			v, err := bareStore(t, f, r, "").LoadTrust(context.Background(), file, install)
			if got := trustRowCount(t, f); got != before {
				t.Fatalf("install=%v installed: %d trust rows, want %d", install, got, before)
			}
			for _, w := range c.want { // want is never empty, so a nil err fails here
				if !errors.Is(err, w) {
					t.Fatalf("install=%v: %v (pending %v), want it to match %q", install, err, v.Pending, w)
				}
			}
			if !strings.Contains(err.Error(), c.text) {
				t.Fatalf("install=%v: %v, want %q", install, err, c.text)
			}
		})
	}
}

// E2 and C1 on a standby: it refuses the same ceremony error its leader
// refuses, rather than starting Pending.
func TestLoadTrustCountsEveryDelegationAtOneRetiredSerial(t *testing.T) { runRetiredSerial(t, false) }

// E2 and C1 on the leader: the reuse check and the differs check run before
// it inserts, so it refuses with a named error, never the table's key.
func TestLoadTrustOnTheLeaderAtOneRetiredSerial(t *testing.T) { runRetiredSerial(t, true) }

// A succession cannot reach the leader's insert at an installed
// prev_root_gen: two successions for one generation halt T4 whatever
// generation is current, so the fork refuses start before anything is
// written, on every node.
func TestLoadTrustRefusesAForkOfAnInstalledSuccession(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	f.SeedMeta(t, r.ID)
	installed, _, _ := signedSuccession(t, r)
	if _, err := f.App.Exec(`INSERT INTO authority.successions (prev_root_gen, bytes, sig_a, sig_b)
		VALUES (0, $1, $2, $3)`, installed.Msg, installed.SigA, installed.SigB); err != nil {
		t.Fatal(err)
	}
	fork, _, _ := signedSuccession(t, r)
	before := trustRowCount(t, f)
	s := bareStore(t, f, r, "")
	for _, install := range []bool{true, false} {
		_, err := s.LoadTrust(context.Background(), envelope(t, fork.Encode(), r.Current.Signed), install)
		var h *ac.HaltError
		if !errors.Is(err, ErrTrustUnusable) || !errors.As(err, &h) {
			t.Fatalf("install=%v: %v, want ErrTrustUnusable carrying the T4 halt", install, err)
		}
		if got := trustRowCount(t, f); got != before {
			t.Fatalf("install=%v: %d trust rows, want %d", install, got, before)
		}
	}
}

// LoadTrust reads one snapshot. Under READ COMMITTED each statement takes its
// own, so a standby restarting while the leader commits a succession and a
// generation-1 delegation could read the delegation without its succession,
// and refuse. Here a delegation committed after the transaction's first read
// (from the db_now hook, which runs after the realm check) is not seen.
func TestLoadTrustReadsOneSnapshot(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	f.SeedMeta(t, r.ID)
	d1 := r.Current
	r.Install(t, f.App, d1)
	d2 := r.NextDelegation(t, authoritytest.WithRSKs(freshPoint(t)))
	committed := false
	s, err := Open(Config{DB: f.App, Signer: r.RSK, Anchors: r.AnchorBytes,
		Hooks: Hooks{Statfs: func() (uint64, error) { return 1 << 40, nil },
			DBNow: func(context.Context) (uint64, error) {
				if !committed {
					committed = true
					r.Install(t, f.App, d2)
				}
				return uint64(time.Now().UnixMilli()), nil
			}},
		TxDeadline: 2 * time.Second, ReadDeadline: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.LoadTrust(context.Background(), trustFile(t, d1), false)
	if err != nil {
		t.Fatal(err)
	}
	if !committed || v.HW.Serial != 1 {
		t.Fatalf("committed %v, HW serial %d: want the snapshot's serial 1", committed, v.HW.Serial)
	}
}

// Sec-M2: LoadTrust's mark is the installed trust it read, plus what it
// installed, and InstalledTrustMark reads the same count afterwards. A
// standby's mark excludes the file items it would install, so a promotion
// onto trust a leader installed since compares unequal.
func TestLoadTrustMarkIsTheInstalledTrust(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	f.SeedMeta(t, r.ID)
	succ, pa, pb := signedSuccession(t, r)
	g1 := r.Current.D
	g1.ProductRootGen, g1.Serial, g1.MinValidSerial, g1.RSKPublicKeys = 1, 1, 1, [][]byte{freshPoint(t)}
	hw := signDelegation(t, pa, pb, g1)
	file := envelope(t, succ.Encode(), r.Current.Signed, hw.Signed)
	s := bareStore(t, f, r, "")
	ctx := context.Background()
	mark := func() TrustMark {
		m, err := s.InstalledTrustMark(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	pending, err := s.LoadTrust(ctx, file, false)
	if err != nil || !pending.Pending || pending.Mark != (TrustMark{}) || mark() != (TrustMark{}) {
		t.Fatalf("a standby before the install: pending %v, mark %+v, %v; want pending and the empty mark", pending.Pending, pending.Mark, err)
	}
	leader, err := s.LoadTrust(ctx, file, true)
	want := TrustMark{Successions: 1, Delegations: 2}
	if err != nil || leader.Mark != want || mark() != want {
		t.Fatalf("the leader's install: mark %+v, installed %+v, %v; want %+v", leader.Mark, mark(), err, want)
	}
	standby, err := s.LoadTrust(ctx, file, false)
	if err != nil || standby.Pending || standby.Mark != want {
		t.Fatalf("a standby after the install: pending %v, mark %+v, %v", standby.Pending, standby.Mark, err)
	}
	g1.Serial, g1.MinValidSerial, g1.RSKPublicKeys = 2, 2, [][]byte{freshPoint(t)}
	r.Install(t, f.App, signDelegation(t, pa, pb, g1)) // a leader's later install, replicated
	if got := mark(); got == standby.Mark || got.Delegations != 3 {
		t.Fatalf("after another install: %+v, want it to differ from the start mark %+v", got, standby.Mark)
	}
}
