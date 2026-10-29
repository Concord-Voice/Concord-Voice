package mfa_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only internal/mfa/rekey.go may set the transaction-local rewrap flag (spec H1).
// A second writer would be able to exempt its own change from the material
// trigger. Test files are skipped; vendor and testdata directories too.
//
// The scan reads each file three ways, because the flag can be spelled without
// the source text ever containing its name:
//   - the evaluated value of every string literal and every all-constant
//     concatenation (so "concord.cred" + "ential_rewrap" and a \x5f escape are
//     seen), matched case-insensitively because PostgreSQL GUC names are;
//   - the fragment "concord.credential_", which also catches a name assembled
//     in SQL ('concord.credential_' || $1);
//   - the identifiers rewrapFlag, rewrapValue and resealRowTx inside package mfa,
//     which are rekey.go's flag-setting surface (resealRowTx sets the flag around
//     an ARBITRARY (enc, nonce) write, so it may be called only from rekeyRow).
//
// It also bars the flag's value, mfa-rekey, outside rekey.go and cmd/mfa-rekey,
// and scans migrations/*.sql: only 000189_* and 000191_* may name the flag.
//
// The walk and the reads go through one os.Root, so a symlink inside the tree
// cannot point a read outside it.

const (
	rewrapAllowed   = "internal/mfa/rekey.go"
	rewrapCmdDir    = "cmd/mfa-rekey/"
	rewrapValueText = "mfa-rekey"
	rewrapCaller    = "rekeyRow"
)

// The migrations that define the flag's reader, accountguard_is_rewrap().
var rewrapOwnerSQL = []string{"000189_", "000191_"}

// Fragments only a writer of the flag needs, lower-cased for a case-insensitive
// match: the GUC name and its namespace prefix.
var rewrapFragments = []string{"credential_rewrap", "concord.credential_"}

// rekey.go's flag-setting surface inside package mfa.
var rewrapIdents = map[string]bool{"rewrapFlag": true, "rewrapValue": true, "resealRowTx": true}

// rewrapConstString evaluates an expression made only of string literals joined
// by +. ok is false for anything else (a variable, a call).
func rewrapConstString(e ast.Expr) (string, bool) {
	switch n := e.(type) {
	case *ast.BasicLit:
		if n.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(n.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return rewrapConstString(n.X)
	case *ast.BinaryExpr:
		if n.Op != token.ADD {
			return "", false
		}
		l, lok := rewrapConstString(n.X)
		r, rok := rewrapConstString(n.Y)
		return l + r, lok && rok
	}
	return "", false
}

// rewrapOffences returns one message per way the non-test Go file at path can set
// or reach the flag. rekey.go itself is judged by rewrapCallerOffences instead.
func rewrapOffences(t *testing.T, path string, body []byte) []string {
	t.Helper()
	if path == rewrapAllowed {
		return rewrapCallerOffences(t, path, body)
	}
	var out []string
	if strings.Contains(strings.ToLower(string(body)), "credential_rewrap") {
		out = append(out, path+": source names credential_rewrap")
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, body, 0)
	require.NoError(t, err, "parse %s", path)
	inMFA := strings.HasPrefix(path, "internal/mfa/")
	inCmd := strings.HasPrefix(path, rewrapCmdDir)
	ast.Inspect(file, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.Ident:
			if inMFA && rewrapIdents[e.Name] {
				out = append(out, path+": references rekey.go's "+e.Name)
			}
		case *ast.BinaryExpr, *ast.BasicLit:
			s, ok := rewrapConstString(n.(ast.Expr))
			if !ok {
				return true
			}
			lower := strings.ToLower(s)
			for _, f := range rewrapFragments {
				if strings.Contains(lower, f) {
					out = append(out, path+": a string constant contains "+strconv.Quote(f))
				}
			}
			if !inCmd && strings.Contains(lower, rewrapValueText) {
				out = append(out, path+": a string constant carries the rewrap value "+strconv.Quote(rewrapValueText))
			}
			if _, whole := e.(*ast.BinaryExpr); whole {
				return false
			}
		}
		return true
	})
	return out
}

// rewrapCallerOffences judges rekey.go: resealRowTx may be referenced only from
// rekeyRow, so no second path in the file can run an arbitrary write under the
// flag. The declaration's own name is not a reference.
func rewrapCallerOffences(t *testing.T, path string, body []byte) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, body, 0)
	require.NoError(t, err, "parse %s", path)
	var out []string
	for _, decl := range file.Decls {
		enclosing := ""
		var root ast.Node = decl
		if fd, ok := decl.(*ast.FuncDecl); ok {
			enclosing, root = fd.Name.Name, fd.Body
			if fd.Body == nil {
				continue
			}
		}
		ast.Inspect(root, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "resealRowTx" && enclosing != rewrapCaller {
				where := enclosing
				if where == "" {
					where = "a package-level declaration"
				}
				out = append(out, path+": resealRowTx is referenced from "+where+", not "+rewrapCaller)
			}
			return true
		})
	}
	return out
}

// rewrapOwnsFlag reports whether a migration file is one of rewrapOwnerSQL.
func rewrapOwnsFlag(name string) bool {
	for _, p := range rewrapOwnerSQL {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// rewrapMigrationOffences returns the message for a migration other than the
// rewrapOwnerSQL ones that names the flag, in any case.
func rewrapMigrationOffences(name, body string) []string {
	if rewrapOwnsFlag(name) {
		return nil
	}
	if strings.Contains(strings.ToLower(body), "credential_rewrap") {
		return []string{"migrations/" + name + ": names credential_rewrap; only " + strings.Join(rewrapOwnerSQL, "*, ") + "* may"}
	}
	return nil
}

func TestRewrapFlagIsSetOnlyByRekey(t *testing.T) {
	root, err := os.OpenRoot(filepath.Join("..", ".."))
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	fsys := root.FS()

	var visitedRekey, rekeyNamesFlag, visitedCmd bool
	visitedOwnerSQL := map[string]bool{}
	var files, migrations int
	var offenders []string
	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		isSQL := strings.HasPrefix(path, "migrations/") && strings.HasSuffix(path, ".sql")
		isGo := strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
		if !isSQL && !isGo {
			return nil
		}
		body, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return readErr
		}
		if isSQL {
			migrations++
			name := strings.TrimPrefix(path, "migrations/")
			for _, p := range rewrapOwnerSQL {
				if strings.HasPrefix(name, p) {
					visitedOwnerSQL[p] = true
				}
			}
			offenders = append(offenders, rewrapMigrationOffences(name, string(body))...)
			return nil
		}
		files++
		visitedCmd = visitedCmd || strings.HasPrefix(path, rewrapCmdDir)
		if path == rewrapAllowed {
			visitedRekey = true
			rekeyNamesFlag = strings.Contains(string(body), "credential_rewrap")
		}
		offenders = append(offenders, rewrapOffences(t, path, body)...)
		return nil
	})
	require.NoError(t, err)
	require.True(t, visitedRekey, "the walk must reach %s, or the scan proves nothing", rewrapAllowed)
	require.True(t, visitedCmd, "the walk must reach %s", rewrapCmdDir)
	for _, p := range rewrapOwnerSQL {
		require.True(t, visitedOwnerSQL[p], "the walk must reach the %s* migration, which does name the flag", p)
	}
	require.Greater(t, files, 100, "the walk must cover the module")
	require.Greater(t, migrations, 100, "the walk must cover the migrations")
	assert.True(t, rekeyNamesFlag, "%s must set the rewrap flag", rewrapAllowed)
	assert.Empty(t, offenders, "only %s may set concord.credential_rewrap", rewrapAllowed)
}

// Positive control: every planted second writer is flagged, and rekey.go, the
// command and unrelated code are not, so the scan above cannot be green merely
// because the module has no second writer.
func TestRewrapScanFlagsPlantedWriters(t *testing.T) {
	offenders := []struct{ name, path, src string }{
		{"package mfa reuses resealRowTx for a secret rotation", "internal/mfa/rotate.go",
			"package mfa\n\nfunc rotate() { _, _ = resealRowTx(ctx, db, pendingRow{userID: u, enc: old, version: v}, newEnc, newNonce, v) }\n"},
		{"package mfa reuses the constants", "internal/mfa/handlers2.go",
			"package mfa\n\nfunc f() { _, _ = tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, rewrapFlag, rewrapValue) }\n"},
		{"name assembled in SQL", "internal/users/x.go",
			"package users\n\nconst q = `SELECT set_config('concord.credential_' || $1, $2, true)`\n"},
		{"name spelled with an escape", "internal/users/y.go",
			"package users\n\nconst flag = \"concord.credential\\x5frewrap\"\n"},
		{"name split across a constant concatenation", "internal/users/z.go",
			"package users\n\nconst flag = \"concord.cred\" + \"ential_rewrap\"\n"},
		{"mixed-case name (GUC names are case-insensitive)", "internal/users/m.go",
			"package users\n\nconst flag = \"Concord.Credential_Rewrap\"\n"},
		{"mixed-case name, split and escaped", "internal/users/m2.go",
			"package users\n\nconst flag = \"CONCORD.Cred\" + \"ENTIAL\\x5fRewrap\"\n"},
		{"the rewrap value outside rekey.go", "internal/users/v.go",
			"package users\n\nconst v = \"mfa-rekey\"\n"},
		{"the rewrap value, other case, outside rekey.go", "internal/users/v2.go",
			"package users\n\nconst v = \"MFA-Rekey\"\n"},
		{"rekey.go calls resealRowTx from a second function", rewrapAllowed,
			"package mfa\n\nfunc rekeyRow() { resealRowTx() }\n\nfunc rotate() { resealRowTx() }\n"},
		{"rekey.go stores resealRowTx in a package variable", rewrapAllowed,
			"package mfa\n\nvar reseal = resealRowTx\n\nfunc rekeyRow() {}\n"},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEmpty(t, rewrapOffences(t, tc.path, []byte(tc.src)))
		})
	}

	clean := []struct{ name, path, src string }{
		{"rekey.go itself, resealRowTx called from rekeyRow", rewrapAllowed,
			"package mfa\n\nconst (\n\trewrapFlag  = \"concord.credential_rewrap\"\n\trewrapValue = \"mfa-rekey\"\n)\n\nfunc rekeyRow() { resealRowTx() }\n\nfunc resealRowTx() {}\n"},
		{"the mfa-rekey command name", rewrapCmdDir + "main.go",
			"package main\n\nvar fs = flag.NewFlagSet(\"mfa-rekey\", flag.ContinueOnError)\n"},
		{"an unrelated set_config", "internal/users/w.go",
			"package users\n\nconst q = `SELECT set_config('statement_timeout', $1, true)`\n"},
		{"a different identifier in package mfa", "internal/mfa/other.go",
			"package mfa\n\nfunc resealOther() {}\n"},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, rewrapOffences(t, tc.path, []byte(tc.src)))
		})
	}

	// The command may carry the value but never the flag name.
	t.Run("the command naming the flag is still flagged", func(t *testing.T) {
		assert.NotEmpty(t, rewrapOffences(t, rewrapCmdDir+"main.go",
			[]byte("package main\n\nconst f = \"concord.credential_rewrap\"\n")))
	})
}

// Only 000189_* and 000191_* may name the flag in SQL, in any case.
func TestRewrapScanFlagsPlantedMigrations(t *testing.T) {
	assert.NotEmpty(t, rewrapMigrationOffences("000190_later.up.sql", "SELECT set_config('concord.credential_rewrap', 'mfa-rekey', true);"))
	assert.NotEmpty(t, rewrapMigrationOffences("000190_later.up.sql", "SELECT current_setting('Concord.Credential_Rewrap', true);"))
	assert.Empty(t, rewrapMigrationOffences("000189_accountguard_maturity.up.sql", "SELECT current_setting('concord.credential_rewrap', true);"))
	assert.Empty(t, rewrapMigrationOffences("000191_accountguard_rewrap_xact_bind.up.sql", "SELECT current_setting('concord.credential_rewrap', true);"))
	assert.NotEmpty(t, rewrapMigrationOffences("000192_later.up.sql", "SELECT current_setting('concord.credential_rewrap', true);"))
	assert.Empty(t, rewrapMigrationOffences("000190_later.up.sql", "SELECT 1;"))
}
