package authoritycontract

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The verification API is pure (DoR §2.12 heading: "pure, no clock reads"; plan
// Global Constraint "Pure, no clock reads"): non-test files read no clock, hold no
// package-level mutable state, and take randomness only in wrap.go (§2.5's fresh
// ephemeral key, nonce and check value).
//
// The checker is a go/ast walk, not a text match, so it sees a method value
// (`clk := time.Now`), an aliased import (`import tm "time"`), a call split over
// lines and `time.AfterFunc`. It is a function over source text so
// TestPurityCheckerFindsPlantedViolations can feed it planted sources and prove each
// rule bites; a scanner that is never shown a violation passes vacuously.

// purityPolicy is what the checker is allowed to let through.
type purityPolicy struct {
	// randomnessFile is the one file allowed to import crypto/rand.
	randomnessFile string
	// vars maps every permitted top-level var name to the one-line reason it is not
	// mutable state. A var absent from this map is a finding.
	vars map[string]string
	// initOnlyMutators maps "func>var" to the reason that function may write that
	// package-level var even though it is not init. Such a function must itself only
	// be called from init; the checker flags any call to it elsewhere.
	initOnlyMutators map[string]string
}

// packagePurityPolicy is the allowlist for the real package. Adding a package-level
// var to a non-test file fails TestNoClockReadsAndConfinedRandomness until the var is
// listed here WITH a reason; the orchestrator reviews each such decision. A var is
// acceptable only when it is written at its declaration or in init and never after.
func packagePurityPolicy() purityPolicy {
	vars := map[string]string{
		"registry":         "layout registry; written only by register(), which only init calls",
		"standaloneCaps":   "section 2.3 size-cap table, assigned at declaration and only read",
		"reasonTable":      "Reason to verdict table, assigned at declaration and only read",
		"QueryRespFailure": "the one byte-identical failed-query answer; callers must not modify it and Encode returns a copy",
		"table":            "DoR section 2.9 rows 1-22 as data; read only through Rows/RowByN/rowsFor, which return deep copies",
		"privateRows":      "private ops' authority-only columns; read only through PrivateRow, which returns a deep copy",
		"anyButPePrE":      "a from-state list shared by table rows at declaration; only read",
		"anyButPrE":        "a from-state list shared by table rows at declaration; only read",
		"anyButE":          "row 20's explicit from-state list (DoR's list), shared at declaration; only read",
		"stateReasons":     "the verdicts after which VerifyChain may persist the chain, assigned at declaration and only read",
		"rowChecks":        "rows 20-22's client checks, registered only from init; an unregistered row fails closed",
	}
	allow := func(reason string, names ...string) {
		for _, n := range names {
			vars[n] = reason
		}
	}
	allow("error sentinel, assigned at declaration", "ErrMalformed", "ErrIgnored")
	allow("kind descriptor, assigned at declaration and only read",
		"kP97", "kEKSPKI", "kS36", "kS36v4", "kU", "kU1", "kU01", "kDev")
	allow("closed enum value set, assigned at declaration and only read",
		"publishedOps", "deviceOps", "scopeValues", "stateValues", "priorStateValues", "assuranceValues",
		"segmentAssurances", "suspenders", "actorValues", "sigRoles", "factorNames", "bindKinds", "bindWaits",
		"signedTypes", "secretsOps", "nodeRoles", "ekAlgs", "refusalCodes", "errorCodes")
	allow("compiled regexp, assigned at declaration; a *Regexp is safe for concurrent use", "reHex64", "reClientV")
	return purityPolicy{
		randomnessFile: "wrap.go",
		vars:           vars,
		initOnlyMutators: map[string]string{
			"register>registry": "register is called only from each layout file's init; the checker flags any other caller",
		},
	}
}

// parsedFile is one parsed source with the name it was supplied under.
type parsedFile struct {
	name string
	file *ast.File
}

type purityChecker struct {
	fset     *token.FileSet
	policy   purityPolicy
	findings []string
}

func (c *purityChecker) report(pos token.Pos, rule, format string, args ...any) {
	p := c.fset.Position(pos)
	c.findings = append(c.findings, fmt.Sprintf("%s:%d: [%s] %s", p.Filename, p.Line, rule, fmt.Sprintf(format, args...)))
}

// purityFindings checks the supplied non-test sources (file name -> text) and returns
// one line per violation, naming the file, line and rule. Empty means pure.
func purityFindings(srcs map[string]string, policy purityPolicy) []string {
	fset, files, parseErrs := parseSources(srcs)
	c := &purityChecker{fset: fset, policy: policy, findings: parseErrs}
	pkgVars := topLevelVarNames(files)
	for _, pf := range files {
		c.checkImportsAndClock(pf)
		c.checkVarDecls(pf)
		c.checkMutations(pf, pkgVars)
	}
	sort.Strings(c.findings)
	return c.findings
}

// staleAllowances lists policy entries that name a var or function the sources no
// longer declare, so a deleted var cannot leave a standing exemption behind.
func staleAllowances(srcs map[string]string, policy purityPolicy) []string {
	_, files, _ := parseSources(srcs)
	vars := topLevelVarNames(files)
	funcs := topLevelFuncNames(files)
	var stale []string
	for name := range policy.vars {
		if !vars[name] {
			stale = append(stale, "allowlisted var "+name+" is not declared")
		}
	}
	for key := range policy.initOnlyMutators {
		fn, v, _ := strings.Cut(key, ">")
		if !funcs[fn] || !vars[v] {
			stale = append(stale, "init-only mutator "+key+" names a function or var that is not declared")
		}
	}
	sort.Strings(stale)
	return stale
}

func parseSources(srcs map[string]string) (*token.FileSet, []parsedFile, []string) {
	names := make([]string, 0, len(srcs))
	for name := range srcs {
		names = append(names, name)
	}
	sort.Strings(names)
	fset := token.NewFileSet()
	var files []parsedFile
	var errs []string
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, srcs[name], parser.SkipObjectResolution)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s:1: [parse] cannot check an unparseable file: %v", name, err))
			continue
		}
		files = append(files, parsedFile{name: name, file: f})
	}
	return fset, files, errs
}

func topLevelVarNames(files []parsedFile) map[string]bool {
	vars := map[string]bool{}
	for _, pf := range files {
		forEachTopLevelVar(pf.file, func(id *ast.Ident) { vars[id.Name] = true })
	}
	return vars
}

func topLevelFuncNames(files []parsedFile) map[string]bool {
	funcs := map[string]bool{}
	for _, pf := range files {
		for _, d := range pf.file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				funcs[fd.Name.Name] = true
			}
		}
	}
	return funcs
}

func forEachTopLevelVar(f *ast.File, visit func(*ast.Ident)) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, s := range gd.Specs {
			if vs, ok := s.(*ast.ValueSpec); ok {
				for _, id := range vs.Names {
					visit(id)
				}
			}
		}
	}
}

// ── clock and randomness ──────────────────────────────────────────────────────

func (c *purityChecker) checkImportsAndClock(pf parsedFile) {
	if timeName := c.checkImports(pf); timeName != "" {
		c.checkClock(pf.file, timeName)
	}
}

// checkImports reports the randomness and dot/blank-time imports and returns the
// local name the "time" package is bound to in this file ("" when absent or refused).
func (c *purityChecker) checkImports(pf parsedFile) string {
	timeName := ""
	for _, imp := range pf.file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			c.report(imp.Pos(), "import", "unreadable import path %s", imp.Path.Value)
			continue
		}
		switch p {
		case "time":
			timeName = c.timeLocalName(imp)
		case "crypto/rand":
			if path.Base(pf.name) != c.policy.randomnessFile {
				c.report(imp.Pos(), "randomness", "crypto/rand is imported outside %s; randomness is confined there (DoR §2.5)", c.policy.randomnessFile)
			}
		case "math/rand", "math/rand/v2":
			c.report(imp.Pos(), "randomness", "%s is imported; the verification API takes no math randomness anywhere", p)
		}
	}
	return timeName
}

func (c *purityChecker) timeLocalName(imp *ast.ImportSpec) string {
	if imp.Name == nil {
		return "time"
	}
	if imp.Name.Name == "." || imp.Name.Name == "_" {
		c.report(imp.Pos(), "clock", "the time package is imported as %q; a dot or blank import hides clock reads from this check, import it by name", imp.Name.Name)
		return ""
	}
	return imp.Name.Name
}

func isClockSelector(name string) bool {
	switch name {
	case "Now", "Since", "Until", "Tick", "After", "AfterFunc", "NewTimer", "NewTicker", "Sleep":
		return true
	}
	return false
}

// checkClock flags every selector on the time package that names a clock or timer
// function. Matching the selector, not the call, is what catches a method value.
func (c *purityChecker) checkClock(f *ast.File, timeName string) {
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == timeName && isClockSelector(sel.Sel.Name) {
			c.report(sel.Pos(), "clock", "%s.%s reads or waits on the clock; the verification API takes now as a parameter", timeName, sel.Sel.Name)
		}
		return true
	})
}

// ── package-level state ───────────────────────────────────────────────────────

func (c *purityChecker) checkVarDecls(pf parsedFile) {
	forEachTopLevelVar(pf.file, func(id *ast.Ident) {
		if _, ok := c.policy.vars[id.Name]; !ok {
			c.report(id.Pos(), "pkg-state", "top-level var %s is package-level state and is not on the allowlist; "+
				"add it to packagePurityPolicy().vars in purity_test.go with a one-line reason it is never written after init", id.Name)
		}
	})
}

func isInitFunc(fd *ast.FuncDecl) bool {
	return fd.Recv == nil && fd.Name.Name == "init"
}

func (c *purityChecker) checkMutations(pf parsedFile, pkgVars map[string]bool) {
	for _, d := range pf.file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && !isInitFunc(fd) {
			c.checkFuncMutations(fd, pkgVars)
		}
	}
}

// checkFuncMutations walks one non-init function, including its function literals.
// Matching is by name: a local that shadows a package-level var is a false positive,
// which fails closed; the fix is to rename the local.
func (c *purityChecker) checkFuncMutations(fd *ast.FuncDecl, pkgVars map[string]bool) {
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if s.Tok != token.DEFINE {
				for _, lhs := range s.Lhs {
					c.checkWrite(fd, lhs, pkgVars)
				}
			}
		case *ast.IncDecStmt:
			c.checkWrite(fd, s.X, pkgVars)
		case *ast.CallExpr:
			c.checkCall(fd, s, pkgVars)
		}
		return true
	})
}

func (c *purityChecker) checkWrite(fd *ast.FuncDecl, target ast.Expr, pkgVars map[string]bool) {
	id := rootIdent(target)
	if id == nil || !pkgVars[id.Name] {
		return
	}
	if _, ok := c.policy.initOnlyMutators[fd.Name.Name+">"+id.Name]; ok {
		return
	}
	c.report(target.Pos(), "pkg-mutation", "function %s writes package-level var %s; only init may. "+
		"If %s is a local that shadows the package var, rename the local", fd.Name.Name, id.Name, id.Name)
}

func (c *purityChecker) checkCall(fd *ast.FuncDecl, call *ast.CallExpr, pkgVars map[string]bool) {
	fn, ok := call.Fun.(*ast.Ident)
	if !ok {
		return
	}
	switch fn.Name {
	case "delete", "clear", "copy":
		if len(call.Args) > 0 {
			c.checkWrite(fd, call.Args[0], pkgVars)
		}
	default:
		if c.isInitOnlyMutator(fn.Name) {
			c.report(call.Pos(), "pkg-mutation", "%s writes package state for init and may be called only from init; %s calls it", fn.Name, fd.Name.Name)
		}
	}
}

func (c *purityChecker) isInitOnlyMutator(name string) bool {
	for key := range c.policy.initOnlyMutators {
		if fn, _, _ := strings.Cut(key, ">"); fn == name {
			return true
		}
	}
	return false
}

// rootIdent is the identifier a written expression is rooted in: it looks through
// index, slice, selector, pointer and parenthesis wrappers.
func rootIdent(e ast.Expr) *ast.Ident {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return nil
		}
	}
}

// ── the real package ──────────────────────────────────────────────────────────

func readPackageSources(t *testing.T) map[string]string {
	t.Helper()
	pkg := os.DirFS(".")
	entries, err := fs.ReadDir(pkg, ".")
	require.NoError(t, err)
	srcs := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := fs.ReadFile(pkg, name)
		require.NoError(t, err)
		srcs[name] = string(b)
	}
	return srcs
}

func TestNoClockReadsAndConfinedRandomness(t *testing.T) {
	srcs := readPackageSources(t)
	// A glob that finds nothing must not pass: these two files exist since A1.
	require.NotEmpty(t, srcs, "no non-test source files were found; the check would pass vacuously")
	require.Contains(t, srcs, "reason.go")
	require.Contains(t, srcs, "codec.go")

	policy := packagePurityPolicy()
	for name, reason := range policy.vars {
		require.NotEmpty(t, strings.TrimSpace(reason), "allowlisted var %s needs a reason", name)
	}
	for key, reason := range policy.initOnlyMutators {
		require.NotEmpty(t, strings.TrimSpace(reason), "init-only mutator %s needs a reason", key)
	}

	findings := purityFindings(srcs, policy)
	require.Empty(t, findings, "purity findings (a new package-level var needs an allowlist entry with a reason in "+
		"packagePurityPolicy):\n%s", strings.Join(findings, "\n"))
	require.Empty(t, staleAllowances(srcs, policy), "the allowlist names declarations that no longer exist")
}
