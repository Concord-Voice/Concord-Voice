import { act, renderHook, waitFor, type RenderHookResult } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// The step-up factor picker's state machine (design 2026-09-26-mfa-factor-picker
// §4.1; plan 2026-10-07 §3). `apiFetch` is mocked at the module boundary so
// every request the hook sends is observable and every answer is scripted; the
// request context, the auth store and the runtime server selection are real, so
// the account and server fences are exercised for what they are.
//
// "Mutant:" comments name the production change each test exists to turn red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

import {
  useStepUpFactor,
  type StepUpFactor,
  type StepUpFactorProps,
  type StepUpSubmit,
  type StepUpSubmitOutcome,
} from '@/renderer/hooks/auth/useStepUpFactor';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useTotpAcceptedStore } from '@/renderer/stores/auth/totpAcceptedStore';
import {
  captureApiRequestContext,
  type ApiRequestContext,
} from '@/renderer/services/system/requestContext';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import type { StepUpRefusal } from '@/renderer/services/system/stepUpRefusal';

const READ = '/api/v1/mfa/step-up';
const BEGIN = '/api/v1/mfa/webauthn/verify-inline/begin';
const FINISH = '/api/v1/mfa/webauthn/verify-inline/finish';
const ACCOUNT = 'acct-1';
const WEBAUTHN_TOKEN = 'webauthn-inline-token';

type Route = (init: RequestInit) => Response | Promise<Response>;
let routes: Record<string, Route>;
let mockGet: ReturnType<typeof vi.fn>;

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function readBody(
  methods: string[],
  defaultMethod: string | null,
  backupCodeAvailable = false
): Response {
  return json({
    methods,
    default_method: defaultMethod,
    backup_code_available: backupCodeAvailable,
  });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

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

function hits(path: string) {
  return mockApiFetch.mock.calls.filter(([p]) => p === path);
}

function switchAccount() {
  useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));
}

function switchServer() {
  setRuntimeServerBase('https://other-server.example.test');
}

const BASE: StepUpFactorProps = {
  enabled: true,
  purpose: 'dm.purge',
  passwordLeg: 'always', // pragma: allowlist secret
  readFailure: 'block',
  allowBackup: true,
};

type View = RenderHookResult<StepUpFactor, StepUpFactorProps>;

function mount(overrides: Partial<StepUpFactorProps> = {}): View {
  return renderHook((props: StepUpFactorProps) => useStepUpFactor(props), {
    initialProps: { ...BASE, ...overrides },
  });
}

/** The reads here ride real microtask chains; a loaded CI worker needs more than waitFor's 1 s. */
const SETTLE = { timeout: 5000 };

async function mountReady(overrides: Partial<StepUpFactorProps> = {}): Promise<View> {
  const view = mount(overrides);
  await waitFor(() => expect(view.result.current.status.kind).toBe('ready'), SETTLE);
  return view;
}

/** A ready TOTP-only instance with a six-digit code typed. */
async function mountTotp(
  code = '123456',
  overrides: Partial<StepUpFactorProps> = {}
): Promise<View> {
  routes[READ] = () => readBody(['totp'], 'totp', true);
  const view = await mountReady(overrides);
  typeCode(view, code);
  return view;
}

function typeCode(view: View, code: string) {
  act(() => view.result.current.setCode(code));
}

async function run(view: View, submit: StepUpSubmit, capture?: ApiRequestContext) {
  let outcome: StepUpSubmitOutcome | null | undefined;
  await act(async () => {
    outcome = await view.result.current.run(submit, capture);
  });
  return outcome;
}

function submitting(outcome: StepUpSubmitOutcome) {
  return vi.fn<StepUpSubmit>(async () => outcome);
}

function refusal(r: StepUpRefusal): StepUpSubmitOutcome {
  return { kind: 'refusal', refusal: r };
}

beforeEach(() => {
  resetAllStores();
  useUserStore.setState({ user: { id: ACCOUNT } as never });
  routes = {
    [READ]: () => readBody(['webauthn', 'totp'], 'webauthn'),
    [BEGIN]: () =>
      json({ publicKey: { challenge: 'AQID', rpId: 'localhost', allowCredentials: [] } }),
    [FINISH]: () => json({ mfa_token: WEBAUTHN_TOKEN }),
  };
  mockApiFetch.mockReset();
  mockApiFetch.mockImplementation(async (path: string, init: RequestInit) => {
    const route = routes[path];
    if (!route) throw new Error(`unexpected request to ${path}`);
    return route(init);
  });
  mockGet = vi.fn().mockResolvedValue(CREDENTIAL);
  Object.defineProperty(navigator, 'credentials', {
    value: { get: mockGet },
    writable: true,
    configurable: true,
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  resetRuntimeServerBase();
});

// ── The read and the offered set ─────────────────────────────────────────

describe('the requirements read', () => {
  it('is reading until the answer lands, then offers the methods strongest first', async () => {
    const answer = deferred<Response>();
    routes[READ] = () => answer.promise;

    const view = mount({ passwordLeg: 'whenNoMfa' }); // pragma: allowlist secret
    expect(view.result.current.status).toEqual({ kind: 'reading' });
    expect(view.result.current.methods).toEqual([]);

    await act(async () => answer.resolve(readBody(['totp', 'webauthn'], null, true)));

    await waitFor(() => expect(view.result.current.status).toEqual({ kind: 'ready' }));
    expect(view.result.current.methods).toEqual(['webauthn', 'totp', 'backup']);
    expect(view.result.current.method).toBe('webauthn');
    expect(hits(READ)).toHaveLength(1);
  });

  it("opens on the server's default when it is offered", async () => {
    routes[READ] = () => readBody(['webauthn', 'totp'], 'totp');

    const view = await mountReady();

    expect(view.result.current.method).toBe('totp');
  });

  it('offers no security key without a purpose, and never a backup code without TOTP', async () => {
    routes[READ] = () => readBody(['webauthn'], 'webauthn', true);

    const view = await mountReady({ purpose: null });

    expect(view.result.current.methods).toEqual([]);
    expect(view.result.current.method).toBeNull();
  });

  it('offers a backup code only when the read reports one and the route allows it', async () => {
    routes[READ] = () => readBody(['totp'], 'totp', true);
    const allowed = await mountReady({ allowBackup: true });
    expect(allowed.result.current.methods).toEqual(['totp', 'backup']);

    const refused = await mountReady({ allowBackup: false });
    expect(refused.result.current.methods).toEqual(['totp']);

    routes[READ] = () => readBody(['totp'], 'totp', false);
    const none = await mountReady({ allowBackup: true });
    expect(none.result.current.methods).toEqual(['totp']);
  });

  it('shows the password leg while reading and when ready on an always route', async () => {
    const answer = deferred<Response>();
    routes[READ] = () => answer.promise;

    const view = mount({ passwordLeg: 'always' }); // pragma: allowlist secret
    expect(view.result.current.passwordLegShown).toBe(true);

    await act(async () => answer.resolve(readBody(['totp'], 'totp')));
    await waitFor(() => expect(view.result.current.status.kind).toBe('ready'));
    expect(view.result.current.passwordLegShown).toBe(true);
  });

  it('shows the password leg on a whenNoMfa route only once ready with nothing to offer', async () => {
    routes[READ] = () => readBody([], null);
    const empty = await mountReady({ passwordLeg: 'whenNoMfa' }); // pragma: allowlist secret
    expect(empty.result.current.passwordLegShown).toBe(true);

    routes[READ] = () => readBody(['totp'], 'totp');
    const offered = await mountReady({ passwordLeg: 'whenNoMfa' }); // pragma: allowlist secret
    expect(offered.result.current.passwordLegShown).toBe(false);
  });

  it('does not read while disabled', async () => {
    const view = mount({ enabled: false });

    await act(async () => {});

    expect(hits(READ)).toHaveLength(0);
    expect(view.result.current.passwordLegShown).toBe(false);
  });

  // Mutant: swapping `block` and `passwordOnly` in the unavailable/aborted arm.
  it.each([
    ['429', 429],
    ['503', 503],
  ])('blocks on a %s read on a block route and offers Retry', async (_name, status) => {
    routes[READ] = () => json({}, status);

    const view = mount({ readFailure: 'block' });

    await waitFor(() => expect(view.result.current.status).toEqual({ kind: 'blocked' }), SETTLE);
    expect(view.result.current.passwordLegShown).toBe(false);

    routes[READ] = () => readBody(['totp'], 'totp');
    act(() => view.result.current.retryRead());
    expect(view.result.current.status).toEqual({ kind: 'reading' });
    await waitFor(() => expect(view.result.current.status).toEqual({ kind: 'ready' }), SETTLE);
    expect(hits(READ)).toHaveLength(2);
  });

  it('lands on the password alone for an unavailable read on a passwordOnly route', async () => {
    routes[READ] = () => json({}, 503);

    const view = await mountReady({ readFailure: 'passwordOnly', passwordLeg: 'whenNoMfa' }); // pragma: allowlist secret

    expect(view.result.current.methods).toEqual([]);
    expect(view.result.current.passwordLegShown).toBe(true);
  });

  // Mutant: `unsupported` blocking on a block route.
  it('lands on the password alone for an unsupported server even on a block route', async () => {
    routes[READ] = () => json({}, 404);

    const view = await mountReady({ readFailure: 'block', passwordLeg: 'whenNoMfa' }); // pragma: allowlist secret

    expect(view.result.current.methods).toEqual([]);
    expect(view.result.current.passwordLegShown).toBe(true);
  });

  // Mutant: `refused` offering Retry (or the password leg).
  it('treats a refused read as terminal: no Retry, no password leg', async () => {
    routes[READ] = () => json({ error_code: 'account_disabled' }, 403);

    const view = mount();

    await waitFor(() =>
      expect(view.result.current.status).toEqual({ kind: 'refused', reason: 'account' })
    );
    expect(view.result.current.passwordLegShown).toBe(false);
    act(() => view.result.current.retryRead());
    await act(async () => {});
    expect(hits(READ)).toHaveLength(1);
    expect(view.result.current.status.kind).toBe('refused');
  });

  it('retryRead does nothing unless the status is blocked', async () => {
    const view = await mountReady();

    act(() => view.result.current.retryRead());
    await act(async () => {});

    expect(hits(READ)).toHaveLength(1);
  });

  it('treats a read fenced by apiFetch on an unchanged account as unavailable', async () => {
    routes[READ] = () => {
      throw new DOMException('Request lifecycle changed before dispatch', 'AbortError');
    };

    const blocked = mount({ readFailure: 'block' });
    await waitFor(() => expect(blocked.result.current.status).toEqual({ kind: 'blocked' }), SETTLE);

    const passwordOnly = mount({ readFailure: 'passwordOnly' });
    await waitFor(
      () => expect(passwordOnly.result.current.status).toEqual({ kind: 'ready' }),
      SETTLE
    );
  });

  it('ends in sessionExpired when the read is fenced by an account change', async () => {
    routes[READ] = () => {
      switchAccount();
      throw new DOMException('Request lifecycle changed before dispatch', 'AbortError');
    };

    const view = mount();

    await waitFor(() => expect(view.result.current.status).toEqual({ kind: 'sessionExpired' }));
  });

  it('ends in sessionExpired when the read is fenced by a server change', async () => {
    routes[READ] = () => {
      switchServer();
      throw new DOMException('Request lifecycle changed before dispatch', 'AbortError');
    };

    const view = mount();

    await waitFor(() => expect(view.result.current.status).toEqual({ kind: 'sessionExpired' }));
  });
});

// ── C57: a read applies only inside the open instance that started it ────

describe('a read result never outlives its instance (C57)', () => {
  // Mutant: applying a read's result without its sequence number / signal check.
  // The first read is aborted by the close, so an unguarded apply would turn it
  // into `blocked` on this block route and overwrite the reopened instance.
  it('drops the first read after close and reopen', async () => {
    const first = deferred<Response>();
    const second = deferred<Response>();
    let reads = 0;
    routes[READ] = () => (++reads === 1 ? first.promise : second.promise);

    const view = mount();
    view.rerender({ ...BASE, enabled: false });
    view.rerender({ ...BASE, enabled: true });
    await waitFor(() => expect(hits(READ)).toHaveLength(2));

    await act(async () => second.resolve(readBody(['totp'], 'totp')));
    await waitFor(() => expect(view.result.current.status).toEqual({ kind: 'ready' }));
    await act(async () => first.resolve(readBody(['webauthn'], 'webauthn')));
    await act(async () => {});

    expect(view.result.current.status).toEqual({ kind: 'ready' });
    expect(view.result.current.methods).toEqual(['totp']);
  });

  it('drops a read that lands while the instance is closed', async () => {
    const first = deferred<Response>();
    routes[READ] = () => first.promise;
    const view = mount();

    view.rerender({ ...BASE, enabled: false });
    await act(async () => first.resolve(readBody(['totp'], 'totp')));
    await act(async () => {});
    view.rerender({ ...BASE, enabled: true });

    expect(view.result.current.status).toEqual({ kind: 'reading' });
  });

  it('starts a fresh instance, and read, when the configuration changes', async () => {
    const view = await mountReady({ purpose: 'dm.purge' });
    routes[READ] = () => readBody(['webauthn'], 'webauthn');

    view.rerender({ ...BASE, purpose: 'dm.clear' });

    expect(view.result.current.status).toEqual({ kind: 'reading' });
    await waitFor(() => expect(view.result.current.status.kind).toBe('ready'));
    expect(hits(READ)).toHaveLength(2);
  });

  it('aborts the read signal on unmount', async () => {
    routes[READ] = () => deferred<Response>().promise;
    const view = mount();

    view.unmount();

    const init = mockApiFetch.mock.calls[0][1] as RequestInit;
    expect(init.signal?.aborted).toBe(true);
  });
});

// ── The activation guard ─────────────────────────────────────────────────

describe('firstMissing and announceMissing', () => {
  it('reports what the primary still needs, in order', async () => {
    const answer = deferred<Response>();
    routes[READ] = () => answer.promise;
    const view = mount();

    expect(view.result.current.firstMissing('')).toBe('password');
    expect(view.result.current.firstMissing('hunter2')).toBe('reading');

    await act(async () => answer.resolve(readBody(['totp'], 'totp')));
    await waitFor(() => expect(view.result.current.status.kind).toBe('ready'));
    expect(view.result.current.firstMissing('hunter2')).toBe('code');
    typeCode(view, '123456');
    expect(view.result.current.firstMissing('hunter2')).toBeNull();
    expect(view.result.current.firstMissing('')).toBe('password');
  });

  // An empty password is missing on an always-shown password leg whatever else is
  // supplied: every account holds a password hash (SSO accounts included), so
  // an MFA factor never stands in for it. Mutant: a "factor supplied" shortcut
  // that lets a typed code or a security-key method excuse an empty password
  // returns null (or 'code') in every case below.
  describe('an empty password is missing whatever factor is supplied', () => {
    it('beside a typed authenticator code', async () => {
      const view = await mountTotp('123456');

      expect(view.result.current.firstMissing('')).toBe('password');
      expect(view.result.current.firstMissing('hunter2')).toBeNull();
    });

    it('beside a typed backup code', async () => {
      routes[READ] = () => readBody(['totp'], 'totp', true);
      const view = await mountReady();
      act(() => view.result.current.switchTo('backup'));
      typeCode(view, 'abcd-1234');

      expect(view.result.current.firstMissing('')).toBe('password');
    });

    it('on the security-key panel, which needs no typed code', async () => {
      const view = await mountReady();

      expect(view.result.current.method).toBe('webauthn');
      expect(view.result.current.firstMissing('')).toBe('password');
      expect(view.result.current.firstMissing('hunter2')).toBeNull();
    });

    it('beside an empty code', async () => {
      const view = await mountTotp('');

      expect(view.result.current.firstMissing('')).toBe('password');
    });

    it('while the read is in flight, whatever has been typed', async () => {
      routes[READ] = () => new Promise<Response>(() => undefined);
      const view = mount();
      typeCode(view, '123456');

      expect(view.result.current.status.kind).toBe('reading');
      expect(view.result.current.firstMissing('')).toBe('password');
    });

    // `whenNoMfa` shows the password only for an empty offered set.
    it('on whenNoMfa with an empty offered set', async () => {
      routes[READ] = () => readBody([], null);
      const view = await mountReady({ passwordLeg: 'whenNoMfa' }); // pragma: allowlist secret

      expect(view.result.current.methods).toEqual([]);
      expect(view.result.current.passwordLegShown).toBe(true);
      expect(view.result.current.firstMissing('')).toBe('password');
      expect(view.result.current.firstMissing('hunter2')).toBeNull();
    });

    // The password check is skipped when the leg is not shown, so the code
    // check decides.
    it('is not asked for on whenNoMfa while a method is offered', async () => {
      routes[READ] = () => readBody(['totp'], 'totp');
      const view = await mountReady({ passwordLeg: 'whenNoMfa' }); // pragma: allowlist secret
      typeCode(view, '');

      expect(view.result.current.passwordLegShown).toBe(false);
      expect(view.result.current.firstMissing('')).toBe('code');
    });
  });

  it('is unavailable in a state no input can complete', async () => {
    routes[READ] = () => json({}, 503);
    const view = mount({ readFailure: 'block' });
    await waitFor(() => expect(view.result.current.status.kind).toBe('blocked'), SETTLE);

    expect(view.result.current.firstMissing('hunter2')).toBe('unavailable');
  });

  it('needs no code on the security-key panel', async () => {
    const view = await mountReady();

    expect(view.result.current.method).toBe('webauthn');
    expect(view.result.current.firstMissing('hunter2')).toBeNull();
  });

  it('announces a status line per kind and queues nothing', async () => {
    const view = await mountTotp('');

    act(() => view.result.current.announceMissing('reading'));
    expect(view.result.current.notice).toEqual({ kind: 'checking' });
    act(() => view.result.current.announceMissing('password'));
    expect(view.result.current.notice).toEqual({ kind: 'missing', field: 'password' });
    act(() => view.result.current.announceMissing('code'));
    expect(view.result.current.notice).toEqual({ kind: 'missing', field: 'totp' });
    act(() => view.result.current.announceMissing('unavailable'));
    expect(view.result.current.notice).toEqual({ kind: 'missing', field: 'totp' });
    expect(hits(READ)).toHaveLength(1);
    expect(hits(BEGIN)).toHaveLength(0);
  });
});

describe('switchTo and setCode', () => {
  it('switches the panel, clears the code and the notice', async () => {
    routes[READ] = () => readBody(['webauthn', 'totp'], 'totp', true);
    const view = await mountReady();
    typeCode(view, '123456');
    act(() => view.result.current.announceMissing('password'));

    act(() => view.result.current.switchTo('backup'));

    expect(view.result.current.method).toBe('backup');
    expect(view.result.current.code).toBe('');
    expect(view.result.current.notice).toBeNull();
  });

  it('ignores a method that is not offered and the active one', async () => {
    routes[READ] = () => readBody(['totp'], 'totp');
    const view = await mountReady();
    typeCode(view, '123456');

    act(() => view.result.current.switchTo('webauthn'));
    act(() => view.result.current.switchTo('backup'));
    act(() => view.result.current.switchTo('totp'));

    expect(view.result.current.method).toBe('totp');
    expect(view.result.current.code).toBe('123456');
  });
});

// ── run: one activation, the proof, one submit ───────────────────────────

describe('run', () => {
  it('submits the TOTP code with spaces and hyphens stripped, with the context, once', async () => {
    const view = await mountTotp('123 456');
    const submit = submitting({ kind: 'success' });

    const outcome = await run(view, submit);

    expect(outcome).toEqual({ kind: 'success' });
    expect(submit).toHaveBeenCalledTimes(1);
    expect(submit.mock.calls[0][0]).toBe('123456');
    expect(submit.mock.calls[0][1]).toEqual(captureApiRequestContext());
    expect(view.result.current.phase).toBe('idle');
    expect(view.result.current.code).toBe('');
    expect(view.result.current.attempt).toBe(1);
  });

  it('submits a trimmed backup code', async () => {
    routes[READ] = () => readBody(['totp'], 'totp', true);
    const view = await mountReady();
    act(() => view.result.current.switchTo('backup'));
    typeCode(view, '  abcd-1234 ');
    const submit = submitting({ kind: 'success' });

    await run(view, submit);

    expect(submit.mock.calls[0][0]).toBe('abcd-1234');
  });

  it('submits undefined when no method is offered', async () => {
    routes[READ] = () => readBody([], null);
    const view = await mountReady({ passwordLeg: 'whenNoMfa' }); // pragma: allowlist secret
    const submit = submitting({ kind: 'success' });

    await run(view, submit);

    expect(submit.mock.calls[0][0]).toBeUndefined();
  });

  it('starts nothing for an empty code, an unready read, a disabled surface', async () => {
    const empty = await mountTotp('');
    const submit = submitting({ kind: 'success' });
    expect(await run(empty, submit)).toBeNull();

    routes[READ] = () => json({}, 503);
    const blocked = mount({ readFailure: 'block' });
    await waitFor(() => expect(blocked.result.current.status.kind).toBe('blocked'), SETTLE);
    expect(await run(blocked, submit)).toBeNull();

    const disabled = mount({ enabled: false });
    expect(await run(disabled, submit)).toBeNull();

    expect(submit).not.toHaveBeenCalled();
    expect(hits(BEGIN)).toHaveLength(0);
  });

  it('runs the whole security-key ceremony against one context and submits the token', async () => {
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });
    const capture = captureApiRequestContext();

    const outcome = await run(view, submit, capture);

    expect(outcome).toEqual({ kind: 'success' });
    expect(submit).toHaveBeenCalledTimes(1);
    expect(submit.mock.calls[0][0]).toBe(WEBAUTHN_TOKEN);
    // C82: begin, finish and the surface's request all use the caller's capture.
    expect(submit.mock.calls[0][1]).toBe(capture);
    expect(hits(BEGIN)[0][2].context).toBe(capture);
    expect(hits(FINISH)[0][2].context).toBe(capture);
    // The ceremony and both requests share one cancellable signal.
    const beginSignal = (hits(BEGIN)[0][1] as RequestInit).signal;
    expect(beginSignal).toBeInstanceOf(AbortSignal);
    expect((hits(FINISH)[0][1] as RequestInit).signal).toBe(beginSignal);
    expect(mockGet.mock.calls[0][0].signal).toBe(beginSignal);
  });

  // Mutant: begin sent without `{purpose}`.
  it.each(['dm.purge', 'dm.clear', 'privacy.purge_fence_disable'] as const)(
    'sends begin the purpose it mounted with: %s',
    async (purpose) => {
      const view = await mountReady({ purpose });

      await run(view, submitting({ kind: 'success' }));

      expect((hits(BEGIN)[0][1] as RequestInit).body).toBe(JSON.stringify({ purpose }));
    }
  );

  // Mutant: the purpose not pinned per mount (a changed purpose keeping the old instance).
  it('uses the new purpose after the surface reopens for another request', async () => {
    const view = await mountReady({ purpose: 'dm.purge' });
    view.rerender({ ...BASE, purpose: 'dm.clear' });
    await waitFor(() => expect(view.result.current.status.kind).toBe('ready'));

    await run(view, submitting({ kind: 'success' }));

    expect((hits(BEGIN)[0][1] as RequestInit).body).toBe(JSON.stringify({ purpose: 'dm.clear' }));
  });

  // Mutant: no ref guard (C6): a double click begins two ceremonies.
  it('begins one ceremony, and submits once, for two activations before React commits', async () => {
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });
    let results: (StepUpSubmitOutcome | null)[] = [];

    await act(async () => {
      const { run: activate } = view.result.current;
      results = await Promise.all([activate(submit), activate(submit)]);
    });

    expect(results).toEqual([{ kind: 'success' }, null]);
    expect(hits(BEGIN)).toHaveLength(1);
    expect(mockGet).toHaveBeenCalledTimes(1);
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it('allows the next activation once the first has settled', async () => {
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });

    await run(view, submit);
    await run(view, submit);

    expect(hits(BEGIN)).toHaveLength(2);
    expect(submit).toHaveBeenCalledTimes(2);
  });

  it('ignores code edits while the request is out', async () => {
    const view = await mountTotp();
    const answer = deferred<StepUpSubmitOutcome>();
    const submit = vi.fn<StepUpSubmit>(() => answer.promise);

    let pending!: Promise<unknown>;
    await act(async () => {
      pending = view.result.current.run(submit);
    });
    expect(view.result.current.phase).toBe('submitting');
    typeCode(view, '999999');
    expect(view.result.current.code).toBe('123456');

    await act(async () => {
      answer.resolve({ kind: 'success' });
      await pending;
    });
    expect(view.result.current.phase).toBe('idle');
  });

  it('reports the ceremony phase while the browser waits', async () => {
    const view = await mountReady();
    const credential = deferred<unknown>();
    mockGet.mockImplementation(() => credential.promise);

    let pending!: Promise<unknown>;
    await act(async () => {
      pending = view.result.current.run(submitting({ kind: 'success' }));
    });
    expect(view.result.current.phase).toBe('ceremony');

    await act(async () => {
      credential.resolve(CREDENTIAL);
      await pending;
    });
    expect(view.result.current.phase).toBe('idle');
  });
});

// ── C45 / C75 / C82: nothing is sent as another account or to another server

describe('an account or server change ends the run with nothing further sent', () => {
  const changes = [
    ['account', switchAccount],
    ['server', switchServer],
  ] as const;

  // Mutant: dropping the pre-ceremony recheck (C75).
  it.each(changes)('after begin, before the browser ceremony: %s', async (_name, change) => {
    const view = await mountReady();
    routes[BEGIN] = () => {
      change();
      return json({ publicKey: { challenge: 'AQID', rpId: 'localhost' } });
    };
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit)).toBeNull();

    expect(mockGet).not.toHaveBeenCalled();
    expect(hits(FINISH)).toHaveLength(0);
    expect(submit).not.toHaveBeenCalled();
    expect(view.result.current.status).toEqual({ kind: 'sessionExpired' });
  });

  // Mutant: dropping the pre-finish recheck (C75).
  it.each(changes)('after the ceremony, before finish: %s', async (_name, change) => {
    const view = await mountReady();
    mockGet.mockImplementation(async () => {
      change();
      return CREDENTIAL;
    });
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit)).toBeNull();

    expect(hits(FINISH)).toHaveLength(0);
    expect(submit).not.toHaveBeenCalled();
    expect(view.result.current.status).toEqual({ kind: 'sessionExpired' });
  });

  // Mutant: dropping the pre-submit recheck (C45): the minted token must not be spent.
  it.each(changes)('after finish, before submit: %s', async (_name, change) => {
    const view = await mountReady();
    routes[FINISH] = () => {
      change();
      return json({ mfa_token: WEBAUTHN_TOKEN });
    };
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit)).toBeNull();

    expect(submit).not.toHaveBeenCalled();
    expect(view.result.current.status).toEqual({ kind: 'sessionExpired' });
    expect(view.result.current.code).toBe('');
    expect(view.result.current.attempt).toBe(1);
  });

  // Mutant: dropping the pre-begin recheck, and `run` ignoring the caller's capture (C82).
  it.each(changes)('with a capture taken before preparation: %s', async (_name, change) => {
    const view = await mountReady();
    const capture = captureApiRequestContext();
    change();
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit, capture)).toBeNull();

    expect(hits(BEGIN)).toHaveLength(0);
    expect(submit).not.toHaveBeenCalled();
    expect(view.result.current.status).toEqual({ kind: 'sessionExpired' });
  });

  // Mutant: dropping the pre-submit recheck for a code factor, which has no earlier check.
  it.each(changes)('for a code factor with a stale capture: %s', async (_name, change) => {
    const view = await mountTotp();
    const capture = captureApiRequestContext();
    change();
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit, capture)).toBeNull();

    expect(submit).not.toHaveBeenCalled();
    expect(view.result.current.status).toEqual({ kind: 'sessionExpired' });
    expect(useTotpAcceptedStore.getState().acceptedAt).toEqual({});
  });

  it('stays live when only the access token refreshes', async () => {
    const view = await mountReady();
    routes[FINISH] = () => {
      useAuthStore.setState({ accessToken: 'refreshed-token' });
      return json({ mfa_token: WEBAUTHN_TOKEN });
    };
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit)).toEqual({ kind: 'success' });
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it('ends in sessionExpired when begin answers 401', async () => {
    const view = await mountReady();
    routes[BEGIN] = () => json({ error: 'Unauthorized' }, 401);
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit)).toBeNull();

    expect(view.result.current.status).toEqual({ kind: 'sessionExpired' });
    expect(submit).not.toHaveBeenCalled();
  });

  it('ends in sessionExpired when the account changes during the surface request', async () => {
    const view = await mountTotp();
    const submit = vi.fn<StepUpSubmit>(async () => {
      switchAccount();
      throw new DOMException('Request lifecycle changed before dispatch', 'AbortError');
    });

    expect(await run(view, submit)).toEqual({ kind: 'aborted' });

    expect(view.result.current.status).toEqual({ kind: 'sessionExpired' });
    expect(useTotpAcceptedStore.getState().acceptedAt).toEqual({});
  });
});

// ── C56: no proof outlives its attempt ───────────────────────────────────

describe('an ended attempt sends nothing (C56)', () => {
  /** Starts a security-key run whose browser ceremony stays pending. */
  async function startPendingCeremony(view: View, submit: StepUpSubmit) {
    const credential = deferred<unknown>();
    mockGet.mockImplementation(() => credential.promise);
    let pending!: Promise<StepUpSubmitOutcome | null>;
    await act(async () => {
      pending = view.result.current.run(submit);
    });
    await waitFor(() => expect(mockGet).toHaveBeenCalled());
    return { credential, pending };
  }

  // Mutant: dropping the attempt-id / `signal.aborted` check from the gate.
  it('switching methods aborts the ceremony and drops a credential that resolves anyway', async () => {
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });
    const { credential, pending } = await startPendingCeremony(view, submit);
    const { signal } = mockGet.mock.calls[0][0] as { signal: AbortSignal };

    act(() => view.result.current.switchTo('totp'));
    expect(signal.aborted).toBe(true);
    await act(async () => {
      credential.resolve(CREDENTIAL);
      expect(await pending).toBeNull();
    });

    expect(hits(FINISH)).toHaveLength(0);
    expect(submit).not.toHaveBeenCalled();
    expect(view.result.current.method).toBe('totp');
    expect(view.result.current.phase).toBe('idle');
    expect(view.result.current.notice).toBeNull();
    expect(view.result.current.status).toEqual({ kind: 'ready' });
  });

  it('closing the surface aborts the ceremony and sends nothing', async () => {
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });
    const { credential, pending } = await startPendingCeremony(view, submit);
    const { signal } = mockGet.mock.calls[0][0] as { signal: AbortSignal };

    view.rerender({ ...BASE, enabled: false });
    expect(signal.aborted).toBe(true);
    await act(async () => {
      credential.resolve(CREDENTIAL);
      expect(await pending).toBeNull();
    });

    expect(hits(FINISH)).toHaveLength(0);
    expect(submit).not.toHaveBeenCalled();
  });

  it('unmounting aborts the ceremony and sends nothing', async () => {
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });
    const { credential, pending } = await startPendingCeremony(view, submit);
    const { signal } = mockGet.mock.calls[0][0] as { signal: AbortSignal };

    view.unmount();
    expect(signal.aborted).toBe(true);
    credential.resolve(CREDENTIAL);
    expect(await pending).toBeNull();

    expect(submit).not.toHaveBeenCalled();
  });

  it('a token that finishes after the attempt ended is dropped unsent', async () => {
    const view = await mountReady();
    const finishAnswer = deferred<Response>();
    routes[FINISH] = () => finishAnswer.promise;
    const submit = submitting({ kind: 'success' });
    let pending!: Promise<StepUpSubmitOutcome | null>;
    await act(async () => {
      pending = view.result.current.run(submit);
    });
    await waitFor(() => expect(hits(FINISH)).toHaveLength(1));

    act(() => view.result.current.switchTo('totp'));
    await act(async () => {
      finishAnswer.resolve(json({ mfa_token: WEBAUTHN_TOKEN }));
      expect(await pending).toBeNull();
    });

    expect(submit).not.toHaveBeenCalled();
  });

  it('a request that settles after the surface closed still returns its outcome, silently', async () => {
    const view = await mountTotp();
    const answer = deferred<StepUpSubmitOutcome>();
    const submit = vi.fn<StepUpSubmit>(() => answer.promise);
    let pending!: Promise<StepUpSubmitOutcome | null>;
    await act(async () => {
      pending = view.result.current.run(submit);
    });

    view.rerender({ ...BASE, enabled: false });
    await act(async () => {
      answer.resolve(refusal({ kind: 'invalidMfaCode' }));
      expect(await pending).toEqual(refusal({ kind: 'invalidMfaCode' }));
    });

    // An invalidMfaCode would start a re-read for a live instance; this one is gone.
    expect(hits(READ)).toHaveLength(1);
  });
});

// ── C30 / C41 / C12: the recently-spent TOTP record ──────────────────────

describe('recording a TOTP acceptance (S2a)', () => {
  const recorded = () => useTotpAcceptedStore.getState().acceptedAt;

  // Mutants: recording on success only (C30); recording on invalidMfaCode,
  // invalidPassword or 429; no record on a transport failure (C41).
  it.each<[string, StepUpSubmitOutcome, boolean]>([
    ['success', { kind: 'success' }, true],
    ['answered', { kind: 'answered' }, true],
    ['transport', { kind: 'transport' }, true],
    ['failed', refusal({ kind: 'failed', message: 'boom' }), true],
    ['unavailable', refusal({ kind: 'unavailable' }), true],
    ['sessionExpired', refusal({ kind: 'sessionExpired' }), true],
    ['inlineFactorRequired', refusal({ kind: 'inlineFactorRequired', message: 'x' }), true],
    ['invalidMfaCode', refusal({ kind: 'invalidMfaCode' }), false],
    ['invalidPassword', refusal({ kind: 'invalidPassword' }), false],
    ['mfaRequired', refusal({ kind: 'mfaRequired', methods: ['totp'] }), false],
    ['passwordRequired', refusal({ kind: 'passwordRequired' }), false],
    ['rateLimited', refusal({ kind: 'rateLimited' }), false],
    ['aborted', { kind: 'aborted' }, false],
  ])('%s -> recorded: %s', async (_name, outcome, expected) => {
    const view = await mountTotp();
    vi.spyOn(Date, 'now').mockReturnValue(1_700_000_000_000);

    await run(view, submitting(outcome));

    expect(recorded()).toEqual(expected ? { [ACCOUNT]: 1_700_000_000_000 } : {});
  });

  // Mutant: no record on a transport failure (C41), when the surface throws.
  it('records a code whose request threw a non-abort error', async () => {
    const view = await mountTotp();
    const submit = vi.fn<StepUpSubmit>(async () => {
      throw new TypeError('Failed to fetch');
    });

    expect(await run(view, submit)).toEqual({ kind: 'transport' });

    expect(recorded()).toHaveProperty(ACCOUNT);
  });

  // Mutant: mapping a thrown AbortError from submit to `transport`.
  it('maps an AbortError out of submit to aborted: unspent, code kept, nothing recorded', async () => {
    const view = await mountTotp();
    const submit = vi.fn<StepUpSubmit>(async () => {
      throw new DOMException('Request lifecycle changed before dispatch', 'AbortError');
    });

    expect(await run(view, submit)).toEqual({ kind: 'aborted' });

    expect(recorded()).toEqual({});
    expect(view.result.current.code).toBe('123456');
    expect(view.result.current.attempt).toBe(0);
    expect(view.result.current.phase).toBe('idle');
    expect(view.result.current.status).toEqual({ kind: 'ready' });
  });

  // Mutant: keying acceptance on the active panel (C12).
  it('records a six-digit value even when the backup panel was active', async () => {
    routes[READ] = () => readBody(['totp'], 'totp', true);
    const view = await mountReady();
    act(() => view.result.current.switchTo('backup'));
    typeCode(view, '123-456');

    await run(view, submitting({ kind: 'success' }));

    expect(recorded()).toHaveProperty(ACCOUNT);
  });

  // Mutant: keying acceptance on the active panel (C12), the other direction.
  it('does not record a value that is not six digits, even on the TOTP panel', async () => {
    const view = await mountTotp('abcd1234');

    await run(view, submitting({ kind: 'success' }));

    expect(recorded()).toEqual({});
  });

  it('does not record a backup code', async () => {
    routes[READ] = () => readBody(['totp'], 'totp', true);
    const view = await mountReady();
    act(() => view.result.current.switchTo('backup'));
    typeCode(view, 'abcd-efgh');

    await run(view, submitting({ kind: 'success' }));

    expect(recorded()).toEqual({});
  });

  it('never records a security-key token, whatever it looks like', async () => {
    routes[FINISH] = () => json({ mfa_token: '123456' });
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });

    await run(view, submit);

    expect(submit.mock.calls[0][0]).toBe('123456');
    expect(recorded()).toEqual({});
  });

  it('records nothing without a signed-in account', async () => {
    useUserStore.setState({ user: null });
    const view = await mountTotp();

    await run(view, submitting({ kind: 'success' }));

    expect(recorded()).toEqual({});
  });

  it('records a request that settles after the surface closed', async () => {
    const view = await mountTotp();
    const answer = deferred<StepUpSubmitOutcome>();
    let pending!: Promise<unknown>;
    await act(async () => {
      pending = view.result.current.run(vi.fn<StepUpSubmit>(() => answer.promise));
    });

    view.rerender({ ...BASE, enabled: false });
    await act(async () => {
      answer.resolve({ kind: 'success' });
      await pending;
    });

    expect(recorded()).toHaveProperty(ACCOUNT);
  });

  // Mutant: recording without the context check: the account changed under the request.
  it('drops a record for a request that settles after an account change', async () => {
    const view = await mountTotp();
    const submit = vi.fn<StepUpSubmit>(async () => {
      switchAccount();
      return { kind: 'success' };
    });

    await run(view, submit);

    expect(recorded()).toEqual({});
  });
});

// ── Refusals of the gated request ────────────────────────────────────────

describe('a refusal of the gated request', () => {
  it('keeps the code for an invalidPassword refusal and says which field (#3466)', async () => {
    const view = await mountTotp();

    await run(view, submitting(refusal({ kind: 'invalidPassword' })));

    expect(view.result.current.notice).toEqual({ kind: 'invalidPassword' });
    expect(view.result.current.code).toBe('123456');
    expect(view.result.current.attempt).toBe(0);
    expect(view.result.current.phase).toBe('idle');
    expect(hits(READ)).toHaveLength(1);
  });

  it('spends the code on an invalidMfaCode refusal and names the method', async () => {
    const view = await mountTotp();

    await run(view, submitting(refusal({ kind: 'invalidMfaCode' })));

    expect(view.result.current.notice).toEqual({ kind: 'invalidFactor', method: 'totp' });
    expect(view.result.current.code).toBe('');
    expect(view.result.current.attempt).toBe(1);
  });

  it.each<[string, StepUpSubmitOutcome]>([
    ['answered', { kind: 'answered' }],
    ['transport', { kind: 'transport' }],
    ['failed', refusal({ kind: 'failed' })],
  ])('spends the code and shows no notice for %s', async (_name, outcome) => {
    const view = await mountTotp();
    act(() => view.result.current.announceMissing('password'));

    await run(view, submitting(outcome));

    expect(view.result.current.notice).toBeNull();
    expect(view.result.current.code).toBe('');
    expect(view.result.current.attempt).toBe(1);
    expect(hits(READ)).toHaveLength(1);
  });

  // Mutant: `passwordRequired` not emptying the set on whenNoMfa (C2).
  it('empties the offered set on passwordRequired when the route is whenNoMfa', async () => {
    routes[READ] = () => readBody(['totp'], 'totp', true);
    const view = await mountReady({ passwordLeg: 'whenNoMfa' }); // pragma: allowlist secret
    typeCode(view, '123456');

    await run(view, submitting(refusal({ kind: 'passwordRequired' })));

    expect(view.result.current.methods).toEqual([]);
    expect(view.result.current.method).toBeNull();
    expect(view.result.current.passwordLegShown).toBe(true);
    expect(view.result.current.notice).toEqual({ kind: 'missing', field: 'password' });
    expect(view.result.current.status).toEqual({ kind: 'ready' });
  });

  it('keeps the offered set on passwordRequired when the password is always collected', async () => {
    const view = await mountTotp();

    await run(view, submitting(refusal({ kind: 'passwordRequired' })));

    expect(view.result.current.methods).toEqual(['totp', 'backup']);
    expect(view.result.current.method).toBe('totp');
    expect(view.result.current.code).toBe('123456');
    expect(view.result.current.notice).toEqual({ kind: 'missing', field: 'password' });
  });

  it('replaces the set with the refusal methods and keeps the active panel while offered', async () => {
    routes[READ] = () => readBody(['webauthn', 'totp'], 'totp');
    const view = await mountReady();
    typeCode(view, '123456');

    await run(view, submitting(refusal({ kind: 'mfaRequired', methods: ['totp', 'webauthn'] })));

    expect(view.result.current.methods).toEqual(['webauthn', 'totp']);
    expect(view.result.current.method).toBe('totp');
    expect(view.result.current.notice).toEqual({ kind: 'missing', field: 'totp' });
    expect(view.result.current.code).toBe('');
    expect(view.result.current.attempt).toBe(1);
    expect(hits(READ)).toHaveLength(1);
  });

  it('moves to the default when the refusal no longer offers the active panel', async () => {
    routes[READ] = () => readBody(['webauthn', 'totp'], 'totp');
    const view = await mountReady();
    typeCode(view, '123456');

    await run(view, submitting(refusal({ kind: 'mfaRequired', methods: ['webauthn'] })));

    expect(view.result.current.methods).toEqual(['webauthn']);
    expect(view.result.current.method).toBe('webauthn');
  });

  // Mutant: a refusal list admitting `email` (G1).
  it('never offers email or SMS from a refusal', async () => {
    const view = await mountTotp();

    await run(
      view,
      submitting(refusal({ kind: 'mfaRequired', methods: ['email', 'sms', 'totp'] }))
    );

    // The read's backup-code report still stands beside the replaced inline set.
    expect(view.result.current.methods).toEqual(['totp', 'backup']);
    expect(view.result.current.method).toBe('totp');
    expect(view.result.current.status).toEqual({ kind: 'ready' });
  });

  // Mutant: a refusal list admitting `email` (G1): nothing usable is terminal.
  it.each([
    ['only email', ['email']],
    ['email and sms', ['email', 'sms']],
    ['an empty list', []],
  ])('ends in noUsableMethod when the refusal names %s', async (_name, methods) => {
    const view = await mountTotp();

    await run(view, submitting(refusal({ kind: 'mfaRequired', methods })));

    expect(view.result.current.status).toEqual({ kind: 'noUsableMethod' });
    expect(view.result.current.methods).toEqual([]);
    expect(view.result.current.method).toBeNull();
    expect(view.result.current.notice).toBeNull();
    expect(view.result.current.passwordLegShown).toBe(false);
  });

  it('offers no security key from a refusal on a route without a purpose', async () => {
    routes[READ] = () => readBody(['totp'], 'totp');
    const view = await mountReady({ purpose: null });
    typeCode(view, '123456');

    await run(view, submitting(refusal({ kind: 'mfaRequired', methods: ['webauthn'] })));

    expect(view.result.current.status).toEqual({ kind: 'noUsableMethod' });
  });
});

// ── C13 / C25 / C32: the background re-read ──────────────────────────────

describe('the background re-read', () => {
  const noReread: [string, StepUpSubmitOutcome][] = [
    ['success', { kind: 'success' }],
    ['answered', { kind: 'answered' }],
    ['transport', { kind: 'transport' }],
    ['aborted', { kind: 'aborted' }],
    ['failed', refusal({ kind: 'failed' })],
    ['invalidPassword', refusal({ kind: 'invalidPassword' })],
    ['passwordRequired', refusal({ kind: 'passwordRequired' })],
    ['mfaRequired', refusal({ kind: 'mfaRequired', methods: ['totp'] })],
    ['rateLimited', refusal({ kind: 'rateLimited' })],
  ];

  // Mutant: a re-read on every refusal (C13).
  it.each(noReread)('does not re-read after %s', async (_name, outcome) => {
    const view = await mountTotp();

    await run(view, submitting(outcome));
    await act(async () => {});

    expect(hits(READ)).toHaveLength(1);
    expect(view.result.current.reread).toBeNull();
  });

  // Mutants: no re-read (C13); two GETs for one response (C32).
  it('reads exactly once more after invalidMfaCode, and exposes the result', async () => {
    routes[READ] = () => readBody(['totp'], 'totp', true);
    const view = await mountReady();
    typeCode(view, '123456');

    await run(view, submitting(refusal({ kind: 'invalidMfaCode' })));
    await waitFor(() => expect(view.result.current.reread).not.toBeNull());
    await act(async () => {});

    expect(hits(READ)).toHaveLength(2);
    expect(view.result.current.reread).toEqual({
      kind: 'ready',
      methods: ['totp'],
      defaultMethod: 'totp',
      backupCodeAvailable: true,
    });
    // Nothing moved: same set, same panel, the refusal's own notice stands.
    expect(view.result.current.method).toBe('totp');
    expect(view.result.current.notice).toEqual({ kind: 'invalidFactor', method: 'totp' });
  });

  it('moves the active panel to the new default and says so when the re-read dropped it', async () => {
    let reads = 0;
    routes[READ] = () =>
      ++reads === 1 ? readBody(['webauthn', 'totp'], 'totp') : readBody(['webauthn'], 'webauthn');
    const view = await mountReady();
    typeCode(view, '123456');

    await run(view, submitting(refusal({ kind: 'invalidMfaCode' })));

    await waitFor(() => expect(view.result.current.notice).toEqual({ kind: 'methodsChanged' }));
    expect(view.result.current.methods).toEqual(['webauthn']);
    expect(view.result.current.method).toBe('webauthn');
    expect(view.result.current.code).toBe('');
    expect(hits(READ)).toHaveLength(2);
  });

  it('is terminal when the re-read is refused', async () => {
    let reads = 0;
    routes[READ] = () =>
      ++reads === 1 ? readBody(['totp'], 'totp') : json({ code: 'EMAIL_NOT_VERIFIED' }, 403);
    const view = await mountReady();
    typeCode(view, '123456');

    await run(view, submitting(refusal({ kind: 'invalidMfaCode' })));

    await waitFor(() =>
      expect(view.result.current.status).toEqual({ kind: 'refused', reason: 'emailUnverified' })
    );
    expect(view.result.current.notice).toBeNull();
  });

  it('changes nothing when the re-read is unavailable, but exposes it', async () => {
    let reads = 0;
    routes[READ] = () => (++reads === 1 ? readBody(['totp'], 'totp') : json({}, 503));
    const view = await mountReady();
    typeCode(view, '123456');

    await run(view, submitting(refusal({ kind: 'invalidMfaCode' })));

    await waitFor(() => expect(view.result.current.reread).toEqual({ kind: 'unavailable' }));
    expect(view.result.current.status).toEqual({ kind: 'ready' });
    expect(view.result.current.methods).toEqual(['totp']);
    expect(view.result.current.notice).toEqual({ kind: 'invalidFactor', method: 'totp' });
  });

  it('discards an aborted re-read', async () => {
    let reads = 0;
    routes[READ] = () => {
      if (++reads === 1) return readBody(['totp'], 'totp');
      throw new DOMException('aborted', 'AbortError');
    };
    const view = await mountReady();
    typeCode(view, '123456');

    await run(view, submitting(refusal({ kind: 'invalidMfaCode' })));
    await waitFor(() => expect(hits(READ)).toHaveLength(2));
    await act(async () => {});

    expect(view.result.current.reread).toBeNull();
    expect(view.result.current.status).toEqual({ kind: 'ready' });
  });

  describe('after begin (C25)', () => {
    // Mutant: no re-read after begin's `No WebAuthn credentials registered`.
    it('reads once when the account has no security key left', async () => {
      routes[BEGIN] = () => json({ error: 'No WebAuthn credentials registered' }, 400);
      const view = await mountReady();
      const submit = submitting({ kind: 'success' });

      expect(await run(view, submit)).toBeNull();
      await waitFor(() => expect(view.result.current.reread).not.toBeNull());
      await act(async () => {});

      expect(hits(READ)).toHaveLength(2);
      expect(submit).not.toHaveBeenCalled();
      expect(mockGet).not.toHaveBeenCalled();
      expect(view.result.current.phase).toBe('idle');
      expect(view.result.current.notice).toEqual({ kind: 'invalidFactor', method: 'webauthn' });
    });

    it('moves off the security-key panel when the re-read says the key is gone', async () => {
      let reads = 0;
      routes[READ] = () =>
        ++reads === 1 ? readBody(['webauthn', 'totp'], 'webauthn') : readBody(['totp'], 'totp');
      routes[BEGIN] = () => json({ error: 'No WebAuthn credentials registered' }, 400);
      const view = await mountReady();

      await run(view, submitting({ kind: 'success' }));

      await waitFor(() => expect(view.result.current.method).toBe('totp'));
      expect(view.result.current.notice).toEqual({ kind: 'methodsChanged' });
    });

    it('does not read for another begin failure', async () => {
      routes[BEGIN] = () => json({ error: 'Failed to start verification' }, 500);
      const view = await mountReady();

      await run(view, submitting({ kind: 'success' }));
      await act(async () => {});

      expect(hits(READ)).toHaveLength(1);
      expect(view.result.current.notice).toEqual({ kind: 'invalidFactor', method: 'webauthn' });
    });

    it('does not read when finish, not begin, reports no credentials', async () => {
      routes[FINISH] = () => json({ error: 'No WebAuthn credentials registered' }, 400);
      const view = await mountReady();

      await run(view, submitting({ kind: 'success' }));
      await act(async () => {});

      expect(hits(READ)).toHaveLength(1);
      expect(view.result.current.notice).toEqual({ kind: 'invalidFactor', method: 'webauthn' });
    });
  });
});

// ── Ceremony failures ────────────────────────────────────────────────────

describe('a ceremony that mints no token', () => {
  // The exact server string that means the begin session expired or was replaced.
  it('says cancelled when finish reports the verification session is gone', async () => {
    routes[FINISH] = () =>
      json({ error: 'No verification session found. Start a new verification.' }, 400);
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit)).toBeNull();

    expect(view.result.current.notice).toEqual({ kind: 'webauthnCancelled' });
    expect(view.result.current.phase).toBe('idle');
    expect(submit).not.toHaveBeenCalled();
    expect(hits(READ)).toHaveLength(1);
  });

  it('says cancelled when the user dismisses the browser prompt', async () => {
    mockGet.mockRejectedValue(new DOMException('denied', 'NotAllowedError'));
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit)).toBeNull();

    expect(view.result.current.notice).toEqual({ kind: 'webauthnCancelled' });
    expect(hits(FINISH)).toHaveLength(0);
    expect(submit).not.toHaveBeenCalled();
  });

  it('says cancelled when the browser times the ceremony out', async () => {
    mockGet.mockRejectedValue(new DOMException('timed out', 'AbortError'));
    const view = await mountReady();

    await run(view, submitting({ kind: 'success' }));

    expect(view.result.current.notice).toEqual({ kind: 'webauthnCancelled' });
  });

  it.each([
    ['the browser returns no credential', () => mockGet.mockResolvedValue(null)],
    [
      'finish answers 400 with another error',
      () => (routes[FINISH] = () => json({ error: 'x' }, 400)),
    ],
    ['finish returns no token', () => (routes[FINISH] = () => json({}))],
  ])('shows the key as not accepted when %s', async (_name, arrange) => {
    arrange();
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit)).toBeNull();

    expect(view.result.current.notice).toEqual({ kind: 'invalidFactor', method: 'webauthn' });
    expect(view.result.current.phase).toBe('idle');
    expect(submit).not.toHaveBeenCalled();
    expect(hits(READ)).toHaveLength(1);
  });

  it('can be tried again after a failure', async () => {
    mockGet.mockRejectedValueOnce(new DOMException('denied', 'NotAllowedError'));
    const view = await mountReady();
    const submit = submitting({ kind: 'success' });

    expect(await run(view, submit)).toBeNull();
    expect(await run(view, submit)).toEqual({ kind: 'success' });
    expect(hits(BEGIN)).toHaveLength(2);
  });
});
