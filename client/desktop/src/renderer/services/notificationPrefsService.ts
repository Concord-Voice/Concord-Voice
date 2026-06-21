import { apiFetch, safeJson } from './apiClient';
import {
  useNotificationPrefsStore,
  type MuteTargetType,
  type PreferenceWire,
} from '../stores/notificationPrefsStore';
import { errorMessage } from '../utils/redactError';

// Re-export so existing service consumers (MuteContextMenuItem) keep working
// without changing their import path. The canonical definition lives in the
// store; the re-export prevents downstream files from having to know whether
// the type comes from a store or a service file.
export type { MuteTargetType };

interface PreferencesResponse {
  preferences: PreferenceWire[];
}

interface MuteResponse {
  status: string;
}

/**
 * Fetch every mute preference for the current user and hydrate the local
 * store. Call once at app boot, after the user is authenticated and the
 * main shell mounts. Safe to call again (e.g. on reconnect) — the store
 * replaces its state with the response.
 */
export async function hydrateNotificationPreferences(): Promise<void> {
  const res = await apiFetch('/api/v1/notifications/preferences');
  if (!res.ok) {
    throw new Error('Failed to fetch notification preferences');
  }
  const data = await safeJson<PreferencesResponse>(res);
  useNotificationPrefsStore.getState().setInitialPreferences(data.preferences);
}

/**
 * Set or clear a mute preference. Updates the local store optimistically
 * BEFORE the network call so the UI reflects the change instantly; on
 * failure, the caller decides whether to roll back (we don't auto-revert
 * because the user's intent is clear and we'd rather show stale-but-
 * intended state than flicker the badge back on).
 *
 * `mutedUntil` may be a Date (timed mute), null (indefinite mute), or
 * unused-but-required-by-the-signature when `muted === false`.
 */
export async function setMutePreference(
  targetType: MuteTargetType,
  targetId: string,
  muted: boolean,
  mutedUntil: Date | null = null
): Promise<void> {
  // Optimistic local update first — the badge / icon should change in the
  // same frame as the click, not after the round-trip.
  useNotificationPrefsStore.getState().setMute(targetType, targetId, muted, mutedUntil);

  const body: Record<string, unknown> = {
    target_type: targetType,
    target_id: targetId,
    muted,
  };
  // Only send muted_until on a mute; sending it on an unmute is meaningless
  // and the server would ignore it anyway, but it keeps the payload tight.
  if (muted && mutedUntil) {
    body.muted_until = mutedUntil.toISOString();
  }

  const res = await apiFetch('/api/v1/notifications/mute', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const err = await safeJson<{ error: string }>(res);
    throw new Error(err.error || 'Failed to update mute preference');
  }
  await safeJson<MuteResponse>(res);
}

/**
 * Convenience: convert a duration label from the context-menu UI into an
 * absolute expiry Date suitable for `setMutePreference`. `'indefinite'`
 * returns null (a permanent mute until manually toggled off).
 *
 * Keeping this in the service rather than the menu component lets every
 * caller agree on the canonical durations and prevents drift if we ever
 * tweak the set ("4 hours" instead of "8 hours", say).
 */
export type MuteDuration = '15m' | '1h' | '8h' | '24h' | 'indefinite';

export function mutedUntilFromDuration(duration: MuteDuration): Date | null {
  if (duration === 'indefinite') return null;
  const minutes = ({ '15m': 15, '1h': 60, '8h': 480, '24h': 1440 } as const)[duration];
  return new Date(Date.now() + minutes * 60_000);
}

/** Human-readable label for each duration; used by the submenu UI. */
export const MUTE_DURATION_LABELS: Record<MuteDuration, string> = {
  '15m': 'For 15 minutes',
  '1h': 'For 1 hour',
  '8h': 'For 8 hours',
  '24h': 'For 24 hours',
  indefinite: 'Until I turn it back on',
};

// ---------------------------------------------------------------------------
// Expiry sweep timer
//
// Timed mutes (`muted_until` set) don't notify the client when they expire —
// we just compare against the wall clock. To keep the in-memory maps from
// growing unboundedly across long sessions we sweep every 60 seconds and
// drop rows that are past their expiry. The store's selector logic ALSO
// checks expiry inline, so a missed sweep window only costs us memory, not
// correctness.
//
// The timer is idempotent (multiple start calls coalesce) and stoppable
// (logout / session tear-down can call stopExpirySweep() to avoid a leaked
// interval).
// ---------------------------------------------------------------------------

const EXPIRY_SWEEP_INTERVAL_MS = 60_000;
let sweepTimer: ReturnType<typeof setInterval> | null = null;

export function startExpirySweep(): void {
  if (sweepTimer !== null) return;
  sweepTimer = setInterval(() => {
    useNotificationPrefsStore.getState().clearExpiredMutes();
  }, EXPIRY_SWEEP_INTERVAL_MS);
}

export function stopExpirySweep(): void {
  if (sweepTimer === null) return;
  clearInterval(sweepTimer);
  sweepTimer = null;
}

/**
 * Hydrate notification preferences and start the expiry sweep, swallowing
 * network/parse failures with a console warning. Used by both fresh-login
 * (Login.tsx) and session-restore (App.tsx) flows — a network blip here
 * is non-fatal: the user can still send/receive, they just see notifications
 * for muted targets until the next successful hydration.
 *
 * Extracted into a single helper so the two call sites stop duplicating the
 * try/catch and so the inner catch contributes 0 cognitive complexity to
 * the calling functions (App.tsx's restore() in particular was at 18/15).
 */
export async function tryHydrateNotificationPrefs(): Promise<void> {
  try {
    await hydrateNotificationPreferences();
    startExpirySweep();
  } catch (err) {
    console.warn('Failed to hydrate notification preferences:', errorMessage(err));
  }
}
