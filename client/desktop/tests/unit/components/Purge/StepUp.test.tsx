import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';
import { usePrivacyStore, type PrivacySettings } from '@/renderer/stores/privacyStore';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';
import { useSettingsOverlayStore } from '@/renderer/stores/settingsOverlayStore';

// Named fixtures, used only by reference: the pre-commit detect-secrets hook
// flags a credential-shaped key sitting beside a quoted literal regardless of
// the value, and an allowlist pragma would blind a detector we want live on
// this path. Mirrors tests/unit/services/purgeApi.test.ts.
// Both values are deliberately long and distinctive. These fixtures are used as
// NEEDLES in `not.toContain` leak sweeps over serialized store, storage and log
// content, so a short value ('pw') risks colliding with unrelated text and
// turning a real signal into noise — or, read the other way, makes a passing
// sweep unconvincing.
const FIXTURE_PW = 'fixture-password-do-not-persist';
const FIXTURE_OTP = '314159';

const DM_ROUTE = '*/api/v1/dm/conversations/:id/messages';

const noop = () => {};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => {
  server.resetHandlers();
  vi.restoreAllMocks();
});
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  // settingsNavStore is not part of resetAllStores, and this suite both seeds
  // and asserts its focus request.
  useSettingsNavStore.getState().clearFocusRequest();
});

/**
 * Task 7 (#1354) adds `requireAuthBeforePurge` to the privacy store; this suite
 * must not depend on its landing order, so the field is written through a cast
 * and `undefined` models both "not fetched yet" and "old server omitted it".
 */
function setRequireAuthBeforePurge(value: boolean | undefined): void {
  const settings = { ...usePrivacyStore.getState().settings } as PrivacySettings & {
    requireAuthBeforePurge?: boolean;
  };
  if (value === undefined) {
    delete settings.requireAuthBeforePurge;
  } else {
    settings.requireAuthBeforePurge = value;
  }
  usePrivacyStore.setState({ settings });
}

/** Every request body the DM purge route received, in order. */
function captureDmBodies(bodies: unknown[], respond: () => Response): void {
  server.use(
    http.delete(DM_ROUTE, async ({ request }) => {
      bodies.push(await request.json());
      return respond();
    })
  );
}

function renderDm(onClose: () => void = noop) {
  return render(
    <PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={onClose} />
  );
}

async function reachStepUp(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
  await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
}

function passwordField(): HTMLInputElement {
  return screen.getByLabelText('Password') as HTMLInputElement;
}

function codeField(): HTMLInputElement {
  return screen.getByLabelText('Authentication code') as HTMLInputElement;
}

// Every Zustand store in the renderer, resolved from the filesystem rather than
// an import list so a store added later is covered without editing this file.
// The negative pattern excludes the one co-located spec in that directory —
// importing it would re-register its suite inside this file.
const storeModules: Record<string, unknown> = import.meta.glob(
  ['../../../../src/renderer/stores/*.ts', '!../../../../src/renderer/stores/*.test.ts'],
  { eager: true }
);

function allStoreSnapshots(): Record<string, unknown> {
  const snapshots: Record<string, unknown> = {};
  for (const [path, mod] of Object.entries(storeModules)) {
    for (const [name, exported] of Object.entries(mod as Record<string, unknown>)) {
      const candidate = exported as { getState?: () => unknown };
      if (typeof candidate?.getState === 'function') {
        snapshots[`${path}#${name}`] = candidate.getState();
      }
    }
  }
  return snapshots;
}

/** JSON.stringify that survives Maps, Sets, bigints and shared references. */
function serializeDeep(value: unknown): string {
  const seen = new WeakSet<object>();
  return (
    JSON.stringify(value, (_key, val: unknown) => {
      if (typeof val === 'bigint') return val.toString();
      if (val instanceof Map) return Array.from(val.entries());
      if (val instanceof Set) return Array.from(val.values());
      if (typeof val === 'object' && val !== null) {
        if (seen.has(val)) return '[circular]';
        seen.add(val);
      }
      return val;
    }) ?? ''
  );
}

describe('PurgeMessagesModal — step-up gate', () => {
  it('fails closed to the step-up stage when the preference is unknown', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, () => HttpResponse.json({ deleted_count: 0, hidden_count: 0 }));
    // An old server omits require_auth_before_purge entirely, and the server's
    // own DM purge handler fail-closes to true.
    setRequireAuthBeforePurge(undefined);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    expect(screen.getByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
    expect(bodies).toHaveLength(0);
  });

  it('collects credentials before spending a request when the preference is on', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, () => HttpResponse.json({ deleted_count: 0, hidden_count: 0 }));
    setRequireAuthBeforePurge(true);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    expect(screen.getByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
    expect(bodies).toHaveLength(0);
  });

  it('submits a channel purge directly — step-up is DM and group only', async () => {
    setRequireAuthBeforePurge(undefined);
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ deleted_count: 4, hidden_count: 0 })
      )
    );

    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    await reachStepUp(user);

    expect(await screen.findByText('Purged 4 messages.')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Confirm it is you' })).not.toBeInTheDocument();
  });

  it('purges directly when the user has turned the preference off', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, () => HttpResponse.json({ deleted_count: 2, hidden_count: 1 }));
    setRequireAuthBeforePurge(false);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ range: '7d' });
    expect(screen.queryByRole('heading', { name: 'Confirm it is you' })).not.toBeInTheDocument();
  });
});

describe('PurgeMessagesModal — single-shot submission', () => {
  it('sends both factors in the first request', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, () => HttpResponse.json({ deleted_count: 3, hidden_count: 1 }));

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));

    // Exactly one request: each probe would spend the same rate-limited purge
    // budget on a call that could never succeed (spec R-7).
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({
      range: '7d',
      current_password: FIXTURE_PW,
      mfa_code: FIXTURE_OTP,
    });
    expect(
      await screen.findByText('Purged 3 messages. 1 more hidden from you.')
    ).toBeInTheDocument();
  });

  it('sends the code alone when the server asked for MFA only', async () => {
    const bodies: unknown[] = [];
    setRequireAuthBeforePurge(false);
    server.use(
      http.delete(DM_ROUTE, async ({ request }) => {
        bodies.push(await request.json());
        if (bodies.length === 1) {
          return HttpResponse.json(
            { error: 'MFA required', mfa_required: true, methods: ['totp'] },
            { status: 403 }
          );
        }
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    // An SSO account with MFA has no password to offer, so the stage must not
    // demand one (copy deck §5).
    expect(await screen.findByLabelText('Authentication code')).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();

    await user.type(codeField(), FIXTURE_OTP);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));

    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1]).toEqual({ range: '7d', mfa_code: FIXTURE_OTP });
  });

  it('reveals the fields when a credential-less purge is refused', async () => {
    setRequireAuthBeforePurge(false);
    server.use(
      http.delete(DM_ROUTE, () =>
        HttpResponse.json({ error: 'password_required', password_required: true }, { status: 403 })
      )
    );

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    expect(await screen.findByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
    expect(passwordField()).toBeInTheDocument();
  });
});

describe('PurgeMessagesModal — secret containment', () => {
  it('sweeps a store surface that would actually surface a leak', () => {
    // Without this the "not.toContain" below could pass because the sweep found
    // nothing. Seeding a store with the fixture proves the detector fires.
    useSettingsNavStore.getState().requestFocus('privacy', FIXTURE_OTP);
    const snapshots = allStoreSnapshots();

    expect(Object.keys(snapshots).length).toBeGreaterThan(20);
    expect(serializeDeep(snapshots)).toContain(FIXTURE_OTP);
  });

  it('never writes the password or the code into a store, storage or a log', async () => {
    const consoleSpies = (['log', 'info', 'warn', 'error', 'debug'] as const).map((level) =>
      vi.spyOn(console, level).mockImplementation(() => {})
    );
    captureDmBodies([], () => HttpResponse.json({ deleted_count: 1, hidden_count: 0 }));

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));
    await screen.findByText('Purged 1 message.');

    const serialized = serializeDeep(allStoreSnapshots());
    expect(serialized).not.toContain(FIXTURE_PW);
    expect(serialized).not.toContain(FIXTURE_OTP);

    // Enumerate through the Storage API, never by spreading. jsdom keeps entries
    // in an internal slot rather than as own enumerable properties, so
    // `{ ...localStorage }` yields `{}` and the sweep below would pass on an
    // empty string no matter what the component wrote — a vacuous assertion
    // wearing the shape of a real one.
    const dumpStorage = (store: Storage): string => {
      const entries: Array<[string, string]> = [];
      for (let i = 0; i < store.length; i += 1) {
        const key = store.key(i);
        if (key !== null) entries.push([key, store.getItem(key) ?? '']);
      }
      return serializeDeep(entries);
    };

    // Positive control: prove the enumeration can see a value at all, so a
    // future regression that empties it cannot masquerade as "no leak found".
    localStorage.setItem('purge-storage-probe', FIXTURE_PW);
    expect(dumpStorage(localStorage)).toContain(FIXTURE_PW);
    localStorage.removeItem('purge-storage-probe');

    const storage = `${dumpStorage(localStorage)}${dumpStorage(sessionStorage)}`;
    expect(storage).not.toContain(FIXTURE_PW);
    expect(storage).not.toContain(FIXTURE_OTP);

    const logged = consoleSpies
      .flatMap((spy) => spy.mock.calls)
      .map((call) => serializeDeep(call))
      .join('');
    expect(logged).not.toContain(FIXTURE_PW);
    expect(logged).not.toContain(FIXTURE_OTP);
  });
});

describe('PurgeMessagesModal — per-field errors', () => {
  it('marks only the password field invalid on a wrong password', async () => {
    server.use(
      http.delete(DM_ROUTE, () => HttpResponse.json({ error: 'Invalid password' }, { status: 403 }))
    );

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));

    expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
    await waitFor(() => expect(passwordField()).toHaveAttribute('aria-invalid', 'true'));
    // A wrong password says nothing about the code the user typed.
    expect(codeField()).not.toHaveAttribute('aria-invalid');
    expect(passwordField()).toHaveFocus();
    expect(passwordField()).toHaveValue('');
    expect(codeField()).toHaveValue(FIXTURE_OTP);
  });

  it('marks only the code field invalid on a wrong code', async () => {
    server.use(
      http.delete(DM_ROUTE, () => HttpResponse.json({ error: 'Invalid MFA code' }, { status: 403 }))
    );

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));

    expect(
      await screen.findByText('That code is not correct, or it has expired. Try the next one.')
    ).toBeInTheDocument();
    await waitFor(() => expect(codeField()).toHaveAttribute('aria-invalid', 'true'));
    expect(passwordField()).not.toHaveAttribute('aria-invalid');
    expect(codeField()).toHaveFocus();
    expect(codeField()).toHaveValue('');
  });
});

describe('PurgeMessagesModal — step-up dead end', () => {
  it('replaces the stage with a dead-end card when the account cannot step up', async () => {
    server.use(
      http.delete(DM_ROUTE, () => HttpResponse.json({ error: 'no credentials' }, { status: 400 }))
    );

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));

    expect(await screen.findByText(/signs in without a password/i)).toBeInTheDocument();
    // Nothing the user could type would work, so no retryable field survives.
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Authentication code')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Confirm and Purge' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Go to Privacy & Security' })).toBeInTheDocument();
  });

  it('sends the user to the named setting and closes the modal', async () => {
    server.use(
      http.delete(DM_ROUTE, () => HttpResponse.json({ error: 'no credentials' }, { status: 400 }))
    );
    let closed = false;

    const user = userEvent.setup();
    renderDm(() => {
      closed = true;
    });
    await reachStepUp(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));
    await user.click(await screen.findByRole('button', { name: 'Go to Privacy & Security' }));

    // SettingsPage consumes a focus request only while it is mounted, and this
    // card is reachable only from a DM/group entry point with Settings closed —
    // so without the overlay open the button closes the modal and does nothing.
    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'privacy',
      controlId: 'requireAuthBeforePurge',
    });
    expect(closed).toBe(true);
  });
});

describe('PurgeMessagesModal — step-up accessibility', () => {
  it('moves focus to the stage heading without renaming the dialog', async () => {
    const user = userEvent.setup();
    renderDm();
    expect(screen.getByRole('heading', { name: 'Purge Messages' })).toBeInTheDocument();

    await reachStepUp(user);

    const heading = screen.getByRole('heading', { name: 'Confirm it is you' });
    await waitFor(() => expect(heading).toHaveFocus());
    // Renaming a dialog mid-interaction breaks WCAG 4.1.2 / 3.2.2.
    expect(screen.getByRole('heading', { name: 'Purge Messages' })).toBeInTheDocument();
  });

  it('gives each field the autofill hints its credential type needs', async () => {
    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    expect(passwordField()).toHaveAttribute('autocomplete', 'current-password');
    expect(passwordField()).toHaveAttribute('type', 'password');
    expect(codeField()).toHaveAttribute('autocomplete', 'one-time-code');
    expect(codeField()).toHaveAttribute('inputmode', 'numeric');
  });

  it('drops the entered credentials when the dialog closes', async () => {
    const user = userEvent.setup();
    const { rerender } = renderDm();
    await reachStepUp(user);
    await user.type(passwordField(), FIXTURE_PW);

    // An entry point that keeps this component mounted across close must not
    // carry a previous attempt's secrets into the next open.
    rerender(
      <PurgeMessagesModal
        context="dm"
        isOpen={false}
        scopeId="d1"
        scopeName="Alex"
        onClose={noop}
      />
    );
    rerender(
      <PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={noop} />
    );

    // The close also rewinds the stage, so the reopened dialog offers no
    // credential field at all; re-reaching step-up gets an empty one.
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    await reachStepUp(user);
    expect(passwordField()).toHaveValue('');
  });

  it('keeps the submit disabled until a credential is entered', async () => {
    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    const submit = screen.getByRole('button', { name: 'Confirm and Purge' });
    expect(submit).toBeDisabled();

    await user.type(codeField(), FIXTURE_OTP);
    expect(submit).toBeEnabled();
  });
});
