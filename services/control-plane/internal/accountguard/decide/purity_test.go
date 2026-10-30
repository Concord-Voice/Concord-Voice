package decide

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The decide package is a pure function over a snapshot (spec §7 "Package": imports are
// limited to time, sort, errors and bytes, and a scan bans the clock; plan Global
// Constraints and Task 1). Non-test files read no clock, hold no package-level variable
// and declare no init.
//
// The checker is a go/ast walk over source text, copied in shape from
// internal/authoritycontract/purity_test.go: it sees a method value (clk := time.Now),
// an aliased import (import tm "time"), a dot or blank import of time, and time.AfterFunc.
// It is a function over source text so TestPurityCheckerFindsPlantedViolations can feed it
// planted sources and prove each rule bites; a scanner never shown a violation passes
// vacuously.

// allowedImports is the closed import allowlist for non-test files (spec §7).
var allowedImports = map[string]bool{
	"time":   true,
	"sort":   true,
	"errors": true,
	"bytes":  true,
}

// clockSelectors are the nine banned names on the time package: the authoritycontract
// list (internal/authoritycontract/purity_test.go isClockSelector).
var clockSelectors = map[string]bool{
	"Now": true, "Since": true, "Until": true, "Tick": true, "After": true,
	"AfterFunc": true, "NewTimer": true, "NewTicker": true, "Sleep": true,
}

type parsedFile struct {
	name string
	file *ast.File
}

type purityChecker struct {
	fset     *token.FileSet
	findings []string
}

func (c *purityChecker) report(pos token.Pos, rule, format string, args ...any) {
	p := c.fset.Position(pos)
	c.findings = append(c.findings, fmt.Sprintf("%s:%d: [%s] %s", p.Filename, p.Line, rule, fmt.Sprintf(format, args...)))
}

// purityFindings checks the supplied non-test sources (file name -> text) and returns one
// line per violation, as "file:line: [rule] detail", sorted. Empty means pure. Rules:
// import, clock, var, init, parse.
func purityFindings(srcs map[string]string) []string {
	names := make([]string, 0, len(srcs))
	for name := range srcs {
		names = append(names, name)
	}
	sort.Strings(names)

	fset := token.NewFileSet()
	c := &purityChecker{fset: fset}
	var files []parsedFile
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, srcs[name], parser.SkipObjectResolution)
		if err != nil {
			c.findings = append(c.findings, fmt.Sprintf("%s:1: [parse] cannot check an unparseable file: %v", name, err))
			continue
		}
		files = append(files, parsedFile{name: name, file: f})
	}
	for _, pf := range files {
		if timeName := c.checkImports(pf); timeName != "" {
			c.checkClock(pf.file, timeName)
		}
		c.checkTopLevel(pf)
	}
	sort.Strings(c.findings)
	return c.findings
}

// checkImports reports imports outside the allowlist and returns the local name the time
// package is bound to in this file ("" when absent or when the import form hides clock reads).
func (c *purityChecker) checkImports(pf parsedFile) string {
	timeName := ""
	for _, imp := range pf.file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			c.report(imp.Pos(), "import", "unreadable import path %s", imp.Path.Value)
			continue
		}
		if !allowedImports[p] {
			c.report(imp.Pos(), "import", "%q is outside the allowlist {time, sort, errors, bytes}", p)
			continue
		}
		if p != "time" {
			continue
		}
		switch {
		case imp.Name == nil:
			timeName = "time"
		case imp.Name.Name == "." || imp.Name.Name == "_":
			c.report(imp.Pos(), "clock", "the time package is imported as %q; a dot or blank import hides clock reads from this check", imp.Name.Name)
		default:
			timeName = imp.Name.Name
		}
	}
	return timeName
}

// checkClock flags every selector on the time package that names a clock or timer
// function. Matching the selector, not the call, is what catches a method value.
func (c *purityChecker) checkClock(f *ast.File, timeName string) {
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == timeName && clockSelectors[sel.Sel.Name] {
			c.report(sel.Pos(), "clock", "%s.%s reads or waits on the clock; Input.Now is the only clock", timeName, sel.Sel.Name)
		}
		return true
	})
}

// checkTopLevel reports any package-level var and any func init.
func (c *purityChecker) checkTopLevel(pf parsedFile) {
	for _, d := range pf.file.Decls {
		switch decl := d.(type) {
		case *ast.GenDecl:
			if decl.Tok != token.VAR {
				continue
			}
			for _, s := range decl.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, id := range vs.Names {
					c.report(id.Pos(), "var", "package-level var %s; constants are const and the sentinel instants are functions", id.Name)
				}
			}
		case *ast.FuncDecl:
			if decl.Recv == nil && decl.Name.Name == "init" {
				c.report(decl.Pos(), "init", "func init is banned; the package has no initialisation order")
			}
		}
	}
}

// readPackageSources reads every non-test .go file of the package directory.
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

// TestDecidePackageIsPure runs the scan over the real package (plan Task 1). A directory
// with no non-test source must not pass vacuously.
func TestDecidePackageIsPure(t *testing.T) {
	srcs := readPackageSources(t)
	require.NotEmpty(t, srcs, "no non-test source files were found; the scan would pass vacuously")
	require.Contains(t, srcs, "doc.go", "plan File Structure: doc.go carries the package comment")
	require.Empty(t, purityFindings(srcs))
}
