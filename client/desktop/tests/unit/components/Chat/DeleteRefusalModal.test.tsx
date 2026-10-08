import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { act, fireEvent, render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import DeleteRefusalModal, {
  type DeleteRefusalModalProps,
} from '@/renderer/components/Chat/DeleteRefusalModal';
import type { DeleteRefusalState } from '@/renderer/hooks/messaging/useChatController';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import type { StepUpSubmitOutcome } from '@/renderer/hooks/auth/useStepUpFactor';
import type { DeleteRefusalView } from '@/renderer/services/messaging/deleteRefusal';
import type { StepUpPurpose } from '@/renderer/components/Auth/stepUpPurpose';

// #3455 T7/T8/T9, rewritten for picker PR 3: the single-message delete refusal
// dialog hosts the shared StepUpCredentials stage. It is controlled by the
// hook's one refusal slot, so these tests drive it with a slot value, answer
// the factor hook's read with MSW, and assert what a person sees, types and
// hears. `onConfirm` resolves to the outcome the hook words in place.
//
// Removed on purpose (the state they pinned no longer exists): the slot's
// `submitting` and `promptKey`, and the confirm/password views' `error`. The
// input remount and the per-attempt copy now belong to the factor hook, and are
// pinned here through the outcomes `onConfirm` resolves to.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const FIXTURE_PW = 'hunter2-fixture';
const CODE = '123456';
const BACKUP = 'abcd1234';
const WEBAUTHN_TOKEN = 'webauthn-inline-token';
const READ_PATH = '*/api/v1/mfa/step-up';
const BEGIN_PATH = '*/api/v1/mfa/webauthn/verify-inline/begin';
const FINISH_PATH = '*/api/v1/mfa/webauthn/verify-inline/finish';
const ENROLLMENT_COPY = 'Set up an authenticator app or security key in Settings to do this.';

function slot(
  view: DeleteRefusalView,
  extra: Partial<DeleteRefusalState> = {}
): DeleteRefusalState {
  // The capture is the controller's, taken as the delete went out (C82).
  return {
    messageId: 'm1',
    view,
    openedAt: Date.now(),
    context: captureApiRequestContext(),
    ...extra,
  };
}

const CONFIRM: DeleteRefusalView = { view: 'confirm', methods: ['totp'] };
const PASSWORD: DeleteRefusalView = { view: 'password' };
const ENROLL: DeleteRefusalView = { view: 'enroll' };

/** What the account's step-up read answers. */
function readAnswers(methods: string[], backup = false) {
  return http.get(READ_PATH, () =>
    HttpResponse.json({
      methods,
      default_method: methods[0] ?? null,
      backup_code_available: backup,
    })
  );
}

const refusal = (r: Extract<StepUpSubmitOutcome, { kind: 'refusal' }>['refusal']) =>
  ({ kind: 'refusal', refusal: r }) as const;

type Confirm = ReturnType<typeof vi.fn<DeleteRefusalModalProps['onConfirm']>>;
const okConfirm = (): Confirm =>
  vi.fn<DeleteRefusalModalProps['onConfirm']>(async () => ({ kind: 'success' }));

interface Props {
  refusal: DeleteRefusalState | null;
  onConfirm?: DeleteRefusalModalProps['onConfirm'];
  onDismiss?: () => void;
  purpose?: StepUpPurpose;
  surfaceId?: string;
}

const SURFACE = 'surface-main';

function ui({
  refusal: state,
  onConfirm = okConfirm(),
  onDismiss = vi.fn(),
  purpose = 'messages.delete',
  surfaceId = SURFACE,
}: Props) {
  return (
    <DeleteRefusalModal
      refusal={state}
      onConfirm={onConfirm}
      onDismiss={onDismiss}
      purpose={purpose}
      surfaceId={surfaceId}
    />
  );
}

const totpInput = () => screen.findByLabelText('Authenticator app code');
async function typeCode(code = CODE) {
  await userEvent.setup().type(await totpInput(), code);
}

function confirmButton() {
  return screen.getByRole('button', { name: /^(Confirm|Waiting|Confirming)/ });
}
const confirmIsInert = () => expect(confirmButton()).toHaveAttribute('aria-disabled', 'true');
const confirmIsLive = () => expect(confirmButton()).not.toHaveAttribute('aria-disabled');

// Nodes a test appends to the document outside React; removed after each test.
const extraNodes: HTMLElement[] = [];

describe('DeleteRefusalModal', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    server.use(readAnswers(['totp']));
  });

  afterEach(() => {
    vi.useRealTimers();
    for (const el of extraNodes.splice(0)) el.remove();
  });

  describe('views', () => {
    it('renders nothing for an empty slot', () => {
      render(ui({ refusal: null }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('confirm: a code field inside a dialog titled for the person, with no error text', async () => {
      render(ui({ refusal: slot(CONFIRM) }));

      const dialog = screen.getByRole('dialog', { name: "Confirm it's you" });
      expect(dialog).toHaveTextContent(
        "You've deleted several messages quickly. Confirm it's you to keep going."
      );
      expect(await totpInput()).toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('password: a labelled password field, not a code field', async () => {
      server.use(readAnswers([]));
      render(ui({ refusal: slot(PASSWORD) }));

      expect(screen.getByRole('dialog', { name: "Confirm it's you" })).toBeInTheDocument();
      const field = await screen.findByLabelText('Password');
      expect(field).toHaveAttribute('type', 'password');
      expect(field).not.toHaveAttribute('aria-invalid');
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    });

    // E8 (Q5): the dialog keeps its name, so it does not change mid-flow.
    it('enroll: the dialog is still "Confirm it\'s you", and says what to do in Settings', async () => {
      render(ui({ refusal: slot(ENROLL) }));

      expect(screen.getByRole('dialog', { name: "Confirm it's you" })).toBeInTheDocument();
      expect(await screen.findByText(ENROLLMENT_COPY)).toBeInTheDocument();
    });

    it('wait: names the problem, offers Close only, and has no form', () => {
      render(ui({ refusal: slot({ view: 'wait', reason: 'requests' }) }));

      expect(screen.getByRole('dialog', { name: 'Deleting too quickly' })).toHaveTextContent(
        'Try again shortly.'
      );
      expect(screen.queryByRole('button', { name: /^Confirm/ })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument();
      expect(screen.getAllByRole('button', { name: 'Close' }).length).toBeGreaterThan(0);
    });

    // Seen in the running app (#3455 captures): a spent step-up budget, after
    // five wrong passwords or codes, rendered as "Deleting too quickly — Try
    // again shortly.", blaming the delete rate for a verification lockout that
    // lasts up to 15 minutes. The two 429 reasons must read differently.
    it('wait (verification): names the spent attempts, not the delete rate', () => {
      render(ui({ refusal: slot({ view: 'wait', reason: 'verification' }) }));

      const dialog = screen.getByRole('dialog', { name: 'Too many attempts' });
      expect(dialog, 'verification wait names the attempts').toHaveTextContent(
        'Too many verification attempts.'
      );
      expect(dialog, 'the budget window is minutes, not a moment').toHaveTextContent(
        'Try again in a few minutes.'
      );
      expect(dialog, 'never blames the delete rate').not.toHaveTextContent('Deleting too quickly');
      expect(dialog).not.toHaveTextContent('Try again shortly.');
    });

    it('unavailable: says nothing happened and gives no countdown', () => {
      render(ui({ refusal: slot({ view: 'unavailable' }) }));

      expect(screen.getByRole('dialog', { name: "Can't delete right now" })).toHaveTextContent(
        'Deleting messages is temporarily unavailable. Try again in a moment.'
      );
      expect(screen.queryByText(/\d+s\b/)).not.toBeInTheDocument();
    });

    it('failed: shows the server text when there is some', () => {
      render(ui({ refusal: slot({ view: 'failed', message: 'Delete not allowed' }) }));
      expect(
        screen.getByRole('dialog', { name: "Couldn't delete that message" })
      ).toHaveTextContent('Delete not allowed');
    });

    it('failed: falls back to generic copy when there is none', () => {
      render(ui({ refusal: slot({ view: 'failed' }) }));
      expect(screen.getByRole('dialog')).toHaveTextContent('Something went wrong. Try again.');
    });

    it('Close on a Close-only view dismisses', async () => {
      const user = userEvent.setup();
      const onDismiss = vi.fn();
      render(ui({ refusal: slot({ view: 'unavailable' }), onDismiss }));

      const close = screen.getAllByRole('button', { name: 'Close' });
      await user.click(close.at(-1) as HTMLElement);
      expect(onDismiss).toHaveBeenCalledTimes(1);
    });
  });

  // E8, and the #17 loop: an account with no authenticator app or security key
  // can type nothing that passes, so the dialog must offer nothing to retry.
  describe('enrolment (E8)', () => {
    async function renderEnroll(view: DeleteRefusalView = ENROLL) {
      const onConfirm = okConfirm();
      render(ui({ refusal: slot(view), onConfirm }));
      await screen.findByText(ENROLLMENT_COPY);
      return onConfirm;
    }

    it('offers no input of any kind', async () => {
      await renderEnroll();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
    });

    it('has no Retry, no countdown and no live alert', async () => {
      vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] });
      render(ui({ refusal: slot(ENROLL, { openedAt: Date.now() }) }));
      act(() => {
        vi.advanceTimersByTime(120_000);
      });

      const dialog = screen.getByRole('dialog');
      expect(screen.queryByRole('button', { name: /retry/i })).not.toBeInTheDocument();
      expect(dialog).not.toHaveTextContent(/try again/i);
      expect(dialog).not.toHaveTextContent(/\d+s\b/);
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(vi.getTimerCount(), 'no countdown interval is running').toBe(0);
    });

    it('its primary can never send: aria-disabled, and activating it sends nothing', async () => {
      const onConfirm = await renderEnroll();

      confirmIsInert();
      await userEvent.setup().click(confirmButton());
      fireEvent.keyDown(confirmButton(), { key: 'Enter' });

      expect(onConfirm).not.toHaveBeenCalled();
    });

    it('is not a dead end: Cancel dismisses', async () => {
      const onDismiss = vi.fn();
      render(ui({ refusal: slot(ENROLL), onDismiss }));
      await screen.findByText(ENROLLMENT_COPY);

      await userEvent.setup().click(screen.getByRole('button', { name: 'Cancel' }));
      expect(onDismiss).toHaveBeenCalledTimes(1);
    });

    it('carries no danger styling', async () => {
      await renderEnroll();
      const dialog = screen.getByRole('dialog');
      for (const el of [dialog, ...dialog.querySelectorAll('*')]) {
        expect(el.getAttribute('class') ?? '').not.toMatch(/danger/i);
      }
    });

    it('a read that would offer a method cannot revive it: the seed ends the instance', async () => {
      server.use(readAnswers(['totp', 'webauthn'], true));
      await renderEnroll();
      // Give a (wrongly) restarted read time to land.
      await new Promise((r) => setTimeout(r, 50));
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      expect(screen.getByText(ENROLLMENT_COPY)).toBeInTheDocument();
    });

    // The retry's own enrolment 403 reaches the hook as an outcome, and the
    // controller swaps the slot's view in the same beat.
    it('an enrolment answer to a retry ends the stage: the password field goes, the copy appears', async () => {
      server.use(readAnswers([]));
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(async () =>
        refusal({ kind: 'enrollmentRequired' })
      );
      const { rerender } = render(ui({ refusal: slot(PASSWORD), onConfirm }));
      const user = userEvent.setup();
      await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);
      await user.click(confirmButton());
      await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
      rerender(ui({ refusal: slot(ENROLL), onConfirm }));

      expect(await screen.findByText(ENROLLMENT_COPY)).toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: /retry/i })).not.toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(onConfirm).toHaveBeenCalledTimes(1);
    });
  });

  describe('confirming with a code', () => {
    it('keeps Confirm inert until a complete code exists, then sends only the code', async () => {
      const user = userEvent.setup();
      const onConfirm = okConfirm();
      render(ui({ refusal: slot(CONFIRM), onConfirm }));
      const input = await totpInput();

      confirmIsInert();
      await user.type(input, '123');
      confirmIsInert();

      await user.type(input, '456');
      confirmIsLive();
      await user.click(confirmButton());

      await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
      expect(onConfirm.mock.calls[0][0]).toEqual({ mfaCode: CODE });
    });

    // Mutation: dropping `{ capture }` from the stage's stepUpActivation re-sends against the
    // factor's own opening capture, not the refused delete's (red).
    it('hands the controller the refused delete’s capture with the code', async () => {
      const onConfirm = okConfirm();
      const refusal = slot(CONFIRM);
      render(ui({ refusal, onConfirm }));
      await typeCode();
      await userEvent.setup().click(confirmButton());

      await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
      // The same capture, not an equal one: the stage never took its own.
      expect(onConfirm.mock.calls[0][1], 'the capture the retry is admitted against').toBe(
        refusal.context
      );
    });

    it('editing a completed code makes Confirm inert again', async () => {
      const user = userEvent.setup();
      render(ui({ refusal: slot(CONFIRM) }));
      await typeCode();
      confirmIsLive();

      await user.type(await totpInput(), '{Backspace}');
      confirmIsInert();
    });

    it('activating Confirm with no code sends nothing and names what is missing', async () => {
      const onConfirm = okConfirm();
      render(ui({ refusal: slot(CONFIRM), onConfirm }));
      await totpInput();

      await userEvent.setup().click(confirmButton());

      expect(onConfirm).not.toHaveBeenCalled();
      expect((await screen.findAllByText(/Enter the 6-digit code/)).length).toBeGreaterThan(0);
    });

    // The #17 kill: a backup code was refused here before the picker.
    it('a backup code is accepted, and travels as the code', async () => {
      server.use(readAnswers(['totp'], true));
      const onConfirm = okConfirm();
      render(ui({ refusal: slot(CONFIRM), onConfirm }));
      const user = userEvent.setup();

      await user.click(await screen.findByRole('button', { name: 'Use a backup code instead' }));
      await user.type(await screen.findByLabelText('Backup code'), BACKUP);
      await user.click(confirmButton());

      await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
      expect(onConfirm.mock.calls[0][0]).toEqual({ mfaCode: BACKUP });
    });

    it('no backup code is offered when the account has none', async () => {
      server.use(readAnswers(['totp'], false));
      render(ui({ refusal: slot(CONFIRM) }));
      await totpInput();
      await new Promise((r) => setTimeout(r, 30));
      expect(screen.queryByRole('button', { name: 'Use a backup code instead' })).toBeNull();
    });

    // G2: the refusal that opened the dialog already named the methods.
    it('a failed read keeps the seeded methods rather than blocking the delete', async () => {
      server.use(http.get(READ_PATH, () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
      render(ui({ refusal: slot({ view: 'confirm', methods: ['totp'] }) }));

      expect(await totpInput()).toBeInTheDocument();
      await new Promise((r) => setTimeout(r, 30));
      expect(screen.queryByRole('button', { name: /retry/i })).not.toBeInTheDocument();
      expect(screen.getByLabelText('Authenticator app code')).toBeInTheDocument();
    });

    it('an invalid code is worded in place by the hook, and the input is emptied and focused', async () => {
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(async () =>
        refusal({ kind: 'invalidMfaCode' })
      );
      render(ui({ refusal: slot(CONFIRM), onConfirm }));
      await typeCode();
      await userEvent.setup().click(confirmButton());

      expect(await screen.findByText(/That code didn't work/)).toBeInTheDocument();
      const input = await totpInput();
      await waitFor(() => expect(input).toHaveFocus());
      expect(input).toHaveValue('');
      confirmIsInert();
    });

    it('an aborted attempt shows nothing and keeps what was typed', async () => {
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(async () => ({
        kind: 'aborted',
      }));
      render(ui({ refusal: slot(CONFIRM), onConfirm }));
      await typeCode();
      await userEvent.setup().click(confirmButton());

      await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
      await waitFor(() => confirmIsLive());
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(screen.queryByText(/didn't work|network|couldn't/i)).not.toBeInTheDocument();
      expect(await totpInput()).toHaveValue(CODE);
    });
  });

  describe('confirming with a security key', () => {
    let mockGet: ReturnType<typeof vi.fn>;

    beforeEach(() => {
      mockGet = vi.fn().mockResolvedValue({
        id: 'credential-id',
        rawId: new Uint8Array([1, 2, 3]).buffer,
        type: 'public-key',
        response: {
          authenticatorData: new Uint8Array([10, 20]).buffer,
          clientDataJSON: new Uint8Array([30, 40]).buffer,
          signature: new Uint8Array([50, 60]).buffer,
          userHandle: null,
        },
      });
      Object.defineProperty(navigator, 'credentials', {
        value: { get: mockGet },
        writable: true,
        configurable: true,
      });
    });

    function ceremonyWorks(begins: unknown[] = []) {
      server.use(
        readAnswers(['webauthn']),
        http.post(BEGIN_PATH, async ({ request }) => {
          begins.push(await request.json());
          return HttpResponse.json({
            publicKey: { challenge: 'AQID', rpId: 'localhost', allowCredentials: [] },
          });
        }),
        http.post(FINISH_PATH, () => HttpResponse.json({ mfa_token: WEBAUTHN_TOKEN }))
      );
    }

    // A WebAuthn token not reaching the delete: the controller sends `mfaCode`.
    it.each(['messages.delete', 'dm.message_delete'] as const)(
      'the token minted for %s is the code the controller is handed',
      async (purpose) => {
        const begins: unknown[] = [];
        ceremonyWorks(begins);
        const onConfirm = okConfirm();
        render(
          ui({ refusal: slot({ view: 'confirm', methods: ['webauthn'] }), purpose, onConfirm })
        );
        await screen.findByText('Passkey or security key');

        await userEvent.setup().click(confirmButton());

        await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
        expect(begins).toEqual([{ purpose }]);
        expect(onConfirm.mock.calls[0][0]).toEqual({ mfaCode: WEBAUTHN_TOKEN });
      }
    );

    it('a cancelled ceremony sends nothing and returns focus to the primary', async () => {
      ceremonyWorks();
      mockGet.mockRejectedValue(new DOMException('cancelled', 'NotAllowedError'));
      const onConfirm = okConfirm();
      render(ui({ refusal: slot({ view: 'confirm', methods: ['webauthn'] }), onConfirm }));
      await screen.findByText('Passkey or security key');

      await userEvent.setup().click(confirmButton());

      expect(await screen.findByText(/cancelled or timed out/)).toBeInTheDocument();
      await waitFor(() => expect(confirmButton()).toHaveFocus());
      expect(onConfirm).not.toHaveBeenCalled();
    });
  });

  describe('confirming with a password', () => {
    beforeEach(() => {
      server.use(readAnswers([]));
    });

    const field = () => screen.findByLabelText('Password');
    const enter = async (value = FIXTURE_PW) => userEvent.setup().type(await field(), value);

    it('keeps Confirm inert until a password is typed, then sends only the password', async () => {
      const onConfirm = okConfirm();
      render(ui({ refusal: slot(PASSWORD), onConfirm }));
      await field();

      confirmIsInert();
      await enter();
      confirmIsLive();
      await userEvent.setup().click(confirmButton());

      await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
      expect(onConfirm.mock.calls[0][0]).toEqual({ currentPassword: FIXTURE_PW });
    });

    it('the sent password leaves the field, whatever the controller answered', async () => {
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(async () => ({
        kind: 'answered',
      }));
      render(ui({ refusal: slot(PASSWORD), onConfirm }));
      await enter();
      await userEvent.setup().click(confirmButton());

      await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
      await waitFor(() => expect(screen.getByLabelText('Password')).toHaveValue(''));
    });

    // D7: nothing was sent, so what was typed is still what the person means.
    it('an aborted attempt keeps the password', async () => {
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(async () => ({
        kind: 'aborted',
      }));
      render(ui({ refusal: slot(PASSWORD), onConfirm }));
      await enter();
      await userEvent.setup().click(confirmButton());

      await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
      await waitFor(() => confirmIsLive());
      expect(screen.getByLabelText('Password')).toHaveValue(FIXTURE_PW);
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('a refused password is worded on the emptied, focused, described field', async () => {
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(async () =>
        refusal({ kind: 'invalidPassword' })
      );
      render(ui({ refusal: slot(PASSWORD), onConfirm }));
      await enter();
      await userEvent.setup().click(confirmButton());

      expect(await screen.findByRole('alert')).toHaveTextContent('That password is not correct.');
      const input = screen.getByLabelText('Password');
      await waitFor(() => expect(input).toHaveFocus());
      expect(input).toHaveValue('');
      expect(input).toHaveAttribute('aria-invalid', 'true');
      expect(input).toHaveAccessibleDescription('That password is not correct.');
    });

    // C2: the account gained an authenticator since the prompt opened.
    it('a mint that names methods moves the dialog from the password to the code', async () => {
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(async () =>
        refusal({ kind: 'mfaRequired', methods: ['totp'] })
      );
      render(ui({ refusal: slot(PASSWORD), onConfirm }));
      await enter();
      await userEvent.setup().click(confirmButton());

      expect(await totpInput()).toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    });

    it('keeps the typed password while the attempt is merely in flight, read-only', async () => {
      const pending = deferred<StepUpSubmitOutcome>();
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(() => pending.promise);
      render(ui({ refusal: slot(PASSWORD), onConfirm }));
      await enter();
      await userEvent.setup().click(confirmButton());

      await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
      expect(screen.getByLabelText('Password')).toHaveValue(FIXTURE_PW);
      await act(async () => {
        pending.resolve({ kind: 'success' });
      });
    });

    // Removed with the unchanged behaviour: "a different message starts from an
    // empty password field". The controller keeps one slot and closes it before
    // another message can fill it, so the dialog never sees an in-place swap.
  });

  describe('while submitting', () => {
    it.each([
      ['confirm', CONFIRM],
      ['password', PASSWORD],
    ] as const)('%s: is not dismissable and says so', async (_, view) => {
      server.use(readAnswers(view.view === 'confirm' ? ['totp'] : []));
      const user = userEvent.setup();
      const onDismiss = vi.fn();
      const pending = deferred<StepUpSubmitOutcome>();
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(() => pending.promise);
      render(ui({ refusal: slot(view), onConfirm, onDismiss }));
      if (view.view === 'confirm') await typeCode();
      else await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);

      await user.click(confirmButton());

      const busy = await screen.findByRole('button', { name: 'Confirming…' });
      expect(busy).toHaveAttribute('aria-disabled', 'true');
      expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
      // ui/Modal drops its header X when dismissable is false.
      expect(screen.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument();

      await user.keyboard('{Escape}');
      expect(onDismiss).not.toHaveBeenCalled();

      await act(async () => {
        pending.resolve({ kind: 'success' });
      });
    });

    it('does not submit a second time', async () => {
      const user = userEvent.setup();
      const pending = deferred<StepUpSubmitOutcome>();
      const onConfirm = vi.fn<DeleteRefusalModalProps['onConfirm']>(() => pending.promise);
      render(ui({ refusal: slot(CONFIRM), onConfirm }));
      await typeCode();
      await user.click(confirmButton());
      await screen.findByRole('button', { name: 'Confirming…' });

      await user.click(screen.getByRole('button', { name: 'Confirming…' }));
      fireEvent.keyDown(screen.getByRole('button', { name: 'Confirming…' }), { key: 'Enter' });

      expect(onConfirm).toHaveBeenCalledTimes(1);
      await act(async () => {
        pending.resolve({ kind: 'success' });
      });
    });

    it('is dismissable by Escape, the header X and Cancel when idle', async () => {
      server.use(readAnswers([]));
      const user = userEvent.setup();
      const onDismiss = vi.fn();
      render(ui({ refusal: slot(PASSWORD), onDismiss }));
      await screen.findByLabelText('Password');

      await user.keyboard('{Escape}');
      expect(onDismiss).toHaveBeenCalledTimes(1);
      await user.click(screen.getByRole('button', { name: 'Close' }));
      expect(onDismiss).toHaveBeenCalledTimes(2);
      await user.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(onDismiss).toHaveBeenCalledTimes(3);
    });
  });

  describe('countdown', () => {
    function liveRegion() {
      return screen.getByRole('status');
    }

    it('ticks in the body, writing to the live region only at zero', () => {
      vi.useFakeTimers();
      render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'requests', retryAfterSeconds: 3 },
            { openedAt: Date.now() }
          ),
        })
      );

      expect(screen.getByText('Try again in 3s.')).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('');
      // The ticking text is outside every live region.
      expect(screen.getByText('Try again in 3s.').closest('[aria-live]')).toBeNull();

      act(() => {
        vi.advanceTimersByTime(1000);
      });
      expect(screen.getByText('Try again in 2s.')).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('');

      act(() => {
        vi.advanceTimersByTime(1000);
      });
      expect(screen.getByText('Try again in 1s.')).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('');

      act(() => {
        vi.advanceTimersByTime(1000);
      });
      expect(screen.getByText('You can try again now.', { selector: 'p' })).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('You can try again now.');
      expect(liveRegion()).toHaveAttribute('aria-live', 'polite');
    });

    it('counts from the wall clock, so a throttled timer cannot drift it', () => {
      vi.useFakeTimers();
      render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'requests', retryAfterSeconds: 60 },
            { openedAt: Date.now() }
          ),
        })
      );

      // One long jump in a single tick, as a backgrounded window would see.
      act(() => {
        vi.advanceTimersByTime(45_000);
      });
      expect(screen.getByText('Try again in 15s.')).toBeInTheDocument();
    });

    it('anchors on the response that carried the header, not on when the dialog mounted', () => {
      vi.useFakeTimers();
      const arrived = Date.now() - 10_000;
      render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'requests', retryAfterSeconds: 30 },
            { openedAt: arrived }
          ),
        })
      );
      expect(screen.getByText('Try again in 20s.')).toBeInTheDocument();
    });

    it('a header already elapsed shows zero at once, and announces it', () => {
      vi.useFakeTimers();
      render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'verification', retryAfterSeconds: 5 },
            { openedAt: Date.now() - 60_000 }
          ),
        })
      );
      expect(screen.getByText('You can try again now.', { selector: 'p' })).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('You can try again now.');
    });

    it('a wait with no header shows a static line and never announces', () => {
      vi.useFakeTimers();
      render(ui({ refusal: slot({ view: 'wait', reason: 'requests' }) }));
      act(() => {
        vi.advanceTimersByTime(120_000);
      });
      expect(screen.getByText('Try again shortly.')).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('');
    });

    it('a failed view appends the countdown, and drops it at zero', () => {
      vi.useFakeTimers();
      render(
        ui({
          refusal: slot(
            { view: 'failed', message: 'Delete not allowed', retryAfterSeconds: 2 },
            { openedAt: Date.now() }
          ),
        })
      );
      expect(screen.getByRole('dialog')).toHaveTextContent(
        'Delete not allowed You can try again in 2s.'
      );

      act(() => {
        vi.advanceTimersByTime(2000);
      });
      expect(screen.getByRole('dialog')).not.toHaveTextContent('You can try again in');
      expect(screen.getByRole('dialog')).toHaveTextContent('Delete not allowed');
    });

    it('stops ticking once the dialog closes', () => {
      vi.useFakeTimers();
      const { rerender } = render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'requests', retryAfterSeconds: 30 },
            { openedAt: Date.now() }
          ),
        })
      );
      const ticking = vi.getTimerCount();
      expect(ticking).toBeGreaterThan(0);

      rerender(ui({ refusal: null }));
      expect(vi.getTimerCount()).toBeLessThan(ticking);
    });
  });

  describe('focus', () => {
    // A chat panel root, as the owners render it: `data-chat-surface` on an element in the document.
    function addSurface(id: string) {
      const root = document.createElement('div');
      root.dataset.chatSurface = id;
      document.body.appendChild(root);
      extraNodes.push(root);
      return root;
    }

    function addRow(id: string, surface: HTMLElement = surfaceRoot) {
      const row = document.createElement('div');
      row.dataset.messageId = id;
      row.tabIndex = -1;
      surface.appendChild(row);
      return row;
    }

    function addComposer(surface: HTMLElement = surfaceRoot) {
      const composer = document.createElement('textarea');
      composer.className = 'message-input-textarea';
      surface.appendChild(composer);
      return composer;
    }

    let surfaceRoot: HTMLElement;
    beforeEach(() => {
      surfaceRoot = addSurface(SURFACE);
    });

    it('moves focus into the first field once the read lands', async () => {
      server.use(readAnswers([]));
      render(ui({ refusal: slot(PASSWORD) }));
      const field = await screen.findByLabelText('Password');
      await waitFor(() => expect(field).toHaveFocus());
    });

    it('lands on the code input when the account has no password leg', async () => {
      render(ui({ refusal: slot(CONFIRM) }));
      const input = await totpInput();
      await waitFor(() => expect(input).toHaveFocus());
    });

    it('an enrolment dialog puts focus inside the dialog, never on a field that is not there', async () => {
      render(ui({ refusal: slot(ENROLL) }));
      await screen.findByText(ENROLLMENT_COPY);
      const dialog = screen.getByRole('dialog');
      await waitFor(() => expect(dialog.contains(document.activeElement)).toBe(true));
    });

    it('moves focus to Close on a Close-only view', () => {
      render(ui({ refusal: slot({ view: 'unavailable' }) }));
      const focused = document.activeElement as HTMLElement;
      expect(focused).toHaveTextContent('Close');
      expect(focused.className).toContain('delete-refusal-modal__cancel');
    });

    it('returns focus to the message row after close', () => {
      const row = addRow('m1');
      const composer = addComposer();
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      expect(row).not.toHaveFocus();

      rerender(ui({ refusal: null }));
      expect(row).toHaveFocus();
      expect(composer).not.toHaveFocus();
    });

    it('falls back to the composer when the row is gone', () => {
      const composer = addComposer();
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));

      rerender(ui({ refusal: null }));
      expect(composer).toHaveFocus();
    });

    it('never lands on body when neither exists', () => {
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      expect(() => rerender(ui({ refusal: null }))).not.toThrow();
    });

    it('finds a row whose id contains a quote, without treating it as selector syntax', () => {
      const row = addRow('m"1]');
      const { rerender } = render(ui({ refusal: slot(PASSWORD, { messageId: 'm"1]' }) }));

      rerender(ui({ refusal: null }));
      expect(row).toHaveFocus();
    });

    it('returns focus to the row of the message that was refused, not another row', () => {
      const other = addRow('m2');
      const row = addRow('m1');
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));

      rerender(ui({ refusal: null }));
      expect(row).toHaveFocus();
      expect(other).not.toHaveFocus();
    });

    it('never lands on another panel that shows the same message', async () => {
      server.use(readAnswers([]));
      // The other panel comes FIRST in the document, so a document-wide lookup would find it.
      const otherPanel = addSurface('surface-panel');
      const otherRow = addRow('m1', otherPanel);
      const otherComposer = addComposer(otherPanel);
      const row = addRow('m1');
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      // Gate: the modal is open and neither target has focus yet.
      const field = await screen.findByLabelText('Password');
      await waitFor(() => expect(field).toHaveFocus());

      rerender(ui({ refusal: null }));

      expect(row).toHaveFocus();
      expect(otherRow).not.toHaveFocus();
      expect(otherComposer).not.toHaveFocus();
    });

    it('falls back to its own composer, not another panel composer, when its row is gone', async () => {
      server.use(readAnswers([]));
      const otherPanel = addSurface('surface-panel');
      const otherRow = addRow('m1', otherPanel);
      const otherComposer = addComposer(otherPanel);
      const composer = addComposer();
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      const field = await screen.findByLabelText('Password');
      await waitFor(() => expect(field).toHaveFocus());

      rerender(ui({ refusal: null }));

      expect(composer).toHaveFocus();
      expect(otherRow).not.toHaveFocus();
      expect(otherComposer).not.toHaveFocus();
    });

    it('moves focus nowhere when its own panel has neither row nor composer', async () => {
      server.use(readAnswers([]));
      const otherPanel = addSurface('surface-panel');
      const otherRow = addRow('m1', otherPanel);
      const otherComposer = addComposer(otherPanel);
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      const field = await screen.findByLabelText('Password');
      await waitFor(() => expect(field).toHaveFocus());

      rerender(ui({ refusal: null }));

      expect(otherRow).not.toHaveFocus();
      expect(otherComposer).not.toHaveFocus();
    });

    it('does not steal focus for a modal that never opened', () => {
      const composer = addComposer();
      render(ui({ refusal: null }));
      expect(composer).not.toHaveFocus();
    });

    it('re-focuses the Close button when the challenge gives way to a Close-only view', () => {
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      rerender(ui({ refusal: slot({ view: 'wait', reason: 'verification' }) }));
      expect(document.activeElement).toHaveTextContent('Close');
    });
  });

  describe('no --danger (T8)', () => {
    const VIEWS: DeleteRefusalView[] = [
      CONFIRM,
      PASSWORD,
      ENROLL,
      { view: 'wait', reason: 'requests', retryAfterSeconds: 3 },
      { view: 'unavailable' },
      { view: 'failed', message: 'Nope' },
    ];

    it.each(VIEWS.map((view) => [view.view, view] as const))(
      'the %s view carries no danger class',
      (_, view) => {
        render(ui({ refusal: slot(view) }));
        const dialog = screen.getByRole('dialog');
        for (const el of [dialog, ...dialog.querySelectorAll('*')]) {
          expect(el.getAttribute('class') ?? '').not.toMatch(/danger/i);
        }
      }
    );

    it('the stylesheet paints nothing with --danger', () => {
      const css = readFileSync(
        resolve(__dirname, '../../../../src/renderer/components/Chat/DeleteRefusalModal.css'),
        'utf-8'
      ).replaceAll(/\/\*[\s\S]*?\*\//g, '');
      expect(css).not.toMatch(/danger/i);
    });
  });
});
