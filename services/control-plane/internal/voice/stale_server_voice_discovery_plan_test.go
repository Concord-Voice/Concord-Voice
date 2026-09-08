package voice_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/stretchr/testify/require"
)

// leaseIndex is the ordered sweep index migration 000132 creates.
const leaseIndex = "idx_voice_participants_lifecycle_observed_at"

// inlineVolatileCutoffSQL is the pre-fix shape: the cutoff written directly
// against the column. It exists ONLY as this test's falsification control --
// production must never issue it. Without it the Index Cond assertion below
// could pass against a planner that reports every predicate that way, and the
// guard would be vacuous.
const inlineVolatileCutoffSQL = `
	SELECT participant.channel_id, participant.user_id, channel.server_id
	FROM voice_participants AS participant
	JOIN channels AS channel ON channel.id = participant.channel_id
	WHERE participant.lifecycle_observed_at <=
	      clock_timestamp() - ($1::bigint * INTERVAL '1 second')
	ORDER BY participant.lifecycle_observed_at,
	         participant.channel_id,
	         participant.user_id
	LIMIT $2;
`

// TestStaleServerVoiceDiscoveryUsesAnIndexRangeBound pins the PLAN SHAPE of the
// lease sweep, which no behavioural test can observe: a Filter and an Index
// Cond return identical rows and differ only in how much of the index the
// planner must walk to find them.
//
// The sweep runs every reconcile tick. Because LIMIT counts MATCHING rows, a
// Filter in the steady state -- no expired participants, which is the normal
// case -- reads the whole active-participant index before returning zero
// candidates, making the pass proportional to concurrent voice users rather
// than to expired rows.
func TestStaleServerVoiceDiscoveryUsesAnIndexRangeBound(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	const limit = 50
	leaseSeconds := int64(presence.ActivityStateTTL / time.Second)

	// Seed and ANALYZE before explaining, so the plan is chosen for a
	// STATISTICAL reason rather than an accidental one.
	//
	// Without this the test passes only in the never-analyzed pg_class state
	// (reltuples = -1), which TruncateAllTables happens to leave behind because
	// PostgreSQL 14+ resets a TRUNCATEd table that way. Measured: after an
	// ANALYZE on the empty table the planner picks a Seq Scan and BOTH queries
	// report Filter, which fails this test for a reason unrelated to the code
	// under review -- and, worse, makes the falsification control below
	// undetectably inert. An autoanalyze landing between a sibling test's
	// truncation and this EXPLAIN is enough to trigger it.
	//
	// seedLeaseRows is what puts the table in the regime the fix is FOR. Below
	// roughly a thousand rows PostgreSQL correctly prefers a sequential scan --
	// scanning a tiny table is genuinely cheaper than descending an index, and
	// the predicate's form cannot change that. Measured on PostgreSQL 16: 64
	// and 256 rows plan as Seq Scan, 1024 as an Index Only Scan.
	seedLeaseRows(ctx, t, db, 2048)

	production := explain(ctx, t, db, voice.StaleServerVoiceDiscoverySQLForTest(), leaseSeconds, limit)
	leaseScan := scanNodeFor(t, production, leaseIndex)
	require.Contains(t, leaseScan, "Index Cond:",
		"lease discovery must bind the cutoff so the predicate is an index range bound\nplan:\n%s", production)
	require.NotContains(t, leaseScan, "Filter:",
		"a Filter means LIMIT counts matching rows over the whole index\nplan:\n%s", production)

	// Falsification control: the shape the fix replaced must still produce the
	// Filter the assertions above reject. If this stops holding, the assertions
	// are no longer discriminating and the guard above proves nothing.
	inline := explain(ctx, t, db, inlineVolatileCutoffSQL, leaseSeconds, limit)
	inlineScan := scanNodeFor(t, inline, leaseIndex)
	require.Contains(t, inlineScan, "Filter:",
		"control lost its teeth: the inline volatile cutoff no longer plans as a Filter\nplan:\n%s", inline)
	require.NotContains(t, inlineScan, "Index Cond:",
		"control lost its teeth: the inline volatile cutoff now binds as a range\nplan:\n%s", inline)
}

// explain returns the EXPLAIN text for query. Costs are omitted so the
// assertion depends on plan shape alone, not on row estimates that move with
// whatever the surrounding suite happened to leave in the table.
func explain(ctx context.Context, t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()

	rows, err := db.QueryContext(ctx, "EXPLAIN (COSTS OFF) "+query, args...)
	require.NoError(t, err, "explain lease discovery")
	defer func() { require.NoError(t, rows.Close(), "close explain rows") }()

	var plan strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line), "scan explain line")
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	require.NoError(t, rows.Err(), "iterate explain rows")
	return plan.String()
}

// scanNodeFor returns the plan lines belonging to the scan of index, i.e. the
// scan line itself plus the deeper-indented qualifier lines under it. Matching
// the whole plan would let a qualifier from the unrelated channels_pkey scan
// satisfy an assertion about the lease index.
func scanNodeFor(t *testing.T, plan, index string) string {
	t.Helper()

	lines := strings.Split(plan, "\n")
	start := -1
	for i, line := range lines {
		if strings.Contains(line, index) {
			start = i
			break
		}
	}
	require.GreaterOrEqual(t, start, 0, "plan never scans %s\nplan:\n%s", index, plan)

	node := []string{lines[start]}
	depth := len(lines[start]) - len(strings.TrimLeft(lines[start], " "))
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line)-len(strings.TrimLeft(line, " ")) <= depth {
			break
		}
		node = append(node, line)
	}
	return strings.Join(node, "\n")
}

// seedLeaseRows bulk-inserts n voice participants on one voice channel, then
// ANALYZEs, so the planner has real statistics for the lease index. Rows are
// stamped at descending ages so the lease column is genuinely ordered rather
// than constant, which is what the index is asked to exploit.
func seedLeaseRows(ctx context.Context, t *testing.T, db *sql.DB, n int) {
	t.Helper()

	const (
		ownerID   = "9d1f0f7a-0000-4000-8000-00000000f001"
		serverID  = "9d1f0f7a-0000-4000-8000-00000000f002"
		channelID = "9d1f0f7a-0000-4000-8000-00000000f003"
	)
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, query, args...)
		require.NoError(t, err, "seed lease rows: %s", query)
	}

	exec(`INSERT INTO users (id, username, email, password_hash)
	      VALUES ($1, 'planowner', 'planowner@example.test', 'x')
	      ON CONFLICT DO NOTHING`, ownerID)
	exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'plan-server', $2)
	      ON CONFLICT DO NOTHING`, serverID, ownerID)
	exec(`INSERT INTO channels (id, server_id, name, type)
	      VALUES ($1, $2, 'plan-channel', 'voice') ON CONFLICT DO NOTHING`, channelID, serverID)
	exec(`INSERT INTO users (id, username, email, password_hash)
	      SELECT gen_random_uuid(), 'planmember' || g, 'planmember' || g || '@example.test', 'x'
	      FROM generate_series(1, $1) AS g`, n)
	exec(`INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at)
	      SELECT $1, member.id,
	             CURRENT_TIMESTAMP - (row_number() OVER () || ' seconds')::interval
	      FROM users AS member
	      WHERE member.username LIKE 'planmember%'`, channelID)
	exec(`ANALYZE voice_participants`)
}
