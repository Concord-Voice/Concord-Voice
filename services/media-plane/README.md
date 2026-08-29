# Media Plane Service

## Aggregate operations metrics

When enabled, the media plane publishes a fixed scalar snapshot of aggregate
room, participant, publisher, egress, and participant-hour counters. It sends no
room/user/server IDs or arbitrary labels. Snapshots use the signed v1 envelope
and fixed `ops.metrics.media.v1` NATS subject. A failed publication drops one
interval, without buffering or blocking media traffic. See ADR-0030.

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
│   ├── lib/               # 21 modules — see the groups below
│   ├── middleware/
│   │   └── auth.ts        # JWT + Socket.IO authentication middleware
│   └── index.ts           # Application entry point (Socket.IO event handlers)
├── Dockerfile
├── package.json
└── tsconfig.json
```

### `src/lib/` modules

Run `ls src/lib/` for the current set. The modules group as follows.

- **Media core** — `mediasoup.ts` (worker/router management), `roomManager.ts` (room lifecycle, transports, producers, consumers), `activeSpeakerSet.ts`, `closeRecvTransport.ts`
- **Layer governance** — `cameraLayerGovernor.ts`, `screenLayerGovernor.ts` (simulcast/SVC layer selection)
- **Admission and abuse control** — `admissionGate.ts`, `originGate.ts`, `rateLimit.ts`, `enforcePermissions.ts`, `forceDisconnect.ts`
- **Socket lifecycle** — `socketJoinLifecycle.ts`, `setDeafen.ts`, `setTestingStatus.ts`
- **State and messaging** — `nats.ts` (control-plane coordination), `redis.ts` (shared state)
- **Observability** — `logger.ts`, `mediaMetrics.ts`, `opsMetricsCatalog.ts`, `opsMetricsPublisher.ts`, `expressErrorHandler.ts`

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
REDIS_URL=redis://:concord_dev_redis@localhost:6379
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
- Open UDP ports `40000-49999` in your firewall
- `NUM_WORKERS=4` is the code default and the value `docker-compose.yml` sets.
  **Production uses `3`**, matching the media-plane's `cpus: '3'` limit in
  `docker-compose.production.yml`. A mediasoup worker is a single-threaded C++
  subprocess pinned to one core, so a count above the CPU allocation
  oversubscribes the CFS quota and shows up as RTP jitter rather than a crash.
  Nothing enforces agreement between the two values — change them together.
- The worker count is validated fail-closed at startup (#2178): a value that is
  not an integer in `[1, 32]` logs `FATAL:` and exits 1 instead of falling back
  to the default. Both bounds matter. `NUM_WORKERS=0` would otherwise build an
  empty worker pool, let `init()` resolve and `/health` answer 200, and then
  fail every voice join **without crashing** — `getNextWorker()` returns
  `undefined`, `worker.createRouter()` throws a TypeError, and that rejected
  promise is caught by the join handler and converted into an ordinary
  join-error ack. The process survives indefinitely: every join fails, `/health`
  keeps answering 200, and nothing restarts the container. An oversized count is
  the likelier typo and would fail differently — absent the ceiling it would fork
  more subprocesses than the container cgroup can hold, `init()` would never
  resolve, `/health` would never answer, and a scoped deploy's `--wait` would
  time out with the old container already gone. Both are descriptions of what
  the guard prevents; neither value reaches the worker loop today.

## Socket.IO Events

### Client → Server

**join-room**

```typescript
socket.emit('join-room', {
  roomId: string,
  rtpCapabilities,
  mediaFrameCryptoVersion: 5,
});
// room-joined participants include { userId, username, isDeafened, isTesting, ... }
```

The media plane accepts versions 3, 4 and 5
(`ACCEPTED_MEDIA_FRAME_CRYPTO_VERSIONS` in `src/lib/roomManager.ts:596`).
Anything else — a missing value, or a legacy declaration such as `2` — is
rejected at the admission gate. A room keeps one version for its lifetime, so a
joiner that advertises a different one gets `CryptoVersionMismatchError`. The
desktop client sends `5` (`MEDIA_E2EE_FRAME_CRYPTO_VERSION`,
`client/desktop/src/renderer/services/mediaEncryption.ts:84`).

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
socket.on(
  'user-joined',
  ({ userId, username, displayName, avatarUrl, e2eeEpoch, isDeafened, isTesting }) => {
    // New user joined the room; the two state flags are optional rollout fields.
  }
);
```

When `isDeafened` or `isTesting` is absent, clients preserve existing state for
backward compatibility and the join-vs-consume media race.

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
- One per consumed Producer

## Port Requirements

### TCP

- **3000** - HTTP/WebSocket server

### UDP

- **40000-49999** - RTC media ports (configurable)

**Note:** in production, consider a smaller port range to reduce firewall rules. A small deployment might use 40000-40099.

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
- Confirm the client has microphone permissions

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
- [x] Simulcast + SVC layer governance for camera and screen
- [x] Aggregate operations metrics (ADR-0030)
- [x] TURN server integration — coturn deployed with TLS (PRs #576, #577)

## Future Enhancements

- [ ] Recording support
- [ ] Bandwidth estimation and quality adaptation
- [ ] Multi-instance horizontal scaling

## CI

Two Docker build verification tiers run on every PR touching `services/media-plane/**`:

- **Tier-1 `Media Plane / Docker build (cache-warm)`** — fast feedback (~4 min on cache hit). Uses Blacksmith's persistent Docker builder, which is intentionally excluded from Tier 2. Defends application logic correctness. A cache miss on a `package-lock.json` or `Dockerfile` change is normal. See [ADR-0006 §"Tier 1"](../../[internal]0006-cache-tier-split.md).
- **Tier-2 `Production-compose / Docker build (cache-cold)`** — defense-in-depth for the full four-image production-active stack (PostgreSQL, control plane, media plane, and `concord-ops-agent`). The no-cache build catches file-`COPY`-order regressions, postinstall failures, and cross-image Compose drift invisible to cache-warm CI. See [ADR-0006 §"Tier 2"](../../[internal]0006-cache-tier-split.md) and [`[internal]rules/media-plane.md`](../../[internal]rules/media-plane.md) §"Docker Build Context Invariant".

Tier 1 runs a direct media-plane build plus the toolchain-absent, mediasoup worker-spawn, and image-size checks. Tier 2 runs the production Compose build, asserts all four images exist, then repeats the media-plane toolchain and worker smokes. The cache-warm job has a success metric: a ≥85% cache hit rate on lockfile-unchanged PRs over a 2-week window. The script at `[internal]artifacts/1167-measurements/measure-hit-rate.sh` measures it, per spec [`[internal]specs/2026-05-28-1167-cache-warm-mediaplane-design.md`](../../[internal]specs/2026-05-28-1167-cache-warm-mediaplane-design.md) §6.5.
