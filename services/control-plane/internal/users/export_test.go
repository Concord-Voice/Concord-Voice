package users

import "github.com/google/uuid"

// KeyResetSessionDisconnectorForTest is the test-only destructive-reset
// session termination seam.
type KeyResetSessionDisconnectorForTest interface {
	DisconnectUser(uuid.UUID)
}

// SetKeyResetSessionDisconnectorForTest installs a deterministic session
// termination test double.
func SetKeyResetSessionDisconnectorForTest(h *Handler, disconnector KeyResetSessionDisconnectorForTest) {
	h.sessionDisconnector = disconnector
}
