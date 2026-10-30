import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../src/config/index.js', () => ({ config: {
  jwtSecret: 'voice-owner-test-only-key', controlPlaneUrl: 'http://control-plane.test',
} }));
import './mocks/logger.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';

import {
  pollVoiceEnforcementHealth,
  registerVoiceEnforcementSession,
  releaseVoiceEnforcementSession,
  type VoiceEnforcementSession,
} from '../src/lib/voiceEnforcementSession.js';

const session: VoiceEnforcementSession = {
  sessionGeneration: '55555555-5555-4555-8555-555555555555',
  nodeBootId: '33333333-3333-4333-8333-333333333333',
  roomId: '44444444-4444-4444-8444-444444444444',
  roomKind: 'channel',
  userId: '11111111-1111-4111-8111-111111111111',
  credentialEpoch: 'a'.repeat(32),
  socketId: 'socket-a',
};
const nativeVerifier = vi.fn(async (_session: VoiceEnforcementSession): Promise<void> => undefined);

function response(ok: boolean, status = ok ? 200 : 409): Response {
  return { ok, status } as Response;
}

describe('voice enforcement session control-plane calls', () => {
  afterEach(() => vi.unstubAllGlobals());
  beforeEach(() => {
    vi.restoreAllMocks();
    nativeVerifier.mockReset().mockResolvedValue(undefined);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(true)));
  });

  it('registers and releases through the actual retained private owner', async () => {
    const f = await ownedFixture();
    await f.api.registerVoiceEnforcementSession('test-original-token', f.session);
    expect(f.requests[1].roomRegister()).toEqual({
      schema: 'concord.auth-room-binding', version: 1, mode: 'attach',
      ...f.owners.snapshot(f.owner).target,
      sessionGeneration: session.sessionGeneration, roomId: session.roomId, roomKind: session.roomKind,
    });
    expect(f.authorizations[1]).toBe('Bearer test-original-token');
    const legacyVerifier = vi.fn(async () => undefined);
    await f.api.releaseVoiceEnforcementSession(f.session, legacyVerifier);
    expect(f.closeNative).toHaveBeenCalledOnce();
    expect(legacyVerifier).not.toHaveBeenCalled();
    expect(f.requests[2].roomRelease()).toEqual({
      schema: 'concord.auth-room-binding', version: 1,
      binding: f.owners.snapshot(f.owner).binding,
      room: { sessionGeneration: session.sessionGeneration, roomId: session.roomId, roomKind: session.roomKind },
    });
    expect(f.authorizations[2]).toBeNull();
    await f.owners.terminate(f.owner);
  });

  it('rejects unsigned owner-backed registration success at the response proof boundary', async () => {
    const f = await ownedFixture();
    f.unsigned.value = true;
    await expect(f.api.registerVoiceEnforcementSession('test-original-token', f.session))
      .rejects.toMatchObject({ reason: 'invalid_headers' });
    expect(f.requests[1].roomRegister()?.mode).toBe('attach');
    f.unsigned.value = false;
    await f.owners.terminate(f.owner);
    expect(f.closeNative).toHaveBeenCalledOnce();
  });

  it('refuses registration without a private connection owner before HTTP', async () => {
    await expect(registerVoiceEnforcementSession('test-original-token', session))
      .rejects.toThrow('Voice connection owner required');
    expect(fetch).not.toHaveBeenCalled();
  });

  it('releases a genuinely unbound legacy exact session without using the user JWT', async () => {
    await releaseVoiceEnforcementSession(session, nativeVerifier);

    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe('http://control-plane.test/api/v1/internal/voice/enforcement-sessions/release');
    expect(init?.headers).not.toHaveProperty('Authorization');
    expect(JSON.parse(String(init?.body))).toEqual({
      session_generation: session.sessionGeneration,
      node_boot_id: session.nodeBootId,
      room_id: session.roomId,
      room_kind: session.roomKind,
      user_id: session.userId,
      credential_epoch: session.credentialEpoch,
      socket_id: session.socketId,
    });
  });

  it('rejects failed release and refuses newline-delimited session fields', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response(false, 503));
    await expect(releaseVoiceEnforcementSession(session, nativeVerifier)).rejects.toThrow('release (503)');

    await expect(
      releaseVoiceEnforcementSession({ ...session, userId: 'user\n-a' }, nativeVerifier)
    ).rejects.toThrow('Unable to sign voice-enforcement release');
    expect(fetch).toHaveBeenCalledOnce();
  });

  it('waits for native proof before sending the release HTTP request', async () => {
    let completeProof!: () => void;
    const proof = new Promise<void>((resolve) => { completeProof = resolve; });
    const verify = vi.fn(() => proof);
    let settled = false;
    const release = releaseVoiceEnforcementSession(session, verify);
    void release.then(() => { settled = true; }, () => { settled = true; });
    try {
      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(fetch).not.toHaveBeenCalled();
      expect(settled).toBe(false);
      expect(verify).toHaveBeenCalledWith(session);
      completeProof();
      await release;
      expect(fetch).toHaveBeenCalledOnce();
    } finally {
      completeProof();
      await release.catch(() => undefined);
    }
  });

  it('retains the release obligation without HTTP when native proof fails', async () => {
    const verify = vi.fn().mockRejectedValue(new Error('native proof unavailable'));
    await expect(releaseVoiceEnforcementSession(session, verify)).rejects.toThrow('native proof unavailable');
    expect(fetch).not.toHaveBeenCalled();
    expect(verify).toHaveBeenCalledWith(session);
  });

  it.each([false, true])('polls strictly signed protocol-4 health with admissionGranted=%s', async admissionGranted => {
    const nonces: string[] = [];
    vi.mocked(fetch).mockImplementation(async (_url, init) => {
      const request = decode('health_bootstrap', '/api/v1/internal/voice/enforcement/health', init);
      nonces.push(request.healthBootstrap()!.nonce);
      return signed(request, { version: 1, voiceEnforcementProtocol: 4,
        nodeBootId: session.nodeBootId, activation: admissionGranted ? 'active' : 'prepared',
        transportOK: true, admissionGranted });
    });
    const expected = { voiceEnforcementProtocol: 4, transportOK: true, admissionGranted };
    await expect(pollVoiceEnforcementHealth(session.nodeBootId)).resolves.toMatchObject(expected);
    await expect(pollVoiceEnforcementHealth(session.nodeBootId)).resolves.toMatchObject(expected);
    expect(nonces).toHaveLength(2);
    expect(nonces[0]).toMatch(/^[0-9a-f]{64}$/);
    expect(nonces[1]).not.toBe(nonces[0]);
    vi.mocked(fetch).mockResolvedValueOnce(new Response(JSON.stringify({ version: 1, voiceEnforcementProtocol: 4, nodeBootId: session.nodeBootId,
      activation: 'prepared', transportOK: true, admissionGranted: false }), { status: 200 }));
    await expect(pollVoiceEnforcementHealth(session.nodeBootId)).rejects.toMatchObject({ reason: 'invalid_headers' });
    vi.mocked(fetch).mockResolvedValueOnce(new Response(null, { status: 401 }));
    await expect(pollVoiceEnforcementHealth(session.nodeBootId)).rejects.toMatchObject({ reason: 'unexpected_status' });
  });
});

const key = Buffer.from('voice-owner-test-only-key');
function decode(endpoint: protocol.Endpoint, path: string, init?: RequestInit) {
  const raw = init?.body;
  if (typeof raw !== 'string' && !(raw instanceof Uint8Array)) throw new Error('Missing exact request bytes');
  const request = protocol.decodeHttpRequest(endpoint, { method: init?.method ?? '', path,
    headers: Array.from(new Headers(init?.headers), ([name, value]) => ({ name, value })), body: Buffer.from(raw) });
  protocol.verifyHttpRequest(request, key, BigInt(Math.floor(Date.now() / 1000)));
  return request;
}
function signed(request: protocol.Request, payload: protocol.ResponsePayload, unsigned = false) {
  const wire = protocol.signHttpResponse(request, payload, String(Math.floor(Date.now() / 1000)), key);
  return new Response(wire.status === 204 ? null : Buffer.from(wire.body), {
    status: wire.status, headers: unsigned ? [] : wire.headers.map(({ name, value }) => [name, value]),
  });
}
async function ownedFixture() {
  vi.resetModules();
  const api = await import('../src/lib/voiceEnforcementSession.js');
  const requests: protocol.Request[] = [];
  const authorizations: (string | null)[] = [];
  const unsigned = { value: false };
  const closeNative = vi.fn(async () => undefined);
  const owners = new VoiceAuthConnections({ nodeBootId: session.nodeBootId,
    secret: key.toString(), controlPlaneUrl: 'http://control-plane.test',
    fetch: vi.fn<typeof fetch>(async (input, init) => {
      const path = new URL(String(input)).pathname;
      const endpoint = path.endsWith('/auth-connections') ? 'connection_register'
        : path.endsWith('/auth-connections/release') ? 'connection_release'
          : path.endsWith('/enforcement-sessions/release') ? 'room_release' : 'room_register';
      const request = decode(endpoint, path, init);
      requests.push(request);
      authorizations.push(new Headers(init?.headers).get('Authorization'));
      const target = request.connectionRegister() ?? request.roomRegister();
      const binding = target && { userId: session.userId, familyId: '22222222-2222-4222-8222-222222222222',
        target: { connectionGeneration: target.connectionGeneration, nodeBootId: target.nodeBootId,
          socketId: target.socketId, credentialEpoch: target.credentialEpoch } };
      return signed(request, request.connectionRegister() && binding
        ? { ...binding, version: 1, voiceEnforcementProtocol: 4 }
        : request.roomRegister() && binding ? { schema: 'concord.auth-room-binding', version: 1,
          voiceEnforcementProtocol: 4, binding, room: { sessionGeneration: session.sessionGeneration,
            roomId: session.roomId, roomKind: session.roomKind } } : null, unsigned.value);
    }),
  });
  api.setVoiceAuthConnections(owners);
  const owner = owners.reserve({ socketId: session.socketId, userId: session.userId,
    credentialEpoch: session.credentialEpoch, token: 'test-original-token',
    tokenExpiresAt: Date.now() + 60000, disconnect: vi.fn() });
  await owners.register(owner);
  const ownedSession = { ...session, connectionGeneration: owners.snapshot(owner).target.connectionGeneration };
  owners.reserveRoom(owner, ownedSession, closeNative);
  return { api, owners, owner, session: ownedSession, closeNative, requests, authorizations, unsigned };
}
