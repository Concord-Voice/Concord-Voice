import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { RouterRtpCodecCapability } from 'mediasoup/types';

// Mock mediasoup types import (used by config module)
vi.mock('mediasoup/node/lib/rtpParametersTypes.js', () => ({}));

// Silence native env-file loading so tests never pick up a developer's local
// .env. The config module calls process.loadEnvFile() at import time.
const loadEnvFileSpy = vi.spyOn(process, 'loadEnvFile').mockImplementation(() => {});

// Helper: dynamically import config with fresh module evaluation
async function loadConfig(envOverrides: Record<string, string> = {}) {
  // Save and override env
  const saved: Record<string, string | undefined> = {};
  for (const [key, value] of Object.entries(envOverrides)) {
    saved[key] = process.env[key];
    process.env[key] = value;
  }

  vi.resetModules();
  const mod = await import('../src/config/index.js');

  // Restore env
  for (const [key] of Object.entries(envOverrides)) {
    if (saved[key] === undefined) {
      delete process.env[key];
    } else {
      process.env[key] = saved[key];
    }
  }

  return mod;
}

describe('config', () => {
  beforeEach(() => {
    // Ensure we're in dev mode by default
    delete process.env.ENVIRONMENT;
    delete process.env.PORT;
    delete process.env['JWT_' + 'SECRET'];
    delete process.env.ANNOUNCED_IP;
    delete process.env.ALLOWED_ORIGINS;
    delete process.env.NUM_WORKERS;
    delete process.env.RTC_MIN_PORT;
    delete process.env.RTC_MAX_PORT;
    delete process.env.MEDIASOUP_ENABLE_TCP;
    delete process.env.OPS_METRICS_ENABLED;
    delete process.env.OPS_METRICS_NODE_ID;
    delete process.env.OPS_METRICS_SHARED_SECRET;
    delete process.env.OPS_METRICS_INTERVAL;
    delete process.env.OPS_METRICS_ROLE;
    loadEnvFileSpy.mockReset();
    loadEnvFileSpy.mockImplementation(() => {});
  });

  afterEach(() => {
    loadEnvFileSpy.mockImplementation(() => {});
  });

  // ── Native .env loading (replaces dotenv) ───────────────────────────

  describe('env file loading', () => {
    it('loads .env via the native process.loadEnvFile on import', async () => {
      await loadConfig();
      expect(loadEnvFileSpy).toHaveBeenCalled();
    });

    it('starts normally when no .env file exists (ENOENT is soft-failed)', async () => {
      const enoent = Object.assign(new Error('no such file or directory'), { code: 'ENOENT' });
      loadEnvFileSpy.mockImplementation(() => {
        throw enoent;
      });

      // Must not reject: a missing .env is the normal deployed state.
      const mod = await loadConfig();
      expect(mod.AUDIO_QUALITY_TIERS).toBeDefined();
    });

    it('rethrows a non-ENOENT failure instead of silently starting', async () => {
      const eacces = Object.assign(new Error('permission denied'), { code: 'EACCES' });
      loadEnvFileSpy.mockImplementation(() => {
        throw eacces;
      });

      await expect(loadConfig()).rejects.toThrow('permission denied');
    });
  });

  // ── Audio Quality Tiers ─────────────────────────────────────────────

  describe('AUDIO_QUALITY_TIERS', () => {
    it('has all 7 quality tiers', async () => {
      const { AUDIO_QUALITY_TIERS } = await loadConfig();
      const tiers = Object.keys(AUDIO_QUALITY_TIERS);
      expect(tiers).toEqual(['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio']);
    });

    it('has ascending bitrates', async () => {
      const { AUDIO_QUALITY_TIERS } = await loadConfig();
      const bitrates = Object.values(AUDIO_QUALITY_TIERS).map((t) => t.maxBitrate);
      for (let i = 1; i < bitrates.length; i++) {
        expect(bitrates[i]).toBeGreaterThan(bitrates[i - 1]);
      }
    });

    it('marks first 4 tiers as non-premium and last 3 as premium', async () => {
      const { AUDIO_QUALITY_TIERS } = await loadConfig();
      const tiers = Object.values(AUDIO_QUALITY_TIERS);
      expect(tiers.slice(0, 4).every((t) => !t.premium)).toBe(true);
      expect(tiers.slice(4).every((t) => t.premium)).toBe(true);
    });

    it('each tier has all required fields', async () => {
      const { AUDIO_QUALITY_TIERS } = await loadConfig();
      for (const tier of Object.values(AUDIO_QUALITY_TIERS)) {
        expect(tier).toHaveProperty('label');
        expect(tier).toHaveProperty('maxBitrate');
        expect(tier).toHaveProperty('opusDtx');
        expect(tier).toHaveProperty('opusFec');
        expect(tier).toHaveProperty('opusStereo');
        expect(tier).toHaveProperty('preferredFrameSize');
        expect(tier).toHaveProperty('premium');
      }
    });
  });

  // ── Media Codecs ────────────────────────────────────────────────────

  describe('mediaCodecs', () => {
    it('contains 8 codecs (1 audio + 7 video)', async () => {
      const { config } = await loadConfig();
      const codecs = config.mediasoup.router.mediaCodecs;
      expect(codecs).toHaveLength(8);
      expect(codecs.filter((c: RouterRtpCodecCapability) => c.kind === 'audio')).toHaveLength(1);
      expect(codecs.filter((c: RouterRtpCodecCapability) => c.kind === 'video')).toHaveLength(7);
    });

    it('audio codec is opus at 48kHz stereo', async () => {
      const { config } = await loadConfig();
      const audio = config.mediasoup.router.mediaCodecs.find((c: RouterRtpCodecCapability) => c.kind === 'audio');
      expect(audio).toBeDefined();
      expect(audio.mimeType).toBe('audio/opus');
      expect(audio.clockRate).toBe(48000);
      expect(audio.channels).toBe(2);
    });

    it('all video codecs have rtcpFeedback', async () => {
      const { config } = await loadConfig();
      const videoCodecs = config.mediasoup.router.mediaCodecs.filter(
        (c: RouterRtpCodecCapability) => c.kind === 'video'
      );
      for (const codec of videoCodecs) {
        expect(codec.rtcpFeedback).toBeDefined();
        expect(codec.rtcpFeedback.length).toBeGreaterThan(0);
      }
    });
  });

  // ── Config Defaults ─────────────────────────────────────────────────

  describe('defaults', () => {
    it('uses development defaults when no env vars set', async () => {
      const { config } = await loadConfig();
      expect(config.environment).toBe('development');
      expect(config.port).toBe(3000);
      expect(config.rtc.minPort).toBe(40000);
      expect(config.rtc.maxPort).toBe(49999);
      expect(config.mediasoup.numWorkers).toBe(4);
    });

    it('parses ALLOWED_ORIGINS as comma-separated list', async () => {
      const { config } = await loadConfig();
      expect(config.allowedOrigins).toEqual(['http://localhost:3001', 'http://localhost:3002']);
    });

    it('keeps operations metrics dormant by default', async () => {
      const { config } = await loadConfig();
      expect(config.opsMetrics).toEqual({
        enabled: false,
        nodeId: '',
        sharedSecret: '',
        intervalMs: 15_000,
        role: 'local',
      });
    });
  });

  // ── Env Overrides ───────────────────────────────────────────────────

  describe('env overrides', () => {
    it('applies PORT override', async () => {
      const { config } = await loadConfig({ PORT: '5000' });
      expect(config.port).toBe(5000);
    });

    it('applies ANNOUNCED_IP override to rtc and transport listenIps', async () => {
      const { config } = await loadConfig({ ANNOUNCED_IP: '1.2.3.4' });
      expect(config.rtc.announcedIp).toBe('1.2.3.4');
      expect(config.mediasoup.webRtcTransport.listenIps[0].announcedIp).toBe('1.2.3.4');
    });

    it('applies ALLOWED_ORIGINS override', async () => {
      const { config } = await loadConfig({ ALLOWED_ORIGINS: 'https://a.com, https://b.com' });
      expect(config.allowedOrigins).toEqual(['https://a.com', 'https://b.com']);
    });

    describe('MEDIASOUP_ENABLE_TCP gate (#3105)', () => {
      it('defaults enableTcp to false when the var is unset', async () => {
        const { config } = await loadConfig();
        expect(config.mediasoup.webRtcTransport.enableTcp).toBe(false);
      });

      // The ONLY input that opens the capability is the exact string 'true',
      // trimmed and case-folded. Every other shape — including the truthy-
      // looking '1'/'yes'/'on' — is false. There is no fail-open path.
      it.each([
        ['true', true],
        ['TRUE', true],
        ['True', true],
        ['  true  ', true],
        ['', false],
        ['1', false],
        ['yes', false],
        ['on', false],
        ['false', false],
        ['FALSE', false],
        ['tru', false],
        ['true!', false],
        ['truthy', false],
      ])('MEDIASOUP_ENABLE_TCP=%j resolves to %s', async (raw, expected) => {
        const { config } = await loadConfig({ MEDIASOUP_ENABLE_TCP: raw });
        expect(config.mediasoup.webRtcTransport.enableTcp).toBe(expected);
      });

      it('leaves enableUdp and preferUdp untouched when the gate is open', async () => {
        const { config } = await loadConfig({ MEDIASOUP_ENABLE_TCP: 'true' });
        expect(config.mediasoup.webRtcTransport.enableUdp).toBe(true);
        expect(config.mediasoup.webRtcTransport.preferUdp).toBe(true);
      });
    });

    it('applies NUM_WORKERS override', async () => {
      const { config } = await loadConfig({ NUM_WORKERS: '8' });
      expect(config.mediasoup.numWorkers).toBe(8);
    });

    it('loads enabled operations metrics settings', async () => {
      const { config } = await loadConfig({
        OPS_METRICS_ENABLED: 'true',
        OPS_METRICS_NODE_ID: 'cvn_aaaaaaaaaaaaaaaa',
        OPS_METRICS_SHARED_SECRET: '0123456789abcdef0123456789abcdef', // pragma: allowlist secret
        OPS_METRICS_INTERVAL: '30s',
      });
      expect(config.opsMetrics).toEqual({
        enabled: true,
        nodeId: 'cvn_aaaaaaaaaaaaaaaa',
        sharedSecret: '0123456789abcdef0123456789abcdef', // pragma: allowlist secret
        intervalMs: 30_000,
        role: 'local',
      });
    });
  });

  describe('operations metrics guard', () => {
    it.each([
      {
        name: 'missing settings',
        env: { OPS_METRICS_ENABLED: 'true' },
      },
      {
        name: 'hostname node id',
        env: {
          OPS_METRICS_ENABLED: 'true',
          OPS_METRICS_NODE_ID: 'media.concordvoice.chat',
          OPS_METRICS_SHARED_SECRET: '0123456789abcdef0123456789abcdef', // pragma: allowlist secret
        },
      },
      {
        name: 'short secret',
        env: {
          OPS_METRICS_ENABLED: 'true',
          OPS_METRICS_NODE_ID: 'cvn_aaaaaaaaaaaaaaaa',
          OPS_METRICS_SHARED_SECRET: 'short', // pragma: allowlist secret
        },
      },
      {
        name: 'interval outside bounds',
        env: {
          OPS_METRICS_ENABLED: 'true',
          OPS_METRICS_NODE_ID: 'cvn_aaaaaaaaaaaaaaaa',
          OPS_METRICS_SHARED_SECRET: '0123456789abcdef0123456789abcdef', // pragma: allowlist secret
          OPS_METRICS_INTERVAL: '4s',
        },
      },
      {
        name: 'reserved aggregator role',
        env: {
          OPS_METRICS_ENABLED: 'true',
          OPS_METRICS_NODE_ID: 'cvn_aaaaaaaaaaaaaaaa',
          OPS_METRICS_SHARED_SECRET: '0123456789abcdef0123456789abcdef', // pragma: allowlist secret
          OPS_METRICS_ROLE: 'aggregator',
        },
      },
    ])('fails closed for $name', async ({ env }) => {
      await loadConfig(env);
      expect(process.exit).toHaveBeenCalledWith(1);
    });
  });

  // ── freeVideoPublisherCap ───────────────────────────────────────────

  describe('freeVideoPublisherCap', () => {
    it('defaults to 8 when env unset', async () => {
      const { config } = await loadConfig();
      expect(config.freeVideoPublisherCap).toBe(8);
    });

    it('reads a valid positive integer from FREE_VIDEO_PUBLISHER_CAP', async () => {
      const { config } = await loadConfig({ FREE_VIDEO_PUBLISHER_CAP: '12' });
      expect(config.freeVideoPublisherCap).toBe(12);
    });

    it.each([['abc'], ['0'], ['-5'], ['']])('falls back to 8 for invalid value %j', async (raw) => {
      const { config } = await loadConfig({ FREE_VIDEO_PUBLISHER_CAP: raw });
      expect(config.freeVideoPublisherCap).toBe(8);
    });
  });

  // ── freeScreenProducerCap (#1542) ───────────────────────────────────

  describe('freeScreenProducerCap (#1542)', () => {
    it('defaults to 8 when env unset', async () => {
      const { config } = await loadConfig();
      expect(config.freeScreenProducerCap).toBe(8);
    });

    it('reads a valid positive integer from FREE_SCREEN_PRODUCER_CAP', async () => {
      const { config } = await loadConfig({ FREE_SCREEN_PRODUCER_CAP: '2' });
      expect(config.freeScreenProducerCap).toBe(2);
    });

    it.each([['abc'], ['0'], ['-5'], ['']])('falls back to 8 for invalid value %j', async (raw) => {
      const { config } = await loadConfig({ FREE_SCREEN_PRODUCER_CAP: raw });
      expect(config.freeScreenProducerCap).toBe(8);
    });
  });

  // ── audio last-N config (#1544) ─────────────────────────────────────

  describe('audio last-N config (#1544)', () => {
    it('defaults freeAudioLastN to 8', async () => {
      const { config } = await loadConfig();
      expect(config.freeAudioLastN).toBe(8);
    });

    it('defaults audioLastNHoldMs to 2500', async () => {
      const { config } = await loadConfig();
      expect(config.audioLastNHoldMs).toBe(2500);
    });

    it('reads a valid positive integer from FREE_AUDIO_LAST_N', async () => {
      const { config } = await loadConfig({ FREE_AUDIO_LAST_N: '12' });
      expect(config.freeAudioLastN).toBe(12);
    });

    it('clamps an out-of-range AUDIO_LAST_N_HOLD_MS to 10000', async () => {
      const { config } = await loadConfig({ AUDIO_LAST_N_HOLD_MS: '999999' });
      expect(config.audioLastNHoldMs).toBe(10_000);
    });

    it('clamps a too-small AUDIO_LAST_N_HOLD_MS to 500', async () => {
      const { config } = await loadConfig({ AUDIO_LAST_N_HOLD_MS: '100' });
      expect(config.audioLastNHoldMs).toBe(500);
    });
  });

  // ── mediasoup worker count (#2178) ──────────────────────────────────

  describe('mediasoup worker count (#2178)', () => {
    it('defaults to 4 workers when NUM_WORKERS is unset', async () => {
      const { config } = await loadConfig();
      expect(config.mediasoup.numWorkers).toBe(4);
      expect(process.exit).not.toHaveBeenCalled();
    });

    it.each([
      { raw: '1', expected: 1 },
      { raw: '3', expected: 3 },
      // The ceiling itself must be accepted — an off-by-one here would reject a
      // legitimate boundary value.
      { raw: '32', expected: 32 },
      // Pins .trim(): without it this parses as NaN and fatal-exits.
      { raw: '  3  ', expected: 3 },
    ])('accepts NUM_WORKERS=$raw as $expected worker(s)', async ({ raw, expected }) => {
      const { config } = await loadConfig({ NUM_WORKERS: raw });
      expect(config.mediasoup.numWorkers).toBe(expected);
      expect(process.exit).not.toHaveBeenCalled();
    });

    it.each([
      ['0'],
      ['-1'],
      ['abc'],
      [''],
      ['3.5'],
      // The exact example named in the parser's own comment.
      ['4abc'],
      // Whitespace-only must reject, not trim-to-empty-then-default.
      ['   '],
      // Above the sanity ceiling (#2178 review). '64' is the archetypal typo —
      // it forks more subprocesses than the container cgroup can hold, which
      // OOM-loops with no startup signal, unlike the zero case.
      ['33'],
      ['64'],
      ['999999999'],
      // Beyond Number.MAX_SAFE_INTEGER. NOTE: while the ceiling stands, no test
      // can distinguish Number.isSafeInteger from Number.isInteger here — this
      // value is rejected by the ceiling either way. The predicate matters only
      // if the ceiling is ever removed.
      ['9007199254740993'],
    ])('fails closed for invalid NUM_WORKERS %j', async (raw) => {
      await loadConfig({ NUM_WORKERS: raw });
      expect(process.exit).toHaveBeenCalledWith(1);
    });

    it('names the variable and the observed value in the FATAL log', async () => {
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      try {
        await loadConfig({ NUM_WORKERS: '0' });
        expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('FATAL: NUM_WORKERS="0"'));
        expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('integer between 1 and 32'));
      } finally {
        errorSpy.mockRestore();
      }
    });
  });

  // ── Production Guard ────────────────────────────────────────────────

  describe('production guard', () => {
    it('calls process.exit(1) in production with default JWT secret', async () => {
      await loadConfig({ ENVIRONMENT: 'production' });
      expect(process.exit).toHaveBeenCalledWith(1);
    });

    it('does NOT call process.exit with custom JWT secret in production', async () => {
      await loadConfig({ ENVIRONMENT: 'production', ['JWT_' + 'SECRET']: 'my-secure-value' });
      expect(process.exit).not.toHaveBeenCalled();
    });

    it('does NOT call process.exit in development with default secret', async () => {
      await loadConfig({ ENVIRONMENT: 'development' });
      expect(process.exit).not.toHaveBeenCalled();
    });
  });
});
