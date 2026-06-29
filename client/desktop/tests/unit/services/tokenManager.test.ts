// @vitest-environment node
//
// tokenManager — storage/restore + proactive refresh + performRefresh tests.

import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';

const { mockGetMachineId, mockNetFetch } = vi.hoisted(() => ({
  mockGetMachineId: vi.fn(() => 'mid'),
  mockNetFetch: vi.fn(),
}));

let safeStorageAvailable = true;
const fsWriteCalls: unknown[][] = [];
const fsFiles = new Map<string, unknown>();
let fsUnlinkCount = 0;
const fsUnlinkCalls: string[] = [];
const fsRead: { impl: (...a: unknown[]) => unknown } = { impl: () => Buffer.from('x') };

vi.mock('electron', () => ({
  app: { getPath: () => '/tmp/td' },
  safeStorage: {
    isEncryptionAvailable: () => safeStorageAvailable,
    encryptString: (s: string) => Buffer.from(s),
    decryptString: (b: Buffer) => b.toString(),
  },
  net: { fetch: mockNetFetch },
}));
vi.mock('../../../src/main/machineId', () => ({ getMachineId: mockGetMachineId }));
vi.mock('fs', () => ({
  default: {
    writeFileSync: (...a: unknown[]) => {
      fsWriteCalls.push(a);
      fsFiles.set(String(a[0]), a[1]);
    },
    readFileSync: (...a: unknown[]) => fsRead.impl(...a),
    unlinkSync: (path: string) => {
      fsUnlinkCount++;
      fsUnlinkCalls.push(path);
      fsFiles.delete(path);
    },
    existsSync: (path: string) => fsFiles.has(path),
    mkdirSync: () => undefined,
  },
  writeFileSync: (...a: unknown[]) => {
    fsWriteCalls.push(a);
    fsFiles.set(String(a[0]), a[1]);
  },
  readFileSync: (...a: unknown[]) => fsRead.impl(...a),
  unlinkSync: (path: string) => {
    fsUnlinkCount++;
    fsUnlinkCalls.push(path);
    fsFiles.delete(path);
  },
  existsSync: (path: string) => fsFiles.has(path),
  mkdirSync: () => undefined,
}));

import {
  storeRefreshToken,
  restoreRefreshToken,
  clearTokens,
  storeE2EEKeys,
  restoreE2EEKeys,
  getPersistedApiBase,
  getCapabilities,
  stopProactiveRefresh,
  performRefresh,
  performLogout,
  setProactiveRefreshCallback,
  onSystemResume,
  getCachedAccessToken,
  _resetForTesting,
} from '@/main/tokenManager';

// ─── JWT Test Helper ────────────────────────────────────────────────
// Creates a minimal JWT with a given exp claim (seconds since epoch).
function makeJwt(exp: number): string {
  const header = Buffer.from(JSON.stringify({ alg: 'HS256', typ: 'JWT' })).toString('base64url');
  const payload = Buffer.from(JSON.stringify({ sub: 'user1', exp })).toString('base64url');
  const sig = 'test-sig';
  return `${header}.${payload}.${sig}`;
}

// ─── Response Factory ───────────────────────────────────────────────
function jsonResponse(data: unknown, status = 200): Response {
  return new Response(JSON.stringify(data), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('tokenManager', () => {
  beforeEach(() => {
    safeStorageAvailable = true;
    fsWriteCalls.length = 0;
    fsFiles.clear();
    fsUnlinkCount = 0;
    fsUnlinkCalls.length = 0;
    fsRead.impl = () => Buffer.from('x');
    _resetForTesting();
    mockGetMachineId.mockClear();
    mockNetFetch.mockReset();
    vi.spyOn(console, 'log').mockImplementation(() => {});
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  describe('storeRefreshToken', () => {
    it('encrypts and writes to disk when rememberMe=true', () => {
      storeRefreshToken({
        refreshToken: 'my-token',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      expect(fsWriteCalls.length).toBeGreaterThan(0);
      const paths = fsWriteCalls.map((c) => c[0] as string);
      expect(paths.some((p) => p.includes('secure-token.dat'))).toBe(true);
      expect(paths.some((p) => p.includes('token-meta.json'))).toBe(true);
    });

    it('writes self-hosted tokens under a per-origin profile namespace', () => {
      storeRefreshToken({
        refreshToken: 'my-token',
        rememberMe: true,
        apiBase: 'https://homelab.lan',
      });

      const paths = fsWriteCalls.map((c) => c[0] as string);
      expect(paths).toContainEqual(
        expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}\/secure-token\.dat$/)
      );
      expect(paths).toContainEqual(
        expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}\/token-meta\.json$/)
      );
      const metaWrite = fsWriteCalls.find((c) => String(c[0]).endsWith('token-meta.json'));
      expect(JSON.parse(metaWrite?.[1] as string)).toMatchObject({
        apiBase: 'https://homelab.lan',
        rememberMe: true,
        profileId: expect.stringMatching(/^selfhost-[0-9a-f]{16}$/),
      });
    });

    it('deletes disk files when rememberMe=false', () => {
      storeRefreshToken({
        refreshToken: 'my-token',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      expect(fsWriteCalls.length).toBe(0);
      expect(fsUnlinkCount).toBeGreaterThan(0);
    });

    it('memory-only when safeStorage unavailable', () => {
      safeStorageAvailable = false;
      storeRefreshToken({
        refreshToken: 'my-token',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      expect(fsWriteCalls.length).toBe(0);
    });
  });

  describe('restoreRefreshToken', () => {
    it('restores rememberMe=false session from main-process memory only', () => {
      storeRefreshToken({
        refreshToken: 'memory-token',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      fsRead.impl = () => {
        throw new Error('disk should not be read for memory session');
      };

      expect(restoreRefreshToken()).toEqual({
        status: 'ok',
        token: 'memory-token',
        apiBase: 'http://localhost:8080',
        rememberMe: false,
      });
      expect(fsWriteCalls.length).toBe(0);
    });

    it('returns memory API base for rememberMe=false sessions', () => {
      storeRefreshToken({
        refreshToken: 'memory-token',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      fsRead.impl = () => {
        throw new Error('disk should not be read for memory session');
      };

      expect(getPersistedApiBase()).toBe('http://localhost:8080');
    });

    it('returns unavailable when safeStorage off', () => {
      safeStorageAvailable = false;
      expect(restoreRefreshToken()).toEqual({ status: 'unavailable' });
    });

    it('returns no_session when meta file missing', () => {
      fsRead.impl = () => {
        throw new Error('ENOENT');
      };
      expect(restoreRefreshToken()).toEqual({ status: 'no_session' });
    });

    it('restores token successfully from disk', () => {
      fsRead.impl = (path) => {
        const file = String(path);
        if (file.endsWith('active-profile.json')) throw new Error('ENOENT');
        if (file.endsWith('token-meta.json')) {
          return JSON.stringify({ apiBase: 'http://localhost:8080', rememberMe: true });
        }
        return Buffer.from('stored-token');
      };
      const result = restoreRefreshToken();
      expect(result).toEqual({
        status: 'ok',
        token: 'stored-token',
        apiBase: 'http://localhost:8080',
        rememberMe: true,
      });
    });

    it('restores a remembered self-hosted token after main-process restart', () => {
      storeRefreshToken({
        refreshToken: 'self-token',
        rememberMe: true,
        apiBase: 'https://homelab.lan',
      });
      _resetForTesting();
      fsRead.impl = (path) => {
        const value = fsFiles.get(String(path));
        if (value === undefined) throw new Error('ENOENT');
        return value;
      };

      expect(restoreRefreshToken()).toEqual({
        status: 'ok',
        token: 'self-token',
        apiBase: 'https://homelab.lan',
        rememberMe: true,
      });
      expect(getPersistedApiBase()).toBe('https://homelab.lan');
    });

    it('returns tampered when decryption fails (read throws)', () => {
      fsRead.impl = (path) => {
        const file = String(path);
        if (file.endsWith('active-profile.json')) throw new Error('ENOENT');
        if (file.endsWith('token-meta.json')) {
          return JSON.stringify({ apiBase: 'http://localhost:8080', rememberMe: true });
        }
        throw new Error('read error');
      };
      expect(restoreRefreshToken()).toEqual({ status: 'tampered' });
      expect(fsUnlinkCount).toBeGreaterThan(0);
    });
  });

  describe('clearTokens', () => {
    it('clears in-memory state and deletes disk files', () => {
      storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      fsUnlinkCount = 0;
      clearTokens();
      expect(fsUnlinkCount).toBeGreaterThan(0);
    });

    it('clears the active self-hosted profile files, not the SaaS root files', () => {
      storeRefreshToken({ refreshToken: 'tk', rememberMe: true, apiBase: 'https://homelab.lan' });
      storeE2EEKeys({
        wrappingKeyBase64: 'wk',
        preferencesKeyBase64: 'pk',
        wrappedPrivateKeyBase64: 'wpk',
      });
      fsUnlinkCalls.length = 0;

      clearTokens();

      expect(fsUnlinkCalls).toContainEqual(
        expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}\/secure-token\.dat$/)
      );
      expect(fsUnlinkCalls).toContainEqual(
        expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}\/token-meta\.json$/)
      );
      expect(fsUnlinkCalls).toContainEqual(
        expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}\/secure-e2ee\.dat$/)
      );
      expect(fsUnlinkCalls).not.toContain('/tmp/td/secure-token.dat');
    });
  });

  describe('E2EE keys', () => {
    it('storeE2EEKeys encrypts and writes when rememberMe=true', () => {
      storeRefreshToken({ refreshToken: 'tk', rememberMe: true, apiBase: 'http://localhost:8080' });
      fsWriteCalls.length = 0;
      storeE2EEKeys({
        wrappingKeyBase64: 'wk',
        preferencesKeyBase64: 'pk',
        wrappedPrivateKeyBase64: 'wpk',
      });
      expect(fsWriteCalls.length).toBeGreaterThan(0);
    });

    it('writes self-hosted E2EE keys under the active profile namespace', () => {
      storeRefreshToken({ refreshToken: 'tk', rememberMe: true, apiBase: 'https://homelab.lan' });
      fsWriteCalls.length = 0;

      storeE2EEKeys({
        wrappingKeyBase64: 'wk',
        preferencesKeyBase64: 'pk',
        wrappedPrivateKeyBase64: 'wpk',
      });

      expect(fsWriteCalls.map((c) => c[0] as string)).toContainEqual(
        expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}\/secure-e2ee\.dat$/)
      );
    });

    it('storeE2EEKeys does nothing when safeStorage unavailable', () => {
      safeStorageAvailable = false;
      storeE2EEKeys({
        wrappingKeyBase64: 'k',
        preferencesKeyBase64: 'k',
        wrappedPrivateKeyBase64: 'k',
      });
      expect(fsWriteCalls.length).toBe(0);
    });

    it('storeE2EEKeys keeps keys in main-process memory (never disk) when rememberMe=false', () => {
      storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      fsWriteCalls.length = 0;
      const keys = {
        wrappingKeyBase64: 'wk',
        preferencesKeyBase64: 'pk',
        wrappedPrivateKeyBase64: 'wpk',
      };
      storeE2EEKeys(keys);
      // Session-only key material is NEVER written to disk (#1870)...
      expect(fsWriteCalls.length).toBe(0);
      // ...but it IS held in main-process memory so it survives a soft reload.
      fsRead.impl = () => {
        throw new Error('disk should not be read for memory-only E2EE keys');
      };
      expect(restoreE2EEKeys()).toEqual(keys);
    });

    it('restoreE2EEKeys prefers the in-memory copy over disk', () => {
      storeRefreshToken({ refreshToken: 'tk', rememberMe: true, apiBase: 'http://localhost:8080' });
      const memKeys = {
        wrappingKeyBase64: 'mem',
        preferencesKeyBase64: 'mem',
        wrappedPrivateKeyBase64: 'mem', // pragma: allowlist secret
      };
      storeE2EEKeys(memKeys);
      // Disk would decode to a DIFFERENT set; the memory copy must win.
      fsRead.impl = () =>
        Buffer.from(
          JSON.stringify({
            wrappingKeyBase64: 'disk',
            preferencesKeyBase64: 'disk',
            wrappedPrivateKeyBase64: 'disk', // pragma: allowlist secret
          })
        );
      expect(restoreE2EEKeys()).toEqual(memKeys);
    });

    it('clearTokens wipes the in-memory E2EE keys (no heap residue after logout)', () => {
      storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      storeE2EEKeys({
        wrappingKeyBase64: 'wk',
        preferencesKeyBase64: 'pk',
        wrappedPrivateKeyBase64: 'wpk',
      });
      // Present before clear (from memory; no disk file for a session-only user).
      fsRead.impl = () => {
        throw new Error('ENOENT');
      };
      expect(restoreE2EEKeys()).not.toBeNull();
      clearTokens();
      // Gone after clear — memory wiped, disk has nothing.
      expect(restoreE2EEKeys()).toBeNull();
    });

    it('restoreE2EEKeys decrypts and returns keys', () => {
      const data = {
        wrappingKeyBase64: 'wk',
        preferencesKeyBase64: 'pk',
        wrappedPrivateKeyBase64: 'wpk',
      };
      fsRead.impl = () => Buffer.from(JSON.stringify(data));
      expect(restoreE2EEKeys()).toEqual(data);
    });

    it('restoreE2EEKeys returns null when safeStorage unavailable', () => {
      safeStorageAvailable = false;
      expect(restoreE2EEKeys()).toBeNull();
    });

    it('restoreE2EEKeys returns null on read failure', () => {
      fsRead.impl = () => {
        throw new Error('ENOENT');
      };
      expect(restoreE2EEKeys()).toBeNull();
    });
  });

  describe('getPersistedApiBase', () => {
    it('returns apiBase from meta file', () => {
      fsRead.impl = () => JSON.stringify({ apiBase: 'http://localhost:8080', rememberMe: true });
      expect(getPersistedApiBase()).toBe('http://localhost:8080');
    });

    it('returns null when meta file does not exist', () => {
      fsRead.impl = () => {
        throw new Error('ENOENT');
      };
      expect(getPersistedApiBase()).toBeNull();
    });
  });

  describe('getCapabilities', () => {
    it('returns persistAvailable=true when safeStorage works', () => {
      safeStorageAvailable = true;
      expect(getCapabilities()).toEqual({ persistAvailable: true });
    });

    it('returns persistAvailable=false when safeStorage unavailable', () => {
      safeStorageAvailable = false;
      expect(getCapabilities()).toEqual({ persistAvailable: false });
    });
  });

  describe('stopProactiveRefresh', () => {
    it('is safe to call when no timer exists', () => {
      expect(() => stopProactiveRefresh()).not.toThrow();
    });
  });

  // ─── performRefresh ─────────────────────────────────────────────────

  describe('performRefresh', () => {
    it('returns no_token when no refresh token is stored', async () => {
      const result = await performRefresh();
      expect(result).toEqual({ status: 'no_token' });
      expect(mockNetFetch).not.toHaveBeenCalled();
    });

    it('makes POST with correct headers on success', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const jwt = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt, session_id: 'sid1' }));

      const result = await performRefresh();

      expect(result).toEqual({ status: 'ok', accessToken: jwt, sessionId: 'sid1' });
      expect(mockNetFetch).toHaveBeenCalledOnce();
      const [url, opts] = mockNetFetch.mock.calls[0];
      expect(url).toBe('http://localhost:8080/api/v1/auth/refresh');
      expect(opts.method).toBe('POST');
      expect(opts.headers['X-Refresh-Token']).toBe('rt-abc');
      expect(opts.headers['X-Machine-Id']).toBe('mid');
      expect(opts.credentials).toBe('omit');
      expect(mockGetMachineId).toHaveBeenCalledWith('http://localhost:8080');
    });

    it('returns refresh_failed on non-ok HTTP status', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(new Response('Unauthorized', { status: 401 }));

      const result = await performRefresh();
      expect(result).toEqual({ status: 'refresh_failed' });
    });

    it('returns mfa_required on 403 with MFA challenge', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse(
          {
            error: 'suspicious_session_mfa',
            mfa_challenge_token: 'chal-tok',
            methods: ['totp'],
            recovery_only_methods: ['recovery_code'],
          },
          403
        )
      );

      const result = await performRefresh();
      expect(result).toEqual({
        status: 'mfa_required',
        mfaChallengeToken: 'chal-tok',
        mfaMethods: ['totp'],
        mfaRecoveryOnlyMethods: ['recovery_code'],
      });
    });

    it('returns refresh_failed on 403 without MFA data', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ error: 'forbidden' }, 403));

      const result = await performRefresh();
      expect(result).toEqual({ status: 'refresh_failed' });
    });

    it('returns refresh_failed when response missing access_token', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ session_id: 'sid1' }));

      const result = await performRefresh();
      expect(result).toEqual({ status: 'refresh_failed' });
    });

    it('returns refresh_failed on network error', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockRejectedValueOnce(new Error('network down'));

      const result = await performRefresh();
      expect(result).toEqual({ status: 'refresh_failed' });
    });

    it('deduplicates concurrent calls (single-flight)', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const jwt = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt }));

      const [r1, r2, r3] = await Promise.all([
        performRefresh(),
        performRefresh(),
        performRefresh(),
      ]);

      expect(mockNetFetch).toHaveBeenCalledOnce();
      expect(r1).toEqual(r2);
      expect(r2).toEqual(r3);
    });

    it('rotates refresh token and persists to disk when rememberMe=true', async () => {
      storeRefreshToken({
        refreshToken: 'rt-old',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      fsWriteCalls.length = 0; // clear storeRefreshToken writes
      const jwt = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse({ access_token: jwt, refresh_token: 'rt-new' })
      );

      const result = await performRefresh();
      expect(result.status).toBe('ok');
      // Should have written the rotated token to disk
      const tokenWrites = fsWriteCalls.filter((c) => (c[0] as string).includes('secure-token.dat'));
      expect(tokenWrites.length).toBeGreaterThan(0);
    });

    it('does not persist rotated token when rememberMe=false', async () => {
      storeRefreshToken({
        refreshToken: 'rt-old',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      fsWriteCalls.length = 0;
      const jwt = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse({ access_token: jwt, refresh_token: 'rt-new' })
      );

      await performRefresh();
      const tokenWrites = fsWriteCalls.filter((c) => (c[0] as string).includes('secure-token.dat'));
      expect(tokenWrites.length).toBe(0);
    });

    it('handles mfa_upgrade_required variant', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse(
          {
            error: 'mfa_upgrade_required',
            mfa_challenge_token: 'chal2',
            methods: ['webauthn'],
          },
          403
        )
      );

      const result = await performRefresh();
      expect(result.status).toBe('mfa_required');
      expect(result.mfaChallengeToken).toBe('chal2');
      expect(result.mfaMethods).toEqual(['webauthn']);
      expect(result.mfaRecoveryOnlyMethods).toEqual([]);
    });
  });

  // ─── Proactive Refresh (timer scheduling via performRefresh) ──────

  describe('proactive refresh scheduling', () => {
    it('schedules proactive timer after successful refresh', async () => {
      vi.useFakeTimers();
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      // First refresh — returns JWT expiring in 900s (15min)
      const exp = Math.floor(Date.now() / 1000) + 900;
      const jwt = makeJwt(exp);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt }));
      await performRefresh();

      // Register callback to capture proactive refresh
      const cb = vi.fn();
      setProactiveRefreshCallback(cb);

      // Second refresh (proactive) — triggered by timer at ~840s
      const jwt2 = makeJwt(Math.floor(Date.now() / 1000) + 900 + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt2 }));

      // Advance to just before the 60s buffer (840s = 900-60)
      await vi.advanceTimersByTimeAsync(839_000);
      expect(cb).not.toHaveBeenCalled();

      // Advance past the trigger point
      await vi.advanceTimersByTimeAsync(2_000);
      expect(cb).toHaveBeenCalledWith(jwt2, undefined);
    });

    it('calls proactive callback with sessionId when present', async () => {
      vi.useFakeTimers();
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      const exp = Math.floor(Date.now() / 1000) + 120; // expires in 2min
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: makeJwt(exp) }));
      await performRefresh();

      const cb = vi.fn();
      setProactiveRefreshCallback(cb);

      // Proactive fires at 60s (120 - 60 buffer)
      const jwt2 = makeJwt(Math.floor(Date.now() / 1000) + 1000);
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse({ access_token: jwt2, session_id: 'sid-new' })
      );

      await vi.advanceTimersByTimeAsync(61_000);
      expect(cb).toHaveBeenCalledWith(jwt2, 'sid-new');
    });

    it('retries proactive refresh after failure', async () => {
      vi.useFakeTimers();
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      const exp = Math.floor(Date.now() / 1000) + 120;
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: makeJwt(exp) }));
      await performRefresh();

      const cb = vi.fn();
      setProactiveRefreshCallback(cb);

      // Proactive fires but fails
      mockNetFetch.mockResolvedValueOnce(new Response('Server Error', { status: 500 }));
      await vi.advanceTimersByTimeAsync(61_000);
      expect(cb).not.toHaveBeenCalled();

      // Retry after 10s cooldown should succeed
      const jwt3 = makeJwt(Math.floor(Date.now() / 1000) + 1000);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt3 }));
      await vi.advanceTimersByTimeAsync(11_000);
      expect(cb).toHaveBeenCalledWith(jwt3, undefined);
    });

    it('rate-limits immediate refresh for near-expiry tokens', async () => {
      vi.useFakeTimers();
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      // Token that expires in 30s (within the 60s buffer → immediate refresh)
      const exp = Math.floor(Date.now() / 1000) + 30;
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: makeJwt(exp) }));
      await performRefresh(); // This triggers scheduleProactiveRefresh with delay ≤ 0

      // Second immediate refresh would be rate-limited
      const cb = vi.fn();
      setProactiveRefreshCallback(cb);

      // The immediate proactive fires right away (but is rate-limited since
      // performRefresh just ran — within 10s window), so it schedules after cooldown
      const jwt2 = makeJwt(Math.floor(Date.now() / 1000) + 1000);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt2 }));

      await vi.advanceTimersByTimeAsync(11_000);
      expect(cb).toHaveBeenCalled();
    });
  });

  // ─── onSystemResume ─────────────────────────────────────────────────

  describe('onSystemResume', () => {
    it('is a no-op when no credentials are stored', async () => {
      onSystemResume();
      expect(mockNetFetch).not.toHaveBeenCalled();
    });

    it('triggers immediate refresh on wake', async () => {
      vi.useFakeTimers();
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      const cb = vi.fn();
      setProactiveRefreshCallback(cb);

      const jwt = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt }));

      onSystemResume();
      // Let the async doProactiveRefresh() complete
      await vi.advanceTimersByTimeAsync(0);

      expect(mockNetFetch).toHaveBeenCalledOnce();
      expect(cb).toHaveBeenCalledWith(jwt, undefined);
    });

    it('rate-limits refresh if recently refreshed', async () => {
      vi.useFakeTimers();
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      // Do a normal refresh first (sets lastProactiveRefreshTimestamp)
      const jwt1 = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt1 }));

      // Trigger the proactive path to set lastProactiveRefreshTimestamp
      const cb = vi.fn();
      setProactiveRefreshCallback(cb);

      // Simulate: advance to trigger the proactive timer, which sets the timestamp
      // Instead, call onSystemResume to set the timestamp via doProactiveRefresh
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt1 }));
      onSystemResume();
      await vi.advanceTimersByTimeAsync(0);
      expect(cb).toHaveBeenCalledTimes(1);
      cb.mockClear();
      mockNetFetch.mockClear();

      // Call onSystemResume again immediately — should be rate-limited
      const jwt2 = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt2 }));
      onSystemResume();

      // Should NOT have fired immediately
      await vi.advanceTimersByTimeAsync(0);
      expect(mockNetFetch).not.toHaveBeenCalled();

      // Advance past rate-limit cooldown (10s)
      await vi.advanceTimersByTimeAsync(11_000);
      expect(cb).toHaveBeenCalledWith(jwt2, undefined);
    });
  });

  // ─── performLogout ──────────────────────────────────────────────────

  describe('performLogout', () => {
    it('calls server logout endpoint with correct headers', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(new Response('', { status: 200 }));

      await performLogout('access-tok');

      expect(mockNetFetch).toHaveBeenCalledOnce();
      const [url, opts] = mockNetFetch.mock.calls[0];
      expect(url).toBe('http://localhost:8080/api/v1/auth/logout');
      expect(opts.method).toBe('POST');
      expect(opts.headers['Authorization']).toBe('Bearer access-tok');
      expect(opts.headers['X-Refresh-Token']).toBe('rt-abc');
    });

    it('clears tokens even if logout HTTP call fails', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      fsUnlinkCount = 0;
      mockNetFetch.mockRejectedValueOnce(new Error('network down'));

      await performLogout('access-tok');

      // clearTokens should still have been called (disk files deleted)
      expect(fsUnlinkCount).toBeGreaterThan(0);
    });

    it('is a no-op when no apiBase is set', async () => {
      await performLogout('access-tok');
      expect(mockNetFetch).not.toHaveBeenCalled();
    });

    it('omits Authorization header when no accessToken provided', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(new Response('', { status: 200 }));

      await performLogout();

      const [, opts] = mockNetFetch.mock.calls[0];
      expect(opts.headers['Authorization']).toBeUndefined();
      expect(opts.headers['X-Refresh-Token']).toBe('rt-abc');
    });
  });

  // ─── getCachedAccessToken (#626) ──────────────────────────────────

  describe('getCachedAccessToken', () => {
    it('returns null before any refresh', () => {
      expect(getCachedAccessToken()).toBeNull();
    });

    it('returns access token after successful performRefresh', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const jwt = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt }));

      await performRefresh();

      expect(getCachedAccessToken()).toBe(jwt);
    });

    it('returns null after clearTokens', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const jwt = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt }));
      await performRefresh();

      clearTokens();

      expect(getCachedAccessToken()).toBeNull();
    });

    it('returns null after _resetForTesting', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const jwt = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt }));
      await performRefresh();

      _resetForTesting();

      expect(getCachedAccessToken()).toBeNull();
    });

    it('does not cache token on failed refresh', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(new Response('Unauthorized', { status: 401 }));

      await performRefresh();

      expect(getCachedAccessToken()).toBeNull();
    });

    it('caches token passed via storeRefreshToken accessToken field', () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
        accessToken: 'initial-jwt',
      });

      expect(getCachedAccessToken()).toBe('initial-jwt');
    });

    it('does not cache when accessToken field is omitted', () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      expect(getCachedAccessToken()).toBeNull();
    });

    it('clears stale token when storeRefreshToken called without accessToken', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
        accessToken: 'old-jwt',
      });
      expect(getCachedAccessToken()).toBe('old-jwt');

      storeRefreshToken({
        refreshToken: 'rt-def',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      expect(getCachedAccessToken()).toBeNull();
    });

    it('updates cached token on successive refreshes', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const jwt1 = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt1 }));
      await performRefresh();
      expect(getCachedAccessToken()).toBe(jwt1);

      const jwt2 = makeJwt(Math.floor(Date.now() / 1000) + 1800);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: jwt2 }));
      await performRefresh();
      expect(getCachedAccessToken()).toBe(jwt2);
    });

    it('clears cached token via performLogout', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
        accessToken: 'my-jwt',
      });
      expect(getCachedAccessToken()).toBe('my-jwt');

      mockNetFetch.mockResolvedValueOnce(new Response('', { status: 200 }));
      await performLogout('my-jwt');
      expect(getCachedAccessToken()).toBeNull();
    });
  });
});
