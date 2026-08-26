package presence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ActivityStore manages exact-key ephemeral Rich Presence current state.
type ActivityStore struct {
	redis *redis.Client
}

const activityLifecycleVerificationBatchSize = 64

// ActivityGeneration identifies one exact sender/category lifecycle version.
// It is intentionally payload-free so reconnect verification cannot trust
// persisted activity data before proving the authoritative lifecycle fence.
type ActivityGeneration struct {
	UserID        uuid.UUID
	Category      Category
	SourceToken   uuid.UUID
	SourceVersion int64
}

// NewActivityStore creates an activity store backed by client.
func NewActivityStore(client *redis.Client) *ActivityStore {
	return &ActivityStore{redis: client}
}

const decodeActivityStateLua = `
local function decode_activity_state(raw)
  local ok, state = pcall(cjson.decode, raw)
  if not ok or type(state) ~= 'table' then
    return nil
  end

  local field_count = 0
  for field, _ in pairs(state) do
    if field ~= 'source_token' and field ~= 'source_version' and
        field ~= 'minimized' and field ~= 'payload' and field ~= 'updated_at' then
      return nil
    end
    field_count = field_count + 1
  end

  if field_count ~= 5 or type(state.source_token) ~= 'string' or
      type(state.source_version) ~= 'number' or state.source_version <= 0 or
      state.source_version > 9007199254740991 or
      state.source_version ~= math.floor(state.source_version) or
      type(state.minimized) ~= 'boolean' or type(state.payload) ~= 'table' or
      string.sub(cjson.encode(state.payload), 1, 1) ~= '{' or
      type(state.updated_at) ~= 'number' or state.updated_at <= 0 or
      state.updated_at > 9007199254 or
      state.updated_at ~= math.floor(state.updated_at) then
    return nil
  end

  local token = state.source_token
  local compact_token = string.gsub(token, '-', '')
  if string.len(token) ~= 36 or string.len(compact_token) ~= 32 or
      string.lower(token) ~= token or
      compact_token == '00000000000000000000000000000000' or
      string.sub(token, 9, 9) ~= '-' or string.sub(token, 14, 14) ~= '-' or
      string.sub(token, 19, 19) ~= '-' or string.sub(token, 24, 24) ~= '-' or
      string.find(compact_token, '[^0-9a-fA-F]') then
    return nil
  end

  return state
end
`

const readActivityStateLua = `
local function read_activity_state(key)
  local key_type = redis.call('TYPE', key).ok
  if key_type == 'none' then
    return 'none', nil
  end
  if key_type ~= 'string' then
    redis.call('DEL', key)
    return 'malformed', nil
  end
  return 'string', redis.call('GET', key)
end
`

var getActivityStateScript = redis.NewScript(readActivityStateLua + `
local status, raw = read_activity_state(KEYS[1])
if status == 'string' then
  return {status, raw}
end
return {status}
`)

// getActivityStateWithLeaseScript is getActivityStateScript plus the expiry
// guard the lifecycle scripts already apply.
//
// A SIBLING rather than an edit: the shared script feeds ActivityStore.Get and
// the reconnect-snapshot pipeline, both of which decode a 1-or-2 element reply.
// Growing a third status there for the reconciler's benefit would change the hot
// path.
//
// The guard is in Lua because that is where the PTTL sentinel is a raw integer.
// In go-redis v9.22.0 the -1/-2 sentinels arrive UNSCALED from DurationCmd --
// one and two NANOSECONDS -- so a Go-side `ttl == -1*time.Second` is permanently
// false and its branch is dead code that reads correctly.
//
// PTTL is atomic with the preceding TYPE inside the script, so -2 cannot occur
// on a key that just typed as 'string'; `<= 0` therefore means exactly "exists
// with no expiry".
var getActivityStateWithLeaseScript = redis.NewScript(readActivityStateLua + `
local status, raw = read_activity_state(KEYS[1])
if status ~= 'string' then
  return {status}
end
if redis.call('PTTL', KEYS[1]) <= 0 then
  return {'unexpiring'}
end
return {status, raw}
`)

// ErrUnexpiringActivityState reports a state key that exists without an expiry.
// It has no 90-second level arm, so its reader must escalate rather than assume
// convergence.
var ErrUnexpiringActivityState = errors.New("rich-presence activity state has no expiry")

var compareAndSetActivityScript = redis.NewScript(decodeActivityStateLua + readActivityStateLua + `
local status, current = read_activity_state(KEYS[1])
if status == 'string' then
  local state = decode_activity_state(current)
  if not state then
    redis.call('DEL', KEYS[1])
  else
    local incoming_version = tonumber(ARGV[2])
    if incoming_version < state.source_version or
        (incoming_version == state.source_version and ARGV[1] ~= state.source_token) then
      return 0
    end
  end
end
redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4])
return 1
`)

var compareAndSetActiveActivityScript = redis.NewScript(decodeActivityStateLua + readActivityStateLua + `
local lifecycle_type = redis.call('TYPE', KEYS[2]).ok
if lifecycle_type == 'none' then
  return 0
end
if lifecycle_type ~= 'hash' then
  redis.call('DEL', KEYS[2])
  return -1
end

local token = redis.call('HGET', KEYS[2], 'token')
local version_raw = redis.call('HGET', KEYS[2], 'version')
local version = tonumber(version_raw)
local active = redis.call('HGET', KEYS[2], 'active')
local compact_token = token and string.gsub(token, '-', '') or ''
local lifecycle_ttl = redis.call('PTTL', KEYS[2])
if redis.call('HLEN', KEYS[2]) ~= 3 or
    not token or string.len(token) ~= 36 or string.len(compact_token) ~= 32 or
    string.lower(token) ~= token or compact_token == '00000000000000000000000000000000' or
    string.sub(token, 9, 9) ~= '-' or string.sub(token, 14, 14) ~= '-' or
    string.sub(token, 19, 19) ~= '-' or string.sub(token, 24, 24) ~= '-' or
    string.find(compact_token, '[^0-9a-f]') or
    not version_raw or string.sub(version_raw, 1, 1) == '0' or
    string.find(version_raw, '[^0-9]') or not version or version <= 0 or
    version > 9007199254740991 or version ~= math.floor(version) or
    (active ~= '0' and active ~= '1') or lifecycle_ttl <= 0 or
    lifecycle_ttl > tonumber(ARGV[4]) then
  redis.call('DEL', KEYS[2])
  return -1
end
if token ~= ARGV[1] or version ~= tonumber(ARGV[2]) or active ~= '1' then
  return 0
end

local status, current = read_activity_state(KEYS[1])
if status == 'string' then
  local state = decode_activity_state(current)
  if not state then
    redis.call('DEL', KEYS[1])
  else
    local incoming_version = tonumber(ARGV[2])
    if incoming_version < state.source_version or
        (incoming_version == state.source_version and ARGV[1] ~= state.source_token) then
      return 0
    end
  end
end
redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4])
return 1
`)

var isActiveActivityGenerationScript = redis.NewScript(`
local lifecycle_type = redis.call('TYPE', KEYS[1]).ok
if lifecycle_type == 'none' then
  return 0
end
if lifecycle_type ~= 'hash' then
  redis.call('DEL', KEYS[1])
  return -1
end

local token = redis.call('HGET', KEYS[1], 'token')
local version_raw = redis.call('HGET', KEYS[1], 'version')
local version = tonumber(version_raw)
local active = redis.call('HGET', KEYS[1], 'active')
local compact_token = token and string.gsub(token, '-', '') or ''
local lifecycle_ttl = redis.call('PTTL', KEYS[1])
if redis.call('HLEN', KEYS[1]) ~= 3 or
    not token or string.len(token) ~= 36 or string.len(compact_token) ~= 32 or
    string.lower(token) ~= token or compact_token == '00000000000000000000000000000000' or
    string.sub(token, 9, 9) ~= '-' or string.sub(token, 14, 14) ~= '-' or
    string.sub(token, 19, 19) ~= '-' or string.sub(token, 24, 24) ~= '-' or
    string.find(compact_token, '[^0-9a-f]') or
    not version_raw or string.sub(version_raw, 1, 1) == '0' or
    string.find(version_raw, '[^0-9]') or not version or version <= 0 or
    version > 9007199254740991 or version ~= math.floor(version) or
    (active ~= '0' and active ~= '1') or lifecycle_ttl <= 0 or
    lifecycle_ttl > tonumber(ARGV[3]) then
  redis.call('DEL', KEYS[1])
  return -1
end
if token == ARGV[1] and version == tonumber(ARGV[2]) and active == '1' then
  return 1
end
return 0
`)

var refreshActivityScript = redis.NewScript(decodeActivityStateLua + readActivityStateLua + `
local status, current = read_activity_state(KEYS[1])
if status == 'none' then
  return 0
end
if status == 'malformed' then
  return -1
end
local state = decode_activity_state(current)
if not state then
  redis.call('DEL', KEYS[1])
  return -1
end
if state.source_token ~= ARGV[1] or state.source_version ~= tonumber(ARGV[2]) then
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

var compareAndDeleteActivityScript = redis.NewScript(decodeActivityStateLua + readActivityStateLua + `
local status, current = read_activity_state(KEYS[1])
if status == 'none' then
  return 0
end
if status == 'malformed' then
  return -1
end
local state = decode_activity_state(current)
if not state then
  redis.call('DEL', KEYS[1])
  return -1
end
if state.source_token ~= ARGV[1] or state.source_version ~= tonumber(ARGV[2]) then
  return 0
end
return redis.call('DEL', KEYS[1])
`)

var compareAndDeleteRawActivityScript = redis.NewScript(readActivityStateLua + `
local status, current = read_activity_state(KEYS[1])
if status == 'string' and current == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// CompareAndSet stores state unless its lifecycle generation is stale.
func (s *ActivityStore) CompareAndSet(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
	state ActivityState,
) (bool, error) {
	key, err := activityKey(userID, category)
	if err != nil {
		return false, err
	}
	if err := validateActivityState(state); err != nil {
		return false, err
	}
	if s == nil || s.redis == nil {
		return false, errors.New("rich-presence activity store unavailable")
	}

	raw, err := json.Marshal(state)
	if err != nil {
		return false, fmt.Errorf("marshal rich-presence activity state: %w", err)
	}
	result, err := compareAndSetActivityScript.Run(
		ctx,
		s.redis,
		[]string{key},
		state.SourceToken.String(),
		state.SourceVersion,
		raw,
		ActivityStateTTL.Milliseconds(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("compare-set rich-presence activity state: %w", err)
	}
	return result == 1, nil
}

// CompareAndSetActive stores a voice activity only while its strict lifecycle
// envelope still names the exact active generation. Redis executes the fence
// check and activity write atomically, closing the post-database publication
// race with a newer terminal event on another control-plane replica.
func (s *ActivityStore) CompareAndSetActive(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
	state ActivityState,
) (bool, error) {
	key, err := activityKey(userID, category)
	if err != nil {
		return false, err
	}
	lifecycleKey, err := VoiceLifecycleKey(userID, category)
	if err != nil {
		return false, err
	}
	if err := validateActivityState(state); err != nil {
		return false, err
	}
	if s == nil || s.redis == nil {
		return false, errors.New("rich-presence activity store unavailable")
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return false, fmt.Errorf("marshal rich-presence activity state: %w", err)
	}
	result, err := compareAndSetActiveActivityScript.Run(
		ctx,
		s.redis,
		[]string{key, lifecycleKey},
		state.SourceToken.String(),
		state.SourceVersion,
		raw,
		ActivityStateTTL.Milliseconds(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("compare-set active rich-presence activity state: %w", err)
	}
	if result == -1 {
		return false, ErrMalformedActivityLifecycle
	}
	return result == 1, nil
}

// IsActiveGeneration atomically verifies that the strict lifecycle envelope
// still names the exact active voice generation. Missing, terminal, or
// successor envelopes return false; poisoned envelopes are deleted and fail
// closed so reconnect snapshots never republish unverifiable activity state.
func (s *ActivityStore) IsActiveGeneration(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
	sourceToken uuid.UUID,
	sourceVersion int64,
) (bool, error) {
	if _, err := activityGenerationKey(
		userID, category, sourceToken, sourceVersion,
	); err != nil {
		return false, err
	}
	lifecycleKey, err := VoiceLifecycleKey(userID, category)
	if err != nil {
		return false, err
	}
	if s == nil || s.redis == nil {
		return false, errors.New("rich-presence activity store unavailable")
	}
	result, err := isActiveActivityGenerationScript.Run(
		ctx,
		s.redis,
		[]string{lifecycleKey},
		sourceToken.String(),
		sourceVersion,
		ActivityStateTTL.Milliseconds(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("verify active rich-presence generation: %w", err)
	}
	if result == -1 {
		return false, ErrMalformedActivityLifecycle
	}
	return result == 1, nil
}

// VerifyActiveGenerations verifies exact lifecycle envelopes in one Redis
// pipeline and preserves input order. Any poisoned envelope is delete-healed
// by its atomic script and fails the whole batch closed.
func (s *ActivityStore) VerifyActiveGenerations(
	ctx context.Context,
	generations []ActivityGeneration,
) ([]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.redis == nil {
		return nil, errors.New("rich-presence activity store unavailable")
	}
	if len(generations) == 0 {
		return []bool{}, nil
	}
	lifecycleKeys, err := activityGenerationLifecycleKeys(generations)
	if err != nil {
		return nil, err
	}
	commands, err := s.queueActiveGenerationVerification(ctx, generations, lifecycleKeys)
	if err != nil {
		return nil, err
	}
	return activeGenerationVerificationResults(commands)
}

func activityGenerationLifecycleKeys(generations []ActivityGeneration) ([]string, error) {
	lifecycleKeys := make([]string, len(generations))
	for index, generation := range generations {
		if _, err := activityGenerationKey(
			generation.UserID,
			generation.Category,
			generation.SourceToken,
			generation.SourceVersion,
		); err != nil {
			return nil, err
		}
		key, err := VoiceLifecycleKey(generation.UserID, generation.Category)
		if err != nil {
			return nil, err
		}
		lifecycleKeys[index] = key
	}
	return lifecycleKeys, nil
}

func (s *ActivityStore) queueActiveGenerationVerification(
	ctx context.Context,
	generations []ActivityGeneration,
	lifecycleKeys []string,
) ([]*redis.Cmd, error) {
	commands := make([]*redis.Cmd, len(generations))
	for start := 0; start < len(generations); start += activityLifecycleVerificationBatchSize {
		end := min(start+activityLifecycleVerificationBatchSize, len(generations))
		_, err := s.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for index := start; index < end; index++ {
				generation := generations[index]
				commands[index] = isActiveActivityGenerationScript.Eval(
					ctx,
					pipe,
					[]string{lifecycleKeys[index]},
					generation.SourceToken.String(),
					generation.SourceVersion,
					ActivityStateTTL.Milliseconds(),
				)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("verify active rich-presence generations: %w", err)
		}
	}
	return commands, nil
}

func activeGenerationVerificationResults(commands []*redis.Cmd) ([]bool, error) {
	results := make([]bool, len(commands))
	for index, command := range commands {
		result, resultErr := command.Int()
		if resultErr != nil {
			return nil, fmt.Errorf("verify active rich-presence generation: %w", resultErr)
		}
		if result == -1 {
			return nil, ErrMalformedActivityLifecycle
		}
		results[index] = result == 1
	}
	return results, nil
}

// Get returns strict current state and removes malformed persisted values.
func (s *ActivityStore) Get(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
) (ActivityState, bool, error) {
	key, err := activityKey(userID, category)
	if err != nil {
		return ActivityState{}, false, err
	}
	if s == nil || s.redis == nil {
		return ActivityState{}, false, errors.New("rich-presence activity store unavailable")
	}

	status, raw, err := activityStateScriptResult(
		getActivityStateScript.Run(ctx, s.redis, []string{key}),
	)
	if err != nil {
		return ActivityState{}, false, fmt.Errorf("read rich-presence activity state: %w", err)
	}
	switch status {
	case "none":
		return ActivityState{}, false, nil
	case "malformed":
		return ActivityState{}, false, ErrMalformedActivityState
	}

	state, err := decodeActivityState(raw)
	if err == nil {
		return state, true, nil
	}
	if deleteErr := compareAndDeleteRawActivityScript.Run(ctx, s.redis, []string{key}, raw).Err(); deleteErr != nil {
		return ActivityState{}, false, errors.Join(
			err,
			fmt.Errorf("delete malformed rich-presence activity state: %w", deleteErr),
		)
	}
	return ActivityState{}, false, err
}

func activityStateScriptResult(command *redis.Cmd) (string, []byte, error) {
	values, err := command.Slice()
	if err != nil {
		return "", nil, err
	}
	switch len(values) {
	case 1:
		status, ok := values[0].(string)
		if ok && (status == "none" || status == "malformed") {
			return status, nil, nil
		}
	case 2:
		status, statusOK := values[0].(string)
		raw, rawOK := values[1].(string)
		if statusOK && rawOK && status == "string" {
			return status, []byte(raw), nil
		}
	}
	return "", nil, errors.New("invalid rich-presence activity script reply")
}

// GetWithLease is Get plus the expiry guard. It is for reconciliation readers
// that must not treat an unexpiring key as self-healing.
func (s *ActivityStore) GetWithLease(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
) (ActivityState, bool, error) {
	key, err := activityKey(userID, category)
	if err != nil {
		return ActivityState{}, false, err
	}
	if s == nil || s.redis == nil {
		return ActivityState{}, false, errors.New("rich-presence activity store unavailable")
	}

	status, raw, err := leasedActivityStateScriptResult(
		getActivityStateWithLeaseScript.Run(ctx, s.redis, []string{key}),
	)
	if err != nil {
		return ActivityState{}, false, fmt.Errorf("read leased rich-presence activity state: %w", err)
	}
	switch status {
	case "none":
		return ActivityState{}, false, nil
	case "malformed":
		return ActivityState{}, false, ErrMalformedActivityState
	case "unexpiring":
		return ActivityState{}, false, ErrUnexpiringActivityState
	}

	state, err := decodeActivityState(raw)
	if err != nil {
		return ActivityState{}, false, err
	}
	return state, true, nil
}

// leasedActivityStateScriptResult decodes the sibling script's reply. The extra
// 'unexpiring' status is absorbed here rather than in
// activityStateScriptResult, so the shared decoder -- and with it
// ActivityStore.Get and the reconnect-snapshot pipeline -- keeps its exact
// two-status vocabulary.
func leasedActivityStateScriptResult(command *redis.Cmd) (string, []byte, error) {
	values, err := command.Slice()
	if err != nil {
		return "", nil, err
	}
	if len(values) == 1 {
		if status, ok := values[0].(string); ok && status == "unexpiring" {
			return status, nil, nil
		}
	}
	return activityStateScriptResult(command)
}

// Refresh renews the TTL only for an exact lifecycle generation.
func (s *ActivityStore) Refresh(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
	sourceToken uuid.UUID,
	sourceVersion int64,
) (bool, error) {
	key, err := activityGenerationKey(userID, category, sourceToken, sourceVersion)
	if err != nil {
		return false, err
	}
	if s == nil || s.redis == nil {
		return false, errors.New("rich-presence activity store unavailable")
	}

	result, err := refreshActivityScript.Run(
		ctx,
		s.redis,
		[]string{key},
		sourceToken.String(),
		sourceVersion,
		ActivityStateTTL.Milliseconds(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("refresh rich-presence activity state: %w", err)
	}
	if result == -1 {
		return false, ErrMalformedActivityState
	}
	return result == 1, nil
}

// CompareAndDelete removes only an exact lifecycle generation.
func (s *ActivityStore) CompareAndDelete(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
	sourceToken uuid.UUID,
	sourceVersion int64,
) (bool, error) {
	key, err := activityGenerationKey(userID, category, sourceToken, sourceVersion)
	if err != nil {
		return false, err
	}
	if s == nil || s.redis == nil {
		return false, errors.New("rich-presence activity store unavailable")
	}

	result, err := compareAndDeleteActivityScript.Run(
		ctx,
		s.redis,
		[]string{key},
		sourceToken.String(),
		sourceVersion,
	).Int()
	if err != nil {
		return false, fmt.Errorf("compare-delete rich-presence activity state: %w", err)
	}
	if result == -1 {
		return false, ErrMalformedActivityState
	}
	return result == 1, nil
}

// Delete removes the one validated sender/category key regardless of lifecycle
// generation. It is reserved for explicit category suppression.
func (s *ActivityStore) Delete(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
) error {
	key, err := activityKey(userID, category)
	if err != nil {
		return err
	}
	if s == nil || s.redis == nil {
		return errors.New("rich-presence activity store unavailable")
	}
	if err := s.redis.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("delete exact rich-presence activity state: %w", err)
	}
	return nil
}

func activityGenerationKey(
	userID uuid.UUID,
	category Category,
	sourceToken uuid.UUID,
	sourceVersion int64,
) (string, error) {
	key, err := activityKey(userID, category)
	if err != nil {
		return "", err
	}
	if sourceToken == uuid.Nil || sourceVersion <= 0 || sourceVersion > MaxActivitySourceVersion {
		return "", ErrInvalidActivityState
	}
	return key, nil
}

func activityKey(userID uuid.UUID, category Category) (string, error) {
	if userID == uuid.Nil {
		return "", ErrInvalidActivityState
	}
	switch category {
	case CategoryServerVoice, CategoryPrivateCall:
		return "presence:rich:" + userID.String() + ":" + string(category), nil
	default:
		return "", ErrInvalidActivityState
	}
}

func validateActivityState(state ActivityState) error {
	payload := bytes.TrimSpace(state.Payload)
	if state.SourceToken == uuid.Nil || state.SourceVersion <= 0 ||
		state.SourceVersion > MaxActivitySourceVersion || state.UpdatedAt <= 0 ||
		state.UpdatedAt > MaxActivityUnixSeconds ||
		len(payload) < 2 || payload[0] != '{' || payload[len(payload)-1] != '}' || !json.Valid(payload) {
		return ErrInvalidActivityState
	}
	return nil
}

func decodeActivityState(raw []byte) (ActivityState, error) {
	var envelope map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&envelope); err != nil {
		return ActivityState{}, fmt.Errorf("%w: %v", ErrMalformedActivityState, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ActivityState{}, fmt.Errorf("%w: trailing JSON", ErrMalformedActivityState)
	}
	if !hasExactActivityEnvelopeFields(envelope) {
		return ActivityState{}, fmt.Errorf("%w: incomplete envelope", ErrMalformedActivityState)
	}

	var sourceTokenText string
	if err := json.Unmarshal(envelope["source_token"], &sourceTokenText); err != nil {
		return ActivityState{}, fmt.Errorf("%w: invalid source token", ErrMalformedActivityState)
	}
	sourceToken, err := uuid.Parse(sourceTokenText)
	if err != nil || sourceToken == uuid.Nil || sourceToken.String() != sourceTokenText {
		return ActivityState{}, fmt.Errorf("%w: noncanonical source token", ErrMalformedActivityState)
	}

	var sourceVersion *int64
	var minimized *bool
	var updatedAt *int64
	if err := json.Unmarshal(envelope["source_version"], &sourceVersion); err != nil || sourceVersion == nil {
		return ActivityState{}, fmt.Errorf("%w: invalid source version", ErrMalformedActivityState)
	}
	if err := json.Unmarshal(envelope["minimized"], &minimized); err != nil || minimized == nil {
		return ActivityState{}, fmt.Errorf("%w: invalid minimized flag", ErrMalformedActivityState)
	}
	if err := json.Unmarshal(envelope["updated_at"], &updatedAt); err != nil || updatedAt == nil {
		return ActivityState{}, fmt.Errorf("%w: invalid update time", ErrMalformedActivityState)
	}

	state := ActivityState{
		SourceToken:   sourceToken,
		SourceVersion: *sourceVersion,
		Minimized:     *minimized,
		Payload:       envelope["payload"],
		UpdatedAt:     *updatedAt,
	}
	if err := validateActivityState(state); err != nil {
		return ActivityState{}, fmt.Errorf("%w: invalid envelope", ErrMalformedActivityState)
	}
	return state, nil
}

func hasExactActivityEnvelopeFields(envelope map[string]json.RawMessage) bool {
	if len(envelope) != 5 {
		return false
	}
	for _, field := range []string{
		"source_token",
		"source_version",
		"minimized",
		"payload",
		"updated_at",
	} {
		if _, ok := envelope[field]; !ok {
			return false
		}
	}
	return true
}
