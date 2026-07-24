// Regression for #2346 — the #1293 consented key-recovery prompt is
// structurally unreachable in the desktop login flow when Login is mounted
// under App's routing gate.
//
// Mechanism: completeLoginFromResponse() calls beginAuthLifecycle() to publish
// the access token BEFORE unwrapLoginKeys() runs (Login.tsx). App's "/" route
// element is `accessToken && emailVerified ? <Navigate to="/app/dms" replace/>
// : <AuthFlow/>` (App.tsx), subscribed via Zustand selectors, and emailVerified
// defaults true (authStore.ts). So the early token-set re-renders the gate and
// unmounts Login mid-login; when unwrapLoginKeys then rejects, promptKeyRecovery
// calls setKeyRecoveryResolver on an unmounted component (React no-ops it), the
// KeyRecoveryPrompt never renders, and the login neither completes nor aborts.
//
// This file is the ROUTER-LEVEL companion to
// tests/unit/components/Auth/Login.test.tsx's
// "shows the recovery prompt (no silent PUT) when unwrapLoginKeys fails" — same
// mocks, same failing unwrap; the ONLY added variable is the route-swap gate
// (which that test lacks: test-utils provides a BrowserRouter *context* but no
// <Routes> that can hide Login). That test passes today; this one fails today.

import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Routes, Route, Navigate } from 'react-router-dom';
import { vi } from 'vitest';

import Login from '@/renderer/components/Auth/Login';
import { ModalProvider } from '@/renderer/components/ui/ModalContext';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useClientConfigStore } from '@/renderer/stores/clientConfigStore';
import { e2eeService } from '@/renderer/services/e2eeService';
import { resetAllStores } from '../../../helpers/store-helpers';
import { resetRuntimeServerBase } from '@/renderer/services/runtimeServerBase';
import { isE2EEUnlockPending } from '@/renderer/utils/authAdmissionGate';

const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

const mockUnwrapLoginKeys = vi.fn().mockResolvedValue(undefined);
const mockGenerateRegistrationKeys = vi.fn().mockResolvedValue({
  wrappedPrivateKey: 'mock', // pragma: allowlist secret
  keyDerivationSalt: 'mock',
  keyDerivationAlg: 'argon2id',
  publicKey: {},
});
const { mockE2EESessionKeys, mockE2EEInitializationReceipt, mockE2EEClearKeys } = vi.hoisted(() => {
  const sessionKeys = {
    wrappingKeyBase64: 'mock-wrapping-key',
    preferencesKeyBase64: 'mock-preferences-key',
    wrappedPrivateKeyBase64: 'mock-wrapped-private-key', // pragma: allowlist secret
  };
  return {
    mockE2EESessionKeys: sessionKeys,
    mockE2EEInitializationReceipt: { sessionKeys, attempt: 1 },
    mockE2EEClearKeys: vi.fn(),
  };
});

vi.mock('@/renderer/utils/crypto', () => ({
  unwrapLoginKeys: (...args: unknown[]) => mockUnwrapLoginKeys(...args),
  generateRegistrationKeys: (...args: unknown[]) => mockGenerateRegistrationKeys(...args),
  exportPublicKey: vi.fn().mockResolvedValue('mock-public-key'),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    initialize: vi.fn().mockResolvedValue(undefined),
    clearKeys: mockE2EEClearKeys,
    captureTeardownEpoch: vi.fn().mockReturnValue(0),
    wasTornDownSince: vi.fn().mockReturnValue(false),
    getSessionKeys: vi.fn().mockReturnValue(mockE2EESessionKeys),
    captureInitializationReceipt: vi.fn().mockReturnValue(mockE2EEInitializationReceipt),
    clearKeysIfInitializationCurrent: vi.fn((receipt: unknown) => {
      if (receipt !== mockE2EEInitializationReceipt) return false;
      mockE2EEClearKeys();
      return true;
    }),
  },
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    init: vi.fn(),
    startWatching: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/apiClient', () => ({
  API_BASE: 'http://localhost:8080',
  apiFetch: vi.fn().mockResolvedValue({ ok: true, json: async () => ({}) }),
  ensureMachineId: vi.fn().mockResolvedValue('mock-machine-id'),
  revokeAbortedSession: vi.fn().mockResolvedValue(undefined),
}));

function makeLoginResponse(overrides = {}) {
  return {
    access_token: 'mock-access',
    refresh_token: 'mock-refresh',
    session_id: 'mock-session',
    user: { id: 'user-1', username: 'testuser', email: 'test@example.com' },
    remember_me: false,
    e2ee_keys: {
      wrapped_private_key: 'mock-wrapped', // pragma: allowlist secret
      key_derivation_salt: 'mock-salt',
      key_derivation_alg: 'pbkdf2',
    },
    ...overrides,
  };
}

const loginProps = {
  onBack: vi.fn(),
  onSuccess: vi.fn(),
  onSwitchToRegister: vi.fn(),
  onForgotPassword: vi.fn(),
};

// Mirrors App.tsx's "/" route element using its REAL gate predicate (the shared
// `isE2EEUnlockPending` helper, not a copy of the condition) over the same store
// selectors. Login stands in for AuthFlow — the unmount boundary is identical
// (the whole subtree under "/" is torn down). The ONLY difference from the
// no-gate positive control in Login.test.tsx is this route swap; the fix's job
// is to keep it from firing while this login's inline E2EE unlock is pending.
function RootRoute() {
  const accessToken = useAuthStore((s) => s.accessToken);
  const emailVerified = useAuthStore((s) => s.emailVerified);
  const authGeneration = useAuthStore((s) => s.authGeneration);
  const pendingE2EEUnlockGeneration = useAuthStore((s) => s.pendingE2EEUnlockGeneration);
  return accessToken &&
    emailVerified &&
    !isE2EEUnlockPending(pendingE2EEUnlockGeneration, authGeneration) ? (
    <Navigate to="/app/dms" replace />
  ) : (
    <Login {...loginProps} />
  );
}

function renderUnderRoutingGate() {
  return render(
    <MemoryRouter initialEntries={['/']}>
      <ModalProvider>
        <Routes>
          <Route path="/" element={<RootRoute />} />
          <Route path="/app/dms" element={<div>dms-placeholder</div>} />
        </Routes>
      </ModalProvider>
    </MemoryRouter>
  );
}

const originalElectron = globalThis.electron;

describe('Login key-recovery prompt under App routing gate (#2346)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    resetRuntimeServerBase();
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...originalElectron,
        storeRefreshToken: vi.fn().mockResolvedValue(41),
        storeE2EEKeys: vi.fn().mockResolvedValue(undefined),
        clearTokensIfOwner: vi.fn().mockResolvedValue(true),
        storeE2EEKeysIfOwner: vi.fn().mockResolvedValue(true),
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });
    vi.mocked(e2eeService.wasTornDownSince).mockReturnValue(false);
    useClientConfigStore.getState().setServerCapabilities({
      auth: { oauthProviders: ['google', 'apple'] },
    });
  });

  afterEach(() => {
    resetRuntimeServerBase();
    Object.defineProperty(globalThis, 'electron', {
      value: originalElectron,
      writable: true,
    });
  });

  it('keeps the consented key-recovery prompt reachable when unwrapLoginKeys fails', async () => {
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => makeLoginResponse() });

    const user = userEvent.setup();
    renderUnderRoutingGate();

    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    // CONTRACT (#2346): a failed unwrap must surface the consented key-recovery
    // prompt even under App's routing gate. Today the early beginAuthLifecycle()
    // token-set navigates to /app/dms and unmounts Login before the prompt can
    // render, so this findByRole times out — that timeout IS the reproduction.
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toBeInTheDocument();

    // …and the user must NOT have been silently stranded on /app/dms with dead
    // E2EE. Under the bug they are (the placeholder renders); after the fix the
    // gate holds Login mounted and this stays absent.
    expect(screen.queryByText('dms-placeholder')).not.toBeInTheDocument();

    // Dismiss so the suspended login flow settles and no dialog leaks.
    await user.click(screen.getByRole('button', { name: /cancel/i }));
  });
});

describe('isE2EEUnlockPending (#2346 gate predicate)', () => {
  it('holds only while the pending generation matches the current auth generation', () => {
    // This session's inline E2EE unlock is pending → hold the "/" route.
    expect(isE2EEUnlockPending(5, 5)).toBe(true);
    // A superseded/aborted flow's stale value: any token change bumps
    // authGeneration past it, so it can never gate a successor session.
    expect(isE2EEUnlockPending(5, 6)).toBe(false);
    // No inline unlock pending.
    expect(isE2EEUnlockPending(null, 5)).toBe(false);
    // Generation 0 is a real generation, not an "unset" sentinel.
    expect(isE2EEUnlockPending(0, 0)).toBe(true);
  });
});
