// Package nats provides a NATS client wrapper for inter-service messaging.
package nats

import (
	"encoding/json"
	"fmt"
	"log"

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
		nats.MaxReconnects(-1),
		nats.ReconnectWait(nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Printf("NATS disconnected: %v", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Printf("NATS reconnected to %s", nc.ConnectedUrl())
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

// Close drains and closes the NATS connection.
func (c *Client) Close() {
	if c.conn != nil {
		_ = c.conn.Drain()
	}
}
