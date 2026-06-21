import { createStore } from '../utils/createStore';
import { apiFetch, safeJson } from '../services/apiClient';
import { EntitlementsChangedSchema, type EntitlementsChangedPayload } from '../types/ws-events';

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
  maxVideoHeight: 1080,
  maxVideoFps: 60,
  maxVideoPixelRate: 62208000,
  maxManualBitrateBps: 5000000,
  maxWebcamPublishers: 8,
  maxScreensharePublishers: 1,
  maxMessageChars: 5120,
  maxAttachmentBytes: 26214400,
  maxAvatarBytes: 1048576,
  maxBannerBytes: 2097152,
  allowAnimatedProfile: false,
  usernameChangeIntervalSeconds: 31536000,
};

interface SubscriptionState {
  entitlement: Entitlement;
  degraded: boolean; // true when the last hydrate failed (showing the free floor)
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
  hydrate: () => Promise<void>;
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
  hydrate: async () => {
    try {
      const res = await apiFetch('/api/v1/entitlements');
      if (!res.ok) throw new Error(`entitlements fetch ${res.status}`);
      const raw = await safeJson<unknown>(res);
      // Runtime-validate against the SAME zod schema the WS push is checked
      // with (safeJson only checks Content-Type + casts — no runtime check).
      // A drifted/partial 200 (missing fields, wrong types, proxy-injected
      // HTML that still parses as JSON) thus fails closed to the free floor via
      // the catch below, rather than being stored as authoritative with
      // undefined fields. Keeps the fetch path symmetric with the
      // entitlements_changed dispatch boundary (#1297 / Gitar review).
      const dto = EntitlementsChangedSchema.shape.data.parse(raw);
      set({ entitlement: dto, degraded: false, hydrated: true });
    } catch {
      // Fail closed: never grant premium on error — reset to the free floor.
      // Do NOT set `hydrated`: a failed hydrate is not an authoritative result,
      // so destructive tier-gated consumers must keep waiting (data-loss guard).
      set({ entitlement: FREE_ENTITLEMENT, degraded: true });
    }
  },
  // Account-switch resets to un-hydrated free so the next user's launch-reset
  // waits for THEIR authoritative entitlement rather than acting on the prior
  // session's (or the default) state.
  reset: () => set({ entitlement: FREE_ENTITLEMENT, degraded: false, hydrated: false }),
}));
