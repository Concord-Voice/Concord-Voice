package websocket

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// A close frame's reason text is chosen by the PEER. gorilla renders it inside
// CloseError.Error(), so logging the raw error with %v hands any client a write
// primitive into the operator's log sink -- and sanitizeLogValue is not applied
// on the readPump failure path, so that includes CRLF log forging (CWE-117).
// describeSocketFailure must therefore never carry the text through.
func TestDescribeSocketFailureNeverCarriesPeerText(t *testing.T) {
	const forged = "\r\n2026-01-01 FABRICATED admin login from 10.0.0.1"
	err := &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: forged}

	// Control: prove the raw error DOES leak it, so a future refactor back to
	// %v cannot pass this file silently.
	if !strings.Contains(err.Error(), "FABRICATED") {
		t.Fatal("control failed: gorilla no longer renders CloseError.Text, " +
			"so this test can no longer prove anything -- re-derive it")
	}

	got := describeSocketFailure(err)
	if strings.Contains(got, "FABRICATED") {
		t.Errorf("peer-controlled close text reached the log line: %q", got)
	}
	for _, ctrl := range []string{"\r", "\n"} {
		if strings.Contains(got, ctrl) {
			t.Errorf("control character survived into the log line: %q", got)
		}
	}
	if !strings.Contains(got, "code=1006") {
		t.Errorf("the close code is the diagnostic value and must survive, got %q", got)
	}
}

// net.OpError renders the remote address, i.e. the client's IP. On a disconnect
// path that fires for every dropped socket that is a standing PII feed.
func TestDescribeSocketFailureNeverCarriesPeerAddress(t *testing.T) {
	err := &net.OpError{
		Op:     "read",
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.ParseIP("203.0.113.77"), Port: 51234},
		Addr:   &net.TCPAddr{IP: net.ParseIP("198.51.100.9"), Port: 443},
		Err:    errors.New("connection reset by peer"),
	}
	if !strings.Contains(err.Error(), "203.0.113.77") {
		t.Fatal("control failed: net.OpError no longer renders the address")
	}
	got := describeSocketFailure(err)
	for _, ip := range []string{"203.0.113.77", "198.51.100.9"} {
		if strings.Contains(got, ip) {
			t.Errorf("peer address %s reached the log line: %q", ip, got)
		}
	}
}

// Timeout is the one distinction the bare type name loses that an operator
// needs: an idle-client read deadline and a peer reset are both *net.OpError.
func TestDescribeSocketFailureSurfacesTimeout(t *testing.T) {
	timeout := &net.OpError{
		Op: "read", Net: "tcp",
		Err: &timeoutError{},
	}
	if got := describeSocketFailure(timeout); !strings.Contains(got, "timeout=true") {
		t.Errorf("a read deadline must be distinguishable, got %q", got)
	}
	if got := describeSocketFailure(io.ErrUnexpectedEOF); strings.Contains(got, "timeout") {
		t.Errorf("a non-timeout error must not claim timeout, got %q", got)
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

var _ net.Error = (*timeoutError)(nil)
