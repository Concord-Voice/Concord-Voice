/**
 * Client Config Polling Service — fetches server-pushed configuration
 * (feature flags, min version, TURN/media-plane URLs) on a periodic interval.
 *
 * The endpoint GET /api/v1/client/config is public (pre-auth), and we poll
 * from the App root so minVersion enforcement works regardless of auth state.
 */

import { apiFetch } from './apiClient';
import { useClientConfigStore } from '../stores/clientConfigStore';
import { useVoiceStore } from '../stores/voiceStore';

const POLL_INTERVAL_MS = 5 * 60 * 1000; // 5 minutes
const STARTUP_DELAY_MS = 2_000; // 2s after mount — let auth + profile settle first
const SPA_CHECK_MIN_INTERVAL_MS = 60_000;

interface ServerConfigResponse {
  minVersion: string;
  featureFlags: { gifsEnabled?: boolean };
  mediaPlaneUrl: string;
  turn: { host?: string; realm?: string };
  spaUrl?: string;
  spaIpcContract?: number;
}

class ClientConfigService {
  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private startupTimer: ReturnType<typeof setTimeout> | null = null;
  private spaCheckPromise: Promise<boolean> | null = null;
  private lastSpaCheckAt = Number.NEGATIVE_INFINITY;
  private deferredSpaReload = false;
  private readonly focusListener = () => {
    void this.checkAndApplySpaUpdate();
  };
  private readonly visibilityListener = () => {
    if (globalThis.document?.visibilityState === 'visible') {
      void this.checkAndApplySpaUpdate();
    }
  };

  private shouldDeferSpaReload(): boolean {
    const voice = useVoiceStore.getState();
    return (
      voice.connectionState === 'connecting' ||
      voice.connectionState === 'connected' ||
      voice.connectionState === 'reconnecting' ||
      voice.isScreenSharing ||
      voice.callState.kind !== 'idle'
    );
  }

  private checkAndApplySpaUpdate(): Promise<boolean> {
    const spaUpdate = globalThis.electron?.spaUpdate;
    if (!spaUpdate) return Promise.resolve(false);

    const now = Date.now();
    if (this.spaCheckPromise) return this.spaCheckPromise;
    if (!this.deferredSpaReload && now - this.lastSpaCheckAt < SPA_CHECK_MIN_INTERVAL_MS) {
      return Promise.resolve(false);
    }

    this.lastSpaCheckAt = now;
    this.spaCheckPromise = (async () => {
      try {
        const status = await spaUpdate.checkForUpdate();
        const shouldApply =
          status.remoteAvailable &&
          (status.currentMode === 'bundled' ||
            status.newerBytesAvailable === true ||
            (this.deferredSpaReload && status.newerBytesAvailable !== false));

        if (!shouldApply) {
          // Clear a pending deferred-reload intent when the server confirms no
          // newer bytes, OR when remote is unavailable. Otherwise the intent
          // sticks true and keeps bypassing the rate limit on every
          // focus/visibility/poll until an exact `false` arrives — a genuinely
          // newer bundle still re-triggers via newerBytesAvailable === true on
          // the next successful check regardless of this flag.
          if (status.newerBytesAvailable === false || !status.remoteAvailable) {
            this.deferredSpaReload = false;
          }
          return false;
        }

        if (this.shouldDeferSpaReload()) {
          this.deferredSpaReload = true;
          console.debug('[ClientConfig] SPA update deferred');
          return false;
        }

        this.deferredSpaReload = false;
        console.debug('[ClientConfig] SPA update applying');
        await spaUpdate.reloadLatest();
        return true;
      } catch (err) {
        console.warn(
          '[ClientConfig] SPA update check failed:',
          err instanceof Error ? err.message : 'unknown'
        );
        return false;
      } finally {
        this.spaCheckPromise = null;
      }
    })();

    return this.spaCheckPromise;
  }

  /** Fetch config from server and apply to store. */
  async fetch(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/client/config');
      if (!res.ok) {
        console.warn('[ClientConfig] Fetch failed');
        return;
      }

      const data: ServerConfigResponse = await res.json();
      // Snapshot the previous config so we can decide whether to log a
      // change. Polling fires every 5 minutes; without this gate the
      // [ClientConfig] Updated config line spammed the console every poll
      // even when nothing actually changed. Now the log only fires on the
      // first fetch (no prior config) or when one of the tracked fields
      // differs.
      const prevState = useClientConfigStore.getState();
      const prevSpaUrl = prevState.spaUrl;
      const nextSpaUrl = data.spaUrl || '';
      const nextTurn = { host: data.turn?.host ?? '', realm: data.turn?.realm ?? '' };
      const nextSpaIpcContract = data.spaIpcContract || 0;

      const isFirstFetch = !prevState.lastFetchedAt;
      const changed =
        prevState.minVersion !== data.minVersion ||
        prevSpaUrl !== nextSpaUrl ||
        prevState.mediaPlaneUrl !== data.mediaPlaneUrl ||
        prevState.spaIpcContract !== nextSpaIpcContract ||
        JSON.stringify(prevState.featureFlags) !== JSON.stringify(data.featureFlags) ||
        JSON.stringify(prevState.turn) !== JSON.stringify(nextTurn);

      useClientConfigStore.getState().setConfig({
        minVersion: data.minVersion,
        featureFlags: data.featureFlags,
        mediaPlaneUrl: data.mediaPlaneUrl,
        turn: nextTurn,
        spaUrl: nextSpaUrl,
        spaIpcContract: nextSpaIpcContract,
      });

      // SPA updates are applied through main's spaUpdate bridge so the shell
      // re-runs resolveSpaSource and keeps session-only auth in main memory.
      // Constant Pages URLs no longer change per deploy, so we always ask main
      // to compare served bytes. A non-empty URL transition still marks the
      // check urgent for legacy per-SHA deployments.
      if (prevSpaUrl && nextSpaUrl && prevSpaUrl !== nextSpaUrl) {
        this.deferredSpaReload = true;
      }

      const spaApplied = await this.checkAndApplySpaUpdate();
      if (spaApplied) {
        return;
      }

      if (isFirstFetch || changed) {
        console.debug('[ClientConfig] Updated config');
      }
    } catch (err) {
      console.warn('[ClientConfig] Fetch error:', err instanceof Error ? err.message : 'unknown');
    }
  }

  /** Start polling. Safe to call multiple times — stops previous timers first. */
  start(): void {
    this.stop();

    // Initial fetch after short delay
    this.startupTimer = setTimeout(() => {
      this.startupTimer = null;
      this.fetch();
    }, STARTUP_DELAY_MS);

    // Periodic polling
    this.pollTimer = setInterval(() => {
      this.fetch();
    }, POLL_INTERVAL_MS);

    globalThis.addEventListener?.('focus', this.focusListener);
    globalThis.document?.addEventListener?.('visibilitychange', this.visibilityListener);
  }

  /** Stop polling and clear all timers. */
  stop(): void {
    if (this.startupTimer) {
      clearTimeout(this.startupTimer);
      this.startupTimer = null;
    }
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
    globalThis.removeEventListener?.('focus', this.focusListener);
    globalThis.document?.removeEventListener?.('visibilitychange', this.visibilityListener);
    this.lastSpaCheckAt = Number.NEGATIVE_INFINITY;
    this.deferredSpaReload = false;
  }
}

// Singleton
export const clientConfigService = new ClientConfigService();
