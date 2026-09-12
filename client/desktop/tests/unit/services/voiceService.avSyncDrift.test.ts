/**
 * A/V sync-drift sampler regressions (#2941) -- `runAvSyncSamplingTick` /
 * `handleAvSyncObservation` in voiceService.ts.
 *
 * Mirrors voiceService.ignisRecovery.test.ts's harness shape (mock the external
 * deps just enough to import voiceService, then reach private sampler state
 * directly via a narrow internal-access cast) rather than driving a full
 * mediasoup/socket join -- runAvSyncSamplingTick and handleAvSyncObservation
 * read only recvTransportAudio/recvTransportVideo and the avSync* latch
 * fields, none of which requires a live room.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// ---------------------------------------------------------------------------
// Mock external dependencies BEFORE importing voiceService
// ---------------------------------------------------------------------------

// --- mediasoup-client ---
const mockDeviceLoad = vi.fn().mockResolvedValue(undefined);
const mockDeviceRtpCapabilities = {
  codecs: [
    { mimeType: 'audio/opus', kind: 'audio', clockRate: 48000, channels: 2, parameters: {} },
    { mimeType: 'video/VP8', kind: 'video', clockRate: 90000, parameters: {} },
  ],
};
const mockCreateSendTransport = vi.fn();
const mockCreateRecvTransport = vi.fn();

vi.mock('mediasoup-client', () => ({
  Device: class MockDevice {
    load = mockDeviceLoad;
    rtpCapabilities = mockDeviceRtpCapabilities;
    createSendTransport = mockCreateSendTransport;
    createRecvTransport = mockCreateRecvTransport;
    loaded = true;
  },
  types: {},
}));

// --- socket.io-client ---
const mockSocket = {
  connected: false,
  emit: vi.fn(),
  on: vi.fn(),
  once: vi.fn(),
  disconnect: vi.fn(),
  io: { on: vi.fn() },
};
vi.mock('socket.io-client', () => ({
  io: vi.fn().mockReturnValue(mockSocket),
}));

// --- apiClient ---
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

// --- e2eeService ---
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    getChannelKey: vi.fn().mockResolvedValue(null),
    getChannelKeyMaterial: vi.fn().mockResolvedValue({ channelKey: null, keyVersion: 0 }),
    invalidateChannelKey: vi.fn(),
    getChannelKeyVersion: vi.fn().mockReturnValue(0),
    getChannelKeyByVersion: vi.fn().mockResolvedValue(null),
    onKeyRotation: vi.fn().mockReturnValue(() => {}),
  },
}));

// --- mediaEncryption ---
vi.mock('@/renderer/services/e2ee/mediaEncryption', () => ({
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 5,
  MediaEncryption: class MockMediaEncryption {
    init = vi.fn().mockResolvedValue(undefined);
    initFromKey = vi.fn();
    destroy = vi.fn();
    getCurrentKeyId = vi.fn().mockReturnValue(0);
    setCurrentKeyId = vi.fn();
    setKeyVersion = vi.fn();
    getKeyVersion = vi.fn().mockReturnValue(0);
    encryptFrame = vi.fn().mockResolvedValue(undefined);
    decryptFrame = vi.fn().mockResolvedValue(undefined);
    addDecryptKey = vi.fn().mockResolvedValue(undefined);
    addDecryptKeyAtEpoch = vi.fn().mockResolvedValue({} as CryptoKey);
    addDecryptKeyAtVersion = vi.fn().mockResolvedValue({} as CryptoKey);
    addDecryptKeyDirect = vi.fn();
    addDecryptKeyDirectV3 = vi.fn();
    debouncedRotateKeys = vi.fn();
    catchUpToEpoch = vi.fn().mockResolvedValue(undefined);
  },
  deriveFrameKey: vi.fn().mockResolvedValue({} as CryptoKey),
  ratchetKey: vi.fn().mockResolvedValue({} as CryptoKey),
}));

// --- osPermissionStore ---
vi.mock('@/renderer/stores/voice/osPermissionStore', () => ({
  useOsPermissionStore: {
    getState: vi.fn().mockReturnValue({
      checkOne: vi.fn().mockResolvedValue('granted'),
      openSettings: vi.fn(),
    }),
    subscribe: vi.fn().mockReturnValue(() => {}),
  },
  ensureOsPermission: vi.fn().mockResolvedValue('granted'),
}));

// --- browser APIs (import-time only -- no join is ever driven in this file) ---
class MockMediaStream {
  private _tracks: unknown[];
  constructor(tracks?: unknown[]) {
    this._tracks = tracks || [];
  }
  getTracks() {
    return this._tracks;
  }
  getAudioTracks() {
    return this._tracks.filter((t: any) => t.kind === 'audio'); // eslint-disable-line @typescript-eslint/no-explicit-any
  }
  getVideoTracks() {
    return this._tracks.filter((t: any) => t.kind === 'video'); // eslint-disable-line @typescript-eslint/no-explicit-any
  }
  addTrack(t: unknown) {
    this._tracks.push(t);
  }
}
Object.defineProperty(globalThis, 'MediaStream', {
  value: MockMediaStream,
  writable: true,
  configurable: true,
});

function MockRTCRtpSender() {}
Object.defineProperty(globalThis, 'RTCRtpSender', {
  value: MockRTCRtpSender,
  writable: true,
  configurable: true,
});
if ('RTCRtpScriptTransform' in globalThis) {
  delete (globalThis as Record<string, unknown>)['RTCRtpScriptTransform'];
}

class MockAudioContext {
  state = 'running';
  currentTime = 0;
  sampleRate = 48000;
  createMediaStreamSource = vi.fn().mockReturnValue({ connect: vi.fn(), disconnect: vi.fn() });
  createAnalyser = vi.fn().mockReturnValue({
    fftSize: 0,
    smoothingTimeConstant: 0,
    frequencyBinCount: 128,
    getByteFrequencyData: vi.fn(),
    getByteTimeDomainData: vi.fn(),
    connect: vi.fn(),
    disconnect: vi.fn(),
  });
  createGain = vi.fn().mockReturnValue({
    gain: { value: 1, setTargetAtTime: vi.fn() },
    connect: vi.fn(),
    disconnect: vi.fn(),
  });
  createMediaStreamDestination = vi.fn().mockReturnValue({
    stream: { getAudioTracks: vi.fn().mockReturnValue([]) },
  });
  close = vi.fn().mockResolvedValue(undefined);
}
Object.defineProperty(globalThis, 'AudioContext', {
  value: MockAudioContext,
  writable: true,
  configurable: true,
});

// `mockGetUserMedia` is captured (rather than left anonymous, as the rest of this
// file's browser-API mocks are) because the join-path arming test below needs to
// configure a resolved mic stream -- same reason voiceService.iceServers.test.ts
// keeps its own named reference.
const mockGetUserMedia = vi.fn();
Object.defineProperty(navigator, 'mediaDevices', {
  value: {
    getUserMedia: mockGetUserMedia,
    getDisplayMedia: vi.fn(),
    enumerateDevices: vi.fn().mockResolvedValue([]),
  },
  writable: true,
  configurable: true,
});

// ---------------------------------------------------------------------------
// Import voiceService AFTER all mocks
// ---------------------------------------------------------------------------
const { voiceService } = await import('@/renderer/services/voice/voiceService');
import {
  FIELD_ABSENCE_STREAK_TO_REPORT,
  AMBIGUOUS_STREAK_TO_REPORT,
  SAMPLE_INTERVAL_MS,
  type AvSyncDriftDetector,
  type AvSyncObservation,
  type AvSyncUnusableReason,
} from '@/renderer/services/voice/avSyncDriftDetector';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

// ---------------------------------------------------------------------------
// Private-surface access -- same technique as voiceService.iceServers.test.ts's
// `heldIceServers` accessor and voiceService.ignisRecovery.test.ts's `svc as any`,
// narrowed to exactly the fields/methods this suite touches.
// ---------------------------------------------------------------------------

interface AvSyncTransport {
  closed: boolean;
  getStats: () => Promise<Map<string, unknown>>;
}

interface VoiceServiceAvSyncInternals {
  runAvSyncSamplingTick(): Promise<void>;
  startAvSyncSampling(): void;
  handleAvSyncObservation(
    observation: AvSyncObservation,
    audioReport: RTCStatsReport,
    videoReport: RTCStatsReport
  ): void;
  avSyncSamplingTimer: ReturnType<typeof setInterval> | null;
  avSyncSamplingInFlight: boolean;
  avSyncDetector: AvSyncDriftDetector;
  avSyncDriftEmitted: boolean;
  avSyncUnavailableEmitted: boolean;
  avSyncFieldAbsentStreak: number;
  // Gitar #3995572815: tests/** is covered by NO tsc project config and Vitest transpiles
  // without type-checking, so an undeclared field here fails silently at runtime instead of
  // at compile time. Every private the harness touches must be declared.
  avSyncUnmeasuredEmitted: boolean;
  avSyncAmbiguousStreak: number;
  avSyncObservationCount: number;
  avSyncSessionGeneration: number;
  recvTransportAudio: AvSyncTransport | null;
  recvTransportVideo: AvSyncTransport | null;
  emergencyCleanup(): void;
  leaveChannel(opts?: { internalRebuild?: boolean }): Promise<void>;
}

function internals(): VoiceServiceAvSyncInternals {
  return voiceService as unknown as VoiceServiceAvSyncInternals;
}

function resetAvSyncState(): void {
  const svc = internals();
  if (svc.avSyncSamplingTimer) clearInterval(svc.avSyncSamplingTimer);
  svc.avSyncSamplingTimer = null;
  svc.avSyncSamplingInFlight = false;
  svc.avSyncDetector.reset();
  svc.avSyncDriftEmitted = false;
  svc.avSyncUnavailableEmitted = false;
  svc.avSyncUnmeasuredEmitted = false;
  svc.avSyncFieldAbsentStreak = 0;
  svc.avSyncAmbiguousStreak = 0;
  svc.avSyncObservationCount = 0;
  svc.recvTransportAudio = null;
  svc.recvTransportVideo = null;
}

function makeTransport(getStats: () => Promise<Map<string, unknown>>): AvSyncTransport {
  return { closed: false, getStats: vi.fn(getStats) };
}

function driftObservation(
  overrides: Partial<Extract<AvSyncObservation, { verdict: 'drift' }>> = {}
): AvSyncObservation {
  return {
    usable: true,
    verdict: 'drift',
    slopeMsPerSec: 1.5,
    spanSec: 70,
    samples: 10,
    jbSkewMs: 2,
    ...overrides,
  };
}

function unusable(reason: AvSyncUnusableReason): AvSyncObservation {
  return { usable: false, reason };
}

// A candidate-pair-shaped RTCStatsReport-like Map -- same fixture shape as
// voiceService.iceServers.test.ts's RELAY_STATS, keyed by the report's own id
// (extractSelectedCandidatePairType reads Map keys, not an `id` field).
const RELAY_STATS = new Map<string, unknown>([
  [
    'cp-1',
    { type: 'candidate-pair', state: 'succeeded', nominated: true, localCandidateId: 'lc-1' },
  ],
  ['lc-1', { type: 'local-candidate', candidateType: 'relay' }],
]);
const HOST_STATS = new Map<string, unknown>([
  [
    'cp-1',
    { type: 'candidate-pair', state: 'succeeded', nominated: true, localCandidateId: 'lc-1' },
  ],
  ['lc-1', { type: 'local-candidate', candidateType: 'host' }],
]);
const NO_PAIR_STATS = new Map<string, unknown>([
  ['cp-1', { type: 'candidate-pair', state: 'failed', nominated: false, localCandidateId: 'lc-1' }],
  ['lc-1', { type: 'local-candidate', candidateType: 'relay' }],
]);

function asReport(m: Map<string, unknown>): RTCStatsReport {
  return m as unknown as RTCStatsReport;
}

/** A realistic startup frame: negotiated inbound-rtp for one kind, but no
 *  remote-outbound-rtp at all -- libwebrtc cannot build that object before the
 *  first RTCP SR arrives, so every healthy call starts this way. */
function inboundOnly(kind: 'audio' | 'video', t: number): Map<string, unknown> {
  return new Map<string, unknown>([
    [
      `in-${kind}`,
      {
        id: `in-${kind}`,
        type: 'inbound-rtp',
        kind,
        ssrc: kind === 'audio' ? 1 : 2,
        trackIdentifier: kind,
        remoteId: `ro-${kind}`,
        timestamp: t,
        jitterBufferDelay: 0.05,
        jitterBufferEmittedCount: 100,
      },
    ],
  ]);
}

describe('A/V sync-drift sampler (#2941)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    resetAvSyncState();
  });

  afterEach(() => {
    resetAvSyncState();
  });

  // -------------------------------------------------------------------------
  // AC-8 + AC-10: the two labels, latching, re-arm, and routine silence.
  // -------------------------------------------------------------------------
  describe('AC-8: drift-detected / fields-unavailable are distinct labels and console methods', () => {
    it('drift-detected uses console.warn; fields-unavailable uses console.debug -- never the reverse, never shared', () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        internals().handleAvSyncObservation(
          driftObservation(),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        expect(warnSpy).toHaveBeenCalledWith('[avsync] drift-detected', expect.any(Object));
        expect(debugSpy).not.toHaveBeenCalled();

        // Capture the drift label BEFORE clearing. mockClear() erases mock.calls, and
        // phase 2 asserts warn is never called again -- so reading warnSpy.mock.calls[0]
        // afterwards is reading an array the test itself just emptied.
        const driftLabel = warnSpy.mock.calls[0][0];

        resetAvSyncState();
        warnSpy.mockClear();
        debugSpy.mockClear();

        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT; i++) {
          internals().handleAvSyncObservation(
            unusable('field-absent'),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
        }
        expect(debugSpy).toHaveBeenCalledWith('[avsync] fields-unavailable', expect.any(Object));
        expect(warnSpy).not.toHaveBeenCalled();

        // The two label strings themselves must never collide -- register row 4's
        // binding precedent: one shared label would mean neither thing.
        expect(driftLabel).not.toBe(debugSpy.mock.calls[0][0]);
      } finally {
        warnSpy.mockRestore();
        debugSpy.mockRestore();
      }
    });
  });

  describe('AC-10: drift-detected and fields-unavailable are both latched exactly once per epoch', () => {
    it('drift-detected fires once; a second drift observation in the same epoch emits nothing', () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      try {
        internals().handleAvSyncObservation(
          driftObservation(),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        internals().handleAvSyncObservation(
          driftObservation(),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        expect(warnSpy).toHaveBeenCalledTimes(1);
      } finally {
        warnSpy.mockRestore();
      }
    });

    it.each(['stream-changed', 'counter-reset', 'offset-step'] as const)(
      're-arms on a genuine pairing reset (%s) so the NEXT drift trips again',
      (reason) => {
        const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
        try {
          internals().handleAvSyncObservation(
            driftObservation(),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
          expect(warnSpy).toHaveBeenCalledTimes(1);

          internals().handleAvSyncObservation(
            unusable(reason),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
          internals().handleAvSyncObservation(
            driftObservation(),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
          expect(warnSpy).toHaveBeenCalledTimes(2);
        } finally {
          warnSpy.mockRestore();
        }
      }
    );

    it('does NOT re-arm on a routine reason (insufficient-samples) -- the latch stays spent', () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      try {
        internals().handleAvSyncObservation(
          driftObservation(),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        expect(warnSpy).toHaveBeenCalledTimes(1);

        internals().handleAvSyncObservation(
          unusable('insufficient-samples'),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        internals().handleAvSyncObservation(
          driftObservation(),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        expect(warnSpy).toHaveBeenCalledTimes(1); // still just the first
      } finally {
        warnSpy.mockRestore();
      }
    });

    it.each([
      'first-sample',
      'kind-incomplete',
      'insufficient-samples',
      'offset-step',
      'missing-stats',
      'stale-stats',
      'future-stats',
      'out-of-order',
      'clock-regression',
      'sr-not-advanced',
    ] satisfies AvSyncUnusableReason[])(
      'routine reason "%s" emits NOTHING -- not warn, not debug',
      (reason) => {
        const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
        const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
        try {
          internals().handleAvSyncObservation(
            unusable(reason),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
          expect(warnSpy).not.toHaveBeenCalled();
          expect(debugSpy).not.toHaveBeenCalled();
        } finally {
          warnSpy.mockRestore();
          debugSpy.mockRestore();
        }
      }
    );

    it('a steady (usable, non-drift) observation emits nothing', () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        internals().handleAvSyncObservation(
          { usable: true, verdict: 'steady' },
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        expect(warnSpy).not.toHaveBeenCalled();
        expect(debugSpy).not.toHaveBeenCalled();
      } finally {
        warnSpy.mockRestore();
        debugSpy.mockRestore();
      }
    });
  });

  // -------------------------------------------------------------------------
  // The startup regression lock (R1): FIELD_ABSENCE_STREAK_TO_REPORT governs
  // the debug latch, imported (never hardcoded), reset by any other reason.
  // -------------------------------------------------------------------------
  describe('startup regression lock: the field-absence streak counter (#2941 R1)', () => {
    it(`${FIELD_ABSENCE_STREAK_TO_REPORT - 1} consecutive field-absent observations emit nothing`, () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT - 1; i++) {
          internals().handleAvSyncObservation(
            unusable('field-absent'),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
        }
        expect(debugSpy).not.toHaveBeenCalled();
      } finally {
        debugSpy.mockRestore();
      }
    });

    it(`the ${FIELD_ABSENCE_STREAK_TO_REPORT}th consecutive field-absent observation emits exactly once`, () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT; i++) {
          internals().handleAvSyncObservation(
            unusable('field-absent'),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
        }
        expect(debugSpy).toHaveBeenCalledTimes(1);
        expect(debugSpy).toHaveBeenCalledWith('[avsync] fields-unavailable', {
          reason: 'field-absent',
        });
      } finally {
        debugSpy.mockRestore();
      }
    });

    it('a single non-field-absence observation resets the streak, so an intermittent absence never accumulates', () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT - 1; i++) {
          internals().handleAvSyncObservation(
            unusable('field-absent'),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
        }
        // One routine, non-absence observation breaks the streak.
        internals().handleAvSyncObservation(
          unusable('insufficient-samples'),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        // Falling one short of the threshold again must NOT trip the latch --
        // if the reset above were a no-op, this loop alone would reach the
        // ORIGINAL streak's remainder and fire early.
        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT - 1; i++) {
          internals().handleAvSyncObservation(
            unusable('field-absent'),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
        }
        expect(debugSpy).not.toHaveBeenCalled();

        // The very next one completes a FRESH full streak.
        internals().handleAvSyncObservation(
          unusable('field-absent'),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        expect(debugSpy).toHaveBeenCalledTimes(1);
      } finally {
        debugSpy.mockRestore();
      }
    });

    it('"unpaired" shares the field-absence streak with "field-absent" (documented actual behavior)', () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        // A mix of the two reasons the sampler treats as one class still reaches
        // the threshold and reports under the field-absent label.
        internals().handleAvSyncObservation(
          unusable('unpaired'),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT - 2; i++) {
          internals().handleAvSyncObservation(
            unusable('field-absent'),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
        }
        expect(debugSpy).not.toHaveBeenCalled();
        internals().handleAvSyncObservation(
          unusable('unpaired'),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        expect(debugSpy).toHaveBeenCalledTimes(1);
      } finally {
        debugSpy.mockRestore();
      }
    });

    it('end-to-end: a real startup sequence through the real detector and a real tick reports fields-unavailable exactly once', async () => {
      const svc = internals();
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      let clock = 1_000_000;
      const nowSpy = vi
        .spyOn(performance, 'now')
        .mockImplementation(() => (clock += SAMPLE_INTERVAL_MS));
      svc.recvTransportAudio = makeTransport(async () => inboundOnly('audio', clock));
      svc.recvTransportVideo = makeTransport(async () => inboundOnly('video', clock));
      try {
        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT - 1; i++) {
          await svc.runAvSyncSamplingTick();
        }
        expect(debugSpy).not.toHaveBeenCalled();

        await svc.runAvSyncSamplingTick();
        expect(debugSpy).toHaveBeenCalledTimes(1);
        expect(debugSpy).toHaveBeenCalledWith('[avsync] fields-unavailable', {
          reason: 'field-absent',
        });
        expect(warnSpy).not.toHaveBeenCalled();
      } finally {
        debugSpy.mockRestore();
        warnSpy.mockRestore();
        nowSpy.mockRestore();
      }
    });
  });

  // -------------------------------------------------------------------------
  // AC-9 (sampler-level): the payload actually handed to console.warn.
  // -------------------------------------------------------------------------
  describe('AC-9: the drift-detected payload actually emitted by handleAvSyncObservation', () => {
    it('has exactly the expected keys, rounds to 2dp, spanSec is an integer, and carries no identifier', () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      try {
        internals().handleAvSyncObservation(
          driftObservation({
            slopeMsPerSec: 1.23456,
            spanSec: 70.7,
            samples: 9,
            jbSkewMs: -3.14159,
          }),
          asReport(RELAY_STATS),
          asReport(HOST_STATS)
        );
        expect(warnSpy).toHaveBeenCalledTimes(1);
        const [label, payload] = warnSpy.mock.calls[0] as [string, Record<string, unknown>];
        expect(label).toBe('[avsync] drift-detected');
        expect(Object.keys(payload).sort()).toEqual(
          ['jbSkewMs', 'pairAudio', 'pairVideo', 'samples', 'slopeMsPerSec', 'spanSec'].sort()
        );
        expect(payload.slopeMsPerSec).toBe(1.23);
        expect(payload.jbSkewMs).toBe(-3.14);
        expect(payload.spanSec).toBe(71);
        expect(Number.isInteger(payload.spanSec)).toBe(true);
        expect(payload.samples).toBe(9);
        expect(payload.pairAudio).toBe('relay');
        expect(payload.pairVideo).toBe('host');

        const serialized = JSON.stringify(warnSpy.mock.calls);
        expect(serialized).not.toMatch(/\b(?:\d{1,3}\.){3}\d{1,3}\b/); // IPv4-shaped
        expect(serialized).not.toMatch(/[^",{}:\s]{40,}/); // any unbroken token >= 40 chars
        expect(serialized).not.toMatch(
          /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i
        ); // UUID
      } finally {
        warnSpy.mockRestore();
      }
    });

    it('passes a null jbSkewMs through as null, never coerced to 0 or rounded', () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      try {
        internals().handleAvSyncObservation(
          driftObservation({ jbSkewMs: null }),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        const [, payload] = warnSpy.mock.calls[0] as [string, Record<string, unknown>];
        expect(payload.jbSkewMs).toBeNull();
      } finally {
        warnSpy.mockRestore();
      }
    });

    it('pairAudio/pairVideo are null when no succeeded candidate pair resolves -- never a fifth value', () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      try {
        internals().handleAvSyncObservation(
          driftObservation(),
          asReport(NO_PAIR_STATS),
          asReport(NO_PAIR_STATS)
        );
        const [, payload] = warnSpy.mock.calls[0] as [string, Record<string, unknown>];
        expect(payload.pairAudio).toBeNull();
        expect(payload.pairVideo).toBeNull();
      } finally {
        warnSpy.mockRestore();
      }
    });

    it('pairAudio and pairVideo are drawn from the closed enum and may legitimately differ (differential path)', () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      try {
        internals().handleAvSyncObservation(
          driftObservation(),
          asReport(RELAY_STATS),
          asReport(HOST_STATS)
        );
        const [, payload] = warnSpy.mock.calls[0] as [string, Record<string, unknown>];
        const closed = new Set(['host', 'srflx', 'prflx', 'relay', null]);
        expect(closed.has(payload.pairAudio as string | null)).toBe(true);
        expect(closed.has(payload.pairVideo as string | null)).toBe(true);
        expect(payload.pairAudio).not.toBe(payload.pairVideo);
      } finally {
        warnSpy.mockRestore();
      }
    });
  });

  // -------------------------------------------------------------------------
  // AC-18: tick arming and both teardown paths.
  // -------------------------------------------------------------------------
  describe('AC-18: tick arming', () => {
    it('skips the tick and calls getStats on neither transport when the audio recv transport is null', async () => {
      const svc = internals();
      const videoGetStats = vi.fn(async () => new Map<string, unknown>());
      svc.recvTransportAudio = null;
      svc.recvTransportVideo = makeTransport(async () => new Map());
      // reassign to capture the spy identity for assertion
      svc.recvTransportVideo.getStats = videoGetStats;
      await svc.runAvSyncSamplingTick();
      expect(videoGetStats).not.toHaveBeenCalled();
    });

    it('skips the tick and calls getStats on neither transport when the video recv transport is null', async () => {
      const svc = internals();
      const audioGetStats = vi.fn(async () => new Map<string, unknown>());
      svc.recvTransportAudio = makeTransport(async () => new Map());
      svc.recvTransportAudio.getStats = audioGetStats;
      svc.recvTransportVideo = null;
      await svc.runAvSyncSamplingTick();
      expect(audioGetStats).not.toHaveBeenCalled();
    });

    it('skips the tick when the audio recv transport is closed', async () => {
      const svc = internals();
      const audioGetStats = vi.fn(async () => new Map<string, unknown>());
      const videoGetStats = vi.fn(async () => new Map<string, unknown>());
      svc.recvTransportAudio = { closed: true, getStats: audioGetStats };
      svc.recvTransportVideo = { closed: false, getStats: videoGetStats };
      await svc.runAvSyncSamplingTick();
      expect(audioGetStats).not.toHaveBeenCalled();
      expect(videoGetStats).not.toHaveBeenCalled();
    });

    it('skips the tick when the video recv transport is closed', async () => {
      const svc = internals();
      const audioGetStats = vi.fn(async () => new Map<string, unknown>());
      const videoGetStats = vi.fn(async () => new Map<string, unknown>());
      svc.recvTransportAudio = { closed: false, getStats: audioGetStats };
      svc.recvTransportVideo = { closed: true, getStats: videoGetStats };
      await svc.runAvSyncSamplingTick();
      expect(audioGetStats).not.toHaveBeenCalled();
      expect(videoGetStats).not.toHaveBeenCalled();
    });

    it('takes its clock stamp before the awaits and calls getStats() on BOTH transports', async () => {
      const svc = internals();
      const nowSpy = vi
        .spyOn(performance, 'now')
        .mockReturnValueOnce(12_345)
        .mockReturnValue(99_999);
      const timeOrigin = performance.timeOrigin;
      let resolveAudio!: (v: Map<string, unknown>) => void;
      const audioPending = new Promise<Map<string, unknown>>((resolve) => {
        resolveAudio = resolve;
      });
      const audioGetStats = vi.fn(() => audioPending);
      const videoGetStats = vi.fn(async () => new Map<string, unknown>());
      svc.recvTransportAudio = { closed: false, getStats: audioGetStats };
      svc.recvTransportVideo = { closed: false, getStats: videoGetStats };
      const observeSpy = vi.spyOn(svc.avSyncDetector, 'observe');

      const tickPromise = svc.runAvSyncSamplingTick();
      // The async function's synchronous prologue (up to the `await Promise.all`)
      // has already run by the time this line executes -- both getStats() calls
      // and the single performance.now() read happened before any microtask flush.
      expect(audioGetStats).toHaveBeenCalledTimes(1);
      expect(videoGetStats).toHaveBeenCalledTimes(1);
      expect(nowSpy).toHaveBeenCalledTimes(1);

      resolveAudio(new Map());
      await tickPromise;

      expect(observeSpy.mock.calls[0][1]).toBe(timeOrigin + 12_345);
      nowSpy.mockRestore();
    });

    it('does not run a second tick while one is still in flight (single-flight)', async () => {
      const svc = internals();
      let resolveAudio!: (v: Map<string, unknown>) => void;
      const audioPending = new Promise<Map<string, unknown>>((resolve) => {
        resolveAudio = resolve;
      });
      const audioGetStats = vi.fn(() => audioPending);
      const videoGetStats = vi.fn(async () => new Map<string, unknown>());
      svc.recvTransportAudio = { closed: false, getStats: audioGetStats };
      svc.recvTransportVideo = { closed: false, getStats: videoGetStats };

      const first = svc.runAvSyncSamplingTick();
      expect(svc.avSyncSamplingInFlight).toBe(true);

      const second = svc.runAvSyncSamplingTick(); // must return immediately
      await second;
      // The second call must not have touched either transport at all.
      expect(audioGetStats).toHaveBeenCalledTimes(1);
      expect(videoGetStats).toHaveBeenCalledTimes(1);

      resolveAudio(new Map());
      await first;
      expect(svc.avSyncSamplingInFlight).toBe(false);
    });
  });

  describe('AC-18: both teardown paths reset timer, detector, latches, and streak -- independently', () => {
    function dirty(): void {
      const svc = internals();
      svc.avSyncSamplingTimer = setInterval(() => undefined, 100_000);
      svc.avSyncSamplingInFlight = true;
      svc.avSyncDriftEmitted = true;
      svc.avSyncUnavailableEmitted = true;
      svc.avSyncFieldAbsentStreak = 3;
    }

    it('emergencyCleanup() clears the timer, resets the detector, and resets both latches and the streak', () => {
      dirty();
      const svc = internals();
      const detectorResetSpy = vi.spyOn(svc.avSyncDetector, 'reset');
      const timerBefore = svc.avSyncSamplingTimer;
      expect(timerBefore).not.toBeNull();

      svc.emergencyCleanup();

      expect(svc.avSyncSamplingTimer).toBeNull();
      expect(svc.avSyncSamplingInFlight).toBe(false);
      expect(detectorResetSpy).toHaveBeenCalled();
      expect(svc.avSyncDriftEmitted).toBe(false);
      expect(svc.avSyncUnavailableEmitted).toBe(false);
      expect(svc.avSyncFieldAbsentStreak).toBe(0);
    });

    it('leaveChannel() clears the timer, resets the detector, and resets both latches and the streak -- NOT via emergencyCleanup', async () => {
      dirty();
      const svc = internals();
      const detectorResetSpy = vi.spyOn(svc.avSyncDetector, 'reset');
      const timerBefore = svc.avSyncSamplingTimer;
      expect(timerBefore).not.toBeNull();

      await svc.leaveChannel();

      expect(svc.avSyncSamplingTimer).toBeNull();
      expect(svc.avSyncSamplingInFlight).toBe(false);
      expect(detectorResetSpy).toHaveBeenCalled();
      expect(svc.avSyncDriftEmitted).toBe(false);
      expect(svc.avSyncUnavailableEmitted).toBe(false);
      expect(svc.avSyncFieldAbsentStreak).toBe(0);
    });
  });

  // -------------------------------------------------------------------------
  // #2941 R1 (criticality 10): the arming path. Deleting both
  // `startAvSyncSampling()` call sites in voiceService.ts survives every
  // other test in this file, because every other test either calls
  // `runAvSyncSamplingTick()` directly or hand-sets `avSyncSamplingTimer`.
  // This proves `startAvSyncSampling()` itself arms a real timer at the
  // correct interval and that the SCHEDULED CALLBACK invokes the tick --
  // it does NOT prove either production call site (voiceService.ts
  // ~:2959 join path, ~:3320 resume path) actually calls
  // `startAvSyncSampling()`; that reachability is unproven by this suite.
  // -------------------------------------------------------------------------
  describe('#2941 R1: startAvSyncSampling arms the fixed-cadence timer', () => {
    it('arms setInterval at SAMPLE_INTERVAL_MS whose callback invokes runAvSyncSamplingTick', () => {
      const svc = internals();
      const setIntervalSpy = vi.spyOn(globalThis, 'setInterval');
      const tickSpy = vi.spyOn(svc, 'runAvSyncSamplingTick').mockResolvedValue(undefined);
      try {
        svc.startAvSyncSampling();

        expect(setIntervalSpy).toHaveBeenCalledWith(expect.any(Function), SAMPLE_INTERVAL_MS);
        expect(svc.avSyncSamplingTimer).not.toBeNull();

        // Invoke the exact callback setInterval was armed with -- proves the
        // arming wires to the real tick, not just that SOME timer exists.
        const call = setIntervalSpy.mock.calls.find(([, ms]) => ms === SAMPLE_INTERVAL_MS);
        expect(call).toBeDefined();
        const scheduledCallback = call?.[0] as () => void;
        expect(tickSpy).not.toHaveBeenCalled();
        scheduledCallback();
        expect(tickSpy).toHaveBeenCalledTimes(1);
      } finally {
        setIntervalSpy.mockRestore();
        tickSpy.mockRestore();
      }
    });

    // CodeRabbit #3994846533. joinChannel() and resumeAfterReconnect() hold
    // INDEPENDENT in-flight guards, so both can reach startAvSyncSampling().
    // A bare re-assignment would overwrite the first handle, leaving an interval
    // that no teardown path holds a reference to -- it would keep sampling for
    // the rest of the process with no way to stop it.
    it('is idempotent: a second arm does not orphan the first interval', () => {
      const svc = internals();
      const setIntervalSpy = vi.spyOn(globalThis, 'setInterval');
      const clearSpy = vi.spyOn(globalThis, 'clearInterval');
      try {
        svc.startAvSyncSampling();
        const first = svc.avSyncSamplingTimer;
        expect(first).not.toBeNull();

        svc.startAvSyncSampling();

        expect(svc.avSyncSamplingTimer).toBe(first);
        expect(
          setIntervalSpy.mock.calls.filter(([, ms]) => ms === SAMPLE_INTERVAL_MS)
        ).toHaveLength(1);
        expect(clearSpy).not.toHaveBeenCalled();
      } finally {
        setIntervalSpy.mockRestore();
        clearSpy.mockRestore();
      }
    });
  });

  // -------------------------------------------------------------------------
  // CodeRabbit #3994846536: a tick that started before a teardown must not let
  // its post-await continuation touch the NEXT session.
  // -------------------------------------------------------------------------
  // -------------------------------------------------------------------------
  // Gitar #3994682744: a 3+-party call refuses on EVERY tick for the whole
  // session. Without its own latched line it emits nothing at all, and a
  // triager reading a group-call bug report sees the same empty log as a
  // healthy two-party call.
  // -------------------------------------------------------------------------
  // -------------------------------------------------------------------------
  // CodeRabbit #3995103950: a witness that builds its OWN detector and drives
  // its OWN getStats() loop proves the module works on live stats. It does not
  // prove this sampler is running -- the tick returns early on a missing
  // transport, a rejecting getStats(), or a session change mid-await. Only a
  // counter stamped inside the production tick separates those.
  // -------------------------------------------------------------------------
  describe('#2941: the production tick carries its own observation witness', () => {
    it('counts a real tick, and counts NOTHING on each early-return path', async () => {
      const svc = internals();
      expect(svc.avSyncObservationCount).toBe(0);

      // 1. No transports -> returns before observe().
      svc.recvTransportAudio = null;
      svc.recvTransportVideo = null;
      await svc.runAvSyncSamplingTick();
      expect(svc.avSyncObservationCount).toBe(0);

      // 2. getStats() rejects -> returns before observe().
      const rejecting = makeTransport(() => Promise.reject(new Error('closing')));
      svc.recvTransportAudio = rejecting;
      svc.recvTransportVideo = rejecting;
      await svc.runAvSyncSamplingTick();
      expect(svc.avSyncObservationCount).toBe(0);

      // 3. A real tick -> reaches observe(), so the witness moves. It counts
      //    EVERY observation, not just usable ones: the question is whether the
      //    sampler ran, not whether it liked what it saw.
      const healthy = makeTransport(() => Promise.resolve(new Map()));
      svc.recvTransportAudio = healthy;
      svc.recvTransportVideo = healthy;
      await svc.runAvSyncSamplingTick();
      expect(svc.avSyncObservationCount).toBe(1);
    });
  });

  describe('#2941: a group call announces itself once instead of going silently dark', () => {
    const feedAmbiguous = (n: number) => {
      for (let i = 0; i < n; i++) {
        internals().handleAvSyncObservation(
          unusable('ambiguous-streams'),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
      }
    };

    it('emits [avsync] unmeasured exactly once, only after the streak persists', () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        feedAmbiguous(AMBIGUOUS_STREAK_TO_REPORT - 1);
        expect(debugSpy).not.toHaveBeenCalled();

        feedAmbiguous(1);
        expect(debugSpy).toHaveBeenCalledTimes(1);
        expect(debugSpy).toHaveBeenCalledWith('[avsync] unmeasured', {
          reason: 'ambiguous-streams',
        });

        // Latched: a group call refuses for its whole session and must not spam.
        feedAmbiguous(50);
        expect(debugSpy).toHaveBeenCalledTimes(1);
      } finally {
        debugSpy.mockRestore();
      }
    });

    it('carries no peer count or identifier -- the payload reaches a PUBLIC repo', () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        feedAmbiguous(AMBIGUOUS_STREAK_TO_REPORT);
        const [, payload] = debugSpy.mock.calls[0] as [string, Record<string, unknown>];
        expect(Object.keys(payload)).toEqual(['reason']);
        expect(JSON.stringify(payload)).not.toMatch(/\d/);
      } finally {
        debugSpy.mockRestore();
      }
    });

    it('a usable observation breaks the streak, so a momentary join does not spend the latch', () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        feedAmbiguous(AMBIGUOUS_STREAK_TO_REPORT - 1);
        internals().handleAvSyncObservation(
          { usable: true, verdict: 'steady' },
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        feedAmbiguous(AMBIGUOUS_STREAK_TO_REPORT - 1);
        expect(debugSpy).not.toHaveBeenCalled();
      } finally {
        debugSpy.mockRestore();
      }
    });

    it('CONTROL: the two latches are independent -- ambiguity does not spend fields-unavailable', () => {
      // This is what keeps the fix faithful to the decision that ambiguous-streams
      // stays OUT of the field-absence class: a group call must still leave the
      // engine-regression latch armed for a genuine later field absence.
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        feedAmbiguous(AMBIGUOUS_STREAK_TO_REPORT + 5);
        expect(debugSpy).toHaveBeenCalledTimes(1);

        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT; i++) {
          internals().handleAvSyncObservation(
            unusable('field-absent'),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
        }
        expect(debugSpy).toHaveBeenCalledTimes(2);
        expect(debugSpy).toHaveBeenLastCalledWith('[avsync] fields-unavailable', {
          reason: 'field-absent',
        });
      } finally {
        debugSpy.mockRestore();
      }
    });
  });

  describe('#2941: a stale tick continuation cannot reach the next session', () => {
    it('does not observe, and does not clear the new in-flight flag, after a teardown', async () => {
      const svc = internals();
      const observeSpy = vi.spyOn(svc.avSyncDetector, 'observe');
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});
      try {
        // Hold getStats() pending so teardown can land mid-tick.
        let release!: () => void;
        const pending = new Promise<void>((r) => {
          release = r;
        });
        const stalled = {
          closed: false,
          getStats: vi.fn().mockImplementation(async () => {
            await pending;
            return new Map();
          }),
        };
        svc.recvTransportAudio = stalled;
        svc.recvTransportVideo = stalled;

        const inFlightTick = svc.runAvSyncSamplingTick();

        // Teardown while the awaits are still pending: bumps the generation.
        svc.avSyncSessionGeneration += 1;
        // A new session arms its own tick and owns the in-flight flag.
        svc.avSyncSamplingInFlight = true;

        release();
        await inFlightTick;

        // The stale continuation must not feed the new session's detector...
        expect(observeSpy).not.toHaveBeenCalled();
        // ...and must not clear the flag the NEW session is holding, which would
        // admit a second concurrent tick into it.
        expect(svc.avSyncSamplingInFlight).toBe(true);
        expect(warnSpy).not.toHaveBeenCalled();
      } finally {
        observeSpy.mockRestore();
        warnSpy.mockRestore();
        debugSpy.mockRestore();
      }
    });
  });

  // -------------------------------------------------------------------------
  // #2941 R2: a USABLE observation must break the field-absence streak, not
  // just an unusable-but-non-absence reason. The existing ~:533 test breaks
  // the streak with 'insufficient-samples' (usable: false), which reaches
  // the OTHER reset branch (isFieldAbsenceClass === false) and never
  // exercises the reset statement at the top of the `observation.usable`
  // block.
  // -------------------------------------------------------------------------
  describe('#2941 R2: a usable (steady) observation resets the field-absence streak', () => {
    it('a steady observation between two partial streaks prevents them from combining into a false report', () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      try {
        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT - 1; i++) {
          internals().handleAvSyncObservation(
            unusable('field-absent'),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
        }

        // A genuine usable, steady observation -- must reset via the
        // `observation.usable` branch's reset statement.
        internals().handleAvSyncObservation(
          { usable: true, verdict: 'steady' },
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );

        // If the reset above were a no-op, this second partial streak would
        // combine with the first to reach (and exceed) the threshold.
        for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT - 1; i++) {
          internals().handleAvSyncObservation(
            unusable('field-absent'),
            asReport(RELAY_STATS),
            asReport(RELAY_STATS)
          );
        }
        expect(debugSpy).not.toHaveBeenCalled();

        // The next one completes a genuinely fresh full streak.
        internals().handleAvSyncObservation(
          unusable('field-absent'),
          asReport(RELAY_STATS),
          asReport(RELAY_STATS)
        );
        expect(debugSpy).toHaveBeenCalledTimes(1);
      } finally {
        debugSpy.mockRestore();
      }
    });
  });

  // -------------------------------------------------------------------------
  // #2941 R3: single-flight must recover from a rejecting getStats(). The
  // `finally` clearing avSyncSamplingInFlight is correct; nothing pinned it,
  // so moving that clear into the `try` (after the merge/observe/dispatch
  // code, past the inner catch's `return`) would wedge the sampler forever
  // after the first rejection -- surviving every other test in this file.
  // -------------------------------------------------------------------------
  describe('#2941 R3: single-flight recovers from a rejecting getStats()', () => {
    it('a rejecting getStats resolves the tick with nothing logged, and a SUBSEQUENT tick still calls getStats', async () => {
      const svc = internals();
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      let audioCalls = 0;
      const audioGetStats = vi.fn(async () => {
        audioCalls += 1;
        if (audioCalls === 1) throw new Error('transport closing');
        return new Map<string, unknown>();
      });
      const videoGetStats = vi.fn(async () => new Map<string, unknown>());
      svc.recvTransportAudio = { closed: false, getStats: audioGetStats };
      svc.recvTransportVideo = { closed: false, getStats: videoGetStats };
      try {
        await expect(svc.runAvSyncSamplingTick()).resolves.toBeUndefined();
        expect(warnSpy).not.toHaveBeenCalled();
        expect(debugSpy).not.toHaveBeenCalled();
        expect(svc.avSyncSamplingInFlight).toBe(false);

        // The mutation-killing assertion: if the in-flight clear were moved
        // into the try (after the inner catch's early return), this second
        // tick would short-circuit at the top and never reach getStats again.
        await svc.runAvSyncSamplingTick();
        expect(audioGetStats).toHaveBeenCalledTimes(2);
        expect(videoGetStats).toHaveBeenCalledTimes(2);
      } finally {
        warnSpy.mockRestore();
        debugSpy.mockRestore();
      }
    });
  });

  // -------------------------------------------------------------------------
  // #2941 register row: the JOIN PATH must actually reach startAvSyncSampling().
  // Every test above calls startAvSyncSampling() or runAvSyncSamplingTick()
  // DIRECTLY -- deleting BOTH `this.startAvSyncSampling()` call sites in
  // voiceService.ts (join path ~:2959, resume path ~:3320) survives every one
  // of them. This drives a REAL joinChannel() through establishMediaSession's
  // success path -- same mocking shape as voiceService.iceServers.test.ts's
  // `joinWith` helper (line 380 there) -- and proves the join itself arms the
  // sampler, closing the reachability gap that suite's own comment names.
  // -------------------------------------------------------------------------
  describe('#2941: joinChannel() itself arms the A/V sync sampler', () => {
    function makeJoinMicTrack() {
      return {
        id: 'mic-1',
        kind: 'audio',
        readyState: 'live',
        enabled: true,
        stop: vi.fn(),
        clone: vi.fn(),
        getSettings: vi.fn().mockReturnValue({}),
        contentHint: '',
        onended: null as (() => void) | null,
      };
    }

    function makeJoinMicStream() {
      const tracks = [makeJoinMicTrack()];
      return {
        getAudioTracks: vi.fn().mockReturnValue(tracks),
        getVideoTracks: vi.fn().mockReturnValue([]),
        getTracks: vi.fn().mockReturnValue(tracks),
        addTrack: vi.fn(),
        removeTrack: vi.fn(),
      };
    }

    function makeJoinMicProducer() {
      return {
        id: 'prod-mic',
        kind: 'audio',
        paused: false,
        closed: false,
        close: vi.fn(),
        pause: vi.fn(),
        resume: vi.fn(),
        replaceTrack: vi.fn().mockResolvedValue(undefined),
        on: vi.fn(),
        getStats: vi.fn().mockResolvedValue(new Map()),
        rtpSender: {
          getParameters: vi.fn().mockReturnValue({
            encodings: [{ maxBitrate: 32000, priority: 'low' }],
            codecs: [{ mimeType: 'audio/opus' }],
          }),
          setParameters: vi.fn().mockResolvedValue(undefined),
          createEncodedStreams: vi.fn().mockImplementation(() => ({
            readable: new ReadableStream({
              start(controller) {
                controller.close();
              },
            }),
            writable: new WritableStream(),
          })),
          transform: null,
        },
        appData: { source: 'mic' },
      };
    }

    function makeJoinSendTransport() {
      return {
        id: 'send-1',
        closed: false,
        close: vi.fn(),
        produce: vi.fn().mockResolvedValue(makeJoinMicProducer()),
        on: vi.fn(),
        getStats: vi.fn().mockResolvedValue(new Map()),
        _awaitQueue: {
          push: vi.fn().mockImplementation(async (fn: () => Promise<void>) => {
            await fn();
          }),
        },
      };
    }

    function makeJoinRecvTransport() {
      return {
        id: 'recv-1',
        closed: false,
        close: vi.fn(),
        consume: vi.fn(),
        on: vi.fn(),
        getStats: vi.fn().mockResolvedValue(new Map()),
      };
    }

    let joinTransportSeq = 0;
    function makeJoinTransportOpts() {
      joinTransportSeq += 1;
      return {
        id: `transport-${joinTransportSeq}`,
        iceParameters: { usernameFragment: 'frag', password: 'pass' }, // pragma: allowlist secret
        iceCandidates: [],
        dtlsParameters: { role: 'auto', fingerprints: [] },
      };
    }

    function joinRoomResponse() {
      return {
        allowed: true,
        media_server_url: 'http://localhost:3000',
        ice_servers: [],
        channel: {
          id: 'channel-1',
          name: 'General',
          server_id: 'server-1',
          audio_quality_tier: null,
        },
      };
    }

    function makeJoinRoomJoined() {
      return {
        rtpCapabilities: mockDeviceRtpCapabilities,
        mediaFrameCryptoVersion: 5,
        existingProducers: [],
        participants: [{ userId: 'user-1', username: 'testuser', displayName: 'Test User' }],
        channelName: 'General',
      };
    }

    function setupJoinEmitResponses() {
      mockSocket.emit.mockImplementation(
        (event: string, _payload: unknown, ack?: (r: unknown) => void) => {
          if (!ack) return;
          if (event === 'join-room') return ack(makeJoinRoomJoined());
          if (event === 'create-transport') return ack(makeJoinTransportOpts());
          if (event === 'produce') return ack({ id: 'prod-mic' });
          return ack(undefined);
        }
      );
    }

    /** Drives a REAL join through establishMediaSession's success path -- the
     *  same mocking shape as voiceService.iceServers.test.ts's `joinWith`. */
    async function driveJoin(): Promise<void> {
      useAuthStore.getState().setAccessToken('test-token');
      useUserStore.setState({
        user: {
          id: 'user-1',
          username: 'testuser',
          display_name: 'Test User',
          avatar_url: null,
          email: 'test@test.com',
          created_at: new Date().toISOString(),
          updated_at: new Date().toISOString(),
        },
      });
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: vi.fn().mockResolvedValue(joinRoomResponse()),
      });
      mockSocket.connected = true;
      mockCreateSendTransport.mockReturnValue(makeJoinSendTransport());
      mockCreateRecvTransport.mockReturnValue(makeJoinRecvTransport());
      mockGetUserMedia.mockResolvedValue(makeJoinMicStream());
      setupJoinEmitResponses();
      await voiceService.joinChannel('channel-1', 'channel');
    }

    beforeEach(() => {
      // initEncryptionCore() refuses outright when no encoded-transform API exists
      // (voiceService.ts:8007) and jsdom defines neither RTCRtpScriptTransform nor
      // RTCRtpSender.prototype.createEncodedStreams. Without one the join aborts in
      // setupE2EEForChannel and never reaches step 10, so the gate under test is
      // never exercised.
      //
      // Supply the createEncodedStreams path, NOT RTCRtpScriptTransform: the modern
      // path constructs a real Worker (voiceService.ts:8269) that jsdom has no
      // implementation for. voiceService.iceServers.test.ts resolves this the same
      // way. Which transform path E2EE selects is irrelevant to this test -- it
      // asserts only that the join REACHES startAvSyncSampling().
      class MockRTCRtpSender {}
      Object.defineProperty(MockRTCRtpSender.prototype, 'createEncodedStreams', {
        value: vi.fn(),
      });
      vi.stubGlobal('RTCRtpSender', MockRTCRtpSender);
    });

    afterEach(() => {
      vi.unstubAllGlobals();
      mockSocket.connected = false;
      mockSocket.emit.mockReset();
      mockCreateSendTransport.mockReset();
      mockCreateRecvTransport.mockReset();
      mockGetUserMedia.mockReset();
      mockApiFetch.mockReset();
    });

    it('a real joinChannel() arms setInterval at SAMPLE_INTERVAL_MS -- proving the join path REACHES startAvSyncSampling(), not just that the method works standalone', async () => {
      const setIntervalSpy = vi.spyOn(globalThis, 'setInterval');
      try {
        await driveJoin();

        expect(internals().avSyncSamplingTimer).not.toBeNull();
        expect(setIntervalSpy).toHaveBeenCalledWith(expect.any(Function), SAMPLE_INTERVAL_MS);
      } finally {
        setIntervalSpy.mockRestore();
      }
    });
  });
});
