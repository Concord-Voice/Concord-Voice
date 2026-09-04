/**
 * Per-window PiP session capability (#3104 D6).
 *
 * `BroadcastChannel('concord-pip')` is reachable by every same-origin document
 * in the app's partition, so the RPC proxy in the main renderer cannot tell a
 * real PiP window from an arbitrary frame that merely knows the channel name.
 * The main process can: `pip:open` created the window, so it mints a secret
 * here and hands it back only over `pip:session`, whose sender check proves the
 * caller is that PiP's own main frame.
 *
 * The renderer names its private BroadcastChannel from this value (see
 * `pipSessionChannelName` in `pipSignalingTypes.ts`), which is why the encoding
 * is base64url — every character is legal in a channel name and needs no
 * escaping — and why the value must never be logged or posted on a shared
 * channel. Knowing the token IS the capability.
 */

import { randomBytes } from 'node:crypto';

/**
 * 256 bits. The token is never transmitted over a network and never stored, so
 * the only attack against it is guessing from another renderer document; 256
 * bits puts that out of reach without making the derived channel name unwieldy.
 */
export const PIP_SESSION_TOKEN_BYTES = 32;

/** Mint a fresh, unguessable capability for exactly one PiP window. */
export function mintPipSessionToken(): string {
  return randomBytes(PIP_SESSION_TOKEN_BYTES).toString('base64url');
}
