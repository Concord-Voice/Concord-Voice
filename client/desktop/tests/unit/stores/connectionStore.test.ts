import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
});

describe('connectionStore', () => {
  it('starts in stable phase', () => {
    const state = useConnectionStore.getState();
    expect(state.phase).toBe('stable');
    expect(state.diagnostics).toBeNull();
    expect(state.lostAt).toBeNull();
    expect(state.lastVoiceChannelId).toBeNull();
    expect(state.recoveryAttempts).toBe(0);
    expect(state.wireViolationCount).toBe(0);
  });

  it('transitions to grace_period and sets lostAt', () => {
    const before = Date.now();
    useConnectionStore.getState().startGracePeriod();
    const state = useConnectionStore.getState();
    expect(state.phase).toBe('grace_period');
    expect(state.lostAt).toBeGreaterThanOrEqual(before);
  });

  it('transitions to preflight', () => {
    useConnectionStore.getState().enterPreflight();
    expect(useConnectionStore.getState().phase).toBe('preflight');
  });

  it('sets diagnostics', () => {
    const diag = {
      internet: 'ok' as const,
      serverReachable: 'failed' as const,
      tokenValid: 'ok' as const,
      sessionRevoked: false,
      rendererStable: 'ok' as const,
    };
    useConnectionStore.getState().setDiagnostics(diag);
    expect(useConnectionStore.getState().diagnostics).toEqual(diag);
  });

  it('transitions to recovery_a', () => {
    useConnectionStore.getState().enterRecoveryA();
    expect(useConnectionStore.getState().phase).toBe('recovery_a');
  });

  it('transitions to recovery_b', () => {
    useConnectionStore.getState().enterRecoveryB();
    expect(useConnectionStore.getState().phase).toBe('recovery_b');
  });

  it('transitions to fatal', () => {
    useConnectionStore.getState().enterFatal();
    expect(useConnectionStore.getState().phase).toBe('fatal');
  });

  it('tracks last voice channel ID', () => {
    useConnectionStore.getState().setLastVoiceChannelId('voice-123');
    expect(useConnectionStore.getState().lastVoiceChannelId).toBe('voice-123');

    useConnectionStore.getState().setLastVoiceChannelId(null);
    expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
  });

  it('increments recovery attempts', () => {
    useConnectionStore.getState().incrementRecoveryAttempts();
    expect(useConnectionStore.getState().recoveryAttempts).toBe(1);
    useConnectionStore.getState().incrementRecoveryAttempts();
    expect(useConnectionStore.getState().recoveryAttempts).toBe(2);
  });

  it('increments wire violation count by 1 per call', () => {
    useConnectionStore.getState().incrementWireViolation();
    expect(useConnectionStore.getState().wireViolationCount).toBe(1);
    useConnectionStore.getState().incrementWireViolation();
    useConnectionStore.getState().incrementWireViolation();
    expect(useConnectionStore.getState().wireViolationCount).toBe(3);
  });

  it('reset() clears wireViolationCount', () => {
    useConnectionStore.getState().incrementWireViolation();
    useConnectionStore.getState().incrementWireViolation();
    expect(useConnectionStore.getState().wireViolationCount).toBe(2);
    useConnectionStore.getState().reset();
    expect(useConnectionStore.getState().wireViolationCount).toBe(0);
  });

  it('resets all state', () => {
    useConnectionStore.getState().startGracePeriod();
    useConnectionStore.getState().setLastVoiceChannelId('voice-1');
    useConnectionStore.getState().incrementRecoveryAttempts();
    useConnectionStore.getState().incrementWireViolation();
    useConnectionStore.getState().setDiagnostics({
      internet: 'failed',
      serverReachable: 'failed',
      tokenValid: 'failed',
      sessionRevoked: false,
      rendererStable: 'failed',
    });

    useConnectionStore.getState().reset();

    const state = useConnectionStore.getState();
    expect(state.phase).toBe('stable');
    expect(state.diagnostics).toBeNull();
    expect(state.lostAt).toBeNull();
    expect(state.lastVoiceChannelId).toBeNull();
    expect(state.recoveryAttempts).toBe(0);
    expect(state.wireViolationCount).toBe(0);
  });

  it('supports full recovery phase flow', () => {
    const store = useConnectionStore.getState();
    store.startGracePeriod();
    expect(useConnectionStore.getState().phase).toBe('grace_period');

    store.enterPreflight();
    expect(useConnectionStore.getState().phase).toBe('preflight');

    store.setDiagnostics({
      internet: 'ok',
      serverReachable: 'failed',
      tokenValid: 'ok',
      sessionRevoked: false,
      rendererStable: 'ok',
    });

    store.enterRecoveryA();
    expect(useConnectionStore.getState().phase).toBe('recovery_a');

    store.incrementRecoveryAttempts();
    store.incrementRecoveryAttempts();
    expect(useConnectionStore.getState().recoveryAttempts).toBe(2);

    store.enterFatal();
    expect(useConnectionStore.getState().phase).toBe('fatal');
  });
});
