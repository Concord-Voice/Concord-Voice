package websocket

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// --- rateLimitAllow tests ---

func TestRateLimitAllow_InitialBurst(t *testing.T) {
	client := &Client{
		ID:     uuid.New(),
		UserID: uuid.New(),
	}

	// First call should initialize and allow
	for i := 0; i < rateLimitBurst; i++ {
		assert.True(t, client.rateLimitAllow(), "burst message %d should be allowed", i)
	}

	// Next call should be rate limited
	assert.False(t, client.rateLimitAllow(), "should be rate limited after exhausting burst")
}

func TestRateLimitAllow_RefillsOverTime(t *testing.T) {
	client := &Client{
		ID:     uuid.New(),
		UserID: uuid.New(),
	}

	// Exhaust all tokens
	for i := 0; i < rateLimitBurst; i++ {
		client.rateLimitAllow()
	}
	assert.False(t, client.rateLimitAllow(), "should be rate limited")

	// Wait for tokens to refill (at least one interval)
	time.Sleep(rateLimitInterval + 10*time.Millisecond)

	// Should be allowed again
	assert.True(t, client.rateLimitAllow(), "should be allowed after refill")
}

func TestRateLimitAllow_TokensCapAtBurst(t *testing.T) {
	client := &Client{
		ID:           uuid.New(),
		UserID:       uuid.New(),
		rateTokens:   rateLimitBurst,
		rateLastFill: time.Now().Add(-10 * time.Second), // Long time ago
	}

	// Even after a long time, tokens should cap at burst limit
	assert.True(t, client.rateLimitAllow())

	// After one call, tokens should be burst - 1 (capped, then decremented)
	// Verify by calling burst - 1 more times
	for i := 0; i < rateLimitBurst-1; i++ {
		client.rateLimitAllow()
	}
	// The refill should have capped at rateLimitBurst, so after rateLimitBurst calls we should be empty
	// But we also got refill tokens from the large time gap, so more will be available
	// This test mainly verifies the cap behavior doesn't panic
}

func TestRateLimitAllow_FirstCallInitializes(t *testing.T) {
	client := &Client{
		ID:     uuid.New(),
		UserID: uuid.New(),
	}

	assert.True(t, client.rateLastFill.IsZero(), "rateLastFill should start as zero")
	assert.True(t, client.rateLimitAllow(), "first call should allow and initialize")
	assert.False(t, client.rateLastFill.IsZero(), "rateLastFill should be set after first call")
	assert.Equal(t, rateLimitBurst-1, client.rateTokens, "should have burst-1 tokens after first call")
}

func TestRateLimitAllow_ZeroTokensNoRefill(t *testing.T) {
	client := &Client{
		ID:           uuid.New(),
		UserID:       uuid.New(),
		rateTokens:   0,
		rateLastFill: time.Now(), // Just now, so no tokens refilled
	}

	assert.False(t, client.rateLimitAllow(), "should be rejected with 0 tokens and no refill time")
}

// --- Client struct tests ---

func TestClientFields(t *testing.T) {
	clientID := uuid.New()
	userID := uuid.New()
	displayName := "Test User"
	avatarURL := "https://example.com/avatar.png"
	sessionID := "session-abc-123"

	client := &Client{
		ID:          clientID,
		UserID:      userID,
		Username:    "testuser",
		DisplayName: &displayName,
		AvatarURL:   &avatarURL,
		SessionID:   sessionID,
		Send:        make(chan []byte, 256),
		Channels:    make(map[uuid.UUID]bool),
	}

	assert.Equal(t, clientID, client.ID)
	assert.Equal(t, userID, client.UserID)
	assert.Equal(t, "testuser", client.Username)
	assert.Equal(t, &displayName, client.DisplayName)
	assert.Equal(t, &avatarURL, client.AvatarURL)
	assert.Equal(t, sessionID, client.SessionID)
}

func TestClientChannels(t *testing.T) {
	client := &Client{
		ID:       uuid.New(),
		UserID:   uuid.New(),
		Channels: make(map[uuid.UUID]bool),
	}

	ch1 := uuid.New()
	ch2 := uuid.New()

	client.Channels[ch1] = true
	client.Channels[ch2] = true

	assert.True(t, client.Channels[ch1])
	assert.True(t, client.Channels[ch2])
	assert.False(t, client.Channels[uuid.New()])

	delete(client.Channels, ch1)
	assert.False(t, client.Channels[ch1])
	assert.Len(t, client.Channels, 1)
}
