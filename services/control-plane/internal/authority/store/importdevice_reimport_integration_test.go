package store

import (
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// TestImportRowTwentyTwoReimportedKeepsTheL1Exit pins B3-D18 (found by the
// PR-B3 pre-PR red-team pass). clear-held's P/D/Pr exit is two writes (DoR:1734,
// LD-46, D-146): row 22, an event, and then under L1 held=false,
// force_unverified=true and the G′ floor, a head write that no event carries
// and that only authority_owner may make (account_heads_guard). Import
// re-admits row 22 on a head restored from a backup taken before the hold
// (D-288), so a second restore from such a backup replays only the first half.
// The CP's retained head is then the imported one, so restore step 7 posts no
// /hold, and reconcile-complete floors only pending heads and filled slots,
// neither of which row 22 leaves. Without B3-D18 the account left the restore
// unheld, with force_unverified false and no floor, and the root the owner was
// replacing could rotate_root at once. With it the replay holds the account,
// and the owner's clear-held re-runs the L1 half.
func TestImportRowTwentyTwoReimportedKeepsTheL1Exit(t *testing.T) {
	e := newEnv(t)
	p, it, _ := rowTwentyTwoFixture(t, e)

	// Control: the first restore's clear-held left the exit D-146 requires.
	if c := readCleared(t, e.f.Super, p.ID); c.held || !c.forceUnv || !c.activation.Valid {
		t.Fatalf("fixture: post-clear head %+v, want unheld, force_unverified and a G' floor", c)
	}
	p.Sync(t, e.f.App)
	candidate := p.Pending
	e.refusedAs(p.RotateRoot(t).Bytes, ac.RefusalWrongState) // the old root is fenced
	p.Pending = candidate

	// The second restore, from the backup taken before the hold ("pre22"). The CP
	// imports every bundle it retains (restore step 7): for this account, row 22.
	rewind(t, e, "pre22", p)
	beginImport(t, e, restorePoint{startedMs: e.now.Load() + 1}) // restore-begin now, as the runbook runs it
	importOK(t, e, importBatch(t, it))
	// Step 7's /hold names only an account whose retained head is ahead of the
	// imported head; the retained head IS the imported row 22, so the CP posts
	// none for it. Then reconcile-complete (step 9).
	if _, err := ownerOver(t, e.f, e.r, OwnerConfig{}).ReconcileComplete(liveCtx(t)); err != nil {
		t.Fatalf("ReconcileComplete = %v", err)
	}

	after := readCleared(t, e.f.Super, p.ID)
	t.Logf("head after the second restore: %+v", after)
	p.Sync(t, e.f.App)
	e.catchUp(t)
	e.advance(ac.IntentWindowMs + 1_000) // past the post-restore intent fence (expiry's restore_started_at clause)
	out := e.submit(p.RotateRoot(t).Bytes)
	t.Logf("the pre-restore root's rotate_root after the second restore: kind=%v http=%d code=%q", out.Kind, out.HTTP, out.Code)
	if out.Kind == KindCommitted {
		t.Fatalf("after the second restore the pre-restore root's rotate_root COMMITTED "+
			"(held=%v force_unverified=%v activation_floor_ms=%v); D-146/LD-46 require wrong_state until an "+
			"unverified replacement activates", after.held, after.forceUnv, after.activation)
	}
	if !after.held && !after.forceUnv {
		t.Fatalf("after the second restore the head is neither held nor force_unverified: %+v", after)
	}
	p.Pending = candidate

	// Convergence: the owner's clear-held now re-runs the L1 half, and the head
	// is where the first restore's clear-held left it.
	got, err := signedOwner(t, e).ClearHeld(liveCtx(t), []string{p.ID})
	if err != nil {
		t.Fatalf("ClearHeld = %v", err)
	}
	wantResults(t, got, ClearResult{p.ID, ClearUnverified})
	if c := readCleared(t, e.f.Super, p.ID); c.held || !c.forceUnv || !c.activation.Valid {
		t.Fatalf("after clear-held the head is %+v, want unheld, force_unverified and a G' floor", c)
	}
	p.Sync(t, e.f.App)
	e.catchUp(t)
	e.refusedAs(p.RotateRoot(t).Bytes, ac.RefusalWrongState)
}
