import { vi, describe, it, expect, beforeEach } from 'vitest';

// Mock apiClient to prevent real HTTP calls (mirrors resetService.test.ts)
vi.mock('@/renderer/services/apiClient', () => ({
  stopProactiveRefresh: vi.fn(),
  refreshAccessToken: vi.fn(),
}));

import { nuclearReset } from '@/renderer/services/resetService';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { useVideoSettingsStore } from '@/renderer/stores/voice/videoSettingsStore';
import { useSettingsStore } from '@/renderer/stores/ui/settingsStore';
import { useTTSSettingsStore } from '@/renderer/stores/audio/ttsSettingsStore';
import { useLayoutStore } from '@/renderer/stores/ui/layoutStore';
import { resetAllStores } from '../../helpers/store-helpers';

/**
 * What a fresh renderer process rehydrates from after an app restart: the
 * zustand/persist envelope in localStorage. If a key is missing here, the
 * next launch starts from defaults — that IS the user-visible revert.
 */
function persistedState(key: string): Record<string, unknown> | null {
  const raw = localStorage.getItem(key);
  if (!raw) return null;
  return (JSON.parse(raw) as { state: Record<string, unknown> }).state;
}

beforeEach(() => {
  resetAllStores();
});

// regression for #1603 — device-local settings (audio, video, theme/appearance,
// TTS) must survive logout-class resets. nuclearReset() runs on every explicit
// logout and every session-only refresh failure ("login screen appears → go
// nuclear"), and it deleted the persisted device-settings keys — so every
// logout silently reverted the user's Audio/Video settings and custom theme
// (public feedback Concord-Voice/Concord-Voice#26) to defaults on next launch.
describe('nuclearReset preserves device-local settings (#1603)', () => {
  it('changed audio settings survive a logout-class reset + restart', () => {
    useAudioSettingsStore.getState().setMusicMode(true);
    useAudioSettingsStore.getState().setInputVolume(150);

    nuclearReset();

    const audio = persistedState('concord:audio-advanced');
    expect(audio?.musicMode).toBe(true);
    expect(audio?.inputVolume).toBe(150);
  });

  it('changed video settings survive a logout-class reset + restart', () => {
    useVideoSettingsStore.getState().setCameraPreset('1080p30');
    useVideoSettingsStore.getState().setScreenFrameRate(60);

    nuclearReset();

    const video = persistedState('concord:video-settings');
    expect(video?.cameraPreset).toBe('1080p30');
    expect(video?.screenFrameRate).toBe(60);
  });

  it('theme/appearance settings survive a logout-class reset + restart (#26)', () => {
    useSettingsStore.getState().setTheme('light');

    nuclearReset();

    const settings = persistedState('concord-settings') as {
      appearance?: { theme?: string };
    } | null;
    expect(settings?.appearance?.theme).toBe('light');
  });

  it('TTS settings survive a logout-class reset + restart', () => {
    useTTSSettingsStore.getState().setTtsRate(1.5);

    nuclearReset();

    const tts = persistedState('concord:tts-settings');
    expect(tts?.ttsRate).toBe(1.5);
  });

  it('layout UI preferences survive a logout-class reset + restart', () => {
    // Panel widths / UI prefs are device-scoped; user content (folders,
    // ordering) is cleared separately by gracefulReset's clearUserContent().
    // Seed a non-default value so the assertion locks preserve-not-recreate
    // (a bare key-exists check would also pass if the reset recreated the
    // key with defaults).
    useLayoutStore.getState().setSidebarWidth('dm', 'left', 320);

    nuclearReset();

    const layout = persistedState('concord-layout');
    expect(
      (layout?.sidebarProfiles as { dm?: { left?: { width?: number } } } | undefined)?.dm?.left
        ?.width
    ).toBe(320);
  });
});
