package nats

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Issue #2854 finding A.
//
// A bus that was down AT BOOT used to return an error from Connect, leaving
// natsClient nil. Nothing retried -- MaxReconnects and ReconnectWait govern
// reconnection only AFTER an initial successful dial, and bindRouter runs once
// -- so the nil was PERMANENT for the process lifetime. The boot guard turned
// that into log.Fatal, taking auth and health down and crash-looping
// self-hosted and dev deployments.
//
// THE FIX IS AT THIS LAYER, NOT AT THE GUARD. An earlier attempt weakened the
// boot guard to a "was SetNATS called" flag; three reviewers found three
// separate defects in it, so the guard stayed fail-closed and the connection
// learned to heal itself instead.

// An unreachable bus must NOT be an error. The connection enters the
// reconnecting state and comes back on its own when the bus returns.
//
// Falsified by removing nats.RetryOnFailedConnect(true) from Connect: this then
// fails with "nats: no servers available for connection", which is exactly the
// nil that reached the boot guard.
func TestAnUnreachableBusYieldsAReconnectingClientNotAnError(t *testing.T) {
	// Port 1 is privileged and never bound by this suite, so the dial fails
	// immediately rather than hanging.
	c, err := Connect("nats://127.0.0.1:1")

	require.NoError(t, err,
		"a bus that is down at boot must not fail Connect -- that nil is what "+
			"reached the boot guard and killed auth and health with it")
	require.NotNil(t, c, "a reconnecting client is still a client")
	require.NotNil(t, c.conn, "the wrapper must hold a real conn, not an empty shell")
	// Added on CodeRabbit's suggestion (PR #2877) to assert STATE rather than
	// mere allocation. Recorded honestly: it is a SHAPE CHECK, not an
	// independently falsifiable lock. At this instant nothing separates it from
	// the two assertions above -- RetryOnFailedConnect puts the conn in
	// RECONNECTING before Connect returns, so NoError, NotNil and this all hold
	// on exactly the same condition. Mutating MaxReconnects(-1) to (0) does NOT
	// redden it: the give-up happens after a retry attempt, later than this
	// read. Reaching a CONNECTED conn would need a live server.
	//
	// Kept because it names the property the fix depends on and would catch a
	// future Connect that returned a non-reconnecting client. Do not cite it as
	// evidence the reconnect behaviour is pinned -- the falsified lock for that
	// is the NoError above, which reddens when RetryOnFailedConnect is removed.
	require.True(t, c.conn.IsReconnecting(),
		"a failed initial dial must leave the connection RECONNECTING; a client "+
			"that is merely non-nil never recovers")

	// Close the reconnect goroutine rather than leaking it into later tests.
	c.Close()
}

// The other half, and the one a careless fix drops: a CONFIGURATION fault must
// still be an error, so the boot guard still fatals on it.
//
// This is what makes the guard's fail-closed arm meaningful after the change.
// If every Connect succeeded, a typo'd NATS_URL would boot silently and the
// erase-account endpoint would keep returning success while no clear was ever
// published -- the exact fail-insecure-on-misconfiguration the guard prevents.
func TestAnUnparseableURLIsStillAnError(t *testing.T) {
	for name, url := range map[string]string{
		"unclosed IPv6 bracket": "nats://[::1",
		"control character":     "nats://host\x7f:4222",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Connect(url)

			require.Error(t, err,
				"a malformed URL is a deterministic deploy defect, not a "+
					"transient outage; it must reach the boot guard as nil")
			require.Nil(t, c)
		})
	}
}

// CALLERS MUST NOT LOG THIS ERROR. It carries the raw URL, credentials included.
//
// natsclient.Connect wraps nats.Connect, whose *url.Error formats the RAW URL,
// so a nats://<user>:<pass>@host misconfiguration writes the credential into any
// caller that logs err (CWE-532). router.go logs a fixed failure_class instead.
//
// The rule is NOT conditional on this error shape. An earlier revision of the
// assertion below said the redaction requirement "can be revisited" if the error
// ever stopped carrying the secret -- offering to weaken a security control on
// the evidence of one observed error type (CodeRabbit, PR #2877). This test
// demonstrates the exposure; it does not license removing the control.
func TestConnectErrorEmbedsTheURLSoCallersMustNotLogIt(t *testing.T) {
	_, err := Connect("nats://sentineluser:sentinelsecret@[::1")

	require.Error(t, err)
	require.Contains(t, err.Error(), "sentinelsecret",
		"the connect error embeds the raw URL, so callers MUST NOT log it")
}
