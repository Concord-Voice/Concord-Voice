package presence

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	activityServiceSender = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	activityServiceRoom   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	activityServiceCall   = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	activityServiceViewer = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	activityServiceOther  = uuid.MustParse("55555555-5555-5555-5555-555555555555")
	activityServiceEvent  = time.Unix(1_784_088_000, 123_456_000).UTC()
)

func TestActivityService_ClearAuthorizesOldStateBeforeMutation(t *testing.T) {
	service, builder, store, delivery, coordinator := newActivityServiceFixture(CategoryServerVoice)
	var events []string
	builder.onBuild = func() { events = append(events, "build") }
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		events = append(events, "authorize")
		return Decision{
			Audience: map[uuid.UUID]bool{activityServiceViewer: true},
			Payload:  json.RawMessage(`{"authorized":true}`),
		}, nil
	}
	store.onDelete = func() { events = append(events, "delete") }
	delivery.onDeliver = func() { events = append(events, "deliver") }

	err := service.ClearServerVoice(
		context.Background(), activityServiceSender, serverActivityScope(),
		func(context.Context) (bool, error) {
			events = append(events, "mutate")
			assert.Equal(t, 1, coordinator.depthNow())
			return true, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"build", "authorize", "mutate", "delete", "deliver"}, events)
	require.Len(t, store.deletes, 1)
	assert.Equal(t, builder.built.SourceToken, store.deletes[0].token)
	assert.Equal(t, builder.built.SourceVersion, store.deletes[0].version)
	require.Len(t, delivery.plans, 1)
	assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true}, delivery.plans[0].ClearRecipients)
	assert.Empty(t, delivery.plans[0].UpdateRecipients)
}

func TestActivityService_RefreshBuildsPostMutationAndPersistsOnlyPolicyDecision(t *testing.T) {
	service, builder, store, delivery, coordinator := newActivityServiceFixture(CategoryServerVoice)
	var events []string
	mutated := false
	builder.onBuild = func() {
		events = append(events, "build")
		assert.True(t, mutated, "refresh must build authoritative post-mutation state")
		assert.Equal(t, 1, coordinator.depthNow())
	}
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		events = append(events, "authorize")
		return Decision{
			Audience:  map[uuid.UUID]bool{activityServiceViewer: true},
			Payload:   json.RawMessage(`{"server_id":"policy-minimized"}`),
			Minimized: true,
		}, nil
	}
	store.onSet = func() { events = append(events, "store") }
	delivery.onDeliver = func() { events = append(events, "deliver") }

	err := service.RefreshServerVoice(
		context.Background(), activityServiceSender, serverActivityScope(),
		func(context.Context) (bool, error) {
			events = append(events, "mutate")
			mutated = true
			return true, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"mutate", "build", "authorize", "store", "deliver"}, events)
	require.Len(t, store.sets, 1)
	assert.JSONEq(t, `{"server_id":"policy-minimized"}`, string(store.sets[0].state.Payload))
	assert.True(t, store.sets[0].state.Minimized)
	assert.Equal(t, activityServiceEvent.Unix(), store.sets[0].state.UpdatedAt)
	require.Len(t, delivery.plans, 1)
	assert.Equal(t, store.sets[0].state.Payload, delivery.plans[0].Payload)
	assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true}, delivery.plans[0].UpdateRecipients)
}

func TestActivityService_PrivateRefreshRechecksRemovedViewer(t *testing.T) {
	for _, test := range []struct {
		name       string
		audience   map[uuid.UUID]bool
		wantClear  map[uuid.UUID]bool
		wantUpdate map[uuid.UUID]bool
	}{
		{
			name: "still externally authorized receives current update",
			audience: map[uuid.UUID]bool{
				activityServiceViewer: true,
				activityServiceOther:  true,
			},
			wantClear: map[uuid.UUID]bool{},
			wantUpdate: map[uuid.UUID]bool{
				activityServiceViewer: true,
				activityServiceOther:  true,
			},
		},
		{
			name:       "no longer authorized receives clear",
			audience:   map[uuid.UUID]bool{activityServiceOther: true},
			wantClear:  map[uuid.UUID]bool{activityServiceViewer: true},
			wantUpdate: map[uuid.UUID]bool{activityServiceOther: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
			service.authorize = func(context.Context, PolicyInput) (Decision, error) {
				return Decision{Audience: test.audience, Payload: json.RawMessage(`{"call_type":"dm","participant_count":2}`)}, nil
			}

			err := service.RefreshPrivateCall(
				context.Background(), activityServiceSender, privateActivityScope(),
				map[uuid.UUID]bool{activityServiceViewer: true}, nil,
			)
			require.NoError(t, err)
			require.Len(t, delivery.plans, 1)
			assert.Equal(t, test.wantClear, delivery.plans[0].ClearRecipients)
			assert.Equal(t, test.wantUpdate, delivery.plans[0].UpdateRecipients)
		})
	}
}

func TestActivityService_PrivateClearIncludesPreMutationRecipients(t *testing.T) {
	preMutationParticipants := map[uuid.UUID]bool{
		activityServiceSender: true,
		activityServiceViewer: true,
		activityServiceOther:  true,
	}
	for _, test := range []struct {
		name       string
		senderID   uuid.UUID
		wantClears map[uuid.UUID]bool
	}{
		{
			name:     "first departure clears every other original participant",
			senderID: activityServiceSender,
			wantClears: map[uuid.UUID]bool{
				activityServiceViewer: true, activityServiceOther: true,
			},
		},
		{
			name:     "later departure still clears the earlier departed viewer",
			senderID: activityServiceViewer,
			wantClears: map[uuid.UUID]bool{
				activityServiceSender: true, activityServiceOther: true,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
			err := service.ClearPrivateCall(
				context.Background(), test.senderID, privateActivityScope(),
				preMutationParticipants, nil,
			)
			require.NoError(t, err)
			require.Len(t, delivery.plans, 1)
			assert.Equal(t, test.wantClears, delivery.plans[0].ClearRecipients)
		})
	}
}

func TestActivityService_PrivateTerminalClearRetainsEventLocalBuildCache(t *testing.T) {
	for _, test := range []struct {
		name       string
		terminal   bool
		wantCached bool
	}{
		{name: "participant clear invalidates", wantCached: false},
		{name: "terminal clear retains", terminal: true, wantCached: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, _, _ := newActivityServiceFixture(CategoryPrivateCall)
			ctx := WithActivityBuildCache(context.Background())
			cache := activityBuildCacheFromContext(ctx)
			key := privateCallBuildCacheKey{
				conversationID: privateActivityScope().RoomID,
				callID:         privateActivityScope().LifecycleID,
			}
			cache.privateCalls[key] = privateCallBuildCacheResult{
				snapshot: &privateCallBuildSnapshot{key: key},
			}
			mutation := func(context.Context) (bool, error) { return true, nil }

			var err error
			if test.terminal {
				err = service.ClearPrivateCallTerminal(
					ctx, activityServiceSender, privateActivityScope(), nil, mutation,
				)
			} else {
				err = service.ClearPrivateCall(
					ctx, activityServiceSender, privateActivityScope(), nil, mutation,
				)
			}

			require.NoError(t, err)
			_, cached := cache.privateCalls[key]
			assert.Equal(t, test.wantCached, cached)
		})
	}
}

func TestActivityService_MoveServerVoiceClearsOldOnlyViewersBeforeUpdatingNewAudience(t *testing.T) {
	service, builder, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	oldRoom := activityServiceRoom
	newRoom := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	oldScope := serverActivityScope()
	newScope := oldScope
	newScope.RoomID = newRoom
	newScope.LifecycleID = newRoom
	newScope.EventAt = oldScope.EventAt.Add(time.Second)
	builder.build = func(_ uuid.UUID, scope Scope) (BuiltActivity, error) {
		return BuiltActivity{
			Input: PolicyInput{
				SenderID: activityServiceSender,
				Category: CategoryServerVoice,
				ServerVoice: &ServerVoicePolicyInput{Context: ServerVoiceContext{
					ChannelID: scope.RoomID,
				}},
			},
			SourceToken: scope.RoomID, SourceVersion: scope.EventAt.UnixMicro(),
		}, nil
	}
	service.authorize = func(_ context.Context, input PolicyInput) (Decision, error) {
		if input.ServerVoice.Context.ChannelID == oldRoom {
			return Decision{
				Audience: map[uuid.UUID]bool{activityServiceViewer: true},
				Payload:  json.RawMessage(`{"channel":"old"}`),
			}, nil
		}
		return Decision{
			Audience: map[uuid.UUID]bool{activityServiceOther: true},
			Payload:  json.RawMessage(`{"channel":"new"}`),
		}, nil
	}

	err := service.MoveServerVoice(
		context.Background(), activityServiceSender, oldScope, newScope,
		func(context.Context) (bool, error) { return true, nil },
	)
	require.NoError(t, err)
	require.Len(t, store.sets, 1)
	assert.Equal(t, newRoom, store.sets[0].state.SourceToken)
	require.Len(t, delivery.plans, 1)
	assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true}, delivery.plans[0].ClearRecipients)
	assert.Equal(t, map[uuid.UUID]bool{activityServiceOther: true}, delivery.plans[0].UpdateRecipients)
}

func TestActivityService_MoveServerVoiceSuppressionCleansPriorGenerationAndAudience(t *testing.T) {
	oldScope := serverActivityScope()
	newScope := oldScope
	newScope.RoomID = uuid.MustParse("66666666-6666-6666-6666-666666666666")
	newScope.LifecycleID = newScope.RoomID
	newScope.EventAt = oldScope.EventAt.Add(time.Second)
	errDelete := errors.New("forced prior suppression delete failure")
	errDeliver := errors.New("forced prior suppression delivery failure")
	errDisconnect := errors.New("forced prior suppression targeted disconnect failure")
	errDisconnectAll := errors.New("forced prior suppression global disconnect failure")
	errInspect := errors.New("forced prior generation inspection failure")

	for _, test := range []struct {
		name                 string
		oldGenerationLive    bool
		deleteErr            error
		deliverErr           error
		disconnectErr        error
		disconnectAllErr     error
		wantClear            bool
		wantDisconnect       bool
		wantDisconnectAll    bool
		wantInspect          bool
		successorFound       bool
		successorActive      bool
		inspectErr           error
		wantNoSuccessorClear bool
		wantErrors           []error
	}{
		{
			name:              "matching prior generation is cleared",
			oldGenerationLive: true,
			wantClear:         true,
		},
		{
			name:                 "concurrent successor is retained and prior viewers disconnect",
			wantDisconnect:       true,
			wantInspect:          true,
			successorFound:       true,
			successorActive:      true,
			wantNoSuccessorClear: true,
		},
		{
			name:                 "missing prior generation disconnects all clients",
			wantInspect:          true,
			wantDisconnectAll:    true,
			wantNoSuccessorClear: true,
		},
		{
			name:                 "prior generation inspection failure disconnects all clients",
			wantInspect:          true,
			inspectErr:           errInspect,
			wantDisconnectAll:    true,
			wantNoSuccessorClear: true,
			wantErrors:           []error{errInspect},
		},
		{
			name:              "delete and global disconnect failures are joined",
			deleteErr:         errDelete,
			disconnectAllErr:  errDisconnectAll,
			wantDisconnectAll: true,
			wantErrors:        []error{errDelete, errDisconnectAll},
		},
		{
			name:              "clear and targeted disconnect failures are joined",
			oldGenerationLive: true,
			deliverErr:        errDeliver,
			disconnectErr:     errDisconnect,
			wantClear:         true,
			wantDisconnect:    true,
			wantErrors:        []error{errDeliver, errDisconnect},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, builder, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
			delivery.deliverErr = test.deliverErr
			delivery.disconnectErr = test.disconnectErr
			delivery.disconnectAllErr = test.disconnectAllErr
			builder.build = func(_ uuid.UUID, scope Scope) (BuiltActivity, error) {
				return BuiltActivity{
					Input: PolicyInput{
						SenderID: activityServiceSender,
						Category: CategoryServerVoice,
						ServerVoice: &ServerVoicePolicyInput{Context: ServerVoiceContext{
							ChannelID: scope.RoomID,
						}},
					},
					SourceToken: scope.LifecycleID, SourceVersion: scope.EventAt.UnixMicro(),
				}, nil
			}
			service.authorize = func(_ context.Context, input PolicyInput) (Decision, error) {
				if input.ServerVoice.Context.ChannelID == oldScope.RoomID {
					return Decision{
						Audience: map[uuid.UUID]bool{activityServiceViewer: true},
						Payload:  json.RawMessage(`{"channel":"old"}`),
					}, nil
				}
				return Decision{}, nil
			}
			store.compareAndDelete = func(
				_ context.Context,
				_ uuid.UUID,
				_ Category,
				token uuid.UUID,
				version int64,
			) (bool, error) {
				return test.oldGenerationLive && token == oldScope.LifecycleID &&
					version == oldScope.EventAt.UnixMicro(), test.deleteErr
			}
			store.getFound = test.successorFound
			store.getState = ActivityState{
				SourceToken: uuid.New(), SourceVersion: newScope.EventAt.UnixMicro(),
				Payload: json.RawMessage(`{"server_id":"successor"}`), UpdatedAt: time.Now().Unix(),
			}
			store.getErr = test.inspectErr
			store.isActiveResult = test.successorActive

			err := service.MoveServerVoice(
				context.Background(), activityServiceSender, oldScope, newScope,
				func(context.Context) (bool, error) { return true, nil },
			)

			if len(test.wantErrors) == 0 {
				require.NoError(t, err)
			} else {
				for _, wantErr := range test.wantErrors {
					assert.ErrorIs(t, err, wantErr)
				}
			}
			require.Len(t, store.deletes, 1)
			assert.Equal(t, oldScope.LifecycleID, store.deletes[0].token,
				"suppression after a committed move must clean the captured prior generation")
			assert.Equal(t, oldScope.EventAt.UnixMicro(), store.deletes[0].version)
			assert.Empty(t, store.sets)
			if test.wantClear {
				require.Len(t, delivery.plans, 1)
				assert.Equal(t, map[uuid.UUID]bool{
					activityServiceViewer: true,
				}, delivery.plans[0].ClearRecipients)
				if !test.wantDisconnect {
					assert.Empty(t, delivery.disconnects)
				}
			}
			if test.wantDisconnect {
				assert.Equal(t, []map[uuid.UUID]bool{{
					activityServiceViewer: true,
				}}, delivery.disconnects)
			}
			if test.wantNoSuccessorClear {
				assert.Empty(t, delivery.plans,
					"a stale cleanup must not clear a concurrent successor")
			}
			if test.wantInspect {
				assert.Len(t, store.gets, 1)
			} else {
				assert.Empty(t, store.gets)
			}
			if test.successorActive {
				require.Len(t, store.activeChecks, 1)
				assert.Equal(t, store.getState.SourceToken, store.activeChecks[0].token)
				assert.Equal(t, store.getState.SourceVersion, store.activeChecks[0].version)
			} else {
				assert.Empty(t, store.activeChecks)
			}
			if test.wantDisconnectAll {
				assert.Equal(t, 1, delivery.disconnectAllCalls)
			} else {
				assert.Zero(t, delivery.disconnectAllCalls)
			}
		})
	}
}

func TestActivityService_MutationNoOpIsInertAndFailureFailsClosed(t *testing.T) {
	errMutation := errors.New("forced mutation failure")
	for _, category := range []Category{CategoryServerVoice, CategoryPrivateCall} {
		for _, test := range []struct {
			name     string
			mutation ActivityMutation
			wantErr  error
		}{
			{name: "not applied", mutation: func(context.Context) (bool, error) { return false, nil }},
			{name: "failed", mutation: func(context.Context) (bool, error) { return false, errMutation }, wantErr: errMutation},
		} {
			t.Run(string(category)+"/refresh/"+test.name, func(t *testing.T) {
				service, builder, store, delivery, _ := newActivityServiceFixture(category)
				err := refreshActivityForTest(service, category, test.mutation)
				if test.wantErr != nil {
					assert.ErrorIs(t, err, test.wantErr)
				} else {
					require.NoError(t, err)
				}
				assert.Equal(t, 0, builder.calls)
				assert.Empty(t, store.sets)
				assert.Empty(t, store.deletes)
				assert.Empty(t, delivery.plans)
				if test.wantErr != nil {
					assert.Equal(t, 1, delivery.disconnectAllCalls)
				} else {
					assert.Zero(t, delivery.disconnectAllCalls)
				}
			})

			t.Run(string(category)+"/clear/"+test.name, func(t *testing.T) {
				service, builder, store, delivery, _ := newActivityServiceFixture(category)
				err := clearActivityForTest(service, category, test.mutation)
				if test.wantErr != nil {
					assert.ErrorIs(t, err, test.wantErr)
				} else {
					require.NoError(t, err)
				}
				assert.Equal(t, 1, builder.calls, "clear authorizes old state before mutation")
				assert.Empty(t, store.sets)
				assert.Empty(t, delivery.plans)
				if test.wantErr != nil {
					require.Len(t, store.deletes, 1)
					wantToken := activityServiceRoom
					if category == CategoryPrivateCall {
						wantToken = activityServiceCall
					}
					assert.Equal(t, wantToken, store.deletes[0].token)
					assert.Equal(t, activityServiceEvent.UnixMicro(), store.deletes[0].version)
					assert.Equal(t, 1, delivery.disconnectAllCalls)
				} else {
					assert.Empty(t, store.deletes)
					assert.Zero(t, delivery.disconnectAllCalls)
				}
			})
		}
	}
}

func TestActivityService_MoveMutationFailureDeletesOnlyPriorGeneration(t *testing.T) {
	service, builder, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	oldScope := serverActivityScope()
	newScope := oldScope
	newScope.RoomID = uuid.MustParse("66666666-6666-6666-6666-666666666666")
	newScope.LifecycleID = newScope.RoomID
	newScope.EventAt = oldScope.EventAt.Add(time.Second)
	builder.build = func(_ uuid.UUID, scope Scope) (BuiltActivity, error) {
		return BuiltActivity{
			Input: PolicyInput{
				SenderID: activityServiceSender,
				Category: CategoryServerVoice,
				ServerVoice: &ServerVoicePolicyInput{Context: ServerVoiceContext{
					ChannelID: scope.RoomID,
				}},
			},
			SourceToken: scope.LifecycleID, SourceVersion: scope.EventAt.UnixMicro(),
		}, nil
	}
	errMutation := errors.New("forced move mutation failure")

	err := service.MoveServerVoice(
		context.Background(), activityServiceSender, oldScope, newScope,
		func(context.Context) (bool, error) { return false, errMutation },
	)
	assert.ErrorIs(t, err, errMutation)
	require.Len(t, store.deletes, 1)
	assert.Equal(t, oldScope.LifecycleID, store.deletes[0].token)
	assert.Equal(t, oldScope.EventAt.UnixMicro(), store.deletes[0].version)
	assert.Equal(t, 1, delivery.disconnectAllCalls)
	assert.Empty(t, store.sets)
	assert.Empty(t, delivery.plans)
}

func TestActivityService_MoveOldStateFailureDeletesOnlyPriorGeneration(t *testing.T) {
	errOldState := errors.New("forced old-state failure")
	for _, seam := range []string{"build", "authorize"} {
		t.Run(seam, func(t *testing.T) {
			service, builder, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
			oldScope := serverActivityScope()
			newScope := oldScope
			newScope.RoomID = uuid.MustParse("66666666-6666-6666-6666-666666666666")
			newScope.LifecycleID = newScope.RoomID
			newScope.EventAt = oldScope.EventAt.Add(time.Second)
			builder.build = func(_ uuid.UUID, scope Scope) (BuiltActivity, error) {
				if seam == "build" {
					return BuiltActivity{}, errOldState
				}
				return BuiltActivity{
					Input: PolicyInput{
						SenderID: activityServiceSender,
						Category: CategoryServerVoice,
						ServerVoice: &ServerVoicePolicyInput{Context: ServerVoiceContext{
							ChannelID: scope.RoomID,
						}},
					},
					SourceToken: scope.LifecycleID, SourceVersion: scope.EventAt.UnixMicro(),
				}, nil
			}
			if seam == "authorize" {
				service.authorize = func(context.Context, PolicyInput) (Decision, error) {
					return Decision{}, errOldState
				}
			}

			err := service.MoveServerVoice(
				context.Background(), activityServiceSender, oldScope, newScope,
				func(context.Context) (bool, error) { return true, nil },
			)

			assert.ErrorIs(t, err, errOldState)
			require.Len(t, store.deletes, 1)
			assert.Equal(t, oldScope.LifecycleID, store.deletes[0].token)
			assert.Equal(t, oldScope.EventAt.UnixMicro(), store.deletes[0].version)
			assert.Equal(t, 1, delivery.disconnectAllCalls)
			assert.Empty(t, store.sets)
			assert.Empty(t, delivery.plans)
		})
	}
}

func TestActivityService_GenerationMismatchNeverClearsSuccessor(t *testing.T) {
	t.Run("refresh without a cached successor disconnects all", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		store.setResult = false

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.NoError(t, err)
		assert.Empty(t, delivery.plans)
		assert.Empty(t, store.deletes, "stale writer must not delete a successor")
		assert.Empty(t, delivery.disconnects)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("refresh preserves an exact active higher successor", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		ctx, cancel := context.WithCancel(context.Background())
		store.setResult = false
		store.getFound = true
		store.getState = ActivityState{
			SourceToken: uuid.New(), SourceVersion: serverActivityScope().EventAt.Add(time.Second).UnixMicro(),
			Payload: json.RawMessage(`{"server_id":"successor"}`), UpdatedAt: time.Now().Unix(),
		}
		store.isActiveResult = true
		store.onSet = cancel

		err := service.RefreshServerVoice(ctx, activityServiceSender, serverActivityScope(), nil)

		require.NoError(t, err)
		assert.Empty(t, delivery.plans)
		require.Len(t, store.activeChecks, 1)
		assert.Equal(t, store.getState.SourceToken, store.activeChecks[0].token)
		assert.Equal(t, store.getState.SourceVersion, store.activeChecks[0].version)
		assert.Equal(t, []map[uuid.UUID]bool{{activityServiceViewer: true}}, delivery.disconnects)
		assert.Zero(t, delivery.disconnectAllCalls)
		require.Len(t, store.activeContextErrors, 1)
		assert.NoError(t, store.activeContextErrors[0])
		require.Len(t, store.activeContextHasDeadline, 1)
		require.True(t, store.activeContextHasDeadline[0])
		require.Len(t, store.activeContextDeadlines, 1)
		remaining := time.Until(store.activeContextDeadlines[0])
		assert.Positive(t, remaining)
		assert.LessOrEqual(t, remaining, activityCleanupTimeout)
	})

	t.Run("clear preserves an exact active higher successor", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		store.deleteResult = false
		store.getFound = true
		store.getState = ActivityState{
			SourceToken: uuid.New(), SourceVersion: privateActivityScope().EventAt.Add(time.Second).UnixMicro(),
			Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: time.Now().Unix(),
		}
		store.isActiveResult = true

		err := service.ClearPrivateCall(context.Background(), activityServiceSender, privateActivityScope(), nil, nil)

		require.NoError(t, err)
		assert.Empty(t, delivery.plans)
		assert.Len(t, store.gets, 1)
		require.Len(t, store.activeChecks, 1)
		assert.Equal(t, store.getState.SourceToken, store.activeChecks[0].token)
		assert.Equal(t, store.getState.SourceVersion, store.activeChecks[0].version)
		assert.Equal(t, []map[uuid.UUID]bool{{activityServiceViewer: true}}, delivery.disconnects)
		assert.Zero(t, delivery.disconnectAllCalls)
	})

	t.Run("higher inactive or mismatched successor disconnects all", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		store.deleteResult = false
		store.getFound = true
		store.getState = ActivityState{
			SourceToken: uuid.New(), SourceVersion: privateActivityScope().EventAt.Add(time.Second).UnixMicro(),
			Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: time.Now().Unix(),
		}

		err := service.ClearPrivateCall(context.Background(), activityServiceSender, privateActivityScope(), nil, nil)

		require.NoError(t, err)
		assert.Empty(t, delivery.plans)
		assert.Len(t, store.activeChecks, 1)
		assert.Empty(t, delivery.disconnects)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("clear missing generation disconnects all", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		store.deleteResult = false

		err := service.ClearPrivateCall(context.Background(), activityServiceSender, privateActivityScope(), nil, nil)

		require.NoError(t, err)
		assert.Empty(t, delivery.plans)
		assert.Len(t, store.gets, 1)
		assert.Empty(t, store.activeChecks)
		assert.Empty(t, delivery.disconnects)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("clear malformed stored state disconnects all and returns error", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		store.deleteResult = false
		store.getErr = ErrMalformedActivityState

		err := service.ClearPrivateCall(context.Background(), activityServiceSender, privateActivityScope(), nil, nil)

		require.ErrorIs(t, err, ErrMalformedActivityState)
		assert.Empty(t, delivery.plans)
		assert.Len(t, store.gets, 1)
		assert.Empty(t, store.activeChecks)
		assert.Empty(t, delivery.disconnects)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("successor verification failure disconnects all and returns error", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		store.deleteResult = false
		store.getFound = true
		store.getState = ActivityState{
			SourceToken: uuid.New(), SourceVersion: privateActivityScope().EventAt.Add(time.Second).UnixMicro(),
			Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: time.Now().Unix(),
		}
		store.isActiveErr = ErrMalformedActivityLifecycle

		err := service.ClearPrivateCall(context.Background(), activityServiceSender, privateActivityScope(), nil, nil)

		require.ErrorIs(t, err, ErrMalformedActivityLifecycle)
		assert.Empty(t, delivery.plans)
		assert.Len(t, store.activeChecks, 1)
		assert.Empty(t, delivery.disconnects)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})
}

// The storm regression (#2444). Ten consecutive suppressed refreshes with an
// empty store must produce zero disconnects.
func TestActivityService_SuppressedSenderDoesNotStormDisconnects(t *testing.T) {
	service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience:                   map[uuid.UUID]bool{},
			SuppressedBySenderPresence: true,
		}, nil
	}
	store.deleteResult = false // nothing stored: the steady state

	for i := 0; i < 10; i++ {
		require.NoError(t, service.RefreshServerVoice(
			context.Background(), activityServiceSender, serverActivityScope(), nil,
		))
	}

	require.Zero(t, delivery.disconnectAllCalls,
		"a suppressed sender must never trigger a global disconnect")
	require.Empty(t, delivery.plans)
	require.Len(t, store.deletes, 10)
	require.Empty(t, store.sets)
}

// Sibling of TestActivityService_SuppressionClassifiesMissingGeneration for the
// hidden-sender reason (#2444). Only the absent-state row is reclassified.
func TestActivityService_SuppressionClassifiesHiddenSender(t *testing.T) {
	newHiddenFixture := func() (*ActivityService, *activityServiceStoreStub, *activityServiceDeliveryStub) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		service.authorize = func(context.Context, PolicyInput) (Decision, error) {
			return Decision{
				Audience:                   map[uuid.UUID]bool{},
				SuppressedBySenderPresence: true,
			}, nil
		}
		store.deleteResult = false
		return service, store, delivery
	}

	t.Run("absent state is a benign terminal", func(t *testing.T) {
		service, store, delivery := newHiddenFixture()

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.NoError(t, err)
		assert.Len(t, store.deletes, 1)
		assert.Len(t, store.gets, 1)
		assert.Zero(t, delivery.disconnectAllCalls)
		assert.Empty(t, delivery.plans)
	})

	t.Run("delete failure disconnects and is returned", func(t *testing.T) {
		service, store, delivery := newHiddenFixture()
		deleteErr := errors.New("forced hidden-sender delete failure")
		store.deleteErr = deleteErr

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.ErrorIs(t, err, deleteErr)
		assert.Empty(t, store.gets)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("inspection failure disconnects and is returned", func(t *testing.T) {
		service, store, delivery := newHiddenFixture()
		inspectErr := errors.New("forced hidden-sender inspection failure")
		store.getErr = inspectErr

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.ErrorIs(t, err, inspectErr)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("live successor disconnects", func(t *testing.T) {
		service, store, delivery := newHiddenFixture()
		store.getFound = true
		store.getState = ActivityState{
			SourceToken:   uuid.New(),
			SourceVersion: serverActivityScope().EventAt.Add(time.Second).UnixMicro(),
			Payload:       json.RawMessage(`{"server_id":"successor"}`),
			UpdatedAt:     time.Now().Unix(),
		}
		store.isActiveResult = true

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.NoError(t, err)
		assert.Equal(t, 1, delivery.disconnectAllCalls,
			"a live generation must not stay published while the sender is hidden")
		assert.Empty(t, delivery.plans)
	})

	t.Run("removed generation is not a silent no-op", func(t *testing.T) {
		service, store, delivery := newHiddenFixture()
		store.deleteResult = true

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.NoError(t, err)
		assert.Len(t, store.deletes, 1)
		assert.Empty(t, store.gets, "a removed generation needs no successor inspection")
		assert.Positive(t, delivery.disconnectAllCalls+len(delivery.plans)+len(delivery.disconnects),
			"a removed generation must clear or disconnect its prior audience")
	})
}

// The clear path must honour the hidden-sender reason too (#2444).
//
// Leaving voice while invisible is an ordinary user action, and the level arm
// has already deleted the stored generation by then. Routing that clear through
// the reason-blind suppressGeneration made CompareAndDelete miss, find no
// successor, and force-disconnect every Rich-Presence client on the replica --
// the same class of user-triggerable global disconnect the storm guard exists to
// prevent, reached through the clear path instead of the refresh path.
func TestActivityService_HiddenSenderClearDoesNotDisconnectAll(t *testing.T) {
	service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{
			Audience:                   map[uuid.UUID]bool{},
			SuppressedBySenderPresence: true,
		}, nil
	}
	// The level arm already removed the row on an earlier suppressed heartbeat.
	store.deleteResult = false

	err := service.ClearServerVoice(
		context.Background(), activityServiceSender, serverActivityScope(), nil,
	)

	require.NoError(t, err)
	assert.Zero(t, delivery.disconnectAllCalls,
		"leaving voice while invisible must not disconnect the whole replica")
	assert.Empty(t, delivery.plans)
}

func TestActivityService_SuppressionClassifiesMissingGeneration(t *testing.T) {
	newSuppressedFixture := func() (*ActivityService, *activityServiceStoreStub, *activityServiceDeliveryStub) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		service.authorize = func(context.Context, PolicyInput) (Decision, error) {
			return Decision{Audience: map[uuid.UUID]bool{}}, nil
		}
		store.deleteResult = false
		return service, store, delivery
	}

	t.Run("successor remains without disconnect", func(t *testing.T) {
		service, store, delivery := newSuppressedFixture()
		store.getFound = true
		store.getState = ActivityState{
			SourceToken: uuid.New(), SourceVersion: serverActivityScope().EventAt.Add(time.Second).UnixMicro(),
			Payload: json.RawMessage(`{"server_id":"successor"}`), UpdatedAt: time.Now().Unix(),
		}
		store.isActiveResult = true

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.NoError(t, err)
		require.Len(t, store.deletes, 1)
		assert.Len(t, store.gets, 1)
		require.Len(t, store.activeChecks, 1)
		assert.Equal(t, store.getState.SourceToken, store.activeChecks[0].token)
		assert.Equal(t, store.getState.SourceVersion, store.activeChecks[0].version)
		assert.Empty(t, store.exactDeletes, "lifecycle suppression must not erase a successor")
		assert.Empty(t, store.sets)
		assert.Empty(t, delivery.plans)
		assert.Zero(t, delivery.disconnectAllCalls)
	})

	t.Run("absent state disconnects stale clients", func(t *testing.T) {
		service, store, delivery := newSuppressedFixture()

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.NoError(t, err)
		assert.Len(t, store.gets, 1)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("inspection failure disconnects and is returned", func(t *testing.T) {
		service, store, delivery := newSuppressedFixture()
		inspectErr := errors.New("forced suppressed-state inspection failure")
		store.getErr = inspectErr

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.ErrorIs(t, err, inspectErr)
		assert.Len(t, store.gets, 1)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("non-successor state disconnects and is returned", func(t *testing.T) {
		service, store, delivery := newSuppressedFixture()
		store.getFound = true
		store.getState = ActivityState{
			SourceToken:   serverActivityScope().LifecycleID,
			SourceVersion: serverActivityScope().EventAt.UnixMicro(),
			Payload:       json.RawMessage(`{"server_id":"same-generation"}`), UpdatedAt: time.Now().Unix(),
		}

		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)

		require.ErrorContains(t, err, "not a successor")
		assert.Len(t, store.gets, 1)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})
}

func TestActivityService_AuthorizedStateWithNoConnectedViewersStillPersists(t *testing.T) {
	service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
	service.authorize = func(context.Context, PolicyInput) (Decision, error) {
		return Decision{Audience: map[uuid.UUID]bool{}, Payload: json.RawMessage(`{"server_id":"authorized"}`)}, nil
	}

	err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)
	require.NoError(t, err)
	assert.Len(t, store.sets, 1)
	assert.Empty(t, store.exactDeletes)
	require.Len(t, delivery.plans, 1)
	assert.Empty(t, delivery.plans[0].UpdateRecipients)
}

func TestActivityService_FailsClosedAtEveryErrorSeam(t *testing.T) {
	errForced := errors.New("forced activity service failure")

	t.Run("post-mutation builder failure cleans expected generation and disconnects all", func(t *testing.T) {
		service, builder, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		builder.err = errForced
		err := service.RefreshServerVoice(
			context.Background(), activityServiceSender, serverActivityScope(),
			func(context.Context) (bool, error) { return true, nil },
		)
		assert.ErrorIs(t, err, errForced)
		require.Len(t, store.deletes, 1)
		assert.Equal(t, serverActivityScope().LifecycleID, store.deletes[0].token)
		assert.Equal(t, serverActivityScope().EventAt.UnixMicro(), store.deletes[0].version)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("policy failure cleans built generation and disconnects all", func(t *testing.T) {
		service, builder, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		service.authorize = func(context.Context, PolicyInput) (Decision, error) { return Decision{}, errForced }
		err := service.RefreshPrivateCall(context.Background(), activityServiceSender, privateActivityScope(), nil, nil)
		assert.ErrorIs(t, err, errForced)
		require.Len(t, store.deletes, 1)
		assert.Equal(t, builder.built.SourceVersion, store.deletes[0].version)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("store set failure attempts exact cleanup and disconnects all", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		store.setErr = errForced
		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)
		assert.ErrorIs(t, err, errForced)
		assert.Len(t, store.deletes, 1)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
		assert.Empty(t, delivery.plans)
	})

	t.Run("suppression delete and disconnect failures are both returned", func(t *testing.T) {
		errDelete := errors.New("forced suppression delete failure")
		errDisconnect := errors.New("forced suppression disconnect failure")
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		service.authorize = func(context.Context, PolicyInput) (Decision, error) {
			return Decision{}, nil
		}
		store.deleteErr = errDelete
		delivery.disconnectAllErr = errDisconnect
		err := service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)
		assert.ErrorIs(t, err, errDelete)
		assert.ErrorIs(t, err, errDisconnect)
		assert.Len(t, store.deletes, 1)
		assert.Empty(t, store.exactDeletes)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("delivery failure deletes matching generation and disconnects intended union", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		delivery.deliverErr = errForced
		err := service.RefreshPrivateCall(
			context.Background(), activityServiceSender, privateActivityScope(),
			map[uuid.UUID]bool{activityServiceOther: true}, nil,
		)
		assert.ErrorIs(t, err, errForced)
		assert.Len(t, store.deletes, 1)
		require.Len(t, delivery.disconnects, 1)
		assert.Equal(t, map[uuid.UUID]bool{
			activityServiceViewer: true,
			activityServiceOther:  true,
		}, delivery.disconnects[0])
	})

	t.Run("delivery cleanup failure still attempts all-client disconnect", func(t *testing.T) {
		errCleanup := errors.New("forced cleanup failure")
		errDisconnect := errors.New("forced disconnect failure")
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		delivery.deliverErr = errForced
		store.deleteErr = errCleanup
		delivery.disconnectAllErr = errDisconnect
		err := service.RefreshPrivateCall(
			context.Background(), activityServiceSender, privateActivityScope(), nil, nil,
		)
		assert.ErrorIs(t, err, errForced)
		assert.ErrorIs(t, err, errCleanup)
		assert.ErrorIs(t, err, errDisconnect)
		assert.Len(t, store.deletes, 1)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("delivery cleanup miss preserves a verified successor", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		delivery.deliverErr = errForced
		store.deleteResult = false
		store.getFound = true
		store.getState = ActivityState{
			SourceToken: uuid.New(), SourceVersion: privateActivityScope().EventAt.Add(time.Second).UnixMicro(),
			Payload: json.RawMessage(`{"call_type":"dm"}`), UpdatedAt: time.Now().Unix(),
		}
		store.isActiveResult = true
		err := service.RefreshPrivateCall(
			context.Background(), activityServiceSender, privateActivityScope(), nil, nil,
		)
		assert.ErrorIs(t, err, errForced)
		assert.Len(t, store.gets, 1)
		require.Len(t, store.activeChecks, 1)
		assert.Equal(t, store.getState.SourceToken, store.activeChecks[0].token)
		assert.Equal(t, store.getState.SourceVersion, store.activeChecks[0].version)
		assert.Len(t, delivery.disconnects, 1)
		assert.Zero(t, delivery.disconnectAllCalls)
	})

	t.Run("delivery cleanup miss with absent state disconnects all", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		delivery.deliverErr = errForced
		store.deleteResult = false
		err := service.RefreshPrivateCall(
			context.Background(), activityServiceSender, privateActivityScope(), nil, nil,
		)
		assert.ErrorIs(t, err, errForced)
		assert.Len(t, store.gets, 1)
		assert.Empty(t, delivery.disconnects)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("delivery cleanup inspection failure disconnects all and is returned", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		delivery.deliverErr = errForced
		store.deleteResult = false
		inspectErr := errors.New("forced delivery generation inspection failure")
		store.getErr = inspectErr
		err := service.RefreshPrivateCall(
			context.Background(), activityServiceSender, privateActivityScope(), nil, nil,
		)
		assert.ErrorIs(t, err, errForced)
		assert.ErrorIs(t, err, inspectErr)
		assert.Len(t, store.gets, 1)
		assert.Empty(t, delivery.disconnects)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("clear store failure emits no frame and disconnects all", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		store.deleteErr = errForced
		err := service.ClearServerVoice(context.Background(), activityServiceSender, serverActivityScope(), nil)
		assert.ErrorIs(t, err, errForced)
		assert.Empty(t, delivery.plans)
		assert.Equal(t, 1, delivery.disconnectAllCalls)
	})

	t.Run("clear delivery failure disconnects the authorized audience", func(t *testing.T) {
		service, _, _, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		delivery.deliverErr = errForced
		err := service.ClearPrivateCall(context.Background(), activityServiceSender, privateActivityScope(), nil, nil)
		assert.ErrorIs(t, err, errForced)
		require.Len(t, delivery.disconnects, 1)
		assert.Equal(t, map[uuid.UUID]bool{activityServiceViewer: true}, delivery.disconnects[0])
	})

	t.Run("clear build and policy failures occur after applied mutation", func(t *testing.T) {
		for _, seam := range []string{"build", "policy"} {
			t.Run(seam, func(t *testing.T) {
				service, builder, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
				if seam == "build" {
					builder.err = errForced
				} else {
					service.authorize = func(context.Context, PolicyInput) (Decision, error) {
						return Decision{}, errForced
					}
				}
				mutated := false
				err := service.ClearServerVoice(
					context.Background(), activityServiceSender, serverActivityScope(),
					func(context.Context) (bool, error) { mutated = true; return true, nil },
				)
				assert.ErrorIs(t, err, errForced)
				assert.True(t, mutated)
				assert.Len(t, store.deletes, 1)
				assert.Equal(t, 1, delivery.disconnectAllCalls)
			})
		}
	})
}

func TestActivityService_CancellationAndCoordination(t *testing.T) {
	t.Run("pre-canceled context performs no work", func(t *testing.T) {
		service, builder, store, delivery, coordinator := newActivityServiceFixture(CategoryServerVoice)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := service.RefreshServerVoice(ctx, activityServiceSender, serverActivityScope(), nil)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, coordinator.calls)
		assert.Zero(t, builder.calls)
		assert.Empty(t, store.sets)
		assert.Empty(t, delivery.plans)
	})

	t.Run("cancellation after mutation uses detached exact cleanup", func(t *testing.T) {
		service, builder, store, delivery, _ := newActivityServiceFixture(CategoryServerVoice)
		ctx, cancel := context.WithCancel(context.Background())
		builder.err = context.Canceled

		err := service.RefreshServerVoice(
			ctx, activityServiceSender, serverActivityScope(),
			func(context.Context) (bool, error) {
				cancel()
				return true, nil
			},
		)
		assert.ErrorIs(t, err, context.Canceled)
		require.Len(t, store.deleteContextErrors, 1)
		assert.NoError(t, store.deleteContextErrors[0])
		require.Len(t, delivery.disconnectAllContextErrors, 1)
		assert.NoError(t, delivery.disconnectAllContextErrors[0])
	})

	t.Run("cancellation after persistence cleans state and disconnects prepared viewers", func(t *testing.T) {
		service, _, store, delivery, _ := newActivityServiceFixture(CategoryPrivateCall)
		ctx, cancel := context.WithCancel(context.Background())
		store.onSet = cancel
		delivery.respectContext = true

		err := service.RefreshPrivateCall(
			ctx, activityServiceSender, privateActivityScope(),
			map[uuid.UUID]bool{activityServiceOther: true}, nil,
		)
		assert.ErrorIs(t, err, context.Canceled)
		require.Len(t, store.deleteContextErrors, 1)
		assert.NoError(t, store.deleteContextErrors[0])
		require.Len(t, delivery.disconnectContextErrors, 1)
		assert.NoError(t, delivery.disconnectContextErrors[0])
	})

	t.Run("one sender gate surrounds the full operation without nesting", func(t *testing.T) {
		service, builder, store, delivery, coordinator := newActivityServiceFixture(CategoryPrivateCall)
		checkGate := func() { assert.Equal(t, 1, coordinator.depthNow()) }
		builder.onBuild = checkGate
		store.onSet = checkGate
		delivery.onDeliver = checkGate
		service.authorize = func(context.Context, PolicyInput) (Decision, error) {
			checkGate()
			return Decision{
				Audience: map[uuid.UUID]bool{activityServiceViewer: true},
				Payload:  json.RawMessage(`{"call_type":"dm","participant_count":2}`),
			}, nil
		}

		require.NoError(t, service.RefreshPrivateCall(
			context.Background(), activityServiceSender, privateActivityScope(), nil, nil,
		))
		assert.Equal(t, 1, coordinator.calls)
		assert.Equal(t, 1, coordinator.maxDepth)
	})
}

func TestNewActivityService_WiresProductionPolicyAndRejectsInvalidCalls(t *testing.T) {
	coordinator := &activityServiceCoordinatorStub{}
	delivery := &activityServiceDeliveryStub{}
	service := NewActivityService(
		coordinator,
		NewActivityBuilder(nil, nil),
		NewActivityStore(nil),
		nil,
		nil,
		delivery,
		alwaysPermitPresence{},
	)
	require.NotNil(t, service)
	_, err := service.authorize(context.Background(), PolicyInput{})
	assert.Error(t, err)

	invalidCategory := serverActivityScope()
	invalidCategory.Category = CategoryPrivateCall
	assert.ErrorIs(t, service.RefreshServerVoice(
		context.Background(), activityServiceSender, invalidCategory, nil,
	), ErrInvalidActivityScope)
	assert.ErrorIs(t, service.RefreshServerVoice(
		context.Background(), uuid.Nil, serverActivityScope(), nil,
	), ErrInvalidActivityScope)
	assert.Error(t, (*ActivityService)(nil).RefreshServerVoice(
		context.Background(), activityServiceSender, serverActivityScope(), nil,
	))
}

func serverActivityScope() Scope {
	return Scope{
		Category: CategoryServerVoice, RoomID: activityServiceRoom,
		LifecycleID: activityServiceRoom, EventAt: activityServiceEvent,
	}
}

func privateActivityScope() Scope {
	return Scope{
		Category: CategoryPrivateCall, RoomID: activityServiceRoom,
		LifecycleID: activityServiceCall, EventAt: activityServiceEvent,
	}
}

func newActivityServiceFixture(category Category) (
	*ActivityService,
	*activityServiceBuilderStub,
	*activityServiceStoreStub,
	*activityServiceDeliveryStub,
	*activityServiceCoordinatorStub,
) {
	built := BuiltActivity{
		Input:       PolicyInput{SenderID: activityServiceSender, Category: category},
		SourceToken: activityServiceRoom, SourceVersion: activityServiceEvent.UnixMicro(),
	}
	if category == CategoryServerVoice {
		built.Input.ServerVoice = &ServerVoicePolicyInput{}
	} else {
		built.Input.PrivateCall = &PrivateCallPolicyInput{}
		built.SourceToken = activityServiceCall
	}
	builder := &activityServiceBuilderStub{built: built}
	store := &activityServiceStoreStub{setResult: true, deleteResult: true}
	delivery := &activityServiceDeliveryStub{}
	coordinator := &activityServiceCoordinatorStub{}
	service := newActivityService(
		coordinator,
		builder,
		store,
		func(context.Context, PolicyInput) (Decision, error) {
			return Decision{
				Audience: map[uuid.UUID]bool{activityServiceViewer: true},
				Payload:  json.RawMessage(`{"authorized":true}`),
			}, nil
		},
		delivery,
	)
	service.settingsRecipients = func(
		context.Context,
		uuid.UUID,
		ActivityPolicySettings,
		ActivityPolicySettings,
	) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{
			activityServiceSender: true,
			activityServiceViewer: true,
		}, nil
	}
	return service, builder, store, delivery, coordinator
}

func refreshActivityForTest(service *ActivityService, category Category, mutation ActivityMutation) error {
	if category == CategoryServerVoice {
		return service.RefreshServerVoice(context.Background(), activityServiceSender, serverActivityScope(), mutation)
	}
	return service.RefreshPrivateCall(
		context.Background(), activityServiceSender, privateActivityScope(), nil, mutation,
	)
}

func clearActivityForTest(service *ActivityService, category Category, mutation ActivityMutation) error {
	if category == CategoryServerVoice {
		return service.ClearServerVoice(context.Background(), activityServiceSender, serverActivityScope(), mutation)
	}
	return service.ClearPrivateCall(
		context.Background(), activityServiceSender, privateActivityScope(), nil, mutation,
	)
}

type activityServiceBuilderStub struct {
	built   BuiltActivity
	err     error
	calls   int
	onBuild func()
	build   func(uuid.UUID, Scope) (BuiltActivity, error)
}

func (b *activityServiceBuilderStub) Build(_ context.Context, senderID uuid.UUID, scope Scope) (BuiltActivity, error) {
	b.calls++
	if b.onBuild != nil {
		b.onBuild()
	}
	if b.build != nil {
		return b.build(senderID, scope)
	}
	return b.built, b.err
}

type activityStoreCall struct {
	userID   uuid.UUID
	category Category
	state    ActivityState
	token    uuid.UUID
	version  int64
}

type activityServiceStoreStub struct {
	setResult        bool
	setErr           error
	deleteResult     bool
	deleteErr        error
	sets             []activityStoreCall
	deletes          []activityStoreCall
	exactDeletes     []activityStoreCall
	onSet            func()
	onDelete         func()
	delete           func(context.Context, uuid.UUID, Category) error
	compareAndDelete func(
		context.Context, uuid.UUID, Category, uuid.UUID, int64,
	) (bool, error)
	getState ActivityState
	getFound bool
	getErr   error
	gets     []activityStoreCall

	isActiveResult           bool
	isActiveErr              error
	activeChecks             []activityStoreCall
	activeContextErrors      []error
	activeContextDeadlines   []time.Time
	activeContextHasDeadline []bool

	deleteContextErrors []error
	exactDeleteContexts []context.Context
}

func (s *activityServiceStoreStub) Get(
	_ context.Context,
	userID uuid.UUID,
	category Category,
) (ActivityState, bool, error) {
	s.gets = append(s.gets, activityStoreCall{userID: userID, category: category})
	return s.getState, s.getFound, s.getErr
}

func (s *activityServiceStoreStub) IsActiveGeneration(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
	token uuid.UUID,
	version int64,
) (bool, error) {
	s.activeChecks = append(s.activeChecks, activityStoreCall{
		userID: userID, category: category, token: token, version: version,
	})
	s.activeContextErrors = append(s.activeContextErrors, ctx.Err())
	deadline, hasDeadline := ctx.Deadline()
	s.activeContextDeadlines = append(s.activeContextDeadlines, deadline)
	s.activeContextHasDeadline = append(s.activeContextHasDeadline, hasDeadline)
	return s.isActiveResult, s.isActiveErr
}

func (s *activityServiceStoreStub) Delete(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
) error {
	s.exactDeleteContexts = append(s.exactDeleteContexts, ctx)
	s.exactDeletes = append(s.exactDeletes, activityStoreCall{userID: userID, category: category})
	if s.onDelete != nil {
		s.onDelete()
	}
	if s.delete != nil {
		return s.delete(ctx, userID, category)
	}
	return s.deleteErr
}

func (s *activityServiceStoreStub) CompareAndSet(
	_ context.Context,
	userID uuid.UUID,
	category Category,
	state ActivityState,
) (bool, error) {
	s.sets = append(s.sets, activityStoreCall{userID: userID, category: category, state: state})
	if s.onSet != nil {
		s.onSet()
	}
	return s.setResult, s.setErr
}

func (s *activityServiceStoreStub) CompareAndSetActive(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
	state ActivityState,
) (bool, error) {
	return s.CompareAndSet(ctx, userID, category, state)
}

func (s *activityServiceStoreStub) CompareAndDelete(
	ctx context.Context,
	userID uuid.UUID,
	category Category,
	token uuid.UUID,
	version int64,
) (bool, error) {
	s.deleteContextErrors = append(s.deleteContextErrors, ctx.Err())
	s.deletes = append(s.deletes, activityStoreCall{
		userID: userID, category: category, token: token, version: version,
	})
	if s.onDelete != nil {
		s.onDelete()
	}
	if s.compareAndDelete != nil {
		return s.compareAndDelete(ctx, userID, category, token, version)
	}
	return s.deleteResult, s.deleteErr
}

type activityServiceDeliveryStub struct {
	plans              []DeliveryPlan
	disconnects        []map[uuid.UUID]bool
	disconnectAllCalls int
	deliverErr         error
	disconnectErr      error
	disconnectAllErr   error
	onDeliver          func()
	onDisconnect       func(context.Context)
	onDisconnectAll    func(context.Context)
	respectContext     bool

	disconnectContextErrors    []error
	disconnectContexts         []context.Context
	disconnectAllContextErrors []error
	disconnectAllContexts      []context.Context
}

func (d *activityServiceDeliveryStub) DeliverRichPresence(ctx context.Context, plan DeliveryPlan) error {
	d.plans = append(d.plans, plan)
	if d.onDeliver != nil {
		d.onDeliver()
	}
	if d.respectContext && ctx.Err() != nil {
		return ctx.Err()
	}
	return d.deliverErr
}

func (d *activityServiceDeliveryStub) DisconnectRichPresenceClients(
	ctx context.Context,
	recipients map[uuid.UUID]bool,
) error {
	d.disconnectContextErrors = append(d.disconnectContextErrors, ctx.Err())
	d.disconnectContexts = append(d.disconnectContexts, ctx)
	d.disconnects = append(d.disconnects, copyActivityAudience(recipients))
	if d.onDisconnect != nil {
		d.onDisconnect(ctx)
	}
	return d.disconnectErr
}

func (d *activityServiceDeliveryStub) DisconnectAllRichPresenceClients(ctx context.Context) error {
	d.disconnectAllContextErrors = append(d.disconnectAllContextErrors, ctx.Err())
	d.disconnectAllContexts = append(d.disconnectAllContexts, ctx)
	d.disconnectAllCalls++
	if d.onDisconnectAll != nil {
		d.onDisconnectAll(ctx)
	}
	return d.disconnectAllErr
}

type activityServiceCoordinatorStub struct {
	mu       sync.Mutex
	depth    int
	maxDepth int
	calls    int
}

func (c *activityServiceCoordinatorStub) WithSender(
	ctx context.Context,
	_ uuid.UUID,
	work func() error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.calls++
	c.depth++
	if c.depth > c.maxDepth {
		c.maxDepth = c.depth
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.depth--
		c.mu.Unlock()
	}()
	return work()
}

func (c *activityServiceCoordinatorStub) depthNow() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.depth
}

func copyActivityAudience(in map[uuid.UUID]bool) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(in))
	for id, included := range in {
		if included {
			out[id] = true
		}
	}
	return out
}
