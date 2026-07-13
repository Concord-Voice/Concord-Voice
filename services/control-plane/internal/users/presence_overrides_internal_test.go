package users

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/presencehistory"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func excludedIDs(ids ...string) *[]string { return &ids }

func validPresenceOverrideRequest(ids ...string) presenceOverrideRequest {
	return presenceOverrideRequest{
		EncryptedData:   "Y2lwaGVydGV4dA==",
		ExpectedVersion: 0,
		ExcludedUserIDs: excludedIDs(ids...),
	}
}

func TestValidatePresenceOverrideRequest_Category(t *testing.T) {
	_, err := validatePresenceOverrideRequest("activity", uuid.New(), validPresenceOverrideRequest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom_text")
}

func TestValidatePresenceOverrideRequest_EncryptedData(t *testing.T) {
	senderID := uuid.New()
	tests := []struct {
		name      string
		value     string
		wantError bool
	}{
		{name: "empty", value: "", wantError: true},
		{name: "invalid base64", value: "not-base64!", wantError: true},
		{name: "exact cap", value: strings.Repeat("A", presenceOverrideMaxEncryptedDataBytes)},
		{name: "over cap", value: strings.Repeat("A", presenceOverrideMaxEncryptedDataBytes+4), wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := validPresenceOverrideRequest()
			req.EncryptedData = test.value
			_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidatePresenceOverrideRequest_ExpectedVersion(t *testing.T) {
	req := validPresenceOverrideRequest()
	req.ExpectedVersion = -1
	_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, uuid.New(), req)
	require.Error(t, err)
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
	for _, req := range []presenceOverrideRequest{
		validPresenceOverrideRequest("not-a-uuid"),
		validPresenceOverrideRequest(senderID.String()),
	} {
		_, err := validatePresenceOverrideRequest(presenceOverrideCategoryCustomText, senderID, req)
		require.Error(t, err)
	}
}

func TestValidatePresenceOverrideRequest_CanonicalizesBeforeCap(t *testing.T) {
	senderID := uuid.New()
	first := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	normalized, err := validatePresenceOverrideRequest(
		presenceOverrideCategoryCustomText,
		senderID,
		validPresenceOverrideRequest(first.String(), second.String(), first.String()),
	)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{second, first}, normalized.ExcludedUserIDs)

	duplicates := make([]string, presenceOverrideMaxTargets+1)
	for index := range duplicates {
		duplicates[index] = first.String()
	}
	normalized, err = validatePresenceOverrideRequest(
		presenceOverrideCategoryCustomText, senderID, validPresenceOverrideRequest(duplicates...),
	)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{first}, normalized.ExcludedUserIDs)
}

func TestValidatePresenceOverrideRequest_RejectsMoreThanTargetCap(t *testing.T) {
	ids := make([]string, 0, presenceOverrideMaxTargets+1)
	for index := 0; index <= presenceOverrideMaxTargets; index++ {
		ids = append(ids, uuid.NewString())
	}
	_, err := validatePresenceOverrideRequest(
		presenceOverrideCategoryCustomText, uuid.New(), validPresenceOverrideRequest(ids...),
	)
	require.Error(t, err)
}

func TestDecodePresenceOverrideRequest_BoundsBody(t *testing.T) {
	body := `{"encrypted_data":"` + strings.Repeat("A", presenceOverrideMaxRequestBodyBytes) +
		`","expected_version":0,"excluded_user_ids":[]}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	_, err := decodePresenceOverrideRequest(httptest.NewRecorder(), req)
	var maxBytesErr *http.MaxBytesError
	require.Error(t, err)
	assert.True(t, errors.As(err, &maxBytesErr))
}

func TestDecodePresenceOverrideRequest_RejectsTrailingJSON(t *testing.T) {
	body := `{"encrypted_data":"YQ==","expected_version":0,"excluded_user_ids":[]} {"extra":true}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	_, err := decodePresenceOverrideRequest(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing")
}

func TestPresenceOverrideErrors_ExposeStableMetadataAndPreserveCauses(t *testing.T) {
	cause := errors.New("sensitive database detail")
	operationErr := &presenceOverrideOperationError{Operation: "query", cause: cause}
	conflictErr := &presenceOverrideVersionConflictError{CurrentVersion: 4}
	assert.Equal(t, "presence override query failed", operationErr.Error())
	assert.Equal(t, "presence override version conflict", conflictErr.Error())
	assert.NotContains(t, operationErr.Error(), cause.Error())
	assert.ErrorIs(t, operationErr, cause)
	writerErr := &presenceWriterFailure{class: "commit", cause: cause}
	assert.Equal(t, "presence writer commit failed", writerErr.Error())
	assert.ErrorIs(t, writerErr, cause)
}

func TestCommitAndClaimPresenceWriterPreservesInitialRollbackFailure(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	senderID := testdb.CreateUser(t, db)
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	tx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	operation, err := service.BeginAudienceOperation(
		context.Background(), tx, senderID, presencehistory.OrdinaryAudienceWrite,
	)
	require.NoError(t, err)

	commitCause := errors.New("commit rejected")
	rollbackCause := errors.New("rollback transport failure")
	restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Commit: func(*sql.Tx) error { return commitCause },
		Rollback: func(tx *sql.Tx) error {
			require.NoError(t, tx.Rollback())
			return rollbackCause
		},
	})
	defer restore()
	handler := &Handler{presenceHistory: service}
	err = handler.commitAndClaimPresenceWriter(context.Background(), tx, operation, presencehistory.DeliveryPlan{
		Mode: presencehistory.DeliveryExactDelta, OperationID: operation.ID, SenderID: senderID,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, commitCause)
	assert.ErrorIs(t, err, rollbackCause)
}

type failingClaimDelivery struct {
	cause error
	plans []presencehistory.DeliveryPlan
}

func (d *failingClaimDelivery) DeliverCustomText(
	_ context.Context,
	plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	d.plans = append(d.plans, plan)
	if plan.Mode == presencehistory.DeliveryExactDelta {
		return presencehistory.DeliveryAck{}, d.cause
	}
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

func TestCommitAndClaimPresenceWriterJoinsConfirmedCommitAndClaimFailures(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	senderID := testdb.CreateUser(t, db)
	claimCause := errors.New("claim delivery failed")
	delivery := &failingClaimDelivery{cause: claimCause}
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	operation, err := service.BeginAudienceOperation(
		context.Background(), tx, senderID, presencehistory.OrdinaryAudienceWrite,
	)
	require.NoError(t, err)

	commitCause := errors.New("ambiguous main commit")
	rollbackCause := errors.New("main rollback transport failure")
	rollbackCalls := 0
	restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Commit: func(tx *sql.Tx) error {
			require.NoError(t, tx.Commit())
			return commitCause
		},
		Rollback: func(tx *sql.Tx) error {
			rollbackCalls++
			if rollbackCalls == 1 {
				return rollbackCause
			}
			return tx.Rollback()
		},
	})
	defer restore()
	handler := &Handler{presenceHistory: service}
	err = handler.commitAndClaimPresenceWriter(context.Background(), tx, operation, presencehistory.DeliveryPlan{
		Mode: presencehistory.DeliveryExactDelta, OperationID: operation.ID, SenderID: senderID,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, commitCause)
	assert.ErrorIs(t, err, rollbackCause)
	assert.ErrorIs(t, err, claimCause)
	require.Len(t, delivery.plans, 2, "delivery failure already performs exactly one conservative recovery")
	assert.Equal(t, presencehistory.DeliveryExactDelta, delivery.plans[0].Mode)
	assert.Equal(t, presencehistory.DeliveryConservativeReset, delivery.plans[1].Mode)
}

func TestUpsertPresenceOverridePreferenceConflictAndQueryError(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	senderID := testdb.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO presence_override_preferences (user_id, category, encrypted_data, version)
		VALUES ($1, 'custom_text', 'b2xk', 4)
	`, senderID)
	require.NoError(t, err)

	t.Run("conflict", func(t *testing.T) {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = upsertPresenceOverridePreference(
			context.Background(), tx, senderID, presenceOverrideCategoryCustomText, "bmV3", 3,
		)
		var conflict *presenceOverrideVersionConflictError
		require.ErrorAs(t, err, &conflict)
		assert.Equal(t, 4, conflict.CurrentVersion)
	})

	t.Run("query error", func(t *testing.T) {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = upsertPresenceOverridePreference(
			ctx, tx, senderID, presenceOverrideCategoryCustomText, "bmV3", 4,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "upsert_preference")
	})
}
