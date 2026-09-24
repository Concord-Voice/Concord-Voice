import { describe, expect, it, vi } from 'vitest';
import { createMockRouter, createMockTransport, createMockProducer } from './mocks/mediasoup.js';
import './mocks/logger.js';

// Mock mediasoup native module (same as roomManager.test.ts — prevents native
// C++ binding loading).
vi.mock('mediasoup', () => ({
  createWorker: vi.fn(),
}));

// Mock config (same shape as roomManager.test.ts).
vi.mock('@/config/index.js', () => ({
  config: {
    freeVideoPublisherCap: 8,
    freeScreenProducerCap: 1,
    freeAudioLastN: 8,
    audioLastNHoldMs: 2500,
    mediasoup: {
      webRtcTransport: {
        listenIps: [{ ip: '0.0.0.0', announcedIp: '127.0.0.1' }],
        enableUdp: true,
        enableTcp: false,
        preferUdp: true,
        initialAvailableOutgoingBitrate: 1_000_000,
        maxIncomingBitrate: 50_000_000,
      },
    },
    audioLevelObserver: {
      maxEntries: 1,
      threshold: -60,
      interval: 300,
    },
  },
}));

import {
  FREE_MEDIA_ENTITLEMENT,
  RoomManager,
  SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
} from '../src/lib/roomManager.js';
import { MediaPolicer, MediaPolicyLedger, POLICER_INTERVAL_MS } from '../src/lib/mediaPolicer.js';
import {
  createMediaPolicerTick,
  type MediaPolicerIO,
  type MediaPolicerRoomManager,
} from '../src/lib/mediaPolicerTick.js';

/**
 * #2153 gap 4: an integration test with the REAL `RoomManager`, the REAL
 * `MediaPolicer` + `MediaPolicyLedger`, and the REAL `createMediaPolicerTick`
 * wiring — only mediasoup and Socket.IO are faked. Every other test in this
 * suite exercises one of those three units in isolation with a hand-rolled
 * double of its neighbours; this is the one place that proves the wiring
 * between them, end to end, the way `index.ts` assembles it (which is
 * coverage-excluded and has no test of its own — see
 * `mediaPolicerWiring.test.ts`, which scans `index.ts`'s source rather than
 * running it).
 */

const ROOM = 'room-1';
const USER = 'user-1';
const SOCKET = 'socket-1';
const PRODUCER_SOURCE = 'mic';

function createFakeIo(socketId: string) {
  const peerEmit = vi.fn();
  const except = vi.fn(() => ({ emit: peerEmit }));
  const to = vi.fn(() => ({ except }));
  const ownerEmit = vi.fn();
  const disconnect = vi.fn();
  const io = {
    to,
    sockets: {
      sockets: new Map([[socketId, { emit: ownerEmit, disconnect }]]),
    },
  } as unknown as MediaPolicerIO;
  return { io, to, except, peerEmit, ownerEmit, disconnect };
}

describe('media policer integration (#2153 real RoomManager + MediaPolicer + tick)', () => {
  it('pauses a free participant’s mic once its observed rate sustains well above the free limit', async () => {
    const mockRouter = createMockRouter();
    const mockMediasoup = {
      getOrCreateRouter: vi.fn().mockResolvedValue(mockRouter),
      removeRouter: vi.fn(),
    };
    const manager = new RoomManager(
      mockMediasoup as unknown as ConstructorParameters<typeof RoomManager>[0]
    );

    // One controllable monotonic clock shared by RoomManager's stamps and the
    // tick — exactly how index.ts wires them (see media-plane.md § "Voice
    // lifecycle publisher clock" / #2153).
    let clockMs = 1_000_000;
    const now = () => clockMs;
    manager.setMonotonicClock(now);
    const ledger = new MediaPolicyLedger();
    manager.setMediaPolicyGate(ledger);

    // --- Join, promote, create a send transport, and produce a mic (free
    // entitlement: FREE_MEDIA_ENTITLEMENT). ---
    await manager.joinRoom(ROOM, USER, SOCKET, { username: 'alice' }, undefined, {
      entitlement: undefined,
      mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
      roomContext: undefined,
    });
    // joinRoom only registers a provisional participant; index.ts promotes it
    // once the voice-enforcement session is registered and reauthorized.
    manager.promoteChannelParticipant(
      ROOM,
      USER,
      SOCKET,
      {
        identity: { username: 'alice' },
        entitlement: { ...FREE_MEDIA_ENTITLEMENT },
        serverMuted: false,
        serverDeafened: false,
      },
      () => undefined
    );

    const transport = createMockTransport({
      getStats: vi.fn(async () => [{ rtpBytesReceived: 0, rtxBytesReceived: 0 }]),
    });
    mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
    await manager.createTransport(ROOM, USER, 'send');

    const producer = createMockProducer({
      kind: 'audio',
      pause: vi.fn(() => Promise.resolve()),
      getStats: vi.fn(async () => [{ ssrc: 12_345, byteCount: 0, packetCount: 0 }]),
    });
    transport.produce.mockResolvedValueOnce(producer);
    const rtpParameters = {
      codecs: [{ mimeType: 'audio/opus', payloadType: 111, clockRate: 48_000, channels: 2 }],
      headerExtensions: [],
      encodings: [{ ssrc: 12_345 }],
      rtcp: { cname: 'test', reducedSize: true },
    };
    await manager.produce(
      ROOM,
      USER,
      transport.id,
      'audio',
      rtpParameters as never,
      PRODUCER_SOURCE
    );

    // --- Wire the real policer + ledger + tick, exactly as index.ts does. ---
    const policer = new MediaPolicer();
    const { io, to, except, peerEmit, ownerEmit } = createFakeIo(SOCKET);
    const tick = createMediaPolicerTick({
      roomManager: manager as unknown as MediaPolicerRoomManager,
      io,
      policer,
      ledger,
      emit: undefined,
      now,
    });

    // Packets stay at the stock 50 pps throughout, so only the byte check can trip.
    const PACKETS_PER_INTERVAL = (50 * POLICER_INTERVAL_MS) / 1_000;
    let cumulativeBytes = 0;
    let cumulativePackets = 0;
    const advance = async (bps: number) => {
      cumulativeBytes += (bps * POLICER_INTERVAL_MS) / 8_000;
      cumulativePackets += PACKETS_PER_INTERVAL;
      clockMs += POLICER_INTERVAL_MS;
      producer.getStats = vi.fn(async () => [
        { ssrc: 12_345, byteCount: cumulativeBytes, packetCount: cumulativePackets },
      ]);
      transport.getStats = vi.fn(async () => [
        { rtpBytesReceived: cumulativeBytes, rtxBytesReceived: 0 },
      ]);
      await tick.runOnce();
    };

    // A compliant interval (half the free 216 kbps limit) sets the baseline and must not trip.
    await advance(0.5 * 216_000);
    expect(producer.pause).not.toHaveBeenCalled();

    // 510 kbps adds (510 - 216) x 5 = 1470 kbit of debt per interval, under the
    // 10 s x 216 kbps = 2160 kbit budget after one interval and over it after two.
    await advance(510_000);
    expect(producer.pause).not.toHaveBeenCalled();
    await advance(510_000);

    // The producer was paused, and latched (never resumable) — the real
    // ProducerEntry, not a fake.
    expect(producer.pause).toHaveBeenCalledTimes(1);
    const participant = manager.getParticipant(ROOM, USER);
    expect(participant?.producers.get(producer.id)?.policed).toBe(true);

    // The owner got the media-policy notice on its own socket...
    expect(ownerEmit).toHaveBeenCalledWith('media-policy-notice', {
      producerId: producer.id,
      kind: 'audio',
      source: PRODUCER_SOURCE,
      action: 'paused',
    });
    // ...and peers (everyone except the owner) got producer-paused.
    expect(to).toHaveBeenCalledWith(ROOM);
    expect(except).toHaveBeenCalledWith(SOCKET);
    expect(peerEmit).toHaveBeenCalledWith('producer-paused', {
      producerId: producer.id,
      userId: USER,
      kind: 'audio',
      source: PRODUCER_SOURCE,
    });
  });
});
