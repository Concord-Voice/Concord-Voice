import { useE2EEStore } from '@/renderer/stores/e2eeStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('e2eeStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts with both flags false', () => {
    const state = useE2EEStore.getState();
    expect(state.ready).toBe(false);
    expect(state.needsSSOUnlock).toBe(false);
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

  it('reset clears both flags', () => {
    useE2EEStore.getState().setReady(true);
    useE2EEStore.getState().setNeedsSSOUnlock(true);

    useE2EEStore.getState().reset();

    const state = useE2EEStore.getState();
    expect(state.ready).toBe(false);
    expect(state.needsSSOUnlock).toBe(false);
  });
});
