import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('authStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts with null accessToken', () => {
    const state = useAuthStore.getState();
    expect(state.accessToken).toBeNull();
  });

  it('setAccessToken stores the token', () => {
    useAuthStore.getState().setAccessToken('access-123');
    const state = useAuthStore.getState();
    expect(state.accessToken).toBe('access-123');
  });

  it('atomically starts a new lifecycle with an access/session pair', () => {
    const previousGeneration = useAuthStore.getState().authGeneration;

    const generation = useAuthStore.getState().beginAuthLifecycle('access-123', 'session-123');

    expect(generation).toBe(previousGeneration + 1);
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'access-123',
      sessionId: 'session-123',
      authGeneration: generation,
    });
  });

  it('clears a prior session ID when a token-only lifecycle replaces it', () => {
    useAuthStore.getState().beginAuthLifecycle('password-token', 'password-session');

    useAuthStore.getState().setAccessToken('sso-token');

    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'sso-token',
      sessionId: null,
    });
  });

  it('rotates credentials atomically only for the expected lifecycle owner', () => {
    const generation = useAuthStore.getState().beginAuthLifecycle('access-1', 'session-1');

    expect(useAuthStore.getState().rotateAuthCredentials(generation, 'access-2', 'session-2')).toBe(
      true
    );
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'access-2',
      sessionId: 'session-2',
      authGeneration: generation,
    });

    useAuthStore.getState().beginAuthLifecycle('successor-access', null);
    expect(
      useAuthStore.getState().rotateAuthCredentials(generation, 'stale-access', 'stale-session')
    ).toBe(false);
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'successor-access',
      sessionId: null,
    });
  });

  it('admits a new lifecycle only while the reservation generation is current', () => {
    const reservation = useAuthStore.getState().authGeneration;
    const admitted = useAuthStore
      .getState()
      .beginAuthLifecycleIfCurrent(reservation, 'reserved-access', 'reserved-session');

    expect(admitted).toBe(reservation + 1);
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'reserved-access',
      sessionId: 'reserved-session',
      authGeneration: admitted,
    });

    useAuthStore.getState().beginAuthLifecycle('successor-access', 'successor-session');
    expect(
      useAuthStore
        .getState()
        .beginAuthLifecycleIfCurrent(reservation, 'stale-access', 'stale-session')
    ).toBeNull();
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'successor-access',
      sessionId: 'successor-session',
    });
  });

  it('clearAccessToken removes the token', () => {
    useAuthStore.getState().setAccessToken('access-123');
    useAuthStore.getState().clearAccessToken();
    const state = useAuthStore.getState();
    expect(state.accessToken).toBeNull();
  });

  it('setRememberMe updates the flag', () => {
    expect(useAuthStore.getState().rememberMe).toBe(true);
    useAuthStore.getState().setRememberMe(false);
    expect(useAuthStore.getState().rememberMe).toBe(false);
  });

  it('starts with rememberMe as true', () => {
    expect(useAuthStore.getState().rememberMe).toBe(true);
  });
});
