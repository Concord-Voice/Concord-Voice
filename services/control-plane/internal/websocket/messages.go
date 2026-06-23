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
