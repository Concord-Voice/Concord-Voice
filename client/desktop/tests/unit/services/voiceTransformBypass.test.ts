// @vitest-environment node
import { describe, expect, it } from 'vitest';
import { afterEach, vi } from 'vitest';
import {
  BYPASS_PROBE_MAX_ATTEMPTS,
  buildDecryptCreationAttach,
  decideBypassProbeAction,
} from '../../../src/renderer/services/e2ee/voiceTransformBypass';

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

describe('buildDecryptCreationAttach', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('attaches a decrypt transform with the full option set at receiver creation', () => {
    class FakeScriptTransform {
      constructor(
        public worker: unknown,
        public options: unknown
      ) {}
    }
    vi.stubGlobal('RTCRtpScriptTransform', FakeScriptTransform);
    const worker = {} as Worker;
    const receiver = { transform: null } as unknown as RTCRtpReceiver;

    buildDecryptCreationAttach(worker, 'user-1', 'opus', 'consumer-9')(receiver);

    expect(receiver.transform).toBeInstanceOf(FakeScriptTransform);
    expect((receiver.transform as unknown as FakeScriptTransform).worker).toBe(worker);
    expect((receiver.transform as unknown as FakeScriptTransform).options).toEqual({
      role: 'decrypt',
      senderUserId: 'user-1',
      codecFamily: 'opus',
      probeId: 'consumer-9',
    });
  });

  it('propagates a constructor failure to the caller (consume rejects, fail-closed)', () => {
    vi.stubGlobal(
      'RTCRtpScriptTransform',
      class {
        constructor() {
          throw new Error('attach refused');
        }
      }
    );
    const receiver = { transform: null } as unknown as RTCRtpReceiver;
    const attach = buildDecryptCreationAttach({} as Worker, 'user-1', 'opus', 'c2');
    expect(() => attach(receiver)).toThrow('attach refused');
    expect(receiver.transform).toBeNull(); // nothing half-attached
  });

  it('reads the constructor at call time, so a late-defined global works', () => {
    const receiver = { transform: null } as unknown as RTCRtpReceiver;
    const attach = buildDecryptCreationAttach({} as Worker, 'user-1', undefined, 'c1');
    class LateTransform {
      constructor(
        public worker: unknown,
        public options: unknown
      ) {}
    }
    vi.stubGlobal('RTCRtpScriptTransform', LateTransform);
    attach(receiver);
    expect(receiver.transform).toBeInstanceOf(LateTransform);
  });
});
