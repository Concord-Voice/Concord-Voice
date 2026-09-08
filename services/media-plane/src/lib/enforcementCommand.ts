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
  if (
    !isCanonicalEnforcementUUID(channelId) ||
    !isCanonicalEnforcementUUID(userId) ||
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
