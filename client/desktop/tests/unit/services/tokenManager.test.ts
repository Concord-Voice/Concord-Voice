// @vitest-environment node
//
// tokenManager — storage/restore + proactive refresh + performRefresh tests.

import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';

const { mockGetMachineId, mockNetFetch } = vi.hoisted(() => ({
  mockGetMachineId: vi.fn(() => 'mid'),
  mockNetFetch: vi.fn(),
}));

let safeStorageAvailable = true;
let storageBackend = 'gnome_libsecret';
// Tag prepended to "ciphertext". 'v11' is a keyring key; 'v10' on Linux is
// Chromium's hardcoded-key fallback, which secureStorage.ts refuses.
let ciphertextTag = 'v11';
const fsWriteCalls: unknown[][] = [];
const fsFiles = new Map<string, unknown>();
let fsUnlinkCount = 0;
const fsUnlinkCalls: string[] = [];
const fsRead: { impl: (...a: unknown[]) => unknown } = { impl: () => Buffer.from('x') };
const fsRenameCalls: [string, string][] = [];
const fsRmCalls: string[] = [];
const fsRename = { fail: false };

// The atomic write lands on a temp path and is renamed into place. Report the
// write under its final path, as a direct write would have, so assertions on
// fsWriteCalls keep reading destinations.
function mockRename(from: string, to: string): void {
  fsRenameCalls.push([from, to]);
  if (fsRename.fail || !fsFiles.has(from)) throw new Error('EXDEV');
  fsFiles.set(to, fsFiles.get(from));
  fsFiles.delete(from);
  for (const call of fsWriteCalls) if (call[0] === from) call[0] = to;
}

function mockRm(path: string): void {
  fsRmCalls.push(path);
  fsFiles.delete(path);
}

vi.mock('electron', () => ({
  app: { getPath: () => '/tmp/td' },
  safeStorage: {
    isAsyncEncryptionAvailable: async () => safeStorageAvailable,
    getSelectedStorageBackend: () => storageBackend,
    encryptStringAsync: async (s: string) => Buffer.from(`${ciphertextTag}${s}`),
    decryptStringAsync: async (b: Buffer) => ({
      shouldReEncrypt: false,
      result: b.toString().replace(/^v1[01]/, ''),
    }),
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
    renameSync: (from: string, to: string) => mockRename(from, to),
    rmSync: (path: string) => mockRm(path),
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
  renameSync: (from: string, to: string) => mockRename(from, to),
  rmSync: (path: string) => mockRm(path),
}));

import {
  storeRefreshToken,
  restoreRefreshToken,
  clearTokens,
  clearTokensIfOwner,
  releaseCredentialReservation,
  reserveCredentialOwner,
  credentialOwnerIsCurrent,
  storeRefreshTokenIfOwner,
  storeE2EEKeys,
  storeE2EEKeysIfOwner,
  restoreE2EEKeys,
  getCredentialCustodyState,
  getPersistedApiBase,
  getCapabilities,
  stopProactiveRefresh,
  performRefresh,
  performLogout,
  setProactiveRefreshCallback,
  onSystemResume,
  getCachedAccessToken,
  _resetForTesting,
  _flushDiskWritesForTesting,
} from '@/main/tokenManager';
// Mocked via vi.mock('electron') above — imported here so tests can spy the
// async encrypt/decrypt to simulate a locked keychain or hold one in flight.
import { safeStorage } from 'electron';
import fs from 'node:fs';

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

const flushDiskWrites = () => _flushDiskWritesForTesting();

const originalPlatform = process.platform;
function setPlatform(platform: NodeJS.Platform) {
  Object.defineProperty(process, 'platform', { value: platform, configurable: true });
}

/** A promise the test settles by hand, to hold an encrypt/decrypt in flight. */
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** Writes that carry credential material (the active-profile pointer holds none). */
function credentialWrites(): string[] {
  return fsWriteCalls
    .map(([path]) => String(path))
    .filter((p) => !p.endsWith('active-profile.json'));
}

function diskFile(suffix: string): unknown {
  return [...fsFiles.entries()].find(([path]) => path.endsWith(suffix))?.[1];
}

function readMockDisk(path: unknown): unknown {
  const value = fsFiles.get(String(path));
  if (value === undefined) throw new Error('ENOENT');
  return value;
}

describe('tokenManager', () => {
  beforeEach(() => {
    safeStorageAvailable = true;
    storageBackend = 'gnome_libsecret';
    ciphertextTag = 'v11';
    fsWriteCalls.length = 0;
    fsFiles.clear();
    fsUnlinkCount = 0;
    fsUnlinkCalls.length = 0;
    fsRenameCalls.length = 0;
    fsRmCalls.length = 0;
    fsRename.fail = false;
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
    setPlatform(originalPlatform);
  });

  describe('storeRefreshToken', () => {
    it('returns a distinct opaque owner for each stored lifecycle', () => {
      const firstOwner = storeRefreshToken({
        refreshToken: 'first-token',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const secondOwner = storeRefreshToken({
        refreshToken: 'second-token',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      expect(Number.isSafeInteger(firstOwner)).toBe(true);
      expect(secondOwner).not.toBe(firstOwner);
    });

    it('encrypts and writes to disk when rememberMe=true', async () => {
      storeRefreshToken({
        refreshToken: 'my-token',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      await flushDiskWrites();
      expect(String(diskFile('secure-token.dat'))).toBe('v11my-token');
      expect(fsWriteCalls.length).toBeGreaterThan(0);
      const paths = fsWriteCalls.map((c) => c[0] as string);
      expect(paths.some((p) => p.includes('secure-token.dat'))).toBe(true);
      expect(paths.some((p) => p.includes('token-meta.json'))).toBe(true);
    });

    it('writes self-hosted tokens under a per-origin profile namespace', async () => {
      storeRefreshToken({
        refreshToken: 'my-token',
        rememberMe: true,
        apiBase: 'https://homelab.lan',
      });
      await flushDiskWrites();

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

    it('memory-only when safeStorage unavailable', async () => {
      safeStorageAvailable = false;
      storeRefreshToken({
        refreshToken: 'my-token',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      await flushDiskWrites();
      expect(credentialWrites()).toEqual([]);
      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok', token: 'my-token' });
    });

    it('removes partial successor metadata when the meta write fails midway', async () => {
      fsRead.impl = readMockDisk;
      storeRefreshToken({
        refreshToken: 'predecessor-token',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      await flushDiskWrites();
      const originalWrite = fs.writeFileSync;
      // token-meta.json lands, then the active-profile pointer write fails.
      vi.spyOn(fs, 'writeFileSync').mockImplementation((file, ...args) => {
        if (String(file).includes('active-profile.json')) throw new Error('disk full');
        return originalWrite(file, ...args);
      });

      storeRefreshToken({
        refreshToken: 'successor-token',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      await flushDiskWrites();

      expect(diskFile('token-meta.json')).toBeUndefined();
      expect(diskFile('secure-token.dat')).toBeUndefined();
      expect(await restoreRefreshToken()).toMatchObject({
        status: 'ok',
        token: 'successor-token',
      });
    });

    it('removes partial successor state when the encrypted token write fails', async () => {
      fsRead.impl = readMockDisk;
      storeRefreshToken({
        refreshToken: 'predecessor-token',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      await flushDiskWrites();
      fsUnlinkCalls.length = 0;
      const originalWrite = fs.writeFileSync;
      vi.spyOn(fs, 'writeFileSync').mockImplementation((file, ...args) => {
        if (String(file).includes('secure-token.dat')) throw new Error('disk full');
        return originalWrite(file, ...args);
      });

      storeRefreshToken({
        refreshToken: 'successor-token',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      await flushDiskWrites();

      // The successor's meta was written before the token write failed; only
      // the failure branch removes it.
      expect(diskFile('token-meta.json')).toBeUndefined();
      expect(diskFile('secure-token.dat')).toBeUndefined();
      expect(await restoreRefreshToken()).toMatchObject({
        status: 'ok',
        token: 'successor-token',
      });
    });
  });

  describe('restoreRefreshToken', () => {
    it('restores rememberMe=false session from main-process memory only', async () => {
      storeRefreshToken({
        refreshToken: 'memory-token',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      fsRead.impl = () => {
        throw new Error('disk should not be read for memory session');
      };

      expect(await restoreRefreshToken()).toEqual({
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

    it('returns unavailable when safeStorage off', async () => {
      safeStorageAvailable = false;
      expect(await restoreRefreshToken()).toEqual({ status: 'unavailable' });
    });

    it('returns no_session when meta file missing', async () => {
      fsRead.impl = () => {
        throw new Error('ENOENT');
      };
      expect(await restoreRefreshToken()).toEqual({ status: 'no_session' });
    });

    it('restores token successfully from disk', async () => {
      fsRead.impl = (path) => {
        const file = String(path);
        if (file.endsWith('active-profile.json')) throw new Error('ENOENT');
        if (file.endsWith('token-meta.json')) {
          return JSON.stringify({ apiBase: 'http://localhost:8080', rememberMe: true });
        }
        return Buffer.from('v11stored-token');
      };
      const result = await restoreRefreshToken();
      expect(result).toEqual({
        status: 'ok',
        token: 'stored-token',
        apiBase: 'http://localhost:8080',
        rememberMe: true,
      });
    });

    it('restores a remembered self-hosted token after main-process restart', async () => {
      storeRefreshToken({
        refreshToken: 'self-token',
        rememberMe: true,
        apiBase: 'https://homelab.lan',
      });
      await flushDiskWrites();
      _resetForTesting();
      fsRead.impl = (path) => {
        const value = fsFiles.get(String(path));
        if (value === undefined) throw new Error('ENOENT');
        return value;
      };

      expect(await restoreRefreshToken()).toEqual({
        status: 'ok',
        token: 'self-token',
        apiBase: 'https://homelab.lan',
        rememberMe: true,
      });
      expect(getPersistedApiBase()).toBe('https://homelab.lan');
    });

    it('returns tampered when decryption fails (read throws)', async () => {
      fsRead.impl = (path) => {
        const file = String(path);
        if (file.endsWith('active-profile.json')) throw new Error('ENOENT');
        if (file.endsWith('token-meta.json')) {
          return JSON.stringify({ apiBase: 'http://localhost:8080', rememberMe: true });
        }
        throw new Error('read error');
      };
      expect(await restoreRefreshToken()).toEqual({ status: 'tampered' });
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

    it('clears the active self-hosted profile files, not the SaaS root files', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: true,
        apiBase: 'https://homelab.lan',
      });
      await storeE2EEKeysIfOwner(
        {
          wrappingKeyBase64: 'wk',
          preferencesKeyBase64: 'pk',
          wrappedPrivateKeyBase64: 'wpk',
        },
        owner
      );
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

    it('clearTokensIfOwner preserves a successor lifecycle', async () => {
      const staleOwner = storeRefreshToken({
        refreshToken: 'rt-old',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const successorOwner = storeRefreshToken({
        refreshToken: 'rt-successor',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      expect(clearTokensIfOwner(staleOwner)).toBe(false);
      expect(await restoreRefreshToken()).toEqual({
        status: 'ok',
        token: 'rt-successor',
        apiBase: 'http://localhost:8080',
        rememberMe: false,
      });
      expect(clearTokensIfOwner(successorOwner)).toBe(true);
      expect(await restoreRefreshToken()).toEqual({ status: 'no_session' });
    });

    it('keeps the same owner across a successful refresh-token rotation', async () => {
      const owner = storeRefreshToken({
        refreshToken: 'rt-old',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse({
          access_token: makeJwt(Math.floor(Date.now() / 1000) + 900),
          refresh_token: 'rt-rotated',
        })
      );

      await expect(performRefresh()).resolves.toMatchObject({ status: 'ok' });
      expect(
        await storeE2EEKeysIfOwner(
          {
            wrappingKeyBase64: 'wk',
            preferencesKeyBase64: 'pk',
            wrappedPrivateKeyBase64: 'wpk',
          },
          owner
        )
      ).toBe(true);
      expect(clearTokensIfOwner(owner)).toBe(true);
      expect(await restoreRefreshToken()).toEqual({ status: 'no_session' });
    });

    it('lets password credentials and keys win while a reserved SSO exchange waits', async () => {
      const ssoOwner = reserveCredentialOwner('http://localhost:8080');
      const passwordOwner = storeRefreshToken({
        refreshToken: 'rt-password',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
        accessToken: 'at-password',
      });
      const passwordKeys = {
        wrappingKeyBase64: 'password-wk',
        preferencesKeyBase64: 'password-pk',
        wrappedPrivateKeyBase64: 'password-wpk', // pragma: allowlist secret
      };
      expect(await storeE2EEKeysIfOwner(passwordKeys, passwordOwner)).toBe(true);

      expect(
        storeRefreshTokenIfOwner(
          {
            refreshToken: 'rt-stale-sso',
            rememberMe: true,
            apiBase: 'http://localhost:8080',
            accessToken: 'at-stale-sso',
          },
          ssoOwner
        )
      ).toBeNull();
      expect(await restoreRefreshToken()).toMatchObject({ token: 'rt-password' });
      expect(await restoreE2EEKeys()).toEqual(passwordKeys);
      expect(getCredentialCustodyState()).toEqual({
        credentialOwner: passwordOwner,
        pendingE2EEUnlock: false,
      });
    });
  });

  describe('E2EE keys', () => {
    it('storeE2EEKeysIfOwner does not overwrite successor-owned key custody', async () => {
      const staleOwner = storeRefreshToken({
        refreshToken: 'rt-old',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const successorOwner = storeRefreshToken({
        refreshToken: 'rt-successor',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const staleKeys = {
        wrappingKeyBase64: 'stale-wk',
        preferencesKeyBase64: 'stale-pk',
        wrappedPrivateKeyBase64: 'stale-wpk', // pragma: allowlist secret
      };
      const successorKeys = {
        wrappingKeyBase64: 'successor-wk',
        preferencesKeyBase64: 'successor-pk',
        wrappedPrivateKeyBase64: 'successor-wpk', // pragma: allowlist secret
      };

      expect(await storeE2EEKeysIfOwner(staleKeys, staleOwner)).toBe(false);
      expect(await restoreE2EEKeys()).toBeNull();
      expect(await storeE2EEKeysIfOwner(successorKeys, successorOwner)).toBe(true);
      expect(await restoreE2EEKeys()).toEqual(successorKeys);
    });

    it('storeE2EEKeysIfOwner encrypts and writes when rememberMe=true', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      await flushDiskWrites();
      fsWriteCalls.length = 0;
      await storeE2EEKeysIfOwner(
        {
          wrappingKeyBase64: 'wk',
          preferencesKeyBase64: 'pk',
          wrappedPrivateKeyBase64: 'wpk',
        },
        owner
      );
      expect(fsWriteCalls.length).toBeGreaterThan(0);
    });

    it('writes self-hosted E2EE keys under the active profile namespace', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: true,
        apiBase: 'https://homelab.lan',
      });
      await flushDiskWrites();
      fsWriteCalls.length = 0;

      await storeE2EEKeysIfOwner(
        {
          wrappingKeyBase64: 'wk',
          preferencesKeyBase64: 'pk',
          wrappedPrivateKeyBase64: 'wpk',
        },
        owner
      );

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

    it('storeE2EEKeys keeps keys in main-process memory (never disk) when rememberMe=false', async () => {
      const owner = storeRefreshToken({
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
      await storeE2EEKeysIfOwner(keys, owner);
      // Session-only key material is NEVER written to disk (#1870)...
      expect(fsWriteCalls.length).toBe(0);
      // ...but it IS held in main-process memory so it survives a soft reload.
      fsRead.impl = () => {
        throw new Error('disk should not be read for memory-only E2EE keys');
      };
      expect(await restoreE2EEKeys()).toEqual(keys);
    });

    it('restoreE2EEKeys prefers the in-memory copy over disk', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      const memKeys = {
        wrappingKeyBase64: 'mem',
        preferencesKeyBase64: 'mem',
        wrappedPrivateKeyBase64: 'mem', // pragma: allowlist secret
      };
      await storeE2EEKeysIfOwner(memKeys, owner);
      // Disk would decode to a DIFFERENT set; the memory copy must win.
      fsRead.impl = () =>
        Buffer.from(
          JSON.stringify({
            wrappingKeyBase64: 'disk',
            preferencesKeyBase64: 'disk',
            wrappedPrivateKeyBase64: 'disk', // pragma: allowlist secret
          })
        );
      expect(await restoreE2EEKeys()).toEqual(memKeys);
    });

    it('clearTokens wipes the in-memory E2EE keys (no heap residue after logout)', async () => {
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      await storeE2EEKeysIfOwner(
        {
          wrappingKeyBase64: 'wk',
          preferencesKeyBase64: 'pk',
          wrappedPrivateKeyBase64: 'wpk',
        },
        owner
      );
      // Present before clear (from memory; no disk file for a session-only user).
      fsRead.impl = () => {
        throw new Error('ENOENT');
      };
      expect(await restoreE2EEKeys()).not.toBeNull();
      clearTokens();
      // Gone after clear — memory wiped, disk has nothing.
      expect(await restoreE2EEKeys()).toBeNull();
    });

    it('restores only an owner-matched E2EE blob after a process restart', async () => {
      const data = {
        wrappingKeyBase64: 'wk',
        preferencesKeyBase64: 'pk',
        wrappedPrivateKeyBase64: 'wpk',
      };
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      expect(await storeE2EEKeysIfOwner(data, owner)).toBe(true);

      _resetForTesting();
      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok', token: 'tk' });
      expect(await restoreE2EEKeys()).toEqual(data);
      expect(getCredentialCustodyState()).toMatchObject({ pendingE2EEUnlock: false });
    });

    it('does not restore predecessor keys in the new-credential crash window', async () => {
      fsRead.impl = readMockDisk;
      const predecessorOwner = storeRefreshToken({
        refreshToken: 'rt-predecessor',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      expect(
        await storeE2EEKeysIfOwner(
          {
            wrappingKeyBase64: 'predecessor-wk',
            preferencesKeyBase64: 'predecessor-pk',
            wrappedPrivateKeyBase64: 'predecessor-wpk', // pragma: allowlist secret
          },
          predecessorOwner
        )
      ).toBe(true);

      storeRefreshToken({
        refreshToken: 'rt-successor',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      // Crash after the successor token landed but before its key write.
      await flushDiskWrites();
      _resetForTesting();

      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok', token: 'rt-successor' });
      expect(await restoreE2EEKeys()).toBeNull();
      expect(getCredentialCustodyState()).toMatchObject({ pendingE2EEUnlock: true });
    });

    it('keeps a new owner pending across a soft reload until its matching key write', async () => {
      const owner = storeRefreshToken({
        refreshToken: 'rt-successor',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });

      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok', token: 'rt-successor' });
      expect(await restoreE2EEKeys()).toBeNull();
      expect(getCredentialCustodyState()).toEqual({
        credentialOwner: owner,
        pendingE2EEUnlock: true,
      });

      expect(
        await storeE2EEKeysIfOwner(
          {
            wrappingKeyBase64: 'successor-wk',
            preferencesKeyBase64: 'successor-pk',
            wrappedPrivateKeyBase64: 'successor-wpk', // pragma: allowlist secret
          },
          owner
        )
      ).toBe(true);
      expect(getCredentialCustodyState()).toMatchObject({ pendingE2EEUnlock: false });
    });

    it('rejects an unowned E2EE write once credentials exist', async () => {
      storeRefreshToken({
        refreshToken: 'rt',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      expect(
        storeE2EEKeys({
          wrappingKeyBase64: 'unowned-wk',
          preferencesKeyBase64: 'unowned-pk',
          wrappedPrivateKeyBase64: 'unowned-wpk', // pragma: allowlist secret
        })
      ).toBe(false);
      expect(await restoreE2EEKeys()).toBeNull();
    });

    it('restoreE2EEKeys returns null when safeStorage unavailable', async () => {
      safeStorageAvailable = false;
      expect(await restoreE2EEKeys()).toBeNull();
    });

    it('restoreE2EEKeys returns null on read failure', async () => {
      fsRead.impl = () => {
        throw new Error('ENOENT');
      };
      expect(await restoreE2EEKeys()).toBeNull();
    });

    it('restoreE2EEKeys returns null when the stored blob fails to decrypt', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      expect(
        await storeE2EEKeysIfOwner(
          { wrappingKeyBase64: 'wk', preferencesKeyBase64: 'pk', wrappedPrivateKeyBase64: 'wpk' },
          owner
        )
      ).toBe(true);
      _resetForTesting();
      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok', token: 'tk' });
      vi.spyOn(safeStorage, 'decryptStringAsync').mockRejectedValueOnce(
        new Error('Error while decrypting the ciphertext')
      );

      await expect(restoreE2EEKeys()).resolves.toBeNull();
      expect(safeStorage.decryptStringAsync).toHaveBeenCalled();
    });
  });

  // ─── E2EE persist-failure signalling (regression #1288) ─────────────
  // storeE2EEKeys used to swallow keychain/disk write failures and return
  // void, so the IPC handler always resolved and the renderer persist-catch
  // (Register / SSOPassphraseSetup / Login) was dead code for a genuine
  // keychain-locked write. Contract now: return `true` when persistence is in
  // its expected state (written, or intentionally skipped for session-only /
  // no-safeStorage), `false` ONLY when a disk write was attempted and failed.
  // In-memory key custody is preserved regardless (the #1278 invariant).
  describe('storeE2EEKeys persist-failure signalling (#1288)', () => {
    const keys = {
      wrappingKeyBase64: 'wk',
      preferencesKeyBase64: 'pk',
      wrappedPrivateKeyBase64: 'wpk',
    };

    it('returns false when the keychain write genuinely fails', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      // Keychain locked → safeStorage.encryptStringAsync rejects (the #1288
      // failure mode). Drain the token write first so only the E2EE encrypt
      // hits the rejection.
      await flushDiskWrites();
      vi.spyOn(safeStorage, 'encryptStringAsync').mockRejectedValueOnce(
        new Error('keychain locked')
      );

      expect(await storeE2EEKeysIfOwner(keys, owner)).toBe(false);
      // #1278 invariant: a persistence failure must NEVER drop the in-memory
      // session — keys stay usable in-session; only restart-survival is lost.
      expect(await restoreE2EEKeys()).toEqual(keys);
    });

    it('returns true on a successful disk persist', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      await flushDiskWrites();
      fsWriteCalls.length = 0;

      expect(await storeE2EEKeysIfOwner(keys, owner)).toBe(true);
      expect(fsWriteCalls.length).toBeGreaterThan(0);
    });

    it('returns true when disk persist is intentionally skipped (session-only)', async () => {
      const owner = storeRefreshToken({
        refreshToken: 'tk',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      fsWriteCalls.length = 0;

      // Session-only skip is not a failure — the renderer must not warn.
      expect(await storeE2EEKeysIfOwner(keys, owner)).toBe(true);
      expect(fsWriteCalls.length).toBe(0);
    });
  });

  describe('releaseCredentialReservation (#2394)', () => {
    const API = 'http://localhost:8080';
    const keys = {
      wrappingKeyBase64: 'wk',
      preferencesKeyBase64: 'pk',
      wrappedPrivateKeyBase64: 'wpk', // pragma: allowlist secret
    };

    it('reopens the generic staging lane after an orphaned SSO reservation', () => {
      reserveCredentialOwner(API);
      expect(storeE2EEKeys(keys)).toBe(false);

      expect(releaseCredentialReservation()).toBe(true);
      expect(storeE2EEKeys(keys)).toBe(true);
    });

    it('lets the staged keys survive to storeRefreshToken adoption', async () => {
      reserveCredentialOwner(API);
      releaseCredentialReservation();
      expect(storeE2EEKeys(keys)).toBe(true);

      storeRefreshToken({ refreshToken: 'rt-new', rememberMe: true, apiBase: API });

      expect(await restoreE2EEKeys()).toEqual(keys);
    });

    // Asserts the outcome that matters — a published credential survives an
    // abandon — but be precise about WHY it passes: publishRefreshToken nulls
    // reservedCredentialOwner, so the release short-circuits on the FIRST
    // guard clause. This is NOT a lock on the inMemoryRefreshToken clause, and
    // it would NOT catch a rewrite to `if (reserved === null) return false;
    // return clearTokensIfOwner(reserved)`. That rewrite is ruled out by the
    // reasoning in the primitive's docstring, not by this test — the state it
    // would need (reserved set AND a token published) is unreachable through
    // the public API, so no test can construct it.
    it('is a no-op after a credential is published (cannot wipe a live session)', async () => {
      const owner = reserveCredentialOwner(API);
      expect(
        storeRefreshTokenIfOwner({ refreshToken: 'rt-live', rememberMe: true, apiBase: API }, owner)
      ).not.toBeNull();

      expect(releaseCredentialReservation()).toBe(false);
      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok', token: 'rt-live' });
    });

    it('is a no-op when no reservation exists', () => {
      expect(releaseCredentialReservation()).toBe(false);
    });

    it('bumps the credential generation so straggler continuations fail their CAS', () => {
      const owner = reserveCredentialOwner(API);
      expect(credentialOwnerIsCurrent(owner)).toBe(true);

      expect(releaseCredentialReservation()).toBe(true);

      expect(credentialOwnerIsCurrent(owner)).toBe(false);
    });

    it('is idempotent — a second release is a no-op', () => {
      reserveCredentialOwner(API);
      expect(releaseCredentialReservation()).toBe(true);
      expect(releaseCredentialReservation()).toBe(false);
      expect(storeE2EEKeys(keys)).toBe(true);
    });

    it('warns without leaking key material or the owner value when the lane is held', () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      const owner = reserveCredentialOwner(API);

      expect(storeE2EEKeys(keys)).toBe(false);

      const emitted = warn.mock.calls.flat().join(' ');
      expect(emitted).toContain('storeE2EEKeys');
      expect(emitted).not.toContain(String(owner));
      // Iterate every component rather than spot-checking one: a future log
      // leak of ANY key field must fail this test, not just the wrapped
      // private key. (CodeRabbit, PR #2655 — CWE-532.)
      for (const value of Object.values(keys)) {
        expect(emitted).not.toContain(value);
      }
      warn.mockRestore();
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
    it('returns persistAvailable=true when safeStorage works', async () => {
      safeStorageAvailable = true;
      expect(await getCapabilities()).toEqual({ persistAvailable: true });
    });

    it('returns persistAvailable=false when safeStorage unavailable', async () => {
      safeStorageAvailable = false;
      expect(await getCapabilities()).toEqual({ persistAvailable: false });
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
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse({
          access_token: jwt,
          session_id: 'sid1',
          previous_session_id: 'sid0',
        })
      );

      const result = await performRefresh();

      expect(result).toEqual({
        status: 'ok',
        accessToken: jwt,
        sessionId: 'sid1',
        previousSessionId: 'sid0',
      });
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
        // The server that issued the challenge: the one main refreshed against.
        mfaApiBase: 'http://localhost:8080',
      });
    });

    it('passes the challenge WebAuthn options through unparsed', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      const webauthnOptions = { publicKey: { challenge: 'AQID', rpId: 'localhost' } };
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse(
          {
            error: 'mfa_upgrade_required',
            mfa_challenge_token: 'chal-wa',
            methods: ['webauthn'],
            webauthn_options: webauthnOptions,
          },
          403
        )
      );

      const result = await performRefresh();
      expect(result.status).toBe('mfa_required');
      expect(result.mfaWebauthnOptions).toEqual(webauthnOptions);
      expect(result.mfaApiBase).toBe('http://localhost:8080');
    });

    // #3663 review. Mutant: the default dropped, so the modal always opens on
    // the strongest method; or the lists cast, so a string reaches the renderer.
    it('carries default_method, and keeps only strings from the method lists', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse(
          {
            error: 'suspicious_session_mfa',
            mfa_challenge_token: 'chal-default',
            methods: 'totp',
            recovery_only_methods: ['backup_code', 7],
            default_method: 'totp',
          },
          403
        )
      );

      const result = await performRefresh();
      expect(result.mfaMethods).toEqual([]);
      expect(result.mfaRecoveryOnlyMethods).toEqual(['backup_code']);
      expect(result.mfaDefaultMethod).toBe('totp');
    });

    it('drops a default_method that is not a string', async () => {
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: false,
        apiBase: 'http://localhost:8080',
      });
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse(
          {
            error: 'suspicious_session_mfa',
            mfa_challenge_token: 'chal-default',
            methods: ['totp'],
            default_method: 42,
          },
          403
        )
      );

      const result = await performRefresh();
      expect(result.mfaDefaultMethod).toBeUndefined();
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

    it('refreshes a successor owner while its predecessor is still in flight', async () => {
      const oldApiBase = 'http://localhost:8080';
      const successorApiBase = 'https://successor.example';
      const successorAccessToken = makeJwt(Math.floor(Date.now() / 1000) + 900);

      let resolveOldRefresh: (response: Response) => void = () => {
        throw new Error('old refresh did not start');
      };
      let resolveSuccessorRefresh: (response: Response) => void = () => {
        throw new Error('successor refresh did not start');
      };
      mockNetFetch
        .mockImplementationOnce(
          () =>
            new Promise<Response>((resolve) => {
              resolveOldRefresh = resolve;
            })
        )
        .mockImplementationOnce(
          () =>
            new Promise<Response>((resolve) => {
              resolveSuccessorRefresh = resolve;
            })
        );

      storeRefreshToken({
        refreshToken: 'rt-old',
        rememberMe: false,
        apiBase: oldApiBase,
      });
      const oldRefresh = performRefresh();

      storeRefreshToken({
        refreshToken: 'rt-successor',
        rememberMe: false,
        apiBase: successorApiBase,
      });
      const successorRefresh = performRefresh();

      expect(mockNetFetch).toHaveBeenCalledTimes(2);
      expect(mockNetFetch).toHaveBeenNthCalledWith(
        2,
        `${successorApiBase}/api/v1/auth/refresh`,
        expect.objectContaining({
          headers: expect.objectContaining({ 'X-Refresh-Token': 'rt-successor' }),
        })
      );

      // Settling the predecessor must neither commit its response nor clear the
      // still-pending successor operation's single-flight pointer.
      resolveOldRefresh(
        jsonResponse({
          access_token: makeJwt(Math.floor(Date.now() / 1000) + 1800),
          refresh_token: 'rt-old-rotated',
        })
      );
      await expect(oldRefresh).resolves.toEqual({ status: 'refresh_failed' });

      const successorJoin = performRefresh();
      expect(mockNetFetch).toHaveBeenCalledTimes(2);

      resolveSuccessorRefresh(
        jsonResponse({
          access_token: successorAccessToken,
          refresh_token: 'rt-successor-rotated',
          session_id: 'sid-successor',
        })
      );
      const [successorResult, joinedResult] = await Promise.all([successorRefresh, successorJoin]);

      expect(successorResult).toEqual({
        status: 'ok',
        accessToken: successorAccessToken,
        sessionId: 'sid-successor',
        previousSessionId: undefined,
      });
      expect(joinedResult).toEqual(successorResult);
      expect(getCachedAccessToken()).toBe(successorAccessToken);
    });

    it('rotates refresh token and persists to disk when rememberMe=true', async () => {
      storeRefreshToken({
        refreshToken: 'rt-old',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
      });
      await flushDiskWrites();
      fsWriteCalls.length = 0; // clear storeRefreshToken writes
      const jwt = makeJwt(Math.floor(Date.now() / 1000) + 900);
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse({ access_token: jwt, refresh_token: 'rt-new' })
      );

      const result = await performRefresh();
      expect(result.status).toBe('ok');
      await flushDiskWrites();
      // Should have written the rotated token to disk
      const tokenWrites = fsWriteCalls.filter((c) => (c[0] as string).includes('secure-token.dat'));
      expect(String(tokenWrites.at(-1)?.[1])).toBe('v11rt-new');
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
      await flushDiskWrites();
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
      expect(cb).toHaveBeenCalledWith(jwt2, undefined, undefined);
    });

    it('calls proactive callback with refresh-session lineage when present', async () => {
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
        jsonResponse({
          access_token: jwt2,
          session_id: 'sid-new',
          previous_session_id: 'sid-old',
        })
      );

      await vi.advanceTimersByTimeAsync(61_000);
      expect(cb).toHaveBeenCalledWith(jwt2, 'sid-new', 'sid-old');
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
      expect(cb).toHaveBeenCalledWith(jwt3, undefined, undefined);
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
      expect(cb).toHaveBeenCalledWith(jwt, undefined, undefined);
    });

    it('uses generation to drop an old response after identical credentials are re-stored', async () => {
      vi.useFakeTimers();
      const apiBase = 'http://localhost:8080';
      const refreshToken = 'rt-same';
      storeRefreshToken({
        refreshToken,
        rememberMe: true,
        apiBase,
        accessToken: makeJwt(Math.floor(Date.now() / 1000) + 120),
      });

      let resolveOldRefresh: (response: Response) => void = () => {
        throw new Error('old refresh did not start');
      };
      mockNetFetch.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveOldRefresh = resolve;
          })
      );
      const cb = vi.fn();
      setProactiveRefreshCallback(cb);

      onSystemResume();
      await vi.advanceTimersByTimeAsync(0);
      expect(mockNetFetch).toHaveBeenCalledWith(
        `${apiBase}/api/v1/auth/refresh`,
        expect.objectContaining({
          headers: expect.objectContaining({ 'X-Refresh-Token': refreshToken }),
        })
      );

      const successorAccessToken = makeJwt(Math.floor(Date.now() / 1000) + 900);
      storeRefreshToken({
        refreshToken,
        rememberMe: true,
        apiBase,
        accessToken: successorAccessToken,
      });
      const successorTimerCount = vi.getTimerCount();
      expect(successorTimerCount).toBe(1);

      resolveOldRefresh(
        jsonResponse({
          access_token: makeJwt(Math.floor(Date.now() / 1000) + 1800),
          refresh_token: 'rt-old-rotated',
          session_id: 'sid-old-2',
          previous_session_id: 'sid-old-1',
        })
      );
      await vi.advanceTimersByTimeAsync(0);

      expect(cb).not.toHaveBeenCalled();
      expect(getCachedAccessToken()).toBe(successorAccessToken);
      expect(await restoreRefreshToken()).toEqual({
        status: 'ok',
        token: refreshToken,
        apiBase,
        rememberMe: true,
      });
      const tokenWrites = fsWriteCalls.filter((call) =>
        String(call[0]).endsWith('secure-token.dat')
      );
      expect((tokenWrites.at(-1)?.[1] as Buffer).toString()).toBe(`v11${refreshToken}`);
      expect(vi.getTimerCount()).toBe(successorTimerCount);

      // The stale response carried a later-expiring access token. If it had
      // replaced the successor's timer, no refresh would fire at this exact
      // successor deadline (900s expiry minus the 60s buffer).
      const timerRefreshAccessToken = makeJwt(Math.floor(Date.now() / 1000) + 1800);
      mockNetFetch.mockResolvedValueOnce(jsonResponse({ access_token: timerRefreshAccessToken }));
      await vi.advanceTimersByTimeAsync(839_999);
      expect(mockNetFetch).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(1);
      expect(mockNetFetch).toHaveBeenCalledTimes(2);
      expect(cb).toHaveBeenCalledWith(timerRefreshAccessToken, undefined, undefined);
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
      expect(cb).toHaveBeenCalledWith(jwt2, undefined, undefined);
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

    it('invalidates local credentials before the logout request settles', async () => {
      vi.useFakeTimers();
      storeRefreshToken({
        refreshToken: 'rt-abc',
        rememberMe: true,
        apiBase: 'http://localhost:8080',
        accessToken: makeJwt(Math.floor(Date.now() / 1000) + 900),
      });
      expect(vi.getTimerCount()).toBe(1);

      let resolveLogout: (response: Response) => void = () => {
        throw new Error('logout did not start');
      };
      mockNetFetch.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveLogout = resolve;
          })
      );

      const logout = performLogout('access-tok');

      expect(getCachedAccessToken()).toBeNull();
      expect(await restoreRefreshToken()).toEqual({ status: 'no_session' });
      expect(vi.getTimerCount()).toBe(0);

      resolveLogout(new Response('', { status: 200 }));
      await logout;
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

  // ─── Async safeStorage: weak keys and await-point fencing ──────────
  // The sync API ran each persist as one uninterruptible block. The async API
  // awaits an encrypt/decrypt, so every disk write and delete after an await
  // must re-check that its credential still owns custody.
  describe('async safeStorage persistence', () => {
    const API = 'http://localhost:8080';
    const keys = {
      wrappingKeyBase64: 'wk',
      preferencesKeyBase64: 'pk',
      wrappedPrivateKeyBase64: 'wpk', // pragma: allowlist secret
    };

    it('keeps a Remember-Me session in memory only under the Linux basic_text backend', async () => {
      setPlatform('linux');
      storageBackend = 'basic_text';
      const owner = storeRefreshToken({ refreshToken: 'rt-weak', rememberMe: true, apiBase: API });
      expect(await storeE2EEKeysIfOwner(keys, owner)).toBe(true);
      await flushDiskWrites();

      expect(credentialWrites()).toEqual([]);
      expect(await getCapabilities()).toEqual({ persistAvailable: false });
      // Still restorable across a soft reload, like rememberMe=false.
      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok', token: 'rt-weak' });
      expect(await restoreE2EEKeys()).toEqual(keys);
    });

    it('keeps a Remember-Me session in memory only when Linux encrypts with the hardcoded v10 key', async () => {
      setPlatform('linux');
      ciphertextTag = 'v10';
      const owner = storeRefreshToken({ refreshToken: 'rt-weak', rememberMe: true, apiBase: API });
      expect(await storeE2EEKeysIfOwner(keys, owner)).toBe(true);
      await flushDiskWrites();

      expect(credentialWrites()).toEqual([]);
      expect(await getCapabilities()).toEqual({ persistAvailable: false });
    });

    it('restores as unavailable without reading disk when only the hardcoded key exists', async () => {
      setPlatform('linux');
      storageBackend = 'basic_text';
      fsRead.impl = () => {
        throw new Error('disk must not be read');
      };
      expect(await restoreRefreshToken()).toEqual({ status: 'unavailable' });
      expect(fsUnlinkCalls).toEqual([]);
    });

    it('removes every predecessor file before the successor encrypt can be interrupted', async () => {
      fsRead.impl = readMockDisk;
      const predecessor = storeRefreshToken({
        refreshToken: 'rt-old',
        rememberMe: true,
        apiBase: API,
      });
      await storeE2EEKeysIfOwner(keys, predecessor);
      await flushDiskWrites();
      expect(diskFile('secure-token.dat')).toBeDefined();

      // Crash while the successor's encrypt is still in flight.
      storeRefreshToken({ refreshToken: 'rt-new', rememberMe: true, apiBase: API });
      _resetForTesting();
      await flushDiskWrites();

      expect(diskFile('secure-token.dat')).toBeUndefined();
      expect(diskFile('secure-e2ee.dat')).toBeUndefined();
      expect(await restoreRefreshToken()).toEqual({ status: 'no_session' });
    });

    it('writes nothing when a logout lands during the token encrypt', async () => {
      const pending = deferred<Buffer>();
      vi.spyOn(safeStorage, 'encryptStringAsync').mockReturnValueOnce(pending.promise);
      storeRefreshToken({ refreshToken: 'rt-1', rememberMe: true, apiBase: API });
      await vi.waitFor(() => expect(safeStorage.encryptStringAsync).toHaveBeenCalled());

      clearTokens();
      pending.resolve(Buffer.from('v11rt-1'));
      await flushDiskWrites();

      expect(credentialWrites()).toEqual([]);
    });

    it('lets only the newest login reach disk when two encrypts overlap', async () => {
      const first = deferred<Buffer>();
      vi.spyOn(safeStorage, 'encryptStringAsync').mockReturnValueOnce(first.promise);
      storeRefreshToken({ refreshToken: 'rt-first', rememberMe: true, apiBase: API });
      await vi.waitFor(() => expect(safeStorage.encryptStringAsync).toHaveBeenCalled());

      const secondOwner = storeRefreshToken({
        refreshToken: 'rt-second',
        rememberMe: true,
        apiBase: API,
      });
      first.resolve(Buffer.from('v11rt-first'));
      await flushDiskWrites();

      expect(String(diskFile('secure-token.dat'))).toBe('v11rt-second');
      expect(JSON.parse(String(diskFile('token-meta.json')))).toMatchObject({
        credentialOwner: secondOwner,
      });
      expect(fsWriteCalls.some(([, data]) => String(data) === 'v11rt-first')).toBe(false);
    });

    it('persists the rotated token, not the published one, when a refresh lands during the encrypt', async () => {
      const publish = deferred<Buffer>();
      vi.spyOn(safeStorage, 'encryptStringAsync').mockReturnValueOnce(publish.promise);
      const owner = storeRefreshToken({
        refreshToken: 'rt-published',
        rememberMe: true,
        apiBase: API,
      });
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse({
          access_token: makeJwt(Math.floor(Date.now() / 1000) + 900),
          refresh_token: 'rt-rotated',
        })
      );
      await performRefresh();

      publish.resolve(Buffer.from('v11rt-published'));
      await flushDiskWrites();

      expect(String(diskFile('secure-token.dat'))).toBe('v11rt-rotated');
      expect(JSON.parse(String(diskFile('token-meta.json')))).toMatchObject({
        credentialOwner: owner,
      });
      // The superseded token never reached disk, even transiently.
      expect(fsWriteCalls.some(([, data]) => String(data) === 'v11rt-published')).toBe(false);
    });

    it('restores as unavailable and keeps the file when the decrypt key is temporarily unavailable', async () => {
      fsRead.impl = (path) =>
        String(path).endsWith('token-meta.json')
          ? JSON.stringify({ apiBase: API, rememberMe: true })
          : Buffer.from('v11stored');
      vi.spyOn(safeStorage, 'decryptStringAsync').mockRejectedValueOnce(
        new Error('safeStorage.decryptStringAsync is temporarily unavailable. Please try again.')
      );

      expect(await restoreRefreshToken()).toEqual({ status: 'unavailable' });
      expect(fsUnlinkCalls).toEqual([]);
    });

    it('does not let a stale restore overwrite or delete a login that lands during its decrypt', async () => {
      fsRead.impl = (path) =>
        String(path).endsWith('token-meta.json')
          ? JSON.stringify({ apiBase: API, rememberMe: true })
          : Buffer.from('v11tampered');
      const decrypt = deferred<{ shouldReEncrypt: boolean; result: string }>();
      vi.spyOn(safeStorage, 'decryptStringAsync').mockReturnValueOnce(decrypt.promise);
      const restoring = restoreRefreshToken();
      await vi.waitFor(() => expect(safeStorage.decryptStringAsync).toHaveBeenCalled());

      storeRefreshToken({ refreshToken: 'rt-login', rememberMe: false, apiBase: API });
      const unlinksAfterLogin = fsUnlinkCalls.length;
      decrypt.reject(new Error('Error while decrypting the ciphertext'));

      expect(await restoring).toMatchObject({ status: 'ok', token: 'rt-login' });
      expect(fsUnlinkCalls.length).toBe(unlinksAfterLogin);
    });

    it('re-encrypts a restored token when safeStorage reports a rotated key', async () => {
      fsRead.impl = (path) =>
        String(path).endsWith('token-meta.json')
          ? JSON.stringify({
              apiBase: API,
              rememberMe: true,
              credentialOwner: 7,
              e2eeState: 'ready',
            })
          : Buffer.from('v11stored');
      vi.spyOn(safeStorage, 'decryptStringAsync').mockResolvedValueOnce({
        shouldReEncrypt: true,
        result: 'rt-stored',
      });

      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok', token: 'rt-stored' });
      await flushDiskWrites();
      expect(String(diskFile('secure-token.dat'))).toBe('v11rt-stored');
    });

    it('reports a key write superseded by a newer key write as not persisted', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      await flushDiskWrites();
      const staleKeys = { ...keys, wrappingKeyBase64: 'stale-wk' };
      const pending = deferred<Buffer>();
      vi.spyOn(safeStorage, 'encryptStringAsync').mockReturnValueOnce(pending.promise);
      const staleWrite = storeE2EEKeysIfOwner(staleKeys, owner);
      await vi.waitFor(() => expect(safeStorage.encryptStringAsync).toHaveBeenCalled());

      const freshWrite = storeE2EEKeysIfOwner(keys, owner);
      pending.resolve(
        Buffer.from(`v11${JSON.stringify({ credentialOwner: owner, keys: staleKeys })}`)
      );

      expect(await staleWrite).toBe(false);
      expect(await freshWrite).toBe(true);
      expect(fsWriteCalls.some(([, data]) => String(data).includes('stale-wk'))).toBe(false);
    });

    it('writes no keys when a logout lands during their encrypt', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      await flushDiskWrites();
      const pending = deferred<Buffer>();
      vi.spyOn(safeStorage, 'encryptStringAsync').mockReturnValueOnce(pending.promise);
      const storing = storeE2EEKeysIfOwner(keys, owner);
      await vi.waitFor(() => expect(safeStorage.encryptStringAsync).toHaveBeenCalled());

      clearTokens();
      pending.resolve(Buffer.from('v11keys'));

      expect(await storing).toBe(false);
      expect(diskFile('secure-e2ee.dat')).toBeUndefined();
    });

    it('does not let a successful stale restore overwrite a login that lands during its decrypt', async () => {
      fsRead.impl = (path) =>
        String(path).endsWith('token-meta.json')
          ? JSON.stringify({ apiBase: API, rememberMe: true })
          : Buffer.from('v11rt-disk');
      const decrypt = deferred<{ shouldReEncrypt: boolean; result: string }>();
      vi.spyOn(safeStorage, 'decryptStringAsync').mockReturnValueOnce(decrypt.promise);
      const restoring = restoreRefreshToken();
      await vi.waitFor(() => expect(safeStorage.decryptStringAsync).toHaveBeenCalled());

      const loginOwner = storeRefreshToken({
        refreshToken: 'rt-login',
        rememberMe: false,
        apiBase: API,
      });
      decrypt.resolve({ shouldReEncrypt: false, result: 'rt-disk' });

      expect(await restoring).toMatchObject({ status: 'ok', token: 'rt-login' });
      expect(getCredentialCustodyState().credentialOwner).toBe(loginOwner);
      expect(await restoreRefreshToken()).toMatchObject({ token: 'rt-login' });
    });

    it('keeps keys written during a key decrypt instead of the older disk copy', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      await storeE2EEKeysIfOwner(keys, owner);
      _resetForTesting();
      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok' });
      const restoredOwner = getCredentialCustodyState().credentialOwner as number;

      const decrypt = deferred<{ shouldReEncrypt: boolean; result: string }>();
      vi.spyOn(safeStorage, 'decryptStringAsync').mockReturnValueOnce(decrypt.promise);
      const restoring = restoreE2EEKeys();
      await vi.waitFor(() => expect(safeStorage.decryptStringAsync).toHaveBeenCalled());

      const freshKeys = { ...keys, wrappingKeyBase64: 'fresh-wk' };
      await storeE2EEKeysIfOwner(freshKeys, restoredOwner);
      decrypt.resolve({
        shouldReEncrypt: false,
        result: JSON.stringify({ credentialOwner: restoredOwner, keys }),
      });

      expect(await restoring).toEqual(freshKeys);
      expect(await restoreE2EEKeys()).toEqual(freshKeys);
    });

    it('returns no keys when a newer login lands during the key decrypt', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      await storeE2EEKeysIfOwner(keys, owner);
      _resetForTesting();
      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok' });

      const decrypt = deferred<{ shouldReEncrypt: boolean; result: string }>();
      vi.spyOn(safeStorage, 'decryptStringAsync').mockReturnValueOnce(decrypt.promise);
      const restoring = restoreE2EEKeys();
      await vi.waitFor(() => expect(safeStorage.decryptStringAsync).toHaveBeenCalled());

      storeRefreshToken({ refreshToken: 'rt-next', rememberMe: false, apiBase: API });
      decrypt.resolve({
        shouldReEncrypt: false,
        result: JSON.stringify({ credentialOwner: owner, keys }),
      });

      expect(await restoring).toBeNull();
    });

    it('refuses a Linux token file under the hardcoded key as tampered, without decrypting it', async () => {
      setPlatform('linux');
      fsRead.impl = (path) =>
        String(path).endsWith('token-meta.json')
          ? JSON.stringify({
              apiBase: API,
              rememberMe: true,
              credentialOwner: 3,
              e2eeState: 'ready',
            })
          : Buffer.from('v10rt-forged');
      const decrypt = vi.spyOn(safeStorage, 'decryptStringAsync');

      expect(await restoreRefreshToken()).toEqual({ status: 'tampered' });
      expect(decrypt).not.toHaveBeenCalled();
      expect(fsUnlinkCalls).toContainEqual(expect.stringContaining('secure-token.dat'));
    });

    it('refuses a Linux E2EE blob under the hardcoded key', async () => {
      setPlatform('linux');
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      await storeE2EEKeysIfOwner(keys, owner);
      _resetForTesting();
      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok' });
      const e2eePath = [...fsFiles.keys()].find((path) => path.endsWith('secure-e2ee.dat'));
      fsFiles.set(
        String(e2eePath),
        Buffer.from(`v10${JSON.stringify({ credentialOwner: owner, keys })}`)
      );

      expect(await restoreE2EEKeys()).toBeNull();
    });

    it('writes credential files through a 0o600 temp file and a rename', async () => {
      fsRead.impl = readMockDisk;
      storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      await flushDiskWrites();

      const tokenPath = [...fsFiles.keys()].find((path) => path.endsWith('secure-token.dat'));
      expect(tokenPath).toBeDefined();
      expect(fsRenameCalls).toContainEqual([`${tokenPath}.${process.pid}.tmp`, tokenPath]);
      expect(fsWriteCalls.find(([path]) => path === tokenPath)?.[2]).toEqual({ mode: 0o600 });
      expect([...fsFiles.keys()].some((path) => path.endsWith('.tmp'))).toBe(false);
    });

    it('removes the temp file and writes no credential when the rename fails', async () => {
      fsRead.impl = readMockDisk;
      fsRename.fail = true;
      storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      await flushDiskWrites();

      expect(diskFile('secure-token.dat')).toBeUndefined();
      expect(fsRmCalls.some((path) => path.endsWith('.tmp'))).toBe(true);
      expect([...fsFiles.keys()].some((path) => path.endsWith('.tmp'))).toBe(false);
    });

    it('restores no session, and falls back to no profile, when the pointer is not a URL', async () => {
      fsRead.impl = readMockDisk;
      // A default-profile credential exists, so a fallback past the pointer would restore it.
      storeRefreshToken({
        refreshToken: 'rt-default',
        rememberMe: true,
        apiBase: 'https://api.concordvoice.chat',
      });
      await flushDiskWrites();
      const pointer = [...fsFiles.keys()].find((path) => path.endsWith('active-profile.json'));
      fsFiles.set(String(pointer), JSON.stringify({ apiBase: ':://// not a url' }));
      _resetForTesting();

      await expect(restoreRefreshToken()).resolves.toEqual({ status: 'no_session' });
      expect(getPersistedApiBase()).toBeNull();
      expect(() => clearTokens()).not.toThrow();
    });

    it('restores no session when the meta file names an API base that is not a URL', async () => {
      fsRead.impl = (path) =>
        String(path).endsWith('token-meta.json')
          ? JSON.stringify({
              apiBase: 'not a url',
              rememberMe: true,
              credentialOwner: 7,
              e2eeState: 'ready',
            })
          : Buffer.from('v11stored');

      await expect(restoreRefreshToken()).resolves.toEqual({ status: 'no_session' });
    });

    it('treats a decrypt that rejects with a non-Error as tampered, without throwing', async () => {
      fsRead.impl = (path) =>
        String(path).endsWith('token-meta.json')
          ? JSON.stringify({
              apiBase: API,
              rememberMe: true,
              credentialOwner: 7,
              e2eeState: 'ready',
            })
          : Buffer.from('v11stored');
      vi.spyOn(safeStorage, 'decryptStringAsync').mockRejectedValueOnce(undefined);

      await expect(restoreRefreshToken()).resolves.toEqual({ status: 'tampered' });
    });

    it('re-encrypts a restored E2EE blob when safeStorage reports a rotated key', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      await storeE2EEKeysIfOwner(keys, owner);
      _resetForTesting();
      expect(await restoreRefreshToken()).toMatchObject({ status: 'ok' });
      const blob = JSON.stringify({ credentialOwner: owner, keys });
      vi.spyOn(safeStorage, 'decryptStringAsync').mockResolvedValueOnce({
        shouldReEncrypt: true,
        result: blob,
      });
      ciphertextTag = 'v12';

      expect(await restoreE2EEKeys()).toEqual(keys);
      await flushDiskWrites();
      expect(String(diskFile('secure-e2ee.dat'))).toBe(`v12${blob}`);
    });

    it('encrypts only the newest of a burst of logins queued behind an encrypt', async () => {
      fsRead.impl = readMockDisk;
      const held = deferred<Buffer>();
      const encrypt = vi.spyOn(safeStorage, 'encryptStringAsync').mockReturnValueOnce(held.promise);
      storeRefreshToken({ refreshToken: 'rt-first', rememberMe: true, apiBase: API });
      await vi.waitFor(() => expect(encrypt).toHaveBeenCalledTimes(1));
      storeRefreshToken({ refreshToken: 'rt-middle', rememberMe: true, apiBase: API });
      storeRefreshToken({ refreshToken: 'rt-last', rememberMe: true, apiBase: API });

      held.resolve(Buffer.from('v11rt-first'));
      await flushDiskWrites();

      expect(encrypt.mock.calls.map(([plain]) => plain)).toEqual(['rt-first', 'rt-last']);
      expect(String(diskFile('secure-token.dat'))).toBe('v11rt-last');
    });

    it('encrypts only the newest of a burst of key writes queued behind an encrypt', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      await flushDiskWrites();
      const held = deferred<Buffer>();
      const encrypt = vi.spyOn(safeStorage, 'encryptStringAsync').mockReturnValueOnce(held.promise);
      const first = storeE2EEKeysIfOwner({ ...keys, wrappingKeyBase64: 'wk-first' }, owner);
      await vi.waitFor(() => expect(encrypt).toHaveBeenCalledTimes(1));
      const middle = storeE2EEKeysIfOwner({ ...keys, wrappingKeyBase64: 'wk-middle' }, owner);
      const last = storeE2EEKeysIfOwner({ ...keys, wrappingKeyBase64: 'wk-last' }, owner);

      held.resolve(Buffer.from('v11first'));

      expect(await first).toBe(false);
      expect(await middle).toBe(false);
      expect(await last).toBe(true);
      const plaintexts = encrypt.mock.calls.map(([plain]) => String(plain));
      expect(plaintexts).toHaveLength(2);
      expect(plaintexts.some((plain) => plain.includes('wk-middle'))).toBe(false);
      expect(String(diskFile('secure-e2ee.dat'))).toContain('wk-last');
    });

    it('restores no other profile when a crash interrupts a login to a new server', async () => {
      fsRead.impl = readMockDisk;
      storeRefreshToken({
        refreshToken: 'rt-saas',
        rememberMe: true,
        apiBase: 'https://api.concordvoice.chat',
      });
      await flushDiskWrites();

      storeRefreshToken({
        refreshToken: 'rt-homelab',
        rememberMe: true,
        apiBase: 'https://homelab.lan',
      });
      _resetForTesting(); // crash before the homelab write lands

      expect(await restoreRefreshToken()).toEqual({ status: 'no_session' });
    });

    it('abandons a restore when registration stages keys during its decrypt', async () => {
      fsRead.impl = (path) =>
        String(path).endsWith('token-meta.json')
          ? JSON.stringify({ apiBase: API, rememberMe: true })
          : Buffer.from('v11rt-disk');
      const decrypt = deferred<{ shouldReEncrypt: boolean; result: string }>();
      vi.spyOn(safeStorage, 'decryptStringAsync').mockReturnValueOnce(decrypt.promise);
      const restoring = restoreRefreshToken();
      await vi.waitFor(() => expect(safeStorage.decryptStringAsync).toHaveBeenCalled());

      expect(storeE2EEKeys(keys)).toBe(true);
      decrypt.resolve({ shouldReEncrypt: false, result: 'rt-disk' });
      expect(await restoring).toEqual({ status: 'no_session' });

      // The staged keys still reach the credential registration mints.
      storeRefreshToken({ refreshToken: 'rt-registered', rememberMe: false, apiBase: API });
      expect(await restoreE2EEKeys()).toEqual(keys);
    });

    it('reports both of two identical key writes as persisted', async () => {
      fsRead.impl = readMockDisk;
      const owner = storeRefreshToken({ refreshToken: 'rt', rememberMe: true, apiBase: API });
      // IPC deserialises a fresh object per call: equal values, distinct identity.
      const first = storeE2EEKeysIfOwner({ ...keys }, owner);
      const second = storeE2EEKeysIfOwner({ ...keys }, owner);

      expect(await first).toBe(true);
      expect(await second).toBe(true);
    });

    it('keeps a rotated token off disk when a logout lands during its encrypt', async () => {
      fsRead.impl = readMockDisk;
      storeRefreshToken({ refreshToken: 'rt-old', rememberMe: true, apiBase: API });
      await flushDiskWrites();
      const rotation = deferred<Buffer>();
      vi.spyOn(safeStorage, 'encryptStringAsync').mockReturnValueOnce(rotation.promise);
      mockNetFetch.mockResolvedValueOnce(
        jsonResponse({
          access_token: makeJwt(Math.floor(Date.now() / 1000) + 900),
          refresh_token: 'rt-rotated',
        })
      );
      await performRefresh();
      await vi.waitFor(() => expect(safeStorage.encryptStringAsync).toHaveBeenCalled());

      clearTokens();
      rotation.resolve(Buffer.from('v11rt-rotated'));
      await flushDiskWrites();

      expect(diskFile('secure-token.dat')).toBeUndefined();
      expect(await restoreRefreshToken()).toEqual({ status: 'no_session' });
    });

    it('returns a login that lands during the availability probe', async () => {
      const probe = deferred<Buffer>();
      vi.spyOn(safeStorage, 'encryptStringAsync').mockReturnValueOnce(probe.promise);
      fsRead.impl = () => {
        throw new Error('ENOENT');
      };
      const restoring = restoreRefreshToken();
      await vi.waitFor(() => expect(safeStorage.encryptStringAsync).toHaveBeenCalled());

      storeRefreshToken({ refreshToken: 'rt-login', rememberMe: false, apiBase: API });
      probe.resolve(Buffer.from('v11secure-storage-probe'));

      expect(await restoring).toMatchObject({ status: 'ok', token: 'rt-login' });
    });
  });
});
