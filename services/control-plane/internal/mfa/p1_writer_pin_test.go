package mfa

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// p1WriteTx is the shared factor-write transaction; rule 2 below attributes
// the closures passed to it.
const p1WriteTx = "withMFAFactorWriteTx"

// p1HookedFunctions are the functions that call invalidatePermissionState after
// their P1 write commits (spec §6, #3453).
var p1HookedFunctions = []string{p1WriteTx, "TOTPVerifySetup", "TOTPDisable", "WebAuthnDeleteCredential"}

// The P1-write matcher. A SQL string is a P1-state write when, after
// collapsing whitespace, upper-casing and dropping identifier quotes, it:
//   - INSERTs into or DELETEs from user_mfa_totp or user_mfa_webauthn
//     (either may be schema-qualified with public.); or
//   - UPDATEs user_mfa_totp with a top-level SET list that assigns enabled or
//     confirmed.
//
// So backup-code updates, WebAuthn sign_count/last_used_at updates and
// rekey.go's secret re-encryption are not P1 writes: none of them changes
// whether the user holds an inline factor.
var (
	p1InsertOrDelete = regexp.MustCompile(`\b(?:INSERT INTO|DELETE FROM(?: ONLY)?) (?:PUBLIC\.)?USER_MFA_(?:TOTP|WEBAUTHN)\b`)
	p1TOTPUpdate     = regexp.MustCompile(`\bUPDATE (?:ONLY )?(?:PUBLIC\.)?USER_MFA_TOTP\b(?: (?:AS )?\w+)? SET (.*)`)
	p1FlagAssignment = regexp.MustCompile(`\b(?:ENABLED|CONFIRMED) ?=`)
	p1SetListEnd     = regexp.MustCompile(`^ (?:WHERE|FROM|RETURNING) `)
)

func isP1Write(sql string) bool {
	s := strings.ToUpper(strings.ReplaceAll(strings.Join(strings.Fields(sql), " "), `"`, ""))
	if p1InsertOrDelete.MatchString(s) {
		return true
	}
	m := p1TOTPUpdate.FindStringSubmatch(s)
	return m != nil && p1FlagAssignment.MatchString(topLevelSetList(m[1]))
}

// topLevelSetList keeps the text of an UPDATE's SET list that sits outside
// parentheses, up to the first top-level WHERE, FROM or RETURNING, so a flag
// read in the WHERE clause or inside a subquery does not count as assigned.
func topLevelSetList(afterSet string) string {
	var b strings.Builder
	depth := 0
	for i, r := range afterSet {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		case depth > 0:
		case r == ' ' && p1SetListEnd.MatchString(afterSet[i:]):
			return b.String()
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestIsP1Write pins the matcher itself, so the writer pin below cannot pass
// because the matcher stopped matching anything.
func TestIsP1Write(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"INSERT INTO user_mfa_totp (user_id) VALUES ($1)", true},
		{"\n\t\tinsert into   user_mfa_webauthn (user_id)\n VALUES ($1)", true},
		{`DELETE FROM "user_mfa_totp" WHERE user_id = $1`, true},
		{"DELETE FROM public.user_mfa_webauthn WHERE id = $1", true},
		{"UPDATE user_mfa_totp SET enabled = TRUE, verified_at = NOW() WHERE user_id = $1", true},
		{"UPDATE user_mfa_totp SET confirmed = TRUE, confirmed_at = NOW() WHERE user_id = $1", true},
		{"UPDATE user_mfa_totp t SET confirmed=FALSE WHERE t.user_id = $1", true},
		{"UPDATE user_mfa_totp SET backup_codes_used = $1, updated_at = NOW() WHERE user_id = $2 AND backup_codes_used = $3", false},
		{"UPDATE user_mfa_totp SET backup_codes_hash = $1, backup_codes_used = $2 WHERE user_id = $3", false},
		{"UPDATE user_mfa_totp SET totp_secret_enc = $1, key_version = $3 WHERE user_id = $4 AND enabled = TRUE", false},
		{"UPDATE user_mfa_totp SET confirmed_at = NOW() WHERE user_id = $1", false},
		{"UPDATE user_mfa_totp SET key_version = (SELECT 2 FROM keys WHERE enabled = TRUE) WHERE user_id = $1", false},
		{"UPDATE user_mfa_webauthn SET sign_count = $1, last_used_at = NOW() WHERE credential_id = $2", false},
		{"SELECT enabled, confirmed FROM user_mfa_totp WHERE user_id = $1 FOR UPDATE", false},
		{"INSERT INTO user_mfa_totp_audit (user_id) VALUES ($1)", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isP1Write(tc.sql), "%q", tc.sql)
	}
}

// TestP1FactorWritersAreExactlyTheHookedFunctions is the static writer pin
// (spec §6). It finds every P1 write in this package's non-test sources and
// asserts the functions they are attributed to are EXACTLY the hooked ones. A
// new unhooked writer fails it, and so does a hooked function that stopped
// writing, which is why this is set equality rather than a subset check.
//
// Attribution, in order:
//  1. A matching string literal belongs to its enclosing top-level declaration.
//  2. A literal inside a function literal passed as an argument to
//     withMFAFactorWriteTx belongs to withMFAFactorWriteTx, because that
//     closure runs on its transaction and its post-commit hook covers it.
//  3. An owner that is not a hooked function is replaced by the owners of the
//     places that use it, recursively: the callers of a helper function, or the
//     users of a package-level const or var. This is what attributes the two
//     disable DELETEs, which live in verifyAndDeleteTOTPTx and
//     verifyAndDeleteWebAuthnTx, to TOTPDisable and WebAuthnDeleteCredential,
//     whose transactions they run on. It fails closed: an owner with no use in
//     the package (an HTTP handler, say) stays in the set, and so does a helper
//     that is referenced without being called (a method value, which could run
//     anywhere) or that sits on a reference cycle.
//
// What it cannot see:
//   - SQL built at run time (concatenation with a non-literal, fmt, a
//     strings.Builder) and any statement shape outside the matcher above, such
//     as MERGE, COPY or TRUNCATE. A literal-only concatenation is also missed,
//     because each operand is matched on its own.
//   - Uses are matched by identifier name, not type, so a same-named identifier
//     elsewhere in the package counts as a use. That can only add owners or keep
//     a helper in the set, never drop an unhooked one.
//   - Order. It proves every P1 write is reachable only from a hooked function,
//     not that the hook runs after the write; the I-5 tests in
//     permission_invalidation_test.go prove that for the writes that exist.
//   - Other packages. None writes these tables today; the account-erasure
//     cascade deletes the rows with the user, whose permissions go with them.
//
// No package-level const or var holds a P1 write today (mfaFlagsExactSQL and
// mfaFlagsDegradedSQL update users), so rule 3's const path is not exercised
// here; it is exercised by the owner-resolution cases in TestP1PinAttribution.
func TestP1FactorWritersAreExactlyTheHookedFunctions(t *testing.T) {
	idx := indexP1Writes(t, parseMFASources(t))
	require.NotEmpty(t, idx.writes, "the matcher found no P1 write at all")

	got := map[string]bool{}
	for _, owner := range idx.writes {
		for _, attributed := range idx.resolve(owner, map[string]bool{}) {
			got[attributed] = true
		}
	}
	assert.ElementsMatch(t, p1HookedFunctions, sortedKeys(got),
		"every P1 write must be attributed to a function that bumps the permission generation after it commits; "+
			"add the hook (h.invalidatePermissionState after the commit) rather than editing p1HookedFunctions alone")

	for _, fn := range p1HookedFunctions {
		assert.True(t, idx.hookCallers[fn], "%s must call invalidatePermissionState", fn)
	}
}

// TestP1PinAttribution drives rule 3 over a synthetic package, including the
// const path and the fail-closed arms, which the real package does not reach.
func TestP1PinAttribution(t *testing.T) {
	const src = `package x
const deleteSQL = "DELETE FROM user_mfa_webauthn WHERE id = $1"
func TOTPDisable() { helper(); run(deleteSQL) }
func helper() { exec("DELETE FROM user_mfa_totp WHERE user_id = $1") }
func escaped() { exec("INSERT INTO user_mfa_totp (user_id) VALUES ($1)") }
func Unhooked() { f := escaped; f() }
func withMFAFactorWriteTx(w func()) {}
func Handler() { withMFAFactorWriteTx(func() { exec("INSERT INTO user_mfa_webauthn (id) VALUES ($1)") }) }
func loop() { loop(); exec("UPDATE user_mfa_totp SET enabled = FALSE") }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	require.NoError(t, err)
	idx := indexP1Writes(t, []*ast.File{file})

	resolved := func(owner string) []string {
		out := idx.resolve(owner, map[string]bool{})
		sort.Strings(out)
		return out
	}
	caller := []string{"TOTPDisable"}
	assert.Equal(t, caller, resolved("deleteSQL"), "a const is attributed where it is used")
	assert.Equal(t, caller, resolved("helper"), "a called helper is attributed to its caller")
	assert.Equal(t, []string{"escaped"}, resolved("escaped"), "a helper used as a value stays in the set")
	assert.Equal(t, []string{"loop"}, resolved("loop"), "a helper on a reference cycle stays in the set")
	assert.Contains(t, idx.writes, p1WriteTx, "a closure passed to withMFAFactorWriteTx is attributed to it")
	assert.NotContains(t, idx.writes, "Handler")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func parseMFASources(t *testing.T) []*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err, name)
		files = append(files, f)
	}
	require.NotEmpty(t, files)
	return files
}

// p1Reference is one use of a top-level name: the owner of the place it is
// used, and whether that use is a call.
type p1Reference struct {
	owner string
	call  bool
}

type p1Index struct {
	writes      []string // the initial owner of each P1 write literal
	funcs       map[string]bool
	refs        map[string][]p1Reference
	hookCallers map[string]bool
}

func indexP1Writes(t *testing.T, files []*ast.File) *p1Index {
	t.Helper()
	idx := &p1Index{funcs: map[string]bool{}, refs: map[string][]p1Reference{}, hookCallers: map[string]bool{}}
	declared := map[*ast.Ident]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				idx.funcs[d.Name.Name] = true
				declared[d.Name] = true
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							declared[n] = true
						}
					}
				}
			}
		}
	}
	for _, f := range files {
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					break
				}
				s, err := strconv.Unquote(x.Value)
				require.NoError(t, err)
				if isP1Write(s) {
					idx.writes = append(idx.writes, p1Owner(stack))
				}
			case *ast.Ident:
				if declared[x] {
					break
				}
				idx.refs[x.Name] = append(idx.refs[x.Name], p1Reference{owner: p1Owner(stack), call: isCallee(stack)})
			case *ast.CallExpr:
				if calleeName(x) == "invalidatePermissionState" {
					idx.hookCallers[p1Owner(stack)] = true
				}
			}
			return true
		})
	}
	return idx
}

// resolve applies rule 3. onPath holds the owners on the current resolution
// path only, so two uses that reach the same helper are both followed.
func (idx *p1Index) resolve(owner string, onPath map[string]bool) []string {
	if onPath[owner] || isHooked(owner) {
		return []string{owner}
	}
	uses := idx.refs[owner]
	if len(uses) == 0 {
		return []string{owner}
	}
	onPath[owner] = true
	defer delete(onPath, owner)
	var out []string
	for _, use := range uses {
		if idx.funcs[owner] && !use.call {
			return []string{owner}
		}
		out = append(out, idx.resolve(use.owner, onPath)...)
	}
	return out
}

func isHooked(name string) bool {
	for _, fn := range p1HookedFunctions {
		if fn == name {
			return true
		}
	}
	return false
}

// p1Owner names the declaration the innermost node of stack belongs to, after
// rule 2. stack[0] is the *ast.File.
func p1Owner(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 1; i-- {
		lit, ok := stack[i].(*ast.FuncLit)
		if !ok {
			continue
		}
		if call, ok := stack[i-1].(*ast.CallExpr); ok && calleeName(call) == p1WriteTx && isArgument(call, lit) {
			return p1WriteTx
		}
	}
	if len(stack) < 2 {
		return ""
	}
	switch d := stack[1].(type) {
	case *ast.FuncDecl:
		return d.Name.Name
	case *ast.GenDecl:
		for _, n := range stack[2:] {
			if vs, ok := n.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
				return valueSpecName(vs, stack[len(stack)-1])
			}
		}
	}
	return ""
}

// valueSpecName picks the name a node initializes in `const a, b = x, y`.
func valueSpecName(vs *ast.ValueSpec, n ast.Node) string {
	if len(vs.Names) == len(vs.Values) {
		for i, v := range vs.Values {
			if v.Pos() <= n.Pos() && n.End() <= v.End() {
				return vs.Names[i].Name
			}
		}
	}
	return vs.Names[0].Name
}

func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func isArgument(call *ast.CallExpr, n ast.Node) bool {
	for _, arg := range call.Args {
		if arg == n {
			return true
		}
	}
	return false
}

// isCallee reports whether the identifier at the top of stack is what a call
// invokes: f(...) or x.f(...).
func isCallee(stack []ast.Node) bool {
	id := stack[len(stack)-1]
	parent := stack[len(stack)-2]
	if call, ok := parent.(*ast.CallExpr); ok {
		return call.Fun == id
	}
	if sel, ok := parent.(*ast.SelectorExpr); ok && sel.Sel == id && len(stack) >= 3 {
		call, ok := stack[len(stack)-3].(*ast.CallExpr)
		return ok && call.Fun == sel
	}
	return false
}
