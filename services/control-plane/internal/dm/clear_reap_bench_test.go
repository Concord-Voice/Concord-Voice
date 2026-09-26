package dm

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// TestMeasureClearReapDiscoveryPlan is the spec §12.7 measurement that decides
// the optional partial index (#3462 D6): add it only if, at 1e5 range rows,
// branch A's scan of dm_message_hidden_ranges exceeds 50 ms or dominates the
// plan. Opt-in: it bulk-seeds up to a million rows and is not a regression test.
func TestMeasureClearReapDiscoveryPlan(t *testing.T) {
	if os.Getenv("CONCORD_CLEAR_REAP_BENCH") != "1" {
		t.Skip("measurement only: set CONCORD_CLEAR_REAP_BENCH=1")
	}
	for _, rangeRows := range []int{100_000, 1_000_000} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(formatRows(rangeRows)), " ", ""), func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			// 200 users; 10,000 conversations with two participants each.
			exec := func(q string, args ...any) {
				t.Helper()
				_, err := db.Exec(q, args...)
				require.NoError(t, err)
			}
			exec(`INSERT INTO users (id, username, email, password_hash)
				SELECT gen_random_uuid(), 'bench_' || g, 'bench_' || g || '@example.test', 'x'
				  FROM generate_series(1, 200) g`)
			exec(`CREATE TEMP TABLE IF NOT EXISTS bench_users AS
				SELECT id, row_number() OVER (ORDER BY id) - 1 AS n FROM users WHERE username LIKE 'bench_%'`)
			exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by)
				SELECT gen_random_uuid(), true, false, (SELECT id FROM users WHERE username = 'bench_1')
				  FROM generate_series(1, 10000)`)
			exec(`INSERT INTO dm_participants (conversation_id, user_id)
				SELECT c.id, u.id
				  FROM (SELECT id, row_number() OVER (ORDER BY id) AS rn FROM dm_conversations) c
				  JOIN (SELECT id, row_number() OVER (ORDER BY id) - 1 AS n FROM users WHERE username LIKE 'bench_%') u
				    ON u.n IN (c.rn % 200, (c.rn + 1) % 200)`)
			// 10% of the range rows are Clear ranges; they make every conversation
			// "already reaped" (all participants cleared, no messages), which is
			// the rescan cost D6 worries about. The rest are legacy ranges.
			clearRows := rangeRows / 10
			exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
				SELECT p.user_id, p.conversation_id, '-infinity', now(), true
				  FROM dm_participants p, generate_series(1, GREATEST(1, $1 / 20000)) g
				 LIMIT $1`, clearRows)
			exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
				SELECT p.user_id, p.conversation_id, now() - interval '2 days', now() - interval '1 day', false
				  FROM dm_participants p, generate_series(1, GREATEST(1, $1 / 20000)) g
				 LIMIT $1`, rangeRows-clearRows)
			exec(`ANALYZE dm_message_hidden_ranges`)
			exec(`ANALYZE dm_participants`)
			exec(`ANALYZE dm_conversations`)

			for _, cursor := range []any{nil, "80000000-0000-0000-0000-000000000000"} {
				rows, err := db.Query(`EXPLAIN (ANALYZE, BUFFERS) `+clearReapCandidateQuery, cursor, clearReapCandidateLimit)
				require.NoError(t, err)
				var plan []string
				for rows.Next() {
					var line string
					require.NoError(t, rows.Scan(&line))
					plan = append(plan, line)
				}
				require.NoError(t, rows.Err())
				require.NoError(t, rows.Close())
				t.Logf("range rows=%d cursor=%v\n%s", rangeRows, cursor, strings.Join(plan, "\n"))
			}
		})
	}
}

func formatRows(n int) string {
	if n >= 1_000_000 {
		return "1e6 rows"
	}
	return "1e5 rows"
}
