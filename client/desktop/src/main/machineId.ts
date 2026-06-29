/**
 * Machine ID — Stable per-installation identifier for token theft detection (#89)
 *
 * Generates a random UUID on first launch and persists it to disk.
 * This ID is sent as X-Machine-Id on all authenticated requests so the server
 * can detect when a refresh token is used from a different installation.
 *
 * The machine ID is never deleted on logout — it's per-installation, not per-session.
 */

import path from 'node:path';
import fs from 'node:fs';
import { randomUUID } from 'node:crypto';
import { profilePathsForApiBase } from './selfHostedProfile';

const DEFAULT_PROFILE_API_BASE = 'https://api.concordvoice.chat';

const cachedIds = new Map<string, string>();

export function getMachineId(apiBase = DEFAULT_PROFILE_API_BASE): string {
  const machineIdFile = profilePathsForApiBase(apiBase || DEFAULT_PROFILE_API_BASE).machineIdFile;
  const cachedId = cachedIds.get(machineIdFile);
  if (cachedId) return cachedId;

  // Try to read existing ID
  try {
    const data = JSON.parse(fs.readFileSync(machineIdFile, 'utf-8'));
    if (data.id && typeof data.id === 'string') {
      cachedIds.set(machineIdFile, data.id);
      return data.id;
    }
  } catch {
    // File doesn't exist or is corrupted — generate new
  }

  // Generate and persist
  const id = randomUUID();
  cachedIds.set(machineIdFile, id);
  try {
    fs.mkdirSync(path.dirname(machineIdFile), { recursive: true });
    fs.writeFileSync(machineIdFile, JSON.stringify({ id }), 'utf-8');
  } catch (err) {
    console.error('[MachineId] Failed to persist machine ID:', (err as Error).message);
  }

  return id;
}

export function _resetMachineIdForTesting(): void {
  cachedIds.clear();
}
