package presence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// A reconnect or one media lifecycle event may touch many senders, but it
	// must never turn that fan-out into an unbounded number of authoritative
	// call reads. These request-local limits align with the snapshot's raw-row
	// and exact-Redis batch bounds.
	activityBuildPrivateCallLimit        = 64
	activityBuildPrivateParticipantLimit = 512
)

var (
	// ErrInvalidActivityScope rejects malformed server-owned lifecycle input.
	ErrInvalidActivityScope = errors.New("invalid rich-presence activity scope")
	// ErrActivityNotCurrent means the requested sender/scope is not uniquely authoritative.
	ErrActivityNotCurrent = errors.New("rich-presence activity is not current")
	// ErrActivityBuildWorkLimit prevents nested private-call reconstruction
	// from amplifying one bounded reconnect or media event.
	ErrActivityBuildWorkLimit = errors.New("rich-presence activity build work limit exceeded")
)

// Scope identifies one trusted media lifecycle generation.
type Scope struct {
	Category    Category
	RoomID      uuid.UUID
	LifecycleID uuid.UUID
	EventAt     time.Time
}

// BuiltActivity carries trusted policy input and its persisted generation.
type BuiltActivity struct {
	Input         PolicyInput
	SourceToken   uuid.UUID
	SourceVersion int64
}

// CallLeaseVerifier checks the exact active call identity for a conversation.
type CallLeaseVerifier interface {
	Matches(ctx context.Context, conversationID, callID uuid.UUID) (bool, error)
}

// ActiveGenerationVerifier proves exact lifecycle generations before trusted
// participant rows can influence a Private Call audience or payload.
type ActiveGenerationVerifier interface {
	VerifyActiveGenerations(context.Context, []ActivityGeneration) ([]bool, error)
}

// ActivityBuilder derives Rich Presence only from current authoritative state.
type ActivityBuilder struct {
	db         DBTX
	leases     CallLeaseVerifier
	lifecycles ActiveGenerationVerifier
}

type activityBuildCacheContextKey struct{}

type activityBuildCache struct {
	mu sync.Mutex

	privateCalls        map[privateCallBuildCacheKey]privateCallBuildCacheResult
	privateCallLoads    int
	privateParticipants int
}

type privateCallBuildCacheKey struct {
	conversationID uuid.UUID
	callID         uuid.UUID
}

type privateCallBuildCacheResult struct {
	snapshot *privateCallBuildSnapshot
	err      error
}

type privateCallBuildSnapshot struct {
	key            privateCallBuildCacheKey
	callType       string
	participantIDs []uuid.UUID
	participantAt  map[uuid.UUID]time.Time
	participants   map[uuid.UUID]bool
}

// WithActivityBuildCache starts one bounded, request/event-local authoritative
// build epoch. It is idempotent for callers that need to pass the same epoch
// through several sender builds. The cache is never global and must not be
// retained across requests or independent authorization passes.
func WithActivityBuildCache(ctx context.Context) context.Context {
	if activityBuildCacheFromContext(ctx) != nil {
		return ctx
	}
	return withFreshActivityBuildCache(ctx)
}

func withFreshActivityBuildCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, activityBuildCacheContextKey{}, &activityBuildCache{
		privateCalls: make(map[privateCallBuildCacheKey]privateCallBuildCacheResult),
	})
}

// InvalidateActivityBuildCache discards authoritative snapshots after an
// applied mutation while retaining the epoch's consumed-work counters. A
// lifecycle event therefore cannot replenish its own hard budget by mutating.
func InvalidateActivityBuildCache(ctx context.Context) {
	cache := activityBuildCacheFromContext(ctx)
	if cache == nil {
		return
	}
	cache.mu.Lock()
	clear(cache.privateCalls)
	cache.mu.Unlock()
}

func activityBuildCacheFromContext(ctx context.Context) *activityBuildCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(activityBuildCacheContextKey{}).(*activityBuildCache)
	return cache
}

func activityBuildCacheOwnsPrivateSnapshot(
	ctx context.Context,
	snapshot *privateCallBuildSnapshot,
) bool {
	cache := activityBuildCacheFromContext(ctx)
	if cache == nil || snapshot == nil {
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	result, found := cache.privateCalls[snapshot.key]
	return found && result.err == nil && result.snapshot == snapshot
}

// NewActivityBuilder creates a trusted activity builder.
func NewActivityBuilder(
	db DBTX,
	leases CallLeaseVerifier,
	lifecycles ...ActiveGenerationVerifier,
) *ActivityBuilder {
	builder := &ActivityBuilder{db: db, leases: leases}
	if len(lifecycles) > 0 {
		builder.lifecycles = lifecycles[0]
	}
	return builder
}

// Build derives one category-specific activity for senderID.
func (b *ActivityBuilder) Build(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
) (BuiltActivity, error) {
	if err := validateActivityScope(senderID, scope); err != nil {
		return BuiltActivity{}, err
	}
	if b == nil || b.db == nil {
		return BuiltActivity{}, errors.New("rich-presence activity builder unavailable")
	}

	switch scope.Category {
	case CategoryServerVoice:
		return b.buildServerVoice(ctx, senderID, scope)
	case CategoryPrivateCall:
		return b.buildPrivateCall(ctx, senderID, scope)
	default:
		return BuiltActivity{}, ErrInvalidActivityScope
	}
}

func validateActivityScope(senderID uuid.UUID, scope Scope) error {
	if senderID == uuid.Nil || scope.RoomID == uuid.Nil || scope.LifecycleID == uuid.Nil ||
		!IsValidActivitySourceTime(scope.EventAt) {
		return ErrInvalidActivityScope
	}
	switch scope.Category {
	case CategoryServerVoice:
		if scope.RoomID != scope.LifecycleID {
			return ErrInvalidActivityScope
		}
	case CategoryPrivateCall:
	default:
		return ErrInvalidActivityScope
	}
	return nil
}

func (b *ActivityBuilder) buildServerVoice(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
) (BuiltActivity, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT vp.channel_id, c.server_id, c.name, s.name,
		       c.type = 'voice', sm.user_id IS NOT NULL,
		       vp.joined_at, vp.lifecycle_event_at
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		JOIN servers s ON s.id = c.server_id
		LEFT JOIN server_members sm
		  ON sm.server_id = c.server_id AND sm.user_id = vp.user_id
		WHERE vp.user_id = $1
		ORDER BY vp.channel_id
		LIMIT 2
	`, senderID)
	if err != nil {
		return BuiltActivity{}, fmt.Errorf("query server voice activity: %w", err)
	}

	var (
		found       bool
		ambiguous   bool
		channelID   uuid.UUID
		serverID    uuid.UUID
		channelName string
		serverName  string
		isVoice     bool
		isMember    bool
		joinedAt    time.Time
		lifecycleAt time.Time
	)
	err = readActivityRows(rows, func() error {
		if found {
			ambiguous = true
		}
		found = true
		if err := rows.Scan(
			&channelID,
			&serverID,
			&channelName,
			&serverName,
			&isVoice,
			&isMember,
			&joinedAt,
			&lifecycleAt,
		); err != nil {
			return fmt.Errorf("scan server voice activity row: %w", err)
		}
		return nil
	})
	if err != nil {
		return BuiltActivity{}, err
	}
	if !found || ambiguous || channelID != scope.RoomID || !isVoice || !isMember ||
		!validPresenceName(channelName) || !validPresenceName(serverName) ||
		joinedAt.Unix() <= 0 || joinedAt.Unix() > MaxActivityUnixSeconds ||
		!IsValidActivitySourceTime(lifecycleAt) {
		return BuiltActivity{}, fmt.Errorf("%w: server voice scope", ErrActivityNotCurrent)
	}

	startedAt := joinedAt.Unix()
	return BuiltActivity{
		Input: PolicyInput{
			SenderID: senderID,
			Category: CategoryServerVoice,
			ServerVoice: &ServerVoicePolicyInput{
				Context: ServerVoiceContext{ServerID: serverID, ChannelID: channelID},
				Payload: ServerVoicePayload{
					ChannelID: channelID, ChannelName: channelName,
					ServerID: serverID, ServerName: serverName,
					StartedAt: &startedAt,
				},
			},
		},
		SourceToken:   channelID,
		SourceVersion: lifecycleAt.UnixMicro(),
	}, nil
}

func (b *ActivityBuilder) buildPrivateCall(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
) (BuiltActivity, error) {
	if err := ctx.Err(); err != nil {
		return BuiltActivity{}, err
	}
	if b.leases == nil {
		return BuiltActivity{}, errors.New("private call lease verifier unavailable")
	}
	if b.lifecycles == nil {
		return BuiltActivity{}, errors.New("private call participant lifecycle verifier unavailable")
	}

	key := privateCallBuildCacheKey{
		conversationID: scope.RoomID,
		callID:         scope.LifecycleID,
	}
	var (
		snapshot *privateCallBuildSnapshot
		err      error
	)
	if cache := activityBuildCacheFromContext(ctx); cache != nil {
		snapshot, err = cache.loadPrivateCall(ctx, key, func(queryLimit, participantBudget int) (
			*privateCallBuildSnapshot,
			int,
			error,
		) {
			return b.loadPrivateCallBuildSnapshot(ctx, key, queryLimit, participantBudget)
		})
	} else {
		snapshot, _, err = b.loadPrivateCallBuildSnapshot(
			ctx,
			key,
			maxPrivateCallParticipants+1,
			maxPrivateCallParticipants+1,
		)
	}
	if err != nil {
		return BuiltActivity{}, err
	}
	return snapshot.build(senderID)
}

func (c *activityBuildCache) loadPrivateCall(
	ctx context.Context,
	key privateCallBuildCacheKey,
	load func(int, int) (*privateCallBuildSnapshot, int, error),
) (*privateCallBuildSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result, found := c.privateCalls[key]; found {
		return result.snapshot, result.err
	}
	if c.privateCallLoads >= activityBuildPrivateCallLimit ||
		c.privateParticipants >= activityBuildPrivateParticipantLimit {
		result := privateCallBuildCacheResult{err: ErrActivityBuildWorkLimit}
		c.privateCalls[key] = result
		return nil, result.err
	}

	c.privateCallLoads++
	remaining := activityBuildPrivateParticipantLimit - c.privateParticipants
	queryLimit := min(maxPrivateCallParticipants+1, remaining+1)
	snapshot, rawParticipants, err := load(queryLimit, remaining)
	if rawParticipants > remaining {
		c.privateParticipants = activityBuildPrivateParticipantLimit
		snapshot = nil
		err = ErrActivityBuildWorkLimit
	} else {
		c.privateParticipants += rawParticipants
	}
	result := privateCallBuildCacheResult{snapshot: snapshot, err: err}
	c.privateCalls[key] = result
	return result.snapshot, result.err
}

func (b *ActivityBuilder) loadPrivateCallBuildSnapshot(
	ctx context.Context,
	key privateCallBuildCacheKey,
	queryLimit int,
	participantBudget int,
) (*privateCallBuildSnapshot, int, error) {
	if queryLimit < 1 || queryLimit > maxPrivateCallParticipants+1 {
		return nil, 0, ErrActivityBuildWorkLimit
	}
	if err := b.verifyPrivateCallBuildLease(ctx, key, false); err != nil {
		return nil, 0, err
	}

	rows, err := b.db.QueryContext(ctx, `
		SELECT dc.is_group, participant.user_id, participant.lifecycle_event_at,
		       member.user_id IS NOT NULL,
		       EXISTS (
		           SELECT 1
		           FROM dm_voice_participants other_scope
		           WHERE other_scope.user_id = participant.user_id
		             AND other_scope.conversation_id <> participant.conversation_id
		       )
		FROM dm_conversations dc
		JOIN dm_voice_participants participant
		  ON participant.conversation_id = dc.id
		LEFT JOIN dm_participants member
		  ON member.conversation_id = participant.conversation_id
		 AND member.user_id = participant.user_id
		WHERE dc.id = $1
		ORDER BY participant.user_id
		LIMIT $2
	`, key.conversationID, queryLimit)
	if err != nil {
		return nil, 0, fmt.Errorf("query private call activity: %w", err)
	}
	loaded, err := readPrivateCallBuildRows(rows)
	if err != nil {
		return nil, loaded.rawParticipants, err
	}
	if loaded.rawParticipants > participantBudget {
		return nil, loaded.rawParticipants, ErrActivityBuildWorkLimit
	}
	if !loaded.valid() {
		return nil, loaded.rawParticipants, fmt.Errorf("%w: private call scope", ErrActivityNotCurrent)
	}
	if err := b.verifyPrivateCallBuildGenerations(ctx, key.callID, loaded); err != nil {
		return nil, loaded.rawParticipants, err
	}
	if err := b.verifyPrivateCallBuildLease(ctx, key, true); err != nil {
		return nil, loaded.rawParticipants, err
	}
	return loaded.snapshot(key), loaded.rawParticipants, nil
}

type privateCallBuildRows struct {
	found              bool
	isGroup            bool
	participantIDs     []uuid.UUID
	participants       map[uuid.UUID]bool
	participantAt      map[uuid.UUID]time.Time
	invalidParticipant bool
	rawParticipants    int
}

func readPrivateCallBuildRows(rows *sql.Rows) (loaded privateCallBuildRows, returnErr error) {
	loaded.participants = make(map[uuid.UUID]bool)
	loaded.participantAt = make(map[uuid.UUID]time.Time)
	returnErr = readActivityRows(rows, func() error { return loaded.scan(rows) })
	return loaded, returnErr
}

func (loaded *privateCallBuildRows) scan(rows *sql.Rows) error {
	var (
		rowIsGroup       bool
		participantID    uuid.UUID
		participantEvent time.Time
		isMember         bool
		ambiguous        bool
	)
	if err := rows.Scan(
		&rowIsGroup,
		&participantID,
		&participantEvent,
		&isMember,
		&ambiguous,
	); err != nil {
		return fmt.Errorf("scan private call activity row: %w", err)
	}
	loaded.rawParticipants++
	if !loaded.found {
		loaded.found = true
		loaded.isGroup = rowIsGroup
	} else if rowIsGroup != loaded.isGroup {
		return errors.New("inconsistent private call activity rows")
	}
	if !isMember || ambiguous || participantID == uuid.Nil || loaded.participants[participantID] ||
		!IsValidActivitySourceTime(participantEvent) {
		loaded.invalidParticipant = true
		return nil
	}
	loaded.participants[participantID] = true
	loaded.participantAt[participantID] = participantEvent
	loaded.participantIDs = append(loaded.participantIDs, participantID)
	return nil
}

func (loaded privateCallBuildRows) valid() bool {
	return loaded.found && !loaded.invalidParticipant && len(loaded.participantIDs) >= 1 &&
		len(loaded.participantIDs) <= maxPrivateCallParticipants
}

func (loaded privateCallBuildRows) snapshot(key privateCallBuildCacheKey) *privateCallBuildSnapshot {
	callType := "dm"
	if loaded.isGroup {
		callType = "group"
	}
	return &privateCallBuildSnapshot{
		key:            key,
		callType:       callType,
		participantIDs: loaded.participantIDs,
		participantAt:  loaded.participantAt,
		participants:   loaded.participants,
	}
}

func (b *ActivityBuilder) verifyPrivateCallBuildGenerations(
	ctx context.Context,
	callID uuid.UUID,
	loaded privateCallBuildRows,
) error {
	generations := make([]ActivityGeneration, 0, len(loaded.participantIDs))
	for _, participantID := range loaded.participantIDs {
		generations = append(generations, ActivityGeneration{
			UserID: participantID, Category: CategoryPrivateCall,
			SourceToken:   callID,
			SourceVersion: loaded.participantAt[participantID].UnixMicro(),
		})
	}
	activeGenerations, err := b.lifecycles.VerifyActiveGenerations(ctx, generations)
	if err != nil {
		return fmt.Errorf("verify private call participant lifecycles: %w", err)
	}
	if len(activeGenerations) != len(generations) {
		return errors.New("invalid private call participant lifecycle verification")
	}
	for _, active := range activeGenerations {
		if !active {
			return fmt.Errorf(
				"%w: private call participant lifecycle", ErrActivityNotCurrent,
			)
		}
	}
	return nil
}

func (b *ActivityBuilder) verifyPrivateCallBuildLease(
	ctx context.Context,
	key privateCallBuildCacheKey,
	recheck bool,
) error {
	matches, err := b.leases.Matches(ctx, key.conversationID, key.callID)
	if err != nil {
		operation := "verify"
		if recheck {
			operation = "reverify"
		}
		return fmt.Errorf("%s private call lease: %w", operation, err)
	}
	if !matches {
		suffix := ""
		if recheck {
			suffix = " rotated"
		}
		return fmt.Errorf("%w: private call lease%s", ErrActivityNotCurrent, suffix)
	}
	return nil
}

func (s *privateCallBuildSnapshot) build(senderID uuid.UUID) (BuiltActivity, error) {
	if s == nil || !s.participants[senderID] {
		return BuiltActivity{}, fmt.Errorf("%w: private call sender", ErrActivityNotCurrent)
	}
	lifecycleAt := s.participantAt[senderID]
	return BuiltActivity{
		Input: PolicyInput{
			SenderID: senderID,
			Category: CategoryPrivateCall,
			PrivateCall: &PrivateCallPolicyInput{
				Context: PrivateCallContext{
					ConversationID: s.key.conversationID,
					ParticipantIDs: s.participantIDs,
				},
				Payload: PrivateCallPayload{
					CallType: s.callType, ParticipantCount: len(s.participantIDs),
				},
				buildSnapshot: s,
			},
		},
		SourceToken:   s.key.callID,
		SourceVersion: lifecycleAt.UnixMicro(),
	}, nil
}

func readActivityRows(rows *sql.Rows, scan func() error) (returnErr error) {
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("close rich-presence activity rows: %w", closeErr),
			)
		}
	}()
	for rows.Next() {
		if err := scan(); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rich-presence activity rows: %w", err)
	}
	return nil
}
