import { act, renderHook, waitFor, type RenderHookResult } from '@testing-library/react';
import { useLayoutEffect, useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// REPRODUCTION tests for three races in useStepUpFactor. Each asserts the
// CORRECT behaviour and is expected to FAIL against the current tree, on the
// named assertion. The harness (mount, routes, readBody, switchAccount,
// mountTotp, submitting) is copied from useStepUpFactor.test.tsx.
//
//   H1 (C13/C32): a stale background re-read overrides a newer refusal.
//   H2 (C57):     a read answered after an account switch is applied.
//   H3 (C56):     a close between commit and the passive-effect flush can still
//                 let a submit go out.

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
import { resetRuntimeServerBase } from '@/renderer/services/system/runtimeServerBase';
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
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
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

async function mountReady(overrides: Partial<StepUpFactorProps> = {}): Promise<View> {
  const view = mount(overrides);
  await waitFor(() => expect(view.result.current.status.kind).toBe('ready'));
  return view;
}

/** A ready TOTP-only instance with a six-digit code typed. */
async function mountTotp(code = '123456'): Promise<View> {
  routes[READ] = () => readBody(['totp'], 'totp', true);
  const view = await mountReady();
  typeCode(view, code);
  return view;
}

function typeCode(view: View, code: string) {
  act(() => view.result.current.setCode(code));
}

async function run(view: View, submit: StepUpSubmit) {
  let outcome: StepUpSubmitOutcome | null | undefined;
  await act(async () => {
    outcome = await view.result.current.run(submit);
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

// ── H1: a stale re-read must not override a newer refusal ────────────────

describe('H1: a background re-read superseded by a newer run (C13/C32)', () => {
  it('does not move the panel off the newer refusal when the old re-read lands late', async () => {
    const view = await mountTotp('123456');
    const staleReread = deferred<Response>();
    routes[READ] = () => staleReread.promise;

    // Run 1: invalidMfaCode starts a re-read that stays in flight.
    await run(view, submitting(refusal({ kind: 'invalidMfaCode' })));
    await waitFor(() => expect(hits(READ)).toHaveLength(2));

    // Run 2: a newer refusal names only a security key, which is authoritative.
    typeCode(view, '654321');
    await run(view, submitting(refusal({ kind: 'mfaRequired', methods: ['webauthn'] })));
    expect(view.result.current.method).toBe('webauthn');

    // The re-read run 1 started answers with the OLD set.
    await act(async () => staleReread.resolve(readBody(['totp'], 'totp')));
    await act(async () => {});

    expect(view.result.current.method, 'stale re-read moved the active panel').toBe('webauthn');
    expect(view.result.current.notice).not.toEqual({ kind: 'methodsChanged' });
  });
});

// ── H2: a read answered after an account switch must not apply ───────────

describe('H2: a read answered after an account switch (C57)', () => {
  it('ends in sessionExpired rather than applying the old account read', async () => {
    routes[READ] = () => {
      switchAccount();
      return readBody(['totp'], 'totp');
    };

    const view = mount();
    await waitFor(() => expect(view.result.current.status.kind).not.toBe('reading'));

    expect(view.result.current.status).toEqual({ kind: 'sessionExpired' });
  });
});

// ── H3: a close between commit and the passive flush ─────────────────────

interface HostHandle {
  factor: StepUpFactor;
  setEnabled: (enabled: boolean) => void;
}

/**
 * Mounts the hook under a real root, outside `act`, so passive effects are
 * deferred the way a default-priority update defers them. `onDisabledCommit`
 * runs in the layout phase of the commit in which `enabled` became false.
 */
function Host({
  handle,
  events,
  onDisabledCommit,
}: Readonly<{
  handle: { current: HostHandle | null };
  events: string[];
  onDisabledCommit: () => void;
}>) {
  const [enabled, setEnabled] = useState(true);
  const factor = useStepUpFactor({ ...BASE, enabled });
  useLayoutEffect(() => {
    handle.current = { factor, setEnabled };
  });
  useEffect(
    () => () => {
      events.push('passive-cleanup');
    },
    [enabled, events]
  );
  useLayoutEffect(() => {
    if (enabled) return;
    events.push('layout:disabled');
    onDisabledCommit();
  }, [enabled, events, onDisabledCommit]);
  return null;
}

describe('H3: a close that lands before the passive-effect flush (C56)', () => {
  let savedActEnv: boolean | undefined;
  beforeEach(() => {
    savedActEnv = (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT;
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = false;
  });
  afterEach(() => {
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = savedActEnv;
  });

  it('never submits a WebAuthn token whose finish resolved after the commit that closed it', async () => {
    const finish = deferred<Response>();
    routes[FINISH] = () => finish.promise;
    const events: string[] = [];
    const handle: { current: HostHandle | null } = { current: null };
    // A plain response: a real Response body read takes macrotasks, and the
    // point is that finish resolves in a microtask before the passive flush.
    const token = {
      ok: true,
      status: 200,
      json: async () => ({ mfa_token: WEBAUTHN_TOKEN }),
    } as unknown as Response;

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    const onDisabledCommit = () => finish.resolve(token);
    root.render(<Host handle={handle} events={events} onDisabledCommit={onDisabledCommit} />);

    try {
      await vi.waitFor(() => expect(handle.current?.factor.status.kind).toBe('ready'));
      const submit = vi.fn<StepUpSubmit>(async () => {
        events.push('submit');
        return { kind: 'success' };
      });

      void handle.current!.factor.run(submit);
      await vi.waitFor(() => expect(hits(FINISH)).toHaveLength(1));
      events.length = 0;

      // The host closes the surface; finish resolves inside that commit.
      handle.current!.setEnabled(false);
      await vi.waitFor(() => expect(events).toContain('passive-cleanup'));
      await new Promise((resolve) => setTimeout(resolve, 20));

      // Faithfulness guard: the close committed (layout) before anything else.
      expect(events[0]).toBe('layout:disabled');
      expect(submit, `events: ${events.join(' > ')}`).not.toHaveBeenCalled();
    } finally {
      root.unmount();
      container.remove();
    }
  });
});
