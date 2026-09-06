// Package nats provides a NATS client wrapper for inter-service messaging.
package nats

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
)

// Client wraps a NATS connection with convenience methods.
type Client struct {
	conn *nats.Conn
}

// Connect establishes a connection to the NATS server.
func Connect(url string) (*Client, error) {
	nc, err := nats.Connect(url,
		nats.Name("concordvoice-control-plane"),
		// #2854 finding A. MaxReconnects and ReconnectWait govern reconnection
		// only AFTER an initial successful dial, so without this a bus that is
		// down at boot returned an error, left natsClient nil, and NOTHING ever
		// retried -- bindRouter runs once. The nil was PERMANENT for the process
		// lifetime, and the old boot guard turned that into log.Fatal, taking
		// auth and health down and crash-looping self-hosted and dev deploys.
		//
		// With this the connection enters the reconnecting state instead and
		// heals itself when the bus returns; Connect then errors only on a
		// genuine CONFIGURATION fault (unparseable URL, bad TLS or credentials),
		// which is a deterministic deploy defect that SHOULD stay fatal.
		//
		// That distinction is the whole fix: an outage stops producing nil, so
		// the boot guard never sees one, and no guard predicate had to be
		// weakened to permit it.
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Printf("NATS disconnected: %v", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Printf("NATS reconnected to %s", nc.ConnectedUrl())
		}),
		// Without this the client library sheds SILENTLY (#2854 B1). Each async
		// subscription defaults to DefaultSubPendingMsgsLimit (500k) and
		// DefaultSubPendingBytesLimit (64MB); on overflow it sets
		// ErrSlowConsumer, increments a drop counter and discards, with no log
		// line and no failure_class. That is a second shedder alongside the
		// ingress gates in internal/voice, and an invisible one -- it would
		// falsify the claim that every shed message is attributable.
		//
		// This LOGS only. It deliberately does NOT call SetPendingLimits:
		// altering the drop thresholds is a separate decision with its own
		// blast radius and does not belong in this change.
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			log.Print(asyncErrorLogLine(sub, err))
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}

	return &Client{conn: nc}, nil
}

// Subscribe registers a handler for messages on the given subject.
// The handler receives the raw JSON payload.
func (c *Client) Subscribe(subject string, handler func(data []byte)) (*nats.Subscription, error) {
	return c.conn.Subscribe(subject, func(msg *nats.Msg) {
		handler(msg.Data)
	})
}

// SubscribeWithSubject registers a handler that also receives the concrete
// subject. Wildcard consumers can use one serialized subscription callback
// when ordering across related subjects matters.
func (c *Client) SubscribeWithSubject(subject string, handler func(subject string, data []byte)) (*nats.Subscription, error) {
	return c.conn.Subscribe(subject, func(msg *nats.Msg) {
		handler(msg.Subject, msg.Data)
	})
}

// Publish sends a JSON-encoded message to the given subject.
func (c *Client) Publish(subject string, data interface{}) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("nats marshal: %w", err)
	}
	return c.conn.Publish(subject, payload)
}

// Flush blocks until the server has processed all buffered messages and
// subscription interest from this connection. It closes the well-known NATS race
// where a Subscribe followed by an immediate Publish on a different connection can
// miss the message because the subscription interest has not yet propagated to the
// server. Callers (notably integration tests that subscribe-then-publish) should
// Flush after Subscribe to make the subscription deterministically active.
func (c *Client) Flush() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Flush()
}

// Drain outcomes. These are OUR sentinels, not nats.go's, so callers classify
// a shutdown without importing the driver -- and so the classification survives
// a driver upgrade that renames its own errors.
var (
	// ErrDrainNothingToDo: the connection was already closed. Benign.
	ErrDrainNothingToDo = errors.New("nats: connection already closed; nothing to drain")
	// ErrDrainSkippedReconnecting: the connection was mid-reconnect, so nats.go
	// closed it WITHOUT draining. Publishes buffered at that moment are gone.
	ErrDrainSkippedReconnecting = errors.New("nats: connection was reconnecting; closed without draining")
	// ErrDrainTimedOut: the drain started but had not finished when the budget
	// expired. The process is about to exit and will kill it.
	ErrDrainTimedOut = errors.New("nats: drain did not finish within its budget")
)

// drainWaitBudget bounds how long Close waits for an asynchronous drain.
const drainWaitBudget = 2 * time.Second

// Close drains the NATS connection and WAITS for the drain to finish, bounded.
//
// The wait is the whole point. nats.go's Drain() is ASYNCHRONOUS: it flips the
// status to DRAINING_SUBS, spawns `go nc.drainConnection()`, and returns nil as
// soon as that goroutine STARTS (nats.go v1.53.1). So a caller that returns
// immediately gets nil, the process exits, and the in-flight drain dies with it
// -- which is precisely the "buffered publishes were dropped" case an earlier
// version of this function reported by returning that nil.
//
// It also used to discard the error entirely with a blank assignment, the shape
// errcheck honours and [internal]rules/backend.md forbids. Callers on a
// best-effort path (defer, t.Cleanup) may still discard it explicitly; the
// production shutdown stage classifies it.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	switch err := c.conn.Drain(); {
	case err == nil:
	case errors.Is(err, nats.ErrConnectionClosed):
		return ErrDrainNothingToDo
	case errors.Is(err, nats.ErrConnectionReconnecting):
		return ErrDrainSkippedReconnecting
	default:
		return err
	}

	deadline := time.Now().Add(drainWaitBudget)
	for time.Now().Before(deadline) {
		if c.conn.IsClosed() || !c.conn.IsDraining() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.conn.IsClosed() || !c.conn.IsDraining() {
		return nil
	}
	return ErrDrainTimedOut
}

// IsConnected reports whether the wrapped connection is currently CONNECTED.
//
// Nil-safe at BOTH levels: Connect returns a nil *Client on a config error,
// and a non-nil Client can hold a nil conn. It reports false during a
// reconnect window, which is the correct signal and is harmless because NATS
// is a non-gating readiness check (#3106) -- RetryOnFailedConnect(true) plus
// MaxReconnects(-1) mean a NATS outage self-heals, so gating on it would 503
// the only control-plane node during every cold start where NATS lags
// Postgres.
func (c *Client) IsConnected() bool {
	return c != nil && c.conn != nil && c.conn.IsConnected()
}
