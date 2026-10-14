import { isCanonicalEnforcementUUID } from './enforcementCommand.js';

export interface VoiceCommand {
  schema: 'concord.voice-enforcement';
  version: 1;
  streamGeneration: string;
  commandId: string;
  sequence: string;
  scope: 'channel' | 'dm';
  kind: 'server_moderation' | 'user_moderation' | 'disconnect';
  action: 'mute' | 'unmute' | 'deafen' | 'undeafen' | 'disconnect';
  channelId: string | null;
  serverId?: string;
  userId: string;
  target: {
    sessionGeneration: string;
    nodeBootId: string;
    socketId: string;
    credentialEpoch: string;
  } | null;
  reason?: 'membership_removed' | 'moderator_request';
  authorizationRevision?: string;
  serverMuted?: boolean;
  serverDeafened?: boolean;
}

export type ExactVoiceCommandTarget = NonNullable<VoiceCommand['target']>;

const maximumRevision = 9223372036854775807n;
const commonFields = [
  'schema',
  'version',
  'streamGeneration',
  'commandId',
  'sequence',
  'scope',
  'kind',
  'action',
  'channelId',
  'userId',
  'target',
];
const targetFields = ['sessionGeneration', 'nodeBootId', 'socketId', 'credentialEpoch'];

function schemaError(): Error {
  return new Error('Invalid voice command schema');
}

function isObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function exactFields(
  value: Record<string, unknown>,
  required: string[],
  optional: string[] = []
): boolean {
  return (
    required.every((key) => Object.hasOwn(value, key)) &&
    Object.keys(value).every((key) => required.includes(key) || optional.includes(key))
  );
}

function positiveRevision(value: unknown): value is string {
  return (
    typeof value === 'string' &&
    /^[1-9]\d{0,18}$/.test(value) &&
    BigInt(value) <= maximumRevision
  );
}

function validTarget(value: unknown): value is ExactVoiceCommandTarget {
  if (!isObject(value) || !exactFields(value, targetFields)) return false;
  return (
    isCanonicalEnforcementUUID(value.sessionGeneration) &&
    isCanonicalEnforcementUUID(value.nodeBootId) &&
    typeof value.socketId === 'string' &&
    value.socketId.length > 0 &&
    Buffer.byteLength(value.socketId, 'utf8') <= 128 &&
    !/[\p{Cc}\p{Surrogate}]/u.test(value.socketId) &&
    typeof value.credentialEpoch === 'string' &&
    /^(?:[0-9a-f]{32})?$/.test(value.credentialEpoch)
  );
}

// JSON.parse discards duplicate fields. Inspect the already syntactically valid
// JSON tokens first, so an escaped key cannot hide a conflicting frozen identity.
function rejectDuplicateFields(wire: string): void {
  const objects: Array<Set<string> | null> = [];
  const tokens = /"(?:[^"\\]|\\.)*"|[{}[\]]/g;
  for (const match of wire.matchAll(tokens)) {
    const token = match[0];
    if (token === '{') objects.push(new Set());
    else if (token === '[') objects.push(null);
    else if (token === '}' || token === ']') objects.pop();
    else {
      const tail = wire.slice((match.index ?? 0) + token.length);
      if (!/^\s*:/.test(tail)) continue;
      const fields = objects.at(-1);
      if (!fields) throw schemaError();
      const key = JSON.parse(token) as string;
      if (fields.has(key)) throw schemaError();
      fields.add(key);
    }
  }
}

function validateKind(value: Record<string, unknown>): boolean {
  if (value.kind === 'server_moderation') {
    const required = [...commonFields, 'authorizationRevision', 'serverMuted', 'serverDeafened'];
    if (value.scope === 'channel') required.push('serverId');
    return (
      exactFields(value, required) &&
      (value.scope !== 'channel' || isCanonicalEnforcementUUID(value.serverId)) &&
      ['mute', 'unmute', 'deafen', 'undeafen'].includes(value.action as string) &&
      positiveRevision(value.authorizationRevision) &&
      typeof value.serverMuted === 'boolean' &&
      typeof value.serverDeafened === 'boolean' &&
      (!value.serverDeafened || value.serverMuted)
    );
  }
  if (value.kind === 'user_moderation') {
    return (
      exactFields(value, commonFields) &&
      (value.action === 'mute' || value.action === 'deafen') &&
      value.target !== null
    );
  }
  if (value.kind === 'disconnect') {
    return (
      exactFields(value, [...commonFields, 'reason']) &&
      value.action === 'disconnect' &&
      (value.reason === 'membership_removed' || value.reason === 'moderator_request') &&
      (value.target !== null || (value.scope === 'dm' && value.reason === 'membership_removed'))
    );
  }
  return false;
}

/** Reserved fields distinguish this contract from legacy version:1 snapshots. */
export function isVersionedVoiceCommand(data: Record<string, unknown>): boolean {
  return [
    'schema',
    'commandId',
    'streamGeneration',
    'sequence',
    'scope',
    'kind',
    'target',
    'serverId',
    'reason',
  ].some((key) => Object.hasOwn(data, key));
}

/** Strictly decode one generation-bound command without leaking rejected bytes. */
export function decodeVoiceCommand(data: Uint8Array, expectedGeneration: string): VoiceCommand {
  try {
    if (
      data.byteLength === 0 ||
      data.byteLength > 4096 ||
      !isCanonicalEnforcementUUID(expectedGeneration)
    )
      throw schemaError();
    const wire = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(data);
    const value: unknown = JSON.parse(wire);
    rejectDuplicateFields(wire);
    if (
      !isObject(value) ||
      value.schema !== 'concord.voice-enforcement' ||
      value.version !== 1 ||
      value.streamGeneration !== expectedGeneration ||
      !isCanonicalEnforcementUUID(value.commandId) ||
      !positiveRevision(value.sequence) ||
      (value.scope !== 'channel' && value.scope !== 'dm') ||
      !isCanonicalEnforcementUUID(value.userId) ||
      (value.target !== null && !validTarget(value.target)) ||
      (value.channelId !== null && !isCanonicalEnforcementUUID(value.channelId)) ||
      (value.channelId === null &&
        !(
          value.scope === 'channel' &&
          value.kind === 'server_moderation' &&
          value.target === null
        )) ||
      !validateKind(value)
    )
      throw schemaError();
    return value as unknown as VoiceCommand;
  } catch {
    throw schemaError();
  }
}
