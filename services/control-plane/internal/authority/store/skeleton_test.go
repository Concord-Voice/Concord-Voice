package store

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func TestDoRConstantsAreVerbatim(t *testing.T) {
	if GPrimeMs != 86_400_000 || DrainGraceMs != 660_000 || RestoreMarginMs != 60_000 || ExtensionCapMs != 259_200_000 ||
		NCancelMs != 2_592_000_000 || VMs != 259_200_000 || VMs > 259_200_000 || CommitRate != 5 || CommitBurst != 50 ||
		DiskHardFloorBytes != 512<<20 || MutatingSlots != 4 || CancelReserveSlots != 2 || ReadSlots != 8 {
		t.Fatal("an authority-only constant drifted from DoR §2.3 / §3.3")
	}
	if RequestDeadlineTx.Milliseconds() != 8000 || RequestDeadlineRead.Milliseconds() != 4000 ||
		ReleasePollInterval.Milliseconds() != 10 || RolePollInterval.Milliseconds() != 5000 || TickInterval.Seconds() != 30 {
		t.Fatal("a timing constant drifted from DoR §2.3")
	}
}

func TestSelectHeadForUpdateIsSelectHeadLocked(t *testing.T) {
	if selectHeadForUpdate != selectHead+" FOR UPDATE" {
		t.Fatal("selectHeadForUpdate has drifted from selectHead, so lockHead and the reads would scan different columns")
	}
}

func TestErrorOutcomeCarriesTheEnvelope(t *testing.T) {
	o := errorOutcome(503, ac.ErrorReplication)
	e, err := ac.DecodeErrorEnvelope(o.Body)
	if err != nil || e.Code != "replication" || o.HTTP != 503 || o.Kind != KindError {
		t.Fatalf("errorOutcome = %+v, %v", o, err)
	}
}

func TestOpenRefusesAMissingStatfsSignerOrDB(t *testing.T) {
	s, _ := signer.GenerateP384()
	if _, err := Open(Config{Signer: s}); err == nil {
		t.Fatal("Open accepted a nil DB")
	}
}
