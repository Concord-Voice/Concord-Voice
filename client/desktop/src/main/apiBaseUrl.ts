/**
 * Main-process API base URL resolution (#974).
 *
 * The client-driven Apple exchange runs entirely in the main process, which
 * must reach the control plane BEFORE any login has persisted an apiBase via
 * tokenManager (SSO is a pre-auth flow). Resolution order:
 *
 *   1. Persisted apiBase from tokenManager metadata (written at first login,
 *      survives restarts) — authoritative when present because it is the
 *      exact base the renderer authenticated against.
 *   2. Packaged builds: the production SaaS endpoint — the same host the
 *      electron-updater feed pins (updatePinningConfig.ts).
 *   3. Dev (unpackaged): CONCORD_DEV_API_BASE env override, else the local
 *      control plane on :8080 (mirrors the renderer config.ts default).
 *
 * NEVER sourced from IPC input — the renderer cannot steer main-process
 * OAuth traffic to an attacker-controlled host (spec §Flow step 6).
 */
import { app } from 'electron';

import { getPersistedApiBase } from './tokenManager';

// Exported so the auth-IPC apiBase guard (main.ts `isValidApiBase`) can pin the
// renderer-supplied apiBase to this same single-tenant origin in packaged builds
// — the host the updater already TLS-pins (updatePinningConfig.ts).
export const PRODUCTION_API_BASE = 'https://api.concordvoice.chat';
const DEV_API_BASE = 'http://localhost:8080';

export function getApiBaseUrl(): string {
  const persisted = getPersistedApiBase();
  if (persisted) return persisted;
  if (app.isPackaged) return PRODUCTION_API_BASE;
  return process.env.CONCORD_DEV_API_BASE ?? DEV_API_BASE;
}
