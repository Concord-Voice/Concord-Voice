package database_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Enrolment provenance and maturity (migration 000189, spec H3): a statement that
// writes {,password_,email_}enrolled_at is a provenance statement, and the trigger
// keeps its enrol_class only because the writer supplied both. So in non-test Go
// string literals, an INSERT or UPDATE that names one of the pair must name the
// other of the SAME family, and only internal/accountguard may name any of the
// columns except in a pure read. The maturity columns ({,password_,email_}matures_at)
// are confined the same way, with no pairing rule: no writer of either exists
// today, so the allowlist is empty.
//
// provenanceViolations is the pure rule; the scan feeds it every string literal
// (and every all-constant concatenation) in the module's non-test Go files. The
// positive control runs the same function on planted literals, so the scan
// cannot pass vacuously while no writer exists. Three more passes follow from
// the same idea: migrations/*.sql (only 000189_* may name these columns), and,
// inside internal/accountguard, no exported const or var may carry a column name.

const (
	provenanceWriterDir = "internal/accountguard/"
	provenanceOwnerSQL  = "000189_"
)

// SQL comments and row-locking clauses are removed before the write test, so a
// comment cannot split "UPDATE t ... SET" and "FOR UPDATE" is not read as a write.
var (
	provenanceSQLCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*`)
	provenanceLockingRE    = regexp.MustCompile(`(?i)\bFOR\s+(NO\s+KEY\s+)?UPDATE\b|\bFOR\s+(KEY\s+)?SHARE\b`)
)

// A statement that writes: INSERT INTO; any UPDATE ... SET (a table alias, ONLY,
// a %s placeholder, MERGE's WHEN MATCHED THEN UPDATE SET, an upsert's DO UPDATE
// SET); MERGE INTO; or COPY. Red-team RT-1: the earlier
// `\bUPDATE\s+[\w."]+\s+SET\b` missed the alias, ONLY, comment, placeholder and
// MERGE forms.
var provenanceWriteRE = regexp.MustCompile(`(?is)\bINSERT\s+INTO\b|\bUPDATE\b.*?\bSET\b|\bMERGE\s+INTO\b|\bCOPY\b`)

// A pure read starts with SELECT (or a WITH that leads to one) and is not a write.
var provenanceReadRE = regexp.MustCompile(`(?is)^\s*(SELECT|WITH)\b`)

// provenanceFamily is one column family. column is its first column (enrolled_at,
// matures_at); class is the enrol_class partner, nil for a maturity family, which
// has no pairing rule. name is only a label.
type provenanceFamily struct {
	name, prefix  string
	column, class *regexp.Regexp
}

var provenanceFamilies = func() []provenanceFamily {
	out := make([]provenanceFamily, 0, 6)
	for _, prefix := range []string{"", "password_", "email_"} {
		// \b is a word boundary, and "_" is a word character, so the unprefixed
		// pattern does not match inside password_enrolled_at.
		out = append(out, provenanceFamily{
			name:   prefix + "enrolment provenance",
			prefix: prefix,
			column: regexp.MustCompile(`(?i)\b` + prefix + `enrolled_at\b`),
			class:  regexp.MustCompile(`(?i)\b` + prefix + `enrol_class\b`),
		})
	}
	for _, prefix := range []string{"", "password_", "email_"} {
		out = append(out, provenanceFamily{
			name:   prefix + "maturity",
			prefix: prefix,
			column: regexp.MustCompile(`(?i)\b` + prefix + `matures_at\b`),
		})
	}
	return out
}()

// provenanceNames reports whether s names any column of any family.
func provenanceNames(s string) bool {
	for _, fam := range provenanceFamilies {
		if fam.column.MatchString(s) || (fam.class != nil && fam.class.MatchString(s)) {
			return true
		}
	}
	return false
}

// provenanceViolations returns one message per rule a literal at path breaks.
// Outside provenanceWriterDir a literal that names a column at all must be a pure
// read: a write, and equally a fragment of a statement assembled elsewhere (a
// pq.CopyIn column, a constant joined into an UPDATE, an addClause argument),
// which no single-literal write test can see.
func provenanceViolations(path, literal string) []string {
	stmt := provenanceLockingRE.ReplaceAllString(provenanceSQLCommentRE.ReplaceAllString(literal, " "), " ")
	isWrite := provenanceWriteRE.MatchString(stmt)
	inside := strings.HasPrefix(path, provenanceWriterDir)
	var out []string
	for _, fam := range provenanceFamilies {
		hasColumn := fam.column.MatchString(stmt)
		hasClass := fam.class != nil && fam.class.MatchString(stmt)
		if !hasColumn && !hasClass {
			continue
		}
		prefix := fam.prefix
		if isWrite && fam.class != nil {
			switch {
			case hasColumn && !hasClass:
				out = append(out, path+": a write names "+prefix+"enrolled_at without "+prefix+"enrol_class")
			case hasClass && !hasColumn:
				out = append(out, path+": a write names "+prefix+"enrol_class without "+prefix+"enrolled_at")
			}
		}
		switch {
		case inside:
		case isWrite:
			out = append(out, path+": "+fam.name+" is written outside "+provenanceWriterDir)
		case !provenanceReadRE.MatchString(stmt):
			out = append(out, path+": "+fam.name+" is named outside "+provenanceWriterDir+" by something other than a pure read (a statement fragment?)")
		}
	}
	return out
}

// provenanceConstString evaluates an expression made only of string literals
// joined by +. ok is false for anything else (a variable, a call).
func provenanceConstString(e ast.Expr) (string, bool) {
	switch n := e.(type) {
	case *ast.BasicLit:
		if n.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(n.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return provenanceConstString(n.X)
	case *ast.BinaryExpr:
		if n.Op != token.ADD {
			return "", false
		}
		l, lok := provenanceConstString(n.X)
		r, rok := provenanceConstString(n.Y)
		return l + r, lok && rok
	}
	return "", false
}

// provenanceLiterals returns every string a file's SQL could be built from: each
// all-constant concatenation whole, and every other string literal on its own.
func provenanceLiterals(t *testing.T, path string, body []byte) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, body, 0)
	require.NoError(t, err, "parse %s", path)
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			if s, ok := provenanceConstString(e); ok {
				out = append(out, s)
				return false
			}
		case *ast.BasicLit:
			if s, ok := provenanceConstString(e); ok {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}

// provenanceExportedLeaks returns one message per exported const or var in a
// file under provenanceWriterDir whose value names a provenance or maturity
// column. An exported name hands the column to every other package as a ready
// made fragment, which is how a writer outside the directory would assemble one.
func provenanceExportedLeaks(t *testing.T, path string, body []byte) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, body, 0)
	require.NoError(t, err, "parse %s", path)
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			var names bool
			for _, v := range vs.Values {
				ast.Inspect(v, func(n ast.Node) bool {
					if lit, isLit := n.(*ast.BasicLit); isLit && lit.Kind == token.STRING {
						if s, uq := provenanceConstString(lit); uq && provenanceNames(s) {
							names = true
						}
					}
					return true
				})
			}
			if !names {
				continue
			}
			for _, name := range vs.Names {
				if ast.IsExported(name.Name) {
					out = append(out, path+": exported "+name.Name+" carries a provenance or maturity column name")
				}
			}
		}
	}
	return out
}

// provenanceMigrationViolations returns the message for a migration file other
// than 000189_* that names a provenance or maturity column. The file is not
// stripped of comments: a later migration naming the column in any way is a
// reason to look.
func provenanceMigrationViolations(name, body string) []string {
	if strings.HasPrefix(name, provenanceOwnerSQL) {
		return nil
	}
	if provenanceNames(body) {
		return []string{"migrations/" + name + ": names a provenance or maturity column; only " + provenanceOwnerSQL + "* may"}
	}
	return nil
}

func TestProvenanceWritersAreConfinedAndPaired(t *testing.T) {
	// The walk and the reads go through one os.Root, so a symlink inside the tree
	// cannot point a read outside it.
	root, err := os.OpenRoot(filepath.Join("..", ".."))
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	fsys := root.FS()

	var files, literals, migrationFiles int
	var sawKnownFile, sawOwner bool
	var violations []string
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
		isMigration := strings.HasPrefix(path, "migrations/") && strings.HasSuffix(path, ".sql")
		isGo := strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
		if !isMigration && !isGo {
			return nil
		}
		body, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return readErr
		}
		if isMigration {
			migrationFiles++
			name := strings.TrimPrefix(path, "migrations/")
			if strings.HasPrefix(name, provenanceOwnerSQL) {
				sawOwner = true
			}
			violations = append(violations, provenanceMigrationViolations(name, string(body))...)
			return nil
		}
		files++
		if path == "internal/mfa/handlers.go" {
			sawKnownFile = true
		}
		if strings.HasPrefix(path, provenanceWriterDir) {
			violations = append(violations, provenanceExportedLeaks(t, path, body)...)
		}
		for _, lit := range provenanceLiterals(t, path, body) {
			literals++
			violations = append(violations, provenanceViolations(path, lit)...)
		}
		return nil
	})
	require.NoError(t, err)

	// The scan must have looked at real code, or an empty result proves nothing.
	require.True(t, sawKnownFile, "the walk must reach internal/mfa/handlers.go")
	require.True(t, sawOwner, "the walk must reach the 000189 migration files, which do name the columns")
	require.Greater(t, files, 100, "the walk must cover the module")
	require.Greater(t, literals, 1000, "the walk must examine the module's string literals")
	require.Greater(t, migrationFiles, 100, "the walk must cover the migrations")
	assert.Empty(t, violations, "provenance and maturity columns must be confined to %s (and, in SQL, to %s*)",
		provenanceWriterDir, provenanceOwnerSQL)
}

// Positive control: the rule flags each planted offender and passes the clean
// ones, so the scan above cannot be green merely because no writer exists.
func TestProvenanceRuleFlagsPlantedOffenders(t *testing.T) {
	const inside = provenanceWriterDir + "write.go"
	const outside = "internal/mfa/write.go"

	offenders := []struct{ name, path, literal, want string }{
		{"enrolled_at without enrol_class", inside,
			`UPDATE user_mfa_totp SET enrolled_at = $2 WHERE user_id = $1`, "enrolled_at without enrol_class"},
		{"enrol_class without enrolled_at", inside,
			`UPDATE user_mfa_totp SET enrol_class = $2 WHERE user_id = $1`, "enrol_class without enrolled_at"},
		{"insert names enrolled_at only", inside,
			`INSERT INTO user_recovery_keys (user_id, enrolled_at) VALUES ($1, $2)`, "enrolled_at without enrol_class"},
		{"password pair broken", inside,
			`UPDATE users SET password_enrolled_at = $2 WHERE id = $1`, "password_enrolled_at without password_enrol_class"},
		{"email pair broken", inside,
			`UPDATE users SET email_enrol_class = $2 WHERE id = $1`, "email_enrol_class without email_enrolled_at"},
		{"class of the wrong family", inside,
			`UPDATE users SET password_enrolled_at = $2, email_enrol_class = $3 WHERE id = $1`, "password_enrolled_at without password_enrol_class"},
		{"paired write outside accountguard", outside,
			`UPDATE user_mfa_totp SET enrolled_at = $2, enrol_class = $3 WHERE user_id = $1`, "written outside " + provenanceWriterDir},
		{"paired insert outside accountguard", outside,
			`INSERT INTO user_sso_identities (user_id, enrolled_at, enrol_class) VALUES ($1, $2, $3)`, "written outside " + provenanceWriterDir},
		{"paired password write outside accountguard", "internal/users/write.go",
			`UPDATE users SET password_enrolled_at = $2, password_enrol_class = $3 WHERE id = $1`, "password_enrolment provenance is written outside"},

		// Red-team RT-1: statements an earlier, narrower write test did not see.
		{"aliased UPDATE without AS", outside,
			`UPDATE user_mfa_totp t SET enrolled_at = $2, enrol_class = $3 WHERE t.user_id = $1`, "written outside"},
		{"aliased UPDATE with AS, unpaired", outside,
			`UPDATE users AS u SET password_hash = $2, password_enrolled_at = now() WHERE u.id = $1`, "written outside"},
		{"UPDATE ONLY", outside,
			`UPDATE ONLY user_sso_identities SET enrolled_at = $2, enrol_class = $3 WHERE id = $1`, "written outside"},
		{"comment between table and SET", outside,
			`UPDATE user_recovery_keys /* re-enrol */ SET enrolled_at = $2, enrol_class = $3 WHERE user_id = $1`, "written outside"},
		{"MERGE ... WHEN MATCHED THEN UPDATE SET", outside,
			"MERGE INTO user_mfa_webauthn w USING (SELECT $1::uuid AS id) s ON w.id = s.id\n" +
				"WHEN MATCHED THEN UPDATE SET enrolled_at = now(), enrol_class = 3", "written outside"},
		{"Sprintf table placeholder", outside,
			`UPDATE %s SET enrolled_at = now(), enrol_class = 3 WHERE user_id = $1`, "written outside"},
		{"COPY", outside,
			`COPY user_mfa_totp (user_id, enrolled_at, enrol_class) FROM STDIN`, "written outside"},

		// Maturity columns are confined too, with no pairing rule.
		{"matures_at writer outside accountguard", outside,
			`UPDATE user_mfa_totp SET matures_at = $2 WHERE user_id = $1`, "maturity is written outside"},
		{"password_matures_at writer outside accountguard", outside,
			`UPDATE users SET password_matures_at = $2 WHERE id = $1`, "password_maturity is written outside"},
		{"email_matures_at insert outside accountguard", outside,
			`INSERT INTO users (id, email_matures_at) VALUES ($1, $2)`, "email_maturity is written outside"},

		// Fragments: no single literal is both a write and a column.
		{"bare column literal (an addClause-style argument)", outside,
			`enrolled_at`, "something other than a pure read"},
		{"bare maturity column literal", outside,
			`matures_at`, "something other than a pure read"},
		{"column assignment fragment", outside,
			`enrolled_at = $2`, "something other than a pure read"},
		{"prose naming the columns is indistinguishable from a fragment", outside,
			`enrolled_at and enrol_class are set by the runner`, "something other than a pure read"},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			got := provenanceViolations(tc.path, tc.literal)
			require.NotEmpty(t, got, "the rule must flag this literal")
			assert.Contains(t, strings.Join(got, "\n"), tc.want)
		})
	}

	clean := []struct{ name, path, literal string }{
		{"paired write inside accountguard", inside,
			`UPDATE user_mfa_totp SET enrolled_at = $2, enrol_class = $3 WHERE user_id = $1`},
		{"paired password write inside accountguard", inside,
			`UPDATE users SET password_enrolled_at = $2, password_enrol_class = $3 WHERE id = $1`},
		{"both families paired inside accountguard", inside,
			`UPDATE users SET password_enrolled_at = $2, password_enrol_class = $3, email_enrolled_at = $2, email_enrol_class = $3 WHERE id = $1`},
		{"paired insert inside accountguard", inside,
			`INSERT INTO user_recovery_keys (user_id, enrolled_at, enrol_class) VALUES ($1, $2, $3)`},
		{"maturity write inside accountguard", inside,
			`UPDATE user_mfa_totp SET matures_at = $2 WHERE user_id = $1`},
		{"read outside accountguard", outside,
			`SELECT enrolled_at, enrol_class FROM user_mfa_totp WHERE user_id = $1`},
		{"maturity read outside accountguard", outside,
			`SELECT matures_at FROM user_mfa_totp WHERE user_id = $1`},
		{"locking read is not a write", outside,
			`SELECT enrolled_at FROM user_mfa_totp WHERE user_id = $1 FOR UPDATE`},
		{"read with a commented-out write", outside,
			"SELECT enrolled_at FROM user_mfa_totp -- UPDATE t SET x\nWHERE user_id = $1"},
		{"write that names no column", outside,
			`UPDATE user_mfa_totp SET last_used_step = $2 WHERE user_id = $1`},
		{"a different column that merely ends in the name", outside,
			`UPDATE user_mfa_totp SET not_enrolled_at_all = $2 WHERE user_id = $1`},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, provenanceViolations(tc.path, tc.literal))
		})
	}
}

// Go sources whose SQL is assembled so that no single literal is both a write and
// a column. The scan pipeline (literal collector, then the rule) must flag them.
func TestProvenancePipelineFlagsAssembledWriters(t *testing.T) {
	const outside = "internal/mfa/redteam_writer.go"
	cases := []struct{ name, src string }{
		{"pq.CopyIn column list",
			"package mfa\n\nvar stmt = pq.CopyIn(\"user_mfa_webauthn\", \"user_id\", \"credential_id\", \"public_key\", \"enrolled_at\", \"enrol_class\")\n"},
		{"UPDATE joined with a named constant",
			"package mfa\n\nconst enrolledCol = \"enrolled_at\"\n\nvar q = \"UPDATE user_mfa_totp SET \" + enrolledCol + \" = now() WHERE user_id = $1\"\n"},
		{"fmt.Sprintf with a table placeholder",
			"package mfa\n\nvar q = fmt.Sprintf(\"UPDATE %s SET enrolled_at = now(), enrol_class = 3 WHERE user_id = $1\", table)\n"},
		{"bare column as an addClause argument",
			"package mfa\n\nfunc f() { clauses = addClause(clauses, \"enrolled_at\", ts) }\n"},
		{"matures_at writer",
			"package mfa\n\nvar q = `UPDATE user_sso_identities SET matures_at = $2 WHERE id = $1`\n"},
		{"split constant concatenation",
			"package mfa\n\nconst q = \"UPDATE user_mfa_totp SET \" +\n\t\"enrolled_at = $2 WHERE user_id = $1\"\n\nvar raw = `INSERT INTO users (enrol_class) VALUES ($1)`\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, lit := range provenanceLiterals(t, outside, []byte(tc.src)) {
				got = append(got, provenanceViolations(outside, lit)...)
			}
			assert.NotEmpty(t, got, "the scan pipeline must flag this Go source")
		})
	}
}

// Inside internal/accountguard an exported const or var may not carry a column
// name; an unexported one, and a name-free export, may.
func TestProvenanceExportedLeaksArePlanted(t *testing.T) {
	const path = provenanceWriterDir + "columns.go"
	offenders := []struct{ name, src string }{
		{"exported const", "package accountguard\n\nconst EnrolledColumn = \"enrolled_at\"\n"},
		{"exported var", "package accountguard\n\nvar MaturityColumn = \"password_matures_at\"\n"},
		{"exported const in a block, concatenated", "package accountguard\n\nconst (\n\tUpdateEnrol = \"UPDATE t SET \" + \"enrolled_at = $1\"\n)\n"},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEmpty(t, provenanceExportedLeaks(t, path, []byte(tc.src)))
		})
	}
	clean := []struct{ name, src string }{
		{"unexported const", "package accountguard\n\nconst enrolledColumn = \"enrolled_at\"\n"},
		{"exported const naming no column", "package accountguard\n\nconst Version = \"v1\"\n"},
	}
	for _, tc := range clean {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, provenanceExportedLeaks(t, path, []byte(tc.src)))
		})
	}
}

// Only 000189_* may name the columns in SQL.
func TestProvenanceMigrationPassFlagsPlantedFiles(t *testing.T) {
	assert.NotEmpty(t, provenanceMigrationViolations("000190_later.up.sql", "ALTER TABLE user_mfa_totp ALTER COLUMN matures_at SET DEFAULT now();"))
	assert.NotEmpty(t, provenanceMigrationViolations("000190_later.down.sql", "-- enrol_class\nSELECT 1;"))
	assert.Empty(t, provenanceMigrationViolations("000189_accountguard_maturity.up.sql", "ALTER TABLE users ADD COLUMN password_matures_at timestamptz;"))
	assert.Empty(t, provenanceMigrationViolations("000190_later.up.sql", "ALTER TABLE users ADD COLUMN nickname text;"))
}

// The literal collector must see a statement split across a constant
// concatenation, and must read raw strings, or a writer could dodge the rule.
func TestProvenanceLiteralsJoinConstantConcatenation(t *testing.T) {
	body := []byte("package p\n\nconst q = \"UPDATE user_mfa_totp SET \" +\n\t\"enrolled_at = $2 WHERE user_id = $1\"\n\nvar raw = `INSERT INTO users (enrol_class) VALUES ($1)`\n")
	got := provenanceLiterals(t, "internal/mfa/split.go", body)
	require.Contains(t, got, "UPDATE user_mfa_totp SET enrolled_at = $2 WHERE user_id = $1")
	require.Contains(t, got, "INSERT INTO users (enrol_class) VALUES ($1)")

	all := make([]string, 0, len(got))
	for _, lit := range got {
		all = append(all, provenanceViolations("internal/mfa/split.go", lit)...)
	}
	assert.NotEmpty(t, all, "a split statement must still be flagged")
}
