package billing

import (
	"fmt"
	"os"
	"testing"
)

// packageConfigHome is the XDG_CONFIG_HOME for the whole test run, set by
// TestMain. It is empty only if TestMain has not run.
var packageConfigHome string

// TestMain points XDG_CONFIG_HOME at a private directory for the whole package
// run. stripe-go computes its user-agent and telemetry id once per process, and
// reads the id file from $XDG_CONFIG_HOME/stripe on every non-Windows OS. Any
// test in this package that sends a request while telemetry is on would
// therefore write ~/.config/stripe/telemetry_id on the developer's machine, and
// a later per-test redirect could not undo the once-per-process write.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "billing-xdg-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "billing tests: create config home: %v\n", err)
		return 1
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			fmt.Fprintf(os.Stderr, "billing tests: remove config home: %v\n", rmErr)
		}
	}()
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		fmt.Fprintf(os.Stderr, "billing tests: set XDG_CONFIG_HOME: %v\n", err)
		return 1
	}
	packageConfigHome = dir
	return m.Run()
}
