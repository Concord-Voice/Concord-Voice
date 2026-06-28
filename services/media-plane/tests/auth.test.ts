import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import jwt from 'jsonwebtoken';
import './mocks/logger.js';

// Mock config
vi.mock('@/config/index.js', () => ({
  config: {
    jwtSecret: ['vitest', 'mock', 'jwt'].join('-'),
    controlPlaneUrl: 'http://localhost:8080',
  },
}));

const TEST_SIGNING_KEY = ['vitest', 'mock', 'jwt'].join('-'); // NOSONAR — test-only mock

import {
  createAuthMiddleware,
  validateChannelAccess,
  FREE_MEDIA_ENTITLEMENT,
} from '../src/middleware/auth.js';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function createMockSocket(auth: Record<string, unknown> = {}) {
  return {
    id: 'socket-123',
    handshake: {
      auth,
      address: '127.0.0.1',
    },
    data: {} as Record<string, unknown>,
  };
}

function signToken(payload: Record<string, unknown>, options: jwt.SignOptions = {}) {
  return jwt.sign(payload, TEST_SIGNING_KEY, {
    algorithm: 'HS256',
    issuer: 'concordvoice-control-plane',
    expiresIn: '15m',
    ...options,
  });
}

// ---------------------------------------------------------------------------
// createAuthMiddleware
// ---------------------------------------------------------------------------

describe('createAuthMiddleware', () => {
  const middleware = createAuthMiddleware();

  it('rejects when no token is provided', () => {
    const socket = createMockSocket({ username: 'alice' });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith(
      expect.objectContaining({ message: 'Authentication required' })
    );
  });

  it('rejects when token is not a string', () => {
    const socket = createMockSocket({ token: 12345, username: 'alice' });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith(
      expect.objectContaining({ message: 'Authentication required' })
    );
  });

  it('rejects when no username is provided', () => {
    const token = signToken({ user_id: 'u-1' });
    const socket = createMockSocket({ token });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith(expect.objectContaining({ message: 'Username required' }));
  });

  it('rejects when username is not a string', () => {
    const token = signToken({ user_id: 'u-1' });
    const socket = createMockSocket({ token, username: 42 });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith(expect.objectContaining({ message: 'Username required' }));
  });

  it('populates socket.data on valid JWT', () => {
    const token = signToken({ user_id: 'u-1' });
    const socket = createMockSocket({
      token,
      username: 'alice',
      displayName: 'Alice A.',
      avatarUrl: 'https://example.com/avatar.png',
    });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith(); // no error
    expect(socket.data.userId).toBe('u-1');
    expect(socket.data.username).toBe('alice');
    expect(socket.data.displayName).toBe('Alice A.');
    expect(socket.data.avatarUrl).toBe('https://example.com/avatar.png');
  });

  it('populates socket.data.tier from the JWT tier claim', () => {
    const token = signToken({ user_id: 'u-1', tier: 'premium' });
    const socket = createMockSocket({ token, username: 'alice' });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith(); // no error
    expect(socket.data.tier).toBe('premium');
  });

  it('defaults socket.data.tier to free when the tier claim is absent', () => {
    const token = signToken({ user_id: 'u-1' });
    const socket = createMockSocket({ token, username: 'alice' });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith();
    expect(socket.data.tier).toBe('free');
  });

  it('defaults socket.data.tier to free when the tier claim is blank', () => {
    const token = signToken({ user_id: 'u-1', tier: '' });
    const socket = createMockSocket({ token, username: 'alice' });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith();
    expect(socket.data.tier).toBe('free');
  });

  it('ignores non-string displayName and avatarUrl', () => {
    const token = signToken({ user_id: 'u-1' });
    const socket = createMockSocket({
      token,
      username: 'alice',
      displayName: 999,
      avatarUrl: true,
    });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith();
    expect(socket.data.displayName).toBeUndefined();
    expect(socket.data.avatarUrl).toBeUndefined();
  });

  it('rejects expired tokens', () => {
    const token = signToken({ user_id: 'u-1' }, { expiresIn: '-1s' });
    const socket = createMockSocket({ token, username: 'alice' });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith(expect.objectContaining({ message: 'Token expired' }));
  });

  it('rejects tokens with invalid signature', () => {
    const token = jwt.sign({ user_id: 'u-1' }, ['wrong', 'key'].join('-'), {
      algorithm: 'HS256',
      issuer: 'concordvoice-control-plane',
    });
    const socket = createMockSocket({ token, username: 'alice' });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith(expect.objectContaining({ message: 'Invalid token' }));
  });

  it('rejects token missing user_id claim', () => {
    const token = signToken({ sub: 'u-1' }); // user_id not present
    const socket = createMockSocket({ token, username: 'alice' });
    const next = vi.fn();

    middleware(socket as any, next);

    expect(next).toHaveBeenCalledWith(
      expect.objectContaining({ message: 'Invalid token: missing user_id' })
    );
  });
});

// ---------------------------------------------------------------------------
// validateChannelAccess
// ---------------------------------------------------------------------------

describe('validateChannelAccess', () => {
  const originalFetch = globalThis.fetch;

  beforeEach(() => {
    globalThis.fetch = vi.fn();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  const mockFetch = () => globalThis.fetch as ReturnType<typeof vi.fn>;

  it('returns allowed=true for a valid voice channel', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: {
            id: 'ch-1',
            server_id: 'srv-1',
            name: 'General',
          },
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(true);
    expect(result.channelId).toBe('ch-1');
    expect(result.serverId).toBe('srv-1');
    expect(result.channelName).toBe('General');
  });

  it('passes Authorization header with bearer token', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
        }),
    });

    await validateChannelAccess('u-1', 'ch-1', 'my-jwt-token');

    expect(mockFetch()).toHaveBeenCalledWith(
      'http://localhost:8080/api/v1/channels/ch-1/voice/join',
      expect.objectContaining({
        method: 'POST',
        headers: expect.objectContaining({
          Authorization: 'Bearer my-jwt-token',
        }),
      })
    );
  });

  it('returns denied for 401 response', async () => {
    mockFetch().mockResolvedValueOnce({ ok: false, status: 401 });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(false);
    expect(result.error).toBe('Not authorized to access this channel');
  });

  it('returns denied for 403 response', async () => {
    mockFetch().mockResolvedValueOnce({ ok: false, status: 403 });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(false);
    expect(result.error).toBe('Not authorized to access this channel');
  });

  it('returns not found for 404 response', async () => {
    mockFetch().mockResolvedValueOnce({ ok: false, status: 404 });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(false);
    expect(result.error).toBe('Channel not found');
  });

  it('returns error for 500 response', async () => {
    mockFetch().mockResolvedValueOnce({ ok: false, status: 500 });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(false);
    expect(result.error).toBe('Control plane returned 500');
  });

  it('rejects non-voice channel type (control plane returns 400)', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: false,
      status: 400,
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(false);
    expect(result.error).toBe('Control plane returned 400');
  });

  it('returns denied on network error (fetch throws)', async () => {
    mockFetch().mockRejectedValueOnce(new Error('ECONNREFUSED'));

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(false);
    expect(result.error).toBe('Failed to validate channel access');
  });

  it('defaults serverMuted and serverDeafened to false when not present in response', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(true);
    expect(result.serverMuted).toBe(false);
    expect(result.serverDeafened).toBe(false);
  });

  it('returns serverMuted and serverDeafened when present in response', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: true,
          server_deafened: true,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(true);
    expect(result.serverMuted).toBe(true);
    expect(result.serverDeafened).toBe(true);
  });

  // ── DM-path tests (#1209, plan task C1 / G7 fix) ──────────────────────

  it('routes to DM authorize endpoint when roomKind=dm', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({ authorized: true, is_group: false }),
    });

    const result = await validateChannelAccess('u-1', 'conv-1', 'jwt-token', 'dm');

    expect(result.allowed).toBe(true);
    expect(result.channelId).toBe('conv-1');
    // DM rooms don't carry server-channel metadata
    expect(result.serverId).toBe('');
    expect(result.channelName).toBe('');
    expect(mockFetch()).toHaveBeenCalledWith(
      'http://localhost:8080/api/v1/dm/conversations/conv-1/voice/authorize',
      expect.objectContaining({
        method: 'POST',
        headers: expect.objectContaining({
          Authorization: 'Bearer jwt-token',
        }),
      })
    );
  });

  it('routes to server-channel endpoint when roomKind omitted (backward compat)', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
        }),
    });

    await validateChannelAccess('u-1', 'ch-1', 'token'); // no roomKind arg

    expect(mockFetch()).toHaveBeenCalledWith(
      'http://localhost:8080/api/v1/channels/ch-1/voice/join',
      expect.anything()
    );
  });

  it('returns denied for 403 from DM authorize endpoint (non-member)', async () => {
    mockFetch().mockResolvedValueOnce({ ok: false, status: 403 });

    const result = await validateChannelAccess('outsider-id', 'conv-1', 'token', 'dm');

    expect(result.allowed).toBe(false);
    expect(result.error).toBe('Not authorized to access this channel');
  });

  it('returns denied when DM authorize endpoint returns 200 but authorized=false', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({ authorized: false, is_group: false }),
    });

    const result = await validateChannelAccess('outsider-id', 'conv-1', 'token', 'dm');

    expect(result.allowed).toBe(false);
  });

  // ── Per-user media entitlements (#1300) ───────────────────────────────

  const premiumEntitlements = {
    tier: 'premium',
    allowed_audio_tiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
    min_ptime_ms: 10,
    max_manual_bitrate_bps: 10_000_000,
  };

  const freeEntitlements = {
    tier: 'free',
    allowed_audio_tiers: ['minimum', 'low', 'moderate', 'standard'],
    min_ptime_ms: 20,
    max_manual_bitrate_bps: 5_000_000,
  };

  it('parses free media_entitlements from the server-channel join response', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
          media_entitlements: freeEntitlements,
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.userTier).toBe('free');
    expect(result.allowedAudioTiers).toEqual(['minimum', 'low', 'moderate', 'standard']);
    expect(result.minPtimeMs).toBe(20);
    expect(result.maxManualBitrateBps).toBe(5_000_000);
  });

  it('parses premium media_entitlements from the server-channel join response', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
          media_entitlements: premiumEntitlements,
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.userTier).toBe('premium');
    expect(result.allowedAudioTiers).toContain('studio');
    expect(result.minPtimeMs).toBe(10);
    expect(result.maxManualBitrateBps).toBe(10_000_000);
  });

  it('parses media_entitlements from the DM authorize response (room-kind-independent)', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          authorized: true,
          is_group: false,
          media_entitlements: premiumEntitlements,
        }),
    });

    const result = await validateChannelAccess('u-1', 'conv-1', 'token', 'dm');

    expect(result.allowed).toBe(true);
    expect(result.userTier).toBe('premium');
    expect(result.maxManualBitrateBps).toBe(10_000_000);
  });

  it('fails closed to the free floor when media_entitlements is ABSENT', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
          // media_entitlements intentionally omitted
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.userTier).toBe(FREE_MEDIA_ENTITLEMENT.tier);
    expect(result.allowedAudioTiers).toEqual(FREE_MEDIA_ENTITLEMENT.allowedAudioTiers);
    expect(result.minPtimeMs).toBe(FREE_MEDIA_ENTITLEMENT.minPtimeMs);
    expect(result.maxManualBitrateBps).toBe(FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps);
  });

  it('fails closed to the free floor when media_entitlements is MALFORMED (wrong types)', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
          media_entitlements: {
            tier: 42, // wrong type
            allowed_audio_tiers: 'not-an-array', // wrong type
            min_ptime_ms: 'fast', // wrong type
            max_manual_bitrate_bps: -1, // out of range
          },
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    // Each field independently floors — a partial-malformed object never
    // partially escalates above the free values.
    expect(result.userTier).toBe(FREE_MEDIA_ENTITLEMENT.tier);
    expect(result.allowedAudioTiers).toEqual(FREE_MEDIA_ENTITLEMENT.allowedAudioTiers);
    expect(result.minPtimeMs).toBe(FREE_MEDIA_ENTITLEMENT.minPtimeMs);
    expect(result.maxManualBitrateBps).toBe(FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps);
  });

  it('floors only the malformed FIELDS, keeping valid ones (no all-or-nothing)', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
          media_entitlements: {
            tier: 'premium',
            allowed_audio_tiers: [], // empty array → floor
            min_ptime_ms: 10, // valid
            max_manual_bitrate_bps: 0, // invalid (must be >0) → floor
          },
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.userTier).toBe('premium'); // valid, kept
    expect(result.minPtimeMs).toBe(10); // valid, kept
    expect(result.allowedAudioTiers).toEqual(FREE_MEDIA_ENTITLEMENT.allowedAudioTiers); // floored
    expect(result.maxManualBitrateBps).toBe(FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps); // floored
  });

  it('clamps a FREE tier carrying premium-shaped caps to the free floor (atomic-free defence-in-depth)', async () => {
    // Cross-field inconsistency (client-unreachable, but possible via a CP bug
    // or a downgrade race): tier=free yet valid premium caps. Caps are tied to
    // the tier — only an explicit premium tier may carry premium values, so a
    // free tier can never end up with a premium cap.
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
          media_entitlements: {
            tier: 'free',
            allowed_audio_tiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
            min_ptime_ms: 10, // premium-shaped (lower) → must clamp UP to 20
            max_manual_bitrate_bps: 10_000_000, // premium-shaped → must clamp DOWN to 5M
          },
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.userTier).toBe('free');
    expect(result.allowedAudioTiers).toEqual(FREE_MEDIA_ENTITLEMENT.allowedAudioTiers);
    expect(result.minPtimeMs).toBe(FREE_MEDIA_ENTITLEMENT.minPtimeMs); // 20, not 10
    expect(result.maxManualBitrateBps).toBe(FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps); // 5M, not 10M
  });

  it('allows server-authoritative channel audio uplift for free users without raising manual bitrate', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
          media_entitlements: {
            tier: 'free',
            channel_audio_uplift: true,
            allowed_audio_tiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
            min_ptime_ms: 10,
            max_manual_bitrate_bps: 10_000_000,
          },
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.userTier).toBe('free');
    expect(result.allowedAudioTiers).toContain('studio');
    expect(result.minPtimeMs).toBe(10);
    expect(result.maxManualBitrateBps).toBe(FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps);
  });

  it('denial paths carry the free-floor media entitlement (never escalate on denial)', async () => {
    mockFetch().mockResolvedValueOnce({ ok: false, status: 403 });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.allowed).toBe(false);
    expect(result.userTier).toBe(FREE_MEDIA_ENTITLEMENT.tier);
    expect(result.maxManualBitrateBps).toBe(FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps);
    expect(result.minPtimeMs).toBe(FREE_MEDIA_ENTITLEMENT.minPtimeMs);
  });
});
