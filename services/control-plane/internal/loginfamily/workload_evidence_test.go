package loginfamily_test

import (
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Source extraction keeps diagnostic EXPLAINs tied to the actual query text.
// A missing/ambiguous identifier is a test failure, never silently omitted evidence.
func workloadQuery(t *testing.T, file, identifier string, constant bool) string {
	t.Helper()
	// Keep the source set explicit; no caller-supplied path reaches ReadFile.
	var source []byte
	var err error
	switch file {
	case "authority.go":
		source, err = os.ReadFile("authority.go")
	case "targets.go":
		source, err = os.ReadFile("targets.go")
	case "retention_query.go":
		source, err = os.ReadFile("retention_query.go")
	default:
		t.Fatalf("unsupported workload SQL source %q", file)
	}
	require.NoError(t, err)
	tree, err := parser.ParseFile(token.NewFileSet(), file, source, 0)
	require.NoError(t, err)
	expressions := map[string]ast.Expr{}
	ast.Inspect(tree, func(node ast.Node) bool {
		if spec, ok := node.(*ast.ValueSpec); ok && len(spec.Names) == 1 && len(spec.Values) == 1 {
			expressions[spec.Names[0].Name] = spec.Values[0]
		}
		return true
	})
	var expand func(ast.Expr) string
	expand = func(expr ast.Expr) string {
		switch value := expr.(type) {
		case *ast.BasicLit:
			require.Equal(t, token.STRING, value.Kind)
			text, unquoteErr := strconv.Unquote(value.Value)
			require.NoError(t, unquoteErr)
			return text
		case *ast.BinaryExpr:
			require.Equal(t, token.ADD, value.Op)
			return expand(value.X) + expand(value.Y)
		case *ast.Ident:
			next, ok := expressions[value.Name]
			require.True(t, ok, "missing SQL constant %s", value.Name)
			return expand(next)
		default:
			t.Fatalf("unsupported SQL expression %T", expr)
			return ""
		}
	}
	if constant {
		expression, ok := expressions[identifier]
		require.True(t, ok, "missing SQL identifier %s", identifier)
		return expand(expression)
	}
	var matches, exact []string
	ast.Inspect(tree, func(node ast.Node) bool {
		if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			value := expand(literal)
			if value == identifier {
				exact = append(exact, value)
			}
			if strings.HasPrefix(value, identifier) {
				matches = append(matches, value)
			}
		}
		return true
	})
	if len(exact) > 0 {
		matches = exact
	}
	require.Len(t, matches, 1, "SQL identifier must select exactly one source literal: %s", identifier)
	return matches[0]
}

func workloadPlan(t *testing.T, tx *sql.Tx, label, query string, args ...any) {
	t.Helper()
	var plan json.RawMessage
	require.NoError(t, tx.QueryRow(`EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT JSON) `+query, args...).Scan(&plan))
	t.Logf("task8-query-plan label=%s query=%q bound=%v plan=%s", label, query, args, plan)
}

type workloadSample struct {
	Phase                                     string
	WAL                                       string
	Rows                                      map[string]int64
	HeapAlloc, HeapInuse, TotalAlloc, Mallocs uint64
}

func workloadSnapshot(t *testing.T, db *sql.DB, phase string) workloadSample {
	t.Helper()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	sample := workloadSample{Phase: phase, Rows: map[string]int64{}, HeapAlloc: stats.HeapAlloc,
		HeapInuse: stats.HeapInuse, TotalAlloc: stats.TotalAlloc, Mallocs: stats.Mallocs}
	require.NoError(t, db.QueryRow(`SELECT pg_current_wal_insert_lsn()::text`).Scan(&sample.WAL))
	rows, err := db.Query(`SELECT relname,pg_relation_size(oid),pg_indexes_size(oid),pg_total_relation_size(oid)
 FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relkind='r'
 AND relname IN ('login_families','login_family_sids','login_family_revocations','login_family_revocation_targets',
 'refresh_tokens','voice_auth_connections','voice_auth_room_bindings','voice_auth_connection_closures','voice_auth_room_closures') ORDER BY relname`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var name string
		var heap, indexes, total int64
		require.NoError(t, rows.Scan(&name, &heap, &indexes, &total))
		// Identifiers come from this fixed allowlist/catalog query, never caller input.
		sample.Rows[name] = count(t, db, `SELECT count(*) FROM `+pq.QuoteIdentifier(name))
		t.Logf("task8-storage phase=%s relation=%s rows=%d heap_bytes=%d index_bytes=%d total_bytes=%d", phase, name, sample.Rows[name], heap, indexes, total)
	}
	require.NoError(t, rows.Err())
	encoded, err := json.Marshal(sample)
	require.NoError(t, err)
	t.Logf("task8-memory-phase %s", encoded)
	return sample
}
func workloadDelta(t *testing.T, db *sql.DB, before, after workloadSample, started time.Time) {
	t.Helper()
	var walBytes string
	require.NoError(t, db.QueryRow(`SELECT pg_wal_lsn_diff($1::pg_lsn,$2::pg_lsn)::text`, after.WAL, before.WAL).Scan(&walBytes))
	t.Logf("task8-phase from=%s to=%s elapsed=%s allocated_bytes=%d mallocs=%d wal_position_delta_bytes=%s caveat=cluster-wide-WAL-and-process-wide-heap", before.Phase, after.Phase, time.Since(started), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, walBytes)
}

func TestWorkloadEvidenceDisplacement207AndRetention205(t *testing.T) {
	db := database(t)
	baseline := workloadSnapshot(t, db, "baseline")
	started := time.Now()
	user := testdb.CreateUser(t, db)
	seed := transaction(t, db)
	actor := authority(t, seed, user, lf.UserNoKeyUpdate)
	var first lf.AccessIdentity
	for n := 0; n < 207; n++ {
		identity := displacementFamily(t, seed, actor, user, "workload-machine", "198.51.100.1", "fixture-agent", n%2 == 0, 3)
		if n == 0 {
			first = identity
		}
	}
	require.NoError(t, seed.Commit())
	populated := workloadSnapshot(t, db, "live207-sids621")
	workloadDelta(t, db, baseline, populated, started)
	for _, table := range []string{"login_families", "login_family_sids", "refresh_tokens"} {
		_, err := db.Exec(`ANALYZE ` + pq.QuoteIdentifier(table))
		require.NoError(t, err)
	}
	started = time.Now()
	var resolveBefore, resolveAfter runtime.MemStats
	runtime.ReadMemStats(&resolveBefore)
	for n := 0; n < 100; n++ {
		_, resolveErr := lf.ResolveVerifiedAccess(ctx, db, first)
		require.NoError(t, resolveErr)
	}
	runtime.ReadMemStats(&resolveAfter)
	t.Logf("task8-resolve operations=100 elapsed=%s allocated_bytes=%d mallocs=%d", time.Since(started), resolveAfter.TotalAlloc-resolveBefore.TotalAlloc, resolveAfter.Mallocs-resolveBefore.Mallocs)
	tx := transaction(t, db)
	actor = authority(t, tx, user, lf.UserNoKeyUpdate)
	started = time.Now()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	targets, err := lf.CaptureDisplacementTargetsTx(ctx, tx, actor, "workload-machine", "198.51.100.1", "fixture-agent")
	require.NoError(t, err)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	var tempRows, tempHeap, tempIndexes int64
	require.NoError(t, tx.QueryRow(`SELECT count(*),pg_relation_size('pg_temp.loginfamily_targets'),pg_indexes_size('pg_temp.loginfamily_targets') FROM pg_temp.loginfamily_targets`).Scan(&tempRows, &tempHeap, &tempIndexes))
	require.EqualValues(t, 414, tempRows) // frozen matched set and independently owned captured set
	t.Logf("task8-frontier families=207 sids=621 rows=%d heap_bytes=%d index_bytes=%d elapsed=%s allocated_bytes=%d mallocs=%d TEMP_privilege=usable", tempRows, tempHeap, tempIndexes, time.Since(started), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs)
	workloadPlan(t, tx, "SID-binding-live207", workloadQuery(t, "authority.go", "SELECT family_id, access_expires_at FROM login_family_sids", false), user, first.SID)
	var family uuid.UUID
	require.NoError(t, tx.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, first.SID).Scan(&family))
	workloadPlan(t, tx, "family-live207", workloadQuery(t, "authority.go", "SELECT user_id, family_id, credential_epoch, terminal_at, auth_operation_id, epoch_generation FROM login_families WHERE user_id=$1 AND family_id=$2", false), user, family)
	workloadPlan(t, tx, "sorted-user-frontier207", workloadQuery(t, "targets.go", "SELECT f.family_id FROM login_families f\n WHERE f.user_id=$1", false), user, uuid.Nil, "user", uuid.Nil, "", pq.Array([]string{}), uuid.Nil)
	result := displacementRevoke(t, tx, targets)
	require.EqualValues(t, 207, result.NewlyTerminalFamilies)
	require.NoError(t, tx.Commit())
	terminal := workloadSnapshot(t, db, "terminal207-refresh-retained")
	require.EqualValues(t, 207, terminal.Rows["login_family_revocation_targets"])
	workloadDelta(t, db, populated, terminal, started)
	// A terminal family still has access/refresh obligations: collection must retain it.
	retained := retentionSweep(t, db, 1)
	require.Zero(t, retained.DeletedFamilies)
	cohortUser := testdb.CreateUser(t, db)
	started = time.Now()
	_, op := retentionCohort(t, db, cohortUser, 205, false)
	pending := workloadSnapshot(t, db, "pending205-plus-retained207")
	require.Zero(t, retentionSweep(t, db, 1).DeletedFamilies)
	retentionComplete(t, db, op)
	for _, table := range []string{"login_families", "login_family_sids", "login_family_revocations", "login_family_revocation_targets"} {
		_, analyzeErr := db.Exec(`ANALYZE ` + pq.QuoteIdentifier(table))
		require.NoError(t, analyzeErr)
	}
	inspect := transaction(t, db)
	workloadPlan(t, inspect, "complete-cohort205", workloadQuery(t, "retention_query.go", "gcFamilyEligible", true), true, op)
	workloadPlan(t, inspect, "cohort-lock-page100", workloadQuery(t, "retention_query.go", "gcFamilyPage", true), true, op, uuid.Nil, 100)
	require.NoError(t, inspect.Rollback())
	var gcBefore, gcAfter runtime.MemStats
	runtime.ReadMemStats(&gcBefore)
	gcStart := time.Now()
	got := retentionSweep(t, db, 1)
	runtime.ReadMemStats(&gcAfter)
	t.Logf("task8-gc cohort=205 candidate_limit=1 elapsed=%s allocated_bytes=%d mallocs=%d", time.Since(gcStart), gcAfter.TotalAlloc-gcBefore.TotalAlloc, gcAfter.Mallocs-gcBefore.Mallocs)
	require.EqualValues(t, 205, got.DeletedFamilies)
	require.EqualValues(t, 205, got.DeletedSIDs)
	retentionEvidence(t, db, cohortUser, 0, 0, 0, 0)
	collected := workloadSnapshot(t, db, "collected205-retained207")
	workloadDelta(t, db, pending, collected, started)
	// Physical bytes need not shrink after logical deletion. Measurements have no
	// throughput, RSS, index-choice or WAL ceiling assertion.
}

func TestWorkloadEvidencePopulatedClosureStreams(t *testing.T) {
	db := database(t)
	retentionTransportCleanup(t, db)
	user := testdb.CreateUser(t, db)
	// Reuse the registry-retention fixture: mint/bind is canonical; SQL models
	// already-committed release/boot lifecycle events, not an HTTP handler claim.
	var boot, connection, session uuid.UUID
	for n := 0; n < 100; n++ {
		boot, connection, session = retentionBoundClosures(t, db, user, true)
	}
	_, op := retentionCohort(t, db, testdb.CreateUser(t, db), 1, false)
	before := workloadSnapshot(t, db, "released100-active-access-families-and-pending-owner")
	for _, table := range []string{"voice_auth_room_closures", "voice_auth_connection_closures", "login_families", "login_family_revocations"} {
		_, err := db.Exec(`ANALYZE ` + pq.QuoteIdentifier(table))
		require.NoError(t, err)
	}
	tx := transaction(t, db)
	maximum := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	for _, identifier := range []string{
		"SELECT operation_id FROM login_family_revocations WHERE operation_id >",
		"SELECT family_id FROM login_families WHERE auth_operation_id IS NULL AND family_id >",
		"SELECT session_generation FROM voice_auth_room_closures WHERE session_generation >",
		"SELECT connection_generation FROM voice_auth_connection_closures WHERE connection_generation >",
	} {
		workloadPlan(t, tx, identifier, workloadQuery(t, "retention_query.go", identifier, false), uuid.Nil, maximum, 100)
	}
	var family uuid.UUID
	require.NoError(t, tx.QueryRow(`SELECT family_id FROM voice_auth_room_closures WHERE session_generation=$1`, session).Scan(&family))
	workloadPlan(t, tx, "closure100-zero-live-eligibility", workloadQuery(t, "retention_query.go", "gcClosureEligible", true), boot, connection, session, user, family, "")
	require.NoError(t, tx.Rollback())
	started := time.Now()
	got := retentionSweep(t, db, 100)
	require.EqualValues(t, 100, got.DeletedRoomClosures)
	require.EqualValues(t, 100, got.DeletedConnectionClosures)
	require.Zero(t, got.DeletedFamilies) // usable access remains, and owner is pending
	after := workloadSnapshot(t, db, "collected100-closures-retained101-families")
	workloadDelta(t, db, before, after, started)
	retentionComplete(t, db, op)
	require.EqualValues(t, 1, retentionSweep(t, db, 100).DeletedFamilies)
}
