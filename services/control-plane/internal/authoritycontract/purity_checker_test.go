package authoritycontract

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// plantedPolicy permits only the names the planted sources declare on purpose, so a
// planted violation can only be caught by the rule it names.
func plantedPolicy() purityPolicy {
	return purityPolicy{
		randomnessFile: "wrap.go",
		vars: map[string]string{
			"table":    "read-only table",
			"registry": "layout registry",
			"pkgMap":   "planted map the mutation cases write",
			"pkgInt":   "planted counter the mutation cases write",
			"pkgSlice": "planted slice the mutation cases write",
			"pkgStr":   "planted struct the mutation cases write",
			"pkgPtr":   "planted pointer the mutation cases write",
		},
		initOnlyMutators: map[string]string{"register>registry": "called from init only"},
	}
}

const plantedDecls = `
var pkgMap = map[string]int{}
var pkgInt int
var pkgSlice = []int{1}
var pkgStr struct{ f int }
var pkgPtr = new(int)
`

func plant(file, body string) map[string]string {
	return map[string]string{file: "package p\n" + body}
}

// requireRule asserts a finding names the file, a line and the rule, so a later task
// that trips the check learns where and what to do.
func requireRule(t *testing.T, findings []string, file, rule string) {
	t.Helper()
	for _, f := range findings {
		if strings.HasPrefix(f, file+":") && strings.Contains(f, "["+rule+"]") {
			return
		}
	}
	t.Fatalf("no [%s] finding for %s in %q", rule, file, findings)
}

func TestPurityCheckerFindsPlantedViolations(t *testing.T) {
	cases := []struct {
		name string
		srcs map[string]string
		rule string
	}{
		{"time.Now call", plant("x.go", `import "time"
func f() int64 { return time.Now().Unix() }`), "clock"},
		{"time.Now method value in a function", plant("x.go", `import "time"
func f() { clk := time.Now; _ = clk }`), "clock"},
		{"time.Now method value in a package var", plant("x.go", `import "time"
var now = time.Now`), "clock"},
		{"aliased time import", plant("x.go", `import tm "time"
func f() int64 { return tm.Now().Unix() }`), "clock"},
		{"dot import of time", plant("x.go", `import . "time"
func f() { _ = Duration(1) }`), "clock"},
		{"blank import of time", plant("x.go", `import _ "time"`), "clock"},
		{"time.AfterFunc", plant("x.go", `import "time"
func f() { time.AfterFunc(time.Second, func() {}) }`), "clock"},
		{"call split over lines", plant("x.go", `import "time"
func f() int64 {
	return time.
		Now().
		Unix()
}`), "clock"},
		{"time.Since", plant("x.go", `import "time"
func f(t time.Time) time.Duration { return time.Since(t) }`), "clock"},
		{"time.Until", plant("x.go", `import "time"
func f(t time.Time) time.Duration { return time.Until(t) }`), "clock"},
		{"time.Sleep", plant("x.go", `import "time"
func f() { time.Sleep(1) }`), "clock"},
		{"time.NewTicker", plant("x.go", `import "time"
func f() { _ = time.NewTicker(1) }`), "clock"},
		{"time.Now in wrap.go is still a clock read", plant("wrap.go", `import "time"
func f() int64 { return time.Now().Unix() }`), "clock"},
		{"crypto/rand outside wrap.go", plant("x.go", `import "crypto/rand"
func f() { _, _ = rand.Read(nil) }`), "randomness"},
		{"aliased crypto/rand outside wrap.go", plant("x.go", `import cr "crypto/rand"
func f() { _, _ = cr.Read(nil) }`), "randomness"},
		{"math/rand", plant("x.go", `import "math/rand"
func f() int { return rand.Int() }`), "randomness"},
		{"math/rand/v2", plant("x.go", `import "math/rand/v2"
func f() int { return rand.Int() }`), "randomness"},
		{"math/rand in wrap.go", plant("wrap.go", `import "math/rand"
func f() int { return rand.Int() }`), "randomness"},
		{"unlisted top-level var", plant("x.go", `var unlisted = 1`), "pkg-state"},
		{"unlisted var in a group", plant("x.go", "var (\n\ttable = 1\n\tsneaky = 2\n)"), "pkg-state"},
		{"blank-named top-level var", plant("x.go", `var _ = 1`), "pkg-state"},
		{"assignment to a package var", plant("x.go", plantedDecls+`func f() { pkgInt = 1 }`), "pkg-mutation"},
		{"compound assignment", plant("x.go", plantedDecls+`func f() { pkgInt += 2 }`), "pkg-mutation"},
		{"increment", plant("x.go", plantedDecls+`func f() { pkgInt++ }`), "pkg-mutation"},
		{"map index assignment", plant("x.go", plantedDecls+`func f(k string, v int) { pkgMap[k] = v }`), "pkg-mutation"},
		{"slice element assignment", plant("x.go", plantedDecls+`func f() { pkgSlice[0] = 9 }`), "pkg-mutation"},
		{"struct field assignment", plant("x.go", plantedDecls+`func f() { pkgStr.f = 3 }`), "pkg-mutation"},
		{"write through a pointer", plant("x.go", plantedDecls+`func f() { *pkgPtr = 3 }`), "pkg-mutation"},
		{"parenthesised write", plant("x.go", plantedDecls+`func f() { (pkgInt) = 3 }`), "pkg-mutation"},
		{"delete", plant("x.go", plantedDecls+`func f(k string) { delete(pkgMap, k) }`), "pkg-mutation"},
		{"clear", plant("x.go", plantedDecls+`func f() { clear(pkgMap) }`), "pkg-mutation"},
		{"copy into a package slice", plant("x.go", plantedDecls+`func f() { copy(pkgSlice, []int{2}) }`), "pkg-mutation"},
		{"write inside a function literal", plant("x.go", plantedDecls+`func f() { g := func() { pkgInt = 1 }; g() }`), "pkg-mutation"},
		{"write in a method named init", plant("x.go", plantedDecls+`type T struct{}
func (T) init() { pkgInt = 1 }`), "pkg-mutation"},
		{"another function writing the registry", plant("x.go", `var registry = map[string]int{}
func other() { registry["a"] = 1 }`), "pkg-mutation"},
		{"init-only mutator called outside init", plant("x.go", `var registry = map[string]int{}
func register() { registry["a"] = 1 }
func other() { register() }`), "pkg-mutation"},
		{"unparseable source", plant("x.go", `func ( {`), "parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := purityFindings(tc.srcs, plantedPolicy())
			require.NotEmpty(t, f)
			for file := range tc.srcs {
				requireRule(t, f, file, tc.rule)
			}
			for _, line := range f {
				require.Regexp(t, `^[a-z]+\.go:\d+: \[[a-z-]+\] `, line, "a finding names file, line and rule")
			}
		})
	}
}

func TestPurityCheckerAcceptsCleanSources(t *testing.T) {
	cases := map[string]map[string]string{
		"clean source": plant("x.go", `import "time"
var table = map[string]int{"a": 1}
var registry = map[string]func(){}

func register(f func()) { registry["a"] = f }

func init() {
	register(func() {})
	table["b"] = 2
	func() { table["c"] = 3 }()
}

// time is used for types only: no clock is read.
func f(d time.Duration, t time.Time) time.Duration { return d + t.Sub(t) }

func g(m map[string]int) int {
	local := 0
	local++
	m["k"] = local
	table := 7 // a := declares a new local; it is not a write to the package var
	return table + len(registry)
}`),
		"crypto/rand in wrap.go": plant("wrap.go", `import "crypto/rand"
func f(b []byte) { _, _ = rand.Read(b) }`),
		"aliased time used for types only": plant("x.go", `import tm "time"
func f(d tm.Duration) tm.Duration { return d }`),
		"a field named like a clock function": plant("x.go", `import "time"
type S struct{ Now int }
func f(s S, d time.Duration) (int, time.Duration) { return s.Now, d }`),
	}
	for name, srcs := range cases {
		t.Run(name, func(t *testing.T) {
			require.Empty(t, purityFindings(srcs, plantedPolicy()))
		})
	}
}

func TestPurityCheckerReportsStaleAllowances(t *testing.T) {
	srcs := plant("x.go", `var table = 1
var registry = 2
func register() {}`)
	require.Empty(t, staleAllowances(srcs, purityPolicy{
		vars:             map[string]string{"table": "r"},
		initOnlyMutators: map[string]string{"register>registry": "r"},
	}))

	stale := staleAllowances(srcs, purityPolicy{
		vars:             map[string]string{"table": "r", "gone": "r"},
		initOnlyMutators: map[string]string{"register>registry": "r", "vanished>registry": "r", "register>absent": "r"},
	})
	require.Len(t, stale, 3)
	require.Contains(t, strings.Join(stale, "\n"), "allowlisted var gone")
	require.Contains(t, strings.Join(stale, "\n"), "vanished>registry")
	require.Contains(t, strings.Join(stale, "\n"), "register>absent")
}

// The real policy must stay minimal: the init-only mutator list names exactly the
// helper the layout files register through, and every var carries a reason.
func TestPackagePurityPolicyIsExplicit(t *testing.T) {
	p := packagePurityPolicy()
	require.Equal(t, "wrap.go", p.randomnessFile)
	require.Contains(t, p.initOnlyMutators, "register>registry")
	require.NotEmpty(t, p.vars)
}
