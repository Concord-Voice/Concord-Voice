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
