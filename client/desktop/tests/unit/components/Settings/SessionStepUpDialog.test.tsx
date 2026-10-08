import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent, waitFor, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import {
  bodiesTo,
  installStepUpApi,
  jsonResponse,
  readCount,
  readOffers,
} from '../../../helpers/stepUpApi';

// The session step-up dialog (#7, plan 2026-10-07 §3): single revoke, revoke-all
// and the revocation-mode change on one dialog. It reads the requirements itself
// (`whenNoMfa`), never `users.mfa_methods` (C2), and reads the route's answers
// through `adaptSessionsRefusal` (exact strings, C5), never the seam classifier.
//
// `apiFetch` is mocked at the module boundary and answers by path, so the
// hook's read and the dialog's own request never compete for a queued response.
//
// "Mutant:" comments name the production change each case exists to turn red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

const mockClassify = vi.hoisted(() => vi.fn());
vi.mock('@/renderer/services/system/stepUpRefusal', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/system/stepUpRefusal')>();
  mockClassify.mockImplementation(actual.classifyStepUpRefusal);
  return { ...actual, classifyStepUpRefusal: mockClassify };
});

import SessionStepUpDialog, {
  sessionStepUpKey,
  type SessionStepUpAction,
} from '@/renderer/components/Settings/SessionStepUpDialog';
import type { StepUpFactorRefusal } from '@/renderer/hooks/auth/useStepUpFactor';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useTotpAcceptedStore } from '@/renderer/stores/auth/totpAcceptedStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

// Named fixtures: the pre-commit detect-secrets hook flags a credential-shaped
// key beside a quoted literal regardless of the value.
const FIXTURE_PW = 'fixture-password-do-not-persist';
const FIXTURE_OTP = '314159';
const FIXTURE_BACKUP = 'ABCD2345';

const CODE_LABEL = 'Authenticator app code';
const REVOKE_ALL: SessionStepUpAction = { kind: 'revokeAll' };
const MODE: SessionStepUpAction = { kind: 'modeChange', mode: 'simple' };
const REVOKE: SessionStepUpAction = { kind: 'revoke', sessionId: 's2' };

const CONFIRM: Record<SessionStepUpAction['kind'], string> = {
  revoke: 'Confirm & Revoke',
  revokeAll: 'Yes, Revoke All Sessions',
  modeChange: 'Confirm',
};
const PATH: Record<SessionStepUpAction['kind'], string> = {
  revoke: '/api/v1/sessions/s2',
  revokeAll: '/api/v1/sessions/revoke-all',
  modeChange: '/api/v1/sessions/revocation-mode',
};

const onClose = vi.fn();
const onAccepted = vi.fn();

function renderDialog(action: SessionStepUpAction, seed?: StepUpFactorRefusal | null) {
  return render(
    <SessionStepUpDialog action={action} seed={seed} onClose={onClose} onAccepted={onAccepted} />
  );
}

const primary = (action: SessionStepUpAction) =>
  screen.getByRole('button', { name: CONFIRM[action.kind] });
const passwordField = () => screen.findByLabelText('Password') as Promise<HTMLInputElement>;
const codeField = () => screen.findByLabelText(CODE_LABEL) as Promise<HTMLInputElement>;
const sent = (action: SessionStepUpAction) => bodiesTo(mockApiFetch, PATH[action.kind]);
const requestsTo = (action: SessionStepUpAction) =>
  mockApiFetch.mock.calls.filter((c) => c[0] === PATH[action.kind]);

/** Waits for the dialog's request (not the read) to have been issued `n` times. */
const waitForRequests = (action: SessionStepUpAction, n = 1) =>
  waitFor(() => expect(requestsTo(action)).toHaveLength(n));

beforeEach(() => {
  resetAllStores();
  onClose.mockReset();
  onAccepted.mockReset();
  mockClassify.mockClear();
  useUserStore.setState({ user: { id: 'acct-1' } as never });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('what each action sends', () => {
  it.each([
    [REVOKE, 'DELETE', {}],
    [REVOKE_ALL, 'POST', { include_current: true }],
    [MODE, 'PUT', { mode: 'simple' }],
  ] as const)('%j sends %s with its fixed body and the password', async (action, method, body) => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(200) });
    renderDialog(action);

    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.click(primary(action));

    await waitForRequests(action);
    expect(requestsTo(action)[0][1]).toMatchObject({ method });
    expect(sent(action)).toEqual([{ ...body, password: FIXTURE_PW }]);
    await waitFor(() => expect(onAccepted).toHaveBeenCalledWith(action));
    expect(onAccepted).toHaveBeenCalledTimes(1);
  });

  it('encodes the session id into the path', async () => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(200) });
    renderDialog({ kind: 'revoke', sessionId: 'a/b c' });

    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.click(primary(REVOKE));

    await waitFor(() =>
      expect(mockApiFetch.mock.calls.some((c) => c[0] === '/api/v1/sessions/a%2Fb%20c')).toBe(true)
    );
  });

  it('describes the mode being switched to', () => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(200) });
    const { unmount } = renderDialog(MODE);
    expect(screen.getByText(/Switching to Simple Revocation/)).toBeInTheDocument();
    unmount();
    renderDialog({ kind: 'modeChange', mode: 'secure' });
    expect(screen.getByText(/Switching to Secure Revocation/)).toBeInTheDocument();
  });
});

describe('the read decides the leg, never mfaMethods (C2)', () => {
  // Mutant: a floor or an unconditional code box (the leg derived from the account's
  // `mfa_methods`, which lists email) puts a field on screen an email-only account
  // can never fill, and takes the password it needs away.
  it.each([REVOKE_ALL, MODE])(
    'an account with no inline method (read: []) gets the password field for %j',
    async (action) => {
      installStepUpApi(mockApiFetch, {
        read: () => readOffers([]),
        route: () => jsonResponse(200),
      });
      renderDialog(action);

      expect(await passwordField()).toBeInTheDocument();
      expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    }
  );

  // Mutant: `passwordLeg: 'always'` shows the password beside the code. // pragma: allowlist secret
  it.each([REVOKE_ALL, MODE])(
    'an authenticator account gets the code field and no password for %j',
    async (action) => {
      installStepUpApi(mockApiFetch, {
        read: () => readOffers(['totp']),
        route: () => jsonResponse(200),
      });
      renderDialog(action);

      expect(await codeField()).toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(readCount(mockApiFetch)).toBe(1);
    }
  );

  it('sends the code as mfa_code and no password', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () => jsonResponse(200),
    });
    renderDialog(MODE);

    await userEvent.type(await codeField(), FIXTURE_OTP);
    await userEvent.click(primary(MODE));

    await waitForRequests(MODE);
    expect(sent(MODE)).toEqual([{ mode: 'simple', mfa_code: FIXTURE_OTP }]);
  });

  // The server's VerifyCode accepts a backup code on this route, so the dialog offers one.
  // Mutant: `allowBackup: false`.
  it('mode change accepts a backup code', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp'], true),
      route: () => jsonResponse(200),
    });
    renderDialog(MODE);

    await userEvent.click(await screen.findByRole('button', { name: 'Use a backup code instead' }));
    await userEvent.type(await screen.findByLabelText('Backup code'), FIXTURE_BACKUP);
    await userEvent.click(primary(MODE));

    await waitForRequests(MODE);
    expect(sent(MODE)).toEqual([{ mode: 'simple', mfa_code: FIXTURE_BACKUP }]);
  });

  // The C2 race: the set was read as TOTP, the route says the account needs its password.
  // Mutant: ignoring `password_required` (the leg stays code-only), or mounting the field
  // without moving focus to it.
  it('a TOTP set that receives password_required mounts the password leg, focuses it, and the next submit carries the password', async () => {
    let n = 0;
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () =>
        ++n === 1 ? jsonResponse(403, { error: 'password_required' }) : jsonResponse(200),
    });
    renderDialog(REVOKE_ALL);

    await userEvent.type(await codeField(), FIXTURE_OTP);
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    await userEvent.click(primary(REVOKE_ALL));

    const password = await passwordField();
    await waitFor(() => expect(password).toHaveFocus());
    await userEvent.type(password, FIXTURE_PW);
    await userEvent.click(primary(REVOKE_ALL));

    await waitForRequests(REVOKE_ALL, 2);
    expect(sent(REVOKE_ALL)[1]).toMatchObject({ include_current: true, password: FIXTURE_PW });
    await waitFor(() => expect(onAccepted).toHaveBeenCalledTimes(1));
  });
});

describe('a refusal-triggered single revoke (seed)', () => {
  // `auth_required` lists users.mfa_methods, so the adapter seeds nothing and the read decides.
  // Mutant: seeding the refusal's own `methods`.
  it('opens seeded on auth_required with methods: null and lets the read decide', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () => jsonResponse(200),
    });
    renderDialog(REVOKE, { kind: 'mfaRequired', methods: null });

    expect(await codeField()).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(readCount(mockApiFetch)).toBe(1);
  });

  // The server found no inline method: a read that cannot answer leaves the seed, and the password.
  it('opens seeded on password_required and keeps the password leg when the read fails', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => jsonResponse(503),
      route: () => jsonResponse(200),
    });
    renderDialog(REVOKE, { kind: 'passwordRequired' });

    expect(await passwordField()).toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
  });
});

describe('what the route answers', () => {
  it('shows a refused password on its field and empties it', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(403, { error: 'Incorrect password' }),
    });
    renderDialog(REVOKE_ALL);

    const field = await passwordField();
    await userEvent.type(field, FIXTURE_PW);
    await userEvent.click(primary(REVOKE_ALL));

    expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
    expect(field).toHaveValue('');
    expect(onAccepted).not.toHaveBeenCalled();
  });

  it('shows a refused code on the code panel', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () => jsonResponse(403, { error: 'Invalid MFA code' }),
    });
    renderDialog(MODE);

    await userEvent.type(await codeField(), FIXTURE_OTP);
    await userEvent.click(primary(MODE));

    expect(await screen.findByText(/That code didn't work/)).toBeInTheDocument();
  });

  // Mutant: an unowned 403 mapped to a field error (a substring or case-folded match).
  it('an unowned 403 keeps the surface error: the server text, on no field', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(403, { error: 'Authentication failed' }),
    });
    renderDialog(MODE);

    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.click(primary(MODE));

    expect(await screen.findByText('Authentication failed')).toBeInTheDocument();
    expect(screen.queryByText('That password is not correct.')).not.toBeInTheDocument();
  });

  it.each([
    [REVOKE, 'Failed to revoke session'],
    [REVOKE_ALL, 'Failed to revoke sessions'],
    [MODE, 'Failed to change revocation mode'],
  ] as const)("a bodiless 500 says %j failed in the action's own words", async (action, text) => {
    installStepUpApi(mockApiFetch, { route: () => new Response(null, { status: 500 }) });
    renderDialog(action);

    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.click(primary(action));

    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  // The sessions routes sit behind `RateLimitByUser`, which answers before the
  // handler reads the code, so a 429 proves it unspent. As an unowned answer it
  // would be `answered`: the code recorded as a TOTP acceptance, and the "already
  // used" hint shown for a code the server never looked at.
  // Mutant: the adapter's 429 arm deleted (null → `failed` → `answered`).
  it('a 429 says to wait, and records no TOTP acceptance', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () => jsonResponse(429, { error: 'Rate limit exceeded' }),
    });
    renderDialog(MODE);

    await userEvent.type(await codeField(), FIXTURE_OTP);
    await userEvent.click(primary(MODE));

    expect(
      await screen.findByText('Too many attempts. Try again in a few minutes.')
    ).toBeInTheDocument();
    expect(screen.queryByText('Rate limit exceeded')).not.toBeInTheDocument();
    expect(useTotpAcceptedStore.getState().acceptedAt).toEqual({});
    expect(onAccepted).not.toHaveBeenCalled();
    // The code is still the user's to send once the limit lifts. Mutant: the
    // hook's default arm clearing it (picker PR 3 review).
    expect(await codeField()).toHaveValue(FIXTURE_OTP);
  });

  // Positive control for the case above: a code the server may have spent IS
  // recorded, so the empty store there is not an artefact of the harness.
  it('a 500 after a code records the TOTP acceptance', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () => jsonResponse(500, { error: 'Failed to change revocation mode' }),
    });
    renderDialog(MODE);

    await userEvent.type(await codeField(), FIXTURE_OTP);
    await userEvent.click(primary(MODE));

    await screen.findByText('Failed to change revocation mode');
    expect(useTotpAcceptedStore.getState().acceptedAt).toHaveProperty('acct-1');
  });

  // Mutant: the 401 worded as a local `failed`, which left the stage live with
  // the password and an active primary under a dead session (picker PR 3 review).
  it('a 401 ends the stage: says the session expired, drops the password, sends nothing more', async () => {
    const route = vi.fn(() => jsonResponse(401, { error: 'Unauthorized' }));
    installStepUpApi(mockApiFetch, { route });
    renderDialog(REVOKE_ALL);

    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.click(primary(REVOKE_ALL));

    expect(
      await screen.findByText('Your session has expired. Sign in again to continue.')
    ).toBeInTheDocument();
    expect(screen.getAllByText(/Sign in again/)).toHaveLength(1);
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(primary(REVOKE_ALL)).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(primary(REVOKE_ALL));
    expect(route).toHaveBeenCalledTimes(1);
  });

  it('a transport failure says the server could not be reached and keeps the password', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => {
        throw new TypeError('Failed to fetch');
      },
    });
    renderDialog(REVOKE_ALL);

    const field = await passwordField();
    await userEvent.type(field, FIXTURE_PW);
    await userEvent.click(primary(REVOKE_ALL));

    expect(
      await screen.findByText("Couldn't reach the server. Check your connection and try again.")
    ).toBeInTheDocument();
    expect(field).toHaveValue(FIXTURE_PW);
  });

  // Mutant: calling the seam classifier on these routes (`classifyStepUpRefusal` reads the
  // internal/stepup bodies; these routes predate that seam).
  it('never calls classifyStepUpRefusal', async () => {
    let n = 0;
    installStepUpApi(mockApiFetch, {
      route: () =>
        ++n === 1
          ? jsonResponse(403, { error: 'Incorrect password' })
          : jsonResponse(403, { error: 'password_required' }),
    });
    renderDialog(REVOKE_ALL);

    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.click(primary(REVOKE_ALL));
    await screen.findByText('That password is not correct.');
    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.click(primary(REVOKE_ALL));
    await waitForRequests(REVOKE_ALL, 2);

    expect(mockClassify).not.toHaveBeenCalled();
  });
});

describe('a sent code is never offered again', () => {
  // The server accepts each code once and can accept one yet still fail the request.
  it.each([
    ['a 500', 500, { error: 'Failed to change revocation mode' }],
    ['a refused code', 403, { error: 'Invalid MFA code' }],
  ] as const)('%s clears the code and leaves the primary inert', async (_name, status, body) => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () => jsonResponse(status, body),
    });
    renderDialog(MODE);

    await userEvent.type(await codeField(), FIXTURE_OTP);
    expect(primary(MODE)).not.toHaveAttribute('aria-disabled');
    await userEvent.click(primary(MODE));

    await waitForRequests(MODE);
    await waitFor(() => expect(primary(MODE)).toHaveAttribute('aria-disabled', 'true'));
    expect(await codeField()).toHaveValue('');
  });
});

describe('the primary and Cancel', () => {
  it('stays aria-disabled until the password is typed, and a click names what is missing', async () => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(200) });
    renderDialog(REVOKE_ALL);

    await passwordField();
    expect(primary(REVOKE_ALL)).toHaveAttribute('aria-disabled', 'true');
    // Never natively disabled: it must stay focusable.
    expect(primary(REVOKE_ALL)).not.toBeDisabled();
    await userEvent.click(primary(REVOKE_ALL));

    expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
    expect(requestsTo(REVOKE_ALL)).toHaveLength(0);
  });

  it('Cancel closes the dialog', async () => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(200) });
    renderDialog(REVOKE_ALL);

    await userEvent.click(screen.getByRole('button', { name: 'No, Cancel' }));

    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('shows the in-flight label and cannot be cancelled while the request is out', async () => {
    const gate = deferred<Response>();
    installStepUpApi(mockApiFetch, { route: () => gate.promise });
    renderDialog(REVOKE_ALL);

    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.click(primary(REVOKE_ALL));

    const busy = await screen.findByRole('button', { name: 'Revoking...' });
    expect(busy).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'No, Cancel' })).toBeDisabled();

    gate.resolve(jsonResponse(200));
    await waitFor(() => expect(onAccepted).toHaveBeenCalledTimes(1));
  });

  // An answer for an account that is no longer current belongs to the old one.
  // Mutant: calling onAccepted without the currentness check.
  it('does not act on an answer that lands after the account changed', async () => {
    const gate = deferred<Response>();
    installStepUpApi(mockApiFetch, { route: () => gate.promise });
    renderDialog(REVOKE_ALL);

    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.click(primary(REVOKE_ALL));
    await waitForRequests(REVOKE_ALL);

    useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
    gate.resolve(jsonResponse(200));

    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Revoking...' })).not.toBeInTheDocument()
    );
    expect(onAccepted).not.toHaveBeenCalled();
  });
});

describe('focus once the read has landed (focusOnReady)', () => {
  // Mutant: dropping `focusOnReady`: focus stays on the dialog, and a keyboard user
  // tabs past a field the read has only just put up.
  it('an authenticator account lands on the code input', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () => jsonResponse(200),
    });
    renderDialog(REVOKE_ALL);

    const code = await codeField();

    await waitFor(() => expect(code).toHaveFocus());
  });

  it('a password-only account lands on the password field', async () => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(200) });
    renderDialog(REVOKE_ALL);

    const password = await passwordField();

    await waitFor(() => expect(password).toHaveFocus());
    expect(within(screen.getByRole('dialog')).getByLabelText('Password')).toBe(password);
  });
});

// The parent keys the dialog with this, so a dialog is never re-pointed at
// another action while its credentials belong to the first.
describe('sessionStepUpKey', () => {
  // Mutant: a key that ignores the session id or the mode.
  it('is distinct for every action, and stable for the same one', () => {
    const actions: SessionStepUpAction[] = [
      { kind: 'revoke', sessionId: 's1' },
      { kind: 'revoke', sessionId: 's2' },
      { kind: 'revokeAll' },
      { kind: 'modeChange', mode: 'simple' },
      { kind: 'modeChange', mode: 'secure' },
    ];
    const keys = actions.map(sessionStepUpKey);
    expect(new Set(keys).size).toBe(actions.length);
    expect(sessionStepUpKey({ kind: 'revoke', sessionId: 's1' })).toBe(keys[0]);
  });
});
