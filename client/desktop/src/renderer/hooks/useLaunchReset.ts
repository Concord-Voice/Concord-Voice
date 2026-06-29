import { useEffect, useRef, useState } from 'react';
import { useSubscriptionStore } from '../stores/subscriptionStore';
import { useSettingsStore } from '../stores/settingsStore';
import { useVoiceStore } from '../stores/voiceStore';
import { useVideoSettingsStore } from '../stores/videoSettingsStore';
import { useAudioSettingsStore } from '../stores/audioSettingsStore';
import { clampToFreeTier, type ClampableSettings } from '../utils/clampToFreeTier';

/**
 * Launch-reset orchestrator (#1301 Decision 4 / spec §4). Runs once per session
 * after the FIRST entitlement state is known. For a FREE user it clamps any
 * over-cap client settings to the free defaults; if anything was clamped AND the
 * persisted `subscriptionResetAcknowledged` flag is unset, it surfaces the
 * one-time `<SubscriptionResetModal>`.
 *
 * Returns `{ showResetModal, acknowledge }` for the host to render the modal.
 *
 * Premium users (`tier !== 'free'`) are a no-op. A free user with no over-cap
 * settings (`changed === false`) is also a no-op — they never had premium
 * settings, so they see nothing.
 *
 * The clamp writes back through the REAL store setters (not raw setState) so the
 * existing persist + sync subscribers fire exactly as a user edit would.
 */
export function useLaunchReset(): { showResetModal: boolean; acknowledge: () => void } {
  const tier = useSubscriptionStore((s) => s.entitlement.tier);
  const hydrated = useSubscriptionStore((s) => s.hydrated);
  const degraded = useSubscriptionStore((s) => s.degraded);
  const [showResetModal, setShowResetModal] = useState(false);
  // Once-per-session guard: the clamp + modal decision runs a single time after
  // the first AUTHORITATIVE entitlement is observed.
  const ranRef = useRef(false);

  useEffect(() => {
    if (ranRef.current) return;

    // Gate on an AUTHORITATIVE, successful hydrate (Gitar review, #1301). Until
    // `hydrate()` resolves, `tier` is the pre-hydrate FREE default; and a FAILED
    // hydrate falls closed to free with `degraded === true`. Acting on either
    // would run the destructive free-tier clamp against a premium user whose
    // real entitlement simply has not arrived yet — silently and persistently
    // wiping their premium-only audio / video settings. So do NOT set `ranRef`
    // and do NOT clamp until `hydrated && !degraded`; the effect re-runs as
    // these flip and acts exactly once when the truth is known.
    if (!hydrated || degraded) return;
    ranRef.current = true;

    if (tier !== 'free') return;

    const entitlement = useSubscriptionStore.getState().entitlement;
    const appearance = useSettingsStore.getState().appearance;
    const video = useVideoSettingsStore.getState();
    const audio = useAudioSettingsStore.getState();
    const voice = useVoiceStore.getState();

    const current: ClampableSettings = {
      colorScheme: appearance.colorScheme,
      qualityTier: voice.qualityTier,
      cameraPreset: video.cameraPreset,
      screenShareBitrate: video.screenShareBitrate,
      musicMode: audio.musicMode,
    };

    const { settings: clamped, changed } = clampToFreeTier(current, entitlement);
    if (!changed) return;

    // Write clamped values back through the real setters — only the fields that
    // actually changed, so unrelated subscribers don't re-fire.
    if (clamped.colorScheme !== current.colorScheme) {
      useSettingsStore.getState().setColorScheme(clamped.colorScheme);
    }
    if (clamped.qualityTier !== current.qualityTier) {
      useVoiceStore.getState().setQualityTier(clamped.qualityTier);
    }
    if (clamped.cameraPreset !== current.cameraPreset) {
      useVideoSettingsStore.getState().setCameraPreset(clamped.cameraPreset);
    }
    if (clamped.screenShareBitrate !== current.screenShareBitrate) {
      useVideoSettingsStore.getState().setScreenShareBitrate(clamped.screenShareBitrate);
    }
    if (clamped.musicMode !== current.musicMode) {
      useAudioSettingsStore.getState().setMusicMode(clamped.musicMode);
    }

    // Surface the one-time explainer only if it hasn't been acknowledged.
    if (!useSettingsStore.getState().subscriptionResetAcknowledged) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: the once-per-session launch-reset decision can only run after the first AUTHORITATIVE entitlement is known (hydrated && !degraded); gated by ranRef so it fires once, not a render loop
      setShowResetModal(true);
    }
  }, [tier, hydrated, degraded]);

  const acknowledge = (): void => {
    useSettingsStore.getState().setSubscriptionResetAcknowledged(true);
    setShowResetModal(false);
  };

  return { showResetModal, acknowledge };
}
