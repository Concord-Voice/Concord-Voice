/**
 * What a host of a role or override write (#3456 §3.4) does with the store's
 * result: whether a refusal asks for verification, how the dialog reads the
 * re-send, and what to say when it does not.
 *
 * The store returns a refusal as it came (`PermissionWriteFailure`) because
 * only the host knows whether its route is one a D1 gate guards; these read it
 * the same way for all four hosts (role create, update and delete, and the
 * override upsert).
 */
import type { DangerousActionSendResult } from '../../components/Auth/DangerousActionStepUpDialog';
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import type {
  PermissionWriteFailure,
  PermissionWriteOutcome,
} from '../../stores/chat/permissionStore';
import { FIRST_SEND_SESSION_CHANGED } from './dangerousActionRequest';
import { apiRequestContextIsCurrent, type ApiRequestContext } from './requestContext';
import { adaptDangerousActionRefusal, serverErrorText } from './stepUpRouteAdapters';

/**
 * What a refusal hands the dialog: the refusal it starts from, and the account
 * and server the refused request went out as, which becomes its `capture`.
 */
export interface PermissionWriteStepUpSeed {
  readonly refusal: StepUpFactorRefusal;
  readonly context: ApiRequestContext;
}

/** A refusal that asks for verification, whether or not its session is still current. */
function verificationRefusalOf(outcome: PermissionWriteOutcome): PermissionWriteStepUpSeed | null {
  if (outcome.ok || outcome.kind !== 'refused') return null;
  const refusal = adaptDangerousActionRefusal(outcome.status, outcome.body);
  return refusal?.kind === 'mfaRequired' || refusal?.kind === 'enrollmentRequired'
    ? { refusal, context: outcome.context }
    : null;
}

/**
 * The refusal that opens the dialog and the capture it opens with, or null when
 * `outcome` is not one.
 *
 * Only a server that wants a verified factor (`mfaRequired`) and an actor with
 * none to give (`enrollmentRequired`) open it. Every other answer, including
 * the adapter's own 429 and 503, is the host's to word as it always was.
 *
 * A refusal that lands after the account or server it went out as changed
 * opens nothing, as `sendFirst`'s does: its dialog would ask whoever is signed
 * in now for a code to re-send a request someone else made. `failureTextOf`
 * words it as the expired session it is.
 */
export function stepUpSeedOf(outcome: PermissionWriteOutcome): PermissionWriteStepUpSeed | null {
  const seed = verificationRefusalOf(outcome);
  return seed !== null && apiRequestContextIsCurrent(seed.context) ? seed : null;
}

/** The dialog's reading of a re-send. */
export function sendResultOf(outcome: PermissionWriteOutcome): DangerousActionSendResult {
  if (outcome.ok) return { kind: 'ok' };
  switch (outcome.kind) {
    case 'refused':
      return { kind: 'refused', status: outcome.status, body: outcome.body };
    case 'aborted':
      return { kind: 'aborted' };
    case 'network':
      return { kind: 'transport' };
  }
}

/**
 * The server's own sentence for a refusal, else `fallback`; for a refusal that
 * asked for verification after its session ended, that the session expired.
 */
export function failureTextOf(failure: PermissionWriteFailure, fallback: string): string {
  if (failure.kind !== 'refused') return fallback;
  const stale = verificationRefusalOf(failure);
  if (stale !== null && !apiRequestContextIsCurrent(stale.context)) {
    return FIRST_SEND_SESSION_CHANGED;
  }
  return serverErrorText(failure.body) ?? fallback;
}
