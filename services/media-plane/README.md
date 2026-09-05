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
# Must stay INSIDE the range the compose file publishes — mediasoup does not
# bound a bind to the published window, so a wider range here binds ports
# Docker never published. The shipped files publish 40000-40099 (dev),
# 40000-40199 (staging) and 40000-41999 (production).
RTC_MAX_PORT=40099

# Mediasoup settings
NUM_WORKERS=4
MEDIASOUP_LOG_LEVEL=warn
```

**Important for Docker/Cloud:**

- Set `ANNOUNCED_IP` to your server's public IP
- Open the UDP range your compose file publishes — `40000-40099` (dev), `40000-40199`
  (staging), `40000-41999` (production) — in your firewall. UDP-only, by design, per ADR-0040 (not an
  oversight): see [ADR-0040](../../[internal]0040-ice-tcp-ingress-posture.md). ICE-TCP is
  gated off by default via `MEDIASOUP_ENABLE_TCP` (only a value that normalizes to `true` —
  trimmed and case-insensitive — opens it). If
  you enable it, open the **same range for TCP first** — on the publish stanza of whichever
  compose file you deploy with, and on both firewall surfaces — or clients will burn ICE
  connectivity checks on a candidate your ingress black-holes with no reset. The firewall
  permits must be **scoped to the public interface**, which the existing UDP rules are not, so
  do not copy their shape:

  ```bash
  # NET_IFACE is a LOCAL variable inside provision-production.sh — it is neither
  # exported nor persisted, so an operator shell must resolve it first or these
  # commands expand to `ufw allow in on  to any ...` and fail.
  NET_IFACE=$(ip route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1)}' | head -1)
  NET_IFACE="${NET_IFACE:-eth0}"

  ufw allow in on ${NET_IFACE} to any port 40000:41999 proto tcp comment 'RTC TCP'
  iptables -I DOCKER-USER -i ${NET_IFACE} -p tcp --dport 40000:41999 -j RETURN
  ```

  **The `iptables` line above is NOT persistent, and running it alone is a trap.**
  `docker-user-firewall.service` runs `/etc/iptables/docker-user-rules.sh` on boot, which
  begins by flushing the chain (`iptables -F DOCKER-USER`) and rebuilding it from the
  provisioning source. A live `-I` therefore survives until the next reboot or firewall-service
  restart and then vanishes — while the compose gate stays enabled, so advertised TCP candidates
  are black-holed again with nothing in the config to show why. Make it persistent WITHOUT re-running
  the provisioner: **`provision-production.sh` unconditionally rewrites the nginx config to its
  port-80 bootstrap form**, which drops HTTPS while the certificate stays valid — the runbook
  warns about exactly this at
  [`hetzner-ovh-migration.md:478-500`](../../[internal]hetzner-ovh-migration.md). ADR-0040
  says this opt-in needs no host reprovisioning, and it should not.

  Instead, do both of these:

  1. **On the host**, add the permit to `/etc/iptables/docker-user-rules.sh` beside the existing
     `-I DOCKER-USER` lines — but write the interface name **literally**; do NOT paste the
     `${NET_IFACE}` form shown above. That file is generated from an _unquoted_ heredoc
     (`provision-production.sh:1002`), so the variable is expanded as the file is written and
     the installed script neither sets nor inherits it — systemd runs it with an empty
     environment. The empty expansion does not survive as an empty argument — it disappears, so
     a pasted `-i ${NET_IFACE}` reaches iptables as `-i -p tcp ...` with `-p` consumed as the
     interface name. Whatever iptables then makes of that, it is not the rule that was written,
     and the rest of the script carries on rebuilding the chain — leaving the gate enabled and
     the permit absent, with no failed unit to notice. Read the concrete name off that file's
     own last line (`-A DOCKER-USER -i <iface> -j DROP`, already expanded for the same reason).
     Then
     `systemctl restart docker-user-firewall.service` to apply it.
  2. **In the repo**, persist **both** permits in `[internal]provision-production.sh`.
     They have separate reset paths, so fixing one leaves the other to vanish:
     - **DOCKER-USER** — inside the `IPTEOF` heredoc, beside the other `-I DOCKER-USER` rules
       (`:1030-1044`). Write `${NET_IFACE}` here: unlike step 1 this is the generating side,
       where it expands correctly.
     - **UFW** — beside `ufw allow 40000:49999/udp comment 'WebRTC RTP media'` (`:975`). The
       firewall section opens with `ufw --force reset` (`:959`) and rebuilds from that list
       alone, so a permit added only with `ufw allow` on the host is erased by the next
       legitimate provision — while the committed gate and publication stay enabled, which
       black-holes the advertised candidates again with nothing in the config to show why.

     **These two edits must land in ONE commit, together with the TCP publication and
     the gate flip** — do not stage them as a separate "firewall first" change. The
     parity test's disabled branch rejects any _recognised_ firewall permit overlapping
     the RTC window while the compose literals still read `"false"`, so a repo commit
     carrying only the permits fails the repository's own blocking check and cannot
     merge. It also refuses, loudly, any permit it cannot classify, along two separate
     axes. A permit yielding **no port range** must fall into one of four provably-safe
     shapes (wrong protocol, established-only, a non-public interface, or a pinned
     service name), or name a pinned variable port. A permit that **does** carry a port
     must actually have been accepted by the range extractor — the test compares the two
     sets and refuses the difference, so a rule the extractor silently dropped for
     carrying a selector it does not recognise (`-o docker0`, `-p 6`, `--syn`) fails
     rather than falling between them. Unrecognised `ufw` verbs and rule languages the
     test does not parse (`iptables-restore`, `nft`) are refused outright rather than
     read partially.

     The residual is **recognition, not classification**: a permit installed by
     something the provisioner calls but does not inline is outside the text this test
     reads at all, and no amount of classifying closes that. That is why "recognised"
     appears in the first sentence of this paragraph: it is a qualifier, not filler.

     The live-host steps in 1 are a different matter and the ordering there is the
     opposite: apply the host permits **before** flipping the gate, or clients burn ICE
     checks on candidates the ingress black-holes. Host actions are not commits, so no
     test gates them. Either way, do not run the provisioner to deliver any of this.

  **Why the interface scope and the `-I`.** The public interface is whatever
  `ip route get 1.1.1.1` resolves to — the same detection `provision-production.sh` performs
  at `:783-784`, defaulting to `eth0`. Scope the permit to it explicitly: an unscoped rule
  also opens the TCP listener on the OVH private `ens3` and on any future WireGuard
  backplane. Use `-I` rather than `-A` because the chain ends in an interface-scoped `DROP`,
  so an appended permit lands after it and is inert.

  `test-rtc-port-ingress-parity.sh` accepts exactly these two shapes, and refuses anything
  it cannot prove equivalent:

  ```sh
  ufw allow in on ${NET_IFACE} to any port <lo>:<hi> proto tcp
  iptables -I DOCKER-USER -i ${NET_IFACE} -p tcp --dport <lo>:<hi> -j RETURN
  ```

  `<lo>:<hi>` must EQUAL the TCP publication — wider is rejected, not just uncovered.

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
`client/desktop/src/renderer/services/e2ee/mediaEncryption.ts:84`).

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

- **RTC media ports** — UDP-only by design, per ADR-0040; no TCP is published. The code default
  is `40000-49999`, but every shipped compose file narrows `RTC_MIN_PORT`/`RTC_MAX_PORT` to match
  what it publishes: `40000-40099` (dev), `40000-40199` (staging), `40000-41999` (production).

**Note:** the RTC range and the compose publish stanza must agree, and the range must not be
wider. mediasoup does not validate this — it will happily bind a port Docker never published,
which fails with no useful diagnostic. `[internal]tests/test-rtc-port-ingress-parity.sh`
asserts the containment for every compose file. A narrower range also means fewer iptables DNAT
rules, which is why production uses 2,000 ports rather than the full 10,000.

**UDP-only is by design** ([ADR-0040](../../[internal]0040-ice-tcp-ingress-posture.md)), not a
gap — no TCP listener is published for the RTC range. ICE-TCP exists in mediasoup but is gated
off by `MEDIASOUP_ENABLE_TCP` (default `false`). Before enabling it, open the **same range for
TCP first**, on the same publish stanza and both firewall surfaces the UDP range already uses;
otherwise you recreate the exact defect ADR-0040 removed. UDP-blocked clients already have a
path: TURN/TURNS relay on `3478/tcp` and `5349/tcp`.

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
