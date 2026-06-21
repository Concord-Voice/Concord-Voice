import { useMFAChallengeStore } from '@/renderer/stores/mfaChallengeStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
});

describe('mfaChallengeStore', () => {
  it('starts with no challenge', () => {
    const state = useMFAChallengeStore.getState();
    expect(state.challengeToken).toBeNull();
    expect(state.methods).toEqual([]);
    expect(state.recoveryOnlyMethods).toEqual([]);
    expect(state.purpose).toBeNull();
    expect(state.resolve).toBeNull();
  });

  it('showChallenge sets challenge state and resolves with { verified: true, payload }', async () => {
    const promise = useMFAChallengeStore
      .getState()
      .showChallenge('token-1', ['totp', 'webauthn'], 'suspicious_refresh');

    const state = useMFAChallengeStore.getState();
    expect(state.challengeToken).toBe('token-1');
    expect(state.methods).toEqual(['totp', 'webauthn']);
    expect(state.purpose).toBe('suspicious_refresh');
    expect(state.resolve).not.toBeNull();

    useMFAChallengeStore
      .getState()
      .completeChallenge({ verified: true, payload: { access_token: 'tok' } });

    const result = await promise;
    expect(result).toEqual({ verified: true, payload: { access_token: 'tok' } });

    // State should be cleared
    const cleared = useMFAChallengeStore.getState();
    expect(cleared.challengeToken).toBeNull();
    expect(cleared.methods).toEqual([]);
    expect(cleared.resolve).toBeNull();
  });

  it('showChallenge propagates a full login-shape payload through completeChallenge', async () => {
    const promise = useMFAChallengeStore
      .getState()
      .showChallenge('token-payload', ['totp'], 'sso_login');

    const payload = {
      access_token: 'jwt-tok',
      session_id: 'sess-1',
      refresh_token: 'ref-1',
    };
    useMFAChallengeStore.getState().completeChallenge({ verified: true, payload });

    const result = await promise;
    expect(result).toEqual({ verified: true, payload });
  });

  it('showChallenge resolves with { verified: false } when completeChallenge is called with verified=false', async () => {
    const promise = useMFAChallengeStore
      .getState()
      .showChallenge('token-2', ['totp'], 'suspicious_refresh');

    useMFAChallengeStore.getState().completeChallenge({ verified: false });

    const result = await promise;
    expect(result).toEqual({ verified: false });
  });

  it('clearChallenge resolves with { verified: false } (cancel path)', async () => {
    const promise = useMFAChallengeStore
      .getState()
      .showChallenge('token-3', ['totp'], 'suspicious_refresh');

    useMFAChallengeStore.getState().clearChallenge();

    const result = await promise;
    expect(result).toEqual({ verified: false });

    const state = useMFAChallengeStore.getState();
    expect(state.challengeToken).toBeNull();
  });

  it('handles recoveryOnlyMethods', async () => {
    const promise = useMFAChallengeStore
      .getState()
      .showChallenge('token-4', ['totp', 'recovery'], 'suspicious_refresh', ['recovery']);

    const state = useMFAChallengeStore.getState();
    expect(state.recoveryOnlyMethods).toEqual(['recovery']);

    useMFAChallengeStore
      .getState()
      .completeChallenge({ verified: true, payload: { access_token: 'tok' } });
    await promise;
  });
});
