package decide

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Generator 2 (plan Task 4 Step 2; spec §7 "ClassOf over every proof multiset of size <= 3,
// with set-composition representatives"): every multiset of size <= 3 over the five
// composition categories (K, paper, non-paper BE=0, non-paper BE=1, anchor) -- 56 including the
// empty one -- crossed with four maturity states per credential, in two variants:
//
//   - distinct: every element of a category is a different credential;
//   - shared:   every element of a category is the same credential, so a proof lists one
//     reference several times (Review Focus 1).
//
// This file never calls Decide, so Task 4's tests do not depend on it. decide_test.go reuses
// forEachGen2Case for the T1b check.

type gen2Cat int

const (
	catK gen2Cat = iota
	catPaper
	catNP0
	catNP1
	catA
	gen2NumCats
)

func (c gen2Cat) String() string {
	return [...]string{"K", "paper", "NP0", "NP1", "A"}[c]
}

type gen2State int

const (
	stMature gen2State = iota
	stImmature
	stAwaiting
	stContested
	gen2NumStates
)

func (s gen2State) String() string {
	return [...]string{"mature", "immature", "awaiting", "contested"}[s]
}

// gen2Refs holds, per category, three distinct credentials. A WebAuthn of catNP0 is
// device-bound and one of catNP1 is a synced passkey, which gen2DeviceBound encodes in
// Proof.DeviceBound. The first entry of each category is the representative
// used by the shared variant.
var gen2Refs = [gen2NumCats][]CredRef{
	catK:     {fxPW, fxAddr, fxSSO},
	catPaper: {cref(CredBackupCodeSet, "set1"), cref(CredBackupCodeSet, "set2"), cref(CredBackupCodeSet, "set3")},
	catNP0:   {fxTOTP, fxWA0, cref(CredTOTP, "totp2")},
	catNP1:   {cref(CredWebAuthn, "wa1a"), cref(CredWebAuthn, "wa1b"), cref(CredWebAuthn, "wa1c")},
	catA:     {fxRK, fxRD, cref(CredRecoveryKey, "rk2")},
}

// gen2Cred builds the credential for a ref in a maturity state, relative to t0 (the reference
// instant of ClassOf in this generator). A contested credential also matured earlier, so only
// the contest keeps it out.
func gen2Cred(r CredRef, s gen2State) Cred {
	switch s {
	case stImmature:
		return credMaturing(r, t0.Add(time.Hour))
	case stAwaiting:
		return credMaturing(r, awaitingInstant)
	case stContested:
		return credContested(r)
	default:
		return matureCred(r)
	}
}

type gen2Slot struct {
	cat   gen2Cat
	ref   CredRef
	state gen2State
}

// gen2Case is one (multiset, variant, maturity assignment).
type gen2Case struct {
	ms     []gen2Cat
	shared bool
	slots  []gen2Slot
	used   []CredRef
	creds  []Cred
	// counted is the composition of the distinct credentials that count at t0.
	counted [gen2NumCats]int
}

func (c gen2Case) proof() Proof {
	used := append([]CredRef(nil), c.used...)
	return Proof{Used: used, DeviceBound: gen2DeviceBound(used)}
}

// gen2DeviceBound lists the refs of used that belong to the non-paper BE=0 category and are
// WebAuthn: the only refs whose assertion carried BE=0 (F1). A TOTP of that category has no BE.
func gen2DeviceBound(used []CredRef) []CredRef {
	out := make([]CredRef, 0, len(used))
	for _, r := range used {
		if r.Kind != CredWebAuthn {
			continue
		}
		for _, np0 := range gen2Refs[catNP0] {
			if r == np0 {
				out = append(out, r)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (c gen2Case) label() string {
	variant := "distinct"
	if c.shared {
		variant = "shared"
	}
	return fmt.Sprintf("%s %v %v", variant, c.ms, c.slots)
}

// nonPaperCounted is the number of counted non-paper possession credentials.
func (c gen2Case) nonPaperCounted() int { return c.counted[catNP0] + c.counted[catNP1] }

// oracleClass is the independent expected class of the case.
func (c gen2Case) oracleClass() Class {
	return classOracle(c.counted[catK], c.counted[catPaper], c.counted[catNP0], c.counted[catNP1], c.counted[catA])
}

// gen2Multisets returns every non-decreasing sequence of categories of length 0..3.
func gen2Multisets() [][]gen2Cat {
	out := [][]gen2Cat{{}}
	var rec func(prefix []gen2Cat, from gen2Cat)
	rec = func(prefix []gen2Cat, from gen2Cat) {
		if len(prefix) == 3 {
			return
		}
		for c := from; c < gen2NumCats; c++ {
			next := append(append([]gen2Cat(nil), prefix...), c)
			out = append(out, next)
			rec(next, c)
		}
	}
	rec(nil, 0)
	return out
}

// gen2Layout maps a multiset to its slots (one per distinct credential) and, for each element,
// the index of its slot.
func gen2Layout(ms []gen2Cat, shared bool) (slots []gen2Slot, elem []int) {
	occ := [gen2NumCats]int{}
	slotOfCat := map[gen2Cat]int{}
	for _, c := range ms {
		if shared {
			idx, ok := slotOfCat[c]
			if !ok {
				idx = len(slots)
				slotOfCat[c] = idx
				slots = append(slots, gen2Slot{cat: c, ref: gen2Refs[c][0]})
			}
			elem = append(elem, idx)
			continue
		}
		elem = append(elem, len(slots))
		slots = append(slots, gen2Slot{cat: c, ref: gen2Refs[c][occ[c]]})
		occ[c]++
	}
	return slots, elem
}

// forEachGen2Case calls fn for every case. It returns the number of cases.
func forEachGen2Case(fn func(c gen2Case)) int {
	n := 0
	for _, shared := range []bool{false, true} {
		for _, ms := range gen2Multisets() {
			slots, elem := gen2Layout(ms, shared)
			total := 1
			for range slots {
				total *= int(gen2NumStates)
			}
			for code := 0; code < total; code++ {
				c := gen2Case{ms: ms, shared: shared, slots: append([]gen2Slot(nil), slots...)}
				rest := code
				for i := range c.slots {
					c.slots[i].state = gen2State(rest % int(gen2NumStates))
					rest /= int(gen2NumStates)
					cred := gen2Cred(c.slots[i].ref, c.slots[i].state)
					c.creds = append(c.creds, cred)
					if c.slots[i].state == stMature {
						c.counted[c.slots[i].cat]++
					}
				}
				for _, idx := range elem {
					c.used = append(c.used, c.slots[idx].ref)
				}
				fn(c)
				n++
			}
		}
	}
	return n
}

func TestGenerator2_ThereAreFiftySixMultisets(t *testing.T) {
	ms := gen2Multisets()
	require.Len(t, ms, 56, "multisets of size <= 3 over 5 categories, including the empty one")
	seen := map[string]bool{}
	for _, m := range ms {
		key := fmt.Sprint(m)
		require.False(t, seen[key], "duplicate multiset %s", key)
		seen[key] = true
	}
}

// TestClassOf_Multisets: (a) ClassOf equals the independent oracle over the counted
// elements; (e) the order of Proof.Used does not matter.
func TestClassOf_Multisets(t *testing.T) {
	calls := forEachGen2Case(func(c gen2Case) {
		got := ClassOf(c.proof(), c.creds, t0)
		if !assert.Equal(t, c.oracleClass(), got, c.label()) {
			return
		}
		rev := c.proof()
		for i, j := 0, len(rev.Used)-1; i < j; i, j = i+1, j-1 {
			rev.Used[i], rev.Used[j] = rev.Used[j], rev.Used[i]
		}
		assert.Equal(t, got, ClassOf(rev, c.creds, t0), "order independence: %s", c.label())
	})
	t.Logf("ClassOf called %d times", calls)
	require.Greater(t, calls, 2000)
}

// TestClassOf_MonotonicAndUncountedElementsAreInert: (b) adding a counted element never
// lowers the class; (c) (P4 for ClassOf) adding a non-counted element -- immature, awaiting
// or contested -- leaves the class unchanged.
func TestClassOf_MonotonicAndUncountedElementsAreInert(t *testing.T) {
	calls := 0
	forEachGen2Case(func(c gen2Case) {
		if c.shared || len(c.ms) > 2 {
			return
		}
		base := ClassOf(c.proof(), c.creds, t0)
		used := map[gen2Cat]int{}
		for _, m := range c.ms {
			used[m]++
		}
		for cat := gen2Cat(0); cat < gen2NumCats; cat++ {
			extra := gen2Refs[cat][used[cat]] // an unused distinct credential of that category
			for st := gen2State(0); st < gen2NumStates; st++ {
				ext := c.proof()
				ext.Used = append(ext.Used, extra)
				creds := append(append([]Cred(nil), c.creds...), gen2Cred(extra, st))
				got := ClassOf(ext, creds, t0)
				calls++
				if st == stMature {
					assert.GreaterOrEqual(t, got, base, "adding a counted %s lowered the class: %s", cat, c.label())
				} else {
					assert.Equal(t, base, got, "adding a %s %s element changed the class: %s", st, cat, c.label())
				}
			}
		}
	})
	t.Logf("%d extension checks", calls)
	require.Greater(t, calls, 500)
}

// TestClassOf_AnchorPlusKNeverReachesC3: parent §6.1, C3 needs a non-backup-eligible
// non-paper P; anchor + K, two anchors and paper + K stop at C2.
func TestClassOf_AnchorPlusKNeverReachesC3(t *testing.T) {
	forEachGen2Case(func(c gen2Case) {
		if c.counted[catNP0] == 0 {
			assert.Less(t, c.oracleClass(), C3, c.label())
			assert.Less(t, ClassOf(c.proof(), c.creds, t0), C3, c.label())
		}
	})
}
