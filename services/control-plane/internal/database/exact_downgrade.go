package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratedatabase "github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Register the fixed file:// migration source.
	_ "github.com/lib/pq"                                // Register the dedicated PostgreSQL connection driver.
)

const (
	exactDowngradeSourceURL = "file://migrations"
	exactDowngradeFrom      = 87
	exactDowngradeTo        = 86
	postgresPoolOwner       = "PostgreSQL pool"
)

var (
	errExactDowngradeStartState = errors.New("exact downgrade requires clean migration version 87")
	errExactDowngradeFinalState = errors.New("exact downgrade did not finish at clean migration version 86")
)

type exactMigration interface {
	Steps(int) error
	Close() (source error, database error)
}

type exactDowngradeDependencies struct {
	openDatabase func(string) (migratedatabase.Driver, error)
	openSource   func(string) (source.Driver, error)
	newMigrate   func(source.Driver, migratedatabase.Driver) (exactMigration, error)
}

type exactVersion87Driver struct {
	migratedatabase.Driver
}

type ownedMigrationDatabase struct {
	migratedatabase.Driver
	pool *sql.DB
}

// ExactDowngradeMigration87 opens an isolated migration connection, applies
// only migration 87's down file, and verifies the resulting clean version 86.
func ExactDowngradeMigration87(databaseURL string) error {
	return runExactDowngradeMigration87(databaseURL, exactDowngradeDependencies{
		openDatabase: openDedicatedMigrationDatabase,
		openSource:   source.Open,
		newMigrate:   newExactMigration,
	})
}

func runExactDowngradeMigration87(databaseURL string, deps exactDowngradeDependencies) (resultErr error) {
	databaseDriver, err := deps.openDatabase(databaseURL)
	if err != nil {
		return fmt.Errorf("open dedicated migration database: %w", err)
	}

	sourceDriver, err := deps.openSource(exactDowngradeSourceURL)
	if err != nil {
		return errors.Join(
			fmt.Errorf("open exact migration source: %w", err),
			exactCloseError("database", databaseDriver.Close()),
		)
	}

	guardedDriver := &exactVersion87Driver{Driver: databaseDriver}
	migration, err := deps.newMigrate(sourceDriver, guardedDriver)
	if err != nil {
		return errors.Join(
			fmt.Errorf("construct exact migration runner: %w", err),
			exactCloseError("source", sourceDriver.Close()),
			exactCloseError("database", databaseDriver.Close()),
		)
	}
	defer func() {
		sourceErr, databaseErr := migration.Close()
		resultErr = errors.Join(
			resultErr,
			exactCloseError("source", sourceErr),
			exactCloseError("database", databaseErr),
		)
	}()

	if err := migration.Steps(-1); err != nil {
		return fmt.Errorf("run exact migration 87 downgrade: %w", err)
	}
	return verifyExactDowngrade(databaseDriver)
}

func (d *exactVersion87Driver) Version() (int, bool, error) {
	version, dirty, err := d.Driver.Version()
	if err != nil {
		return version, dirty, errors.Join(errExactDowngradeStartState, fmt.Errorf("read migration version: %w", err))
	}
	if dirty || version != exactDowngradeFrom {
		return version, dirty, errExactDowngradeStartState
	}
	return version, false, nil
}

func verifyExactDowngrade(driver migratedatabase.Driver) error {
	version, dirty, err := driver.Version()
	if err != nil {
		return errors.Join(errExactDowngradeFinalState, fmt.Errorf("read migration version: %w", err))
	}
	if dirty || version != exactDowngradeTo {
		return errExactDowngradeFinalState
	}
	return nil
}

func newExactMigration(sourceDriver source.Driver, databaseDriver migratedatabase.Driver) (exactMigration, error) {
	return migrate.NewWithInstance("file", sourceDriver, "postgres", databaseDriver)
}

func openDedicatedMigrationDatabase(databaseURL string) (migratedatabase.Driver, error) {
	pool, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", postgresPoolOwner, err)
	}

	ctx := context.Background()
	connection, err := pool.Conn(ctx)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("open dedicated PostgreSQL connection: %w", err),
			exactCloseError(postgresPoolOwner, pool.Close()),
		)
	}

	driver, err := postgres.WithConnection(ctx, connection, &postgres.Config{})
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("construct PostgreSQL migration driver: %w", err),
			exactCloseError("PostgreSQL connection", connection.Close()),
			exactCloseError(postgresPoolOwner, pool.Close()),
		)
	}
	return &ownedMigrationDatabase{Driver: driver, pool: pool}, nil
}

func (d *ownedMigrationDatabase) Close() error {
	return errors.Join(
		exactCloseError("PostgreSQL migration driver", d.Driver.Close()),
		exactCloseError(postgresPoolOwner, d.pool.Close()),
	)
}

func exactCloseError(owner string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close exact downgrade %s: %w", owner, err)
}
