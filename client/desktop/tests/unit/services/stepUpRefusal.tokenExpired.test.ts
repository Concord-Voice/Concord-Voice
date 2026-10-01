import { describe, expect, it } from 'vitest';
import { classifyStepUpRefusal } from '@/renderer/services/system/stepUpRefusal';
import { toDeleteRefusalView } from '@/renderer/services/messaging/deleteRefusal';

// #3509: an own-rule route that refuses a step_up_token answers the password
// prompt again, flagged step_up_token_invalid. The classifier keeps it a
// password refusal and marks it, so the delete and purge flows re-prompt with
// the expiry copy instead of a bare field.
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

  it('re-prompts the delete password view with the expiry copy', () => {
    expect(toDeleteRefusalView(403, body, '30', { view: 'password' })).toEqual({
      view: 'password',
      error: 'Your confirmation expired. Enter your password again.',
    });
  });
});
