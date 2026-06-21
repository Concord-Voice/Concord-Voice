import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import {
  startSSOFlow,
  completeSSORegistration,
  completeSSOLink,
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
      appleSignIn: vi.fn().mockResolvedValue({ kind: 'tokens', accessToken: 'apple-at' }),
      appleCancel: vi.fn(),
      googleSignIn: vi.fn().mockResolvedValue({ kind: 'tokens', accessToken: 'google-at' }),
      googleCancel: vi.fn(),
    },
    openExternal: vi.fn().mockResolvedValue({ ok: true }),
  };
  // setup.ts installs `window.electron` as writable but not configurable, so we
  // assign rather than redefine. Cast through `unknown` because the test surface
  // is intentionally narrower than the full ElectronAPI.
  (window as unknown as { electron: ElectronTestSurface }).electron = surface;
  return surface;
}

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());

let electron: ElectronTestSurface;

beforeEach(() => {
  // The shared MSW server has handlers for /api/v1/auth/register etc. but no
  // SSO routes — we register them per-test via server.use().
  server.resetHandlers();
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
      const result = await startSSOFlow('google');
      expect(result).toEqual({ kind: 'logged_in', accessToken: 'google-at' });
      expect(electron.sso.googleSignIn).toHaveBeenCalledTimes(1);
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
      const result = await startSSOFlow('google');
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
      const result = await startSSOFlow('google');
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
      const result = await startSSOFlow('google');
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
      await expect(startSSOFlow('google')).rejects.toThrow('google_id_token_invalid');
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
      const result = await startSSOFlow('apple');
      expect(result).toEqual({ kind: 'logged_in', accessToken: 'apple-at' });
      expect(electron.sso.appleSignIn).toHaveBeenCalledTimes(1);
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
      const result = await startSSOFlow('apple');
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
      const result = await startSSOFlow('apple');
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
      const result = await startSSOFlow('apple');
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
      await expect(startSSOFlow('apple')).rejects.toThrow('apple_id_token_invalid');
    });
  });
});

describe('completeSSORegistration', () => {
  it('returns an access token on 2xx', async () => {
    server.use(
      http.post(`${API_BASE}/api/v1/auth/sso/google/complete-registration`, () =>
        HttpResponse.json({ access_token: 'reg-token-1' }, { status: 201 })
      )
    );

    const { accessToken } = await completeSSORegistration({
      provider: 'google',
      ssoToken: 'tok',
      username: 'newuser',
      passphrase: 'StrongPassphrase123!', // pragma: allowlist secret
      wrappedPrivateKey: 'd3JhcHBlZA==', // pragma: allowlist secret
      keyDerivationSalt: 'c2FsdA==',
      publicKey: 'cHViLWtleQ==',
    });

    expect(accessToken).toBe('reg-token-1');
  });

  it('throws sso_complete_registration_failed_<status> on non-2xx', async () => {
    server.use(
      http.post(`${API_BASE}/api/v1/auth/sso/google/complete-registration`, () =>
        HttpResponse.json({ error_code: 'username_taken' }, { status: 409 })
      )
    );

    await expect(
      completeSSORegistration({
        provider: 'google',
        ssoToken: 'tok',
        username: 'taken',
        passphrase: 'StrongPassphrase123!', // pragma: allowlist secret
        wrappedPrivateKey: 'd3JhcHBlZA==', // pragma: allowlist secret
        keyDerivationSalt: 'c2FsdA==',
        publicKey: 'cHViLWtleQ==',
      })
    ).rejects.toThrow(/sso_complete_registration_failed_409/);
  });
});

describe('completeSSOLink', () => {
  it('returns an access token on 2xx', async () => {
    server.use(
      http.post(`${API_BASE}/api/v1/auth/sso/google/complete-link`, () =>
        HttpResponse.json({ access_token: 'link-token-1' })
      )
    );

    const { accessToken } = await completeSSOLink({
      provider: 'google',
      ssoToken: 'link-tok',
      password: 'CorrectPW!', // pragma: allowlist secret
    });

    expect(accessToken).toBe('link-token-1');
  });

  it('throws sso_complete_link_failed_<status> on non-2xx', async () => {
    server.use(
      http.post(`${API_BASE}/api/v1/auth/sso/google/complete-link`, () =>
        HttpResponse.json({ error_code: 'invalid_credentials' }, { status: 401 })
      )
    );

    await expect(
      completeSSOLink({
        provider: 'google',
        ssoToken: 'link-tok',
        password: 'wrong', // pragma: allowlist secret
      })
    ).rejects.toThrow(/sso_complete_link_failed_401/);
  });
});
