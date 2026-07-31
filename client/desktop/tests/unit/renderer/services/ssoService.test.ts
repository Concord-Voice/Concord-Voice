import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  startSSOFlow,
  completeSSORegistration,
  completeSSOLink,
  completeSSOMFA,
  abandonSSOReservation,
} from '@/renderer/services/ssoService';
import { useAuthStore } from '@/renderer/stores/authStore';

const API_BASE = 'http://localhost:8080';

interface ElectronSSO {
  startLoopback: ReturnType<typeof vi.fn>;
  awaitCallback: ReturnType<typeof vi.fn>;
  cancelLoopback: ReturnType<typeof vi.fn>;
  appleSignIn: ReturnType<typeof vi.fn>;
  appleCancel: ReturnType<typeof vi.fn>;
  googleSignIn: ReturnType<typeof vi.fn>;
  googleCancel: ReturnType<typeof vi.fn>;
  completeRegistration: ReturnType<typeof vi.fn>;
  completeLink: ReturnType<typeof vi.fn>;
  completeMFA: ReturnType<typeof vi.fn>;
  abandonReservation?: ReturnType<typeof vi.fn>;
}

interface ElectronTestSurface {
  sso: ElectronSSO;
  openExternal: ReturnType<typeof vi.fn>;
}

function installElectronMock(): ElectronTestSurface {
  const surface: ElectronTestSurface = {
    sso: {
      startLoopback: vi
        .fn()
        .mockResolvedValue({ port: 65432, redirectURI: 'http://127.0.0.1:65432/oauth/callback' }),
      awaitCallback: vi.fn().mockResolvedValue({ code: 'fake-code', state: 'returned-state' }),
      cancelLoopback: vi.fn(),
      appleSignIn: vi.fn().mockResolvedValue({
        kind: 'tokens',
        accessToken: 'apple-at',
        sessionId: 'apple-session',
        credentialOwner: 41,
      }),
      appleCancel: vi.fn(),
      googleSignIn: vi.fn().mockResolvedValue({
        kind: 'tokens',
        accessToken: 'google-at',
        sessionId: 'google-session',
        credentialOwner: 42,
      }),
      googleCancel: vi.fn(),
      completeRegistration: vi.fn().mockResolvedValue({
        kind: 'tokens',
        accessToken: 'reg-token-1',
        sessionId: 'reg-session-1',
        credentialOwner: 43,
      }),
      completeLink: vi.fn().mockResolvedValue({
        kind: 'tokens',
        accessToken: 'link-token-1',
        sessionId: 'link-session-1',
        credentialOwner: 44,
      }),
      completeMFA: vi.fn().mockResolvedValue({
        kind: 'tokens',
        accessToken: 'mfa-token-1',
        sessionId: 'mfa-session-1',
        credentialOwner: 45,
      }),
    },
    openExternal: vi.fn().mockResolvedValue({ ok: true }),
  };
  // setup.ts installs `window.electron` as writable but not configurable, so we
  // assign rather than redefine. Cast through `unknown` because the test surface
  // is intentionally narrower than the full ElectronAPI.
  (window as unknown as { electron: ElectronTestSurface }).electron = surface;
  return surface;
}

let electron: ElectronTestSurface;

beforeEach(() => {
  // Auth token must be present for apiFetch to attach Authorization header.
  // The SSO start endpoint itself does not require auth, but apiFetch is shared.
  useAuthStore.getState().clearAccessToken();
  electron = installElectronMock();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('startSSOFlow', () => {
  // ── Google — main-driven flow (#975) ─────────────────────────────────────
  // Google now goes through the main process exactly like Apple (#974).
  // The renderer calls electron.sso.googleSignIn() and maps the discriminated
  // SSOSignInResult onto SSOResult — no loopback, no /callback POST.

  describe('startSSOFlow — google (#975 main-driven)', () => {
    it('maps tokens → logged_in without touching the loopback IPC trio', async () => {
      const result = await startSSOFlow('google', API_BASE);
      expect(result).toEqual({
        kind: 'logged_in',
        accessToken: 'google-at',
        sessionId: 'google-session',
        credentialOwner: 42,
      });
      expect(electron.sso.googleSignIn).toHaveBeenCalledTimes(1);
      expect(electron.sso.googleSignIn).toHaveBeenCalledWith(API_BASE);
      expect(electron.sso.startLoopback).not.toHaveBeenCalled();
      expect(electron.sso.awaitCallback).not.toHaveBeenCalled();
      expect(electron.openExternal).not.toHaveBeenCalled();
    });

    it('maps mfa_challenge → mfa_required with method passthrough', async () => {
      electron.sso.googleSignIn.mockResolvedValueOnce({
        kind: 'mfa_challenge',
        mfaChallengeToken: 'mfa-g-1',
        methods: ['totp'],
        recoveryOnlyMethods: ['backup_code'],
        webauthnOptions: { rpId: 'y' },
      });
      const result = await startSSOFlow('google', API_BASE);
      expect(result).toEqual({
        kind: 'mfa_required',
        mfaChallengeToken: 'mfa-g-1',
        methods: ['totp'],
        recoveryOnlyMethods: ['backup_code'],
        webauthnOptions: { rpId: 'y' },
      });
    });

    it('maps sso_token/new_user → register_required', async () => {
      electron.sso.googleSignIn.mockResolvedValueOnce({
        kind: 'sso_token',
        branch: 'new_user',
        ssoToken: 'tok-g-n',
        email: 'new@example.test',
        name: 'Jane Doe',
      });
      const result = await startSSOFlow('google', API_BASE);
      expect(result).toEqual({
        kind: 'register_required',
        ssoToken: 'tok-g-n',
        email: 'new@example.test',
        name: 'Jane Doe',
      });
    });

    it('maps sso_token/account_link → link_available', async () => {
      electron.sso.googleSignIn.mockResolvedValueOnce({
        kind: 'sso_token',
        branch: 'account_link',
        ssoToken: 'tok-g-l',
        maskedEmail: 'j***@example.test',
      });
      const result = await startSSOFlow('google', API_BASE);
      expect(result).toEqual({
        kind: 'link_available',
        ssoToken: 'tok-g-l',
        maskedEmail: 'j***@example.test',
      });
    });

    it('throws the stable taxonomy code on the error kind', async () => {
      electron.sso.googleSignIn.mockResolvedValueOnce({
        kind: 'error',
        code: 'google_id_token_invalid',
      });
      await expect(startSSOFlow('google', API_BASE)).rejects.toThrow('google_id_token_invalid');
    });
  });

  // ── Apple — main-driven flow (#974) ─────────────────────────────────────
  // The renderer no longer drives loopback/callback for apple: one IPC
  // invoke returns the discriminated AppleSignInResult, which maps onto the
  // existing SSOResult union. appleUserData threading is covered by the
  // main-process appleFlow suite; the server-side parse by the Go
  // AppleSession matrix.

  describe('startSSOFlow — apple (#974 main-driven)', () => {
    it('maps tokens → logged_in without touching the loopback IPC trio', async () => {
      const result = await startSSOFlow('apple', API_BASE);
      expect(result).toEqual({
        kind: 'logged_in',
        accessToken: 'apple-at',
        sessionId: 'apple-session',
        credentialOwner: 41,
      });
      expect(electron.sso.appleSignIn).toHaveBeenCalledTimes(1);
      expect(electron.sso.appleSignIn).toHaveBeenCalledWith(API_BASE);
      expect(electron.sso.startLoopback).not.toHaveBeenCalled();
      expect(electron.sso.awaitCallback).not.toHaveBeenCalled();
      expect(electron.openExternal).not.toHaveBeenCalled();
    });

    it('maps mfa_challenge → mfa_required with method passthrough', async () => {
      electron.sso.appleSignIn.mockResolvedValueOnce({
        kind: 'mfa_challenge',
        mfaChallengeToken: 'mfa-1',
        methods: ['totp'],
        recoveryOnlyMethods: ['backup_code'],
        webauthnOptions: { rpId: 'x' },
      });
      const result = await startSSOFlow('apple', API_BASE);
      expect(result).toEqual({
        kind: 'mfa_required',
        mfaChallengeToken: 'mfa-1',
        methods: ['totp'],
        recoveryOnlyMethods: ['backup_code'],
        webauthnOptions: { rpId: 'x' },
      });
    });

    it('maps sso_token/new_user → register_required', async () => {
      electron.sso.appleSignIn.mockResolvedValueOnce({
        kind: 'sso_token',
        branch: 'new_user',
        ssoToken: 'tok-n',
        email: 'new@example.test',
        name: 'Jane Doe',
      });
      const result = await startSSOFlow('apple', API_BASE);
      expect(result).toEqual({
        kind: 'register_required',
        ssoToken: 'tok-n',
        email: 'new@example.test',
        name: 'Jane Doe',
      });
    });

    it('maps sso_token/account_link → link_available', async () => {
      electron.sso.appleSignIn.mockResolvedValueOnce({
        kind: 'sso_token',
        branch: 'account_link',
        ssoToken: 'tok-l',
        maskedEmail: 'j***@example.test',
      });
      const result = await startSSOFlow('apple', API_BASE);
      expect(result).toEqual({
        kind: 'link_available',
        ssoToken: 'tok-l',
        maskedEmail: 'j***@example.test',
      });
    });

    it('throws the stable taxonomy code on the error kind', async () => {
      electron.sso.appleSignIn.mockResolvedValueOnce({
        kind: 'error',
        code: 'apple_id_token_invalid',
      });
      await expect(startSSOFlow('apple', API_BASE)).rejects.toThrow('apple_id_token_invalid');
    });
  });
});

describe('completeSSORegistration', () => {
  it('returns only the main-custodied credential reference', async () => {
    const params = {
      provider: 'google' as const,
      ssoToken: 'tok',
      username: 'newuser',
      passphrase: 'StrongPassphrase123!', // pragma: allowlist secret
      wrappedPrivateKey: 'd3JhcHBlZA==', // pragma: allowlist secret
      keyDerivationSalt: 'c2FsdA==',
      publicKey: 'cHViLWtleQ==',
    };
    const result = await completeSSORegistration(params, { apiBase: API_BASE, epoch: 0 });

    expect(result).toEqual({
      accessToken: 'reg-token-1',
      sessionId: 'reg-session-1',
      credentialOwner: 43,
    });
    expect(electron.sso.completeRegistration).toHaveBeenCalledWith(API_BASE, params);
    expect(result).not.toHaveProperty('refreshToken');
  });

  it('throws sso_complete_registration_failed_<status> on non-2xx', async () => {
    electron.sso.completeRegistration.mockResolvedValueOnce({
      kind: 'error',
      status: 409,
      code: 'username_taken',
      body: { error_code: 'username_taken' },
    });

    await expect(
      completeSSORegistration(
        {
          provider: 'google',
          ssoToken: 'tok',
          username: 'taken',
          passphrase: 'StrongPassphrase123!', // pragma: allowlist secret
          wrappedPrivateKey: 'd3JhcHBlZA==', // pragma: allowlist secret
          keyDerivationSalt: 'c2FsdA==',
          publicKey: 'cHViLWtleQ==',
        },
        { apiBase: API_BASE, epoch: 0 }
      )
    ).rejects.toThrow(/sso_complete_registration_failed_409/);
  });
});

describe('completeSSOLink', () => {
  it('returns only the main-custodied credential reference', async () => {
    const params = {
      provider: 'google' as const,
      ssoToken: 'link-tok',
      password: 'CorrectPW!', // pragma: allowlist secret
    };
    const result = await completeSSOLink(params, { apiBase: API_BASE, epoch: 0 });

    expect(result).toEqual({
      accessToken: 'link-token-1',
      sessionId: 'link-session-1',
      credentialOwner: 44,
    });
    expect(electron.sso.completeLink).toHaveBeenCalledWith(API_BASE, params);
    expect(result).not.toHaveProperty('refreshToken');
  });

  it('throws sso_complete_link_failed_<status> on non-2xx', async () => {
    electron.sso.completeLink.mockResolvedValueOnce({
      kind: 'error',
      status: 401,
      code: 'invalid_credentials',
      body: { error_code: 'invalid_credentials' },
    });

    await expect(
      completeSSOLink(
        {
          provider: 'google',
          ssoToken: 'link-tok',
          password: 'wrong', // pragma: allowlist secret
        },
        { apiBase: API_BASE, epoch: 0 }
      )
    ).rejects.toThrow(/sso_complete_link_failed_401/);
  });
});

describe('completeSSOMFA (#2424)', () => {
  it('returns only the main-custodied credential reference', async () => {
    const params = {
      provider: 'google' as const,
      mfaChallengeToken: 'mfa-chal',
      credentialOwner: 45,
      method: 'totp',
      code: '123456',
    };
    const result = await completeSSOMFA(params, { apiBase: API_BASE, epoch: 0 });

    expect(result).toEqual({
      accessToken: 'mfa-token-1',
      sessionId: 'mfa-session-1',
      credentialOwner: 45,
    });
    expect(electron.sso.completeMFA).toHaveBeenCalledWith(API_BASE, params);
    // The refresh token stays in main — never on the returned reference.
    expect(result).not.toHaveProperty('refreshToken');
  });

  it('throws sso_complete_mfa_failed_<status> on non-2xx', async () => {
    electron.sso.completeMFA.mockResolvedValueOnce({
      kind: 'error',
      status: 401,
      code: 'mfa_code_invalid',
      body: { error_code: 'mfa_code_invalid' },
    });

    await expect(
      completeSSOMFA(
        {
          provider: 'google',
          mfaChallengeToken: 'mfa-chal',
          credentialOwner: 45,
          method: 'totp',
          code: '000000',
        },
        { apiBase: API_BASE, epoch: 0 }
      )
    ).rejects.toThrow(/sso_complete_mfa_failed_401/);
  });
});

describe('abandonSSOReservation (#2394)', () => {
  // The helper must NEVER throw: it is called from the registration submit
  // path and from modal cancels, where a rejection would abort user-visible
  // work. Absence degrades to the pre-#2394 bug, never to a weaker writer.

  it('returns false when the bridge is entirely absent (older shell)', async () => {
    (window as unknown as { electron?: unknown }).electron = undefined;
    await expect(abandonSSOReservation()).resolves.toBe(false);
  });

  it('returns false when the sso namespace lacks the method', async () => {
    delete electron.sso.abandonReservation;
    await expect(abandonSSOReservation()).resolves.toBe(false);
  });

  it('swallows an IPC rejection and returns false', async () => {
    electron.sso.abandonReservation = vi.fn().mockRejectedValue(new Error('ipc down'));
    await expect(abandonSSOReservation()).resolves.toBe(false);
  });

  // Register.handleSubmit AWAITS this before doing anything else, so an
  // ipcRenderer.invoke that never settles (wedged main process) would hang the
  // submit with isSubmitting stuck true and no error surfaced.
  it('resolves false instead of hanging when the bridge never settles', async () => {
    vi.useFakeTimers();
    try {
      electron.sso.abandonReservation = vi.fn(() => new Promise<boolean>(() => {}));
      const pending = abandonSSOReservation();
      await vi.advanceTimersByTimeAsync(3000);
      await expect(pending).resolves.toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('passes the main-process boolean through', async () => {
    electron.sso.abandonReservation = vi.fn().mockResolvedValue(true);
    await expect(abandonSSOReservation()).resolves.toBe(true);

    electron.sso.abandonReservation = vi.fn().mockResolvedValue(false);
    await expect(abandonSSOReservation()).resolves.toBe(false);
  });
});
