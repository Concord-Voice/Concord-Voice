import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import {
  isDangerousChallengeResult,
  isStepUpPurgeResult,
  purgeMessages,
} from '@/renderer/services/messaging/purgeApi';

// #3456 (C-6): a channel/server purge refused by its dangerous-action (D1)
// gate answers the plain seam body, with no `delete_rate_limited`. The
// delete-rate soft-lock always carries the flag, and the DM/group routes carry
// neither, so the flag and the route decide which reading a 403 gets.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
});

const D1_MFA = { error: 'MFA verification required', mfa_required: true, methods: ['totp'] };
const D1_ENROLL = { error: 'Set up an app', mfa_enrollment_required: true };

const SELF_PURGE_ROUTES = [
  ['channel', 'c1', '*/api/v1/channels/:id/messages'],
  ['server', 's1', '*/api/v1/servers/:id/messages'],
] as const;

const PRIVATE_ROUTES = [['dm'], ['group']] as const;
const DM_PATH = '*/api/v1/dm/conversations/:id/messages';

describe.each(SELF_PURGE_ROUTES)(
  'purgeMessages D1 gate on the %s route',
  (context, scopeId, path) => {
    const refuseWith = (body: object, status = 403) =>
      server.use(http.delete(path, () => HttpResponse.json(body, { status })));

    // Mutation: mapSelfPurgeForbidden returns null for an unflagged 403.
    it('mfa_required is a dangerousChallenge carrying the methods the gate named', async () => {
      refuseWith({ ...D1_MFA, methods: ['totp', 'webauthn'] });
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'dangerousChallenge',
        refusal: { kind: 'mfaRequired', methods: ['totp', 'webauthn'] },
      });
    });

    // Mutation: dangerousChallenge() accepts only 'mfaRequired'.
    it('mfa_enrollment_required is a dangerousChallenge, not forbidden', async () => {
      refuseWith(D1_ENROLL);
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'dangerousChallenge',
        refusal: { kind: 'enrollmentRequired' },
      });
    });

    // Mutation: dangerousChallenge() accepts only 'mfaRequired' (the enrolment shape is lost).
    it('the enrolment refusal wins over a body that also says mfa_required', async () => {
      refuseWith({ ...D1_MFA, mfa_enrollment_required: true });
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'dangerousChallenge',
        refusal: { kind: 'enrollmentRequired' },
      });
    });

    // Mutation: mapSelfPurgeForbidden returns null for an unflagged 403.
    it('a challenge is its own kind: not a DM step-up result and not a soft-lock', async () => {
      refuseWith(D1_MFA);
      const result = await purgeMessages({ context, scopeId, range: '7d' });
      expect(isDangerousChallengeResult(result)).toBe(true);
      expect(isStepUpPurgeResult(result)).toBe(false);
      expect(result.kind).not.toBe('softLockChallenge');
    });

    // Mutation: the `!isDeleteRateLimited` test is inverted, or the self-purge guard drops 'server'.
    it('a soft-lock MFA 403 stays the soft-lock, whatever else the body says', async () => {
      refuseWith({ ...D1_MFA, delete_rate_limited: true });
      const result = await purgeMessages({ context, scopeId, range: '7d' });
      expect(result.kind).toBe('softLockChallenge');
    });

    // Mutation: dangerousChallenge() answers any 403 as a challenge.
    it('an invalid code on the retry stays the field’s refusal', async () => {
      refuseWith({ error: 'Invalid MFA code' });
      expect(await purgeMessages({ context, scopeId, range: '7d', mfaCode: '000000' })).toEqual({
        kind: 'invalidMfaCode',
      });
    });

    // Mutation: dangerousChallenge() answers any 403 as a challenge.
    it('a plain 403 is still forbidden', async () => {
      refuseWith({ error: 'forbidden' });
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({ kind: 'forbidden' });
    });

    // Mutation: dangerousChallenge() answers any 403 as a challenge.
    it('a 403 the D1 adapter does not own is forbidden, not a challenge', async () => {
      refuseWith({ error: 'Something else' });
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({ kind: 'forbidden' });
    });
  }
);

describe.each(PRIVATE_ROUTES)(
  'purgeMessages on the %s route is unchanged by the D1 gate',
  (context) => {
    // Mutation: mapForbidden's channel/server guard is removed (`if (true)`).
    it('mfa_required keeps the DM step-up kind', async () => {
      server.use(http.delete(DM_PATH, () => HttpResponse.json(D1_MFA, { status: 403 })));
      expect(await purgeMessages({ context, scopeId: 'dm1', range: '7d' })).toEqual({
        kind: 'mfaRequired',
        methods: ['totp'],
      });
    });

    // Mutation: mapForbidden's channel/server guard is removed (`if (true)`).
    it('mfa_enrollment_required stays forbidden: no D1 gate guards a DM purge', async () => {
      server.use(http.delete(DM_PATH, () => HttpResponse.json(D1_ENROLL, { status: 403 })));
      expect(await purgeMessages({ context, scopeId: 'dm1', range: '7d' })).toEqual({
        kind: 'forbidden',
      });
    });
  }
);
