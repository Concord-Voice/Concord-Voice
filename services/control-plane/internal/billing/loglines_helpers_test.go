package billing

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// logLine returns the first captured log line at level whose message is msg
// and that carries every attr (each a `key=value` as the text handler writes
// it), or "" if none does. Matching one whole line keeps a test from passing
// on a message from one line and a class from another.
func logLine(logs, level, msg string, attrs ...string) string {
	wantLevel := "level=" + level + " "
	wantMsg := `msg="` + msg + `"`
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, wantLevel) || !strings.Contains(line, wantMsg) {
			continue
		}
		matched := true
		for _, attr := range attrs {
			if !strings.Contains(line, " "+attr) {
				matched = false
				break
			}
		}
		if matched {
			return line
		}
	}
	return ""
}

// requireLogLine fails the test unless logLine finds a match.
func requireLogLine(t *testing.T, env *testEnv, level, msg string, attrs ...string) string {
	t.Helper()
	line := logLine(env.Logs.String(), level, msg, attrs...)
	if line == "" {
		t.Fatalf("no %s line %q with %v in the log:\n%s", level, msg, attrs, env.Logs.String())
	}
	return line
}

// uniqueSQLName returns a SQL identifier with prefix and a random suffix, so a
// trigger or function a test creates cannot collide with another run's on a
// shared test database.
func uniqueSQLName(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}
