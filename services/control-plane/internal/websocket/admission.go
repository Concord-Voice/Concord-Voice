package websocket

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxConnectionsPerUser      = 8
	maxConnections             = 10000
	pendingRegistrationTimeout = 10 * time.Second
)

// reserveConnection counts pending upgrades as well as registered clients.
func (h *Hub) reserveConnection(userID uuid.UUID) (func(), bool) {
	h.admissionMu.Lock()
	defer h.admissionMu.Unlock()
	select {
	case <-h.done:
		return nil, false
	default:
	}
	if h.admittedConnections >= maxConnections || h.admittedConnectionsByUser[userID] >= maxConnectionsPerUser {
		return nil, false
	}
	h.admittedConnections++
	h.admittedConnectionsByUser[userID]++
	return sync.OnceFunc(func() {
		h.admissionMu.Lock()
		defer h.admissionMu.Unlock()
		h.admittedConnections--
		h.admittedConnectionsByUser[userID]--
		if h.admittedConnectionsByUser[userID] == 0 {
			delete(h.admittedConnectionsByUser, userID)
		}
	}), true
}

// Keep the slot until the handler, hub cleanup, and both pumps have finished.
func (c *Client) releaseConnection() {
	if c.releaseAdmission != nil && c.admissionOwners.Add(-1) == 0 {
		c.releaseAdmission()
	}
}
