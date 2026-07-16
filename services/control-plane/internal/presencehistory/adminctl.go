package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
)

const (
	adminCtlOwnerBatch      = 100
	adminCtlRequiredTables  = 2
	adminCtlRequiredColumns = 8
)

type adminCtlVerb uint8

const (
	adminCtlDisableAll adminCtlVerb = iota
	adminCtlDowngradeSchema
	adminCtlPreflight
)

type adminCtlCommand struct {
	verb           adminCtlVerb
	confirmDrained bool
}

type adminCtlConfig struct {
	clusterEnabled       bool
	replicaCount         int
	replicaCountExplicit bool
}

type adminCtlEnvironment struct {
	databaseURL      string
	config           adminCtlConfig
	instanceType     string
	environment      string
	operatorName     string
	privacyPolicyURL string
}

type adminCtlResidual struct {
	enabled int64
	history int64
	pending int64
}

type adminCtlOwnerResult struct {
	settingsCleared int64
	historyDeleted  int64
	pendingDeleted  int64
}

type adminCtlSettingsLock struct {
	present       bool
	markerPresent bool
}

type adminCtlDisableResult struct {
	ownersProcessed int64
	settingsCleared int64
	historyDeleted  int64
	pendingDeleted  int64
	remaining       adminCtlResidual
}

type adminCtlPreflightResult struct {
	migrationVersion    uint
	migrationDirty      bool
	requiredTables      int
	requiredColumns     int
	disclosureAvailable bool
	pendingRows         int64
	inconsistentPending int64
}

type adminCtlOperations struct {
	disableAll     func(context.Context) (adminCtlDisableResult, error)
	preflight      func(context.Context) (adminCtlPreflightResult, error)
	assertZero     func(context.Context) (adminCtlResidual, error)
	exactDowngrade func(context.Context) error
}

type adminCtlDependencies struct {
	config     adminCtlConfig
	operations adminCtlOperations
	stdout     io.Writer
}

type adminCtlController struct {
	db             *sql.DB
	service        *Service
	disclosure     DisclosureState
	ownerBatchSize int
	withSender     func(context.Context, uuid.UUID, func() error) error
	disableOwner   func(context.Context, *sql.Tx, uuid.UUID) (adminCtlOwnerResult, error)
	countResidual  func(context.Context) (adminCtlResidual, error)
}

// RunAdminCtl runs one non-serving Activity History operator command. It reads
// only the environment needed by this maintenance path and never initializes
// Redis, NATS, HTTP/WebSocket listeners, delivery, or marker acknowledgement.
func RunAdminCtl(args []string) int {
	if _, err := parseAdminCtlCommand(args); err != nil {
		writeAdminCtl(os.Stderr, "usage: control-plane activity-history <disable-all|downgrade-schema|preflight> --confirm-drained\n")
		return 2
	}
	environment, err := adminCtlEnvironmentFromLookup(os.LookupEnv)
	if err != nil {
		writeAdminCtl(os.Stderr, "activity-history command status=failed error_class=configuration\n")
		return 1
	}
	if err := validateAdminCtlGate(adminCtlCommandForArgs(args), environment.config); err != nil {
		writeAdminCtl(os.Stderr, "activity-history command status=failed error_class=guard\n")
		return 1
	}
	return runAdminCtlEnvironment(context.Background(), environment, args)
}

func runAdminCtlEnvironment(ctx context.Context, environment adminCtlEnvironment, args []string) int {
	db, err := database.New(environment.databaseURL)
	if err != nil {
		writeAdminCtl(os.Stderr, "activity-history command status=failed error_class=database_open\n")
		return 1
	}
	defer func() { _ = db.Close() }()

	disclosure := BuildDisclosure(DisclosureOptions{
		InstanceType:     environment.instanceType,
		OperatorName:     environment.operatorName,
		PrivacyPolicyURL: environment.privacyPolicyURL,
		Development:      environment.environment == "development" || environment.environment == "test",
	})
	service := NewService(db, disclosure, false)
	controller := newAdminCtlController(db, service, disclosure)
	deps := adminCtlDependencies{
		config: environment.config,
		operations: adminCtlOperations{
			disableAll: controller.disableAll,
			preflight:  controller.preflight,
			assertZero: controller.assertZero,
			exactDowngrade: func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return database.ExactDowngradeMigration87(environment.databaseURL)
			},
		},
		stdout: os.Stdout,
	}
	return runAdminCtl(ctx, deps, args)
}

func adminCtlEnvironmentFromLookup(lookup func(string) (string, bool)) (adminCtlEnvironment, error) {
	var result adminCtlEnvironment
	databaseURL, present := lookup("DATABASE_URL")
	if !present || strings.TrimSpace(databaseURL) == "" {
		return result, errors.New("maintenance database URL unavailable")
	}
	result.databaseURL = databaseURL

	gate, _ := lookup("ACTIVITY_HISTORY_CLUSTER_ENABLED")
	switch gate {
	case "", "false":
		result.config.clusterEnabled = false
	case "true":
		result.config.clusterEnabled = true
	default:
		return adminCtlEnvironment{}, errors.New("invalid activity history cluster gate")
	}

	count, explicit := lookup("CONTROL_PLANE_REPLICA_COUNT")
	result.config.replicaCountExplicit = explicit
	if explicit {
		parsed, err := strconv.Atoi(count)
		if err != nil {
			return adminCtlEnvironment{}, errors.New("invalid control-plane replica count")
		}
		result.config.replicaCount = parsed
	}
	result.instanceType = adminCtlEnvironmentValue(lookup, "INSTANCE_TYPE", "saas")
	result.environment = adminCtlEnvironmentValue(lookup, "ENVIRONMENT", "development")
	result.operatorName, _ = lookup("ACTIVITY_HISTORY_OPERATOR_NAME")
	result.privacyPolicyURL, _ = lookup("ACTIVITY_HISTORY_PRIVACY_POLICY_URL")
	return result, nil
}

func adminCtlEnvironmentValue(lookup func(string) (string, bool), name, fallback string) string {
	value, present := lookup(name)
	if !present || value == "" {
		return fallback
	}
	return value
}

func adminCtlCommandForArgs(args []string) adminCtlCommand {
	command, _ := parseAdminCtlCommand(args)
	return command
}

func parseAdminCtlCommand(args []string) (adminCtlCommand, error) {
	if len(args) != 2 || args[1] != "--confirm-drained" {
		return adminCtlCommand{}, errors.New("exact verb and confirmation required")
	}
	command := adminCtlCommand{confirmDrained: true}
	switch args[0] {
	case "disable-all":
		command.verb = adminCtlDisableAll
	case "downgrade-schema":
		command.verb = adminCtlDowngradeSchema
	case "preflight":
		command.verb = adminCtlPreflight
	default:
		return adminCtlCommand{}, errors.New("unknown activity history command")
	}
	return command, nil
}

func validateAdminCtlGate(command adminCtlCommand, cfg adminCtlConfig) error {
	switch command.verb {
	case adminCtlDisableAll, adminCtlDowngradeSchema:
		if cfg.clusterEnabled {
			return errors.New("destructive activity history command requires disabled gate")
		}
	case adminCtlPreflight:
		if !cfg.clusterEnabled || !cfg.replicaCountExplicit || cfg.replicaCount != 1 {
			return errors.New("activity history preflight requires enabled single-replica gate")
		}
	default:
		return errors.New("unknown activity history command")
	}
	return nil
}

func runAdminCtl(ctx context.Context, deps adminCtlDependencies, args []string) int {
	command, err := parseAdminCtlCommand(args)
	if err != nil {
		writeAdminCtl(deps.stdout, "activity-history command status=failed error_class=usage\n")
		return 2
	}
	if err := validateAdminCtlGate(command, deps.config); err != nil {
		writeAdminCtl(deps.stdout, "activity-history command status=failed error_class=guard\n")
		return 1
	}
	return executeAdminCtl(ctx, deps, command)
}

func executeAdminCtl(ctx context.Context, deps adminCtlDependencies, command adminCtlCommand) int {
	switch command.verb {
	case adminCtlDisableAll:
		return executeAdminCtlDisable(ctx, deps)
	case adminCtlDowngradeSchema:
		return executeAdminCtlDowngrade(ctx, deps)
	case adminCtlPreflight:
		return executeAdminCtlPreflight(ctx, deps)
	default:
		writeAdminCtl(deps.stdout, "activity-history command status=failed error_class=usage\n")
		return 2
	}
}

func executeAdminCtlDisable(ctx context.Context, deps adminCtlDependencies) int {
	if deps.operations.disableAll == nil {
		writeAdminCtl(deps.stdout, "activity-history disable-all status=failed error_class=database_operation\n")
		return 1
	}
	result, err := deps.operations.disableAll(ctx)
	if err != nil {
		writeAdminCtl(deps.stdout, "activity-history disable-all status=failed error_class=database_operation\n")
		return 1
	}
	if !result.remaining.zero() {
		writeAdminCtl(deps.stdout, "activity-history disable-all status=failed error_class=zero_assertion\n")
		return 1
	}
	writeAdminCtl(deps.stdout,
		"activity-history disable-all status=success owners=%d settings=%d history=%d pending=%d enabled_remaining=%d history_remaining=%d pending_remaining=%d\n",
		result.ownersProcessed, result.settingsCleared, result.historyDeleted, result.pendingDeleted,
		result.remaining.enabled, result.remaining.history, result.remaining.pending,
	)
	return 0
}

func executeAdminCtlPreflight(ctx context.Context, deps adminCtlDependencies) int {
	if deps.operations.preflight == nil {
		writeAdminCtl(deps.stdout, "activity-history preflight status=failed error_class=readiness_probe\n")
		return 1
	}
	result, err := deps.operations.preflight(ctx)
	if err == nil {
		err = validateAdminCtlPreflight(result)
	}
	if err != nil {
		writeAdminCtl(deps.stdout, "activity-history preflight status=failed error_class=readiness_probe\n")
		return 1
	}
	writeAdminCtl(deps.stdout,
		"activity-history preflight status=success migration_version=%d migration_dirty=%t required_tables=%d required_columns=%d disclosure_available=%t pending=%d pending_inconsistent=%d\n",
		result.migrationVersion, result.migrationDirty, result.requiredTables, result.requiredColumns,
		result.disclosureAvailable, result.pendingRows, result.inconsistentPending,
	)
	return 0
}

func executeAdminCtlDowngrade(ctx context.Context, deps adminCtlDependencies) int {
	if deps.operations.assertZero == nil || deps.operations.exactDowngrade == nil {
		writeAdminCtl(deps.stdout, "activity-history downgrade-schema status=failed error_class=zero_assertion\n")
		return 1
	}
	remaining, err := deps.operations.assertZero(ctx)
	if err != nil || !remaining.zero() {
		writeAdminCtl(deps.stdout, "activity-history downgrade-schema status=failed error_class=zero_assertion\n")
		return 1
	}
	if err := deps.operations.exactDowngrade(ctx); err != nil {
		writeAdminCtl(deps.stdout, "activity-history downgrade-schema status=failed error_class=exact_downgrade\n")
		return 1
	}
	writeAdminCtl(deps.stdout,
		"activity-history downgrade-schema status=success enabled_remaining=%d history_remaining=%d pending_remaining=%d migration_version=86\n",
		remaining.enabled, remaining.history, remaining.pending,
	)
	return 0
}

func writeAdminCtl(output io.Writer, format string, values ...any) {
	if output == nil {
		return
	}
	_, _ = fmt.Fprintf(output, format, values...)
}

func (residual adminCtlResidual) zero() bool {
	return residual.enabled == 0 && residual.history == 0 && residual.pending == 0
}

func newAdminCtlController(db *sql.DB, service *Service, disclosure DisclosureState) *adminCtlController {
	controller := &adminCtlController{
		db:             db,
		service:        service,
		disclosure:     cloneDisclosure(disclosure),
		ownerBatchSize: adminCtlOwnerBatch,
	}
	if service != nil {
		controller.withSender = service.WithSender
		controller.disableOwner = disableAdminCtlOwner
	}
	controller.countResidual = controller.readResidual
	return controller
}

func (controller *adminCtlController) disableAll(ctx context.Context) (adminCtlDisableResult, error) {
	var result adminCtlDisableResult
	if controller == nil || controller.db == nil || controller.service == nil ||
		controller.withSender == nil || controller.disableOwner == nil || controller.ownerBatchSize <= 0 {
		return result, errors.New("activity history disable controller unavailable")
	}
	var cursor *uuid.UUID
	for {
		owners, err := controller.discoverAffectedOwners(ctx, cursor)
		if err != nil {
			return result, err
		}
		if len(owners) == 0 {
			break
		}
		for _, ownerID := range owners {
			ownerResult, err := controller.disableOne(ctx, ownerID)
			if err != nil {
				return result, err
			}
			result.add(ownerResult)
		}
		last := owners[len(owners)-1]
		cursor = &last
	}
	remaining, err := controller.assertZero(ctx)
	result.remaining = remaining
	return result, err
}

func (result *adminCtlDisableResult) add(owner adminCtlOwnerResult) {
	result.ownersProcessed++
	result.settingsCleared += owner.settingsCleared
	result.historyDeleted += owner.historyDeleted
	result.pendingDeleted += owner.pendingDeleted
}

func (controller *adminCtlController) discoverAffectedOwners(
	ctx context.Context,
	cursor *uuid.UUID,
) ([]uuid.UUID, error) {
	var after any
	if cursor != nil {
		after = *cursor
	}
	rows, err := controller.db.QueryContext(ctx, `
		SELECT owner_id
		FROM (
			SELECT user_id AS owner_id
			FROM user_presence_settings
			WHERE activity_history_enabled = TRUE
			   OR activity_history_consent_version IS NOT NULL
			   OR activity_history_consent_copy_hash IS NOT NULL
			   OR activity_history_consented_at IS NOT NULL
			   OR activity_history_reconsent_required = TRUE
			   OR presence_settings_operation_id IS NOT NULL
			UNION
			SELECT sender_id AS owner_id FROM presence_history
			UNION
			SELECT user_id AS owner_id FROM presence_settings_pending_operations
		) affected
		WHERE ($1::uuid IS NULL OR owner_id > $1::uuid)
		ORDER BY owner_id
		LIMIT $2
	`, after, controller.ownerBatchSize)
	if err != nil {
		return nil, fmt.Errorf("discover affected activity history owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owners := make([]uuid.UUID, 0, controller.ownerBatchSize)
	for rows.Next() {
		var ownerID uuid.UUID
		if err := rows.Scan(&ownerID); err != nil {
			return nil, fmt.Errorf("scan affected activity history owner: %w", err)
		}
		owners = append(owners, ownerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate affected activity history owners: %w", err)
	}
	return owners, nil
}

func (controller *adminCtlController) disableOne(
	ctx context.Context,
	ownerID uuid.UUID,
) (result adminCtlOwnerResult, err error) {
	err = controller.withSender(ctx, ownerID, func() error {
		var transactionErr error
		result, transactionErr = controller.disableOneTransaction(ctx, ownerID)
		return transactionErr
	})
	return result, err
}

func (controller *adminCtlController) disableOneTransaction(
	ctx context.Context,
	ownerID uuid.UUID,
) (result adminCtlOwnerResult, returnErr error) {
	tx, err := controller.service.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	if tx == nil {
		return result, errors.New("activity history disable transaction unavailable")
	}
	defer tx.Rollback() //nolint:errcheck
	committed := false
	defer func() {
		returnErr = controller.finishAdminCtlTransaction(tx, committed, returnErr)
	}()
	result, returnErr = controller.disableOwner(ctx, tx, ownerID)
	if returnErr != nil {
		return result, returnErr
	}
	if returnErr = controller.service.CommitTx(tx); returnErr != nil {
		return result, returnErr
	}
	committed = true
	return result, nil
}

func (controller *adminCtlController) finishAdminCtlTransaction(
	tx *sql.Tx,
	committed bool,
	operationErr error,
) error {
	if committed {
		return operationErr
	}
	rollbackErr := controller.service.RollbackTx(tx)
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return operationErr
	}
	return errors.Join(operationErr, fmt.Errorf("roll back activity history disable: %w", rollbackErr))
}

func disableAdminCtlOwner(
	ctx context.Context,
	tx *sql.Tx,
	ownerID uuid.UUID,
) (adminCtlOwnerResult, error) {
	if err := lockUser(ctx, tx, ownerID); err != nil {
		return adminCtlOwnerResult{}, err
	}
	settings, err := lockAdminCtlSettings(ctx, tx, ownerID)
	if err != nil {
		return adminCtlOwnerResult{}, err
	}
	pendingPresent, err := lockAdminCtlPending(ctx, tx, ownerID)
	if err != nil {
		return adminCtlOwnerResult{}, err
	}
	if err := lockAdminCtlHistory(ctx, tx, ownerID); err != nil {
		return adminCtlOwnerResult{}, err
	}
	forceOff := adminCtlMustForceOff(settings.markerPresent, pendingPresent)
	return clearAdminCtlOwner(ctx, tx, ownerID, settings.present, forceOff)
}

func lockAdminCtlSettings(ctx context.Context, tx *sql.Tx, ownerID uuid.UUID) (adminCtlSettingsLock, error) {
	var marker uuid.NullUUID
	err := tx.QueryRowContext(ctx, `
		SELECT presence_settings_operation_id
		FROM user_presence_settings
		WHERE user_id = $1
		FOR UPDATE
	`, ownerID).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return adminCtlSettingsLock{}, nil
	}
	if err != nil {
		return adminCtlSettingsLock{}, fmt.Errorf("lock activity history settings: %w", err)
	}
	return adminCtlSettingsLock{present: true, markerPresent: marker.Valid}, nil
}

func adminCtlMustForceOff(_, pendingPresent bool) bool {
	return pendingPresent
}

func lockAdminCtlPending(ctx context.Context, tx *sql.Tx, ownerID uuid.UUID) (bool, error) {
	var operationID uuid.UUID
	err := tx.QueryRowContext(ctx, `
		SELECT operation_id
		FROM presence_settings_pending_operations
		WHERE user_id = $1
		FOR UPDATE
	`, ownerID).Scan(&operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock pending activity history operation: %w", err)
	}
	return true, nil
}

func lockAdminCtlHistory(ctx context.Context, tx *sql.Tx, ownerID uuid.UUID) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id
		FROM presence_history
		WHERE sender_id = $1
		ORDER BY id
		FOR UPDATE
	`, ownerID)
	if err != nil {
		return fmt.Errorf("lock activity history rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var historyID uuid.UUID
		if err := rows.Scan(&historyID); err != nil {
			return fmt.Errorf("scan locked activity history row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate locked activity history rows: %w", err)
	}
	return nil
}

func clearAdminCtlOwner(
	ctx context.Context,
	tx *sql.Tx,
	ownerID uuid.UUID,
	settingsPresent bool,
	forceOff bool,
) (adminCtlOwnerResult, error) {
	var result adminCtlOwnerResult
	if settingsPresent {
		cleared, err := clearAdminCtlSettings(ctx, tx, ownerID, forceOff)
		if err != nil {
			return result, err
		}
		result.settingsCleared = cleared
	}
	historyResult, err := tx.ExecContext(ctx, `DELETE FROM presence_history WHERE sender_id = $1`, ownerID)
	if err != nil {
		return result, fmt.Errorf("delete activity history rows: %w", err)
	}
	history, err := historyResult.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("count deleted activity history rows: %w", err)
	}
	pendingResult, err := tx.ExecContext(ctx, `
		DELETE FROM presence_settings_pending_operations WHERE user_id = $1
	`, ownerID)
	if err != nil {
		return result, fmt.Errorf("delete pending activity history operation: %w", err)
	}
	pending, err := pendingResult.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("count deleted pending activity history operation: %w", err)
	}
	result.historyDeleted = history
	result.pendingDeleted = pending
	return result, nil
}

func clearAdminCtlSettings(
	ctx context.Context,
	tx *sql.Tx,
	ownerID uuid.UUID,
	forceOff bool,
) (int64, error) {
	if forceOff {
		return clearAdminCtlSettingsAndStatus(ctx, tx, ownerID)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET activity_history_enabled = FALSE,
		    activity_history_consent_version = NULL,
		    activity_history_consent_copy_hash = NULL,
		    activity_history_consented_at = NULL,
		    activity_history_reconsent_required = FALSE,
		    presence_settings_operation_id = NULL,
		    updated_at = clock_timestamp()
		WHERE user_id = $1
	`, ownerID)
	if err != nil {
		return 0, fmt.Errorf("clear activity history settings: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleared activity history settings: %w", err)
	}
	return rows, nil
}

func clearAdminCtlSettingsAndStatus(ctx context.Context, tx *sql.Tx, ownerID uuid.UUID) (int64, error) {
	result, err := tx.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET custom_text_tier = 0,
		    custom_text = NULL,
		    custom_text_emoji = NULL,
		    activity_history_enabled = FALSE,
		    activity_history_consent_version = NULL,
		    activity_history_consent_copy_hash = NULL,
		    activity_history_consented_at = NULL,
		    activity_history_reconsent_required = FALSE,
		    presence_settings_operation_id = NULL,
		    updated_at = clock_timestamp()
		WHERE user_id = $1
	`, ownerID)
	if err != nil {
		return 0, fmt.Errorf("clear pending activity history settings: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleared pending activity history settings: %w", err)
	}
	return rows, nil
}

func (controller *adminCtlController) assertZero(ctx context.Context) (adminCtlResidual, error) {
	if controller == nil || controller.countResidual == nil {
		return adminCtlResidual{}, errors.New("activity history zero assertion unavailable")
	}
	residual, err := controller.countResidual(ctx)
	if err != nil {
		return residual, err
	}
	if !residual.zero() {
		return residual, errors.New("activity history residual rows remain")
	}
	return residual, nil
}

func (controller *adminCtlController) readResidual(ctx context.Context) (adminCtlResidual, error) {
	var result adminCtlResidual
	err := controller.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM user_presence_settings WHERE activity_history_enabled = TRUE),
			(SELECT COUNT(*) FROM presence_history),
			(SELECT COUNT(*) FROM presence_settings_pending_operations)
	`).Scan(&result.enabled, &result.history, &result.pending)
	if err != nil {
		return result, fmt.Errorf("read activity history residual counts: %w", err)
	}
	return result, nil
}

func (controller *adminCtlController) preflight(ctx context.Context) (result adminCtlPreflightResult, returnErr error) {
	if controller == nil || controller.db == nil {
		return result, errors.New("activity history preflight database unavailable")
	}
	tx, err := controller.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, fmt.Errorf("begin activity history preflight: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("close activity history preflight: %w", rollbackErr))
		}
	}()
	result, returnErr = readAdminCtlPreflight(ctx, tx, controller.disclosure)
	if returnErr != nil {
		return result, returnErr
	}
	return result, validateAdminCtlPreflight(result)
}

func readAdminCtlPreflight(
	ctx context.Context,
	tx *sql.Tx,
	disclosure DisclosureState,
) (adminCtlPreflightResult, error) {
	var result adminCtlPreflightResult
	var migrationVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).
		Scan(&migrationVersion, &result.migrationDirty); err != nil {
		return result, fmt.Errorf("read activity history migration state: %w", err)
	}
	if migrationVersion < 0 {
		return result, errors.New("invalid activity history migration version")
	}
	result.migrationVersion = uint(migrationVersion)
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_name IN ('presence_history', 'presence_settings_pending_operations')
	`).Scan(&result.requiredTables); err != nil {
		return result, fmt.Errorf("read activity history required tables: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'user_presence_settings'
		  AND column_name IN (
			'presence_settings_version',
			'presence_settings_operation_id',
			'activity_history_enabled',
			'activity_history_retention_days',
			'activity_history_consent_version',
			'activity_history_consent_copy_hash',
			'activity_history_consented_at',
			'activity_history_reconsent_required'
		  )
	`).Scan(&result.requiredColumns); err != nil {
		return result, fmt.Errorf("read activity history required columns: %w", err)
	}
	if err := readAdminCtlPendingState(ctx, tx, &result); err != nil {
		return result, err
	}
	result.disclosureAvailable = disclosure.Available
	return result, nil
}

func readAdminCtlPendingState(ctx context.Context, tx *sql.Tx, result *adminCtlPreflightResult) error {
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE
			   settings.user_id IS NULL
			   OR settings.presence_settings_operation_id IS DISTINCT FROM pending.operation_id
			   OR settings.presence_settings_version <= 0
			   OR pending.prior_settings_version <> settings.presence_settings_version - 1
		       )
		FROM presence_settings_pending_operations pending
		LEFT JOIN user_presence_settings settings ON settings.user_id = pending.user_id
	`).Scan(&result.pendingRows, &result.inconsistentPending)
	if err != nil {
		return fmt.Errorf("read pending activity history readiness: %w", err)
	}
	return nil
}

func validateAdminCtlPreflight(result adminCtlPreflightResult) error {
	if result.migrationDirty || result.migrationVersion < 87 {
		return errors.New("activity history migration is not clean and current")
	}
	if result.requiredTables != adminCtlRequiredTables || result.requiredColumns != adminCtlRequiredColumns {
		return errors.New("activity history schema is incomplete")
	}
	if result.inconsistentPending != 0 {
		return errors.New("activity history pending operations are inconsistent")
	}
	return nil
}
