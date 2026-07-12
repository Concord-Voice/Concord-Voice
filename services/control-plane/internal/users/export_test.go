package users

import "github.com/google/uuid"

// CustomStatusCoordinatorForTest is the test-only shape used to inject an
// observable same-sender coordinator from the external integration-test
// package without exporting a production API.
type CustomStatusCoordinatorForTest interface {
	WithSender(uuid.UUID, func())
}

// PresenceOverrideBroadcasterForTest exposes the existing private broadcaster
// contract to external tests only.
type PresenceOverrideBroadcasterForTest = presenceOverrideBroadcaster

// CustomTextResetBroadcasterForTest is the test-only destructive-reset clear
// seam used to verify post-commit delivery ordering.
type CustomTextResetBroadcasterForTest interface {
	ClearCustomTextForPresenceAudience(uuid.UUID)
}

// KeyResetSessionDisconnectorForTest is the test-only destructive-reset
// session termination seam.
type KeyResetSessionDisconnectorForTest interface {
	DisconnectUser(uuid.UUID)
}

// SetCustomStatusCoordinatorForTest replaces the handler's coordinator for a
// deterministic integration test.
func SetCustomStatusCoordinatorForTest(h *Handler, coordinator CustomStatusCoordinatorForTest) {
	h.customStatusCoordinator = coordinator
}

// SetPresenceOverrideBroadcasterForTest installs a deterministic test double.
func SetPresenceOverrideBroadcasterForTest(h *Handler, broadcaster PresenceOverrideBroadcasterForTest) {
	h.presenceOverrideBroadcaster = broadcaster
}

// SetCustomTextResetBroadcasterForTest installs a deterministic reset clearer.
func SetCustomTextResetBroadcasterForTest(h *Handler, broadcaster CustomTextResetBroadcasterForTest) {
	h.customTextResetBroadcaster = broadcaster
}

// SetKeyResetSessionDisconnectorForTest installs a deterministic session
// termination test double.
func SetKeyResetSessionDisconnectorForTest(h *Handler, disconnector KeyResetSessionDisconnectorForTest) {
	h.sessionDisconnector = disconnector
}
