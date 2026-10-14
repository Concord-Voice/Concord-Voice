import { act, renderHook } from '@testing-library/react';
import {
  ServerCapabilitiesSchema,
  isExpirationPinnedPolicyUnsupported,
  useClientConfigStore,
  useExpirationPinnedPolicyUnsupported,
} from '@/renderer/stores/ui/clientConfigStore';
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

// #3459: the capability behind the "Include pinned messages" option. It is a UX gate, so the
// whole question is which states count as CONFIRMED unsupported (hide) versus not told (show).
describe('expirationPinnedPolicySupported capability (#3459)', () => {
  const caps = (features: Record<string, unknown>) =>
    ServerCapabilitiesSchema.parse({ auth: { oauthProviders: [] }, features });

  it('survives parsing: zod strips an undeclared key, which would make it permanently unreadable', () => {
    // The exact trap chunkedAttachmentUpload fell into: a flag the schema does not declare is
    // stripped, reads as undefined forever, and the control it gates never appears.
    expect(caps({ expirationPinnedPolicySupported: true }).features).toMatchObject({
      expirationPinnedPolicySupported: true,
    });
    expect(caps({ expirationPinnedPolicySupported: false }).features).toMatchObject({
      expirationPinnedPolicySupported: false,
    });
  });

  it('is confirmed-unsupported only for a payload that parsed and does not say true', () => {
    expect(isExpirationPinnedPolicyUnsupported(caps({}))).toBe(true);
    expect(
      isExpirationPinnedPolicyUnsupported(caps({ expirationPinnedPolicySupported: false }))
    ).toBe(true);
    expect(
      isExpirationPinnedPolicyUnsupported(caps({ expirationPinnedPolicySupported: true }))
    ).toBe(false);
  });

  it('fails OPEN when capabilities are not known (loading or a failed fetch)', () => {
    // A transient /server/capabilities blip nulls the object; hiding a safe control behind it
    // would be worse than showing it.
    expect(isExpirationPinnedPolicyUnsupported(null)).toBe(false);
  });

  it('the selector tracks the store through load, support, withdrawal and a failed re-fetch', () => {
    const { result } = renderHook(() => useExpirationPinnedPolicyUnsupported());
    expect(result.current).toBe(false); // loading

    act(() =>
      useClientConfigStore
        .getState()
        .setServerCapabilities(caps({ expirationPinnedPolicySupported: true }))
    );
    expect(result.current).toBe(false); // supported

    act(() => useClientConfigStore.getState().setServerCapabilities(caps({})));
    expect(result.current).toBe(true); // a server that predates it

    act(() => useClientConfigStore.getState().setServerCapabilities(null));
    expect(result.current).toBe(false); // the fetch failed: not told, so open again
  });
});
