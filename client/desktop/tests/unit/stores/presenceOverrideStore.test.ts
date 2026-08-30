import { usePresenceOverrideStore } from '@/renderer/stores/ui/presenceOverrideStore';
import { resetAllStores } from '../../helpers/store-helpers';

const UUID_A = '11111111-1111-4111-8111-111111111111';

beforeEach(() => {
  resetAllStores();
  usePresenceOverrideStore.getState().reset();
});

describe('presenceOverrideStore', () => {
  it('starts empty without persistence state', () => {
    const state = usePresenceOverrideStore.getState();
    expect(state.excludedUserIds).toEqual([]);
    expect(state.appliedVersion).toBe(0);
    expect(state.loading).toBe(false);
    expect(state.saving).toBe(false);
    expect(state.conflict).toBe(false);
    expect(state.error).toBeNull();
  });

  it('applies a copied ID list and server version', () => {
    const ids = [UUID_A];
    usePresenceOverrideStore.getState().setError('old error');

    usePresenceOverrideStore.getState().apply(ids, 4);
    ids.push('22222222-2222-4222-8222-222222222222');

    const state = usePresenceOverrideStore.getState();
    expect(state.excludedUserIds).toEqual([UUID_A]);
    expect(state.appliedVersion).toBe(4);
    expect(state.error).toBeNull();
  });

  it('updates loading, saving, conflict, and error state', () => {
    const actions = usePresenceOverrideStore.getState();
    actions.setLoading(true);
    actions.setSaving(true);
    actions.setConflict(true);
    actions.setError('fixed error');

    expect(usePresenceOverrideStore.getState()).toMatchObject({
      loading: true,
      saving: true,
      conflict: true,
      error: 'fixed error',
    });
  });

  it('resets decrypted state and every status field', () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 9);
    usePresenceOverrideStore.getState().setLoading(true);
    usePresenceOverrideStore.getState().setSaving(true);
    usePresenceOverrideStore.getState().setConflict(true);
    usePresenceOverrideStore.getState().setError('fixed error');

    usePresenceOverrideStore.getState().reset();

    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [],
      appliedVersion: 0,
      loading: false,
      saving: false,
      conflict: false,
      error: null,
    });
  });
});
