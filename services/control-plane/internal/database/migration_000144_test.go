package database_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000144 (#2869) replaces the existence-only member_roles.role_id FK
// with a composite (role_id, server_id) FK into roles(id, server_id), so a
// membership can only hold a role owned by the SAME server. It also pins five
// permission bitfield columns non-negative: the sign bit is not a permission, and
// Go, SQL and the client's BigInt each read it differently.
//
// The constraint tests below cover the durable guarantee: the schema refuses
// those states continuously, not just at migrate time. The up migration's
// guard blocks and its deny normalisation are exercised separately, by
// TestMigration000144_UpGuardsAndDenyNormalisation, which runs the down
// migration to reconstruct the pre-migration schema (the migration_000110
// pattern).
//
// Every refusal is paired with a CONTROL that must succeed. A constraint that
// rejected everything would satisfy the refusal assertions alone, so the
// controls are what make a pass meaningful rather than vacuous.
func TestMigration000144_CrossServerRoleAssignment(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	t.Run("the composite unique and FK constraints exist", func(t *testing.T) {
		for _, tc := range []struct {
			conname string
			want    int
			why     string
		}{
			{"roles_id_server_key", 1, "migration 000144 must add the FK's unique target"},
			{"member_roles_role_server_fkey", 1, "migration 000144 must add the composite FK"},
			{"member_roles_role_id_fkey", 0, "migration 000144 must drop the existence-only FK"},
		} {
			var n int
			require.NoError(t, ts.DB.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM pg_constraint WHERE conname = $1`, tc.conname).Scan(&n))
			assert.Equal(t, tc.want, n, tc.why)
		}
	})

	owner := ts.CreateTestUser(t, "mig144owner")
	serverA := ts.CreateTestServer(t, owner.ID, "Migration 000144 Server A")
	serverB := ts.CreateTestServer(t, owner.ID, "Migration 000144 Server B")

	// A very high-position role owned by server B. Its permission bits are zero:
	// the position alone was enough to exploit the gap, because before #2869 the
	// hierarchy ceiling was MAX(position) over role_id with no server predicate,
	// so importing this row into server A lifted the member's ceiling there.
	foreignRole := ts.CreateTestRole(t, serverB, "Foreign High", 999, 0)
	homeRole := ts.CreateTestRole(t, serverA, "Home Role", 1, 0)
	otherHomeRole := ts.CreateTestRole(t, serverA, "Other Home Role", 2, 0)

	t.Run("CONTROL: a same-server assignment is accepted", func(t *testing.T) {
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
			serverA, owner.ID, homeRole)
		require.NoError(t, err,
			"the composite FK must not refuse a legitimate same-server assignment")
	})

	t.Run("the composite FK rejects a role from a foreign server", func(t *testing.T) {
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
			serverA, owner.ID, foreignRole)
		require.Error(t, err,
			"a server-A membership naming a server-B role must violate member_roles_role_server_fkey")
		assert.Contains(t, err.Error(), "member_roles_role_server_fkey",
			"the refusal must come from the composite FK, not from some other constraint that happens to fire first")
	})

	t.Run("the composite FK also rejects an UPDATE into a foreign role", func(t *testing.T) {
		// The INSERT path is the one the API exercises, but a composite FK that
		// only covered inserts would leave the state reachable by a later write.
		//
		// The `role_id = $4` predicate is load-bearing, not redundant. The owner
		// also holds the @all default role CreateTestServer assigns, so matching
		// on (server_id, user_id) alone updates BOTH rows to the same role_id and
		// collides on UNIQUE (server_id, user_id, role_id) -- which fails the
		// statement before the FK is ever consulted. That earlier form was
		// refused, so a bare require.Error passed while asserting nothing about
		// the constraint this test exists to exercise. The Contains check below
		// is what caught it; do not weaken either one.
		_, err := ts.DB.ExecContext(ctx,
			`UPDATE member_roles SET role_id = $1 WHERE server_id = $2 AND user_id = $3 AND role_id = $4`,
			foreignRole, serverA, owner.ID, homeRole)
		require.Error(t, err, "repointing an existing assignment at a foreign role must be refused")
		assert.Contains(t, err.Error(), "member_roles_role_server_fkey",
			"must be refused by the composite FK, not by the unique constraint on a multi-row update")
	})

	t.Run("CONTROL: an UPDATE into another same-server role is accepted", func(t *testing.T) {
		// Same statement shape as the refusal above, differing only in whose role
		// the row is repointed at. Without it, an FK that refused every UPDATE of
		// role_id would pass the refusal case.
		res, err := ts.DB.ExecContext(ctx,
			`UPDATE member_roles SET role_id = $1 WHERE server_id = $2 AND user_id = $3 AND role_id = $4`,
			otherHomeRole, serverA, owner.ID, homeRole)
		require.NoError(t, err, "repointing an assignment at a role on the same server must be allowed")
		n, err := res.RowsAffected()
		require.NoError(t, err)
		assert.Equal(t, int64(1), n, "the control must actually move a row, or it proves nothing")
	})

	t.Run("deleting the role still cascades the assignment away", func(t *testing.T) {
		// ON DELETE CASCADE is carried over from the FK 000144 replaced. Losing it
		// would strand assignment rows pointing at a role that no longer exists --
		// a silent behaviour change the constraint-existence check above cannot see.
		cascadeRole := ts.CreateTestRole(t, serverA, "Cascade Role", 3, 0)
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
			serverA, owner.ID, cascadeRole)
		require.NoError(t, err)

		var before, after int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM member_roles WHERE role_id = $1`, cascadeRole).Scan(&before))
		require.Equal(t, 1, before, "control: the assignment exists before the role is deleted")

		_, err = ts.DB.ExecContext(ctx, `DELETE FROM roles WHERE id = $1`, cascadeRole)
		require.NoError(t, err)

		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM member_roles WHERE role_id = $1`, cascadeRole).Scan(&after))
		assert.Equal(t, 0, after, "deleting the role must still cascade the assignment away")
	})
}

// Bit 63 is not a permission, and bit 62 is PermAdministrator. -1 sets both, so
// a -1 anywhere in the actor's BIT_OR makes `Permission(req) &^ actorPerms` zero
// for every request and opens the escalation subset check. The CHECKs refuse the
// sign bit on all five bitfield columns: constraining some would leave exactly
// the asymmetry #2869 exists to remove.
func TestMigration000144_PermissionBitfieldsRejectNegatives(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "mig144negowner")
	server := ts.CreateTestServer(t, owner.ID, "Migration 000144 Negative Server")
	role := ts.CreateTestRole(t, server, "Ordinary", 1, 0)

	t.Run("CONTROL: a non-negative role bitfield is accepted", func(t *testing.T) {
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO roles (server_id, name, position, permissions) VALUES ($1, 'Positive', 3, $2)`,
			server, int64(math.MaxInt64))
		require.NoError(t, err,
			"the CHECK must accept every non-negative value, including all 63 permission bits")
	})

	t.Run("a negative roles.permissions is rejected", func(t *testing.T) {
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO roles (server_id, name, position, permissions) VALUES ($1, 'Negative', 4, -1)`,
			server)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "roles_permissions_non_negative")
	})

	// Each override case needs its OWN parent: both override tables are
	// UNIQUE (parent, target_type, target_id), so reusing one would make the
	// second insert fail on the unique constraint and pass the CHECK assertion
	// for the wrong reason.
	newChannel := func(t *testing.T, name string) string { return ts.CreateTestChannel(t, server, name) }
	newCategory := func(t *testing.T, name string) string {
		var id string
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`INSERT INTO channel_groups (server_id, name) VALUES ($1, $2) RETURNING id`, server, name).Scan(&id))
		return id
	}
	for _, table := range []struct {
		name, insert string
		newParent    func(*testing.T, string) string
	}{
		{"channel_permission_overrides",
			`INSERT INTO channel_permission_overrides (channel_id, target_type, target_id, allow, deny) VALUES ($1, 'role', $2, $3, $4)`,
			newChannel},
		{"category_permission_overrides",
			`INSERT INTO category_permission_overrides (category_id, target_type, target_id, allow, deny) VALUES ($1, 'role', $2, $3, $4)`,
			newCategory},
	} {
		for _, tc := range []struct {
			name        string
			allow, deny int64
			constraint  string // empty = CONTROL, must be accepted
		}{
			{"CONTROL: a non-negative override is accepted", math.MaxInt64, math.MaxInt64, ""},
			{"a negative allow is rejected", -1, 0, table.name + "_allow_non_negative"},
			{"a negative deny is rejected", 0, -1, table.name + "_deny_non_negative"},
		} {
			t.Run(table.name+": "+tc.name, func(t *testing.T) {
				_, err := ts.DB.ExecContext(ctx, table.insert, table.newParent(t, tc.name), role, tc.allow, tc.deny)
				if tc.constraint == "" {
					require.NoError(t, err)
					return
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.constraint)
			})
		}
	}
}

// The up migration refuses to proceed over a cross-server assignment or a
// negative grant, and normalises a negative deny. None of those states can
// exist after SetupTestServer migrates an empty database, so this test runs the
// down migration to restore the pre-000144 schema, seeds each state, and runs
// the up migration against it -- the migration_000110 pattern.
//
// Each refusal case is followed by the same up migration succeeding once that
// one row is repaired, which is what shows the refusal was caused by the row
// the case names rather than by anything else in the fixture.
func TestMigration000144_UpGuardsAndDenyNormalisation(t *testing.T) {
	up := migrationReadFile(t, "../../migrations/000144_member_roles_server_composite_fk.up.sql")
	down := migrationReadFile(t, "../../migrations/000144_member_roles_server_composite_fk.down.sql")

	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "mig144guardowner")
	serverA := ts.CreateTestServer(t, owner.ID, "Migration 000144 Guard A")
	serverB := ts.CreateTestServer(t, owner.ID, "Migration 000144 Guard B")
	homeRole := ts.CreateTestRole(t, serverA, "Guard Home", 1, 0)
	foreignRole := ts.CreateTestRole(t, serverB, "Guard Foreign", 999, 0)
	channel := ts.CreateTestChannel(t, serverA, "mig144-guard")
	var category string
	require.NoError(t, ts.DB.QueryRowContext(ctx,
		`INSERT INTO channel_groups (server_id, name) VALUES ($1, 'mig144-guard-cat') RETURNING id`,
		serverA).Scan(&category))

	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		_, err := ts.DB.ExecContext(ctx, q, args...)
		require.NoError(t, err)
	}

	// Snapshot of every constraint on the four tables, used to prove the
	// round trip restores exactly what the up migration built.
	constraints := func(t *testing.T) []string {
		t.Helper()
		rows, err := ts.DB.QueryContext(ctx, `
			SELECT conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid)
			FROM pg_constraint
			WHERE conrelid::regclass::text IN ('roles', 'member_roles', 'channel_permission_overrides', 'category_permission_overrides')
			ORDER BY 1`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var c string
			require.NoError(t, rows.Scan(&c))
			out = append(out, c)
		}
		require.NoError(t, rows.Err())
		return out
	}
	migrated := constraints(t)

	// Leave the shared test database on the 000144 schema even if an assertion
	// fails partway through. The up migration is not re-runnable, so re-apply it
	// only when the FK is missing -- and report a failure, because a database left
	// on the pre-000144 schema while schema_migrations says 144 breaks every later
	// test with no clue why.
	t.Cleanup(func() {
		var applied bool
		if err := ts.DB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'member_roles_role_server_fkey')`).Scan(&applied); err != nil {
			t.Errorf("cleanup: cannot read schema state: %v", err)
			return
		}
		if applied {
			return
		}
		for _, step := range []struct {
			q    string
			args []any
		}{
			{`DELETE FROM member_roles WHERE role_id = $1`, []any{foreignRole}},
			{`UPDATE roles SET permissions = 0 WHERE permissions < 0`, nil},
			{`DELETE FROM channel_permission_overrides WHERE channel_id = $1`, []any{channel}},
			{`DELETE FROM category_permission_overrides WHERE category_id = $1`, []any{category}},
			{up, nil},
		} {
			if _, err := ts.DB.ExecContext(ctx, step.q, step.args...); err != nil {
				t.Errorf("cleanup: could not restore the 000144 schema: %v", err)
				return
			}
		}
	})

	exec(t, down)
	t.Run("down restores the original FK and removes every 000144 constraint", func(t *testing.T) {
		after := strings.Join(constraints(t), "\n")
		assert.Contains(t, after, "member_roles member_roles_role_id_fkey FOREIGN KEY (role_id) REFERENCES roles(id) ON DELETE CASCADE")
		for _, name := range []string{"roles_id_server_key", "member_roles_role_server_fkey", "roles_permissions_non_negative",
			"channel_permission_overrides_allow_non_negative", "channel_permission_overrides_deny_non_negative",
			"category_permission_overrides_allow_non_negative", "category_permission_overrides_deny_non_negative"} {
			assert.NotContains(t, after, " "+name+" ")
		}
	})

	t.Run("a cross-server assignment is refused, not deleted", func(t *testing.T) {
		exec(t, `INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
			serverA, owner.ID, foreignRole)

		_, err := ts.DB.ExecContext(ctx, up)
		require.Error(t, err, "up must refuse while a cross-server assignment exists")
		assert.Contains(t, err.Error(), "member_roles holds 1 row(s) naming a role from a different server",
			"the guard must name the cause and the count, not surface Postgres's opaque FK error")

		var survived int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM member_roles WHERE role_id = $1`, foreignRole).Scan(&survived))
		assert.Equal(t, 1, survived, "the refused migration must leave the evidence row in place")

		exec(t, `DELETE FROM member_roles WHERE role_id = $1`, foreignRole)
		exec(t, up)
		exec(t, down)
	})

	for _, tc := range []struct {
		name, seed, repair string
		args               []any
		message            string
	}{
		{
			"a negative role grant is refused, not rewritten",
			`UPDATE roles SET permissions = -1 WHERE id = $1`,
			`UPDATE roles SET permissions = 0 WHERE id = $1`,
			[]any{homeRole},
			"found 1 role(s), 0 channel override(s) and 0 category override(s)",
		},
		{
			"a negative channel allow is refused, not rewritten",
			`INSERT INTO channel_permission_overrides (channel_id, target_type, target_id, allow, deny) VALUES ($1, 'role', $2, -1, 0)`,
			`DELETE FROM channel_permission_overrides WHERE channel_id = $1 AND target_id = $2`,
			[]any{channel, homeRole},
			"found 0 role(s), 1 channel override(s) and 0 category override(s)",
		},
		{
			"a negative category allow is refused, not rewritten",
			`INSERT INTO category_permission_overrides (category_id, target_type, target_id, allow, deny) VALUES ($1, 'role', $2, -1, 0)`,
			`DELETE FROM category_permission_overrides WHERE category_id = $1 AND target_id = $2`,
			[]any{category, homeRole},
			"found 0 role(s), 0 channel override(s) and 1 category override(s)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec(t, tc.seed, tc.args...)

			_, err := ts.DB.ExecContext(ctx, up)
			require.Error(t, err, "up must refuse while a negative grant exists")
			assert.Contains(t, err.Error(), tc.message)

			exec(t, tc.repair, tc.args...)
			exec(t, up)
			exec(t, down)
		})
	}

	t.Run("a negative deny is normalised by clearing only bit 63", func(t *testing.T) {
		// -1 carries every bit; clearing bit 63 must leave bits 0-62 exactly as they
		// were. The CONTROL row (5) must come through unchanged, which catches a
		// normalisation that rewrites the value rather than masking one bit. It
		// cannot catch a wider WHERE clause, and needs not: the mask is the
		// identity on every non-negative value.
		exec(t, `INSERT INTO channel_permission_overrides (channel_id, target_type, target_id, allow, deny)
			VALUES ($1, 'role', $2, 0, -1), ($1, 'user', $3, 0, 5)`, channel, homeRole, owner.ID)
		exec(t, `INSERT INTO category_permission_overrides (category_id, target_type, target_id, allow, deny)
			VALUES ($1, 'role', $2, 0, -1), ($1, 'user', $3, 0, 5)`, category, homeRole, owner.ID)

		exec(t, up)

		for _, tbl := range []struct{ q, parent string }{
			{`SELECT deny FROM channel_permission_overrides WHERE channel_id = $1 AND target_type = $2`, channel},
			{`SELECT deny FROM category_permission_overrides WHERE category_id = $1 AND target_type = $2`, category},
		} {
			var normalised, control int64
			require.NoError(t, ts.DB.QueryRowContext(ctx, tbl.q, tbl.parent, "role").Scan(&normalised))
			require.NoError(t, ts.DB.QueryRowContext(ctx, tbl.q, tbl.parent, "user").Scan(&control))
			assert.Equal(t, int64(math.MaxInt64), normalised, "-1 & MaxInt64 keeps all 63 permission bits")
			assert.Equal(t, int64(5), control, "a non-negative deny must not be touched")
		}
	})

	t.Run("up after down rebuilds exactly the original constraints", func(t *testing.T) {
		assert.Equal(t, migrated, constraints(t))
	})
}
