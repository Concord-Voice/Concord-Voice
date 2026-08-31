import { createStore } from '../../utils/runtime/createStore';
import { apiFetch, safeJson } from '../../services/system/apiClient';
import { EntitlementsChangedSchema, type EntitlementsChangedPayload } from '../../types/ws-events';
import {
  isHydrationLifecycleCurrent,
  type HydrationLifecycleGuard,
} from '../../services/system/postLoginHydrationLifecycle';

// The store holds the entitlement capability set. The wire type (validated at
// the WS dispatch boundary) IS the store type — no transform needed.
export type Entitlement = EntitlementsChangedPayload;

/**
 * Conservative client-side free floor. This is NOT the source of truth — the
 * server's entitlements.For("free") is. It exists only so the store has a valid
 * least-privilege default before the first successful hydrate and after a failed
 * one. Because free is the FLOOR, drift here can never escalate to premium:
 * premium is only ever reached via a successful server fetch/push.
 * Mirrors services/control-plane/internal/entitlements/entitlements.go freeEntitlement.
 */
export const FREE_ENTITLEMENT: Entitlement = {
  tier: 'free',
  allowCustomScheme: false,
  allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard'],
  minPtimeMs: 20,
  allowMusicMode: false,
  maxAudioLastN: 8,
  // Split video axes (#1602). Free floor: stream 1080p30/≤5M, camera 720p60/≤2.5M.
  // streamMaxPixelRate (#2163) tiers stream fps: 1080p30 & 720p60 admit, 1080p60 rejected.
  streamMaxHeight: 1080,
  streamMaxFps: 60,
  streamMaxPixelRate: 62208000, // = 1920*1080*30 (1080p30); mirrors Go free floor
  streamMaxBitrate: 5000000,
  cameraMaxHeight: 720,
  cameraMaxFps: 60,
  cameraMaxBitrate: 2500000,
  maxManualBitrateBps: 5000000,
  maxWebcamPublishers: 8,
  maxScreensharePublishers: 8, // raised 1→8 for Discord parity — mirrors the Go free floor
  maxMessageChars: 5120,
  maxAttachmentBytes: 33554432, // 32 MiB — matches Go free floor (entitlements.go, post-#1522)
  maxAvatarBytes: 5242880,
  maxBannerBytes: 5242880,
  allowAnimatedProfile: false,
  usernameChangeIntervalSeconds: 31536000,
  maxServersCreated: 5,
  messageHistorySearchDays: 90,
};

let hydrationGeneration = 0;

interface SubscriptionState {
  entitlement: Entitlement;
  // true when the last hydrate failed. On a FIRST-LOAD failure the entitlement is the
  // free floor; on a RECONNECT failure the last-known-good entitlement is preserved (#2172).
  degraded: boolean;
  /**
   * True once an AUTHORITATIVE entitlement has been received — a successful
   * `hydrate()` or an `entitlements_changed` WS push via `setEntitlement`. It is
   * NOT set by a failed hydrate (which only flips `degraded`). Consumers that
   * take a DESTRUCTIVE action on `tier` (e.g. the launch-reset free-tier clamp,
   * #1301) MUST gate on `hydrated && !degraded` — otherwise they act on the
   * pre-hydrate FREE default and silently wipe a premium user's settings.
   */
  hydrated: boolean;
  setEntitlement: (e: Entitlement) => void;
  hydrate: (guard?: HydrationLifecycleGuard) => Promise<void>;
  /**
   * Reset to the least-privilege free floor. Called on logout / account-switch
   * (resetService.gracefulReset) so a prior user's premium capability set can
   * never leak into the next session's in-memory store ("no ghost profiles").
   */
  reset: () => void;
}

export const useSubscriptionStore = createStore<SubscriptionState>()((set) => ({
  entitlement: FREE_ENTITLEMENT,
  degraded: false,
  hydrated: false,
  setEntitlement: (e) => set({ entitlement: e, degraded: false, hydrated: true }),
  hydrate: async (guard) => {
    const generation = hydrationGeneration;
    const isCurrent = (): boolean =>
      generation === hydrationGeneration && isHydrationLifecycleCurrent(guard);
    if (!isCurrent()) return;
    try {
      const res = await apiFetch(
        '/api/v1/entitlements',
        guard ? { signal: guard.signal } : undefined
      );
      if (!isCurrent()) return;
      if (!res.ok) throw new Error(`entitlements fetch ${res.status}`);
      const raw = await safeJson<unknown>(res);
      if (!isCurrent()) return;
      // Runtime-validate against the SAME zod schema the WS push is checked
      // with (safeJson only checks Content-Type + casts — no runtime check).
      // A drifted/partial 200 (missing fields, wrong types, proxy-injected
      // HTML that still parses as JSON) thus fails closed to the free floor via
      // the catch below, rather than being stored as authoritative with
      // undefined fields. Keeps the fetch path symmetric with the
      // entitlements_changed dispatch boundary (#1297 / Gitar review).
      const dto = EntitlementsChangedSchema.shape.data.parse(raw);
      if (!isCurrent()) return;
      set({ entitlement: dto, degraded: false, hydrated: true });
    } catch {
      if (!isCurrent()) return;
      // Distinguish a FIRST-LOAD failure from a RECONNECT failure (#2172):
      //  - First load (`!hydrated`, never authoritatively hydrated): fail CLOSED to
      //    the free floor. Premium is only ever reached via a successful server
      //    fetch/push, so a user who never authenticated as premium can NEVER obtain
      //    it via a degraded state — the monetization invariant.
      //  - Reconnect (`hydrated`, a prior authoritative hydrate succeeded): PRESERVE
      //    the last-known-good entitlement and only flip `degraded`. A premium user's
      //    screen share / features are not clamped to free by a transient network blip.
      //    (A genuine downgrade arrives via a SUCCESSFUL fetch/push, not a failure, so
      //    the only residue is a just-expired user holding stale premium across sustained
      //    fetch failures — bounded, self-limiting, and moot for anything server-gated.)
      // `hydrated` is left untouched either way: on reconnect it stays true (still
      // authoritative), on first load it stays false (destructive tier-gated consumers
      // keep waiting — the #1301 data-loss guard).
      set((s) =>
        s.hydrated ? { degraded: true } : { entitlement: FREE_ENTITLEMENT, degraded: true }
      );
    }
  },
  // Account-switch resets to un-hydrated free so the next user's launch-reset
  // waits for THEIR authoritative entitlement rather than acting on the prior
  // session's (or the default) state.
  reset: () => {
    hydrationGeneration += 1;
    set({ entitlement: FREE_ENTITLEMENT, degraded: false, hydrated: false });
  },
}));
