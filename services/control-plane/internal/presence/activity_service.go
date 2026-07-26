package presence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const activityCleanupTimeout = 2 * time.Second

// ActivityMutation applies one watermark-guarded authoritative lifecycle
// mutation. The bool reports whether the event changed current state.
type ActivityMutation func(context.Context) (bool, error)

// SenderCoordinator serializes Rich Presence work for one sender.
type SenderCoordinator interface {
	WithSender(context.Context, uuid.UUID, func() error) error
}

// Delivery applies privacy-critical Rich Presence deltas to local clients.
type Delivery interface {
	DeliverRichPresence(context.Context, DeliveryPlan) error
	DisconnectRichPresenceClients(context.Context, map[uuid.UUID]bool) error
	DisconnectAllRichPresenceClients(context.Context) error
}

type activityBuilder interface {
	Build(context.Context, uuid.UUID, Scope) (BuiltActivity, error)
}

type activityStateStore interface {
	CompareAndSetActive(context.Context, uuid.UUID, Category, ActivityState) (bool, error)
	IsActiveGeneration(context.Context, uuid.UUID, Category, uuid.UUID, int64) (bool, error)
	CompareAndDelete(context.Context, uuid.UUID, Category, uuid.UUID, int64) (bool, error)
	Get(context.Context, uuid.UUID, Category) (ActivityState, bool, error)
	Delete(context.Context, uuid.UUID, Category) error
}

type activityAuthorizer func(context.Context, PolicyInput) (Decision, error)

type activitySettingsRecipientResolver func(
	context.Context,
	uuid.UUID,
	ActivityPolicySettings,
	ActivityPolicySettings,
) (map[uuid.UUID]bool, error)

// ActivityService bridges authoritative media lifecycle state to Redis and
// privacy-critical local delivery.
type ActivityService struct {
	coordinator        SenderCoordinator
	builder            activityBuilder
	store              activityStateStore
	authorize          activityAuthorizer
	delivery           Delivery
	settingsRecipients activitySettingsRecipientResolver
}

// NewActivityService creates the authoritative Rich Presence lifecycle bridge.
func NewActivityService(
	coordinator SenderCoordinator,
	builder *ActivityBuilder,
	store *ActivityStore,
	db DBTX,
	visibility ChannelVisibilityResolver,
	delivery Delivery,
	senderPresence SenderPresenceResolver,
) *ActivityService {
	// Required, not optional: a nil resolver fails closed in
	// AuthorizeAndMinimize, so forgetting to wire it would silently suppress ALL
	// Rich Presence rather than erroring. Making the parameter positional turns
	// that omission into a compile error (#2444).
	presenceGate := senderPresence
	service := newActivityService(
		coordinator,
		builder,
		store,
		func(ctx context.Context, input PolicyInput) (Decision, error) {
			return AuthorizeAndMinimize(ctx, db, visibility, presenceGate, input)
		},
		delivery,
	)
	service.settingsRecipients = func(
		ctx context.Context,
		userID uuid.UUID,
		before, after ActivityPolicySettings,
	) (map[uuid.UUID]bool, error) {
		return computeActivitySettingsRecipients(
			ctx,
			activitySettingsRecipientDependencies{
				db: db, visibility: visibility, builder: builder, store: store,
			},
			userID,
			before,
			after,
		)
	}
	return service
}

func newActivityService(
	coordinator SenderCoordinator,
	builder activityBuilder,
	store activityStateStore,
	authorize activityAuthorizer,
	delivery Delivery,
) *ActivityService {
	return &ActivityService{
		coordinator: coordinator,
		builder:     builder,
		store:       store,
		authorize:   authorize,
		delivery:    delivery,
	}
}

// RefreshServerVoice applies an optional guarded mutation, then publishes the
// sender's freshly rebuilt Server Voice state.
func (s *ActivityService) RefreshServerVoice(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
	mutation ActivityMutation,
) error {
	return s.refresh(ctx, senderID, scope, nil, nil, mutation, CategoryServerVoice)
}

// MoveServerVoice atomically applies a server-channel move, then clears viewers
// authorized only for the old channel and updates the freshly authorized new
// audience.
func (s *ActivityService) MoveServerVoice(
	ctx context.Context,
	senderID uuid.UUID,
	oldScope, newScope Scope,
	mutation ActivityMutation,
) error {
	if oldScope.Category != CategoryServerVoice ||
		newScope.Category != CategoryServerVoice {
		return ErrInvalidActivityScope
	}
	return s.refresh(
		ctx, senderID, newScope, &oldScope, nil, mutation, CategoryServerVoice,
	)
}

// ClearServerVoice authorizes the old state before an optional guarded
// mutation, then clears only the matching Server Voice generation.
func (s *ActivityService) ClearServerVoice(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
	mutation ActivityMutation,
) error {
	return s.clear(ctx, senderID, scope, nil, mutation, CategoryServerVoice, true)
}

// RefreshPrivateCall publishes one participant's freshly rebuilt Private Call
// state. Recheck viewers receive an update when still authorized, otherwise a
// clear.
func (s *ActivityService) RefreshPrivateCall(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
	recheckViewers map[uuid.UUID]bool,
	mutation ActivityMutation,
) error {
	return s.refresh(ctx, senderID, scope, nil, recheckViewers, mutation, CategoryPrivateCall)
}

// ClearPrivateCall authorizes the old state before an optional guarded
// mutation, then clears only the matching Private Call generation.
func (s *ActivityService) ClearPrivateCall(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
	additionalClearRecipients map[uuid.UUID]bool,
	mutation ActivityMutation,
) error {
	return s.clear(
		ctx, senderID, scope, additionalClearRecipients, mutation, CategoryPrivateCall, true,
	)
}

// ClearPrivateCallTerminal preserves the event-local authoritative call
// snapshot so one terminal event can clear every participant after its first
// mutation removes the shared live-call state.
func (s *ActivityService) ClearPrivateCallTerminal(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
	additionalClearRecipients map[uuid.UUID]bool,
	mutation ActivityMutation,
) error {
	return s.clear(
		ctx, senderID, scope, additionalClearRecipients, mutation, CategoryPrivateCall, false,
	)
}

func (s *ActivityService) refresh(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
	previousScope *Scope,
	recheckViewers map[uuid.UUID]bool,
	mutation ActivityMutation,
	category Category,
) error {
	if err := validateActivityServiceCall(ctx, s, senderID, scope, category); err != nil {
		return err
	}
	request := refreshActivityRequest{
		senderID: senderID, scope: scope, previousScope: previousScope,
		recheckViewers: recheckViewers, mutation: mutation, category: category,
	}
	return s.coordinator.WithSender(ctx, senderID, func() error {
		return s.refreshAlreadyGated(ctx, request)
	})
}

type refreshActivityRequest struct {
	senderID       uuid.UUID
	scope          Scope
	previousScope  *Scope
	recheckViewers map[uuid.UUID]bool
	mutation       ActivityMutation
	category       Category
}

type previousActivity struct {
	audience map[uuid.UUID]bool
	built    *BuiltActivity
	err      error
}

func (s *ActivityService) refreshAlreadyGated(
	ctx context.Context,
	request refreshActivityRequest,
) error {
	previous := s.loadPreviousActivity(ctx, request)
	applied, err := s.runRefreshMutation(ctx, request, previous)
	if err != nil || !applied {
		return err
	}
	// A mutation may change the exact private-call participant/lifecycle
	// snapshot consumed by the old-state build. Preserve the event budget but
	// force the fresh sender build below to re-read authoritative state.
	if request.mutation != nil {
		InvalidateActivityBuildCache(ctx)
	}
	if previous.err != nil {
		return s.failPreviousActivity(ctx, request, previous)
	}
	built, decision, err := s.buildFreshActivity(ctx, request)
	if err != nil {
		return err
	}
	if len(decision.Payload) == 0 {
		return s.suppressRefreshedActivity(ctx, request, previous, built, decision)
	}
	return s.persistAndDeliverRefreshedActivity(ctx, request, previous, built, decision)
}

func (s *ActivityService) loadPreviousActivity(
	ctx context.Context,
	request refreshActivityRequest,
) previousActivity {
	if request.previousScope == nil {
		return previousActivity{}
	}
	built, err := s.builder.Build(ctx, request.senderID, *request.previousScope)
	if err != nil {
		return previousActivity{err: fmt.Errorf(
			"build old %s rich-presence activity: %w", request.category, err,
		)}
	}
	previous := previousActivity{built: &built}
	decision, err := s.authorize(ctx, built.Input)
	if err != nil {
		previous.err = fmt.Errorf(
			"authorize old %s rich-presence activity: %w", request.category, err,
		)
		return previous
	}
	previous.audience = includedActivityRecipients(decision.Audience)
	return previous
}

func (s *ActivityService) runRefreshMutation(
	ctx context.Context,
	request refreshActivityRequest,
	previous previousActivity,
) (bool, error) {
	applied, err := runActivityMutation(ctx, request.mutation)
	if err == nil {
		return applied, nil
	}
	primary := fmt.Errorf("mutate %s rich-presence activity: %w", request.category, err)
	if previous.built == nil {
		return false, errors.Join(primary, s.disconnectAll(ctx))
	}
	return false, s.failClosedGeneration(
		ctx, request.senderID, request.category,
		previous.built.SourceToken, previous.built.SourceVersion, primary,
	)
}

func (s *ActivityService) failPreviousActivity(
	ctx context.Context,
	request refreshActivityRequest,
	previous previousActivity,
) error {
	failureToken := request.previousScope.LifecycleID
	failureVersion := request.previousScope.EventAt.UnixMicro()
	if previous.built != nil {
		failureToken = previous.built.SourceToken
		failureVersion = previous.built.SourceVersion
	}
	return s.failClosedGeneration(
		ctx, request.senderID, request.category,
		failureToken, failureVersion, previous.err,
	)
}

func (s *ActivityService) buildFreshActivity(
	ctx context.Context,
	request refreshActivityRequest,
) (BuiltActivity, Decision, error) {
	built, err := s.builder.Build(ctx, request.senderID, request.scope)
	if err != nil {
		failure := fmt.Errorf("build fresh %s rich-presence activity: %w", request.category, err)
		return BuiltActivity{}, Decision{}, s.failClosedGeneration(
			ctx, request.senderID, request.category,
			request.scope.LifecycleID, request.scope.EventAt.UnixMicro(), failure,
		)
	}
	decision, err := s.authorize(ctx, built.Input)
	if err != nil {
		failure := fmt.Errorf("authorize fresh %s rich-presence activity: %w", request.category, err)
		return BuiltActivity{}, Decision{}, s.failClosedGeneration(
			ctx, request.senderID, request.category,
			built.SourceToken, built.SourceVersion, failure,
		)
	}
	return built, decision, nil
}

func (s *ActivityService) suppressRefreshedActivity(
	ctx context.Context,
	request refreshActivityRequest,
	previous previousActivity,
	built BuiltActivity,
	decision Decision,
) error {
	if previous.built != nil {
		// The move branch stays FIRST because only it holds the prior
		// generation's exact token/version, which is what must be deleted. The
		// suppression reason is threaded in rather than reordered: without it a
		// suppressed move takes the reason-blind global-disconnect terminal, and
		// the heartbeat bridge calls MoveServerVoice on every channel change, so
		// an invisible user hopping channels could disconnect the replica at will
		// (#2444).
		return s.suppressMovedGeneration(
			ctx, request.senderID, request.category,
			previous.built.SourceToken, previous.built.SourceVersion, previous.audience,
			decision.SuppressedBySenderPresence,
		)
	}
	if decision.SuppressedBySenderPresence {
		return s.suppressHiddenSenderGeneration(
			ctx, request.senderID, request.category,
			built.SourceToken, built.SourceVersion,
		)
	}
	return s.suppressGeneration(
		ctx, request.senderID, request.category, built.SourceToken, built.SourceVersion,
	)
}

func (s *ActivityService) persistAndDeliverRefreshedActivity(
	ctx context.Context,
	request refreshActivityRequest,
	previous previousActivity,
	built BuiltActivity,
	decision Decision,
) error {
	audience := includedActivityRecipients(decision.Audience)
	clearRecipients := recheckedActivityClears(
		unionActivityRecipients(previous.audience, request.recheckViewers), audience,
	)
	state := ActivityState{
		SourceToken: built.SourceToken, SourceVersion: built.SourceVersion,
		Minimized: decision.Minimized, Payload: decision.Payload,
		UpdatedAt: request.scope.EventAt.Unix(),
	}
	stored, err := s.store.CompareAndSetActive(ctx, request.senderID, request.category, state)
	if err != nil {
		failure := fmt.Errorf("persist %s rich-presence activity: %w", request.category, err)
		return s.failClosedGeneration(
			ctx, request.senderID, request.category,
			built.SourceToken, built.SourceVersion, failure,
		)
	}
	preparedRecipients := unionActivityRecipients(audience, clearRecipients)
	if !stored {
		return s.disconnectAfterGenerationMiss(
			ctx, request.senderID, request.category, built.SourceVersion, preparedRecipients,
		)
	}
	plan := DeliveryPlan{
		SenderID: request.senderID, Category: request.category,
		ClearRecipients: clearRecipients, UpdateRecipients: audience,
		Minimized: state.Minimized, Payload: state.Payload, UpdatedAt: state.UpdatedAt,
	}
	if err := s.delivery.DeliverRichPresence(ctx, plan); err != nil {
		failure := fmt.Errorf("deliver %s rich-presence activity: %w", request.category, err)
		return s.failClosedDelivery(
			ctx, request.senderID, request.category,
			built.SourceToken, built.SourceVersion, preparedRecipients, failure,
		)
	}
	return nil
}

func (s *ActivityService) clear(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
	additionalClearRecipients map[uuid.UUID]bool,
	mutation ActivityMutation,
	category Category,
	invalidateBuildCache bool,
) error {
	if err := validateActivityServiceCall(ctx, s, senderID, scope, category); err != nil {
		return err
	}
	request := clearActivityRequest{
		senderID: senderID, scope: scope, additionalClearRecipients: additionalClearRecipients,
		mutation: mutation, category: category, invalidateBuildCache: invalidateBuildCache,
	}
	return s.coordinator.WithSender(ctx, senderID, func() error {
		return s.clearAlreadyGated(ctx, request)
	})
}

type clearActivityRequest struct {
	senderID                  uuid.UUID
	scope                     Scope
	additionalClearRecipients map[uuid.UUID]bool
	mutation                  ActivityMutation
	category                  Category
	invalidateBuildCache      bool
}

type preparedOldActivity struct {
	built     BuiltActivity
	decision  Decision
	buildErr  error
	policyErr error
}

func (s *ActivityService) clearAlreadyGated(
	ctx context.Context,
	request clearActivityRequest,
) error {
	prepared := s.prepareOldActivity(ctx, request)
	applied, err := s.runClearMutation(ctx, request, prepared)
	if err != nil || !applied {
		return err
	}
	// Clear invalidates the same event-local snapshot before callers fan out
	// subsequent participant refreshes on this context.
	if request.mutation != nil && request.invalidateBuildCache {
		InvalidateActivityBuildCache(ctx)
	}
	if err := s.validatePreparedOldActivity(ctx, request, prepared); err != nil {
		return err
	}
	if len(prepared.decision.Payload) == 0 {
		// Mirror suppressRefreshedActivity: the clear path must key on the
		// suppression REASON, not just an empty payload. By the time an
		// invisible sender leaves voice, the level arm has already removed the
		// stored generation, so the reason-blind path below would find nothing
		// to delete, find no successor, and force-disconnect every
		// Rich-Presence client on the replica -- on an ordinary user action
		// (#2444).
		if prepared.decision.SuppressedBySenderPresence {
			return s.suppressHiddenSenderGeneration(
				ctx, request.senderID, request.category,
				prepared.built.SourceToken, prepared.built.SourceVersion,
			)
		}
		return s.suppressGeneration(
			ctx, request.senderID, request.category,
			prepared.built.SourceToken, prepared.built.SourceVersion,
		)
	}
	return s.deleteAndDeliverActivityClear(ctx, request, prepared)
}

func (s *ActivityService) prepareOldActivity(
	ctx context.Context,
	request clearActivityRequest,
) preparedOldActivity {
	prepared := preparedOldActivity{}
	prepared.built, prepared.buildErr = s.builder.Build(ctx, request.senderID, request.scope)
	if prepared.buildErr == nil {
		prepared.decision, prepared.policyErr = s.authorize(ctx, prepared.built.Input)
	}
	return prepared
}

func (s *ActivityService) runClearMutation(
	ctx context.Context,
	request clearActivityRequest,
	prepared preparedOldActivity,
) (bool, error) {
	applied, err := runActivityMutation(ctx, request.mutation)
	if err == nil {
		return applied, nil
	}
	primary := fmt.Errorf("mutate clear %s rich-presence activity: %w", request.category, err)
	if prepared.buildErr != nil {
		return false, errors.Join(primary, s.disconnectAll(ctx))
	}
	return false, s.failClosedGeneration(
		ctx, request.senderID, request.category,
		prepared.built.SourceToken, prepared.built.SourceVersion, primary,
	)
}

func (s *ActivityService) validatePreparedOldActivity(
	ctx context.Context,
	request clearActivityRequest,
	prepared preparedOldActivity,
) error {
	if prepared.buildErr != nil {
		failure := fmt.Errorf("build old %s rich-presence activity: %w", request.category, prepared.buildErr)
		return s.failClosedGeneration(
			ctx, request.senderID, request.category,
			request.scope.LifecycleID, request.scope.EventAt.UnixMicro(), failure,
		)
	}
	if prepared.policyErr != nil {
		failure := fmt.Errorf("authorize old %s rich-presence activity: %w", request.category, prepared.policyErr)
		return s.failClosedGeneration(
			ctx, request.senderID, request.category,
			prepared.built.SourceToken, prepared.built.SourceVersion, failure,
		)
	}
	return nil
}

func (s *ActivityService) deleteAndDeliverActivityClear(
	ctx context.Context,
	request clearActivityRequest,
	prepared preparedOldActivity,
) error {
	audience := unionActivityRecipients(
		includedActivityRecipients(prepared.decision.Audience), request.additionalClearRecipients,
	)
	delete(audience, request.senderID)
	deleted, err := s.store.CompareAndDelete(
		ctx, request.senderID, request.category,
		prepared.built.SourceToken, prepared.built.SourceVersion,
	)
	if err != nil {
		failure := fmt.Errorf("delete %s rich-presence activity: %w", request.category, err)
		return s.failClosedGeneration(
			ctx, request.senderID, request.category,
			prepared.built.SourceToken, prepared.built.SourceVersion, failure,
		)
	}
	if !deleted {
		return s.disconnectAfterGenerationMiss(
			ctx, request.senderID, request.category,
			prepared.built.SourceVersion,
			audience,
		)
	}
	plan := DeliveryPlan{
		SenderID: request.senderID, Category: request.category, ClearRecipients: audience,
	}
	if err := s.delivery.DeliverRichPresence(ctx, plan); err != nil {
		return errors.Join(
			fmt.Errorf("deliver %s rich-presence clear: %w", request.category, err),
			s.disconnectKnown(ctx, audience),
		)
	}
	return nil
}

func validateActivityServiceCall(
	ctx context.Context,
	service *ActivityService,
	senderID uuid.UUID,
	scope Scope,
	category Category,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.Category != category {
		return ErrInvalidActivityScope
	}
	if err := validateActivityScope(senderID, scope); err != nil {
		return err
	}
	if service == nil || service.coordinator == nil || service.builder == nil ||
		service.store == nil || service.authorize == nil || service.delivery == nil {
		return errors.New("rich-presence activity service unavailable")
	}
	return nil
}

func runActivityMutation(ctx context.Context, mutation ActivityMutation) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if mutation == nil {
		return true, nil
	}
	return mutation(ctx)
}

func (s *ActivityService) suppressGeneration(
	ctx context.Context,
	senderID uuid.UUID,
	category Category,
	sourceToken uuid.UUID,
	sourceVersion int64,
) error {
	deleteCtx, cancelDelete := boundedActivityCleanupContext(ctx)
	deleted, deleteErr := s.store.CompareAndDelete(
		deleteCtx, senderID, category, sourceToken, sourceVersion,
	)
	cancelDelete()
	var disconnectErr error
	if deleteErr == nil && !deleted {
		successor, inspectErr := s.hasGenerationSuccessor(
			ctx, senderID, category, sourceVersion,
		)
		if inspectErr != nil || !successor {
			disconnectErr = s.disconnectAll(ctx)
		}
		return errors.Join(inspectErr, disconnectErr)
	}
	disconnectErr = s.disconnectAll(ctx)
	return errors.Join(
		wrapActivityError("delete suppressed rich-presence activity", deleteErr),
		disconnectErr,
	)
}

// suppressHiddenSenderGeneration removes an active generation for a sender whose
// base presence forbids emission. It differs from suppressGeneration in exactly
// one classification: a delete that finds nothing stored is a BENIGN terminal
// here, not a conservative global disconnect.
//
// Why that is safe: this path runs on every heartbeat for as long as the sender
// stays invisible, so treating "nothing to delete" as an anomaly would force a
// full-replica disconnect every heartbeat, indefinitely (#2444). And it cannot
// leave a stale badge, because the edge arm already cleared any audience that
// existed at the transition, or failed loudly and disconnected. This function's
// job is preventing RE-publication, not cleanup.
//
// Every other outcome keeps the conservative posture unchanged.
func (s *ActivityService) suppressHiddenSenderGeneration(
	ctx context.Context,
	senderID uuid.UUID,
	category Category,
	sourceToken uuid.UUID,
	sourceVersion int64,
) error {
	// Resolve the audience BEFORE deleting, mirroring the edge arm's ordering.
	//
	// The resolver reconstructs each category's scope from the STORED generation
	// (currentServerSettingsRecipients -> store.Get), and a widest-prior policy
	// makes priorEligible true -- so resolving after a successful delete finds no
	// row, returns "settings evidence unavailable for prior-eligible policy", and
	// escalates to a full-replica disconnect on the FIRST suppressed heartbeat of
	// every sender who goes invisible while in voice. That is precisely the storm
	// this function exists to prevent (#2444).
	//
	// Resolving unconditionally costs one wasted resolver call per heartbeat in the
	// steady state (nothing stored), where its error is discarded and the benign
	// terminal below is taken. That is the cheaper mistake: the alternative is an
	// extra existence read for the same information.
	var recipients map[uuid.UUID]bool
	var resolveErr error
	if s.settingsRecipients != nil {
		recipients, resolveErr = s.settingsRecipients(
			ctx, senderID, hiddenSenderWidestPriorSettings, hiddenSenderSuppressedSettings,
		)
	}

	deleteCtx, cancelDelete := boundedActivityCleanupContext(ctx)
	deleted, deleteErr := s.store.CompareAndDelete(
		deleteCtx, senderID, category, sourceToken, sourceVersion,
	)
	cancelDelete()

	if deleteErr != nil {
		return errors.Join(
			wrapActivityError("delete hidden-sender rich-presence activity", deleteErr),
			s.disconnectAll(ctx),
		)
	}

	if deleted {
		// We removed a live generation, so a prior audience may hold a badge.
		// Clear it precisely rather than disconnecting (#2444 spec 3.2.1). An
		// unusable audience is the one case that must still fail closed.
		if s.settingsRecipients == nil {
			return s.disconnectAll(ctx)
		}
		if resolveErr != nil {
			return errors.Join(
				wrapActivityError("resolve hidden-sender activity recipients", resolveErr),
				s.disconnectAllWithinBudget(ctx),
			)
		}
		return s.deliverHiddenSenderClears(ctx, senderID, recipients)
	}

	// Nothing stored. Distinguish "already gone" (benign) from "a newer
	// generation exists that we must not silently ignore".
	successor, inspectErr := s.hasGenerationSuccessor(ctx, senderID, category, sourceVersion)
	if inspectErr != nil {
		return errors.Join(inspectErr, s.disconnectAll(ctx))
	}
	if successor {
		return s.disconnectAll(ctx)
	}
	return nil // benign terminal — the steady state for an invisible sender
}

func (s *ActivityService) suppressMovedGeneration(
	ctx context.Context,
	senderID uuid.UUID,
	category Category,
	priorSourceToken uuid.UUID,
	priorSourceVersion int64,
	priorAudience map[uuid.UUID]bool,
	hiddenSender bool,
) error {
	deleteCtx, cancelDelete := boundedActivityCleanupContext(ctx)
	deleted, deleteErr := s.store.CompareAndDelete(
		deleteCtx,
		senderID,
		category,
		priorSourceToken,
		priorSourceVersion,
	)
	cancelDelete()
	if deleteErr != nil {
		return errors.Join(
			wrapActivityError("delete prior suppressed rich-presence activity", deleteErr),
			s.disconnectAll(ctx),
		)
	}
	if !deleted {
		if hiddenSender {
			// Same benign terminal as suppressHiddenSenderGeneration, and for the
			// same reason: for a hidden sender "nothing stored" is the STEADY
			// STATE, because the edge arm already removed the row. Treating it as
			// an anomaly here would force a full-replica disconnect on every
			// channel hop, indefinitely. Every other outcome keeps the
			// conservative posture unchanged.
			return nil
		}
		return s.disconnectAfterGenerationMiss(
			ctx, senderID, category, priorSourceVersion, priorAudience,
		)
	}
	if len(priorAudience) == 0 {
		return nil
	}
	if err := s.delivery.DeliverRichPresence(ctx, DeliveryPlan{
		SenderID: senderID, Category: category, ClearRecipients: priorAudience,
	}); err != nil {
		return errors.Join(
			fmt.Errorf("deliver prior suppressed %s rich-presence clear: %w", category, err),
			s.disconnectKnown(ctx, priorAudience),
		)
	}
	return nil
}

func (s *ActivityService) failClosedGeneration(
	ctx context.Context,
	senderID uuid.UUID,
	category Category,
	sourceToken uuid.UUID,
	sourceVersion int64,
	primary error,
) error {
	deleteCtx, cancelDelete := boundedActivityCleanupContext(ctx)
	_, deleteErr := s.store.CompareAndDelete(
		deleteCtx,
		senderID,
		category,
		sourceToken,
		sourceVersion,
	)
	cancelDelete()
	disconnectErr := s.disconnectAll(ctx)
	return errors.Join(
		primary,
		wrapActivityError("clean failed rich-presence generation", deleteErr),
		disconnectErr,
	)
}

func (s *ActivityService) failClosedDelivery(
	ctx context.Context,
	senderID uuid.UUID,
	category Category,
	sourceToken uuid.UUID,
	sourceVersion int64,
	preparedRecipients map[uuid.UUID]bool,
	primary error,
) error {
	deleteCtx, cancelDelete := boundedActivityCleanupContext(ctx)
	deleted, deleteErr := s.store.CompareAndDelete(
		deleteCtx,
		senderID,
		category,
		sourceToken,
		sourceVersion,
	)
	cancelDelete()
	var disconnectErr error
	if deleteErr != nil {
		disconnectErr = s.disconnectAll(ctx)
	} else if !deleted {
		disconnectErr = s.disconnectAfterGenerationMiss(
			ctx, senderID, category, sourceVersion, preparedRecipients,
		)
	} else {
		disconnectErr = s.disconnectKnown(ctx, preparedRecipients)
	}
	return errors.Join(
		primary,
		wrapActivityError("clean undelivered rich-presence generation", deleteErr),
		disconnectErr,
	)
}

func (s *ActivityService) disconnectAfterGenerationMiss(
	ctx context.Context,
	senderID uuid.UUID,
	category Category,
	sourceVersion int64,
	knownRecipients map[uuid.UUID]bool,
) error {
	successor, inspectErr := s.hasGenerationSuccessor(
		ctx, senderID, category, sourceVersion,
	)
	if inspectErr != nil || !successor {
		return errors.Join(inspectErr, s.disconnectAll(ctx))
	}
	return s.disconnectKnown(ctx, knownRecipients)
}

func (s *ActivityService) hasGenerationSuccessor(
	ctx context.Context,
	senderID uuid.UUID,
	category Category,
	sourceVersion int64,
) (bool, error) {
	inspectCtx, cancelInspect := boundedActivityCleanupContext(ctx)
	defer cancelInspect()
	state, found, err := s.store.Get(inspectCtx, senderID, category)
	if err != nil {
		return false, fmt.Errorf("inspect current rich-presence activity: %w", err)
	}
	if !found {
		return false, nil
	}
	if state.SourceVersion <= sourceVersion {
		return false, errors.New("inspect current rich-presence activity: state is not a successor")
	}
	active, err := s.store.IsActiveGeneration(
		inspectCtx, senderID, category, state.SourceToken, state.SourceVersion,
	)
	if err != nil {
		return false, fmt.Errorf("verify current rich-presence activity successor: %w", err)
	}
	return active, nil
}

func (s *ActivityService) disconnectKnown(
	ctx context.Context,
	recipients map[uuid.UUID]bool,
) error {
	if len(recipients) == 0 {
		return nil
	}
	disconnectCtx, cancelDisconnect := boundedActivityCleanupContext(ctx)
	defer cancelDisconnect()
	return wrapActivityError(
		"disconnect affected rich-presence clients",
		s.delivery.DisconnectRichPresenceClients(disconnectCtx, recipients),
	)
}

func (s *ActivityService) disconnectAll(ctx context.Context) error {
	disconnectCtx, cancelDisconnect := boundedActivityCleanupContext(ctx)
	defer cancelDisconnect()
	return s.disconnectAllWithinBudget(disconnectCtx)
}

func (s *ActivityService) disconnectAllWithinBudget(ctx context.Context) error {
	return wrapActivityError(
		"disconnect all rich-presence clients",
		s.delivery.DisconnectAllRichPresenceClients(ctx),
	)
}

func boundedActivityCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), activityCleanupTimeout)
}

func includedActivityRecipients(in map[uuid.UUID]bool) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(in))
	for userID, included := range in {
		if included {
			out[userID] = true
		}
	}
	return out
}

func recheckedActivityClears(
	recheckViewers, audience map[uuid.UUID]bool,
) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool)
	for userID, included := range recheckViewers {
		if included && !audience[userID] {
			out[userID] = true
		}
	}
	return out
}

func unionActivityRecipients(groups ...map[uuid.UUID]bool) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool)
	for _, group := range groups {
		for userID, included := range group {
			if included {
				out[userID] = true
			}
		}
	}
	return out
}

func wrapActivityError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
