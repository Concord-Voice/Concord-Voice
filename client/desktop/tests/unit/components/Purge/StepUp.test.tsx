import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { act, render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';
import { purgeMessages } from '@/renderer/services/messaging/purgeApi';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { usePrivacyStore, type PrivacySettings } from '@/renderer/stores/ui/privacyStore';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';

// The DM/group purge step-up stage on the shared credentials stage
// (StepUpCredentials + useStepUpFactor, purpose `dm.purge`, password always
// shown, a failed read blocks, backup allowed). Every test mocks
// GET /api/v1/mfa/step-up explicitly: the read starts when the dialog opens,
// and an unmocked read falls into the blocked state.
//
// Decisions pinned here that differ from the pre-picker suite:
// - the code field is "Authenticator app code" and exists only when the read
//   offered TOTP (an empty or unsupported read is the password alone);
// - the primary is `aria-disabled` rather than `disabled`, and its guard moves
//   focus to the first empty field;
// - the password is required before the primary activates whatever factor is
//   supplied (D19 was reverted): an empty password beside a code or a security
//   key sends nothing and focuses the password field;
// - a thrown AbortError is "not sent" (the code is unspent), not a network error.
//
// "Mutant:" comments name the production change a test exists to turn red.

// A passthrough spy: the real purgeMessages runs, and the second argument (the
// hook's request context) becomes observable. Deliberately a `vi.fn(impl)`, not
// a `vi.spyOn`, so `vi.restoreAllMocks` below leaves it alone.
vi.mock('@/renderer/services/messaging/purgeApi', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/messaging/purgeApi')>();
  return { ...actual, purgeMessages: vi.fn(actual.purgeMessages) };
});
const purgeSpy = vi.mocked(purgeMessages);

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
const FIXTURE_TOKEN = 'inline-token-do-not-persist';

const DM_ROUTE = '*/api/v1/dm/conversations/:id/messages';
const READ = '*/api/v1/mfa/step-up';
const BEGIN = '*/api/v1/mfa/webauthn/verify-inline/begin';
const FINISH = '*/api/v1/mfa/webauthn/verify-inline/finish';

const BLOCKED_TEXT =
  "We couldn't check your verification methods. Check your connection and try again.";
const SESSION_TEXT = 'Your session expired. Nothing was purged. Sign in again, then try again.';

const noop = () => {};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => {
  server.resetHandlers();
  vi.restoreAllMocks();
  Reflect.deleteProperty(navigator, 'credentials');
});
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  purgeSpy.mockClear();
  // settingsNavStore is not part of resetAllStores, and this suite both seeds
  // and asserts its focus request.
  useSettingsNavStore.getState().clearFocusRequest();
  readReturns(TOTP_READ);
});

// ── Wire helpers ─────────────────────────────────────────────────────────

interface ReadBody {
  methods: string[];
  default_method: string | null;
  backup_code_available: boolean;
}

/** Answers the requirements read; a later call for the same route wins. */
function mockRead(respond: () => Response | Promise<Response>): { count: () => number } {
  let hits = 0;
  server.use(
    http.get(READ, () => {
      hits += 1;
      return respond();
    })
  );
  return { count: () => hits };
}

function readReturns(body: ReadBody) {
  return mockRead(() => HttpResponse.json(body));
}

function readStatus(status: number, body: unknown = { error: 'x' }) {
  return mockRead(() => HttpResponse.json(body, { status }));
}

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

const OK = () => HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
const refusal =
  (body: unknown, status = 403) =>
  () =>
    HttpResponse.json(body, { status });

function renderDm(onClose: () => void = noop) {
  return render(
    <PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={onClose} />
  );
}

async function reachStepUp(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
  await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
}

/** The stage, with the read answered and the TOTP panel on screen. */
async function reachReady(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await reachStepUp(user);
  await screen.findByLabelText('Authenticator app code');
}

/**
 * The stage after a read that offers no panel (an empty or unsupported read):
 * the picker's region stops being "reserved" when the read settles, which is
 * the one observable difference from the read still being in flight.
 */
async function reachPasswordOnly(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await reachStepUp(user);
  await waitFor(() => expect(document.querySelector('.step-up__region--reserved')).toBeNull());
}

function passwordField(): HTMLInputElement {
  return screen.getByLabelText('Password') as HTMLInputElement;
}

function codeField(): HTMLInputElement {
  return screen.getByLabelText('Authenticator app code') as HTMLInputElement;
}

const primary = () => screen.getByRole('button', { name: 'Confirm and Purge' });
const heading = () => screen.getByRole('heading', { name: 'Confirm it is you' });

// ── Secret sweeps ────────────────────────────────────────────────────────

// Every Zustand store in the renderer, resolved from the filesystem rather than
// an import list so a store added later is covered without editing this file.
// The negative pattern excludes the one co-located spec in that directory —
// importing it would re-register its suite inside this file.
const storeModules: Record<string, unknown> = import.meta.glob(
  ['../../../../src/renderer/stores/**/*.ts', '!../../../../src/renderer/stores/**/*.test.ts'],
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

/** Enumerate through the Storage API, never by spreading: jsdom keeps entries in an internal slot. */
function dumpStorage(store: Storage): string {
  const entries: Array<[string, string]> = [];
  for (let i = 0; i < store.length; i += 1) {
    const key = store.key(i);
    if (key !== null) entries.push([key, store.getItem(key) ?? '']);
  }
  return serializeDeep(entries);
}

function silenceConsole() {
  return (['log', 'info', 'warn', 'error', 'debug'] as const).map((level) =>
    vi.spyOn(console, level).mockImplementation(() => {})
  );
}

/** Asserts none of `needles` reached a store, storage or a console call. */
function expectNoLeak(needles: string[], consoleSpies: ReturnType<typeof silenceConsole>) {
  // Positive control: prove the enumeration can see a value at all, so a
  // future regression that empties it cannot masquerade as "no leak found".
  localStorage.setItem('purge-storage-probe', needles[0]);
  expect(dumpStorage(localStorage)).toContain(needles[0]);
  localStorage.removeItem('purge-storage-probe');

  const everywhere = [
    serializeDeep(allStoreSnapshots()),
    dumpStorage(localStorage),
    dumpStorage(sessionStorage),
    consoleSpies
      .flatMap((spy) => spy.mock.calls)
      .map((call) => serializeDeep(call))
      .join(''),
  ].join('\n');
  for (const needle of needles) expect(everywhere).not.toContain(needle);
}

// ── The security-key ceremony ────────────────────────────────────────────

const CREDENTIAL = {
  id: 'credential-id',
  rawId: new Uint8Array([1, 2, 3]).buffer,
  type: 'public-key',
  response: {
    authenticatorData: new Uint8Array([10, 20]).buffer,
    clientDataJSON: new Uint8Array([30, 40]).buffer,
    signature: new Uint8Array([50, 60]).buffer,
    userHandle: null,
  },
};

const TOTP_READ: ReadBody = {
  methods: ['totp'],
  default_method: 'totp',
  backup_code_available: false,
};

const KEY_READ: ReadBody = {
  methods: ['webauthn', 'totp'],
  default_method: 'webauthn',
  backup_code_available: false,
};

/** Installs the ceremony; returns the bodies begin and finish saw and the browser stub. */
function installCeremony(browser: () => Promise<unknown> = async () => CREDENTIAL) {
  const begins: unknown[] = [];
  const finishes: unknown[] = [];
  const get = vi.fn(browser);
  Object.defineProperty(navigator, 'credentials', {
    value: { get },
    writable: true,
    configurable: true,
  });
  server.use(
    http.post(BEGIN, async ({ request }) => {
      begins.push(await request.json());
      return HttpResponse.json({
        publicKey: { challenge: 'AQID', rpId: 'localhost', allowCredentials: [] },
      });
    }),
    http.post(FINISH, async ({ request }) => {
      finishes.push(await request.json());
      return HttpResponse.json({ mfa_token: FIXTURE_TOKEN });
    })
  );
  return { begins, finishes, get };
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

    expect(heading()).toBeInTheDocument();
    expect(bodies).toHaveLength(0);
  });

  it('collects credentials before spending a request when the preference is on', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, () => HttpResponse.json({ deleted_count: 0, hidden_count: 0 }));
    setRequireAuthBeforePurge(true);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    expect(heading()).toBeInTheDocument();
    expect(bodies).toHaveLength(0);
  });

  it.each([
    ['channel', '*/api/v1/channels/:id/messages', 'c1', 'general'],
    ['server', '*/api/v1/servers/:id/messages', 's1', 'Guild'],
  ] as const)(
    'submits a %s purge directly and never issues the requirements read',
    async (context, route, scopeId, scopeName) => {
      setRequireAuthBeforePurge(undefined);
      const reads = readReturns(TOTP_READ);
      server.use(
        http.delete(route, () => HttpResponse.json({ deleted_count: 4, hidden_count: 0 }))
      );

      const user = userEvent.setup();
      render(
        <PurgeMessagesModal
          context={context}
          isOpen
          scopeId={scopeId}
          scopeName={scopeName}
          onClose={noop}
        />
      );
      await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
      if (context === 'server') {
        await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
      }
      await user.click(screen.getByRole('button', { name: 'Purge Messages' }));

      // Mutant: the channel/server path enabling the factor hook, which would
      // spend a request on a stage it never shows.
      expect(await screen.findByRole('button', { name: 'Done' })).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Confirm it is you' })).not.toBeInTheDocument();
      expect(reads.count()).toBe(0);
    }
  );

  it('purges directly, and reads nothing, when the user has turned the preference off', async () => {
    const reads = readReturns(TOTP_READ);
    const bodies: unknown[] = [];
    captureDmBodies(bodies, () => HttpResponse.json({ deleted_count: 2, hidden_count: 1 }));
    setRequireAuthBeforePurge(false);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ range: '7d' });
    expect(screen.queryByRole('heading', { name: 'Confirm it is you' })).not.toBeInTheDocument();
    expect(reads.count()).toBe(0);
  });

  it('starts the read when the dialog opens, before any range is chosen', async () => {
    const reads = readReturns(TOTP_READ);
    renderDm();
    await waitFor(() => expect(reads.count()).toBe(1));
  });

  it('reads nothing while the dialog is closed', async () => {
    const reads = readReturns(TOTP_READ);
    render(
      <PurgeMessagesModal
        context="dm"
        isOpen={false}
        scopeId="d1"
        scopeName="Alex"
        onClose={noop}
      />
    );
    await act(async () => {});
    expect(reads.count()).toBe(0);
  });

  it('serves a group conversation the same stage', async () => {
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal context="group" isOpen scopeId="g1" scopeName="Crew" onClose={noop} />
    );
    await reachStepUp(user);
    expect(heading()).toBeInTheDocument();
    expect(await screen.findByLabelText('Authenticator app code')).toBeInTheDocument();
  });
});

describe('PurgeMessagesModal — what the read decides', () => {
  // Mutant: the §1.3 regression, an unconditional code field. An account that
  // offers no inline method was shown a box it could not fill.
  it('shows the password alone when the account offers no inline method', async () => {
    const reads = readReturns({ methods: [], default_method: null, backup_code_available: false });
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);

    const user = userEvent.setup();
    renderDm();
    await reachPasswordOnly(user);

    expect(reads.count()).toBe(1);
    expect(passwordField()).toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /instead$/ })).not.toBeInTheDocument();

    await user.type(passwordField(), FIXTURE_PW);
    await user.click(primary());
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ range: '7d', current_password: FIXTURE_PW });
  });

  // Mutant: `unsupported` blocking #1. A server that predates the route is
  // permanent, so the password alone must still work there.
  it('shows the password alone, not the blocked state, on a server without the route', async () => {
    readStatus(404);
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);

    const user = userEvent.setup();
    renderDm();
    await reachPasswordOnly(user);

    expect(screen.queryByText(BLOCKED_TEXT)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    await user.type(passwordField(), FIXTURE_PW);
    await user.click(primary());
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ range: '7d', current_password: FIXTURE_PW });
  });

  // Mutant: `unavailable` falling through to the password leg on #1. The purge
  // is irreversible and its attempt budget is the purge's own, so a failed read
  // blocks with Retry rather than guessing which factors to ask for.
  it.each([
    ['a 503', () => readStatus(503)],
    ['a 429', () => readStatus(429)],
    ['a malformed 200', () => mockRead(() => HttpResponse.json({ methods: 'totp' }))],
    ['a dropped connection', () => mockRead(() => HttpResponse.error())],
  ] as const)('blocks with Retry when the read fails with %s', async (_name, install) => {
    install();
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    expect(await screen.findByText(BLOCKED_TEXT)).toBeInTheDocument();
    // Nothing to type and nothing to send: no password field, no code field.
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await user.click(primary());
    expect(bodies).toHaveLength(0);
  });

  it('Retry reads again and, when it succeeds, opens the stage', async () => {
    let calls = 0;
    const reads = mockRead(() => {
      calls += 1;
      return calls === 1
        ? HttpResponse.json({ error: 'down' }, { status: 503 })
        : HttpResponse.json(TOTP_READ);
    });

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await user.click(await screen.findByRole('button', { name: 'Retry' }));

    expect(await screen.findByLabelText('Authenticator app code')).toBeInTheDocument();
    expect(passwordField()).toBeInTheDocument();
    expect(reads.count()).toBe(2);
    expect(screen.queryByText(BLOCKED_TEXT)).not.toBeInTheDocument();
  });

  it('keeps focus in the dialog when Retry unmounts', async () => {
    readStatus(503);
    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await user.click(await screen.findByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(heading()).toHaveFocus());
  });

  // Refused is terminal: the action route sits behind the same middleware, so
  // retrying or typing a password cannot help.
  it.each([
    ['account', 403, { error_code: 'account_disabled' }, "Your account can't do this right now."],
    [
      'emailUnverified',
      403,
      { code: 'EMAIL_NOT_VERIFIED' },
      'Verify your email address to do this.',
    ],
    ['client', 400, { error: 'bad' }, "This isn't available right now."],
  ] as const)(
    'ends on the %s refusal with no Retry and no password field',
    async (_reason, status, body, text) => {
      readStatus(status, body);
      const bodies: unknown[] = [];
      captureDmBodies(bodies, OK);

      const user = userEvent.setup();
      renderDm();
      await reachStepUp(user);

      expect(await screen.findByText(text)).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(primary()).toHaveAttribute('aria-disabled', 'true');
      await waitFor(() => expect(heading()).toHaveFocus());
      await user.click(primary());
      expect(bodies).toHaveLength(0);
    }
  );

  it('queues nothing when the primary is pressed while the read is in flight', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    mockRead(async () => {
      await gate;
      return HttpResponse.json(TOTP_READ);
    });
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.click(primary());

    expect(await screen.findByText('Checking your verification methods…')).toBeInTheDocument();
    expect(bodies).toHaveLength(0);

    release();
    await screen.findByLabelText('Authenticator app code');
    // The press was answered with the status line, never queued behind the read.
    await act(async () => {});
    expect(bodies).toHaveLength(0);
    expect(screen.queryByText('Checking your verification methods…')).not.toBeInTheDocument();
  });

  it('offers a backup code only when the read reported one', async () => {
    readReturns({ ...TOTP_READ, backup_code_available: true });
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.click(screen.getByRole('button', { name: 'Use a backup code instead' }));
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    await user.type(screen.getByLabelText('Backup code'), 'ABCD1234');
    await user.type(passwordField(), FIXTURE_PW);
    await user.click(primary());

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ range: '7d', current_password: FIXTURE_PW, mfa_code: 'ABCD1234' });
  });

  it('offers no backup code when the read did not report one', async () => {
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    expect(screen.queryByRole('button', { name: 'Use a backup code instead' })).toBeNull();
  });

  // Authoritative over the read: an mfa_required refusal naming only methods
  // this app cannot collect (email, SMS) is the no-usable-method state.
  it('ends on the no-usable-method state when the refusal names only email', async () => {
    server.use(
      http.delete(
        DM_ROUTE,
        refusal({ error: 'MFA verification required', mfa_required: true, methods: ['email'] })
      )
    );
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

    expect(
      await screen.findByText(
        "Your account's verification method can't be used here. Add an authenticator app or security key in Settings."
      )
    ).toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    await waitFor(() => expect(heading()).toHaveFocus());
  });
});

describe('PurgeMessagesModal — single-shot submission', () => {
  it('sends both factors in the first request', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, () => HttpResponse.json({ deleted_count: 3, hidden_count: 1 }));
    const events: unknown[] = [];
    const listener = (e: Event) => events.push((e as CustomEvent).detail);
    globalThis.addEventListener('messages-purged', listener);

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);

    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

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
    globalThis.removeEventListener('messages-purged', listener);
    // The actor refetches from their own request, not from an echo.
    expect(events).toEqual([{ scopeId: 'd1' }]);
    // The credentials left with the stage.
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  // Mutant: the DM purge request not carrying the hook's request context (the
  // second argument of purgeMessages). Without it the request is its own
  // operation and goes out as whoever is signed in by then.
  it('sends the purge under the capture the factor was proven under', async () => {
    captureDmBodies([], OK);
    useAuthStore.setState({ accessToken: 'tok', sessionId: 'sid', authGeneration: 7 });

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());
    await screen.findByText('Purged 1 message.');

    expect(purgeSpy).toHaveBeenCalledTimes(1);
    const [, requestContext] = purgeSpy.mock.calls[0];
    expect(requestContext).toMatchObject({
      authLifecycle: { sessionId: 'sid', authGeneration: 7 },
      serverSelection: expect.anything(),
    });
  });

  it('keeps the accepted password through an MFA challenge, so the retry carries both', async () => {
    // The read found nothing to offer (an old server), so the password goes
    // alone and the seam's `mfa_required` is what mounts the picker.
    readStatus(404);
    const bodies: unknown[] = [];
    server.use(
      http.delete(DM_ROUTE, async ({ request }) => {
        bodies.push(await request.json());
        if (bodies.length === 1) {
          // The seam checks the password first: this is what a CORRECT
          // password with no code receives.
          return HttpResponse.json(
            { error: 'MFA verification required', mfa_required: true, methods: ['totp'] },
            { status: 403 }
          );
        }
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );

    const user = userEvent.setup();
    renderDm();
    await reachPasswordOnly(user);

    await user.type(passwordField(), FIXTURE_PW);
    await user.click(primary());
    await waitFor(() => expect(bodies).toHaveLength(1));

    // The refusal's methods replace the read: the code field appears, asks for
    // the code, and takes focus. Hiding the password here made the retry send
    // none, drew `password_required`, and spent a second attempt from the purge
    // budget — the loop PurgeFenceStepUpDialog closed in #2792.
    expect(
      await screen.findByText('Enter the 6-digit code from your authenticator app to continue.')
    ).toBeInTheDocument();
    await waitFor(() => expect(codeField()).toHaveFocus());
    expect(codeField()).toHaveAttribute('aria-invalid', 'true');
    expect(passwordField()).not.toHaveAttribute('aria-invalid');
    expect(passwordField()).toHaveValue(FIXTURE_PW);

    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1]).toEqual({ range: '7d', current_password: FIXTURE_PW, mfa_code: FIXTURE_OTP });
  });

  it('reveals the fields when a credential-less purge is refused', async () => {
    setRequireAuthBeforePurge(false);
    const reads = readReturns(TOTP_READ);
    server.use(
      http.delete(DM_ROUTE, refusal({ error: 'password_required', password_required: true }))
    );

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    expect(await screen.findByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
    // The local setting was stale: the read starts now that the stage is open.
    expect(await screen.findByLabelText('Authenticator app code')).toBeInTheDocument();
    expect(reads.count()).toBe(1);
    expect(passwordField()).toBeInTheDocument();
    // This refusal is the challenge itself: nothing was typed, so no field is wrong.
    expect(screen.queryByText('Enter your password to continue.')).not.toBeInTheDocument();
    expect(passwordField()).not.toHaveAttribute('aria-invalid');
    expect(heading()).toHaveFocus();
  });

  it('sends nothing twice when the primary is pressed again while the request is out', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const bodies: unknown[] = [];
    server.use(
      http.delete(DM_ROUTE, async ({ request }) => {
        bodies.push(await request.json());
        await gate;
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());
    await waitFor(() => expect(bodies).toHaveLength(1));

    // Every dismiss affordance is withdrawn and the fields are inert.
    expect(screen.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
    expect(passwordField()).toBeDisabled();
    await user.click(screen.getByRole('button', { name: /Purging/ }));
    release();

    expect(await screen.findByText('Purged 1 message.')).toBeInTheDocument();
    expect(bodies).toHaveLength(1);
  });

  // Mutant: `disabled={stepUpSubmitting}` re-added to the step-up primary. A
  // natively disabled button loses focus, which falls to <body> mid-request.
  it('keeps the primary aria-disabled, not natively disabled, and focused while the request is out', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const bodies: unknown[] = [];
    server.use(
      http.delete(DM_ROUTE, async ({ request }) => {
        bodies.push(await request.json());
        await gate;
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());
    await waitFor(() => expect(bodies).toHaveLength(1));

    const inFlight = screen.getByRole('button', { name: /Purging/ });
    expect(inFlight).toHaveAttribute('aria-disabled', 'true');
    expect(inFlight).not.toBeDisabled();
    expect(inFlight).toHaveFocus();
    release();
    expect(await screen.findByText('Purged 1 message.')).toBeInTheDocument();
  });

  it('drops an outcome that lands after the dialog closed and reopened', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.delete(DM_ROUTE, async () => {
        await gate;
        return HttpResponse.json({ deleted_count: 7, hidden_count: 0 });
      })
    );
    const ui = (isOpen: boolean) => (
      <PurgeMessagesModal
        context="dm"
        isOpen={isOpen}
        scopeId="d1"
        scopeName="Alex"
        onClose={noop}
      />
    );

    const user = userEvent.setup();
    const view = render(ui(true));
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());
    await screen.findByRole('button', { name: /Purging/ });

    view.rerender(ui(false));
    view.rerender(ui(true));
    release();

    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(screen.queryByText('Purged 7 messages.')).not.toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Range' })).toHaveValue('');
  });

  // The uncertain outcomes: a transport rejection cannot prove nothing was
  // purged, and a 5xx can arrive after batches committed.
  it.each([
    ['a dropped connection', () => HttpResponse.error()],
    ['a 500', () => HttpResponse.json({ error: 'internal' }, { status: 500 })],
  ] as const)(
    'ends on the result stage, never "nothing was purged", after %s',
    async (_n, respond) => {
      captureDmBodies([], respond);
      const events: unknown[] = [];
      const listener = (e: Event) => events.push((e as CustomEvent).detail);
      globalThis.addEventListener('messages-purged', listener);

      const user = userEvent.setup();
      renderDm();
      await reachReady(user);
      await user.type(passwordField(), FIXTURE_PW);
      await user.type(codeField(), FIXTURE_OTP);
      await user.click(primary());

      const alert = await screen.findByRole('alert');
      globalThis.removeEventListener('messages-purged', listener);
      expect(alert).not.toHaveTextContent(/nothing was purged/i);
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(events).toEqual([{ scopeId: 'd1' }]);
    }
  );

  it('ends on the result stage with the rate-limit copy on a 429', async () => {
    captureDmBodies([], () =>
      HttpResponse.json(
        { error: 'Rate limit exceeded' },
        { status: 429, headers: { 'Retry-After': '900' } }
      )
    );
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  // Mutant: a thrown AbortError mapped to `networkError`. apiFetch's pre-dispatch
  // fence throws it when the account changed: the request never left, so the
  // result stage's "some messages may already have been purged" would be false
  // and the unspent code would be thrown away.
  it('treats a pre-dispatch AbortError as not sent: the stage stays and the code is kept', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);
    purgeSpy.mockImplementationOnce(async () => {
      throw new DOMException('aborted', 'AbortError');
    });

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

    await waitFor(() => expect(purgeSpy).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(primary()).not.toHaveAttribute('aria-disabled'));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(codeField()).toHaveValue(FIXTURE_OTP);
    expect(passwordField()).toHaveValue(FIXTURE_PW);
    expect(bodies).toHaveLength(0);

    // Nothing was spent, so the same press goes through.
    await user.click(primary());
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(await screen.findByText('Purged 1 message.')).toBeInTheDocument();
  });

  it('tells the user the session ended when the abort came from an account change', async () => {
    purgeSpy.mockImplementationOnce(async () => {
      useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));
      throw new DOMException('aborted', 'AbortError');
    });

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

    expect(await screen.findByText(SESSION_TEXT)).toBeInTheDocument();
    await waitFor(() => expect(heading()).toHaveFocus());
  });

  // An answer that lands after the account changed belongs to the old account.
  // Mutant: the submit's context recheck removed, so the old account's result
  // stage renders and its cache clear is dispatched for whoever is signed in.
  it("neither shows nor dispatches the old account's purge result", async () => {
    purgeSpy.mockImplementationOnce(async () => {
      useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));
      return { kind: 'success', deletedCount: 1, hiddenCount: 0 };
    });
    const events: unknown[] = [];
    const listener = (e: Event) => events.push((e as CustomEvent).detail);
    globalThis.addEventListener('messages-purged', listener);

    try {
      const user = userEvent.setup();
      renderDm();
      await reachReady(user);
      await user.type(passwordField(), FIXTURE_PW);
      await user.type(codeField(), FIXTURE_OTP);
      await user.click(primary());

      await waitFor(() => expect(purgeSpy).toHaveBeenCalledTimes(1));
      await waitFor(() =>
        expect(screen.queryByRole('button', { name: /Purging\.\.\./ })).not.toBeInTheDocument()
      );
      expect(screen.queryByText('Purged 1 message.')).not.toBeInTheDocument();
      expect(events).toEqual([]);
    } finally {
      globalThis.removeEventListener('messages-purged', listener);
    }
  });
});

describe('PurgeMessagesModal — empty password', () => {
  // The password is required before the primary activates, whatever factor is
  // supplied (D19 reverted: every account has a password hash). Mutant:
  // activating without a password.
  it('blocks a typed code with no password, focuses the password and sends nothing', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(codeField(), FIXTURE_OTP);
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await user.click(primary());

    expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
    expect(passwordField()).toHaveAttribute('aria-invalid', 'true');
    expect(codeField()).not.toHaveAttribute('aria-invalid');
    await waitFor(() => expect(passwordField()).toHaveFocus());
    expect(codeField()).toHaveValue(FIXTURE_OTP);
    expect(bodies).toHaveLength(0);
    expect(purgeSpy).not.toHaveBeenCalled();
  });

  it('blocks a security key with no password before any ceremony starts', async () => {
    readReturns(KEY_READ);
    const ceremony = installCeremony();
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await screen.findByText('Passkey or security key');
    await user.click(primary());

    expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
    await waitFor(() => expect(passwordField()).toHaveFocus());
    expect(ceremony.begins).toHaveLength(0);
    expect(ceremony.get).not.toHaveBeenCalled();
    expect(bodies).toHaveLength(0);
  });

  it('asks for the password before the code when both are missing', async () => {
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.click(primary());
    expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
    await waitFor(() => expect(passwordField()).toHaveFocus());
  });

  it('asks for the code, and focuses it, when only the password is typed', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.click(primary());

    expect(
      await screen.findByText('Enter the 6-digit code from your authenticator app to continue.')
    ).toBeInTheDocument();
    await waitFor(() => expect(codeField()).toHaveFocus());
    expect(bodies).toHaveLength(0);
  });

  it('activates once both are present', async () => {
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    expect(primary()).not.toHaveAttribute('aria-disabled');
  });

  // The server's own refusal still maps onto the field when it arrives.
  it('marks the password field when the server asks for it, and keeps the code', async () => {
    server.use(
      http.delete(DM_ROUTE, refusal({ error: 'password_required', password_required: true }))
    );
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

    expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
    await waitFor(() => expect(passwordField()).toHaveAttribute('aria-invalid', 'true'));
    expect(codeField()).not.toHaveAttribute('aria-invalid');
    await waitFor(() => expect(passwordField()).toHaveFocus());
    expect(codeField()).toHaveValue(FIXTURE_OTP);
  });
});

describe('PurgeMessagesModal — security key', () => {
  it('runs the ceremony for the purge purpose and sends the token with the password', async () => {
    readReturns(KEY_READ);
    const ceremony = installCeremony();
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await screen.findByText('Passkey or security key');
    await user.type(passwordField(), FIXTURE_PW);
    await user.click(primary());

    await waitFor(() => expect(bodies).toHaveLength(1));
    // The token is spendable on this route only; begin must name it.
    expect(ceremony.begins).toEqual([{ purpose: 'dm.purge' }]);
    expect(ceremony.finishes).toHaveLength(1);
    expect(bodies[0]).toEqual({
      range: '7d',
      current_password: FIXTURE_PW,
      mfa_code: FIXTURE_TOKEN,
    });
    expect(await screen.findByText('Purged 1 message.')).toBeInTheDocument();
  });

  it('sends nothing, and returns focus to the primary, when the ceremony is cancelled', async () => {
    readReturns(KEY_READ);
    installCeremony(async () => {
      throw new DOMException('cancelled', 'NotAllowedError');
    });
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await screen.findByText('Passkey or security key');
    await user.type(passwordField(), FIXTURE_PW);
    await user.click(primary());

    expect(
      await screen.findByText(
        'Passkey or security key request was cancelled or timed out. Try again.'
      )
    ).toBeInTheDocument();
    await waitFor(() => expect(primary()).toHaveFocus());
    expect(bodies).toHaveLength(0);
  });

  it('can switch to the authenticator app instead', async () => {
    readReturns(KEY_READ);
    installCeremony();
    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await user.click(await screen.findByRole('button', { name: 'Use authenticator app instead' }));
    expect(await screen.findByLabelText('Authenticator app code')).toHaveFocus();
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
    const consoleSpies = silenceConsole();
    captureDmBodies([], OK);

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());
    await screen.findByText('Purged 1 message.');

    expectNoLeak([FIXTURE_PW, FIXTURE_OTP], consoleSpies);
  });

  it('never writes the security-key token into a store, storage or a log', async () => {
    const consoleSpies = silenceConsole();
    readReturns(KEY_READ);
    installCeremony();
    captureDmBodies([], OK);

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);
    await screen.findByText('Passkey or security key');
    await user.type(passwordField(), FIXTURE_PW);
    await user.click(primary());
    await screen.findByText('Purged 1 message.');

    expectNoLeak([FIXTURE_TOKEN, FIXTURE_PW], consoleSpies);
  });
});

describe('PurgeMessagesModal — per-field errors', () => {
  it('marks only the password field invalid on a wrong password, and keeps the code', async () => {
    server.use(http.delete(DM_ROUTE, refusal({ error: 'Invalid password' })));
    const reads = readReturns(TOTP_READ);

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

    expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
    await waitFor(() => expect(passwordField()).toHaveAttribute('aria-invalid', 'true'));
    // A wrong password says nothing about the code the user typed.
    expect(codeField()).not.toHaveAttribute('aria-invalid');
    expect(passwordField()).toHaveFocus();
    expect(passwordField()).toHaveValue('');
    // Mutant: a password refusal clearing the code. The seam checks the
    // password first, so the code was never read and is still good (#3466).
    expect(codeField()).toHaveValue(FIXTURE_OTP);
    // Not recorded as an accepted code either.
    expect(screen.queryByText(/You just used a code/)).not.toBeInTheDocument();
    // And no re-read: only a wrong code earns one.
    expect(reads.count()).toBe(1);
  });

  it('marks only the code field invalid on a wrong code, clears it, and re-reads once', async () => {
    server.use(http.delete(DM_ROUTE, refusal({ error: 'Invalid MFA code' })));
    const reads = readReturns(TOTP_READ);

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

    expect(
      await screen.findByText(
        "That code didn't work. It may be mistyped or already used. Enter the next code your app shows."
      )
    ).toBeInTheDocument();
    await waitFor(() => expect(codeField()).toHaveAttribute('aria-invalid', 'true'));
    expect(passwordField()).not.toHaveAttribute('aria-invalid');
    expect(codeField()).toHaveFocus();
    // A code is spent by a try: the input remounts empty.
    expect(codeField()).toHaveValue('');
    // The password was right and is kept for the next try.
    expect(passwordField()).toHaveValue(FIXTURE_PW);
    // The factor may have been removed elsewhere: one background re-read.
    await waitFor(() => expect(reads.count()).toBe(2));
  });

  it('keeps the dialog title while the stage heading announces the stage', async () => {
    const user = userEvent.setup();
    renderDm();
    expect(screen.getByRole('heading', { name: 'Purge Messages' })).toBeInTheDocument();
    await reachStepUp(user);
    expect(screen.getByRole('heading', { name: 'Purge Messages' })).toBeInTheDocument();
  });
});

describe('PurgeMessagesModal — step-up dead end', () => {
  it('replaces the stage with a dead-end card when the account cannot step up', async () => {
    server.use(http.delete(DM_ROUTE, refusal({ error: 'no credentials' }, 400)));

    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());

    expect(await screen.findByText(/signs in without a password/i)).toBeInTheDocument();
    // Nothing the user could type would work, so no retryable field survives.
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Confirm and Purge' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Go to Privacy & Security' })).toBeInTheDocument();
    // The button that was just pressed unmounted: focus must not fall out.
    await waitFor(() => expect(heading()).toHaveFocus());
  });

  it('opens straight on the dead end when a credential-less purge is refused with a 400', async () => {
    setRequireAuthBeforePurge(false);
    server.use(http.delete(DM_ROUTE, refusal({ error: 'no credentials' }, 400)));

    const user = userEvent.setup();
    renderDm();
    await reachStepUp(user);

    expect(await screen.findByText(/signs in without a password/i)).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  it('sends the user to the named setting and closes the modal', async () => {
    server.use(http.delete(DM_ROUTE, refusal({ error: 'no credentials' }, 400)));
    let closed = false;

    const user = userEvent.setup();
    renderDm(() => {
      closed = true;
    });
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    await user.click(primary());
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

    await waitFor(() => expect(heading()).toHaveFocus());
    // Renaming a dialog mid-interaction breaks WCAG 4.1.2 / 3.2.2.
    expect(screen.getByRole('heading', { name: 'Purge Messages' })).toBeInTheDocument();
  });

  it('gives each field the autofill hints its credential type needs', async () => {
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);

    expect(passwordField()).toHaveAttribute('autocomplete', 'current-password');
    expect(passwordField()).toHaveAttribute('type', 'password');
    expect(codeField()).toHaveAttribute('autocomplete', 'one-time-code');
    expect(codeField()).toHaveAttribute('inputmode', 'numeric');
  });

  it('drops the entered credentials when the dialog closes, and reads afresh on reopen', async () => {
    const reads = readReturns(TOTP_READ);
    const user = userEvent.setup();
    const { rerender } = renderDm();
    await reachReady(user);
    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);

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
    // credential field at all; re-reaching step-up gets empty ones.
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    await reachReady(user);
    expect(passwordField()).toHaveValue('');
    expect(codeField()).toHaveValue('');
    expect(reads.count()).toBe(2);
  });

  it('keeps the primary aria-disabled until the requirement is complete, and says why on press', async () => {
    const bodies: unknown[] = [];
    captureDmBodies(bodies, OK);
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);

    // aria-disabled, not disabled: the press must stay reachable to explain itself.
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    expect(primary()).not.toBeDisabled();
    await user.click(primary());
    expect(bodies).toHaveLength(0);
    await waitFor(() => expect(passwordField()).toHaveFocus());

    await user.type(passwordField(), FIXTURE_PW);
    await user.type(codeField(), FIXTURE_OTP);
    expect(primary()).not.toHaveAttribute('aria-disabled');
  });

  it('never leaves focus on the document body when the stage opens', async () => {
    const user = userEvent.setup();
    renderDm();
    await reachReady(user);
    expect(document.activeElement).not.toBe(document.body);
    expect(document.activeElement).not.toBe(primary());
  });
});
