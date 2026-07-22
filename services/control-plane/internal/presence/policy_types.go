package presence

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// Category identifies a supported rich-presence payload category.
type Category string

// Supported rich-presence categories.
const (
	CategoryServerVoice Category = "server_voice"
	CategoryPrivateCall Category = "private_call"
)

// Tier controls the breadth of a rich-presence audience.
type Tier int16

// Supported rich-presence audience tiers.
const (
	TierOff Tier = iota
	TierFriends
	TierServers
)

// ServerVoiceContext binds a server-voice payload to its authoritative scope.
type ServerVoiceContext struct {
	ServerID  uuid.UUID
	ChannelID uuid.UUID
}

// PrivateCallContext binds a private-call payload to its current participants.
type PrivateCallContext struct {
	ConversationID uuid.UUID
	ParticipantIDs []uuid.UUID
}

// ServerVoicePayload is the serializable Server Voice presence shape.
type ServerVoicePayload struct {
	ChannelID   uuid.UUID `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	ServerID    uuid.UUID `json:"server_id"`
	ServerName  string    `json:"server_name"`
	StartedAt   *int64    `json:"started_at,omitempty"`
}

// PrivateCallPayload is the serializable Private Call presence shape.
type PrivateCallPayload struct {
	CallType         string `json:"call_type"`
	ParticipantCount int    `json:"participant_count"`
	StartedAt        *int64 `json:"started_at,omitempty"`
}

// ServerVoicePolicyInput groups trusted Server Voice context and payload data.
type ServerVoicePolicyInput struct {
	Context ServerVoiceContext
	Payload ServerVoicePayload
}

// PrivateCallPolicyInput groups trusted Private Call context and payload data.
type PrivateCallPolicyInput struct {
	Context PrivateCallContext
	Payload PrivateCallPayload

	// buildSnapshot is an immutable request/event-local authoritative read
	// produced only by ActivityBuilder. It lets the policy reuse the exact
	// participant proof instead of re-querying the entire call per sender.
	buildSnapshot *privateCallBuildSnapshot
}

// PolicyInput carries one category-specific rich-presence authorization input.
type PolicyInput struct {
	SenderID    uuid.UUID
	Category    Category
	ServerVoice *ServerVoicePolicyInput
	PrivateCall *PrivateCallPolicyInput
}

// Decision contains the authorized audience and serialized payload.
type Decision struct {
	Audience  map[uuid.UUID]bool
	Payload   json.RawMessage
	Minimized bool
}

// FailureClass is a stable, non-sensitive rich-presence policy error category.
type FailureClass string

// Stable rich-presence policy failure classes.
const (
	FailureUnknown           FailureClass = "unknown"
	FailureInvalidInput      FailureClass = "invalid_input"
	FailureSettingsRead      FailureClass = "settings_read"
	FailureStateRead         FailureClass = "state_read"
	FailureAudienceRead      FailureClass = "audience_read"
	FailureAuthorizationRead FailureClass = "authorization_read"
	FailureMinimization      FailureClass = "minimization"
)

// PolicyError classifies a policy failure without exposing its cause publicly.
type PolicyError struct {
	class FailureClass
	cause error
}

// Error returns the fixed public message for this failure class.
func (e *PolicyError) Error() string {
	return "rich-presence policy failed: " + string(e.class)
}

// Unwrap returns the underlying policy failure cause.
func (e *PolicyError) Unwrap() error {
	return e.cause
}

// PolicyErrorClass returns a policy error's stable class or FailureUnknown.
func PolicyErrorClass(err error) FailureClass {
	var policyErr *PolicyError
	if errors.As(err, &policyErr) {
		return policyErr.class
	}
	return FailureUnknown
}
