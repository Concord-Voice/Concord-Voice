package presence

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	// ActivityStateTTL bounds ephemeral Rich Presence current state.
	ActivityStateTTL = 90 * time.Second
	// MaxActivitySourceVersion is the largest integer Lua can retain exactly.
	MaxActivitySourceVersion int64 = (1 << 53) - 1
	// MaxActivityUnixSeconds is the matching exact whole-second ceiling for
	// payload/update timestamps derived from a source-time generation.
	MaxActivityUnixSeconds int64 = MaxActivitySourceVersion / int64(time.Second/time.Microsecond)
)

const (
	serverVoiceLifecycleKeyPrefix = "voice:lifecycle:server:"
	privateCallLifecycleKeyPrefix = "voice:lifecycle:private:"
)

// IsValidActivitySourceTime reports whether eventAt converts to the exact,
// positive integer range shared by Go, PostgreSQL, JSON, and Redis Lua.
func IsValidActivitySourceTime(eventAt time.Time) bool {
	version := eventAt.UnixMicro()
	return version > 0 && version <= MaxActivitySourceVersion
}

// VoiceLifecycleKey returns the strict Redis lifecycle envelope key paired
// with one sender/category activity generation.
func VoiceLifecycleKey(senderID uuid.UUID, category Category) (string, error) {
	if senderID == uuid.Nil {
		return "", ErrInvalidActivityState
	}
	switch category {
	case CategoryServerVoice:
		return serverVoiceLifecycleKeyPrefix + senderID.String(), nil
	case CategoryPrivateCall:
		return privateCallLifecycleKeyPrefix + senderID.String(), nil
	default:
		return "", ErrInvalidActivityState
	}
}

var (
	// ErrInvalidActivityState rejects invalid trusted state before Redis mutation.
	ErrInvalidActivityState = errors.New("invalid rich-presence activity state")
	// ErrMalformedActivityState means persisted state failed strict decoding and was removed.
	ErrMalformedActivityState = errors.New("malformed rich-presence activity state")
	// ErrMalformedActivityLifecycle means a voice lifecycle fence failed strict
	// decoding and was removed so a subsequent authoritative event can recover.
	ErrMalformedActivityLifecycle = errors.New("malformed rich-presence activity lifecycle")
)

// ActivityState is the server-owned Redis envelope for one current category.
type ActivityState struct {
	SourceToken   uuid.UUID       `json:"source_token"`
	SourceVersion int64           `json:"source_version"`
	Minimized     bool            `json:"minimized"`
	Payload       json.RawMessage `json:"payload"`
	UpdatedAt     int64           `json:"updated_at"`
}
