package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers/testdb"
)

func TestAdminCtlParseRequiresExactVerbAndConfirmDrained(t *testing.T) {
	tests := []struct {
		name string
		args []string
		verb adminCtlVerb
		ok   bool
	}{
		{name: "disable all", args: []string{"disable-all", "--confirm-drained"}, verb: adminCtlDisableAll, ok: true},
		{name: "downgrade schema", args: []string{"downgrade-schema", "--confirm-drained"}, verb: adminCtlDowngradeSchema, ok: true},
		{name: "preflight", args: []string{"preflight", "--confirm-drained"}, verb: adminCtlPreflight, ok: true},
		{name: "missing verb"},
		{name: "unknown verb", args: []string{"activate", "--confirm-drained"}},
		{name: "missing confirm", args: []string{"disable-all"}},
		{name: "duplicate confirm", args: []string{"disable-all", "--confirm-drained", "--confirm-drained"}},
		{name: "confirm value", args: []string{"disable-all", "--confirm-drained=true"}},
		{name: "extra flag", args: []string{"disable-all", "--confirm-drained", "--force"}},
		{name: "extra positional", args: []string{"disable-all", "--confirm-drained", "now"}},
		{name: "wrong order", args: []string{"--confirm-drained", "disable-all"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command, err := parseAdminCtlCommand(tc.args)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.verb, command.verb)
			assert.True(t, command.confirmDrained)
		})
	}
}

func TestAdminCtlGateFailsClosedByVerb(t *testing.T) {
	validDisabled := adminCtlConfig{}
	validPreflight := adminCtlConfig{
		clusterEnabled:       true,
		replicaCount:         1,
		replicaCountExplicit: true,
	}

	require.NoError(t, validateAdminCtlGate(adminCtlCommand{verb: adminCtlDisableAll}, validDisabled))
	require.NoError(t, validateAdminCtlGate(adminCtlCommand{verb: adminCtlDowngradeSchema}, validDisabled))
	require.NoError(t, validateAdminCtlGate(adminCtlCommand{verb: adminCtlPreflight}, validPreflight))

	for _, verb := range []adminCtlVerb{adminCtlDisableAll, adminCtlDowngradeSchema} {
		err := validateAdminCtlGate(adminCtlCommand{verb: verb}, adminCtlConfig{clusterEnabled: true})
		require.Error(t, err)
	}
	for _, cfg := range []adminCtlConfig{
		{},
		{clusterEnabled: true},
		{clusterEnabled: true, replicaCount: 1},
		{clusterEnabled: true, replicaCount: 0, replicaCountExplicit: true},
		{clusterEnabled: true, replicaCount: 2, replicaCountExplicit: true},
	} {
		require.Error(t, validateAdminCtlGate(adminCtlCommand{verb: adminCtlPreflight}, cfg))
	}
}

func TestAdminCtlDestructiveGateRefusalPrecedesEveryOperation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "disable all", args: []string{"disable-all", "--confirm-drained"}},
		{name: "downgrade schema", args: []string{"downgrade-schema", "--confirm-drained"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			deps := adminCtlDependencies{
				config: adminCtlConfig{clusterEnabled: true, replicaCount: 1, replicaCountExplicit: true},
				operations: adminCtlOperations{
					disableAll: func(context.Context) (adminCtlDisableResult, error) {
						called = true
						return adminCtlDisableResult{}, nil
					},
					assertZero: func(context.Context) (adminCtlResidual, error) {
						called = true
						return adminCtlResidual{}, nil
					},
					exactDowngrade: func(context.Context) error {
						called = true
						return nil
					},
				},
				stdout: &strings.Builder{},
			}
			assert.Equal(t, 1, runAdminCtl(context.Background(), deps, tc.args))
			assert.False(t, called)
		})
	}
}

func TestPreflightValidationRequiresCleanSchemaAndConsistentPendingRows(t *testing.T) {
	valid := adminCtlPreflightResult{
		migrationVersion:    87,
		requiredTables:      2,
		requiredColumns:     8,
		disclosureAvailable: false,
	}
	require.NoError(t, validateAdminCtlPreflight(valid))
	valid.migrationVersion = 88
	valid.disclosureAvailable = true
	require.NoError(t, validateAdminCtlPreflight(valid))

	tests := []struct {
		name   string
		mutate func(*adminCtlPreflightResult)
	}{
		{name: "migration too old", mutate: func(result *adminCtlPreflightResult) { result.migrationVersion = 86 }},
		{name: "dirty migration", mutate: func(result *adminCtlPreflightResult) { result.migrationDirty = true }},
		{name: "missing table", mutate: func(result *adminCtlPreflightResult) { result.requiredTables = 1 }},
		{name: "missing column", mutate: func(result *adminCtlPreflightResult) { result.requiredColumns = 7 }},
		{name: "inconsistent pending", mutate: func(result *adminCtlPreflightResult) {
			result.pendingRows = 1
			result.inconsistentPending = 1
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := valid
			result.migrationVersion = 87
			result.disclosureAvailable = false
			tc.mutate(&result)
			require.Error(t, validateAdminCtlPreflight(result))
		})
	}
}

func TestPreflightReadsRealSchemaWithoutMutatingSettingsOrPending(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	ownerID := dbtest.CreateUser(t, db)
	operationID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, custom_text_tier, custom_text,
			presence_settings_version, presence_settings_operation_id
		) VALUES ($1, 2, 'pending secret', 1, $2)
	`, ownerID, operationID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO presence_settings_pending_operations (
			user_id, operation_id, prior_settings_version
		) VALUES ($1, $2, 0)
	`, ownerID, operationID)
	require.NoError(t, err)

	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "self-hosted"})
	controller := newAdminCtlController(db, NewService(db, disclosure, false), disclosure)
	result, err := controller.preflight(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.migrationVersion, uint(87))
	assert.False(t, result.migrationDirty)
	assert.Equal(t, 2, result.requiredTables)
	assert.Equal(t, 8, result.requiredColumns)
	assert.False(t, result.disclosureAvailable)
	assert.EqualValues(t, 1, result.pendingRows)
	assert.Zero(t, result.inconsistentPending)
	assertAdminCtlPendingState(t, db, ownerID, operationID, operationID, 1, 2, "pending secret")

	replacementID := uuid.New()
	_, err = db.Exec(`
		UPDATE user_presence_settings
		SET presence_settings_operation_id = $2
		WHERE user_id = $1
	`, ownerID, replacementID)
	require.NoError(t, err)
	result, err = controller.preflight(context.Background())
	require.Error(t, err)
	assert.EqualValues(t, 1, result.pendingRows)
	assert.EqualValues(t, 1, result.inconsistentPending)
	assertAdminCtlPendingState(t, db, ownerID, replacementID, operationID, 1, 2, "pending secret")
}

func TestPreflightRejectsEveryPendingStructuralInconsistencyWithoutMutation(t *testing.T) {
	tests := []struct {
		name            string
		createSettings  bool
		nullMarker      bool
		mismatchMarker  bool
		settingsVersion int64
		priorVersion    int64
	}{
		{name: "missing settings", settingsVersion: 1},
		{name: "null marker", createSettings: true, nullMarker: true, settingsVersion: 1},
		{name: "mismatched marker", createSettings: true, mismatchMarker: true, settingsVersion: 1},
		{name: "mismatched version", createSettings: true, settingsVersion: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			ownerID := dbtest.CreateUser(t, db)
			operationID := uuid.New()
			var settingsMarker any = operationID
			if tc.nullMarker {
				settingsMarker = nil
			}
			if tc.mismatchMarker {
				settingsMarker = uuid.New()
			}
			if tc.createSettings {
				_, err := db.Exec(`
					INSERT INTO user_presence_settings (
						user_id, presence_settings_version, presence_settings_operation_id
					) VALUES ($1, $2, $3)
				`, ownerID, tc.settingsVersion, settingsMarker)
				require.NoError(t, err)
			}
			_, err := db.Exec(`
				INSERT INTO presence_settings_pending_operations (
					user_id, operation_id, prior_settings_version
				) VALUES ($1, $2, $3)
			`, ownerID, operationID, tc.priorVersion)
			require.NoError(t, err)

			controller := newAdminCtlController(db, NewService(db, DisclosureState{}, false), DisclosureState{})
			result, err := controller.preflight(context.Background())
			require.Error(t, err)
			assert.EqualValues(t, 1, result.pendingRows)
			assert.EqualValues(t, 1, result.inconsistentPending)

			var gotOperation uuid.UUID
			var gotPrior int64
			require.NoError(t, db.QueryRow(`
				SELECT operation_id, prior_settings_version
				FROM presence_settings_pending_operations WHERE user_id = $1
			`, ownerID).Scan(&gotOperation, &gotPrior))
			assert.Equal(t, operationID, gotOperation)
			assert.Equal(t, tc.priorVersion, gotPrior)
			if tc.createSettings {
				var gotVersion int64
				var gotMarker uuid.NullUUID
				require.NoError(t, db.QueryRow(`
					SELECT presence_settings_version, presence_settings_operation_id
					FROM user_presence_settings WHERE user_id = $1
				`, ownerID).Scan(&gotVersion, &gotMarker))
				assert.Equal(t, tc.settingsVersion, gotVersion)
				assert.Equal(t, settingsMarker != nil, gotMarker.Valid)
			}
		})
	}
}

func TestDisableAllUsesOneOrderedSenderTransactionAndConverges(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	owners := seedAdminCtlAffectedOwners(t, db)
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	service := NewService(db, disclosure, false)
	controller := newAdminCtlController(db, service, disclosure)
	controller.ownerBatchSize = 2

	var gated []string
	controller.withSender = func(ctx context.Context, ownerID uuid.UUID, work func() error) error {
		gated = append(gated, ownerID.String())
		return service.WithSender(ctx, ownerID, work)
	}
	var begins, commits int
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		Begin: func(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
			begins++
			return db.BeginTx(ctx, options)
		},
		Commit: func(tx *sql.Tx) error {
			commits++
			return tx.Commit()
		},
	})
	defer restore()

	result, err := controller.disableAll(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 6, result.ownersProcessed)
	assert.EqualValues(t, 4, result.settingsCleared)
	assert.EqualValues(t, 2, result.historyDeleted)
	assert.EqualValues(t, 2, result.pendingDeleted)
	assert.Equal(t, adminCtlResidual{}, result.remaining)
	assert.Equal(t, 6, begins)
	assert.Equal(t, 6, commits)

	wantOrder := make([]string, 0, len(owners))
	for _, ownerID := range owners {
		wantOrder = append(wantOrder, ownerID.String())
	}
	slices.Sort(wantOrder)
	assert.Equal(t, wantOrder, gated)
	assertAdminCtlConverged(t, db, owners)

	gated = nil
	result, err = controller.disableAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, adminCtlDisableResult{remaining: adminCtlResidual{}}, result)
	assert.Empty(t, gated)
}

func TestDisableAllRollsBackBeginStatementAndCommitFailuresAndIsRerunnable(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		db, _ := dbtest.SetupTestDB(t)
		ownerID := seedAdminCtlEnabledOwner(t, db)
		service := NewService(db, DisclosureState{}, false)
		controller := newAdminCtlController(db, service, DisclosureState{})
		sentinel := errors.New("begin failure")
		restore := service.SetTransactionTestHooks(TransactionTestHooks{
			Begin: func(context.Context, *sql.TxOptions) (*sql.Tx, error) { return nil, sentinel },
		})
		_, err := controller.disableAll(context.Background())
		require.ErrorIs(t, err, sentinel)
		assertAdminCtlEnabled(t, db, ownerID, true)
		restore()
		_, err = controller.disableAll(context.Background())
		require.NoError(t, err)
		assertAdminCtlEnabled(t, db, ownerID, false)
	})

	t.Run("statement", func(t *testing.T) {
		db, _ := dbtest.SetupTestDB(t)
		ownerID := seedAdminCtlEnabledOwner(t, db)
		controller := newAdminCtlController(db, NewService(db, DisclosureState{}, false), DisclosureState{})
		sentinel := errors.New("statement failure")
		productionStep := controller.disableOwner
		controller.disableOwner = func(ctx context.Context, tx *sql.Tx, id uuid.UUID) (adminCtlOwnerResult, error) {
			_, execErr := tx.ExecContext(ctx, `
				UPDATE user_presence_settings
				SET activity_history_enabled = FALSE,
				    activity_history_consent_version = NULL,
				    activity_history_consent_copy_hash = NULL,
				    activity_history_consented_at = NULL
				WHERE user_id = $1
			`, id)
			require.NoError(t, execErr)
			return adminCtlOwnerResult{}, sentinel
		}
		_, err := controller.disableAll(context.Background())
		require.ErrorIs(t, err, sentinel)
		assertAdminCtlEnabled(t, db, ownerID, true)
		controller.disableOwner = productionStep
		_, err = controller.disableAll(context.Background())
		require.NoError(t, err)
		assertAdminCtlEnabled(t, db, ownerID, false)
	})

	t.Run("commit", func(t *testing.T) {
		db, _ := dbtest.SetupTestDB(t)
		ownerID := seedAdminCtlEnabledOwner(t, db)
		service := NewService(db, DisclosureState{}, false)
		controller := newAdminCtlController(db, service, DisclosureState{})
		sentinel := errors.New("commit failure")
		restore := service.SetTransactionTestHooks(TransactionTestHooks{
			Commit: func(*sql.Tx) error { return sentinel },
		})
		_, err := controller.disableAll(context.Background())
		require.ErrorIs(t, err, sentinel)
		assertAdminCtlEnabled(t, db, ownerID, true)
		restore()
		_, err = controller.disableAll(context.Background())
		require.NoError(t, err)
		assertAdminCtlEnabled(t, db, ownerID, false)
	})
}

func TestDisableAllFailsClosedWhenFreshZeroAssertionsDoNotConverge(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	controller := newAdminCtlController(db, NewService(db, DisclosureState{}, false), DisclosureState{})
	controller.countResidual = func(context.Context) (adminCtlResidual, error) {
		return adminCtlResidual{enabled: 1, history: 2, pending: 3}, nil
	}
	result, err := controller.disableAll(context.Background())
	require.Error(t, err)
	assert.Equal(t, adminCtlResidual{enabled: 1, history: 2, pending: 3}, result.remaining)
}

func TestDisableAllForceOffDecisionRequiresPendingRow(t *testing.T) {
	assert.False(t, adminCtlMustForceOff(true, false))
	assert.True(t, adminCtlMustForceOff(false, true))
	assert.True(t, adminCtlMustForceOff(true, true))
	assert.False(t, adminCtlMustForceOff(false, false))
}

func TestDisableAllLockOrderIsCanonicalAndAuditable(t *testing.T) {
	source, err := os.ReadFile("adminctl.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	body := string(source)
	start := strings.Index(body, "func disableAdminCtlOwner(")
	require.NotEqual(t, -1, start)
	body = body[start:]
	steps := []string{
		"lockUser(ctx, tx, ownerID)",
		"lockAdminCtlSettings(ctx, tx, ownerID)",
		"lockAdminCtlPending(ctx, tx, ownerID)",
		"lockAdminCtlHistory(ctx, tx, ownerID)",
	}
	previous := -1
	for _, step := range steps {
		index := strings.Index(body, step)
		require.Greater(t, index, previous, "canonical lock step %q", step)
		previous = index
	}
}

func TestAdminCtlEnvironmentParsingIsStrictAndScoped(t *testing.T) {
	environment, err := adminCtlEnvironmentFromLookup(adminCtlLookup(map[string]string{
		"DATABASE_URL":                        "postgres://maintenance",
		"ACTIVITY_HISTORY_CLUSTER_ENABLED":    "true",
		"CONTROL_PLANE_REPLICA_COUNT":         "1",
		"INSTANCE_TYPE":                       "self-hosted",
		"ENVIRONMENT":                         "production",
		"ACTIVITY_HISTORY_OPERATOR_NAME":      "Example Operator",
		"ACTIVITY_HISTORY_PRIVACY_POLICY_URL": "https://example.test/privacy",
	}))
	require.NoError(t, err)
	assert.Equal(t, "postgres://maintenance", environment.databaseURL)
	assert.Equal(t, adminCtlConfig{clusterEnabled: true, replicaCount: 1, replicaCountExplicit: true}, environment.config)
	assert.Equal(t, "self-hosted", environment.instanceType)
	assert.Equal(t, "production", environment.environment)
	assert.Equal(t, "Example Operator", environment.operatorName)
	assert.Equal(t, "https://example.test/privacy", environment.privacyPolicyURL)

	defaults, err := adminCtlEnvironmentFromLookup(adminCtlLookup(map[string]string{
		"DATABASE_URL": "postgres://maintenance",
	}))
	require.NoError(t, err)
	assert.Equal(t, adminCtlConfig{}, defaults.config)
	assert.Equal(t, "saas", defaults.instanceType)
	assert.Equal(t, "development", defaults.environment)

	for _, values := range []map[string]string{
		{},
		{"DATABASE_URL": "postgres://maintenance", "ACTIVITY_HISTORY_CLUSTER_ENABLED": "TRUE"},
		{"DATABASE_URL": "postgres://maintenance", "CONTROL_PLANE_REPLICA_COUNT": "one"},
	} {
		_, err := adminCtlEnvironmentFromLookup(adminCtlLookup(values))
		require.Error(t, err)
	}
}

func TestRunAdminCtlFailsBeforeServingDependencies(t *testing.T) {
	t.Run("usage before environment", func(t *testing.T) {
		assert.Equal(t, 2, RunAdminCtl(nil))
	})
	t.Run("configuration before database", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "")
		t.Setenv("ACTIVITY_HISTORY_CLUSTER_ENABLED", "false")
		assert.Equal(t, 1, RunAdminCtl([]string{"disable-all", "--confirm-drained"}))
	})
	t.Run("guard before database", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://must-not-open")
		t.Setenv("ACTIVITY_HISTORY_CLUSTER_ENABLED", "true")
		t.Setenv("CONTROL_PLANE_REPLICA_COUNT", "1")
		assert.Equal(t, 1, RunAdminCtl([]string{"disable-all", "--confirm-drained"}))
	})
	t.Run("database open is stable failure", func(t *testing.T) {
		environment := adminCtlEnvironment{databaseURL: ":"}
		assert.Equal(t, 1, runAdminCtlEnvironment(context.Background(), environment,
			[]string{"disable-all", "--confirm-drained"}))
	})
}

func TestAdminCtlPrintsOnlyAggregateResultsAndStableFailureClasses(t *testing.T) {
	var stdout strings.Builder
	deps := adminCtlDependencies{
		config: adminCtlConfig{},
		operations: adminCtlOperations{
			disableAll: func(context.Context) (adminCtlDisableResult, error) {
				return adminCtlDisableResult{
					ownersProcessed: 4,
					settingsCleared: 3,
					historyDeleted:  2,
					pendingDeleted:  1,
					remaining:       adminCtlResidual{},
				}, nil
			},
		},
		stdout: &stdout,
	}
	code := runAdminCtl(context.Background(), deps, []string{"disable-all", "--confirm-drained"})
	assert.Zero(t, code)
	assert.Equal(t, "activity-history disable-all status=success owners=4 settings=3 history=2 pending=1 enabled_remaining=0 history_remaining=0 pending_remaining=0\n", stdout.String())

	stdout.Reset()
	sensitive := "owner=159f3f4e-6584-46b0-b96b-20f9d9f58257 SELECT payload postgres://secret@db"
	deps.operations.disableAll = func(context.Context) (adminCtlDisableResult, error) {
		return adminCtlDisableResult{}, errors.New(sensitive)
	}
	code = runAdminCtl(context.Background(), deps, []string{"disable-all", "--confirm-drained"})
	assert.Equal(t, 1, code)
	assert.Equal(t, "activity-history disable-all status=failed error_class=database_operation\n", stdout.String())
	for _, forbidden := range strings.Fields(sensitive) {
		assert.NotContains(t, stdout.String(), forbidden)
	}
}

func TestAdminCtlDisableRejectsNonzeroResultEvenWithoutOperationError(t *testing.T) {
	var stdout strings.Builder
	deps := adminCtlDependencies{
		config: adminCtlConfig{},
		operations: adminCtlOperations{
			disableAll: func(context.Context) (adminCtlDisableResult, error) {
				return adminCtlDisableResult{remaining: adminCtlResidual{history: 1}}, nil
			},
		},
		stdout: &stdout,
	}
	code := runAdminCtl(context.Background(), deps, []string{"disable-all", "--confirm-drained"})
	assert.Equal(t, 1, code)
	assert.Equal(t, "activity-history disable-all status=failed error_class=zero_assertion\n", stdout.String())
	assert.NotContains(t, stdout.String(), "status=success")
}

func TestAdminCtlMissingOperationsAndUnknownDispatchFailClosed(t *testing.T) {
	ctx := context.Background()
	var stdout strings.Builder
	assert.Equal(t, 1, runAdminCtl(ctx, adminCtlDependencies{
		config: adminCtlConfig{}, stdout: &stdout,
	}, []string{"disable-all", "--confirm-drained"}))
	assert.Contains(t, stdout.String(), "error_class=database_operation")

	stdout.Reset()
	assert.Equal(t, 1, runAdminCtl(ctx, adminCtlDependencies{
		config: adminCtlConfig{clusterEnabled: true, replicaCount: 1, replicaCountExplicit: true},
		stdout: &stdout,
	}, []string{"preflight", "--confirm-drained"}))
	assert.Contains(t, stdout.String(), "error_class=readiness_probe")

	stdout.Reset()
	assert.Equal(t, 1, runAdminCtl(ctx, adminCtlDependencies{
		config: adminCtlConfig{}, stdout: &stdout,
	}, []string{"downgrade-schema", "--confirm-drained"}))
	assert.Contains(t, stdout.String(), "error_class=zero_assertion")

	stdout.Reset()
	assert.Equal(t, 2, executeAdminCtl(ctx, adminCtlDependencies{stdout: &stdout},
		adminCtlCommand{verb: adminCtlVerb(255)}))
	assert.Contains(t, stdout.String(), "error_class=usage")
	require.Error(t, validateAdminCtlGate(adminCtlCommand{verb: adminCtlVerb(255)}, adminCtlConfig{}))
	writeAdminCtl(nil, "must be discarded")
}

func TestAdminCtlPreflightAndDowngradeDispatchUseAggregateContracts(t *testing.T) {
	t.Run("preflight", func(t *testing.T) {
		var stdout strings.Builder
		deps := adminCtlDependencies{
			config: adminCtlConfig{clusterEnabled: true, replicaCount: 1, replicaCountExplicit: true},
			operations: adminCtlOperations{
				preflight: func(context.Context) (adminCtlPreflightResult, error) {
					return adminCtlPreflightResult{
						migrationVersion:    87,
						requiredTables:      2,
						requiredColumns:     8,
						disclosureAvailable: false,
						pendingRows:         2,
					}, nil
				},
			},
			stdout: &stdout,
		}
		code := runAdminCtl(context.Background(), deps, []string{"preflight", "--confirm-drained"})
		assert.Zero(t, code)
		assert.Equal(t, "activity-history preflight status=success migration_version=87 migration_dirty=false required_tables=2 required_columns=8 disclosure_available=false pending=2 pending_inconsistent=0\n", stdout.String())
	})

	t.Run("downgrade", func(t *testing.T) {
		var stdout strings.Builder
		var zeroChecked, downgraded bool
		deps := adminCtlDependencies{
			config: adminCtlConfig{},
			operations: adminCtlOperations{
				assertZero: func(context.Context) (adminCtlResidual, error) {
					zeroChecked = true
					return adminCtlResidual{}, nil
				},
				exactDowngrade: func(context.Context) error {
					downgraded = true
					return nil
				},
			},
			stdout: &stdout,
		}
		code := runAdminCtl(context.Background(), deps, []string{"downgrade-schema", "--confirm-drained"})
		assert.Zero(t, code)
		assert.True(t, zeroChecked)
		assert.True(t, downgraded)
		assert.Equal(t, "activity-history downgrade-schema status=success enabled_remaining=0 history_remaining=0 pending_remaining=0 migration_version=86\n", stdout.String())
	})
}

func TestDowngradeDoesNotRunWhenFreshZeroProbeFails(t *testing.T) {
	tests := []struct {
		name     string
		residual adminCtlResidual
		err      error
	}{
		{name: "nonzero", residual: adminCtlResidual{pending: 1}},
		{name: "probe error", err: errors.New("raw database probe failure")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout strings.Builder
			downgraded := false
			deps := adminCtlDependencies{
				config: adminCtlConfig{},
				operations: adminCtlOperations{
					assertZero: func(context.Context) (adminCtlResidual, error) {
						return tc.residual, tc.err
					},
					exactDowngrade: func(context.Context) error {
						downgraded = true
						return nil
					},
				},
				stdout: &stdout,
			}
			code := runAdminCtl(context.Background(), deps, []string{"downgrade-schema", "--confirm-drained"})
			assert.Equal(t, 1, code)
			assert.False(t, downgraded)
			assert.Equal(t, "activity-history downgrade-schema status=failed error_class=zero_assertion\n", stdout.String())
		})
	}
}

func TestAdminCtlPreflightAndDowngradeFailuresNeverPrintRawDetails(t *testing.T) {
	sensitive := errors.New("owner=159f3f4e-6584-46b0-b96b-20f9d9f58257 SELECT payload postgres://secret@db")
	tests := []struct {
		name       string
		config     adminCtlConfig
		args       []string
		operations adminCtlOperations
		want       string
	}{
		{
			name:   "preflight",
			config: adminCtlConfig{clusterEnabled: true, replicaCount: 1, replicaCountExplicit: true},
			args:   []string{"preflight", "--confirm-drained"},
			operations: adminCtlOperations{
				preflight: func(context.Context) (adminCtlPreflightResult, error) {
					return adminCtlPreflightResult{}, sensitive
				},
			},
			want: "activity-history preflight status=failed error_class=readiness_probe\n",
		},
		{
			name: "downgrade",
			args: []string{"downgrade-schema", "--confirm-drained"},
			operations: adminCtlOperations{
				assertZero: func(context.Context) (adminCtlResidual, error) {
					return adminCtlResidual{}, nil
				},
				exactDowngrade: func(context.Context) error { return sensitive },
			},
			want: "activity-history downgrade-schema status=failed error_class=exact_downgrade\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout strings.Builder
			deps := adminCtlDependencies{config: tc.config, operations: tc.operations, stdout: &stdout}
			code := runAdminCtl(context.Background(), deps, tc.args)
			assert.Equal(t, 1, code)
			assert.Equal(t, tc.want, stdout.String())
			for _, forbidden := range strings.Fields(sensitive.Error()) {
				assert.NotContains(t, stdout.String(), forbidden)
			}
		})
	}
}

func seedAdminCtlAffectedOwners(t *testing.T, db *sql.DB) []uuid.UUID {
	t.Helper()
	enabledOwner := seedAdminCtlEnabledOwner(t, db)
	residueOwner := dbtest.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (user_id, activity_history_reconsent_required)
		VALUES ($1, TRUE)
	`, residueOwner)
	require.NoError(t, err)

	pendingOwner := dbtest.CreateUser(t, db)
	operationID := uuid.New()
	_, err = db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, custom_text_tier, custom_text, custom_text_emoji,
			presence_settings_version, presence_settings_operation_id
		) VALUES ($1, 2, 'pending secret', 'lock', 1, $2)
	`, pendingOwner, operationID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO presence_settings_pending_operations (
			user_id, operation_id, prior_settings_version
		) VALUES ($1, $2, 0)
	`, pendingOwner, operationID)
	require.NoError(t, err)

	markerOnlyOwner := dbtest.CreateUser(t, db)
	_, err = db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, custom_text_tier, custom_text, custom_text_emoji,
			presence_settings_operation_id
		)
		VALUES ($1, 2, 'marker-only secret', 'marker', $2)
	`, markerOnlyOwner, uuid.New())
	require.NoError(t, err)

	pendingOnlyOwner := dbtest.CreateUser(t, db)
	_, err = db.Exec(`
		INSERT INTO presence_settings_pending_operations (
			user_id, operation_id, prior_settings_version
		) VALUES ($1, $2, 0)
	`, pendingOnlyOwner, uuid.New())
	require.NoError(t, err)

	historyOnlyOwner := dbtest.CreateUser(t, db)
	seedAdminCtlHistory(t, db, enabledOwner)
	seedAdminCtlHistory(t, db, historyOnlyOwner)
	return []uuid.UUID{
		enabledOwner,
		residueOwner,
		pendingOwner,
		markerOnlyOwner,
		pendingOnlyOwner,
		historyOnlyOwner,
	}
}

func seedAdminCtlEnabledOwner(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()
	ownerID := dbtest.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, activity_history_enabled, activity_history_consent_version,
			activity_history_consent_copy_hash, activity_history_consented_at
		) VALUES ($1, TRUE, 1, $2, clock_timestamp())
	`, ownerID, strings.Repeat("a", 64))
	require.NoError(t, err)
	return ownerID
}

func seedAdminCtlHistory(t *testing.T, db *sql.DB, ownerID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO presence_history (
			id, sender_id, category, payload_version, payload,
			started_at, ended_at, recorded_at, expires_at
		) VALUES (
			$1, $2, 'custom_text', 1, '{"text":"history secret"}',
			clock_timestamp() - INTERVAL '1 minute', clock_timestamp(),
			clock_timestamp(), clock_timestamp() + INTERVAL '30 days'
		)
	`, uuid.New(), ownerID)
	require.NoError(t, err)
}

func assertAdminCtlConverged(t *testing.T, db *sql.DB, owners []uuid.UUID) {
	t.Helper()
	for _, ownerID := range owners {
		var enabled bool
		var consentVersion sql.NullInt16
		var consentHash sql.NullString
		var consentedAt sql.NullTime
		var reconsent bool
		var marker uuid.NullUUID
		err := db.QueryRow(`
			SELECT activity_history_enabled, activity_history_consent_version,
			       activity_history_consent_copy_hash, activity_history_consented_at,
			       activity_history_reconsent_required, presence_settings_operation_id
			FROM user_presence_settings WHERE user_id = $1
		`, ownerID).Scan(&enabled, &consentVersion, &consentHash, &consentedAt, &reconsent, &marker)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		require.NoError(t, err)
		assert.False(t, enabled)
		assert.False(t, consentVersion.Valid)
		assert.False(t, consentHash.Valid)
		assert.False(t, consentedAt.Valid)
		assert.False(t, reconsent)
		assert.False(t, marker.Valid)
	}
	var history, pending int64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM presence_history`).Scan(&history))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM presence_settings_pending_operations`).Scan(&pending))
	assert.Zero(t, history)
	assert.Zero(t, pending)

	var pendingTier int
	var pendingText, pendingEmoji sql.NullString
	require.NoError(t, db.QueryRow(`
		SELECT custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings WHERE user_id = $1
	`, owners[2]).Scan(&pendingTier, &pendingText, &pendingEmoji))
	assert.Zero(t, pendingTier)
	assert.False(t, pendingText.Valid)
	assert.False(t, pendingEmoji.Valid)

	var markerTier int
	var markerText, markerEmoji string
	require.NoError(t, db.QueryRow(`
		SELECT custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings WHERE user_id = $1
	`, owners[3]).Scan(&markerTier, &markerText, &markerEmoji))
	assert.Equal(t, 2, markerTier)
	assert.Equal(t, "marker-only secret", markerText)
	assert.Equal(t, "marker", markerEmoji)
}

func assertAdminCtlEnabled(t *testing.T, db *sql.DB, ownerID uuid.UUID, want bool) {
	t.Helper()
	var enabled bool
	require.NoError(t, db.QueryRow(`
		SELECT activity_history_enabled FROM user_presence_settings WHERE user_id = $1
	`, ownerID).Scan(&enabled))
	assert.Equal(t, want, enabled)
}

func assertAdminCtlPendingState(
	t *testing.T,
	db *sql.DB,
	ownerID uuid.UUID,
	settingsMarker uuid.UUID,
	pendingMarker uuid.UUID,
	version int64,
	tier int,
	text string,
) {
	t.Helper()
	var gotMarker uuid.UUID
	var gotVersion int64
	var gotTier int
	var gotText string
	require.NoError(t, db.QueryRow(`
		SELECT presence_settings_operation_id, presence_settings_version,
		       custom_text_tier, custom_text
		FROM user_presence_settings WHERE user_id = $1
	`, ownerID).Scan(&gotMarker, &gotVersion, &gotTier, &gotText))
	assert.Equal(t, settingsMarker, gotMarker)
	assert.Equal(t, version, gotVersion)
	assert.Equal(t, tier, gotTier)
	assert.Equal(t, text, gotText)
	var gotPendingMarker uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT operation_id FROM presence_settings_pending_operations WHERE user_id = $1
	`, ownerID).Scan(&gotPendingMarker))
	assert.Equal(t, pendingMarker, gotPendingMarker)
}

func adminCtlLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
