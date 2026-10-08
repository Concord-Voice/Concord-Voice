import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';

// Reproductions from the #3509 frontend review for the self-purge soft-lock:
// M1 — mint mfa_required moves the stage to the MFA prompt;
// M2 — a mint the server does not have is named as unsupported;
// L2 — a password_required refusal without step_up_token_invalid still clears
//      the sent password;
// L3 — a retry's outcome is worded from the retry alone (the stage no longer
//      clears the previous error by hand: `withoutError` is gone);
// L4 — a mint and purge still in flight when the dialog closes and reopens
//      must not write their outcome into the reopened dialog.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
  server.use(passwordOnlyRead());
});

// The stage reads the account's inline methods. Stubbed so the result never
// depends on whatever answers an unhandled request: a password-only account,
// as the password_required challenge says.
const passwordOnlyRead = () =>
  http.get('*/api/v1/mfa/step-up', () =>
    HttpResponse.json({ methods: [], default_method: null, backup_code_available: false })
  );

const CHANNEL_PATH = '*/api/v1/channels/:id/messages';
const PASSWORD_CHALLENGE = {
  error: 'Current password required to keep deleting messages',
  delete_rate_limited: true,
  password_required: true,
};

function purgeAnswers(answers: Array<() => Response | Promise<Response>>) {
  let n = 0;
  server.use(http.delete(CHANNEL_PATH, () => answers[Math.min(n++, answers.length - 1)]()));
}
function mintAnswers(answer: () => Response | Promise<Response>) {
  server.use(http.post(`*${MINT_PATH}`, () => answer()));
}
const challenge = () =>
  HttpResponse.json(PASSWORD_CHALLENGE, { status: 403, headers: { 'Retry-After': '30' } });

function ui(isOpen: boolean) {
  return (
    <PurgeMessagesModal
      context="channel"
      isOpen={isOpen}
      scopeId="c1"
      scopeName="general"
      onClose={() => {}}
    />
  );
}

async function toPasswordStage() {
  const user = userEvent.setup();
  const view = render(ui(true));
  await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
  await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
  await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);
  return { user, view };
}
const submit = () => screen.getByRole('button', { name: 'Confirm and Purge' });

describe('PurgeMessagesModal mint refusals (#3509 frontend review)', () => {
  it('M1: an mfa_required mint refusal moves to the MFA prompt', async () => {
    purgeAnswers([challenge]);
    mintAnswers(() =>
      HttpResponse.json(
        { error: 'MFA verification required', mfa_required: true, mfa_methods: ['totp'] },
        { status: 403 }
      )
    );
    const { user } = await toPasswordStage();

    await user.click(submit());

    expect(await screen.findByLabelText('Authenticator app code')).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  it('M2: a mint the server does not have is named as unsupported', async () => {
    purgeAnswers([challenge]);
    mintAnswers(() => HttpResponse.json({ error: 'Not Found' }, { status: 404 }));
    const { user } = await toPasswordStage();

    await user.click(submit());

    // The fields cannot answer a missing endpoint, so `endExchangeRefusal`
    // ends the purge as a `softLockFailed` result carrying the exchange's own
    // words, not a stage that asks for the password again.
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("This server doesn't support this confirmation yet.");
    expect(screen.getByRole('button', { name: 'Done' })).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  it('L2: a plain password_required after the exchange leaves no password behind', async () => {
    purgeAnswers([challenge]);
    mintAnswers(() => HttpResponse.json({ step_up_token: 'tok', expires_in: 60 }));
    const { user } = await toPasswordStage();

    await user.click(submit());

    await waitFor(() => expect(screen.getByLabelText('Password')).toHaveValue(''));
  });

  it('L3: a retry that is refused again is worded again, in place', async () => {
    purgeAnswers([challenge]);
    let calls = 0;
    mintAnswers(() => {
      calls += 1;
      return HttpResponse.json({ error: 'Invalid password' }, { status: 403 });
    });
    const { user } = await toPasswordStage();
    await user.click(submit());
    expect(await screen.findByRole('alert')).toHaveTextContent('That password is not correct.');

    // The rejected password was dropped, so the retry starts from an empty field.
    await waitFor(() => expect(screen.getByLabelText('Password')).toHaveValue(''));
    await user.type(screen.getByLabelText('Password'), FIXTURE_PW);
    await user.click(submit());

    await waitFor(() => expect(calls).toBe(2));
    expect(await screen.findByRole('alert')).toHaveTextContent('That password is not correct.');
    expect(screen.getByLabelText('Password')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
  });

  it('L4: an outcome that lands after the dialog closed and reopened is dropped', async () => {
    purgeAnswers([challenge, () => HttpResponse.json({ deleted_count: 7, hidden_count: 0 })]);
    let release!: () => void;
    mintAnswers(
      () =>
        new Promise<Response>((r) => {
          release = () => r(HttpResponse.json({ step_up_token: 'tok', expires_in: 60 }));
        })
    );
    const { user, view } = await toPasswordStage();
    await user.click(submit());
    await waitFor(() => expect(release).toBeTypeOf('function'));

    view.rerender(ui(false));
    view.rerender(ui(true));
    release();

    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByText('Purged 7 messages.')).not.toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Range' })).toHaveValue('');
  });
});
