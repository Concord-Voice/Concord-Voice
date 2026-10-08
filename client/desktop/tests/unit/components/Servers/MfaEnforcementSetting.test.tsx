import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, userEvent, waitFor, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import { jsonResponse, readOffers, STEP_UP_READ_PATH } from '../../../helpers/stepUpApi';
import { createMockWsService, requireHandler } from '../../../helpers/wsServiceMock';

// The "Require MFA for dangerous actions" switch (#3456 §3.5, T4). The stage, the
// factor hook, the adapter and the openVerificationSetup route are real; only
// `apiFetch` (the setting's GET/PUT and the requirements read, routed by path)
// and the WebSocket service are replaced, as the neighbouring step-up tests do.
//
// "Mutant:" comments name the production change each case exists to turn red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

const wsHolder = vi.hoisted(() => ({ current: null as unknown }));
// `server_permissions_changed` re-reads through the shared jittered timer (§3.8);
// a zero draw keeps these cases on real time.
vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => wsHolder.current,
  fullJitter: () => 0,
}));

import MfaEnforcementSetting from '@/renderer/components/Servers/MfaEnforcementSetting';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

// Named fixtures: the pre-commit detect-secrets hook flags credential-shaped keys.
const FIXTURE_OTP = '314159';

const SERVER = 'srv-1';
const OTHER_SERVER = 'srv-2';
const pathOf = (serverId: string) => `/api/v1/servers/${serverId}/mfa-enforcement`;
const SETTING_PATH = pathOf(SERVER);

const ENROLMENT = 'Set up an authenticator app or security key on your account to turn this on.';
const UNSUPPORTED = 'This server version cannot require MFA for dangerous actions yet.';
const NETWORK = "Couldn't reach the server. Check your connection and try again.";
const LOAD_FAILED = "Couldn't load this setting.";
const LABEL = 'Require MFA for dangerous actions';

type Reply = Response | Promise<Response>;

interface Script {
  get: () => Reply;
  put: (body: Record<string, unknown>) => Reply;
  read: () => Reply;
}

const script: Script = {
  get: () => settingResponse(false),
  put: () => jsonResponse(200, {}),
  read: () => readOffers(['totp']),
};

const settingResponse = (enforcing: boolean) =>
  jsonResponse(200, { enforce_mfa_dangerous_actions: enforcing });

function serve(overrides: Partial<Script> = {}): void {
  Object.assign(script, {
    get: () => settingResponse(false),
    put: () => jsonResponse(200, {}),
    read: () => readOffers(['totp']),
    ...overrides,
  });
}

let ws: ReturnType<typeof createMockWsService>;

const callsTo = (method: string, path = SETTING_PATH) =>
  mockApiFetch.mock.calls.filter(
    (c) => c[0] === path && ((c[1] as RequestInit | undefined)?.method ?? 'GET') === method
  );
const putBodies = (): Record<string, unknown>[] =>
  callsTo('PUT').map((c) => JSON.parse((c[1] as { body: string }).body) as Record<string, unknown>);
const requirementReads = () => mockApiFetch.mock.calls.filter((c) => c[0] === STEP_UP_READ_PATH);

const theSwitch = () => screen.getByRole('switch', { name: LABEL }) as HTMLInputElement;
const findSwitch = () => screen.findByRole('switch', { name: LABEL }) as Promise<HTMLInputElement>;
const status = () => screen.getByRole('status');
const setupLink = () => screen.queryByRole('button', { name: 'Set up verification' });

function renderSetting(props: { serverId?: string; confirmDiscard?: () => boolean } = {}) {
  return render(<MfaEnforcementSetting serverId={props.serverId ?? SERVER} {...props} />);
}

/** Renders and waits for the switch and for the requirements read to have settled. */
async function renderReady(props: Parameters<typeof renderSetting>[0] = {}) {
  const view = renderSetting(props);
  const el = await findSwitch();
  await waitFor(() => expect(requirementReads().length).toBeGreaterThan(0));
  await act(async () => {});
  return { ...view, el };
}

beforeEach(() => {
  resetAllStores();
  mockApiFetch.mockReset().mockImplementation(async (path: string, init?: RequestInit) => {
    if (path === STEP_UP_READ_PATH) return script.read();
    const method = init?.method ?? 'GET';
    if (path === SETTING_PATH && method === 'GET') return script.get();
    if (path === SETTING_PATH && method === 'PUT') {
      return script.put(JSON.parse(init?.body as string) as Record<string, unknown>);
    }
    if (path === pathOf(OTHER_SERVER) && method === 'GET') return settingResponse(false);
    return jsonResponse(404);
  });
  serve();
  ws = createMockWsService();
  wsHolder.current = ws;
  useUserStore.setState({ user: { id: 'acct-1' } as never });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('visibility of the setting', () => {
  // Mutant: the pending read renders a placeholder, a spinner or a heading.
  it('renders nothing and reserves no space while the first read is out', async () => {
    const pending = deferred<Response>();
    serve({ get: () => pending.promise });
    const { container } = renderSetting();

    await waitFor(() => expect(callsTo('GET')).toHaveLength(1));
    expect(container).toBeEmptyDOMElement();

    await act(async () => pending.resolve(settingResponse(false)));
    expect(await findSwitch()).not.toBeChecked();
  });

  // Mutant: 'ok' mapped to a fixed value, or the body's boolean ignored.
  it.each([
    ['off', false],
    ['on', true],
  ])('shows the switch %s when the read says %s', async (_name, enforcing) => {
    serve({ get: () => settingResponse(enforcing) });
    renderSetting();

    const el = await findSwitch();
    expect(el.checked).toBe(enforcing);
    expect(screen.getByRole('heading', { name: 'Security' })).toBeInTheDocument();
  });

  // Mutant: 403/404 fall into `unavailable` (a Retry row for a member who may not see the setting).
  it.each([403, 404])('renders nothing at all when the read answers %i', async (code) => {
    serve({ get: () => jsonResponse(code, {}) });
    const { container } = renderSetting();

    await waitFor(() => expect(callsTo('GET')).toHaveLength(1));
    await act(async () => {});
    expect(container).toBeEmptyDOMElement();
  });

  // Mutant: `switchShown` ignores the read, so a member who may not see the setting spends the requirements read.
  it('does not spend the requirements read for a member who may not see the setting', async () => {
    serve({ get: () => jsonResponse(403, {}) });
    renderSetting();

    await waitFor(() => expect(callsTo('GET')).toHaveLength(1));
    await act(async () => {});
    expect(requirementReads()).toHaveLength(0);
  });

  // Mutant: 5xx, a transport failure or a malformed 200 mapped to `absent` (hides the setting instead of degrading open).
  it.each([
    ['a 500', () => jsonResponse(500, {})],
    ['a rejected request', () => Promise.reject(new TypeError('network'))],
    ['a 200 with no boolean', () => jsonResponse(200, { enforce_mfa_dangerous_actions: 'yes' })],
  ])('shows a Retry row, with no switch, when the read fails with %s', async (_name, reply) => {
    serve({ get: reply });
    renderSetting();

    expect(await screen.findByText(LOAD_FAILED)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
  });

  // Mutant: Retry bound to nothing, or the Retry row left up after a good read.
  it('Retry reads again and replaces the row with the switch', async () => {
    let reads = 0;
    serve({ get: () => (++reads === 1 ? jsonResponse(500, {}) : settingResponse(true)) });
    renderSetting();

    await userEvent.click(await screen.findByRole('button', { name: 'Retry' }));

    expect((await findSwitch()).checked).toBe(true);
    expect(screen.queryByText(LOAD_FAILED)).not.toBeInTheDocument();
    expect(callsTo('GET')).toHaveLength(2);
  });

  // Mutant: a failed REFRESH turns a shown switch into the Retry row (`nextRead` unavailable arm returns 'failed').
  it('keeps the shown value when a later refresh fails', async () => {
    serve({ get: () => settingResponse(true) });
    await renderReady();

    serve({ get: () => jsonResponse(500, {}) });
    await act(async () =>
      requireHandler(ws, 'server_permissions_changed')({ data: { server_id: SERVER } })
    );
    await waitFor(() => expect(callsTo('GET')).toHaveLength(2));
    await act(async () => {});

    expect(theSwitch().checked).toBe(true);
    expect(screen.queryByText(LOAD_FAILED)).not.toBeInTheDocument();
  });

  // Mutant: a new `serverId` keeps the previous server's value (the `key={serverId}` remount dropped).
  it('reads the new server, and shows nothing for it meanwhile, when serverId changes', async () => {
    serve({ get: () => settingResponse(true) });
    const { rerender } = await renderReady();

    const pending = deferred<Response>();
    mockApiFetch.mockImplementation(async (path: string) =>
      path === pathOf(OTHER_SERVER) ? pending.promise : jsonResponse(404)
    );
    rerender(<MfaEnforcementSetting serverId={OTHER_SERVER} />);

    await waitFor(() => expect(callsTo('GET', pathOf(OTHER_SERVER))).toHaveLength(1));
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
    await act(async () => pending.resolve(settingResponse(false)));
    expect((await findSwitch()).checked).toBe(false);
  });
});

describe('enrolment gates turning ON', () => {
  // Mutant: `ready` with no methods is not read as unenrolled, or `aria-disabled` is not set.
  it('an unenrolled account has ON aria-disabled, with the sentence and the link', async () => {
    serve({ read: () => readOffers([]) });
    await renderReady();

    await waitFor(() => expect(theSwitch()).toHaveAttribute('aria-disabled', 'true'));
    expect(theSwitch()).not.toBeDisabled();
    expect(screen.getByText(ENROLMENT)).toBeInTheDocument();
    expect(theSwitch()).toHaveAccessibleDescription(expect.stringContaining(ENROLMENT));
    expect(setupLink()).toBeInTheDocument();
  });

  // Mutant: the activation guard in the switch's onChange dropped (aria-disabled alone is only a hint).
  it('activating the blocked switch sends nothing', async () => {
    serve({ read: () => readOffers([]) });
    await renderReady();
    await waitFor(() => expect(theSwitch()).toHaveAttribute('aria-disabled', 'true'));

    await userEvent.click(theSwitch());

    expect(callsTo('PUT')).toHaveLength(0);
    expect(theSwitch()).not.toBeChecked();
  });

  // Mutant: methods are not intersected, so an email-only account reads as enrolled.
  it('an account whose only methods are not inline is unenrolled too', async () => {
    serve({ read: () => readOffers(['email', 'sms']) });
    await renderReady();

    await waitFor(() => expect(theSwitch()).toHaveAttribute('aria-disabled', 'true'));
  });

  // Mutant: `enrolled` mapped to a block, or aria-disabled left on.
  it('an enrolled account leaves ON live', async () => {
    await renderReady();

    expect(theSwitch()).not.toHaveAttribute('aria-disabled');
    expect(screen.queryByText(ENROLMENT)).not.toBeInTheDocument();
    expect(setupLink()).not.toBeInTheDocument();
  });

  // Mutant: any non-ready answer is treated as unenrolled (the client predicting a refusal it was not told of).
  it.each([
    ['unavailable (500)', () => jsonResponse(500, {})],
    ['unavailable (a rejected request)', () => Promise.reject(new TypeError('network'))],
    ['unsupported (404)', () => jsonResponse(404, {})],
    ['refused (400)', () => jsonResponse(400, {})],
  ])('an %s requirements read leaves ON live', async (_name, read) => {
    serve({ read });
    await renderReady();

    expect(theSwitch()).not.toHaveAttribute('aria-disabled');
    expect(screen.queryByText(ENROLMENT)).not.toBeInTheDocument();
    await userEvent.click(theSwitch());
    await waitFor(() => expect(putBodies()).toHaveLength(1));
  });

  // Mutant: `onBlockOf` drops the `read.enforcing` clause, so an ON switch an unenrolled owner could still turn OFF is locked.
  it('does not block a switch that is already ON, whatever the enrolment', async () => {
    serve({ get: () => settingResponse(true), read: () => readOffers([]) });
    await renderReady();

    expect(theSwitch()).not.toHaveAttribute('aria-disabled');
    expect(screen.queryByText(ENROLMENT)).not.toBeInTheDocument();
  });

  // Mutant: the link calls openVerificationSetup with another `returnTo` (or without the discard guard).
  it('"Set up verification" opens App Settings and records the way back to this server', async () => {
    serve({ read: () => readOffers([]) });
    await renderReady();
    await waitFor(() => expect(setupLink()).toBeInTheDocument());

    await userEvent.click(setupLink() as HTMLElement);

    await waitFor(() => expect(useSettingsOverlayStore.getState().open).toBe('app'));
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
      kind: 'serverSettings',
      serverId: SERVER,
      section: 'general',
    });
  });

  // Mutant: `confirmDiscard` is not forwarded, so unsaved edits are lost without asking.
  it('a declined discard keeps the page where it is', async () => {
    serve({ read: () => readOffers([]) });
    const confirmDiscard = vi.fn(() => false);
    await renderReady({ confirmDiscard });
    await waitFor(() => expect(setupLink()).toBeInTheDocument());

    await userEvent.click(setupLink() as HTMLElement);

    await waitFor(() => expect(confirmDiscard).toHaveBeenCalledTimes(1));
    expect(useSettingsOverlayStore.getState().open).toBeNull();
    expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();
  });
});

describe('the server capability (X16)', () => {
  const setCapability = (
    capability: Parameters<
      ReturnType<typeof useClientConfigStore.getState>['setMfaEnforcementCapability']
    >[0]
  ) => useClientConfigStore.getState().setMfaEnforcementCapability(capability);

  // Mutant: `confirmed-unsupported` no longer reaches `onBlockOf`, or its sentence is another one.
  it('confirmed-unsupported disables ON with the version copy and no link', async () => {
    setCapability({ status: 'confirmed-unsupported' });
    await renderReady();

    expect(theSwitch()).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByText(UNSUPPORTED)).toBeInTheDocument();
    expect(theSwitch()).toHaveAccessibleDescription(expect.stringContaining(UNSUPPORTED));
    expect(setupLink()).not.toBeInTheDocument();
    await userEvent.click(theSwitch());
    expect(callsTo('PUT')).toHaveLength(0);
  });

  // Mutant: the unsupported check moved after the enrolment one (an unenrolled owner on an old server is told to enrol).
  it('says the server is too old, not "enrol", when both apply', async () => {
    setCapability({ status: 'confirmed-unsupported' });
    serve({ read: () => readOffers([]) });
    await renderReady();

    expect(screen.getByText(UNSUPPORTED)).toBeInTheDocument();
    expect(screen.queryByText(ENROLMENT)).not.toBeInTheDocument();
  });

  // Mutant: OFF is blocked on an old server too (the `read.enforcing` clause lost).
  it('confirmed-unsupported leaves OFF available for a setting that is already ON', async () => {
    setCapability({ status: 'confirmed-unsupported' });
    serve({ get: () => settingResponse(true) });
    await renderReady();

    expect(theSwitch()).not.toHaveAttribute('aria-disabled');
    await userEvent.click(theSwitch());
    expect(await screen.findByRole('dialog', { name: "Confirm it's you" })).toBeInTheDocument();
  });

  // Mutant: loading/error/supported counted as "too old" (a transient capabilities failure claims the server cannot).
  it.each([
    ['loading', { status: 'loading' }],
    ['error', { status: 'error' }],
    ['supported', { status: 'supported' }],
  ] as const)('%s leaves ON live and says nothing about the version', async (_name, capability) => {
    setCapability(capability);
    await renderReady();

    expect(theSwitch()).not.toHaveAttribute('aria-disabled');
    expect(screen.queryByText(UNSUPPORTED)).not.toBeInTheDocument();
    await userEvent.click(theSwitch());
    await waitFor(() => expect(putBodies()).toHaveLength(1));
  });

  // Mutant: the capability is read once and not subscribed to.
  it('follows the capability when the capabilities fetch settles after the first render', async () => {
    await renderReady();
    expect(theSwitch()).not.toHaveAttribute('aria-disabled');

    act(() => setCapability({ status: 'confirmed-unsupported' }));

    expect(theSwitch()).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByText(UNSUPPORTED)).toBeInTheDocument();
  });
});

describe('turning ON', () => {
  // Mutant: a code, or an `mfa_code` key, goes with ON.
  it('sends the PUT with no code and never opens the dialog', async () => {
    await renderReady();

    await userEvent.click(theSwitch());

    await waitFor(() => expect(putBodies()).toHaveLength(1));
    expect(putBodies()[0]).toEqual({ enabled: true });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  // Mutant: the value is set before the answer (optimistic), or busy/aria-disabled/"Turning on…" dropped.
  it('is not optimistic, shows busy meanwhile, then settles with the polite status', async () => {
    const reply = deferred<Response>();
    serve({ put: () => reply.promise });
    const { container } = await renderReady();

    await userEvent.click(theSwitch());

    await waitFor(() => expect(putBodies()).toHaveLength(1));
    expect(theSwitch().checked).toBe(false);
    expect(theSwitch()).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByText('Turning on…')).toBeInTheDocument();
    expect(container.querySelector('[aria-busy="true"]')).not.toBeNull();
    expect(status()).toHaveTextContent('');

    await act(async () => reply.resolve(jsonResponse(200, {})));

    await waitFor(() => expect(theSwitch().checked).toBe(true));
    expect(screen.queryByText('Turning on…')).not.toBeInTheDocument();
    expect(container.querySelector('[aria-busy]')).toBeNull();
    expect(theSwitch()).not.toHaveAttribute('aria-disabled');
    expect(status()).toHaveTextContent('MFA enforcement is on.');
  });

  // Mutant: the in-flight guard dropped (a second activation sends a second PUT).
  it('a second activation while the first is out sends nothing', async () => {
    const reply = deferred<Response>();
    serve({ put: () => reply.promise });
    await renderReady();

    await userEvent.click(theSwitch());
    await waitFor(() => expect(putBodies()).toHaveLength(1));
    await userEvent.click(theSwitch());
    await userEvent.click(theSwitch());

    expect(putBodies()).toHaveLength(1);
    await act(async () => reply.resolve(jsonResponse(200, {})));
  });

  // Mutant: 'enrollment required' falls through to the generic error (no sentence, no link).
  it('a refusal for enrolment shows the sentence and the link and leaves the switch off', async () => {
    serve({
      put: () =>
        jsonResponse(403, { error: 'MFA verification required', mfa_enrollment_required: true }),
    });
    await renderReady();

    await userEvent.click(theSwitch());

    // The sentence is the inline note and the polite announcement: two places, on purpose.
    expect(await screen.findAllByText(ENROLMENT)).toHaveLength(2);
    expect(setupLink()).toBeInTheDocument();
    expect(theSwitch().checked).toBe(false);
    expect(theSwitch()).toHaveAttribute('aria-disabled', 'true');
    expect(status()).toHaveTextContent(ENROLMENT);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  // Mutant: `describeFailure` swaps its arms or the banner is dropped, or the value moves on a refusal.
  it.each([
    [403, "You don't have permission to change this setting."],
    [429, 'Too many attempts. Try again in a few minutes.'],
    [500, "Couldn't change this setting. Try again."],
  ])('a %i refusal keeps the switch off with an inline error', async (code, text) => {
    serve({ put: () => jsonResponse(code, { error: 'no' }) });
    await renderReady();

    await userEvent.click(theSwitch());

    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(theSwitch().checked).toBe(false);
    expect(status()).toHaveTextContent('');
  });

  // Mutant: a lost answer says nothing, or does not re-read (the change may have applied).
  it('a lost answer says so and re-reads, because the change may have applied', async () => {
    let reads = 0;
    serve({
      get: () => settingResponse(++reads > 1),
      put: () => Promise.reject(new TypeError('network')),
    });
    await renderReady();

    await userEvent.click(theSwitch());

    expect(await screen.findByRole('alert')).toHaveTextContent(NETWORK);
    await waitFor(() => expect(theSwitch().checked).toBe(true));
    expect(callsTo('GET')).toHaveLength(2);
  });

  // Mutant: the success path does not supersede a read that began before the write.
  it('a read that began before the confirmed write cannot land on top of it', async () => {
    await renderReady();
    const stale = deferred<Response>();
    serve({ get: () => stale.promise });
    await act(async () =>
      requireHandler(ws, 'server_permissions_changed')({ data: { server_id: SERVER } })
    );
    await waitFor(() => expect(callsTo('GET')).toHaveLength(2));

    await userEvent.click(theSwitch());
    await waitFor(() => expect(theSwitch().checked).toBe(true));
    await act(async () => stale.resolve(settingResponse(false)));

    expect(theSwitch().checked).toBe(true);
  });
});

describe('turning OFF', () => {
  const offDialog = () => screen.findByRole('dialog', { name: "Confirm it's you" });

  // Mutant: OFF sends before it asks, or is optimistic.
  it('opens the confirmation and sends nothing until it is confirmed', async () => {
    serve({ get: () => settingResponse(true) });
    await renderReady();

    await userEvent.click(theSwitch());

    const dialog = await offDialog();
    expect(dialog).toHaveAccessibleDescription(
      "You're about to stop requiring MFA for dangerous actions on this server."
    );
    expect(within(dialog).getByRole('button', { name: 'Turn Off' })).toBeInTheDocument();
    expect(callsTo('PUT')).toHaveLength(0);
    expect(theSwitch().checked).toBe(true);
  });

  // Mutant: the code is not carried, or `enabled` is not false, or settleOff/the announcement lost.
  it('a TOTP code sends the disable PUT with mfa_code, then settles OFF', async () => {
    serve({ get: () => settingResponse(true) });
    await renderReady();
    await userEvent.click(theSwitch());

    await userEvent.type(await screen.findByLabelText('Authenticator app code'), FIXTURE_OTP);
    await userEvent.click(screen.getByRole('button', { name: 'Turn Off' }));

    await waitFor(() => expect(theSwitch().checked).toBe(false));
    expect(putBodies()).toEqual([{ enabled: false, mfa_code: FIXTURE_OTP }]);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(status()).toHaveTextContent('MFA enforcement is off.');
  });

  // Mutant: Cancel (or onClose) settles OFF, or sends a request.
  it('Cancel leaves the switch on and sends nothing', async () => {
    serve({ get: () => settingResponse(true) });
    await renderReady();
    await userEvent.click(theSwitch());
    await offDialog();

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(theSwitch().checked).toBe(true);
    expect(callsTo('PUT')).toHaveLength(0);
    expect(status()).toHaveTextContent('');
  });

  // Mutant: a refused OFF still settles OFF (`onSuccess` runs on a refusal).
  it('a refused disable keeps the switch on and the dialog open with the reason', async () => {
    serve({ get: () => settingResponse(true), put: () => jsonResponse(500, { error: 'boom' }) });
    await renderReady();
    await userEvent.click(theSwitch());
    await userEvent.type(await screen.findByLabelText('Authenticator app code'), FIXTURE_OTP);

    await userEvent.click(screen.getByRole('button', { name: 'Turn Off' }));

    expect(await screen.findByText("Couldn't change this setting. Try again.")).toBeInTheDocument();
    expect(theSwitch().checked).toBe(true);
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  // Mutant: the dialog's `onSetUpVerification` does not close it (or does not route), or `focusFallback` returns nothing.
  it('an unenrolled owner turning OFF ends in the enrolment state, whose link closes the dialog and routes to setup', async () => {
    serve({
      get: () => settingResponse(true),
      read: () => readOffers([]),
      put: () =>
        jsonResponse(403, { error: 'MFA verification required', mfa_enrollment_required: true }),
    });
    await renderReady();
    await userEvent.click(theSwitch());
    await offDialog();

    await userEvent.click(screen.getByRole('button', { name: 'Turn Off' }));
    await waitFor(() => expect(putBodies()).toEqual([{ enabled: false }]));
    const inDialog = within(screen.getByRole('dialog'));
    await userEvent.click(await inDialog.findByRole('button', { name: 'Set up verification' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
      kind: 'serverSettings',
      serverId: SERVER,
      section: 'general',
    });
    expect(theSwitch().checked).toBe(true);
  });
});

describe('refreshing while mounted', () => {
  // Mutant: the handler ignores the server id (re-reads for another server's event) or never re-reads.
  it("re-reads this server's setting on its own event, and ignores another server's", async () => {
    await renderReady();
    expect(callsTo('GET')).toHaveLength(1);

    await act(async () =>
      requireHandler(ws, 'server_permissions_changed')({ data: { server_id: OTHER_SERVER } })
    );
    expect(callsTo('GET')).toHaveLength(1);

    serve({ get: () => settingResponse(true) });
    await act(async () =>
      requireHandler(ws, 'server_permissions_changed')({ data: { server_id: SERVER } })
    );

    await waitFor(() => expect(theSwitch().checked).toBe(true));
    expect(callsTo('GET')).toHaveLength(2);
  });

  // Mutant: permissions_changed is not subscribed (an account that just enrolled stays blocked until a reload).
  it('re-reads the requirements on permissions_changed, and an enrolment lifts the block', async () => {
    serve({ read: () => readOffers([]) });
    await renderReady();
    await waitFor(() => expect(theSwitch()).toHaveAttribute('aria-disabled', 'true'));
    const before = requirementReads().length;

    serve({ read: () => readOffers(['totp']) });
    await act(async () => requireHandler(ws, 'permissions_changed')({ data: {} }));

    await waitFor(() => expect(requirementReads().length).toBe(before + 1));
    await waitFor(() => expect(theSwitch()).not.toHaveAttribute('aria-disabled'));
    expect(screen.queryByText(ENROLMENT)).not.toBeInTheDocument();
  });

  // Mutant: the effect returns no cleanup for one or both subscriptions.
  it('unsubscribes both events on unmount', async () => {
    const { unmount } = await renderReady();
    expect(ws.handlers.has('server_permissions_changed')).toBe(true);
    expect(ws.handlers.has('permissions_changed')).toBe(true);

    unmount();

    expect(ws.handlers.has('server_permissions_changed')).toBe(false);
    expect(ws.handlers.has('permissions_changed')).toBe(false);
  });
});
