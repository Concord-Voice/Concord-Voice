// @vitest-environment node
import { describe, expect, it } from 'vitest';
import {
  BYPASS_PROBE_MAX_ATTEMPTS,
  decideBypassProbeAction,
} from '../../../src/renderer/services/voiceTransformBypass';

describe('decideBypassProbeAction', () => {
  it('verifies when any frame entered the transform, regardless of packets', () => {
    expect(decideBypassProbeAction(0, 1, 'first', 1)).toBe('verified');
    expect(decideBypassProbeAction(5000, 3, 'reattached', 2)).toBe('verified');
  });

  it('retries fast while inconclusive, then drops to a slow poll — never gives up', () => {
    expect(decideBypassProbeAction(0, 0, 'first', 1)).toBe('retry');
    expect(decideBypassProbeAction(9, 0, 'first', BYPASS_PROBE_MAX_ATTEMPTS - 1)).toBe('retry');
    // A producer that joins muted can start sending minutes later; the probe
    // must still be watching when it does (Gitar finding, PR #2865).
    expect(decideBypassProbeAction(9, 0, 'first', BYPASS_PROBE_MAX_ATTEMPTS)).toBe('slow-retry');
    expect(decideBypassProbeAction(0, 0, 'first', BYPASS_PROBE_MAX_ATTEMPTS + 5)).toBe(
      'slow-retry'
    );
  });

  it('confirms bypass: re-attach on the first phase, fail-closed after', () => {
    expect(decideBypassProbeAction(315, 0, 'first', 1)).toBe('reattach');
    expect(decideBypassProbeAction(315, 0, 'reattached', 1)).toBe('close');
  });
});
