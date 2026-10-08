import { useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, userEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import { installStepUpApi, jsonResponse, readOffers } from '../../../helpers/stepUpApi';

// The shared dangerous-action step-up dialog (#3456 §3.3, wave 1). The host's
// request is a fake `send`; the stage, the factor hook and the adapter are real,
// and only `apiFetch` (the requirements read) is replaced, by path, as the
// neighbouring step-up dialog tests do.
//
// "Mutant:" comments name the production change each case exists to turn red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

import DangerousActionStepUpDialog, {
  type DangerousActionSendResult,
  type DangerousActionStepUpDialogProps,
} from '@/renderer/components/Auth/DangerousActionStepUpDialog';
import type { StepUpFactorRefusal } from '@/renderer/hooks/auth/useStepUpFactor';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

// Named fixtures: the pre-commit detect-secrets hook flags credential-shaped keys.
const FIXTURE_OTP = '314159';
const FIXTURE_OTP_2 = '271828';

const CODE_LABEL = 'Authenticator app code';
const PRIMARY = 'Delete Channel';
const BUSY = 'Deleting…';
const INTRO = 'Deleting #general removes its messages for everyone.';
const HOST_TEXT = 'The host could not delete that channel.';
const SESSION_TEXT = 'Your session ended. Sign in again to delete the channel.';

const RATE_LIMITED = 'Too many attempts. Try again in a few minutes.';
const UNAVAILABLE = "We couldn't confirm that right now. Try again.";
const NETWORK = "Couldn't reach the server. Check your connection and try again.";
const ENROLMENT = 'Set up an authenticator app or security key in Settings to do this.';

const TOTP_SEED: StepUpFactorRefusal = { kind: 'mfaRequired', methods: ['totp'] };

type Send = DangerousActionStepUpDialogProps['send'];
const ok = (): DangerousActionSendResult => ({ kind: 'ok' });
const refused = (status: number, body: unknown = {}): DangerousActionSendResult => ({
  kind: 'refused',
  status,
  body,
});

const onSuccess = vi.fn();
const onClose = vi.fn();
const describeFailure = vi.fn((_status: number, _body: unknown) => HOST_TEXT);
const focusFallback = vi.fn((): HTMLElement | null => null);

function renderDialog(send: Send, props: Partial<DangerousActionStepUpDialogProps> = {}) {
  return render(
    <DangerousActionStepUpDialog
      isOpen
      purpose="channels.delete"
      seed={TOTP_SEED}
      intro={INTRO}
      primaryLabel={PRIMARY}
      busyLabel={BUSY}
      send={send}
      describeFailure={describeFailure}
      sessionMessage={SESSION_TEXT}
      onSuccess={onSuccess}
      onClose={onClose}
      focusFallback={focusFallback}
      {...props}
    />
  );
}

const primary = () => screen.getByRole('button', { name: PRIMARY });
const cancel = () => screen.getByRole('button', { name: 'Cancel' });
const codeField = () => screen.findByLabelText(CODE_LABEL) as Promise<HTMLInputElement>;
const alerts = () => screen.queryAllByRole('alert');

/** Types a code and presses the primary; resolves once `send` has been called `n` times. */
async function submitCode(send: ReturnType<typeof vi.fn>, code = FIXTURE_OTP, n = 1) {
  await userEvent.type(await codeField(), code);
  await userEvent.click(primary());
  await waitFor(() => expect(send).toHaveBeenCalledTimes(n));
}

function switchAccount(): void {
  useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
}

beforeEach(() => {
  resetAllStores();
  mockApiFetch.mockReset();
  installStepUpApi(mockApiFetch, {
    read: () => readOffers(['totp'], true),
    route: () => jsonResponse(200),
  });
  onSuccess.mockReset();
  onClose.mockReset();
  describeFailure.mockClear();
  focusFallback.mockReset().mockReturnValue(null);
  useUserStore.setState({ user: { id: 'acct-1' } as never });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('what it shows', () => {
  // Mutant: the title, the intro or its aria-describedby binding dropped.
  it('names the action in the intro and describes the dialog by it', async () => {
    renderDialog(vi.fn<Send>());
    await codeField();

    const dialog = screen.getByRole('dialog', { name: "Confirm it's you" });
    expect(dialog).toHaveAccessibleDescription(INTRO);
    expect(primary()).toBeInTheDocument();
  });

  // Mutant: `isOpen` ignored, or the stage mounted while closed.
  it('renders nothing while closed', () => {
    renderDialog(vi.fn<Send>(), { isOpen: false });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  // Mutant: `passwordLeg` changed from FACTOR_ONLY_LEG: a password field no D1 gate reads (#3456 V18).
  it.each([
    ['an authenticator account', ['totp']],
    ['an account whose read offers no inline method', []],
  ])('never shows a password field for %s', async (_name, offers) => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(offers),
      route: () => jsonResponse(200),
    });
    renderDialog(vi.fn<Send>(), { seed: null });

    await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(1));
    await act(async () => undefined);
    if (offers.length > 0) await codeField();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  // Mutant: `allowBackup: false`.
  it('offers a backup code when the read reports one', async () => {
    renderDialog(vi.fn<Send>(), { seed: { kind: 'mfaRequired', methods: null } });

    expect(
      await screen.findByRole('button', { name: 'Use a backup code instead' })
    ).toBeInTheDocument();
  });
});

describe('opening', () => {
  // Mutant: the seed passed as `null` regardless of the prop (a failed read would then leave no field).
  it('a failed read keeps the set the seed named', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => jsonResponse(503),
      route: () => jsonResponse(200),
    });
    renderDialog(vi.fn<Send>());

    expect(await codeField()).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: 'Use a backup code instead' })
    ).not.toBeInTheDocument();
  });

  // Mutant: the seed ignored for a proactive dialog's read, or the read skipped.
  it('a proactive dialog reads the requirements and then shows the code field', async () => {
    renderDialog(vi.fn<Send>(), { seed: null });

    expect(await codeField()).toBeInTheDocument();
    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/mfa/step-up', expect.anything());
  });

  // Mutant: `focusOnReady` dropped: a keyboard user tabs past the field the read just put up.
  it.each([
    ['a seed that names the set', TOTP_SEED],
    ['a seed that names none', { kind: 'mfaRequired', methods: null } as StepUpFactorRefusal],
    ['no seed', null],
  ])('focuses the code field when the opening read lands, for %s', async (_name, seed) => {
    renderDialog(vi.fn<Send>(), { seed });

    const code = await codeField();

    await waitFor(() => expect(code).toHaveFocus());
  });
});

describe('a code that passes', () => {
  // Mutant: the typed code not passed through, or passed under another position.
  it('sends the code once and succeeds once', async () => {
    const send = vi.fn<Send>(async () => ok());
    renderDialog(send);

    await submitCode(send);

    expect(send).toHaveBeenCalledWith(
      FIXTURE_OTP,
      expect.objectContaining({ authLifecycle: expect.anything() })
    );
    await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
    expect(onClose).not.toHaveBeenCalled();
    expect(alerts()).toHaveLength(0);
  });

  // Mutant: `activate` bypassing the guard, so an empty code sends.
  it('sends nothing while the code is empty, and says what is missing', async () => {
    const send = vi.fn<Send>(async () => ok());
    renderDialog(send);
    await codeField();

    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(primary());

    expect(send).not.toHaveBeenCalled();
    expect(
      await screen.findByText('Enter the 6-digit code from your authenticator app to continue.')
    ).toBeInTheDocument();
  });

  // Mutant: the context passed to `send` captured later than the stage's own (a switch would go unseen).
  it('admits the request against the account that was current when it was sent', async () => {
    const send = vi.fn<Send>(async () => ok());
    renderDialog(send);
    const generation = useAuthStore.getState().authGeneration;

    await submitCode(send);

    const [, context] = send.mock.calls[0];
    expect(context.authLifecycle.authGeneration).toBe(generation);
  });

  // Mutant: the apiRequestContextIsCurrent check removed before `onSuccess`; or the
  // hook's stale-answer arm returning to a live code field (a second code for an
  // action that may already have gone through).
  it('an ok that lands after an account switch never succeeds, and ends the stage', async () => {
    const gate = deferred<DangerousActionSendResult>();
    const send = vi.fn<Send>(() => gate.promise);
    renderDialog(send);
    await submitCode(send);

    switchAccount();
    await act(async () => {
      gate.resolve(ok());
      await gate.promise;
    });

    expect(await screen.findByText(SESSION_TEXT)).toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(onSuccess).not.toHaveBeenCalled();
    expect(alerts()).toHaveLength(0);
  });

  // Mutant: `capture` not passed to `stepUpActivation` (the re-send then goes out
  // as whoever opened the dialog, not as the account the first send went out as).
  it("sends nothing once the first send's account is no longer current", async () => {
    const capture = captureApiRequestContext();
    switchAccount();
    const send = vi.fn<Send>(async () => ok());
    renderDialog(send, { capture });

    await userEvent.type(await codeField(), FIXTURE_OTP);
    await userEvent.click(primary());

    expect(await screen.findByText(SESSION_TEXT)).toBeInTheDocument();
    expect(send).not.toHaveBeenCalled();
    expect(onSuccess).not.toHaveBeenCalled();
  });

  // Mutant: the capture taken but the send admitted against the dialog's own.
  it("admits the re-send against the first send's capture", async () => {
    const capture = captureApiRequestContext();
    const send = vi.fn<Send>(async () => ok());
    renderDialog(send, { capture });

    await submitCode(send);

    expect(send.mock.calls[0][1]).toBe(capture);
  });

  // Mutant: busyLabel not shown, or the primary left clickable while the request is out.
  it('shows the busy label and sends once however often the primary is pressed', async () => {
    const gate = deferred<DangerousActionSendResult>();
    const send = vi.fn<Send>(() => gate.promise);
    renderDialog(send);
    await submitCode(send);

    const busy = await screen.findByRole('button', { name: BUSY });
    expect(busy).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(busy);
    expect(send).toHaveBeenCalledTimes(1);

    gate.resolve(ok());
    await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
  });
});

describe('while the request is out (D-1)', () => {
  // Mutant: `disabled={submitting}` dropped from Cancel (or made aria-disabled, which stays clickable).
  it('Cancel is natively disabled and does nothing', async () => {
    const gate = deferred<DangerousActionSendResult>();
    const send = vi.fn<Send>(() => gate.promise);
    renderDialog(send);
    await submitCode(send);

    await screen.findByRole('button', { name: BUSY });
    expect(cancel()).toBeDisabled();
    await userEvent.click(cancel());
    expect(onClose).not.toHaveBeenCalled();

    gate.resolve(ok());
    await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
  });

  // Mutant: the layout effect that moves focus off Cancel as it goes natively disabled
  // removed (Chromium would drop that focus to <body>).
  it('moves focus from Cancel to the primary when the request goes out', async () => {
    const gate = deferred<DangerousActionSendResult>();
    const send = vi.fn<Send>(() => gate.promise);
    renderDialog(send);
    await userEvent.type(await codeField(), FIXTURE_OTP);

    cancel().focus();
    act(() => primary().click());

    const busy = await screen.findByRole('button', { name: BUSY });
    expect(document.activeElement).toBe(busy);

    gate.resolve(ok());
    await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
  });

  // Mutant: `dismissable={!submitting}` replaced by `dismissable`.
  it('Escape and the close button are gone until the answer lands', async () => {
    const gate = deferred<DangerousActionSendResult>();
    const send = vi.fn<Send>(() => gate.promise);
    renderDialog(send);
    await submitCode(send);
    await screen.findByRole('button', { name: BUSY });

    await userEvent.keyboard('{Escape}');

    expect(onClose).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument();

    gate.resolve(refused(503, { lock_conflict: true }));
    await screen.findByText(UNAVAILABLE);
    expect(cancel()).toBeEnabled();
  });

  // Positive control for the two cases above: idle, the same keys and buttons DO close.
  it('when idle, Cancel, Escape and the close button all close', async () => {
    renderDialog(vi.fn<Send>());
    await codeField();

    await userEvent.click(cancel());
    await userEvent.keyboard('{Escape}');
    await userEvent.click(screen.getByRole('button', { name: 'Close' }));

    expect(onClose).toHaveBeenCalledTimes(3);
  });
});

describe('a spent attempt budget: 429 step_up_budget_exhausted (D-8)', () => {
  const exhausted = () => refused(429, { step_up_budget_exhausted: true });

  // Mutant: refusalBanner's rateLimited arm returning '' or the unavailable text.
  it('says to wait, once, and keeps the code the server never looked at', async () => {
    const send = vi.fn<Send>(async () => exhausted());
    renderDialog(send);
    await submitCode(send);

    expect(await screen.findByText(RATE_LIMITED)).toBeInTheDocument();
    expect(alerts()).toHaveLength(1);
    expect(await codeField()).toHaveValue(FIXTURE_OTP);
    expect(describeFailure).not.toHaveBeenCalled();
    expect(onSuccess).not.toHaveBeenCalled();
  });

  // Mutant: `locks` not computed for rateLimited, or `setLocked(true)` dropped.
  it('locks the primary: aria-disabled, and a click sends nothing more', async () => {
    const send = vi.fn<Send>(async () => exhausted());
    renderDialog(send);
    await submitCode(send);
    await screen.findByText(RATE_LIMITED);

    // The code is intact, so only the lock keeps the primary down.
    expect(await codeField()).toHaveValue(FIXTURE_OTP);
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(primary());

    expect(send).toHaveBeenCalledTimes(1);
  });

  // Mutant: `activate` no longer guarded by `locked`; or `ariaDisabled` ignoring `locked`.
  it('stays locked through Enter in the field and through retyping', async () => {
    const send = vi.fn<Send>(async () => exhausted());
    renderDialog(send);
    await submitCode(send);
    await screen.findByText(RATE_LIMITED);

    const field = await codeField();
    await userEvent.clear(field);
    await userEvent.type(field, `${FIXTURE_OTP_2}{Enter}`);

    expect(send).toHaveBeenCalledTimes(1);
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
  });

  // Mutant: the layout effect choosing the primary for every banner.
  it('moves focus to Cancel, since there is no retry to reach', async () => {
    const send = vi.fn<Send>(async () => exhausted());
    renderDialog(send);
    await submitCode(send);
    await screen.findByText(RATE_LIMITED);

    await waitFor(() => expect(cancel()).toHaveFocus());
  });

  // Mutant: the lock held above the Stage (it would survive a close and reopen).
  it('a reopened dialog starts unlocked', async () => {
    const send = vi.fn<Send>(async () => exhausted());
    const { rerender } = renderDialog(send);
    await submitCode(send);
    await screen.findByText(RATE_LIMITED);

    const closed = { isOpen: false } as const;
    const props = {
      purpose: 'channels.delete',
      seed: TOTP_SEED,
      intro: INTRO,
      primaryLabel: PRIMARY,
      busyLabel: BUSY,
      send,
      describeFailure,
      onSuccess,
      onClose,
      focusFallback,
    } as const;
    rerender(<DangerousActionStepUpDialog {...props} {...closed} />);
    rerender(<DangerousActionStepUpDialog {...props} isOpen />);

    await userEvent.type(await codeField(), FIXTURE_OTP_2);
    expect(primary()).not.toHaveAttribute('aria-disabled');
    expect(alerts()).toHaveLength(0);
  });
});

describe('503 with lock_conflict or step_up_budget_unavailable', () => {
  // Mutant: refusalBanner's unavailable arm returning '' or the rate-limit text.
  it.each([
    ['lock_conflict', { lock_conflict: true }],
    ['step_up_budget_unavailable', { step_up_budget_unavailable: true }],
  ])('%s says it could not confirm, once', async (_name, body) => {
    const send = vi.fn<Send>(async () => refused(503, body));
    renderDialog(send);
    await submitCode(send);

    expect(await screen.findByText(UNAVAILABLE)).toBeInTheDocument();
    expect(alerts()).toHaveLength(1);
    expect(describeFailure).not.toHaveBeenCalled();
  });

  // Mutant: `locks` true for every refusal banner: focus would go to Cancel and the retry stay down.
  it('focuses the primary and does not lock: the next code goes out', async () => {
    const send = vi.fn<Send>(async () => refused(503, { lock_conflict: true }));
    renderDialog(send);
    await submitCode(send);
    await screen.findByText(UNAVAILABLE);

    await waitFor(() => expect(primary()).toHaveFocus());
    await userEvent.type(await codeField(), FIXTURE_OTP_2);
    expect(primary()).not.toHaveAttribute('aria-disabled');
    await userEvent.click(primary());

    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls[1][0]).toBe(FIXTURE_OTP_2);
  });

  // Mutant: the banner element not keyed by answer: a repeated sentence is not announced again.
  it('a repeated answer is a new alert, so it is announced again', async () => {
    const send = vi.fn<Send>(async () => refused(503, { lock_conflict: true }));
    renderDialog(send);
    await submitCode(send);
    const first = await screen.findByRole('alert');

    await userEvent.type(await codeField(), FIXTURE_OTP_2);
    await userEvent.click(primary());
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));

    await waitFor(() => expect(screen.getByRole('alert')).not.toBe(first));
    expect(alerts()).toHaveLength(1);
  });
});

describe('an answer the dialog does not own', () => {
  // Mutant: the adapter treating an unflagged 429 as the budget's: the host's own limiter would lock the dialog.
  it('an unflagged 429 shows the host sentence and does not lock', async () => {
    const body = { error: 'Rate limit exceeded' };
    const send = vi.fn<Send>(async () => refused(429, body));
    renderDialog(send);
    await submitCode(send);

    expect(await screen.findByText(HOST_TEXT)).toBeInTheDocument();
    expect(describeFailure).toHaveBeenCalledWith(429, body);
    expect(screen.queryByText(RATE_LIMITED)).not.toBeInTheDocument();
    expect(alerts()).toHaveLength(1);

    // A banner that does not lock leaves the retry one click away.
    await waitFor(() => expect(primary()).toHaveFocus());
    await userEvent.type(await codeField(), FIXTURE_OTP_2);
    expect(primary()).not.toHaveAttribute('aria-disabled');
  });

  // Mutant: the `refusal === null` arm returning `refusal` outcomes, or skipping describeFailure.
  it.each([
    ['an unflagged 503', 503],
    ['a 500', 500],
    ['a 404', 404],
  ])('%s shows the host sentence with the status and body it got', async (_name, status) => {
    const send = vi.fn<Send>(async () => refused(status, null));
    renderDialog(send);
    await submitCode(send);

    expect(await screen.findByText(HOST_TEXT)).toBeInTheDocument();
    expect(describeFailure).toHaveBeenCalledWith(status, null);
    expect(onSuccess).not.toHaveBeenCalled();
  });

  // The code may be spent by an answer that is not a step-up one. Mutant: outcome 'aborted' (code kept).
  it('treats the code as spent: the field is empty and the primary waits for a new one', async () => {
    const send = vi.fn<Send>(async () => refused(500, null));
    renderDialog(send);
    await submitCode(send);
    await screen.findByText(HOST_TEXT);

    expect(await codeField()).toHaveValue('');
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
  });
});

describe('a dead session: 401', () => {
  // Mutant: refusalBanner returning text for sessionExpired (the sentence would be said twice, as an alert).
  it('ends in the terminal state: the host sentence once, no banner, no field', async () => {
    const send = vi.fn<Send>(async () => refused(401, { error: 'Unauthorized' }));
    renderDialog(send);
    await submitCode(send);

    expect(await screen.findByText(SESSION_TEXT)).toBeInTheDocument();
    expect(screen.getAllByText(SESSION_TEXT)).toHaveLength(1);
    expect(alerts()).toHaveLength(0);
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    expect(describeFailure).not.toHaveBeenCalled();
  });

  // Mutant: the sessionMessage prop not threaded to the stage (its default sentence would show).
  it('uses the stage default when the host gives no sentence', async () => {
    const send = vi.fn<Send>(async () => refused(401));
    renderDialog(send, { sessionMessage: undefined });
    await submitCode(send);

    expect(await screen.findByText('Sign in again to continue.')).toBeInTheDocument();
  });

  // Mutant: the terminal state leaving the primary live: a click would resend under a dead session.
  it('sends nothing more', async () => {
    const send = vi.fn<Send>(async () => refused(401));
    renderDialog(send);
    await submitCode(send);
    await screen.findByText(SESSION_TEXT);

    await userEvent.click(primary());

    expect(send).toHaveBeenCalledTimes(1);
  });
});

describe('enrolment required: 403 mfa_enrollment_required', () => {
  const enrolment = () => refused(403, { mfa_enrollment_required: true });

  // Mutant: refusalBanner giving the enrolment kind a banner, or the seed arm dropping it.
  it.each([
    ['from the first answer', { seed: TOTP_SEED, answered: true }],
    [
      'from a seed',
      { seed: { kind: 'enrollmentRequired' } as StepUpFactorRefusal, answered: false },
    ],
  ])('shows the enrolment sentence %s, with no field and no alert', async (_name, c) => {
    const send = vi.fn<Send>(async () => enrolment());
    renderDialog(send, { seed: c.seed });
    if (c.answered) await submitCode(send);

    expect(await screen.findByText(ENROLMENT)).toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(alerts()).toHaveLength(0);
    expect(send).toHaveBeenCalledTimes(c.answered ? 1 : 0);
  });

  // Mutant: onSetUpVerification not forwarded to the stage, or forwarded for every state.
  it('offers no link when the host gave no handler', async () => {
    const send = vi.fn<Send>(async () => enrolment());
    renderDialog(send);
    await submitCode(send);
    await screen.findByText(ENROLMENT);

    expect(screen.queryByRole('button', { name: 'Set up verification' })).not.toBeInTheDocument();
  });

  // Mutant: the dialog wrapping the handler in `() => { onClose(); handler(); }` (D-4).
  it('the link calls the handler once and does not close the dialog first (D-4)', async () => {
    const onSetUpVerification = vi.fn();
    const send = vi.fn<Send>(async () => enrolment());
    renderDialog(send, { onSetUpVerification });
    await submitCode(send);
    await screen.findByText(ENROLMENT);

    await userEvent.click(screen.getByRole('button', { name: 'Set up verification' }));

    expect(onSetUpVerification).toHaveBeenCalledTimes(1);
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  // Mutant: the link acting for an account the dialog was not opened for.
  it('the link does nothing for a dialog whose account has changed, and ends the stage', async () => {
    const onSetUpVerification = vi.fn();
    const send = vi.fn<Send>(async () => enrolment());
    renderDialog(send, { onSetUpVerification });
    await submitCode(send);
    await screen.findByText(ENROLMENT);

    switchAccount();
    await userEvent.click(screen.getByRole('button', { name: 'Set up verification' }));

    expect(onSetUpVerification).not.toHaveBeenCalled();
    expect(await screen.findByText(SESSION_TEXT)).toBeInTheDocument();
  });

  // Mutant: the link shown outside the enrolment state (a seeded TOTP dialog would offer it).
  it('offers the link only in the enrolment state', async () => {
    renderDialog(
      vi.fn<Send>(async () => ok()),
      { onSetUpVerification: vi.fn() }
    );
    await codeField();

    expect(screen.queryByRole('button', { name: 'Set up verification' })).not.toBeInTheDocument();
  });
});

describe('a refused code: 403 Invalid MFA code', () => {
  // Mutant: the adapter or hook dropping invalidMfaCode, or the banner set for it (a second alert).
  it('marks the field, empties it, and raises exactly one alert', async () => {
    const send = vi.fn<Send>(async () => refused(403, { error: 'Invalid MFA code' }));
    renderDialog(send);
    await submitCode(send);

    expect(await screen.findByText(/That code didn't work/)).toBeInTheDocument();
    const field = await codeField();
    expect(field).toHaveAttribute('aria-invalid', 'true');
    expect(field).toHaveValue('');
    expect(alerts()).toHaveLength(1);
    expect(describeFailure).not.toHaveBeenCalled();
    expect(onSuccess).not.toHaveBeenCalled();
  });

  // Mutant: the hook not clearing a spent code: it would be sent a second time.
  it('the next code goes out as typed', async () => {
    const send = vi.fn<Send>(async () => refused(403, { error: 'Invalid MFA code' }));
    renderDialog(send);
    await submitCode(send);
    await screen.findByText(/That code didn't work/);

    await submitCode(send, FIXTURE_OTP_2, 2);

    expect(send.mock.calls[1][0]).toBe(FIXTURE_OTP_2);
  });
});

describe('a request that did not complete', () => {
  // Mutant: networkBanner empty, or 'transport' mapped to 'aborted'.
  it('a transport result says the server could not be reached, once', async () => {
    const send = vi.fn<Send>(async () => ({ kind: 'transport' }));
    renderDialog(send);
    await submitCode(send);

    expect(await screen.findByText(NETWORK)).toBeInTheDocument();
    expect(alerts()).toHaveLength(1);
    expect(onSuccess).not.toHaveBeenCalled();
  });

  // Mutant: sendOnce not catching, so a thrown fetch leaves the dialog stuck submitting.
  it('a send that throws is read as a transport failure', async () => {
    const send = vi.fn<Send>(async () => {
      throw new TypeError('Failed to fetch');
    });
    renderDialog(send);
    await submitCode(send);

    expect(await screen.findByText(NETWORK)).toBeInTheDocument();
    expect(cancel()).toBeEnabled();
  });

  // Mutant: sendOnce reading every throw as an abort: a request that may have gone out looks unsent.
  it('a thrown AbortError is not shown as a network failure', async () => {
    const send = vi.fn<Send>(async () => {
      throw new DOMException('aborted', 'AbortError');
    });
    renderDialog(send);
    await submitCode(send);

    await waitFor(() => expect(cancel()).toBeEnabled());
    expect(screen.queryByText(NETWORK)).not.toBeInTheDocument();
    expect(await screen.findByText(UNAVAILABLE)).toBeInTheDocument();
  });

  // Mutant: 'aborted' given no banner (the click visibly does nothing), or the
  // code treated as spent (nothing was sent).
  it('an aborted result says to try again, once, and keeps the code', async () => {
    const send = vi.fn<Send>(async () => ({ kind: 'aborted' }));
    renderDialog(send);
    await submitCode(send);

    expect(await screen.findByText(UNAVAILABLE)).toBeInTheDocument();
    expect(alerts()).toHaveLength(1);
    expect(primary()).toHaveTextContent(PRIMARY);
    expect(await codeField()).toHaveValue(FIXTURE_OTP);
    expect(onSuccess).not.toHaveBeenCalled();
  });

  // Mutant: the aborted banner shown for a stale context too (beside the session sentence).
  it('an aborted result after an account switch shows no banner and ends the stage', async () => {
    const gate = deferred<DangerousActionSendResult>();
    const send = vi.fn<Send>(() => gate.promise);
    renderDialog(send);
    await submitCode(send);

    switchAccount();
    await act(async () => {
      gate.resolve({ kind: 'aborted' });
      await gate.promise;
    });

    expect(await screen.findByText(SESSION_TEXT)).toBeInTheDocument();
    expect(screen.queryByText(UNAVAILABLE)).not.toBeInTheDocument();
    expect(alerts()).toHaveLength(0);
  });

  // Mutant: `setBanner(null)` dropped from the activation: an earlier attempt's
  // banner stands beside a new attempt that has not answered yet.
  it("clears an earlier attempt's banner when the next attempt starts", async () => {
    const gate = deferred<DangerousActionSendResult>();
    const send = vi
      .fn<Send>()
      .mockResolvedValueOnce({ kind: 'transport' })
      .mockImplementationOnce(() => gate.promise);
    renderDialog(send);
    await submitCode(send);
    expect(await screen.findByText(NETWORK)).toBeInTheDocument();

    await submitCode(send, FIXTURE_OTP_2, 2);

    expect(screen.queryByText(NETWORK)).not.toBeInTheDocument();
    gate.resolve(ok());
    await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
  });
});

describe('closing focus', () => {
  /** A host with a trigger that may leave the document, and a landmark focus can fall back to. */
  function Harness({ send, removeTrigger }: Readonly<{ send: Send; removeTrigger: boolean }>) {
    const [open, setOpen] = useState(false);
    const [triggerShown, setTriggerShown] = useState(true);
    return (
      <>
        {triggerShown && (
          <button type="button" onClick={() => setOpen(true)}>
            Open
          </button>
        )}
        <button type="button" id="list">
          Channel list
        </button>
        <DangerousActionStepUpDialog
          isOpen={open}
          purpose="channels.delete"
          seed={TOTP_SEED}
          intro={INTRO}
          primaryLabel={PRIMARY}
          busyLabel={BUSY}
          send={send}
          describeFailure={describeFailure}
          onSuccess={onSuccess}
          onClose={() => {
            if (removeTrigger) setTriggerShown(false);
            setOpen(false);
          }}
          focusFallback={() => document.getElementById('list')}
        />
      </>
    );
  }

  // Mutant: the fallback effect deleted, or run only when the dialog was never open.
  it('with the trigger gone, focus lands on the fallback and never on <body>', async () => {
    render(<Harness send={vi.fn<Send>()} removeTrigger />);
    await userEvent.click(screen.getByRole('button', { name: 'Open' }));
    await codeField();

    await userEvent.click(cancel());

    await waitFor(() => expect(screen.getByRole('button', { name: 'Channel list' })).toHaveFocus());
    expect(document.body).not.toHaveFocus();
  });

  // Mutant: the fallback taken unconditionally, stealing focus from the element Modal just restored.
  it('with the trigger still there, focus returns to it and the fallback is left alone', async () => {
    render(<Harness send={vi.fn<Send>()} removeTrigger={false} />);
    await userEvent.click(screen.getByRole('button', { name: 'Open' }));
    await codeField();

    await userEvent.click(cancel());

    await waitFor(() => expect(screen.getByRole('button', { name: 'Open' })).toHaveFocus());
    expect(screen.getByRole('button', { name: 'Channel list' })).not.toHaveFocus();
  });

  // Mutant: the effect firing on mount (wasOpenRef initialised to false), pulling focus on first render.
  it('a dialog that never opened moves no focus', () => {
    render(<Harness send={vi.fn<Send>()} removeTrigger />);

    expect(screen.getByRole('button', { name: 'Open' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Channel list' })).not.toHaveFocus();
  });

  // Mutant: `focusFallback()?.focus()` without the optional chain.
  it('a fallback that finds nothing does not throw', async () => {
    const { rerender } = renderDialog(vi.fn<Send>(), { focusFallback });
    await codeField();

    expect(() =>
      rerender(
        <DangerousActionStepUpDialog
          isOpen={false}
          purpose="channels.delete"
          intro={INTRO}
          primaryLabel={PRIMARY}
          busyLabel={BUSY}
          send={vi.fn<Send>()}
          describeFailure={describeFailure}
          onSuccess={onSuccess}
          onClose={onClose}
          focusFallback={focusFallback}
        />
      )
    ).not.toThrow();
    expect(focusFallback).toHaveBeenCalled();
  });
});
