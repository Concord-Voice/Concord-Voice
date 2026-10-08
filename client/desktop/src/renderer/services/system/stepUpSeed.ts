/**
 * The refusal that opens `DangerousActionStepUpDialog` (#3456 §3.3): a gated
 * route that asks for a verified code, or refuses an actor with none to give.
 *
 * For a host whose first send is not a JSON `FrozenRequest` (a multipart crop
 * upload, a PATCH, a policy change that arrives as a typed result). Every other
 * answer, including the adapter's own 429, 503 and 401, is the host's to word
 * as it always was.
 */
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import { adaptDangerousActionRefusal } from './stepUpRouteAdapters';

export function stepUpSeed(status: number, body: unknown): StepUpFactorRefusal | null {
  const refusal = adaptDangerousActionRefusal(status, body);
  return refusal?.kind === 'mfaRequired' || refusal?.kind === 'enrollmentRequired' ? refusal : null;
}
