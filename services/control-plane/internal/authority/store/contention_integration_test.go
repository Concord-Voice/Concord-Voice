package store

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func runConcurrently(e *env, bodies [][]byte, device bool) []Outcome {
	outs := make([]Outcome, len(bodies))
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if device {
				outs[i] = e.device(bodies[i])
			} else {
				outs[i] = e.submit(bodies[i])
			}
		}(i)
	}
	wg.Wait()
	return outs
}

// The published rotation and the private add_device serialize on L1 (proven
// waiting through pg_locks), and exactly one commits. The loser's code depends
// on the winner. If add_device wins, it moves dseq, so the rotation's device
// part is stale_device_seq (step 8a). If the rotation wins, it moves seq, so
// add_device is stale_seq at policy, before step 8a. Sixteen rounds, because
// the scheduler picks the L1 winner, and mutations I2-01 and G-06 show only in
// rounds the rotation wins, where both commit.
func TestConcurrentAddDeviceAndRotationCommitExactlyOne(t *testing.T) {
	e := newEnv(t)
	for round := 0; round < 16; round++ {
		a, _ := e.enrolledWith()
		add, rot := a.AddDevice(t, authoritytest.NewDevice(t)), a.RotateEK(t)
		blocker, err := e.f.App.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := blocker.Exec(`SELECT 1 FROM authority.account_heads WHERE account_id = $1 FOR UPDATE`, a.ID); err != nil {
			t.Fatal(err)
		}
		var outs [2]Outcome
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); outs[0] = e.device(add.Bytes) }()
		go func() { defer wg.Done(); outs[1] = e.submit(rot.Bytes) }()
		waitForLockWaiters(t, e.f.Super, 2)
		_ = blocker.Rollback()
		wg.Wait()
		add0, rot1 := outs[0], outs[1]
		switch {
		case add0.Kind == KindCommitted && rot1.Kind == KindRefused && rot1.Code == string(ac.RefusalStaleDeviceSeq):
		case rot1.Kind == KindCommitted && add0.Kind == KindRefused && add0.Code == string(ac.RefusalStaleSeq):
		default:
			t.Fatalf("round %d: add_device %+v, rotation %+v; want one commit, and the loser stale_device_seq "+
				"(rotation lost) or stale_seq (add_device lost)", round, add0, rot1)
		}
	}
}

// D-192: two ops whose device keys are the same two keys in opposite roles
// insert in one global (ascending digest) order, so they wait, never deadlock.
// Sixteen rounds: under an unordered insert (mutation G-04) the deadlock needs
// one particular wake-up order, which a single round shows about half the time.
func TestOpposingKeyOrdersDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	for round := 0; round < 16; round++ {
		x, y := authoritytest.NewDevice(t), authoritytest.NewDevice(t)
		first := &authoritytest.Device{ID: x.ID, DSK: x.DSK, DKX: y.DSK}
		swapped := &authoritytest.Device{ID: y.ID, DSK: y.DSK, DKX: x.DSK}
		a, b := e.r.NewAccount(t), e.r.NewAccount(t)
		ba, bb := enrollSharing(t, a, first), enrollSharing(t, b, swapped)
		low := ac.KeyDigest(authoritytest.Point(x.DSK))
		if hi := ac.KeyDigest(authoritytest.Point(y.DSK)); bytes.Compare(hi, low) < 0 {
			low = hi
		}
		blocker, err := e.f.App.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := blocker.Exec(`INSERT INTO authority.key_bindings (key_digest, account_id, role, first_seq)
			VALUES ($1, gen_random_uuid(), 'dsk', 0)`, low); err != nil {
			t.Fatal(err)
		}
		done := make(chan []Outcome, 1)
		go func() { done <- runConcurrently(e, [][]byte{ba, bb}, false) }()
		waitForLockWaiters(t, e.f.Super, 2)
		_ = blocker.Rollback()
		for _, o := range <-done {
			if o.Kind == KindError {
				t.Fatalf("round %d: an op failed with %+v: the binding batch deadlocked or timed out", round, o)
			}
		}
	}
}
