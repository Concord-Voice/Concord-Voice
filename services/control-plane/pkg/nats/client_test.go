package nats

import "testing"

// TestIsConnectedIsNilSafeAtBothLevels asserts BEHAVIOUR, never assert.Nil --
// a typed nil satisfies a nil assertion and would leave the guard untested.
func TestIsConnectedIsNilSafeAtBothLevels(t *testing.T) {
	var nilClient *Client
	if nilClient.IsConnected() {
		t.Fatal("nil *Client must report not-connected, not panic")
	}
	if (&Client{}).IsConnected() {
		t.Fatal("a Client with no conn must report not-connected, not panic")
	}
}
