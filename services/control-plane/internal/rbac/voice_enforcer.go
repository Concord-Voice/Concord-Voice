package rbac

// VoiceEnforcer pushes recomputed effective permissions to voice-connected
// members after an RBAC mutation, so the media-plane's join-time permission
// snapshot cannot go stale mid-session (CV-CAN-007 review P1). Implemented by
// voice.PermissionEnforcer; declared here as a narrow interface because
// internal/voice already imports internal/rbac (a concrete dependency would
// cycle). Methods are fire-and-forget: implementations run on a background
// context so the push survives the originating request, and failures degrade
// to the join-snapshot behavior — they never block or fail the mutation.
type VoiceEnforcer interface {
	// RecheckUser re-pushes permissions for one member's current voice
	// channel(s) in the server (role assign/unassign scope).
	RecheckUser(serverID, userID string)
	// RecheckChannel re-pushes permissions for every member currently in the
	// given voice channel (channel-override scope).
	RecheckChannel(serverID, channelID string)
	// RecheckServer re-pushes permissions for every member currently in any
	// voice channel of the server (role-edit/delete scope).
	RecheckServer(serverID string)
	// DisconnectUser force-disconnects one member from every voice channel they
	// currently occupy in the server. Used by the member-timeout path, where the
	// join gate bars the member independently of the permission bitfield, so a
	// recheck would not evict them.
	DisconnectUser(serverID, userID string)
}

// SetVoiceEnforcer wires the mid-session voice permission push. Called once at
// router construction, before the handler serves traffic; a nil enforcer (or
// never calling this) leaves every recheck a no-op — the pre-push, join-time
// snapshot behavior.
func (h *Handler) SetVoiceEnforcer(e VoiceEnforcer) {
	h.voiceEnforcer = e
}

// recheckVoiceUser, recheckVoiceChannel, and recheckVoiceServer are nil-safe
// call-site helpers: RBAC mutation handlers call them immediately after the
// matching PermissionCache invalidation, so the enforcer's re-resolve computes
// from post-mutation state.
func (h *Handler) recheckVoiceUser(serverID, userID string) {
	if h.voiceEnforcer != nil {
		h.voiceEnforcer.RecheckUser(serverID, userID)
	}
}

func (h *Handler) recheckVoiceChannel(serverID, channelID string) {
	if h.voiceEnforcer != nil {
		h.voiceEnforcer.RecheckChannel(serverID, channelID)
	}
}

func (h *Handler) recheckVoiceServer(serverID string) {
	if h.voiceEnforcer != nil {
		h.voiceEnforcer.RecheckServer(serverID)
	}
}
