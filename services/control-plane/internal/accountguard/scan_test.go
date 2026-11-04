package accountguard

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Source scans that confine the account guard's writes and transactions to the runner
// (#3680 PR3, Task 12). Four scans, each an AST walk over non-test Go source:
//
//   - OnlyRunnerOpensTransactions: BeginTx and Begin appear only in runner.go.
//   - SourceTxCalledOnlyByRunner: sourceTx is referenced only in runner.go.
//   - SingleWriterRatchet: no SQL write to the five guard tables, and no matures_at
//     assignment, outside internal/accountguard (a committed, shrink-only allowlist covers
//     the matures_at writers that already exist).
//   - RunnerWrapsWithhold: in runner.go and registration.go (and the registry functions the
//     runner relays) every place an error's text can leave is already redacted: %w and text
//     verbs, errors.Join, log attributes, .Error() calls and returned errors. A driver's or a
//     callback's text cannot reach the error Run returns.
//
// Accepted limits of RunnerWrapsWithhold (task-12 ruling 17). Three shapes route an error
// around the binding and return legs and are not judged:
//
//   - a named result assigned inside a stored closure that is later deferred
//     (cleanup := func() { err = ... }; defer cleanup()), because the deferred-result leg
//     needs a DeferStmt on the path to the write and sees only defer func() { ... }();
//   - a closure defined before the nearest binding of a name and called after it, whose
//     assignment to that name the binding walk therefore drops;
//   - a backward goto, which is a loop with no ForStmt for the loop-carried union to find.
//
// Each needs an author deliberately routing an error around the scan. The scan is a regression
// ratchet, and code review catches the deliberate case; none of the three shapes exists in
// runner.go or registration.go. The cost if this is wrong is one unredacted driver error
// through a shape a reviewer would also have to miss.
//
// Each scan is a pure function over source bytes. The tree test applies it to the real files;
// the planted controls run the SAME function over fixture source, once with an offender it
// must flag and once with clean source it must pass. A scan that cannot be seen to fail on a
// planted offender, and to find real sites on the real tree, proves nothing.
//
// Everything here is prefixed gs so it cannot collide with the package's own identifiers.

const (
	gsPkgDir     = "internal/accountguard"
	gsRunnerFile = "internal/accountguard/runner.go"
	gsFenceRecv  = "r.d.Fence"
)

// gsParse parses one source file or fails the test.
func gsParse(t *testing.T, name string, body []byte) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, body, parser.SkipObjectResolution)
	require.NoError(t, err, "parse %s", name)
	return fset, f
}

// gsReadTree returns every non-test Go file under the given roots of the module, keyed by its
// module-relative slash path. A root that does not exist is skipped (cmd/ is optional); the
// walk and the reads go through one os.Root, so a symlink cannot point a read outside it.
func gsReadTree(t *testing.T, roots ...string) map[string][]byte {
	t.Helper()
	root, err := os.OpenRoot(filepath.Join("..", ".."))
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	fsys := root.FS()
	out := make(map[string][]byte)
	for _, dir := range roots {
		_, statErr := fs.Stat(fsys, dir)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		require.NoError(t, statErr)
		walkErr := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				if d.Name() == "vendor" || d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			body, rerr := fs.ReadFile(fsys, p)
			if rerr != nil {
				return rerr
			}
			out[p] = body
			return nil
		})
		require.NoError(t, walkErr)
	}
	return out
}

// gsHdr is the file header every fixture gets: the imports the scans resolve by path.
const gsHdr = `package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
)

`

// gsFn wraps statements in a function so a fixture is a complete Go source file.
func gsFn(stmts string) string {
	return gsHdr + "func fixture() {\n" + stmts + "\n}\n"
}

// gsStale returns the allowlist keys the scan never used: a stale entry must be deleted, which
// is what makes an allowlist shrink-only.
func gsStale[K comparable](allow map[K]string, used map[K]bool) []K {
	var out []K
	for k := range allow {
		if !used[k] {
			out = append(out, k)
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------
// Scan 1: only the runner opens transactions.
// ---------------------------------------------------------------------------------------

// gsTxSite is one selector whose name begins with Begin. Matching is by selector name, whatever
// the receiver is called, so a renamed receiver or a method value cannot hide an open.
type gsTxSite struct {
	file string
	name string // BeginTx, Begin, BeginTxx, BeginFunc, ...
	recv string // the receiver expression, as written
	line int
}

// gsNotAnOpen are Begin-prefixed names that open no transaction: the presence history's forced
// clear starts inside the caller's transaction.
var gsNotAnOpen = map[string]bool{"BeginForcedSecurityClear": true}

// gsTxLiteralRE is a literal whose first SQL token opens a transaction: BEGIN (alone or with a
// mode) or START TRANSACTION. "begin: wrapped" in an error message is not one.
var gsTxLiteralRE = regexp.MustCompile(`(?is)^\s*(?:BEGIN|START\s+TRANSACTION)\s*(?:;|$|TRANSACTION\b|ISOLATION\b|READ\s+(?:WRITE|ONLY)\b|WORK\b|(?:NOT\s+)?DEFERRABLE\b)`)

// gsTxSiteViolation applies the rule to one site, or returns "". Only runner.go may open a
// transaction, and the only names it may use are BeginTx and credepoch's Fence.Begin, which is
// not a transaction: it starts the fence op, outside any transaction (R4), and is matched by
// its receiver expression. A wrapper inside runner.go is still the runner (ruling 11, M4).
func gsTxSiteViolation(s gsTxSite) string {
	switch {
	case s.file != gsRunnerFile:
		return fmt.Sprintf("%s:%d: %s (receiver %s) outside runner.go", s.file, s.line, s.name, s.recv)
	case s.name == "BeginTx", s.name == "Begin" && s.recv == gsFenceRecv:
		return ""
	}
	return fmt.Sprintf("%s:%d: %s on %s is not an opener runner.go may use (BeginTx, or %s.Begin, the fence op)",
		s.file, s.line, s.name, s.recv, gsFenceRecv)
}

// gsTxScan is the whole scan over a set of files: every Begin* selector, and every string
// literal that opens a transaction in SQL. It returns the violations and the sites it saw.
func gsTxScan(t *testing.T, files map[string][]byte) ([]string, []gsTxSite) {
	t.Helper()
	var sites []gsTxSite
	for p, body := range files {
		fset, f := gsParse(t, p, body)
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "Begin") && !gsNotAnOpen[sel.Sel.Name] {
				sites = append(sites, gsTxSite{file: p, name: sel.Sel.Name, recv: types.ExprString(sel.X), line: fset.Position(sel.Pos()).Line})
			}
			return true
		})
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].file != sites[j].file {
			return sites[i].file < sites[j].file
		}
		return sites[i].line < sites[j].line
	})
	var violations []string
	for _, s := range sites {
		if v := gsTxSiteViolation(s); v != "" {
			violations = append(violations, v)
		}
	}
	for _, lit := range gsAllLiterals(t, files) {
		if gsTxLiteralRE.MatchString(lit.text) {
			violations = append(violations, lit.path+": a string literal opens a transaction in SQL (BEGIN or START TRANSACTION)")
		}
	}
	return violations, sites
}

func TestScan_OnlyRunnerOpensTransactions(t *testing.T) {
	files := gsReadTree(t, gsPkgDir)
	require.Contains(t, files, gsRunnerFile, "the walk must reach runner.go")
	require.Contains(t, files, "internal/accountguard/registration.go", "the walk must reach registration.go")
	var sawDecide bool
	for p := range files {
		sawDecide = sawDecide || strings.HasPrefix(p, gsPkgDir+"/decide/")
	}
	require.True(t, sawDecide, "the walk must descend into decide/")
	require.Greater(t, len(files), 15, "the walk must cover the package")

	violations, sites := gsTxScan(t, files)
	var runnerBeginTx, runnerFenceBegin int
	for _, s := range sites {
		switch {
		case s.file != gsRunnerFile:
		case s.name == "BeginTx":
			runnerBeginTx++
		case s.recv == gsFenceRecv:
			runnerFenceBegin++
		}
	}
	// The scan must see the sites it exists to confine: db.BeginTx twice and Presence.BeginTx
	// once, and the one Fence.Begin. A walk that found none would pass an empty tree.
	assert.GreaterOrEqual(t, runnerBeginTx, 3, "runner.go opens its transactions with BeginTx (pass 1, plain pass 2, forced pass 2)")
	assert.GreaterOrEqual(t, runnerFenceBegin, 1, "runner.go begins the fence op with Fence.Begin")
	sort.Strings(violations)
	assert.Empty(t, violations, "only runner.go may open a transaction")
}

func TestScan_OnlyRunnerOpensTransactions_PlantedControls(t *testing.T) {
	const registration = "internal/accountguard/registration.go"
	offenders := []struct{ name, file, src string }{
		{"db.BeginTx in registration.go", registration,
			gsFn("tx, _ := db.BeginTx(ctx, nil)\n_ = tx")},
		{"Presence.BeginTx in apply.go", "internal/accountguard/apply.go",
			gsFn("tx, _ := d.Presence.BeginTx(ctx, nil)\n_ = tx")},
		{"plain Begin in decide/", "internal/accountguard/decide/decide.go",
			gsFn("tx, _ := conn.Begin()\n_ = tx")},
		{"renamed receiver", registration,
			gsFn("tx, _ := somethingElseEntirely.BeginTx(ctx, nil)\n_ = tx")},
		{"method value, not a call", registration,
			gsFn("open := db.BeginTx\n_ = open")},
		{"the fence receiver is allowed only in runner.go", registration,
			gsFn("op, _ := r.d.Fence.Begin(ctx, id)\n_ = op")},
		{"a Begin in runner.go on another receiver", gsRunnerFile,
			gsFn("tx, _ := other.Begin(ctx, id)\n_ = tx")},
		{"BeginTxx (sqlx) in registration.go", registration,
			gsFn("tx, _ := db.BeginTxx(ctx, nil)\n_ = tx")},
		{"BeginFunc (pgx) in registration.go", registration,
			gsFn("_ = db.BeginFunc(ctx, func(tx int) error { return nil })")},
		{"BeginTxFunc in registration.go", registration,
			gsFn("_ = db.BeginTxFunc(ctx, nil, func(tx int) error { return nil })")},
		{"BeginFunc even in runner.go", gsRunnerFile,
			gsFn("_ = db.BeginFunc(ctx, func(tx int) error { return nil })")},
		{"a BEGIN literal executed as SQL", registration,
			gsFn(`_, _ = tx.ExecContext(ctx, "BEGIN")`)},
		{"START TRANSACTION with a mode", registration,
			gsFn(`_, _ = tx.ExecContext(ctx, "start transaction isolation level serializable")`)},
		{"a BEGIN literal joined by concatenation", registration,
			gsFn(`_, _ = tx.ExecContext(ctx, "BE" + "GIN ISOLATION LEVEL REPEATABLE READ")`)},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := gsTxScan(t, map[string][]byte{tc.file: []byte(tc.src)})
			assert.NotEmpty(t, got, "the scan must flag this source")
		})
	}
	clean := []struct{ name, file, src string }{
		{"db.BeginTx in runner.go", gsRunnerFile, gsFn("tx, _ := r.d.DB.BeginTx(ctx, nil)\n_ = tx")},
		{"Presence.BeginTx in runner.go", gsRunnerFile, gsFn("tx, _ := r.d.Presence.BeginTx(ctx, nil)\n_ = tx")},
		{"Fence.Begin in runner.go", gsRunnerFile, gsFn("op, _ := r.d.Fence.Begin(ctx, id)\n_ = op")},
		{"neither name, in registration.go", registration,
			gsFn("tx.Commit()\nc, _ := p.BeginForcedSecurityClear(ctx, tx, id)\n_ = c")},
		{"an error message that begins with the word begin", registration,
			gsFn(`_ = fmt.Errorf("begin: %w", withhold(err))`)},
		{"a SELECT that mentions BEGIN later", registration,
			gsFn(`_, _ = tx.ExecContext(ctx, "SELECT 1 -- BEGIN")`)},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := gsTxScan(t, map[string][]byte{tc.file: []byte(tc.src)})
			assert.Empty(t, got)
		})
	}
}

// ---------------------------------------------------------------------------------------
// Scan 2: sourceTx is called only by the runner.
// ---------------------------------------------------------------------------------------

// gsSourceTxRefs returns the line of every reference to sourceTx other than its own
// declaration: a call, a method value, or a function value passed along all count.
func gsSourceTxRefs(t *testing.T, name string, body []byte) []int {
	t.Helper()
	fset, f := gsParse(t, name, body)
	declared := make(map[*ast.Ident]bool)
	var lines []int
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			declared[x.Name] = true
		case *ast.Ident:
			if x.Name == "sourceTx" && !declared[x] {
				lines = append(lines, fset.Position(x.Pos()).Line)
			}
		}
		return true
	})
	return lines
}

func gsSourceTxViolations(name string, lines []int) []string {
	if name == gsRunnerFile {
		return nil
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, fmt.Sprintf("%s:%d: sourceTx referenced outside runner.go", name, l))
	}
	return out
}

func TestScan_SourceTxCalledOnlyByRunner(t *testing.T) {
	files := gsReadTree(t, gsPkgDir)
	require.Contains(t, files, gsRunnerFile)
	require.Contains(t, files, "internal/accountguard/source.go", "the walk must reach sourceTx's own file")
	require.Contains(t, files, "internal/accountguard/registration.go", "registration.go must have none (Task 11 ruling 5)")

	violations := make([]string, 0, len(files))
	var runnerRefs int
	for p, body := range files {
		lines := gsSourceTxRefs(t, p, body)
		violations = append(violations, gsSourceTxViolations(p, lines)...)
		if p == gsRunnerFile {
			runnerRefs = len(lines)
		}
	}
	// pass 1 and pass 2 each attribute their writes with sourceTx (R5).
	assert.GreaterOrEqual(t, runnerRefs, 2, "runner.go calls sourceTx in both passes")
	sort.Strings(violations)
	assert.Empty(t, violations, "sourceTx may be called only by runner.go")
}

func TestScan_SourceTxCalledOnlyByRunner_PlantedControls(t *testing.T) {
	offenders := []struct{ name, file, src string }{
		{"a call in registration.go", "internal/accountguard/registration.go",
			gsFn("_ = sourceTx(ctx, tx, src)")},
		{"a call in apply.go", "internal/accountguard/apply.go",
			gsFn("if err := sourceTx(ctx, tx, s); err != nil {\n\treturn\n}")},
		{"a function value, not a call", "internal/accountguard/challenge.go",
			gsFn("f := sourceTx\n_ = f")},
		{"in decide/", "internal/accountguard/decide/decide.go",
			gsFn("_ = sourceTx(ctx, tx, s)")},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			got := gsSourceTxViolations(tc.file, gsSourceTxRefs(t, tc.file, []byte(tc.src)))
			assert.NotEmpty(t, got, "the scan must flag this source")
		})
	}
	t.Run("the runner may call it", func(t *testing.T) {
		src := []byte(gsFn("_ = sourceTx(ctx, tx, o.Source)"))
		refs := gsSourceTxRefs(t, gsRunnerFile, src)
		require.NotEmpty(t, refs, "the reference is still seen in runner.go")
		assert.Empty(t, gsSourceTxViolations(gsRunnerFile, refs))
	})
	t.Run("the declaration is not a call", func(t *testing.T) {
		src := []byte("package accountguard\n\nfunc sourceTx(ctx int) error { return nil }\n")
		assert.Empty(t, gsSourceTxRefs(t, "internal/accountguard/source.go", src))
	})
	t.Run("a comment is not a call", func(t *testing.T) {
		src := []byte(gsFn("// sourceTx is not called here\n_ = 1"))
		assert.Empty(t, gsSourceTxRefs(t, "internal/accountguard/registration.go", src))
	})
}

// ---------------------------------------------------------------------------------------
// Scan 3: the single-writer ratchet.
// ---------------------------------------------------------------------------------------

// The five tables only internal/accountguard may write. accountguard_hmac_keys holds the
// address-fingerprint keys; the other four are the guard's pending-request, origin, credential
// history and address-reservation state.
var gsGuardTables = []string{
	"account_pending_requests",
	"account_pending_request_origins",
	"user_credential_history",
	"account_address_reservations",
	"accountguard_hmac_keys",
}

// The scan reads string literals, so it has two known limits. A table name that exists only at
// runtime (a Sprintf placeholder, a non-constant variable) is out of reach, as in the precedent
// scan. And it is text-based, so prose that names a guard table next to a write verb
// ("failed to update account_address_reservations") trips it: reword the message. Nothing on
// the tree does today.
var (
	// A write: INSERT INTO, MERGE INTO, DELETE FROM, UPDATE, COPY or TRUNCATE, then an optional
	// ONLY and schema qualifier, then one of the tables, bare or double-quoted.
	gsTableWriteRE = regexp.MustCompile(`(?is)(?:\bINSERT\s+INTO|\bMERGE\s+INTO|\bDELETE\s+FROM|\bUPDATE|\bCOPY|\bTRUNCATE(?:\s+TABLE)?)\s+` +
		`(?:ONLY\s+)?(?:"?[a-z_][a-z0-9_]*"?\.)?"?(` + strings.Join(gsGuardTables, "|") + `)(?:"|\b)`)
	// SQL comments and row-locking clauses come out first, so a comment cannot split a
	// statement and "FOR UPDATE" is not read as a write.
	gsSQLCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*`)
	gsLockingRE    = regexp.MustCompile(`(?i)\bFOR\s+(NO\s+KEY\s+)?UPDATE\b|\bFOR\s+(KEY\s+)?SHARE\b`)
	gsSetWordRE    = regexp.MustCompile(`(?i)\bSET\b`)
	gsListEndRE    = regexp.MustCompile(`(?i)^(?:WHERE|FROM|RETURNING)\b`)
	// A matures_at column name, bare or prefixed (password_matures_at, email_matures_at).
	gsMaturesColRE = regexp.MustCompile(`(?i)\b[a-z_]*matures_at\b`)
	// An INSERT whose column list names a matures_at column.
	gsInsertColsRE = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+[^(;]*\(([^)]*)\)`)
)

func gsStripSQL(literal string) string {
	return gsLockingRE.ReplaceAllString(gsSQLCommentRE.ReplaceAllString(literal, " "), " ")
}

// gsTableWrites returns the guard tables a literal writes.
func gsTableWrites(literal string) []string {
	matches := gsTableWriteRE.FindAllStringSubmatch(gsStripSQL(literal), -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.ToLower(m[1]))
	}
	return out
}

func gsIsWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// gsSetLists returns the text of every SET list in stmt: from SET to the first WHERE, FROM or
// RETURNING (or a semicolon) at parenthesis depth 0 and outside a single-quoted string, so a
// subselect with its own WHERE inside the list does not end it.
func gsSetLists(stmt string) []string {
	locs := gsSetWordRE.FindAllStringIndex(stmt, -1)
	out := make([]string, 0, len(locs))
	for _, loc := range locs {
		depth, quoted := 0, false
		end := len(stmt)
		for i := loc[1]; i < len(stmt); i++ {
			ch := stmt[i]
			switch {
			case quoted:
				quoted = ch != '\''
			case ch == '\'':
				quoted = true
			case ch == '(':
				depth++
			case ch == ')':
				depth--
			case depth == 0 && ch == ';':
				end = i
			case depth == 0 && (i == 0 || !gsIsWordByte(stmt[i-1])) && gsListEndRE.MatchString(stmt[i:]):
				end = i
			}
			if end != len(stmt) {
				break
			}
		}
		out = append(out, stmt[loc[1]:end])
	}
	return out
}

// gsSplitTop splits s at the commas that are at depth 0 and outside a single-quoted string.
func gsSplitTop(s string) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case quoted:
			quoted = ch != '\''
		case ch == '\'':
			quoted = true
		case ch == '(':
			depth++
		case ch == ')':
			depth--
		case ch == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// gsAssignTarget is the text before an assignment item's first top-level "=": the column, or
// the row constructor "(a, b)" of `SET (a, b) = (...)`.
func gsAssignTarget(item string) string {
	depth, quoted := 0, false
	for i := 0; i < len(item); i++ {
		switch ch := item[i]; {
		case quoted:
			quoted = ch != '\''
		case ch == '\'':
			quoted = true
		case ch == '(':
			depth++
		case ch == ')':
			depth--
		case ch == '=' && depth == 0:
			return item[:i]
		}
	}
	return ""
}

// gsMaturesAssigned reports whether a literal assigns a matures_at column: as a target in a SET
// list (a column, a quoted column, or one of a row constructor), or in an INSERT column list.
func gsMaturesAssigned(literal string) bool {
	stmt := strings.ReplaceAll(gsStripSQL(literal), `"`, "")
	for _, list := range gsSetLists(stmt) {
		for _, item := range gsSplitTop(list) {
			if gsMaturesColRE.MatchString(gsAssignTarget(item)) {
				return true
			}
		}
	}
	for _, m := range gsInsertColsRE.FindAllStringSubmatch(stmt, -1) {
		if gsMaturesColRE.MatchString(m[1]) {
			return true
		}
	}
	return false
}

const (
	gsRuleTableWrite    = "table-write"
	gsRuleMaturesAssign = "matures-assign"
)

type gsFinding struct{ path, rule, msg string }

// gsWriterFindings is the pure rule: a literal outside internal/accountguard may not write a
// guard table or assign a matures_at column.
func gsWriterFindings(p, literal string) []gsFinding {
	if strings.HasPrefix(p, gsPkgDir+"/") {
		return nil
	}
	var out []gsFinding
	for _, table := range gsTableWrites(literal) {
		out = append(out, gsFinding{p, gsRuleTableWrite, p + ": writes " + table + " outside " + gsPkgDir + " (a message that merely names it next to a write verb trips this too: reword it)"})
	}
	if gsMaturesAssigned(literal) {
		out = append(out, gsFinding{p, gsRuleMaturesAssign, p + ": assigns a matures_at column outside " + gsPkgDir + " (prose that merely names the assignment trips this too: reword it)"})
	}
	return out
}

// gsConstStrings evaluates an expression made only of string literals, parentheses, + and names
// found in env (the package's string constants and variables, in every scope). A name may have
// several values, so the result is every combination, capped. nil means not constant.
func gsConstStrings(e ast.Expr, env map[string][]string) []string {
	switch n := e.(type) {
	case *ast.BasicLit:
		if n.Kind != token.STRING {
			return nil
		}
		if s, err := strconv.Unquote(n.Value); err == nil {
			return []string{s}
		}
	case *ast.ParenExpr:
		return gsConstStrings(n.X, env)
	case *ast.Ident:
		return env[n.Name]
	case *ast.BinaryExpr:
		if n.Op != token.ADD {
			return nil
		}
		l, r := gsConstStrings(n.X, env), gsConstStrings(n.Y, env)
		if len(l) == 0 || len(r) == 0 {
			return nil
		}
		out := make([]string, 0, len(l)*len(r))
		for _, a := range l {
			for _, b := range r {
				if len(out) < 16 {
					out = append(out, a+b)
				}
			}
		}
		return out
	}
	return nil
}

// gsConstString is gsConstStrings for a single-valued expression.
func gsConstString(e ast.Expr, env map[string][]string) (string, bool) {
	if vals := gsConstStrings(e, env); len(vals) == 1 {
		return vals[0], true
	}
	return "", false
}

// gsPackageEnv collects a package's string constants and variables from every scope: file
// level, function-local const and var, and `x := "..."`. Over-approximate on purpose: a name
// declared twice keeps both values. It resolves chains such as `const a = b + "x"` by repeating
// until nothing new resolves.
func gsPackageEnv(files []*ast.File) map[string][]string {
	env := make(map[string][]string)
	add := func(name string, e ast.Expr) {
		for _, v := range gsConstStrings(e, env) {
			if !slices.Contains(env[name], v) {
				env[name] = append(env[name], v)
			}
		}
	}
	for range 4 {
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.ValueSpec:
					if len(x.Names) == len(x.Values) {
						for i, name := range x.Names {
							add(name.Name, x.Values[i])
						}
					}
				case *ast.AssignStmt:
					if len(x.Lhs) == len(x.Rhs) {
						for i, l := range x.Lhs {
							if id, ok := l.(*ast.Ident); ok {
								add(id.Name, x.Rhs[i])
							}
						}
					}
				}
				return true
			})
		}
	}
	return env
}

type gsLiteral struct{ path, text string }

// gsIsGuardTable reports whether v is exactly a guard table's name, bare or schema-qualified.
func gsIsGuardTable(v string) bool {
	v = strings.ToLower(strings.Trim(v, `"`))
	if i := strings.LastIndex(v, "."); i >= 0 {
		v = strings.Trim(v[i+1:], `"`)
	}
	return slices.Contains(gsGuardTables, v)
}

// gsCopyCallees are the bulk-load entry points that take a table name outside a SQL statement:
// pq.CopyIn, pq.CopyInSchema and pgx's CopyFrom.
var gsCopyCallees = map[string]bool{"CopyIn": true, "CopyInSchema": true, "CopyFrom": true}

// gsPackageLiterals returns every string a package's SQL could be built from: each all-constant
// concatenation whole (named constants resolved), every other string literal on its own, and,
// for a bulk-load call naming a guard table, the equivalent COPY statement. files is one
// directory's non-test sources, which is one Go package.
func gsPackageLiterals(t *testing.T, files map[string][]byte) []gsLiteral {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	parsed := make([]*ast.File, 0, len(names))
	for _, n := range names {
		_, f := gsParse(t, n, files[n])
		parsed = append(parsed, f)
	}
	env := gsPackageEnv(parsed)
	var out []gsLiteral
	for i, f := range parsed {
		p := names[i]
		ast.Inspect(f, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.BinaryExpr:
				if vals := gsConstStrings(e, env); len(vals) > 0 {
					for _, s := range vals {
						out = append(out, gsLiteral{p, s})
					}
					return false
				}
			case *ast.BasicLit:
				if s, ok := gsConstString(e, nil); ok {
					out = append(out, gsLiteral{p, s})
				}
			case *ast.CallExpr:
				out = append(out, gsCopyLiterals(p, e, env)...)
			}
			return true
		})
	}
	return out
}

// gsCopyLiterals turns a CopyIn/CopyFrom call that names a guard table (as a string, a named
// constant, or an element of a pgx.Identifier literal) into a COPY literal for the write rule.
func gsCopyLiterals(p string, call *ast.CallExpr, env map[string][]string) []gsLiteral {
	var name string
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
	case *ast.SelectorExpr:
		name = fun.Sel.Name
	}
	if !gsCopyCallees[name] {
		return nil
	}
	var out []gsLiteral
	for _, arg := range call.Args {
		ast.Inspect(arg, func(m ast.Node) bool {
			if e, ok := m.(ast.Expr); ok {
				for _, v := range gsConstStrings(e, env) {
					if gsIsGuardTable(v) {
						out = append(out, gsLiteral{p, "COPY " + v + " FROM STDIN"})
					}
				}
			}
			return true
		})
	}
	return out
}

// gsGroupByDir groups files by directory, which is by Go package.
func gsGroupByDir(files map[string][]byte) map[string]map[string][]byte {
	out := make(map[string]map[string][]byte)
	for p, body := range files {
		dir := path.Dir(p)
		if out[dir] == nil {
			out[dir] = make(map[string][]byte)
		}
		out[dir][p] = body
	}
	return out
}

// gsAllLiterals is gsPackageLiterals over every package of files.
func gsAllLiterals(t *testing.T, files map[string][]byte) []gsLiteral {
	t.Helper()
	groups := gsGroupByDir(files)
	out := make([]gsLiteral, 0, len(groups)*8)
	for _, pkg := range groups {
		out = append(out, gsPackageLiterals(t, pkg)...)
	}
	return out
}

// gsFindingsOf applies the rule to every literal.
func gsFindingsOf(lits []gsLiteral) []gsFinding {
	out := make([]gsFinding, 0, len(lits)/64)
	for _, lit := range lits {
		out = append(out, gsWriterFindings(lit.path, lit.text)...)
	}
	return out
}

// gsPipeline is the whole scan over a set of files.
func gsPipeline(t *testing.T, files map[string][]byte) []gsFinding {
	t.Helper()
	return gsFindingsOf(gsAllLiterals(t, files))
}

// gsApplyMaturesAllow drops the matures_at findings of an allowlisted file and reports which
// entries it used. Table writes are never allowlisted. The tree test and the planted control
// both go through it.
func gsApplyMaturesAllow(findings []gsFinding, allow map[string]string) ([]string, map[string]bool) {
	var violations []string
	used := make(map[string]bool)
	for _, f := range findings {
		if _, ok := allow[f.path]; ok && f.rule == gsRuleMaturesAssign {
			used[f.path] = true
			continue
		}
		violations = append(violations, f.msg)
	}
	return violations, used
}

// gsMaturesAllowMax pins the allowlist's size. It is a ratchet: it may only shrink, an entry
// the scan no longer needs fails the staleness assertion, and raising this number needs a
// controller ruling.
const gsMaturesAllowMax = 0

// gsMaturesAllow is the committed allowlist of files OUTSIDE internal/accountguard that may
// assign a matures_at column, each with the reason. Empty today: no writer of either column
// exists outside the package (the precedent scan in internal/database confines the same columns
// by name). Table writes outside internal/accountguard have NO allowlist: the package is the
// only writer.
var gsMaturesAllow = map[string]string{}

func TestScan_SingleWriterRatchet(t *testing.T) {
	files := gsReadTree(t, "internal", "cmd")
	var sawKnown, sawCmd bool
	for p := range files {
		sawKnown = sawKnown || p == "internal/mfa/handlers.go"
		sawCmd = sawCmd || strings.HasPrefix(p, "cmd/")
	}
	require.True(t, sawKnown, "the walk must reach internal/mfa/handlers.go")
	require.True(t, sawCmd, "the walk must reach cmd/")
	require.Contains(t, files, "internal/accountguard/apply.go")
	require.Contains(t, files, "internal/accountguard/registration.go")
	require.Greater(t, len(files), 300, "the walk must cover the module")

	// The detector must see the real writers inside the package, or a clean result outside
	// it proves nothing.
	lits := gsAllLiterals(t, files)
	var insideTableWrites, insideMatures int
	tablesSeen := make(map[string]bool)
	for _, lit := range lits {
		if !strings.HasPrefix(lit.path, gsPkgDir+"/") {
			continue
		}
		for _, tbl := range gsTableWrites(lit.text) {
			insideTableWrites++
			tablesSeen[tbl] = true
		}
		if gsMaturesAssigned(lit.text) {
			insideMatures++
		}
	}
	require.Greater(t, len(lits), 1000, "the walk must examine the module's string literals")
	require.Positive(t, insideTableWrites, "the detector must see the package's own table writes")
	require.Positive(t, insideMatures, "the detector must see the package's own matures_at writes")
	assert.Contains(t, tablesSeen, "account_pending_requests", "the detector must see the pending-request writer")

	violations, used := gsApplyMaturesAllow(gsFindingsOf(lits), gsMaturesAllow)
	sort.Strings(violations)
	assert.Empty(t, violations, "only %s may write the guard tables or assign matures_at", gsPkgDir)
	assert.Empty(t, gsStale(gsMaturesAllow, used), "a stale gsMaturesAllow entry must be deleted: the allowlist only shrinks")
	require.LessOrEqual(t, len(gsMaturesAllow), gsMaturesAllowMax, "gsMaturesAllow may only shrink: a new entry needs a controller ruling")
	for p, reason := range gsMaturesAllow {
		assert.GreaterOrEqual(t, len(strings.TrimSpace(reason)), 20, "%s needs a real reason", p)
	}
}

func TestScan_SingleWriterRatchet_PlantedControls(t *testing.T) {
	const outside = "internal/mfa/write.go"
	const inside = "internal/accountguard/write.go"
	offenders := []struct{ name, path, src, rule string }{
		{"DELETE FROM a pending request", outside,
			"package mfa\n\nvar q = `DELETE FROM account_pending_requests WHERE id = $1`\n", gsRuleTableWrite},
		{"INSERT INTO an origin, joined by concatenation", outside,
			"package mfa\n\nconst q = \"INSERT INTO \" + \"account_pending_request_origins (id) VALUES ($1)\"\n", gsRuleTableWrite},
		{"UPDATE through a named constant table", outside,
			"package mfa\n\nconst tbl = \"user_credential_history\"\n\nvar q = \"UPDATE \" + tbl + \" SET x = 1\"\n", gsRuleTableWrite},
		{"a chain of named constants", outside,
			"package mfa\n\nconst a = \"account_address_\"\nconst b = a + \"reservations\"\nvar q = \"DELETE FROM \" + b\n", gsRuleTableWrite},
		{"a function-local constant", outside,
			"package mfa\n\nfunc f() string {\n\tconst tb = \"account_pending_requests\"\n\treturn \"DELETE FROM \" + tb\n}\n", gsRuleTableWrite},
		{"a function-local variable", outside,
			"package mfa\n\nfunc f() string {\n\ttb := \"accountguard_hmac_keys\"\n\treturn \"UPDATE \" + tb + \" SET x = 1\"\n}\n", gsRuleTableWrite},
		{"TRUNCATE", outside,
			"package mfa\n\nvar q = `TRUNCATE account_pending_requests`\n", gsRuleTableWrite},
		{"TRUNCATE TABLE ONLY, schema-qualified", outside,
			"package mfa\n\nvar q = `truncate table only public.user_credential_history`\n", gsRuleTableWrite},
		{"pq.CopyIn on a guard table", outside,
			"package mfa\n\nvar q = pq.CopyIn(\"account_pending_requests\", \"id\", \"user_id\")\n", gsRuleTableWrite},
		{"pgx CopyFrom with an Identifier", outside,
			"package mfa\n\nfunc f() {\n\t_, _ = conn.CopyFrom(ctx, pgx.Identifier{\"public\", \"user_credential_history\"}, cols, src)\n}\n", gsRuleTableWrite},
		{"CopyIn through a named constant", outside,
			"package mfa\n\nconst tbl = \"account_address_reservations\"\n\nvar q = pq.CopyIn(tbl, \"id\")\n", gsRuleTableWrite},
		{"lower case, with ONLY", outside,
			"package mfa\n\nvar q = \"update only account_address_reservations set x = 1\"\n", gsRuleTableWrite},
		{"schema-qualified and quoted", outside,
			"package mfa\n\nvar q = `INSERT INTO public.\"accountguard_hmac_keys\" (id) VALUES ($1)`\n", gsRuleTableWrite},
		{"a write in a CTE", outside,
			"package mfa\n\nvar q = `WITH d AS (DELETE FROM account_pending_requests RETURNING id) SELECT id FROM d`\n", gsRuleTableWrite},
		{"under cmd/", "cmd/tool/main.go",
			"package main\n\nvar q = `INSERT INTO accountguard_hmac_keys (id) VALUES ($1)`\n", gsRuleTableWrite},
		{"password_matures_at in a SET list", outside,
			"package mfa\n\nvar q = `UPDATE users SET password_matures_at = $2 WHERE id = $1`\n", gsRuleMaturesAssign},
		{"email_matures_at with no spaces", outside,
			"package mfa\n\nvar q = `UPDATE users SET email_matures_at=now() WHERE id = $1`\n", gsRuleMaturesAssign},
		{"matures_at after a subselect in the SET list", outside,
			"package mfa\n\nvar q = `UPDATE t SET a = (SELECT 1 FROM x), matures_at = $2 WHERE id = $1`\n", gsRuleMaturesAssign},
		{"matures_at after a subselect that has its own WHERE", outside,
			"package mfa\n\nvar q = `UPDATE users SET a = (SELECT 1 FROM x WHERE y = 1), password_matures_at = $2 WHERE id = $1`\n", gsRuleMaturesAssign},
		{"matures_at in a row constructor", outside,
			"package mfa\n\nvar q = `UPDATE users SET (name, matures_at) = ($1, $2) WHERE id = $3`\n", gsRuleMaturesAssign},
		{"a quoted matures_at column", outside,
			"package mfa\n\nvar q = `UPDATE users SET \"password_matures_at\" = $1 WHERE id = $2`\n", gsRuleMaturesAssign},
		{"matures_at in an upsert", outside,
			"package mfa\n\nvar q = `INSERT INTO t (id) VALUES ($1) ON CONFLICT (id) DO UPDATE SET matures_at = EXCLUDED.matures_at`\n", gsRuleMaturesAssign},
		{"matures_at in an INSERT column list", outside,
			"package mfa\n\nvar q = `INSERT INTO user_sso_identities (id, matures_at) VALUES ($1, $2)`\n", gsRuleMaturesAssign},
		{"matures_at in a MERGE update", outside,
			"package mfa\n\nvar q = `MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET matures_at = s.m`\n", gsRuleMaturesAssign},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			got := gsPipeline(t, map[string][]byte{tc.path: []byte(tc.src)})
			require.NotEmpty(t, got, "the pipeline must flag this source")
			var rules []string
			for _, f := range got {
				rules = append(rules, f.rule)
			}
			assert.Contains(t, rules, tc.rule)
		})
	}

	clean := []struct{ name, path, src string }{
		{"a guard-table write inside the package", inside,
			"package accountguard\n\nvar q = `DELETE FROM account_pending_requests WHERE id = $1`\n"},
		{"a CopyIn and a TRUNCATE inside the package", inside,
			"package accountguard\n\nvar q = pq.CopyIn(\"account_pending_requests\", \"id\")\nvar r = `TRUNCATE account_pending_requests`\n"},
		{"a matures_at write inside decide/", "internal/accountguard/decide/write.go",
			"package decide\n\nvar q = `UPDATE users SET password_matures_at = $2 WHERE id = $1`\n"},
		{"a locking read of a guard table", outside,
			"package mfa\n\nvar q = `SELECT id FROM account_pending_requests WHERE id = $1 FOR UPDATE`\n"},
		{"FOR UPDATE OF a guard table", outside,
			"package mfa\n\nvar q = `SELECT u.id FROM users u JOIN account_pending_requests r ON r.user_id = u.id FOR UPDATE OF u`\n"},
		{"a plain read of a guard table", outside,
			"package mfa\n\nvar q = `SELECT count(*) FROM user_credential_history WHERE user_id = $1`\n"},
		{"a CopyIn on another table", outside,
			"package mfa\n\nvar q = pq.CopyIn(\"user_events\", \"id\")\n"},
		{"a commented-out write", outside,
			"package mfa\n\nvar q = \"SELECT 1 -- DELETE FROM account_pending_requests\\nWHERE x = 1\"\n"},
		{"a write to another table", outside,
			"package mfa\n\nvar q = `UPDATE users SET name = $2 WHERE id = $1`\n"},
		{"a name that merely begins like a guard table", outside,
			"package mfa\n\nvar q = `UPDATE account_pending_requests_archive SET x = 1`\n"},
		{"a matures_at read", outside,
			"package mfa\n\nvar q = `SELECT matures_at FROM users WHERE matures_at <= now()`\n"},
		{"matures_at only in the WHERE of a write", outside,
			"package mfa\n\nvar q = `UPDATE users SET name = $2 WHERE matures_at = $1`\n"},
		{"matures_at only read inside a subselect of the SET list", outside,
			"package mfa\n\nvar q = `UPDATE t SET a = (SELECT matures_at FROM u WHERE id = 1) WHERE id = 2`\n"},
		{"matures_at only read on the right of a SET assignment", outside,
			"package mfa\n\nvar q = `UPDATE t SET last_seen = EXCLUDED.matures_at WHERE id = 2`\n"},
		{"matures_at named inside a quoted string of the SET list", outside,
			"package mfa\n\nvar q = `UPDATE t SET note = 'a, matures_at = b' WHERE id = 2`\n"},
		{"a column that merely ends in the name", outside,
			"package mfa\n\nvar q = `UPDATE users SET not_matures_at_all = $2 WHERE id = $1`\n"},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, gsPipeline(t, map[string][]byte{tc.path: []byte(tc.src)}))
		})
	}

	t.Run("the allowlist exempts a matures_at file but never a table write", func(t *testing.T) {
		allow := map[string]string{"internal/mfa/write.go": "a planted reason that is long enough"}
		src := []byte("package mfa\n\nvar a = `UPDATE users SET matures_at = $1`\nvar b = `DELETE FROM account_pending_requests`\n")
		violations, used := gsApplyMaturesAllow(gsPipeline(t, map[string][]byte{"internal/mfa/write.go": src}), allow)
		require.Len(t, violations, 1)
		assert.Contains(t, violations[0], "account_pending_requests")
		assert.Empty(t, gsStale(allow, used))
	})
}

func TestScan_SingleWriterRatchet_StalenessIsDetected(t *testing.T) {
	allow := map[string]string{"internal/gone/file.go": "a planted entry nothing in the tree needs any more"}
	assert.Equal(t, []string{"internal/gone/file.go"}, gsStale(allow, map[string]bool{}),
		"an allowlist entry the scan never used is stale")
	assert.Empty(t, gsStale(allow, map[string]bool{"internal/gone/file.go": true}))
}

// ---------------------------------------------------------------------------------------
// Scan 4: every place an error's text can leave the runner and the registration stamp.
// ---------------------------------------------------------------------------------------

// gsRedactSite names an operand that is not redacted by construction but is allowed because it
// comes back already redacted from this package's own code: the file (base name), the enclosing
// function, and the source the operand was assigned from (the callee of its assignment).
type gsRedactSite struct{ file, fn, source string }

// gsWithholdAllowMax pins the allowlist's size. It is a ratchet: it may only shrink, an entry
// the scan no longer needs fails the staleness assertion, and raising this number needs a
// controller ruling.
const gsWithholdAllowMax = 36

// gsWithholdAllow is the committed allowlist (ruling 4(d), widened by ruling 7 to returns).
// Keyed by the SOURCE as well as the function, so an allowlisted function that later wraps a
// raw driver error from a different call still fails. It covers every leg: %w, text verbs,
// errors.Join, log attributes and returned errors use the same operand rules.
var gsWithholdAllow = map[gsRedactSite]string{
	{"runner.go", "Run", "r.firstPass"}:                                       gsReasonRelay,
	{"runner.go", "Run", "r.secondPass"}:                                      gsReasonRelay,
	{"runner.go", "Run", "r.ready"}:                                           gsReasonRelay,
	{"runner.go", "firstPass", "r.decideTx"}:                                  gsReasonRelay,
	{"runner.go", "firstPass", "r.reg.covers"}:                                gsReasonRelay,
	{"runner.go", "firstPass", "r.applyTx"}:                                   gsReasonRelay,
	{"runner.go", "firstPass", "r.commitUnknown"}:                             gsReasonRelay,
	{"runner.go", "secondPass", "r.plainSecond"}:                              gsReasonRelay,
	{"runner.go", "secondPass", "r.forcedSecond"}:                             gsReasonRelay,
	{"runner.go", "secondPass", "r.d.Presence.WithReadySenderModeWithin"}:     gsReasonSender,
	{"runner.go", "plainSecond", "r.prepareSecondTx"}:                         gsReasonRelay,
	{"runner.go", "plainSecond", "r.commitUnknown"}:                           gsReasonRelay,
	{"runner.go", "forcedSecond", "r.prepareSecondTx"}:                        gsReasonRelay,
	{"runner.go", "forcedSecond", "r.commitUnknown"}:                          gsReasonRelay,
	{"runner.go", "prepareSecondTx", "r.decideTx"}:                            gsReasonRelay,
	{"runner.go", "prepareSecondTx", "r.reg.covers"}:                          gsReasonRelay,
	{"runner.go", "prepareSecondTx", "r.applyTx"}:                             gsReasonRelay,
	{"runner.go", "decideTx", "lockAuthorityTx"}:                              gsReasonRelay,
	{"runner.go", "decideTx", "refuseActivatedTx"}:                            gsReasonRelay,
	{"runner.go", "decideTx", "lockAddressesTx"}:                              gsReasonRelay,
	{"runner.go", "lockAddressesTx", "lockAddressTx"}:                         gsReasonRelay,
	{"runner.go", "scanAddressKey", "rows.Scan"}:                              gsReasonScan,
	{"runner.go", "applyTx", "r.reg.finish"}:                                  gsReasonRelay,
	{"runner.go", "rotateFamiliesTx", "loginfamily.CaptureEpochTargetsTx"}:    gsReasonRotate,
	{"runner.go", "rotateFamiliesTx", "op.RotateTx"}:                          gsReasonRotate,
	{"runner.go", "rotateFamiliesTx", "loginfamily.RevokeFamiliesForEpochTx"}: gsReasonRotate,
	{"runner.go", "commitPlainTx", "r.commitPlain"}:                           gsReasonCommit,
	{"runner.go", "commitPlainTx", "tx.Commit"}:                               gsReasonCommit,
	{"runner.go", "withhold", "&withheld{…}"}:                                 gsReasonHelper,
	{"runner.go", "Unwrap", "w.cause"}:                                        gsReasonHelper,
	{"registration.go", "EnrolAtRegistrationTx", "refuseActivatedTx"}:         gsReasonRelay,
	{"registration.go", "EnrolAtRegistrationTx", "lockAddressTx"}:             gsReasonRelay,
	{"registration.go", "EnrolAtRegistrationTx", "execOne"}:                   gsReasonRelay,
	{"registry.go", "covers", "keyOf"}:                                        gsReasonRelay,
	{"registry.go", "covers", "r.wiredFor"}:                                   gsReasonRelay,
	{"registry.go", "covers", "holdsUnique"}:                                  gsReasonRelay,
}

// The reasons. Each is a claim a reviewer can check against the named function, and each relay
// is itself held to these rules, so a chain of relays ends at withhold, a sentinel, or one of
// the raw-by-design cases below.
const (
	gsReasonRelay = "a relay of a function in the scanned files (or of registry.covers/finish and their helpers) whose own " +
		"returned errors are judged by this same scan: withhold, a sentinel, or another reasoned relay, so nothing raw passes through"
	gsReasonSender = "WithSender returns its own errors only before work runs, and those take secondPass's wrapped !started " +
		"branch; after work runs the error is work()'s, the runner's own already-withheld error (Task 10 re-review trace)"
	gsReasonScan = "the rows.Scan error of the queryRows scan callback; its only user is lockAddressesTx, which wraps " +
		"queryRows' error in withhold(...)"
	gsReasonRotate = "a raw loginfamily or fence error; rotateFamiliesTx has one caller, prepareSecondTx, which wraps its " +
		"result in withhold(...) (pass 2: rotate)"
	gsReasonCommit = "the raw commit error, ambiguous by design; commitPlainTx's callers hand it to commitUnknown, which " +
		"returns it only inside loginfamily.NewError(ReasonCommitUnknown, withhold(err))"
	gsReasonHelper = "the redaction helper itself: withhold builds the wrapper, and Unwrap returns the cause for errors.Is " +
		"and errors.As only (its Error() text is a fixed class, never the cause)"
)

var gsVerbRE = regexp.MustCompile(`%[-+# 0-9.*]*[a-zA-Z%]`)

type gsWithholdResult struct {
	findings []string
	used     map[gsRedactSite]bool
	wraps    int // %w operands examined
	redacted int // operands judged, of those redacted by construction (rules a, b, c)
	logCalls int // log calls whose arguments were examined
	returns  int // returned errors judged
}

// gsCfg is what one scan needs beyond the source: the allowlist, the package's sentinel
// errors, and, when only is set, the one set of functions to check (the rest of the file is
// skipped).
type gsCfg struct {
	allow       map[gsRedactSite]string
	sentinels   map[string]bool
	only        map[string]bool
	passThrough map[string]int
}

// gsPassThrough is the committed table of functions that return one of their parameters
// unchanged (callee name to the index of that parameter among the call's arguments). Their own
// return is exempt, and every call site judges the argument instead, as if it were returned
// there. The tree test derives the table from the source and requires it to equal this one, so
// a new pass-through function cannot slip in unjudged.
var gsPassThrough = map[string]int{"withLockConflict": 0, "settle": 2}

// gsFixtureCfg is the configuration planted fixtures run under: the real sentinels' names.
func gsFixtureCfg(allow map[gsRedactSite]string) gsCfg {
	return gsCfg{allow: allow, passThrough: gsPassThrough, sentinels: map[string]bool{
		"ErrContract": true, "ErrActivated": true, "ErrLockConflict": true, "ErrEffectUnwired": true,
	}}
}

// gsImports maps a file's local import names to their paths. A dot import has no local name;
// its path is returned in dots. A blank import binds nothing.
func gsImports(f *ast.File) (names map[string]string, dots []string) {
	names = make(map[string]string)
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		switch {
		case spec.Name == nil:
			names[path.Base(p)] = p
		case spec.Name.Name == ".":
			dots = append(dots, p)
		case spec.Name.Name != "_":
			names[spec.Name.Name] = p
		}
	}
	return names, dots
}

// gsPackageSentinels returns the names of the package-level vars initialised by
// errors.New("constant"): the only identifiers rule (b) trusts. A var that is declared without
// a value, built by anything else, or given a dynamic message is not a sentinel. files are the
// package's non-test sources.
func gsPackageSentinels(t *testing.T, files map[string][]byte) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	for p, body := range files {
		_, f := gsParse(t, p, body)
		imp, _ := gsImports(f)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					call, isCall := vs.Values[i].(*ast.CallExpr)
					if !strings.HasPrefix(name.Name, "Err") || !isCall || len(call.Args) != 1 {
						continue
					}
					sel, isSel := call.Fun.(*ast.SelectorExpr)
					if !isSel || sel.Sel.Name != "New" {
						continue
					}
					if pkg, isID := sel.X.(*ast.Ident); !isID || imp[pkg.Name] != "errors" {
						continue
					}
					if _, constant := gsConstString(call.Args[0], nil); constant {
						out[name.Name] = true
					}
				}
			}
		}
	}
	return out
}

func gsEnclosingFunc(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if fd, ok := stack[i].(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
	}
	return "<package>"
}

// gsEnclosingFuncType is the signature of the innermost function the stack is inside.
func gsEnclosingFuncType(stack []ast.Node) *ast.FuncType {
	for i := len(stack) - 1; i >= 0; i-- {
		switch fn := stack[i].(type) {
		case *ast.FuncLit:
			return fn.Type
		case *ast.FuncDecl:
			return fn.Type
		}
	}
	return nil
}

func gsFieldsName(fl *ast.FieldList, name string) bool {
	if fl == nil {
		return false
	}
	for _, f := range fl.List {
		for _, n := range f.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

// gsRHS describes what an assignment bound its i-th left-hand side to: the callee of a call,
// else the expression as written.
func gsRHS(a *ast.AssignStmt, i int) string {
	e := a.Rhs[0]
	if len(a.Rhs) == len(a.Lhs) {
		e = a.Rhs[i]
	}
	return gsExprSource(e)
}

// gsExprSource names what a value comes from: the callee of a call, else the expression.
func gsExprSource(e ast.Expr) string {
	if call, ok := e.(*ast.CallExpr); ok {
		return types.ExprString(call.Fun)
	}
	return types.ExprString(e)
}

// gsDirectBinding reports what statement s binds name to when s is itself a binding of name at
// its own block level: `x, err := f()`, `err = f()` or `var err error`.
func gsDirectBinding(name string, s ast.Stmt) (string, bool) {
	switch st := s.(type) {
	case *ast.AssignStmt:
		for i, l := range st.Lhs {
			if id, ok := l.(*ast.Ident); ok && id.Name == name {
				return gsRHS(st, i), true
			}
		}
	case *ast.DeclStmt:
		if gen, ok := st.Decl.(*ast.GenDecl); ok && gen.Tok == token.VAR {
			for _, spec := range gen.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for i, n := range vs.Names {
						if n.Name != name {
							continue
						}
						// `var x, err = f()` binds from its value, as an assignment does.
						switch {
						case len(vs.Values) == len(vs.Names):
							return gsExprSource(vs.Values[i]), true
						case len(vs.Values) == 1:
							return gsExprSource(vs.Values[0]), true
						}
						return "var", true
					}
				}
			}
		}
	}
	return "", false
}

// gsNestedAssigns returns the sources of every `name = ...` (not `:=`, which declares a new
// variable in its own scope) anywhere inside s: a reassignment in a nested if, for, switch or
// closure body, which still rebinds the outer variable.
func gsNestedAssigns(name string, s ast.Stmt) []string {
	var out []string
	ast.Inspect(s, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x.Tok != token.ASSIGN {
				return true
			}
			for i, l := range x.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == name {
					out = append(out, gsRHS(x, i))
				}
			}
		case *ast.RangeStmt:
			if x.Tok == token.ASSIGN {
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if id, ok := e.(*ast.Ident); ok && id.Name == name {
						out = append(out, "range")
					}
				}
			}
		}
		return true
	})
	return out
}

// gsBlockBinding walks the statements of one block that finished before pos, in order. A direct
// binding replaces what came before it; a reassignment nested in an earlier statement adds a
// possible source. found reports a direct binding, which ends the search outward.
func gsBlockBinding(name string, pos token.Pos, list []ast.Stmt) (sources []string, found bool) {
	for _, s := range list {
		if s.End() > pos {
			break
		}
		if src, ok := gsDirectBinding(name, s); ok {
			sources, found = []string{src}, true
			continue
		}
		sources = append(sources, gsNestedAssigns(name, s)...)
	}
	return sources, found
}

// gsBinding resolves a local name at pos by walking the enclosing block chain, innermost first.
// It returns every source the name may hold there: its nearest direct binding in a block that
// precedes pos, plus every reassignment nested in a statement between that binding and pos. A
// parameter, receiver or named result is "param" and a range variable "range". An empty result
// means no function-local binding, a package-level name.
func gsBinding(name string, pos token.Pos, stack []ast.Node) []string {
	var acc []string
	done := func() []string {
		slices.Sort(acc)
		return slices.Compact(acc)
	}
	for i := len(stack) - 1; i >= 0; i-- {
		var (
			list []ast.Stmt
			init ast.Stmt
		)
		switch n := stack[i].(type) {
		case *ast.BlockStmt:
			list = n.List
		case *ast.CaseClause:
			list = n.Body
		case *ast.CommClause:
			list = n.Body
		case *ast.IfStmt:
			init = n.Init
		case *ast.ForStmt:
			// A loop carries a reassignment back to the top: every `=` anywhere in it may reach
			// a use earlier in the body.
			acc = append(acc, gsNestedAssigns(name, n)...)
			init = n.Init
		case *ast.SwitchStmt:
			init = n.Init
		case *ast.TypeSwitchStmt:
			init = n.Init
		case *ast.RangeStmt:
			acc = append(acc, gsNestedAssigns(name, n)...)
			for _, e := range []ast.Expr{n.Key, n.Value} {
				if id, ok := e.(*ast.Ident); ok && id.Name == name {
					acc = append(acc, "range")
					return done()
				}
			}
		case *ast.FuncLit:
			if gsFieldsName(n.Type.Params, name) {
				acc = append(acc, "param")
				return done()
			}
			if gsFieldsName(n.Type.Results, name) {
				acc = append(acc, "var") // a named result starts as nil
				return done()
			}
		case *ast.FuncDecl:
			if gsFieldsName(n.Type.Params, name) || gsFieldsName(n.Recv, name) {
				acc = append(acc, "param")
				return done()
			}
			if gsFieldsName(n.Type.Results, name) {
				acc = append(acc, "var")
				return done()
			}
		}
		if init != nil {
			if src, ok := gsDirectBinding(name, init); ok {
				acc = append(acc, src)
				return done()
			}
		}
		if list != nil {
			sources, found := gsBlockBinding(name, pos, list)
			acc = append(acc, sources...)
			if found {
				return done()
			}
		}
	}
	return done()
}

// gsSiteCtx is one node being checked: where it is, the file's imports, and where findings and
// allowlist uses go.
type gsSiteCtx struct {
	file  string
	fn    string
	line  int
	stack []ast.Node
	cfg   *gsCfg
	imp   map[string]string
	res   *gsWithholdResult
}

func (c *gsSiteCtx) fail(format string, a ...any) {
	c.res.findings = append(c.res.findings, fmt.Sprintf("%s:%d: in %s, %s", c.file, c.line, c.fn, fmt.Sprintf(format, a...)))
}

// bound reports whether name is a function-local binding at pos.
func (c *gsSiteCtx) bound(name string, pos token.Pos) bool {
	return len(gsBinding(name, pos, c.stack)) > 0
}

// pkgOf resolves the left side of a selector to the import path it names, or "". A name bound
// locally (a parameter called fmt) is not the package.
func (c *gsSiteCtx) pkgOf(x ast.Expr) string {
	id, ok := x.(*ast.Ident)
	if !ok || c.bound(id.Name, id.Pos()) {
		return ""
	}
	return c.imp[id.Name]
}

// callee returns the import path and name of a package-qualified callee, e.g. ("fmt", "Errorf").
func (c *gsSiteCtx) callee(call *ast.CallExpr) (string, string) {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		return c.pkgOf(sel.X), sel.Sel.Name
	}
	return "", ""
}

const (
	gsPathFmt    = "fmt"
	gsPathErrors = "errors"
	gsPathSlog   = "log/slog"
	gsPathLog    = "log"
)

// isWithholdCall is rule (a). withhold must resolve to the package-level function, so a local
// closure that shadows it is not one.
func (c *gsSiteCtx) isWithholdCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "withhold" && !c.bound("withhold", id.Pos())
}

// isSentinel is rule (b): a package-level var initialised by errors.New with a constant. A
// local that merely has such a name is bound, and is not a sentinel.
func (c *gsSiteCtx) isSentinel(id *ast.Ident) bool {
	return c.cfg.sentinels[id.Name] && !c.bound(id.Name, id.Pos())
}

// causeOK is the cause argument of loginfamily.NewError: itself rule (a), rule (b) or nil.
func (c *gsSiteCtx) causeOK(e ast.Expr) bool {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name == "nil" || c.isSentinel(id)
	}
	return c.isWithholdCall(e)
}

// redactedByConstruction is rules (a), (b) and (c) of ruling 4.
func (c *gsSiteCtx) redactedByConstruction(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.CallExpr:
		if c.isWithholdCall(x) {
			return true
		}
		pkg, name := c.callee(x)
		return name == "NewError" && strings.HasSuffix(pkg, "/internal/loginfamily") && len(x.Args) == 2 && c.causeOK(x.Args[1])
	case *ast.Ident:
		return c.isSentinel(x)
	}
	return false
}

// judgedOnItsOwn reports a call that builds an error from its own operands. Those operands are
// judged where the call is: fmt.Errorf, errors.New and errors.Join.
func (c *gsSiteCtx) judgedOnItsOwn(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if _, ok := c.passThroughIdx(call); ok {
		return true // its error argument is judged at this call site
	}
	pkg, name := c.callee(call)
	return pkg == gsPathFmt && name == "Errorf" || pkg == gsPathErrors && (name == "New" || name == "Join")
}

// calleeName is the name a call is made by, whatever its receiver: f(...) and x.y.f(...) are f.
func gsCalleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// passThroughIdx reports a call to a function in the pass-through table (a method or a plain
// function, never a package-qualified one) and the index of its error argument.
func (c *gsSiteCtx) passThroughIdx(call *ast.CallExpr) (int, bool) {
	if pkg, _ := c.callee(call); pkg != "" {
		return 0, false
	}
	idx, ok := c.cfg.passThrough[gsCalleeName(call)]
	return idx, ok
}

// gsParamNames flattens a parameter list into one name per parameter, "_" for unnamed ones.
func gsParamNames(fl *ast.FieldList) []string {
	var out []string
	if fl == nil {
		return out
	}
	for _, f := range fl.List {
		if len(f.Names) == 0 {
			out = append(out, "_")
		}
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// isPassThroughParam reports whether name is the pass-through parameter of the function the
// site is in: its own return of that parameter is exempt, because every call site judges the
// argument.
//
// The walk stops at a function literal: a closure's parameter that happens to share the name is
// that closure's own value, and its return is judged like any other (N-4).
func (c *gsSiteCtx) isPassThroughParam(name string) bool {
	for i := len(c.stack) - 1; i >= 0; i-- {
		switch fn := c.stack[i].(type) {
		case *ast.FuncLit:
			return false
		case *ast.FuncDecl:
			idx, ok := c.cfg.passThrough[fn.Name.Name]
			params := gsParamNames(fn.Type.Params)
			return ok && idx < len(params) && params[idx] == name
		}
	}
	return false
}

// judge applies the rules to one operand that carries an error's text somewhere: a %w, a text
// verb, an errors.Join argument, a log attribute, a returned error. It is nil, redacted by
// construction (rules a, b, c), built from judged operands, or allowlisted by (file, func,
// source) for EVERY source it may hold there; otherwise a finding. how names the position.
func (c *gsSiteCtx) judge(operand ast.Expr, how string) {
	if id, ok := operand.(*ast.Ident); ok && id.Name == "nil" {
		return
	}
	if c.redactedByConstruction(operand) {
		c.res.redacted++
		return
	}
	if c.judgedOnItsOwn(operand) {
		return
	}
	sources := []string{types.ExprString(operand)}
	if call, ok := operand.(*ast.CallExpr); ok {
		sources = []string{types.ExprString(call.Fun)}
	}
	if id, ok := operand.(*ast.Ident); ok {
		sources = gsBinding(id.Name, id.Pos(), c.stack)
		if len(sources) == 0 {
			sources = []string{"unbound:" + id.Name}
		}
	}
	for _, source := range sources {
		if source == "var" {
			continue // declared and never assigned: nil
		}
		if id, isIdent := operand.(*ast.Ident); isIdent && source == "param" && c.isPassThroughParam(id.Name) {
			continue
		}
		if id, isIdent := operand.(*ast.Ident); isIdent && source == "withhold" && !c.bound("withhold", id.Pos()) {
			continue // bound from withhold(...) itself: redacted by construction
		}
		site := gsRedactSite{file: c.file, fn: c.fn, source: source}
		if _, allowed := c.cfg.allow[site]; allowed {
			c.res.used[site] = true
			continue
		}
		c.fail("%s operand `%s` (from %s) is not redacted: wrap it in withhold, or add a reasoned allowlist entry",
			how, types.ExprString(operand), source)
	}
}

// gsErrName reports whether an identifier's name says it holds an error: err, serr, workErr,
// completionError. No type checker runs here, so this is a naming heuristic, in the way the
// precedent scans key on identifier names.
func gsErrName(n string) bool {
	lower := strings.ToLower(n)
	return strings.HasPrefix(n, "Err") || strings.HasSuffix(lower, "err") || strings.HasSuffix(lower, "error")
}

// gsErrorShaped reports whether an operand looks like an error value: an error-named
// identifier or field (err, completion.Err), a call to an error-named function (subjectError),
// or err.Error().
func gsErrorShaped(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return gsErrorShaped(x.X)
	case *ast.Ident:
		return gsErrName(x.Name)
	case *ast.SelectorExpr:
		return gsErrName(x.Sel.Name)
	case *ast.CallExpr:
		switch fun := x.Fun.(type) {
		case *ast.Ident:
			return gsErrName(fun.Name)
		case *ast.SelectorExpr:
			return gsErrName(fun.Sel.Name)
		}
	}
	return false
}

// gsIsErrorCall reports x.Error() with no arguments.
func gsIsErrorCall(e ast.Expr) (*ast.CallExpr, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return nil, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return call, ok && sel.Sel.Name == "Error"
}

// gsErrorText judges an operand formatted as text when it looks like an error. An .Error() call
// is judged by the universal .Error() check, wherever it sits.
func gsErrorText(c *gsSiteCtx, operand ast.Expr, how string) {
	if _, isErrorCall := gsIsErrorCall(operand); isErrorCall {
		return
	}
	if gsErrorShaped(operand) {
		c.judge(operand, how)
	}
}

// gsTextVerbs are the verbs that print an error's message.
const gsTextVerbs = "vsqxX"

// gsCheckFormat checks a fmt.Errorf or fmt.Sprintf call: a %w operand must be redacted, and
// so must any operand that looks like an error under a text verb (%v, %+v, %s, ...).
func gsCheckFormat(c *gsSiteCtx, call *ast.CallExpr, name string, first int) {
	if call.Ellipsis != token.NoPos || len(call.Args) <= first {
		c.fail("%s with spread arguments cannot be checked", name)
		return
	}
	format, ok := gsConstString(call.Args[first], nil)
	if !ok {
		c.fail("%s format is not a constant, so its operands cannot be checked", name)
		return
	}
	if strings.Contains(format, "%[") {
		c.fail("an indexed verb cannot be checked")
		return
	}
	idx := 0
	for _, verb := range gsVerbRE.FindAllString(format, -1) {
		last := verb[len(verb)-1]
		if last == '%' {
			continue
		}
		if strings.Contains(verb, "*") {
			c.fail("a * width cannot be checked")
			return
		}
		pos := idx
		idx++
		isWrap := last == 'w'
		if !isWrap && !strings.ContainsRune(gsTextVerbs, rune(last)) {
			continue
		}
		if first+1+pos >= len(call.Args) {
			c.fail("a %%%c verb has no operand", last)
			continue
		}
		operand := call.Args[first+1+pos]
		if isWrap {
			c.res.wraps++
			c.judge(operand, "a %w")
			continue
		}
		gsErrorText(c, operand, "a "+verb+" text")
	}
}

// gsLogMethods are the slog.Logger methods whose attributes can carry an error.
var gsLogMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true, "LogAttrs": true, "With": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
}

// gsLogKeys are the attribute keys that say the value is an error.
var gsLogKeys = map[string]bool{"error": true, "err": true, "cause": true}

// gsCheckLog checks a log call's or an attribute constructor's arguments. An argument that
// looks like an error, or one that follows an "error", "err" or "cause" key, must be redacted:
// the repo's logging rule is a fixed class, never an error's text. Constant strings are not
// errors.
func gsCheckLog(c *gsSiteCtx, call *ast.CallExpr) {
	for i, arg := range call.Args {
		if _, isLit := arg.(*ast.BasicLit); isLit {
			continue
		}
		keyed := false
		if i > 0 {
			if key, ok := gsConstString(call.Args[i-1], nil); ok && gsLogKeys[strings.ToLower(key)] {
				keyed = true
			}
		}
		if _, isErrorCall := gsIsErrorCall(arg); isErrorCall {
			continue // the universal .Error() check judges it
		}
		switch {
		case gsErrorShaped(arg):
			c.judge(arg, "a log attribute")
		case keyed:
			// A keyed value that does not look like an error (a Sprint, a field) is judged
			// directly: the key says it is one.
			c.judge(arg, "a log attribute under an error key,")
		}
	}
}

// gsCheckJoin checks errors.Join: every argument is an error, so each must be redacted.
func gsCheckJoin(c *gsSiteCtx, call *ast.CallExpr) {
	if call.Ellipsis != token.NoPos {
		c.fail("errors.Join with spread arguments cannot be checked")
		return
	}
	for _, arg := range call.Args {
		c.judge(arg, "an errors.Join")
	}
}

// gsCheckCall dispatches one call to the check that applies. Packages are matched by import
// path, so an aliased import cannot hide a call.
func gsCheckCall(c *gsSiteCtx, call *ast.CallExpr) {
	if recv, ok := gsIsErrorCall(call); ok {
		// x.Error() is x's text, wherever it appears: in a concatenation, an errors.New, a
		// Sprint. withhold(err).Error() is a fixed class and passes.
		if sel, isSel := recv.Fun.(*ast.SelectorExpr); isSel && gsErrorShaped(sel.X) {
			c.judge(sel.X, "an .Error() text of")
		}
		return
	}
	pkg, name := c.callee(call)
	if idx, ok := c.passThroughIdx(call); ok && !c.parentIsWithhold() {
		if idx < len(call.Args) {
			c.judge(call.Args[idx], "a pass-through argument of "+gsCalleeName(call)+", an")
		} else {
			// A multi-value call such as r.settle(f()) hides the error argument inside f's
			// results, so there is nothing to judge here.
			c.fail("a multi-value or short call into the pass-through function %s has no error argument at index %d to judge",
				gsCalleeName(call), idx)
		}
	}
	if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "panic" && len(call.Args) == 1 && !c.bound("panic", id.Pos()) {
		gsErrorText(c, call.Args[0], "a panic")
	}
	switch {
	case pkg == gsPathFmt && gsFmtFormatters[name] > 0:
		gsCheckFormat(c, call, "fmt."+name, gsFmtFormatters[name]-1)
	case pkg == gsPathFmt && gsFmtPrinters[name] > 0:
		for _, arg := range call.Args[min(gsFmtPrinters[name]-1, len(call.Args)):] {
			gsErrorText(c, arg, "a fmt."+name)
		}
	case pkg == gsPathLog && gsLogFormatters[name] > 0:
		gsCheckFormat(c, call, "log."+name, gsLogFormatters[name]-1)
	case pkg == gsPathLog && gsLogPrinters[name] > 0:
		for _, arg := range call.Args[min(gsLogPrinters[name]-1, len(call.Args)):] {
			gsErrorText(c, arg, "a log."+name)
		}
	case pkg == gsPathErrors && name == "Join":
		gsCheckJoin(c, call)
	case pkg == gsPathSlog && (name == "Any" || name == "String"):
		gsCheckLog(c, call)
	case gsLogMethods[name]:
		if len(call.Args) > 0 {
			c.res.logCalls++
			gsCheckLog(c, call)
		}
	}
}

// gsFmtFormatters are the fmt functions that take a format string, and gsFmtPrinters the ones
// that print their operands. The value is one more than the index of the first argument that
// belongs to the function itself: the format for a formatter, the first operand for a printer.
// Fprint* and Append* take a writer or buffer first.
//
// Printf, Print and Println write straight to stdout, and the stdlib log package's Print*,
// Fatal* and Panic* write unescaped text to stderr: both are sinks for an error's message just
// as the Sprint family is a route to one (N-5, observability.md). gsLogFormatters and
// gsLogPrinters are the stdlib log equivalents, matched by import path "log" and never "log/slog".
var gsFmtFormatters = map[string]int{"Errorf": 1, "Sprintf": 1, "Fprintf": 2, "Appendf": 2, "Printf": 1}
var gsFmtPrinters = map[string]int{"Sprint": 1, "Sprintln": 1, "Fprint": 2, "Fprintln": 2, "Append": 2, "Appendln": 2,
	"Print": 1, "Println": 1}
var gsLogFormatters = map[string]int{"Printf": 1, "Fatalf": 1, "Panicf": 1}
var gsLogPrinters = map[string]int{"Print": 1, "Println": 1, "Fatal": 1, "Fatalln": 1, "Panic": 1, "Panicln": 1}

// parentIsWithhold reports a call that is itself the argument of withhold(...): whatever it
// returns is redacted before it goes anywhere.
func (c *gsSiteCtx) parentIsWithhold() bool {
	if len(c.stack) < 2 {
		return false
	}
	parent, ok := c.stack[len(c.stack)-2].(*ast.CallExpr)
	return ok && c.isWithholdCall(parent)
}

// gsWatchedValues are the functions that may only be called, never held as values: a method
// value such as `wrap := fmt.Errorf` hides every later call from the call checks.
var gsWatchedValues = map[string]map[string]bool{
	gsPathFmt: {"Errorf": true, "Sprintf": true, "Sprint": true, "Sprintln": true, "Fprintf": true, "Fprint": true,
		"Fprintln": true, "Appendf": true, "Append": true, "Appendln": true, "Print": true, "Printf": true, "Println": true},
	gsPathErrors: {"Join": true},
	gsPathLog: {"Print": true, "Printf": true, "Println": true, "Fatal": true, "Fatalf": true, "Fatalln": true,
		"Panic": true, "Panicf": true, "Panicln": true},
}

// gsCheckValueUse flags a watched function used other than as the callee of a call.
func gsCheckValueUse(c *gsSiteCtx, sel *ast.SelectorExpr) {
	if len(c.stack) >= 2 {
		if call, ok := c.stack[len(c.stack)-2].(*ast.CallExpr); ok && call.Fun == sel {
			return
		}
	}
	if sel.Sel.Name == "Error" && gsErrorShaped(sel.X) {
		// err.Error held as a method value is err's text, called later out of sight.
		c.judge(sel.X, "an .Error method value of")
		return
	}
	pkg := c.pkgOf(sel.X)
	if !gsWatchedValues[pkg][sel.Sel.Name] {
		return
	}
	c.fail("%s.%s is used as a value, which hides its calls from the redaction checks", pkg, sel.Sel.Name)
}

// gsCheckReturn judges the error a return statement yields: the last result of a function whose
// last result is `error`, or the single call of `return f()`. A bare `return err` of a driver
// error is a violation, even inside an allowlisted function.
func gsCheckReturn(c *gsSiteCtx, ret *ast.ReturnStmt) {
	ft := gsEnclosingFuncType(c.stack)
	if ft == nil || ft.Results == nil {
		return
	}
	total := 0
	lastIsError := false
	for i, f := range ft.Results.List {
		total += max(1, len(f.Names))
		if id, ok := f.Type.(*ast.Ident); ok && i == len(ft.Results.List)-1 {
			lastIsError = id.Name == "error"
		}
	}
	if !lastIsError {
		return
	}
	switch len(ret.Results) {
	case 0:
		// A naked return yields the named error result as it stands.
		if name := gsNamedErrResult(ft); name != "" {
			c.res.returns++
			c.judge(&ast.Ident{Name: name, NamePos: ret.Pos()}, "a naked return of the named result")
		}
	case total:
		c.res.returns++
		c.judge(ret.Results[total-1], "a returned error")
	case 1:
		c.res.returns++
		c.judge(ret.Results[0], "a returned error")
	}
}

// gsNamedErrResult is the name of a function's last result when it is a named error, or "".
func gsNamedErrResult(ft *ast.FuncType) string {
	if ft == nil || ft.Results == nil || len(ft.Results.List) == 0 {
		return ""
	}
	last := ft.Results.List[len(ft.Results.List)-1]
	if id, ok := last.Type.(*ast.Ident); !ok || id.Name != "error" || len(last.Names) == 0 {
		return ""
	}
	return last.Names[len(last.Names)-1].Name
}

// gsCheckDeferredResult judges an assignment to a named error result made inside a deferred
// closure: it replaces whatever the function returned, so its right side is a returned error.
func gsCheckDeferredResult(c *gsSiteCtx, as *ast.AssignStmt) {
	if as.Tok != token.ASSIGN {
		return
	}
	var ft *ast.FuncType
	deferred := false
	for _, n := range c.stack {
		switch x := n.(type) {
		case *ast.FuncDecl:
			ft = x.Type
		case *ast.DeferStmt:
			deferred = true
		}
	}
	name := gsNamedErrResult(ft)
	if name == "" || !deferred {
		return
	}
	for i, l := range as.Lhs {
		if id, ok := l.(*ast.Ident); ok && id.Name == name {
			rhs := as.Rhs[0]
			if len(as.Rhs) == len(as.Lhs) {
				rhs = as.Rhs[i]
			}
			c.res.returns++
			c.judge(rhs, "a deferred write to the named result")
		}
	}
}

// gsWithholdScan checks, in one file, every place an error's text can leave: fmt.Errorf %w and
// text verbs, fmt.Sprintf/Sprint/Sprintln, errors.Join, log attributes, .Error() calls, and
// returned errors. file is the base name.
func gsWithholdScan(t *testing.T, file string, body []byte, cfg gsCfg) gsWithholdResult {
	t.Helper()
	fset, f := gsParse(t, file, body)
	imp, dots := gsImports(f)
	res := gsWithholdResult{used: make(map[gsRedactSite]bool)}
	for _, d := range dots {
		if d == gsPathFmt || d == gsPathErrors {
			res.findings = append(res.findings, fmt.Sprintf("%s: dot-imports %s, which hides its calls from the redaction checks", file, d))
		}
	}
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		if fd, ok := n.(*ast.FuncDecl); ok && cfg.only != nil && !cfg.only[fd.Name.Name] {
			stack = stack[:len(stack)-1]
			return false
		}
		c := &gsSiteCtx{file: file, fn: gsEnclosingFunc(stack), line: fset.Position(n.Pos()).Line,
			stack: stack, cfg: &cfg, imp: imp, res: &res}
		switch x := n.(type) {
		case *ast.CallExpr:
			gsCheckCall(c, x)
		case *ast.SelectorExpr:
			gsCheckValueUse(c, x)
		case *ast.ReturnStmt:
			gsCheckReturn(c, x)
		case *ast.AssignStmt:
			gsCheckDeferredResult(c, x)
		}
		return true
	})
	return res
}

// gsRegistryRedactionFuncs are the registry.go functions whose errors the runner relays through
// covers and finish. They are held to the same rules, so those allowlist reasons are checked.
var gsRegistryRedactionFuncs = map[string]bool{
	"covers": true, "finish": true, "keyOf": true, "wiredFor": true, "holdsUnique": true,
}

// gsDerivePassThrough finds the functions that return one of their error-typed-result's
// parameters unchanged, directly or through a call to another such function. The result maps
// each name to the parameter's index among the call arguments.
func gsDerivePassThrough(t *testing.T, files map[string][]byte) map[string]int {
	t.Helper()
	var decls []*ast.FuncDecl
	for p, body := range files {
		_, f := gsParse(t, p, body)
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				decls = append(decls, fd)
			}
		}
	}
	out := make(map[string]int)
	for changed := true; changed; {
		changed = false
		for _, fd := range decls {
			if _, done := out[fd.Name.Name]; done {
				continue
			}
			if idx := gsPassedParam(fd, out); idx >= 0 {
				out[fd.Name.Name] = idx
				changed = true
			}
		}
	}
	return out
}

// gsPassedParam returns the index of the parameter fd returns unchanged as its error, or -1.
func gsPassedParam(fd *ast.FuncDecl, known map[string]int) int {
	res := fd.Type.Results
	if res == nil || len(res.List) == 0 {
		return -1
	}
	if id, ok := res.List[len(res.List)-1].Type.(*ast.Ident); !ok || id.Name != "error" {
		return -1
	}
	params := gsParamNames(fd.Type.Params)
	paramIdx := func(e ast.Expr) int {
		if id, ok := e.(*ast.Ident); ok {
			return slices.Index(params, id.Name)
		}
		return -1
	}
	found := -1
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			if len(x.Results) == 0 {
				return true
			}
			operand := x.Results[len(x.Results)-1]
			if i := paramIdx(operand); i >= 0 {
				found = i
			}
			if call, ok := operand.(*ast.CallExpr); ok {
				if k, isKnown := known[gsCalleeName(call)]; isKnown && k < len(call.Args) {
					if i := paramIdx(call.Args[k]); i >= 0 {
						found = i
					}
				}
			}
		}
		return true
	})
	return found
}

func TestScan_RunnerWrapsWithhold(t *testing.T) {
	files := gsReadTree(t, gsPkgDir)
	own := make(map[string][]byte)
	for p, body := range files {
		if path.Dir(p) == gsPkgDir {
			own[p] = body
		}
	}
	sentinels := gsPackageSentinels(t, own)
	for _, name := range []string{"ErrContract", "ErrActivated", "ErrLockConflict", "ErrEffectUnwired"} {
		require.Contains(t, sentinels, name, "the package's sentinel errors are found by their errors.New initialiser")
	}

	derived := gsDerivePassThrough(t, map[string][]byte{
		"runner.go": files[gsRunnerFile], "registration.go": files[gsPkgDir+"/registration.go"],
	})
	assert.Equal(t, gsPassThrough, derived, "gsPassThrough must equal the functions that return a parameter unchanged")

	var total, redacted, logCalls, returns int
	used := make(map[gsRedactSite]bool)
	var findings []string
	for _, tc := range []struct {
		name string
		only map[string]bool
	}{
		{"runner.go", nil},
		{"registration.go", nil},
		{"registry.go", gsRegistryRedactionFuncs},
	} {
		body, ok := files[gsPkgDir+"/"+tc.name]
		require.True(t, ok, "the walk must reach %s", tc.name)
		res := gsWithholdScan(t, tc.name, body, gsCfg{allow: gsWithholdAllow, sentinels: sentinels, only: tc.only, passThrough: gsPassThrough})
		findings = append(findings, res.findings...)
		total += res.wraps
		redacted += res.redacted
		logCalls += res.logCalls
		returns += res.returns
		for k := range res.used {
			used[k] = true
		}
	}
	// The scan must have examined real code on every leg.
	require.Greater(t, total, 25, "the scan must examine the runner's and the stamp's %%w operands")
	require.Greater(t, redacted, 20, "most operands are withhold(...) calls or sentinels")
	require.GreaterOrEqual(t, logCalls, 6, "the scan must examine the runner's log calls (the log attribute leg); the three post-commit warnings share logDeferred")
	require.Greater(t, returns, 30, "the scan must examine the returned errors (the return leg)")
	assert.Empty(t, findings, "every error operand in runner.go, registration.go and the registry's relays must be redacted or allowlisted")
	assert.Empty(t, gsStale(gsWithholdAllow, used), "a stale gsWithholdAllow entry must be deleted: the allowlist only shrinks")
	require.LessOrEqual(t, len(gsWithholdAllow), gsWithholdAllowMax, "gsWithholdAllow may only shrink: a new entry needs a controller ruling")
	for site, reason := range gsWithholdAllow {
		assert.GreaterOrEqual(t, len(strings.TrimSpace(reason)), 40, "%v needs a real reason", site)
	}
}

func TestScan_RunnerWrapsWithhold_PlantedControls(t *testing.T) {
	const file = "runner.go"
	offenders := []struct{ name, src, want string }{
		{"a raw err after tx.ExecContext (the planted control of ruling 4)",
			gsHdr + "func stamp(ctx context.Context, tx *sql.Tx) error {\n" +
				"\t_, err := tx.ExecContext(ctx, \"x\")\n\tif err != nil {\n\t\treturn fmt.Errorf(\"x: %w\", err)\n\t}\n\treturn nil\n}\n",
			"tx.ExecContext"},
		{"a raw err from an if-init QueryRowContext",
			gsFn("if err := tx.QueryRowContext(ctx, q).Scan(&v); err != nil {\n\t_ = fmt.Errorf(\"x: %w\", err)\n}"),
			"tx.QueryRowContext(ctx, q).Scan"},
		{"the second operand of two %w is raw",
			gsFn("_, err := tx.ExecContext(ctx, q)\n_ = fmt.Errorf(\"%w: %w\", ErrLockConflict, err)"),
			"tx.ExecContext"},
		{"a selector operand",
			gsFn("_ = fmt.Errorf(\"pass 2: forced clear rolled back: %w\", completion.Err)"),
			"completion.Err"},
		{"loginfamily.NewError with a raw cause",
			gsFn("_, err := tx.ExecContext(ctx, q)\n_ = fmt.Errorf(\"x: %w\", loginfamily.NewError(loginfamily.ReasonSQLUnavailable, err))"),
			"loginfamily.NewError"},
		{"a local that merely looks like a sentinel",
			gsFn("ErrFake := tx.Commit()\n_ = fmt.Errorf(\"x: %w\", ErrFake)"),
			"tx.Commit"},
		{"a parameter",
			gsHdr + "func commitUnknown(pass string, err error) error {\n\treturn fmt.Errorf(\"%s: %w\", pass, err)\n}\n",
			"param"},
		{"a non-constant format",
			gsFn("_ = fmt.Errorf(format, withhold(err))"),
			"not a constant"},
		{"spread arguments",
			gsFn("_ = fmt.Errorf(\"x: %w\", args...)"),
			"spread"},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(nil))
			require.NotEmpty(t, res.findings, "the scan must flag this source")
			assert.Contains(t, strings.Join(res.findings, "\n"), tc.want)
		})
	}

	clean := []struct{ name, src string }{
		{"withhold(err)", gsFn("_, err := tx.ExecContext(ctx, q)\n_ = fmt.Errorf(\"x: %w\", withhold(err))")},
		{"a sentinel", gsFn("_ = fmt.Errorf(\"x: %w: wrote %d rows\", ErrContract, 3)")},
		{"a sentinel and a withhold", gsFn("_, err := tx.ExecContext(ctx, q)\n_ = fmt.Errorf(\"%w: %w\", ErrLockConflict, withhold(err))")},
		{"NewError over withhold",
			gsFn("_, err := tx.ExecContext(ctx, q)\n_ = fmt.Errorf(\"x: %w\", loginfamily.NewError(loginfamily.ReasonCommitUnknown, withhold(err)))")},
		{"NewError over a sentinel", gsFn("_ = fmt.Errorf(\"x: %w\", loginfamily.NewError(loginfamily.ReasonSQLUnavailable, ErrContract))")},
		{"NewError over nil", gsFn("_ = fmt.Errorf(\"x: %w\", loginfamily.NewError(loginfamily.ReasonSQLUnavailable, nil))")},
		{"no %w at all", gsFn("_ = fmt.Errorf(\"%s: wrote %d rows\", step, 3)")},
		{"a literal percent", gsFn("_ = fmt.Errorf(\"100%% of %s: %w\", step, withhold(err))")},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(nil))
			assert.Empty(t, res.findings)
		})
	}

	t.Run("an allowlist entry is keyed by file, function and source", func(t *testing.T) {
		src := gsHdr + "func pass(ctx context.Context, tx *sql.Tx) error {\n" +
			"\td, err := r.decideTx(ctx, tx)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"pass: %w\", err)\n\t}\n" +
			"\t_, err = tx.ExecContext(ctx, q)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"pass: %w\", err)\n\t}\n\t_ = d\n\treturn nil\n}\n"
		allow := map[gsRedactSite]string{{file: file, fn: "pass", source: "r.decideTx"}: "decideTx returns only withheld errors"}
		res := gsWithholdScan(t, file, []byte(src), gsFixtureCfg(allow))
		require.Len(t, res.findings, 1, "the allowlisted source passes; the raw driver error in the same function does not")
		assert.Contains(t, res.findings[0], "tx.ExecContext")
		assert.True(t, res.used[gsRedactSite{file: file, fn: "pass", source: "r.decideTx"}])

		other := map[gsRedactSite]string{{file: file, fn: "elsewhere", source: "r.decideTx"}: "a different function"}
		res = gsWithholdScan(t, file, []byte(src), gsFixtureCfg(other))
		assert.Len(t, res.findings, 2, "an entry for another function suppresses nothing")
		assert.Empty(t, res.used)
	})

	t.Run("staleness is detected", func(t *testing.T) {
		allow := map[gsRedactSite]string{{file: file, fn: "gone", source: "r.gone"}: "a planted entry nothing needs any more"}
		res := gsWithholdScan(t, file, []byte(gsFn("_ = fmt.Errorf(\"x: %w\", withhold(err))")), gsFixtureCfg(allow))
		assert.Len(t, gsStale(allow, res.used), 1)
	})
}

// The three other ways an error's text leaves the runner (ruling 2 of the Task 12 follow-up):
// a text verb, errors.Join, and a log attribute. Same rules, same function, same allowlist.
func TestScan_RunnerWrapsWithhold_TextJoinAndLogControls(t *testing.T) {
	const file = "runner.go"
	const exec = "_, err := tx.ExecContext(ctx, q)\n"
	offenders := []struct{ name, src, want string }{
		{"Errorf %v of a raw err", gsFn(exec + `_ = fmt.Errorf("x: %v", err)`), "tx.ExecContext"},
		{"Errorf %s of a raw err", gsFn(exec + `_ = fmt.Errorf("x: %s", err)`), "tx.ExecContext"},
		{"Errorf %+v of a raw err", gsFn(exec + `_ = fmt.Errorf("x: %+v", err)`), "tx.ExecContext"},
		{"Errorf: a sentinel %w beside a raw %v", gsFn(exec + `_ = fmt.Errorf("%w: %v", ErrContract, err)`), "tx.ExecContext"},
		{"Errorf %s of err.Error()", gsFn(exec + `_ = fmt.Errorf("x: %s", err.Error())`), "tx.ExecContext"},
		{"Sprintf %v of a raw err", gsFn(exec + `_ = fmt.Sprintf("failed: %v", err)`), "tx.ExecContext"},
		{"Sprintf %s of a selector", gsFn(`_ = fmt.Sprintf("failed: %s", completion.Err)`), "completion.Err"},
		{"Sprint of a raw err", gsFn(exec + `_ = fmt.Sprint(err)`), "tx.ExecContext"},
		{"Sprintln of a raw err", gsFn(exec + `_ = fmt.Sprintln("x", err)`), "tx.ExecContext"},
		{"Sprintf of the result of an error-named call", gsFn(`_ = fmt.Sprintf("x: %v", subjectError(serr))`), "subjectError"},
		{"errors.Join with a raw err", gsFn(exec + `_ = errors.Join(ErrContract, err)`), "tx.ExecContext"},
		{"errors.Join of a raw err alone", gsFn(exec + `_ = errors.Join(err)`), "tx.ExecContext"},
		{"errors.Join with spread arguments", gsFn(`_ = errors.Join(errs...)`), "spread"},
		{"a log attribute under the error key", gsFn(exec + `r.d.Log.Warn("x", "error", err)`), "tx.ExecContext"},
		{"a log attribute under the err key, after other attributes",
			gsFn(`r.d.Log.Warn("x", "failure_class", c, "err", completion.Err)`), "completion.Err"},
		{"an error-shaped value under another key", gsFn(exec + `r.d.Log.Warn("x", "detail", err)`), "tx.ExecContext"},
		{"err.Error() as an attribute", gsFn(exec + `r.d.Log.Warn("x", "error", err.Error())`), "tx.ExecContext"},
		{"a slog.Any attribute", gsFn(exec + `_ = slog.Any("error", err)`), "tx.ExecContext"},
		{"a keyed value that does not look like an error", gsFn(`l.Warn("x", "error", describe(x))`), "describe"},
		{"a renamed logger and a context method", gsFn(exec + `lg.ErrorContext(ctx, "x", "cause", err)`), "tx.ExecContext"},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(nil))
			require.NotEmpty(t, res.findings, "the scan must flag this source")
			assert.Contains(t, strings.Join(res.findings, "\n"), tc.want)
		})
	}

	clean := []struct{ name, src string }{
		{"Errorf %s and %d of plain values", gsFn(`_ = fmt.Errorf("%s: wrote %d rows", step, n)`)},
		{"Sprintf of a plain string", gsFn(`_ = fmt.Sprintf("%s: %v", step, count)`)},
		{"Sprintf %v of withhold(err)", gsFn(exec + `_ = fmt.Sprintf("x: %v", withhold(err))`)},
		{"Sprintf %v of a sentinel", gsFn(`_ = fmt.Sprintf("x: %v", ErrContract)`)},
		{"withhold(err).Error()", gsFn(exec + `_ = fmt.Errorf("x: %s", withhold(err).Error())`)},
		{"NewError over withhold, as text", gsFn(exec + `_ = fmt.Sprint(loginfamily.NewError(loginfamily.ReasonSQLUnavailable, withhold(err)))`)},
		{"errors.Join of sentinels and withhold", gsFn(exec + `_ = errors.Join(ErrContract, withhold(err), nil)`)},
		{"a log call of fixed classes", gsFn(`r.d.Log.Warn("x", "failure_class", failureRollback, "user_id", userID.String())`)},
		{"a log attribute that is withheld", gsFn(exec + `r.d.Log.Warn("x", "error", withhold(err))`)},
		{"a log attribute that is a sentinel", gsFn(`r.d.Log.Warn("x", "error", ErrContract)`)},
		{"a nil error attribute", gsFn(`r.d.Log.Warn("x", "error", nil)`)},
		{"slog.String of a class", gsFn(`_ = slog.String("failure_class", c)`)},
		{"a constant string under the error key", gsFn(`r.d.Log.Warn("x", "error", "fixed")`)},
		{"an Error method with no arguments is not a log call", gsFn(exec + `_ = withhold(err).Error()`)},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(nil))
			assert.Empty(t, res.findings)
		})
	}

	t.Run("the log leg is counted, so the tree test can see it ran", func(t *testing.T) {
		res := gsWithholdScan(t, file, []byte(gsFn(`r.d.Log.Warn("x", "failure_class", c)`)), gsFixtureCfg(nil))
		assert.Equal(t, 1, res.logCalls)
	})
}

// The review's bypasses (rulings 7 to 11): a returned error, a reassignment in a nested block,
// an aliased or held fmt, text built from .Error(), a shadowed withhold, a mutable Err var.
// Every case runs through the same gsWithholdScan the tree test uses.
func TestScan_RunnerWrapsWithhold_ReviewBypassControls(t *testing.T) {
	const file = "runner.go"
	const exec = "_, err := tx.ExecContext(ctx, q)\n"
	relay := func(fn, source string) map[gsRedactSite]string {
		return map[gsRedactSite]string{{file: file, fn: fn, source: source}: "a planted relay entry for the control"}
	}
	type bypass struct {
		name  string
		src   string
		allow map[gsRedactSite]string
		want  string
	}

	offenders := []bypass{
		// Ruling 7: the return leg.
		{"a bare return of a raw relayed callee error",
			gsHdr + "func (r *Runner) applyTx(ctx int) error {\n\tif err := r.reg.apply(ctx); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n",
			nil, "r.reg.apply"},
		{"a relay entry for one source does not cover a raw driver error returned beside it",
			gsHdr + "func (r *Runner) applyTx(ctx int) error {\n\t_, err := tx.ExecContext(ctx, q)\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn r.reg.finish(a)\n}\n",
			relay("applyTx", "r.reg.finish"), "tx.ExecContext"},
		{"return tx.Commit()", gsHdr + "func commitPlainTx(tx *sql.Tx) error {\n\treturn tx.Commit()\n}\n", nil, "tx.Commit"},
		{"a raw error returned from a closure",
			gsFn("f := func() error {\n" + exec + "\treturn err\n}\n_ = f"), nil, "tx.ExecContext"},
		{"the error of a multi-result return",
			gsHdr + "func f(ctx int) (int, error) {\n\t_, err := tx.ExecContext(ctx, q)\n\treturn 0, err\n}\n", nil, "tx.ExecContext"},
		{"return of a call to an unknown callee",
			gsHdr + "func f() error {\n\treturn r.helper()\n}\n", nil, "r.helper"},

		// Ruling 8: scope-aware binding. The reviewer's nested-block probe, and its kin.
		{"a reassignment in a nested if rebinds the identifier (the reviewer's probe)",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\td, err := r.decideTx(ctx, tx, req, keyID)\n\tif d.x {\n\t\t_, err = tx.ExecContext(ctx, q)\n\t}\n\tif err != nil {\n\t\treturn fmt.Errorf(\"pass 1: %w\", err)\n\t}\n\treturn nil\n}\n",
			relay("firstPass", "r.decideTx"), "tx.ExecContext"},
		{"a reassignment in a for body",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\td, err := r.decideTx(ctx)\n\tfor range d.items {\n\t\t_, err = tx.ExecContext(ctx, q)\n\t}\n\treturn fmt.Errorf(\"pass 1: %w\", err)\n}\n",
			relay("firstPass", "r.decideTx"), "tx.ExecContext"},
		{"a reassignment in a switch case",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\td, err := r.decideTx(ctx)\n\tswitch d.kind {\n\tcase 1:\n\t\t_, err = tx.ExecContext(ctx, q)\n\t}\n\treturn fmt.Errorf(\"pass 1: %w\", err)\n}\n",
			relay("firstPass", "r.decideTx"), "tx.ExecContext"},
		{"a reassignment in a closure",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\td, err := r.decideTx(ctx)\n\t_ = d\n\tfunc() {\n\t\t_, err = tx.ExecContext(ctx, q)\n\t}()\n\treturn fmt.Errorf(\"pass 1: %w\", err)\n}\n",
			relay("firstPass", "r.decideTx"), "tx.ExecContext"},

		// Ruling 9: imports resolved by path, and held functions.
		{"an aliased fmt",
			"package accountguard\n\nimport f \"fmt\"\n\nfunc x() {\n\t" + exec + "\t_ = f.Errorf(\"x: %w\", err)\n}\n", nil, "tx.ExecContext"},
		{"an aliased errors",
			"package accountguard\n\nimport e \"errors\"\n\nfunc x() {\n\t" + exec + "\t_ = e.Join(ErrContract, err)\n}\n", nil, "tx.ExecContext"},
		{"an aliased log/slog",
			"package accountguard\n\nimport sl \"log/slog\"\n\nfunc x() {\n\t" + exec + "\t_ = sl.Any(\"error\", err)\n}\n", nil, "tx.ExecContext"},
		{"a dot-import of fmt",
			"package accountguard\n\nimport . \"fmt\"\n\nfunc x() {\n\t_ = Errorf(\"x: %w\", err)\n}\n", nil, "dot-imports"},
		{"a dot-import of errors",
			"package accountguard\n\nimport . \"errors\"\n\nfunc x() {\n\t_ = Join(ErrContract, err)\n}\n", nil, "dot-imports"},
		{"a method value (the reviewer's probe)",
			gsFn(exec + "wrap := fmt.Errorf\n_ = wrap(\"x: %w\", err)"), nil, "used as a value"},
		{"errors.Join held as a value", gsFn("join := errors.Join\n_ = join(ErrContract)"), nil, "used as a value"},
		{"fmt.Sprintf as a package-level value", gsHdr + "var render = fmt.Sprintf\n", nil, "used as a value"},
		{"fmt.Errorf passed as an argument", gsFn("apply(fmt.Errorf)"), nil, "used as a value"},

		// Ruling 11, M2.
		{"errors.New of err.Error() text (the reviewer's probe)",
			gsFn(exec + "_ = errors.New(\"x: \" + err.Error())"), nil, "tx.ExecContext"},
		{"an .Error() text in a concatenated Errorf operand (the reviewer's probe)",
			gsFn(exec + "_ = fmt.Errorf(\"x: %s\", \"a: \"+err.Error())"), nil, "tx.ExecContext"},
		{"an .Error() text assigned to a string",
			gsFn(exec + "msg := err.Error()\n_ = msg"), nil, "tx.ExecContext"},
		{"a local closure that shadows withhold (the reviewer's probe)",
			gsFn(exec + "withhold := func(e error) error { return e }\n_ = fmt.Errorf(\"x: %w\", withhold(err))"), nil, "withhold(err)"},
		{"a parameter named withhold",
			gsHdr + "func g(withhold func(error) error) error {\n\t" + exec + "\treturn fmt.Errorf(\"x: %w\", withhold(err))\n}\n", nil, "withhold(err)"},
		{"an Err name that is not a package sentinel",
			gsFn("_ = fmt.Errorf(\"x: %w\", ErrMutable)"), nil, "ErrMutable"},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(tc.allow))
			require.NotEmpty(t, res.findings, "the scan must flag this source")
			assert.Contains(t, strings.Join(res.findings, "\n"), tc.want)
		})
	}

	clean := []bypass{
		{"returns of nil, a sentinel, a withhold and a wrapped error",
			gsHdr + "func f(ctx int) (int, error) {\n\t" + exec + "\tif err != nil {\n\t\treturn 0, withhold(err)\n\t}\n\tif ctx > 3 {\n\t\treturn 0, ErrContract\n\t}\n\tif ctx > 2 {\n\t\treturn 0, fmt.Errorf(\"x: %w\", withhold(err))\n\t}\n\treturn 1, nil\n}\n",
			nil, ""},
		{"a returned relay of an allowlisted source",
			gsHdr + "func decideTx(ctx int) error {\n\tif err := lockAuthorityTx(ctx); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n",
			relay("decideTx", "lockAuthorityTx"), ""},
		{"a return in a function that returns no error",
			gsHdr + "func f() bool {\n\t" + exec + "\t_ = err\n\treturn true\n}\n", nil, ""},
		{"a later direct binding replaces an earlier raw one",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\ttx, err := r.d.DB.BeginTx(ctx, nil)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"begin: %w\", withhold(err))\n\t}\n\t_ = tx\n\td, err := r.decideTx(ctx)\n\t_ = d\n\tif err != nil {\n\t\treturn fmt.Errorf(\"pass 1: %w\", err)\n\t}\n\treturn nil\n}\n",
			relay("firstPass", "r.decideTx"), ""},
		{"a := in a nested block declares a new variable and rebinds nothing",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\td, err := r.decideTx(ctx)\n\tif d.x {\n\t\terr := tx.Commit()\n\t\t_ = err\n\t}\n\treturn fmt.Errorf(\"pass 1: %w\", err)\n}\n",
			relay("firstPass", "r.decideTx"), ""},
		{"a nested reassignment from an allowlisted source",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\td, err := r.decideTx(ctx)\n\tif d.x {\n\t\t_, err = r.decideTx(ctx)\n\t}\n\treturn fmt.Errorf(\"pass 1: %w\", err)\n}\n",
			relay("firstPass", "r.decideTx"), ""},
		{"an aliased fmt used correctly",
			"package accountguard\n\nimport f \"fmt\"\n\nfunc x() error {\n\t" + exec + "\treturn f.Errorf(\"x: %w\", withhold(err))\n}\n", nil, ""},
		{"a call of fmt.Sprintf, not a value", gsFn("_ = fmt.Sprintf(\"%d rows\", 3)"), nil, ""},
		{"a text of a withheld error", gsFn(exec + "_ = errors.New(\"x: \" + withhold(err).Error())"), nil, ""},
		{"an .Error() on a value that is not error-shaped", gsFn("_ = report.Error()"), nil, ""},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(tc.allow))
			assert.Empty(t, res.findings)
			assert.Empty(t, gsStale(tc.allow, res.used), "an allowlisted relay in a clean control is used")
		})
	}

	t.Run("sentinels are package-level vars built by errors.New with a constant", func(t *testing.T) {
		src := "package accountguard\n\nimport (\n\t\"errors\"\n\t\"fmt\"\n\tER \"errors\"\n)\n\nvar (\n" +
			"\tErrA = errors.New(\"a\")\n\tErrB error\n\tErrC = fmt.Errorf(\"c\")\n\tErrD = errors.New(dynamic)\n" +
			"\tErrE = errors.Join(ErrA)\n\tErrG = ER.New(\"g\")\n\tother = errors.New(\"o\")\n)\n"
		got := gsPackageSentinels(t, map[string][]byte{"errors.go": []byte(src)})
		assert.Equal(t, map[string]bool{"ErrA": true, "ErrG": true}, got)
	})

	t.Run("only restricts the scan to the named functions", func(t *testing.T) {
		src := gsHdr + "func skip() error {\n\t" + exec + "\treturn err\n}\n\nfunc keep() error {\n\t" + exec + "\treturn err\n}\n"
		cfg := gsFixtureCfg(nil)
		cfg.only = map[string]bool{"keep": true}
		res := gsWithholdScan(t, "registry.go", []byte(src), cfg)
		require.Len(t, res.findings, 1)
		assert.Contains(t, res.findings[0], "in keep")
	})

	t.Run("a returned error is counted, so the tree test can see the leg ran", func(t *testing.T) {
		res := gsWithholdScan(t, file, []byte(gsHdr+"func f() error {\n\treturn nil\n}\n"), gsFixtureCfg(nil))
		assert.Equal(t, 1, res.returns)
	})
}

// The re-review's findings (rulings 13 and 14): a pass-through argument that nothing judged, a
// var declaration that carries a value, a named error result written by a naked return or a
// deferred closure, a loop that carries a reassignment to the top of its body, and the text
// routes the first rounds did not watch. Every case runs through the same gsWithholdScan.
func TestScan_RunnerWrapsWithhold_RereviewControls(t *testing.T) {
	const file = "runner.go"
	const exec = "_, err := tx.ExecContext(ctx, q)\n"
	relay := func(fn, source string) map[gsRedactSite]string {
		return map[gsRedactSite]string{{file: file, fn: fn, source: source}: "a planted relay entry for the control"}
	}
	type probe struct {
		name  string
		src   string
		allow map[gsRedactSite]string
		want  string
	}

	offenders := []probe{
		// I-A: the pass-through table judges the error argument at every call site.
		{"N3a: Run passes a raw err into settle",
			gsHdr + "func (r *Runner) Run(ctx int) (Result, error) {\n\t" + exec + "\tif err != nil {\n\t\treturn r.settle(req, Result{}, err)\n\t}\n\treturn Result{}, nil\n}\n",
			nil, "tx.ExecContext"},
		{"N3b: settle passes a raw err into withLockConflict",
			gsHdr + "func (r *Runner) settle(req Request, res Result, err error) (Result, error) {\n\t_, err2 := tx.ExecContext(ctx, q)\n\tif err2 != nil {\n\t\treturn Result{}, withLockConflict(err2)\n\t}\n\treturn res, nil\n}\n",
			nil, "tx.ExecContext"},
		{"a pass-through call used as a statement still has its argument judged",
			gsFn(exec + "_ = withLockConflict(err)"), nil, "tx.ExecContext"},
		{"a function outside the table that returns its parameter",
			gsHdr + "func relayErr(err error) error {\n\treturn err\n}\n", nil, "param"},

		// I-B: a var declaration with values binds from them.
		{"N5: var _, err = f(); return err",
			gsHdr + "func x() error {\n\tvar _, err = tx.ExecContext(ctx, q)\n\treturn err\n}\n", nil, "tx.ExecContext"},
		{"N5b: var _, err = f(); %w of err",
			gsHdr + "func x() error {\n\tvar _, err = tx.ExecContext(ctx, q)\n\treturn fmt.Errorf(\"x: %w\", err)\n}\n", nil, "tx.ExecContext"},
		{"a single-value var declaration",
			gsHdr + "func x() error {\n\tvar err = tx.Commit()\n\treturn err\n}\n", nil, "tx.Commit"},

		// I-C: named error results.
		{"N1: a naked return of a named result",
			gsHdr + "func (r *Runner) applyTx(ctx int) (err error) {\n\t_, err = tx.ExecContext(ctx, q)\n\treturn\n}\n", nil, "tx.ExecContext"},
		{"N2: a deferred closure assigns the named result",
			gsHdr + "func (r *Runner) applyTx(ctx int) (err error) {\n\tdefer func() {\n\t\tif rerr := tx.Rollback(); rerr != nil {\n\t\t\terr = rerr\n\t\t}\n\t}()\n\treturn nil\n}\n", nil, "tx.Rollback"},
		{"a naked return after a nested assignment to the named result",
			gsHdr + "func f(ctx int) (n int, err error) {\n\tif ctx > 1 {\n\t\t_, err = tx.ExecContext(ctx, q)\n\t}\n\treturn\n}\n", nil, "tx.ExecContext"},

		// M-D: a loop carries a reassignment to the top of its body.
		{"N4: a loop-carried reassignment",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\td, err := r.decideTx(ctx, tx, req, keyID)\n\tfor i := 0; i < 2; i++ {\n\t\tif err != nil {\n\t\t\treturn fmt.Errorf(\"pass 1: %w\", err)\n\t\t}\n\t\t_, err = tx.ExecContext(ctx, q)\n\t}\n\t_ = d\n\treturn nil\n}\n",
			relay("firstPass", "r.decideTx"), "tx.ExecContext"},
		{"a loop-carried reassignment in a range body",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\t_, err := r.decideTx(ctx)\n\tfor range ctx {\n\t\tif err != nil {\n\t\t\treturn fmt.Errorf(\"pass 1: %w\", err)\n\t\t}\n\t\t_, err = tx.ExecContext(ctx, q)\n\t}\n\treturn nil\n}\n",
			relay("firstPass", "r.decideTx"), "tx.ExecContext"},

		// M-E: the text routes that were not watched.
		{"N6: Fprintf of a raw err", gsFn(exec + "fmt.Fprintf(b, \"%v\", err)"), nil, "tx.ExecContext"},
		{"Fprint of a raw err", gsFn(exec + "fmt.Fprint(b, err)"), nil, "tx.ExecContext"},
		{"Fprintln of a raw err", gsFn(exec + "fmt.Fprintln(b, \"x\", err)"), nil, "tx.ExecContext"},
		{"N7: Appendf of a raw err", gsFn(exec + "_ = fmt.Appendf(nil, \"%v\", err)"), nil, "tx.ExecContext"},
		{"Append of a raw err", gsFn(exec + "_ = fmt.Append(nil, err)"), nil, "tx.ExecContext"},
		{"Appendln of a raw err", gsFn(exec + "_ = fmt.Appendln(nil, err)"), nil, "tx.ExecContext"},
		{"Fprintf held as a value", gsFn("w := fmt.Fprintf\n_ = w"), nil, "used as a value"},
		{"N10: err.Error held as a method value", gsFn(exec + "s := err.Error\n_ = s"), nil, "tx.ExecContext"},
		{"N12: panic(err)", gsFn(exec + "panic(err)"), nil, "tx.ExecContext"},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(tc.allow))
			require.NotEmpty(t, res.findings, "the scan must flag this source")
			assert.Contains(t, strings.Join(res.findings, "\n"), tc.want)
		})
	}

	clean := []probe{
		{"a pass-through call with a withheld argument",
			gsHdr + "func (r *Runner) Run(ctx int) (Result, error) {\n\t" + exec + "\tif err != nil {\n\t\treturn r.settle(req, Result{}, withhold(err))\n\t}\n\treturn Result{}, nil\n}\n", nil, ""},
		{"a pass-through call with an argument from an allowlisted source",
			gsHdr + "func (r *Runner) Run(ctx int) (Result, error) {\n\tfirst, err := r.firstPass(ctx)\n\t_ = first\n\treturn r.settle(req, Result{}, err)\n}\n",
			relay("Run", "r.firstPass"), ""},
		{"a pass-through function returns its own parameter, and its call passes it on",
			gsHdr + "func withLockConflict(err error) error {\n\tif err != nil {\n\t\treturn fmt.Errorf(\"%w: %w\", ErrLockConflict, err)\n\t}\n\treturn err\n}\n\nfunc (r *Runner) settle(req Request, res Result, err error) (Result, error) {\n\treturn Result{}, withLockConflict(err)\n}\n",
			nil, ""},
		{"a pass-through call inside withhold is redacted before it goes anywhere",
			gsHdr + "func lockAuthorityTx(serr error) error {\n\treturn fmt.Errorf(\"lock subject: %w\", withhold(subjectError(serr)))\n}\n", nil, ""},
		{"a var declaration with no value is nil", gsHdr + "func x() error {\n\tvar err error\n\treturn err\n}\n", nil, ""},
		{"a var declaration from an allowlisted source",
			gsHdr + "func decideTx(ctx int) error {\n\tvar err = lockAuthorityTx(ctx)\n\treturn err\n}\n", relay("decideTx", "lockAuthorityTx"), ""},
		{"a naked return of a named result that is never assigned",
			gsHdr + "func f() (n int, err error) {\n\treturn\n}\n", nil, ""},
		{"a naked return of a named result assigned from withhold",
			gsHdr + "func f(ctx int) (n int, err error) {\n\t_, e := tx.ExecContext(ctx, q)\n\terr = withhold(e)\n\treturn\n}\n", nil, ""},
		{"a deferred closure assigns a withheld error to the named result",
			gsHdr + "func f() (err error) {\n\tdefer func() {\n\t\tif rerr := tx.Rollback(); rerr != nil {\n\t\t\terr = withhold(rerr)\n\t\t}\n\t}()\n\treturn nil\n}\n", nil, ""},
		{"a deferred assignment to something that is not the named result",
			gsHdr + "func f() (err error) {\n\tvar other error\n\tdefer func() {\n\t\tother = tx.Rollback()\n\t}()\n\t_ = other\n\treturn nil\n}\n", nil, ""},
		{"a loop whose reassignment is also an allowlisted source",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\t_, err := r.decideTx(ctx)\n\tfor i := 0; i < 2; i++ {\n\t\tif err != nil {\n\t\t\treturn fmt.Errorf(\"pass 1: %w\", err)\n\t\t}\n\t\t_, err = r.decideTx(ctx)\n\t}\n\treturn nil\n}\n",
			relay("firstPass", "r.decideTx"), ""},
		{"a variable declared inside the loop body is not carried",
			gsHdr + "func (r *Runner) firstPass(ctx int) error {\n\tfor i := 0; i < 2; i++ {\n\t\t_, err := r.decideTx(ctx)\n\t\tif err != nil {\n\t\t\treturn fmt.Errorf(\"pass 1: %w\", err)\n\t\t}\n\t}\n\treturn nil\n}\n",
			relay("firstPass", "r.decideTx"), ""},
		{"Fprintf of plain values", gsFn("fmt.Fprintf(b, \"%d rows of %s\", 3, step)"), nil, ""},
		{"Fprintf of a withheld error", gsFn(exec + "fmt.Fprintf(b, \"%v\", withhold(err))"), nil, ""},
		{"Appendf of plain values", gsFn("_ = fmt.Appendf(nil, \"%d\", 3)"), nil, ""},
		{"the .Error method value of a withheld error", gsFn(exec + "s := withhold(err).Error\n_ = s"), nil, ""},
		{"panic with a constant string", gsFn("panic(\"unreachable\")"), nil, ""},
		{"panic with a withheld error", gsFn(exec + "panic(withhold(err))"), nil, ""},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(tc.allow))
			assert.Empty(t, res.findings)
			assert.Empty(t, gsStale(tc.allow, res.used), "an allowlisted relay in a clean control is used")
		})
	}

	t.Run("the pass-through table is derived from the source", func(t *testing.T) {
		src := gsHdr + "func direct(a int, err error) error {\n\treturn err\n}\n\n" +
			"func chained(x error, n int) error {\n\treturn direct(n, x)\n}\n\n" +
			"func wrapped(err error) error {\n\treturn fmt.Errorf(\"x: %w\", withhold(err))\n}\n\n" +
			"func notAnError(err error) bool {\n\treturn err != nil\n}\n\n" +
			"func inClosure(err error) error {\n\tf := func() error { return err }\n\t_ = f\n\treturn nil\n}\n"
		got := gsDerivePassThrough(t, map[string][]byte{"runner.go": []byte(src)})
		assert.Equal(t, map[string]int{"direct": 1, "chained": 0}, got)
	})
}

// The final whole-branch review's scan minors (task-12 ruling 16): a multi-value call into a
// pass-through callee, a closure parameter that shares a pass-through parameter's name, and the
// unescaped stdlib print and log sinks. Every case runs through the same gsWithholdScan the
// tree test uses; each offender is a planted positive, each clean case a negative.
func TestScan_RunnerWrapsWithhold_FinalReviewControls(t *testing.T) {
	const file = "runner.go"
	const exec = "_, err := tx.ExecContext(ctx, q)\n"
	// gsHdrStd is gsHdr plus the stdlib log package, which the printer sinks need imported.
	const gsHdrStd = "package accountguard\n\nimport (\n\t\"database/sql\"\n\t\"fmt\"\n\t\"log\"\n\t\"log/slog\"\n)\n\n"
	stdFn := func(stmts string) string { return gsHdrStd + "func fixture() {\n" + stmts + "\n}\n" }
	type probe struct{ name, src, want string }

	offenders := []probe{
		// N-1: the error argument is beyond the call's arguments, so it cannot be judged.
		{"N-1: a multi-value call into settle (the error is the third result)",
			gsHdr + "func (r *Runner) Run(ctx int) (Result, error) {\n\treturn r.settle(f())\n}\n", "multi-value"},
		{"N-1: settle with too few arguments",
			gsHdr + "func (r *Runner) Run(ctx int) (Result, error) {\n\treturn r.settle(req, Result{})\n}\n", "multi-value"},
		{"N-1: a multi-value call used as a statement",
			gsFn("_, _ = r.settle(f())"), "multi-value"},

		// N-4: a closure's own parameter does not inherit the enclosing function's exemption.
		{"N-4: a closure parameter named like the pass-through parameter",
			gsHdr + "func withLockConflict(err error) error {\n\t_ = func(err error) error {\n\t\treturn fmt.Errorf(\"x: %w\", err)\n\t}\n\treturn err\n}\n", "param"},
		{"N-4: a closure inside settle returns its own parameter",
			gsHdr + "func (r *Runner) settle(req Request, res Result, err error) (Result, error) {\n\tf := func(err error) error {\n\t\treturn err\n\t}\n\t_ = f\n\treturn res, err\n}\n", "param"},

		// N-5: the stdlib print and log sinks.
		{"N-5: fmt.Print of a raw err", stdFn(exec + "fmt.Print(err)"), "tx.ExecContext"},
		{"N-5: fmt.Println of a raw err", stdFn(exec + "fmt.Println(\"x\", err)"), "tx.ExecContext"},
		{"N-5: fmt.Printf %v of a raw err", stdFn(exec + "fmt.Printf(\"x: %v\\n\", err)"), "tx.ExecContext"},
		{"N-5: fmt.Printf with a non-constant format", stdFn("fmt.Printf(format, 1)"), "not a constant"},
		{"N-5: log.Print of a raw err", stdFn(exec + "log.Print(err)"), "tx.ExecContext"},
		{"N-5: log.Printf %v of a raw err", stdFn(exec + "log.Printf(\"x: %v\", err)"), "tx.ExecContext"},
		{"N-5: log.Println of a raw err", stdFn(exec + "log.Println(\"x\", err)"), "tx.ExecContext"},
		{"N-5: log.Fatal of a raw err", stdFn(exec + "log.Fatal(err)"), "tx.ExecContext"},
		{"N-5: log.Fatalf of a raw err", stdFn(exec + "log.Fatalf(\"x: %v\", err)"), "tx.ExecContext"},
		{"N-5: log.Fatalln of a raw err", stdFn(exec + "log.Fatalln(\"x\", err)"), "tx.ExecContext"},
		{"N-5: log.Panic of a raw err", stdFn(exec + "log.Panic(err)"), "tx.ExecContext"},
		{"N-5: log.Panicf of a raw err", stdFn(exec + "log.Panicf(\"x: %v\", err)"), "tx.ExecContext"},
		{"N-5: log.Panicln of a raw err", stdFn(exec + "log.Panicln(\"x\", err)"), "tx.ExecContext"},
		{"N-5: log.Printf of a selector", stdFn("log.Printf(\"x: %s\", completion.Err)"), "completion.Err"},
		{"N-5: fmt.Println held as a value", stdFn("p := fmt.Println\n_ = p"), "used as a value"},
		{"N-5: log.Printf held as a value", stdFn("p := log.Printf\n_ = p"), "used as a value"},
		{"N-5: an aliased stdlib log",
			"package accountguard\n\nimport lg \"log\"\n\nfunc x() {\n\t" + exec + "\tlg.Print(err)\n}\n", "tx.ExecContext"},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(nil))
			require.NotEmpty(t, res.findings, "the scan must flag this source")
			assert.Contains(t, strings.Join(res.findings, "\n"), tc.want)
		})
	}

	clean := []probe{
		{"N-1: settle with its error argument present and withheld",
			gsHdr + "func (r *Runner) Run(ctx int) (Result, error) {\n\t" + exec + "\treturn r.settle(req, Result{}, withhold(err))\n}\n", ""},
		{"N-1: a multi-value call inside withhold is redacted before it goes anywhere",
			gsFn("_ = withhold(r.settle(f()))"), ""},
		{"N-4: the pass-through function still returns its own parameter",
			gsHdr + "func withLockConflict(err error) error {\n\t_ = func() error { return fmt.Errorf(\"x: %w\", withhold(err)) }\n\treturn err\n}\n", ""},
		{"N-4: a closure that withholds its own parameter",
			gsHdr + "func withLockConflict(err error) error {\n\t_ = func(err error) error { return withhold(err) }\n\treturn err\n}\n", ""},
		{"N-5: fmt.Println of plain values", stdFn("fmt.Println(\"rows\", 3)"), ""},
		{"N-5: fmt.Printf of plain values", stdFn("fmt.Printf(\"%d rows of %s\\n\", 3, step)"), ""},
		{"N-5: fmt.Printf of a withheld error", stdFn(exec + "fmt.Printf(\"%v\\n\", withhold(err))"), ""},
		{"N-5: log.Printf of plain values", stdFn("log.Printf(\"%d rows\", 3)"), ""},
		{"N-5: log.Println of a sentinel", stdFn("log.Println(\"x\", ErrContract)"), ""},
		{"N-5: log.Fatalf of a withheld error", stdFn(exec + "log.Fatalf(\"x: %v\", withhold(err))"), ""},
		{"N-5: log.Print of a constant string", stdFn("log.Print(\"unreachable\")"), ""},
		{"N-5: log.SetFlags is not a sink", stdFn("log.SetFlags(0)\nlog.SetPrefix(\"x\")"), ""},
		{"N-5: a local named log is not the package",
			gsHdrStd + "func x(log func(...any)) {\n\t" + exec + "\tlog(err)\n}\n", ""},
		// The slog negatives: the structured logger the repo uses stays clean, and the new
		// stdlib rules do not reach it.
		{"slog.Info of fixed classes", stdFn("slog.Info(\"x\", \"failure_class\", c, \"user_id\", id)"), ""},
		{"slog.Warn of a withheld error", stdFn(exec + "slog.Warn(\"x\", \"error\", withhold(err))"), ""},
		{"slog.Error of a sentinel", stdFn("slog.Error(\"x\", \"error\", ErrContract)"), ""},
		{"slog.String of a class", stdFn("_ = slog.String(\"failure_class\", c)"), ""},
		{"a logger's Info of fixed classes", stdFn("r.d.Log.Info(\"x\", \"failure_class\", c)"), ""},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			res := gsWithholdScan(t, file, []byte(tc.src), gsFixtureCfg(nil))
			assert.Empty(t, res.findings)
		})
	}

	t.Run("a slog call with a raw error is still flagged beside the stdlib rules", func(t *testing.T) {
		res := gsWithholdScan(t, file, []byte(stdFn(exec+"slog.Warn(\"x\", \"error\", err)")), gsFixtureCfg(nil))
		require.NotEmpty(t, res.findings)
		assert.Contains(t, strings.Join(res.findings, "\n"), "tx.ExecContext")
	})
}

// A literal that begins with the word begin is prose, not a transaction.
func TestScan_OnlyRunnerOpensTransactions_ProseIsNotATransaction(t *testing.T) {
	const registration = "internal/accountguard/registration.go"
	for _, lit := range []string{"begin reading the stamp", "begin read the docs", "begin: wrapped", "Begin the pass", "start transaction handling"} {
		got, _ := gsTxScan(t, map[string][]byte{registration: []byte(gsFn(fmt.Sprintf("_ = %q", lit)))})
		assert.Empty(t, got, lit)
	}
	for _, lit := range []string{"BEGIN", "begin;", "BEGIN TRANSACTION", "begin isolation level serializable", "BEGIN READ ONLY", "begin work", "START TRANSACTION", "start transaction read write", "BEGIN NOT DEFERRABLE"} {
		got, _ := gsTxScan(t, map[string][]byte{registration: []byte(gsFn(fmt.Sprintf("_ = %q", lit)))})
		assert.NotEmpty(t, got, lit)
	}
}

// The matures-assign message carries the prose caveat.
func TestScan_SingleWriterRatchet_MaturesMessageNamesTheProseCaveat(t *testing.T) {
	got := gsPipeline(t, map[string][]byte{"internal/mfa/write.go": []byte("package mfa\n\nvar q = `UPDATE users SET matures_at = $1`\n")})
	require.Len(t, got, 1)
	assert.Contains(t, got[0].msg, "reword it")
}
