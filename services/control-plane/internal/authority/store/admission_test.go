package store

import (
	"testing"
	"time"
)

func TestEnrollNewKeepsItsReservedSlotWhenEveryGeneralSlotIsHeld(t *testing.T) {
	a := NewAdmission()
	var held []func()
	for i := 0; i < MutatingSlots-1; i++ {
		rel, ok := a.acquireMutating(classOther)
		if !ok {
			t.Fatalf("general slot %d refused", i)
		}
		held = append(held, rel)
	}
	if _, ok := a.acquireMutating(classOther); ok {
		t.Fatal("a fourth general op got a slot (the fourth is enroll_new's)")
	}
	rel, ok := a.acquireMutating(classEnrollNew)
	if !ok {
		t.Fatal("enroll_new was refused with its reserved slot free (D-179)")
	}
	rel()
	for _, r := range held {
		r()
	}
}

func TestCancelsUseTheCancelReserve(t *testing.T) {
	a := NewAdmission()
	for i := 0; i < MutatingSlots-1; i++ {
		if _, ok := a.acquireMutating(classOther); !ok {
			t.Fatal("general slot refused")
		}
	}
	for i := 0; i < CancelReserveSlots; i++ {
		if _, ok := a.acquireMutating(classCancel); !ok {
			t.Fatalf("cancel %d refused with the reserve free (D-271)", i)
		}
	}
	if _, ok := a.acquireMutating(classCancel); ok {
		t.Fatal("a cancel exceeded general + reserve")
	}
}

func TestCommitBucketExemptsCancelsAndRefillsAtCommitRate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a := newAdmissionAt(func() time.Time { return now })
	for i := 0; i < CommitBurst; i++ {
		if !a.takeCommitToken(classOther) {
			t.Fatalf("token %d refused within the burst", i)
		}
	}
	if a.takeCommitToken(classOther) {
		t.Fatal("the bucket gave a token past COMMIT_BURST")
	}
	if !a.takeCommitToken(classCancel) {
		t.Fatal("a cancel was refused by an empty bucket (D-211, D-271)")
	}
	now = now.Add(time.Second)
	for i := 0; i < CommitRate; i++ {
		if !a.takeCommitToken(classOther) {
			t.Fatalf("refill token %d refused after 1 s", i)
		}
	}
	if a.takeCommitToken(classOther) {
		t.Fatal("the bucket refilled faster than COMMIT_RATE")
	}
}

func TestDiskFloors(t *testing.T) {
	avail := uint64(1 << 40)
	s := &Store{adm: NewAdmission(), hooks: Hooks{Statfs: func() (uint64, error) { return avail, nil }}}
	if _, ok := s.diskCheck(classOther); !ok {
		t.Fatal("refused with plenty of space")
	}
	s.adm.SetDiskReady(false)
	if out, ok := s.diskCheck(classOther); ok || out.Code != "disk" {
		t.Fatalf("non-cancel below the soft floor = %+v, want 503 disk", out)
	}
	if _, ok := s.diskCheck(classCancel); !ok {
		t.Fatal("a cancel was refused below the soft floor only")
	}
	if s.nonClient503.Load() {
		t.Fatal("the soft floor set the tick's non-client 503 leg, though cancels stay open below it (B1-D31)")
	}
	s.adm.SetDiskReady(true)
	avail = DiskHardFloorBytes - 1
	if out, ok := s.diskCheck(classCancel); ok || out.Code != "disk" {
		t.Fatalf("a cancel below the hard floor = %+v, want 503 disk (every request)", out)
	}
	if !s.nonClient503.Load() {
		t.Fatal("a cancel refused at the hard floor did not set the tick's non-client 503 leg (D-147, B1-D31)")
	}
	avail = DiskHardFloorBytes
	if _, ok := s.diskCheck(classOther); !ok {
		t.Fatal("refused exactly at the hard floor")
	}
}
