package store

import "testing"

func TestTickIncrementLegs(t *testing.T) {
	const tick = uint64(30_000)
	cases := []struct {
		name               string
		prev, now, peer    uint64
		reconciled, non503 bool
		want               uint64
	}{
		{"healthy", 1_000_000, 1_030_000, 1_029_000, true, false, 0},
		{"gap over 90 s adds the excess", 1_000_000, 1_100_000, 1_099_000, true, false, 10_000},
		{"gap of exactly 90 s adds nothing", 1_000_000, 1_090_000, 1_089_000, true, false, 0},
		{"unreconciled adds a full tick", 1_000_000, 1_030_000, 1_029_000, false, false, tick},
		{"no CP contact in 120 s", 1_010_000, 1_130_000, 1_009_999, true, false, 30_000 + tick},
		{"CP contact exactly 120 s ago is contact", 1_000_000, 1_030_000, 910_000, true, false, 0},
		{"a non-client 503", 1_000_000, 1_030_000, 1_029_000, true, true, tick},
		{"several legs add one tick (B1-D25)", 1_000_000, 1_030_000, 0, false, true, tick},
	}
	for _, c := range cases {
		if got := tickIncrement(c.prev, c.now, c.reconciled, c.peer, c.non503); got != c.want {
			t.Fatalf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestPurgeDue(t *testing.T) {
	if !purgeDue(0, 5) || purgeDue(1_000, 1_000+86_400_000-1) || !purgeDue(1_000, 1_000+86_400_000) {
		t.Fatal("purge is due when last_purge_at is NULL or ≥ 24 h ago, and only then")
	}
}
