package auth

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

type recordingRecoveryResetHub struct {
	events []string
}

func (h *recordingRecoveryResetHub) ClearCustomTextForPresenceAudience(uuid.UUID) {
	h.events = append(h.events, "clear")
}

func (h *recordingRecoveryResetHub) DisconnectUser(uuid.UUID) {
	h.events = append(h.events, "disconnect")
}

type legacyRecoveryDisconnector struct {
	disconnects int
}

func (h *legacyRecoveryDisconnector) DisconnectUser(uuid.UUID) {
	h.disconnects++
}

func TestClearCustomTextAndDisconnectClearsBeforeDisconnect(t *testing.T) {
	hub := &recordingRecoveryResetHub{}
	handler := &Handler{hub: hub}

	handler.clearCustomTextAndDisconnect(uuid.New())

	assert.Equal(t, []string{"clear", "disconnect"}, hub.events)
}

func TestClearCustomTextAndDisconnectSupportsLegacySessionDisconnector(t *testing.T) {
	hub := &legacyRecoveryDisconnector{}
	handler := &Handler{hub: hub}

	handler.clearCustomTextAndDisconnect(uuid.New())

	assert.Equal(t, 1, hub.disconnects)
}
