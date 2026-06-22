import { describe, it, expect, vi, beforeEach } from 'vitest';
import { createHash } from 'node:crypto';

// Mock electron's `net` module before importing the module under test.
vi.mock('electron', () => {
  const fetch = vi.fn();
  return {
    net: { fetch },
    __mocks__: { fetch },
  };
});

// Mock tokenManager so getPersistedApiBase is controllable per test.
vi.mock('@/main/tokenManager', () => {
  const getPersistedApiBase = vi.fn();
  return { getPersistedApiBase };
});

// Mock IPC contract version to a known value.
vi.mock('@/main/ipcContract', () => ({ IPC_CONTRACT_VERSION: 7 }));

// Mock buildInfo so getBuildSha7 is controllable per test.
vi.mock('@/main/buildInfo', () => {
  const getBuildSha7 = vi.fn().mockReturnValue('');
  return { getBuildSha7 };
});

import { net } from 'electron';
import { getPersistedApiBase } from '@/main/tokenManager';
import { getBuildSha7 } from '@/main/buildInfo';
import { getSpaHash, getSpaVersion, setSpaHash, setSpaVersion } from '@/main/spaState';
import {
  resolveSpaSource,
  isUnexpectedBundled,
  captureSpaHash,
  hashEntryHtml,
} from '@/main/spaLoader';

const mockNet = net as unknown as { fetch: ReturnType<typeof vi.fn> };
const mockGetApiBase = getPersistedApiBase as unknown as ReturnType<typeof vi.fn>;
const mockGetBuildSha7 = getBuildSha7 as unknown as ReturnType<typeof vi.fn>;

function mockConfigResponse(spaUrl: string | undefined, spaIpcContract: number | undefined) {
  const body = { spaUrl, spaIpcContract };
  mockNet.fetch.mockResolvedValue({
    ok: true,
    status: 200,
    json: vi.fn().mockResolvedValue(body),
  } as unknown as Response);
}

describe('spaLoader — defensive /api/v1/spa/ sentinel', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGetApiBase.mockReturnValue('https://api.concordvoice.chat');
  });

  it('rejects spaUrl with /api/v1/spa/ pathname as poisoned sentinel', async () => {
    mockConfigResponse('https://api.concordvoice.chat/api/v1/spa/abc1234/index.html', 7);
    const result = await resolveSpaSource();
    expect(result.mode).toBe('bundled');
    expect(result.reason).toMatch(/poisoned sentinel|legacy \/api\/v1\/spa\//);
  });

  it('does NOT reject adjacent /api/v1/client/config paths (narrow match)', async () => {
    // /api/v1/client/config would never be a legitimate spaUrl, but this confirms
    // the sentinel matches narrowly on /api/v1/spa/ and not on the broader /api/.
    mockConfigResponse('https://api.concordvoice.chat/api/v1/client/config', 7);
    const result = await resolveSpaSource();
    // Not rejected by the sentinel. (Will still fall back to bundled for other
    // reasons, but the reason string must NOT mention the sentinel.)
    expect(result.reason).not.toMatch(/poisoned sentinel|legacy \/api\/v1\/spa\//);
  });

  it('allows legitimate /spa/ pathname past the sentinel', async () => {
    mockConfigResponse('https://api.concordvoice.chat/spa/abc1234/index.html', 7);
    const result = await resolveSpaSource();
    expect(result.mode).toBe('remote');
    expect(result.url).toBe('https://api.concordvoice.chat/spa/abc1234/index.html');
  });

  // T4a (#976): SPA bundle moved to Cloudflare Pages at a constant host.
  // spaLoader validates https + contract only (no host allowlist by design —
  // spec §8 defers host pinning; see [internal]rules/electron.md "Auto-Updater").
  it('accepts the Cloudflare Pages SPA URL (T4a, #976)', async () => {
    mockConfigResponse('https://spa.concordvoice.chat/index.html', 7);
    const result = await resolveSpaSource();
    expect(result.mode).toBe('remote');
    expect(result.url).toBe('https://spa.concordvoice.chat/index.html');
  });

  it('rejects non-HTTPS Pages-shaped URL (http:// still blocked)', async () => {
    mockConfigResponse('http://spa.concordvoice.chat/index.html', 7);
    const result = await resolveSpaSource();
    expect(result.mode).toBe('bundled');
    expect(result.reason).toMatch(/non-HTTPS protocol/);
  });
});

describe('isUnexpectedBundled', () => {
  it('treats first-launch reason as expected', () => {
    expect(isUnexpectedBundled('no persisted API base (first launch or logged out)')).toBe(false);
  });

  it('treats no-spaUrl-configured as expected', () => {
    expect(isUnexpectedBundled('server has no spaUrl configured')).toBe(false);
  });

  it('treats spaIpcContract-zero as expected', () => {
    expect(isUnexpectedBundled('server spaIpcContract is zero or absent')).toBe(false);
  });

  it('treats config-fetch-returned as UNEXPECTED', () => {
    expect(isUnexpectedBundled('config fetch returned 500')).toBe(true);
  });

  it('treats config-fetch-failed as UNEXPECTED', () => {
    expect(isUnexpectedBundled('config fetch failed: timeout after 5000ms')).toBe(true);
  });

  it('treats spaUrl-rejected as UNEXPECTED', () => {
    expect(isUnexpectedBundled('spaUrl rejected: non-HTTPS protocol http:')).toBe(true);
    expect(isUnexpectedBundled('spaUrl rejected: invalid URL')).toBe(true);
    expect(
      isUnexpectedBundled('spaUrl rejected: legacy /api/v1/spa/ path (poisoned sentinel)')
    ).toBe(true);
  });

  it('treats IPC-contract-mismatch as UNEXPECTED (shell needs update)', () => {
    expect(isUnexpectedBundled('IPC contract 8 < required 9 — shell update needed')).toBe(true);
  });

  it('treats unknown reasons as UNEXPECTED (fail-loud)', () => {
    expect(isUnexpectedBundled('something we did not anticipate')).toBe(true);
  });
});

// ─── captureSpaHash tests (#677) ────────────────────────────────────────────

/**
 * Return a mock Response whose arrayBuffer() yields the given Buffer.
 */
function mockFetchResponse(bytes: Buffer): Response {
  return {
    arrayBuffer: vi
      .fn()
      .mockResolvedValue(bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength)),
  } as unknown as Response;
}

describe('captureSpaHash — remote mode', () => {
  const remoteUrl = 'https://api.concordvoice.chat/spa/abc123def/index.html';
  const knownBytes = Buffer.from('<html>remote</html>');
  const expectedHash = `sha256:${createHash('sha256').update(knownBytes).digest('hex')}`;

  beforeEach(() => {
    vi.clearAllMocks();
    // Reset spaState singletons to a known baseline before each test.
    setSpaHash('');
    setSpaVersion('');
  });

  it('sets spaHash to sha256:<hex> of the fetched bytes', async () => {
    mockNet.fetch.mockResolvedValue(mockFetchResponse(knownBytes));
    await captureSpaHash('remote', remoteUrl);
    expect(getSpaHash()).toBe(expectedHash);
  });

  it('extracts the SPA SHA from the URL path as spaVersion', async () => {
    mockNet.fetch.mockResolvedValue(mockFetchResponse(knownBytes));
    await captureSpaHash('remote', remoteUrl);
    expect(getSpaVersion()).toBe('abc123def'); // pragma: allowlist secret
  });

  it('sets spaVersion to "" when the remote URL has no /spa/<sha>/ segment', async () => {
    mockNet.fetch.mockResolvedValue(mockFetchResponse(knownBytes));
    // A remote URL whose pathname does not match /spa/<sha>/ — hash capture
    // still succeeds, but version extraction yields the empty string.
    await captureSpaHash('remote', 'https://api.concordvoice.chat/index.html');
    expect(getSpaHash()).toBe(expectedHash);
    expect(getSpaVersion()).toBe('');
  });

  it('uses net.fetch with the remote URL (not the bundled URL)', async () => {
    mockNet.fetch.mockResolvedValue(mockFetchResponse(knownBytes));
    await captureSpaHash('remote', remoteUrl);
    expect(mockNet.fetch).toHaveBeenCalledWith(remoteUrl);
  });
});

describe('captureSpaHash — bundled mode', () => {
  // sha7 = first 7 lowercase hex chars of the commit SHA. The server's SPA
  // registry is keyed by sha7 (main-cd.yml publishes with GITHUB_SHA:0:7), so
  // bundled-mode spa_version MUST be sha7, NOT the build tag.
  const knownSha7 = 'abc1234';
  const knownBytes = Buffer.from('<html>bundled</html>');
  const expectedHash = `sha256:${createHash('sha256').update(knownBytes).digest('hex')}`;

  beforeEach(() => {
    vi.clearAllMocks();
    setSpaHash('');
    setSpaVersion('');
    mockGetBuildSha7.mockReturnValue(knownSha7);
  });

  it('sets spaHash to sha256:<hex> of the fetched bytes', async () => {
    mockNet.fetch.mockResolvedValue(mockFetchResponse(knownBytes));
    await captureSpaHash('bundled');
    expect(getSpaHash()).toBe(expectedHash);
  });

  it('uses getBuildSha7() as the spaVersion (HIGH #17 — server registry expects sha7)', async () => {
    mockNet.fetch.mockResolvedValue(mockFetchResponse(knownBytes));
    await captureSpaHash('bundled');
    expect(getSpaVersion()).toBe(knownSha7);
  });

  it('sets spaVersion to "" when getBuildSha7() returns empty (PR-smoke / dev build)', async () => {
    // PR-smoke build tags are `pr-smoke-<run-id>` — not commit-derived, so sha7
    // is unresolvable. The empty-string posture surfaces as
    // ATTESTATION_UNKNOWN_RELEASE on the server, which is correct: non-release
    // bundles are not in the server's SPA registry.
    mockGetBuildSha7.mockReturnValue('');
    mockNet.fetch.mockResolvedValue(mockFetchResponse(knownBytes));
    await captureSpaHash('bundled');
    expect(getSpaVersion()).toBe('');
    // Hash still captured normally.
    expect(getSpaHash()).toBe(expectedHash);
  });

  it('fetches app://concord/index.html (not a remote URL)', async () => {
    mockNet.fetch.mockResolvedValue(mockFetchResponse(knownBytes));
    await captureSpaHash('bundled');
    expect(mockNet.fetch).toHaveBeenCalledWith('app://concord/index.html');
  });
});

describe('captureSpaHash — best-effort (never throws)', () => {
  const priorHash = 'sha256:priorvalue';

  beforeEach(() => {
    vi.clearAllMocks();
    // Pre-populate state so we can assert it is UNCHANGED after a failure.
    setSpaHash(priorHash);
    setSpaVersion('prior-version');
  });

  it('resolves (does not throw) when net.fetch rejects', async () => {
    mockNet.fetch.mockRejectedValue(new Error('net::ERR_CONNECTION_REFUSED'));
    // Must resolve without throwing — a throw would break the SPA load path.
    await expect(captureSpaHash('remote', 'https://x/spa/zzz/index.html')).resolves.toBeUndefined();
  });

  it('leaves spaHash unchanged when capture fails', async () => {
    mockNet.fetch.mockRejectedValue(new Error('network error'));
    await captureSpaHash('remote', 'https://x/spa/zzz/index.html');
    // Singletons must retain their pre-failure values.
    expect(getSpaHash()).toBe(priorHash);
  });
});

// ─── hashEntryHtml (powers the spa:checkForUpdate available-bytes diff) ───────
describe('hashEntryHtml', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('returns sha256:<hex> of the fetched entry HTML bytes', async () => {
    const bytes = Buffer.from('<html>latest ui</html>');
    const expected = `sha256:${createHash('sha256').update(bytes).digest('hex')}`;
    mockNet.fetch.mockResolvedValue(mockFetchResponse(bytes));
    await expect(hashEntryHtml('https://spa.concordvoice.chat/index.html')).resolves.toBe(expected);
    expect(mockNet.fetch).toHaveBeenCalledWith('https://spa.concordvoice.chat/index.html');
  });

  it('returns null (fail-open, never throws) when net.fetch rejects', async () => {
    mockNet.fetch.mockRejectedValue(new Error('net::ERR_CONNECTION_REFUSED'));
    await expect(hashEntryHtml('https://spa.concordvoice.chat/index.html')).resolves.toBeNull();
  });
});
