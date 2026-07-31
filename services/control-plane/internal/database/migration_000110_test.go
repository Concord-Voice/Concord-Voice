package database_test

import (
	"database/sql"
	"regexp"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var migration000110SourceColumn = regexp.MustCompile(`(?m)^[\t ]*source[\t ]+VARCHAR\(32\)[\t ]+NOT NULL[\t ]*\r?\n[\t ]*(CHECK \(source IN \([^)]+\)\)),[\t ]*$`)

func migration000110RequireStatement(t *testing.T, sql, statement string) {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^[\t ]*` + strings.ReplaceAll(regexp.QuoteMeta(statement), " ", `\s+`) + `[\t ]*$`)
	require.Regexp(t, pattern, sql)
}

func migration000110OriginalSourceCheck(t *testing.T, sql string) string {
	t.Helper()
	match := migration000110SourceColumn.FindStringSubmatch(sql)
	require.Len(t, match, 2)
	return match[1]
}

func migration000110InsertSource(t *testing.T, db *sql.DB, source string) error {
	t.Helper()
	userID := testhelpers.CreateUser(t, db)
	_, err := db.Exec(
		"INSERT INTO subscriptions (user_id, tier, status, source) VALUES ($1, 'premium', 'active', $2)",
		userID, source)
	return err
}

func TestMigration000110_TightensSubscriptionSource(t *testing.T) {
	up := migrationReadFile(t, "../../migrations/000110_tighten_subscription_source.up.sql")
	down := migrationReadFile(t, "../../migrations/000110_tighten_subscription_source.down.sql")
	original := migrationReadFile(t, "../../migrations/000070_subscriptions.up.sql")
	readme := migrationReadFile(t, "../../migrations/README.md")

	t.Run("migration text declares the narrowed and restored constraints", func(t *testing.T) {
		migration000110RequireStatement(t, up, "ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_source_check CHECK (source IN ('code', 'stripe'));")
		migration000110RequireStatement(t, down, "ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_source_check "+migration000110OriginalSourceCheck(t, original)+";")
		assert.Contains(t, readme, "| 000110 | tighten_subscription_source |")
	})

	t.Run("permitted sources are accepted", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		for _, source := range []string{"code", "stripe"} {
			require.NoError(t, migration000110InsertSource(t, db, source))
		}
	})

	// The value whose acceptance this migration actually changes. Asserting any
	// other rejected value (e.g. 'legacy') passes identically against the 000070
	// constraint, so it would prove nothing about the narrowing.
	t.Run("kickstarter is rejected after the narrowing", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		require.Error(t, migration000110InsertSource(t, db, "kickstarter"))
	})

	// Exercises the down migration rather than regex-matching its text, and with it
	// the up migration's fail-closed pre-flight guard — which is otherwise
	// unreachable, because SetupTestDB only ever migrates an empty database.
	t.Run("down widens, and up refuses to re-apply while a legacy row survives", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()

		// Always leave the shared test database on the narrowed constraint, even if
		// an assertion below fails partway through.
		t.Cleanup(func() {
			_, _ = db.Exec("DELETE FROM subscriptions WHERE source = 'kickstarter'")
			_, _ = db.Exec(up)
		})

		_, err := db.Exec(down)
		require.NoError(t, err, "down migration must re-widen the constraint")
		require.NoError(t, migration000110InsertSource(t, db, "kickstarter"),
			"down migration must restore acceptance of the legacy value")

		_, err = db.Exec(up)
		require.Error(t, err, "up migration must refuse while a kickstarter-sourced row exists")
		assert.Contains(t, err.Error(), "kickstarter-sourced row",
			"the guard must name the cause rather than surface Postgres's opaque constraint error")

		_, err = db.Exec("DELETE FROM subscriptions WHERE source = 'kickstarter'")
		require.NoError(t, err)

		_, err = db.Exec(up)
		require.NoError(t, err, "up migration must re-apply once the legacy rows are gone")
		require.Error(t, migration000110InsertSource(t, db, "kickstarter"))
	})
}
