import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { createHash, createHmac } from 'node:crypto';
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
  releaseDMVoiceAuthorization,
  validateChannelAccess,
  resolveParticipantIdentity,
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
    const callId = '11111111-1111-4111-8111-111111111111';
    const ringId = callId;
    const callerUserId = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          authorized: true,
          is_group: false,
          server_muted: true,
          server_deafened: false,
          call_id: callId,
          call_ring_id: ringId,
          call_caller_user_id: callerUserId,
        }),
    });

    const result = await validateChannelAccess('u-1', 'conv-1', 'jwt-token', 'dm', callId);

    expect(result.allowed).toBe(true);
    expect(result.channelId).toBe('conv-1');
    expect(result.callId).toBe(callId);
    expect(result.callRingId).toBe(ringId);
    expect(result.callCallerUserId).toBe(callerUserId);
    expect(result.serverMuted).toBe(true);
    expect(result.serverDeafened).toBe(false);
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
        body: JSON.stringify({ call_id: callId }),
      })
    );
    const request = mockFetch().mock.calls[0][1] as RequestInit;
    expect(request.signal).toBeInstanceOf(AbortSignal);
    const headers = request.headers as Record<string, string>;
    const timestamp = headers['X-Concord-Media-Timestamp'];
    expect(timestamp).toMatch(/^\d+$/);
    const tokenDigest = createHash('sha256').update('jwt-token').digest('hex');
    const proofKey = createHmac('sha256', TEST_SIGNING_KEY)
      .update('concord/dm-voice-media-authorization/v1')
      .digest();
    const expectedProof = createHmac('sha256', proofKey)
      .update(['v1', timestamp, 'POST', 'conv-1', callId, tokenDigest].join('\n'))
      .digest('hex');
    expect(headers['X-Concord-Media-Proof']).toBe(expectedProof);
  });

  it('binds a DM media-authorization release proof to DELETE', async () => {
    const callId = '11111111-1111-4111-8111-111111111111';
    mockFetch().mockResolvedValueOnce({ ok: true });

    await releaseDMVoiceAuthorization('conv-1', 'jwt-token', callId);

    const [endpoint, request] = mockFetch().mock.calls[0] as [string, RequestInit];
    expect(endpoint).toBe('http://localhost:8080/api/v1/dm/conversations/conv-1/voice/authorize');
    expect(request.method).toBe('DELETE');
    expect(request.signal).toBeInstanceOf(AbortSignal);
    expect(request.body).toBe(JSON.stringify({ call_id: callId }));
    const headers = request.headers as Record<string, string>;
    const timestamp = headers['X-Concord-Media-Timestamp'];
    const tokenDigest = createHash('sha256').update('jwt-token').digest('hex');
    const proofKey = createHmac('sha256', TEST_SIGNING_KEY)
      .update('concord/dm-voice-media-authorization/v1')
      .digest();
    const expectedProof = createHmac('sha256', proofKey)
      .update(['v1', timestamp, 'DELETE', 'conv-1', callId, tokenDigest].join('\n'))
      .digest('hex');
    expect(headers['X-Concord-Media-Proof']).toBe(expectedProof);
  });

  // ── CV-CAN-017: server-authoritative display identity ─────────────────
  it('parses server-authoritative identity from a channel response', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 'srv-1', name: 'General' },
          username: 'realuser',
          display_name: 'Real User',
          avatar_url: '/api/v1/media/avatars/real.png',
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.authIdentityPresent).toBe(true);
    expect(result.authUsername).toBe('realuser');
    expect(result.authDisplayName).toBe('Real User');
    expect(result.authAvatarUrl).toBe('/api/v1/media/avatars/real.png');
  });

  it('treats empty display_name/avatar_url as undefined but keeps authUsername', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 'srv-1', name: 'General' },
          username: 'realuser',
          display_name: '',
          avatar_url: '',
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    // authUsername present ⇒ the control-plane IS identity-aware; the join
    // handler will therefore NOT fall back to the handshake display_name/avatar.
    expect(result.authIdentityPresent).toBe(true);
    expect(result.authUsername).toBe('realuser');
    expect(result.authDisplayName).toBeUndefined();
    expect(result.authAvatarUrl).toBeUndefined();
  });

  it('reports identity present but username undefined for an empty server username (fails closed)', async () => {
    // An identity-aware control-plane that returns an empty username: the
    // response carries a string `username` field, so authIdentityPresent is
    // true even though nonEmpty() maps the value to undefined. This is the
    // signal resolveParticipantIdentity uses to fail CLOSED to the empty
    // authoritative identity instead of the spoofable handshake.
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 'srv-1', name: 'General' },
          username: '',
          display_name: 'Ignored Display',
          avatar_url: '',
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.authIdentityPresent).toBe(true);
    expect(result.authUsername).toBeUndefined();
  });

  it('leaves authUsername undefined for a pre-CV-CAN-017 response (handshake fallback)', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 'srv-1', name: 'General' },
          // no username/display_name/avatar_url — old control-plane build
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    expect(result.authIdentityPresent).toBe(false);
    expect(result.authUsername).toBeUndefined();
    expect(result.authDisplayName).toBeUndefined();
    expect(result.authAvatarUrl).toBeUndefined();
  });

  it('parses server-authoritative identity from a DM response', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          authorized: true,
          is_group: false,
          server_muted: false,
          server_deafened: false,
          username: 'dmuser',
          display_name: 'DM User',
          avatar_url: '/api/v1/media/avatars/dm.png',
        }),
    });

    const result = await validateChannelAccess('u-1', 'conv-1', 'token', 'dm');

    expect(result.authIdentityPresent).toBe(true);
    expect(result.authUsername).toBe('dmuser');
    expect(result.authDisplayName).toBe('DM User');
    expect(result.authAvatarUrl).toBe('/api/v1/media/avatars/dm.png');
  });

  it('fails closed when a DM response omits exact moderation booleans', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({ authorized: true, is_group: false }),
    });

    const result = await validateChannelAccess('u-1', 'conv-1', 'token', 'dm');

    expect(result.allowed).toBe(false);
    expect(result.error).toBe('Invalid DM voice moderation state');
  });

  it.each([
    { label: 'string mute', serverMuted: 'false', serverDeafened: false },
    { label: 'numeric deafen', serverMuted: false, serverDeafened: 0 },
    { label: 'null mute', serverMuted: null, serverDeafened: false },
  ])('fails closed for non-boolean DM moderation values ($label)', async (moderation) => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          authorized: true,
          is_group: false,
          server_muted: moderation.serverMuted,
          server_deafened: moderation.serverDeafened,
        }),
    });

    const result = await validateChannelAccess('u-1', 'conv-1', 'token', 'dm');

    expect(result.allowed).toBe(false);
    expect(result.error).toBe('Invalid DM voice moderation state');
  });

  it('ignores non-string identity fields (fails closed to undefined)', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 'srv-1', name: 'General' },
          username: 42,
          display_name: { nested: 'object' },
          avatar_url: ['array'],
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token');

    // username: 42 is non-string ⇒ NOT identity-aware ⇒ handshake fallback.
    expect(result.authIdentityPresent).toBe(false);
    expect(result.authUsername).toBeUndefined();
    expect(result.authDisplayName).toBeUndefined();
    expect(result.authAvatarUrl).toBeUndefined();
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

  // This parse path went LIVE with the #1542 review reconciliation: the DM
  // authorize endpoint (POST /dm/conversations/:id/voice/authorize) now emits
  // media_entitlements, so the premium userTier asserted here is what feeds
  // Participant.tier for the ADR-0029 DM max-participant-tier room-cap
  // resolution. Regression-lock: do not weaken these assertions.
  it('parses media_entitlements from the DM authorize response (room-kind-independent)', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          authorized: true,
          is_group: false,
          server_muted: false,
          server_deafened: false,
          media_entitlements: premiumEntitlements,
        }),
    });

    const result = await validateChannelAccess('u-1', 'conv-1', 'token', 'dm');

    expect(result.allowed).toBe(true);
    expect(result.userTier).toBe('premium');
    expect(result.allowedAudioTiers).toContain('studio');
    expect(result.minPtimeMs).toBe(10);
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
            allowed_audio_tiers: [
              'minimum',
              'low',
              'moderate',
              'standard',
              'high',
              'hifi',
              'studio',
            ],
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
            allowed_audio_tiers: [
              'minimum',
              'low',
              'moderate',
              'standard',
              'high',
              'hifi',
              'studio',
            ],
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

  // ── Room-owner cap tier (#1542) ────────────────────────────────────────

  it('parses room_owner_tier=premium from a channel join-authorize', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 'srv-1', name: 'v' },
          room_owner_tier: 'premium',
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.roomOwnerTier).toBe('premium');
  });

  it('fail-closes room_owner_tier to "free" when the field is absent', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 'srv-1', name: 'v' },
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.roomOwnerTier).toBe('free');
  });

  it('fail-closes room_owner_tier to "free" on a malformed (non-tier) value', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 'srv-1', name: 'v' },
          room_owner_tier: { sneaky: 'premium' },
        }),
    });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.roomOwnerTier).toBe('free');
  });

  it('leaves roomOwnerTier undefined for DM rooms', async () => {
    mockFetch().mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          authorized: true,
          is_group: true,
          server_muted: false,
          server_deafened: false,
        }),
    });

    const result = await validateChannelAccess('u-1', 'conv-1', 'token', 'dm');

    expect(result.roomOwnerTier).toBeUndefined();
  });

  it('leaves roomOwnerTier undefined on the denial path', async () => {
    mockFetch().mockResolvedValueOnce({ ok: false, status: 403 });

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.roomOwnerTier).toBeUndefined();
  });

  // ── Publish permission bitfield (CV-CAN-007) ───────────────────────────

  const channelJoin = (permissions?: unknown) => ({
    ok: true,
    json: () =>
      Promise.resolve({
        allowed: true,
        server_muted: false,
        server_deafened: false,
        channel: { id: 'ch-1', server_id: 'srv-1', name: 'v' },
        ...(permissions === undefined ? {} : { permissions }),
      }),
  });

  it('parses a decimal permissions string into a bigint bitfield', async () => {
    mockFetch().mockResolvedValueOnce(channelJoin('131072')); // 1<<17 (Speak)

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.permissions).toBe(1n << 17n);
  });

  it('fail-closes permissions to 0n when the field is absent', async () => {
    mockFetch().mockResolvedValueOnce(channelJoin(undefined));

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.permissions).toBe(0n);
  });

  // BigInt() is coercive: without a strict decimal-string guard these inputs
  // would each parse to a real bitfield (or all-bits-set) and grant publish
  // rights, contradicting the fail-closed contract.
  it('fail-closes permissions to 0n on a hex string ("0x20000")', async () => {
    mockFetch().mockResolvedValueOnce(channelJoin('0x20000')); // == 131072

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.permissions).toBe(0n);
  });

  it('fail-closes permissions to 0n on an array value (["131072"])', async () => {
    mockFetch().mockResolvedValueOnce(channelJoin(['131072']));

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.permissions).toBe(0n);
  });

  it('fail-closes permissions to 0n on a numeric value (131072)', async () => {
    mockFetch().mockResolvedValueOnce(channelJoin(131072));

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.permissions).toBe(0n);
  });

  it('fail-closes permissions to 0n on a negative string ("-1")', async () => {
    mockFetch().mockResolvedValueOnce(channelJoin('-1'));

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.permissions).toBe(0n);
  });

  // The control-plane serializes an int64, so a value above 2^63-1 cannot be a
  // valid rbac.Permission. Fail closed rather than honoring its low publish bits.
  it('fail-closes permissions to 0n on an out-of-int64-range decimal ("18446744073709551615")', async () => {
    mockFetch().mockResolvedValueOnce(channelJoin('18446744073709551615')); // 2^64-1

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.permissions).toBe(0n);
  });

  it('accepts the max non-negative int64 permissions value (boundary)', async () => {
    const maxInt64 = ((1n << 63n) - 1n).toString(); // 9223372036854775807
    mockFetch().mockResolvedValueOnce(channelJoin(maxInt64));

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.permissions).toBe((1n << 63n) - 1n);
  });

  it('accepts the Administrator bit (1<<62), within the int64 domain', async () => {
    mockFetch().mockResolvedValueOnce(channelJoin((1n << 62n).toString()));

    const result = await validateChannelAccess('u-1', 'ch-1', 'token', 'channel');

    expect(result.permissions).toBe(1n << 62n);
  });
});

// ---------------------------------------------------------------------------
// resolveParticipantIdentity (CV-CAN-017)
// The load-bearing security decision: prefer server-authoritative identity,
// fall back to the client-supplied handshake identity ONLY when the
// control-plane did not supply it (pre-CV-CAN-017 build).
// ---------------------------------------------------------------------------

describe('resolveParticipantIdentity', () => {
  const spoofedHandshake = {
    username: 'spoofed',
    displayName: 'Spoofed Admin',
    avatarUrl: '/api/v1/media/avatars/spoof.png',
  };

  it('uses server-authoritative identity and ignores the handshake when identity is present', () => {
    const result = resolveParticipantIdentity(
      {
        authIdentityPresent: true,
        authUsername: 'realuser',
        authDisplayName: 'Real User',
        authAvatarUrl: '/api/v1/media/avatars/real.png',
      },
      spoofedHandshake
    );

    expect(result).toEqual({
      username: 'realuser',
      displayName: 'Real User',
      avatarUrl: '/api/v1/media/avatars/real.png',
    });
  });

  it('does NOT fall back to handshake display/avatar when identity is present but they are undefined', () => {
    // A user with no display name / avatar: identity present, the other two
    // undefined. The spoofable handshake values must NOT leak in.
    const result = resolveParticipantIdentity(
      {
        authIdentityPresent: true,
        authUsername: 'realuser',
        authDisplayName: undefined,
        authAvatarUrl: undefined,
      },
      spoofedHandshake
    );

    expect(result.username).toBe('realuser');
    expect(result.displayName).toBeUndefined();
    expect(result.avatarUrl).toBeUndefined();
  });

  it('fails CLOSED to the empty authoritative username when identity is present but the username is empty', () => {
    // An identity-aware control-plane (authIdentityPresent true) that returns an
    // empty username must NOT re-open to the spoofable handshake — every field
    // stays authoritative (empty username, no display/avatar leak).
    const result = resolveParticipantIdentity(
      {
        authIdentityPresent: true,
        authUsername: undefined,
        authDisplayName: undefined,
        authAvatarUrl: undefined,
      },
      spoofedHandshake
    );

    expect(result.username).toBe('');
    expect(result.displayName).toBeUndefined();
    expect(result.avatarUrl).toBeUndefined();
  });

  it('falls back to ALL handshake fields when identity is absent (pre-CV-CAN-017 control-plane)', () => {
    const result = resolveParticipantIdentity(
      {
        authIdentityPresent: false,
        authUsername: undefined,
        authDisplayName: undefined,
        authAvatarUrl: undefined,
      },
      spoofedHandshake
    );

    expect(result).toEqual(spoofedHandshake);
  });
});

// ---------------------------------------------------------------------------
// Service-hop proof on the server-to-server calls
//
// Regression cover for the 2026-09-02 voice outage. The control plane applies
// RequireClientVersion to every route under `authRequired` — which includes both
// voice-join endpoints the media plane re-validates against. That gate reads a
// header the renderer sends and this hop did not, so setting
// CLIENT_MIN_VERSION=0.2.44 made `clientversion.Parse("")` fail and 403'd every
// join. The media plane collapsed that into the same "Not authorized to access
// this channel" string a real RBAC denial produces.
//
// The hop now authenticates itself instead, and the control plane exempts a
// proven hop from that ONE gate. It deliberately does not forward a client
// version: that leg required a client update to work at all (deployed builds
// send nothing), so it fixed the outage for nobody while adding an unvalidated
// client string to an outbound header.
// ---------------------------------------------------------------------------

describe('service-hop proof on control-plane calls', () => {
  const SERVICE_PROOF_CONTEXT = 'concord/media-plane-service-hop/v1';
  const originalFetch = globalThis.fetch;

  beforeEach(() => {
    globalThis.fetch = vi.fn();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  const mockFetch = () => globalThis.fetch as ReturnType<typeof vi.fn>;

  function expectedServiceProof(timestamp: string, method: string, path: string, token: string) {
    const tokenDigest = createHash('sha256').update(token).digest('hex');
    const proofKey = createHmac('sha256', TEST_SIGNING_KEY).update(SERVICE_PROOF_CONTEXT).digest();
    return createHmac('sha256', proofKey)
      .update(['v1', timestamp, method, path, tokenDigest].join('\n'))
      .digest('hex');
  }

  function okChannelResponse() {
    return {
      ok: true,
      json: () =>
        Promise.resolve({
          allowed: true,
          media_server_url: 'http://media',
          server_muted: false,
          server_deafened: false,
          channel: { id: 'ch-1', server_id: 's', name: 'n' },
        }),
    };
  }

  function okDmResponse() {
    return {
      ok: true,
      json: () =>
        Promise.resolve({
          authorized: true,
          is_group: false,
          server_muted: false,
          server_deafened: false,
        }),
    };
  }

  it('binds a service-hop proof to the server-channel join path', async () => {
    mockFetch().mockResolvedValueOnce(okChannelResponse());

    await validateChannelAccess('u-1', 'ch-1', 'jwt-token');

    const headers = (mockFetch().mock.calls[0][1] as RequestInit).headers as Record<string, string>;
    const timestamp = headers['X-Concord-Service-Timestamp'];
    expect(timestamp).toMatch(/^\d+$/);
    expect(headers['X-Concord-Service-Proof']).toBe(
      expectedServiceProof(timestamp, 'POST', '/api/v1/channels/ch-1/voice/join', 'jwt-token')
    );
  });

  it('binds a service-hop proof to the DM authorize path', async () => {
    mockFetch().mockResolvedValueOnce(okDmResponse());

    await validateChannelAccess('u-1', 'conv-1', 'jwt-token', 'dm');

    const headers = (mockFetch().mock.calls[0][1] as RequestInit).headers as Record<string, string>;
    expect(headers['X-Concord-Service-Proof']).toBe(
      expectedServiceProof(
        headers['X-Concord-Service-Timestamp'],
        'POST',
        '/api/v1/dm/conversations/conv-1/voice/authorize',
        'jwt-token'
      )
    );
  });

  it('carries the proof on the DM authorization RELEASE too', async () => {
    // The DELETE runs under the same authRequired chain as the admission hop.
    // A gate rejecting it strands the call lease rather than failing a join, so
    // the failure is quieter and the coverage matters more, not less.
    mockFetch().mockResolvedValueOnce({ ok: true });

    await releaseDMVoiceAuthorization('conv-1', 'jwt-token', '11111111-1111-4111-8111-111111111111');

    const headers = (mockFetch().mock.calls[0][1] as RequestInit).headers as Record<string, string>;
    expect(headers['X-Concord-Service-Proof']).toBe(
      expectedServiceProof(
        headers['X-Concord-Service-Timestamp'],
        'DELETE',
        '/api/v1/dm/conversations/conv-1/voice/authorize',
        'jwt-token'
      )
    );
  });

  it('never forwards a client version — that leg was removed', async () => {
    mockFetch().mockResolvedValueOnce(okChannelResponse());

    await validateChannelAccess('u-1', 'ch-1', 'jwt-token');

    const headers = (mockFetch().mock.calls[0][1] as RequestInit).headers as Record<string, string>;
    expect(headers).not.toHaveProperty('X-Concord-Client-Version');
  });

  it('signs a path with no query string, matching the control plane\'s RequestURI binding', async () => {
    // The control plane verifies against c.Request.URL.RequestURI(), which
    // includes the query string. These calls carry none, so the two coincide —
    // asserted rather than assumed, because a future query parameter here would
    // silently invalidate every proof.
    mockFetch().mockResolvedValueOnce(okChannelResponse());

    await validateChannelAccess('u-1', 'ch-1', 'jwt-token');

    const [endpoint] = mockFetch().mock.calls[0] as [string, RequestInit];
    expect(endpoint).toBe('http://localhost:8080/api/v1/channels/ch-1/voice/join');
    expect(endpoint).not.toContain('?');
  });

  it('keeps the DM endpoint proof alongside the service-hop proof', async () => {
    // The DM-specific proof binds the call ID and gates the media handoff
    // itself; the service-hop proof only asserts "this is the media plane".
    // They answer different questions, so adding one must not drop the other.
    mockFetch().mockResolvedValueOnce(okDmResponse());

    await validateChannelAccess('u-1', 'conv-1', 'jwt-token', 'dm');

    const headers = (mockFetch().mock.calls[0][1] as RequestInit).headers as Record<string, string>;
    expect(headers['X-Concord-Media-Proof']).toMatch(/^[0-9a-f]{64}$/);
    expect(headers['X-Concord-Service-Proof']).toMatch(/^[0-9a-f]{64}$/);
    expect(headers['X-Concord-Media-Proof']).not.toBe(headers['X-Concord-Service-Proof']);
  });
});

// ---------------------------------------------------------------------------
// Denial diagnosability
//
// channelAccessDenial() maps 401 and 403 to one user-facing sentence, and until
// this PR that collapsed string was ALSO all that reached the log — so an
// operator reading media-plane logs saw exactly what the user saw. That is why
// the 2026-09-02 CLIENT_MIN_VERSION rollout was indistinguishable from an RBAC
// denial for hours. These tests pin both halves: the log gains the control
// plane's own status and reason, and the user-facing string does not.
// ---------------------------------------------------------------------------

describe('control-plane denial diagnosability', () => {
  const originalFetch = globalThis.fetch;

  beforeEach(() => {
    globalThis.fetch = vi.fn();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    vi.restoreAllMocks();
  });

  const mockFetch = () => globalThis.fetch as ReturnType<typeof vi.fn>;

  async function denyWith(body: unknown, status = 403) {
    const { logger } = await import('../src/lib/logger.js');
    const warn = vi.spyOn(logger, 'warn').mockImplementation(() => logger);
    mockFetch().mockResolvedValueOnce({
      ok: false,
      status,
      json: () => (body instanceof Error ? Promise.reject(body) : Promise.resolve(body)),
    });
    const result = await validateChannelAccess('u-1', 'ch-1', 'jwt-token');
    // findLast, not find: vi.spyOn returns the SAME spy on repeat calls, so its
    // mock.calls accumulate across denyWith invocations within one test.
    const matching = warn.mock.calls.filter(
      ([msg]) => msg === 'Control plane rejected voice-join re-validation'
    );
    const entry = matching[matching.length - 1];
    return { result, meta: entry?.[1] as Record<string, unknown> | undefined };
  }

  it('logs the control plane status and its structured reason', async () => {
    const { result, meta } = await denyWith({
      code: 'CLIENT_VERSION_TOO_OLD',
      error: 'Client version too old',
    });

    expect(meta).toMatchObject({ status: 403, reason: 'CLIENT_VERSION_TOO_OLD: Client version too old' });
    // The user-facing string must stay status-derived: it must not disclose
    // which gate rejected the caller.
    expect(result.error).toBe('Not authorized to access this channel');
  });

  it('distinguishes a body with no reason from one that could not be read', async () => {
    const { meta: empty } = await denyWith({});
    expect(empty?.reason).toBe('no reason in body');

    const { meta: broken } = await denyWith(new Error('not json'));
    expect(broken?.reason).toBe('unreadable body');
  });

  it('bounds the logged reason so a pathological body cannot blow up log volume', async () => {
    const { meta } = await denyWith({ error: 'x'.repeat(5000) });

    expect((meta?.reason as string).length).toBeLessThanOrEqual(200);
  });
});
