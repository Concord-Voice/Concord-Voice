import { createHash, createHmac, randomBytes } from 'node:crypto';
import { config } from '../config/index.js';
import { createServiceHopProofHeaders } from '../middleware/auth.js';

const registerPath = '/api/v1/voice/enforcement-sessions';
const releasePath = '/api/v1/internal/voice/enforcement-sessions/release';
const releaseProofContext = 'concord/voice-enforcement-session/release/v1';
const releaseProofVersion = 'v1';
const registrationProofContext = 'concord/voice-enforcement-session/register/v1';
const registrationProofVersion = 'v1';
const controlPlaneTimeoutMs = 5_000;
const healthPath = '/api/v1/internal/voice/enforcement/health';
const healthProofContext = 'concord/voice-enforcement-session/health/bootstrap/v1';

export type VoiceEnforcementRoomKind = 'channel' | 'dm';

export interface VoiceEnforcementSession {
  sessionGeneration: string;
  nodeBootId: string;
  roomId: string;
  roomKind: VoiceEnforcementRoomKind;
  userId: string;
  credentialEpoch: string;
  socketId: string;
}

// A boot identifier is intentionally process-local. It is an exact routing
// address, not a node lease: a crashed process leaves its durable rows pending.
export { voiceEnforcementNodeBootId } from './voiceEnforcementIdentity.js';

function releaseProof(session: VoiceEnforcementSession, timestamp: string): string {
  const key = createHmac('sha256', config.jwtSecret).update(releaseProofContext).digest();
  const fields = [
    session.sessionGeneration,
    session.nodeBootId,
    session.roomId,
    session.roomKind,
    session.userId,
    session.credentialEpoch,
    session.socketId,
  ];
  if (fields.some((field) => field.includes('\n'))) return '';
  return createHmac('sha256', key)
    .update([releaseProofVersion, timestamp, ...fields].join('\n'))
    .digest('hex');
}

function registrationProof(
  session: VoiceEnforcementSession,
  token: string,
  timestamp: string
): string {
  const key = createHmac('sha256', config.jwtSecret).update(registrationProofContext).digest();
  const fields = [
    createHash('sha256').update(token).digest('hex'),
    session.sessionGeneration,
    session.nodeBootId,
    session.roomId,
    session.roomKind,
    session.userId,
    session.credentialEpoch,
    session.socketId,
  ];
  if (fields.some((field) => field.includes('\n'))) return '';
  return createHmac('sha256', key)
    .update([registrationProofVersion, timestamp, ...fields].join('\n'))
    .digest('hex');
}

export async function registerVoiceEnforcementSession(
  token: string,
  session: VoiceEnforcementSession
): Promise<void> {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const proof = registrationProof(session, token, timestamp);
  if (!proof) throw new Error('Unable to sign voice-enforcement registration');
  const response = await fetch(`${config.controlPlaneUrl}${registerPath}`, {
    method: 'POST',
    signal: AbortSignal.timeout(controlPlaneTimeoutMs),
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      'X-Concord-Voice-Enforcement-Registration-Timestamp': timestamp,
      'X-Concord-Voice-Enforcement-Registration-Proof': proof,
      ...createServiceHopProofHeaders('POST', registerPath, token),
    },
    body: JSON.stringify({
      session_generation: session.sessionGeneration,
      node_boot_id: session.nodeBootId,
      room_id: session.roomId,
      room_kind: session.roomKind,
      credential_epoch: session.credentialEpoch,
      socket_id: session.socketId,
    }),
  });
  if (!response.ok) {
    throw new Error(`Control plane rejected voice-enforcement registration (${response.status})`);
  }
}

// The release proof is independent of a user JWT. A credential-epoch ejection
// may invalidate that JWT before the target node can tear down the socket.
export async function releaseVoiceEnforcementSession(
  session: VoiceEnforcementSession
): Promise<void> {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const proof = releaseProof(session, timestamp);
  if (!proof) throw new Error('Unable to sign voice-enforcement release');
  const response = await fetch(`${config.controlPlaneUrl}${releasePath}`, {
    method: 'POST',
    signal: AbortSignal.timeout(controlPlaneTimeoutMs),
    headers: {
      'Content-Type': 'application/json',
      'X-Concord-Voice-Enforcement-Timestamp': timestamp,
      'X-Concord-Voice-Enforcement-Proof': proof,
    },
    body: JSON.stringify({
      session_generation: session.sessionGeneration,
      node_boot_id: session.nodeBootId,
      room_id: session.roomId,
      room_kind: session.roomKind,
      user_id: session.userId,
      credential_epoch: session.credentialEpoch,
      socket_id: session.socketId,
    }),
  });
  if (!response.ok) {
    throw new Error(`Control plane rejected voice-enforcement release (${response.status})`);
  }
}

// This poll alone never grants admission: CP must complete the challenge over
// the same exact target subscriber, whose verified receipt renews the lease.
export async function pollVoiceEnforcementHealth(nodeBootId: string): Promise<void> {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const nonce = randomBytes(32).toString('hex');
  const key = createHmac('sha256', config.jwtSecret).update(healthProofContext).digest();
  const proof = createHmac('sha256', key)
    .update(['v1', timestamp, nodeBootId, nonce, 'POST', healthPath].join('\n'))
    .digest('hex');
  const response = await fetch(`${config.controlPlaneUrl}${healthPath}`, {
    method: 'POST',
    signal: AbortSignal.timeout(controlPlaneTimeoutMs),
    headers: {
      'Content-Type': 'application/json',
      'X-Concord-Voice-Enforcement-Timestamp': timestamp,
      'X-Concord-Voice-Enforcement-Proof': proof,
    },
    body: JSON.stringify({ node_boot_id: nodeBootId, nonce }),
  });
  if (!response.ok)
    throw new Error(`Control plane rejected voice-enforcement health (${response.status})`);
}
