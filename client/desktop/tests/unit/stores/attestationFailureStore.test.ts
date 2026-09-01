import { useAttestationFailureStore } from '@/renderer/stores/auth/attestationFailureStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
});

describe('attestationFailureStore', () => {
  it('starts with visible: false and code: null', () => {
    const state = useAttestationFailureStore.getState();
    expect(state.visible).toBe(false);
    expect(state.code).toBeNull();
    expect(state.requiredMinVersion).toBeUndefined();
    expect(state.downloadHelpUrl).toBeUndefined();
  });

  it('showFailure sets visible: true and code from info', () => {
    useAttestationFailureStore.getState().showFailure({ code: 'CLIENT_VERSION_TOO_OLD' });
    const state = useAttestationFailureStore.getState();
    expect(state.visible).toBe(true);
    expect(state.code).toBe('CLIENT_VERSION_TOO_OLD');
    expect(state.requiredMinVersion).toBeUndefined();
    expect(state.downloadHelpUrl).toBeUndefined();
  });

  it('showFailure sets requiredMinVersion and downloadHelpUrl when provided', () => {
    useAttestationFailureStore.getState().showFailure({
      code: 'ATTESTATION_UNKNOWN_RELEASE',
      requiredMinVersion: '0.2.0',
      downloadHelpUrl: 'https://concordvoice.com/download',
    });
    const state = useAttestationFailureStore.getState();
    expect(state.visible).toBe(true);
    expect(state.code).toBe('ATTESTATION_UNKNOWN_RELEASE');
    expect(state.requiredMinVersion).toBe('0.2.0');
    expect(state.downloadHelpUrl).toBe('https://concordvoice.com/download');
  });

  it('dismiss resets all fields to initial state', () => {
    useAttestationFailureStore.getState().showFailure({
      code: 'ATTESTATION_REVOKED',
      requiredMinVersion: '0.3.0',
      downloadHelpUrl: 'https://concordvoice.com/download',
      observedConfigRequestRevision: 7,
    });

    useAttestationFailureStore.getState().dismiss();

    const state = useAttestationFailureStore.getState();
    expect(state.visible).toBe(false);
    expect(state.code).toBeNull();
    expect(state.requiredMinVersion).toBeUndefined();
    expect(state.downloadHelpUrl).toBeUndefined();
    expect(state.observedConfigRequestRevision).toBeUndefined();
    expect(state.failureRevision).toBe(1);
  });

  it('dismiss from initial state does not throw', () => {
    expect(() => {
      useAttestationFailureStore.getState().dismiss();
    }).not.toThrow();
    const state = useAttestationFailureStore.getState();
    expect(state.visible).toBe(false);
    expect(state.code).toBeNull();
  });

  it('showFailure called multiple times overwrites previous state', () => {
    useAttestationFailureStore.getState().showFailure({ code: 'CLIENT_VERSION_TOO_OLD' });
    useAttestationFailureStore.getState().showFailure({
      code: 'ATTESTATION_UNKNOWN_RELEASE',
      requiredMinVersion: '1.0.0',
    });

    const state = useAttestationFailureStore.getState();
    expect(state.code).toBe('ATTESTATION_UNKNOWN_RELEASE');
    expect(state.requiredMinVersion).toBe('1.0.0');
  });

  it('clears only the matching visible version-floor failure', () => {
    useAttestationFailureStore
      .getState()
      .showFailure({ code: 'CLIENT_VERSION_TOO_OLD', observedConfigRequestRevision: 3 });
    const first = useAttestationFailureStore.getState().failureRevision;
    useAttestationFailureStore.getState().showFailure({ code: 'CLIENT_VERSION_TOO_OLD' });

    useAttestationFailureStore.getState().clearVersionFloorIfCurrent(first, 3);

    expect(useAttestationFailureStore.getState()).toMatchObject({
      visible: true,
      code: 'CLIENT_VERSION_TOO_OLD',
      failureRevision: first + 1,
    });
  });

  it('test reset restores the initial failure revision', () => {
    useAttestationFailureStore.getState().showFailure({ code: 'CLIENT_VERSION_TOO_OLD' });

    resetAllStores();

    expect(useAttestationFailureStore.getState()).toMatchObject({
      visible: false,
      code: null,
      failureRevision: 0,
      observedConfigRequestRevision: undefined,
    });
  });
});
