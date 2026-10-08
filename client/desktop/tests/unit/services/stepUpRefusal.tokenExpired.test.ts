import { describe, expect, it } from 'vitest';
import { classifyStepUpRefusal } from '@/renderer/services/system/stepUpRefusal';
import {
  softLockSeed,
  toDeleteRefusalView,
  toDeleteSubmitOutcome,
} from '@/renderer/services/messaging/deleteRefusal';

// #3509: an own-rule route that refuses a step_up_token answers the password
// prompt again, flagged step_up_token_invalid. The classifier keeps it a
// password refusal and marks it. The delete view carries no copy of its own
// since picker PR 3 (the credential stage words the reply from the hook's
// outcome), so the marker travels on the OUTCOME the hook is handed.
describe('step_up_token_invalid (#3509)', () => {
  const body = {
    error: 'Your confirmation expired. Enter your password again.',
    password_required: true,
    step_up_token_invalid: true,
    delete_rate_limited: true,
  };

  it('is a password refusal marked tokenExpired', () => {
    expect(classifyStepUpRefusal(403, body)).toEqual({
      kind: 'passwordRequired',
      tokenExpired: true,
    });
    expect(classifyStepUpRefusal(403, { password_required: true })).toEqual({
      kind: 'passwordRequired',
    });
  });

  it('re-prompts the delete password view bare: the copy is the stage’s, not the view’s', () => {
    const view = toDeleteRefusalView(403, body, '30', { view: 'password' });
    expect(view).toEqual({ view: 'password' });
    expect(view).not.toHaveProperty('error');
    expect(softLockSeed(view)).toEqual({ kind: 'passwordRequired' });
  });

  it('hands the factor hook the marked refusal, whatever view was on screen', () => {
    const expected = {
      kind: 'refusal',
      refusal: { kind: 'passwordRequired', tokenExpired: true },
    };
    expect(toDeleteSubmitOutcome(403, body)).toEqual(expected);
    expect(
      toDeleteSubmitOutcome(403, { password_required: true, step_up_token_invalid: true })
    ).toEqual(expected);
  });

  it('an unmarked password refusal carries no marker', () => {
    expect(toDeleteSubmitOutcome(403, { password_required: true })).toEqual({
      kind: 'refusal',
      refusal: { kind: 'passwordRequired' },
    });
  });

  it('the marker is matched strictly: a truthy non-boolean is not the flag', () => {
    expect(
      classifyStepUpRefusal(403, { password_required: true, step_up_token_invalid: 'true' })
    ).toEqual({ kind: 'passwordRequired' });
  });
});
