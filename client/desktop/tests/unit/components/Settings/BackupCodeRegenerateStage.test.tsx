import { useRef } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import {
  bodiesTo,
  installStepUpApi,
  jsonResponse,
  readCount,
  readOffers,
} from '../../../helpers/stepUpApi';

// Backup-code regeneration (#8, plan 2026-10-07 §3). The route checks the
// password and then an authenticator-app `code` with nothing else, so the stage
// has TOTP as its floor and NO requirements read (`purpose: null`): nothing can
// add a backup code or a security key to it (C4). Its answers go through
// `adaptBackupCodeRegenerateRefusal` (exact strings, C5), never the seam
// classifier.
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

import BackupCodeRegenerateStage from '@/renderer/components/Settings/BackupCodeRegenerateStage';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useTotpAcceptedStore } from '@/renderer/stores/auth/totpAcceptedStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

// Named fixtures: the pre-commit detect-secrets hook flags a credential-shaped
// key beside a quoted literal regardless of the value.
const FIXTURE_PW = 'fixture-password-do-not-persist';
const FIXTURE_OTP = '314159';

const PATH = '/api/v1/mfa/backup-codes/regenerate';
const CODE_LABEL = 'Authenticator app code';
const TOTP_REMOVED =
  'Your authenticator app was turned off. Close this and check your security settings.';
const UNREADABLE_CODES = "We couldn't read your new backup codes. Generate them again.";
const RATE_LIMITED = 'Too many attempts. Try again in a few minutes.';

const onRegenerated = vi.fn();
const onTotpRemoved = vi.fn();
const onCancel = vi.fn();

/** The host: it owns the heading, which is where focus goes on a terminal state. */
function Host() {
  const headingRef = useRef<HTMLHeadingElement>(null);
  return (
    <div>
      <h3 tabIndex={-1} ref={headingRef}>
        Reset Backup Codes
      </h3>
      <BackupCodeRegenerateStage
        headingRef={headingRef}
        onRegenerated={onRegenerated}
        onTotpRemoved={onTotpRemoved}
        onCancel={onCancel}
      />
    </div>
  );
}

const primary = () => screen.getByRole('button', { name: 'Regenerate Codes' });
const password = () => screen.getByLabelText('Password') as HTMLInputElement;
const code = () => screen.getByLabelText(CODE_LABEL) as HTMLInputElement;
const requests = () => mockApiFetch.mock.calls.filter((c) => c[0] === PATH);
const waitForRequests = (n = 1) => waitFor(() => expect(requests()).toHaveLength(n));

async function fillAndSend(): Promise<void> {
  await userEvent.type(password(), FIXTURE_PW);
  await userEvent.type(code(), FIXTURE_OTP);
  await userEvent.click(primary());
}

beforeEach(() => {
  resetAllStores();
  onRegenerated.mockReset();
  onTotpRemoved.mockReset();
  onCancel.mockReset();
  mockClassify.mockClear();
  useUserStore.setState({ user: { id: 'acct-1' } as never });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('the stage offers TOTP and nothing else, and reads nothing (C4)', () => {
  // Mutant: a `purpose` on the hook. The read would then run, and the account below
  // would be offered a security key and a backup code on a route that accepts neither.
  it('issues no requirements read, even for an account the read would offer more', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['webauthn', 'totp'], true),
      route: () => jsonResponse(200, { backup_codes: ['AAAA1111'] }),
    });
    render(<Host />);

    await fillAndSend();
    await waitForRequests();

    expect(readCount(mockApiFetch)).toBe(0);
  });

  it('shows the password and the authenticator code at once, with no wait for a read', () => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(200) });
    render(<Host />);

    expect(password()).toBeInTheDocument();
    expect(code()).toBeInTheDocument();
  });

  // Mutant: `allowBackup: true`, or a read that lets webauthn into the set.
  it('offers no backup code and no security key, and no way to switch', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['webauthn', 'totp'], true),
      route: () => jsonResponse(200),
    });
    render(<Host />);

    expect(screen.queryByLabelText('Backup code')).not.toBeInTheDocument();
    expect(
      screen.queryByRole('group', { name: 'Passkey or security key' })
    ).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /instead/i })).not.toBeInTheDocument();
  });

  it('keeps the primary aria-disabled until both fields are complete', async () => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(200) });
    render(<Host />);

    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await userEvent.type(password(), FIXTURE_PW);
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await userEvent.type(code(), FIXTURE_OTP);
    expect(primary()).not.toHaveAttribute('aria-disabled');
    // Never natively disabled: it must stay focusable.
    expect(primary()).not.toBeDisabled();
  });
});

describe('the request', () => {
  // The route binds the code as `code`, not the seam's `mfa_code` (C4).
  // Mutant: sending `mfa_code` (the route would read an empty code and answer 400).
  it('carries the password and the code as `code`, never `mfa_code`', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(200, { backup_codes: ['AAAA1111'] }),
    });
    render(<Host />);

    await fillAndSend();
    await waitForRequests();

    expect(requests()[0][1]).toMatchObject({ method: 'POST' });
    const [body] = bodiesTo(mockApiFetch, PATH);
    expect(body).toEqual({ password: FIXTURE_PW, code: FIXTURE_OTP });
    expect(body).not.toHaveProperty('mfa_code');
  });

  it('hands the new codes to the host, dropping anything that is not a string', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(200, { backup_codes: ['AAAA1111', 7, null, 'BBBB2222'] }),
    });
    render(<Host />);

    await fillAndSend();

    await waitFor(() => expect(onRegenerated).toHaveBeenCalledWith(['AAAA1111', 'BBBB2222']));
    expect(onRegenerated).toHaveBeenCalledTimes(1);
  });

  // The server has replaced the old codes by the time it answers 200, so a host
  // handed `[]` would show the user nothing to save and call it done.
  // Mutant: `acceptedResult` returning `regenerated` for an empty list.
  it.each<[string, () => Response]>([
    ['no backup_codes member', () => jsonResponse(200, {})],
    ['an empty list', () => jsonResponse(200, { backup_codes: [] })],
    ['a list of no strings', () => jsonResponse(200, { backup_codes: [7, null] })],
    ['a string, not a list', () => jsonResponse(200, { backup_codes: 'AAAA1111' })],
    ['a body that is not JSON', () => new Response('<html>', { status: 200 })],
  ])('a 200 with %s is a failure, never an empty list', async (_name, answer) => {
    installStepUpApi(mockApiFetch, { route: answer });
    render(<Host />);

    await fillAndSend();

    expect(await screen.findByText(UNREADABLE_CODES)).toBeInTheDocument();
    expect(onRegenerated).not.toHaveBeenCalled();
    // The request was answered: the code may be spent, so it is not offered again.
    expect(code()).toHaveValue('');
    expect(screen.getByRole('button', { name: 'Regenerate Codes' })).toBeInTheDocument();
  });

  it('shows the in-flight label and cannot be cancelled while the request is out', async () => {
    const gate = deferred<Response>();
    installStepUpApi(mockApiFetch, { route: () => gate.promise });
    render(<Host />);

    await fillAndSend();

    const busy = await screen.findByRole('button', { name: 'Regenerating...' });
    expect(busy).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();

    gate.resolve(jsonResponse(200, { backup_codes: ['AAAA1111'] }));
    await waitFor(() => expect(onRegenerated).toHaveBeenCalledTimes(1));
  });

  it('Cancel hands control back to the host', async () => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(200) });
    render(<Host />);

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  // Mutant: calling onRegenerated without the currentness check hands one account's
  // codes to the screen of whoever is signed in by then.
  it('does not hand over codes that arrive after the account changed', async () => {
    const gate = deferred<Response>();
    installStepUpApi(mockApiFetch, { route: () => gate.promise });
    render(<Host />);

    await fillAndSend();
    await waitForRequests();
    useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
    gate.resolve(jsonResponse(200, { backup_codes: ['AAAA1111'] }));

    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Regenerating...' })).not.toBeInTheDocument()
    );
    expect(onRegenerated).not.toHaveBeenCalled();
  });
});

describe('what the route answers', () => {
  // Mutant: `includes` for `===` in the adapter, or no mapping at all (the string would
  // surface as a banner instead of on the field).
  it('403 Incorrect password shows on the password field, empties it, and keeps the code', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(403, { error: 'Incorrect password' }),
    });
    render(<Host />);

    await fillAndSend();

    expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
    expect(password()).toHaveValue('');
    expect(password()).toHaveAttribute('aria-invalid', 'true');
    expect(code()).toHaveValue(FIXTURE_OTP);
    expect(onRegenerated).not.toHaveBeenCalled();
  });

  it('403 Invalid TOTP code shows on the code panel and clears the code', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(403, { error: 'Invalid TOTP code' }),
    });
    render(<Host />);

    await fillAndSend();

    expect(await screen.findByText(/That code didn't work/)).toBeInTheDocument();
    expect(code()).toHaveValue('');
    expect(password()).toHaveValue(FIXTURE_PW);
    await waitFor(() => expect(code()).toHaveFocus());
  });

  it('a 400 for missing fields says something went wrong, not that the code failed', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(400, { error: 'Password and TOTP code are required' }),
    });
    render(<Host />);

    await fillAndSend();

    expect(await screen.findByText('Something went wrong. Try again.')).toBeInTheDocument();
    expect(onTotpRemoved).not.toHaveBeenCalled();
  });

  // The route's limiter answers before the handler reads anything, so the code
  // was not spent: as an unowned answer it would be recorded as a TOTP
  // acceptance (the "already used" hint) and shown as the limiter's own text.
  // Mutant: the adapter's 429 arm deleted (null → `failed` → `answered`).
  it('a 429 says to wait, and records no TOTP acceptance', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(429, { error: 'Rate limit exceeded' }),
    });
    render(<Host />);

    await fillAndSend();

    expect(await screen.findByText(RATE_LIMITED)).toBeInTheDocument();
    expect(screen.queryByText('Rate limit exceeded')).not.toBeInTheDocument();
    expect(useTotpAcceptedStore.getState().acceptedAt).toEqual({});
  });

  // Positive control for the case above: an answer that may have spent the code
  // IS recorded, so an empty store there is not an artefact of the harness.
  it('a 500 records the TOTP acceptance', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(500, { error: 'Failed to generate backup codes' }),
    });
    render(<Host />);

    await fillAndSend();

    await screen.findByText('Failed to generate backup codes');
    expect(useTotpAcceptedStore.getState().acceptedAt).toHaveProperty('acct-1');
  });

  it('an unowned answer keeps the surface error: the server text', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(500, { error: 'Failed to generate backup codes' }),
    });
    render(<Host />);

    await fillAndSend();

    expect(await screen.findByText('Failed to generate backup codes')).toBeInTheDocument();
  });

  it('a bodiless failure says regeneration failed', async () => {
    installStepUpApi(mockApiFetch, { route: () => new Response(null, { status: 502 }) });
    render(<Host />);

    await fillAndSend();

    expect(await screen.findByText('Failed to regenerate backup codes')).toBeInTheDocument();
  });

  it('a transport failure says the server could not be reached', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => {
        throw new TypeError('Failed to fetch');
      },
    });
    render(<Host />);

    await fillAndSend();

    expect(
      await screen.findByText("Couldn't reach the server. Check your connection and try again.")
    ).toBeInTheDocument();
  });

  // The server accepts each code once and can accept one yet still fail the request.
  it('a failed request clears the code and leaves the primary inert', async () => {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(500, { error: 'Failed to generate backup codes' }),
    });
    render(<Host />);

    await fillAndSend();

    await waitFor(() => expect(primary()).toHaveAttribute('aria-disabled', 'true'));
    expect(code()).toHaveValue('');
  });

  // Mutant: routing these answers through the seam classifier.
  it('never calls classifyStepUpRefusal', async () => {
    let n = 0;
    installStepUpApi(mockApiFetch, {
      route: () =>
        ++n === 1
          ? jsonResponse(403, { error: 'Incorrect password' })
          : jsonResponse(403, { error: 'Invalid TOTP code' }),
    });
    render(<Host />);

    await fillAndSend();
    await screen.findByText('That password is not correct.');
    // The code survives a refused password; only the password is typed again.
    await userEvent.type(password(), FIXTURE_PW);
    await userEvent.click(primary());
    await waitForRequests(2);

    expect(mockClassify).not.toHaveBeenCalled();
  });
});

describe('TOTP was turned off after the stage opened (Q7)', () => {
  async function reachTotpRemoved(): Promise<void> {
    installStepUpApi(mockApiFetch, {
      route: () => jsonResponse(400, { error: 'TOTP is not enabled' }),
    });
    render(<Host />);
    await fillAndSend();
    await screen.findByText(TOTP_REMOVED);
  }

  // Mutant: a `failed` message shown verbatim ("TOTP is not enabled"), or credentials left up.
  it('says so, hides the credentials, and offers only Close', async () => {
    await reachTotpRemoved();

    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Regenerate Codes|Regenerating/ })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument();
    expect(screen.queryByText('TOTP is not enabled')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument();
  });

  // Mutant: dropping `onTotpRemoved`: the host keeps showing a TOTP-only screen as current.
  it('tells the host once, so it can re-read the MFA status', async () => {
    await reachTotpRemoved();

    expect(onTotpRemoved).toHaveBeenCalledTimes(1);
    expect(onRegenerated).not.toHaveBeenCalled();
  });

  it('moves focus to Close, and Close hands control back', async () => {
    await reachTotpRemoved();

    const close = screen.getByRole('button', { name: 'Close' });
    await waitFor(() => expect(close).toHaveFocus());
    await userEvent.click(close);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  // The stage ends only on the route's exact 400 (C5). Anything near it is an
  // ordinary failure that leaves the credentials up.
  // Mutant: the stage's match widened to `includes`, case-folded, or any status.
  it.each<[string, number, string]>([
    ['a trailing period', 400, 'TOTP is not enabled.'],
    ['a different case', 400, 'totp is not enabled'],
    ['a superstring', 400, 'TOTP is not enabled for this account'],
    ['the right string under 403', 403, 'TOTP is not enabled'],
    ['the right string under 500', 500, 'TOTP is not enabled'],
  ])('%s does not end the stage', async (_name, status, error) => {
    installStepUpApi(mockApiFetch, { route: () => jsonResponse(status, { error }) });
    render(<Host />);

    await fillAndSend();

    expect(await screen.findByText(error)).toBeInTheDocument();
    expect(onTotpRemoved).not.toHaveBeenCalled();
    expect(screen.queryByText(TOTP_REMOVED)).not.toBeInTheDocument();
    expect(password()).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
  });
});
