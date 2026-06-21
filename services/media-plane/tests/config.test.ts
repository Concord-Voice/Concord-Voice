import { describe, it, expect, vi, beforeEach } from 'vitest';

// Mock mediasoup types import (used by config module)
vi.mock('mediasoup/node/lib/rtpParametersTypes.js', () => ({}));

// Silence dotenv
vi.mock('dotenv', () => ({
  default: { config: vi.fn() },
}));

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
      expect(codecs.filter((c: any) => c.kind === 'audio')).toHaveLength(1);
      expect(codecs.filter((c: any) => c.kind === 'video')).toHaveLength(7);
    });

    it('audio codec is opus at 48kHz stereo', async () => {
      const { config } = await loadConfig();
      const audio = config.mediasoup.router.mediaCodecs.find((c: any) => c.kind === 'audio');
      expect(audio).toBeDefined();
      expect(audio.mimeType).toBe('audio/opus');
      expect(audio.clockRate).toBe(48000);
      expect(audio.channels).toBe(2);
    });

    it('all video codecs have rtcpFeedback', async () => {
      const { config } = await loadConfig();
      const videoCodecs = config.mediasoup.router.mediaCodecs.filter(
        (c: any) => c.kind === 'video'
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

    it('applies NUM_WORKERS override', async () => {
      const { config } = await loadConfig({ NUM_WORKERS: '8' });
      expect(config.mediasoup.numWorkers).toBe(8);
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
