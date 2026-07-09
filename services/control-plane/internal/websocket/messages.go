package websocket

import "github.com/google/uuid"

// IncomingMessage represents a message received from a client
type IncomingMessage struct {
	// Message type: subscribe, unsubscribe, message, typing, heartbeat, set_status
	Type string `json:"type"`

	// Message data (type-specific)
	Data map[string]interface{} `json:"data"`

	// Sender info (set by server)
	UserID   uuid.UUID `json:"-"`
	ClientID uuid.UUID `json:"-"`
}

// OutgoingMessage represents a message sent to a client
type OutgoingMessage struct {
	// Message type: connected, subscribed, message, typing, presence, error
	Type string `json:"type"`

	// Message data (type-specific)
	Data map[string]interface{} `json:"data"`
}

// BroadcastMessage represents a message to be broadcast to channel subscribers
type BroadcastMessage struct {
	// Target channel ID
	ChannelID uuid.UUID

	// Server ID and required view permission for auth-filtered channel delivery.
	// Zero values keep legacy internal broadcasts unchanged.
	ServerID       uuid.UUID
	ViewPermission int64

	// Message to send
	Data OutgoingMessage

	// Optional: exclude this user from receiving the message
	ExcludeUser *uuid.UUID

	// RequireViewAuth requests that the hub resolve the channel's server + view
	// permission in the run loop and filter each recipient by it. Set by
	// BroadcastToChannelAuthorized for REST-triggered channel-mutation events so
	// a stale/unauthorized subscriber does not receive them (CV-CAN-021..026).
	// Resolution must run in the hub loop (deliveryAuthForChannel mutates
	// subscription maps), so callers only set the flag — never ServerID /
	// ViewPermission directly for this path.
	RequireViewAuth bool
}

// UserBroadcastMessage represents a message to be sent to all clients of a specific user
type UserBroadcastMessage struct {
	// Target user ID
	UserID uuid.UUID

	// Message to send
	Data OutgoingMessage

	// Optional: exclude the client that triggered the broadcast
	ExcludeClientID *uuid.UUID
}

// ServerBroadcastMessage represents a message to be sent to all clients subscribed to a server
type ServerBroadcastMessage struct {
	// Target server ID
	ServerID uuid.UUID

	// Message to send
	Data OutgoingMessage

	// PruneUserAfter, when non-nil, makes handleServerBroadcast remove this user's
	// clients from the server subscription set AFTER delivering Data, within the same
	// serialized hub operation. This evicts a removed/banned member so they still
	// receive their own member_removed event but no later server fanout, while keeping
	// the eviction ordered relative to other server broadcasts on this channel
	// (e.g. a subsequent key_revocation). CV-CAN-027/028.
	PruneUserAfter *uuid.UUID
}

// PresenceUpdate represents a user presence change
type PresenceUpdate struct {
	UserID    uuid.UUID `json:"user_id"`
	Status    string    `json:"status"`    // online, offline, dnd, invisible
	Timestamp int64     `json:"timestamp"` // Unix timestamp
}

// DMBroadcastMessage represents a message to be sent to all clients subscribed to a DM conversation
type DMBroadcastMessage struct {
	// Target DM conversation ID
	ConversationID uuid.UUID

	// Message to send
	Data OutgoingMessage

	// Optional: exclude this user from receiving the message
	ExcludeUser *uuid.UUID
}

// TypingIndicator represents a typing indicator event
type TypingIndicator struct {
	UserID    uuid.UUID `json:"user_id"`
	ChannelID uuid.UUID `json:"channel_id"`
	IsTyping  bool      `json:"is_typing"`
}
