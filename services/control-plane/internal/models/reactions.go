package models

import "time"

// ReactionSummary is a grouped reaction for a message, used in API responses.
type ReactionSummary struct {
	Emoji string         `json:"emoji"`
	Count int            `json:"count"`
	Users []ReactionUser `json:"users"`
	Me    bool           `json:"me"`
}

// ReactionUser is a minimal user representation for reaction tooltips.
type ReactionUser struct {
	UserID      string  `json:"user_id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
}

// MessageReaction is a single user-emoji reaction on a message (DB row).
type MessageReaction struct {
	ID        string    `json:"id" db:"id"`
	MessageID string    `json:"message_id" db:"message_id"`
	UserID    string    `json:"user_id" db:"user_id"`
	Emoji     string    `json:"emoji" db:"emoji"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// RepliedToSummary is a truncated message preview for reply context.
type RepliedToSummary struct {
	ID          string  `json:"id"`
	UserID      string  `json:"user_id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
	Content     string  `json:"content"`
	KeyVersion  int     `json:"key_version"`
}
