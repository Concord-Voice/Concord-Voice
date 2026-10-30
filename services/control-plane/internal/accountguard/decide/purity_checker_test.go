package decide

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// plant wraps a body (imports and declarations) in a package clause.
func plant(body string) map[string]string {
	return map[string]string{"x.go": "package decide\n\n" + body + "\n"}
}

// TestPurityCheckerFindsPlantedViolations proves each rule bites (plan Task 1 Step 1 table):
// every planted source yields exactly one finding, and it names the expected rule.
func TestPurityCheckerFindsPlantedViolations(t *testing.T) {
	cases := []struct {
		name string
		src  string
		rule string
	}{
		{"import outside allowlist", `import "fmt"`, "import"},
		{"database import", `import "database/sql"`, "import"},
		{"context import", `import "context"`, "import"},
		{"net import", `import "net/http"`, "import"},
		{"strings import", `import "strings"`, "import"},
		{"encoding import", `import "encoding/json"`, "import"},
		{"clock call", "import \"time\"\nfunc f() { _ = time.Now() }", "clock"},
		{"clock method value", "import \"time\"\nfunc f() { clk := time.Now; _ = clk }", "clock"},
		{"aliased clock", "import tm \"time\"\nfunc f(x tm.Time) { _ = tm.Since(x) }", "clock"},
		{"dot import of time", "import . \"time\"\nfunc f() { _ = Duration(1) }", "clock"},
		{"blank import of time", `import _ "time"`, "clock"},
		{"AfterFunc", "import \"time\"\nfunc f(d time.Duration, g func()) { time.AfterFunc(d, g) }", "clock"},
		{"package var", `var x = 1`, "var"},
		{"blank package var", `var _ = 1`, "var"},
		{"init", `func init() {}`, "init"},
		{"unparseable source", `func ( {`, "parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := purityFindings(plant(tc.src))
			require.Len(t, findings, 1, "findings: %v", findings)
			require.True(t, strings.HasPrefix(findings[0], "x.go:"), findings[0])
			require.Contains(t, findings[0], "["+tc.rule+"]")
		})
	}
}

// TestPurityCheckerBansAllNineClockNames pins the full authoritycontract list: Now,
// Since, Until, Tick, After, AfterFunc, NewTimer, NewTicker and Sleep.
func TestPurityCheckerBansAllNineClockNames(t *testing.T) {
	names := []string{"Now", "Since", "Until", "Tick", "After", "AfterFunc", "NewTimer", "NewTicker", "Sleep"}
	require.Len(t, names, 9)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			findings := purityFindings(plant("import \"time\"\nfunc f() { _ = time." + name + " }"))
			require.Len(t, findings, 1, "findings: %v", findings)
			require.Contains(t, findings[0], "[clock]")
			require.Contains(t, findings[0], "time."+name)
		})
	}
}

// TestPurityCheckerAllowsEachAllowlistedImport confirms the four allowed imports are not flagged.
func TestPurityCheckerAllowsEachAllowlistedImport(t *testing.T) {
	for _, imp := range []string{"time", "sort", "errors", "bytes"} {
		t.Run(imp, func(t *testing.T) {
			require.Empty(t, purityFindings(plant("import \""+imp+"\"")))
		})
	}
}

// TestPurityCheckerAcceptsCleanSources feeds the constructs the real package uses
// (plan Task 1 Step 1): time.Date, Duration arithmetic, sort.Slice and bytes.Equal.
func TestPurityCheckerAcceptsCleanSources(t *testing.T) {
	src := `import (
	"bytes"
	"errors"
	"sort"
	"time"
)

const holdCap = 72 * time.Hour

type k uint8

func legacy() time.Time { return time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC) }

func f(a, b []byte, xs []int, now, then time.Time) (bool, error) {
	var local int
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	local++
	d := now.Sub(then) + holdCap
	_ = d
	if !bytes.Equal(a, b) {
		return false, errors.New("x")
	}
	return then.Before(now), nil
}

func (k) init() {}`
	require.Empty(t, purityFindings(plant(src)))
}

// TestPurityCheckerResolvesTheTimeAlias confirms the alias is resolved, so a local
// identifier that happens to be named time is not mistaken for the package.
func TestPurityCheckerResolvesTheTimeAlias(t *testing.T) {
	findings := purityFindings(plant("import tm \"time\"\nfunc f(time interface{ Now() int }) { _ = time.Now() }"))
	require.Empty(t, findings, "a local named time is not the time package when the package is aliased")
}

// TestPurityFindingsAreSortedAndReportEveryFile checks several files are all scanned and
// the output order is stable.
func TestPurityFindingsAreSortedAndReportEveryFile(t *testing.T) {
	srcs := map[string]string{
		"b.go": "package decide\n\nvar b = 1\n",
		"a.go": "package decide\n\nvar a = 1\n",
	}
	findings := purityFindings(srcs)
	require.Len(t, findings, 2)
	require.True(t, strings.HasPrefix(findings[0], "a.go:"))
	require.True(t, strings.HasPrefix(findings[1], "b.go:"))
}
