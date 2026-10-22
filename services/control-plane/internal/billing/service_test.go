package billing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewService_Defaults(t *testing.T) {
	t.Parallel()
	s := NewService(Deps{Environment: "development"})
	require.NotNil(t, s)
	assert.Equal(t, 4, cap(s.sem), "at most four Stripe-bound reconciles run at once")
	assert.Equal(t, "development", s.Environment)
	assert.NotNil(t, s.discovery)
	assert.WithinDuration(t, time.Now(), s.now(), time.Minute, "now defaults to the wall clock")
}

func TestNewTierChanger_ForwardsToEntitlements(t *testing.T) {
	t.Parallel()
	changer := NewTierChanger(nil, nil)
	require.NotNil(t, changer)
	_, ok := changer.(tierChangeAdapter)
	assert.True(t, ok, "the production changer is the entitlements adapter")
}
