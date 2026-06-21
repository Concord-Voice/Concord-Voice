# WebSocket Server Infrastructure

This package implements real-time WebSocket communication for Concord's control plane service.

## Architecture

### Components

1. **Hub** - Central message router and connection manager
   - Maintains all active WebSocket connections
   - Routes messages between clients
   - Manages channel subscriptions
   - Supports multi-device connections per user

2. **Client** - Represents a single WebSocket connection
   - Unique client ID for each connection
   - Associated with a user ID (from JWT)
   - Bidirectional message pumps (read/write)
   - Ping/pong heartbeat mechanism
   - Channel subscriptions

3. **Handler** - HTTP upgrade and authentication
   - Upgrades HTTP connections to WebSocket
   - Validates ticket-based auth (POST /auth/ws-ticket → single-use 30s ticket)
   - Creates and registers new clients

### Message Flow

```
Client -> WebSocket -> readPump() -> Hub.incoming -> Hub.handleIncoming() -> Route/Process
Hub.broadcast -> handleBroadcast() -> writePump() -> WebSocket -> Clients
```

## Connection

### Endpoint

```
GET wss://host/api/v1/ws?ticket=<SINGLE_USE_TICKET>
```

### Authentication

WebSocket connections use **ticket-based authentication**:

1. Client calls `POST /api/v1/auth/ws-ticket` with a valid access token to obtain a single-use ticket (30s TTL)
2. Client connects to `wss://host/api/v1/ws?ticket=<ticket>`
3. Server validates and consumes the ticket (single-use, prevents replay attacks)

> **Security note:** Raw JWT tokens should NOT be passed in the WebSocket URL. Tickets are single-use and expire in 30 seconds, minimizing the window for interception.

### Connection Lifecycle

1. **Connect** - Client opens WebSocket with single-use ticket
2. **Upgrade** - Server validates ticket and upgrades connection
3. **Register** - Client is registered with Hub
4. **Confirmation** - Server sends `connected` message with client/user IDs
5. **Subscribe** - Client subscribes to channels
6. **Messaging** - Bidirectional message exchange
7. **Heartbeat** - Automatic ping/pong every 54 seconds
8. **Disconnect** - Client closes connection or timeout
9. **Unregister** - Client removed from Hub and all subscriptions

## Message Types

### Client -> Server (Incoming)

#### Subscribe to Channel
```json
{
  "type": "subscribe",
  "data": {
    "channel_id": "uuid-string"
  }
}
```

#### Unsubscribe from Channel
```json
{
  "type": "unsubscribe",
  "data": {
    "channel_id": "uuid-string"
  }
}
```

#### Send Message
```json
{
  "type": "message",
  "data": {
    "channel_id": "uuid-string",
    "content": "message content (can be encrypted)",
    "timestamp": 1234567890
  }
}
```

#### Typing Indicator
```json
{
  "type": "typing",
  "data": {
    "channel_id": "uuid-string",
    "is_typing": true
  }
}
```

### Server -> Client (Outgoing)

#### Connection Confirmation
```json
{
  "type": "connected",
  "data": {
    "client_id": "uuid-string",
    "user_id": "uuid-string"
  }
}
```

#### Subscription Confirmation
```json
{
  "type": "subscribed",
  "data": {
    "channel_id": "uuid-string"
  }
}
```

#### Broadcast Message
```json
{
  "type": "message",
  "data": {
    "channel_id": "uuid-string",
    "user_id": "uuid-string",
    "content": "message content",
    "timestamp": 1234567890
  }
}
```

#### Typing Indicator (Broadcast)
```json
{
  "type": "typing",
  "data": {
    "channel_id": "uuid-string",
    "user_id": "uuid-string",
    "is_typing": true
  }
}
```

## Protocol Version 2 — Connection-Ready Barrier

**Shipped:** 2026-04-24 (issue #752)

Version 2 adds a subscribe-barrier handshake to eliminate a race where a
client's message queue could begin processing before the hub had committed
its resubscribe frames.

### Client-emitted frame

```json
{
  "type": "connection_ready_probe",
  "data": { "protocol_version": 2 }
}
```

Sent by v2 clients after the last `subscribe` / `subscribe_server` /
`subscribe_dm` frame during a reconnect handshake (or on cold connect).

### Hub-emitted frame

```json
{
  "type": "connection_ready",
  "data": {
    "subscribed_channels": 12,
    "protocol_version": 2
  }
}
```

Emitted in response to `connection_ready_probe`. Because the hub's
`Run()` goroutine processes all incoming frames serially, every
resubscribe queued before the probe is guaranteed to be committed before
this response fires — no explicit lock or barrier is needed beyond the
natural ordering of a single-goroutine consumer.

### Backwards compatibility

- V1 clients never emit the probe — hub never emits the response — identical to pre-v2 behaviour.
- V2 client against a V1 hub — client times out after 5s, proceeds best-effort. No regression vs pre-v2.
- V1 client against a V2 hub — hub never emits `connection_ready` (no probe received) — identical to pre-v2 behaviour.

No server-side version negotiation required.

## Heartbeat Mechanism

- **Ping Interval**: 54 seconds (9/10 of pong timeout)
- **Pong Timeout**: 60 seconds
- **Read Timeout**: Updated on each pong received
- **Write Timeout**: 10 seconds per message

If a client fails to respond to a ping within 60 seconds, the connection is closed.

## Configuration

### Connection Limits

- **Read Buffer**: 1024 bytes
- **Write Buffer**: 1024 bytes
- **Max Message Size**: 512 KB
- **Send Channel Buffer**: 256 messages

### Timeouts

- **Write Wait**: 10 seconds
- **Pong Wait**: 60 seconds
- **Ping Period**: 54 seconds

## Features

### Multi-Device Support

- Each user can have multiple connected devices
- Each connection gets a unique client ID
- Messages can be broadcast to all user's devices
- Presence tracking per connection

### Channel Subscriptions

- Clients subscribe to specific channels
- Messages only sent to subscribed clients
- Automatic cleanup on disconnect
- Channel-level broadcasting

### Graceful Shutdown

- Connections closed cleanly on server shutdown
- Send channel properly closed
- All goroutines terminated

## Security

### Authentication

- JWT validation before upgrade
- User ID extracted from valid token
- Invalid tokens rejected with 401

### Origin Checking

- Currently allows all origins (development)
- **TODO**: Restrict to allowed origins in production

### Message Validation

- JSON parsing with error handling
- Type checking on message fields
- UUID validation for IDs

## Performance Considerations

### Goroutines

- 2 goroutines per connection (read pump + write pump)
- Hub runs in single goroutine for thread safety
- Blocking operations avoid locks

### Memory

- Buffered channels prevent blocking
- Automatic cleanup on disconnect
- Map-based lookups for O(1) routing

### Scalability

Current implementation is single-instance. For horizontal scaling:
- Use Redis Pub/Sub for inter-instance communication
- Shared session store for presence
- Sticky sessions or connection affinity

## Testing

See [TEST_WEBSOCKET.html](TEST_WEBSOCKET.html) for a manual test client.

### Manual Testing

1. Start control-plane server: `make run`
2. Register/login to get JWT token
3. Open `TEST_WEBSOCKET.html` in browser
4. Paste JWT token and connect
5. Test subscribe, message, typing events

### Integration Testing

```bash
# TODO: Add integration tests
go test -v ./internal/websocket/...
```

## Implementation Status

- [x] Presence system (online/offline status) — ✅ Implemented (Phase 1B)
- [x] Message persistence (save to database) — ✅ Implemented (Phase 1B)
- [x] Message history on subscribe — ✅ Implemented via REST pagination
- [x] Read receipts (channel read states) — ✅ Implemented (Phase 1B)
- [x] Voice/video signaling — ✅ Implemented via NATS (Phase 1C)
- [x] Ticket-based authentication — ✅ Implemented (replaces JWT-in-URL)
- [ ] File upload support — Planned (Phase 2)
- [ ] Redis Pub/Sub for multi-instance — Planned (Phase 2+)
- [ ] Reconnection with message recovery — Planned
- [ ] Compression support — Planned
- [ ] Binary message support — Planned
