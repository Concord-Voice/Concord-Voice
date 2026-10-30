import { createHmac, randomBytes } from 'node:crypto';
import { readVoiceAuthResponse, type VoiceAuthConnections } from './voiceAuthConnection.js';
import { signHttpRequest, newRequestContext, verifyHttpResponse, type HealthBootstrapResult } from './voiceAuthProtocol.js';
import { config } from '../config/index.js';

const releasePath = '/api/v1/internal/voice/enforcement-sessions/release';
const releaseProofContext = 'concord/voice-enforcement-session/release/v1';
const releaseProofVersion = 'v1';
const controlPlaneTimeoutMs = 5_000;
const healthPath = '/api/v1/internal/voice/enforcement/health';

export type VoiceEnforcementRoomKind = 'channel' | 'dm';

export interface VoiceEnforcementSession {
  /** Present only on a reserved family-aware room owner. */
  connectionGeneration?: string;
  sessionGeneration: string;
  nodeBootId: string;
  roomId: string;
  roomKind: VoiceEnforcementRoomKind;
  userId: string;
  credentialEpoch: string;
  socketId: string;
}

export type NativeSessionVerifier = (session: VoiceEnforcementSession) => Promise<void>;

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

let connections: VoiceAuthConnections | undefined;

export function setVoiceAuthConnections(owner: VoiceAuthConnections): void {
  if (connections && connections !== owner) throw new Error('Voice ownership already installed');
  connections = owner;
}

export async function registerVoiceEnforcementSession(token: string, session: VoiceEnforcementSession): Promise<void> {
  if (!connections || !session.connectionGeneration) throw new Error('Voice connection owner required');
  await connections.attachRoom(connections.handleForSession(session), session, token);
}

export async function renewVoiceEnforcementSession(
  token: string, session: VoiceEnforcementSession, sessionIsCurrent: () => boolean,
  _releaseAfterTerminalClose: (session: VoiceEnforcementSession) => Promise<void>
): Promise<void> {
  if (!connections || !session.connectionGeneration) throw new Error('Voice connection owner required');
  await connections.renewRoom(connections.handleForSession(session), session, token, sessionIsCurrent);
}

// The release proof is independent of a user JWT. A credential-epoch ejection
// may invalidate that JWT before the target node can tear down the socket.
export async function releaseVoiceEnforcementSession(
  session: VoiceEnforcementSession,
  verifyNative: NativeSessionVerifier
): Promise<void> {
  // Legacy exact-room commands carry no parent marker. Resolve their complete
  // immutable tuple against the private retained owner before choosing proofs.
  if (connections?.ownsSession(session)) {
    await connections.releaseRoom(session);
    return;
  }
  if (session.connectionGeneration) throw new Error('Voice connection owner required');
  await verifyNative(session);
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

// Only the shared strict codec can establish transport/productive permission.
export async function pollVoiceEnforcementHealth(nodeBootId: string): Promise<HealthBootstrapResult> {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const nonce = randomBytes(32).toString('hex');
  const secret = Buffer.from(config.jwtSecret, 'utf8');
  const body = Buffer.from(JSON.stringify({ version: 1, nodeBootId, nonce }));
  const { wire, request } = signHttpRequest('health_bootstrap', body, '', { timestamp, nonce }, secret);
  const context = newRequestContext(request, {});
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  for (const header of wire.headers) headers[header.name] = header.value;
  const response = await fetch(`${config.controlPlaneUrl}${healthPath}`, {
    method: 'POST', signal: AbortSignal.timeout(controlPlaneTimeoutMs), headers, body,
  });
  const result = verifyHttpResponse('health_bootstrap', await readVoiceAuthResponse(response),
    context, secret, BigInt(Math.floor(Date.now() / 1000))).healthBootstrap();
  if (!result) throw new Error('Missing voice health result');
  return result;
}
