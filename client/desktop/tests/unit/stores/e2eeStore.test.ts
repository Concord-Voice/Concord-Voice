import { useE2EEStore } from '@/renderer/stores/e2eeStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('e2eeStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts with both flags false and no SSO credential owner', () => {
    const state = useE2EEStore.getState();
    expect(state.ready).toBe(false);
    expect(state.needsSSOUnlock).toBe(false);
    expect(state.ssoCredentialOwner).toBeNull();
  });

  it('setReady toggles the ready flag without affecting needsSSOUnlock', () => {
    useE2EEStore.getState().setNeedsSSOUnlock(true);
    useE2EEStore.getState().setReady(true);

    const state = useE2EEStore.getState();
    expect(state.ready).toBe(true);
    expect(state.needsSSOUnlock).toBe(true);

    useE2EEStore.getState().setReady(false);
    expect(useE2EEStore.getState().ready).toBe(false);
    // needsSSOUnlock is independent — its lifecycle belongs to useSSOFlow.
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(true);
  });

  it('carries the SSO credential owner only while eager unlock is pending', () => {
    useE2EEStore.getState().setNeedsSSOUnlock(true, 41);

    expect(useE2EEStore.getState().needsSSOUnlock).toBe(true);
    expect(useE2EEStore.getState().ssoCredentialOwner).toBe(41);

    useE2EEStore.getState().setNeedsSSOUnlock(false);

    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
    expect(useE2EEStore.getState().ssoCredentialOwner).toBeNull();
  });

  it('reset clears both flags and the pending SSO credential owner', () => {
    useE2EEStore.getState().setReady(true);
    useE2EEStore.getState().setNeedsSSOUnlock(true, 73);

    useE2EEStore.getState().reset();

    const state = useE2EEStore.getState();
    expect(state.ready).toBe(false);
    expect(state.needsSSOUnlock).toBe(false);
    expect(state.ssoCredentialOwner).toBeNull();
  });
});
