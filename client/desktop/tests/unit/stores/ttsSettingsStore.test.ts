import { useTTSSettingsStore } from '@/renderer/stores/ttsSettingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  localStorage.clear();
  useTTSSettingsStore.setState({
    ttsEnabled: false,
    ttsSendEnabled: false,
    ttsVoice: null,
    ttsRate: 1,
    ttsVolume: 1,
  });
});

describe('ttsSettingsStore', () => {
  it('has correct defaults', () => {
    const state = useTTSSettingsStore.getState();
    expect(state.ttsEnabled).toBe(false);
    expect(state.ttsSendEnabled).toBe(false);
    expect(state.ttsVoice).toBeNull();
    expect(state.ttsRate).toBe(1.0);
    expect(state.ttsVolume).toBe(1.0);
  });

  it('toggles ttsEnabled', () => {
    useTTSSettingsStore.getState().setTtsEnabled(true);
    expect(useTTSSettingsStore.getState().ttsEnabled).toBe(true);

    useTTSSettingsStore.getState().setTtsEnabled(false);
    expect(useTTSSettingsStore.getState().ttsEnabled).toBe(false);
  });

  it('toggles ttsSendEnabled', () => {
    useTTSSettingsStore.getState().setTtsSendEnabled(true);
    expect(useTTSSettingsStore.getState().ttsSendEnabled).toBe(true);
  });

  it('sets ttsVoice', () => {
    useTTSSettingsStore.getState().setTtsVoice('Microsoft David - English (United States)');
    expect(useTTSSettingsStore.getState().ttsVoice).toBe(
      'Microsoft David - English (United States)'
    );

    useTTSSettingsStore.getState().setTtsVoice(null);
    expect(useTTSSettingsStore.getState().ttsVoice).toBeNull();
  });

  it('clamps ttsRate to 0.5-2.0', () => {
    useTTSSettingsStore.getState().setTtsRate(0.3);
    expect(useTTSSettingsStore.getState().ttsRate).toBe(0.5);

    useTTSSettingsStore.getState().setTtsRate(3.0);
    expect(useTTSSettingsStore.getState().ttsRate).toBe(2.0);

    useTTSSettingsStore.getState().setTtsRate(1.5);
    expect(useTTSSettingsStore.getState().ttsRate).toBe(1.5);
  });

  it('clamps ttsVolume to 0-1', () => {
    useTTSSettingsStore.getState().setTtsVolume(-0.5);
    expect(useTTSSettingsStore.getState().ttsVolume).toBe(0);

    useTTSSettingsStore.getState().setTtsVolume(1.5);
    expect(useTTSSettingsStore.getState().ttsVolume).toBe(1);

    useTTSSettingsStore.getState().setTtsVolume(0.7);
    expect(useTTSSettingsStore.getState().ttsVolume).toBe(0.7);
  });
});
