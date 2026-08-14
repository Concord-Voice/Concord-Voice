package testhelpers

import (
	"errors"
	"strings"
	"testing"
)

// Both shapes reach the same branch: NOAUTH is an uncredentialed URL, WRONGPASS
// is a stale or shell-overridden REDIS_PASSWORD. The second is not theoretical —
// a URL whose credential is sent as a 2-arg AUTH produces exactly this.
func TestPingFailureMessageExplainsAuthFailures(t *testing.T) {
	for _, driverErr := range []string{
		"NOAUTH Authentication required",
		"WRONGPASS invalid username-password pair or user is disabled",
	} {
		msg := pingFailureMessage(errors.New(driverErr))

		if !strings.Contains(msg, "REDIS_PASSWORD") {
			t.Errorf("%q: message should name REDIS_PASSWORD, got: %s", driverErr, msg)
		}
		if !strings.Contains(msg, "docs/development.md") {
			t.Errorf("%q: message should point at the docs, got: %s", driverErr, msg)
		}
		if !strings.Contains(msg, "/1") {
			t.Errorf("%q: message should show the DB-1 suffix, got: %s", driverErr, msg)
		}
	}
}

// CWE-209: the diagnostic is printed on every failing Redis-backed test, so it
// must describe the credential without ever reproducing it.
func TestPingFailureMessageNeverLeaksThePassword(t *testing.T) {
	for _, driverErr := range []string{
		"NOAUTH Authentication required",
		"WRONGPASS invalid username-password pair or user is disabled",
		"dial tcp 127.0.0.1:6379: connect: connection refused",
	} {
		msg := pingFailureMessage(errors.New(driverErr))
		if strings.Contains(msg, testRedisVal) {
			t.Errorf("%q: message leaked the password, got: %s", driverErr, msg)
		}
	}
}

func TestPingFailureMessagePassesThroughOtherErrors(t *testing.T) {
	msg := pingFailureMessage(errors.New("dial tcp 127.0.0.1:6379: connect: connection refused"))

	if !strings.Contains(msg, "connection refused") {
		t.Errorf("non-auth message should carry the driver error, got: %s", msg)
	}
	if strings.Contains(msg, "REDIS_PASSWORD") {
		t.Errorf("non-auth message should not give the auth hint, got: %s", msg)
	}
}
