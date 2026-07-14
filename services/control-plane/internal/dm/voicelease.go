package dm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	dmVoiceCallLeaseKeyPrefix     = "dm_voice_call_lease:"
	dmVoiceCallClosedKeyPrefix    = "dm_voice_call_closed:"
	dmVoiceCallCleanupKeyPrefix   = "dm_voice_call_cleanup:"
	dmVoiceJoinAdmissionKeyPrefix = "dm_voice_join_admission:"
	dmVoiceCallCleanupTTL         = 15 * time.Second
	// DMVoiceCallReservationTTL bounds a direct renderer-join handoff. A successful
	// voice.joined event promotes it to DMVoiceCallLeaseTTL immediately; 45
	// seconds also leaves one 30-second heartbeat interval to recover a dropped
	// join event without retaining a phantom room for the full active-call TTL.
	DMVoiceCallReservationTTL = 45 * time.Second
	// DMVoiceCallLeaseTTL allows three missed 30-second media-room heartbeats to
	// bound stale state without expiring a live call during a brief reconnect.
	DMVoiceCallLeaseTTL = 90 * time.Second
)

var (
	// ErrDMVoiceCallLeaseConflict means another call owns the conversation lease.
	ErrDMVoiceCallLeaseConflict = errors.New("different DM voice call already owns conversation")
	// ErrDMVoiceCallLeaseClosed means the exact call ID is terminal and tombstoned.
	ErrDMVoiceCallLeaseClosed = errors.New("DM voice call is already terminal")
)

// VoiceCallLease is the shared, expiring identity of one active media-room
// lifecycle. Once a lease exists, Redis makes its exact identity and later
// reconnects consistent across control-plane replicas without turning live
// presence into durable database state. Pending rings remain replica-local;
// this lease does not make the pre-accept ring transition horizontally safe.
type VoiceCallLease struct {
	ConversationID  uuid.UUID
	CallID          uuid.UUID
	RingID          uuid.UUID
	CallerUserID    uuid.UUID
	MediaAuthorized bool
}

func dmVoiceCallLeaseKey(conversationID uuid.UUID) string {
	return dmVoiceCallLeaseKeyPrefix + conversationID.String()
}

func dmVoiceCallClosedKey(callID uuid.UUID) string {
	return dmVoiceCallClosedKeyPrefix + callID.String()
}

func dmVoiceCallCleanupKey(conversationID uuid.UUID) string {
	return dmVoiceCallCleanupKeyPrefix + conversationID.String()
}

func dmVoiceJoinAdmissionKey(conversationID, userID uuid.UUID) string {
	return dmVoiceJoinAdmissionKeyPrefix + conversationID.String() + ":" + userID.String()
}

var refreshDMVoiceCallLeaseScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 1 then
  return -1
end
if redis.call('EXISTS', KEYS[3]) == 1 then
  return 0
end

local current = redis.call('HGET', KEYS[1], 'call_id')
if current and current ~= ARGV[1] then
  return 0
end

if not current then
  redis.call('HSET', KEYS[1],
    'call_id', ARGV[1],
    'ring_id', ARGV[2],
    'caller_user_id', ARGV[3],
    'media_authorized', '0',
    'promoted', ARGV[6])
else
  if ARGV[5] == '1' then
    redis.call('HSET', KEYS[1],
      'ring_id', ARGV[2],
      'caller_user_id', ARGV[3])
  end
  if ARGV[6] == '1' then
    redis.call('HSET', KEYS[1], 'promoted', '1')
  end
end

redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 1
`)

var markDMVoiceCallMediaAuthorizedScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'call_id') ~= ARGV[1] then
  return 0
end
redis.call('HSET', KEYS[1], 'media_authorized', '1')
return 1
`)

var beginDMVoiceCallCleanupScript = redis.NewScript(`
redis.call('SET', KEYS[2], '1', 'PX', ARGV[2])
local current = redis.call('HGET', KEYS[1], 'call_id')
if current and current ~= ARGV[1] then
  return 0
end
if redis.call('EXISTS', KEYS[3]) == 1 then
  return 0
end
redis.call('SET', KEYS[3], ARGV[1], 'PX', ARGV[3])
if current == ARGV[1] then
  redis.call('DEL', KEYS[1])
end
return 1
`)

var rememberDMVoiceJoinAdmissionScript = redis.NewScript(`
local current = redis.call('HGET', KEYS[1], 'call_id')
if not current or current ~= ARGV[1] then
  return 0
end
redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[2])
return 1
`)

var endDMVoiceCallCleanupScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

var clearUnpromotedDMVoiceCallReservationScript = redis.NewScript(`
local current = redis.call('HGET', KEYS[1], 'call_id')
if not current or current ~= ARGV[1] then
  return 0
end
local ring_id = redis.call('HGET', KEYS[1], 'ring_id')
local media_authorized = redis.call('HGET', KEYS[1], 'media_authorized')
local promoted = redis.call('HGET', KEYS[1], 'promoted')
local ttl = redis.call('PTTL', KEYS[1])
if ring_id ~= '' or media_authorized == '1' or promoted == '1' or ttl <= 0 or ttl > tonumber(ARGV[2]) then
  return 0
end
redis.call('SET', KEYS[2], '1', 'PX', ARGV[3])
return redis.call('DEL', KEYS[1])
`)

var abortAuthorizedDMVoiceCallReservationScript = redis.NewScript(`
local current = redis.call('HGET', KEYS[1], 'call_id')
if not current or current ~= ARGV[1] then
  return 0
end
local ring_id = redis.call('HGET', KEYS[1], 'ring_id')
local media_authorized = redis.call('HGET', KEYS[1], 'media_authorized')
local promoted = redis.call('HGET', KEYS[1], 'promoted')
local ttl = redis.call('PTTL', KEYS[1])
if ring_id ~= '' or media_authorized ~= '1' or promoted == '1' or ttl <= 0 or ttl > tonumber(ARGV[2]) then
  return 0
end
redis.call('SET', KEYS[2], '1', 'PX', ARGV[3])
return redis.call('DEL', KEYS[1])
`)

// RefreshDMVoiceCallLease claims or renews one exact conversation/call pair.
// A different unexpired call ID fails closed instead of replacing the owner.
func RefreshDMVoiceCallLease(
	ctx context.Context,
	client *redis.Client,
	lease VoiceCallLease,
	ttl time.Duration,
	authoritativeMetadata bool,
) error {
	if client == nil {
		return errors.New("DM voice call lease store unavailable")
	}
	if lease.ConversationID == uuid.Nil || lease.CallID == uuid.Nil ||
		lease.CallerUserID == uuid.Nil || ttl <= 0 {
		return errors.New("invalid DM voice call lease")
	}

	ringID := ""
	if lease.RingID != uuid.Nil {
		ringID = lease.RingID.String()
	}
	result, err := refreshDMVoiceCallLeaseScript.Run(
		ctx,
		client,
		[]string{
			dmVoiceCallLeaseKey(lease.ConversationID),
			dmVoiceCallClosedKey(lease.CallID),
			dmVoiceCallCleanupKey(lease.ConversationID),
		},
		lease.CallID.String(),
		ringID,
		lease.CallerUserID.String(),
		ttl.Milliseconds(),
		boolToRedisFlag(authoritativeMetadata),
		boolToRedisFlag(ttl > DMVoiceCallReservationTTL),
	).Int()
	if err != nil {
		return fmt.Errorf("refresh DM voice call lease: %w", err)
	}
	if result == -1 {
		return ErrDMVoiceCallLeaseClosed
	}
	if result != 1 {
		return ErrDMVoiceCallLeaseConflict
	}
	return nil
}

// MarkDMVoiceCallMediaAuthorized atomically records that the SFU authorization
// boundary accepted this exact lease. It deliberately does not extend the
// reservation TTL; its only purpose is to prevent an overlapping ring from
// tombstoning the authorized handoff before voice.joined promotes the lease.
func MarkDMVoiceCallMediaAuthorized(
	ctx context.Context,
	client *redis.Client,
	conversationID, callID uuid.UUID,
) error {
	if client == nil {
		return errors.New("DM voice call lease store unavailable")
	}
	if conversationID == uuid.Nil || callID == uuid.Nil {
		return errors.New("invalid DM voice call lease identity")
	}
	result, err := markDMVoiceCallMediaAuthorizedScript.Run(
		ctx,
		client,
		[]string{dmVoiceCallLeaseKey(conversationID)},
		callID.String(),
	).Int()
	if err != nil {
		return fmt.Errorf("mark DM voice call media authorized: %w", err)
	}
	if result != 1 {
		return ErrDMVoiceCallLeaseConflict
	}
	return nil
}

// RememberDMVoiceJoinAdmission binds a member's renderer-facing /voice/join
// to one exact current lease for the short media handoff. The compare-and-set
// prevents a delayed join from overwriting a newer admission. Legacy media
// clients that omit call_id may authorize only through this user-scoped key.
func RememberDMVoiceJoinAdmission(
	ctx context.Context,
	client *redis.Client,
	conversationID, userID, callID uuid.UUID,
	ttl time.Duration,
) error {
	if client == nil {
		return errors.New("DM voice join admission store unavailable")
	}
	if conversationID == uuid.Nil || userID == uuid.Nil || callID == uuid.Nil || ttl <= 0 {
		return errors.New("invalid DM voice join admission")
	}
	result, err := rememberDMVoiceJoinAdmissionScript.Run(
		ctx,
		client,
		[]string{
			dmVoiceCallLeaseKey(conversationID),
			dmVoiceJoinAdmissionKey(conversationID, userID),
		},
		callID.String(),
		ttl.Milliseconds(),
	).Int()
	if err != nil {
		return fmt.Errorf("remember DM voice join admission: %w", err)
	}
	if result != 1 {
		return ErrDMVoiceCallLeaseConflict
	}
	return nil
}

// LookupDMVoiceJoinAdmission returns the exact call most recently authorized
// for this conversation/member pair by /voice/join.
func LookupDMVoiceJoinAdmission(
	ctx context.Context,
	client *redis.Client,
	conversationID, userID uuid.UUID,
) (uuid.UUID, bool, error) {
	if client == nil {
		return uuid.Nil, false, errors.New("DM voice join admission store unavailable")
	}
	rawCallID, err := client.Get(ctx, dmVoiceJoinAdmissionKey(conversationID, userID)).Result()
	if errors.Is(err, redis.Nil) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("lookup DM voice join admission: %w", err)
	}
	callID, err := uuid.Parse(rawCallID)
	if err != nil || callID == uuid.Nil {
		return uuid.Nil, false, fmt.Errorf("invalid stored DM voice join admission")
	}
	return callID, true, nil
}

func boolToRedisFlag(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// LookupDMVoiceCallLease returns the current unexpired shared call identity.
func LookupDMVoiceCallLease(
	ctx context.Context,
	client *redis.Client,
	conversationID uuid.UUID,
) (VoiceCallLease, bool, error) {
	if client == nil {
		return VoiceCallLease{}, false, errors.New("DM voice call lease store unavailable")
	}
	values, err := client.HGetAll(ctx, dmVoiceCallLeaseKey(conversationID)).Result()
	if err != nil {
		return VoiceCallLease{}, false, fmt.Errorf("lookup DM voice call lease: %w", err)
	}
	if len(values) == 0 {
		return VoiceCallLease{}, false, nil
	}

	callID, err := uuid.Parse(values["call_id"])
	if err != nil {
		return VoiceCallLease{}, false, fmt.Errorf("invalid stored DM voice call ID: %w", err)
	}
	callerUserID, err := uuid.Parse(values["caller_user_id"])
	if err != nil {
		return VoiceCallLease{}, false, fmt.Errorf("invalid stored DM voice caller ID: %w", err)
	}
	ringID := uuid.Nil
	if rawRingID := values["ring_id"]; rawRingID != "" {
		ringID, err = uuid.Parse(rawRingID)
		if err != nil {
			return VoiceCallLease{}, false, fmt.Errorf("invalid stored DM voice ring ID: %w", err)
		}
	}

	return VoiceCallLease{
		ConversationID:  conversationID,
		CallID:          callID,
		RingID:          ringID,
		CallerUserID:    callerUserID,
		MediaAuthorized: values["media_authorized"] == "1",
	}, true, nil
}

// BeginDMVoiceCallCleanup atomically tombstones the terminal call, deletes only
// its exact lease, and installs a short conversation guard. While the guard is
// held, a replacement call cannot claim the Redis lease before conversation-
// wide presence cleanup completes on another control-plane replica.
func BeginDMVoiceCallCleanup(
	ctx context.Context,
	client *redis.Client,
	conversationID, callID uuid.UUID,
) (bool, error) {
	if client == nil {
		return false, errors.New("DM voice call lease store unavailable")
	}
	if conversationID == uuid.Nil || callID == uuid.Nil {
		return false, errors.New("invalid DM voice call lease identity")
	}
	result, err := beginDMVoiceCallCleanupScript.Run(
		ctx,
		client,
		[]string{
			dmVoiceCallLeaseKey(conversationID),
			dmVoiceCallClosedKey(callID),
			dmVoiceCallCleanupKey(conversationID),
		},
		callID.String(),
		DMVoiceCallLeaseTTL.Milliseconds(),
		dmVoiceCallCleanupTTL.Milliseconds(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("begin DM voice call cleanup: %w", err)
	}
	return result == 1, nil
}

// EndDMVoiceCallCleanup releases only the guard owned by this exact terminal
// call. The TTL remains a crash-safe fallback if cleanup exits unexpectedly.
func EndDMVoiceCallCleanup(
	ctx context.Context,
	client *redis.Client,
	conversationID, callID uuid.UUID,
) error {
	if client == nil {
		return errors.New("DM voice call lease store unavailable")
	}
	if conversationID == uuid.Nil || callID == uuid.Nil {
		return errors.New("invalid DM voice call lease identity")
	}
	if _, err := endDMVoiceCallCleanupScript.Run(
		ctx,
		client,
		[]string{dmVoiceCallCleanupKey(conversationID)},
		callID.String(),
	).Result(); err != nil {
		return fmt.Errorf("end DM voice call cleanup: %w", err)
	}
	return nil
}

// DeleteDMVoiceCallLease is the non-transactional compatibility helper used by
// callers that do not perform conversation-wide state cleanup. NATS terminal
// handlers must hold BeginDMVoiceCallCleanup through their DB mutation instead.
func DeleteDMVoiceCallLease(
	ctx context.Context,
	client *redis.Client,
	conversationID, callID uuid.UUID,
) error {
	acquired, err := BeginDMVoiceCallCleanup(ctx, client, conversationID, callID)
	if err != nil || !acquired {
		return err
	}
	return EndDMVoiceCallCleanup(ctx, client, conversationID, callID)
}

// ClearUnpromotedDMVoiceCallReservation removes and tombstones only the exact
// short direct-join reservation observed by its caller: it must have no ring
// correlation, no successful SFU authorization, and still be within the
// reservation TTL. A concurrent replacement or a lease authorized/promoted by
// the media lifecycle is preserved.
func ClearUnpromotedDMVoiceCallReservation(
	ctx context.Context,
	client *redis.Client,
	conversationID, expectedCallID uuid.UUID,
) (bool, error) {
	if client == nil {
		return false, errors.New("DM voice call lease store unavailable")
	}
	if conversationID == uuid.Nil || expectedCallID == uuid.Nil {
		return false, errors.New("invalid DM voice call reservation identity")
	}
	result, err := clearUnpromotedDMVoiceCallReservationScript.Run(
		ctx,
		client,
		[]string{
			dmVoiceCallLeaseKey(conversationID),
			dmVoiceCallClosedKey(expectedCallID),
		},
		expectedCallID.String(),
		DMVoiceCallReservationTTL.Milliseconds(),
		DMVoiceCallLeaseTTL.Milliseconds(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("clear unpromoted DM voice call reservation: %w", err)
	}
	return result == 1, nil
}

// AbortAuthorizedDMVoiceCallReservation tombstones only an exact, short,
// ringless reservation whose media handoff was authorized but never admitted.
// Promoted calls, accepted rings, and successor call IDs are preserved.
func AbortAuthorizedDMVoiceCallReservation(
	ctx context.Context,
	client *redis.Client,
	conversationID, expectedCallID uuid.UUID,
) (bool, error) {
	if client == nil {
		return false, errors.New("DM voice call lease store unavailable")
	}
	if conversationID == uuid.Nil || expectedCallID == uuid.Nil {
		return false, errors.New("invalid DM voice call reservation identity")
	}
	result, err := abortAuthorizedDMVoiceCallReservationScript.Run(
		ctx,
		client,
		[]string{
			dmVoiceCallLeaseKey(conversationID),
			dmVoiceCallClosedKey(expectedCallID),
		},
		expectedCallID.String(),
		DMVoiceCallReservationTTL.Milliseconds(),
		DMVoiceCallLeaseTTL.Milliseconds(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("abort authorized DM voice call reservation: %w", err)
	}
	return result == 1, nil
}
