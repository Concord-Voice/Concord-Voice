package store

import (
	"sync"
	"sync/atomic"
	"time"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

type opClass uint8

const (
	classOther     opClass = iota
	classCancel            // cancel_pending, recover, both cancel_recovery_bind forms (D-271, RB3-10)
	classEnrollNew         // the registration lane (D-179)
)

func classOfOp(op string) opClass {
	switch op {
	case ac.OpCancelPending, ac.OpRecover, ac.OpCancelRecoveryBind:
		return classCancel
	case ac.OpEnrollNew:
		return classEnrollNew
	}
	return classOther
}

// Admission holds the in-flight classes of §3.3 and the commit bucket of
// §3.2.4 step 2b. B2 shares one instance between its handlers and the store.
type Admission struct {
	general, enroll, cancel, read chan struct{}
	now                           func() time.Time
	mu                            sync.Mutex
	tokens                        float64
	last                          time.Time
	diskReady                     atomic.Bool
}

// NewAdmission builds the admission state with its slots full and disk_ready set.
func NewAdmission() *Admission { return newAdmissionAt(time.Now) }

func newAdmissionAt(now func() time.Time) *Admission {
	a := &Admission{general: make(chan struct{}, MutatingSlots-1), enroll: make(chan struct{}, 1),
		cancel: make(chan struct{}, CancelReserveSlots), read: make(chan struct{}, ReadSlots), now: now,
		tokens: CommitBurst, last: now()}
	a.diskReady.Store(true)
	return a
}

// SetDiskReady is the disk watcher's output (B2's tick, LD-25 hysteresis).
func (a *Admission) SetDiskReady(ok bool) { a.diskReady.Store(ok) }

func try(ch chan struct{}) (func(), bool) {
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	default:
		return nil, false
	}
}

// acquireGeneral takes a general mutating slot only. Steps 1–2 decode under
// it, so a burst of bodies cannot decode outside admission (review round 2).
func (a *Admission) acquireGeneral() (func(), bool) { return try(a.general) }

// acquireMutating never blocks: no slot is 503 busy.
func (a *Admission) acquireMutating(c opClass) (func(), bool) {
	if rel, ok := try(a.general); ok {
		return rel, true
	}
	switch c {
	case classCancel:
		return try(a.cancel)
	case classEnrollNew:
		return try(a.enroll)
	}
	return nil, false
}

func (a *Admission) acquireRead() (func(), bool) { return try(a.read) }

// takeCommitToken is the global bucket (COMMIT_RATE/COMMIT_BURST). The cancel
// class is exempt (D-211, D-271).
func (a *Admission) takeCommitToken(c opClass) bool {
	if c == classCancel {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.tokens = min(float64(CommitBurst), a.tokens+now.Sub(a.last).Seconds()*CommitRate)
	a.last = now
	if a.tokens < 1 {
		return false
	}
	a.tokens--
	return true
}

// diskCheck: a non-cancel op while disk_ready = 0 → 503 disk, then the hard
// floor. The soft floor leaves cancels open, so it does not set the tick's
// D-147 leg (B1-D31).
func (s *Store) diskCheck(c opClass) (Outcome, bool) {
	if c != classCancel && !s.adm.diskReady.Load() {
		return errorOutcome(503, ac.ErrorDisk), false
	}
	return s.hardFloorCheck()
}

// hardFloorCheck: any request below DISK_HARD_FLOOR by this request's own
// Statfs → 503 disk (D-232), reads included (Task 13). A Statfs error fails
// closed. It refuses cancels too, so it sets the tick's D-147 leg (B1-D31).
func (s *Store) hardFloorCheck() (Outcome, bool) {
	avail, err := s.hooks.Statfs()
	if err != nil || avail < DiskHardFloorBytes {
		s.nonClient503.Store(true)
		return errorOutcome(503, ac.ErrorDisk), false
	}
	return Outcome{}, true
}
