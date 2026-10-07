package messages

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPurgeProvenance_ZeroIsNotAProvenance pins the fail-closed shape of the
// tri-state (#3454 A-3.6): the three named values are distinct, and none is
// the zero value, so a serverPurgeRequest that never set its provenance can
// be told apart from every deliberate one (and read as unconfirmed).
func TestPurgeProvenance_ZeroIsNotAProvenance(t *testing.T) {
	var zero PurgeProvenance
	named := map[PurgeProvenance]bool{PurgeExempt: true, PurgeConfirmed: true, PurgeUnconfirmed: true}
	require.Len(t, named, 3, "the three provenances must be distinct")
	require.NotContains(t, named, zero, "the zero value must not be a provenance")
}
