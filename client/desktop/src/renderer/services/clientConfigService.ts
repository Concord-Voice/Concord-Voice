/**
 * Client Config Polling Service — fetches server-pushed configuration
 * (feature flags, min version, TURN/media-plane URLs) on a periodic interval.
 *
 * The endpoint GET /api/v1/client/config is public (pre-auth), and we poll
 * from the App root so minVersion enforcement works regardless of auth state.
 */

import { apiFetch } from './apiClient';
import { useClientConfigStore } from '../stores/clientConfigStore';

const POLL_INTERVAL_MS = 5 * 60 * 1000; // 5 minutes
const STARTUP_DELAY_MS = 2_000; // 2s after mount — let auth + profile settle first

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

      // Hot SPA reload: if a new SPA build was deployed mid-session, reload the
      // renderer immediately so users don't sit on the old version. We skip the
      // very first poll (prevSpaUrl empty) so the initial config load doesn't
      // trigger a reload of the SPA the shell already booted.
      if (prevSpaUrl && nextSpaUrl && prevSpaUrl !== nextSpaUrl) {
        console.debug('[ClientConfig] SPA updated, reloading renderer');
        globalThis.location.reload();
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
  }
}

// Singleton
export const clientConfigService = new ClientConfigService();
