import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';

vi.mock('@/config/index.js', () => ({
  config: {
    jwtSecret: ['vitest', 'mock', 'jwt'].join('-'),
    controlPlaneUrl: 'http://localhost:8080',
  },
}));

import jwt from 'jsonwebtoken';
import type { Socket } from 'socket.io';
import {
  attestationRelayHeaders,
  bearerAttestationHeaders,
  releaseDMVoiceAuthorization,
  updateAttestationRelay,
  validateChannelAccess,
} from '../src/middleware/auth.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import type { VoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';

// #4206: RequireAttestation gates every authenticated control-plane route, so
// each media-plane hop carrying a user's bearer must also carry that user's
// own attestation triple. Cross-user rejection is the control plane's half and
// is pinned in middleware/attestation_relay_test.go.
const attestationToken = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const sessionId = '0a1b2c3d-4e5f-4a6b-8c7d-8e9fa0b1c2d3';
const otherSessionId = '0b2c3d4e-5f60-4b7c-8d9e-0f1a2b3c4d5e';
const machineId = '9e8d7c6b-5a49-4382-9716-05f4e3d2c1b0';
const bearer = (sid: string) => jwt.sign({ user_id: 'u-1', sid }, ['vitest', 'mock', 'jwt'].join('-'),
  { algorithm: 'HS256', issuer: 'concordvoice-control-plane', expiresIn: '1h' });
const token = bearer(sessionId);
// session_id here is another of the user's sessions: it must never be relayed.
const handshake = { token, attestation_token: attestationToken, session_id: otherSessionId, machine_id: machineId };
const relayed = { 'X-Attestation-Token': attestationToken, 'X-Machine-Id': machineId };
const sent = { ...relayed, 'X-Session-ID': sessionId };

describe('attestationRelayHeaders', () => {
  it('maps the handshake token and machine id, never the client session id', () => {
    expect(attestationRelayHeaders(handshake)).toEqual(relayed);
  });

  it('drops anything that is not a UUID, including header injection', () => {
    expect(
      attestationRelayHeaders({
        attestation_token: `${attestationToken}\r\nX-Evil: 1`,
        session_id: 42,
        machine_id: 'not-a-uuid',
      })
    ).toEqual({});
  });

  it('warns when a token arrives without a usable machine id', async () => {
    const { logger } = await import('../src/lib/logger.js');
    vi.mocked(logger.warn).mockClear();
    expect(attestationRelayHeaders({ attestation_token: attestationToken, machine_id: 'nope' })).toEqual({
      'X-Attestation-Token': attestationToken,
    });
    expect(logger.warn).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({ event: 'attestation.relay_machine_id_unreadable' })
    );
    vi.mocked(logger.warn).mockClear();
    attestationRelayHeaders(handshake);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('returns no headers for an absent or non-object handshake', () => {
    expect(attestationRelayHeaders(undefined)).toEqual({});
    expect(attestationRelayHeaders('token')).toEqual({});
    expect(attestationRelayHeaders({})).toEqual({});
  });
});

describe('bearerAttestationHeaders', () => {
  it('takes the session id from the verified bearer, replacing any supplied one', () => {
    expect(bearerAttestationHeaders({ ...relayed, 'X-Session-ID': otherSessionId }, token)).toEqual(sent);
  });

  it('follows a refreshed bearer to its rotated session', () => {
    expect(bearerAttestationHeaders(relayed, bearer(otherSessionId))['X-Session-ID']).toBe(otherSessionId);
  });

  it('adds no session id for a bearer that does not verify', () => {
    const forged = jwt.sign({ user_id: 'u-1', sid: sessionId }, ['not', 'the', 'secret'].join('-'),
      { algorithm: 'HS256', issuer: 'concordvoice-control-plane', expiresIn: '1h' });
    expect(bearerAttestationHeaders(relayed, forged)).toEqual(relayed);
  });

  it('adds no session id for a verified bearer that carries no sid', () => {
    const noSid = jwt.sign({ user_id: 'u-1' }, ['vitest', 'mock', 'jwt'].join('-'),
      { algorithm: 'HS256', issuer: 'concordvoice-control-plane', expiresIn: '1h' });
    expect(bearerAttestationHeaders(relayed, noSid)).toEqual(relayed);
  });

  it('adds nothing when no attestation token is relayed', () => {
    expect(bearerAttestationHeaders({}, token)).toEqual({});
  });
});

describe('updateAttestationRelay', () => {
  it('replaces the relayed token with a re-minted one and ignores junk', () => {
    const socket = { handshake: { auth: { ...handshake } } } as unknown as Socket;
    const fresh = '11111111-2222-4333-8444-555555555555';
    updateAttestationRelay(socket, fresh);
    expect(socket.handshake.auth.attestation_token).toBe(fresh);
    updateAttestationRelay(socket, 'garbage');
    updateAttestationRelay(socket, { token: fresh });
    expect(socket.handshake.auth.attestation_token).toBe(fresh);
    updateAttestationRelay(socket, null);
    expect(socket.handshake.auth).not.toHaveProperty('attestation_token');
  });
});

describe('relay on the admission and release hops', () => {
  const originalFetch = globalThis.fetch;
  const ok = (body: unknown) => ({ ok: true, json: () => Promise.resolve(body) });
  const sentHeaders = (call = 0) =>
    (vi.mocked(globalThis.fetch).mock.calls[call][1] as RequestInit).headers as Record<string, string>;

  beforeEach(() => {
    globalThis.fetch = vi.fn();
  });
  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('relays on the server-channel join', async () => {
    vi.mocked(globalThis.fetch).mockResolvedValueOnce(
      ok({ allowed: true, channel: { id: 'ch-1', server_id: 's', name: 'n' } }) as Response
    );
    await validateChannelAccess('u-1', 'ch-1', token, 'channel', undefined, undefined, relayed);
    expect(sentHeaders()).toMatchObject({ ...sent, Authorization: `Bearer ${token}` });
  });

  it('relays on the DM authorize and its release', async () => {
    vi.mocked(globalThis.fetch)
      .mockResolvedValueOnce(ok({ authorized: true, is_group: false }) as Response)
      .mockResolvedValueOnce({ ok: true } as Response);
    await validateChannelAccess('u-1', 'conv-1', token, 'dm', undefined, undefined, relayed);
    await releaseDMVoiceAuthorization('conv-1', token, '11111111-1111-4111-8111-111111111111', relayed);
    expect(sentHeaders(0)).toMatchObject(sent);
    expect(sentHeaders(1)).toMatchObject(sent);
  });

  it('sends no attestation headers when the client supplied none', async () => {
    vi.mocked(globalThis.fetch).mockResolvedValueOnce(
      ok({ allowed: true, channel: { id: 'ch-1', server_id: 's', name: 'n' } }) as Response
    );
    await validateChannelAccess('u-1', 'ch-1', 'jwt');
    expect(sentHeaders()).not.toHaveProperty('X-Attestation-Token');
  });
});

describe('relay on the signed voice-auth hops', () => {
  const userId = '11111111-1111-4111-8111-111111111111';
  const familyId = '22222222-2222-4222-8222-222222222222';
  const nodeBootId = '33333333-3333-4333-8333-333333333333';
  const secret = ['relay', 'test', 'only'].join('-');
  const now = 1_800_000_000_000;
  const session: VoiceEnforcementSession = { sessionGeneration: '55555555-5555-4555-8555-555555555555',
    nodeBootId, roomId: '44444444-4444-4444-8444-444444444444', roomKind: 'channel', userId,
    credentialEpoch: 'a'.repeat(32), socketId: 'socket-1' };

  function fixture(relay: () => Record<string, string>) {
    const sent: { endpoint: protocol.Endpoint; headers: Headers }[] = [];
    const fetchBoundary = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const path = new URL(String(input)).pathname;
      const endpoint: protocol.Endpoint = path.endsWith('/auth-connections') ? 'connection_register'
        : path.endsWith('/auth-connections/release') ? 'connection_release'
        : path.endsWith('/enforcement-sessions/release') ? 'room_release' : 'room_register';
      const headers = new Headers(init?.headers);
      sent.push({ endpoint, headers });
      // The extra headers must not disturb the signed protocol's decoding.
      const request = protocol.decodeHttpRequest(endpoint, {
        method: 'POST', path, body: Buffer.from(init?.body as Uint8Array),
        headers: Array.from(headers, ([name, value]) => ({ name, value })),
      });
      protocol.verifyHttpRequest(request, Buffer.from(secret), BigInt(now / 1000));
      const registration = request.connectionRegister();
      const room = request.roomRegister();
      const target = registration ?? room;
      const binding = target ? { userId, familyId, target: {
        connectionGeneration: target.connectionGeneration, nodeBootId: target.nodeBootId,
        socketId: target.socketId, credentialEpoch: target.credentialEpoch } } : undefined;
      const payload: protocol.ResponsePayload = registration && binding
        ? { ...binding, version: 1, voiceEnforcementProtocol: 4 }
        : room && binding ? { schema: 'concord.auth-room-binding', version: 1, voiceEnforcementProtocol: 4,
          binding, room: { sessionGeneration: room.sessionGeneration, roomId: room.roomId, roomKind: room.roomKind } }
        : null;
      const signed = protocol.signHttpResponse(request, payload, String(now / 1000), Buffer.from(secret));
      return new Response(signed.body.length ? Buffer.from(signed.body) : null, {
        status: signed.status, headers: signed.headers.map(({ name, value }) => [name, value]),
      });
    });
    const owners = new VoiceAuthConnections({ nodeBootId, secret, controlPlaneUrl: 'http://control.test',
      fetch: fetchBoundary as typeof fetch, now: () => now });
    const handle = owners.reserve({ socketId: 'socket-1', userId, credentialEpoch: 'a'.repeat(32),
      token, tokenExpiresAt: now + 60_000, disconnect: vi.fn(), relayHeaders: relay });
    return { owners, handle, sent };
  }

  it('relays the current triple on every bearer hop and never on a service-signed one', async () => {
    let current: Record<string, string> = {};
    const f = fixture(() => current);
    // Read at send time, so a refresh-attestation update reaches the next hop.
    current = relayed;
    await f.owners.register(f.handle);
    f.owners.reserveRoom(f.handle, session, async () => undefined);
    await f.owners.attachRoom(f.handle, session, token);
    const reminted = '66666666-6666-4666-8666-666666666666';
    current = { ...relayed, 'X-Attestation-Token': reminted };
    // A refresh that rotates the session moves the lookup with the bearer.
    await f.owners.renewRoom(f.handle, session, bearer(otherSessionId), () => true);
    await f.owners.releaseRoom(session);
    await f.owners.terminate(f.handle);

    const tokens = (endpoint: protocol.Endpoint) =>
      f.sent.filter((s) => s.endpoint === endpoint).map((s) => s.headers.get('X-Attestation-Token'));
    expect(tokens('connection_register')).toEqual([attestationToken]);
    expect(tokens('room_register')).toEqual([attestationToken, reminted]);
    expect(tokens('room_release')).toEqual([null]);
    expect(tokens('connection_release')).toEqual([null]);
    const sessions = (endpoint: protocol.Endpoint) =>
      f.sent.filter((s) => s.endpoint === endpoint).map((s) => s.headers.get('X-Session-ID'));
    expect(sessions('connection_register')).toEqual([sessionId]);
    expect(sessions('room_register')).toEqual([sessionId, otherSessionId]);
    const register = f.sent.find((s) => s.endpoint === 'connection_register');
    expect(register?.headers.get('X-Machine-Id')).toBe(machineId);
  });
});
