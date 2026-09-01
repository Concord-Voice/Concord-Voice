import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  useClientConfigStore.setState({
    minVersion: '',
    featureFlags: {},
    mediaPlaneUrl: '',
    turn: { host: '', realm: '' },
    spaUrl: '',
    spaIpcContract: 0,
    serverCapabilities: null,
    activityHistoryCapability: { status: 'loading' },
    lastFetchedAt: null,
  });
});

describe('clientConfigStore', () => {
  it('has correct default values', () => {
    const state = useClientConfigStore.getState();
    expect(state.minVersion).toBe('');
    expect(state.featureFlags).toEqual({});
    expect(state.mediaPlaneUrl).toBe('');
    expect(state.turn).toEqual({ host: '', realm: '' });
    expect(state.spaUrl).toBe('');
    expect(state.spaIpcContract).toBe(0);
    expect(state.serverCapabilities).toBeNull();
    expect(state.activityHistoryCapability).toEqual({ status: 'loading' });
    expect(state.lastFetchedAt).toBeNull();
    expect(state.configRequestRevision).toBe(0);
    expect(state.acceptedConfigRevision).toBe(0);
  });

  it('setConfig updates all fields', () => {
    const before = Date.now();
    const requestRevision = useClientConfigStore.getState().beginConfigRequest();
    useClientConfigStore.getState().setConfig(
      {
        minVersion: '0.2.0',
        featureFlags: { gifsEnabled: true },
        mediaPlaneUrl: 'https://media.concordvoice.chat',
        turn: { host: 'turn.concordvoice.chat', realm: 'concord' },
        spaUrl: 'https://app.concordvoice.chat',
        spaIpcContract: 3,
      },
      requestRevision
    );

    const state = useClientConfigStore.getState();
    expect(state.minVersion).toBe('0.2.0');
    expect(state.featureFlags.gifsEnabled).toBe(true);
    expect(state.mediaPlaneUrl).toBe('https://media.concordvoice.chat');
    expect(state.turn.host).toBe('turn.concordvoice.chat');
    expect(state.spaUrl).toBe('https://app.concordvoice.chat');
    expect(state.spaIpcContract).toBe(3);
    expect(state.lastFetchedAt).toBeGreaterThanOrEqual(before);
    expect(state.configRequestRevision).toBe(requestRevision);
    expect(state.acceptedConfigRevision).toBe(requestRevision);
  });

  it('setConfig sets lastFetchedAt timestamp', () => {
    expect(useClientConfigStore.getState().lastFetchedAt).toBeNull();

    const requestRevision = useClientConfigStore.getState().beginConfigRequest();
    useClientConfigStore.getState().setConfig(
      {
        minVersion: '0.1.0',
        featureFlags: { gifsEnabled: true },
        mediaPlaneUrl: '',
        turn: { host: '', realm: '' },
        spaUrl: '',
        spaIpcContract: 0,
      },
      requestRevision
    );

    expect(useClientConfigStore.getState().lastFetchedAt).not.toBeNull();
  });

  it('updates and resets server capability state explicitly', () => {
    useClientConfigStore.getState().setServerCapabilities({
      auth: { oauthProviders: ['google'] },
      features: { activityHistorySupported: true },
    });
    useClientConfigStore.getState().setActivityHistoryCapability({ status: 'supported' });

    expect(useClientConfigStore.getState().serverCapabilities).toEqual({
      auth: { oauthProviders: ['google'] },
      features: { activityHistorySupported: true },
    });
    expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
      status: 'supported',
    });

    useClientConfigStore.getState().resetForRuntimeServer();

    expect(useClientConfigStore.getState().serverCapabilities).toBeNull();
    expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
      status: 'loading',
    });
  });

  it('preserves config request order but invalidates accepted evidence across a runtime-server reset', () => {
    const requestRevision = useClientConfigStore.getState().beginConfigRequest();
    useClientConfigStore.getState().setConfig(
      {
        minVersion: '0.1.0',
        featureFlags: {},
        mediaPlaneUrl: '',
        turn: { host: '', realm: '' },
        spaUrl: '',
        spaIpcContract: 0,
      },
      requestRevision
    );

    useClientConfigStore.getState().resetForRuntimeServer();

    const state = useClientConfigStore.getState();
    expect(state.configRequestRevision).toBe(requestRevision);
    expect(state.acceptedConfigRevision).toBe(0);
  });
});
