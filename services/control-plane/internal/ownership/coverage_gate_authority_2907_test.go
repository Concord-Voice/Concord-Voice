package ownership

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPresenceReconciliationHelpersFailClosedForNilDependencies(t *testing.T) {
	h := &Handler{}
	require.False(t, h.HasPresenceRecheck())
	h.presenceExecute(nil)
	h.presenceAbandon(nil, "coverage")
	require.NotPanics(t, func() { h.recheckVoiceBothParties("server", "from", "to") })
}
