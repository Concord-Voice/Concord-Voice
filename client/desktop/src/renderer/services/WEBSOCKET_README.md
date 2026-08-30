# WebSocket Client Implementation

**Status:** ✅ Complete.
**Issue:** #20 - Build WebSocket Client Connection Manager (Phase 1B).
**Related Backend:** Issue #10 - Implement WebSocket Server Infrastructure (Phase 1B).

---

## Overview

This directory contains the complete WebSocket client implementation for Concord's real-time communication features. The client connects to the backend WebSocket server (`ws://localhost:8080/api/v1/ws`) and handles:

- Real-time message delivery
- Channel subscriptions
- Typing indicators
- Presence updates
- Auto-reconnection with exponential backoff
- Ticket-based authentication (POST /auth/ws-ticket → single-use 30s ticket)

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Frontend (React/TypeScript)              │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐ │
│  │  UI Components │──>│  React Hooks │──>│ WebSocket    │ │
│  │  (MainView,   │<──│  (useWebSocket,│<──│ Service      │ │
│  │   Chat, etc.) │    │   useChannel  │    │              │ │
│  └──────────────┘    │   Subscription)│    └──────┬───────┘ │
│                       └──────────────┘            │          │
│                                                    │          │
│  ┌──────────────┐                                 │          │
│  │  Zustand     │<─── State Management ───────────┘          │
│  │  Stores      │                                             │
│  │  - chatStore │                                             │
│  │  - authStore │                                             │
│  └──────────────┘                                             │
└───────────────────────────────┬─────────────────────────────┘
                                 │
                        WebSocket Connection
                        (ws://localhost:8080/api/v1/ws?ticket=<30s-single-use-ticket>)
                                 │
┌───────────────────────────────┴─────────────────────────────┐
│                     Backend (Go/gorilla/websocket)           │
├─────────────────────────────────────────────────────────────┤
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐ │
│  │  WebSocket   │──>│  Hub         │──>│  Clients      │ │
│  │  Handler     │    │  (Message    │    │  (Connection  │ │
│  │              │    │   Router)    │    │   Manager)    │ │
│  └──────────────┘    └──────────────┘    └──────────────┘ │
│                                                              │
│  JWT Auth ✓ | Heartbeat ✓ | Subscriptions ✓ | Broadcast ✓ │
└─────────────────────────────────────────────────────────────┘
```

---

## File Structure

```
src/renderer/
├── services/
│   ├── websocketService.ts    # WebSocket connection manager (singleton)
│   └── WEBSOCKET_README.md    # This file
├── stores/
│   ├── chat/
│   │   └── chatStore.ts       # Real-time message and typing state (Zustand)
│   └── auth/
│       └── authStore.ts       # JWT token storage (existing)
├── hooks/
│   ├── useWebSocket.ts         # React hook for WebSocket integration
│   └── useChannelSubscription.ts # Auto-subscribe/unsubscribe to channels
├── components/
│   └── ConnectionStatus/
│       ├── ConnectionStatus.tsx # Visual connection indicator
│       └── ConnectionStatus.css
└── types/
    └── chat.ts                 # Message, Channel types (existing)
```

---

## Core Components

### 1. WebSocketService (`services/websocketService.ts`)

Singleton service that manages the WebSocket connection lifecycle.

**Features:**

- Ticket-based authentication (POST /auth/ws-ticket → 30s single-use ticket)
- Auto-reconnect with exponential backoff (1s → 2s → 4s → ... up to 30s)
- Max 10 reconnection attempts
- Connection state tracking: `DISCONNECTED`, `CONNECTING`, `CONNECTED`, `RECONNECTING`, `ERROR`
- Type-safe message routing
- Channel subscription management
- Ping interval (30s) for connection health

**Usage:**

```typescript
import { getWebSocketService } from '../services/websocketService';

const wsService = getWebSocketService();

// Connect with JWT token
wsService.connect(token);

// Subscribe to a channel
wsService.subscribe(channelId);

// Send a message. `content` MUST already be ciphertext, and key_version MUST be
// the epoch it was encrypted under — the server rejects a frame without one
// (close 4400). Encrypt via e2eeService rather than calling this directly; see
// useMessaging.ts for the real send path.
wsService.sendMessage(channelId, ciphertext, { keyVersion, nonce });

// Listen for messages
const unsubscribe = wsService.on('message', (msg) => {
  console.log('Received message:', msg);
});

// Disconnect
wsService.disconnect();
```

**Connection Flow:**

1. User logs in → JWT access token stored in `authStore`
2. `useWebSocket` hook detects token → calls `POST /auth/ws-ticket` to get a 30-second single-use ticket
3. WebSocket connects to `ws://localhost:8080/api/v1/ws?ticket=<ticket>`
4. Backend validates ticket (single-use, expires in 30s) → sends `connected` message with `client_id` and `user_id`
5. Client stores connection info → notifies UI via `chatStore.setConnectionStatus(true, clientId)`
6. Auto-resubscribes to all previously subscribed channels

---

### 2. ChatStore (`stores/chat/chatStore.ts`)

Zustand store for real-time message state.

**State:**

```typescript
{
  messagesByChannel: Map<string, Message[]>,  // Messages grouped by channel ID
  typingByChannel: Map<string, Map<string, TypingUser>>,  // Typing indicators
  isConnected: boolean,                        // WebSocket connection status
  connectionClientId: string | null,           // Client ID from backend
}
```

**Actions:**

- `addMessage(channelId, message)` - Add/update message (deduplication by ID)
- `updateMessage(channelId, messageId, updates)` - Edit message
- `deleteMessage(channelId, messageId)` - Delete message
- `setMessages(channelId, messages)` - Replace all messages (REST API initial load)
- `prependMessages(channelId, messages)` - Add older messages (pagination)
- `setTyping(channelId, userId, isTyping, username)` - Update typing indicator
- `getTypingUsers(channelId)` - Get list of users typing in channel
- `clearOldTypingIndicators(channelId)` - Remove stale typing indicators (>5s)
- `setConnectionStatus(isConnected, clientId)` - Update connection status

**Usage:**

```typescript
import { useChatStore } from '../stores/chat/chatStore';

// In React component
const messages = useChatStore((state) => state.messagesByChannel.get(channelId) || []);

const isConnected = useChatStore((state) => state.isConnected);

const { addMessage } = useChatStore();
```

---

### 3. useWebSocket Hook (`hooks/useWebSocket.ts`)

React hook that bridges `websocketService` with `chatStore`.

**Responsibilities:**

- Auto-connect when JWT token is available
- Auto-disconnect on unmount or token change
- Set up message handlers for all message types:
  - `message` → `addMessage()`
  - `message_update` → `updateMessage()`
  - `message_delete` → `deleteMessage()`
  - `typing` → `setTyping()`
  - `subscribed` → Log confirmation
  - `error` → Log error
- Update connection status in chatStore

**Usage:**

```typescript
import { useWebSocket } from '../hooks/useWebSocket';

function MyComponent() {
  const { subscribe, unsubscribe, sendTyping } = useWebSocket();

  // Subscribe to a channel
  useEffect(() => {
    subscribe(channelId);
    return () => unsubscribe(channelId);
  }, [channelId]);

  return <div>...</div>;
}
```

This hook deliberately exposes **no** send-message function. It used to export a
`sendMessage(channelId, content)` that forwarded straight to the service with no
encryption and no `key_version` — residue from before #1024, when an unencrypted
channel could legitimately send plaintext. It was removed in #2843: the hook
holds no `e2eeService` key material, so there is no correct version of it, and
the name invited exactly the wrong call.

Send an encrypted message through `useMessaging` (channels and DMs). It encrypts
via `e2eeService` and stamps the `key_version` the server requires, and it is
fail-closed: if encryption fails, nothing is sent.

```typescript
import { useMessaging } from '../hooks/useMessaging';

function Composer({ channelId, username }: { channelId: string; username: string }) {
  const messaging = useMessaging();

  const handleSend = (content: string) => {
    // `content` is plaintext here — useMessaging encrypts it before it reaches
    // the socket. Never call wsService.sendMessage with plaintext directly.
    messaging.sendMessage(channelId, content, username);
  };

  return <div>...</div>;
}
```

`messaging.sendDMMessage(conversationId, content, username, opts)` is the DM
equivalent; see `useChatController.ts` for a live call site that picks between
them.

---

### 4. useChannelSubscription Hook (`hooks/useChannelSubscription.ts`)

Convenience hook for automatic channel subscription management.

**Usage:**

```typescript
import { useChannelSubscription } from '../hooks/useChannelSubscription';

function ChannelView({ channelId }: { channelId: string }) {
  const { isSubscribed } = useChannelSubscription(channelId);

  // Automatically subscribes when mounted and channelId changes
  // Automatically unsubscribes when unmounted or channelId changes

  return (
    <div>
      {isSubscribed ? 'Connected to channel' : 'Connecting...'}
    </div>
  );
}
```

---

### 5. ConnectionStatus Component (`components/ConnectionStatus/`)

Visual indicator of WebSocket connection state.

**Features:**

- Green "Connected" badge when `isConnected === true`
- Red "Offline" badge when `isConnected === false`
- Animated pulsing dot for visual feedback
- Shows client ID on hover (for debugging)

**Placement:** Currently in `MainView` channels sidebar header.

---

## Message Protocol

The frame catalogue below is illustrative. `types/ws-events.ts` holds the canonical set —
the zod discriminated union every inbound frame is validated against at the dispatch
boundary. Read that file before you add or change a frame type.

### Client → Server

**Subscribe to a channel:**

```json
{
  "type": "subscribe",
  "data": {
    "channel_id": "uuid"
  }
}
```

**Unsubscribe from a channel:**

```json
{
  "type": "unsubscribe",
  "data": {
    "channel_id": "uuid"
  }
}
```

**Send a message:**

```json
{
  "type": "message",
  "data": {
    "channel_id": "uuid",
    "content": "<base64 ciphertext>",
    "key_version": 3,
    "timestamp": 1234567890
  }
}
```

Encrypt through `useMessaging` before you send. `content` must already be ciphertext and
`key_version` must be the epoch it was encrypted under. The server closes the connection with
4400 on a frame that carries no `key_version`.

**Send typing indicator:**

```json
{
  "type": "typing",
  "data": {
    "channel_id": "uuid",
    "is_typing": true
  }
}
```

---

### Server → Client

**Connection confirmation:**

```json
{
  "type": "connected",
  "data": {
    "client_id": "uuid",
    "user_id": "uuid"
  }
}
```

**Subscription confirmation:**

```json
{
  "type": "subscribed",
  "data": {
    "channel_id": "uuid"
  }
}
```

**Incoming message:**

```json
{
  "type": "message",
  "data": {
    "id": "uuid",
    "channel_id": "uuid",
    "user_id": "uuid",
    "username": "string",
    "content": "Hello world!",
    "created_at": "2026-02-17T12:00:00Z",
    "updated_at": null
  }
}
```

**Message update:**

```json
{
  "type": "message_update",
  "data": {
    "id": "uuid",
    "channel_id": "uuid",
    "content": "Updated message",
    "updated_at": "2026-02-17T12:05:00Z"
  }
}
```

**Message deletion:**

```json
{
  "type": "message_delete",
  "data": {
    "id": "uuid",
    "channel_id": "uuid"
  }
}
```

**Typing indicator:**

```json
{
  "type": "typing",
  "data": {
    "user_id": "uuid",
    "username": "string",
    "channel_id": "uuid",
    "is_typing": true
  }
}
```

**Error:**

```json
{
  "type": "error",
  "data": {
    "message": "Error description"
  }
}
```

---

## Connection Lifecycle

### 1. Initial Connection

1. User logs in → JWT access token stored in `authStore`
2. `App.tsx` renders `ProtectedRoute` → `useWebSocket()` hook activates
3. Hook detects `accessToken` → calls `POST /auth/ws-ticket` to get a 30s single-use ticket
4. WebSocket connects to `ws://localhost:8080/api/v1/ws?ticket=<ticket>`
5. Backend validates ticket (single-use, 30s TTL) → sends `connected` message
6. Frontend stores `client_id` and `user_id` → updates `chatStore.isConnected = true`
7. `ConnectionStatus` component shows "Connected" badge

### 2. Channel Subscription

1. User navigates to a channel
2. `useChannelSubscription(channelId)` hook subscribes
3. Backend adds client to channel's subscriber set
4. Backend sends `subscribed` confirmation
5. User can now receive messages from that channel

### 3. Sending a Message

1. User types message → clicks send
2. Frontend encrypts via `e2eeService`, then calls
   `wsService.sendMessage(channelId, ciphertext, { keyVersion, ... })`
3. Backend receives message → broadcasts to all subscribers (including sender)
4. All clients receive `message` event → update UI

### 4. Reconnection

1. WebSocket connection lost (network issue, server restart, etc.)
2. `wsService` detects close event → schedules reconnect
3. Exponential backoff: 1s, 2s, 4s, 8s, 16s, 30s (max)
4. Attempts up to 10 times
5. On successful reconnect:
   - Backend sends new `connected` message
   - Frontend resubscribes to all channels automatically
   - UI shows "Connected" again

---

## Testing

### Manual Testing (with TEST_WEBSOCKET.html)

**Backend test client location:**
`services/control-plane/internal/websocket/TEST_WEBSOCKET.html`

**Steps:**

1. Start backend server: `cd services/control-plane && ./bin/control-plane`
2. Register/login to get JWT token:
   ```bash
   curl -X POST http://localhost:8080/api/v1/auth/login \
     -H "Content-Type: application/json" \
     -d '{"username":"testuser","password":"password"}'
   ```
3. Copy `access_token` from response
4. Open `TEST_WEBSOCKET.html` in browser
5. Paste JWT token → Click "Connect"
6. Create a channel (via REST API or UI)
7. Subscribe to channel (paste channel UUID)
8. Send messages
9. Open multiple tabs to test multi-device support

### Integration Testing

**Test the full stack:**

1. Start backend: `cd services/control-plane && ./bin/control-plane`
2. Start frontend: `cd client/desktop && npm run dev`
3. Login to frontend
4. Check connection status (should show "Connected")
5. Create a server and channel
6. Send messages (should appear in real-time)
7. Open DevTools → Console → Verify no errors
8. Open DevTools → Network → WS → Verify messages

### Automated Testing (Future)

Create E2E tests for:

- [ ] WebSocket connection establishment
- [ ] Auto-reconnection after disconnect
- [ ] Message send/receive
- [ ] Typing indicators
- [ ] Multi-device synchronization

---

## Performance Considerations

### Current Limits

- **Max message size:** 512 KB (backend limit)
- **Ping interval:** 30 seconds (client-side check)
- **Pong timeout:** 60 seconds (backend closes connection if no pong)
- **Reconnect attempts:** 10 max (then ERROR state)
- **Max reconnect delay:** 30 seconds

### Optimizations

- Buffered channels in Go backend (256 message buffer)
- Message deduplication in chatStore (by ID)
- Lazy loading of messages (pagination with `prependMessages`)
- Typing indicator cleanup (remove >5s old indicators)

### Scalability

- **Current:** Single backend instance (in-memory Hub)
- **Future (Phase 2+):** Redis Pub/Sub for multi-instance message routing

---

## Debugging

### Enable Verbose Logging

```typescript
// In websocketService.ts, uncomment console.log statements
// In useWebSocket.ts, add console.log to handlers
```

### Check Connection State

```typescript
// In React DevTools console:
import { getWebSocketService } from './services/websocketService';
const wsService = getWebSocketService();
console.log('State:', wsService.getState());
console.log('Connection info:', wsService.getConnectionInfo());
```

### Inspect Zustand State

```typescript
// In React DevTools console:
import { useChatStore } from './stores/chat/chatStore';
console.log('Messages:', useChatStore.getState().messagesByChannel);
console.log('Connected:', useChatStore.getState().isConnected);
```

### Backend Logs

```bash
# Backend logs show:
# - Client registered: user=<uuid> client=<uuid> total_clients=1
# - Client unregistered: user=<uuid> client=<uuid> remaining=0
# - Subscription events
# - Message routing
```

---

## Known Issues & Future Improvements

### Current Limitations

- [x] Message delivery confirmation (sent vs received) — ✅ Implemented (Phase 1B)
- [x] Offline message queue — ✅ Implemented via `messageQueue.ts`
- [ ] No typing indicator debouncing (could spam server)
- [x] Presence tracking UI — ✅ Implemented (online/offline indicators)

### Phase 1C Enhancements (Complete)

- [x] E2EE message encryption/decryption — ✅ Implemented
- [x] Presence system UI (online/offline/away indicators) — ✅ Implemented
- [x] User avatars in messages — ✅ Implemented
- [x] Unread message badges — ✅ Implemented
- [x] Voice/video signaling via NATS — ✅ Implemented (Media Plane)

### Phase 2 Enhancements

- [x] Message reactions (emoji) — ✅ Implemented
- [x] Message threading/replies — ✅ Implemented
- [x] File attachments — ✅ Implemented
- [x] Message search — ✅ Implemented
- [ ] Push notifications

---

## Security Considerations

### Ticket-Based Authentication

- ✅ Client gets a 30-second single-use ticket via `POST /auth/ws-ticket`
- ✅ Ticket sent via query parameter (WebSocket upgrade handshake) — NOT the raw JWT
- ✅ Ticket validated and consumed on backend before upgrade (single-use, prevents replay)
- ✅ JWT blacklist check runs during ticket creation
- ⚠️ Use HTTPS/WSS in production to protect ticket in transit

### Connection Security

- ✅ Ticket-based authentication required for all connections
- ✅ User ID extracted from validated session (no client can spoof it)
- ⚠️ Currently uses `ws://` (unencrypted), must use `wss://` in production
- ✅ CORS properly configured in backend

### Message Validation

- ✅ All messages validated for channel_id
- ✅ User permissions checked on backend (channel membership)
- ✅ E2EE message encryption/decryption implemented (Phase 1C)
- ✅ Every inbound frame is validated against `WebSocketEventSchema` (a zod 4 discriminated union in `types/ws-events.ts`) at the dispatch boundary (`websocketService.ts:780`) before any handler sees it. Unparseable frames are dropped, counted as a wire violation, and logged through `scrubZodIssues` so rejected-payload PII never reaches the log sink.

---

## Related Issues

- **#10** - Backend WebSocket Server Infrastructure ✅ Complete
- **#20** - Frontend WebSocket Client Connection Manager ✅ Complete (this issue)
- **#21** - WebSocket Message Handler ✅ Complete
- **#22** - E2EE Message Encryption/Decryption ✅ Complete (Phase 1B)
- **#23** - Presence UI Integration ✅ Complete (Phase 1B)
- **#24** - State Management Consolidation ✅ Complete (Zustand)

---

## Maintainers

- **Mark** (Backend WebSocket server)
- **Michael** (Frontend WebSocket client)

Last updated: 2026-03-03
