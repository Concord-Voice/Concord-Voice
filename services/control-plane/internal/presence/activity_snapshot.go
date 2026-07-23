package presence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	activitySnapshotCandidateLimit = 512
	activitySnapshotRedisBatchSize = 64
)

var (
	// ErrActivitySnapshotCandidateLimit prevents unbounded reconnect work.
	ErrActivitySnapshotCandidateLimit = errors.New("rich-presence snapshot candidate limit exceeded")
	// ErrInvalidActivitySnapshot rejects invalid service input or trusted output.
	ErrInvalidActivitySnapshot = errors.New("invalid rich-presence snapshot")
)

// ActivitySnapshotEntry is one freshly authorized reconnect projection.
type ActivitySnapshotEntry struct {
	Minimized bool
	Payload   json.RawMessage
	UpdatedAt int64

	// projection is retained only until the Hub's publication barrier. It lets
	// finalization reauthorize the exact generation without rebuilding from a
	// stale candidate query. The websocket shape exposes only the fields above.
	projection activitySnapshotProjection
}

type activitySnapshotProjection struct {
	SourceToken    uuid.UUID
	SourceVersion  int64
	Scope          Scope
	ParticipantIDs []uuid.UUID
}

// ActivitySnapshot groups current Rich Presence by sender and category.
type ActivitySnapshot map[uuid.UUID]map[Category]ActivitySnapshotEntry

type activitySnapshotKey struct {
	SenderID uuid.UUID
	Category Category
}

type activitySnapshotCandidate struct {
	activitySnapshotKey
	RoomID      uuid.UUID
	LifecycleAt time.Time
}

type activitySnapshotCandidateGroup struct {
	Candidate        activitySnapshotCandidate
	Ambiguous        bool
	MaxSourceVersion int64
}

type activitySnapshotCandidateLoader func(
	context.Context,
	uuid.UUID,
) ([]activitySnapshotCandidate, error)

// SenderPublicationCoordinator holds the process-local writer gates for every
// sender represented by one reconnect publication.
type SenderPublicationCoordinator interface {
	WithSenders(context.Context, []uuid.UUID, func() error) error
}

// ActivitySnapshotService rebuilds authorized reconnect state from exact
// Redis keys and authoritative database state.
type ActivitySnapshotService struct {
	db              DBTX
	builder         activityBuilder
	store           *ActivityStore
	authorize       activityAuthorizer
	candidateLoader activitySnapshotCandidateLoader
	coordinator     SenderPublicationCoordinator
}

// NewActivitySnapshotService creates the bounded current-state snapshot reader.
func NewActivitySnapshotService(
	db DBTX,
	builder *ActivityBuilder,
	store *ActivityStore,
	visibility ChannelVisibilityResolver,
	coordinator SenderPublicationCoordinator,
) *ActivitySnapshotService {
	return newActivitySnapshotService(
		db,
		builder,
		store,
		func(ctx context.Context, input PolicyInput) (Decision, error) {
			return AuthorizeAndMinimize(ctx, db, visibility, input)
		},
		coordinator,
	)
}

func newActivitySnapshotService(
	db DBTX,
	builder activityBuilder,
	store *ActivityStore,
	authorize activityAuthorizer,
	coordinators ...SenderPublicationCoordinator,
) *ActivitySnapshotService {
	service := &ActivitySnapshotService{
		db: db, builder: builder, store: store, authorize: authorize,
	}
	if len(coordinators) > 0 {
		service.coordinator = coordinators[0]
	}
	return service
}

// Snapshot returns only state freshly authorized for viewerID. Any global read
// failure returns no partial replacement.
func (s *ActivitySnapshotService) Snapshot(
	ctx context.Context,
	viewerID uuid.UUID,
) (ActivitySnapshot, error) {
	if err := validateActivitySnapshotRequest(ctx, s, viewerID); err != nil {
		return nil, err
	}
	ctx = withFreshActivityBuildCache(ctx)
	candidates, err := s.loadSnapshotCandidates(ctx, viewerID)
	if err != nil {
		return nil, err
	}
	groups, err := groupActivitySnapshotCandidates(candidates)
	if err != nil {
		return nil, err
	}
	keys := sortedActivitySnapshotKeys(groups)
	states, err := loadActivitySnapshotStates(ctx, s.store, keys)
	if err != nil {
		return nil, err
	}
	if err := s.removeInactiveSnapshotStates(ctx, keys, states); err != nil {
		return nil, err
	}

	out := make(ActivitySnapshot)
	for _, key := range keys {
		state, found := states[key]
		if !found {
			continue
		}
		entry, included, err := s.snapshotCandidateEntry(ctx, viewerID, key, groups[key], state)
		if err != nil {
			return nil, err
		}
		if included {
			addActivitySnapshotEntry(out, key, entry)
		}
	}
	return out, nil
}

func validateActivitySnapshotRequest(
	ctx context.Context,
	service *ActivitySnapshotService,
	viewerID uuid.UUID,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if viewerID == uuid.Nil || service == nil || service.builder == nil || service.store == nil ||
		service.store.redis == nil || service.authorize == nil {
		return ErrInvalidActivitySnapshot
	}
	return nil
}

func (s *ActivitySnapshotService) loadSnapshotCandidates(
	ctx context.Context,
	viewerID uuid.UUID,
) ([]activitySnapshotCandidate, error) {
	loader := s.candidateLoader
	if loader == nil {
		if s.db == nil {
			return nil, ErrInvalidActivitySnapshot
		}
		loader = s.loadCandidates
	}
	candidates, err := loader(ctx, viewerID)
	if err != nil {
		return nil, fmt.Errorf("load rich-presence snapshot candidates: %w", err)
	}
	return candidates, nil
}

func sortedActivitySnapshotKeys[T any](entries map[activitySnapshotKey]T) []activitySnapshotKey {
	keys := make([]activitySnapshotKey, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].SenderID != keys[right].SenderID {
			return keys[left].SenderID.String() < keys[right].SenderID.String()
		}
		return keys[left].Category < keys[right].Category
	})
	return keys
}

func (s *ActivitySnapshotService) snapshotCandidateEntry(
	ctx context.Context,
	viewerID uuid.UUID,
	key activitySnapshotKey,
	group activitySnapshotCandidateGroup,
	state ActivityState,
) (ActivitySnapshotEntry, bool, error) {
	if group.Ambiguous {
		return ActivitySnapshotEntry{}, false, s.deleteSnapshotGenerationAtMost(
			ctx, key, state, group.MaxSourceVersion,
		)
	}
	built, scope, current, err := s.loadCurrentSnapshotCandidate(ctx, key, group, state)
	if err != nil || !current {
		return ActivitySnapshotEntry{}, false, err
	}
	return s.authorizeSnapshotCandidate(ctx, viewerID, key, state, built, scope)
}

func (s *ActivitySnapshotService) loadCurrentSnapshotCandidate(
	ctx context.Context,
	key activitySnapshotKey,
	group activitySnapshotCandidateGroup,
	state ActivityState,
) (BuiltActivity, Scope, bool, error) {
	scope := snapshotCandidateScope(key, group.Candidate, state)
	built, err := s.builder.Build(ctx, key.SenderID, scope)
	if err != nil {
		if !errors.Is(err, ErrActivityNotCurrent) {
			return BuiltActivity{}, Scope{}, false, fmt.Errorf("build rich-presence snapshot candidate: %w", err)
		}
		deleteErr := s.deleteSnapshotGenerationAtMost(ctx, key, state, group.MaxSourceVersion)
		return BuiltActivity{}, Scope{}, false, deleteErr
	}
	if built.SourceToken != state.SourceToken || built.SourceVersion != state.SourceVersion {
		deleteErr := s.deleteSnapshotGenerationAtMost(ctx, key, state, built.SourceVersion)
		return BuiltActivity{}, Scope{}, false, deleteErr
	}
	if built.Input.SenderID != key.SenderID || built.Input.Category != key.Category {
		return BuiltActivity{}, Scope{}, false, ErrInvalidActivitySnapshot
	}
	// The shortlist may lag an already-authoritative Redis/DB generation.
	// Retain one internally consistent scope for publication finalization.
	scope.EventAt = time.UnixMicro(built.SourceVersion)
	return built, scope, true, nil
}

func snapshotCandidateScope(
	key activitySnapshotKey,
	candidate activitySnapshotCandidate,
	state ActivityState,
) Scope {
	scope := Scope{
		Category: key.Category, RoomID: candidate.RoomID,
		LifecycleID: state.SourceToken, EventAt: candidate.LifecycleAt,
	}
	if key.Category == CategoryServerVoice {
		// The exact stored generation names the current channel. Using it here
		// lets the builder validate a move that raced the candidate query.
		scope.RoomID = state.SourceToken
	}
	return scope
}

func (s *ActivitySnapshotService) deleteSnapshotGenerationAtMost(
	ctx context.Context,
	key activitySnapshotKey,
	state ActivityState,
	observedVersion int64,
) error {
	if state.SourceVersion > observedVersion {
		return nil
	}
	return s.deleteSnapshotGeneration(ctx, key, state)
}

func (s *ActivitySnapshotService) authorizeSnapshotCandidate(
	ctx context.Context,
	viewerID uuid.UUID,
	key activitySnapshotKey,
	state ActivityState,
	built BuiltActivity,
	scope Scope,
) (ActivitySnapshotEntry, bool, error) {
	decision, err := s.authorize(ctx, built.Input)
	if err != nil {
		return ActivitySnapshotEntry{}, false, fmt.Errorf("authorize rich-presence snapshot candidate: %w", err)
	}
	if !decision.Audience[viewerID] {
		return ActivitySnapshotEntry{}, false, nil
	}
	if !validActivitySnapshotPayload(decision.Payload) {
		return ActivitySnapshotEntry{}, false, ErrInvalidActivitySnapshot
	}
	projection, err := snapshotCandidateProjection(key, built, scope)
	if err != nil {
		return ActivitySnapshotEntry{}, false, err
	}
	return ActivitySnapshotEntry{
		Minimized: decision.Minimized, Payload: append(json.RawMessage(nil), decision.Payload...),
		UpdatedAt: state.UpdatedAt, projection: projection,
	}, true, nil
}

func snapshotCandidateProjection(
	key activitySnapshotKey,
	built BuiltActivity,
	scope Scope,
) (activitySnapshotProjection, error) {
	projection := activitySnapshotProjection{
		SourceToken: built.SourceToken, SourceVersion: built.SourceVersion, Scope: scope,
	}
	if key.Category != CategoryPrivateCall {
		return projection, nil
	}
	if built.Input.PrivateCall == nil || !validActivitySnapshotParticipantIDs(
		built.Input.PrivateCall.Context.ParticipantIDs, key.SenderID,
	) {
		return activitySnapshotProjection{}, ErrInvalidActivitySnapshot
	}
	projection.ParticipantIDs = append(
		[]uuid.UUID(nil), built.Input.PrivateCall.Context.ParticipantIDs...,
	)
	return projection, nil
}

func validActivitySnapshotPayload(payload json.RawMessage) bool {
	trimmed := bytes.TrimSpace(payload)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' &&
		json.Valid(trimmed)
}

func addActivitySnapshotEntry(
	out ActivitySnapshot,
	key activitySnapshotKey,
	entry ActivitySnapshotEntry,
) {
	if out[key.SenderID] == nil {
		out[key.SenderID] = make(map[Category]ActivitySnapshotEntry)
	}
	out[key.SenderID][key.Category] = entry
}

// FinalizeSnapshot reloads every projected Redis generation, rebuilds its
// authoritative scope (including private-call lease verification), and performs
// a fresh authorization pass while holding every represented sender's local
// writer gate. publish must marshal, capacity-check, and atomically enqueue the
// replacement; it runs before those gates are released. The coordinator is
// intentionally process-local. Exact Redis checks reject cross-process
// lifecycle changes committed before the final read; changes after that read
// and distributed settings serialization require a future cross-replica
// publication coordinator outside the current single-Hub delivery architecture.
// Missing or superseded generations and newly excluded viewers are omitted;
// a global read or policy failure returns no partial replacement.
func (s *ActivitySnapshotService) FinalizeSnapshot(
	ctx context.Context,
	viewerID uuid.UUID,
	projected ActivitySnapshot,
	publish func(ActivitySnapshot) error,
) error {
	if err := validateActivitySnapshotFinalizationRequest(ctx, s, viewerID, publish); err != nil {
		return err
	}
	keys, gateIDs, err := prepareActivitySnapshotFinalization(projected)
	if err != nil {
		return err
	}

	ctx = withFreshActivityBuildCache(ctx)
	return s.coordinator.WithSenders(ctx, gateIDs, func() error {
		finalized, err := s.finalizeSnapshotAlreadyGated(ctx, viewerID, projected, keys)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return publish(finalized)
	})
}

func validateActivitySnapshotFinalizationRequest(
	ctx context.Context,
	service *ActivitySnapshotService,
	viewerID uuid.UUID,
	publish func(ActivitySnapshot) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if viewerID == uuid.Nil || service == nil || service.builder == nil || service.store == nil ||
		service.store.redis == nil || service.authorize == nil || service.coordinator == nil || publish == nil {
		return ErrInvalidActivitySnapshot
	}
	return nil
}

func prepareActivitySnapshotFinalization(
	projected ActivitySnapshot,
) ([]activitySnapshotKey, []uuid.UUID, error) {
	keySet := make(map[activitySnapshotKey]bool)
	gateIDSet := make(map[uuid.UUID]bool, len(projected))
	for senderID, entries := range projected {
		if senderID == uuid.Nil {
			return nil, nil, ErrInvalidActivitySnapshot
		}
		gateIDSet[senderID] = true
		if err := collectProjectedActivityEntries(senderID, entries, keySet, gateIDSet); err != nil {
			return nil, nil, err
		}
	}
	return sortedActivitySnapshotKeys(keySet), sortedActivitySnapshotGateIDs(gateIDSet), nil
}

func collectProjectedActivityEntries(
	senderID uuid.UUID,
	entries map[Category]ActivitySnapshotEntry,
	keySet map[activitySnapshotKey]bool,
	gateIDSet map[uuid.UUID]bool,
) error {
	for category, entry := range entries {
		participantIDs, err := validateProjectedActivityEntry(senderID, category, entry)
		if err != nil {
			return err
		}
		keySet[activitySnapshotKey{SenderID: senderID, Category: category}] = true
		for _, participantID := range participantIDs {
			gateIDSet[participantID] = true
		}
	}
	return nil
}

func validateProjectedActivityEntry(
	senderID uuid.UUID,
	category Category,
	entry ActivitySnapshotEntry,
) ([]uuid.UUID, error) {
	if category != CategoryServerVoice && category != CategoryPrivateCall {
		return nil, ErrInvalidActivitySnapshot
	}
	projection := entry.projection
	if projection.SourceToken == uuid.Nil || projection.SourceVersion <= 0 ||
		projection.Scope.Category != category ||
		projection.Scope.LifecycleID != projection.SourceToken ||
		projection.Scope.EventAt.UnixMicro() != projection.SourceVersion ||
		validateActivityScope(senderID, projection.Scope) != nil {
		return nil, ErrInvalidActivitySnapshot
	}
	if category == CategoryServerVoice {
		if len(projection.ParticipantIDs) != 0 {
			return nil, ErrInvalidActivitySnapshot
		}
		return nil, nil
	}
	if !validActivitySnapshotParticipantIDs(projection.ParticipantIDs, senderID) {
		return nil, ErrInvalidActivitySnapshot
	}
	return projection.ParticipantIDs, nil
}

func sortedActivitySnapshotGateIDs(gateIDSet map[uuid.UUID]bool) []uuid.UUID {
	gateIDs := make([]uuid.UUID, 0, len(gateIDSet))
	for gateID := range gateIDSet {
		gateIDs = append(gateIDs, gateID)
	}
	sort.Slice(gateIDs, func(left, right int) bool {
		return gateIDs[left].String() < gateIDs[right].String()
	})
	return gateIDs
}

func (s *ActivitySnapshotService) finalizeSnapshotAlreadyGated(
	ctx context.Context,
	viewerID uuid.UUID,
	projected ActivitySnapshot,
	keys []activitySnapshotKey,
) (ActivitySnapshot, error) {
	states, err := loadActivitySnapshotStates(ctx, s.store, keys)
	if err != nil {
		return nil, err
	}
	if err := s.removeInactiveSnapshotStates(ctx, keys, states); err != nil {
		return nil, err
	}
	out := make(ActivitySnapshot)
	for _, key := range keys {
		entry, included, err := s.finalizedSnapshotEntry(ctx, viewerID, key, projected, states)
		if err != nil {
			return nil, err
		}
		if included {
			addActivitySnapshotEntry(out, key, entry)
		}
	}
	return out, nil
}

func (s *ActivitySnapshotService) finalizedSnapshotEntry(
	ctx context.Context,
	viewerID uuid.UUID,
	key activitySnapshotKey,
	projected ActivitySnapshot,
	states map[activitySnapshotKey]ActivityState,
) (ActivitySnapshotEntry, bool, error) {
	projection := projected[key.SenderID][key.Category].projection
	state, found := states[key]
	if !found || state.SourceToken != projection.SourceToken ||
		state.SourceVersion != projection.SourceVersion {
		return ActivitySnapshotEntry{}, false, nil
	}
	built, current, err := s.loadFinalizedSnapshotBuild(ctx, key, projection, state)
	if err != nil || !current {
		return ActivitySnapshotEntry{}, false, err
	}
	decision, err := s.authorize(ctx, built.Input)
	if err != nil {
		return ActivitySnapshotEntry{}, false, fmt.Errorf(
			"finalize rich-presence snapshot authorization: %w", err,
		)
	}
	if !decision.Audience[viewerID] {
		return ActivitySnapshotEntry{}, false, nil
	}
	if !validActivitySnapshotPayload(decision.Payload) {
		return ActivitySnapshotEntry{}, false, ErrInvalidActivitySnapshot
	}
	return ActivitySnapshotEntry{
		Minimized: decision.Minimized, Payload: append(json.RawMessage(nil), decision.Payload...),
		UpdatedAt: state.UpdatedAt, projection: projection,
	}, true, nil
}

func (s *ActivitySnapshotService) loadFinalizedSnapshotBuild(
	ctx context.Context,
	key activitySnapshotKey,
	projection activitySnapshotProjection,
	state ActivityState,
) (BuiltActivity, bool, error) {
	built, err := s.builder.Build(ctx, key.SenderID, projection.Scope)
	if err != nil {
		if errors.Is(err, ErrActivityNotCurrent) {
			return BuiltActivity{}, false, s.deleteSnapshotGeneration(ctx, key, state)
		}
		return BuiltActivity{}, false, fmt.Errorf("finalize rich-presence snapshot candidate: %w", err)
	}
	if built.SourceToken != state.SourceToken || built.SourceVersion != state.SourceVersion {
		return BuiltActivity{}, false, s.deleteSnapshotGenerationAtMost(
			ctx, key, state, built.SourceVersion,
		)
	}
	if built.Input.SenderID != key.SenderID || built.Input.Category != key.Category {
		return BuiltActivity{}, false, ErrInvalidActivitySnapshot
	}
	participantsCurrent, err := finalizedSnapshotParticipantsCurrent(key, projection, built)
	if err != nil || !participantsCurrent {
		return BuiltActivity{}, false, err
	}
	return built, true, nil
}

func finalizedSnapshotParticipantsCurrent(
	key activitySnapshotKey,
	projection activitySnapshotProjection,
	built BuiltActivity,
) (bool, error) {
	if key.Category != CategoryPrivateCall {
		return true, nil
	}
	if built.Input.PrivateCall == nil {
		return false, ErrInvalidActivitySnapshot
	}
	// The exact sender generation may still be current while the call's
	// participant set changed outside the gates acquired from the initial
	// projection. Omit fail-closed without deleting that state.
	return sameActivitySnapshotParticipantIDs(
		projection.ParticipantIDs,
		built.Input.PrivateCall.Context.ParticipantIDs,
	), nil
}

func validActivitySnapshotParticipantIDs(ids []uuid.UUID, senderID uuid.UUID) bool {
	if senderID == uuid.Nil || len(ids) < 1 || len(ids) > maxPrivateCallParticipants {
		return false
	}
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			return false
		}
		seen[id] = true
	}
	return seen[senderID]
}

func sameActivitySnapshotParticipantIDs(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[uuid.UUID]bool, len(left))
	for _, id := range left {
		if id == uuid.Nil || seen[id] {
			return false
		}
		seen[id] = true
	}
	for _, id := range right {
		if id == uuid.Nil || !seen[id] {
			return false
		}
		delete(seen, id)
	}
	return len(seen) == 0
}

func (s *ActivitySnapshotService) removeInactiveSnapshotStates(
	ctx context.Context,
	keys []activitySnapshotKey,
	states map[activitySnapshotKey]ActivityState,
) error {
	generationKeys := make([]activitySnapshotKey, 0, len(states))
	generations := make([]ActivityGeneration, 0, len(states))
	for _, key := range keys {
		state, found := states[key]
		if !found {
			continue
		}
		generationKeys = append(generationKeys, key)
		generations = append(generations, ActivityGeneration{
			UserID: key.SenderID, Category: key.Category,
			SourceToken: state.SourceToken, SourceVersion: state.SourceVersion,
		})
	}
	active, err := s.store.VerifyActiveGenerations(ctx, generations)
	if err != nil {
		return fmt.Errorf("verify rich-presence snapshot lifecycles: %w", err)
	}
	for index, isActive := range active {
		if isActive {
			continue
		}
		key := generationKeys[index]
		if err := s.deleteSnapshotGeneration(ctx, key, states[key]); err != nil {
			return err
		}
		delete(states, key)
	}
	return nil
}

func (s *ActivitySnapshotService) deleteSnapshotGeneration(
	ctx context.Context,
	key activitySnapshotKey,
	state ActivityState,
) error {
	_, err := s.store.CompareAndDelete(
		ctx,
		key.SenderID,
		key.Category,
		state.SourceToken,
		state.SourceVersion,
	)
	if err != nil {
		return fmt.Errorf("delete stale rich-presence snapshot state: %w", err)
	}
	return nil
}

func groupActivitySnapshotCandidates(
	candidates []activitySnapshotCandidate,
) (map[activitySnapshotKey]activitySnapshotCandidateGroup, error) {
	if len(candidates) > activitySnapshotCandidateLimit {
		return nil, ErrActivitySnapshotCandidateLimit
	}
	groups := make(map[activitySnapshotKey]activitySnapshotCandidateGroup, len(candidates))
	for _, candidate := range candidates {
		if candidate.SenderID == uuid.Nil || candidate.RoomID == uuid.Nil ||
			!IsValidActivitySourceTime(candidate.LifecycleAt) {
			return nil, ErrInvalidActivitySnapshot
		}
		switch candidate.Category {
		case CategoryServerVoice, CategoryPrivateCall:
		default:
			return nil, ErrInvalidActivitySnapshot
		}
		group, exists := groups[candidate.activitySnapshotKey]
		if !exists {
			groups[candidate.activitySnapshotKey] = activitySnapshotCandidateGroup{
				Candidate: candidate, MaxSourceVersion: candidate.LifecycleAt.UnixMicro(),
			}
			continue
		}
		if candidate.RoomID != group.Candidate.RoomID ||
			candidate.LifecycleAt != group.Candidate.LifecycleAt {
			group.Ambiguous = true
		}
		if version := candidate.LifecycleAt.UnixMicro(); version > group.MaxSourceVersion {
			group.MaxSourceVersion = version
		}
		groups[candidate.activitySnapshotKey] = group
	}
	return groups, nil
}

func loadActivitySnapshotStates(
	ctx context.Context,
	store *ActivityStore,
	keys []activitySnapshotKey,
) (map[activitySnapshotKey]ActivityState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store == nil || store.redis == nil {
		return nil, ErrInvalidActivitySnapshot
	}
	states := make(map[activitySnapshotKey]ActivityState, len(keys))
	for start := 0; start < len(keys); start += activitySnapshotRedisBatchSize {
		end := min(start+activitySnapshotRedisBatchSize, len(keys))
		batchStates, err := loadActivitySnapshotStateBatch(ctx, store, keys[start:end])
		if err != nil {
			return nil, err
		}
		for key, state := range batchStates {
			states[key] = state
		}
	}
	return states, nil
}

type activitySnapshotGet struct {
	key     activitySnapshotKey
	command *redis.Cmd
}

func loadActivitySnapshotStateBatch(
	ctx context.Context,
	store *ActivityStore,
	keys []activitySnapshotKey,
) (map[activitySnapshotKey]ActivityState, error) {
	pipeline := store.redis.Pipeline()
	gets, err := queueActivitySnapshotGets(ctx, pipeline, keys)
	if err != nil {
		return nil, err
	}
	if _, err := pipeline.Exec(ctx); err != nil {
		return nil, fmt.Errorf("read rich-presence snapshot state batch: %w", err)
	}
	states := make(map[activitySnapshotKey]ActivityState, len(gets))
	for _, get := range gets {
		state, found, err := decodeActivitySnapshotGet(ctx, store, get)
		if err != nil {
			return nil, err
		}
		if found {
			states[get.key] = state
		}
	}
	return states, nil
}

func queueActivitySnapshotGets(
	ctx context.Context,
	pipeline redis.Pipeliner,
	keys []activitySnapshotKey,
) ([]activitySnapshotGet, error) {
	gets := make([]activitySnapshotGet, 0, len(keys))
	for _, key := range keys {
		redisKey, err := activityKey(key.SenderID, key.Category)
		if err != nil {
			return nil, err
		}
		gets = append(gets, activitySnapshotGet{
			key: key, command: getActivityStateScript.Eval(ctx, pipeline, []string{redisKey}),
		})
	}
	return gets, nil
}

func decodeActivitySnapshotGet(
	ctx context.Context,
	store *ActivityStore,
	get activitySnapshotGet,
) (ActivityState, bool, error) {
	status, raw, err := activityStateScriptResult(get.command)
	if err != nil {
		return ActivityState{}, false, fmt.Errorf("read rich-presence snapshot state: %w", err)
	}
	if status != "string" {
		return ActivityState{}, false, nil
	}
	state, err := decodeActivityState(raw)
	if err == nil {
		return state, true, nil
	}
	if err := deleteMalformedActivitySnapshotState(ctx, store, get.key, raw); err != nil {
		return ActivityState{}, false, err
	}
	return ActivityState{}, false, nil
}

func deleteMalformedActivitySnapshotState(
	ctx context.Context,
	store *ActivityStore,
	key activitySnapshotKey,
	raw []byte,
) error {
	redisKey, err := activityKey(key.SenderID, key.Category)
	if err != nil {
		return err
	}
	if err := compareAndDeleteRawActivityScript.Run(
		ctx, store.redis, []string{redisKey}, raw,
	).Err(); err != nil {
		return fmt.Errorf("delete malformed rich-presence snapshot state: %w", err)
	}
	return nil
}

func (s *ActivitySnapshotService) loadCandidates(
	ctx context.Context,
	viewerID uuid.UUID,
) (candidates []activitySnapshotCandidate, returnErr error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH candidate_scopes AS (
			SELECT vp.user_id AS sender_id,
			       'server_voice'::text AS category,
			       vp.channel_id AS room_id,
			       vp.lifecycle_event_at
			FROM voice_participants vp
			JOIN channels c ON c.id = vp.channel_id
			JOIN server_members viewer_member
			  ON viewer_member.server_id = c.server_id
			 AND viewer_member.user_id = $1
			WHERE vp.user_id <> $1

			UNION ALL

			SELECT call_sender.user_id AS sender_id,
			       'private_call'::text AS category,
			       call_sender.conversation_id AS room_id,
			       call_sender.lifecycle_event_at
			FROM dm_voice_participants call_sender
			WHERE call_sender.user_id <> $1
			  AND (
				EXISTS (
					SELECT 1
					FROM dm_voice_participants call_viewer
					WHERE call_viewer.conversation_id = call_sender.conversation_id
					  AND call_viewer.user_id = $1
				)
				OR EXISTS (
					SELECT 1
					FROM friendships direct_friend
					WHERE direct_friend.status = 'accepted'
					  AND (
					    (direct_friend.requester_id = call_sender.user_id
					      AND direct_friend.addressee_id = $1)
					    OR
					    (direct_friend.addressee_id = call_sender.user_id
					      AND direct_friend.requester_id = $1)
					  )
				)
				OR EXISTS (
					SELECT 1
					FROM privacy_settings sender_privacy
					JOIN friendships sender_friend
					  ON sender_friend.status = 'accepted'
					 AND (
					   sender_friend.requester_id = call_sender.user_id
					   OR sender_friend.addressee_id = call_sender.user_id
					 )
					WHERE sender_privacy.user_id = call_sender.user_id
					  AND sender_privacy.dm_friends_of_friends
					  AND EXISTS (
					    SELECT 1
					    FROM friendships friend_viewer
					    WHERE friend_viewer.status = 'accepted'
					      AND (
					        (friend_viewer.requester_id = CASE
					           WHEN sender_friend.requester_id = call_sender.user_id
					           THEN sender_friend.addressee_id
					           ELSE sender_friend.requester_id END
					          AND friend_viewer.addressee_id = $1)
					        OR
					        (friend_viewer.addressee_id = CASE
					           WHEN sender_friend.requester_id = call_sender.user_id
					           THEN sender_friend.addressee_id
					           ELSE sender_friend.requester_id END
					          AND friend_viewer.requester_id = $1)
					      )
					  )
				)
				OR EXISTS (
					SELECT 1
					FROM server_members sender_member
					JOIN server_members viewer_member
					  ON viewer_member.server_id = sender_member.server_id
					 AND viewer_member.user_id = $1
					WHERE sender_member.user_id = call_sender.user_id
				)
			  )
		)
		SELECT sender_id, category, room_id, lifecycle_event_at
		FROM candidate_scopes
		ORDER BY sender_id, category, room_id, lifecycle_event_at
		LIMIT $2
	`, viewerID, activitySnapshotCandidateLimit+1)
	if err != nil {
		return nil, fmt.Errorf("query rich-presence snapshot candidates: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			candidates = nil
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("close rich-presence snapshot candidates: %w", closeErr),
			)
		}
	}()

	for rows.Next() {
		var (
			candidate activitySnapshotCandidate
			category  string
		)
		if err := rows.Scan(
			&candidate.SenderID,
			&category,
			&candidate.RoomID,
			&candidate.LifecycleAt,
		); err != nil {
			return nil, fmt.Errorf("scan rich-presence snapshot candidate: %w", err)
		}
		candidate.Category = Category(category)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rich-presence snapshot candidates: %w", err)
	}
	return candidates, nil
}
