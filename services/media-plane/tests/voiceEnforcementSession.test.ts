import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../src/middleware/auth.js', () => ({
  createServiceHopProofHeaders: vi.fn(() => ({
    'X-Concord-Service-Hop': 'test-hop',
  })),
}));

import {
  pollVoiceEnforcementHealth,
  registerVoiceEnforcementSession,
  releaseVoiceEnforcementSession,
  type VoiceEnforcementSession,
} from '../src/lib/voiceEnforcementSession.js';

const session: VoiceEnforcementSession = {
  sessionGeneration: 'generation-a',
  nodeBootId: 'boot-a',
  roomId: 'room-a',
  roomKind: 'channel',
  userId: 'user-a',
  credentialEpoch: 'epoch-a',
  socketId: 'socket-a',
};

function response(ok: boolean, status = ok ? 200 : 409): Response {
  return { ok, status } as Response;
}

describe('voice enforcement session control-plane calls', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(true)));
  });

  it('registers an exact session with service-hop and body-bound proof headers', async () => {
    await registerVoiceEnforcementSession('jwt-token', session);

    expect(fetch).toHaveBeenCalledOnce();
    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe('http://localhost:8080/api/v1/voice/enforcement-sessions');
    expect(init?.method).toBe('POST');
    expect(init?.headers).toMatchObject({
      Authorization: 'Bearer jwt-token',
      'Content-Type': 'application/json',
      'X-Concord-Service-Hop': 'test-hop',
    });
    expect(JSON.parse(String(init?.body))).toEqual({
      session_generation: session.sessionGeneration,
      node_boot_id: session.nodeBootId,
      room_id: session.roomId,
      room_kind: session.roomKind,
      credential_epoch: session.credentialEpoch,
      socket_id: session.socketId,
    });
  });

  it('rejects failed registration and refuses newline-delimited session fields', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response(false, 422));
    await expect(registerVoiceEnforcementSession('jwt-token', session)).rejects.toThrow(
      'registration (422)'
    );

    await expect(
      registerVoiceEnforcementSession('jwt-token', { ...session, socketId: 'socket\n-a' })
    ).rejects.toThrow('Unable to sign voice-enforcement registration');
    expect(fetch).toHaveBeenCalledOnce();
  });

  it('releases an exact session without using the user JWT', async () => {
    await releaseVoiceEnforcementSession(session);

    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe('http://localhost:8080/api/v1/internal/voice/enforcement-sessions/release');
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
    await expect(releaseVoiceEnforcementSession(session)).rejects.toThrow('release (503)');

    await expect(
      releaseVoiceEnforcementSession({ ...session, userId: 'user\n-a' })
    ).rejects.toThrow('Unable to sign voice-enforcement release');
    expect(fetch).toHaveBeenCalledOnce();
  });

  it('polls health with a fresh nonce and rejects failed health checks', async () => {
    await pollVoiceEnforcementHealth(session.nodeBootId);

    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe('http://localhost:8080/api/v1/internal/voice/enforcement/health');
    expect(init?.method).toBe('POST');
    expect(init?.headers).toMatchObject({
      'Content-Type': 'application/json',
      'X-Concord-Voice-Enforcement-Proof': expect.any(String),
    });
    const body = JSON.parse(String(init?.body));
    expect(body).toMatchObject({ node_boot_id: session.nodeBootId });
    expect(body.nonce).toMatch(/^[0-9a-f]{64}$/);

    vi.mocked(fetch).mockResolvedValueOnce(response(false, 401));
    await expect(pollVoiceEnforcementHealth(session.nodeBootId)).rejects.toThrow('health (401)');
  });
});
