// Package roomplacement fences media room ownership across colour changes.
package roomplacement

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	reservationTTLMillis = 45_000
	liveTTLMillis        = 90_000
	joinIntentTTLMillis  = 60_000
	redisTimeout         = time.Second
	roomOwnerKeyPrefix   = "voice:room_owner:"
	roomLiveKeyPrefix    = "voice:room_live:"
)

// ErrUnavailable means no registered active colour can accept a new room.
var ErrUnavailable = errors.New("voice room placement unavailable")

// Selector claims the authoritative media colour for a room. Enabled remains
// false until the media create guard and deployment registration are present.
type Selector struct {
	Redis   *redis.Client
	Enabled bool
}

// LifecycleAuthority describes whether this media instance may mutate room state.
type LifecycleAuthority int64

const (
	// LifecycleRefuse forbids a stale or foreign instance from mutating a room.
	LifecycleRefuse LifecycleAuthority = iota
	// LifecycleRefreshOnly permits lease refresh without room lifecycle changes.
	LifecycleRefreshOnly
	// LifecycleFull permits lifecycle changes for the current owner and generation.
	LifecycleFull
)

var claimRoomScript = redis.NewScript(`
local roomLive = redis.call('GET', KEYS[1])
local roomOwner = redis.call('GET', KEYS[2])
local active = redis.pcall('GET', KEYS[3])
local registry = redis.pcall('SMEMBERS', KEYS[4])
local registryUsable = type(registry) == 'table' and #registry > 0
local registered = {}
if registryUsable then
  for _, colour in ipairs(registry) do registered[colour] = true end
end

local function validColour(colour)
  return colour == 'blue' or colour == 'green'
end
local function draining(colour)
  return redis.call('EXISTS', 'media:colour_draining:' .. colour) == 1
end
local function registeredColour(colour)
  return not registryUsable or registered[colour] == true
end

if roomLive then
  local liveColour = nil
  if string.sub(roomLive, 1, 5) == 'blue-' and #roomLive > 5 then
    liveColour = 'blue'
  elseif string.sub(roomLive, 1, 6) == 'green-' and #roomLive > 6 then
    liveColour = 'green'
  end
  if liveColour and registeredColour(liveColour) then
    redis.call('SET', KEYS[2], liveColour, 'PX', ARGV[1])
    return {1, liveColour}
  end
end

if roomOwner and not validColour(roomOwner) then return {0, ''} end
if roomOwner and registeredColour(roomOwner) and not draining(roomOwner) then
  local ttl = redis.call('PTTL', KEYS[2])
  if ttl < tonumber(ARGV[2]) then redis.call('PEXPIRE', KEYS[2], ARGV[2]) end
  return {1, roomOwner}
end

if type(active) ~= 'string' or not validColour(active) then return {0, ''} end
if draining(active) then return {0, ''} end

redis.call('SET', KEYS[2], active, 'PX', ARGV[2])
return {1, active}
`)

var guardLifecycleScript = redis.NewScript(`
local owner = redis.call('GET', KEYS[1])
local live = redis.call('GET', KEYS[2])
local colour = ARGV[1]
local instance = ARGV[2]
local subject = ARGV[3]
local ttl = tonumber(ARGV[4])
if owner and owner ~= colour then return 0 end
if owner == colour then
  if subject == 'voice.room_empty' then
    local currentTTL = redis.call('PTTL', KEYS[1])
    if currentTTL > ttl then redis.call('PEXPIRE', KEYS[1], ttl) end
  else
    redis.call('PEXPIRE', KEYS[1], ARGV[5])
  end
  if live == instance then return 2 end
  return 1
end
if live and live ~= instance then return 0 end
if live == instance then
  redis.call('SET', KEYS[1], colour, 'PX', ARGV[5])
  return 2
end
return 1
`)

var mintJoinIntentScript = redis.NewScript(`
return redis.call('SET', KEYS[1], '1', 'NX', 'PX', ARGV[1])
`)

var consumeJoinIntentScript = redis.NewScript(`
return redis.call('GETDEL', KEYS[1])
`)

// GuardLifecycle applies the D8 owner/instance fence before a lifecycle
// handler opens a transaction. instanceID is compared as an opaque value.
func (s *Selector) GuardLifecycle(
	ctx context.Context,
	roomID, colour, instanceID, subject string,
) (LifecycleAuthority, error) {
	if _, err := uuid.Parse(roomID); err != nil || s == nil || s.Redis == nil ||
		(colour != "blue" && colour != "green") || instanceID == "" || len(instanceID) > 128 {
		return LifecycleRefuse, ErrUnavailable
	}
	switch subject {
	case "voice.joined", "voice.left", "voice.room_empty", "voice.heartbeat":
	default:
		return LifecycleRefuse, ErrUnavailable
	}
	guardCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	result, err := guardLifecycleScript.Run(guardCtx, s.Redis, []string{
		roomOwnerKeyPrefix + roomID,
		roomLiveKeyPrefix + roomID,
	}, colour, instanceID, subject, reservationTTLMillis, liveTTLMillis).Int64()
	if err != nil {
		return LifecycleRefuse, fmt.Errorf("guard voice lifecycle ownership: %w", err)
	}
	if result < int64(LifecycleRefuse) || result > int64(LifecycleFull) {
		return LifecycleRefuse, fmt.Errorf("%w: invalid lifecycle authority %d", ErrUnavailable, result)
	}
	return LifecycleAuthority(result), nil
}

// LifecycleStillFull rechecks destructive authority without renewing the owner
// marker a second time after a lifecycle transaction has waited for its lock.
func (s *Selector) LifecycleStillFull(ctx context.Context, roomID, colour, instanceID string) (bool, error) {
	roomUUID, err := uuid.Parse(roomID)
	if err != nil || roomUUID == uuid.Nil || s == nil || s.Redis == nil ||
		(colour != "blue" && colour != "green") || instanceID == "" || len(instanceID) > 128 {
		return false, ErrUnavailable
	}
	readCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	markers, err := s.Redis.MGet(readCtx,
		roomOwnerKeyPrefix+roomUUID.String(), roomLiveKeyPrefix+roomUUID.String(),
	).Result()
	if err != nil {
		return false, fmt.Errorf("recheck voice lifecycle ownership: %w", err)
	}
	if len(markers) != 2 {
		return false, ErrUnavailable
	}
	return (markers[0] == nil || markers[0] == colour) && markers[1] == instanceID, nil
}

// HasLiveRoom suspends destructive stale-row cleanup while media occupancy exists.
func (s *Selector) HasLiveRoom(ctx context.Context, roomID string) (bool, error) {
	if s == nil || !s.Enabled {
		return false, nil
	}
	if s.Redis == nil {
		return false, ErrUnavailable
	}
	readCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	exists, err := s.Redis.Exists(readCtx, roomLiveKeyPrefix+roomID).Result()
	if err != nil {
		return false, fmt.Errorf("read voice room occupancy: %w", err)
	}
	return exists != 0, nil
}

// MintJoinIntent authorizes one refresh-only voice.joined insert for a room/user.
func (s *Selector) MintJoinIntent(ctx context.Context, roomID, userID string) error {
	roomUUID, roomErr := uuid.Parse(roomID)
	userUUID, userErr := uuid.Parse(userID)
	if s == nil || !s.Enabled || s.Redis == nil || roomErr != nil || userErr != nil ||
		roomUUID == uuid.Nil || userUUID == uuid.Nil {
		return ErrUnavailable
	}
	intentCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	_, err := mintJoinIntentScript.Run(intentCtx, s.Redis, []string{
		"voice:join_intent:" + roomUUID.String() + ":" + userUUID.String(),
	}, joinIntentTTLMillis).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("mint voice join intent: %w", err)
	}
	return nil
}

// ConsumeJoinIntent atomically spends the one-use admission intent.
func (s *Selector) ConsumeJoinIntent(ctx context.Context, roomID, userID string) (bool, error) {
	roomUUID, roomErr := uuid.Parse(roomID)
	userUUID, userErr := uuid.Parse(userID)
	if s == nil || !s.Enabled || s.Redis == nil || roomErr != nil || userErr != nil ||
		roomUUID == uuid.Nil || userUUID == uuid.Nil {
		return false, ErrUnavailable
	}
	intentCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	value, err := consumeJoinIntentScript.Run(intentCtx, s.Redis, []string{
		"voice:join_intent:" + roomUUID.String() + ":" + userUUID.String(),
	}).Text()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("consume voice join intent: %w", err)
	}
	return value == "1", nil
}

// Select returns the original URL while placement is intentionally inactive.
// Once enabled, Redis failure or an invalid/missing active colour fails closed.
func (s *Selector) Select(ctx context.Context, roomID, mediaURL string) (string, error) {
	if s == nil || !s.Enabled {
		return mediaURL, nil
	}
	if s.Redis == nil || strings.TrimSpace(roomID) == "" {
		return "", ErrUnavailable
	}
	parsed, err := url.Parse(mediaURL)
	if err != nil {
		return "", fmt.Errorf("parse media plane URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid media plane URL")
	}
	claimCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	result, err := claimRoomScript.Run(claimCtx, s.Redis, []string{
		roomLiveKeyPrefix + roomID,
		roomOwnerKeyPrefix + roomID,
		"media:active_colour",
		"media:colours:registered",
	}, liveTTLMillis, reservationTTLMillis).Slice()
	if err != nil {
		return "", fmt.Errorf("claim voice room placement: %w", err)
	}
	if len(result) != 2 || result[0] != int64(1) {
		return "", ErrUnavailable
	}
	colour, ok := result[1].(string)
	if !ok || (colour != "blue" && colour != "green") {
		return "", fmt.Errorf("%w: invalid selected colour", ErrUnavailable)
	}
	query := parsed.Query()
	query.Set("colour", colour)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
