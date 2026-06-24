# Media Plane Service

**Status:** ✅ IMPLEMENTED (Phase 1C - Complete)

WebRTC Selective Forwarding Unit (SFU) for routing voice and video media in Concord Voice.

## Tech Stack

- **Node.js** 24+
- **TypeScript**
- **mediasoup** - WebRTC SFU library
- **Socket.IO** - WebSocket signaling
- **Express** - HTTP server
- **Redis** - State management and pub/sub
- **NATS** - Inter-service messaging with control-plane
- **JWT** - Authentication (shared secret with control-plane)

## Architecture

The media plane is responsible for:
- WebRTC media routing using SFU architecture
- Handling ICE/STUN/TURN for NAT traversal
- Managing mediasoup workers, routers, and transports
- Real-time signaling via Socket.IO
- Efficient audio forwarding (no mixing)

### Why SFU?

An SFU (Selective Forwarding Unit) forwards media streams without decoding/encoding:
- **Low latency** - No transcoding overhead
- **Scalable** - Each client receives optimized streams
- **Quality** - Preserves original audio quality
- **Efficient** - Server just routes packets

## Project Structure

```
media-plane/
├── src/
│   ├── config/            # Configuration management
│   ├── lib/
│   │   ├── logger.ts      # Winston logger
│   │   ├── mediasoup.ts   # Low-level mediasoup worker/router management
│   │   ├── roomManager.ts # Room lifecycle, transports, producers, consumers
│   │   ├── nats.ts        # NATS client for control-plane coordination
│   │   └── redis.ts       # Redis state management
│   ├── middleware/
│   │   └── auth.ts        # JWT + Socket.IO authentication middleware
│   ├── types/             # TypeScript type definitions
│   └── index.ts           # Application entry point (Socket.IO event handlers)
├── Dockerfile
├── package.json
└── tsconfig.json
```

## Development

### Prerequisites

- Node.js 24+
- npm 10+
- Python 3 (for mediasoup build)
- Build tools (make, g++)

### Setup

1. **Install dependencies**
   ```bash
   npm install
   ```

2. **Set up environment variables**
   ```bash
   cp .env.example .env
   # Edit .env with your configuration
   ```

3. **Run the service**
   ```bash
   npm run dev
   ```

The service will start on port 3000 by default.

### Environment Variables

```bash
ENVIRONMENT=development
PORT=3000
REDIS_URL=redis://localhost:6379
CONTROL_PLANE_URL=http://localhost:8080

# WebRTC settings
ANNOUNCED_IP=127.0.0.1
RTC_MIN_PORT=40000
RTC_MAX_PORT=49999

# Mediasoup settings
NUM_WORKERS=4
MEDIASOUP_LOG_LEVEL=warn
```

**Important for Docker/Cloud:**
- Set `ANNOUNCED_IP` to your server's public IP
- Ensure UDP ports `40000-49999` are open in your firewall

## Socket.IO Events

### Client → Server

**join-room**
```typescript
socket.emit('join-room', {
  roomId: string,
  rtpCapabilities,
  mediaFrameCryptoVersion: 2,
});
// room-joined participants include { userId, username, isDeafened, isTesting, ... }
```

`mediaFrameCryptoVersion: 2` is the AES-256-GCM media-frame format. The media
plane rejects missing or legacy frame-format declarations and keeps one
room-wide value so mixed AES-128/AES-256 clients cannot enter the same call.

**create-transport**
```typescript
socket.emit('create-transport', { roomId: string, direction: 'send' | 'recv' }, (data) => {
  // data: { id, iceParameters, iceCandidates, dtlsParameters }
});
```

**connect-transport**
```typescript
socket.emit('connect-transport', { transportId: string, dtlsParameters }, (data) => {
  // data: { success: boolean }
});
```

**produce**
```typescript
socket.emit('produce', { transportId, kind: 'audio', rtpParameters }, (data) => {
  // data: { id: producerId }
});
```

**consume**
```typescript
socket.emit('consume', { producerId, rtpCapabilities }, (data) => {
  // data: { id, producerId, kind, rtpParameters }
});
```

**request-keyframe**
```typescript
socket.emit('request-keyframe', { senderUserId }, (data) => {
  // data: { success: true, requested: number } | { error: string }
});
```

Requests a fresh video keyframe for the caller's consumer of `senderUserId` after E2EE epoch recovery. The media plane validates room membership and applies a 5s per-sender cooldown before calling mediasoup `consumer.requestKeyFrame()`.

**update-test-status**
```typescript
socket.emit('update-test-status', { isTesting: boolean }, (data) => {
  // data: { success: true } | { error: string }
});
```

### Server → Client

**router-rtp-capabilities**
```typescript
socket.on('router-rtp-capabilities', ({ rtpCapabilities }) => {
  // Use to create send/recv transports
});
```

**user-joined**
```typescript
socket.on('user-joined', ({ userId }) => {
  // New user joined the room
});
```

**new-producer**
```typescript
socket.on('new-producer', ({ producerId, userId }) => {
  // Another user started producing media
});
```

**user-left**
```typescript
socket.on('user-left', ({ userId }) => {
  // User left the room
});
```

**participant-testing-changed**
```typescript
socket.on('participant-testing-changed', ({ userId, isTesting }) => {
  // A participant started or stopped an audio device test
});
```

## Mediasoup Architecture

```
Client 1 (Producer) → WebRTC Transport → Router → WebRTC Transport → Client 2 (Consumer)
Client 3 (Producer) → WebRTC Transport ↗         ↘ WebRTC Transport → Client 4 (Consumer)
```

### Components

**Worker** - CPU-bound process that handles media
- One per CPU core recommended
- Isolated failure domains

**Router** - Routes media within a room
- One per voice channel/room
- Handles codec negotiation

**Transport** - WebRTC connection endpoint
- Two per client (send + receive)
- Handles ICE, DTLS, SRTP

**Producer** - Media stream source
- Created when user starts sending audio
- One per media track

**Consumer** - Media stream sink
- Created when user wants to receive audio
- One per Producer being consumed

## Port Requirements

### TCP
- **3000** - HTTP/WebSocket server

### UDP
- **40000-49999** - RTC media ports (configurable)

**Note:** In production, you may want to limit the port range to reduce firewall rules, e.g., 40000-40099 for a small deployment.

## Scaling

### Horizontal Scaling

For larger deployments:

1. **Multiple media plane instances**
   - Use Redis for shared state
   - Implement room-to-instance mapping
   - Load balance based on geographic location

2. **Router mesh**
   - For rooms with 100+ users
   - Multiple routers pipe media between them
   - Reduces load per router

3. **Regional deployment**
   - Deploy media planes in multiple regions
   - Route users to nearest instance
   - Reduces latency

### Vertical Scaling

- Add more CPU cores → more workers
- Increase port range for more concurrent connections
- Monitor memory usage (scales with active transports)

## Monitoring

Key metrics to monitor:

- Active workers
- Active routers
- Active transports/producers/consumers
- CPU usage per worker
- Memory usage
- Network I/O (UDP traffic)
- Latency (ICE connection time)

## Troubleshooting

### No audio
- Check firewall allows UDP ports
- Verify `ANNOUNCED_IP` is correct
- Check ICE candidates in logs
- Ensure client has microphone permissions

### High latency
- Check geographic distance to media plane
- Monitor CPU usage
- Verify network bandwidth

### Connection failures
- Check TURN server configuration
- Verify firewall rules
- Test with different network types

## Current Features ✅

- [x] Multi-worker mediasoup with round-robin routing
- [x] RoomManager with per-participant transport tracking
- [x] Audio/video/screen share producing and consuming
- [x] AudioLevelObserver for active speaker detection
- [x] JWT-authenticated Socket.IO connections
- [x] NATS integration for control-plane voice coordination
- [x] Redis state management
- [x] Graceful shutdown with resource cleanup
- [x] DM voice call support

## Future Enhancements

- [x] TURN server integration — coturn deployed with TLS (PRs #576, #577)
- [ ] Recording support
- [ ] Simulcast for video
- [ ] Bandwidth estimation and quality adaptation
- [ ] Multi-instance horizontal scaling
- [ ] Metrics and monitoring integration

## CI

Two media-plane Docker build verification jobs run on every PR touching `services/media-plane/**`:

- **Tier-1 `Media Plane / Docker build (cache-warm)`** — fast feedback (~4 min on cache hit). Uses BuildKit's GHA cache backend (`type=gha,mode=max`). Defends application logic correctness; cache miss on `package-lock.json` or `Dockerfile` change is expected. See [ADR-0006 §"Tier 1"](../../[internal]0006-cache-tier-split.md).
- **Tier-2 `Media Plane / Docker build (cache-cold)`** — defense-in-depth (~19 min). Catches file-`COPY`-order regressions and postinstall failures invisible to cache-warm CI. See [ADR-0006 §"Tier 2"](../../[internal]0006-cache-tier-split.md) and [`[internal]rules/media-plane.md`](../../[internal]rules/media-plane.md) §"Docker Build Context Invariant".

Both jobs run `docker build` + three smokes (toolchain-absent assert, mediasoup worker-spawn check, image size log). The cache-warm job's success metric (≥85% cache hit rate on lockfile-unchanged PRs over a 2-week window) is measured per spec [`[internal]specs/2026-05-28-1167-cache-warm-mediaplane-design.md`](../../[internal]specs/2026-05-28-1167-cache-warm-mediaplane-design.md) §6.5 by the script at `[internal]artifacts/1167-measurements/measure-hit-rate.sh`.
