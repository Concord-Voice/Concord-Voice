import { describe, it, expect, vi, beforeEach } from 'vitest';

// Mock the recovery primitive — Phase 5's tests cover its internals.
vi.mock('@/main/spaSelfHeal', () => ({
  attemptSelfHeal: vi.fn().mockResolvedValue({ mode: 'recovered', retryCount: 1 }),
  __resetSelfHealState: vi.fn(),
}));

import { attemptSelfHeal } from '@/main/spaSelfHeal';
import {
  handleDidFailLoad,
  handleSpaRequestSelfHeal,
  shouldTriggerSelfHealForFailedLoad,
  validateSelfHealSenderFrame,
} from '@/main/spaSelfHealMainFrame';

const mockAttempt = attemptSelfHeal as unknown as ReturnType<typeof vi.fn>;

// Legacy per-SHA host (pre-#976): SPA served under /spa/<sha>/, SHARING the
// origin with the API. Base dir = '/spa/abc1234/' — so non-SPA same-origin
// paths (apex, hash routes, /api/*) are correctly rejected.
const LEGACY_ORIGIN = 'https://api.example.com';
const LEGACY_DIR = '/spa/abc1234/';
// Flat Cloudflare Pages host (post-#976, ADR-0015): SPA served at the origin
// root on a DEDICATED host. Base dir = '/' — so any same-origin frame is the
// SPA. This is the host the #976 regression silently broke self-heal on.
const FLAT_ORIGIN = 'https://spa.example.com';
const FLAT_DIR = '/';

describe('shouldTriggerSelfHealForFailedLoad — did-fail-load filter', () => {
  beforeEach(() => vi.clearAllMocks());

  // ─── Legacy per-SHA host ────────────────────────────────────────────────
  it('triggers on isMainFrame=true when validatedURL is under the SPA base dir (legacy)', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -105,
        validatedURL: 'https://api.example.com/spa/abc1234/index.html',
        isMainFrame: true,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(true);
  });

  it('triggers on isMainFrame=false for a chunk under the SPA assets dir (legacy)', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: 404,
        validatedURL: 'https://api.example.com/spa/abc1234/assets/Settings-Xyz.js',
        isMainFrame: false,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(true);
  });

  it('does NOT trigger on errorCode=-3 (ABORTED) — user-cancelled nav', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -3,
        validatedURL: 'https://api.example.com/spa/abc1234/index.html',
        isMainFrame: true,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('does NOT trigger when remoteSpaBaseUrl is null (bundled mode)', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -105,
        validatedURL: 'https://api.example.com/spa/abc1234/index.html',
        isMainFrame: true,
        remoteSpaBaseUrl: null,
        remoteSpaBaseDir: null,
      })
    ).toBe(false);
  });

  it('does NOT trigger on isMainFrame=true with cross-origin validatedURL', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -105,
        validatedURL: 'https://evil.example.com/spa/abc1234/index.html',
        isMainFrame: true,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('does NOT trigger on isMainFrame=false with cross-origin chunk URL (Copilot review on PR #773)', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: 404,
        validatedURL: 'https://evil.example.com/spa/abc1234/assets/Settings-Xyz.js',
        isMainFrame: false,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('does NOT trigger on non-SPA sub-resource (same origin, not under assets/)', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: 404,
        validatedURL: 'https://api.example.com/api/v1/messages',
        isMainFrame: false,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('does NOT trigger when validatedURL is malformed', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -105,
        validatedURL: 'not-a-url',
        isMainFrame: true,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('does NOT trigger on legacy host with same-origin apex pathname', () => {
    // The legacy SHARED origin also hosts the API; the apex `/` is not the SPA.
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -105,
        validatedURL: 'https://api.example.com/',
        isMainFrame: true,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('does NOT trigger on legacy host with same-origin hash-route URL', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -105,
        validatedURL: 'https://api.example.com/#/pip/abc',
        isMainFrame: true,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('does NOT trigger on legacy host with same-origin /api/* path', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: 404,
        validatedURL: 'https://api.example.com/api/v1/something',
        isMainFrame: true,
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  // ─── Flat Cloudflare Pages host (post-#976) — the regression fix ─────────
  it('triggers on flat-host main-frame apex (the dedicated host IS the SPA)', () => {
    // Pre-fix the hardcoded SPA_URL_PATTERN rejected `https://spa.example.com/`
    // (no /spa/<sha>/), silently disabling self-heal on the production host.
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -105,
        validatedURL: 'https://spa.example.com/',
        isMainFrame: true,
        remoteSpaBaseUrl: FLAT_ORIGIN,
        remoteSpaBaseDir: FLAT_DIR,
      })
    ).toBe(true);
  });

  it('triggers on flat-host main-frame index.html', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -105,
        validatedURL: 'https://spa.example.com/index.html',
        isMainFrame: true,
        remoteSpaBaseUrl: FLAT_ORIGIN,
        remoteSpaBaseDir: FLAT_DIR,
      })
    ).toBe(true);
  });

  it('triggers on flat-host chunk asset (sub-resource)', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: 404,
        validatedURL: 'https://spa.example.com/assets/recoveryService-40XchBlv.js',
        isMainFrame: false,
        remoteSpaBaseUrl: FLAT_ORIGIN,
        remoteSpaBaseDir: FLAT_DIR,
      })
    ).toBe(true);
  });

  it('does NOT trigger on flat-host non-asset sub-resource', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: 404,
        validatedURL: 'https://spa.example.com/api/v1/x',
        isMainFrame: false,
        remoteSpaBaseUrl: FLAT_ORIGIN,
        remoteSpaBaseDir: FLAT_DIR,
      })
    ).toBe(false);
  });

  it('does NOT trigger on flat host with cross-origin URL', () => {
    expect(
      shouldTriggerSelfHealForFailedLoad({
        errorCode: -105,
        validatedURL: 'https://evil.example.com/',
        isMainFrame: true,
        remoteSpaBaseUrl: FLAT_ORIGIN,
        remoteSpaBaseDir: FLAT_DIR,
      })
    ).toBe(false);
  });
});

describe('validateSelfHealSenderFrame — IPC origin guard', () => {
  // ─── Legacy per-SHA host ────────────────────────────────────────────────
  it('accepts a sender under the SPA base dir (legacy)', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://api.example.com/spa/abc1234/index.html',
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(true);
  });

  it('accepts a sender with a chunk-asset path under the SPA base dir (legacy)', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://api.example.com/spa/abc1234/assets/Settings.js',
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(true);
  });

  it('rejects a sender from a different origin', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://evil.example.com/abc',
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('rejects when remoteSpaBaseUrl is null (no SPA mode active)', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://api.example.com/spa/abc1234/index.html',
        remoteSpaBaseUrl: null,
        remoteSpaBaseDir: null,
      })
    ).toBe(false);
  });

  it('rejects when senderFrameUrl is empty', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: '',
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('rejects when senderFrameUrl is malformed', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'not-a-url',
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('rejects a legacy-host sender on same origin but non-SPA apex pathname', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://api.example.com/',
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('rejects a legacy-host sender on same-origin hash-route URL', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://api.example.com/#/pip/xyz',
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  it('rejects a legacy-host sender on same-origin /api/* path', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://api.example.com/api/v1/messages',
        remoteSpaBaseUrl: LEGACY_ORIGIN,
        remoteSpaBaseDir: LEGACY_DIR,
      })
    ).toBe(false);
  });

  // ─── Flat Cloudflare Pages host (post-#976) — the regression fix ─────────
  it('accepts a flat-host sender at the apex (the dedicated host IS the SPA)', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://spa.example.com/',
        remoteSpaBaseUrl: FLAT_ORIGIN,
        remoteSpaBaseDir: FLAT_DIR,
      })
    ).toBe(true);
  });

  it('accepts a flat-host sender at index.html', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://spa.example.com/index.html',
        remoteSpaBaseUrl: FLAT_ORIGIN,
        remoteSpaBaseDir: FLAT_DIR,
      })
    ).toBe(true);
  });

  it('accepts a flat-host sender on a hash route (the real reconnect-window frame)', () => {
    // The origin-502 logs showed the frame at `/#/app`; Pages canonicalises
    // /index.html→/, so the live sender frame is root + hash. This must pass —
    // it is exactly the frame whose self-heal IPC the stale pattern rejected.
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://spa.example.com/#/app',
        remoteSpaBaseUrl: FLAT_ORIGIN,
        remoteSpaBaseDir: FLAT_DIR,
      })
    ).toBe(true);
  });

  it('rejects a flat-host sender from a different origin', () => {
    expect(
      validateSelfHealSenderFrame({
        senderFrameUrl: 'https://evil.example.com/',
        remoteSpaBaseUrl: FLAT_ORIGIN,
        remoteSpaBaseDir: FLAT_DIR,
      })
    ).toBe(false);
  });
});

describe('handleSpaRequestSelfHeal — extracted IPC handler body', () => {
  beforeEach(() => vi.clearAllMocks());

  it('rejects when sender frame fails origin/path validation, does not call attemptSelfHeal', async () => {
    const result = await handleSpaRequestSelfHeal({
      senderFrameUrl: 'https://evil.example.com/spa/abc1234/index.html',
      payload: { reason: 'chunk-load' },
      remoteSpaBaseUrl: LEGACY_ORIGIN,
      remoteSpaBaseDir: LEGACY_DIR,
    });
    expect(result).toBeNull();
    expect(mockAttempt).not.toHaveBeenCalled();
  });

  it('rejects when payload is not a valid renderer request (main-process-only reason)', async () => {
    const result = await handleSpaRequestSelfHeal({
      senderFrameUrl: 'https://api.example.com/spa/abc1234/index.html',
      payload: { reason: 'main-frame-load' },
      remoteSpaBaseUrl: LEGACY_ORIGIN,
      remoteSpaBaseDir: LEGACY_DIR,
    });
    expect(result).toBeNull();
    expect(mockAttempt).not.toHaveBeenCalled();
  });

  it('rejects when payload is not an object', async () => {
    const result = await handleSpaRequestSelfHeal({
      senderFrameUrl: 'https://api.example.com/spa/abc1234/index.html',
      payload: 'not-an-object',
      remoteSpaBaseUrl: LEGACY_ORIGIN,
      remoteSpaBaseDir: LEGACY_DIR,
    });
    expect(result).toBeNull();
    expect(mockAttempt).not.toHaveBeenCalled();
  });

  it('dispatches to attemptSelfHeal on valid request, returns the outcome', async () => {
    const result = await handleSpaRequestSelfHeal({
      senderFrameUrl: 'https://api.example.com/spa/abc1234/index.html',
      payload: {
        reason: 'chunk-load',
        url: 'https://api.example.com/spa/abc1234/assets/x.js',
      },
      remoteSpaBaseUrl: LEGACY_ORIGIN,
      remoteSpaBaseDir: LEGACY_DIR,
    });

    expect(mockAttempt).toHaveBeenCalledOnce();
    expect(mockAttempt).toHaveBeenCalledWith({
      reason: 'chunk-load',
      url: 'https://api.example.com/spa/abc1234/assets/x.js',
    });
    expect(result).toEqual({ mode: 'recovered', retryCount: 1 });
  });

  it('dispatches with chunk-import-rejected reason and undefined url', async () => {
    await handleSpaRequestSelfHeal({
      senderFrameUrl: 'https://api.example.com/spa/abc1234/index.html',
      payload: { reason: 'chunk-import-rejected' },
      remoteSpaBaseUrl: LEGACY_ORIGIN,
      remoteSpaBaseDir: LEGACY_DIR,
    });

    expect(mockAttempt).toHaveBeenCalledWith({ reason: 'chunk-import-rejected', url: undefined });
  });

  it('dispatches on a flat-host hash-route sender (post-#976 end-to-end)', async () => {
    // The exact failure from the origin-502 logs: a chunk-import-rejected from
    // the flat-host `/#/app` frame. Pre-fix this was rejected at the sender gate.
    await handleSpaRequestSelfHeal({
      senderFrameUrl: 'https://spa.example.com/#/app',
      payload: { reason: 'chunk-import-rejected' },
      remoteSpaBaseUrl: FLAT_ORIGIN,
      remoteSpaBaseDir: FLAT_DIR,
    });
    expect(mockAttempt).toHaveBeenCalledWith({ reason: 'chunk-import-rejected', url: undefined });
  });
});

describe('handleDidFailLoad — extracted listener body', () => {
  beforeEach(() => vi.clearAllMocks());

  it('does not dispatch when filter rejects (errorCode -3 ABORTED)', async () => {
    const result = await handleDidFailLoad({
      errorCode: -3,
      validatedURL: 'https://api.example.com/spa/abc1234/index.html',
      isMainFrame: true,
      remoteSpaBaseUrl: LEGACY_ORIGIN,
      remoteSpaBaseDir: LEGACY_DIR,
    });
    expect(result).toBeNull();
    expect(mockAttempt).not.toHaveBeenCalled();
  });

  it('does not dispatch when filter rejects (no remoteSpaBaseUrl)', async () => {
    const result = await handleDidFailLoad({
      errorCode: -105,
      validatedURL: 'https://api.example.com/spa/abc1234/index.html',
      isMainFrame: true,
      remoteSpaBaseUrl: null,
      remoteSpaBaseDir: null,
    });
    expect(result).toBeNull();
    expect(mockAttempt).not.toHaveBeenCalled();
  });

  it('dispatches main-frame-load on isMainFrame=true with SPA URL', async () => {
    await handleDidFailLoad({
      errorCode: -105,
      validatedURL: 'https://api.example.com/spa/abc1234/index.html',
      isMainFrame: true,
      remoteSpaBaseUrl: LEGACY_ORIGIN,
      remoteSpaBaseDir: LEGACY_DIR,
    });

    expect(mockAttempt).toHaveBeenCalledOnce();
    expect(mockAttempt).toHaveBeenCalledWith({
      reason: 'main-frame-load',
      url: 'https://api.example.com/spa/abc1234/index.html',
      errorCode: -105,
    });
  });

  it('dispatches sub-resource on isMainFrame=false with chunk URL', async () => {
    await handleDidFailLoad({
      errorCode: 404,
      validatedURL: 'https://api.example.com/spa/abc1234/assets/Settings-x.js',
      isMainFrame: false,
      remoteSpaBaseUrl: LEGACY_ORIGIN,
      remoteSpaBaseDir: LEGACY_DIR,
    });

    expect(mockAttempt).toHaveBeenCalledWith({
      reason: 'sub-resource',
      url: 'https://api.example.com/spa/abc1234/assets/Settings-x.js',
      errorCode: 404,
    });
  });

  it('returns the recovery outcome on dispatch', async () => {
    const result = await handleDidFailLoad({
      errorCode: -105,
      validatedURL: 'https://api.example.com/spa/abc1234/index.html',
      isMainFrame: true,
      remoteSpaBaseUrl: LEGACY_ORIGIN,
      remoteSpaBaseDir: LEGACY_DIR,
    });
    expect(result).toEqual({ mode: 'recovered', retryCount: 1 });
  });
});
