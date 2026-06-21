import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';

// Mock dependencies before importing
vi.mock('@/renderer/services/apiClient', () => ({
  refreshAccessToken: vi.fn(),
}));

import {
  runPreflight,
  markRendererCrashed,
  clearCrashFlag,
} from '@/renderer/services/recoveryService';
import { refreshAccessToken } from '@/renderer/services/apiClient';

const mockRefresh = vi.mocked(refreshAccessToken);

beforeEach(() => {
  vi.clearAllMocks();
  sessionStorage.clear();
  // Reset navigator.onLine
  Object.defineProperty(navigator, 'onLine', { value: true, writable: true });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('recoveryService', () => {
  describe('runPreflight', () => {
    it('returns all-healthy when everything works', async () => {
      global.fetch = vi.fn().mockResolvedValue({ ok: true });
      mockRefresh.mockResolvedValue('new-token');

      const results = await runPreflight();
      expect(results.internet).toBe('ok');
      expect(results.serverReachable).toBe('ok');
      expect(results.tokenValid).toBe('ok');
      expect(results.sessionRevoked).toBe(false);
      expect(results.rendererStable).toBe('ok');
    });

    it('reports each check independently even when navigator.onLine is false', async () => {
      Object.defineProperty(navigator, 'onLine', { value: false, writable: true });
      // fetch throws (network unreachable) and refresh returns null — both
      // rows should be 'failed' (check ran and failed), not 'unknown' (check skipped).
      global.fetch = vi.fn().mockRejectedValue(new Error('network error'));
      // Refresh returns null (matches Promise<string | null> contract) →
      // tokenValid = 'failed' under the new CheckResult semantics.
      mockRefresh.mockResolvedValue(null);

      const results = await runPreflight();
      expect(results.internet).toBe('failed');
      // server check ran but failed (fetch threw); not 'unknown'.
      expect(results.serverReachable).toBe('failed');
      // token check ran but failed (refresh returned null → tokenValid 'failed').
      expect(results.tokenValid).toBe('failed');
    });

    it('returns serverReachable=failed when health check fails', async () => {
      // After #817 Defect 1 fix: only one fetch happens (server /health) — no Google probe.
      global.fetch = vi.fn().mockResolvedValue({ ok: false });

      const results = await runPreflight();
      expect(results.internet).toBe('ok');
      expect(results.serverReachable).toBe('failed');
    });

    it('returns tokenValid=failed when refresh fails', async () => {
      global.fetch = vi.fn().mockResolvedValue({ ok: true });
      mockRefresh.mockResolvedValue(null);

      const results = await runPreflight();
      expect(results.tokenValid).toBe('failed');
      expect(results.sessionRevoked).toBe(true);
    });

    it('returns rendererStable=failed after crash', async () => {
      global.fetch = vi.fn().mockResolvedValue({ ok: true });
      mockRefresh.mockResolvedValue('new-token');
      markRendererCrashed();

      const results = await runPreflight();
      expect(results.rendererStable).toBe('failed');
    });
  });

  describe('markRendererCrashed', () => {
    it('sets sessionStorage crash flag', () => {
      markRendererCrashed();
      expect(sessionStorage.getItem('concord:renderer-crashed')).not.toBeNull();
    });
  });

  describe('clearCrashFlag', () => {
    it('removes sessionStorage crash flag', () => {
      markRendererCrashed();
      clearCrashFlag();
      expect(sessionStorage.getItem('concord:renderer-crashed')).toBeNull();
    });
  });

  describe('runPreflight — defect regression (issue #817)', () => {
    it('does not fetch clients3.google.com', async () => {
      const fetchSpy = vi.fn().mockResolvedValue({ ok: true });
      global.fetch = fetchSpy;
      mockRefresh.mockResolvedValue('token');

      await runPreflight();

      const urls = fetchSpy.mock.calls.map((c) => String(c[0]));
      expect(urls.some((u) => u.includes('google.com'))).toBe(false);
    });

    it('internet field equals navigator.onLine and triggers no fetch for it', async () => {
      Object.defineProperty(navigator, 'onLine', { value: true, writable: true });
      const fetchSpy = vi.fn().mockResolvedValue({ ok: true });
      global.fetch = fetchSpy;
      mockRefresh.mockResolvedValue('token');

      const result = await runPreflight();

      expect(result.internet).toBe('ok');
      // Exactly one fetch happens — the server /health check. The internet
      // field is sourced from navigator.onLine, not from a probe.
      expect(fetchSpy).toHaveBeenCalledTimes(1);
      expect(String(fetchSpy.mock.calls[0][0])).toMatch(/\/health(?:$|\?)/);
    });

    it('uses /health bare path, not /api/v1/health', async () => {
      const fetchSpy = vi.fn().mockResolvedValue({ ok: true });
      global.fetch = fetchSpy;
      mockRefresh.mockResolvedValue('token');

      await runPreflight();

      const urls = fetchSpy.mock.calls.map((c) => String(c[0]));
      // Bare /health path is hit; /api/v1/health is NOT.
      expect(urls.some((u) => u.includes('/api/v1/health'))).toBe(false);
      expect(urls.some((u) => /\/health(?:$|\?)/.test(u))).toBe(true);
    });

    it('runs server check even when navigator.onLine is false (no short-circuit)', async () => {
      Object.defineProperty(navigator, 'onLine', { value: false, writable: true });
      const fetchSpy = vi.fn().mockResolvedValue({ ok: true });
      global.fetch = fetchSpy;
      mockRefresh.mockResolvedValue('token');

      const result = await runPreflight();

      // fetch was called even though navigator.onLine is false (no short-circuit).
      expect(fetchSpy).toHaveBeenCalled();
      expect(result.internet).toBe('failed');
      expect(result.serverReachable).toBe('ok');
      expect(result.tokenValid).toBe('ok');
    });

    it('runs token check even when serverReachable is false (no short-circuit)', async () => {
      Object.defineProperty(navigator, 'onLine', { value: true, writable: true });
      // Mock fetch to return ok:false (server unreachable)
      global.fetch = vi.fn().mockResolvedValue({ ok: false });
      mockRefresh.mockResolvedValue('token');

      const result = await runPreflight();

      expect(result.serverReachable).toBe('failed');
      // Token check still ran:
      expect(mockRefresh).toHaveBeenCalled();
      expect(result.tokenValid).toBe('ok');
    });

    it('does NOT claim sessionRevoked when serverReachable is false (#817 regression)', async () => {
      // When server is unreachable, refresh fails for network reasons —
      // not because the server explicitly revoked the session. sessionRevoked
      // requires positive evidence (a reachable server refusing us), not just
      // a refresh-returning-null signal.
      global.fetch = vi.fn().mockRejectedValue(new Error('network error'));
      mockRefresh.mockResolvedValue(null);

      const result = await runPreflight();

      expect(result.serverReachable).toBe('failed');
      expect(result.tokenValid).toBe('failed');
      expect(result.sessionRevoked).toBe(false);
    });

    it('DOES claim sessionRevoked when serverReachable is ok and refresh returns null', async () => {
      // /health succeeded but refresh refused — server is reachable and
      // explicitly told us the session is gone. This is the actual
      // "session revoked" signal.
      global.fetch = vi.fn().mockResolvedValue({ ok: true });
      mockRefresh.mockResolvedValue(null);

      const result = await runPreflight();

      expect(result.serverReachable).toBe('ok');
      expect(result.tokenValid).toBe('failed');
      expect(result.sessionRevoked).toBe(true);
    });

    it('does NOT claim sessionRevoked when refreshAccessToken throws and server is unreachable', async () => {
      // IPC bridge failure OR runtime error in refresh AND network is down.
      // A throw is not positive evidence of revocation — must default to false.
      Object.defineProperty(navigator, 'onLine', { value: false, writable: true });
      global.fetch = vi.fn().mockRejectedValue(new Error('network error'));
      mockRefresh.mockRejectedValue(new Error('ipc bridge unavailable'));

      const result = await runPreflight();

      expect(result.serverReachable).toBe('failed');
      expect(result.tokenValid).toBe('failed');
      expect(result.sessionRevoked).toBe(false);
    });

    it('does NOT claim sessionRevoked when refreshAccessToken throws even when server is reachable', async () => {
      // /health returned ok, but the IPC refresh threw. We have no
      // POSITIVE evidence the session is revoked — a throw is ambiguous
      // (could be IPC failure, runtime error). Conservative: not revoked.
      Object.defineProperty(navigator, 'onLine', { value: true, writable: true });
      global.fetch = vi.fn().mockResolvedValue({ ok: true });
      mockRefresh.mockRejectedValue(new Error('ipc bridge error'));

      const result = await runPreflight();

      expect(result.serverReachable).toBe('ok');
      expect(result.tokenValid).toBe('failed');
      expect(result.sessionRevoked).toBe(false);
    });
  });
});
