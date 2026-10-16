/**
 * The audio-producer codec/kind check (#3858), against a real mediasoup worker.
 *
 * mediasoup does not compare a producer's `kind` with its codecs, so without the
 * check a `kind: 'audio'` mic carrying `video/VP8` became a live producer that
 * skipped the opus-only audio-tier gate. These cases drive RoomManager.produce()
 * against a real router (transport bound to 127.0.0.1, no packets sent): the
 * non-audio shapes are refused before any producer exists, and the stock free
 * mic is still admitted by the worker. Removing the check turns the first two
 * cases into admitted producers.
 */
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import './mocks/logger.js';

vi.mock('@/config/index.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../src/config/index.js')>();
  return {
    ...actual,
    config: {
      ...actual.config,
      mediasoup: {
        ...actual.config.mediasoup,
        webRtcTransport: {
          ...actual.config.mediasoup.webRtcTransport,
          listenIps: [{ ip: '127.0.0.1' }],
          enableTcp: false,
        },
      },
    },
  };
});

import * as mediasoup from 'mediasoup';
import type { types } from 'mediasoup';
import { config } from '@/config/index.js';
import {
  RoomManager,
  FREE_MEDIA_ENTITLEMENT,
  SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
} from '../src/lib/roomManager.js';

const SPEAK_ONLY = (1n << 9n) | (1n << 16n) | (1n << 17n);
// View + Join voice, Video and ScreenShare, and NOT Speak (#1308): the member
// the symmetric video check exists for.
const VIDEO_NO_SPEAK = (1n << 9n) | (1n << 16n) | (1n << 28n) | (1n << 21n);
const VP8 = { mimeType: 'video/VP8', payloadType: 96, clockRate: 90000 };
const OPUS_IN_TIER = {
  mimeType: 'audio/opus',
  payloadType: 111,
  clockRate: 48000,
  channels: 2,
  parameters: { ptime: 20 },
};

let worker: types.Worker;
let router: types.Router;
let manager: RoomManager;

async function join(userId: string, permissions: bigint = SPEAK_ONLY): Promise<string> {
  await manager.joinRoom('room-fix', userId, `sock-${userId}`, { username: userId }, undefined, {
    entitlement: { ...FREE_MEDIA_ENTITLEMENT },
    mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
    permissions,
    credentialEpoch: '',
  });
  return (await manager.createTransport('room-fix', userId, 'send')).id;
}

function mic(codecs: unknown[], encodings: unknown[]) {
  return { codecs, headerExtensions: [], encodings, rtcp: { cname: 'c' } } as never;
}

beforeAll(async () => {
  worker = await mediasoup.createWorker({ logLevel: 'error' });
  router = await worker.createRouter({ mediaCodecs: config.mediasoup.router.mediaCodecs });
  manager = new RoomManager({
    getWebRtcServer: () => undefined,
    getOrCreateRouter: async () => router,
    removeRouter: () => undefined,
    captureNativeTermination: (r: types.Router, transportIds?: readonly string[]) => ({
      worker,
      router: r,
      routerId: r.id,
      transportIds: transportIds === undefined ? undefined : [...transportIds],
    }),
    confirmNativeTermination: async () => undefined,
    confirmAllRoutersClosed: async () => undefined,
  } as never);
}, 30_000);

afterAll(() => {
  worker?.close();
});

describe('audio producer codec/kind check (#3858), real worker', () => {
  it('refuses a VP8-only mic and creates no producer', async () => {
    const t = await join('f-vp8');
    await expect(
      manager.produce('room-fix', 'f-vp8', t, 'audio', mic([VP8], [{ ssrc: 3, maxBitrate: 510_000 }]), 'mic')
    ).rejects.toThrow('Invalid codec for producer kind');
    expect(manager.getParticipant('room-fix', 'f-vp8')?.producers.size).toBe(0);
  });

  it('refuses VP8 listed ahead of an in-tier opus entry', async () => {
    const t = await join('f-mixed');
    await expect(
      manager.produce(
        'room-fix',
        'f-mixed',
        t,
        'audio',
        mic([VP8, OPUS_IN_TIER], [{ ssrc: 4, maxBitrate: 64_000 }]),
        'mic'
      )
    ).rejects.toThrow('Invalid codec for producer kind');
    expect(manager.getParticipant('room-fix', 'f-mixed')?.producers.size).toBe(0);
  });

  it('refuses a mic with no codecs, and one whose mimeType is not a string', async () => {
    const t = await join('f-shape');
    await expect(
      manager.produce('room-fix', 'f-shape', t, 'audio', mic([], [{ ssrc: 5, maxBitrate: 64_000 }]), 'mic')
    ).rejects.toThrow('Invalid codec for producer kind');
    await expect(
      manager.produce(
        'room-fix',
        'f-shape',
        t,
        'audio',
        mic([{ ...OPUS_IN_TIER, mimeType: 42 }], [{ ssrc: 6, maxBitrate: 64_000 }]),
        'mic'
      )
    ).rejects.toThrow('Invalid codec for producer kind');
  });

  it('admits the stock free mic (opus, ptime 20, 64 kbps)', async () => {
    const t = await join('f-stock');
    const info = await manager.produce(
      'room-fix',
      'f-stock',
      t,
      'audio',
      mic([{ ...OPUS_IN_TIER, parameters: { ptime: 20, useinbandfec: 1, minptime: 10 } }], [{ ssrc: 7, maxBitrate: 64_000 }]),
      'mic'
    );
    expect(info.producerId).toBeTruthy();
    expect(manager.getParticipant('room-fix', 'f-stock')?.producers.size).toBe(1);
  });

  it('leaves the tier gate in force: an opus mic with no ptime is refused', async () => {
    const t = await join('f-gate');
    await expect(
      manager.produce(
        'room-fix',
        'f-gate',
        t,
        'audio',
        mic([{ ...OPUS_IN_TIER, parameters: {} }], [{ ssrc: 8, maxBitrate: 64_000 }]),
        'mic'
      )
    ).rejects.toThrow('Audio producer exceeds tier media limits');
  });
});

/**
 * The symmetric video check (#1308), against the same real worker. mediasoup
 * matches each codec against every router codec, so `kind: 'video'` carrying
 * `audio/opus` would otherwise become a live producer: mic audio published by
 * a member with Video but no Speak, past the Speak check and the audio gate.
 * Removing `codecsAreVideo` turns the first case into an admitted producer.
 */
describe('video producer codec/kind check (#1308), real worker', () => {
  it('refuses an opus-only camera from a member with Video but no Speak', async () => {
    const t = await join('v-opus', VIDEO_NO_SPEAK);
    await expect(
      manager.produce(
        'room-fix',
        'v-opus',
        t,
        'video',
        mic([OPUS_IN_TIER], [{ ssrc: 9, maxBitrate: 64_000 }]),
        'camera'
      )
    ).rejects.toThrow('Invalid codec for producer kind');
    expect(manager.getParticipant('room-fix', 'v-opus')?.producers.size).toBe(0);
  });

  it('admits a stock VP8 camera from the same member', async () => {
    const t = await join('v-vp8', VIDEO_NO_SPEAK);
    const info = await manager.produce(
      'room-fix',
      'v-vp8',
      t,
      'video',
      mic([VP8], [{ ssrc: 10, maxBitrate: 500_000 }]),
      'camera'
    );
    expect(info.producerId).toBeTruthy();
    expect(manager.getParticipant('room-fix', 'v-vp8')?.producers.size).toBe(1);
  });
});
