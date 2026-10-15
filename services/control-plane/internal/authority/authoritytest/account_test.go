package authoritytest

import "testing"

func TestActivatedPromotesThePendingRootOnlyWhenOneIsPending(t *testing.T) {
	root, _ := genKey(t)
	cand, _ := genKey(t)
	a := &Account{Root: root}
	a.Activated()
	if a.Root != root || a.Pending != nil {
		t.Fatal("Activated with nothing pending changed the root")
	}
	a.Pending = cand
	a.Activated()
	if a.Root != cand || a.Pending != nil {
		t.Fatal("Activated did not make the pending candidate the active root")
	}
}
