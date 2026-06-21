import { describe, it, expect, beforeEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { useUpdateStatusStore } from '../../../src/renderer/stores/updateStatusStore';

describe('useUpdateStatusStore (#658)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts with criticalError=null and dismissedForSession=false', () => {
    const state = useUpdateStatusStore.getState();
    expect(state.criticalError).toBeNull();
    expect(state.dismissedForSession).toBe(false);
  });

  it('setSecurityError populates subtype + message + firstSeenAt', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'pin miss');
    const state = useUpdateStatusStore.getState();
    expect(state.criticalError).toEqual({
      subtype: 'cert-pin-failure',
      message: 'pin miss',
      firstSeenAt: expect.any(Number),
    });
    expect(state.criticalError?.firstSeenAt).toBeGreaterThan(0);
  });

  it('setSecurityError accepts publisher-failure subtype', () => {
    useUpdateStatusStore.getState().setSecurityError('publisher-failure', 'sig fail');
    expect(useUpdateStatusStore.getState().criticalError?.subtype).toBe('publisher-failure');
  });

  it('dismissForSession flips the session flag', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(false);
    useUpdateStatusStore.getState().dismissForSession();
    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(true);
  });

  it('reset clears both criticalError and dismissedForSession', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    useUpdateStatusStore.getState().dismissForSession();
    useUpdateStatusStore.getState().reset();
    expect(useUpdateStatusStore.getState().criticalError).toBeNull();
    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(false);
  });

  it('setSecurityError overwrites prior error (not merged)', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'first');
    useUpdateStatusStore.getState().setSecurityError('publisher-failure', 'second');
    const state = useUpdateStatusStore.getState();
    expect(state.criticalError?.subtype).toBe('publisher-failure');
    expect(state.criticalError?.message).toBe('second');
  });

  // ─── dismiss-preserve semantics (#719 Copilot review) ──────────────────
  it('preserves dismissedForSession when the same error recurs', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'same-msg');
    useUpdateStatusStore.getState().dismissForSession();
    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(true);

    // Same subtype AND message — should preserve dismiss so the user's
    // consent isn't thrashed by rapid re-emissions.
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'same-msg');

    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(true);
  });

  it('clears dismissedForSession when a new subtype arrives', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    useUpdateStatusStore.getState().dismissForSession();

    useUpdateStatusStore.getState().setSecurityError('publisher-failure', 'x');

    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(false);
  });

  it('clears dismissedForSession when the message changes (same subtype)', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'first message');
    useUpdateStatusStore.getState().dismissForSession();

    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'second message');

    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(false);
  });

  it('sets dismissedForSession=false on fresh setSecurityError from empty state', () => {
    // No prior error; dismissedForSession is already false; new error should
    // leave it false (covers the "state.criticalError is null" branch).
    expect(useUpdateStatusStore.getState().criticalError).toBeNull();
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(false);
  });
});
