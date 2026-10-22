package store

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// memMerge is newMerge over r's anchors with nothing installed, without a
// database.
func memMerge(t *testing.T, r *authoritytest.Realm) *merge {
	t.Helper()
	a, err := ac.DecodeAnchors(r.AnchorBytes)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := ac.NewTrust(a)
	if err != nil {
		t.Fatal(err)
	}
	all, err := ac.NewTrust(a)
	if err != nil {
		t.Fatal(err)
	}
	return &merge{tr: tr, all: all, dels: map[[2]uint64][]ac.Delegation{}, installed: map[string]bool{},
		installedDel: map[[2]uint64][]byte{}}
}

func admitAll(t *testing.T, m *merge, items ...ac.Signed) {
	t.Helper()
	for _, sg := range items {
		if ok, err := m.add(sg); !ok || err != nil {
			t.Fatalf("add: accepted %v, %v", ok, err)
		}
	}
}

func signedOf(d authoritytest.SignedDelegation) ac.Signed {
	return ac.Signed{Type: "delegation", Msg: d.Msg, SigA: d.SigA, SigB: d.SigB}
}

// E2: the merged view counts every delegation at one retired (gen, serial),
// in both arrival orders. LoadTrust refuses any trust that holds two (the
// reuse check, or differs), so this positive control runs on the merge alone.
func TestMergeViewCountsEveryDelegationAtOneRetiredKey(t *testing.T) {
	r := authoritytest.NewRealm(t)
	succ, hw, at02 := retiredTrust(t, r, false)
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		m := memMerge(t, r)
		admitAll(t, m, succ, signedOf(r.Current), signedOf(at02[order[0]]), signedOf(at02[order[1]]), signedOf(hw))
		v, err := m.view()
		if err != nil {
			t.Fatal(err)
		}
		if v.HW.ProductRootGen != 1 || v.HW.Serial != 1 {
			t.Fatalf("order %v: HW (%d, %d), want (1, 1)", order, v.HW.ProductRootGen, v.HW.Serial)
		}
		for _, k := range [][]byte{r.RSK.PublicKey(), at02[0].D.RSKPublicKeys[0], at02[1].D.RSKPublicKeys[0]} {
			if !slices.ContainsFunc(v.otherRSKs, func(o []byte) bool { return bytes.Equal(o, k) }) {
				t.Fatalf("order %v: other RSKs (%d) miss a delegation's RSK", order, len(v.otherRSKs))
			}
		}
	}
}

// C1: differs is one pass with a running map seeded from the installed
// delegations, so a collected delegation is refused against an installed one
// and against an earlier collected one. A trust file cannot carry two at one
// key (§2.6), so the second case runs on the merge alone.
func TestMergeDiffersChecksEveryCollectedDelegation(t *testing.T) {
	r := authoritytest.NewRealm(t)
	_, _, at02 := retiredTrust(t, r, false)
	x, y := signedOf(at02[0]), signedOf(at02[1])
	for _, c := range []struct {
		name      string
		installed []ac.Signed
		collected []ac.Signed
		want      string
	}{
		{"installed, then collected", []ac.Signed{x}, []ac.Signed{y}, "a delegation at installed (0, 2) differs"},
		{"two collected", nil, []ac.Signed{x, y}, "two file delegations at (0, 2) differ"},
		{"one collected", nil, []ac.Signed{x}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := memMerge(t, r)
			for _, sg := range c.installed {
				if err := m.noteInstalled(sg); err != nil {
					t.Fatal(err)
				}
			}
			m.toInsert = c.collected
			err := m.differs()
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrTrustUnusable) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
		})
	}
}
