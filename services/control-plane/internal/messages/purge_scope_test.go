package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
)

func TestServerPurgeEventScopes(t *testing.T) {
	deletes := []purge.DeleteSpec{{ScopeID: "changed"}, {ScopeID: "maybe"}, {ScopeID: "empty"}}
	for _, tc := range []struct {
		name   string
		result purge.Result
		want   []string
	}{
		{"known commit", purge.Result{DeletedCount: 1, DeletedByScope: map[string]int{"changed": 1}}, []string{"changed"}},
		{"reconciled additional commit", purge.Result{DeletedCount: 2, DeletedByScope: map[string]int{"changed": 1}}, []string{"changed", "maybe", "empty"}},
		{"unreadable reconciliation", purge.Result{DeletedCount: 1, DeletedByScope: map[string]int{"changed": 1}, AffectedScopesUncertain: true}, []string{"changed", "maybe", "empty"}},
		{"unknown first commit", purge.Result{AffectedScopesUncertain: true}, []string{"changed", "maybe", "empty"}},
		{"confirmed zero", purge.Result{}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, serverPurgeEventScopes(deletes, tc.result))
		})
	}
}
