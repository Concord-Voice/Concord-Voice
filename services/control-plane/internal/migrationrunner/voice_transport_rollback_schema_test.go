package migrationrunner_test

import (
	"database/sql"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Regression for #2624: migration 167 must downgrade the schema where its up
// ran, including the provisioning refusal, rather than another schema.
func TestVoiceTransportRollback_RespectsCurrentSchema(t *testing.T) {
	for _, schema := range []string{"public", "non_public", "non_public_without_public_receipt"} {
		for _, provisioned := range []bool{false, true} {
			state := "unprovisioned"
			if provisioned {
				state = "provisioned"
			}
			t.Run(schema+"/"+state, func(t *testing.T) {
				var db *sql.DB
				if schema == "public" {
					db, _ = testhelpers.SetupTestDB(t)
				} else {
					db = freshSchemaDB(t)
				}

				// The real files are each executed whole, matching the runner's
				// default simple-query transaction shape. The outer transaction
				// restores the shared public fixture even if a buggy down targets it.
				tx, err := db.Begin()
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, tx.Rollback()) })
				var current string
				require.NoError(t, tx.QueryRow(`SELECT current_schema()`).Scan(&current))
				// SetupTestDB installs the latest public schema. Restore this
				// historical 167 fixture inside the outer rollback transaction:
				// empty 169/170 additive FKs must not decide the 167 rollback outcome.
				voiceTransportHistorical167Boundary(t, tx)
				if schema == "public" {
					require.Equal(t, "public", current)
				} else {
					require.NotEqual(t, "public", current, "the regression must reach an active non-public schema")
					if schema == "non_public_without_public_receipt" {
						_, err = tx.Exec(`DROP TABLE public.voice_command_transport;
							DROP FUNCTION public.protect_voice_command_transport_receipt();`)
						require.NoError(t, err, "the absent-public control must remove only its transaction-local fixture")
					}
					_, err = tx.Exec(voiceTransportMigrationSQL(t, "up"))
					require.NoError(t, err, "the real up must create a valid receipt in the active schema")
				}

				require.True(t, voiceTransportObjectExists(t, tx, "table"), "up must have created the receipt table")
				require.True(t, voiceTransportObjectExists(t, tx, "function"), "up must have created the protection function")
				var receiptCount int
				var initiallyProvisioned bool
				require.NoError(t, tx.QueryRow(`SELECT COUNT(*), COALESCE(BOOL_OR(provisioned_at IS NOT NULL), FALSE) FROM voice_command_transport`).Scan(&receiptCount, &initiallyProvisioned))
				require.Equal(t, 1, receiptCount, "the real up must seed its singleton receipt")
				require.False(t, initiallyProvisioned, "the control fixture must start unprovisioned")
				if provisioned {
					result, updateErr := tx.Exec(`UPDATE voice_command_transport SET
						provisioned_at = '2026-10-14T00:00:00Z',
						stream_created_at = '2026-10-14T00:00:00.123456789Z',
						owner_store_created_at = '2026-10-14T00:00:00.987654321Z',
						initializing_revision = 7 WHERE id = TRUE`)
					require.NoError(t, updateErr, "the provisioned fixture must satisfy the actual proof constraints")
					affected, affectedErr := result.RowsAffected()
					require.NoError(t, affectedErr)
					require.Equal(t, int64(1), affected, "the guard fixture must contain a provisioned receipt")
				}
				before := voiceTransportReceipt(t, tx)
				_, err = tx.Exec(`SAVEPOINT before_voice_transport_down`)
				require.NoError(t, err)
				_, downErr := tx.Exec(voiceTransportMigrationSQL(t, "down"))
				if provisioned {
					assert.ErrorContains(t, downErr, "cannot drop voice_command_transport after durable transport provisioning", "rollback must refuse the provisioned receipt in the active schema")
					if downErr != nil {
						var pgErr *pq.Error
						require.ErrorAs(t, downErr, &pgErr)
						assert.Equal(t, "P0001", string(pgErr.Code), "the refusal must come from the migration guard")
						_, err = tx.Exec(`ROLLBACK TO SAVEPOINT before_voice_transport_down`)
						require.NoError(t, err)
					}
					require.True(t, voiceTransportObjectExists(t, tx, "table"), "refused rollback must preserve the active receipt table")
					assert.True(t, voiceTransportObjectExists(t, tx, "function"), "refused rollback must preserve the active protection function")
					assert.Equal(t, before, voiceTransportReceipt(t, tx), "refused rollback must preserve every receipt field")
				} else {
					require.NoError(t, downErr, "rollback must allow an unprovisioned receipt")
					assert.False(t, voiceTransportObjectExists(t, tx, "table"), "rollback must remove the unprovisioned receipt table in the active schema")
					assert.False(t, voiceTransportObjectExists(t, tx, "function"), "rollback must remove the protection function in the active schema")
				}
				if schema != "public" {
					var publicReceiptExists, publicFunctionExists bool
					require.NoError(t, tx.QueryRow(`SELECT
						to_regclass('public.voice_command_transport') IS NOT NULL,
						to_regprocedure('public.protect_voice_command_transport_receipt()') IS NOT NULL`).Scan(&publicReceiptExists, &publicFunctionExists))
					publicExpected := schema == "non_public"
					assert.Equal(t, publicExpected, publicReceiptExists, "a non-public rollback must preserve the unrelated public table state")
					assert.Equal(t, publicExpected, publicFunctionExists, "a non-public rollback must preserve the unrelated public function state")
				}
			})
		}
	}
}

// This fixture removes only migrations 170 and 169's empty additive objects,
// using their actual guarded down files. The public 167 receipt remains unchanged, and the
// original search_path is restored before the schema-specific regression runs.
// The caller's outer rollback restores the latest shared schema after each cell.
func voiceTransportHistorical167Boundary(t *testing.T, tx *sql.Tx) {
	t.Helper()
	var schemaBefore, searchPathBefore, publicReceiptBefore string
	require.NoError(t, tx.QueryRow(`SELECT current_schema(), current_setting('search_path')`).Scan(&schemaBefore, &searchPathBefore))
	require.NoError(t, tx.QueryRow(`SELECT to_jsonb(receipt)::text FROM public.voice_command_transport AS receipt WHERE id = TRUE`).Scan(&publicReceiptBefore))
	_, err := tx.Exec(`SET LOCAL search_path TO public`)
	require.NoError(t, err)
	dir := strings.TrimPrefix(controlPlaneMigrations(t), "file://")
	for _, name := range []string{"000170_add_voice_command_capacity.down.sql", "000169_add_voice_command_outbox.down.sql"} {
		contents, readErr := fs.ReadFile(os.DirFS(dir), name)
		require.NoError(t, readErr)
		require.NotEmpty(t, contents, "historical setup must execute the actual guarded migration")
		_, err = tx.Exec(string(contents))
		require.NoError(t, err, "the historical fixture requires empty evidence and never-used cursors")
	}
	var publicReceiptAfter string
	require.NoError(t, tx.QueryRow(`SELECT to_jsonb(receipt)::text FROM public.voice_command_transport AS receipt WHERE id = TRUE`).Scan(&publicReceiptAfter))
	assert.Equal(t, publicReceiptBefore, publicReceiptAfter, "historical setup must preserve every public 167 receipt field")
	_, err = tx.Exec(`SELECT set_config('search_path', $1, true)`, searchPathBefore)
	require.NoError(t, err)
	var schemaAfter string
	require.NoError(t, tx.QueryRow(`SELECT current_schema()`).Scan(&schemaAfter))
	require.Equal(t, schemaBefore, schemaAfter, "historical setup must restore the active schema")
}

func voiceTransportMigrationSQL(t *testing.T, direction string) string {
	t.Helper()
	dir := strings.TrimPrefix(controlPlaneMigrations(t), "file://")
	name := "000167_add_voice_command_transport.down.sql"
	if direction == "up" {
		name = "000167_add_voice_command_transport.up.sql"
	}
	contents, err := fs.ReadFile(os.DirFS(dir), name)
	require.NoError(t, err)
	require.NotEmpty(t, contents, "the integration test must execute the actual migration")
	return string(contents)
}

func voiceTransportObjectExists(t *testing.T, tx *sql.Tx, kind string) bool {
	t.Helper()
	query := `SELECT to_regclass(quote_ident(current_schema()) || '.voice_command_transport') IS NOT NULL`
	if kind == "function" {
		query = `SELECT to_regprocedure(quote_ident(current_schema()) || '.protect_voice_command_transport_receipt()') IS NOT NULL`
	}
	var exists bool
	require.NoError(t, tx.QueryRow(query).Scan(&exists))
	return exists
}

func voiceTransportReceipt(t *testing.T, tx *sql.Tx) string {
	t.Helper()
	var snapshot string
	require.NoError(t, tx.QueryRow(`SELECT to_jsonb(receipt)::text FROM voice_command_transport AS receipt WHERE id = TRUE`).Scan(&snapshot))
	return snapshot
}
