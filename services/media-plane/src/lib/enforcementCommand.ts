import type { EmitSecurityEvent } from './securityEvent.js';

// Control-plane publishers use lowercase RFC 4122 UUIDs. The version range
// admits deployed UUID generations while excluding nil and malformed variants.
const canonicalUUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export function isCanonicalEnforcementUUID(value: unknown): value is string {
  return typeof value === 'string' && canonicalUUID.test(value);
}

export interface NatsEnforcementCommand {
  channelId: string;
  userId: string;
  action?: string;
  socketId?: string;
  admissionId?: string;
  callId?: string;
}

function isSocketId(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && Buffer.byteLength(value, 'utf8') <= 128;
}

/** Validates an enforcement command before it reaches a room mutation. */
export async function handleNatsEnforcementCommand(
  data: Record<string, unknown>,
  allowedActions: readonly string[] | undefined,
  handle: (command: NatsEnforcementCommand) => void | Promise<void>,
  emit?: EmitSecurityEvent
): Promise<void> {
  const channelId = data.channelId;
  const userId = data.userId;
  const action = data.action;
  const socketId = data.socketId;
  const admissionId = data.admissionId;
  const callId = data.callId;
  const hasExactAdmissionIdentity = socketId !== undefined || admissionId !== undefined;
  if (
    !isCanonicalEnforcementUUID(channelId) ||
    !isCanonicalEnforcementUUID(userId) ||
    (hasExactAdmissionIdentity &&
      (!isSocketId(socketId) || !isCanonicalEnforcementUUID(admissionId))) ||
    (callId !== undefined && !isCanonicalEnforcementUUID(callId)) ||
    (allowedActions !== undefined &&
      (typeof action !== 'string' || !allowedActions.includes(action)))
  ) {
    try {
      emit?.({
        eventType: 'media_integrity',
        outcome: 'denied',
        severity: 'medium',
        reasonCode: 'media_schema_rejected',
      });
    } catch {
      // Telemetry cannot make malformed input reach a room mutation.
    }
    return;
  }

  try {
    await handle({
      channelId,
      userId,
      ...(typeof action === 'string' ? { action } : {}),
      ...(socketId === undefined ? {} : { socketId }),
      ...(admissionId === undefined ? {} : { admissionId }),
      ...(callId === undefined ? {} : { callId }),
    });
  } catch (error) {
    try {
      emit?.({
        eventType: 'media_integrity',
        outcome: 'failure',
        severity: 'medium',
        reasonCode: 'socket_handler_failed',
      });
    } catch {
      // Telemetry cannot replace the original enforcement failure.
    }
    throw error;
  }
}
