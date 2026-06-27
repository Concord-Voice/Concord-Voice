import { useEffect, useState } from 'react';
import { apiFetch, safeJson } from '../services/apiClient';

// Durable age-verification status for the NSFW gate's mount-time short-circuit (#1763).
// The verified OUTCOME lives server-side in age_verification_records (written by
// PUT /age/claim); this hook reads it back via GET /api/v1/age/status so the gate
// rehydrates the verified state instead of re-rendering the first-run DOB-entry form.
//
// Identity-blind: the endpoint returns ONLY the two eligibility booleans, never any
// DOB/age value (the schema stores none; ADR-0025), so nothing sensitive is cached
// in the renderer.
//
// FAIL-CLOSED: any error / non-OK / unparseable / unverified response resolves to
// 'unverified' — NEVER 'verified' on a degraded read. A failed fetch can only
// over-prompt (re-ask for DOB), never under-gate (treat an unverified user as
// verified). Re-prompting is the accepted lesser evil (#1763 security note).
export type AgeStatus =
  | { state: 'loading' }
  | { state: 'unverified' }
  | { state: 'verified'; validAge: boolean; nsfwAuth: boolean };

interface AgeStatusResponse {
  verified?: boolean;
  valid_age?: boolean;
  nsfw_auth?: boolean;
}

export function useAgeStatus(): AgeStatus {
  const [status, setStatus] = useState<AgeStatus>({ state: 'loading' });

  useEffect(() => {
    let active = true;
    void (async () => {
      // Default to the fail-closed verdict; only a definitively-verified response
      // upgrades it.
      let next: AgeStatus = { state: 'unverified' };
      try {
        const res = await apiFetch('/api/v1/age/status');
        if (res.ok) {
          const data = await safeJson<AgeStatusResponse>(res);
          if (data?.verified === true) {
            next = {
              state: 'verified',
              validAge: data.valid_age === true,
              nsfwAuth: data.nsfw_auth === true,
            };
          }
        }
      } catch {
        // Network / parse error → fail closed (leave next as 'unverified').
      }
      if (active) setStatus(next);
    })();
    return () => {
      active = false;
    };
  }, []);

  return status;
}
