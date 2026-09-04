// @vitest-environment node
/**
 * #3104 D6 — per-window PiP session tokens minted in the MAIN process.
 *
 * The token is the ONLY capability that distinguishes a real PiP window from an
 * arbitrary same-origin document on the `concord-pip` BroadcastChannel, and the
 * renderer derives its private reply-channel name from it. Two properties
 * therefore matter and are locked here: it must be unguessable, and it must be
 * safe to use verbatim as a BroadcastChannel name.
 */
import { describe, expect, it } from 'vitest';
import { mintPipSessionToken, PIP_SESSION_TOKEN_BYTES } from '../../../src/main/pipSession';

describe('mintPipSessionToken (#3104 D6)', () => {
  it('mints at least 256 bits of entropy', () => {
    expect(PIP_SESSION_TOKEN_BYTES).toBeGreaterThanOrEqual(32);
    // base64url of N bytes is ceil(4N/3) unpadded characters.
    expect(mintPipSessionToken().length).toBe(Math.ceil((PIP_SESSION_TOKEN_BYTES * 4) / 3));
  });

  it('uses only the base64url alphabet, so it is safe as a channel name', () => {
    for (let i = 0; i < 200; i++) {
      expect(mintPipSessionToken()).toMatch(/^[A-Za-z0-9_-]+$/);
    }
  });

  it('never repeats a value', () => {
    const seen = new Set<string>();
    for (let i = 0; i < 2000; i++) seen.add(mintPipSessionToken());
    expect(seen.size).toBe(2000);
  });
});
