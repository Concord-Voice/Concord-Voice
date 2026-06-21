import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  ConsumerPauseCoordinator,
  type PauseEffects,
} from '../../../src/renderer/services/consumerPauseCoordinator';

function makeEffects() {
  return {
    pauseLocalDecode: vi.fn(),
    resumeLocalDecode: vi.fn(),
    pauseServerForwarding: vi.fn(),
    resumeServerForwarding: vi.fn(),
  } satisfies PauseEffects;
}

describe('ConsumerPauseCoordinator', () => {
  let effects: ReturnType<typeof makeEffects>;
  let coord: ConsumerPauseCoordinator;

  beforeEach(() => {
    effects = makeEffects();
    coord = new ConsumerPauseCoordinator(effects);
  });

  it('visibility (both-scope) pauses local decode AND server forwarding', () => {
    coord.addReason('c1', 'visibility');
    expect(effects.pauseLocalDecode).toHaveBeenCalledWith('c1');
    expect(effects.pauseServerForwarding).toHaveBeenCalledWith('c1');
  });

  it('ignis (local-scope) pauses local decode ONLY — never server forwarding', () => {
    coord.addReason('c1', 'ignis');
    expect(effects.pauseLocalDecode).toHaveBeenCalledWith('c1');
    expect(effects.pauseServerForwarding).not.toHaveBeenCalled();
  });

  it('#1541 bug 1: an IGNIS-paused consumer still gets server forwarding cut on visibility', () => {
    coord.addReason('c1', 'ignis'); // local only
    expect(effects.pauseServerForwarding).not.toHaveBeenCalled();
    coord.addReason('c1', 'visibility'); // egress reason added on top
    expect(effects.pauseServerForwarding).toHaveBeenCalledTimes(1);
  });

  it('#1541 bug 2: clearing visibility while ignis still holds keeps local decode paused', () => {
    coord.addReason('c1', 'ignis');
    coord.addReason('c1', 'visibility');
    effects.resumeLocalDecode.mockClear();
    effects.resumeServerForwarding.mockClear();
    coord.removeReason('c1', 'visibility');
    expect(effects.resumeServerForwarding).toHaveBeenCalledTimes(1); // egress resumes
    expect(effects.resumeLocalDecode).not.toHaveBeenCalled(); // but ignis keeps decode paused
  });

  it('manual + ignis: clearing manual keeps decode paused while ignis holds', () => {
    coord.addReason('c1', 'manual');
    coord.addReason('c1', 'ignis');
    effects.resumeLocalDecode.mockClear();
    coord.removeReason('c1', 'manual');
    expect(effects.resumeServerForwarding).toHaveBeenCalledTimes(1);
    expect(effects.resumeLocalDecode).not.toHaveBeenCalled();
  });

  it('is idempotent — repeated addReason emits the effect once', () => {
    coord.addReason('c1', 'visibility');
    coord.addReason('c1', 'visibility');
    expect(effects.pauseLocalDecode).toHaveBeenCalledTimes(1);
    expect(effects.pauseServerForwarding).toHaveBeenCalledTimes(1);
  });

  it('fully resumes only when the last reason clears', () => {
    coord.addReason('c1', 'visibility');
    coord.addReason('c1', 'ignis');
    coord.removeReason('c1', 'ignis');
    expect(effects.resumeLocalDecode).not.toHaveBeenCalled(); // visibility still holds
    coord.removeReason('c1', 'visibility');
    expect(effects.resumeLocalDecode).toHaveBeenCalledTimes(1);
    expect(effects.resumeServerForwarding).toHaveBeenCalledTimes(1);
  });

  it('consumersWithReason returns ids holding a reason', () => {
    coord.addReason('a', 'ignis');
    coord.addReason('b', 'ignis');
    coord.addReason('b', 'visibility');
    coord.addReason('c', 'visibility');
    expect(coord.consumersWithReason('ignis').sort()).toEqual(['a', 'b']);
  });

  it('hasReason reflects current state', () => {
    coord.addReason('a', 'ignis');
    expect(coord.hasReason('a', 'ignis')).toBe(true);
    expect(coord.hasReason('a', 'visibility')).toBe(false);
  });

  it('clearConsumer drops all reasons WITHOUT emitting resume effects', () => {
    coord.addReason('a', 'visibility');
    effects.resumeLocalDecode.mockClear();
    effects.resumeServerForwarding.mockClear();
    coord.clearConsumer('a');
    expect(effects.resumeLocalDecode).not.toHaveBeenCalled();
    expect(effects.resumeServerForwarding).not.toHaveBeenCalled();
    expect(coord.hasReason('a', 'visibility')).toBe(false);
    // After clear, re-adding the same reason emits afresh (applied-state was dropped)
    coord.addReason('a', 'visibility');
    expect(effects.pauseLocalDecode).toHaveBeenCalledTimes(2);
  });

  it('reset clears everything', () => {
    coord.addReason('a', 'visibility');
    coord.addReason('b', 'ignis');
    coord.reset();
    expect(coord.consumersWithReason('visibility')).toEqual([]);
    expect(coord.consumersWithReason('ignis')).toEqual([]);
  });

  it('removeReason on an unknown consumer is a no-op', () => {
    coord.removeReason('ghost', 'ignis');
    expect(effects.resumeLocalDecode).not.toHaveBeenCalled();
  });

  it('removeReason for a reason the consumer does not hold is a no-op', () => {
    coord.addReason('c1', 'visibility');
    effects.resumeLocalDecode.mockClear();
    effects.resumeServerForwarding.mockClear();
    coord.removeReason('c1', 'ignis'); // set exists but has no 'ignis'
    expect(effects.resumeLocalDecode).not.toHaveBeenCalled();
    expect(effects.resumeServerForwarding).not.toHaveBeenCalled();
    expect(coord.hasReason('c1', 'visibility')).toBe(true); // still held
  });
});
