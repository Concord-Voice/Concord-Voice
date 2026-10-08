import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';

// #3456 (C-6): the 'mfa' stage of a channel or server purge refused by its
// dangerous-action (D1) gate on an MFA-enforcing server. The gate writes the
// plain seam body, with no `delete_rate_limited`; the purge is sent again with
// the code it asked for.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => {
  server.resetHandlers();
  resetRuntimeServerBase();
});
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
  server.use(readAnswers(['totp']));
});

const READ_PATH = '*/api/v1/mfa/step-up';
function readAnswers(methods: string[]) {
  return http.get(READ_PATH, () =>
    HttpResponse.json({
      methods,
      default_method: methods[0] ?? null,
      backup_code_available: false,
    })
  );
}

const CHANNEL_PATH = '*/api/v1/channels/:id/messages';
const SERVER_PATH = '*/api/v1/servers/:id/messages';
const BEGIN_PATH = '*/api/v1/mfa/webauthn/verify-inline/begin';
const ENROLLMENT_COPY = 'Set up an authenticator app or security key in Settings to do this.';
const DANGEROUS_INTRO = "Purging messages is permanent, so confirm it's you to continue.";
const CODE = '123456';

const D1_MFA = { error: 'MFA verification required', mfa_required: true, methods: ['totp'] };
const D1_ENROLL = { error: 'Set up an app', mfa_enrollment_required: true };
const SOFT_LOCK_ENROLL = { delete_rate_limited: true, mfa_enrollment_required: true };

type Sent = Record<string, unknown>;

/** Answers each successive purge request with the next response, recording bodies. */
function scriptedPurge(path: string, responses: Array<() => Response>): Sent[] {
  const bodies: Sent[] = [];
  server.use(
    http.delete(path, async ({ request }) => {
      bodies.push((await request.json()) as Sent);
      return responses[Math.min(bodies.length - 1, responses.length - 1)]();
    })
  );
  return bodies;
}

const refuse = (body: object) => () => HttpResponse.json(body, { status: 403 });
const purged = (n: number) => () => HttpResponse.json({ deleted_count: n, hidden_count: 0 });

async function startChannelPurge(onClose: () => void = () => {}) {
  const user = userEvent.setup();
  render(
    <PurgeMessagesModal
      context="channel"
      isOpen
      scopeId="c1"
      scopeName="general"
      onClose={onClose}
    />
  );
  await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
  await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
  return user;
}

async function startServerPurge() {
  const user = userEvent.setup();
  render(
    <PurgeMessagesModal context="server" isOpen scopeId="s1" scopeName="Guild" onClose={() => {}} />
  );
  await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
  await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
  await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
  return user;
}

const codeInput = () => screen.findByLabelText('Authenticator app code');
const submit = () => screen.getByRole('button', { name: /^(Confirm and Purge|Waiting|Purging)/ });

describe('PurgeMessagesModal: dangerous-action stage', () => {
  // Mutation: showDangerousChallenge sets stage 'softlock' instead of 'mfa', the intro is swapped, or isCredentialStage drops 'mfa'.
  it('a D1 mfa_required opens the MFA stage: heading focused, its own intro, a code and no password', async () => {
    scriptedPurge(CHANNEL_PATH, [refuse(D1_MFA)]);
    await startChannelPurge();

    expect(await screen.findByRole('heading', { name: 'Confirm it is you' })).toHaveFocus();
    expect(screen.getByText(DANGEROUS_INTRO)).toBeInTheDocument();
    expect(await codeInput()).toBeInTheDocument();
    // Not the DM stage (its intro and its password field) and not a result.
    expect(screen.queryByText(/confirm your identity first/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(submit()).toHaveAttribute('aria-disabled', 'true');
  });

  // The `none` leg: no D1 gate reads a password, so none is asked for, even
  // when the read lists no method to pair a code with.
  // Mutation: dangerousFactor's passwordLeg FACTOR_ONLY_LEG becomes LEG_ONLY_WITHOUT_MFA.
  it('never asks for a password, whatever the read lists', async () => {
    server.use(readAnswers([]));
    scriptedPurge(CHANNEL_PATH, [refuse(D1_MFA)]);
    await startChannelPurge();

    await screen.findByRole('heading', { name: 'Confirm it is you' });
    await waitFor(() => expect(screen.queryByText(/checking your verification/i)).toBeNull());
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  // Mutation: applyOutcome stops opening a stage for a dangerousChallenge result.
  it('re-sends the same range with mfa_code only, then reports the purge', async () => {
    const bodies = scriptedPurge(CHANNEL_PATH, [refuse(D1_MFA), purged(4)]);
    const user = await startChannelPurge();

    await user.type(await codeInput(), CODE);
    await user.click(submit());

    expect(await screen.findByText('Purged 4 messages.')).toBeInTheDocument();
    expect(bodies).toEqual([
      { range: '7d', include_pinned: false },
      { range: '7d', include_pinned: false, mfa_code: CODE },
    ]);
  });

  // Mutation: mapForbidden's self-purge guard drops 'server'.
  it('the server route re-sends to the server endpoint', async () => {
    const bodies = scriptedPurge(SERVER_PATH, [refuse(D1_MFA), purged(0)]);
    const user = await startServerPurge();

    await user.type(await codeInput(), CODE);
    await user.click(submit());

    expect(await screen.findByText(/Messages purged\./)).toBeInTheDocument();
    expect(bodies).toEqual([
      { range: '7d', include_pinned: false },
      { range: '7d', include_pinned: false, mfa_code: CODE },
    ]);
  });

  // The route's purpose is the only place a security-key token is accepted, so
  // a wrong one fails in production while every code test passes.
  // Mutation: selfPurgePurpose swaps its channel and server purposes.
  it.each([
    ['channel', CHANNEL_PATH, startChannelPurge, 'messages.channel_purge'],
    ['server', SERVER_PATH, startServerPurge, 'messages.server_purge'],
  ] as const)(
    'a security key on the %s route is begun for that route’s purpose',
    async (_route, path, start, purpose) => {
      server.use(readAnswers(['webauthn']));
      let begun: unknown;
      server.use(
        http.post(BEGIN_PATH, async ({ request }) => {
          begun = await request.json();
          return HttpResponse.json({ error: 'stop here' }, { status: 500 });
        })
      );
      scriptedPurge(path, [refuse({ ...D1_MFA, methods: ['webauthn'] }), purged(1)]);
      const user = await start();

      await screen.findByText('Passkey or security key');
      await user.click(submit());

      await waitFor(() => expect(begun).toEqual({ purpose }));
    }
  );

  // Mutation: dangerousChallenge() answers any 403 as a challenge (the wrong-code 403 reopens the stage).
  it('a wrong code stays on the stage and is worded in place', async () => {
    scriptedPurge(CHANNEL_PATH, [refuse(D1_MFA), refuse({ error: 'Invalid MFA code' })]);
    const user = await startChannelPurge();
    await user.type(await codeInput(), CODE);
    await user.click(submit());

    expect(await screen.findByText(/That code didn't work/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
    expect(submit()).toHaveAttribute('aria-disabled', 'true');
  });

  // The gate's own answer to the retry is the stage's to word: an enrolment
  // answer ends it in the enrolment state, as on the soft-lock.
  // Mutation: toSubmitOutcome's 'dangerousChallenge' case is removed.
  it('an enrolment answer to the retry ends the stage in the enrolment state', async () => {
    scriptedPurge(CHANNEL_PATH, [refuse(D1_MFA), refuse(D1_ENROLL)]);
    const user = await startChannelPurge();
    await user.type(await codeInput(), CODE);
    await user.click(submit());

    expect(await screen.findByText(ENROLLMENT_COPY)).toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getAllByRole('heading', { name: 'Confirm it is you' })).toHaveLength(1);
  });

  // Mutation: dangerousChallenge() answers any 403 as a challenge.
  it('a plain 403 stays a result: no stage opens', async () => {
    scriptedPurge(CHANNEL_PATH, [refuse({ error: 'forbidden' })]);
    await startChannelPurge();

    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Confirm it is you' })).not.toBeInTheDocument();
  });

  // Mutation: the mfa stage's onCancel becomes a no-op.
  it('Cancel closes the dialog without sending again', async () => {
    const onClose = vi.fn();
    const bodies = scriptedPurge(CHANNEL_PATH, [refuse(D1_MFA)]);
    const user = await startChannelPurge(onClose);
    await codeInput();

    await user.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(onClose).toHaveBeenCalledTimes(1);
    expect(bodies).toHaveLength(1);
  });
});

const BUDGET_SPENT_COPY = 'Too many attempts. Try again in a few minutes.';
const UNAVAILABLE_COPY = "We couldn't confirm that right now. Try again.";

const budgetSpent = () =>
  HttpResponse.json(
    { error: 'Too many verification attempts', step_up_budget_exhausted: true },
    { status: 429, headers: { 'Retry-After': '300' } }
  );
const gateBusy = () =>
  HttpResponse.json(
    { error: 'Another change is in progress', lock_conflict: true },
    { status: 503, headers: { 'Retry-After': '1' } }
  );
const budgetUnavailable = () =>
  HttpResponse.json(
    { error: 'Verification is unavailable', step_up_budget_unavailable: true },
    { status: 503 }
  );

// §3.3: a 429 or 503 the gate flags is an answer about the confirmation, not a purge result. The
// stage keeps the range the user chose and words it in place, so nothing is lost to a result stage.
describe('PurgeMessagesModal: dangerous-action stage, answers about the confirmation', () => {
  // Mutation: dangerousAnswer returns null for 'verificationLimited' (the stage ends in a result), or
  // the banner is not rendered (no alert), or the stage's primary ignores `dangerousLocked`.
  it('a spent verification budget keeps the stage: one banner, the primary locked, Cancel focused', async () => {
    const bodies = scriptedPurge(CHANNEL_PATH, [refuse(D1_MFA), budgetSpent]);
    const user = await startChannelPurge();
    await user.type(await codeInput(), CODE);
    await user.click(submit());

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(BUDGET_SPENT_COPY);
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    // Still the credential stage: no result, no "nothing was purged" sentence.
    expect(screen.getByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
    expect(submit()).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    expect(bodies).toHaveLength(2);
  });

  // Mutation: the click guard (`if (!dangerousLocked)`) is removed: the aria-disabled primary
  // still acts, and the held code is sent into a budget that cannot pass.
  it('a locked primary sends nothing when pressed, by click or by Enter in the field', async () => {
    const bodies = scriptedPurge(CHANNEL_PATH, [refuse(D1_MFA), budgetSpent]);
    const user = await startChannelPurge();
    await user.type(await codeInput(), CODE);
    await user.click(submit());
    await screen.findByRole('alert');

    // The budget answered before reading anything, so the code is still typed (C30).
    await user.click(submit());
    await user.type(screen.getByLabelText('Authenticator app code'), '{Enter}');

    expect(bodies).toHaveLength(2);
    expect(screen.getAllByRole('alert')).toHaveLength(1);
  });

  // Mutation: dangerousAnswer returns null for 'unavailable' (the stage ends in a result and the range is lost).
  it.each([
    ['a busy gate (lock_conflict)', gateBusy],
    ['an unavailable budget (step_up_budget_unavailable)', budgetUnavailable],
  ])('%s keeps the stage with a live retry that re-sends the same range', async (_name, answer) => {
    const bodies = scriptedPurge(CHANNEL_PATH, [refuse(D1_MFA), answer, purged(3)]);
    const user = await startChannelPurge();
    await user.type(await codeInput(), CODE);
    await user.click(submit());

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(UNAVAILABLE_COPY);
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(screen.getByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
    expect(screen.queryByText(/Temporarily unavailable/)).not.toBeInTheDocument();
    // Nothing locks: a retry is one click away.
    expect(submit()).toHaveFocus();

    await user.type(screen.getByLabelText('Authenticator app code'), CODE);
    await user.click(submit());

    expect(await screen.findByText('Purged 3 messages.')).toBeInTheDocument();
    expect(bodies).toEqual([
      { range: '7d', include_pinned: false },
      { range: '7d', include_pinned: false, mfa_code: CODE },
      { range: '7d', include_pinned: false, mfa_code: CODE },
    ]);
  });

  // Mutation: setDangerBanner stops running on an answer that is not a banner (the stale sentence stands).
  it('the next answer replaces the banner, so two sentences never stand together', async () => {
    scriptedPurge(CHANNEL_PATH, [
      refuse(D1_MFA),
      budgetUnavailable,
      refuse({ error: 'Invalid MFA code' }),
    ]);
    const user = await startChannelPurge();
    await user.type(await codeInput(), CODE);
    await user.click(submit());
    expect(await screen.findByText(UNAVAILABLE_COPY)).toBeInTheDocument();

    await user.type(screen.getByLabelText('Authenticator app code'), CODE);
    await user.click(submit());

    expect(await screen.findByText(/That code didn't work/)).toBeInTheDocument();
    expect(screen.queryByText(UNAVAILABLE_COPY)).not.toBeInTheDocument();
    expect(screen.getAllByRole('alert')).toHaveLength(1);
  });

  // The route's own limiter is not the gate's: its answer is the purge result, with its countdown.
  // Mutation: dangerousAnswer reads any 429 as the budget.
  it('an unflagged 429 is still the purge limit, a result with its countdown', async () => {
    scriptedPurge(CHANNEL_PATH, [
      refuse(D1_MFA),
      () =>
        HttpResponse.json(
          { error: 'Rate limit exceeded' },
          { status: 429, headers: { 'Retry-After': '900' } }
        ),
    ]);
    const user = await startChannelPurge();
    await user.type(await codeInput(), CODE);
    await user.click(submit());

    expect(await screen.findByRole('alert')).toHaveTextContent('Purge limit reached');
    expect(screen.queryByText(BUDGET_SPENT_COPY)).not.toBeInTheDocument();
  });
});

describe('PurgeMessagesModal: dangerous-action stage, lifecycle', () => {
  // Mutation: runPurge's isChallenge() stops counting dangerousChallenge (a challenge for another server opens a stage).
  it('a D1 challenge answered after a server switch opens no stage', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let requests = 0;
    server.use(
      http.delete(CHANNEL_PATH, async () => {
        requests += 1;
        await gate;
        return HttpResponse.json(D1_MFA, { status: 403 });
      })
    );
    await startChannelPurge();
    await waitFor(() => expect(requests).toBe(1));

    setRuntimeServerBase('https://other-server.example.test');
    release();

    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeEnabled()
    );
    expect(screen.queryByRole('heading', { name: 'Confirm it is you' })).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
  });

  // Mutation: `submitting` stops counting dangerousFactor.phase (the dialog can be dismissed mid-exchange).
  it('withdraws every dismiss affordance while the code is in flight', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let requests = 0;
    server.use(
      http.delete(CHANNEL_PATH, async () => {
        requests += 1;
        if (requests === 1) return HttpResponse.json(D1_MFA, { status: 403 });
        await gate;
        return HttpResponse.json({ deleted_count: 2, hidden_count: 0 });
      })
    );
    const user = await startChannelPurge();
    await user.type(await codeInput(), CODE);
    await user.click(submit());

    await waitFor(() => expect(requests).toBe(2));
    expect(screen.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument();
    release();

    expect(await screen.findByText('Purged 2 messages.')).toBeInTheDocument();
  });
});

describe('PurgeMessagesModal: routing between the gates', () => {
  // Mutation: the `!isDeleteRateLimited` test is inverted.
  it('a 403 WITH delete_rate_limited still opens the soft-lock stage, not the MFA stage', async () => {
    scriptedPurge(CHANNEL_PATH, [refuse({ ...D1_MFA, delete_rate_limited: true })]);
    await startChannelPurge();

    expect(
      await screen.findByText(
        "You've deleted several messages quickly. Confirm it's you to keep going."
      )
    ).toBeInTheDocument();
    expect(screen.queryByText(DANGEROUS_INTRO)).not.toBeInTheDocument();
  });

  // The soft-lock stage is not the dangerous-action stage (spec §3.3 leaves it alone): the same
  // flagged answers still end in a result there, with the range gone.
  // Mutation: dangerousAnswer is applied to every stage, not only the dangerous-action one.
  it.each([
    ['429 budget', budgetSpent, 'Too many verification attempts. Nothing was purged.'],
    ['503 busy gate', gateBusy, 'Temporarily unavailable. Try again shortly.'],
  ])('the soft-lock stage still ends in a result on a flagged %s', async (_name, answer, copy) => {
    scriptedPurge(CHANNEL_PATH, [refuse({ ...D1_MFA, delete_rate_limited: true }), answer]);
    const user = await startChannelPurge();
    await user.type(await codeInput(), CODE);
    await user.click(submit());

    expect(await screen.findByRole('alert')).toHaveTextContent(copy);
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'Done' })).toBeInTheDocument();
    expect(screen.queryByText(BUDGET_SPENT_COPY)).not.toBeInTheDocument();
    expect(screen.queryByText(UNAVAILABLE_COPY)).not.toBeInTheDocument();
  });

  // Mutation: mapForbidden's channel/server guard is removed (`if (true)`).
  it('a DM purge refused with mfa_required keeps the DM step-up stage and its password field', async () => {
    // Protection off locally, on at the server: the code-less purge is what draws the 403.
    usePrivacyStore.setState((st) => ({
      settings: { ...st.settings, requireAuthBeforePurge: false },
    }));
    server.use(readAnswers([]));
    let sent = 0;
    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', () => {
        sent += 1;
        return HttpResponse.json(D1_MFA, { status: 403 });
      })
    );
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={() => {}} />
    );
    await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));

    expect(await screen.findByText(/confirm your identity first/)).toBeInTheDocument();
    expect(sent).toBe(1);
    expect(screen.getByLabelText('Password')).toBeInTheDocument();
    expect(screen.queryByText(DANGEROUS_INTRO)).not.toBeInTheDocument();
  });
});

describe('PurgeMessagesModal: set up verification', () => {
  // Mutation: dangerousChallenge() drops 'enrollmentRequired', dangerousFactor loses its seed, or the mfa link is removed.
  it('a D1 enrolment refusal ends in the enrolment state with the link, and no retry', async () => {
    scriptedPurge(CHANNEL_PATH, [refuse(D1_ENROLL)]);
    await startChannelPurge();

    expect(await screen.findByText(ENROLLMENT_COPY)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Set up verification' })).toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  // Mutation: the mfa stage's onSetUpVerification is removed, closeHost becomes a no-op, or returnTo is not 'chat'.
  it('the D1 stage’s link closes the dialog and opens setup, returning to chat', async () => {
    const onClose = vi.fn();
    const bodies = scriptedPurge(CHANNEL_PATH, [refuse(D1_ENROLL)]);
    const user = await startChannelPurge(onClose);

    await user.click(await screen.findByRole('button', { name: 'Set up verification' }));

    await waitFor(() => expect(useSettingsOverlayStore.getState().open).toBe('app'));
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    expect(onClose).toHaveBeenCalledTimes(1);
    // Abandoned, never retried (§3.6a).
    expect(bodies).toHaveLength(1);
  });

  // Mutation: the softlock stage's onSetUpVerification is removed, closeHost becomes a no-op, or returnTo is not 'chat'.
  it('the soft-lock stage offers the same link in its enrolment state', async () => {
    const onClose = vi.fn();
    scriptedPurge(CHANNEL_PATH, [refuse(SOFT_LOCK_ENROLL)]);
    const user = await startChannelPurge(onClose);

    expect(await screen.findByText(ENROLLMENT_COPY)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Set up verification' }));

    await waitFor(() => expect(useSettingsOverlayStore.getState().open).toBe('app'));
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
