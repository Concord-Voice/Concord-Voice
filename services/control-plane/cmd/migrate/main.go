// Package main provides a CLI tool for managing database migrations.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // register file source driver for side effects
	_ "github.com/joho/godotenv/autoload"                // register dotenv autoload for side effects
	_ "github.com/lib/pq"                                // register PostgreSQL driver for side effects
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

func main() {
	var (
		migrationsPath = flag.String("path", "migrations", "Path to migrations directory")
		steps          = flag.Int("steps", 0, "Number of migrations to run/rollback (0 = all)")
		command        = flag.String("command", "", "Migration command: up, down, force, version, create")
		migrationName  = flag.String("name", "", "Name for new migration (used with 'create' command)")
		forceVersion   = flag.Int("force-version", -1, "Version to force (used with 'force' command)")
	)
	flag.Parse()

	if *command == "" {
		printUsage()
		os.Exit(1)
	}

	if *command == "create" {
		handleCreateCommand(*migrationsPath, *migrationName)
		return
	}

	m := initMigrate(*migrationsPath)

	switch *command {
	case "up":
		runUp(m, *steps)
	case "down":
		runDown(m, *steps)
	case "force":
		runForce(m, *forceVersion)
	case "version":
		runVersion(m)
	default:
		log.Fatalf("Unknown command: %s", *command)
	}
}

func printUsage() {
	fmt.Println("Usage: migrate -command=<up|down|force|version|create> [options]")
	fmt.Println("\nCommands:")
	fmt.Println("  up      - Apply all pending migrations (or use -steps=N for specific count)")
	fmt.Println("  down    - Rollback migrations (use -steps=N, default=1)")
	fmt.Println("  force   - Force migration version (fixes dirty state). Use -force-version=N")
	fmt.Println("  version - Show current migration version")
	fmt.Println("  create  - Create new migration files (requires -name)")
	fmt.Println("\nOptions:")
	fmt.Println("  -path           Path to migrations directory (default: migrations)")
	fmt.Println("  -steps          Number of migrations to apply/rollback")
	fmt.Println("  -force-version  Version number to force (used with 'force' command)")
	fmt.Println("  -name           Name for new migration")
	fmt.Println("\nExamples:")
	fmt.Println("  migrate -command=up")
	fmt.Println("  migrate -command=down -steps=1")
	fmt.Println("  migrate -command=force -force-version=19")
	fmt.Println("  migrate -command=create -name=add_user_status")
}

func handleCreateCommand(path, name string) {
	if name == "" {
		log.Fatal("Migration name is required for 'create' command. Use -name flag.")
	}
	if err := createMigration(path, name); err != nil {
		log.Fatalf("Failed to create migration: %v", err)
	}
}

func initMigrate(migrationsPath string) *migrate.Migrate {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("Error closing database: %v", err)
		}
	}()

	if err := db.Ping(); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		log.Fatalf("Could not create migration driver: %v", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", migrationsPath),
		"postgres",
		driver,
	)
	if err != nil {
		log.Fatalf("Could not create migrate instance: %v", err)
	}

	return m
}

func runUp(m *migrate.Migrate, steps int) {
	var err error
	if steps > 0 {
		err = m.Steps(steps)
	} else {
		err = m.Up()
	}
	if err != nil && err != migrate.ErrNoChange {
		log.Fatalf("Migration failed: %v", err)
	}
	if err == migrate.ErrNoChange {
		fmt.Println("No migrations to apply")
	} else {
		fmt.Println("Migrations applied successfully")
	}
}

func runDown(m *migrate.Migrate, steps int) {
	if steps == 0 {
		steps = 1
	}
	err := m.Steps(-steps)
	if err != nil && err != migrate.ErrNoChange {
		log.Fatalf("Rollback failed: %v", err)
	}
	if err == migrate.ErrNoChange {
		fmt.Println("No migrations to rollback")
	} else {
		fmt.Printf("Rolled back %d migration(s) successfully\n", steps)
	}
}

func runForce(m *migrate.Migrate, forceVersion int) {
	if forceVersion < -1 {
		log.Fatal("Force requires -force-version=N (use -1 to reset to no version)")
	}
	if err := m.Force(forceVersion); err != nil {
		log.Fatalf("Force failed: %v", err)
	}
	if forceVersion == -1 {
		fmt.Println("Reset migration version to no version (clean)")
		return
	}
	fmt.Printf("Forced migration version to %d (clean)\n", forceVersion)
}

func runVersion(m *migrate.Migrate) {
	version, dirty, err := m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		log.Fatalf("Could not get version: %v", err)
	}
	if err == migrate.ErrNilVersion {
		fmt.Println("No migrations applied yet")
		return
	}
	status := "clean"
	if dirty {
		status = "dirty"
	}
	fmt.Printf("Current version: %d (%s)\n", version, status)
}

func nextMigrationVersion(path string) (int, error) {
	files, err := os.ReadDir(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("could not read migrations directory: %w", err)
	}

	if os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0750); err != nil {
			return 0, fmt.Errorf("could not create migrations directory: %w", err)
		}
		return 1, nil
	}

	version := 1
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(file.Name(), "%d_", &v); err == nil && v >= version {
			version = v + 1
		}
	}
	return version, nil
}

func createMigration(path, name string) error {
	version, err := nextMigrationVersion(path)
	if err != nil {
		return err
	}

	upFile := fmt.Sprintf("%s/%06d_%s.up.sql", path, version, name)
	downFile := fmt.Sprintf("%s/%06d_%s.down.sql", path, version, name)

	upContent := fmt.Sprintf("-- Migration: %s (up)\n-- Created: %s\n\n-- Add your UP migration here\n", name, "auto-generated")
	if err := os.WriteFile(upFile, []byte(upContent), 0600); err != nil {
		return fmt.Errorf("could not create up migration: %w", err)
	}

	downContent := fmt.Sprintf("-- Migration: %s (down)\n-- Created: %s\n\n-- Add your DOWN migration here (rollback)\n", name, "auto-generated")
	if err := os.WriteFile(downFile, []byte(downContent), 0600); err != nil {
		return fmt.Errorf("could not create down migration: %w", err)
	}

	fmt.Printf("Created migration files:\n")
	fmt.Printf("  - %s\n", upFile)
	fmt.Printf("  - %s\n", downFile)

	return nil
}
