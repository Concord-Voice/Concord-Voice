/**
 * Attestation signal assembly for client identity verification (#677).
 *
 * Collects build identity + SPA attestation state and returns a structured
 * payload that `POST /api/v1/attestation/verify` (Task 18) sends to the
 * server. Field names are snake_case wire names matching the Go server's
 * `VerifyPayload` JSON tags — do NOT camelCase them.
 *
 * The function is `async` by contract (Task 18 awaits it; future signal
 * sources such as TPM attestation or OS-keychain reads may be async).
 * It currently performs no async I/O.
 */

import { getMachineId } from './machineId';
import { getBuildCertHash } from './buildInfo';
import { getSpaHash, getSpaVersion } from './spaState';

export interface AttestationSignals {
  version: string;
  platform: 'macos' | 'windows' | 'linux' | 'web';
  cert_hash?: string;
  machine_id?: string;
  spa_version: string;
  spa_hash: string;
}

export async function collectAttestationSignals(opts: {
  platform: AttestationSignals['platform'];
  version: string;
}): Promise<AttestationSignals> {
  const out: AttestationSignals = {
    version: opts.version,
    platform: opts.platform,
    spa_version: getSpaVersion(),
    spa_hash: getSpaHash(),
  };
  if (opts.platform === 'macos' || opts.platform === 'windows') {
    // Omit cert_hash entirely when unavailable (dev/unsigned builds return '').
    // Sending `cert_hash: ""` would have the server compare against an empty
    // string; an absent field lets it apply its missing-signal policy instead.
    const certHash = getBuildCertHash();
    if (certHash) {
      out.cert_hash = certHash;
    }
  }
  if (opts.platform !== 'web') {
    out.machine_id = getMachineId();
  }
  return out;
}
