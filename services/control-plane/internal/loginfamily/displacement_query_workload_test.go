package loginfamily_test

import (
	"crypto/sha256"
	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func displacementStagingQuery(t *testing.T, fallback bool) string {
	t.Helper()
	raw, err := os.ReadFile("targets.go")
	require.NoError(t, err)
	tree, err := parser.ParseFile(token.NewFileSet(), "targets.go", raw, 0)
	require.NoError(t, err)
	var fn *ast.FuncDecl
	for _, decl := range tree.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == "CaptureDisplacementTargetsTx" {
			require.Nil(t, fn)
			fn = f
		}
	}
	require.NotNil(t, fn)
	var predicate []string
	var queries []ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if a, ok := n.(*ast.AssignStmt); ok && len(a.Lhs) == 1 && len(a.Rhs) == 1 {
			if id, ok := a.Lhs[0].(*ast.Ident); ok && id.Name == "predicate" {
				lit, ok := a.Rhs[0].(*ast.BasicLit)
				require.True(t, ok)
				s, e := strconv.Unquote(lit.Value)
				require.NoError(t, e)
				predicate = append(predicate, s)
			}
		}
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "QueryRowContext" {
				queries = append(queries, c.Args[1])
			}
		}
		return true
	})
	require.Len(t, predicate, 2)
	require.Len(t, queries, 1)
	branch := predicate[0]
	if fallback {
		branch = predicate[1]
	}
	var expand func(ast.Expr) string
	expand = func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.BasicLit:
			s, err := strconv.Unquote(v.Value)
			require.NoError(t, err)
			return s
		case *ast.Ident:
			require.Equal(t, "predicate", v.Name)
			return branch
		case *ast.BinaryExpr:
			require.Equal(t, token.ADD, v.Op)
			return expand(v.X) + expand(v.Y)
		default:
			t.Fatalf("unsupported query node %T", e)
			return ""
		}
	}
	query := expand(queries[0])
	require.True(t, strings.Contains(query, "INSERT INTO pg_temp.loginfamily_targets"))
	t.Logf("task8-displacement-source sha256=%x function=CaptureDisplacementTargetsTx fallback=%t", sha256.Sum256(raw), fallback)
	return query
}
func TestDisplacementQueryWorkloadActualStagingCTEs(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "machine"
		if fallback {
			name = "ip-agent"
		}
		t.Run(name, func(t *testing.T) {
			db := database(t)
			user := testdb.CreateUser(t, db)
			seed := transaction(t, db)
			u := authority(t, seed, user, lf.UserNoKeyUpdate)
			for n := 0; n < 207; n++ {
				displacementFamily(t, seed, u, user, "workload-machine", "198.51.100.1", "fixture-agent", n%2 == 0, 3)
			}
			displacementFamily(t, seed, u, user, "excluded-machine", "198.51.100.2", "excluded-agent", false, 3)
			require.NoError(t, seed.Commit())
			for _, table := range []string{"refresh_tokens", "login_family_sids", "login_families"} {
				_, err := db.Exec(`ANALYZE ` + table)
				require.NoError(t, err)
			}
			tx := transaction(t, db)
			actor := authority(t, tx, user, lf.UserNoKeyUpdate)
			machine := "workload-machine"
			if fallback {
				machine = ""
			}
			_, err := lf.CaptureDisplacementTargetsTx(ctx, tx, actor, machine, "198.51.100.1", "fixture-agent")
			require.NoError(t, err)
			capture := uuid.New()
			args := []any{user, capture, machine}
			if fallback {
				args = []any{user, capture, "198.51.100.1", "fixture-agent"}
			}
			workloadPlan(t, tx, "actual-displacement-staging-"+name, displacementStagingQuery(t, fallback), args...)
			var staged int
			require.NoError(t, tx.QueryRow(`SELECT count(*) FROM pg_temp.loginfamily_targets WHERE capture=$1`, capture).Scan(&staged))
			require.Equal(t, 207, staged)
			t.Logf("task8-displacement matched_refresh_rows=621 excluded_refresh_rows=3 distinct_staged_families=%d TEMP_privilege=usable EXPLAIN_ANALYZE_executes_INSERT=true rollback=true", staged)
			require.NoError(t, tx.Rollback())
			require.EqualValues(t, 208, count(t, db, `SELECT count(*) FROM login_families`))
			require.EqualValues(t, 624, count(t, db, `SELECT count(*) FROM refresh_tokens`))
			require.Zero(t, count(t, db, `SELECT count(*) FROM login_families WHERE terminal_at IS NOT NULL`))
			require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
			require.Zero(t, count(t, db, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NOT NULL`))
		})
	}
}
