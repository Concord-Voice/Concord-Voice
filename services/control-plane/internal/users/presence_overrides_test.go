package users_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/presencehistory"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const customTextOverrideCategory = "custom_text"

const presenceOverrideURL = "/api/v1/users/me/presence-overrides/custom_text"

type presenceOverridePUTBody struct {
	EncryptedData   string   `json:"encrypted_data"`
	ExpectedVersion int      `json:"expected_version"`
	ExcludedUserIDs []string `json:"excluded_user_ids"`
}

type immediatePresenceDelivery struct{}

func (immediatePresenceDelivery) DeliverCustomText(
	_ context.Context,
	plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

func directPresenceOverrideHandler(db *sql.DB) *users.Handler {
	handler := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	if err := service.BindDelivery(immediatePresenceDelivery{}); err != nil {
		panic(err)
	}
	handler.SetPresenceHistory(service)
	return handler
}

func invokePresenceOverridePUT(
	t *testing.T,
	h *users.Handler,
	senderID uuid.UUID,
	body presenceOverridePUTBody,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", senderID.String())
	c.Params = gin.Params{{Key: "category", Value: customTextOverrideCategory}}
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/users/me/presence-overrides/custom_text",
		bytes.NewReader(encoded),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	h.ReplacePresenceOverrides(c)
	return w
}

func seedOverridePreference(
	t *testing.T,
	db *sql.DB,
	senderID uuid.UUID,
	ciphertext string,
	version int,
	targets ...uuid.UUID,
) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO presence_override_preferences (user_id, category, encrypted_data, version)
		 VALUES ($1, 'custom_text', $2, $3)`, senderID, ciphertext, version,
	)
	require.NoError(t, err)
	for _, targetID := range targets {
		_, err = db.Exec(
			`INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
			 VALUES ($1, 'custom_text', $2)`, senderID, targetID,
		)
		require.NoError(t, err)
	}
}

func readOverridePreference(t *testing.T, db *sql.DB, senderID uuid.UUID) (string, int, []uuid.UUID) {
	t.Helper()
	var ciphertext string
	var version int
	require.NoError(t, db.QueryRow(
		`SELECT encrypted_data, version FROM presence_override_preferences
		 WHERE user_id = $1 AND category = 'custom_text'`, senderID,
	).Scan(&ciphertext, &version))
	rows, err := db.Query(
		`SELECT target_user_id FROM user_presence_overrides
		 WHERE sender_id = $1 AND category = 'custom_text' ORDER BY target_user_id`, senderID,
	)
	require.NoError(t, err)
	var targets []uuid.UUID
	for rows.Next() {
		var targetID uuid.UUID
		require.NoError(t, rows.Scan(&targetID))
		targets = append(targets, targetID)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	return ciphertext, version, targets
}

func TestReplacePresenceOverrides_AtomicWrites(t *testing.T) {
	t.Run("first write", func(t *testing.T) {
		db, _ := testhelpers.SetupTestDB(t)
		senderID := testhelpers.CreateUser(t, db)
		targetID := testhelpers.CreateUser(t, db)
		testhelpers.AddFriendship(t, db, senderID, targetID)
		_, err := db.Exec(
			`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
			 VALUES ($1, 1, 'focused')`, senderID,
		)
		require.NoError(t, err)

		w := invokePresenceOverridePUT(t, directPresenceOverrideHandler(db), senderID, presenceOverridePUTBody{
			EncryptedData: "Zmlyc3Q=", ExcludedUserIDs: []string{targetID.String()},
		})

		require.Equal(t, http.StatusOK, w.Code)
		ciphertext, version, targets := readOverridePreference(t, db, senderID)
		assert.Equal(t, "Zmlyc3Q=", ciphertext)
		assert.Equal(t, 1, version)
		assert.Equal(t, []uuid.UUID{targetID}, targets)
	})

	t.Run("replace", func(t *testing.T) {
		db, _ := testhelpers.SetupTestDB(t)
		senderID := testhelpers.CreateUser(t, db)
		oldTargetID := testhelpers.CreateUser(t, db)
		newTargetID := testhelpers.CreateUser(t, db)
		seedOverridePreference(t, db, senderID, "b2xk", 1, oldTargetID)

		w := invokePresenceOverridePUT(t, directPresenceOverrideHandler(db), senderID, presenceOverridePUTBody{
			EncryptedData: "bmV3", ExpectedVersion: 1, ExcludedUserIDs: []string{newTargetID.String()},
		})

		require.Equal(t, http.StatusOK, w.Code)
		ciphertext, version, targets := readOverridePreference(t, db, senderID)
		assert.Equal(t, "bmV3", ciphertext)
		assert.Equal(t, 2, version)
		assert.Equal(t, []uuid.UUID{newTargetID}, targets)
	})

	t.Run("clear", func(t *testing.T) {
		db, _ := testhelpers.SetupTestDB(t)
		senderID := testhelpers.CreateUser(t, db)
		targetID := testhelpers.CreateUser(t, db)
		seedOverridePreference(t, db, senderID, "b2xk", 1, targetID)

		w := invokePresenceOverridePUT(t, directPresenceOverrideHandler(db), senderID, presenceOverridePUTBody{
			EncryptedData: "Y2xlYXI=", ExpectedVersion: 1, ExcludedUserIDs: []string{},
		})

		require.Equal(t, http.StatusOK, w.Code)
		ciphertext, version, targets := readOverridePreference(t, db, senderID)
		assert.Equal(t, "Y2xlYXI=", ciphertext)
		assert.Equal(t, 2, version)
		assert.Empty(t, targets)
	})
}

func TestReplacePresenceOverrides_StaleVersionDoesNotWrite(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	oldTargetID := testhelpers.CreateUser(t, db)
	newTargetID := testhelpers.CreateUser(t, db)
	seedOverridePreference(t, db, senderID, "b2xk", 4, oldTargetID)

	w := invokePresenceOverridePUT(t, directPresenceOverrideHandler(db), senderID, presenceOverridePUTBody{
		EncryptedData: "bmV3", ExpectedVersion: 3, ExcludedUserIDs: []string{newTargetID.String()},
	})

	require.Equal(t, http.StatusConflict, w.Code)
	ciphertext, version, targets := readOverridePreference(t, db, senderID)
	assert.Equal(t, "b2xk", ciphertext)
	assert.Equal(t, 4, version)
	assert.Equal(t, []uuid.UUID{oldTargetID}, targets)
}

func TestReplacePresenceOverrides_ConcurrentFirstWritesUseCAS(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	h := directPresenceOverrideHandler(db)
	bodies := []presenceOverridePUTBody{
		{EncryptedData: "YQ==", ExcludedUserIDs: []string{}},
		{EncryptedData: "Yg==", ExcludedUserIDs: []string{}},
	}
	responses := make([]*httptest.ResponseRecorder, len(bodies))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			responses[i] = invokePresenceOverridePUT(t, h, senderID, bodies[i])
		}(i)
	}
	close(start)
	wg.Wait()

	codes := []int{responses[0].Code, responses[1].Code}
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, codes)
	_, version, targets := readOverridePreference(t, db, senderID)
	assert.Equal(t, 1, version)
	assert.Empty(t, targets)
}

func TestReplacePresenceOverrides_PreservesOpaquePreferenceAndSkipsDeletedTargets(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	existingNonfriendID := testhelpers.CreateUser(t, db)
	deletedID := testhelpers.CreateUser(t, db)
	seedOverridePreference(t, db, senderID, "b2xk", 1, deletedID)
	_, err := db.Exec(`DELETE FROM users WHERE id = $1`, deletedID)
	require.NoError(t, err)

	w := invokePresenceOverridePUT(t, directPresenceOverrideHandler(db), senderID, presenceOverridePUTBody{
		EncryptedData:   "YQ==",
		ExpectedVersion: 1,
		ExcludedUserIDs: []string{existingNonfriendID.String(), deletedID.String()},
	})

	require.Equal(t, http.StatusOK, w.Code)
	ciphertext, version, targets := readOverridePreference(t, db, senderID)
	assert.Equal(t, "YQ==", ciphertext)
	assert.Equal(t, 2, version)
	assert.Equal(t, []uuid.UUID{existingNonfriendID}, targets)
	assert.NotContains(t, w.Body.String(), existingNonfriendID.String())
	assert.NotContains(t, w.Body.String(), deletedID.String())
}

func TestReplacePresenceOverrides_MissingTargetHasSameSuccessResponseShape(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	existingSenderID := testhelpers.CreateUser(t, db)
	missingSenderID := testhelpers.CreateUser(t, db)
	existingTargetID := testhelpers.CreateUser(t, db)
	missingTargetID := uuid.New()

	existingResponse := invokePresenceOverridePUT(
		t,
		directPresenceOverrideHandler(db),
		existingSenderID,
		presenceOverridePUTBody{
			EncryptedData:   "YQ==",
			ExcludedUserIDs: []string{existingTargetID.String()},
		},
	)
	missingResponse := invokePresenceOverridePUT(
		t,
		directPresenceOverrideHandler(db),
		missingSenderID,
		presenceOverridePUTBody{
			EncryptedData:   "Yg==",
			ExcludedUserIDs: []string{missingTargetID.String()},
		},
	)

	require.Equal(t, http.StatusOK, existingResponse.Code)
	require.Equal(t, http.StatusOK, missingResponse.Code)
	assert.JSONEq(t, existingResponse.Body.String(), missingResponse.Body.String())
	ciphertext, version, targets := readOverridePreference(t, db, missingSenderID)
	assert.Equal(t, "Yg==", ciphertext)
	assert.Equal(t, 1, version)
	assert.Empty(t, targets)
}

func TestReplacePresenceOverrides_InsertFailureRollsBack(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	oldTargetID := testhelpers.CreateUser(t, db)
	newTargetID := testhelpers.CreateUser(t, db)
	seedOverridePreference(t, db, senderID, "b2xk", 1, oldTargetID)
	_, err := db.Exec(`
		CREATE FUNCTION test_fail_presence_override_insert_1234()
		RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'forced'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_presence_override_insert_1234
		BEFORE INSERT ON user_presence_overrides
		FOR EACH ROW EXECUTE FUNCTION test_fail_presence_override_insert_1234()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, cleanupErr := db.Exec(`
			DROP TRIGGER IF EXISTS test_fail_presence_override_insert_1234 ON user_presence_overrides;
			DROP FUNCTION IF EXISTS test_fail_presence_override_insert_1234()`); cleanupErr != nil {
			t.Errorf("cleanup insert failure trigger: %v", cleanupErr)
		}
	})

	w := invokePresenceOverridePUT(t, directPresenceOverrideHandler(db), senderID, presenceOverridePUTBody{
		EncryptedData: "bmV3", ExpectedVersion: 1, ExcludedUserIDs: []string{newTargetID.String()},
	})

	require.Equal(t, http.StatusInternalServerError, w.Code)
	ciphertext, version, targets := readOverridePreference(t, db, senderID)
	assert.Equal(t, "b2xk", ciphertext)
	assert.Equal(t, 1, version)
	assert.Equal(t, []uuid.UUID{oldTargetID}, targets)
}

func TestReplacePresenceOverrides_OldAudienceFailureDoesNotWrite(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	oldTargetID := testhelpers.CreateUser(t, db)
	newTargetID := testhelpers.CreateUser(t, db)
	seedOverridePreference(t, db, senderID, "b2xk", 1, oldTargetID)
	_, err := db.Exec(`ALTER TABLE user_presence_settings RENAME TO test_presence_settings_hidden_1234`)
	require.NoError(t, err)
	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		if _, cleanupErr := db.Exec(
			`ALTER TABLE test_presence_settings_hidden_1234 RENAME TO user_presence_settings`,
		); cleanupErr != nil {
			t.Errorf("restore presence settings table: %v", cleanupErr)
		}
	})

	w := invokePresenceOverridePUT(t, directPresenceOverrideHandler(db), senderID, presenceOverridePUTBody{
		EncryptedData: "bmV3", ExpectedVersion: 1, ExcludedUserIDs: []string{newTargetID.String()},
	})
	_, err = db.Exec(`ALTER TABLE test_presence_settings_hidden_1234 RENAME TO user_presence_settings`)
	require.NoError(t, err)
	restored = true

	require.Equal(t, http.StatusInternalServerError, w.Code)
	ciphertext, version, targets := readOverridePreference(t, db, senderID)
	assert.Equal(t, "b2xk", ciphertext)
	assert.Equal(t, 1, version)
	assert.Equal(t, []uuid.UUID{oldTargetID}, targets)
}

func TestReplacePresenceOverrides_PreparationFailureRollsBack(t *testing.T) {
	tests := []struct {
		name       string
		triggerSQL string
		cleanupSQL string
	}{
		{
			name: "new audience",
			triggerSQL: `
				CREATE FUNCTION test_hide_friendships_1234() RETURNS trigger AS $$
				BEGIN EXECUTE 'ALTER TABLE friendships RENAME TO test_friendships_hidden_1234'; RETURN NEW; END;
				$$ LANGUAGE plpgsql;
				CREATE TRIGGER test_hide_friendships_1234 AFTER UPDATE ON presence_override_preferences
				FOR EACH ROW EXECUTE FUNCTION test_hide_friendships_1234()`,
			cleanupSQL: `DO $$ BEGIN
				IF to_regclass('public.test_friendships_hidden_1234') IS NOT NULL THEN
					ALTER TABLE test_friendships_hidden_1234 RENAME TO friendships;
				END IF;
			END $$;
			DROP TRIGGER IF EXISTS test_hide_friendships_1234 ON presence_override_preferences;
			DROP FUNCTION IF EXISTS test_hide_friendships_1234()`,
		},
		{
			name: "payload",
			triggerSQL: `
				CREATE FUNCTION test_hide_custom_text_1234() RETURNS trigger AS $$
				BEGIN EXECUTE 'ALTER TABLE user_presence_settings RENAME COLUMN custom_text TO test_custom_text_hidden_1234'; RETURN NEW; END;
				$$ LANGUAGE plpgsql;
				CREATE TRIGGER test_hide_custom_text_1234 AFTER UPDATE ON presence_override_preferences
				FOR EACH ROW EXECUTE FUNCTION test_hide_custom_text_1234()`,
			cleanupSQL: `DO $$ BEGIN
				IF EXISTS (
					SELECT 1 FROM information_schema.columns
					WHERE table_schema = 'public' AND table_name = 'user_presence_settings'
					  AND column_name = 'test_custom_text_hidden_1234'
				) THEN
					ALTER TABLE user_presence_settings
						RENAME COLUMN test_custom_text_hidden_1234 TO custom_text;
				END IF;
			END $$;
			DROP TRIGGER IF EXISTS test_hide_custom_text_1234 ON presence_override_preferences;
			DROP FUNCTION IF EXISTS test_hide_custom_text_1234()`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _ := testhelpers.SetupTestDB(t)
			senderID := testhelpers.CreateUser(t, db)
			oldTargetID := testhelpers.CreateUser(t, db)
			newTargetID := testhelpers.CreateUser(t, db)
			seedOverridePreference(t, db, senderID, "b2xk", 1, oldTargetID)
			_, err := db.Exec(
				`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
				 VALUES ($1, 1, 'focused')`, senderID,
			)
			require.NoError(t, err)
			_, err = db.Exec(tt.triggerSQL)
			require.NoError(t, err)
			t.Cleanup(func() {
				if _, cleanupErr := db.Exec(tt.cleanupSQL); cleanupErr != nil {
					t.Errorf("cleanup preparation trigger: %v", cleanupErr)
				}
			})

			w := invokePresenceOverridePUT(t, directPresenceOverrideHandler(db), senderID, presenceOverridePUTBody{
				EncryptedData: "bmV3", ExpectedVersion: 1, ExcludedUserIDs: []string{newTargetID.String()},
			})

			require.Equal(t, http.StatusInternalServerError, w.Code)
			ciphertext, version, targets := readOverridePreference(t, db, senderID)
			assert.Equal(t, "b2xk", ciphertext)
			assert.Equal(t, 1, version)
			assert.Equal(t, []uuid.UUID{oldTargetID}, targets)
		})
	}
}

func TestPresenceOverrideRoutes_RequireAuthentication(t *testing.T) {
	ts := setupTS(t)

	getResponse := ts.DoRequest(http.MethodGet, presenceOverrideURL, nil, nil)
	putResponse := ts.DoRequest(http.MethodPut, presenceOverrideURL, presenceOverridePUTBody{
		EncryptedData: "YQ==", ExcludedUserIDs: []string{},
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, getResponse.Code)
	assert.Equal(t, http.StatusUnauthorized, putResponse.Code)
}

func TestGetPresenceOverrides_NullAndPersisted(t *testing.T) {
	t.Run("null preference", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "overridegetnull")

		w := ts.DoRequest(http.MethodGet, presenceOverrideURL, nil, testhelpers.AuthHeaders(user.AccessToken))

		require.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t, `{"preference":null}`, w.Body.String())
		assert.Equal(t, "30", w.Header().Get("X-RateLimit-Limit"))
	})

	t.Run("persisted preference omits materialized targets", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "overridegetrow")
		target := ts.CreateTestUser(t, "overridegettarget")
		senderID := uuid.MustParse(user.ID)
		seedOverridePreference(t, ts.DB, senderID, "c2VhbGVk", 3, uuid.MustParse(target.ID))

		w := ts.DoRequest(http.MethodGet, presenceOverrideURL, nil, testhelpers.AuthHeaders(user.AccessToken))

		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]any
		testhelpers.ParseJSON(t, w, &body)
		preference := body["preference"].(map[string]any)
		assert.Equal(t, "c2VhbGVk", preference["encrypted_data"])
		assert.Equal(t, float64(3), preference["version"])
		assert.NotEmpty(t, preference["updated_at"])
		assert.NotContains(t, preference, "excluded_user_ids")
		assert.NotContains(t, w.Body.String(), target.ID)
	})
}

func TestPutPresenceOverrides_RouteResponseAndConflictBodies(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "overrideputroute")
	headers := testhelpers.AuthHeaders(user.AccessToken)

	w := ts.DoRequest(http.MethodPut, presenceOverrideURL, presenceOverridePUTBody{
		EncryptedData: "Zmlyc3Q=", ExcludedUserIDs: []string{},
	}, headers)

	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"version":1}`, w.Body.String())
	assert.Equal(t, "10", w.Header().Get("X-RateLimit-Limit"))

	w = ts.DoRequest(http.MethodPut, presenceOverrideURL, presenceOverridePUTBody{
		EncryptedData: "c3RhbGU=", ExpectedVersion: 0, ExcludedUserIDs: []string{},
	}, headers)

	require.Equal(t, http.StatusConflict, w.Code)
	assert.JSONEq(t,
		`{"code":"presence_override_version_conflict","current_version":1}`,
		w.Body.String(),
	)
}

func TestPutPresenceOverrides_ValidationAndBodyLimit(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "overridevalidation")
	headers := testhelpers.AuthHeaders(user.AccessToken)

	w := ts.DoRequest(http.MethodPut,
		"/api/v1/users/me/presence-overrides/activity",
		presenceOverridePUTBody{EncryptedData: "YQ==", ExcludedUserIDs: []string{}},
		headers,
	)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = ts.DoRequest(http.MethodPut, presenceOverrideURL, map[string]any{
		"encrypted_data": "YQ==", "expected_version": 0,
	}, headers)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	largeBody := `{"encrypted_data":"` + strings.Repeat("A", 128*1024) +
		`","expected_version":0,"excluded_user_ids":[]}`
	req := httptest.NewRequest(http.MethodPut, presenceOverrideURL, strings.NewReader(largeBody))
	req.Header = headers.Clone()
	w = httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestPutPresenceOverrides_LogsOnlyStableMetadataOnDatabaseFailure(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "overridelogs")
	target := ts.CreateTestUser(t, "overridelogtarget")
	_, err := ts.DB.Exec(`
		CREATE FUNCTION test_fail_presence_override_log_1234()
		RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'sensitive-constraint-detail'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_presence_override_log_1234
		BEFORE INSERT ON user_presence_overrides
		FOR EACH ROW EXECUTE FUNCTION test_fail_presence_override_log_1234()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_fail_presence_override_log_1234 ON user_presence_overrides;
			DROP FUNCTION IF EXISTS test_fail_presence_override_log_1234()`); cleanupErr != nil {
			t.Errorf("cleanup log failure trigger: %v", cleanupErr)
		}
	})
	logs := ts.CaptureLogs(t)
	ciphertext := "cHJpdmF0ZS1jaXBoZXJ0ZXh0"

	w := ts.DoRequest(http.MethodPut, presenceOverrideURL, presenceOverridePUTBody{
		EncryptedData: ciphertext, ExcludedUserIDs: []string{target.ID},
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusInternalServerError, w.Code)
	logOutput := logs.String()
	assert.NotContains(t, logOutput, ciphertext)
	assert.NotContains(t, logOutput, target.ID)
	assert.NotContains(t, logOutput, "sensitive-constraint-detail")
	assert.Contains(t, logOutput, "error_class=insert_targets")
}
