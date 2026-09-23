package websocket

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScheduleSubscriptionRevalidationMarksPendingAndWakesRunLoop(t *testing.T) {
	hub := &Hub{
		subscriptionRevalidationWake: make(chan struct{}, 1),
	}

	hub.scheduleSubscriptionRevalidation()

	assert.True(t, hub.subscriptionRevalidationPending.Load())
	select {
	case <-hub.subscriptionRevalidationWake:
	default:
		t.Fatal("subscription revalidation did not signal the run loop")
	}
}

func TestMarkSubscriptionRevalidationPendingDoesNotRequireWakeChannel(t *testing.T) {
	hub := &Hub{}
	hub.markSubscriptionRevalidationPending()
	assert.True(t, hub.subscriptionRevalidationPending.Load())
}
