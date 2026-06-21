/**
 * Machine ID — Stable per-installation identifier for token theft detection (#89)
 *
 * Generates a random UUID on first launch and persists it to disk.
 * This ID is sent as X-Machine-Id on all authenticated requests so the server
 * can detect when a refresh token is used from a different installation.
 *
 * The machine ID is never deleted on logout — it's per-installation, not per-session.
 */

import { app } from 'electron';
import path from 'node:path';
import fs from 'node:fs';
import { randomUUID } from 'node:crypto';

const MACHINE_ID_FILE = path.join(app.getPath('userData'), 'machine-id.json');

let cachedId: string | null = null;

export function getMachineId(): string {
  if (cachedId) return cachedId;

  // Try to read existing ID
  try {
    const data = JSON.parse(fs.readFileSync(MACHINE_ID_FILE, 'utf-8'));
    if (data.id && typeof data.id === 'string') {
      cachedId = data.id;
      return data.id;
    }
  } catch {
    // File doesn't exist or is corrupted — generate new
  }

  // Generate and persist
  cachedId = randomUUID();
  try {
    fs.writeFileSync(MACHINE_ID_FILE, JSON.stringify({ id: cachedId }), 'utf-8');
  } catch (err) {
    console.error('[MachineId] Failed to persist machine ID:', (err as Error).message);
  }

  return cachedId;
}
