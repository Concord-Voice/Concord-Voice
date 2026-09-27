import { createHmac } from 'node:crypto';
import { describe, expect, it, vi } from 'vitest';
import { createChannelAdmissionActivationHandler } from '../src/lib/channelAdmissionActivation.js';

const SECRET = 'activation-test-secret'; // pragma: allowlist secret -- deterministic test fixture, not a credential
const CHANNEL_ID = '11111111-1111-4111-8111-111111111111';
const USER_ID = '22222222-2222-4222-8222-222222222222';
const ADMISSION_ID = '33333333-3333-4333-8333-333333333333';
const SOCKET_ID = 'socket-activation';
function signedPayload(overrides: Record<string, unknown> = {}) {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const fields = ['activate', CHANNEL_ID, USER_ID, ADMISSION_ID, SOCKET_ID, '7', 'a'.repeat(64)];
  const key = createHmac('sha256', SECRET)
    .update('concord/voice-admission-activate/request/v1')
    .digest();
  const proof = createHmac('sha256', key)
    .update(['v1', timestamp, ...fields].join('\n'))
    .digest('hex');
  return {
    version: 'v1',
    timestamp,
    channelId: CHANNEL_ID,
    userId: USER_ID,
    admissionId: ADMISSION_ID,
    socketId: SOCKET_ID,
    revision: '7',
    nonce: 'a'.repeat(64),
    proof,
    ...overrides,
  };
}

function responseProof(response: Record<string, unknown>, purpose: string): string {
  const key = createHmac('sha256', SECRET).update(purpose).digest();
  return createHmac('sha256', key)
    .update(
      [
        'v1',
        String(response.timestamp),
        'activate',
        CHANNEL_ID,
        USER_ID,
        ADMISSION_ID,
        SOCKET_ID,
        '7',
        'a'.repeat(64),
        String(response.result),
      ].join('\n')
    )
    .digest('hex');
}

describe('channel admission activation fence', () => {
  it('prepares the exact minimal signed candidate without granting membership', () => {
    const hasExact = vi.fn().mockReturnValue(true);
    const handler = createChannelAdmissionActivationHandler(
      { hasExactProvisionalChannelParticipant: hasExact } as never,
      SECRET,
      () => true
    );

    const response = handler(signedPayload());

    expect(response).toMatchObject({
      result: 'ok',
      channelId: CHANNEL_ID,
      userId: USER_ID,
      admissionId: ADMISSION_ID,
      socketId: SOCKET_ID,
      revision: '7',
    });
    expect(hasExact).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, SOCKET_ID, ADMISSION_ID);
  });

  it('keeps an exact duplicate read-only at the prepare seam', () => {
    const hasExact = vi.fn().mockReturnValue(true);
    const handler = createChannelAdmissionActivationHandler(
      { hasExactProvisionalChannelParticipant: hasExact } as never,
      SECRET,
      () => true
    );
    const payload = signedPayload();

    expect(handler(payload)).toMatchObject({ result: 'ok' });
    expect(handler(payload)).toMatchObject({ result: 'ok' });
    expect(hasExact).toHaveBeenCalledTimes(2);
  });

  it.each([
    ['disconnected socket', () => false, vi.fn()],
    ['missing exact candidate', () => true, vi.fn().mockReturnValue(false)],
  ])('returns a signed not-ready acknowledgement for %s', (_name, isConnected, hasExact) => {
    const handler = createChannelAdmissionActivationHandler(
      { hasExactProvisionalChannelParticipant: hasExact } as never,
      SECRET,
      isConnected
    );

    const response = handler(signedPayload());

    expect(response).toMatchObject({ result: 'not_ready' });
    const acknowledgement = response as Record<string, unknown>;
    expect(acknowledgement.proof).toBe(
      responseProof(acknowledgement, 'concord/voice-admission-activate/response/not-ready/v1')
    );
  });

  it.each([
    ['tampered revision', { revision: '8' }],
    ['nonnumeric timestamp', { timestamp: 'not-a-time' }],
    ['wrong admission', { admissionId: '44444444-4444-4444-8444-444444444444' }],
    ['disconnected socket marker', { socketId: '' }],
  ])('fails closed for %s', (_name, overrides) => {
    const hasExact = vi.fn();
    const handler = createChannelAdmissionActivationHandler(
      { hasExactProvisionalChannelParticipant: hasExact } as never,
      SECRET,
      () => true
    );

    expect(handler(signedPayload(overrides))).toBeUndefined();
    expect(hasExact).not.toHaveBeenCalled();
  });

  it('returns no acknowledgement for a tampered request', () => {
    const handler = createChannelAdmissionActivationHandler(
      {
        hasExactProvisionalChannelParticipant: vi.fn().mockReturnValue(false),
      } as never,
      SECRET,
      () => true
    );

    expect(handler(signedPayload({ proof: '0'.repeat(64) }))).toBeUndefined();
  });
});
