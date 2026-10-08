import { act } from '@testing-library/react';
import type { StepUpSubmitOutcome } from '@/renderer/hooks/auth/useStepUpFactor';
import type { DeleteStepUp } from '@/renderer/hooks/messaging/useChatController';
import type { ApiRequestContext } from '@/renderer/services/system/requestContext';

interface ConfirmHost {
  result: {
    current: {
      confirmDelete: (
        step: DeleteStepUp,
        context?: ApiRequestContext
      ) => Promise<StepUpSubmitOutcome>;
    };
  };
}

/**
 * Runs `confirmDelete` to completion inside an async `act` and returns the
 * outcome the factor hook would be handed (D6). `confirmDelete` returns a
 * promise now, so an expression-bodied `act(() => confirmDelete(...))` makes
 * `act` async with nothing awaiting it.
 */
export async function confirmAndSettle(
  hook: ConfirmHost,
  step: DeleteStepUp,
  context?: ApiRequestContext
): Promise<StepUpSubmitOutcome> {
  let outcome: StepUpSubmitOutcome | undefined;
  await act(async () => {
    outcome = await hook.result.current.confirmDelete(step, context);
  });
  if (outcome === undefined) throw new Error('confirmDelete did not settle');
  return outcome;
}

/** Starts `confirmDelete` without waiting; the caller settles it later. */
export function confirmInFlight(
  hook: ConfirmHost,
  step: DeleteStepUp,
  context?: ApiRequestContext
): Promise<StepUpSubmitOutcome> {
  let pending: Promise<StepUpSubmitOutcome> = Promise.resolve({ kind: 'aborted' });
  act(() => {
    pending = hook.result.current.confirmDelete(step, context);
  });
  return pending;
}
