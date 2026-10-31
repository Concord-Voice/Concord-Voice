// Shared by apiClient and the voice service, which relays the token to the
// media plane (#4206). Kept out of apiClient so voice tests that mock apiClient
// still get the real, never-throwing getter.

/**
 * Fetch the cached attestation token WITHOUT letting an IPC failure brick the
 * request. `X-Attestation-Token` is an OPTIONAL header — the server is the gate
 * that enforces attestation and returns 403 (handled separately) when it is
 * required. A rejected `attestation:get-token` IPC must therefore degrade to
 * "no token attached", never propagate up and fail the whole request.
 *
 * Defense-in-depth for the bundled-build outage: a sender-frame regression made
 * this IPC throw on every call, and the unguarded `await` cascaded an optional
 * header into total connectivity loss. The optional-chain still skips the call
 * entirely on the web/test path where `globalThis.electron` is undefined.
 */
export async function getAttestationTokenSafe(): Promise<string | null> {
  try {
    return (await globalThis.electron?.attestation?.getToken()) ?? null;
  } catch (err) {
    console.warn(
      'Attestation token fetch failed; proceeding without X-Attestation-Token:',
      err instanceof Error ? err.message : 'unknown error'
    );
    return null;
  }
}

/** The handshake fields the media plane relays on its control-plane hops (#4206). */
export interface MediaAttestationRelay {
  attestation_token?: string;
  machine_id?: string;
}

/**
 * Reads the attestation token and machine id for the media-plane handshake.
 * Never throws: an absent value only means the hop is unattested, and the
 * control plane decides whether that is allowed. The session id is read from
 * the auth store at handshake time instead, so it always matches the JWT.
 */
export async function loadMediaAttestationRelay(apiBase: string): Promise<MediaAttestationRelay> {
  const relay: MediaAttestationRelay = {};
  const token = await getAttestationTokenSafe();
  if (token) relay.attestation_token = token;
  try {
    const machineId = await globalThis.electron?.getMachineId?.(apiBase);
    if (machineId) relay.machine_id = machineId;
  } catch {
    // Same posture as the token: an IPC failure leaves the header off.
  }
  return relay;
}
