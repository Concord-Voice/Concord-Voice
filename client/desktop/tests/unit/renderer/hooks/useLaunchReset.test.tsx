import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useLaunchReset } from '@/renderer/hooks/useLaunchReset';
import {
  useSubscriptionStore,
  FREE_ENTITLEMENT,
  type Entitlement,
} from '@/renderer/stores/subscriptionStore';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useVideoSettingsStore } from '@/renderer/stores/videoSettingsStore';
import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';

const PREMIUM_ENTITLEMENT: Entitlement = {
  ...FREE_ENTITLEMENT,
  tier: 'premium',
  allowCustomScheme: true,
  allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
  allowMusicMode: true,
  maxVideoHeight: 2160,
  maxVideoFps: 120,
  maxManualBitrateBps: 30_000_000,
};

/** Put stores into a custom-theme plus premium-only over-cap state. */
function seedOverCapSettings(): void {
  useSettingsStore.getState().setColorScheme('custom');
  useVoiceStore.getState().setQualityTier('studio');
  useVideoSettingsStore.getState().setCameraPreset('4K60');
  useVideoSettingsStore.getState().setScreenShareBitrate(16_000_000);
  useAudioSettingsStore.getState().setMusicMode(true);
  useSettingsStore.getState().setSubscriptionResetAcknowledged(false);
}

beforeEach(() => {
  localStorage.clear();
  useSubscriptionStore.getState().reset();
  useSettingsStore.getState().setColorScheme('concord');
  useSettingsStore.getState().setSubscriptionResetAcknowledged(false);
  useVoiceStore.getState().setQualityTier('standard');
  useVideoSettingsStore.getState().setCameraPreset('system');
  useVideoSettingsStore.getState().setScreenShareBitrate(0);
  useAudioSettingsStore.getState().setMusicMode(false);
});

afterEach(() => {
  localStorage.clear();
});

describe('useLaunchReset — free user with over-cap settings', () => {
  it('clamps premium-only over-cap settings back through the real setters', () => {
    seedOverCapSettings();
    useSubscriptionStore.getState().setEntitlement(FREE_ENTITLEMENT);

    renderHook(() => useLaunchReset());

    expect(useSettingsStore.getState().appearance.colorScheme).toBe('custom');
    expect(useVoiceStore.getState().qualityTier).toBe('standard');
    expect(useVideoSettingsStore.getState().cameraPreset).not.toBe('4K60');
    expect(useVideoSettingsStore.getState().screenShareBitrate).toBe(
      FREE_ENTITLEMENT.maxManualBitrateBps
    );
    expect(useAudioSettingsStore.getState().musicMode).toBe(false);
  });

  it('shows the reset modal once when settings were clamped', () => {
    seedOverCapSettings();
    useSubscriptionStore.getState().setEntitlement(FREE_ENTITLEMENT);

    const { result } = renderHook(() => useLaunchReset());
    expect(result.current.showResetModal).toBe(true);
  });

  it('acknowledge() persists the flag and hides the modal', () => {
    seedOverCapSettings();
    useSubscriptionStore.getState().setEntitlement(FREE_ENTITLEMENT);

    const { result } = renderHook(() => useLaunchReset());
    expect(result.current.showResetModal).toBe(true);

    act(() => result.current.acknowledge());

    expect(result.current.showResetModal).toBe(false);
    expect(useSettingsStore.getState().subscriptionResetAcknowledged).toBe(true);
  });
});

describe('useLaunchReset — no-op paths', () => {
  it('is a no-op for a premium user (no clamp, no modal)', () => {
    seedOverCapSettings();
    useSubscriptionStore.getState().setEntitlement(PREMIUM_ENTITLEMENT);

    const { result } = renderHook(() => useLaunchReset());

    expect(result.current.showResetModal).toBe(false);
    // Premium settings untouched.
    expect(useSettingsStore.getState().appearance.colorScheme).toBe('custom');
    expect(useVoiceStore.getState().qualityTier).toBe('studio');
    expect(useAudioSettingsStore.getState().musicMode).toBe(true);
  });

  it('does not show the modal for a free user with already-free settings', () => {
    // All stores already at free defaults (set in beforeEach).
    useSubscriptionStore.getState().setEntitlement(FREE_ENTITLEMENT);
    const { result } = renderHook(() => useLaunchReset());
    expect(result.current.showResetModal).toBe(false);
  });

  it('does NOT show the modal when already acknowledged (but still clamps)', () => {
    seedOverCapSettings();
    useSettingsStore.getState().setSubscriptionResetAcknowledged(true);
    useSubscriptionStore.getState().setEntitlement(FREE_ENTITLEMENT);

    const { result } = renderHook(() => useLaunchReset());

    expect(result.current.showResetModal).toBe(false);
    // Premium-only clamps still run; the modal is just suppressed.
    expect(useSettingsStore.getState().appearance.colorScheme).toBe('custom');
    expect(useAudioSettingsStore.getState().musicMode).toBe(false);
  });

  it('runs once per session — a rerender does not re-trigger the clamp', () => {
    seedOverCapSettings();
    useSubscriptionStore.getState().setEntitlement(FREE_ENTITLEMENT);

    const { result, rerender } = renderHook(() => useLaunchReset());
    act(() => result.current.acknowledge());
    expect(result.current.showResetModal).toBe(false);

    // Re-dirty a setting; a rerender must NOT re-clamp it (ranRef guard).
    act(() => useAudioSettingsStore.getState().setMusicMode(true));
    rerender();
    expect(useAudioSettingsStore.getState().musicMode).toBe(true);
    expect(result.current.showResetModal).toBe(false);
  });
});

describe('useLaunchReset — data-loss guard (Gitar review, #1301)', () => {
  it('does NOT clamp before an authoritative hydrate (pre-hydrate window)', () => {
    // A premium user, mid-launch, before hydrate() resolves: the store still
    // shows the FREE default (hydrated=false). The destructive clamp must NOT
    // run — otherwise their persisted premium settings are silently wiped.
    seedOverCapSettings();
    expect(useSubscriptionStore.getState().hydrated).toBe(false);

    renderHook(() => useLaunchReset());

    // Settings untouched — the hook waited for an authoritative entitlement.
    expect(useSettingsStore.getState().appearance.colorScheme).toBe('custom');
    expect(useVoiceStore.getState().qualityTier).toBe('studio');
    expect(useVideoSettingsStore.getState().cameraPreset).toBe('4K60');
    expect(useAudioSettingsStore.getState().musicMode).toBe(true);
  });

  it('does NOT clamp on a degraded (failed) hydrate (transient blip ≠ data loss)', () => {
    // A failed hydrate falls closed to free with degraded=true. A premium user
    // hitting a network blip on launch must keep their settings.
    seedOverCapSettings();
    act(() =>
      useSubscriptionStore.setState({
        entitlement: FREE_ENTITLEMENT,
        degraded: true,
        hydrated: false,
      })
    );

    renderHook(() => useLaunchReset());

    expect(useSettingsStore.getState().appearance.colorScheme).toBe('custom');
    expect(useVoiceStore.getState().qualityTier).toBe('studio');
    expect(useAudioSettingsStore.getState().musicMode).toBe(true);
  });

  it('waits for hydration, then clamps when an authoritative FREE entitlement arrives', () => {
    seedOverCapSettings();

    // Mount pre-hydrate — must not clamp yet.
    renderHook(() => useLaunchReset());
    expect(useVoiceStore.getState().qualityTier).toBe('studio');

    // The real (free) entitlement arrives — NOW the clamp runs, exactly once.
    act(() => useSubscriptionStore.getState().setEntitlement(FREE_ENTITLEMENT));

    expect(useVoiceStore.getState().qualityTier).toBe('standard');
    expect(useSettingsStore.getState().appearance.colorScheme).toBe('custom');
  });
});
