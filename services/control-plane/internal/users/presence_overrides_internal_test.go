package users

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func excludedIDs(ids ...string) *[]string {
	return &ids
}

func validPresenceOverrideRequest(ids ...string) presenceOverrideRequest {
	return presenceOverrideRequest{
		EncryptedData:   "Y2lwaGVydGV4dA==",
		ExpectedVersion: 0,
		ExcludedUserIDs: excludedIDs(ids...),
	}
}

func TestValidatePresenceOverrideRequest_Category(t *testing.T) {
	senderID := uuid.New()

	_, err := validatePresenceOverrideRequest("activity", senderID, validPresenceOverrideRequest())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom_text")
}

func TestValidatePresenceOverrideRequest_EncryptedData(t *testing.T) {
	senderID := uuid.New()

	t.Run("rejects empty ciphertext", func(t *testing.T) {
		req := validPresenceOverrideRequest()
		req.EncryptedData = ""

		_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "nonempty")
	})

	t.Run("rejects invalid base64", func(t *testing.T) {
		req := validPresenceOverrideRequest()
		req.EncryptedData = "not-base64!"

		_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "base64")
	})

	t.Run("accepts exactly 64 KiB encoded", func(t *testing.T) {
		req := validPresenceOverrideRequest()
		req.EncryptedData = strings.Repeat("A", presenceOverrideMaxEncryptedDataBytes)

		_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)

		require.NoError(t, err)
	})

	t.Run("rejects more than 64 KiB encoded", func(t *testing.T) {
		req := validPresenceOverrideRequest()
		req.EncryptedData = strings.Repeat("A", presenceOverrideMaxEncryptedDataBytes+4)

		_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "65536")
	})
}

func TestValidatePresenceOverrideRequest_ExpectedVersion(t *testing.T) {
	req := validPresenceOverrideRequest()
	req.ExpectedVersion = -1

	_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, uuid.New(), req)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected_version")
}

func TestValidatePresenceOverrideRequest_ExcludedUserIDsRequired(t *testing.T) {
	for _, body := range []string{
		`{"encrypted_data":"YQ==","expected_version":0}`,
		`{"encrypted_data":"YQ==","expected_version":0,"excluded_user_ids":null}`,
	} {
		var req presenceOverrideRequest
		require.NoError(t, json.Unmarshal([]byte(body), &req))

		_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, uuid.New(), req)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "excluded_user_ids")
	}
}

func TestValidatePresenceOverrideRequest_ExplicitEmptyExcludedUserIDs(t *testing.T) {
	var req presenceOverrideRequest
	require.NoError(t, json.Unmarshal([]byte(
		`{"encrypted_data":"YQ==","expected_version":0,"excluded_user_ids":[]}`,
	), &req))

	normalized, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, uuid.New(), req)

	require.NoError(t, err)
	assert.Empty(t, normalized.ExcludedUserIDs)
}

func TestValidatePresenceOverrideRequest_TargetIDs(t *testing.T) {
	senderID := uuid.New()

	t.Run("rejects malformed UUID", func(t *testing.T) {
		req := validPresenceOverrideRequest("not-a-uuid")

		_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "UUID")
	})

	t.Run("rejects sender", func(t *testing.T) {
		req := validPresenceOverrideRequest(senderID.String())

		_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "self")
	})
}

func TestValidatePresenceOverrideRequest_CanonicalizesBeforeCap(t *testing.T) {
	senderID := uuid.New()
	first := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	req := validPresenceOverrideRequest(first.String(), second.String(), first.String())

	normalized, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)

	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{second, first}, normalized.ExcludedUserIDs)

	duplicates := make([]string, presenceOverrideMaxTargets+1)
	for i := range duplicates {
		duplicates[i] = first.String()
	}
	req = validPresenceOverrideRequest(duplicates...)

	normalized, err = validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)

	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{first}, normalized.ExcludedUserIDs)
}

func TestValidatePresenceOverrideRequest_RejectsMoreThanTargetCap(t *testing.T) {
	ids := make([]string, 0, presenceOverrideMaxTargets+1)
	for i := 0; i <= presenceOverrideMaxTargets; i++ {
		ids = append(ids, uuid.NewString())
	}

	_, err := validatePresenceOverrideRequest(
		presenceOverrideCategoryCustomText,
		uuid.New(),
		validPresenceOverrideRequest(ids...),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "1000")
}

func TestDecodePresenceOverrideRequest_BoundsBody(t *testing.T) {
	body := `{"encrypted_data":"` + strings.Repeat("A", presenceOverrideMaxRequestBodyBytes) +
		`","expected_version":0,"excluded_user_ids":[]}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	w := httptest.NewRecorder()

	_, err := decodePresenceOverrideRequest(w, req)

	var maxBytesErr *http.MaxBytesError
	require.Error(t, err)
	assert.True(t, errors.As(err, &maxBytesErr))
}

func TestDecodePresenceOverrideRequest_RejectsTrailingJSON(t *testing.T) {
	body := `{"encrypted_data":"YQ==","expected_version":0,"excluded_user_ids":[]} {"extra":true}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	w := httptest.NewRecorder()

	_, err := decodePresenceOverrideRequest(w, req)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing")
}

type recordedPresenceOverrideUserBroadcast struct {
	userID  uuid.UUID
	message websocket.OutgoingMessage
}

type recordedPresenceOverrideDelta struct {
	senderID    uuid.UUID
	oldAudience map[uuid.UUID]bool
	newAudience map[uuid.UUID]bool
	payload     *websocket.CustomTextPayload
}

type recordedCustomTextBroadcast struct {
	senderID uuid.UUID
	oldTier  int
	payload  *websocket.CustomTextPayload
}

type recordingPresenceOverrideBroadcaster struct {
	responseWriter *httptest.ResponseRecorder
	responseCodes  []int
	userBroadcasts []recordedPresenceOverrideUserBroadcast
	deltas         []recordedPresenceOverrideDelta
	customTexts    []recordedCustomTextBroadcast
}

type blockedMetadataPresenceOverrideBroadcaster struct {
	events          chan string
	releaseMetadata chan struct{}
}

func awaitInternalValue[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for deterministic test signal")
		var zero T
		return zero
	}
}

func (b *blockedMetadataPresenceOverrideBroadcaster) BroadcastCustomText(
	uuid.UUID,
	int,
	*websocket.CustomTextPayload,
) {
}

func (b *blockedMetadataPresenceOverrideBroadcaster) BroadcastToUser(
	uuid.UUID,
	websocket.OutgoingMessage,
) {
	b.events <- "metadata"
	<-b.releaseMetadata
}

func (b *blockedMetadataPresenceOverrideBroadcaster) BroadcastCustomTextAudienceDelta(
	uuid.UUID,
	map[uuid.UUID]bool,
	map[uuid.UUID]bool,
	*websocket.CustomTextPayload,
) {
	b.events <- "privacy_delta"
}

func (b *recordingPresenceOverrideBroadcaster) BroadcastCustomText(
	senderID uuid.UUID,
	oldTier int,
	payload *websocket.CustomTextPayload,
) {
	b.customTexts = append(b.customTexts, recordedCustomTextBroadcast{
		senderID: senderID,
		oldTier:  oldTier,
		payload:  payload,
	})
}

func (b *recordingPresenceOverrideBroadcaster) BroadcastToUser(userID uuid.UUID, message websocket.OutgoingMessage) {
	b.responseCodes = append(b.responseCodes, b.responseWriter.Code)
	b.userBroadcasts = append(b.userBroadcasts, recordedPresenceOverrideUserBroadcast{userID: userID, message: message})
}

func (b *recordingPresenceOverrideBroadcaster) BroadcastCustomTextAudienceDelta(
	senderID uuid.UUID,
	oldAudience map[uuid.UUID]bool,
	newAudience map[uuid.UUID]bool,
	payload *websocket.CustomTextPayload,
) {
	b.responseCodes = append(b.responseCodes, b.responseWriter.Code)
	b.deltas = append(b.deltas, recordedPresenceOverrideDelta{
		senderID:    senderID,
		oldAudience: oldAudience,
		newAudience: newAudience,
		payload:     payload,
	})
}

func TestNewHandler_NilHubDoesNotInstallTypedNilPresenceOverrideBroadcaster(t *testing.T) {
	h := NewHandler(nil, logger.NewWithWriter(io.Discard), nil, nil, nil)

	assert.Nil(t, h.presenceOverrideBroadcaster)
}

func TestCustomStatusCoordinator_SerializesSameSenderCallbacks(t *testing.T) {
	h := &Handler{}
	senderID := uuid.MustParse("10000000-0000-0000-0000-000000000001")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		h.withCustomStatusSender(senderID, func() {
			close(firstEntered)
			<-releaseFirst
		})
	}()
	<-firstEntered
	go func() {
		defer wg.Done()
		h.withCustomStatusSender(senderID, func() {
			close(secondEntered)
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("same-sender callback entered while prior delivery was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("same-sender callback did not resume after prior delivery completed")
	}
	wg.Wait()
}

func TestCustomStatusCoordinator_DifferentSendersRemainIndependent(t *testing.T) {
	h := &Handler{}
	firstSenderID := uuid.MustParse("10000000-0000-0000-0000-000000000001")
	secondSenderID := uuid.MustParse("20000000-0000-0000-0000-000000000002")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	go h.withCustomStatusSender(firstSenderID, func() {
		close(firstEntered)
		<-releaseFirst
	})
	<-firstEntered
	go h.withCustomStatusSender(secondSenderID, func() {
		close(secondEntered)
	})

	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("different sender was blocked by unrelated Custom Status delivery")
	}
	close(releaseFirst)
}

func TestCustomStatusCoordinator_IsBoundedAndStablePerSender(t *testing.T) {
	h := &Handler{}
	senderID := uuid.New()
	samePrefixFirst := uuid.MustParse("10000000-0000-0000-0000-000000000001")
	samePrefixSecond := uuid.MustParse("10000000-0000-0000-0000-000000000002")

	first := h.customStatusLock(senderID)
	second := h.customStatusLock(senderID)

	assert.Same(t, first, second)
	assert.NotSame(t, h.customStatusLock(samePrefixFirst), h.customStatusLock(samePrefixSecond))
	assert.Equal(t, customStatusLockStripes, len(h.customStatusLocks))
}

func TestRespondPresenceOverrideWrite_SuccessSendsPreparedDeltaThenMetadata(t *testing.T) {
	senderID := uuid.New()
	removedID := uuid.New()
	addedID := uuid.New()
	w := httptest.NewRecorder()
	broadcaster := &recordingPresenceOverrideBroadcaster{responseWriter: w}
	h := &Handler{presenceOverrideBroadcaster: broadcaster, log: logger.NewWithWriter(io.Discard)}
	c, _ := gin.CreateTestContext(w)
	result := presenceOverrideWriteResult{
		Version:            4,
		ExpectedVersion:    3,
		OldAudience:        map[uuid.UUID]bool{removedID: true},
		NewAudience:        map[uuid.UUID]bool{uuid.New(): true},
		Payload:            &websocket.CustomTextPayload{Text: "prepared only"},
		DeliveryAudience:   map[uuid.UUID]bool{addedID: true},
		DeliveryPayload:    &websocket.CustomTextPayload{Text: "current"},
		ReauthorizationErr: nil,
	}

	h.respondPresenceOverrideWrite(c, senderID, presenceOverrideCategoryCustomText, result, nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"version":4}`, w.Body.String())
	require.Len(t, broadcaster.userBroadcasts, 1)
	assert.Equal(t, senderID, broadcaster.userBroadcasts[0].userID)
	assert.Equal(t, websocket.OutgoingMessage{
		Type: "presence_overrides_updated",
		Data: map[string]any{"category": "custom_text", "version": 4},
	}, broadcaster.userBroadcasts[0].message)
	require.Len(t, broadcaster.deltas, 1)
	assert.Equal(t, recordedPresenceOverrideDelta{
		senderID:    senderID,
		oldAudience: result.OldAudience,
		newAudience: result.DeliveryAudience,
		payload:     result.DeliveryPayload,
	}, broadcaster.deltas[0])
	assert.Equal(t, []int{http.StatusOK, http.StatusOK}, broadcaster.responseCodes)
}

func TestRespondPresenceOverrideWrite_SuccessWithoutCurrentStatusDoesNotClearOldAudience(t *testing.T) {
	senderID := uuid.New()
	oldViewerID := uuid.New()
	w := httptest.NewRecorder()
	broadcaster := &recordingPresenceOverrideBroadcaster{responseWriter: w}
	h := &Handler{presenceOverrideBroadcaster: broadcaster, log: logger.NewWithWriter(io.Discard)}
	c, _ := gin.CreateTestContext(w)

	h.respondPresenceOverrideWrite(c, senderID, presenceOverrideCategoryCustomText, presenceOverrideWriteResult{
		Version:            2,
		ExpectedVersion:    1,
		OldAudience:        map[uuid.UUID]bool{oldViewerID: true},
		DeliveryAudience:   map[uuid.UUID]bool{},
		DeliveryPayload:    nil,
		ReauthorizationErr: nil,
	}, nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"version":2}`, w.Body.String())
	require.Len(t, broadcaster.userBroadcasts, 1)
	require.Len(t, broadcaster.deltas, 1)
	assert.Empty(t, broadcaster.deltas[0].oldAudience)
	assert.Empty(t, broadcaster.deltas[0].newAudience)
	assert.Nil(t, broadcaster.deltas[0].payload)
}

func TestRespondPresenceOverrideWrite_SuccessKeepsOldAudienceWhenTransactionalStatusWasActive(t *testing.T) {
	senderID := uuid.New()
	oldViewerID := uuid.New()
	w := httptest.NewRecorder()
	broadcaster := &recordingPresenceOverrideBroadcaster{responseWriter: w}
	h := &Handler{presenceOverrideBroadcaster: broadcaster, log: logger.NewWithWriter(io.Discard)}
	c, _ := gin.CreateTestContext(w)
	oldAudience := map[uuid.UUID]bool{oldViewerID: true}

	h.respondPresenceOverrideWrite(c, senderID, presenceOverrideCategoryCustomText, presenceOverrideWriteResult{
		Version:            2,
		ExpectedVersion:    1,
		OldAudience:        oldAudience,
		Payload:            &websocket.CustomTextPayload{Text: "previously active"},
		DeliveryAudience:   map[uuid.UUID]bool{},
		DeliveryPayload:    nil,
		ReauthorizationErr: nil,
	}, nil)

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, broadcaster.deltas, 1)
	assert.Equal(t, oldAudience, broadcaster.deltas[0].oldAudience)
	assert.Empty(t, broadcaster.deltas[0].newAudience)
	assert.Nil(t, broadcaster.deltas[0].payload)
}

func TestRespondPresenceOverrideWrite_SuccessKeepsOldAudienceWhenReauthorizedStatusIsActive(t *testing.T) {
	senderID := uuid.New()
	oldViewerID := uuid.New()
	w := httptest.NewRecorder()
	broadcaster := &recordingPresenceOverrideBroadcaster{responseWriter: w}
	h := &Handler{presenceOverrideBroadcaster: broadcaster, log: logger.NewWithWriter(io.Discard)}
	c, _ := gin.CreateTestContext(w)
	oldAudience := map[uuid.UUID]bool{oldViewerID: true}
	deliveryPayload := &websocket.CustomTextPayload{Text: "newly active"}

	h.respondPresenceOverrideWrite(c, senderID, presenceOverrideCategoryCustomText, presenceOverrideWriteResult{
		Version:            2,
		ExpectedVersion:    1,
		OldAudience:        oldAudience,
		Payload:            nil,
		DeliveryAudience:   map[uuid.UUID]bool{},
		DeliveryPayload:    deliveryPayload,
		ReauthorizationErr: nil,
	}, nil)

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, broadcaster.deltas, 1)
	assert.Equal(t, oldAudience, broadcaster.deltas[0].oldAudience)
	assert.Empty(t, broadcaster.deltas[0].newAudience)
	assert.Equal(t, deliveryPayload, broadcaster.deltas[0].payload)
}

func TestRespondPresenceOverrideWrite_BlockedMetadataCannotDelayPrivacyDelta(t *testing.T) {
	senderID := uuid.New()
	viewerID := uuid.New()
	w := httptest.NewRecorder()
	broadcaster := &blockedMetadataPresenceOverrideBroadcaster{
		events:          make(chan string, 2),
		releaseMetadata: make(chan struct{}),
	}
	h := &Handler{presenceOverrideBroadcaster: broadcaster, log: logger.NewWithWriter(io.Discard)}
	c, _ := gin.CreateTestContext(w)
	done := make(chan struct{})

	go func() {
		defer close(done)
		h.respondPresenceOverrideWrite(c, senderID, presenceOverrideCategoryCustomText, presenceOverrideWriteResult{
			Version:          2,
			ExpectedVersion:  1,
			OldAudience:      map[uuid.UUID]bool{viewerID: true},
			Payload:          &websocket.CustomTextPayload{Text: "current"},
			DeliveryAudience: map[uuid.UUID]bool{},
			DeliveryPayload:  &websocket.CustomTextPayload{Text: "current"},
		}, nil)
	}()

	first := awaitInternalValue(t, broadcaster.events)
	if first == "metadata" {
		close(broadcaster.releaseMetadata)
		require.Equal(t, "privacy_delta", awaitInternalValue(t, broadcaster.events))
	} else {
		require.Equal(t, "privacy_delta", first)
		require.Equal(t, "metadata", awaitInternalValue(t, broadcaster.events))
		close(broadcaster.releaseMetadata)
	}
	awaitInternalValue(t, done)

	assert.Equal(t, "privacy_delta", first, "privacy revocation must run before a saturated metadata queue can block")
}

func TestRespondPresenceOverrideWrite_ReauthorizationFailureKeepsSuccessAndClearsOld(t *testing.T) {
	senderID := uuid.New()
	oldViewerID := uuid.New()
	w := httptest.NewRecorder()
	broadcaster := &recordingPresenceOverrideBroadcaster{responseWriter: w}
	h := &Handler{presenceOverrideBroadcaster: broadcaster, log: logger.NewWithWriter(io.Discard)}
	c, _ := gin.CreateTestContext(w)
	result := presenceOverrideWriteResult{
		Version:            2,
		ExpectedVersion:    1,
		OldAudience:        map[uuid.UUID]bool{oldViewerID: true},
		NewAudience:        map[uuid.UUID]bool{uuid.New(): true},
		Payload:            &websocket.CustomTextPayload{Text: "must not send"},
		ReauthorizationErr: errors.New("forced reauthorization failure"),
	}

	h.respondPresenceOverrideWrite(c, senderID, presenceOverrideCategoryCustomText, result, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"version":2}`, w.Body.String())
	require.Len(t, broadcaster.userBroadcasts, 1)
	require.Len(t, broadcaster.deltas, 1)
	assert.Equal(t, result.OldAudience, broadcaster.deltas[0].oldAudience)
	assert.Empty(t, broadcaster.deltas[0].newAudience)
	assert.Nil(t, broadcaster.deltas[0].payload)
}

func TestRespondPresenceOverrideWrite_PrecommitFailureSendsNothing(t *testing.T) {
	senderID := uuid.New()
	w := httptest.NewRecorder()
	broadcaster := &recordingPresenceOverrideBroadcaster{responseWriter: w}
	h := &Handler{presenceOverrideBroadcaster: broadcaster, log: logger.NewWithWriter(io.Discard)}
	c, _ := gin.CreateTestContext(w)

	h.respondPresenceOverrideWrite(
		c,
		senderID,
		presenceOverrideCategoryCustomText,
		presenceOverrideWriteResult{OldAudience: map[uuid.UUID]bool{uuid.New(): true}},
		presenceOverrideOperation("prepare_new_audience", errors.New("forced")),
	)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Empty(t, broadcaster.userBroadcasts)
	assert.Empty(t, broadcaster.deltas)
}

func TestRespondPresenceOverrideWrite_ConfirmedRollbackSendsNothing(t *testing.T) {
	senderID := uuid.New()
	w := httptest.NewRecorder()
	broadcaster := &recordingPresenceOverrideBroadcaster{responseWriter: w}
	h := &Handler{presenceOverrideBroadcaster: broadcaster, log: logger.NewWithWriter(io.Discard)}
	c, _ := gin.CreateTestContext(w)

	h.respondPresenceOverrideWrite(
		c,
		senderID,
		presenceOverrideCategoryCustomText,
		presenceOverrideWriteResult{OldAudience: map[uuid.UUID]bool{uuid.New(): true}},
		&presenceOverrideConfirmedRollbackError{},
	)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Empty(t, broadcaster.userBroadcasts)
	assert.Empty(t, broadcaster.deltas)
}

func TestRespondPresenceOverrideWrite_UnresolvedCommitClearsOldAudienceOnly(t *testing.T) {
	senderID := uuid.New()
	oldViewerID := uuid.New()
	w := httptest.NewRecorder()
	broadcaster := &recordingPresenceOverrideBroadcaster{responseWriter: w}
	h := &Handler{presenceOverrideBroadcaster: broadcaster, log: logger.NewWithWriter(io.Discard)}
	c, _ := gin.CreateTestContext(w)
	result := presenceOverrideWriteResult{
		OldAudience: map[uuid.UUID]bool{oldViewerID: true},
		NewAudience: map[uuid.UUID]bool{uuid.New(): true},
		Payload:     &websocket.CustomTextPayload{Text: "must not send"},
	}

	h.respondPresenceOverrideWrite(
		c,
		senderID,
		presenceOverrideCategoryCustomText,
		result,
		&presenceOverrideUnresolvedCommitError{},
	)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Empty(t, broadcaster.userBroadcasts)
	require.Len(t, broadcaster.deltas, 1)
	assert.Equal(t, senderID, broadcaster.deltas[0].senderID)
	assert.Equal(t, result.OldAudience, broadcaster.deltas[0].oldAudience)
	assert.Empty(t, broadcaster.deltas[0].newAudience)
	assert.Nil(t, broadcaster.deltas[0].payload)
}

func TestClassifyPresenceOverrideCommitResolution(t *testing.T) {
	targetA := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	targetB := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	attempted := normalizedPresenceOverrideRequest{
		EncryptedData:   "YXR0ZW1wdGVk",
		ExpectedVersion: 3,
		ExcludedUserIDs: []uuid.UUID{targetA},
	}
	attemptedWithMissingTarget := attempted
	attemptedWithMissingTarget.ExcludedUserIDs = []uuid.UUID{targetA, targetB}
	tests := []struct {
		name                  string
		attempted             normalizedPresenceOverrideRequest
		materializedTargetIDs []uuid.UUID
		newVersion            int
		persisted             persistedPresenceOverrideState
		exists                bool
		lookupErr             error
		want                  presenceOverrideCommitResolution
	}{
		{
			name: "confirmed exact commit", attempted: attempted, newVersion: 4, exists: true,
			persisted: persistedPresenceOverrideState{Version: 4, EncryptedData: "YXR0ZW1wdGVk", ExcludedUserIDs: []uuid.UUID{targetA}},
			want:      presenceOverrideCommitConfirmed,
		},
		{
			name:                  "confirmed commit with a missing opaque target",
			attempted:             attemptedWithMissingTarget,
			materializedTargetIDs: []uuid.UUID{targetA},
			newVersion:            4,
			exists:                true,
			persisted:             persistedPresenceOverrideState{Version: 4, EncryptedData: "YXR0ZW1wdGVk", ExcludedUserIDs: []uuid.UUID{targetA}},
			want:                  presenceOverrideCommitConfirmed,
		},
		{
			name:                  "filtered commit with different materialized targets is unresolved",
			attempted:             attemptedWithMissingTarget,
			materializedTargetIDs: []uuid.UUID{targetA},
			newVersion:            4,
			exists:                true,
			persisted:             persistedPresenceOverrideState{Version: 4, EncryptedData: "YXR0ZW1wdGVk", ExcludedUserIDs: []uuid.UUID{targetB}},
			want:                  presenceOverrideCommitUnresolved,
		},
		{
			name: "same version from different ciphertext is unresolved", attempted: attempted, newVersion: 4, exists: true,
			persisted: persistedPresenceOverrideState{Version: 4, EncryptedData: "b3RoZXI=", ExcludedUserIDs: []uuid.UUID{targetA}},
			want:      presenceOverrideCommitUnresolved,
		},
		{
			name: "same version from different targets is unresolved", attempted: attempted, newVersion: 4, exists: true,
			persisted: persistedPresenceOverrideState{Version: 4, EncryptedData: "YXR0ZW1wdGVk", ExcludedUserIDs: []uuid.UUID{targetB}},
			want:      presenceOverrideCommitUnresolved,
		},
		{name: "confirmed rollback existing", attempted: attempted, newVersion: 4, exists: true, persisted: persistedPresenceOverrideState{Version: 3}, want: presenceOverrideRollbackConfirmed},
		{name: "confirmed rollback absent first write", attempted: normalizedPresenceOverrideRequest{ExpectedVersion: 0}, newVersion: 1, exists: false, want: presenceOverrideRollbackConfirmed},
		{name: "unresolved absent replacement", attempted: attempted, newVersion: 4, exists: false, want: presenceOverrideCommitUnresolved},
		{name: "unresolved unexpected version", attempted: attempted, newVersion: 4, exists: true, persisted: persistedPresenceOverrideState{Version: 5}, want: presenceOverrideCommitUnresolved},
		{name: "unresolved lookup error", attempted: attempted, newVersion: 4, lookupErr: errors.New("query failed"), want: presenceOverrideCommitUnresolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			materializedTargetIDs := tt.materializedTargetIDs
			if materializedTargetIDs == nil {
				materializedTargetIDs = tt.attempted.ExcludedUserIDs
			}
			got := classifyPresenceOverrideCommit(
				tt.attempted,
				materializedTargetIDs,
				tt.newVersion,
				tt.persisted,
				tt.exists,
				tt.lookupErr,
			)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPresenceOverrideCommitResolutionContext_DetachesCancellationAndKeepsValues(t *testing.T) {
	type contextKey string
	requestCtx := context.WithValue(context.Background(), contextKey("request"), "value")
	requestCtx, cancelRequest := context.WithCancel(requestCtx)
	cancelRequest()

	resolutionCtx, cancelResolution := presenceOverrideCommitResolutionContext(requestCtx)
	defer cancelResolution()

	require.NoError(t, resolutionCtx.Err())
	assert.Equal(t, "value", resolutionCtx.Value(contextKey("request")))
	deadline, ok := resolutionCtx.Deadline()
	require.True(t, ok)
	assert.Positive(t, time.Until(deadline))
	assert.LessOrEqual(t, time.Until(deadline), presenceOverrideCommitResolutionTimeout)
}

func TestPreparePresenceOverrideDelivery_DetachesCanceledRequest(t *testing.T) {
	presenceOverrideDeliveryDriverOnce.Do(func() {
		sql.Register(presenceOverrideDeliveryDriverName, presenceOverrideDeliveryTestDriver{})
	})
	db, err := sql.Open(presenceOverrideDeliveryDriverName, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	requestCtx := context.WithValue(
		context.Background(), presenceOverrideDeliveryRequestValueKey, "request-value",
	)
	requestCtx, cancelRequest := context.WithCancel(requestCtx)
	cancelRequest()
	h := &Handler{db: db}

	audience, payload, err := h.preparePresenceOverrideDelivery(requestCtx, uuid.New())

	require.NoError(t, err)
	assert.Empty(t, audience)
	assert.Nil(t, payload)
}

type presenceOverrideDeliveryContextKey string

const (
	presenceOverrideDeliveryDriverName      = "presence-override-delivery-test"
	presenceOverrideDeliveryRequestValueKey = presenceOverrideDeliveryContextKey("request")
	presenceOverrideDeliveryMaxTestTimeout  = 3 * time.Second
)

var presenceOverrideDeliveryDriverOnce sync.Once

type presenceOverrideDeliveryTestDriver struct{}

func (presenceOverrideDeliveryTestDriver) Open(string) (driver.Conn, error) {
	return &presenceOverrideDeliveryTestConn{}, nil
}

type presenceOverrideDeliveryTestConn struct{}

func (c *presenceOverrideDeliveryTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *presenceOverrideDeliveryTestConn) Close() error {
	return nil
}

func (c *presenceOverrideDeliveryTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *presenceOverrideDeliveryTestConn) QueryContext(
	ctx context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	if ctx.Value(presenceOverrideDeliveryRequestValueKey) != "request-value" {
		return nil, errors.New("request context value was not preserved")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("delivery context is not bounded")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > presenceOverrideDeliveryMaxTestTimeout {
		return nil, errors.New("delivery context deadline is outside the allowed bound")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	switch {
	case strings.Contains(query, "SELECT custom_text_tier, custom_text, custom_text_emoji"):
		return &presenceOverrideDeliveryTestRows{
			columns: []string{"custom_text_tier", "custom_text", "custom_text_emoji"},
			values:  []driver.Value{int64(0), nil, nil},
		}, nil
	case strings.Contains(query, "SELECT custom_text_tier FROM user_presence_settings"):
		return &presenceOverrideDeliveryTestRows{
			columns: []string{"custom_text_tier"},
			values:  []driver.Value{int64(0)},
		}, nil
	case strings.Contains(query, "SELECT target_user_id"):
		return &presenceOverrideDeliveryTestRows{columns: []string{"target_user_id"}}, nil
	default:
		return nil, errors.New("unexpected delivery query")
	}
}

type presenceOverrideDeliveryTestRows struct {
	columns   []string
	values    []driver.Value
	delivered bool
}

func (r *presenceOverrideDeliveryTestRows) Columns() []string {
	return r.columns
}

func (r *presenceOverrideDeliveryTestRows) Close() error {
	return nil
}

func (r *presenceOverrideDeliveryTestRows) Next(values []driver.Value) error {
	if r.delivered || len(r.values) == 0 {
		return io.EOF
	}
	r.delivered = true
	copy(values, r.values)
	return nil
}

func TestReadPersistedPresenceOverrideState(t *testing.T) {
	presenceOverrideVersionDriverOnce.Do(func() {
		sql.Register(presenceOverrideVersionDriverName, presenceOverrideVersionTestDriver{})
	})
	persistedTarget := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	tests := []struct {
		name       string
		scenario   string
		wantState  persistedPresenceOverrideState
		wantExists bool
		wantErr    bool
	}{
		{name: "absent", scenario: "absent"},
		{
			name: "present", scenario: "present", wantExists: true,
			wantState: persistedPresenceOverrideState{
				Version: 7, EncryptedData: "Y2lwaGVydGV4dA==", ExcludedUserIDs: []uuid.UUID{persistedTarget},
			},
		},
		{name: "invalid target", scenario: "invalid-target", wantErr: true},
		{name: "query error", scenario: "error", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := sql.Open(presenceOverrideVersionDriverName, tt.scenario)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			h := &Handler{db: db}

			state, exists, err := h.readPersistedPresenceOverrideState(
				context.Background(), uuid.New(), presenceOverrideCategoryCustomText,
			)

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantState, state)
			assert.Equal(t, tt.wantExists, exists)
		})
	}
}

func TestPresenceOverrideErrors_ExposeStableMetadataAndPreserveCauses(t *testing.T) {
	cause := errors.New("sensitive database detail")
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "version conflict", err: &presenceOverrideVersionConflictError{CurrentVersion: 4}, want: "presence override version conflict"},
		{name: "operation", err: &presenceOverrideOperationError{Operation: "query", cause: cause}, want: "presence override query failed"},
		{name: "commit", err: &presenceOverrideCommitError{cause: cause}, want: "presence override commit failed"},
		{name: "confirmed rollback", err: &presenceOverrideConfirmedRollbackError{}, want: "presence override commit confirmed rolled back"},
		{name: "unresolved commit", err: &presenceOverrideUnresolvedCommitError{}, want: "presence override commit state unresolved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.err.Error())
			assert.NotContains(t, tt.err.Error(), cause.Error())
		})
	}
	assert.ErrorIs(t, tests[1].err, cause)
	assert.ErrorIs(t, tests[2].err, cause)
}

const presenceOverrideVersionDriverName = "presence-override-version-test"

var presenceOverrideVersionDriverOnce sync.Once

type presenceOverrideVersionTestDriver struct{}

func (presenceOverrideVersionTestDriver) Open(scenario string) (driver.Conn, error) {
	return &presenceOverrideVersionTestConn{scenario: scenario}, nil
}

type presenceOverrideVersionTestConn struct {
	scenario string
}

func (c *presenceOverrideVersionTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *presenceOverrideVersionTestConn) Close() error {
	return nil
}

func (c *presenceOverrideVersionTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *presenceOverrideVersionTestConn) QueryContext(
	context.Context,
	string,
	[]driver.NamedValue,
) (driver.Rows, error) {
	if c.scenario == "error" {
		return nil, errors.New("forced query failure")
	}
	return &presenceOverrideVersionTestRows{
		present:       c.scenario == "present" || c.scenario == "invalid-target",
		invalidTarget: c.scenario == "invalid-target",
	}, nil
}

type presenceOverrideVersionTestRows struct {
	present       bool
	invalidTarget bool
	delivered     bool
}

func (r *presenceOverrideVersionTestRows) Columns() []string {
	return []string{"version", "encrypted_data", "excluded_user_ids"}
}

func (r *presenceOverrideVersionTestRows) Close() error {
	return nil
}

func (r *presenceOverrideVersionTestRows) Next(values []driver.Value) error {
	if !r.present || r.delivered {
		return io.EOF
	}
	r.delivered = true
	values[0] = int64(7)
	values[1] = "Y2lwaGVydGV4dA=="
	if r.invalidTarget {
		values[2] = "{not-a-uuid}"
	} else {
		values[2] = "{11111111-1111-4111-8111-111111111111}"
	}
	return nil
}

func TestReplacePresenceOverrides_BeginFailureSendsNothing(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://unused")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	w := httptest.NewRecorder()
	broadcaster := &recordingPresenceOverrideBroadcaster{responseWriter: w}
	h := &Handler{
		db:                          db,
		log:                         logger.NewWithWriter(io.Discard),
		presenceOverrideBroadcaster: broadcaster,
	}
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", uuid.NewString())
	c.Params = gin.Params{{Key: "category", Value: presenceOverrideCategoryCustomText}}
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/users/me/presence-overrides/custom_text",
		strings.NewReader(`{"encrypted_data":"YQ==","expected_version":0,"excluded_user_ids":[]}`),
	)

	h.ReplacePresenceOverrides(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Empty(t, broadcaster.userBroadcasts)
	assert.Empty(t, broadcaster.deltas)
}
