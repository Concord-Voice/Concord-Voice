import { createHmac, timingSafeEqual } from 'node:crypto';
import type { RoomManager } from './roomManager.js';

const version = 'v1';
const requestPurpose = 'concord/voice-admission-activate/request/v1';
const responsePurpose = 'concord/voice-admission-activate/response/v1';
const notReadyResponsePurpose = 'concord/voice-admission-activate/response/not-ready/v1';
const maxSignedInt64 = 9_223_372_036_854_775_807n;

type ActivationRequest = {
  version: string;
  timestamp: string;
  channelId: string;
  userId: string;
  admissionId: string;
  socketId: string;
  revision: string;
  nonce: string;
  proof: string;
};

function deriveKey(secret: string, purpose: string): Buffer {
  return createHmac('sha256', secret).update(purpose).digest();
}

function sign(key: Buffer, timestamp: string, fields: string[]): string {
  return createHmac('sha256', key)
    .update([version, timestamp, ...fields].join('\n'))
    .digest('hex');
}

function validText(value: unknown, max = 256, allowEmpty = false): value is string {
  return (
    typeof value === 'string' &&
    (allowEmpty || value.length > 0) &&
    value.length <= max &&
    !value.includes('\n')
  );
}

function validProof(value: unknown): value is string {
  return typeof value === 'string' && /^[a-f0-9]{64}$/.test(value);
}

function proofMatches(secret: string, request: ActivationRequest): boolean {
  if (Math.abs(Date.now() - Number(request.timestamp) * 1000) > 30_000) return false;
  const expected = sign(
    deriveKey(secret, requestPurpose),
    request.timestamp,
    requestFields(request)
  );
  return timingSafeEqual(Buffer.from(expected, 'hex'), Buffer.from(request.proof, 'hex'));
}

function requestFields(request: ActivationRequest): string[] {
  return [
    'activate',
    request.channelId,
    request.userId,
    request.admissionId,
    request.socketId,
    request.revision,
    request.nonce,
  ];
}

function response(
  secret: string,
  request: ActivationRequest,
  result: 'ok' | 'not_ready',
  purpose: string
) {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const fields = [
    'activate',
    request.channelId,
    request.userId,
    request.admissionId,
    request.socketId,
    request.revision,
    request.nonce,
    result,
  ];
  return {
    version,
    timestamp,
    channelId: request.channelId,
    userId: request.userId,
    admissionId: request.admissionId,
    socketId: request.socketId,
    revision: request.revision,
    nonce: request.nonce,
    result,
    proof: sign(deriveKey(secret, purpose), timestamp, fields),
  };
}

function parseRequest(value: unknown): ActivationRequest | null {
  if (!value || typeof value !== 'object') return null;
  const request = value as ActivationRequest;
  if (
    request.version !== version ||
    !/^\d{1,20}$/.test(request.timestamp) ||
    !validText(request.channelId) ||
    !validText(request.userId) ||
    !/^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/i.test(request.admissionId) ||
    !validText(request.socketId, 128) ||
    !/^\d{1,19}$/.test(request.revision) ||
    BigInt(request.revision) <= 0n ||
    BigInt(request.revision) > maxSignedInt64 ||
    !/^[a-f0-9]{64}$/.test(request.nonce) ||
    !validProof(request.proof)
  )
    return null;
  return request;
}

/** Creates the synchronous, read-only A2 prepare responder. */
export function createChannelAdmissionActivationHandler(
  roomManager: Pick<RoomManager, 'hasExactProvisionalChannelParticipant'>,
  secret: string,
  isSocketConnected: (socketId: string) => boolean
): (payload: unknown) => Record<string, unknown> | undefined {
  return (payload) => {
    const request = parseRequest(payload);
    if (!request || !proofMatches(secret, request)) return undefined;
    if (!isSocketConnected(request.socketId)) {
      return response(secret, request, 'not_ready', notReadyResponsePurpose);
    }
    // PREPARE is strictly read-only. The local post-commit join flow performs
    // the sole promotion, membership attachment, epoch increment and event.
    if (
      !roomManager.hasExactProvisionalChannelParticipant(
        request.channelId,
        request.userId,
        request.socketId,
        request.admissionId
      )
    ) {
      return response(secret, request, 'not_ready', notReadyResponsePurpose);
    }
    return response(secret, request, 'ok', responsePurpose);
  };
}
