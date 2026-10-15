package store

import (
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// enrollSharing builds an enroll_new whose genesis device is dev.
func enrollSharing(t *testing.T, a *authoritytest.Account, dev *authoritytest.Device) []byte {
	return a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, NewDevice: dev,
		Factors: []string{ac.FactorRegistration}}).Bytes
}

func TestTwoAccountsWithTheSameDSKOneCommitsOneDeviceKeyBound(t *testing.T) {
	e := newEnv(t)
	dev := authoritytest.NewDevice(t)
	a, b := e.r.NewAccount(t), e.r.NewAccount(t)
	e.commitOK(a, enrollSharing(t, a, dev))
	e.refusedAs(enrollSharing(t, b, dev), ac.RefusalDeviceKeyBound)
	var n int
	_ = e.f.Super.QueryRow(`SELECT count(*) FROM authority.key_bindings WHERE account_id = $1`, b.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("the refused op left %d partial bindings (the savepoint must roll them back)", n)
	}
}

// §3.2.5: concurrent cross-account binding → exactly one commit plus one
// recorded conflict, never a 503.
func TestConcurrentCrossAccountBindingNeverA503(t *testing.T) {
	e := newEnv(t)
	dev := authoritytest.NewDevice(t)
	a, b := e.r.NewAccount(t), e.r.NewAccount(t)
	bodies := [][]byte{enrollSharing(t, a, dev), enrollSharing(t, b, dev)}
	outs := make([]Outcome, 2)
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Add(1)
		go func(i int) { defer wg.Done(); outs[i] = e.submit(bodies[i]) }(i)
	}
	wg.Wait()
	c, r := 0, 0
	for _, o := range outs {
		switch {
		case o.Kind == KindCommitted:
			c++
		case o.Kind == KindRefused && o.Code == string(ac.RefusalDeviceKeyBound):
			r++
		default:
			t.Fatalf("unexpected outcome %+v (never a 503)", o)
		}
	}
	if c != 1 || r != 1 {
		t.Fatalf("committed %d, refused %d; want 1 and 1", c, r)
	}
}

func TestAnEKSPKIBoundTwiceIsEKReused(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	first := a.RotateEK(t)
	e.commitOK(a, first.Bytes)
	reuse := a.Build(t, authoritytest.Spec{Op: ac.OpRotateEK, NewEK: true, RootSigns: true, Factors: []string{ac.FactorPassword},
		PasswordLogin: true, Mutate: func(p *authoritytest.Parts) {
			if p.EKBinding != nil {
				p.EKBinding.EKPublicKey = first.EKSPKI
			}
		}})
	e.refusedAs(reuse.Bytes, ac.RefusalEKReused)
}

func TestARootReusedBySameAccountAndElsewhere(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	reuse := a.Build(t, authoritytest.Spec{Op: ac.OpRotateRoot, NewRoot: true, NewEK: true, RootSigns: true,
		CandidateKey: a.Root, Factors: []string{ac.FactorPassword}, PasswordLogin: true})
	e.refusedAs(reuse.Bytes, ac.RefusalRootReused)
	other := e.r.NewAccount(t)
	elsewhere := other.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, CandidateKey: a.Root,
		Factors: []string{ac.FactorRegistration}})
	e.refusedAs(elsewhere.Bytes, ac.RefusalRootBoundElsewhere)
}
