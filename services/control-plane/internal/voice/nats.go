package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/ingressbudget"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// NATSSubscriber listens for voice events from the media plane and
// updates the database + broadcasts WebSocket messages to clients.
type NATSSubscriber struct {
	db                  *sql.DB
	log                 *logger.Logger
	hub                 *websocket.Hub
	nats                *natsclient.Client
	redis               *redis.Client
	tempGrant           *tempGrantManager
	activity            *presence.ActivityService
	lifecycleDispatchMu sync.Mutex
	lifecycleDispatcher *voiceLifecycleDispatcher
	// voiceLifecycleClaimedHook is a deterministic test seam invoked while the
	// distributed lifecycle critical section is held, after Redis accepts the
	// event and before its PostgreSQL mutation begins.
	voiceLifecycleClaimedHook func(presence.Category, uuid.UUID, time.Time)
	// serverVoiceScopeObservedHook pauses focused cross-replica tests after the
	// optimistic pre-lock scope and audience reads, before the guarded mutation.
	serverVoiceScopeObservedHook func(uuid.UUID, uuid.UUID, time.Time)
	// privateJoinBeforeMutationHook and privateJoinBroadcastHook are deterministic
	// test seams around the private joined transaction and its base-state frame.
	privateJoinBeforeMutationHook  func(uuid.UUID, uuid.UUID)
	privateJoinBroadcastHook       func(uuid.UUID, uuid.UUID)
	privateLeaveAfterCommitHook    func()
	dmHeartbeatPostCommitHook      func()
	privateVoiceStateBroadcastHook func(uuid.UUID, uuid.UUID, string)
	dmRoomEmptyVerificationHook    func() error
	// Deterministic test seam for conservative reconnect assertions.
	disconnectAllRichPresenceClientsHook func()

	// clearErasedSenderHook observes the erasure-clear sink in tests. It exists
	// for the same reason the hook above does: the sink is a Hub call, and
	// without a seam a test can only assert "nothing panicked", which holds
	// even when the handler never reaches the sink at all. That is precisely
	// how the erasure-clear tests went vacuous when this handler stopped
	// disconnecting and started clearing (CodeRabbit, PR #2840).
	clearErasedSenderHook func(uuid.UUID)
	// Ingress admission control for the two NATS doors this subscriber
	// registers (#2854 stage B1). Constants and gate logic live in
	// ingress_gate.go.
	//
	// All THREE are IN-PROCESS and replica-scoped. For erasureSeen that is a
	// CORRECTNESS property rather than an economy: core NATS fans every publish
	// to every replica and each replica holds a different set of connected
	// viewers, so a fleet-shared dedup key would let one replica's consumption
	// suppress the clear on another replica where it was never delivered. Do
	// NOT "improve" this to Redis.
	//
	// A nil gate is a no-op, matching permEnforcer below. That keeps
	// struct-literal test subscribers working and means a wiring bug cannot
	// DENY traffic on a right-to-erasure path -- but it also makes such a bug
	// silent, so TestNewNATSSubscriberWiresTheIngressGates locks the production
	// construction path.
	erasureBudget    *ingressbudget.Bucket
	erasureSeen      *ingressbudget.Window
	voiceRoomBudget  *ingressbudget.KeyedBuckets
	erasureShedState ingressShedState
	voiceShedState   ingressShedState
	// voiceDropShedState aggregates voice-lifecycle events DROPPED at the
	// dispatcher, separately from voiceShedState so each keeps an accurate
	// message: the gate sheds at ingress, the dispatcher drops under saturation.
	//
	// A FIELD, not a package global, for the reason stated above newErasureBudget:
	// package-level state stayed mutated after the test that set it and handed
	// every later test in the package the leftover (CodeRabbit, PR #2871). This
	// one dies with the subscriber, like its two siblings.
	voiceDropShedState ingressShedState
	// erasureExistenceProbedHook counts existence queries, which is how the
	// dedup-ahead-of-the-database ordering is asserted rather than assumed.
	erasureExistenceProbedHook func()
	// ingressShedObservedHook fires once per shed and ingressShedLoggedHook once
	// per emitted log line. Two seams, because the property under test is that
	// the first count is unbounded while the second is not.
	ingressShedObservedHook func(string)
	ingressShedLoggedHook   func()

	// permEnforcer re-pushes fresh permissions when a join lands (CV-CAN-007
	// review P1 join-race): a join-authorize resolved before a mutation whose
	// recheck sweep ran before this voice_participants row existed would
	// otherwise hold a stale snapshot no push covers. Optional (nil = no-op).
	permEnforcer *PermissionEnforcer
}

var (
	errInvalidDMVoiceCallLifecycle = errors.New("invalid DM voice call lifecycle identity")
	errAmbiguousServerVoiceScope   = errors.New("ambiguous current server voice scope")
)

type voiceLifecycleClaimStatus int

const (
	voiceLifecycleRejected voiceLifecycleClaimStatus = iota
	voiceLifecycleFresh
	voiceLifecycleDuplicate
)

const (
	dmRoomEmptyCleanupTimeout = 10 * time.Second
	// ponytail: bounded retries cover transient DB errors; persistent failures
	// fall back to the existing 90-second presence expiry.
	dmRoomEmptyCleanupAttempts   = 3
	richPresenceLifecycleTimeout = 10 * time.Second

	// msgErasureClearRejected is the single rejection message for the erasure
	// clear. Named because every rejection arm shares it and a stray literal in a
	// fifth would be invisible to a grep for the constant.
	msgErasureClearRejected = "Presence erasure clear rejected"
	// msgErasureProbeUnconfirmed is the fail-open arm of the existence probe.
	// Its failure_class discriminates the cause; the message is fixed.
	msgErasureProbeUnconfirmed  = "Presence erasure clear proceeding without confirmation"
	maxVoiceMutationReplayBytes = 16 * 1024
	maxVoiceReplayRemovedRooms  = 255
	// Private Calls are tightly bounded because their lifecycle generation is
	// claimed and delivered as one participant set. Server rooms have a larger,
	// explicit media-admission bound; heartbeat DB and media sets are each capped
	// independently, so stale+replacement reconciliation remains possible at
	// full capacity while total work is still bounded to twice the room limit.
	maxPrivateVoiceParticipantIDs        = 255
	maxServerVoiceParticipantIDs         = 1000
	serverHeartbeatParticipantWorkers    = 5
	logPrivateBridgeUnavailable          = "Private Call Rich Presence bridge unavailable"
	logPrivateLifecycleDependencyFailure = "Private Call lifecycle dependency failure"
	logPrivatePreMutationReadFailure     = "Private Call pre-mutation participant read failed"
	logPrivateParticipantRefreshFailure  = "Private Call Rich Presence participant refresh failed"
	logServerBridgeUnavailable           = "Server voice Rich Presence bridge unavailable"
	logVoiceLifecycleApplied             = "Voice lifecycle event applied"
)

var claimVoiceLifecycleScript = redis.NewScript(`
local key_type = redis.call('TYPE', KEYS[1]).ok
if key_type ~= 'none' and key_type ~= 'hash' then
  redis.call('DEL', KEYS[1])
  return -1
end

local current_token = redis.call('HGET', KEYS[1], 'token')
local current_version_raw = redis.call('HGET', KEYS[1], 'version')
local current_version = tonumber(current_version_raw)
local current_active = redis.call('HGET', KEYS[1], 'active')
local incoming_version = tonumber(ARGV[2])
local incoming_active = ARGV[3]

if key_type == 'hash' and redis.call('HLEN', KEYS[1]) > 0 then
  local compact_token = current_token and string.gsub(current_token, '-', '') or ''
  local current_ttl = redis.call('PTTL', KEYS[1])
  if redis.call('HLEN', KEYS[1]) ~= 3 or
      not current_token or string.len(current_token) ~= 36 or
      string.len(compact_token) ~= 32 or string.lower(current_token) ~= current_token or
      compact_token == '00000000000000000000000000000000' or
      string.sub(current_token, 9, 9) ~= '-' or string.sub(current_token, 14, 14) ~= '-' or
      string.sub(current_token, 19, 19) ~= '-' or string.sub(current_token, 24, 24) ~= '-' or
      string.find(compact_token, '[^0-9a-f]') or
      not current_version_raw or string.sub(current_version_raw, 1, 1) == '0' or
      string.find(current_version_raw, '[^0-9]') or
      not current_version or current_version <= 0 or current_version > 9007199254740991 or
      current_version ~= math.floor(current_version) or
      (current_active ~= '0' and current_active ~= '1') or
      current_ttl <= 0 or current_ttl > tonumber(ARGV[4]) then
    redis.call('DEL', KEYS[1])
    return -1
  end
  if incoming_active == '0' and current_token ~= ARGV[1] then
    return 0
  end
  if incoming_version < current_version then
    return 0
  end
  if incoming_version == current_version then
    if current_token ~= ARGV[1] then
      return 0
    end
    if current_active == '0' and incoming_active == '1' then
      return 0
    end
    if current_active == incoming_active then
      redis.call('PEXPIRE', KEYS[1], ARGV[4])
      return 2
    end
  end
end

redis.call('HSET', KEYS[1],
  'token', ARGV[1],
  'version', ARGV[2],
  'active', incoming_active)
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 1
`)

// claimPrivateVoiceParticipantSetScript advances one complete Private Call set
// revision atomically. Every key is preflighted before any valid envelope is
// created or renewed, so one stale/conflicting participant rejects the whole
// set and one poisoned participant is delete-healed without partially
// advancing its peers.
var claimPrivateVoiceParticipantSetScript = redis.NewScript(`
local max_ttl = tonumber(ARGV[1])
local malformed = {}
local rejected = false
local any_fresh = false

for index, key in ipairs(KEYS) do
  local offset = 2 + ((index - 1) * 3)
  local incoming_token = ARGV[offset]
  local incoming_version = tonumber(ARGV[offset + 1])
  local incoming_active = ARGV[offset + 2]
  local key_type = redis.call('TYPE', key).ok
  if key_type ~= 'none' and key_type ~= 'hash' then
    table.insert(malformed, key)
  elseif key_type == 'hash' and redis.call('HLEN', key) > 0 then
    local current_token = redis.call('HGET', key, 'token')
    local current_version_raw = redis.call('HGET', key, 'version')
    local current_version = tonumber(current_version_raw)
    local current_active = redis.call('HGET', key, 'active')
    local compact_token = current_token and string.gsub(current_token, '-', '') or ''
    local current_ttl = redis.call('PTTL', key)
    if redis.call('HLEN', key) ~= 3 or
        not current_token or string.len(current_token) ~= 36 or
        string.len(compact_token) ~= 32 or string.lower(current_token) ~= current_token or
        compact_token == '00000000000000000000000000000000' or
        string.sub(current_token, 9, 9) ~= '-' or string.sub(current_token, 14, 14) ~= '-' or
        string.sub(current_token, 19, 19) ~= '-' or string.sub(current_token, 24, 24) ~= '-' or
        string.find(compact_token, '[^0-9a-f]') or
        not current_version_raw or string.sub(current_version_raw, 1, 1) == '0' or
        string.find(current_version_raw, '[^0-9]') or
        not current_version or current_version <= 0 or current_version > 9007199254740991 or
        current_version ~= math.floor(current_version) or
        (current_active ~= '0' and current_active ~= '1') or
        current_ttl <= 0 or current_ttl > max_ttl then
      table.insert(malformed, key)
    elseif incoming_active == '0' and current_token ~= incoming_token then
      rejected = true
    elseif incoming_version < current_version then
      rejected = true
    elseif incoming_version == current_version then
      if current_token ~= incoming_token or
          (current_active == '0' and incoming_active == '1') then
        rejected = true
      elseif current_active ~= incoming_active then
        any_fresh = true
      end
    else
      any_fresh = true
    end
  else
    any_fresh = true
  end
end

if #malformed > 0 then
  for _, key in ipairs(malformed) do
    redis.call('DEL', key)
  end
  return -1
end
if rejected then
  return 0
end

for index, key in ipairs(KEYS) do
  local offset = 2 + ((index - 1) * 3)
  redis.call('HSET', key,
    'token', ARGV[offset],
    'version', ARGV[offset + 1],
    'active', ARGV[offset + 2])
  redis.call('PEXPIRE', key, ARGV[1])
end
if any_fresh then
  return 1
end
return 2
`)

var matchesActiveVoiceLifecycleTokenScript = redis.NewScript(`
local key_type = redis.call('TYPE', KEYS[1]).ok
if key_type == 'none' then
  return 0
end
if key_type ~= 'hash' then
  redis.call('DEL', KEYS[1])
  return -1
end

local token = redis.call('HGET', KEYS[1], 'token')
local version_raw = redis.call('HGET', KEYS[1], 'version')
local version = tonumber(version_raw)
local active = redis.call('HGET', KEYS[1], 'active')
local compact_token = token and string.gsub(token, '-', '') or ''
local current_ttl = redis.call('PTTL', KEYS[1])
if redis.call('HLEN', KEYS[1]) ~= 3 or
    not token or string.len(token) ~= 36 or string.len(compact_token) ~= 32 or
    string.lower(token) ~= token or compact_token == '00000000000000000000000000000000' or
    string.sub(token, 9, 9) ~= '-' or string.sub(token, 14, 14) ~= '-' or
    string.sub(token, 19, 19) ~= '-' or string.sub(token, 24, 24) ~= '-' or
    string.find(compact_token, '[^0-9a-f]') or
    not version_raw or string.sub(version_raw, 1, 1) == '0' or
    string.find(version_raw, '[^0-9]') or not version or version <= 0 or
    version > 9007199254740991 or version ~= math.floor(version) or
    (active ~= '0' and active ~= '1') or current_ttl <= 0 or
    current_ttl > tonumber(ARGV[2]) then
  redis.call('DEL', KEYS[1])
  return -1
end
if token == ARGV[1] then
	if active == '1' then
		return 1
	end
	return 2
end
return 3
`)

type voiceLifecycleTokenState int

const (
	voiceLifecycleTokenMissing voiceLifecycleTokenState = iota
	voiceLifecycleTokenExactActive
	voiceLifecycleTokenExactTerminal
	voiceLifecycleTokenDifferent
)

var loadServerVoiceMutationReplayScript = redis.NewScript(`
local key_type = redis.call('TYPE', KEYS[1]).ok
if key_type == 'none' then
  return nil
end
if key_type ~= 'string' then
  redis.call('DEL', KEYS[1])
  return redis.error_reply('MALFORMED_VOICE_MUTATION_REPLAY')
end
local ttl = redis.call('PTTL', KEYS[1])
local raw = redis.call('GET', KEYS[1])
if ttl <= 0 or ttl > tonumber(ARGV[1]) or not raw or
    string.len(raw) == 0 or string.len(raw) > tonumber(ARGV[2]) then
  redis.call('DEL', KEYS[1])
  return redis.error_reply('MALFORMED_VOICE_MUTATION_REPLAY')
end
return raw
`)

func (s *NATSSubscriber) claimVoiceLifecycle(
	ctx context.Context,
	category presence.Category,
	senderID, token uuid.UUID,
	eventAt time.Time,
	active bool,
) (bool, error) {
	status, err := s.claimVoiceLifecycleStatus(
		ctx, category, senderID, token, eventAt, active,
	)
	return status != voiceLifecycleRejected, err
}

func (s *NATSSubscriber) claimVoiceLifecycleStatus(
	ctx context.Context,
	category presence.Category,
	senderID, token uuid.UUID,
	eventAt time.Time,
	active bool,
) (voiceLifecycleClaimStatus, error) {
	if s == nil || s.redis == nil || senderID == uuid.Nil || token == uuid.Nil ||
		!presence.IsValidActivitySourceTime(eventAt) {
		return voiceLifecycleRejected, errors.New("invalid voice lifecycle watermark claim")
	}
	key, err := presence.VoiceLifecycleKey(senderID, category)
	if err != nil {
		return voiceLifecycleRejected, err
	}
	activeFlag := "0"
	if active {
		activeFlag = "1"
	}
	result, err := claimVoiceLifecycleScript.Run(
		ctx,
		s.redis,
		[]string{key},
		token.String(),
		eventAt.UnixMicro(),
		activeFlag,
		presence.ActivityStateTTL.Milliseconds(),
	).Int()
	if err != nil {
		return voiceLifecycleRejected, fmt.Errorf("claim voice lifecycle watermark: %w", err)
	}
	if result == -1 {
		return voiceLifecycleRejected, errors.New("malformed voice lifecycle watermark")
	}
	switch result {
	case 0:
		return voiceLifecycleRejected, nil
	case 1:
		return voiceLifecycleFresh, nil
	case 2:
		return voiceLifecycleDuplicate, nil
	default:
		return voiceLifecycleRejected, errors.New("invalid voice lifecycle watermark result")
	}
}

type privateVoiceParticipantSetClaim struct {
	userID  uuid.UUID
	token   uuid.UUID
	version int64
	active  bool
}

func (s *NATSSubscriber) claimPrivateVoiceParticipantSet(
	ctx context.Context,
	callID uuid.UUID,
	eventAt time.Time,
	claims []privateVoiceParticipantSetClaim,
) (voiceLifecycleClaimStatus, error) {
	if callID == uuid.Nil || !presence.IsValidActivitySourceTime(eventAt) {
		return voiceLifecycleRejected, errors.New("invalid private voice participant-set claim")
	}
	version := eventAt.UnixMicro()
	resolved := append([]privateVoiceParticipantSetClaim(nil), claims...)
	for index := range resolved {
		resolved[index].token = callID
		resolved[index].version = version
	}
	return s.claimPrivateVoiceLifecycles(ctx, resolved)
}

func (s *NATSSubscriber) claimPrivateVoiceLifecycles(
	ctx context.Context,
	claims []privateVoiceParticipantSetClaim,
) (voiceLifecycleClaimStatus, error) {
	if err := ctx.Err(); err != nil {
		return voiceLifecycleRejected, err
	}
	if s == nil || s.redis == nil || len(claims) == 0 ||
		len(claims) > maxPrivateVoiceParticipantIDs {
		return voiceLifecycleRejected, errors.New("invalid private voice participant-set claim")
	}

	ordered := append([]privateVoiceParticipantSetClaim(nil), claims...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].userID.String() < ordered[right].userID.String()
	})
	keys := make([]string, 0, len(ordered))
	args := make([]interface{}, 0, 1+(3*len(ordered)))
	args = append(args, presence.ActivityStateTTL.Milliseconds())
	for index, claim := range ordered {
		if claim.userID == uuid.Nil || claim.token == uuid.Nil ||
			claim.version <= 0 || claim.version > presence.MaxActivitySourceVersion ||
			(index > 0 && claim.userID == ordered[index-1].userID) {
			return voiceLifecycleRejected, errors.New("invalid private voice participant-set sender")
		}
		key, err := presence.VoiceLifecycleKey(
			claim.userID, presence.CategoryPrivateCall,
		)
		if err != nil {
			return voiceLifecycleRejected, err
		}
		keys = append(keys, key)
		activeFlag := "0"
		if claim.active {
			activeFlag = "1"
		}
		args = append(args, claim.token.String(), claim.version, activeFlag)
	}

	result, err := claimPrivateVoiceParticipantSetScript.Run(
		ctx, s.redis, keys, args...,
	).Int()
	if err != nil {
		return voiceLifecycleRejected, fmt.Errorf(
			"claim private voice participant set: %w", err,
		)
	}
	switch result {
	case -1:
		return voiceLifecycleRejected, errors.New("malformed voice lifecycle watermark")
	case 0:
		return voiceLifecycleRejected, nil
	case 1:
		return voiceLifecycleFresh, nil
	case 2:
		return voiceLifecycleDuplicate, nil
	default:
		return voiceLifecycleRejected, errors.New(
			"invalid private voice participant-set watermark result",
		)
	}
}

func (s *NATSSubscriber) matchesActiveVoiceLifecycleToken(
	ctx context.Context,
	category presence.Category,
	senderID, token uuid.UUID,
) (bool, error) {
	state, err := s.voiceLifecycleTokenState(ctx, category, senderID, token)
	return state == voiceLifecycleTokenExactActive, err
}

func (s *NATSSubscriber) voiceLifecycleTokenState(
	ctx context.Context,
	category presence.Category,
	senderID, token uuid.UUID,
) (voiceLifecycleTokenState, error) {
	if s == nil || s.redis == nil || senderID == uuid.Nil || token == uuid.Nil {
		return voiceLifecycleTokenMissing, errors.New("invalid voice lifecycle token match")
	}
	key, err := presence.VoiceLifecycleKey(senderID, category)
	if err != nil {
		return voiceLifecycleTokenMissing, err
	}
	result, err := matchesActiveVoiceLifecycleTokenScript.Run(
		ctx,
		s.redis,
		[]string{key},
		token.String(),
		presence.ActivityStateTTL.Milliseconds(),
	).Int()
	if err != nil {
		return voiceLifecycleTokenMissing, fmt.Errorf("match voice lifecycle token: %w", err)
	}
	if result == -1 {
		return voiceLifecycleTokenMissing, errors.New("malformed voice lifecycle watermark")
	}
	state := voiceLifecycleTokenState(result)
	if state < voiceLifecycleTokenMissing || state > voiceLifecycleTokenDifferent {
		return voiceLifecycleTokenMissing, errors.New("invalid voice lifecycle token match result")
	}
	return state, nil
}

func voiceLifecycleAdvisoryKey(
	category presence.Category,
	senderID uuid.UUID,
) (int64, error) {
	if senderID == uuid.Nil {
		return 0, errors.New("invalid voice lifecycle lock sender")
	}
	switch category {
	case presence.CategoryServerVoice, presence.CategoryPrivateCall:
	default:
		return 0, errors.New("unsupported voice lifecycle lock category")
	}
	digest := sha256.Sum256([]byte(string(category) + "\x00" + senderID.String()))
	// PostgreSQL advisory locks accept signed int64 keys; preserving all 64 hash
	// bits intentionally maps the upper half of uint64 into negative keys.
	return int64(binary.BigEndian.Uint64(digest[:8])), nil //nolint:gosec
}

func lockPrivateVoiceScopes(
	ctx context.Context,
	tx *sql.Tx,
	userIDs []uuid.UUID,
) error {
	if len(userIDs) > maxPrivateVoiceParticipantIDs {
		return errors.New("invalid private voice scope locks")
	}
	return dm.LockPrivateVoiceScopesTx(ctx, tx, userIDs)
}

func lockPrivateVoiceParticipantSets(
	ctx context.Context,
	tx *sql.Tx,
	conversationIDs []uuid.UUID,
) ([]uuid.UUID, error) {
	if tx == nil || len(conversationIDs) == 0 {
		return nil, errors.New("invalid private voice participant-set locks")
	}
	ordered := append([]uuid.UUID(nil), conversationIDs...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].String() < ordered[right].String()
	})
	unique := ordered[:0]
	for _, conversationID := range ordered {
		if conversationID == uuid.Nil {
			return nil, errors.New("invalid private voice participant-set lock")
		}
		if len(unique) > 0 && unique[len(unique)-1] == conversationID {
			continue
		}
		unique = append(unique, conversationID)
	}
	if len(unique) > maxPrivateVoiceParticipantIDs {
		return nil, errors.New("private voice participant-set lock limit exceeded")
	}
	for _, conversationID := range unique {
		if err := lockPrivateVoiceParticipantSet(ctx, tx, conversationID); err != nil {
			return nil, err
		}
	}
	return unique, nil
}

func lockPrivateVoiceParticipantSet(
	ctx context.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
) error {
	return dm.LockDMVoiceParticipantSetTx(ctx, tx, conversationID)
}

func lockPrivateVoiceParticipantLifecycles(
	ctx context.Context,
	tx *sql.Tx,
	participantIDs []uuid.UUID,
) ([]uuid.UUID, error) {
	if tx == nil {
		return nil, errors.New("private voice participant transaction unavailable")
	}
	ordered := append([]uuid.UUID(nil), participantIDs...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].String() < ordered[right].String()
	})
	unique := ordered[:0]
	for _, participantID := range ordered {
		if participantID == uuid.Nil {
			return nil, errors.New("invalid private voice participant lock")
		}
		if len(unique) > 0 && unique[len(unique)-1] == participantID {
			continue
		}
		unique = append(unique, participantID)
	}
	if len(unique) > maxPrivateVoiceParticipantIDs {
		return nil, errors.New("private voice participant limit exceeded")
	}
	for _, participantID := range unique {
		lockKey, err := voiceLifecycleAdvisoryKey(
			presence.CategoryPrivateCall, participantID,
		)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(
			ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey,
		); err != nil {
			return nil, fmt.Errorf("lock private voice participant lifecycle: %w", err)
		}
	}
	return unique, nil
}

func advancePrivateVoiceParticipantRows(
	ctx context.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
	participantIDs []uuid.UUID,
	eventAt time.Time,
) error {
	if tx == nil || conversationID == uuid.Nil ||
		!presence.IsValidActivitySourceTime(eventAt) ||
		len(participantIDs) > maxPrivateVoiceParticipantIDs {
		return errors.New("invalid private voice participant-set row advance")
	}
	if len(participantIDs) == 0 {
		return nil
	}
	rawParticipantIDs := make([]string, 0, len(participantIDs))
	for _, participantID := range participantIDs {
		if participantID == uuid.Nil {
			return errors.New("invalid private voice participant-set row")
		}
		rawParticipantIDs = append(rawParticipantIDs, participantID.String())
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE dm_voice_participants
		SET lifecycle_event_at = $3
		WHERE conversation_id = $1
		  AND user_id = ANY($2::uuid[])
		  AND lifecycle_event_at <= $3
	`, conversationID, pq.Array(rawParticipantIDs), eventAt)
	if err != nil {
		return fmt.Errorf("advance private voice participant-set rows: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read private voice participant-set advance result: %w", err)
	}
	if rowsAffected != int64(len(participantIDs)) {
		return fmt.Errorf(
			"private voice participant-set advance affected %d of %d rows",
			rowsAffected, len(participantIDs),
		)
	}
	return nil
}

func (s *NATSSubscriber) withVoiceLifecycleClaim(
	ctx context.Context,
	category presence.Category,
	senderID, token uuid.UUID,
	eventAt time.Time,
	active bool,
	mutation func(context.Context, *sql.Tx) (bool, error),
) (applied bool, returnErr error) {
	return s.withVoiceLifecycleClaimInParticipantSet(
		ctx,
		voiceLifecycleClaimRequest{
			category: category, senderID: senderID, token: token,
			eventAt: eventAt, active: active,
		},
		mutation,
	)
}

type voiceLifecycleClaimRequest struct {
	category       presence.Category
	senderID       uuid.UUID
	token          uuid.UUID
	eventAt        time.Time
	active         bool
	conversationID uuid.UUID
}

func (s *NATSSubscriber) withVoiceLifecycleClaimInParticipantSet(
	ctx context.Context,
	request voiceLifecycleClaimRequest,
	mutation func(context.Context, *sql.Tx) (bool, error),
) (applied bool, returnErr error) {
	applied, status, err := s.withVoiceLifecycleClaimStatusInParticipantSet(
		ctx,
		request,
		func(ctx context.Context, tx *sql.Tx, _ voiceLifecycleClaimStatus) (bool, error) {
			return mutation(ctx, tx)
		},
	)
	// An exact terminal duplicate is still authoritative for this replica even
	// when another replica already deleted the shared row. Let the ActivityService
	// clear local Rich Presence state and emit the local base terminal frame.
	if err == nil && !request.active && status == voiceLifecycleDuplicate {
		applied = true
	}
	return applied, err
}

func (s *NATSSubscriber) withVoiceLifecycleClaimStatus(
	ctx context.Context,
	category presence.Category,
	senderID, token uuid.UUID,
	eventAt time.Time,
	active bool,
	mutation func(context.Context, *sql.Tx, voiceLifecycleClaimStatus) (bool, error),
) (applied bool, status voiceLifecycleClaimStatus, returnErr error) {
	return s.withVoiceLifecycleClaimStatusInParticipantSet(
		ctx,
		voiceLifecycleClaimRequest{
			category: category, senderID: senderID, token: token,
			eventAt: eventAt, active: active,
		},
		mutation,
	)
}

func (s *NATSSubscriber) withVoiceLifecycleClaimStatusInParticipantSet(
	ctx context.Context,
	request voiceLifecycleClaimRequest,
	mutation func(context.Context, *sql.Tx, voiceLifecycleClaimStatus) (bool, error),
) (applied bool, status voiceLifecycleClaimStatus, returnErr error) {
	if err := ctx.Err(); err != nil {
		return false, voiceLifecycleRejected, err
	}
	if s == nil || s.db == nil || s.redis == nil || request.senderID == uuid.Nil ||
		request.token == uuid.Nil || mutation == nil ||
		!presence.IsValidActivitySourceTime(request.eventAt) {
		return false, voiceLifecycleRejected, errors.New("invalid voice lifecycle mutation")
	}
	if request.conversationID != uuid.Nil {
		if request.category != presence.CategoryPrivateCall {
			return false, voiceLifecycleRejected, errors.New(
				"participant-set lock requires private call",
			)
		}
		return s.withPrivateVoiceParticipantSetClaim(
			ctx, request, mutation,
		)
	}
	if request.category != presence.CategoryServerVoice {
		return false, voiceLifecycleRejected, errors.New("invalid server voice lifecycle mutation")
	}
	return s.withServerVoiceLifecycleClaim(ctx, request, mutation)
}

// joinRollbackErr folds a non-benign rollback error into returnErr and returns
// the result. A nil rollbackErr or a benign sql.ErrTxDone (the tx was already
// committed or rolled back) is ignored. Shared by the transactional
// voice-lifecycle mutators so they declare one rollback error-handling contract
// in a single place. Each caller keeps the tx.Rollback() call itself inline in
// its deferred closure so the rollback stays statically visible.
func joinRollbackErr(returnErr, rollbackErr error, msg string) error {
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return returnErr
	}
	return errors.Join(returnErr, fmt.Errorf("%s: %w", msg, rollbackErr))
}

func (s *NATSSubscriber) withServerVoiceLifecycleClaim(
	ctx context.Context,
	request voiceLifecycleClaimRequest,
	mutation func(context.Context, *sql.Tx, voiceLifecycleClaimStatus) (bool, error),
) (applied bool, status voiceLifecycleClaimStatus, returnErr error) {
	lockKey, err := voiceLifecycleAdvisoryKey(request.category, request.senderID)
	if err != nil {
		return false, voiceLifecycleRejected, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, voiceLifecycleRejected, fmt.Errorf("begin voice lifecycle mutation: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		returnErr = joinRollbackErr(returnErr, rollbackErr, "rollback voice lifecycle mutation")
	}()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return false, voiceLifecycleRejected, fmt.Errorf("lock voice lifecycle mutation: %w", err)
	}
	qualified, err := s.serverVoiceLifecycleClaimQualified(ctx, tx, request)
	if err != nil || !qualified {
		return false, voiceLifecycleRejected, err
	}
	status, err = s.claimVoiceLifecycleStatus(
		ctx,
		request.category,
		request.senderID,
		request.token,
		request.eventAt,
		request.active,
	)
	if err != nil || status == voiceLifecycleRejected {
		return false, status, err
	}
	if s.voiceLifecycleClaimedHook != nil {
		s.voiceLifecycleClaimedHook(request.category, request.senderID, request.eventAt)
	}
	applied, err = mutation(ctx, tx, status)
	if err != nil {
		return false, status, err
	}
	if err := tx.Commit(); err != nil {
		return false, status, fmt.Errorf("commit voice lifecycle mutation: %w", err)
	}
	return applied, status, nil
}

func (s *NATSSubscriber) serverVoiceLifecycleClaimQualified(
	ctx context.Context,
	tx *sql.Tx,
	request voiceLifecycleClaimRequest,
) (bool, error) {
	tokenState, err := s.voiceLifecycleTokenState(
		ctx, request.category, request.senderID, request.token,
	)
	if err != nil || tokenState != voiceLifecycleTokenMissing {
		return err == nil, err
	}
	return qualifyMissingServerVoiceLifecycle(ctx, tx, request)
}

func qualifyMissingServerVoiceLifecycle(
	ctx context.Context,
	tx *sql.Tx,
	request voiceLifecycleClaimRequest,
) (bool, error) {
	var qualified bool
	if request.active {
		err := tx.QueryRowContext(ctx, `
			SELECT NOT EXISTS (
				SELECT 1
				FROM voice_participants
				WHERE user_id = $1
				  AND (
					  lifecycle_event_at > $3
					  OR (channel_id <> $2 AND lifecycle_event_at >= $3)
				  )
			)
		`, request.senderID, request.token, request.eventAt).Scan(&qualified)
		if err != nil {
			return false, fmt.Errorf("qualify durable server voice join: %w", err)
		}
		return qualified, nil
	}
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
				SELECT 1
				FROM voice_participants
				WHERE user_id = $1 AND channel_id = $2
				  AND lifecycle_event_at <= $3
			)
			AND NOT EXISTS (
				SELECT 1
				FROM voice_participants
				WHERE user_id = $1 AND channel_id <> $2
			)
	`, request.senderID, request.token, request.eventAt).Scan(&qualified)
	if err != nil {
		return false, fmt.Errorf("qualify durable server voice terminal: %w", err)
	}
	return qualified, nil
}

type privateVoiceOldScopeRevision struct {
	conversationID      uuid.UUID
	callID              uuid.UUID
	participantIDs      []uuid.UUID
	recheckViewerIDs    []uuid.UUID
	capturedGenerations map[uuid.UUID]presence.ActivityGeneration
}

type privateVoiceScopeBaseDelta struct {
	conversationID uuid.UUID
	participantIDs []uuid.UUID
}

type privateVoiceLifecycleClaimsResult struct {
	acceptedParticipantIDs []uuid.UUID
	oldScopeRevisions      []privateVoiceOldScopeRevision
	oldScopeBaseDeltas     []privateVoiceScopeBaseDelta
}

type privateVoiceParticipantUpsertResult struct {
	reconnectParticipantIDs []uuid.UUID
	oldScopeRevisions       []privateVoiceOldScopeRevision
	oldScopeBaseDeltas      []privateVoiceScopeBaseDelta
}

func privateVoiceMovedScopeBaseDeltas(
	moved map[uuid.UUID]map[uuid.UUID]bool,
) []privateVoiceScopeBaseDelta {
	conversationIDs := make([]uuid.UUID, 0, len(moved))
	for conversationID, participantSet := range moved {
		if len(participantSet) > 0 {
			conversationIDs = append(conversationIDs, conversationID)
		}
	}
	sort.Slice(conversationIDs, func(left, right int) bool {
		return conversationIDs[left].String() < conversationIDs[right].String()
	})
	deltas := make([]privateVoiceScopeBaseDelta, 0, len(conversationIDs))
	for _, conversationID := range conversationIDs {
		participantIDs := make([]uuid.UUID, 0, len(moved[conversationID]))
		for participantID := range moved[conversationID] {
			participantIDs = append(participantIDs, participantID)
		}
		sort.Slice(participantIDs, func(left, right int) bool {
			return participantIDs[left].String() < participantIDs[right].String()
		})
		deltas = append(deltas, privateVoiceScopeBaseDelta{
			conversationID: conversationID, participantIDs: participantIDs,
		})
	}
	return deltas
}

func (s *NATSSubscriber) capturePrivateVoiceOldScopeRevisions(
	ctx context.Context,
	participantIDsByConversation map[uuid.UUID][]uuid.UUID,
	callIDsByConversation map[uuid.UUID]uuid.UUID,
	recheckViewersByConversation map[uuid.UUID]map[uuid.UUID]bool,
) ([]privateVoiceOldScopeRevision, error) {
	conversationIDs := make([]uuid.UUID, 0, len(participantIDsByConversation))
	for conversationID, participantIDs := range participantIDsByConversation {
		if len(participantIDs) > 0 {
			conversationIDs = append(conversationIDs, conversationID)
		}
	}
	if len(conversationIDs) > maxPrivateVoiceParticipantIDs {
		return nil, errors.New("private voice old-scope revision limit exceeded")
	}
	sort.Slice(conversationIDs, func(left, right int) bool {
		return conversationIDs[left].String() < conversationIDs[right].String()
	})

	revisions := make([]privateVoiceOldScopeRevision, 0, len(conversationIDs))
	allParticipantIDs := make([]uuid.UUID, 0)
	seenParticipants := make(map[uuid.UUID]bool)
	totalRecheckViewers := 0
	for _, conversationID := range conversationIDs {
		revision, participantIDs, buildErr := buildPrivateVoiceOldScopeRevision(
			conversationID,
			participantIDsByConversation[conversationID],
			callIDsByConversation[conversationID],
			recheckViewersByConversation[conversationID],
			seenParticipants,
		)
		if buildErr != nil {
			return nil, buildErr
		}
		allParticipantIDs = append(allParticipantIDs, participantIDs...)
		if len(allParticipantIDs) > maxPrivateVoiceParticipantIDs {
			return nil, errors.New("private voice old-scope participant limit exceeded")
		}
		totalRecheckViewers += len(revision.recheckViewerIDs)
		if totalRecheckViewers > maxPrivateVoiceParticipantIDs {
			return nil, errors.New("private voice old-scope recheck limit exceeded")
		}
		revisions = append(revisions, revision)
	}

	capturedGenerations, err := s.capturePrivateActivityGenerations(
		ctx, allParticipantIDs,
	)
	if err != nil {
		return nil, err
	}
	attachCapturedPrivateActivityGenerations(revisions, capturedGenerations)
	return revisions, nil
}

func buildPrivateVoiceOldScopeRevision(
	conversationID uuid.UUID,
	participantIDs []uuid.UUID,
	callID uuid.UUID,
	recheckViewers map[uuid.UUID]bool,
	seenParticipants map[uuid.UUID]bool,
) (privateVoiceOldScopeRevision, []uuid.UUID, error) {
	if conversationID == uuid.Nil || callID == uuid.Nil {
		return privateVoiceOldScopeRevision{}, nil, errors.New(
			"invalid private voice old-scope revision",
		)
	}
	participantIDs = append([]uuid.UUID(nil), participantIDs...)
	if len(participantIDs) > maxPrivateVoiceParticipantIDs {
		return privateVoiceOldScopeRevision{}, nil, errors.New(
			"private voice old-scope participant limit exceeded",
		)
	}
	sort.Slice(participantIDs, func(left, right int) bool {
		return participantIDs[left].String() < participantIDs[right].String()
	})
	for _, participantID := range participantIDs {
		if participantID == uuid.Nil || seenParticipants[participantID] {
			return privateVoiceOldScopeRevision{}, nil, errors.New(
				"invalid private voice old-scope participant",
			)
		}
		seenParticipants[participantID] = true
	}
	recheckViewerIDs := make([]uuid.UUID, 0, len(recheckViewers))
	for viewerID := range recheckViewers {
		if viewerID == uuid.Nil {
			return privateVoiceOldScopeRevision{}, nil, errors.New(
				"invalid private voice old-scope recheck viewer",
			)
		}
		recheckViewerIDs = append(recheckViewerIDs, viewerID)
	}
	sort.Slice(recheckViewerIDs, func(left, right int) bool {
		return recheckViewerIDs[left].String() < recheckViewerIDs[right].String()
	})
	return privateVoiceOldScopeRevision{
		conversationID: conversationID,
		callID:         callID, participantIDs: participantIDs,
		recheckViewerIDs: recheckViewerIDs,
	}, participantIDs, nil
}

func attachCapturedPrivateActivityGenerations(
	revisions []privateVoiceOldScopeRevision,
	capturedGenerations map[uuid.UUID]presence.ActivityGeneration,
) {
	for index := range revisions {
		revision := &revisions[index]
		revision.capturedGenerations = make(
			map[uuid.UUID]presence.ActivityGeneration, len(revision.participantIDs),
		)
		for _, participantID := range revision.participantIDs {
			if generation, found := capturedGenerations[participantID]; found {
				revision.capturedGenerations[participantID] = generation
			}
		}
	}
}

func (s *NATSSubscriber) refreshPrivateVoiceOldScopeRevisions(
	ctx context.Context,
	revisions []privateVoiceOldScopeRevision,
	eventAt time.Time,
) error {
	if len(revisions) == 0 {
		return nil
	}
	if s == nil || s.activity == nil || len(revisions) > maxPrivateVoiceParticipantIDs ||
		!presence.IsValidActivitySourceTime(eventAt) {
		return errors.New("invalid private voice old-scope refresh")
	}
	presence.InvalidateActivityBuildCache(ctx)
	for _, revision := range revisions {
		if err := s.deleteCapturedPrivateActivityGenerations(
			ctx, revision.participantIDs, revision.capturedGenerations,
		); err != nil {
			return fmt.Errorf("delete old-scope private activity generations: %w", err)
		}
	}
	for _, revision := range revisions {
		recheckViewers := make(map[uuid.UUID]bool, len(revision.recheckViewerIDs))
		for _, viewerID := range revision.recheckViewerIDs {
			recheckViewers[viewerID] = true
		}
		for _, participantID := range revision.participantIDs {
			if err := s.activity.RefreshPrivateCall(
				ctx,
				participantID,
				presence.Scope{
					Category:    presence.CategoryPrivateCall,
					RoomID:      revision.conversationID,
					LifecycleID: revision.callID,
					EventAt:     eventAt,
				},
				recheckViewers,
				nil,
			); err != nil {
				return fmt.Errorf("refresh old-scope private activity: %w", err)
			}
		}
	}
	return nil
}

type privateVoiceParticipantSetClaimState struct {
	preRecords      []voiceParticipantRecord
	tokenStates     map[uuid.UUID]voiceLifecycleTokenState
	postSet         map[uuid.UUID]bool
	postIDs         []uuid.UUID
	claimSet        map[uuid.UUID]bool
	mutationApplied bool
	reconnect       bool
}

func (s *NATSSubscriber) withPrivateVoiceParticipantSetClaim(
	ctx context.Context,
	request voiceLifecycleClaimRequest,
	mutation func(context.Context, *sql.Tx, voiceLifecycleClaimStatus) (bool, error),
) (applied bool, status voiceLifecycleClaimStatus, returnErr error) {
	if request.senderID == uuid.Nil || request.token == uuid.Nil ||
		request.conversationID == uuid.Nil ||
		!presence.IsValidActivitySourceTime(request.eventAt) || mutation == nil {
		return false, voiceLifecycleRejected, errors.New(
			"invalid private voice participant-set mutation",
		)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, voiceLifecycleRejected, fmt.Errorf(
			"begin private voice participant-set mutation: %w", err,
		)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		returnErr = joinRollbackErr(returnErr, rollbackErr, "rollback private voice participant-set mutation")
	}()
	state, rejected, err := s.preparePrivateVoiceParticipantSetClaim(ctx, tx, request)
	if err != nil || rejected {
		return false, voiceLifecycleRejected, err
	}
	state.mutationApplied, err = mutation(ctx, tx, voiceLifecycleFresh)
	if err != nil {
		return false, voiceLifecycleRejected, err
	}
	rejected, err = s.reconcilePrivateVoiceParticipantSetClaim(ctx, tx, request, state)
	if err != nil || rejected {
		return false, voiceLifecycleRejected, err
	}
	claims := privateVoiceParticipantSetClaims(state.claimSet)
	status, err = s.claimPrivateVoiceParticipantSetRevision(
		ctx, request.token, request.eventAt, claims,
	)
	if err != nil || status == voiceLifecycleRejected {
		return false, status, err
	}
	return s.commitPrivateVoiceParticipantSetClaim(
		ctx, tx, request, state, claims, status,
	)
}

func (s *NATSSubscriber) preparePrivateVoiceParticipantSetClaim(
	ctx context.Context,
	tx *sql.Tx,
	request voiceLifecycleClaimRequest,
) (*privateVoiceParticipantSetClaimState, bool, error) {
	if err := lockPrivateVoiceParticipantSet(ctx, tx, request.conversationID); err != nil {
		return nil, false, err
	}
	preRecords, err := s.collectVoiceParticipantRecordsFrom(
		ctx, tx, request.conversationID, true,
	)
	if err != nil {
		return nil, false, err
	}
	lockedIDs, rejected := privateVoiceParticipantSetLockedIDs(
		preRecords, request.senderID, request.eventAt,
	)
	if rejected {
		return nil, true, nil
	}
	lockedIDs, err = lockPrivateVoiceParticipantLifecycles(ctx, tx, lockedIDs)
	if err != nil {
		return nil, false, err
	}
	tokenStates, err := s.privateVoiceLifecycleTokenStates(
		ctx, lockedIDs, request.token,
	)
	if err != nil {
		return nil, false, err
	}
	preSet := privateVoiceParticipantSet(preRecords)
	// A terminal event from call A whose sender already has neither an A row nor
	// an A/missing lifecycle watermark is stale. A retained stale row is the only
	// durable evidence authorizing recovery from a failed post-claim move.
	if !request.active && tokenStates[request.senderID] == voiceLifecycleTokenDifferent &&
		!preSet[request.senderID] {
		return nil, true, nil
	}
	return &privateVoiceParticipantSetClaimState{
		preRecords: preRecords, tokenStates: tokenStates,
	}, false, nil
}

func privateVoiceParticipantSetLockedIDs(
	preRecords []voiceParticipantRecord,
	senderID uuid.UUID,
	eventAt time.Time,
) ([]uuid.UUID, bool) {
	lockedIDs := make([]uuid.UUID, 0, len(preRecords)+1)
	lockedIDs = append(lockedIDs, senderID)
	for _, participant := range preRecords {
		if participant.lifecycleEventAt.After(eventAt) {
			return nil, true
		}
		lockedIDs = append(lockedIDs, participant.userID)
	}
	return lockedIDs, false
}

func (s *NATSSubscriber) privateVoiceLifecycleTokenStates(
	ctx context.Context,
	participantIDs []uuid.UUID,
	callID uuid.UUID,
) (map[uuid.UUID]voiceLifecycleTokenState, error) {
	tokenStates := make(map[uuid.UUID]voiceLifecycleTokenState, len(participantIDs))
	for _, participantID := range participantIDs {
		tokenState, err := s.voiceLifecycleTokenState(
			ctx, presence.CategoryPrivateCall, participantID, callID,
		)
		if err != nil {
			return nil, err
		}
		tokenStates[participantID] = tokenState
	}
	return tokenStates, nil
}

func (s *NATSSubscriber) reconcilePrivateVoiceParticipantSetClaim(
	ctx context.Context,
	tx *sql.Tx,
	request voiceLifecycleClaimRequest,
	state *privateVoiceParticipantSetClaimState,
) (bool, error) {
	postRecords, err := s.collectVoiceParticipantRecordsFrom(
		ctx, tx, request.conversationID, true,
	)
	if err != nil {
		return false, err
	}
	state.postSet = make(map[uuid.UUID]bool, len(postRecords))
	state.postIDs = make([]uuid.UUID, 0, len(postRecords))
	state.claimSet = make(map[uuid.UUID]bool, len(state.preRecords)+len(postRecords)+1)
	state.addPrivateVoicePreClaims()
	for _, participant := range postRecords {
		rejected, reconcileErr := s.reconcilePrivateVoicePostParticipant(
			ctx, tx, request, participant, state,
		)
		if reconcileErr != nil || rejected {
			return rejected, reconcileErr
		}
	}
	if state.tokenStates[request.senderID] != voiceLifecycleTokenDifferent {
		state.claimSet[request.senderID] = state.postSet[request.senderID]
	}
	if request.active != state.postSet[request.senderID] {
		return true, nil
	}
	if state.tokenStates[request.senderID] == voiceLifecycleTokenDifferent &&
		!state.mutationApplied {
		return true, nil
	}
	return false, nil
}

func (state *privateVoiceParticipantSetClaimState) addPrivateVoicePreClaims() {
	for _, participant := range state.preRecords {
		if state.tokenStates[participant.userID] == voiceLifecycleTokenDifferent {
			state.reconnect = true
			continue
		}
		state.claimSet[participant.userID] = false
	}
}

func (s *NATSSubscriber) reconcilePrivateVoicePostParticipant(
	ctx context.Context,
	tx *sql.Tx,
	request voiceLifecycleClaimRequest,
	participant voiceParticipantRecord,
	state *privateVoiceParticipantSetClaimState,
) (bool, error) {
	if participant.lifecycleEventAt.After(request.eventAt) {
		return true, nil
	}
	switch state.tokenStates[participant.userID] {
	case voiceLifecycleTokenMissing, voiceLifecycleTokenExactActive:
		state.postSet[participant.userID] = true
		state.postIDs = append(state.postIDs, participant.userID)
		state.claimSet[participant.userID] = true
		return false, nil
	case voiceLifecycleTokenExactTerminal:
		state.claimSet[participant.userID] = false
	case voiceLifecycleTokenDifferent:
		// Preserve the successor lifecycle while deleting only its stale DB row.
	default:
		return false, errors.New("invalid private voice participant lifecycle state")
	}
	deleted, err := deleteMismatchedPrivateVoiceParticipant(
		ctx, tx, request.conversationID, participant.userID, request.eventAt,
	)
	if err != nil {
		return false, err
	}
	if deleted {
		state.mutationApplied = true
		state.reconnect = true
	}
	return false, nil
}

func deleteMismatchedPrivateVoiceParticipant(
	ctx context.Context,
	tx *sql.Tx,
	conversationID, participantID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
		  AND lifecycle_event_at <= $3
	`, conversationID, participantID, eventAt)
	if err != nil {
		return false, fmt.Errorf("delete mismatched private voice participant: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read mismatched private voice participant delete: %w", err)
	}
	if rowsAffected > 1 {
		return false, errors.New(
			"mismatched private voice participant delete affected multiple rows",
		)
	}
	return rowsAffected == 1, nil
}

func privateVoiceParticipantSetClaims(
	claimSet map[uuid.UUID]bool,
) []privateVoiceParticipantSetClaim {
	claims := make([]privateVoiceParticipantSetClaim, 0, len(claimSet))
	for participantID, participantActive := range claimSet {
		claims = append(claims, privateVoiceParticipantSetClaim{
			userID: participantID, active: participantActive,
		})
	}
	return claims
}

func (s *NATSSubscriber) claimPrivateVoiceParticipantSetRevision(
	ctx context.Context,
	callID uuid.UUID,
	eventAt time.Time,
	claims []privateVoiceParticipantSetClaim,
) (voiceLifecycleClaimStatus, error) {
	if len(claims) == 0 {
		return voiceLifecycleFresh, nil
	}
	status, err := s.claimPrivateVoiceParticipantSet(ctx, callID, eventAt, claims)
	if err != nil || status == voiceLifecycleRejected {
		return status, err
	}
	for _, claim := range claims {
		if s.voiceLifecycleClaimedHook != nil {
			s.voiceLifecycleClaimedHook(
				presence.CategoryPrivateCall, claim.userID, eventAt,
			)
		}
	}
	return status, nil
}

func (s *NATSSubscriber) commitPrivateVoiceParticipantSetClaim(
	ctx context.Context,
	tx *sql.Tx,
	request voiceLifecycleClaimRequest,
	state *privateVoiceParticipantSetClaimState,
	claims []privateVoiceParticipantSetClaim,
	status voiceLifecycleClaimStatus,
) (bool, voiceLifecycleClaimStatus, error) {
	if !state.mutationApplied &&
		(len(claims) == 0 || status != voiceLifecycleDuplicate) {
		return false, status, errors.New(
			"fresh private voice participant-set claim changed no database rows",
		)
	}
	if err := advancePrivateVoiceParticipantRows(
		ctx, tx, request.conversationID, state.postIDs, request.eventAt,
	); err != nil {
		return false, status, err
	}
	if err := tx.Commit(); err != nil {
		return false, status, fmt.Errorf(
			"commit private voice participant-set mutation: %w", err,
		)
	}
	if !state.mutationApplied && !request.active && status == voiceLifecycleDuplicate {
		state.mutationApplied = true
	}
	if state.reconnect {
		s.disconnectAllRichPresenceClients()
	}
	return state.mutationApplied, status, nil
}

func (s *NATSSubscriber) withVoiceLifecycleLockInParticipantSet(
	ctx context.Context,
	category presence.Category,
	senderID, conversationID uuid.UUID,
	mutation func(context.Context, *sql.Tx) (bool, error),
) (applied bool, returnErr error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s == nil || s.db == nil || mutation == nil {
		return false, errors.New("invalid voice lifecycle lock mutation")
	}
	lockKey, err := voiceLifecycleAdvisoryKey(category, senderID)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin voice lifecycle lock mutation: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		returnErr = joinRollbackErr(returnErr, rollbackErr, "rollback voice lifecycle lock mutation")
	}()
	if err := lockVoiceLifecycleMutationScope(
		ctx, tx, category, conversationID,
	); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return false, fmt.Errorf("lock voice lifecycle mutation: %w", err)
	}
	applied, err = mutation(ctx, tx)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit voice lifecycle lock mutation: %w", err)
	}
	return applied, nil
}

func lockVoiceLifecycleMutationScope(
	ctx context.Context,
	tx *sql.Tx,
	category presence.Category,
	conversationID uuid.UUID,
) error {
	if conversationID == uuid.Nil {
		return nil
	}
	if category != presence.CategoryPrivateCall {
		return errors.New("participant-set lock requires private call")
	}
	return lockPrivateVoiceParticipantSet(ctx, tx, conversationID)
}

type privateVoiceLifecycleClaimsRequest struct {
	conversationID uuid.UUID
	senderIDs      []uuid.UUID
	token          uuid.UUID
	eventAt        time.Time
	refreshLease   func(context.Context) error
	mutation       func(context.Context, *sql.Tx, []uuid.UUID, []uuid.UUID) (bool, error)
}

type privateVoiceLifecycleClaimsState struct {
	senderIDs         []uuid.UUID
	accepted          []uuid.UUID
	scopeMemberships  []privateVoiceScopeMembership
	targetRecords     []voiceParticipantRecord
	targetIDs         []uuid.UUID
	oldMoved          map[uuid.UUID]map[uuid.UUID]bool
	oldScopePost      map[uuid.UUID][]uuid.UUID
	oldScopeStale     map[uuid.UUID][]uuid.UUID
	oldScopeCallIDs   map[uuid.UUID]uuid.UUID
	oldScopeRevisions []privateVoiceOldScopeRevision
	targetRevisionIDs []uuid.UUID
	targetRemoved     []uuid.UUID
	targetStale       []uuid.UUID
	reconnect         bool
}

func (s *NATSSubscriber) withVoiceLifecycleClaims(
	ctx context.Context,
	request privateVoiceLifecycleClaimsRequest,
) (claimResult *privateVoiceLifecycleClaimsResult, applied bool, reconnect bool, returnErr error) {
	state, err := s.prepareGroupedVoiceLifecycleClaimsState(ctx, request)
	if err != nil {
		return nil, false, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, false, fmt.Errorf("begin grouped voice lifecycle mutation: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		returnErr = joinRollbackErr(returnErr, rollbackErr, "rollback grouped voice lifecycle mutation")
	}()
	if err := s.lockGroupedPrivateVoiceScopes(ctx, tx, request, state); err != nil {
		return nil, false, false, err
	}
	if err := s.prepareGroupedPrivateVoiceTarget(ctx, tx, request, state); err != nil {
		return nil, false, false, err
	}
	rejected, err := s.prepareGroupedPrivateVoiceOldScopes(ctx, tx, request, state)
	if err != nil || rejected {
		return nil, false, false, err
	}
	claims, rejected, err := s.prepareGroupedPrivateVoiceClaims(ctx, request, state)
	if err != nil || rejected {
		return nil, false, false, err
	}
	status, err := s.claimGroupedPrivateVoiceLifecycles(ctx, claims, request.eventAt)
	if err != nil || status == voiceLifecycleRejected {
		return nil, false, false, err
	}
	state.reconnect = state.reconnect || status == voiceLifecycleDuplicate
	applied, err = s.applyGroupedPrivateVoiceMutation(ctx, tx, request, state)
	if err != nil {
		return nil, false, false, err
	}
	if err := s.commitGroupedPrivateVoiceMutation(ctx, tx, request, state); err != nil {
		return nil, false, false, err
	}
	return &privateVoiceLifecycleClaimsResult{
		acceptedParticipantIDs: append([]uuid.UUID(nil), state.accepted...),
		oldScopeRevisions:      state.oldScopeRevisions,
		oldScopeBaseDeltas:     privateVoiceMovedScopeBaseDeltas(state.oldMoved),
	}, applied, state.reconnect, nil
}

func (s *NATSSubscriber) prepareGroupedVoiceLifecycleClaimsState(
	ctx context.Context,
	request privateVoiceLifecycleClaimsRequest,
) (*privateVoiceLifecycleClaimsState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.db == nil || s.redis == nil ||
		request.conversationID == uuid.Nil || request.token == uuid.Nil ||
		request.mutation == nil || !presence.IsValidActivitySourceTime(request.eventAt) {
		return nil, errors.New("invalid grouped voice lifecycle mutation")
	}
	senderIDs, err := normalizeGroupedVoiceSenderIDs(request.senderIDs)
	if err != nil {
		return nil, err
	}
	return &privateVoiceLifecycleClaimsState{senderIDs: senderIDs}, nil
}

func normalizeGroupedVoiceSenderIDs(senderIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(senderIDs) > maxPrivateVoiceParticipantIDs {
		return nil, errors.New("grouped voice lifecycle participant limit exceeded")
	}
	ordered := append([]uuid.UUID(nil), senderIDs...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].String() < ordered[right].String()
	})
	unique := ordered[:0]
	for _, senderID := range ordered {
		if senderID == uuid.Nil {
			return nil, errors.New("invalid grouped voice lifecycle sender")
		}
		if len(unique) == 0 || unique[len(unique)-1] != senderID {
			unique = append(unique, senderID)
		}
	}
	if len(unique) == 0 {
		return nil, errors.New("empty grouped voice lifecycle mutation")
	}
	return unique, nil
}

func (s *NATSSubscriber) lockGroupedPrivateVoiceScopes(
	ctx context.Context,
	tx *sql.Tx,
	request privateVoiceLifecycleClaimsRequest,
	state *privateVoiceLifecycleClaimsState,
) error {
	if err := lockPrivateVoiceScopes(ctx, tx, state.senderIDs); err != nil {
		return err
	}
	memberships, err := s.collectPrivateVoiceScopeMembershipsFrom(ctx, tx, state.senderIDs)
	if err != nil {
		return err
	}
	lockedConversationIDs, err := lockPrivateVoiceParticipantSets(
		ctx, tx, groupedPrivateVoiceConversationIDs(request.conversationID, memberships),
	)
	if err != nil {
		return err
	}
	memberships, err = s.collectPrivateVoiceScopeMembershipsFrom(ctx, tx, state.senderIDs)
	if err != nil {
		return err
	}
	if !privateVoiceMembershipsWithinLocks(memberships, lockedConversationIDs) {
		return errors.New("private voice scope changed during heartbeat lock acquisition")
	}
	state.scopeMemberships = memberships
	state.accepted, err = s.currentDMVoiceMembers(
		ctx, tx, request.conversationID, state.senderIDs,
	)
	if err != nil {
		return err
	}
	return s.refreshGroupedPrivateVoiceLease(ctx, request, state.accepted)
}

func groupedPrivateVoiceConversationIDs(
	targetConversationID uuid.UUID,
	memberships []privateVoiceScopeMembership,
) []uuid.UUID {
	conversationIDs := make([]uuid.UUID, 0, 1+len(memberships))
	conversationIDs = append(conversationIDs, targetConversationID)
	for _, membership := range memberships {
		conversationIDs = append(conversationIDs, membership.conversationID)
	}
	return conversationIDs
}

func privateVoiceMembershipsWithinLocks(
	memberships []privateVoiceScopeMembership,
	lockedConversationIDs []uuid.UUID,
) bool {
	locked := make(map[uuid.UUID]bool, len(lockedConversationIDs))
	for _, conversationID := range lockedConversationIDs {
		locked[conversationID] = true
	}
	for _, membership := range memberships {
		if !locked[membership.conversationID] {
			return false
		}
	}
	return true
}

func (s *NATSSubscriber) refreshGroupedPrivateVoiceLease(
	ctx context.Context,
	request privateVoiceLifecycleClaimsRequest,
	accepted []uuid.UUID,
) error {
	if len(accepted) == 0 {
		return s.requireDMVoiceCallLease(ctx, request.conversationID, request.token)
	}
	if request.refreshLease == nil {
		return errors.New("private voice lease refresh unavailable")
	}
	if err := request.refreshLease(ctx); err != nil {
		return fmt.Errorf("refresh private voice call lease: %w", err)
	}
	return nil
}

func (s *NATSSubscriber) prepareGroupedPrivateVoiceTarget(
	ctx context.Context,
	tx *sql.Tx,
	request privateVoiceLifecycleClaimsRequest,
	state *privateVoiceLifecycleClaimsState,
) error {
	var err error
	state.targetRecords, err = s.collectVoiceParticipantRecordsFrom(
		ctx, tx, request.conversationID, true,
	)
	if err != nil {
		return err
	}
	state.targetIDs = voiceParticipantRecordIDs(state.targetRecords)
	if !privateVoiceParticipantUnionWithinLimit(state.targetIDs, state.accepted) {
		return errors.New("private voice participant limit exceeded")
	}
	state.oldMoved = groupedPrivateVoiceMovedScopes(
		request.conversationID, state.scopeMemberships, uuidSliceSet(state.accepted),
	)
	return nil
}

func voiceParticipantRecordIDs(records []voiceParticipantRecord) []uuid.UUID {
	participantIDs := make([]uuid.UUID, 0, len(records))
	for _, participant := range records {
		participantIDs = append(participantIDs, participant.userID)
	}
	return participantIDs
}

// prioritizeServerHeartbeatParticipants makes a partially completed heartbeat
// durable and fair across retries. Missing rows lead so successful inserts stop
// being missing on the next tick; existing rows follow oldest lifecycle first,
// so every successful refresh moves behind untouched rows without an in-memory
// cursor or schema state.
func prioritizeServerHeartbeatParticipants(
	mediaParticipantIDs []uuid.UUID,
	existingRecords []voiceParticipantRecord,
) []uuid.UUID {
	existing := make(map[uuid.UUID]time.Time, len(existingRecords))
	for _, record := range existingRecords {
		existing[record.userID] = record.lifecycleEventAt
	}
	missing := make([]uuid.UUID, 0, len(mediaParticipantIDs))
	present := make([]voiceParticipantRecord, 0, len(mediaParticipantIDs))
	for _, participantID := range mediaParticipantIDs {
		lifecycleEventAt, found := existing[participantID]
		if !found {
			missing = append(missing, participantID)
			continue
		}
		present = append(present, voiceParticipantRecord{
			userID: participantID, lifecycleEventAt: lifecycleEventAt,
		})
	}
	sort.Slice(missing, func(left, right int) bool {
		return missing[left].String() < missing[right].String()
	})
	sort.Slice(present, func(left, right int) bool {
		if present[left].lifecycleEventAt.Equal(present[right].lifecycleEventAt) {
			return present[left].userID.String() < present[right].userID.String()
		}
		return present[left].lifecycleEventAt.Before(present[right].lifecycleEventAt)
	})
	ordered := make([]uuid.UUID, 0, len(mediaParticipantIDs))
	ordered = append(ordered, missing...)
	for _, record := range present {
		ordered = append(ordered, record.userID)
	}
	return ordered
}

func uuidSliceSet(participantIDs []uuid.UUID) map[uuid.UUID]bool {
	participants := make(map[uuid.UUID]bool, len(participantIDs))
	for _, participantID := range participantIDs {
		participants[participantID] = true
	}
	return participants
}

func groupedPrivateVoiceMovedScopes(
	targetConversationID uuid.UUID,
	memberships []privateVoiceScopeMembership,
	accepted map[uuid.UUID]bool,
) map[uuid.UUID]map[uuid.UUID]bool {
	moved := make(map[uuid.UUID]map[uuid.UUID]bool)
	for _, membership := range memberships {
		if membership.conversationID == targetConversationID || !accepted[membership.userID] {
			continue
		}
		if moved[membership.conversationID] == nil {
			moved[membership.conversationID] = make(map[uuid.UUID]bool)
		}
		moved[membership.conversationID][membership.userID] = true
	}
	return moved
}

func (s *NATSSubscriber) prepareGroupedPrivateVoiceOldScopes(
	ctx context.Context,
	tx *sql.Tx,
	request privateVoiceLifecycleClaimsRequest,
	state *privateVoiceLifecycleClaimsState,
) (bool, error) {
	state.oldScopePost = make(map[uuid.UUID][]uuid.UUID)
	state.oldScopeStale = make(map[uuid.UUID][]uuid.UUID)
	state.oldScopeCallIDs = make(map[uuid.UUID]uuid.UUID)
	lockedParticipantIDs := append(append([]uuid.UUID(nil), state.targetIDs...), state.accepted...)
	for conversationID, movedSet := range state.oldMoved {
		participantIDs, rejected, err := s.prepareGroupedPrivateVoiceOldScope(
			ctx, tx, request.eventAt, conversationID, movedSet, state,
		)
		if err != nil || rejected {
			return rejected, err
		}
		lockedParticipantIDs = append(lockedParticipantIDs, participantIDs...)
	}
	if _, err := lockPrivateVoiceParticipantLifecycles(
		ctx, tx, lockedParticipantIDs,
	); err != nil {
		return false, err
	}
	var err error
	state.oldScopeRevisions, err = s.capturePrivateVoiceOldScopeRevisions(
		ctx, state.oldScopePost, state.oldScopeCallIDs, state.oldMoved,
	)
	return false, err
}

func (s *NATSSubscriber) prepareGroupedPrivateVoiceOldScope(
	ctx context.Context,
	tx *sql.Tx,
	eventAt time.Time,
	conversationID uuid.UUID,
	movedSet map[uuid.UUID]bool,
	state *privateVoiceLifecycleClaimsState,
) ([]uuid.UUID, bool, error) {
	oldRecords, err := s.collectVoiceParticipantRecordsFrom(ctx, tx, conversationID, true)
	if err != nil {
		return nil, false, err
	}
	candidates, rejected := partitionGroupedPrivateVoiceOldScope(
		oldRecords, movedSet, eventAt,
	)
	if rejected {
		return nil, true, nil
	}
	participantIDs := voiceParticipantRecordIDs(oldRecords)
	if len(candidates) == 0 {
		state.oldScopePost[conversationID] = nil
		return participantIDs, false, nil
	}
	lease, found, err := dm.LookupDMVoiceCallLease(ctx, s.redis, conversationID)
	if err != nil {
		return nil, false, err
	}
	if !found || lease.CallID == uuid.Nil {
		state.oldScopePost[conversationID] = nil
		state.reconnect = true
		return participantIDs, false, nil
	}
	state.oldScopeCallIDs[conversationID] = lease.CallID
	rejected, err = s.classifyGroupedPrivateVoiceOldScope(
		ctx, conversationID, candidates, lease.CallID, eventAt, state,
	)
	return participantIDs, rejected, err
}

func partitionGroupedPrivateVoiceOldScope(
	records []voiceParticipantRecord,
	movedSet map[uuid.UUID]bool,
	eventAt time.Time,
) ([]voiceParticipantRecord, bool) {
	candidates := make([]voiceParticipantRecord, 0, len(records))
	for _, participant := range records {
		moved := movedSet[participant.userID]
		if moved && !participant.lifecycleEventAt.Before(eventAt) {
			return nil, true
		}
		if !moved {
			candidates = append(candidates, participant)
		}
	}
	return candidates, false
}

func (s *NATSSubscriber) classifyGroupedPrivateVoiceOldScope(
	ctx context.Context,
	conversationID uuid.UUID,
	candidates []voiceParticipantRecord,
	callID uuid.UUID,
	eventAt time.Time,
	state *privateVoiceLifecycleClaimsState,
) (bool, error) {
	for _, participant := range candidates {
		if participant.lifecycleEventAt.After(eventAt) {
			return true, nil
		}
		matches, err := s.matchesActiveVoiceLifecycleToken(
			ctx, presence.CategoryPrivateCall, participant.userID, callID,
		)
		if err != nil {
			return false, err
		}
		if matches {
			state.oldScopePost[conversationID] = append(
				state.oldScopePost[conversationID], participant.userID,
			)
			continue
		}
		state.oldScopeStale[conversationID] = append(
			state.oldScopeStale[conversationID], participant.userID,
		)
	}
	return false, nil
}

func (s *NATSSubscriber) prepareGroupedPrivateVoiceClaims(
	ctx context.Context,
	request privateVoiceLifecycleClaimsRequest,
	state *privateVoiceLifecycleClaimsState,
) ([]privateVoiceParticipantSetClaim, bool, error) {
	for _, participant := range state.targetRecords {
		if participant.lifecycleEventAt.After(request.eventAt) {
			return nil, true, nil
		}
	}
	claims := make(map[uuid.UUID]privateVoiceParticipantSetClaim)
	state.targetRevisionIDs = append(state.targetRevisionIDs, state.accepted...)
	for _, participantID := range state.accepted {
		claims[participantID] = privateVoiceParticipantSetClaim{
			userID: participantID, token: request.token,
			version: request.eventAt.UnixMicro(), active: true,
		}
	}
	if err := s.classifyGroupedPrivateVoiceTarget(ctx, request, state, claims); err != nil {
		return nil, false, err
	}
	if err := addGroupedPrivateVoiceOldScopeClaims(
		request.eventAt, state, claims,
	); err != nil {
		return nil, false, err
	}
	result := make([]privateVoiceParticipantSetClaim, 0, len(claims))
	for _, claim := range claims {
		result = append(result, claim)
	}
	return result, false, nil
}

func (s *NATSSubscriber) classifyGroupedPrivateVoiceTarget(
	ctx context.Context,
	request privateVoiceLifecycleClaimsRequest,
	state *privateVoiceLifecycleClaimsState,
	claims map[uuid.UUID]privateVoiceParticipantSetClaim,
) error {
	targetRevisionSet := uuidSliceSet(state.targetRevisionIDs)
	for _, participant := range state.targetRecords {
		if targetRevisionSet[participant.userID] {
			continue
		}
		matches, err := s.matchesActiveVoiceLifecycleToken(
			ctx, presence.CategoryPrivateCall, participant.userID, request.token,
		)
		if err != nil {
			return err
		}
		if !matches {
			state.targetStale = append(state.targetStale, participant.userID)
			continue
		}
		claims[participant.userID] = privateVoiceParticipantSetClaim{
			userID: participant.userID, token: request.token,
			version: request.eventAt.UnixMicro(), active: false,
		}
		state.targetRemoved = append(state.targetRemoved, participant.userID)
	}
	return nil
}

func addGroupedPrivateVoiceOldScopeClaims(
	eventAt time.Time,
	state *privateVoiceLifecycleClaimsState,
	claims map[uuid.UUID]privateVoiceParticipantSetClaim,
) error {
	for conversationID, participantIDs := range state.oldScopePost {
		callID := state.oldScopeCallIDs[conversationID]
		for _, participantID := range participantIDs {
			if _, conflict := claims[participantID]; conflict {
				return errors.New("conflicting private voice scope participant")
			}
			claims[participantID] = privateVoiceParticipantSetClaim{
				userID: participantID, token: callID,
				version: eventAt.UnixMicro(), active: true,
			}
		}
	}
	return nil
}

func (s *NATSSubscriber) claimGroupedPrivateVoiceLifecycles(
	ctx context.Context,
	claims []privateVoiceParticipantSetClaim,
	eventAt time.Time,
) (voiceLifecycleClaimStatus, error) {
	if len(claims) == 0 {
		return voiceLifecycleFresh, nil
	}
	status, err := s.claimPrivateVoiceLifecycles(ctx, claims)
	if err != nil || status == voiceLifecycleRejected {
		return status, err
	}
	for _, claim := range claims {
		if s.voiceLifecycleClaimedHook != nil {
			s.voiceLifecycleClaimedHook(
				presence.CategoryPrivateCall, claim.userID, eventAt,
			)
		}
	}
	return status, nil
}

func (s *NATSSubscriber) applyGroupedPrivateVoiceMutation(
	ctx context.Context,
	tx *sql.Tx,
	request privateVoiceLifecycleClaimsRequest,
	state *privateVoiceLifecycleClaimsState,
) (bool, error) {
	if err := deleteGroupedPrivateVoiceMovedScopes(
		ctx, tx, state.oldMoved, request.eventAt,
	); err != nil {
		return false, err
	}
	deletedOldStale, err := deleteGroupedPrivateVoiceParticipantLists(
		ctx, tx, state.oldScopeStale, request.eventAt, "old-scope",
	)
	if err != nil {
		return false, err
	}
	deletedTarget, err := deleteGroupedPrivateVoiceTargetParticipants(
		ctx, tx, request.conversationID,
		append(append([]uuid.UUID(nil), state.targetRemoved...), state.targetStale...),
		request.eventAt,
	)
	if err != nil {
		return false, err
	}
	state.reconnect = state.reconnect || deletedOldStale || deletedTarget
	return request.mutation(ctx, tx, state.accepted, state.targetRemoved)
}

func deleteGroupedPrivateVoiceMovedScopes(
	ctx context.Context,
	tx *sql.Tx,
	moved map[uuid.UUID]map[uuid.UUID]bool,
	eventAt time.Time,
) error {
	for conversationID, movedSet := range moved {
		rawMoved := make([]string, 0, len(movedSet))
		for participantID := range movedSet {
			rawMoved = append(rawMoved, participantID.String())
		}
		result, err := tx.ExecContext(ctx, `
			DELETE FROM dm_voice_participants
			WHERE conversation_id = $1 AND user_id = ANY($2::uuid[])
			  AND lifecycle_event_at <= $3
		`, conversationID, pq.Array(rawMoved), eventAt)
		if err != nil {
			return fmt.Errorf("delete old private voice scopes: %w", err)
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read old private voice scope delete: %w", err)
		}
		if removed > int64(len(movedSet)) {
			return errors.New("old private voice scope delete affected unexpected rows")
		}
	}
	return nil
}

func deleteGroupedPrivateVoiceParticipantLists(
	ctx context.Context,
	tx *sql.Tx,
	participants map[uuid.UUID][]uuid.UUID,
	eventAt time.Time,
	scopeLabel string,
) (bool, error) {
	deleted := false
	for conversationID, participantIDs := range participants {
		for _, participantID := range participantIDs {
			if err := deleteGroupedPrivateVoiceParticipant(
				ctx, tx, conversationID, participantID, eventAt,
			); err != nil {
				return false, fmt.Errorf("delete stale %s participant: %w", scopeLabel, err)
			}
			deleted = true
		}
	}
	return deleted, nil
}

func deleteGroupedPrivateVoiceTargetParticipants(
	ctx context.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
	participantIDs []uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	deleted := false
	for _, participantID := range participantIDs {
		if err := deleteGroupedPrivateVoiceParticipant(
			ctx, tx, conversationID, participantID, eventAt,
		); err != nil {
			return false, fmt.Errorf("delete stale target participant: %w", err)
		}
		deleted = true
	}
	return deleted, nil
}

func deleteGroupedPrivateVoiceParticipant(
	ctx context.Context,
	tx *sql.Tx,
	conversationID, participantID uuid.UUID,
	eventAt time.Time,
) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
		  AND lifecycle_event_at <= $3
	`, conversationID, participantID, eventAt)
	return err
}

func (s *NATSSubscriber) commitGroupedPrivateVoiceMutation(
	ctx context.Context,
	tx *sql.Tx,
	request privateVoiceLifecycleClaimsRequest,
	state *privateVoiceLifecycleClaimsState,
) error {
	if err := advancePrivateVoiceParticipantRows(
		ctx, tx, request.conversationID, state.targetRevisionIDs, request.eventAt,
	); err != nil {
		return err
	}
	for conversationID, participantIDs := range state.oldScopePost {
		if err := advancePrivateVoiceParticipantRows(
			ctx, tx, conversationID, participantIDs, request.eventAt,
		); err != nil {
			return err
		}
		if len(participantIDs) > 0 {
			if err := s.requireDMVoiceCallLease(
				ctx, conversationID, state.oldScopeCallIDs[conversationID],
			); err != nil {
				return err
			}
		}
	}
	if err := s.requireDMVoiceCallLease(
		ctx, request.conversationID, request.token,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit grouped voice lifecycle mutation: %w", err)
	}
	return nil
}

func (s *NATSSubscriber) requireDMVoiceCallLease(
	ctx context.Context,
	conversationID, callID uuid.UUID,
) error {
	lease, found, err := dm.LookupDMVoiceCallLease(ctx, s.redis, conversationID)
	if err != nil {
		return fmt.Errorf("verify private voice call lease: %w", err)
	}
	if !found || lease.CallID != callID {
		return fmt.Errorf("%w: private voice call lease changed", dm.ErrDMVoiceCallLeaseConflict)
	}
	return nil
}

func (s *NATSSubscriber) currentDMVoiceMembers(
	ctx context.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
	participantIDs []uuid.UUID,
) (members []uuid.UUID, returnErr error) {
	rawParticipantIDs := make([]string, 0, len(participantIDs))
	for _, participantID := range participantIDs {
		rawParticipantIDs = append(rawParticipantIDs, participantID.String())
	}
	var parentID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM dm_conversations
		WHERE id = $1
		FOR KEY SHARE
	`, conversationID).Scan(&parentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lock private voice conversation: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT user_id
		FROM dm_participants
		WHERE conversation_id = $1 AND user_id = ANY($2::uuid[])
		ORDER BY user_id
		FOR KEY SHARE
	`, conversationID, pq.Array(rawParticipantIDs))
	if err != nil {
		return nil, fmt.Errorf("validate private voice members: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("close private voice member rows: %w", closeErr),
			)
		}
	}()
	for rows.Next() {
		var memberID uuid.UUID
		if scanErr := rows.Scan(&memberID); scanErr != nil {
			return nil, fmt.Errorf("scan private voice member: %w", scanErr)
		}
		members = append(members, memberID)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate private voice members: %w", rowsErr)
	}
	return members, nil
}

type privateVoiceParticipantUpsertRequest struct {
	conversationID uuid.UUID
	senderID       uuid.UUID
	callID         uuid.UUID
	eventAt        time.Time
}

type privateVoiceParticipantUpsertState struct {
	request                  privateVoiceParticipantUpsertRequest
	scopeMemberships         []privateVoiceScopeMembership
	existingRecords          []voiceParticipantRecord
	existingParticipantIDs   []uuid.UUID
	oldScopePost             map[uuid.UUID][]uuid.UUID
	oldScopeStale            map[uuid.UUID][]uuid.UUID
	oldScopeCallIDs          map[uuid.UUID]uuid.UUID
	oldScopeMoved            map[uuid.UUID]map[uuid.UUID]bool
	seenOldScopes            map[uuid.UUID]bool
	oldScopeRevisions        []privateVoiceOldScopeRevision
	reconnectUnknownOldScope bool
	postParticipantIDs       []uuid.UUID
	postParticipantSet       map[uuid.UUID]bool
	staleParticipantIDs      []uuid.UUID
	claimStatus              voiceLifecycleClaimStatus
	rowsAffected             int64
}

func (s *NATSSubscriber) upsertPrivateVoiceParticipant(
	ctx context.Context,
	conversationID, senderID, callID uuid.UUID,
	eventAt time.Time,
) (applied bool, mutationResult *privateVoiceParticipantUpsertResult, returnErr error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	if s == nil || s.db == nil || s.redis == nil || conversationID == uuid.Nil ||
		senderID == uuid.Nil || callID == uuid.Nil ||
		!presence.IsValidActivitySourceTime(eventAt) {
		return false, nil, errors.New("invalid private voice participant upsert")
	}
	state := &privateVoiceParticipantUpsertState{
		request: privateVoiceParticipantUpsertRequest{
			conversationID: conversationID, senderID: senderID,
			callID: callID, eventAt: eventAt,
		},
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, fmt.Errorf("begin private voice participant upsert: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		returnErr = joinRollbackErr(returnErr, rollbackErr, "rollback private voice participant upsert")
	}()
	rejected, err := s.lockPrivateVoiceParticipantUpsert(ctx, tx, state)
	if err != nil || rejected {
		return false, nil, err
	}
	rejected, err = s.preparePrivateVoiceParticipantUpsertOldScopes(ctx, tx, state)
	if err != nil || rejected {
		return false, nil, err
	}
	if err := s.refreshPrivateVoiceParticipantUpsertLease(ctx, state.request); err != nil {
		return false, nil, err
	}
	rejected, err = s.preparePrivateVoiceParticipantUpsertClaims(ctx, state)
	if err != nil || rejected {
		return false, nil, err
	}
	if err := s.applyPrivateVoiceParticipantUpsert(ctx, tx, state); err != nil {
		return false, nil, err
	}
	if err := s.commitPrivateVoiceParticipantUpsert(ctx, tx, state); err != nil {
		return false, nil, err
	}
	return state.rowsAffected == 1, &privateVoiceParticipantUpsertResult{
		reconnectParticipantIDs: state.staleParticipantIDs,
		oldScopeRevisions:       state.oldScopeRevisions,
		oldScopeBaseDeltas:      privateVoiceMovedScopeBaseDeltas(state.oldScopeMoved),
	}, nil
}

func (s *NATSSubscriber) lockPrivateVoiceParticipantUpsert(
	ctx context.Context,
	tx *sql.Tx,
	state *privateVoiceParticipantUpsertState,
) (bool, error) {
	request := state.request
	participantIDs := []uuid.UUID{request.senderID}
	if err := lockPrivateVoiceScopes(ctx, tx, participantIDs); err != nil {
		return false, err
	}
	memberships, err := s.collectPrivateVoiceScopeMembershipsFrom(ctx, tx, participantIDs)
	if err != nil {
		return false, err
	}
	lockedConversationIDs, err := lockPrivateVoiceParticipantSets(
		ctx, tx, groupedPrivateVoiceConversationIDs(request.conversationID, memberships),
	)
	if err != nil {
		return false, err
	}
	memberships, err = s.collectPrivateVoiceScopeMembershipsFrom(ctx, tx, participantIDs)
	if err != nil {
		return false, err
	}
	if !privateVoiceMembershipsWithinLocks(memberships, lockedConversationIDs) {
		return false, errors.New("private voice scope changed during lock acquisition")
	}
	state.scopeMemberships = memberships
	members, err := s.currentDMVoiceMembers(
		ctx, tx, request.conversationID, participantIDs,
	)
	if err != nil {
		return false, err
	}
	if len(members) == 0 {
		return true, nil
	}
	state.existingRecords, err = s.collectVoiceParticipantRecordsFrom(
		ctx, tx, request.conversationID, true,
	)
	if err != nil {
		return false, err
	}
	state.existingParticipantIDs = voiceParticipantRecordIDs(state.existingRecords)
	if !privateVoiceParticipantUnionWithinLimit(state.existingParticipantIDs, participantIDs) {
		return false, errors.New("private voice participant limit exceeded")
	}
	return false, nil
}

func (s *NATSSubscriber) preparePrivateVoiceParticipantUpsertOldScopes(
	ctx context.Context,
	tx *sql.Tx,
	state *privateVoiceParticipantUpsertState,
) (bool, error) {
	state.oldScopePost = make(map[uuid.UUID][]uuid.UUID)
	state.oldScopeStale = make(map[uuid.UUID][]uuid.UUID)
	state.oldScopeCallIDs = make(map[uuid.UUID]uuid.UUID)
	state.oldScopeMoved = make(map[uuid.UUID]map[uuid.UUID]bool)
	state.seenOldScopes = make(map[uuid.UUID]bool)
	lockedParticipantIDs := append(
		append([]uuid.UUID(nil), state.existingParticipantIDs...), state.request.senderID,
	)
	for _, membership := range state.scopeMemberships {
		conversationID := membership.conversationID
		if conversationID == state.request.conversationID || state.seenOldScopes[conversationID] {
			continue
		}
		state.seenOldScopes[conversationID] = true
		state.oldScopeMoved[conversationID] = map[uuid.UUID]bool{state.request.senderID: true}
		participantIDs, rejected, err := s.preparePrivateVoiceParticipantUpsertOldScope(
			ctx, tx, conversationID, state,
		)
		if err != nil || rejected {
			return rejected, err
		}
		lockedParticipantIDs = append(lockedParticipantIDs, participantIDs...)
	}
	if _, err := lockPrivateVoiceParticipantLifecycles(
		ctx, tx, lockedParticipantIDs,
	); err != nil {
		return false, err
	}
	var err error
	state.oldScopeRevisions, err = s.capturePrivateVoiceOldScopeRevisions(
		ctx, state.oldScopePost, state.oldScopeCallIDs, state.oldScopeMoved,
	)
	return false, err
}

func (s *NATSSubscriber) preparePrivateVoiceParticipantUpsertOldScope(
	ctx context.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
	state *privateVoiceParticipantUpsertState,
) ([]uuid.UUID, bool, error) {
	oldRecords, err := s.collectVoiceParticipantRecordsFrom(ctx, tx, conversationID, true)
	if err != nil {
		return nil, false, err
	}
	candidates, rejected := partitionPrivateVoiceParticipantUpsertOldScope(
		oldRecords, state.request.senderID, state.request.eventAt,
	)
	if rejected {
		return nil, true, nil
	}
	participantIDs := voiceParticipantRecordIDs(oldRecords)
	if len(candidates) == 0 {
		state.oldScopePost[conversationID] = nil
		return participantIDs, false, nil
	}
	lease, found, err := dm.LookupDMVoiceCallLease(ctx, s.redis, conversationID)
	if err != nil {
		return nil, false, err
	}
	if !found || lease.CallID == uuid.Nil {
		state.oldScopePost[conversationID] = nil
		state.reconnectUnknownOldScope = true
		return participantIDs, false, nil
	}
	state.oldScopeCallIDs[conversationID] = lease.CallID
	rejected, err = s.classifyPrivateVoiceParticipantUpsertOldScope(
		ctx, conversationID, candidates, lease.CallID, state,
	)
	return participantIDs, rejected, err
}

func partitionPrivateVoiceParticipantUpsertOldScope(
	records []voiceParticipantRecord,
	senderID uuid.UUID,
	eventAt time.Time,
) ([]voiceParticipantRecord, bool) {
	candidates := make([]voiceParticipantRecord, 0, len(records))
	for _, participant := range records {
		movedSender := participant.userID == senderID
		if movedSender && !participant.lifecycleEventAt.Before(eventAt) {
			return nil, true
		}
		if !movedSender {
			candidates = append(candidates, participant)
		}
	}
	return candidates, false
}

func (s *NATSSubscriber) classifyPrivateVoiceParticipantUpsertOldScope(
	ctx context.Context,
	conversationID uuid.UUID,
	candidates []voiceParticipantRecord,
	callID uuid.UUID,
	state *privateVoiceParticipantUpsertState,
) (bool, error) {
	for _, participant := range candidates {
		if participant.lifecycleEventAt.After(state.request.eventAt) {
			return true, nil
		}
		matches, err := s.matchesActiveVoiceLifecycleToken(
			ctx, presence.CategoryPrivateCall, participant.userID, callID,
		)
		if err != nil {
			return false, err
		}
		if matches {
			state.oldScopePost[conversationID] = append(
				state.oldScopePost[conversationID], participant.userID,
			)
			continue
		}
		state.oldScopeStale[conversationID] = append(
			state.oldScopeStale[conversationID], participant.userID,
		)
	}
	return false, nil
}

func (s *NATSSubscriber) refreshPrivateVoiceParticipantUpsertLease(
	ctx context.Context,
	request privateVoiceParticipantUpsertRequest,
) error {
	if err := s.refreshDMVoiceCallLease(
		ctx, request.conversationID, request.callID.String(), "",
		request.senderID.String(), false,
	); err != nil {
		return fmt.Errorf("refresh private voice join lease: %w", err)
	}
	return s.requireDMVoiceCallLease(ctx, request.conversationID, request.callID)
}

func (s *NATSSubscriber) preparePrivateVoiceParticipantUpsertClaims(
	ctx context.Context,
	state *privateVoiceParticipantUpsertState,
) (bool, error) {
	for _, participant := range state.existingRecords {
		if participant.lifecycleEventAt.After(state.request.eventAt) {
			return true, nil
		}
	}
	state.postParticipantSet = make(
		map[uuid.UUID]bool, len(state.existingParticipantIDs)+1,
	)
	for _, participant := range state.existingRecords {
		if participant.userID == state.request.senderID {
			continue
		}
		matches, err := s.matchesActiveVoiceLifecycleToken(
			ctx, presence.CategoryPrivateCall, participant.userID, state.request.callID,
		)
		if err != nil {
			return false, err
		}
		if !matches {
			continue
		}
		state.postParticipantSet[participant.userID] = true
		state.postParticipantIDs = append(state.postParticipantIDs, participant.userID)
	}
	state.postParticipantSet[state.request.senderID] = true
	state.postParticipantIDs = append(state.postParticipantIDs, state.request.senderID)
	claims, err := privateVoiceParticipantUpsertClaims(state)
	if err != nil {
		return false, err
	}
	state.claimStatus, err = s.claimGroupedPrivateVoiceLifecycles(
		ctx, claims, state.request.eventAt,
	)
	if err != nil {
		return false, err
	}
	return state.claimStatus == voiceLifecycleRejected, nil
}

func privateVoiceParticipantUpsertClaims(
	state *privateVoiceParticipantUpsertState,
) ([]privateVoiceParticipantSetClaim, error) {
	claims := make(map[uuid.UUID]privateVoiceParticipantSetClaim)
	for _, participantID := range state.postParticipantIDs {
		claims[participantID] = privateVoiceParticipantSetClaim{
			userID: participantID, token: state.request.callID,
			version: state.request.eventAt.UnixMicro(), active: true,
		}
	}
	for conversationID, participantIDs := range state.oldScopePost {
		callID, hasCall := state.oldScopeCallIDs[conversationID]
		if len(participantIDs) > 0 && !hasCall {
			return nil, errors.New("old private voice scope call unavailable")
		}
		for _, participantID := range participantIDs {
			if _, conflict := claims[participantID]; conflict {
				return nil, errors.New("conflicting private voice scope participant")
			}
			claims[participantID] = privateVoiceParticipantSetClaim{
				userID: participantID, token: callID,
				version: state.request.eventAt.UnixMicro(), active: true,
			}
		}
	}
	result := make([]privateVoiceParticipantSetClaim, 0, len(claims))
	for _, claim := range claims {
		result = append(result, claim)
	}
	return result, nil
}

func (s *NATSSubscriber) applyPrivateVoiceParticipantUpsert(
	ctx context.Context,
	tx *sql.Tx,
	state *privateVoiceParticipantUpsertState,
) error {
	if err := deletePrivateVoiceParticipantOtherScopes(ctx, tx, state); err != nil {
		return err
	}
	if state.reconnectUnknownOldScope {
		state.staleParticipantIDs = append(
			state.staleParticipantIDs, state.request.senderID,
		)
	}
	deletedOldScope, err := deletePrivateVoiceParticipantOldScopeStale(ctx, tx, state)
	if err != nil {
		return err
	}
	state.staleParticipantIDs = append(state.staleParticipantIDs, deletedOldScope...)
	if state.claimStatus == voiceLifecycleDuplicate && len(state.staleParticipantIDs) == 0 {
		state.staleParticipantIDs = append(
			state.staleParticipantIDs, state.request.senderID,
		)
	}
	deletedTarget, err := deletePrivateVoiceParticipantTargetStale(ctx, tx, state)
	if err != nil {
		return err
	}
	state.staleParticipantIDs = append(state.staleParticipantIDs, deletedTarget...)
	state.rowsAffected, err = upsertPrivateVoiceParticipantRow(ctx, tx, state.request)
	return err
}

func deletePrivateVoiceParticipantOtherScopes(
	ctx context.Context,
	tx *sql.Tx,
	state *privateVoiceParticipantUpsertState,
) error {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM dm_voice_participants
		WHERE user_id = $1 AND conversation_id <> $2
		  AND lifecycle_event_at <= $3
	`, state.request.senderID, state.request.conversationID, state.request.eventAt)
	if err != nil {
		return fmt.Errorf("delete stale private call sender scopes: %w", err)
	}
	removedScopeCount, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read stale private call sender scope delete result: %w", err)
	}
	if removedScopeCount > int64(len(state.seenOldScopes)) {
		return errors.New("stale private call sender scope delete affected unexpected rows")
	}
	return nil
}

func deletePrivateVoiceParticipantOldScopeStale(
	ctx context.Context,
	tx *sql.Tx,
	state *privateVoiceParticipantUpsertState,
) ([]uuid.UUID, error) {
	var deletedParticipantIDs []uuid.UUID
	for conversationID, participantIDs := range state.oldScopeStale {
		for _, participantID := range participantIDs {
			deleted, err := deletePrivateVoiceParticipantRowBounded(
				ctx, tx, conversationID, participantID, state.request.eventAt,
			)
			if err != nil {
				return nil, fmt.Errorf("delete stale old-scope participant: %w", err)
			}
			if deleted {
				deletedParticipantIDs = append(deletedParticipantIDs, participantID)
			}
		}
	}
	return deletedParticipantIDs, nil
}

func deletePrivateVoiceParticipantTargetStale(
	ctx context.Context,
	tx *sql.Tx,
	state *privateVoiceParticipantUpsertState,
) ([]uuid.UUID, error) {
	var deletedParticipantIDs []uuid.UUID
	for _, participantID := range state.existingParticipantIDs {
		if participantID == state.request.senderID || state.postParticipantSet[participantID] {
			continue
		}
		deleted, err := deletePrivateVoiceParticipantRowBounded(
			ctx, tx, state.request.conversationID, participantID, state.request.eventAt,
		)
		if err != nil {
			return nil, fmt.Errorf("delete stale private call participant: %w", err)
		}
		if deleted {
			deletedParticipantIDs = append(deletedParticipantIDs, participantID)
		}
	}
	return deletedParticipantIDs, nil
}

func deletePrivateVoiceParticipantRowBounded(
	ctx context.Context,
	tx *sql.Tx,
	conversationID, participantID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
		  AND lifecycle_event_at <= $3
	`, conversationID, participantID, eventAt)
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rowsAffected > 1 {
		return false, errors.New("private voice participant delete affected multiple rows")
	}
	return rowsAffected == 1, nil
}

func upsertPrivateVoiceParticipantRow(
	ctx context.Context,
	tx *sql.Tx,
	request privateVoiceParticipantUpsertRequest,
) (int64, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO dm_voice_participants AS participant
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (conversation_id, user_id) DO UPDATE
		SET lifecycle_event_at = EXCLUDED.lifecycle_event_at
		WHERE participant.lifecycle_event_at <= EXCLUDED.lifecycle_event_at
	`, request.conversationID, request.senderID, request.eventAt)
	if err != nil {
		return 0, fmt.Errorf("upsert private call participant: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read private call participant upsert result: %w", err)
	}
	if rowsAffected > 1 {
		return 0, fmt.Errorf(
			"private call participant upsert affected %d rows", rowsAffected,
		)
	}
	return rowsAffected, nil
}

func (s *NATSSubscriber) commitPrivateVoiceParticipantUpsert(
	ctx context.Context,
	tx *sql.Tx,
	state *privateVoiceParticipantUpsertState,
) error {
	if err := advancePrivateVoiceParticipantRows(
		ctx, tx, state.request.conversationID,
		state.postParticipantIDs, state.request.eventAt,
	); err != nil {
		return err
	}
	for conversationID, participantIDs := range state.oldScopePost {
		if err := advancePrivateVoiceParticipantRows(
			ctx, tx, conversationID, participantIDs, state.request.eventAt,
		); err != nil {
			return err
		}
		if len(participantIDs) > 0 {
			if err := s.requireDMVoiceCallLease(
				ctx, conversationID, state.oldScopeCallIDs[conversationID],
			); err != nil {
				return err
			}
		}
	}
	if err := s.requireDMVoiceCallLease(
		ctx, state.request.conversationID, state.request.callID,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit private voice participant upsert: %w", err)
	}
	return nil
}

func (s *NATSSubscriber) capturePrivateActivityGenerations(
	ctx context.Context,
	participantIDs []uuid.UUID,
) (map[uuid.UUID]presence.ActivityGeneration, error) {
	store := presence.NewActivityStore(s.redis)
	generations := make(map[uuid.UUID]presence.ActivityGeneration, len(participantIDs))
	for _, participantID := range participantIDs {
		if participantID == uuid.Nil {
			return nil, errors.New("invalid private activity generation participant")
		}
		if _, captured := generations[participantID]; captured {
			continue
		}
		state, found, err := store.Get(ctx, participantID, presence.CategoryPrivateCall)
		if err != nil {
			return nil, fmt.Errorf("capture private activity generation: %w", err)
		}
		if !found {
			continue
		}
		generations[participantID] = presence.ActivityGeneration{
			UserID: participantID, Category: presence.CategoryPrivateCall,
			SourceToken: state.SourceToken, SourceVersion: state.SourceVersion,
		}
	}
	return generations, nil
}

func (s *NATSSubscriber) deleteCapturedPrivateActivityGenerations(
	ctx context.Context,
	participantIDs []uuid.UUID,
	generations map[uuid.UUID]presence.ActivityGeneration,
) error {
	store := presence.NewActivityStore(s.redis)
	var deleteErr error
	for _, participantID := range participantIDs {
		generation, found := generations[participantID]
		if !found {
			continue
		}
		deleted, err := store.CompareAndDelete(
			ctx,
			generation.UserID,
			generation.Category,
			generation.SourceToken,
			generation.SourceVersion,
		)
		if err != nil {
			deleteErr = errors.Join(
				deleteErr,
				fmt.Errorf("delete captured private activity generation: %w", err),
			)
			continue
		}
		if deleted {
			continue
		}
		current, currentFound, inspectErr := store.Get(
			ctx, generation.UserID, generation.Category,
		)
		switch {
		case inspectErr != nil:
			deleteErr = errors.Join(
				deleteErr,
				fmt.Errorf("inspect captured private activity generation: %w", inspectErr),
			)
		case !currentFound:
			deleteErr = errors.Join(
				deleteErr,
				errors.New("captured private activity generation absent after compare-delete miss"),
			)
		case current.SourceVersion <= generation.SourceVersion:
			deleteErr = errors.Join(
				deleteErr,
				errors.New("captured private activity generation has no verified successor"),
			)
		}
	}
	return deleteErr
}

type serverVoiceMutationResult struct {
	applied                bool
	added                  bool
	removedRoomIDs         []uuid.UUID
	removedAudienceUnknown bool
	duplicate              bool
	replayMissing          bool
}

type serverVoiceMutationReplay struct {
	TargetRoomID   string   `json:"target_room_id"`
	Added          bool     `json:"added"`
	RemovedRoomIDs []string `json:"removed_room_ids"`
}

func serverVoiceMutationReplayKey(senderID uuid.UUID, eventAt time.Time) (string, error) {
	if senderID == uuid.Nil || !presence.IsValidActivitySourceTime(eventAt) {
		return "", errors.New("invalid server voice mutation replay identity")
	}
	return fmt.Sprintf(
		"voice:result:server:%s:%d", senderID.String(), eventAt.UnixMicro(),
	), nil
}

func validateServerVoiceMutationReplay(
	replay serverVoiceMutationReplay,
	wantTarget uuid.UUID,
) (serverVoiceMutationResult, error) {
	targetRoomID, err := uuid.Parse(replay.TargetRoomID)
	if err != nil || targetRoomID == uuid.Nil || replay.TargetRoomID != targetRoomID.String() ||
		targetRoomID != wantTarget {
		return serverVoiceMutationResult{}, errors.New("invalid server voice mutation replay target")
	}
	result := serverVoiceMutationResult{applied: true, added: replay.Added, duplicate: true}
	seen := make(map[uuid.UUID]bool, len(replay.RemovedRoomIDs))
	if replay.RemovedRoomIDs == nil || len(replay.RemovedRoomIDs) > maxVoiceReplayRemovedRooms {
		return serverVoiceMutationResult{}, errors.New("invalid server voice mutation replay removals")
	}
	previousRawRoomID := ""
	for _, rawRoomID := range replay.RemovedRoomIDs {
		roomID, parseErr := uuid.Parse(rawRoomID)
		if parseErr != nil || roomID == uuid.Nil || rawRoomID != roomID.String() ||
			roomID == targetRoomID || seen[roomID] ||
			(previousRawRoomID != "" && rawRoomID <= previousRawRoomID) {
			return serverVoiceMutationResult{}, errors.New("invalid server voice mutation replay removal")
		}
		seen[roomID] = true
		previousRawRoomID = rawRoomID
		result.removedRoomIDs = append(result.removedRoomIDs, roomID)
	}
	return result, nil
}

func (s *NATSSubscriber) storeServerVoiceMutationReplay(
	ctx context.Context,
	senderID, targetRoomID uuid.UUID,
	eventAt time.Time,
	result serverVoiceMutationResult,
) error {
	key, err := serverVoiceMutationReplayKey(senderID, eventAt)
	if err != nil {
		return err
	}
	replay := serverVoiceMutationReplay{
		TargetRoomID: targetRoomID.String(),
		Added:        result.added,
		RemovedRoomIDs: make(
			[]string, 0, len(result.removedRoomIDs),
		),
	}
	for _, roomID := range result.removedRoomIDs {
		replay.RemovedRoomIDs = append(replay.RemovedRoomIDs, roomID.String())
	}
	payload, err := json.Marshal(replay)
	if err != nil {
		return fmt.Errorf("encode server voice mutation replay: %w", err)
	}
	stored, err := s.redis.SetNX(ctx, key, payload, presence.ActivityStateTTL).Result()
	if err != nil {
		return fmt.Errorf("store server voice mutation replay: %w", err)
	}
	if !stored {
		return s.deleteServerVoiceMutationReplay(
			ctx, key, errors.New("conflicting server voice mutation replay"),
		)
	}
	return nil
}

func (s *NATSSubscriber) deleteServerVoiceMutationReplay(
	ctx context.Context,
	key string,
	cause error,
) error {
	if deleteErr := s.redis.Del(ctx, key).Err(); deleteErr != nil {
		return errors.Join(
			cause,
			fmt.Errorf("delete malformed server voice mutation replay: %w", deleteErr),
		)
	}
	return cause
}

func (s *NATSSubscriber) loadServerVoiceMutationReplay(
	ctx context.Context,
	senderID, targetRoomID uuid.UUID,
	eventAt time.Time,
) (serverVoiceMutationResult, bool, error) {
	key, err := serverVoiceMutationReplayKey(senderID, eventAt)
	if err != nil {
		return serverVoiceMutationResult{}, false, err
	}
	rawPayload, err := loadServerVoiceMutationReplayScript.Run(
		ctx,
		s.redis,
		[]string{key},
		presence.ActivityStateTTL.Milliseconds(),
		maxVoiceMutationReplayBytes,
	).Result()
	if errors.Is(err, redis.Nil) {
		return serverVoiceMutationResult{}, false, nil
	}
	if err != nil {
		return serverVoiceMutationResult{}, false, errors.New("malformed server voice mutation replay")
	}
	payloadString, ok := rawPayload.(string)
	if !ok {
		return serverVoiceMutationResult{}, false, s.deleteServerVoiceMutationReplay(
			ctx, key, errors.New("malformed server voice mutation replay"),
		)
	}
	payload := []byte(payloadString)
	fields, err := decodeServerVoiceMutationReplayFields(payload)
	if err != nil {
		return serverVoiceMutationResult{}, false, s.deleteServerVoiceMutationReplay(
			ctx, key, errors.New("malformed server voice mutation replay"),
		)
	}
	targetRaw, hasTarget := fields["target_room_id"]
	addedRaw, hasAdded := fields["added"]
	removedRaw, hasRemoved := fields["removed_room_ids"]
	if !hasTarget || !hasAdded || !hasRemoved || bytes.Equal(bytes.TrimSpace(removedRaw), []byte("null")) {
		return serverVoiceMutationResult{}, false, s.deleteServerVoiceMutationReplay(
			ctx, key, errors.New("malformed server voice mutation replay"),
		)
	}
	var replay serverVoiceMutationReplay
	if err := json.Unmarshal(targetRaw, &replay.TargetRoomID); err != nil {
		return serverVoiceMutationResult{}, false, s.deleteServerVoiceMutationReplay(
			ctx, key, errors.New("malformed server voice mutation replay"),
		)
	}
	if err := json.Unmarshal(addedRaw, &replay.Added); err != nil {
		return serverVoiceMutationResult{}, false, s.deleteServerVoiceMutationReplay(
			ctx, key, errors.New("malformed server voice mutation replay"),
		)
	}
	if err := json.Unmarshal(removedRaw, &replay.RemovedRoomIDs); err != nil {
		return serverVoiceMutationResult{}, false, s.deleteServerVoiceMutationReplay(
			ctx, key, errors.New("malformed server voice mutation replay"),
		)
	}
	result, err := validateServerVoiceMutationReplay(replay, targetRoomID)
	if err != nil {
		return serverVoiceMutationResult{}, false, s.deleteServerVoiceMutationReplay(
			ctx, key, err,
		)
	}
	return result, true, nil
}

func decodeServerVoiceMutationReplayFields(
	payload []byte,
) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("server voice mutation replay must be an object")
	}

	fields := make(map[string]json.RawMessage, 3)
	for decoder.More() {
		if err := decodeServerVoiceMutationReplayField(decoder, fields); err != nil {
			return nil, err
		}
	}

	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(fields) != 3 {
		return nil, errors.New("invalid server voice mutation replay object")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing server voice mutation replay data")
	}
	return fields, nil
}

func decodeServerVoiceMutationReplayField(
	decoder *json.Decoder,
	fields map[string]json.RawMessage,
) error {
	rawKey, err := decoder.Token()
	if err != nil {
		return errors.New("invalid server voice mutation replay key")
	}
	field, ok := rawKey.(string)
	if !ok {
		return errors.New("invalid server voice mutation replay key")
	}
	if field != "target_room_id" && field != "added" && field != "removed_room_ids" {
		return errors.New("unexpected server voice mutation replay field")
	}
	if _, duplicate := fields[field]; duplicate {
		return errors.New("duplicate server voice mutation replay field")
	}
	var rawValue json.RawMessage
	if err := decoder.Decode(&rawValue); err != nil {
		return errors.New("invalid server voice mutation replay value")
	}
	fields[field] = rawValue
	return nil
}

func (s *NATSSubscriber) upsertServerVoiceParticipant(
	ctx context.Context,
	channelID, senderID uuid.UUID,
	eventAt time.Time,
) (serverVoiceMutationResult, error) {
	var mutationResult serverVoiceMutationResult
	applied, claimStatus, err := s.withVoiceLifecycleClaimStatus(
		ctx,
		presence.CategoryServerVoice,
		senderID,
		channelID,
		eventAt,
		true,
		func(
			ctx context.Context,
			tx *sql.Tx,
			_ voiceLifecycleClaimStatus,
		) (bool, error) {
			var mutationErr error
			mutationResult, mutationErr = moveServerVoiceParticipant(
				ctx, tx, channelID, senderID, eventAt,
			)
			return mutationResult.applied, mutationErr
		},
	)
	if err != nil {
		return mutationResult, err
	}
	if !applied {
		return mutationResult, nil
	}
	if claimStatus == voiceLifecycleFresh && !mutationResult.removedAudienceUnknown {
		if replayErr := s.storeServerVoiceMutationReplay(
			ctx, senderID, channelID, eventAt, mutationResult,
		); replayErr != nil {
			return mutationResult, replayErr
		}
	}
	if claimStatus != voiceLifecycleDuplicate {
		return mutationResult, nil
	}
	mutationResult.duplicate = true
	replayed, found, replayErr := s.loadServerVoiceMutationReplay(
		ctx, senderID, channelID, eventAt,
	)
	if replayErr != nil {
		return mutationResult, replayErr
	}
	if found {
		return replayed, nil
	}
	mutationResult.replayMissing = true
	return mutationResult, nil
}

func moveServerVoiceParticipant(
	ctx context.Context,
	tx *sql.Tx,
	channelID, senderID uuid.UUID,
	eventAt time.Time,
) (serverVoiceMutationResult, error) {
	var (
		upsertedCount int
		priorTarget   bool
		removedRaw    []string
	)
	err := tx.QueryRowContext(ctx, `
		WITH prior_target AS MATERIALIZED (
			SELECT 1
			FROM voice_participants
			WHERE channel_id = $1 AND user_id = $2
		), blocked AS MATERIALIZED (
			SELECT 1
			FROM voice_participants
			WHERE user_id = $2 AND channel_id <> $1
			  AND lifecycle_event_at >= $3
			LIMIT 1
		), removed AS (
			DELETE FROM voice_participants
			WHERE user_id = $2 AND channel_id <> $1
			  AND lifecycle_event_at < $3
			  AND NOT EXISTS (SELECT 1 FROM blocked)
			RETURNING channel_id
		), upserted AS (
			INSERT INTO voice_participants AS participant
				(channel_id, user_id, joined_at, lifecycle_event_at)
			SELECT $1, $2, $3, $3
			WHERE NOT EXISTS (SELECT 1 FROM blocked)
			ON CONFLICT (channel_id, user_id) DO UPDATE
			SET lifecycle_event_at = EXCLUDED.lifecycle_event_at
			WHERE participant.lifecycle_event_at <= EXCLUDED.lifecycle_event_at
			RETURNING 1
		)
		SELECT
			(SELECT COUNT(*) FROM upserted),
			EXISTS (SELECT 1 FROM prior_target),
			COALESCE(ARRAY(
				SELECT channel_id::text
				FROM removed
				ORDER BY channel_id
				LIMIT $4
			), ARRAY[]::text[])
	`, channelID, senderID, eventAt, maxVoiceReplayRemovedRooms+1).Scan(
		&upsertedCount, &priorTarget, pq.Array(&removedRaw),
	)
	if err != nil {
		return serverVoiceMutationResult{}, fmt.Errorf("move server voice participant: %w", err)
	}
	if upsertedCount > 1 {
		return serverVoiceMutationResult{}, fmt.Errorf(
			"server voice participant upsert affected %d rows", upsertedCount,
		)
	}
	result := serverVoiceMutationResult{
		applied: upsertedCount == 1,
		added:   upsertedCount == 1 && !priorTarget,
	}
	if len(removedRaw) > maxVoiceReplayRemovedRooms {
		result.removedAudienceUnknown = true
		return result, nil
	}
	for _, rawRoomID := range removedRaw {
		roomID, parseErr := uuid.Parse(rawRoomID)
		if parseErr != nil || roomID == uuid.Nil {
			return serverVoiceMutationResult{}, errors.New("invalid removed server voice room")
		}
		result.removedRoomIDs = append(result.removedRoomIDs, roomID)
	}
	return result, nil
}

func (s *NATSSubscriber) currentServerVoiceScope(
	ctx context.Context,
	senderID uuid.UUID,
) (presence.Scope, bool, error) {
	var (
		scope presence.Scope
		count int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT channel_id, lifecycle_event_at, COUNT(*) OVER ()
		FROM voice_participants
		WHERE user_id = $1
		ORDER BY channel_id
		LIMIT 1
	`, senderID).Scan(&scope.RoomID, &scope.EventAt, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return presence.Scope{}, false, nil
	}
	if err != nil {
		return presence.Scope{}, false, fmt.Errorf("query current server voice scope: %w", err)
	}
	if count != 1 {
		return presence.Scope{}, false, errAmbiguousServerVoiceScope
	}
	scope.Category = presence.CategoryServerVoice
	scope.LifecycleID = scope.RoomID
	return scope, true, nil
}

// SetPermissionEnforcer wires the mid-session permission push into the
// voice.joined bridge. Called once at router construction.
func (s *NATSSubscriber) SetPermissionEnforcer(e *PermissionEnforcer) {
	s.permEnforcer = e
}

// SetPresenceRecheck forwards the #2445 Rich Presence capture to the shared
// tempGrantManager, so a voice.left / heartbeat-triggered temporary-SBAC revoke
// captures its pre-mutation Server Voice audience under the same per-server
// advisory lock the RBAC authority writes use.
func (s *NATSSubscriber) SetPresenceRecheck(p rbac.PresenceRecheck) {
	if s.tempGrant == nil {
		return
	}
	s.tempGrant.SetPresenceRecheck(p)
}

// NewNATSSubscriber creates a new NATS subscriber for voice events. The resolver is
// required so the subscriber can drive temporary-SBAC cleanup (#487 P1) on
// voice.left / heartbeat stale-removal through the shared tempGrantManager.
func NewNATSSubscriber(db *sql.DB, log *logger.Logger, hub *websocket.Hub, nats *natsclient.Client, redisClient *redis.Client, resolver *rbac.Resolver, activity *presence.ActivityService) *NATSSubscriber {
	return &NATSSubscriber{
		db:        db,
		log:       log,
		hub:       hub,
		nats:      nats,
		redis:     redisClient,
		tempGrant: newTempGrantManager(db, log, hub, resolver, nats),
		activity:  activity,
		// #2854 B1. Wired here and only here; a nil gate is a silent no-op, so
		// TestNewNATSSubscriberWiresTheIngressGates asserts all three are set.
		// That test proves the gates EXIST. What proves the voice gate is
		// INSTALLED is TestSubscribeInstallsTheVoiceGate -- see Subscribe().
		erasureBudget:   newErasureBudget(),
		erasureSeen:     newErasureSeen(),
		voiceRoomBudget: newVoiceRoomBudget(),
	}
}

func (s *NATSSubscriber) disconnectAllRichPresenceClients() {
	if s.disconnectAllRichPresenceClientsHook != nil {
		s.disconnectAllRichPresenceClientsHook()
		return
	}
	disconnectCtx, cancelDisconnect := context.WithTimeout(
		context.Background(), richPresenceLifecycleTimeout,
	)
	defer cancelDisconnect()
	if s.hub.DisconnectAllRichPresenceClients(disconnectCtx) != nil {
		s.log.Error("Rich Presence conservative disconnect failed",
			"failure_class", "delivery")
	}
}

func (s *NATSSubscriber) handleDMVoiceDependencyFailure(_ string, err error) {
	switch {
	case errors.Is(err, dm.ErrDMVoiceCallLeaseConflict):
		s.log.Warn(logPrivateLifecycleDependencyFailure, "failure_class", "lease_conflict")
		return
	case errors.Is(err, dm.ErrDMVoiceCallLeaseClosed):
		s.log.Warn(logPrivateLifecycleDependencyFailure, "failure_class", "lease_closed")
		return
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		s.log.Error(logPrivateLifecycleDependencyFailure, "failure_class", "deadline")
	default:
		s.log.Error(logPrivateLifecycleDependencyFailure, "failure_class", "dependency")
	}
	s.disconnectAllRichPresenceClients()
}

// voiceJoinedEvent matches the media plane's voice.joined NATS payload.
type voiceJoinedEvent struct {
	ChannelID   string `json:"channelId"`
	CallID      string `json:"callId"`
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName,omitempty"`
	Timestamp   string `json:"timestamp"`
}

// voiceLeftEvent matches the media plane's voice.left NATS payload.
type voiceLeftEvent struct {
	ChannelID string `json:"channelId"`
	CallID    string `json:"callId"`
	UserID    string `json:"userId"`
	Timestamp string `json:"timestamp"`
}

// voiceRoomEmptyEvent matches the media plane's voice.room_empty NATS payload.
type voiceRoomEmptyEvent struct {
	ChannelID          string   `json:"channelId"`
	CallID             string   `json:"callId"`
	RingID             string   `json:"ringId"`
	CallerUserID       string   `json:"callerUserId"`
	ParticipantUserIDs []string `json:"participantUserIds"`
	StartedAt          string   `json:"startedAt"`
	Timestamp          string   `json:"timestamp"`
}

// voiceHeartbeatEvent is the per-room heartbeat from the media plane.
type voiceHeartbeatEvent struct {
	ChannelID    string   `json:"channelId"`
	CallID       string   `json:"callId"`
	RingID       string   `json:"ringId"`
	CallerUserID string   `json:"callerUserId"`
	UserIDs      []string `json:"userIds"`
	Timestamp    string   `json:"timestamp"`
}

func parseVoiceEventTime(raw string) (time.Time, error) {
	eventAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || !presence.IsValidActivitySourceTime(eventAt) {
		return time.Time{}, errors.New("invalid voice lifecycle timestamp")
	}
	return eventAt, nil
}

func completedCallSummaryFromRoomEmpty(
	event voiceRoomEmptyEvent,
	endedAt time.Time,
) (dm.CompletedCallSummary, bool, error) {
	hasSummary := event.CallID != "" || event.CallerUserID != "" ||
		len(event.ParticipantUserIDs) > 0 || event.StartedAt != ""
	if !hasSummary {
		return dm.CompletedCallSummary{}, false, nil
	}
	if len(event.ParticipantUserIDs) > maxPrivateVoiceParticipantIDs {
		return dm.CompletedCallSummary{}, true, errors.New("too many participant user IDs")
	}

	callID, err := uuid.Parse(event.CallID)
	if err != nil || callID == uuid.Nil {
		return dm.CompletedCallSummary{}, true, errors.New("invalid callId")
	}
	callerUserID, err := uuid.Parse(event.CallerUserID)
	if err != nil || callerUserID == uuid.Nil {
		return dm.CompletedCallSummary{}, true, errors.New("invalid callerUserId")
	}
	startedAt, err := time.Parse(time.RFC3339Nano, event.StartedAt)
	if err != nil {
		return dm.CompletedCallSummary{}, true, fmt.Errorf("invalid startedAt: %w", err)
	}

	participants, err := parseCompletedCallParticipants(event.ParticipantUserIDs)
	if err != nil {
		return dm.CompletedCallSummary{}, true, err
	}
	ringID, err := parseOptionalCompletedCallRing(event.RingID)
	if err != nil {
		return dm.CompletedCallSummary{}, true, err
	}

	return dm.CompletedCallSummary{
		CallID:             callID,
		RingID:             ringID,
		CallerUserID:       callerUserID,
		ParticipantUserIDs: participants,
		StartedAt:          startedAt,
		EndedAt:            endedAt,
	}, true, nil
}

func parseCompletedCallParticipants(rawUserIDs []string) ([]uuid.UUID, error) {
	participants := make([]uuid.UUID, 0, len(rawUserIDs))
	seen := make(map[uuid.UUID]struct{}, len(rawUserIDs))
	for _, rawUserID := range rawUserIDs {
		userID, err := uuid.Parse(rawUserID)
		if err != nil || userID == uuid.Nil {
			return nil, errors.New("invalid participant user ID")
		}
		if _, duplicate := seen[userID]; duplicate {
			continue
		}
		seen[userID] = struct{}{}
		participants = append(participants, userID)
	}
	sort.Slice(participants, func(left, right int) bool {
		return participants[left].String() < participants[right].String()
	})
	return participants, nil
}

func parseOptionalCompletedCallRing(rawRingID string) (uuid.UUID, error) {
	if rawRingID == "" {
		return uuid.Nil, nil
	}
	ringID, err := uuid.Parse(rawRingID)
	if err != nil || ringID == uuid.Nil {
		return uuid.Nil, errors.New("invalid ringId")
	}
	return ringID, nil
}

// roomContext holds the resolved context for a voice room.
// Either serverID is set (server channel) or isDM is true (DM conversation).
type roomContext struct {
	isDM       bool
	serverID   string
	serverUUID uuid.UUID
	convUUID   uuid.UUID
}

// resolveRoom performs the dual-lookup: tries channels first, falls back to dm_conversations.
func (s *NATSSubscriber) resolveRoom(
	ctx context.Context,
	channelID string,
) (*roomContext, error) {
	var serverID string
	err := s.db.QueryRowContext(
		ctx, "SELECT server_id FROM channels WHERE id = $1", channelID,
	).Scan(&serverID)
	if err == nil {
		serverUUID, parseErr := uuid.Parse(serverID)
		if parseErr != nil {
			return nil, parseErr
		}
		return &roomContext{isDM: false, serverID: serverID, serverUUID: serverUUID}, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	// Not a server channel — try DM conversation
	var convID string
	err = s.db.QueryRowContext(
		ctx, "SELECT id FROM dm_conversations WHERE id = $1", channelID,
	).Scan(&convID)
	if err != nil {
		return nil, err
	}
	convUUID, parseErr := uuid.Parse(convID)
	if parseErr != nil {
		return nil, parseErr
	}
	return &roomContext{isDM: true, convUUID: convUUID}, nil
}

// Subscribe registers one wildcard lifecycle ingress. The callback only copies
// and dispatches each event; bounded keyed lanes preserve order within a room
// while preventing expensive work for one room from blocking unrelated rooms.
func (s *NATSSubscriber) Subscribe() error {
	s.lifecycleDispatchMu.Lock()
	if s.lifecycleDispatcher != nil {
		s.lifecycleDispatchMu.Unlock()
		return errors.New("voice lifecycle subscriber already started")
	}
	s.lifecycleDispatchMu.Unlock()
	dispatcher := newVoiceLifecycleDispatcher(
		s.handleVoiceLifecycleEvent,
		s.handleVoiceLifecycleDispatchOverflow,
	)
	// #2854 B1: admission control wraps the handler here rather than inside
	// dispatcher.enqueue, which copies the payload as its first statement and
	// computes a SHA-256 room key before taking its lock -- a gate inside it
	// would already have paid both on the reject path.
	voiceSub, err := s.nats.SubscribeWithSubject(
		natsSubjectVoiceWildcard, s.gateVoiceLifecycle(dispatcher.enqueue))
	if err != nil {
		dispatcher.close()
		return err
	}
	s.lifecycleDispatchMu.Lock()
	s.lifecycleDispatcher = dispatcher
	s.lifecycleDispatchMu.Unlock()

	// #2447: the cross-replica Custom Status clear for an erased account. This
	// subscriber is the only place in the tree that already holds both a NATS
	// connection and the Hub, so it carries the fan-out even though the event is
	// not a voice event. The alternative -- giving the Hub its own NATS
	// connection -- would add a transport to a component that deliberately has
	// none.
	if _, err := s.nats.Subscribe(
		users.NATSSubjectPresenceErasureCleared, s.handlePresenceErasureCleared,
	); err != nil {
		s.unwindLifecycleSubscription(voiceSub, dispatcher)
		return fmt.Errorf("subscribe presence erasure clear: %w", err)
	}

	s.log.Info("Subscribed to voice NATS events")
	return nil
}

// erasedAccountStillExists reports whether the named account is STILL present,
// which is the one case a clear provably must not act on.
//
// Defence in depth, not the barrier — "absent" is true of every id that was
// never a user, so this cannot distinguish a forged id from an erased one.
//
// The barrier is the INGRESS BUDGET, not the inertness of the action (#2854
// stage B1). A single admitted clear is inert for any client that never held
// the sender's status, but that is a PER-CLIENT property and it does not hold
// in aggregate: every admitted clear costs one enqueue per Rich Presence client
// on this replica, under the hub's read lock. Unmetered, that is what a forged
// flood was buying. This comment previously named the action's inertness as the
// barrier, which was true when written and became false once #2855 made the
// fan-out the cost centre.
//
// A lookup error therefore still reports "not present" and lets the clear
// proceed: an unreachable database must not suppress a legitimate erasure
// clear, which has no TTL and converges only at the viewer's next
// presence_snapshot. The budget is what makes that arm affordable.
// The second return reports whether the answer was actually VERIFIED. It is not
// decoration: the proceed-on-read-error arm returns "not present" without having
// established anything, and the caller must not treat an unverified accept as
// grounds to occupy that principal's dedup slot.
//
// Without the distinction, a forged clear naming a LIVE user, sent during a
// database blip, took the fail-open arm, was accepted, and MARKED that user --
// so their genuine clear, issued after their real erasure minutes later, was
// deduped away. That is the exact hazard the mark-on-accept-only rule exists to
// prevent, reached through the accept arm rather than a reject arm (#2854 B1
// adversarial pass, finding C6).
func (s *NATSSubscriber) erasedAccountStillExists(erased uuid.UUID) (stillExists, verified bool) {
	checkCtx, cancelCheck := context.WithTimeout(
		context.Background(), richPresenceLifecycleTimeout)
	defer cancelCheck()

	if s.erasureExistenceProbedHook != nil {
		s.erasureExistenceProbedHook()
	}

	queryErr := s.db.QueryRowContext(checkCtx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, erased,
	).Scan(&stillExists)
	if queryErr != nil {
		// Report WHY the probe failed, not merely that it did. This is the
		// fail-open arm on a right-to-erasure route, and verified=false disables
		// dedup for the message, so this line is the only signal that
		// verification stopped working -- an operator who cannot tell a deadline
		// from an unavailable pool has two different pages collapsed into one.
		//
		// Written as three statements with LITERAL failure_class values rather
		// than one call to a classifier helper. nats_rich_presence_log_guard_test
		// walks this file's AST and requires failure_class to be a literal (or
		// the sanctioned PolicyErrorClass helper), and it caught the helper form
		// immediately. Fitting the guard is the right direction; widening a
		// closed-vocabulary guard to accommodate new code is not.
		//
		// The raw driver text is deliberately NOT logged: a driver error can echo
		// a parameter value, and this is a privacy path.
		switch {
		case errors.Is(queryErr, context.DeadlineExceeded):
			s.log.Error(msgErasureProbeUnconfirmed, "failure_class", "lookup_deadline")
		case errors.Is(queryErr, sql.ErrConnDone):
			s.log.Error(msgErasureProbeUnconfirmed, "failure_class", "lookup_unavailable")
		default:
			s.log.Error(msgErasureProbeUnconfirmed, "failure_class", "lookup")
		}
		return false, false
	}
	return stillExists, true
}

// unsubscriber is the one method unwindLifecycleSubscription needs from a
// subscription. An interface rather than *nats.Subscription so this file does
// not take a direct nats.go dependency purely to name a parameter.
type unsubscriber interface{ Unsubscribe() error }

// unwindLifecycleSubscription reverses what Subscribe already established, so a
// later failure in the same call does not leave the dispatcher goroutine and the
// wildcard subscription live.
//
// Extracted so it can be tested directly: reaching it through Subscribe would
// need the erasure subscription to fail while the wildcard one succeeded, and
// s.nats is a concrete client with no seam to make one call fail and not the
// other. Without this the leak fix would ship unexercised (CodeRabbit, PR #2840).
func (s *NATSSubscriber) unwindLifecycleSubscription(
	voiceSub unsubscriber, dispatcher *voiceLifecycleDispatcher,
) {
	// A typed-nil *nats.Subscription is non-nil as an interface, so guard the
	// call rather than the value — Unsubscribe on a nil subscription errors
	// harmlessly, which is why the result is discarded.
	if voiceSub != nil {
		_ = voiceSub.Unsubscribe()
	}
	s.lifecycleDispatchMu.Lock()
	s.lifecycleDispatcher = nil
	s.lifecycleDispatchMu.Unlock()
	if dispatcher != nil {
		dispatcher.close()
	}
}

// handlePresenceErasureCleared retracts an erased principal's already-delivered
// Custom Status on THIS replica, as a targeted clear naming that one sender.
//
// It does NOT disconnect. An earlier revision of this comment said it "reuses
// the conservative disconnect rather than a targeted clear" and proposed
// carrying the affected viewer set as a future upgrade path — both were true
// when written, both were made false by #2840, and the comment survived the
// change describing behaviour the function no longer had. It had also lost its
// blank-line separator, so godoc rendered it as erasedAccountStillExists's doc
// and this function had none. Reconciled in #2854 stage B1.
//
// Admission ordering is load-bearing and is asserted by tests, not merely
// described here: dedup, then budget, then the nil-database refusal, then the
// existence check, and only then the dedup mark. See the comments at each step.
func (s *NATSSubscriber) handlePresenceErasureCleared(data []byte) {
	var payload struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		// Aggregated, not per message. These two arms sit AHEAD of the dedup and
		// budget gates below -- they must, since neither has a key to work with
		// until the payload parses -- so a per-message log here is reachable by
		// simply sending invalid JSON, and would be the amplification primitive
		// the gate exists to remove (#2854 B1 adversarial pass, finding C3).
		s.erasureShed("malformed_payload")
		return
	}
	erased, err := uuid.Parse(payload.UserID)
	if err != nil {
		s.erasureShed("invalid_user") // aggregated -- see the note above
		return
	}

	// Ingress admission (#2854 B1). DEDUP BEFORE BUDGET, deliberately.
	//
	// The cheapest high-amplification attack is replaying one captured message.
	// Rejecting it here costs the attacker everything and costs the budget
	// nothing, so a genuine clear arriving during a same-UUID flood still has
	// budget to spend. Budget-first starves it -- which is why the ordering is
	// locked by TestAFloodOfOneUUIDDoesNotStarveAGenuineClearForAnother rather
	// than merely commented.
	//
	// Both also sit ahead of the database round-trip below, so a replay costs
	// zero queries.
	if s.erasureSeen.Seen(erased.String()) {
		s.erasureShed("replay")
		return
	}
	if !s.erasureBudget.Allow() {
		s.erasureShed("ingress_budget")
		return
	}

	// Confirm the account is actually gone before acting.
	//
	// A UUID check alone is only syntactic, and this handler's blast radius is
	// every Rich Presence client on this replica. NATS carries no per-service
	// authorization in this deployment (the media-plane, which terminates
	// untrusted WebRTC traffic, shares the same bus), so a syntactically valid
	// message from anywhere on that network would otherwise be an arbitrarily
	// repeatable fleet-wide session-teardown primitive. The users row is the
	// authoritative post-erasure signal, and it makes a forged or replayed
	// message a no-op (security review, PR #2840; also raised by Gitar and the
	// RBAC review).
	//
	// TWO DIFFERENT QUESTIONS, TWO DIFFERENT ANSWERS. This block previously
	// stated both in adjacent paragraphs using "open" and "closed" in opposite
	// senses, which read as a self-contradiction; reconciled in #2854 stage B1.
	//
	// MOMENTARILY unverifiable — a read error — PROCEEDS. Under
	// [internal]rules/backend.md's "unknown state fails closed; proven no-change
	// must not", the conservative direction on a REVOCATION path is to act. A
	// database blip must not silently drop a genuine erasure clear: Custom
	// Status carries no TTL and is not republished on a heartbeat, so a dropped
	// clear persists until that viewer's next presence_snapshot, which for a
	// long-lived socket can be an entire session.
	//
	// PERMANENTLY unverifiable — no database wired at all — REFUSES. That is a
	// wiring fault rather than a transient one, and its correct response is a
	// loud refusal rather than an unbounded stream of unauthorizable clears
	// (code review, PR #2840).
	if s.db == nil {
		s.log.Error(msgErasureClearRejected, "failure_class", "unverifiable")
		return
	}

	stillExists, verified := s.erasedAccountStillExists(erased)
	if stillExists {
		s.log.Error(msgErasureClearRejected, "failure_class", "user_not_erased")
		return
	}

	// A targeted CLEAR for this one sender, not a fleet-wide disconnect.
	//
	// The disconnect was a denial-of-service primitive: red-team PoCs on PR #2840
	// proved a forged publish carrying ANY random UUID reached it (the existence
	// check accepts "absent", and every id that was never a user is absent), that
	// a genuine message could be replayed without bound, and that a lookup error
	// fell open into it. Making the action proportional to the claim removes all
	// three at once — a client that never held this sender's status ignores the
	// frame, so a forged or replayed clear is inert rather than destructive.
	//
	// The existence check above is retained as defence in depth, not as the
	// barrier: it rejects the one case it genuinely proves wrong, a clear naming
	// an account that still exists.
	// Mark ONLY here, on the accept path (#2854 B1). Marking on any rejection
	// arm would let a forged clear naming a still-existing user occupy that
	// user's dedup slot and suppress their genuine clear after the real erasure.
	// Locked by TestARejectedClearDoesNotPoisonTheDedupSlot.
	//
	// This is why ingressbudget.Window splits Seen from Mark: a combined
	// SeenOrMark would make the invariant inexpressible at the type level.
	// Mark only on a VERIFIED accept. The proceed-on-read-error arm above
	// accepts without having established anything, and marking there would let a
	// forged clear naming a LIVE user occupy that user's slot during a database
	// blip and suppress their genuine clear after the real erasure (finding C6).
	// An unverified accept therefore still clears -- that posture is deliberate
	// and unchanged -- it simply buys no dedup credit.
	if verified {
		s.erasureSeen.Mark(erased.String())
	}

	if s.clearErasedSenderHook != nil {
		s.clearErasedSenderHook(erased)
		return
	}
	// Kept for its diagnostic value only: it tells an operator a clear was
	// dropped on a hub-less replica, which would otherwise be silent on a privacy
	// path. It is NOT load-bearing for safety — ClearErasedSenderCustomText fails
	// closed on a nil receiver already.
	//
	// Deliberately untested, and stated rather than covered by a test that cannot
	// fail: with the Hub's own nil guard in place, removing this changes nothing
	// observable except the log line, so a NotPanics assertion here passed with
	// the guard deleted. That test was removed rather than kept as coverage.
	if s.hub == nil {
		s.log.Error("Presence erasure clear skipped", "failure_class", "no_hub")
		return
	}
	s.hub.ClearErasedSenderCustomText(erased)
}

// handleVoiceLifecycleDispatchOverflow reports dispatcher drops. It does NOT
// disconnect.
//
// The drop happens inside enqueue, BEFORE any handler runs, so nothing is
// partially applied: client and server state are identical and both stale by
// the same event. A disconnected client would reconnect and rebuild from
// voice_participants (still holding the phantom row) and the Redis activity
// store (TTL not yet expired), re-reading exactly the state it already had --
// a no-op with a DoS attached.
//
// Keeping the teardown for the terminal class was considered and rejected:
// voice.room_empty is forgeable on the unauthenticated bus and is NOT metered
// by G2 (voiceIngressMeteredSubjects is heartbeat only), so that would
// concentrate a fleet-disconnect primitive into the one subject an attacker can
// forge freely and no budget bounds. See #2868 spec section 2, R1.
//
// Reporting is delegated to voiceDropShed rather than emitted here: the whole
// payload of an aggregated report is a class name and a running total, and
// nats_rich_presence_log_guard_test admits neither as a value in this file.
func (s *NATSSubscriber) handleVoiceLifecycleDispatchOverflow(
	counts voiceLifecycleDropCounts,
) {
	for class := range voiceLifecycleDropClassCount {
		if counts[class] == 0 {
			continue
		}
		s.voiceDropShed(voiceLifecycleDropClassName(class), counts[class])
	}
}

// voiceLifecycleDropClassName returns the failure_class literal for a class.
// The three names MUST stay string literals here -- [internal]rules/backend.md
// pins failure_class as a CLOSED vocabulary whose values carry alerting meaning,
// so a name assembled by concatenation or fmt.Sprintf is not greppable and not
// alertable.
func voiceLifecycleDropClassName(class voiceLifecycleDropClass) string {
	switch class {
	case voiceLifecycleDropTerminal:
		return "dispatch_drop_terminal"
	case voiceLifecycleDropUnresolvable:
		return "dispatch_drop_unresolvable"
	default:
		return "dispatch_drop_convergent"
	}
}

// Close drains and joins the lifecycle dispatcher before its NATS, database,
// Redis, Hub, or ActivityService dependencies are torn down.
func (s *NATSSubscriber) Close() {
	if s == nil {
		return
	}
	s.lifecycleDispatchMu.Lock()
	dispatcher := s.lifecycleDispatcher
	s.lifecycleDispatcher = nil
	s.lifecycleDispatchMu.Unlock()
	if dispatcher != nil {
		dispatcher.close()
	}
}

func (s *NATSSubscriber) handleVoiceLifecycleEvent(subject string, data []byte) {
	switch subject {
	case natsSubjectVoiceJoined:
		s.handleJoined(data)
	case natsSubjectVoiceLeft:
		s.handleLeft(data)
	case natsSubjectVoiceRoomEmpty:
		s.handleRoomEmpty(data)
	case natsSubjectVoiceHeartbeat:
		s.handleHeartbeat(data)
	}
}

func parseDMVoiceCallLifecycleID(rawID, label string) (uuid.UUID, error) {
	id, err := uuid.Parse(rawID)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: invalid %s ID", errInvalidDMVoiceCallLifecycle, label)
	}
	return id, nil
}

func parseOptionalDMVoiceCallLifecycleID(rawID, label string) (uuid.UUID, error) {
	if rawID == "" {
		return uuid.Nil, nil
	}
	return parseDMVoiceCallLifecycleID(rawID, label)
}

func (s *NATSSubscriber) refreshDMVoiceCallLease(
	ctx context.Context,
	conversationID uuid.UUID,
	rawCallID, rawRingID, rawCallerUserID string,
	authoritativeMetadata bool,
) error {
	callID, err := parseDMVoiceCallLifecycleID(rawCallID, "call")
	if err != nil {
		return err
	}
	ringID, err := parseOptionalDMVoiceCallLifecycleID(rawRingID, "ring")
	if err != nil {
		return err
	}
	callerUserID, err := parseOptionalDMVoiceCallLifecycleID(rawCallerUserID, "caller")
	if err != nil {
		return err
	}

	existing, hasLease, lookupErr := dm.LookupDMVoiceCallLease(ctx, s.redis, conversationID)
	if lookupErr != nil {
		return lookupErr
	}
	// /ring and media lifecycle claims share LockDMCallLifecycle. Once /ring
	// publishes a new pending ring, an old room whose lease expired must not be
	// allowed to reclaim the empty shared slot on its next joined/heartbeat.
	if !hasLease && dm.HasLocalPendingDMCall(conversationID) {
		return fmt.Errorf("%w: pending ring owns conversation", errInvalidDMVoiceCallLifecycle)
	}
	if callerUserID == uuid.Nil && hasLease && existing.CallID == callID {
		callerUserID = existing.CallerUserID
		if ringID == uuid.Nil {
			ringID = existing.RingID
		}
	}
	if callerUserID == uuid.Nil {
		return fmt.Errorf("%w: missing caller ID", errInvalidDMVoiceCallLifecycle)
	}

	return dm.RefreshDMVoiceCallLease(ctx, s.redis, dm.VoiceCallLease{
		ConversationID: conversationID,
		CallID:         callID,
		RingID:         ringID,
		CallerUserID:   callerUserID,
	}, dm.DMVoiceCallLeaseTTL, authoritativeMetadata)
}

// dmCallEventMayMutateLiveState checks whether an exact media lifecycle event
// can change conversation-wide live presence. Exact events are accepted when no
// newer lease or pending ring exists, or when they match the current lease's
// call ID. An ID-less legacy event is accepted only after the shared lease and
// local pending ring are gone; otherwise there is no safe way to correlate it.
//
// Callers must hold dm.LockDMCallLifecycle for the conversation so a local
// authorize/ring transition cannot appear between this check and DB/WS effects.
func (s *NATSSubscriber) dmCallEventMayMutateLiveState(
	ctx context.Context,
	conversationID uuid.UUID,
	rawCallID string,
) (bool, error) {
	lease, hasLease, err := dm.LookupDMVoiceCallLease(ctx, s.redis, conversationID)
	if err != nil {
		return false, err
	}
	if !hasLease && dm.HasLocalPendingDMCall(conversationID) {
		return false, nil
	}
	if rawCallID == "" {
		return !hasLease, nil
	}
	callID, err := uuid.Parse(rawCallID)
	if err != nil || callID == uuid.Nil {
		return false, fmt.Errorf("%w: invalid call ID", errInvalidDMVoiceCallLifecycle)
	}
	return !hasLease || lease.CallID == callID, nil
}

func (s *NATSSubscriber) handleJoined(data []byte) {
	var event voiceJoinedEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Rejected voice.joined", "failure_class", "invalid_event")
		return
	}
	eventAt, err := parseVoiceEventTime(event.Timestamp)
	if err != nil {
		s.log.Error("Rejected voice.joined with invalid timestamp")
		return
	}

	activityCtx, cancelActivity := context.WithTimeout(
		context.Background(), richPresenceLifecycleTimeout,
	)
	defer cancelActivity()
	activityCtx = presence.WithActivityBuildCache(activityCtx)
	ctx, err := s.resolveRoom(activityCtx, event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.joined", "failure_class", "state_read")
		return
	}

	if !s.handleResolvedJoined(activityCtx, event, ctx, eventAt) {
		return
	}
	s.log.Info(logVoiceLifecycleApplied, "action", "joined", "is_dm", ctx.isDM)
	if !ctx.isDM {
		s.hub.BroadcastServerVoiceCounts()
	}
}

func (s *NATSSubscriber) handleResolvedJoined(
	ctx context.Context,
	event voiceJoinedEvent,
	room *roomContext,
	eventAt time.Time,
) bool {
	if room.isDM {
		unlockLifecycle := dm.LockDMCallLifecycle(room.convUUID)
		defer unlockLifecycle()
		return s.handlePrivateVoiceJoined(ctx, event, room.convUUID, eventAt)
	}
	return s.handleServerVoiceJoined(ctx, event, room, eventAt)
}

type privateVoiceJoinMutation struct {
	subscriber              *NATSSubscriber
	conversationID          uuid.UUID
	senderID                uuid.UUID
	callID                  uuid.UUID
	eventAt                 time.Time
	event                   voiceJoinedEvent
	capturedGenerations     map[uuid.UUID]presence.ActivityGeneration
	applied                 bool
	durablyApplied          bool
	baseBroadcasted         bool
	reconnectParticipantIDs []uuid.UUID
	oldScopeRevisions       []privateVoiceOldScopeRevision
	oldScopeBaseDeltas      []privateVoiceScopeBaseDelta
}

func (mutation *privateVoiceJoinMutation) apply(ctx context.Context) (bool, error) {
	applied, result, err := mutation.subscriber.upsertPrivateVoiceParticipant(
		ctx, mutation.conversationID, mutation.senderID, mutation.callID, mutation.eventAt,
	)
	mutation.durablyApplied = applied
	if result != nil {
		mutation.reconnectParticipantIDs = append(
			mutation.reconnectParticipantIDs[:0], result.reconnectParticipantIDs...,
		)
		mutation.oldScopeRevisions = result.oldScopeRevisions
		mutation.oldScopeBaseDeltas = result.oldScopeBaseDeltas
	}
	if mutation.durablyApplied {
		// Preserve the pre-existing group-DM hard-moderation convergence after
		// the participant row has been committed.
		mutation.subscriber.reEnforceDM(
			ctx, mutation.conversationID.String(), mutation.senderID.String(),
		)
		mutation.subscriber.broadcastPrivateVoiceOldScopeLeaves(
			mutation.oldScopeBaseDeltas,
		)
		mutation.subscriber.broadcastPrivateVoiceJoined(
			mutation.event, mutation.conversationID, mutation.senderID,
		)
		mutation.baseBroadcasted = true
	}
	if err == nil && applied {
		presence.InvalidateActivityBuildCache(ctx)
		postParticipantIDs, collectErr := mutation.subscriber.collectVoiceParticipantIDs(
			ctx, mutation.conversationID, true,
		)
		if collectErr != nil {
			return false, fmt.Errorf("collect post-join private participants: %w", collectErr)
		}
		if deleteErr := mutation.subscriber.deleteCapturedPrivateActivityGenerations(
			ctx, postParticipantIDs, mutation.capturedGenerations,
		); deleteErr != nil {
			return false, fmt.Errorf("delete advanced private activity generations: %w", deleteErr)
		}
	}
	mutation.applied = applied
	return applied, err
}

func (s *NATSSubscriber) handlePrivateVoiceJoined(
	ctx context.Context,
	event voiceJoinedEvent,
	conversationID uuid.UUID,
	eventAt time.Time,
) bool {
	mutation, prepared := s.preparePrivateVoiceJoin(ctx, event, conversationID, eventAt)
	if !prepared {
		return false
	}
	if s.privateJoinBeforeMutationHook != nil {
		s.privateJoinBeforeMutationHook(conversationID, mutation.senderID)
	}
	activityErr := s.activity.RefreshPrivateCall(
		ctx,
		mutation.senderID,
		presence.Scope{
			Category: presence.CategoryPrivateCall, RoomID: conversationID,
			LifecycleID: mutation.callID, EventAt: eventAt,
		},
		nil,
		mutation.apply,
	)
	if mutation.durablyApplied {
		if !mutation.baseBroadcasted {
			s.broadcastPrivateVoiceJoined(event, conversationID, mutation.senderID)
		}
	}
	if activityErr != nil {
		s.log.Error("Private Call Rich Presence refresh failed",
			"failure_class", presence.PolicyErrorClass(activityErr))
		s.disconnectAllRichPresenceClients()
		return mutation.durablyApplied
	}
	if !mutation.applied {
		return mutation.durablyApplied
	}
	if !s.finishPrivateVoiceJoin(ctx, mutation) {
		return true
	}
	return true
}

func (s *NATSSubscriber) broadcastPrivateVoiceJoined(
	event voiceJoinedEvent,
	conversationID, senderID uuid.UUID,
) {
	if s.privateJoinBroadcastHook != nil {
		s.privateJoinBroadcastHook(conversationID, senderID)
	}
	if s.privateVoiceStateBroadcastHook != nil {
		s.privateVoiceStateBroadcastHook(conversationID, senderID, "joined")
	}
	s.hub.BroadcastToDMParticipants(conversationID, websocket.OutgoingMessage{
		Type: "dm_voice_state_update",
		Data: map[string]interface{}{
			"conversation_id": event.ChannelID,
			"user_id":         event.UserID,
			"username":        event.Username,
			"display_name":    event.DisplayName,
			"action":          "joined",
		},
	})
}

func (s *NATSSubscriber) preparePrivateVoiceJoin(
	ctx context.Context,
	event voiceJoinedEvent,
	conversationID uuid.UUID,
	eventAt time.Time,
) (*privateVoiceJoinMutation, bool) {
	senderID, err := uuid.Parse(event.UserID)
	if err != nil || senderID == uuid.Nil {
		s.log.Error("Rejected private voice.joined with invalid sender")
		return nil, false
	}
	callID, err := parseDMVoiceCallLifecycleID(event.CallID, "call")
	if err != nil {
		s.log.Error("Rejected private voice.joined with invalid call")
		return nil, false
	}
	if s.activity == nil {
		s.log.Error(logPrivateBridgeUnavailable)
		return nil, false
	}
	mayMutate, err := s.dmCallEventMayMutateLiveState(ctx, conversationID, event.CallID)
	if err != nil {
		s.handleDMVoiceDependencyFailure("Private Call join ownership validation failed", err)
		return nil, false
	}
	if !mayMutate {
		s.log.Warn("Ignored stale Private Call join", "failure_class", "stale_event")
		return nil, false
	}
	participantIDs, err := s.collectVoiceParticipantIDs(ctx, conversationID, true)
	if err != nil || !privateVoiceParticipantUnionWithinLimit(participantIDs, []uuid.UUID{senderID}) {
		s.log.Error(logPrivatePreMutationReadFailure, "failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return nil, false
	}
	participantIDs = append(participantIDs, senderID)
	capturedGenerations, err := s.capturePrivateActivityGenerations(ctx, participantIDs)
	if err != nil {
		s.log.Error("Private Call pre-mutation activity read failed",
			"failure_class", presence.PolicyErrorClass(err))
		s.disconnectAllRichPresenceClients()
		return nil, false
	}
	return &privateVoiceJoinMutation{
		subscriber: s, conversationID: conversationID,
		senderID: senderID, callID: callID, eventAt: eventAt, event: event,
		capturedGenerations: capturedGenerations,
	}, true
}

func (s *NATSSubscriber) finishPrivateVoiceJoin(
	ctx context.Context,
	mutation *privateVoiceJoinMutation,
) bool {
	if err := s.refreshPrivateVoiceOldScopeRevisions(
		ctx, mutation.oldScopeRevisions, mutation.eventAt,
	); err != nil {
		s.log.Error("Private Call old-scope Rich Presence refresh failed",
			"failure_class", presence.PolicyErrorClass(err))
		s.disconnectAllRichPresenceClients()
		return false
	}
	if len(mutation.reconnectParticipantIDs) > 0 {
		s.disconnectAllRichPresenceClients()
	}
	s.refreshPrivateVoicePeers(
		ctx, mutation.conversationID, mutation.callID, mutation.eventAt,
		mutation.senderID, nil,
	)
	return true
}

func (s *NATSSubscriber) refreshPrivateVoicePeers(
	ctx context.Context,
	conversationID, callID uuid.UUID,
	eventAt time.Time,
	skipParticipantID uuid.UUID,
	removedParticipants map[uuid.UUID]bool,
) {
	participantIDs, err := s.collectVoiceParticipantIDs(ctx, conversationID, true)
	if err != nil {
		s.log.Error(logPrivateParticipantRefreshFailure, "failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return
	}
	for _, participantID := range participantIDs {
		if participantID == skipParticipantID {
			continue
		}
		if refreshErr := s.activity.RefreshPrivateCall(
			ctx,
			participantID,
			presence.Scope{
				Category: presence.CategoryPrivateCall, RoomID: conversationID,
				LifecycleID: callID, EventAt: eventAt,
			},
			removedParticipants,
			nil,
		); refreshErr != nil {
			s.log.Error(logPrivateParticipantRefreshFailure,
				"failure_class", presence.PolicyErrorClass(refreshErr))
			s.disconnectAllRichPresenceClients()
		}
	}
}

func (s *NATSSubscriber) handleServerVoiceJoined(
	ctx context.Context,
	event voiceJoinedEvent,
	room *roomContext,
	eventAt time.Time,
) bool {
	senderID, err := uuid.Parse(event.UserID)
	if err != nil || senderID == uuid.Nil {
		s.log.Error("Rejected voice.joined with invalid sender")
		return false
	}
	channelID, err := uuid.Parse(event.ChannelID)
	if err != nil || channelID == uuid.Nil {
		s.log.Error("Rejected voice.joined with invalid room")
		return false
	}
	s.convergeServerVoiceParticipant(ctx, room, channelID, senderID)
	if s.activity == nil {
		s.log.Error(logServerBridgeUnavailable)
		return false
	}
	if !s.serverVoiceJoinHasCapacity(ctx, channelID, senderID) {
		return false
	}
	oldScope, hasOldScope, scopeErr := s.currentServerVoiceScope(ctx, senderID)
	ambiguousOldScope := errors.Is(scopeErr, errAmbiguousServerVoiceScope)
	if scopeErr != nil && !ambiguousOldScope {
		s.log.Error("Server voice Rich Presence prior-state read failed", "failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return false
	}
	if ambiguousOldScope {
		s.disconnectAllRichPresenceClients()
		hasOldScope = false
	}
	result, activityErr := s.applyServerHeartbeatParticipant(
		ctx, channelID, senderID, eventAt, oldScope, hasOldScope,
	)
	if activityErr != nil {
		s.log.Error("Server voice Rich Presence refresh failed",
			"failure_class", presence.PolicyErrorClass(activityErr))
		s.disconnectAllRichPresenceClients()
	}
	if serverHeartbeatMutationNeedsReconnect(
		result, channelID, oldScope, hasOldScope,
	) {
		s.disconnectAllRichPresenceClients()
	}
	if !result.applied {
		return false
	}
	s.convergeServerVoiceParticipant(ctx, room, channelID, senderID)
	s.broadcastRemovedServerVoiceRooms(ctx, senderID, result.removedRoomIDs)
	s.hub.BroadcastToServer(room.serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"channel_id": event.ChannelID, "user_id": event.UserID,
			"username": event.Username, "display_name": event.DisplayName,
			"action": "joined", "server_id": room.serverID,
		},
	})
	return true
}

func (s *NATSSubscriber) serverVoiceJoinHasCapacity(
	ctx context.Context,
	channelID, senderID uuid.UUID,
) bool {
	participantIDs, err := s.collectVoiceParticipantIDs(ctx, channelID, false)
	if err != nil {
		s.log.Error("Server voice Rich Presence pre-join participant read failed",
			"failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return false
	}
	for _, participantID := range participantIDs {
		if participantID == senderID {
			return true
		}
	}
	if len(participantIDs) >= maxServerVoiceParticipantIDs {
		s.log.Warn("Deferred server voice join at participant limit",
			"failure_class", "capacity")
		s.disconnectAllRichPresenceClients()
		return false
	}
	return true
}

func (s *NATSSubscriber) handleLeft(data []byte) {
	var event voiceLeftEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Rejected voice.left", "failure_class", "invalid_event")
		return
	}
	eventAt, err := parseVoiceEventTime(event.Timestamp)
	if err != nil {
		s.log.Error("Rejected voice.left with invalid timestamp")
		return
	}

	activityCtx, cancelActivity := context.WithTimeout(
		context.Background(), richPresenceLifecycleTimeout,
	)
	defer cancelActivity()
	activityCtx = presence.WithActivityBuildCache(activityCtx)
	ctx, err := s.resolveRoom(activityCtx, event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.left", "failure_class", "state_read")
		return
	}

	if !s.handleResolvedLeft(activityCtx, event, ctx, eventAt) {
		return
	}
	s.log.Info(logVoiceLifecycleApplied, "action", "left", "is_dm", ctx.isDM)
	if !ctx.isDM {
		s.hub.BroadcastServerVoiceCounts()
	}
}

func (s *NATSSubscriber) handleResolvedLeft(
	ctx context.Context,
	event voiceLeftEvent,
	room *roomContext,
	eventAt time.Time,
) bool {
	if room.isDM {
		unlockLifecycle := dm.LockDMCallLifecycle(room.convUUID)
		defer unlockLifecycle()
		return s.handlePrivateVoiceLeft(ctx, event, room.convUUID, eventAt)
	}
	return s.handleServerVoiceLeft(ctx, event, room, eventAt)
}

type privateVoiceLeaveMutation struct {
	subscriber          *NATSSubscriber
	conversationID      uuid.UUID
	senderID            uuid.UUID
	callID              uuid.UUID
	eventAt             time.Time
	capturedGenerations map[uuid.UUID]presence.ActivityGeneration
	applied             bool
	durablyApplied      bool
	baseBroadcasted     bool
}

func (mutation *privateVoiceLeaveMutation) deleteRow(
	ctx context.Context,
	tx *sql.Tx,
) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
		  AND lifecycle_event_at <= $3
	`, mutation.conversationID, mutation.senderID, mutation.eventAt)
	if err != nil {
		return false, fmt.Errorf("delete private call participant: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read private call participant delete result: %w", err)
	}
	if rowsAffected > 1 {
		return false, fmt.Errorf(
			"private call participant delete affected %d rows", rowsAffected,
		)
	}
	return rowsAffected == 1, nil
}

func (mutation *privateVoiceLeaveMutation) apply(ctx context.Context) (bool, error) {
	var mutationApplied bool
	var err error
	if mutation.callID == uuid.Nil {
		mutationApplied, err = mutation.subscriber.withVoiceLifecycleLockInParticipantSet(
			ctx, presence.CategoryPrivateCall, mutation.senderID,
			mutation.conversationID, mutation.deleteRow,
		)
	} else {
		mutationApplied, err = mutation.subscriber.withVoiceLifecycleClaimInParticipantSet(
			ctx,
			voiceLifecycleClaimRequest{
				category: presence.CategoryPrivateCall,
				senderID: mutation.senderID, token: mutation.callID,
				eventAt: mutation.eventAt, conversationID: mutation.conversationID,
			},
			mutation.deleteRow,
		)
	}
	if err != nil || !mutationApplied {
		mutation.durablyApplied = mutationApplied
		mutation.applied = mutationApplied
		return mutationApplied, err
	}
	mutation.durablyApplied = true
	mutation.subscriber.broadcastPrivateVoiceLeft(
		mutation.conversationID, mutation.senderID,
	)
	mutation.baseBroadcasted = true
	if mutation.subscriber.privateLeaveAfterCommitHook != nil {
		mutation.subscriber.privateLeaveAfterCommitHook()
	}
	presence.InvalidateActivityBuildCache(ctx)
	postParticipantIDs, collectErr := mutation.subscriber.collectVoiceParticipantIDs(
		ctx, mutation.conversationID, true,
	)
	if collectErr != nil {
		return false, fmt.Errorf("collect post-leave private participants: %w", collectErr)
	}
	if deleteErr := mutation.subscriber.deleteCapturedPrivateActivityGenerations(
		ctx, postParticipantIDs, mutation.capturedGenerations,
	); deleteErr != nil {
		return false, fmt.Errorf("delete advanced private activity generations: %w", deleteErr)
	}
	mutation.applied = mutationApplied
	return true, nil
}

func (s *NATSSubscriber) preparePrivateVoiceLeave(
	ctx context.Context,
	event voiceLeftEvent,
	conversationID uuid.UUID,
	eventAt time.Time,
) (*privateVoiceLeaveMutation, bool) {
	if s.activity == nil {
		s.log.Error(logPrivateBridgeUnavailable)
		return nil, false
	}
	mayMutate, err := s.dmCallEventMayMutateLiveState(ctx, conversationID, event.CallID)
	if err != nil {
		s.handleDMVoiceDependencyFailure("Private Call terminal ownership validation failed", err)
		return nil, false
	}
	if !mayMutate {
		s.log.Warn("Ignored stale or uncorrelated Private Call terminal event",
			"failure_class", "stale_event")
		return nil, false
	}
	senderID, err := uuid.Parse(event.UserID)
	if err != nil || senderID == uuid.Nil {
		s.log.Error("Rejected private voice.left with invalid sender")
		return nil, false
	}
	callID, err := parseOptionalDMVoiceCallID(event.CallID)
	if err != nil {
		s.log.Error("Rejected private voice.left with invalid call")
		return nil, false
	}
	participantIDs, err := s.collectVoiceParticipantIDs(ctx, conversationID, true)
	if err != nil {
		s.log.Error(logPrivatePreMutationReadFailure, "failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return nil, false
	}
	capturedGenerations, err := s.capturePrivateActivityGenerations(ctx, participantIDs)
	if err != nil {
		s.log.Error("Private Call pre-mutation activity read failed",
			"failure_class", presence.PolicyErrorClass(err))
		s.disconnectAllRichPresenceClients()
		return nil, false
	}
	return &privateVoiceLeaveMutation{
		subscriber: s, conversationID: conversationID,
		senderID: senderID, callID: callID, eventAt: eventAt,
		capturedGenerations: capturedGenerations,
	}, true
}

func parseOptionalDMVoiceCallID(rawCallID string) (uuid.UUID, error) {
	if rawCallID == "" {
		return uuid.Nil, nil
	}
	return parseDMVoiceCallLifecycleID(rawCallID, "call")
}

func (s *NATSSubscriber) handlePrivateVoiceLeft(
	ctx context.Context,
	event voiceLeftEvent,
	conversationID uuid.UUID,
	eventAt time.Time,
) bool {
	mutation, prepared := s.preparePrivateVoiceLeave(ctx, event, conversationID, eventAt)
	if !prepared {
		return false
	}
	if event.CallID == "" {
		_, mutationErr := mutation.apply(ctx)
		if mutation.durablyApplied && !mutation.baseBroadcasted {
			s.broadcastPrivateVoiceLeft(conversationID, mutation.senderID)
		}
		if mutationErr != nil {
			s.log.Error("Private Call legacy participant clear failed", "failure_class", "state_write")
		}
		if mutation.durablyApplied {
			s.disconnectAllRichPresenceClients()
		}
		return mutation.durablyApplied
	}
	activityErr := s.activity.ClearPrivateCall(
		ctx,
		mutation.senderID,
		presence.Scope{
			Category: presence.CategoryPrivateCall, RoomID: conversationID,
			LifecycleID: mutation.callID, EventAt: eventAt,
		},
		nil,
		mutation.apply,
	)
	if mutation.durablyApplied && !mutation.baseBroadcasted {
		s.broadcastPrivateVoiceLeft(conversationID, mutation.senderID)
	}
	if mutation.durablyApplied {
		s.refreshPrivateVoicePeers(
			ctx, conversationID, mutation.callID, eventAt, uuid.Nil,
			map[uuid.UUID]bool{mutation.senderID: true},
		)
	}
	if activityErr != nil {
		s.log.Error("Private Call Rich Presence clear failed",
			"failure_class", presence.PolicyErrorClass(activityErr))
		s.disconnectAllRichPresenceClients()
	}
	return mutation.durablyApplied
}

func (s *NATSSubscriber) broadcastPrivateVoiceLeft(
	conversationID, senderID uuid.UUID,
) {
	if s.privateVoiceStateBroadcastHook != nil {
		s.privateVoiceStateBroadcastHook(conversationID, senderID, "left")
	}
	s.hub.BroadcastToDMParticipants(conversationID, websocket.OutgoingMessage{
		Type: "dm_voice_state_update",
		Data: map[string]interface{}{
			"conversation_id": conversationID.String(),
			"user_id":         senderID.String(), "action": "left",
		},
	})
}

func (s *NATSSubscriber) broadcastPrivateVoiceOldScopeLeaves(
	deltas []privateVoiceScopeBaseDelta,
) {
	for _, delta := range deltas {
		for _, participantID := range delta.participantIDs {
			s.broadcastPrivateVoiceLeft(delta.conversationID, participantID)
		}
	}
}

type serverVoiceLeaveMutation struct {
	subscriber *NATSSubscriber
	channelID  uuid.UUID
	senderID   uuid.UUID
	eventAt    time.Time
	applied    bool
}

func (mutation *serverVoiceLeaveMutation) deleteRow(
	ctx context.Context,
	tx *sql.Tx,
) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM voice_participants
		WHERE channel_id = $1 AND user_id = $2
		  AND lifecycle_event_at <= $3
	`, mutation.channelID, mutation.senderID, mutation.eventAt)
	if err != nil {
		return false, fmt.Errorf("delete server voice participant: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read server voice participant delete result: %w", err)
	}
	if rowsAffected > 1 {
		return false, fmt.Errorf(
			"server voice participant delete affected %d rows", rowsAffected,
		)
	}
	return rowsAffected == 1, nil
}

func (mutation *serverVoiceLeaveMutation) apply(ctx context.Context) (bool, error) {
	var err error
	mutation.applied, err = mutation.subscriber.withVoiceLifecycleClaim(
		ctx, presence.CategoryServerVoice, mutation.senderID,
		mutation.channelID, mutation.eventAt, false, mutation.deleteRow,
	)
	if err == nil && mutation.applied {
		presence.InvalidateActivityBuildCache(ctx)
	}
	return mutation.applied, err
}

func (s *NATSSubscriber) handleServerVoiceLeft(
	ctx context.Context,
	event voiceLeftEvent,
	room *roomContext,
	eventAt time.Time,
) bool {
	if s.activity == nil {
		s.log.Error(logServerBridgeUnavailable)
		return false
	}
	senderID, err := uuid.Parse(event.UserID)
	if err != nil || senderID == uuid.Nil {
		s.log.Error("Rejected voice.left with invalid sender")
		return false
	}
	channelID, err := uuid.Parse(event.ChannelID)
	if err != nil || channelID == uuid.Nil {
		s.log.Error("Rejected voice.left with invalid room")
		return false
	}
	mutation := &serverVoiceLeaveMutation{
		subscriber: s, channelID: channelID, senderID: senderID, eventAt: eventAt,
	}
	activityErr := s.activity.ClearServerVoice(
		ctx,
		senderID,
		presence.Scope{
			Category: presence.CategoryServerVoice, RoomID: channelID,
			LifecycleID: channelID, EventAt: eventAt,
		},
		mutation.apply,
	)
	if activityErr != nil {
		s.log.Error("Server voice Rich Presence clear failed",
			"failure_class", presence.PolicyErrorClass(activityErr))
		s.disconnectAllRichPresenceClients()
	}
	if !mutation.applied {
		return false
	}
	s.broadcastServerVoiceParticipant(room, channelID, senderID, "left")
	s.revokeTempGrantIfHeld(ctx, room.serverID, event.ChannelID, event.UserID, false)
	return true
}

func (s *NATSSubscriber) persistDMRoomEmptySummary(
	ctx context.Context,
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	summary, hasSummary, err := completedCallSummaryFromRoomEmpty(event, eventAt)
	switch {
	case err != nil:
		s.log.Error("Rejected malformed DM room-empty call summary",
			"failure_class", "invalid_event")
	case hasSummary:
		// Persist an exact old-call summary even when a newer call now owns
		// live presence. The call ID makes this insert idempotent and distinct
		// from the replacement lifecycle.
		if dm.InsertCompletedCallEvent(ctx, s.db, conversationID, summary) != nil {
			s.log.Error("Failed to insert completed call_event row",
				"failure_class", "state_write")
		}
	default:
		// Best-effort legacy fallback only. ID-less media events cannot renew or
		// terminate an exact shared lease and therefore do not provide complete
		// rolling-version compatibility. We run the live-presence fallback below
		// only when no exact call currently owns the conversation.
		s.log.Warn("Legacy DM room-empty event lacks terminal call summary",
			"failure_class", "legacy_event")
	}
	return hasSummary, err
}

func parseDMRoomEmptyCallID(rawCallID string) (uuid.UUID, error) {
	callID, err := uuid.Parse(rawCallID)
	if err != nil {
		return uuid.Nil, err
	}
	if callID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: zero call ID", errInvalidDMVoiceCallLifecycle)
	}
	return callID, nil
}

func (s *NATSSubscriber) beginDMRoomEmptyCleanup(
	ctx context.Context,
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
) (func(), bool) {
	if event.CallID == "" {
		return func() {
			// Legacy ID-less events do not acquire a distributed cleanup guard.
		}, true
	}
	callID, err := parseDMRoomEmptyCallID(event.CallID)
	if err != nil {
		s.log.Error("Failed to begin malformed DM voice call cleanup",
			"failure_class", "invalid_event")
		return nil, false
	}
	acquired, err := dm.BeginDMVoiceCallCleanup(
		ctx, s.redis, conversationID, callID,
	)
	if err != nil {
		s.log.Error("Failed to begin terminal DM voice call cleanup",
			"failure_class", "dependency")
		return nil, false
	}
	if !acquired {
		s.log.Warn("Ignored stale or concurrent DM voice call cleanup",
			"failure_class", "stale_event")
		return nil, false
	}

	release := func() {
		releaseCtx, cancelRelease := context.WithTimeout(
			context.Background(), richPresenceLifecycleTimeout,
		)
		defer cancelRelease()
		if err := dm.EndDMVoiceCallCleanup(
			releaseCtx, s.redis, conversationID, callID,
		); err != nil {
			s.log.Error("Failed to release terminal DM voice call cleanup guard",
				"failure_class", "dependency")
		}
	}
	return release, true
}

func (s *NATSSubscriber) handleDMRoomEmpty(
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
	eventAt time.Time,
) bool {
	cleanupCtx, cancelCleanup := context.WithTimeout(
		context.Background(), dmRoomEmptyCleanupTimeout,
	)
	defer cancelCleanup()
	cleanupCtx = presence.WithActivityBuildCache(cleanupCtx)
	if _, participantErr := s.collectVoiceParticipantRecords(
		cleanupCtx, conversationID, true,
	); participantErr != nil {
		s.log.Error(logPrivatePreMutationReadFailure,
			"failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return false
	}
	hasSummary, summaryErr := s.persistDMRoomEmptySummary(
		cleanupCtx, event, conversationID, eventAt,
	)

	mayMutate, err := s.dmCallEventMayMutateLiveState(
		cleanupCtx, conversationID, event.CallID,
	)
	if err != nil {
		s.handleDMVoiceDependencyFailure(
			"Private Call room-empty ownership validation failed", err,
		)
		return false
	}
	if !mayMutate {
		s.log.Warn("Ignored stale or uncorrelated DM room-empty live-state cleanup",
			"failure_class", "stale_event")
		return false
	}

	return s.finishDMRoomEmptyLiveState(
		cleanupCtx, event, conversationID, eventAt, !hasSummary && summaryErr == nil,
	)
}

func (s *NATSSubscriber) finishDMRoomEmptyLiveState(
	ctx context.Context,
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
	eventAt time.Time,
	persistFallback bool,
) bool {
	if s.activity == nil {
		s.log.Error(logPrivateBridgeUnavailable)
		return false
	}
	ctx = presence.WithActivityBuildCache(ctx)
	participantRecords, err := s.collectVoiceParticipantRecords(ctx, conversationID, true)
	if err != nil {
		s.log.Error("Private Call Rich Presence terminal state read failed",
			"failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return false
	}
	if persistFallback {
		s.persistDMRoomEmptyFallback(ctx, event, conversationID, eventAt)
	}
	if event.CallID != "" && len(participantRecords) == 0 {
		// Another replica may already have removed the shared rows for this exact
		// terminal. Its local Hub cannot clear this replica's clients, so reconnect
		// them before emitting the idempotent local room-empty base frame.
		s.disconnectAllRichPresenceClients()
	}
	callID, err := parseOptionalDMRoomEmptyCallID(event.CallID)
	if err != nil {
		s.log.Error("Rejected malformed Private Call Rich Presence terminal event")
		return false
	}
	cleanupGuard := dmRoomEmptyCleanupGuard{
		subscriber: s, event: event,
		conversationID: conversationID, callID: callID,
	}
	defer cleanupGuard.release()
	preMutationParticipants := privateVoiceParticipantSet(participantRecords)
	removedCount := 0
	legacyCleared := false
	for _, participant := range participantRecords {
		removed := s.clearDMRoomEmptyParticipant(
			ctx, participant, conversationID, callID, eventAt,
			preMutationParticipants, &cleanupGuard,
		)
		if removed {
			removedCount++
		}
		if removed && callID == uuid.Nil {
			legacyCleared = true
		}
	}
	if legacyCleared {
		s.disconnectAllRichPresenceClients()
	}
	if err := cleanupGuard.begin(ctx); err != nil {
		s.log.Error("Private Call terminal cleanup guard unavailable",
			"failure_class", "dependency")
		return false
	}
	return s.verifyDMRoomEmptyTerminal(
		ctx, event, conversationID, len(participantRecords), removedCount,
	)
}

func (s *NATSSubscriber) verifyDMRoomEmptyTerminal(
	ctx context.Context,
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
	observedCount, removedCount int,
) bool {
	remainingParticipants, err := s.collectDMRoomEmptyRemainingParticipants(
		ctx, conversationID,
	)
	if err != nil {
		s.log.Error("Private Call Rich Presence terminal verification failed",
			"failure_class", "state_read")
		if observedCount == 0 || removedCount == observedCount {
			s.broadcastPrivateVoiceRoomEmpty(event, conversationID)
			s.disconnectAllRichPresenceClients()
			return true
		}
		s.disconnectAllRichPresenceClients()
		return false
	}
	if len(remainingParticipants) > 0 {
		return false
	}

	s.broadcastPrivateVoiceRoomEmpty(event, conversationID)
	return true
}

func (s *NATSSubscriber) broadcastPrivateVoiceRoomEmpty(
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
) {
	if s.privateVoiceStateBroadcastHook != nil {
		s.privateVoiceStateBroadcastHook(conversationID, uuid.Nil, "room_empty")
	}
	s.hub.BroadcastToDMParticipants(conversationID, websocket.OutgoingMessage{
		Type: "dm_voice_state_update",
		Data: map[string]interface{}{
			"conversation_id": event.ChannelID,
			"action":          "room_empty",
		},
	})
}

func (s *NATSSubscriber) collectDMRoomEmptyRemainingParticipants(
	ctx context.Context,
	conversationID uuid.UUID,
) ([]uuid.UUID, error) {
	if s.dmRoomEmptyVerificationHook != nil {
		return nil, s.dmRoomEmptyVerificationHook()
	}
	return s.collectVoiceParticipantIDs(ctx, conversationID, true)
}

type dmRoomEmptyCleanupGuard struct {
	subscriber     *NATSSubscriber
	event          voiceRoomEmptyEvent
	conversationID uuid.UUID
	callID         uuid.UUID
	releaseCleanup func()
	attempted      bool
}

func (guard *dmRoomEmptyCleanupGuard) begin(ctx context.Context) error {
	if guard.callID == uuid.Nil || guard.releaseCleanup != nil {
		return nil
	}
	if guard.attempted {
		return errors.New("terminal private voice call cleanup guard unavailable")
	}
	guard.attempted = true
	release, acquired := guard.subscriber.beginDMRoomEmptyCleanup(
		ctx, guard.event, guard.conversationID,
	)
	if !acquired {
		return errors.New("terminal private voice call cleanup guard unavailable")
	}
	guard.releaseCleanup = release
	return nil
}

func (guard *dmRoomEmptyCleanupGuard) release() {
	if guard.releaseCleanup != nil {
		guard.releaseCleanup()
	}
}

func parseOptionalDMRoomEmptyCallID(rawCallID string) (uuid.UUID, error) {
	if rawCallID == "" {
		return uuid.Nil, nil
	}
	return parseDMRoomEmptyCallID(rawCallID)
}

func privateVoiceParticipantSet(
	participantRecords []voiceParticipantRecord,
) map[uuid.UUID]bool {
	participants := make(map[uuid.UUID]bool, len(participantRecords))
	for _, participant := range participantRecords {
		participants[participant.userID] = true
	}
	return participants
}

func (s *NATSSubscriber) clearDMRoomEmptyParticipant(
	ctx context.Context,
	participant voiceParticipantRecord,
	conversationID, callID uuid.UUID,
	eventAt time.Time,
	preMutationParticipants map[uuid.UUID]bool,
	cleanupGuard *dmRoomEmptyCleanupGuard,
) bool {
	applied := false
	deleteParticipant := func(mutationCtx context.Context) (bool, error) {
		if err := cleanupGuard.begin(mutationCtx); err != nil {
			return false, err
		}
		var deleteErr error
		applied, deleteErr = s.deleteDMVoiceParticipantWithRetry(
			mutationCtx, conversationID, participant.userID, callID, eventAt,
		)
		if callID == uuid.Nil && deleteErr == nil && applied {
			presence.InvalidateActivityBuildCache(mutationCtx)
		}
		return applied, deleteErr
	}
	if callID == uuid.Nil {
		_, deleteErr := deleteParticipant(ctx)
		if deleteErr != nil {
			s.log.Error("Private Call legacy terminal participant clear failed",
				"failure_class", "state_write")
		}
		return applied
	}
	clearErr := s.activity.ClearPrivateCallTerminal(
		ctx,
		participant.userID,
		presence.Scope{
			Category: presence.CategoryPrivateCall, RoomID: conversationID,
			LifecycleID: callID, EventAt: participant.lifecycleEventAt,
		},
		preMutationParticipants,
		deleteParticipant,
	)
	if clearErr != nil {
		s.log.Error("Private Call Rich Presence terminal participant clear failed",
			"failure_class", presence.PolicyErrorClass(clearErr))
		s.disconnectAllRichPresenceClients()
	}
	return applied
}

func (s *NATSSubscriber) deleteDMVoiceParticipantWithRetry(
	ctx context.Context,
	conversationID, participantID, callID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	deleteRow := func(ctx context.Context, tx *sql.Tx) (bool, error) {
		return deleteDMVoiceParticipantRowWithRetry(
			ctx, tx, conversationID, participantID, eventAt,
		)
	}
	if callID == uuid.Nil {
		return s.withVoiceLifecycleLockInParticipantSet(
			ctx, presence.CategoryPrivateCall, participantID, conversationID, deleteRow,
		)
	}
	return s.withVoiceLifecycleClaimInParticipantSet(
		ctx,
		voiceLifecycleClaimRequest{
			category: presence.CategoryPrivateCall,
			senderID: participantID, token: callID,
			eventAt: eventAt, conversationID: conversationID,
		},
		deleteRow,
	)
}

func deleteDMVoiceParticipantRowWithRetry(
	ctx context.Context,
	tx *sql.Tx,
	conversationID, participantID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	var deleteErr error
	for attempt := 0; attempt < dmRoomEmptyCleanupAttempts; attempt++ {
		if _, err := tx.ExecContext(ctx, `SAVEPOINT voice_participant_delete_retry`); err != nil {
			return false, fmt.Errorf("create private call terminal delete savepoint: %w", err)
		}
		result, attemptErr := tx.ExecContext(
			ctx,
			`DELETE FROM dm_voice_participants
			 WHERE conversation_id = $1 AND user_id = $2
			   AND lifecycle_event_at <= $3`,
			conversationID,
			participantID,
			eventAt,
		)
		if attemptErr == nil {
			return finishDMVoiceParticipantDelete(ctx, tx, result)
		}
		deleteErr = attemptErr
		if rollbackErr := rollbackDMVoiceParticipantDelete(ctx, tx); rollbackErr != nil {
			return false, errors.Join(deleteErr, rollbackErr)
		}
		if ctx.Err() != nil {
			return false, deleteErr
		}
		if waitForDMVoiceParticipantDeleteRetry(ctx, attempt) != nil {
			return false, deleteErr
		}
	}
	return false, deleteErr
}

func finishDMVoiceParticipantDelete(
	ctx context.Context,
	tx *sql.Tx,
	result sql.Result,
) (bool, error) {
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT voice_participant_delete_retry`); err != nil {
		return false, fmt.Errorf("release private call terminal delete savepoint: %w", err)
	}
	rowsAffected, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return false, fmt.Errorf("read private call terminal delete result: %w", rowsErr)
	}
	if rowsAffected > 1 {
		return false, fmt.Errorf(
			"private call terminal delete affected %d rows", rowsAffected,
		)
	}
	return rowsAffected == 1, nil
}

func rollbackDMVoiceParticipantDelete(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(
		ctx, `ROLLBACK TO SAVEPOINT voice_participant_delete_retry`,
	); err != nil {
		return fmt.Errorf("rollback private call terminal delete savepoint: %w", err)
	}
	return nil
}

func waitForDMVoiceParticipantDeleteRetry(ctx context.Context, attempt int) error {
	if attempt+1 >= dmRoomEmptyCleanupAttempts {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}

func (s *NATSSubscriber) persistDMRoomEmptyFallback(
	ctx context.Context,
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
	endedAt time.Time,
) {
	if event.CallID == "" {
		if err := dm.InsertCompletedCallEventForDMRoom(
			ctx, s.db, conversationID,
		); err != nil {
			s.log.Error("Failed to insert legacy completed call_event row",
				"failure_class", "state_write")
		}
		return
	}

	callID, err := parseDMRoomEmptyCallID(event.CallID)
	if err != nil {
		s.log.Error("Failed to parse heartbeat fallback call ID",
			"failure_class", "invalid_event")
		return
	}
	callerUserID, err := uuid.Parse(event.CallerUserID)
	if err != nil || callerUserID == uuid.Nil {
		s.log.Error("Failed to parse heartbeat fallback caller ID",
			"failure_class", "invalid_event")
		return
	}
	ringID, err := parseOptionalDMVoiceCallLifecycleID(event.RingID, "ring")
	if err != nil {
		s.log.Error("Failed to parse heartbeat fallback ring ID",
			"failure_class", "invalid_event")
		return
	}
	if err := dm.InsertCompletedCallEventForDMHeartbeat(
		ctx, s.db, conversationID, callID, ringID, callerUserID, endedAt,
	); err != nil {
		s.log.Error("Failed to insert exact heartbeat completed call_event row",
			"failure_class", "state_write")
	}
}

func (s *NATSSubscriber) handleEmptyDMHeartbeat(
	event voiceHeartbeatEvent,
	conversationID uuid.UUID,
	eventAt time.Time,
) bool {
	// An empty heartbeat is an exact terminal reconciliation signal. Fence it
	// against a replacement call before deleting the matching lease/tombstoning
	// its ID, then preserve the presence-derived fallback while rows still exist.
	callID, err := parseDMRoomEmptyCallID(event.CallID)
	if err != nil {
		s.log.Error("Rejected empty DM heartbeat without an exact call ID",
			"failure_class", "invalid_event")
		return false
	}
	terminal := voiceRoomEmptyEvent{
		ChannelID:    event.ChannelID,
		CallID:       event.CallID,
		RingID:       event.RingID,
		CallerUserID: event.CallerUserID,
		Timestamp:    event.Timestamp,
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(
		context.Background(), dmRoomEmptyCleanupTimeout,
	)
	defer cancelCleanup()
	cleanupCtx = presence.WithActivityBuildCache(cleanupCtx)
	lease, hasLease, err := dm.LookupDMVoiceCallLease(
		cleanupCtx, s.redis, conversationID,
	)
	if err != nil {
		s.handleDMVoiceDependencyFailure(
			"Private Call empty-heartbeat lease lookup failed", err,
		)
		return false
	}
	if hasLease && lease.CallID == callID {
		if terminal.CallerUserID == "" {
			terminal.CallerUserID = lease.CallerUserID.String()
		}
		if terminal.RingID == "" && lease.RingID != uuid.Nil {
			terminal.RingID = lease.RingID.String()
		}
	}

	mayMutate, err := s.dmCallEventMayMutateLiveState(
		cleanupCtx, conversationID, event.CallID,
	)
	if err != nil {
		s.handleDMVoiceDependencyFailure(
			"Private Call empty-heartbeat ownership validation failed", err,
		)
		return false
	}
	if !mayMutate {
		s.log.Warn("Ignored stale or uncorrelated empty DM heartbeat",
			"failure_class", "stale_event")
		return false
	}

	return s.finishDMRoomEmptyLiveState(
		cleanupCtx, terminal, conversationID, eventAt, true,
	)
}

func (s *NATSSubscriber) handleRoomEmpty(data []byte) {
	var event voiceRoomEmptyEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Rejected voice.room_empty", "failure_class", "invalid_event")
		return
	}
	if len(event.ParticipantUserIDs) > maxPrivateVoiceParticipantIDs {
		s.log.Error("Rejected voice.room_empty with oversized participant set",
			"failure_class", "invalid_event")
		return
	}
	eventAt, err := parseVoiceEventTime(event.Timestamp)
	if err != nil {
		s.log.Error("Rejected voice.room_empty with invalid timestamp")
		return
	}

	activityCtx, cancelActivity := context.WithTimeout(
		context.Background(), richPresenceLifecycleTimeout,
	)
	defer cancelActivity()
	activityCtx = presence.WithActivityBuildCache(activityCtx)
	ctx, err := s.resolveRoom(activityCtx, event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.room_empty", "failure_class", "state_read")
		return
	}

	applied := s.handleResolvedRoomEmpty(activityCtx, event, ctx, eventAt)
	if !applied {
		return
	}

	s.log.Info(logVoiceLifecycleApplied, "action", "room_empty", "is_dm", ctx.isDM)
}

func (s *NATSSubscriber) handleResolvedRoomEmpty(
	ctx context.Context,
	event voiceRoomEmptyEvent,
	room *roomContext,
	eventAt time.Time,
) bool {
	if room.isDM {
		unlockLifecycle := dm.LockDMCallLifecycle(room.convUUID)
		defer unlockLifecycle()
		return s.handleDMRoomEmpty(event, room.convUUID, eventAt)
	}
	if s.activity == nil {
		s.log.Error(logServerBridgeUnavailable)
		return false
	}
	channelID, err := uuid.Parse(event.ChannelID)
	if err != nil || channelID == uuid.Nil {
		s.log.Error("Rejected voice.room_empty with invalid room")
		return false
	}
	removedAny, databaseEmpty, reconcileErr := s.reconcileServerHeartbeat(
		ctx, channelID, nil, eventAt, room,
	)
	if databaseEmpty {
		if !removedAny {
			s.disconnectAllRichPresenceClients()
		}
		s.broadcastRoomEmpty(ctx, event.ChannelID, room)
		s.hub.BroadcastServerVoiceCounts()
	}
	if reconcileErr != nil {
		s.log.Error("Server voice Rich Presence terminal reconciliation failed",
			"failure_class", presence.PolicyErrorClass(reconcileErr))
		s.disconnectAllRichPresenceClients()
	}
	return databaseEmpty
}

func (s *NATSSubscriber) refreshDMHeartbeat(
	event voiceHeartbeatEvent,
	conversationID uuid.UUID,
	participantIDs []uuid.UUID,
	eventAt time.Time,
) bool {
	callID, err := parseDMVoiceCallLifecycleID(event.CallID, "call")
	if err != nil {
		s.log.Error("Rejected private voice.heartbeat with invalid call")
		return false
	}
	activityCtx, cancelActivity := context.WithTimeout(
		context.Background(), richPresenceLifecycleTimeout,
	)
	defer cancelActivity()
	activityCtx = presence.WithActivityBuildCache(activityCtx)
	if s.activity == nil {
		s.log.Error(logPrivateBridgeUnavailable)
		return false
	}
	mayMutate, err := s.dmCallEventMayMutateLiveState(
		activityCtx, conversationID, event.CallID,
	)
	if err != nil {
		s.handleDMVoiceDependencyFailure(
			"Private Call heartbeat ownership validation failed", err,
		)
		return false
	}
	if !mayMutate {
		s.log.Warn("Ignored stale Private Call heartbeat", "failure_class", "stale_event")
		return false
	}
	preparation, prepared := s.prepareDMHeartbeatRefresh(
		activityCtx, conversationID, participantIDs,
	)
	if !prepared {
		return false
	}
	refreshResult, refreshed := s.runDMHeartbeatBulkRefresh(
		activityCtx, event, conversationID, callID, participantIDs, eventAt,
	)
	if !refreshed {
		return false
	}
	removedParticipants := s.applyDMHeartbeatDurableEffects(
		conversationID, preparation, refreshResult,
	)
	if s.dmHeartbeatPostCommitHook != nil {
		s.dmHeartbeatPostCommitHook()
	}
	return s.finalizeDMHeartbeatRefresh(
		activityCtx, conversationID, callID, eventAt, preparation,
		refreshResult, removedParticipants,
	)
}

type dmHeartbeatRefreshPreparation struct {
	beforeParticipants  map[uuid.UUID]bool
	capturedGenerations map[uuid.UUID]presence.ActivityGeneration
}

type dmHeartbeatRefreshResult struct {
	appliedParticipants    map[uuid.UUID]bool
	removedParticipantIDs  []uuid.UUID
	rejectedParticipantIDs []uuid.UUID
	oldScopeRevisions      []privateVoiceOldScopeRevision
	oldScopeBaseDeltas     []privateVoiceScopeBaseDelta
	reconnect              bool
}

func (s *NATSSubscriber) prepareDMHeartbeatRefresh(
	ctx context.Context,
	conversationID uuid.UUID,
	participantIDs []uuid.UUID,
) (dmHeartbeatRefreshPreparation, bool) {
	beforeRecords, err := s.collectVoiceParticipantRecords(ctx, conversationID, true)
	if err != nil {
		s.log.Error("Private Call Rich Presence heartbeat state read failed",
			"failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return dmHeartbeatRefreshPreparation{}, false
	}
	beforeIDs := make([]uuid.UUID, 0, len(beforeRecords))
	for _, participant := range beforeRecords {
		beforeIDs = append(beforeIDs, participant.userID)
	}
	if !privateVoiceParticipantUnionWithinLimit(beforeIDs, participantIDs) {
		s.log.Error("Rejected private voice.heartbeat with oversized participant union",
			"failure_class", "invalid_event")
		s.disconnectAllRichPresenceClients()
		return dmHeartbeatRefreshPreparation{}, false
	}
	captureIDs := append(append([]uuid.UUID(nil), beforeIDs...), participantIDs...)
	capturedGenerations, err := s.capturePrivateActivityGenerations(
		ctx, captureIDs,
	)
	if err != nil {
		s.log.Error("Private Call heartbeat activity capture failed",
			"failure_class", presence.PolicyErrorClass(err))
		s.disconnectAllRichPresenceClients()
		return dmHeartbeatRefreshPreparation{}, false
	}
	return dmHeartbeatRefreshPreparation{
		beforeParticipants:  privateVoiceParticipantSet(beforeRecords),
		capturedGenerations: capturedGenerations,
	}, true
}

func (s *NATSSubscriber) runDMHeartbeatBulkRefresh(
	ctx context.Context,
	event voiceHeartbeatEvent,
	conversationID, callID uuid.UUID,
	participantIDs []uuid.UUID,
	eventAt time.Time,
) (dmHeartbeatRefreshResult, bool) {
	result := dmHeartbeatRefreshResult{
		appliedParticipants: make(map[uuid.UUID]bool, len(participantIDs)),
	}
	claimResult, _, reconnectAfterBulk, bulkRefreshErr := s.withVoiceLifecycleClaims(
		ctx,
		privateVoiceLifecycleClaimsRequest{
			conversationID: conversationID,
			senderIDs:      participantIDs,
			token:          callID,
			eventAt:        eventAt,
			refreshLease: func(refreshCtx context.Context) error {
				return s.refreshDMVoiceCallLease(
					refreshCtx,
					conversationID,
					event.CallID,
					event.RingID,
					event.CallerUserID,
					true,
				)
			},
			mutation: func(
				mutationCtx context.Context,
				tx *sql.Tx,
				accepted []uuid.UUID,
				removed []uuid.UUID,
			) (applied bool, returnErr error) {
				result.removedParticipantIDs = append(
					result.removedParticipantIDs[:0], removed...,
				)
				return bulkRefreshPrivateVoiceParticipants(
					mutationCtx, tx, conversationID, accepted, eventAt,
					result.appliedParticipants,
				)
			},
		},
	)
	if bulkRefreshErr != nil {
		s.log.Error("Private Call Rich Presence heartbeat bulk refresh failed",
			"failure_class", presence.PolicyErrorClass(bulkRefreshErr))
		s.disconnectAllRichPresenceClients()
		return dmHeartbeatRefreshResult{}, false
	}
	if claimResult != nil {
		result.rejectedParticipantIDs = privateVoiceRejectedParticipantIDs(
			participantIDs, claimResult.acceptedParticipantIDs,
		)
		result.oldScopeRevisions = claimResult.oldScopeRevisions
		result.oldScopeBaseDeltas = claimResult.oldScopeBaseDeltas
	}
	result.reconnect = reconnectAfterBulk
	return result, true
}

func privateVoiceRejectedParticipantIDs(
	reported, accepted []uuid.UUID,
) []uuid.UUID {
	acceptedSet := uuidSliceSet(accepted)
	rejectedSet := make(map[uuid.UUID]bool)
	for _, participantID := range reported {
		if participantID != uuid.Nil && !acceptedSet[participantID] {
			rejectedSet[participantID] = true
		}
	}
	rejected := make([]uuid.UUID, 0, len(rejectedSet))
	for participantID := range rejectedSet {
		rejected = append(rejected, participantID)
	}
	sort.Slice(rejected, func(left, right int) bool {
		return rejected[left].String() < rejected[right].String()
	})
	return rejected
}

func bulkRefreshPrivateVoiceParticipants(
	ctx context.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
	participantIDs []uuid.UUID,
	eventAt time.Time,
	appliedParticipants map[uuid.UUID]bool,
) (applied bool, returnErr error) {
	mediaParticipantIDs := make([]string, 0, len(participantIDs))
	for _, participantID := range participantIDs {
		mediaParticipantIDs = append(mediaParticipantIDs, participantID.String())
	}
	rows, queryErr := tx.QueryContext(ctx, `
		INSERT INTO dm_voice_participants AS participant
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		SELECT $1, member.user_id, $3, $3
		FROM dm_participants AS member
		WHERE member.conversation_id = $1
		  AND member.user_id = ANY($2::uuid[])
		ON CONFLICT (conversation_id, user_id) DO UPDATE
		SET lifecycle_event_at = EXCLUDED.lifecycle_event_at
		WHERE participant.lifecycle_event_at <= EXCLUDED.lifecycle_event_at
		RETURNING user_id
	`, conversationID, pq.Array(mediaParticipantIDs), eventAt)
	if queryErr != nil {
		return false, fmt.Errorf("bulk refresh private call participants: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("close private call heartbeat refresh rows: %w", closeErr),
			)
		}
	}()
	for rows.Next() {
		var participantID uuid.UUID
		if scanErr := rows.Scan(&participantID); scanErr != nil {
			return false, fmt.Errorf("scan refreshed private call participant: %w", scanErr)
		}
		appliedParticipants[participantID] = true
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return false, fmt.Errorf("iterate refreshed private call participants: %w", rowsErr)
	}
	return len(appliedParticipants) > 0, nil
}

func (s *NATSSubscriber) applyDMHeartbeatDurableEffects(
	conversationID uuid.UUID,
	preparation dmHeartbeatRefreshPreparation,
	result dmHeartbeatRefreshResult,
) map[uuid.UUID]bool {
	s.disconnectRejectedDMHeartbeatParticipants(
		conversationID, result.rejectedParticipantIDs,
	)
	s.broadcastPrivateVoiceOldScopeLeaves(result.oldScopeBaseDeltas)
	return s.broadcastDMHeartbeatChanges(
		conversationID, preparation.beforeParticipants,
		result.appliedParticipants, result.removedParticipantIDs,
	)
}

// disconnectRejectedDMHeartbeatParticipants turns every successful media
// heartbeat into a retry point for membership revocation. A removed peer can
// therefore be ejected even if its first post-commit Core NATS command was
// missed, and even when no dm_voice_participants row had landed yet.
func (s *NATSSubscriber) disconnectRejectedDMHeartbeatParticipants(
	conversationID uuid.UUID,
	participantIDs []uuid.UUID,
) {
	if s.nats == nil {
		return
	}
	for _, participantID := range participantIDs {
		if err := s.nats.Publish(natsSubjectEnforceDisconnect, map[string]interface{}{
			"channelId": conversationID.String(),
			"userId":    participantID.String(),
			"action":    "disconnect",
		}); err != nil {
			s.log.Error("Failed to disconnect rejected Private Call heartbeat participant",
				"failure_class", "dependency",
				"action", "disconnect")
		}
	}
}

func (s *NATSSubscriber) finalizeDMHeartbeatRefresh(
	ctx context.Context,
	conversationID, callID uuid.UUID,
	eventAt time.Time,
	preparation dmHeartbeatRefreshPreparation,
	result dmHeartbeatRefreshResult,
	removedParticipants map[uuid.UUID]bool,
) bool {
	if len(result.appliedParticipants) > 0 || len(result.removedParticipantIDs) > 0 {
		presence.InvalidateActivityBuildCache(ctx)
	}
	currentParticipants, err := s.collectVoiceParticipantRecords(
		ctx, conversationID, true,
	)
	if err != nil {
		s.log.Error("Private Call Rich Presence heartbeat state read failed",
			"failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return true
	}
	s.cleanupDMHeartbeatActivityGenerations(
		ctx, currentParticipants, result.removedParticipantIDs,
		preparation.capturedGenerations,
	)
	if refreshErr := s.refreshPrivateVoiceOldScopeRevisions(
		ctx, result.oldScopeRevisions, eventAt,
	); refreshErr != nil {
		s.log.Error("Private Call heartbeat old-scope refresh failed",
			"failure_class", presence.PolicyErrorClass(refreshErr))
		s.disconnectAllRichPresenceClients()
		return true
	}
	if result.reconnect {
		s.disconnectAllRichPresenceClients()
	}
	s.refreshDMHeartbeatParticipants(
		ctx, conversationID, callID, eventAt, currentParticipants, removedParticipants,
	)
	return true
}

func (s *NATSSubscriber) cleanupDMHeartbeatActivityGenerations(
	ctx context.Context,
	currentParticipants []voiceParticipantRecord,
	removedParticipantIDs []uuid.UUID,
	capturedGenerations map[uuid.UUID]presence.ActivityGeneration,
) {
	cleanupParticipantIDs := append([]uuid.UUID(nil), removedParticipantIDs...)
	for _, participant := range currentParticipants {
		cleanupParticipantIDs = append(cleanupParticipantIDs, participant.userID)
	}
	if len(cleanupParticipantIDs) == 0 {
		return
	}
	if deleteErr := s.deleteCapturedPrivateActivityGenerations(
		ctx, cleanupParticipantIDs, capturedGenerations,
	); deleteErr != nil {
		s.log.Error("Private Call heartbeat activity cleanup failed",
			"failure_class", presence.PolicyErrorClass(deleteErr))
		s.disconnectAllRichPresenceClients()
	}
}

func (s *NATSSubscriber) broadcastDMHeartbeatChanges(
	conversationID uuid.UUID,
	beforeParticipants, appliedParticipants map[uuid.UUID]bool,
	removedParticipantIDs []uuid.UUID,
) map[uuid.UUID]bool {
	for participantID := range appliedParticipants {
		if beforeParticipants[participantID] {
			continue
		}
		if s.privateVoiceStateBroadcastHook != nil {
			s.privateVoiceStateBroadcastHook(conversationID, participantID, "joined")
		}
		s.hub.BroadcastToDMParticipants(conversationID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": conversationID.String(),
				"user_id":         participantID.String(),
				"action":          "joined",
			},
		})
	}
	removedParticipants := make(map[uuid.UUID]bool, len(removedParticipantIDs))
	for _, participantID := range removedParticipantIDs {
		removedParticipants[participantID] = true
		if s.privateVoiceStateBroadcastHook != nil {
			s.privateVoiceStateBroadcastHook(conversationID, participantID, "left")
		}
		s.hub.BroadcastToDMParticipants(conversationID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": conversationID.String(),
				"user_id":         participantID.String(),
				"action":          "left",
			},
		})
	}
	return removedParticipants
}

func (s *NATSSubscriber) refreshDMHeartbeatParticipants(
	ctx context.Context,
	conversationID, callID uuid.UUID,
	eventAt time.Time,
	currentParticipants []voiceParticipantRecord,
	removedParticipants map[uuid.UUID]bool,
) {
	for _, participant := range currentParticipants {
		refreshAt := eventAt
		if participant.lifecycleEventAt.After(refreshAt) {
			refreshAt = participant.lifecycleEventAt
		}
		if refreshErr := s.activity.RefreshPrivateCall(
			ctx,
			participant.userID,
			presence.Scope{
				Category: presence.CategoryPrivateCall, RoomID: conversationID,
				LifecycleID: callID, EventAt: refreshAt,
			},
			removedParticipants,
			nil,
		); refreshErr != nil {
			s.log.Error("Private Call Rich Presence heartbeat participant refresh failed",
				"failure_class", presence.PolicyErrorClass(refreshErr))
			s.disconnectAllRichPresenceClients()
		}
	}
}

// handleHeartbeat reconciles voice_participants against the media plane's
// ground-truth room state. Any DB entries not present in the heartbeat are
// stale (client crashed / network dropped) and get cleaned up.
func (s *NATSSubscriber) handleHeartbeat(data []byte) {
	var event voiceHeartbeatEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Rejected voice.heartbeat", "failure_class", "invalid_event")
		return
	}
	eventAt, err := parseVoiceEventTime(event.Timestamp)
	if err != nil {
		s.log.Error("Rejected voice.heartbeat with invalid timestamp")
		return
	}

	activityCtx, cancelActivity := context.WithTimeout(
		context.Background(), richPresenceLifecycleTimeout,
	)
	defer cancelActivity()
	activityCtx = presence.WithActivityBuildCache(activityCtx)
	ctx, err := s.resolveRoom(activityCtx, event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.heartbeat", "failure_class", "state_read")
		return
	}
	participantLimit := maxServerVoiceParticipantIDs
	if ctx.isDM {
		participantLimit = maxPrivateVoiceParticipantIDs
	}
	if len(event.UserIDs) > participantLimit {
		s.log.Error("Rejected voice.heartbeat with oversized participant set",
			"failure_class", "invalid_event")
		return
	}
	mediaParticipantIDs, err := parseSortedVoiceParticipantIDs(
		event.UserIDs, participantLimit,
	)
	if err != nil {
		s.log.Error("Rejected voice.heartbeat with invalid participant set")
		return
	}
	if ctx.isDM {
		s.handleDMHeartbeat(event, ctx.convUUID, mediaParticipantIDs, eventAt)
		return
	}
	s.handleServerHeartbeat(activityCtx, event, ctx, mediaParticipantIDs, eventAt)
}

func (s *NATSSubscriber) handleDMHeartbeat(
	event voiceHeartbeatEvent,
	conversationID uuid.UUID,
	mediaParticipantIDs []uuid.UUID,
	eventAt time.Time,
) {
	unlockLifecycle := dm.LockDMCallLifecycle(conversationID)
	defer unlockLifecycle()

	if len(event.UserIDs) == 0 {
		_ = s.handleEmptyDMHeartbeat(event, conversationID, eventAt)
		return
	}
	s.refreshDMHeartbeat(event, conversationID, mediaParticipantIDs, eventAt)
}

func (s *NATSSubscriber) handleServerHeartbeat(
	ctx context.Context,
	event voiceHeartbeatEvent,
	room *roomContext,
	mediaParticipantIDs []uuid.UUID,
	eventAt time.Time,
) {
	channelID, parseErr := uuid.Parse(event.ChannelID)
	if parseErr != nil || channelID == uuid.Nil {
		s.log.Error("Rejected voice.heartbeat with invalid room")
		return
	}
	s.recheckServerHeartbeatPermissions(room, channelID, mediaParticipantIDs)
	if s.activity == nil {
		s.reEnforceServerHeartbeatParticipants(ctx, room, channelID, mediaParticipantIDs)
		s.log.Error(logServerBridgeUnavailable)
		return
	}
	databaseParticipantIDs, databaseParticipantErr := s.collectVoiceParticipantIDs(
		ctx, channelID, false,
	)
	if databaseParticipantErr != nil {
		s.log.Error("Server voice Rich Presence heartbeat pre-mutation read failed",
			"failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return
	}
	removedAny, databaseEmpty, reconcileErr := s.reconcileServerHeartbeatParticipants(
		ctx, channelID, mediaParticipantIDs, databaseParticipantIDs, eventAt, room,
	)
	if len(mediaParticipantIDs) == 0 && databaseEmpty {
		s.broadcastRoomEmpty(ctx, event.ChannelID, room)
	}
	if reconcileErr != nil {
		if removedAny {
			s.hub.BroadcastServerVoiceCounts()
		}
		s.log.Error("Server voice Rich Presence heartbeat reconciliation failed",
			"failure_class", presence.PolicyErrorClass(reconcileErr))
		s.disconnectAllRichPresenceClients()
		return
	}
	s.finishServerHeartbeatReconciliation(
		ctx, room, channelID, mediaParticipantIDs, eventAt, removedAny,
	)
}

func (s *NATSSubscriber) finishServerHeartbeatReconciliation(
	ctx context.Context,
	room *roomContext,
	channelID uuid.UUID,
	mediaParticipantIDs []uuid.UUID,
	eventAt time.Time,
	removedAny bool,
) {
	remainingParticipantRecords, remainingErr := s.collectVoiceParticipantRecords(
		ctx, channelID, false,
	)
	remainingParticipantIDs := voiceParticipantRecordIDs(remainingParticipantRecords)
	if remainingErr != nil || !serverVoiceParticipantUnionWithinLimit(
		remainingParticipantIDs, mediaParticipantIDs,
	) {
		if removedAny {
			s.hub.BroadcastServerVoiceCounts()
		}
		s.log.Error("Server voice Rich Presence heartbeat replacement bound failed",
			"failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return
	}
	refreshParticipantIDs := prioritizeServerHeartbeatParticipants(
		mediaParticipantIDs, remainingParticipantRecords,
	)
	addedAny, broadcastFailed := s.refreshServerHeartbeatParticipants(
		ctx, room, channelID, refreshParticipantIDs, eventAt,
	)
	if broadcastFailed {
		s.recoverServerHeartbeatBroadcastFailure()
	}
	s.reEnforceServerHeartbeatParticipants(ctx, room, channelID, refreshParticipantIDs)
	if addedAny || removedAny {
		s.hub.BroadcastServerVoiceCounts()
	}
}

func (s *NATSSubscriber) refreshServerHeartbeatParticipants(
	ctx context.Context,
	room *roomContext,
	channelID uuid.UUID,
	participantIDs []uuid.UUID,
	eventAt time.Time,
) (bool, bool) {
	var addedAny atomic.Bool
	var broadcastFailed atomic.Bool
	forEachServerHeartbeatParticipant(ctx, participantIDs, func(participantID uuid.UUID) {
		added, deliveryFailed := s.refreshServerHeartbeatParticipant(
			ctx, room, channelID, participantID, eventAt,
		)
		if added {
			addedAny.Store(true)
		}
		if deliveryFailed {
			broadcastFailed.Store(true)
		}
	})
	return addedAny.Load(), broadcastFailed.Load()
}

// forEachServerHeartbeatParticipant preserves the established deterministic
// ordering for rooms within the Private Call-sized legacy bound. Larger Server
// Voice rooms use a fixed worker count: work remains bounded, distinct senders
// still serialize on their own lifecycle locks, and cancellation stops each
// worker promptly. Stale-first ordering makes any partial callback resumable.
func forEachServerHeartbeatParticipant(
	ctx context.Context,
	participantIDs []uuid.UUID,
	work func(uuid.UUID),
) {
	if len(participantIDs) <= maxPrivateVoiceParticipantIDs {
		forEachServerHeartbeatParticipantSequential(ctx, participantIDs, work)
		return
	}
	workerCount := min(serverHeartbeatParticipantWorkers, len(participantIDs))
	var nextIndex atomic.Int64
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for workerIndex := 0; workerIndex < workerCount; workerIndex++ {
		go func() {
			defer workers.Done()
			runServerHeartbeatParticipantWorker(ctx, participantIDs, &nextIndex, work)
		}()
	}
	workers.Wait()
}

func forEachServerHeartbeatParticipantSequential(
	ctx context.Context,
	participantIDs []uuid.UUID,
	work func(uuid.UUID),
) {
	for _, participantID := range participantIDs {
		if ctx.Err() != nil {
			return
		}
		work(participantID)
	}
}

func runServerHeartbeatParticipantWorker(
	ctx context.Context,
	participantIDs []uuid.UUID,
	nextIndex *atomic.Int64,
	work func(uuid.UUID),
) {
	for {
		if ctx.Err() != nil {
			return
		}
		index := int(nextIndex.Add(1)) - 1
		if index >= len(participantIDs) {
			return
		}
		if ctx.Err() != nil {
			return
		}
		work(participantIDs[index])
	}
}

// recheckServerHeartbeatPermissions queues the asynchronous permission sweep
// once for the authoritative room set. RecheckParticipants coalesces overlapping
// heartbeats, so a large room never creates one detached goroutine per participant.
func (s *NATSSubscriber) recheckServerHeartbeatPermissions(
	room *roomContext,
	channelID uuid.UUID,
	participantIDs []uuid.UUID,
) {
	if s.permEnforcer != nil {
		s.permEnforcer.RecheckParticipants(
			room.serverID, channelID.String(), participantIDs,
		)
	}
}

// reEnforceServerHeartbeatParticipants performs the best-effort synchronous
// mute/deafen sweep only after durable stale/add reconciliation has had the
// shared lifecycle deadline. Slow flag reads therefore cannot prevent every
// heartbeat from making forward progress on authoritative presence.
func (s *NATSSubscriber) reEnforceServerHeartbeatParticipants(
	ctx context.Context,
	room *roomContext,
	channelID uuid.UUID,
	participantIDs []uuid.UUID,
) {
	forEachServerHeartbeatParticipant(ctx, participantIDs, func(participantID uuid.UUID) {
		s.reEnforceServer(ctx, room.serverID, channelID.String(), participantID.String())
	})
}

func (s *NATSSubscriber) refreshServerHeartbeatParticipant(
	ctx context.Context,
	room *roomContext,
	channelID, participantID uuid.UUID,
	eventAt time.Time,
) (added bool, deliveryFailed bool) {
	oldScope, hasOldScope, scopeErr := s.currentServerVoiceScope(ctx, participantID)
	ambiguousOldScope := errors.Is(scopeErr, errAmbiguousServerVoiceScope)
	if scopeErr != nil && !ambiguousOldScope {
		s.log.Error("Server voice Rich Presence heartbeat prior-state read failed",
			"failure_class", "state_read")
		s.disconnectAllRichPresenceClients()
		return false, false
	}
	if ambiguousOldScope {
		s.disconnectAllRichPresenceClients()
		hasOldScope = false
	}
	mutationResult, activityErr := s.applyServerHeartbeatParticipant(
		ctx, channelID, participantID, eventAt, oldScope, hasOldScope,
	)
	if activityErr != nil {
		s.log.Error("Server voice Rich Presence heartbeat refresh failed",
			"failure_class", presence.PolicyErrorClass(activityErr))
		s.disconnectAllRichPresenceClients()
	}
	if serverHeartbeatMutationNeedsReconnect(
		mutationResult, channelID, oldScope, hasOldScope,
	) {
		s.disconnectAllRichPresenceClients()
	}
	if !mutationResult.applied {
		return false, false
	}
	delivered := true
	if mutationResult.added {
		delivered = s.broadcastServerVoiceParticipantContext(
			ctx, room, channelID, participantID, "joined",
		)
	}
	removedDelivered := s.broadcastRemovedServerVoiceRoomsContext(
		ctx, participantID, mutationResult.removedRoomIDs,
	)
	return mutationResult.added, !delivered || !removedDelivered
}

func (s *NATSSubscriber) applyServerHeartbeatParticipant(
	ctx context.Context,
	channelID, participantID uuid.UUID,
	eventAt time.Time,
	oldScope presence.Scope,
	hasOldScope bool,
) (serverVoiceMutationResult, error) {
	newScope := presence.Scope{
		Category: presence.CategoryServerVoice, RoomID: channelID,
		LifecycleID: channelID, EventAt: eventAt,
	}
	var mutationResult serverVoiceMutationResult
	mutation := func(mutationCtx context.Context) (bool, error) {
		if s.serverVoiceScopeObservedHook != nil {
			s.serverVoiceScopeObservedHook(participantID, channelID, eventAt)
		}
		var mutationErr error
		mutationResult, mutationErr = s.upsertServerVoiceParticipant(
			mutationCtx, channelID, participantID, eventAt,
		)
		if mutationErr == nil && mutationResult.applied {
			presence.InvalidateActivityBuildCache(mutationCtx)
		}
		return mutationResult.applied, mutationErr
	}
	if hasOldScope && oldScope.RoomID != channelID {
		return mutationResult, s.activity.MoveServerVoice(
			ctx, participantID, oldScope, newScope, mutation,
		)
	}
	return mutationResult, s.activity.RefreshServerVoice(
		ctx, participantID, newScope, mutation,
	)
}

func serverHeartbeatMutationNeedsReconnect(
	result serverVoiceMutationResult,
	targetRoomID uuid.UUID,
	observedScope presence.Scope,
	hasObservedScope bool,
) bool {
	return result.removedAudienceUnknown ||
		(result.duplicate && (result.replayMissing || len(result.removedRoomIDs) > 0)) ||
		freshServerVoiceRemovalDiffersFromObservedScope(
			result, targetRoomID, observedScope, hasObservedScope,
		)
}

func freshServerVoiceRemovalDiffersFromObservedScope(
	result serverVoiceMutationResult,
	targetRoomID uuid.UUID,
	observedScope presence.Scope,
	hasObservedScope bool,
) bool {
	if !result.applied || result.duplicate || result.removedAudienceUnknown {
		return false
	}
	if !hasObservedScope || observedScope.RoomID == targetRoomID {
		return len(result.removedRoomIDs) != 0
	}
	return len(result.removedRoomIDs) != 1 ||
		result.removedRoomIDs[0] != observedScope.RoomID
}

func (s *NATSSubscriber) broadcastServerVoiceParticipant(
	room *roomContext,
	channelID, participantID uuid.UUID,
	action string,
) {
	s.hub.BroadcastToServer(room.serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"channel_id": channelID.String(),
			"user_id":    participantID.String(),
			"action":     action,
			"server_id":  room.serverID,
		},
	})
}

func (s *NATSSubscriber) broadcastServerVoiceParticipantContext(
	ctx context.Context,
	room *roomContext,
	channelID, participantID uuid.UUID,
	action string,
) bool {
	return s.hub.BroadcastToServerContext(ctx, room.serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"channel_id": channelID.String(),
			"user_id":    participantID.String(),
			"action":     action,
			"server_id":  room.serverID,
		},
	})
}

func (s *NATSSubscriber) broadcastRemovedServerVoiceRooms(
	ctx context.Context,
	participantID uuid.UUID,
	removedRoomIDs []uuid.UUID,
) {
	for _, removedRoomID := range removedRoomIDs {
		removedRoom, resolveErr := s.resolveRoom(ctx, removedRoomID.String())
		if resolveErr != nil || removedRoom.isDM {
			s.disconnectAllRichPresenceClients()
			continue
		}
		s.broadcastServerVoiceParticipant(
			removedRoom, removedRoomID, participantID, "left",
		)
	}
}

func (s *NATSSubscriber) broadcastRemovedServerVoiceRoomsContext(
	ctx context.Context,
	participantID uuid.UUID,
	removedRoomIDs []uuid.UUID,
) bool {
	delivered := true
	for _, removedRoomID := range removedRoomIDs {
		removedRoom, resolveErr := s.resolveRoom(ctx, removedRoomID.String())
		if resolveErr != nil || removedRoom.isDM {
			delivered = false
			continue
		}
		if !s.broadcastServerVoiceParticipantContext(
			ctx, removedRoom, removedRoomID, participantID, "left",
		) {
			delivered = false
		}
	}
	return delivered
}

func (s *NATSSubscriber) reconcileServerHeartbeat(
	ctx context.Context,
	channelID uuid.UUID,
	mediaParticipantIDs []uuid.UUID,
	eventAt time.Time,
	room *roomContext,
) (removedAny bool, databaseEmpty bool, returnErr error) {
	participantIDs, err := s.collectVoiceParticipantIDs(ctx, channelID, false)
	if err != nil {
		return false, false, err
	}
	return s.reconcileServerHeartbeatParticipants(
		ctx, channelID, mediaParticipantIDs, participantIDs, eventAt, room,
	)
}

func (s *NATSSubscriber) reconcileServerHeartbeatParticipants(
	ctx context.Context,
	channelID uuid.UUID,
	mediaParticipantIDs, participantIDs []uuid.UUID,
	eventAt time.Time,
	room *roomContext,
) (removedAny bool, databaseEmpty bool, returnErr error) {
	mediaParticipants := make(map[uuid.UUID]bool, len(mediaParticipantIDs))
	for _, participantID := range mediaParticipantIDs {
		mediaParticipants[participantID] = true
	}

	staleParticipantIDs := make([]uuid.UUID, 0, len(participantIDs))
	for _, participantID := range participantIDs {
		if !mediaParticipants[participantID] {
			staleParticipantIDs = append(staleParticipantIDs, participantID)
		}
	}
	var removed atomic.Bool
	var broadcastFailed atomic.Bool
	var reconcileErrors sync.Mutex
	forEachServerHeartbeatParticipant(ctx, staleParticipantIDs, func(participantID uuid.UUID) {
		applied, activityErr := s.clearStaleServerHeartbeatParticipant(
			ctx, channelID, participantID, eventAt,
		)
		if applied {
			removed.Store(true)
			if !s.broadcastServerVoiceParticipantContext(
				ctx, room, channelID, participantID, "left",
			) {
				broadcastFailed.Store(true)
			}
			s.revokeTempGrantIfHeld(
				ctx, room.serverID, channelID.String(), participantID.String(), true,
			)
		}
		if activityErr != nil {
			reconcileErrors.Lock()
			returnErr = errors.Join(returnErr, activityErr)
			reconcileErrors.Unlock()
		}
	})
	if broadcastFailed.Load() {
		s.recoverServerHeartbeatBroadcastFailure()
	}
	removedAny = removed.Load()

	remainingParticipantIDs, err := s.collectVoiceParticipantIDs(ctx, channelID, false)
	if err != nil {
		return removedAny, false, err
	}
	return removedAny, len(remainingParticipantIDs) == 0, returnErr
}

func (s *NATSSubscriber) clearStaleServerHeartbeatParticipant(
	ctx context.Context,
	channelID, participantID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	applied := false
	activityErr := s.activity.ClearServerVoice(
		ctx,
		participantID,
		presence.Scope{
			Category: presence.CategoryServerVoice, RoomID: channelID,
			LifecycleID: channelID, EventAt: eventAt,
		},
		func(mutationCtx context.Context) (bool, error) {
			mutationApplied, mutationErr := s.deleteStaleServerHeartbeatParticipant(
				mutationCtx, channelID, participantID, eventAt,
			)
			if mutationErr == nil && mutationApplied {
				presence.InvalidateActivityBuildCache(mutationCtx)
			}
			applied = mutationApplied
			return mutationApplied, mutationErr
		},
	)
	return applied, activityErr
}

func (s *NATSSubscriber) deleteStaleServerHeartbeatParticipant(
	ctx context.Context,
	channelID, participantID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	return s.withVoiceLifecycleClaim(
		ctx,
		presence.CategoryServerVoice,
		participantID,
		channelID,
		eventAt,
		false,
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			result, execErr := tx.ExecContext(ctx, `
				DELETE FROM voice_participants
				WHERE channel_id = $1 AND user_id = $2
				  AND lifecycle_event_at <= $3
			`, channelID, participantID, eventAt)
			if execErr != nil {
				return false, fmt.Errorf("delete stale server voice participant: %w", execErr)
			}
			rowsAffected, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				return false, fmt.Errorf("read server heartbeat delete result: %w", rowsErr)
			}
			if rowsAffected > 1 {
				return false, fmt.Errorf(
					"server heartbeat delete affected %d rows", rowsAffected,
				)
			}
			return rowsAffected == 1, nil
		},
	)
}

func parseSortedVoiceParticipantIDs(rawIDs []string, participantLimit int) ([]uuid.UUID, error) {
	if participantLimit <= 0 || len(rawIDs) > participantLimit {
		return nil, errors.New("too many voice participant IDs")
	}
	seen := make(map[uuid.UUID]bool, len(rawIDs))
	participantIDs := make([]uuid.UUID, 0, len(rawIDs))
	for _, rawID := range rawIDs {
		participantID, err := uuid.Parse(rawID)
		if err != nil || participantID == uuid.Nil {
			return nil, fmt.Errorf("invalid voice participant ID")
		}
		if seen[participantID] {
			continue
		}
		seen[participantID] = true
		participantIDs = append(participantIDs, participantID)
	}
	sort.Slice(participantIDs, func(left, right int) bool {
		return participantIDs[left].String() < participantIDs[right].String()
	})
	return participantIDs, nil
}

func privateVoiceParticipantUnionWithinLimit(
	existingIDs, incomingIDs []uuid.UUID,
) bool {
	unique := make(map[uuid.UUID]struct{}, len(existingIDs)+len(incomingIDs))
	for _, participantID := range existingIDs {
		unique[participantID] = struct{}{}
	}
	for _, participantID := range incomingIDs {
		unique[participantID] = struct{}{}
	}
	return len(unique) <= maxPrivateVoiceParticipantIDs
}

func serverVoiceParticipantUnionWithinLimit(
	existingIDs, incomingIDs []uuid.UUID,
) bool {
	if len(existingIDs) > maxServerVoiceParticipantIDs ||
		len(incomingIDs) > maxServerVoiceParticipantIDs {
		return false
	}
	unique := make(map[uuid.UUID]struct{}, len(existingIDs)+len(incomingIDs))
	for _, participantID := range existingIDs {
		unique[participantID] = struct{}{}
	}
	for _, participantID := range incomingIDs {
		unique[participantID] = struct{}{}
	}
	return len(unique) <= maxServerVoiceParticipantIDs
}

func (s *NATSSubscriber) collectVoiceParticipantIDs(
	ctx context.Context,
	roomID uuid.UUID,
	isDM bool,
) (participantIDs []uuid.UUID, returnErr error) {
	if s == nil || s.db == nil {
		return nil, errors.New("voice participant store unavailable")
	}
	return s.collectVoiceParticipantIDsFrom(ctx, s.db, roomID, isDM)
}

type voiceParticipantQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *NATSSubscriber) collectVoiceParticipantIDsFrom(
	ctx context.Context,
	queryer voiceParticipantQueryer,
	roomID uuid.UUID,
	isDM bool,
) (participantIDs []uuid.UUID, returnErr error) {
	if queryer == nil {
		return nil, errors.New("voice participant queryer unavailable")
	}
	participantLimit := maxServerVoiceParticipantIDs
	if isDM {
		participantLimit = maxPrivateVoiceParticipantIDs
	}
	query := `
		SELECT user_id
		FROM voice_participants
		WHERE channel_id = $1
		ORDER BY user_id
		LIMIT $2
	`
	if isDM {
		query = `
			SELECT user_id
			FROM dm_voice_participants
			WHERE conversation_id = $1
			ORDER BY user_id
			LIMIT $2
		`
	}
	rows, err := queryer.QueryContext(ctx, query, roomID, participantLimit+1)
	if err != nil {
		return nil, fmt.Errorf("query current voice participants: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close current voice participants: %w", closeErr))
		}
	}()
	for rows.Next() {
		var participantID uuid.UUID
		if scanErr := rows.Scan(&participantID); scanErr != nil {
			return nil, fmt.Errorf("scan current voice participant: %w", scanErr)
		}
		participantIDs = append(participantIDs, participantID)
		if len(participantIDs) > participantLimit {
			return nil, errors.New("current voice participant limit exceeded")
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate current voice participants: %w", rowsErr)
	}
	return participantIDs, nil
}

type voiceParticipantRecord struct {
	userID           uuid.UUID
	lifecycleEventAt time.Time
}

type privateVoiceScopeMembership struct {
	conversationID uuid.UUID
	userID         uuid.UUID
}

func (s *NATSSubscriber) collectPrivateVoiceScopeMembershipsFrom(
	ctx context.Context,
	queryer voiceParticipantQueryer,
	userIDs []uuid.UUID,
) (memberships []privateVoiceScopeMembership, returnErr error) {
	if queryer == nil || len(userIDs) == 0 || len(userIDs) > maxPrivateVoiceParticipantIDs {
		return nil, errors.New("invalid private voice scope membership query")
	}
	rawUserIDs := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		if userID == uuid.Nil {
			return nil, errors.New("invalid private voice scope member")
		}
		rawUserIDs = append(rawUserIDs, userID.String())
	}
	rows, err := queryer.QueryContext(ctx, `
		SELECT conversation_id, user_id
		FROM dm_voice_participants
		WHERE user_id = ANY($1::uuid[])
		ORDER BY conversation_id, user_id
		LIMIT $2
	`, pq.Array(rawUserIDs), maxPrivateVoiceParticipantIDs+1)
	if err != nil {
		return nil, fmt.Errorf("query private voice scope memberships: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("close private voice scope memberships: %w", closeErr),
			)
		}
	}()
	for rows.Next() {
		var membership privateVoiceScopeMembership
		if scanErr := rows.Scan(&membership.conversationID, &membership.userID); scanErr != nil {
			return nil, fmt.Errorf("scan private voice scope membership: %w", scanErr)
		}
		memberships = append(memberships, membership)
		if len(memberships) > maxPrivateVoiceParticipantIDs {
			return nil, errors.New("private voice scope membership limit exceeded")
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate private voice scope memberships: %w", rowsErr)
	}
	return memberships, nil
}

func (s *NATSSubscriber) collectVoiceParticipantRecords(
	ctx context.Context,
	roomID uuid.UUID,
	isDM bool,
) (participants []voiceParticipantRecord, returnErr error) {
	if s == nil || s.db == nil {
		return nil, errors.New("voice participant store unavailable")
	}
	return s.collectVoiceParticipantRecordsFrom(ctx, s.db, roomID, isDM)
}

func (s *NATSSubscriber) collectVoiceParticipantRecordsFrom(
	ctx context.Context,
	queryer voiceParticipantQueryer,
	roomID uuid.UUID,
	isDM bool,
) (participants []voiceParticipantRecord, returnErr error) {
	if queryer == nil {
		return nil, errors.New("voice participant queryer unavailable")
	}
	participantLimit := maxServerVoiceParticipantIDs
	if isDM {
		participantLimit = maxPrivateVoiceParticipantIDs
	}
	query := `
		SELECT user_id, lifecycle_event_at
		FROM voice_participants
		WHERE channel_id = $1
		ORDER BY user_id
		LIMIT $2
	`
	if isDM {
		query = `
			SELECT user_id, lifecycle_event_at
			FROM dm_voice_participants
			WHERE conversation_id = $1
			ORDER BY user_id
			LIMIT $2
		`
	}
	rows, err := queryer.QueryContext(ctx, query, roomID, participantLimit+1)
	if err != nil {
		return nil, fmt.Errorf("query current voice participant records: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("close current voice participant records: %w", closeErr),
			)
		}
	}()
	for rows.Next() {
		var participant voiceParticipantRecord
		if scanErr := rows.Scan(&participant.userID, &participant.lifecycleEventAt); scanErr != nil {
			return nil, fmt.Errorf("scan current voice participant record: %w", scanErr)
		}
		participants = append(participants, participant)
		if len(participants) > participantLimit {
			return nil, errors.New("current voice participant record limit exceeded")
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate current voice participant records: %w", rowsErr)
	}
	return participants, nil
}

// revokeTempGrantIfHeld is the #487 T8 cleanup-trigger guard shared by the
// voice.left handler and the heartbeat stale-removal path. It cheaply checks
// whether the departing user holds a temporary SBAC grant on the channel and, only
// if so, drives the single revoke convergence point (revoke override + CSK rotation
// + force-disconnect + directed notify). The common no-temp-grant case short-
// circuits after the EXISTS probe so plain leaves stay cheap. actorID is "" because
// these triggers are system-initiated (no human moderator) — the rotator stores
// revoked_by as NULL.
//
// respectGrace=true (heartbeat reconcile path only, finding #7) additionally
// requires the grant be past a 60s grace window, so a brand-new grant whose
// voice.joined event has not yet landed in voice_participants is not revoked by a
// heartbeat that races the join. The voice.left graceful-leave path passes
// respectGrace=false: an explicit leave is authoritative regardless of grant age,
// and the moderator-revoke endpoint never goes through this guard at all.
func (s *NATSSubscriber) revokeTempGrantIfHeld(
	ctx context.Context,
	serverID, channelID, userID string,
	respectGrace bool,
) {
	var held bool
	var err error
	if respectGrace {
		held, err = s.tempGrant.hasTemporaryGrantPastGrace(ctx, channelID, userID)
	} else {
		held, err = s.tempGrant.hasTemporaryGrant(ctx, channelID, userID)
	}
	if err != nil {
		s.log.Error("temp-grant cleanup probe failed", "failure_class", "state_read")
		return
	}
	if !held {
		return
	}
	if err := s.tempGrant.revokeTemporaryChannelAccess(ctx, serverID, channelID, userID, ""); err != nil {
		s.log.Error("temp-grant cleanup revoke failed", "failure_class", "state_write")
	}
}

func (s *NATSSubscriber) broadcastRoomEmpty(
	lifecycleCtx context.Context,
	channelID string,
	room *roomContext,
) {
	if room.isDM {
		s.hub.BroadcastToDMParticipants(room.convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": channelID,
				"action":          "room_empty",
			},
		})
		return
	}
	if !s.hub.BroadcastToServerContext(lifecycleCtx, room.serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"channel_id": channelID,
			"action":     "room_empty",
			"server_id":  room.serverID,
		},
	}) {
		s.recoverServerHeartbeatBroadcastFailure()
	}
}

func (s *NATSSubscriber) recoverServerHeartbeatBroadcastFailure() {
	s.log.Error("Server voice heartbeat broadcast delivery failed",
		"failure_class", "delivery")
	s.disconnectAllRichPresenceClients()
}

const (
	natsSubjectEnforceMute       = "voice.enforce.mute"
	natsSubjectEnforceDeafen     = "voice.enforce.deafen"
	natsSubjectEnforceDisconnect = "voice.enforce.disconnect"

	natsSubjectVoiceWildcard  = "voice.*"
	natsSubjectVoiceJoined    = "voice.joined"
	natsSubjectVoiceLeft      = "voice.left"
	natsSubjectVoiceRoomEmpty = "voice.room_empty"
	natsSubjectVoiceHeartbeat = "voice.heartbeat"
)

// publishForceDisconnect publishes a voice.enforce.disconnect command so the
// media plane closes that peer's transports and removes it from the room (#487
// P3). Revoking VIEW/CONNECT does NOT eject an already-connected peer, so this
// is the authoritative ejection path used by temporary-SBAC access revocation.
// Delegates to the shared tempGrantManager so the publish primitive lives in one
// place (the manager is also driven from the REST Handler's moderator-revoke
// endpoint, DELETE /servers/:id/voice/:userId/temp-access — RevokeTempAccess).
func (s *NATSSubscriber) publishForceDisconnect(channelID, userID string) {
	s.tempGrant.publishForceDisconnect(channelID, userID)
}

// publishEnforcementFlags publishes NATS enforcement commands for active mute/deafen flags.
func (s *NATSSubscriber) publishEnforcementFlags(
	channelID, userID string,
	serverMuted, serverDeafened bool,
) {
	if s.nats == nil {
		return
	}
	if serverMuted {
		if err := s.nats.Publish(natsSubjectEnforceMute, map[string]interface{}{
			"channelId": channelID, "userId": userID, "action": "mute",
		}); err != nil {
			s.log.Error("Failed to publish re-enforcement", "failure_class", "dependency",
				"action", "mute")
		}
	}
	if serverDeafened {
		if err := s.nats.Publish(natsSubjectEnforceDeafen, map[string]interface{}{
			"channelId": channelID, "userId": userID, "action": "deafen",
		}); err != nil {
			s.log.Error("Failed to publish re-enforcement", "failure_class", "dependency",
				"action", "deafen")
		}
	}
}

func (s *NATSSubscriber) convergeServerVoiceParticipant(
	ctx context.Context,
	room *roomContext,
	channelID, participantID uuid.UUID,
) {
	if s.permEnforcer != nil {
		s.permEnforcer.RecheckParticipant(channelID.String(), participantID.String())
	}
	s.reEnforceServer(ctx, room.serverID, channelID.String(), participantID.String())
}

// reEnforceServer publishes NATS enforcement commands if a server member has
// active server_muted or server_deafened flags. Called on voice.joined as a
// belt-and-suspenders safety net alongside the join authorization response.
func (s *NATSSubscriber) reEnforceServer(
	ctx context.Context,
	serverID, channelID, userID string,
) {
	var serverMuted, serverDeafened bool
	if err := s.db.QueryRowContext(ctx, `SELECT server_muted, server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, userID).Scan(&serverMuted, &serverDeafened); err != nil {
		s.log.Error("Failed to query enforcement flags", "failure_class", "state_read")
		return
	}
	s.publishEnforcementFlags(channelID, userID, serverMuted, serverDeafened)
}

// reEnforceDM preserves the pre-existing group-DM hard-moderation convergence
// when a participant joins an active call.
func (s *NATSSubscriber) reEnforceDM(ctx context.Context, channelID, userID string) {
	var serverMuted, serverDeafened bool
	if err := s.db.QueryRowContext(ctx, `SELECT server_muted, server_deafened FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		channelID, userID).Scan(&serverMuted, &serverDeafened); err != nil {
		s.log.Error("Failed to query DM enforcement flags", "failure_class", "state_read")
		return
	}
	s.publishEnforcementFlags(channelID, userID, serverMuted, serverDeafened)
}
