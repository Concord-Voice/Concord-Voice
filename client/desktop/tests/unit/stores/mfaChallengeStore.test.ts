import { useMFAChallengeStore } from '@/renderer/stores/auth/mfaChallengeStore';
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

  it('stores ssoContext and resolves the ssoCompletion variant for sso_login (#2424)', async () => {
    const promise = useMFAChallengeStore
      .getState()
      .showChallenge('token-sso', ['totp'], 'sso_login', undefined, {
        provider: 'apple',
        credentialOwner: 12,
      });
    expect(useMFAChallengeStore.getState().ssoContext).toEqual({
      provider: 'apple',
      credentialOwner: 12,
    });
    expect(useMFAChallengeStore.getState().purpose).toBe('sso_login');

    useMFAChallengeStore.getState().completeChallenge(
      {
        verified: true,
        ssoCompletion: { accessToken: 'ax', sessionId: 'sx', credentialOwner: 12 },
      },
      'token-sso'
    );
    await expect(promise).resolves.toEqual({
      verified: true,
      ssoCompletion: { accessToken: 'ax', sessionId: 'sx', credentialOwner: 12 },
    });
    // State (including ssoContext) is cleared after completion.
    expect(useMFAChallengeStore.getState().ssoContext).toBeNull();
    expect(useMFAChallengeStore.getState().challengeToken).toBeNull();
  });

  it('a stale challenge-A completion does not settle a superseding challenge B (AC-11, #2424)', async () => {
    // Challenge A
    const promiseA = useMFAChallengeStore
      .getState()
      .showChallenge('token-A', ['totp'], 'sso_login', undefined, {
        provider: 'google',
        credentialOwner: 7,
      });
    let aResult: unknown;
    void promiseA.then((result) => {
      aResult = result;
    });
    // Challenge B supersedes A (overwrites the active challenge + resolver).
    const promiseB = useMFAChallengeStore
      .getState()
      .showChallenge('token-B', ['totp'], 'sso_login', undefined, {
        provider: 'apple',
        credentialOwner: 9,
      });

    // A late completion bound to challenge A must be IGNORED (token mismatch):
    // it cannot turn A verified, and it does not clear the active challenge B.
    // A itself was already settled not-verified by the supersession.
    useMFAChallengeStore
      .getState()
      .completeChallenge(
        { verified: true, ssoCompletion: { accessToken: 'a', sessionId: 's', credentialOwner: 7 } },
        'token-A'
      );
    await Promise.resolve();
    expect(aResult).toEqual({ verified: false });
    expect(useMFAChallengeStore.getState().challengeToken).toBe('token-B');

    // A completion bound to challenge B settles B.
    useMFAChallengeStore.getState().completeChallenge(
      {
        verified: true,
        ssoCompletion: { accessToken: 'b', sessionId: 's2', credentialOwner: 9 },
      },
      'token-B'
    );
    await expect(promiseB).resolves.toEqual({
      verified: true,
      ssoCompletion: { accessToken: 'b', sessionId: 's2', credentialOwner: 9 },
    });
  });

  it('settles a superseded challenge as not verified, so its caller is not left waiting forever', async () => {
    const promiseA = useMFAChallengeStore
      .getState()
      .showChallenge('token-A', ['totp'], 'suspicious_refresh');
    let aResult: unknown;
    void promiseA.then((result) => {
      aResult = result;
    });

    useMFAChallengeStore.getState().showChallenge('token-B', ['totp'], 'suspicious_refresh');
    await Promise.resolve();

    expect(aResult, 'the request gated on A must be released when B replaces it').toEqual({
      verified: false,
    });
    expect(useMFAChallengeStore.getState().challengeToken).toBe('token-B');
  });

  it('settles a challenge with an empty token at once, since nothing can be shown to answer it', async () => {
    const result = useMFAChallengeStore
      .getState()
      .showChallenge('', ['totp'], 'suspicious_refresh');
    let settled: unknown;
    void result.then((value) => {
      settled = value;
    });
    await Promise.resolve();

    expect(settled).toEqual({ verified: false });
    expect(useMFAChallengeStore.getState().challengeToken).toBeNull();
  });

  it('an empty token clears the challenge it replaces, not only its token', async () => {
    const replaced = useMFAChallengeStore
      .getState()
      .showChallenge('tok-A', ['totp', 'webauthn'], 'sso_login', ['totp'], {
        provider: 'apple',
        credentialOwner: 12,
      });

    await expect(
      useMFAChallengeStore.getState().showChallenge('', ['totp'], 'suspicious_refresh')
    ).resolves.toEqual({
      verified: false,
    });
    await expect(replaced).resolves.toEqual({ verified: false });
    expect(useMFAChallengeStore.getState()).toMatchObject({
      challengeToken: null,
      methods: [],
      recoveryOnlyMethods: [],
      purpose: null,
      webauthnOptions: null,
      ssoContext: null,
      resolve: null,
    });
  });
});
