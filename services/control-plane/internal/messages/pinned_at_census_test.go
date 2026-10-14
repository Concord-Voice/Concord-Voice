package messages_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pinned_at writer census (#3459 §5, AC14).
//
// I-HELD says a held_expires_at exists only on a pinned row. The pin and unpin
// statements keep it true by moving the stamp in the SAME UPDATE that writes
// pinned_at, through the SET fragments internal/expiration exports. A future
// writer that sets or clears pinned_at without them would leave a stamp live
// under a pin that was meant to keep it (a kept pin deleted), or a held stamp
// under an unpinned row (the 000168 CHECK turns that into a 500). This test
// makes forgetting the fragment a build failure:
//
//   - every UPDATE under internal/, cmd/ and pkg/ whose top-level SET list
//     assigns pinned_at to a value must embed the table's hold fragment
//     (messages: ChannelPinHoldAssignments, dm_messages:
//     ConversationPinHoldAssignments) in that SET list;
//   - every one that assigns pinned_at = NULL must embed UnpinReleaseAssignments
//     in that SET list;
//   - an INSERT that names pinned_at, a tuple assignment, a CASE or NULL mixed
//     into the right-hand side, and an unknown table all fail, because no
//     fragment can be checked against them. Split the statement or extend this
//     census deliberately.
//
// How a statement is found. Each maximal string expression in a non-test file
// (a string literal, or a chain of "+") is evaluated to text: literals as-is,
// "+" as concatenation, identifiers through the package's const and var
// initialisers, and expiration.<Fragment> to the fragment's real value. Anything
// else (a function call such as purge.HiddenRangeFilter) becomes an opaque
// marker. A write is then read from the text: the top-level SET list of an
// UPDATE (outside parentheses, up to the first top-level WHERE, FROM or
// RETURNING), so a pinned_at in a WHERE clause or a subquery is a read.
//
// What it cannot see:
//   - SQL built at run time (fmt, strings.Builder, a variable assembled across
//     statements) and any text a call or a cross-package constant supplies. A
//     fmt.Sprintf format string is a literal and IS checked.
//   - SQL outside Go source (go:embed files, migrations). Migrations are the
//     schema, not the application's writers.
//   - That the fragment is correct for the statement's aliases. The integration
//     tests in pin_expiry_hold_integration_test.go prove each writer's effect.
//   - Order. It proves each writer carries the fragment, not that the parent
//     lock was taken first; the writers' lock order is reviewed, not parsed.

const (
	censusOpaque           = "?"
	censusExpirationImport = "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
)

// censusFragments is what expiration.<Name> evaluates to inside a statement.
var censusFragments = map[string]string{
	"ChannelPinHoldAssignments":      expiration.ChannelPinHoldAssignments,
	"ConversationPinHoldAssignments": expiration.ConversationPinHoldAssignments,
	"UnpinReleaseAssignments":        expiration.UnpinReleaseAssignments,
}

var (
	censusUpdateHead = regexp.MustCompile(`(?i)\bUPDATE (?:ONLY )?(?:PUBLIC\.)?(\w+)(?: (?:AS )?(\w+))? SET `)
	censusUpsertHead = regexp.MustCompile(`(?i)\bDO UPDATE SET `)
	censusInsertHead = regexp.MustCompile(`(?i)\bINSERT INTO (?:PUBLIC\.)?(\w+) ?\(([^)]*)\)`)
	censusMixedRHS   = regexp.MustCompile(`(?i)\b(?:NULL|CASE)\b`)
	censusSetEnd     = regexp.MustCompile(`(?i)^ (?:WHERE|FROM|RETURNING)(?: |$)`)
)

// censusWrite is one pinned_at write read out of a statement.
type censusWrite struct {
	table   string // lower-case target table
	kind    string // set, clear, mixed, tuple or insert
	setList string // normalised SET list; empty for an insert
}

func censusNormalise(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, `"`, "")), " ")
}

// censusWrites reads every pinned_at write out of one statement's text.
func censusWrites(raw string) []censusWrite {
	sql := censusNormalise(raw)
	var out []censusWrite
	for _, m := range censusUpdateHead.FindAllStringSubmatchIndex(sql, -1) {
		out = append(out, censusSetWrites(sql[m[1]:], strings.ToLower(sql[m[2]:m[3]]))...)
	}
	for _, m := range censusUpsertHead.FindAllStringIndex(sql, -1) {
		table := "?"
		if inserts := censusInsertHead.FindAllStringSubmatch(sql[:m[0]], -1); len(inserts) > 0 {
			table = strings.ToLower(inserts[len(inserts)-1][1])
		}
		out = append(out, censusSetWrites(sql[m[1]:], table)...)
	}
	for _, m := range censusInsertHead.FindAllStringSubmatch(sql, -1) {
		for _, column := range strings.Split(m[2], ",") {
			if strings.EqualFold(strings.TrimSpace(column), "pinned_at") {
				out = append(out, censusWrite{table: strings.ToLower(m[1]), kind: "insert"})
			}
		}
	}
	return out
}

// censusSetWrites reads the assignments of the SET list that starts at the
// beginning of afterSet and keeps those that name pinned_at.
func censusSetWrites(afterSet, table string) []censusWrite {
	setList := censusSetList(afterSet)
	var out []censusWrite
	for _, assignment := range censusSplitTop(setList, ',') {
		eq := censusIndexTop(assignment, '=')
		if eq < 0 {
			continue
		}
		lhs, rhs := strings.TrimSpace(assignment[:eq]), strings.TrimSpace(assignment[eq+1:])
		if !censusNamesColumn(lhs, "pinned_at") {
			continue
		}
		kind := "set"
		switch {
		case strings.HasPrefix(lhs, "("):
			kind = "tuple"
		case strings.EqualFold(rhs, "NULL"):
			kind = "clear"
		case censusMixedRHS.MatchString(rhs):
			kind = "mixed"
		}
		out = append(out, censusWrite{table: table, kind: kind, setList: setList})
	}
	return out
}

func censusNamesColumn(lhs, column string) bool {
	for _, name := range strings.Split(strings.Trim(lhs, "() "), ",") {
		if strings.EqualFold(strings.TrimSpace(name), column) {
			return true
		}
	}
	return false
}

// censusSetList keeps the text of an UPDATE's SET list that precedes the first
// top-level WHERE, FROM or RETURNING, a semicolon, or the parenthesis that
// closes an enclosing CTE.
func censusSetList(afterSet string) string {
	depth, quoted := 0, false
	for i := 0; i < len(afterSet); i++ {
		switch c := afterSet[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				return afterSet[:i]
			}
			depth--
		case depth == 0 && (c == ';' || (c == ' ' && censusSetEnd.MatchString(afterSet[i:]))):
			return afterSet[:i]
		}
	}
	return afterSet
}

// censusIndexTop returns the index of the first sep outside parentheses and
// quotes, or -1.
func censusIndexTop(s string, sep byte) int {
	if parts := censusSplitTopIndexes(s, sep); len(parts) > 0 {
		return parts[0]
	}
	return -1
}

func censusSplitTop(s string, sep byte) []string {
	var out []string
	last := 0
	for _, i := range censusSplitTopIndexes(s, sep) {
		out = append(out, s[last:i])
		last = i + 1
	}
	return append(out, s[last:])
}

func censusSplitTopIndexes(s string, sep byte) []int {
	var out []int
	depth, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && c == sep:
			out = append(out, i)
		}
	}
	return out
}

// censusProblems reports each pinned_at write in a statement that does not
// embed the fragment its table and direction require.
func censusProblems(sql string) []string {
	var problems []string
	for _, w := range censusWrites(sql) {
		if problem := censusCheck(w); problem != "" {
			problems = append(problems, problem)
		}
	}
	return problems
}

func censusCheck(w censusWrite) string {
	switch w.kind {
	case "insert":
		return fmt.Sprintf("INSERT INTO %s names pinned_at: a pinned insert cannot carry a hold fragment; insert unpinned and pin through the pin writer", w.table)
	case "tuple", "mixed":
		return fmt.Sprintf("UPDATE %s assigns pinned_at in a %s form this census cannot classify; use a plain `pinned_at = <value>` or `pinned_at = NULL` assignment", w.table, w.kind)
	}
	var name, fragment string
	switch {
	case w.kind == "clear" && (w.table == "messages" || w.table == "dm_messages"):
		name, fragment = "UnpinReleaseAssignments", expiration.UnpinReleaseAssignments
	case w.kind == "set" && w.table == "messages":
		name, fragment = "ChannelPinHoldAssignments", expiration.ChannelPinHoldAssignments
	case w.kind == "set" && w.table == "dm_messages":
		name, fragment = "ConversationPinHoldAssignments", expiration.ConversationPinHoldAssignments
	default:
		return fmt.Sprintf("UPDATE %s writes pinned_at, a table this census has no fragment for", w.table)
	}
	if !strings.Contains(w.setList, censusNormalise(fragment)) {
		return fmt.Sprintf("UPDATE %s %ss pinned_at without embedding expiration.%s in its SET list (I-HELD, I-KEEP)", w.table, w.kind, name)
	}
	return ""
}

// censusSource is one parsed non-test file and the package directory it is in.
type censusSource struct {
	dir  string
	path string
	file *ast.File
}

// censusFinding is one distinct statement text that writes pinned_at and every
// place it appears.
type censusFinding struct {
	sql       string
	locations []string
}

// censusEval turns a string-valued expression into text.
type censusEval struct {
	consts map[string]ast.Expr // package-level and local const/var initialisers; nil when the name is ambiguous
	exp    map[string]bool     // this file's names for internal/expiration
}

func (e *censusEval) eval(x ast.Expr, seen map[string]bool) string {
	switch n := x.(type) {
	case *ast.BasicLit:
		if n.Kind == token.STRING {
			if s, err := strconv.Unquote(n.Value); err == nil {
				return s
			}
		}
	case *ast.ParenExpr:
		return e.eval(n.X, seen)
	case *ast.BinaryExpr:
		if n.Op == token.ADD {
			return e.eval(n.X, seen) + e.eval(n.Y, seen)
		}
	case *ast.Ident:
		if init := e.consts[n.Name]; init != nil && !seen[n.Name] {
			seen[n.Name] = true
			defer delete(seen, n.Name)
			return e.eval(init, seen)
		}
	case *ast.SelectorExpr:
		if pkg, ok := n.X.(*ast.Ident); ok && e.exp[pkg.Name] {
			if fragment, ok := censusFragments[n.Sel.Name]; ok {
				return fragment
			}
		}
	}
	return censusOpaque
}

func censusExpirationNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != censusExpirationImport {
			continue
		}
		if imp.Name != nil {
			names[imp.Name.Name] = true
		} else {
			names["expiration"] = true
		}
	}
	return names
}

// censusWalk visits every node with its parent (nil for the file itself).
func censusWalk(file *ast.File, visit func(n, parent ast.Node)) {
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, n)
		visit(n, parent)
		return true
	})
}

// censusIndex is what resolving a name needs, per package directory.
type censusIndex struct {
	consts map[string]map[string]ast.Expr // dir -> name -> initialiser; nil when the name is declared twice
	refs   map[string]map[string]int      // dir -> name -> uses outside its declaration
}

// censusIsUse reports whether id, seen under parent, refers to a name rather
// than declaring one or selecting a member.
func censusIsUse(parent ast.Node, id *ast.Ident) bool {
	switch p := parent.(type) {
	case *ast.SelectorExpr:
		return p.Sel != id
	case *ast.ValueSpec:
		for _, name := range p.Names {
			if name == id {
				return false
			}
		}
	}
	return true
}

// censusBuildIndex maps each package directory to its const and var
// initialisers by name. A name declared twice in one package (two functions
// each with a local `query`) is ambiguous and resolves to nothing.
func censusBuildIndex(sources []censusSource) censusIndex {
	index := censusIndex{consts: map[string]map[string]ast.Expr{}, refs: map[string]map[string]int{}}
	for _, src := range sources {
		if index.consts[src.dir] == nil {
			index.consts[src.dir] = map[string]ast.Expr{}
			index.refs[src.dir] = map[string]int{}
		}
		names, refs := index.consts[src.dir], index.refs[src.dir]
		censusWalk(src.file, func(n, parent ast.Node) {
			switch x := n.(type) {
			case *ast.ValueSpec:
				if len(x.Names) != len(x.Values) {
					return
				}
				for i, name := range x.Names {
					if _, dup := names[name.Name]; dup {
						names[name.Name] = nil
					} else {
						names[name.Name] = x.Values[i]
					}
				}
			case *ast.Ident:
				if censusIsUse(parent, x) {
					refs[x.Name]++
				}
			}
		})
	}
	return index
}

// declaredValueIsUsed reports whether n is the initialiser of a name that some
// other expression resolves to. Such a value is a component of the statement
// that uses it, and is scanned there with the rest of that statement: scanning
// `setHead` alone would read a SET list that ends in a comma and has no
// fragment, though the statement that uses it has one.
func (i censusIndex) declaredValueIsUsed(dir string, n, parent ast.Node) bool {
	spec, ok := parent.(*ast.ValueSpec)
	if !ok || len(spec.Names) != len(spec.Values) {
		return false
	}
	for k, value := range spec.Values {
		if value == n {
			name := spec.Names[k].Name
			return i.consts[dir][name] != nil && i.refs[dir][name] > 0
		}
	}
	return false
}

// censusScan evaluates every maximal string expression in the sources and
// returns the distinct statements that write pinned_at. A maximal expression is
// a string literal or "+" chain not itself an operand of "+", or a use of a
// constant (the constant's value in full).
func censusScan(fset *token.FileSet, sources []censusSource) map[string]*censusFinding {
	index := censusBuildIndex(sources)
	findings := map[string]*censusFinding{}
	for _, src := range sources {
		eval := &censusEval{consts: index.consts[src.dir], exp: censusExpirationNames(src.file)}
		censusWalk(src.file, func(n, parent ast.Node) {
			if add, ok := parent.(*ast.BinaryExpr); ok && add.Op == token.ADD {
				return
			}
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return
				}
			case *ast.BinaryExpr:
				if x.Op != token.ADD {
					return
				}
			case *ast.Ident:
				if !censusIsUse(parent, x) || index.consts[src.dir][x.Name] == nil {
					return
				}
			default:
				return
			}
			if index.declaredValueIsUsed(src.dir, n, parent) {
				return
			}
			sql := censusNormalise(eval.eval(n.(ast.Expr), map[string]bool{}))
			if !strings.Contains(strings.ToLower(sql), "pinned_at") || len(censusWrites(sql)) == 0 {
				return
			}
			finding := findings[sql]
			if finding == nil {
				finding = &censusFinding{sql: sql}
				findings[sql] = finding
			}
			finding.locations = append(finding.locations, fmt.Sprintf("%s:%d", src.path, fset.Position(n.Pos()).Line))
		})
	}
	return findings
}

func censusSortedFindings(findings map[string]*censusFinding) []*censusFinding {
	out := make([]*censusFinding, 0, len(findings))
	for _, f := range findings {
		sort.Strings(f.locations)
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].locations[0] < out[j].locations[0] })
	return out
}

// censusLoadTree parses every non-test Go file under the three source roots of
// the control plane. The test runs from internal/messages.
func censusLoadTree(t *testing.T) (*token.FileSet, []censusSource) {
	t.Helper()
	fset := token.NewFileSet()
	var sources []censusSource
	for _, root := range []string{"internal", "cmd", "pkg"} {
		dir := filepath.Join("..", "..", root)
		require.DirExists(t, dir, "the census must see the %s tree", root)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				return fmt.Errorf("parse %s: %w", path, parseErr)
			}
			sources = append(sources, censusSource{dir: filepath.Dir(path), path: filepath.ToSlash(strings.TrimPrefix(path, filepath.Join("..", "..")+string(filepath.Separator))), file: file})
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, sources)
	return fset, sources
}

// TestPinnedAtWritersEmbedTheirExpiryFragment is the census itself (AC14). The
// real tree must contain all four writers (so a matcher that stopped matching
// cannot pass vacuously) and every writer it finds must embed its fragment.
func TestPinnedAtWritersEmbedTheirExpiryFragment(t *testing.T) {
	fset, sources := censusLoadTree(t)
	findings := censusSortedFindings(censusScan(fset, sources))

	seen := map[string]bool{}
	for _, finding := range findings {
		t.Logf("pinned_at writer at %s", strings.Join(finding.locations, ", "))
		for _, w := range censusWrites(finding.sql) {
			seen[w.table+":"+w.kind] = true
		}
		for _, problem := range censusProblems(finding.sql) {
			t.Errorf("%s: %s", strings.Join(finding.locations, ", "), problem)
		}
	}
	for _, want := range []string{"messages:set", "dm_messages:set", "messages:clear", "dm_messages:clear"} {
		assert.True(t, seen[want], "the census found no %s writer: the matcher or the tree walk stopped seeing the pin writers", want)
	}
}

// TestCensusClassifiesPinnedAtWrites pins the matcher itself, so the census
// above cannot pass because the matcher stopped matching, and proves it does not
// read a pinned_at in a WHERE clause, a subquery or a SELECT as a write.
func TestCensusClassifiesPinnedAtWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want []string
	}{
		{"channel pin", "UPDATE messages SET pinned_at = NOW(), pinned_by = $1 WHERE id = $2", []string{"messages:set"}},
		{"channel unpin", "UPDATE messages SET pinned_at = NULL, pinned_by = NULL WHERE id = $1", []string{"messages:clear"}},
		{"aliased DM unpin", "UPDATE dm_messages dm SET pinned_at = NULL WHERE dm.id = $1", []string{"dm_messages:clear"}},
		{"quoted and as-aliased", `UPDATE "dm_messages" AS dm SET pinned_by = $1, pinned_at = NOW() WHERE dm.id = $2`, []string{"dm_messages:set"}},
		{"schema-qualified, no spaces", "UPDATE public.messages SET pinned_at=NULL WHERE id = $1", []string{"messages:clear"}},
		{"returning ends the list", "UPDATE messages SET pinned_at = NOW() RETURNING pinned_at", []string{"messages:set"}},
		{"CTE", "WITH p AS (UPDATE messages SET pinned_at = NULL WHERE id = $1 RETURNING id) SELECT 1 FROM p", []string{"messages:clear"}},
		{"upsert takes the INSERT table", "INSERT INTO messages (id) VALUES ($1) ON CONFLICT (id) DO UPDATE SET pinned_at = NULL", []string{"messages:clear"}},
		{"pinned insert", "INSERT INTO messages (id, content, pinned_at) VALUES ($1, $2, NOW())", []string{"messages:insert"}},
		{"unlisted table", "UPDATE users SET pinned_at = NOW() WHERE id = $1", []string{"users:set"}},
		{"tuple assignment", "UPDATE messages SET (pinned_at, pinned_by) = (NOW(), $1) WHERE id = $2", []string{"messages:tuple"}},
		{"CASE assignment", "UPDATE messages SET pinned_at = CASE WHEN $1 THEN NULL ELSE NOW() END WHERE id = $2", []string{"messages:mixed"}},
		{"select", "SELECT pinned_at, pinned_by FROM messages WHERE pinned_at IS NOT NULL", nil},
		{"unpinned insert", "INSERT INTO messages (id, content) VALUES ($1, $2)", nil},
		{"pinned_at only in WHERE", "UPDATE messages SET content = $1 WHERE pinned_at IS NOT NULL", nil},
		{"pinned_at compared in WHERE", "UPDATE messages SET expires_at = NULL WHERE channel_id = $1 AND pinned_at = $2", nil},
		{"pinned_at only in a SET subquery", "UPDATE messages SET held_expires_at = (SELECT pinned_at FROM messages WHERE id = $1) WHERE id = $2", nil},
		{"pinned_at in a quoted literal", "UPDATE messages SET content = 'x (pinned_at = NULL' WHERE id = $1", nil},
		{"pinned_at only in a later UPDATE's WHERE", "UPDATE messages SET content = $1 WHERE id = $2; UPDATE dm_messages SET content = $1 WHERE pinned_at IS NULL", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := censusWrites(tc.sql)
			got := make([]string, 0, len(writes))
			for _, w := range writes {
				got = append(got, w.table+":"+w.kind)
			}
			assert.ElementsMatch(t, tc.want, got)
		})
	}
}

// censusScanSources scans in-memory sources that form one package.
func censusScanSources(t *testing.T, sources map[string]string) []*censusFinding {
	t.Helper()
	fset := token.NewFileSet()
	parsed := make([]censusSource, 0, len(sources))
	for name, src := range sources {
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		require.NoError(t, err, name)
		parsed = append(parsed, censusSource{dir: "x", path: name, file: file})
	}
	return censusSortedFindings(censusScan(fset, parsed))
}

func censusAllProblems(findings []*censusFinding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, censusProblems(f.sql)...)
	}
	return out
}

// TestCensusEvaluatesConstantSQL drives the evaluator and the fragment check
// over synthetic packages, including every way a writer can fail to embed its
// fragment and the const-splitting shapes the real tree does not use today.
func TestCensusEvaluatesConstantSQL(t *testing.T) {
	const header = `package x
import "` + censusExpirationImport + `"
import exp "` + censusExpirationImport + `"
var _ = expiration.ChannelPinHoldAssignments
var _ = exp.UnpinReleaseAssignments
`
	t.Run("a writer embedding its fragment passes, through consts and an import alias", func(t *testing.T) {
		findings := censusScanSources(t, map[string]string{"a.go": header + "\n" + `
const setHead = "UPDATE messages SET pinned_at = NOW(), "
const pin = setHead + expiration.ChannelPinHoldAssignments + " WHERE id = $1"
const unpin = "UPDATE dm_messages dm SET pinned_at = NULL, " + exp.UnpinReleaseAssignments + " WHERE dm.id = $1"
const dmPin = "UPDATE dm_messages SET pinned_at = NOW(), " + expiration.ConversationPinHoldAssignments + " WHERE id = $1" + filter("dm", 1)
func filter(string, int) string { return "" }
`})
		require.NotEmpty(t, findings)
		assert.Empty(t, censusAllProblems(findings))
		seen := map[string]bool{}
		for _, f := range findings {
			for _, w := range censusWrites(f.sql) {
				seen[w.table+":"+w.kind] = true
			}
		}
		assert.Equal(t, map[string]bool{"messages:set": true, "dm_messages:clear": true, "dm_messages:set": true}, seen)
	})

	for _, tc := range []struct {
		name    string
		body    string
		problem string
	}{
		{"no fragment", `const q = "UPDATE messages SET pinned_at = NOW() WHERE id = $1"`, "without embedding expiration.ChannelPinHoldAssignments"},
		{"unpin without the release", `const q = "UPDATE messages SET pinned_at = NULL, pinned_by = NULL WHERE id = $1"`, "without embedding expiration.UnpinReleaseAssignments"},
		{"DM writer with the channel fragment", `const q = "UPDATE dm_messages SET pinned_at = NOW(), " + expiration.ChannelPinHoldAssignments + " WHERE id = $1"`, "without embedding expiration.ConversationPinHoldAssignments"},
		{"channel writer with the release fragment", `const q = "UPDATE messages SET pinned_at = NOW(), " + exp.UnpinReleaseAssignments + " WHERE id = $1"`, "without embedding expiration.ChannelPinHoldAssignments"},
		{"fragment pasted into the WHERE clause", `const q = "UPDATE messages SET pinned_at = NOW() WHERE " + expiration.ChannelPinHoldAssignments`, "without embedding expiration.ChannelPinHoldAssignments"},
		{"fragment lives in a call the evaluator cannot see", `const q = "UPDATE messages SET pinned_at = NOW(), " + fragment() + " WHERE id = $1"
func fragment() string { return "" }`, "without embedding expiration.ChannelPinHoldAssignments"},
		{"split across consts, no fragment", `const set = "pinned_at = NULL"
const q = "UPDATE dm_messages dm SET " + set + " WHERE dm.id = $1"`, "without embedding expiration.UnpinReleaseAssignments"},
		{"in a function body", `func f() string { return "UPDATE messages SET pinned_at = NULL WHERE id = $1" }`, "without embedding expiration.UnpinReleaseAssignments"},
		{"pinned insert", `const q = "INSERT INTO messages (id, pinned_at) VALUES ($1, NOW())"`, "names pinned_at"},
		{"unknown table", `const q = "UPDATE widgets SET pinned_at = NOW() WHERE id = $1"`, "no fragment for"},
		{"mixed assignment", `const q = "UPDATE messages SET pinned_at = CASE WHEN $1 THEN NULL ELSE NOW() END WHERE id = $2"`, "cannot classify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := censusAllProblems(censusScanSources(t, map[string]string{"a.go": header + "\n" + tc.body}))
			require.Len(t, problems, 1)
			assert.Contains(t, problems[0], tc.problem)
		})
	}

	t.Run("an ambiguous constant name is opaque, and its own declaration is still scanned", func(t *testing.T) {
		findings := censusScanSources(t, map[string]string{"a.go": header + `
func one() string { const q = "UPDATE messages SET pinned_at = NULL WHERE id = $1"; return q }
func two() string { const q = "SELECT 1"; return q }
`})
		problems := censusAllProblems(findings)
		require.Len(t, problems, 1, "the declaration is a root of its own, so the write is found even though the name resolves to nothing")
		assert.Contains(t, problems[0], "UnpinReleaseAssignments")
	})

	t.Run("reads are not writers", func(t *testing.T) {
		findings := censusScanSources(t, map[string]string{"a.go": header + `
const read = "SELECT pinned_at FROM messages WHERE pinned_at IS NOT NULL"
const guard = "UPDATE messages SET content = $1 WHERE pinned_at IS NULL"
`})
		assert.Empty(t, findings)
	})
}
