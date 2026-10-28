package database_test

import (
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Column inventory for migration 000189 (#3680, spec H1): every column of the
// five credential tables is classified, and each classification is checked
// against the live trigger. A column added to a credential table later fails
// the first test until someone decides whether changing it is material.

type agColumnClass int

const (
	agMaterial agColumnClass = iota
	agNonMaterial
	agVersion
)

// agColumn classifies one column. Change is a SQL expression for a value that
// differs from the seeded fixture's and satisfies the column's constraints.
// It may use $2, the id of a second bare user (for user_id), or
// gen_random_uuid() (for unique columns). Version and Matures name the pair a
// users material column moves; empty elsewhere (material_version/matures_at).
type agColumn struct {
	Class            agColumnClass
	Change           string
	Version, Matures string
}

const (
	agLaterTS  = `'2002-01-01Z'`
	agEarlierT = `'2001-01-01Z'`
)

// factorCommon is the provenance block every factor table carries. It is
// written by the trigger or the runner: provenance, not credential material.
func agFactorCommon() map[string]agColumn {
	return map[string]agColumn{
		"matures_at":            {Class: agNonMaterial, Change: agLaterTS},
		"maturity_contested_at": {Class: agNonMaterial, Change: `now()`},
		"enrolled_at":           {Class: agNonMaterial, Change: agLaterTS},
		"enrol_class":           {Class: agNonMaterial, Change: `3`},
		"material_version":      {Class: agVersion, Change: `7`},
	}
}

func agMerge(base map[string]agColumn, extra map[string]agColumn) map[string]agColumn {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

var agInventory = map[string]map[string]agColumn{
	"users": {
		"password_hash": {Class: agMaterial, Change: `'y'`, Version: "password_version", Matures: "password_matures_at"},
		"email": {
			Class:   agMaterial,
			Change:  `'ag-changed-' || gen_random_uuid()::text || '@example.test'`,
			Version: "email_version", Matures: "email_matures_at",
		},
		"password_version": {Class: agVersion, Change: `7`},
		"email_version":    {Class: agVersion, Change: `7`},

		"id":                         {Class: agNonMaterial, Change: `gen_random_uuid()`},
		"username":                   {Class: agNonMaterial, Change: `'agx' || replace(gen_random_uuid()::text, '-', '')`},
		"email_verified":             {Class: agNonMaterial, Change: `TRUE`}, // spec §6.5: settings and mirrors, not material
		"age_verified":               {Class: agNonMaterial, Change: `TRUE`},
		"created_at":                 {Class: agNonMaterial, Change: agEarlierT},
		"updated_at":                 {Class: agNonMaterial, Change: agEarlierT},
		"display_name":               {Class: agNonMaterial, Change: `'dn'`},
		"bio":                        {Class: agNonMaterial, Change: `'bio'`},
		"avatar_url":                 {Class: agNonMaterial, Change: `'https://example.test/a.png'`},
		"links":                      {Class: agNonMaterial, Change: `'[{"label":"x"}]'::jsonb`},
		"header_image_url":           {Class: agNonMaterial, Change: `'https://example.test/h.png'`},
		"color_scheme":               {Class: agNonMaterial, Change: `'ocean'`},
		"revocation_mode":            {Class: agNonMaterial, Change: `'fast'`}, // varchar(10)
		"mfa_enabled":                {Class: agNonMaterial, Change: `TRUE`},
		"mfa_methods":                {Class: agNonMaterial, Change: `ARRAY['totp']`}, // spec §6.5: mirror, not material
		"recovery_only_methods":      {Class: agNonMaterial, Change: `ARRAY['totp']`},
		"recovery_hardened":          {Class: agNonMaterial, Change: `FALSE`},
		"backup_email":               {Class: agNonMaterial, Change: `'b@example.test'`}, // C5's user_backup_addresses owns backup-address maturity
		"mfa_enabled_at":             {Class: agNonMaterial, Change: `now()`},
		"email_verification_sent_at": {Class: agNonMaterial, Change: `now()`},
		"username_changed_at":        {Class: agNonMaterial, Change: agEarlierT},
		"password_login_disabled":    {Class: agNonMaterial, Change: `TRUE`}, // spec §6.5: a setting, not material
		"trust_sso_security":         {Class: agNonMaterial, Change: `TRUE`},
		"disabled":                   {Class: agNonMaterial, Change: `TRUE`},
		"disabled_reason":            {Class: agNonMaterial, Change: `'r'`},
		"disabled_at":                {Class: agNonMaterial, Change: `now()`},
		"ops_last_active_at":         {Class: agNonMaterial, Change: `now()`},
		"credential_epoch":           {Class: agNonMaterial, Change: `'epoch-x'`}, // spec H1: never material
		"token_theft_generation":     {Class: agNonMaterial, Change: `5`},
		"password_matures_at":        {Class: agNonMaterial, Change: agLaterTS}, // provenance, not credential material
		"email_matures_at":           {Class: agNonMaterial, Change: agLaterTS},
		"email_contested_at":         {Class: agNonMaterial, Change: `now()`},
		"password_enrolled_at":       {Class: agNonMaterial, Change: agLaterTS},
		"email_enrolled_at":          {Class: agNonMaterial, Change: agLaterTS},
		"password_enrol_class":       {Class: agNonMaterial, Change: `3`},
		"email_enrol_class":          {Class: agNonMaterial, Change: `3`},
	},
	"user_mfa_totp": agMerge(agFactorCommon(), map[string]agColumn{
		"user_id":           {Class: agMaterial, Change: `$2`},
		"totp_secret_enc":   {Class: agMaterial, Change: `decode('99aa', 'hex')`},
		"totp_secret_nonce": {Class: agMaterial, Change: `decode('99bb', 'hex')`},
		"enabled":           {Class: agMaterial, Change: `TRUE`},
		"confirmed":         {Class: agMaterial, Change: `TRUE`},
		"backup_codes_hash": {Class: agNonMaterial, Change: `ARRAY['h']`}, // H4-a: a backup set gets its own maturity in PR6's user_backup_code_sets
		"backup_codes_used": {Class: agNonMaterial, Change: `ARRAY[TRUE]`},
		"created_at":        {Class: agNonMaterial, Change: agEarlierT},
		"verified_at":       {Class: agNonMaterial, Change: `now()`},
		"confirmed_at":      {Class: agNonMaterial, Change: `now()`},
		"updated_at":        {Class: agNonMaterial, Change: agEarlierT},
		"key_version":       {Class: agNonMaterial, Change: `2`}, // a rewrap, exempted by the rewrap flag only
		"last_used_step":    {Class: agNonMaterial, Change: `5`},
	}),
	"user_mfa_webauthn": agMerge(agFactorCommon(), map[string]agColumn{
		"id":              {Class: agMaterial, Change: `gen_random_uuid()`},
		"user_id":         {Class: agMaterial, Change: `$2`},
		"credential_id":   {Class: agMaterial, Change: `convert_to(gen_random_uuid()::text, 'UTF8')`},
		"public_key":      {Class: agMaterial, Change: `decode('88aa', 'hex')`},
		"aaguid":          {Class: agNonMaterial, Change: `decode('01', 'hex')`},
		"sign_count":      {Class: agNonMaterial, Change: `5`},
		"credential_name": {Class: agNonMaterial, Change: `'n'`},
		"credential_type": {Class: agNonMaterial, Change: `'platform'`},
		"transports":      {Class: agNonMaterial, Change: `ARRAY['usb']`},
		"created_at":      {Class: agNonMaterial, Change: agEarlierT},
		"last_used_at":    {Class: agNonMaterial, Change: `now()`},
	}),
	"user_sso_identities": agMerge(agFactorCommon(), map[string]agColumn{
		"id":               {Class: agMaterial, Change: `gen_random_uuid()`},
		"user_id":          {Class: agMaterial, Change: `$2`},
		"provider":         {Class: agMaterial, Change: `'github'`},
		"provider_user_id": {Class: agMaterial, Change: `'sub-new-' || gen_random_uuid()::text`},
		"provider_email":   {Class: agNonMaterial, Change: `'c@example.test'`},
		"is_relay_email":   {Class: agNonMaterial, Change: `TRUE`},
		"created_at":       {Class: agNonMaterial, Change: agEarlierT},
		"last_used_at":     {Class: agNonMaterial, Change: `now()`},
	}),
	"user_recovery_keys": agMerge(agFactorCommon(), map[string]agColumn{
		"user_id":                      {Class: agMaterial, Change: `$2`},
		"recovery_wrapped_private_key": {Class: agMaterial, Change: `decode('aa', 'hex')`},
		"recovery_key_salt":            {Class: agMaterial, Change: `decode('ab', 'hex')`},
		"recovery_wrapped_prefs_key":   {Class: agMaterial, Change: `decode('ac', 'hex')`},
		"recovery_prefs_key_salt":      {Class: agMaterial, Change: `decode('ad', 'hex')`},
		"created_at":                   {Class: agNonMaterial, Change: agEarlierT},
		"updated_at":                   {Class: agNonMaterial, Change: agEarlierT},
	}),
}

func agSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestMigration000189_CredentialColumnsAreAllClassified(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)

	for _, table := range agSortedKeys(agInventory) {
		rows, err := tx.Query(`
			SELECT column_name FROM information_schema.columns
			 WHERE table_schema = current_schema() AND table_name = $1::text`, table)
		require.NoError(t, err)
		actual := map[string]bool{}
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			actual[name] = true
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())

		var unclassified, stale []string
		for name := range actual {
			if _, ok := agInventory[table][name]; !ok {
				unclassified = append(unclassified, name)
			}
		}
		for name := range agInventory[table] {
			if !actual[name] {
				stale = append(stale, name)
			}
		}
		sort.Strings(unclassified)
		sort.Strings(stale)
		assert.Empty(t, unclassified, "%s: columns with no classification; decide whether changing each is material (spec H1)", table)
		assert.Empty(t, stale, "%s: inventory names that are not columns", table)
	}
}

// agCredsOf returns the credentials stored in table (two for users, one otherwise).
func agCredsOf(table string) []agCred {
	var out []agCred
	for _, c := range agCreds {
		if c.Table == table {
			out = append(out, c)
		}
	}
	return out
}

// agIsStateColumn reports whether the column is a maturity or provenance column,
// whose new value the test can compare to Change deterministically.
func agIsStateColumn(col string) bool {
	return strings.HasSuffix(col, "matures_at") || strings.HasSuffix(col, "enrolled_at") || strings.HasSuffix(col, "enrol_class")
}

func TestMigration000189_MaterialClassificationMatchesTrigger(t *testing.T) {
	f := migration000189Seeded(t)
	now, stamp := agNow(t, f.Tx), agStamp(t, f.Tx)

	for _, table := range agSortedKeys(agInventory) {
		creds := agCredsOf(table)
		// users.id cannot move on a user with factor rows, so the users cases
		// run on the bare user.
		key := f.User
		if table == "users" {
			key = f.Bare
		}
		for _, col := range agSortedKeys(agInventory[table]) {
			spec := agInventory[table][col]
			t.Run(table+"."+col, func(t *testing.T) {
				agSavepoint(t, f.Tx, func() {
					// RETURNING sees the new row, so a column that moves the key
					// (user_id, id) is still read correctly.
					var cols []string
					for _, c := range creds {
						cols = append(cols, agStateCols(c))
					}
					eq := ""
					if agIsStateColumn(col) {
						eq = ", " + col + " IS NOT DISTINCT FROM (" + spec.Change + ")"
					}
					q := agSQL(`UPDATE %s SET %s = %s WHERE %s = $1 AND $2::uuid IS NOT NULL RETURNING %s%s`,
						table, col, spec.Change, creds[0].Key, strings.Join(cols, ", "), eq)

					states := make([]agState, len(creds))
					var targets []any
					for i := range states {
						targets = append(targets, states[i].scanTargets()...)
					}
					var changed bool
					if eq != "" {
						targets = append(targets, &changed)
					}
					require.NoError(t, f.Tx.QueryRow(q, key, f.Other).Scan(targets...))
					if eq != "" {
						assert.True(t, changed, "the write must have taken effect")
					}

					for i, c := range creds {
						moved := spec.Class == agMaterial && agMovesCred(table, spec, c)
						s := states[i]
						if moved {
							agMaterialVersion(t, c, s.Version, f.Base[c.Name])
							agRequireTime(t, stamp, s.Matures, c.Name+" matures_at")
							agRequireTime(t, now, s.Enrolled, c.Name+" enrolled_at")
							assert.False(t, s.Class.Valid, "%s enrol_class", c.Name)
							continue
						}
						assert.Equal(t, f.Base[c.Name], s.Version, "%s version must not move", c.Name)
						if col != c.Matures {
							agRequireTime(t, agLegacyMaturity, s.Matures, c.Name+" matures_at")
						}
						if col != c.Enrolled {
							agRequireTime(t, agLegacyMaturity, s.Enrolled, c.Name+" enrolled_at")
						}
						if col != c.Class {
							assert.Equal(t, sql.NullInt64{Int64: 2, Valid: true}, s.Class, "%s enrol_class", c.Name)
						}
					}
				})
			})
		}
	}
}

// agMovesCred reports whether a material write to the column moves credential c:
// every credential of a factor table, and the named pair on users.
func agMovesCred(table string, spec agColumn, c agCred) bool {
	if table != "users" {
		return true
	}
	return c.Version == spec.Version
}
