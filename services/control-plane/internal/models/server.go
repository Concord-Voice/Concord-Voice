// Package models contains data models for the Concord platform.
package models

import "time"

// Server represents a Concord server (Discord-like community)
type Server struct {
	ID                   string    `json:"id" db:"id"`
	Name                 string    `json:"name" db:"name"`
	IconURL              *string   `json:"icon_url,omitempty" db:"icon_url"`
	BannerURL            *string   `json:"banner_url,omitempty" db:"banner_url"`
	OwnerID              string    `json:"owner_id" db:"owner_id"`
	AllowEmbeddedContent bool      `json:"allow_embedded_content" db:"allow_embedded_content"`
	CreatedAt            time.Time `json:"created_at" db:"created_at"`
	UpdatedAt            time.Time `json:"updated_at" db:"updated_at"`
}

// ServerMember represents a user's membership in a server
type ServerMember struct {
	ServerID string    `json:"server_id" db:"server_id"`
	UserID   string    `json:"user_id" db:"user_id"`
	Role     string    `json:"role" db:"role"` // owner, admin, member
	JoinedAt time.Time `json:"joined_at" db:"joined_at"`
}

// ServerWithRole combines server info with user's role
type ServerWithRole struct {
	Server
	Role        string `json:"role" db:"role"`
	MemberCount int    `json:"member_count" db:"member_count"`
	OnlineCount int    `json:"online_count" db:"online_count"`
}

// ChannelGroup represents a user-defined category for organizing channels
type ChannelGroup struct {
	ID        string    `json:"id" db:"id"`
	ServerID  string    `json:"server_id" db:"server_id"`
	Name      string    `json:"name" db:"name"`
	Position  int       `json:"position" db:"position"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// Channel represents a voice or text channel within a server
type Channel struct {
	ID                   string    `json:"id" db:"id"`
	ServerID             string    `json:"server_id" db:"server_id"`
	Name                 string    `json:"name" db:"name"`
	Description          *string   `json:"description,omitempty" db:"description"`
	Type                 string    `json:"type" db:"type"`                                                 // voice, text, bulletin
	Emoji                *string   `json:"emoji,omitempty" db:"emoji"`                                     // Optional custom emoji
	AudioQualityTier     *string   `json:"audio_quality_tier,omitempty" db:"audio_quality_tier"`           // voice/standard/high/hifi/studio or nil (personal)
	GroupID              *string   `json:"group_id,omitempty" db:"group_id"`                               // FK to channel_groups; nil = uncategorized
	LinkedVoiceChannelID *string   `json:"linked_voice_channel_id,omitempty" db:"linked_voice_channel_id"` // Non-nil = hidden text chat for a voice channel
	SyncPermissions      bool      `json:"sync_permissions" db:"sync_permissions"`                         // Whether channel overrides sync from parent category
	Position             int       `json:"position" db:"position"`
	CreatedAt            time.Time `json:"created_at" db:"created_at"`
	UpdatedAt            time.Time `json:"updated_at" db:"updated_at"`
}

// ChannelKey represents a channel symmetric key wrapped for a specific user
type ChannelKey struct {
	ID         string    `json:"id" db:"id"`
	ChannelID  string    `json:"channel_id" db:"channel_id"`
	UserID     string    `json:"user_id" db:"user_id"`
	WrappedKey string    `json:"wrapped_key" db:"wrapped_key"`
	KeyVersion int       `json:"key_version" db:"key_version"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
}

// Message represents a chat message in a channel
type Message struct {
	ID               string     `json:"id" db:"id"`
	ChannelID        string     `json:"channel_id" db:"channel_id"`
	UserID           string     `json:"user_id" db:"user_id"`
	Content          string     `json:"content" db:"content"`
	KeyVersion       int        `json:"key_version" db:"key_version"`
	EmbedsSuppressed bool       `json:"embeds_suppressed" db:"embeds_suppressed"`
	GifSlug          *string    `json:"gif_slug,omitempty" db:"gif_slug"`
	ReplyToID        *string    `json:"reply_to_id,omitempty" db:"reply_to_id"`
	PinnedAt         *time.Time `json:"pinned_at,omitempty" db:"pinned_at"`
	PinnedBy         *string    `json:"pinned_by,omitempty" db:"pinned_by"`
	EditedAt         *time.Time `json:"edited_at,omitempty" db:"edited_at"`
	CreatedAt        time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at" db:"updated_at"`
}

// MessageWithUser combines message with user details
type MessageWithUser struct {
	Message
	Username    string              `json:"username" db:"username"`
	DisplayName *string             `json:"display_name,omitempty" db:"display_name"`
	AvatarURL   *string             `json:"avatar_url,omitempty" db:"avatar_url"`
	Reactions   []ReactionSummary   `json:"reactions,omitempty"`
	RepliedTo   *RepliedToSummary   `json:"replied_to,omitempty"`
	Attachments []AttachmentSummary `json:"attachments,omitempty"`
}

// ServerInvite represents an invite code for joining a server
type ServerInvite struct {
	ID        string     `json:"id" db:"id"`
	ServerID  string     `json:"server_id" db:"server_id"`
	Code      string     `json:"code" db:"code"`
	CreatedBy string     `json:"created_by" db:"created_by"`
	MaxUses   *int       `json:"max_uses" db:"max_uses"`
	UseCount  int        `json:"use_count" db:"use_count"`
	ExpiresAt *time.Time `json:"expires_at,omitempty" db:"expires_at"`
	IsRevoked bool       `json:"is_revoked" db:"is_revoked"`
	CreatedAt time.Time  `json:"created_at" db:"created_at"`
}

// ServerInviteWithCreator combines invite info with creator's username
type ServerInviteWithCreator struct {
	ServerInvite
	CreatorUsername string `json:"creator_username" db:"creator_username"`
}

// ChannelUnreadCount represents the unread message count for a channel
type ChannelUnreadCount struct {
	ChannelID   string `json:"channel_id"`
	UnreadCount int    `json:"unread_count"`
}
