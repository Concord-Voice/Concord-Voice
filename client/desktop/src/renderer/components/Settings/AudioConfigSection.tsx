import { useCallback, useState } from 'react';
import { useVoiceStore, AUDIO_QUALITY_TIERS, type AudioQualityTier } from '../../stores/voiceStore';
import { useAudioSettingsStore } from '../../stores/audioSettingsStore';
import {
  useDraftAudioSetting,
  setDraftAudioSetting,
  batchSetAudioDrafts,
  useStashAndSwapAudioMode,
} from '../../hooks/useDraftSettings';
import { useEntitlement } from '../../hooks/useEntitlement';
import { useGateActivation } from '../../hooks/useGateActivation';
import PremiumChip from '../common/PremiumChip';
import ToggleSwitch from './ToggleSwitch';
import CollapsibleSection from './CollapsibleSection';
import AudioOpusSection from './AudioOpusSection';

// ─── Constants ───────────────────────────────────────────────────────────────

const TIER_ORDER: AudioQualityTier[] = [
  'minimum',
  'low',
  'moderate',
  'standard',
  'high',
  'hifi',
  'studio',
];

const TIER_DESCRIPTIONS_BASIC: Record<AudioQualityTier, string> = {
  minimum:
    '16 kbps \u00b7 Mono | FEC On | DTX | AFS (60ms Preferred)\nOptimized for pure survival over quality. Uses silence detection and aggressive error correction to keep you connected when the digital world is ending. Use if:\n\u2022 Bandwidth is an absolute luxury.\n\u2022 Your system is basically a Commodore 64.\n\u2022 Your internet just filed for divorce and took the router.',
  low: "32 kbps \u00b7 Mono | FEC On | DTX | AFS (40ms Preferred)\nPrioritizes keeping you in the conversation without chewing through your data cap. You won't sound like a podcast host, but we will understand you. Use if:\n\u2022 You are on a strict data diet.\n\u2022 Your microphone was found at the bottom of a cereal box.\n\u2022 Your Wi-Fi is acting suspiciously spotty.",
  moderate:
    "64 kbps \u00b7 Mono | FEC On | DTX | AFS (20ms Preferred)\nThe ol' reliable. The industry standard sweet spot that perfectly balances crisp voice audio with reasonable bandwidth demands. Use if:\n\u2022 You just want it to work without overthinking it.\n\u2022 You are using standard consumer-grade headsets.\n\u2022 Your internet connection actually pays rent.",
  standard:
    '96 kbps \u00b7 Mono | FEC On | DTX | AFS (20ms Preferred)\nThe Concord default. Optimized for maximum clarity in voice chats, making you sound exactly like your actual self (for better or worse). Use if:\n\u2022 You want the intended, nearly-transparent Concord Voice experience.\n\u2022 You refuse to mess with settings menus.',
  high: '192 kbps \u00b7 Mono | FEC On | AFS (10ms Preferred)\nCranking the dial to "virtually transparent." Delivers exceptional clarity and frequency response without crossing into absolute overkill territory. Use if:\n\u2022 You are an enthusiast who appreciates the finer frequencies.\n\u2022 You invested in a seriously good microphone.\n\u2022 Your internet speeds are something you brag about at parties.',
  hifi: '256 kbps \u00b7 Stereo | FEC Off | AFS (10ms Preferred)\nMaximum fidelity for power users. This is unfiltered, transparent audio that squeezes the full factor of quality with quantity out of the Opus codec. Use if:\n\u2022 You demand maximum bang-for-the-buck quality.\n\u2022 You have enthusiast-grade, borderline-unreasonable audio gear.\n\u2022 Your internet is hardwired and unflinching.',
  studio:
    "510 kbps \u00b7 Stereo | FEC Off | AFS (10ms Preferred)\nThe absolute ceiling. Massive, glorious, overkill bandwidth for acoustically transparent 48kHz/16-bit audio. It's dangerous to go alone, so take these 510,000 bits per second... Every second. Use if:\n\u2022 You are running studio-level audio production over the web.\n\u2022 You own audiophile equipment that costs more than a used car.\n\u2022 You have a rock-solid, direct-to-the-vein fiber optic connection.",
};

/** Returns hint text for a processing toggle that can be locked by Music Mode. */
function processingHint(
  musicMode: boolean,
  enabled: boolean,
  lockedText: string,
  enabledText: string,
  disabledText: string
): string {
  if (musicMode) return lockedText;
  return enabled ? enabledText : disabledText;
}

/** Describe the noise gate threshold level for the hint text. */
function gateThresholdHint(level: number): string {
  if (level >= -25)
    return `${level} dBFS \u2014 Gates everything except loud, close-mic speech. Only passes through someone talking directly into a microphone.`;
  if (level >= -35)
    return `${level} dBFS \u2014 Gates background noise and quiet sounds. Passes through normal conversational speaking volume.`;
  if (level >= -50)
    return `${level} dBFS \u2014 Gates ambient room noise. Passes through most intentional speech, including softer voices.`;
  if (level >= -65)
    return `${level} dBFS \u2014 Gates only faint background hum. Most sounds above a quiet whisper will pass through.`;
  return `${level} dBFS \u2014 Gates only near-total silence. Virtually everything audible passes through unaffected.`;
}

/** Describe the quiet boost threshold level for the hint text. */
function boostThresholdHint(level: number): string {
  if (level >= -24)
    return `${level} dBFS \u2014 Boosts anyone not talking directly at their mic. Like someone speaking to you from across the room.`;
  if (level >= -30)
    return `${level} dBFS \u2014 Boosts participants who sound turned away from their mic. Like someone talking but facing the other direction.`;
  if (level >= -38)
    return `${level} dBFS \u2014 Boosts noticeably quiet participants. Like someone whispering nearby or speaking softly from a distance.`;
  if (level >= -45)
    return `${level} dBFS \u2014 Boosts only very quiet participants. Like someone whispering close to their mic.`;
  return `${level} dBFS \u2014 Boosts only barely-audible participants. Like catching a faint whisper from across a quiet room.`;
}

// ─── Component ───────────────────────────────────────────────────────────────

const AudioConfigSection: React.FC = () => {
  const qualityTier = useVoiceStore((s) => s.qualityTier);
  const setQualityTier = useVoiceStore((s) => s.setQualityTier);

  const advancedMode = useAudioSettingsStore((s) => s.advancedMode);

  const stashAndSwapAudioMode = useStashAndSwapAudioMode();

  // L1 (#1301): the quality slider stays live across the free tiers; premium
  // tiers (those NOT in `allowedAudioTiers`) render a 🔒 tick and snap back to
  // the highest free tier when selected, surfacing a chip popover — no
  // mid-action modal. The slider is fully keyboard-operable within the free
  // range (we don't cap `max`; we intercept the value on change).
  const allowedAudioTiers = useEntitlement((e) => e.allowedAudioTiers);
  const audioTierGate = useGateActivation('audio-tier');
  const [tierLockHinted, setTierLockHinted] = useState(false);

  const isTierLocked = useCallback(
    (tier: AudioQualityTier): boolean =>
      AUDIO_QUALITY_TIERS[tier]?.premium === true && !allowedAudioTiers.includes(tier),
    [allowedAudioTiers]
  );

  /** Highest free tier the snap-back lands on — the last allowed tier in
   *  display order (falls back to 'standard' if the floor list is unexpected). */
  const highestFreeTier: AudioQualityTier =
    [...TIER_ORDER].reverse().find((t) => allowedAudioTiers.includes(t)) ?? 'standard';

  /** Apply a tier selection, batching its drafts in basic mode (the existing
   *  side effect). Shared by the slider + label paths. */
  const applyTier = useCallback(
    (tier: AudioQualityTier) => {
      setQualityTier(tier);
      if (!useAudioSettingsStore.getState().advancedMode) {
        const tc = AUDIO_QUALITY_TIERS[tier];
        batchSetAudioDrafts({
          silenceDetection: tc.opusDtx,
          inlineFec: tc.opusFec,
          fecHeadroom: tc.opusFec,
          frameSize: 0,
          stereoOverride: null,
        });
      }
    },
    [setQualityTier]
  );

  /** Resolve a tier selection through the L1 gate: a locked tier snaps back to
   *  the highest free tier and reveals the chip; a free tier passes through. */
  const selectTierGated = useCallback(
    (tier: AudioQualityTier) => {
      if (isTierLocked(tier)) {
        applyTier(highestFreeTier);
        setTierLockHinted(true);
        return;
      }
      setTierLockHinted(false);
      applyTier(tier);
    },
    [isTierLocked, applyTier, highestFreeTier]
  );

  // Audio processing settings (drafted)
  const noiseCancellation = useDraftAudioSetting('noiseCancellation');
  const echoCancellation = useDraftAudioSetting('echoCancellation');
  const autoGainControl = useDraftAudioSetting('autoGainControl');
  const noiseGateMode = useDraftAudioSetting('noiseGateMode');
  const noiseGateLevel = useDraftAudioSetting('noiseGateLevel');
  const quietBoost = useDraftAudioSetting('quietBoost');
  const quietBoostThreshold = useDraftAudioSetting('quietBoostThreshold');
  const musicMode = useDraftAudioSetting('musicMode');

  const tierIndex = TIER_ORDER.indexOf(qualityTier);

  const handleTierSlider = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const idx = Number(e.target.value);
      if (idx >= 0 && idx < TIER_ORDER.length) {
        // L1: a drag/keyboard move onto a locked premium tier snaps back to the
        // highest free tier and reveals the chip; free tiers pass through.
        selectTierGated(TIER_ORDER[idx]);
      }
    },
    [selectTierGated]
  );

  const handleAdvancedToggle = useCallback(
    (enabled: boolean) => {
      useAudioSettingsStore.getState().setAdvancedMode(enabled); // immediate — UI toggle
      stashAndSwapAudioMode(enabled, qualityTier);
    },
    [qualityTier, stashAndSwapAudioMode]
  );

  return (
    <CollapsibleSection id="section-audio-config" title="Audio Configuration">
      {/* ── Mode Toggle ── */}
      <div className="settings-mode-toggle" role="tablist">
        <span
          className={`settings-mode-pill ${advancedMode ? '' : 'active'}`}
          role="tab"
          tabIndex={0}
          aria-selected={!advancedMode}
          onClick={() => handleAdvancedToggle(false)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              handleAdvancedToggle(false);
            }
          }}
        >
          Basic Settings
        </span>
        <span
          className={`settings-mode-pill ${advancedMode ? 'active' : ''}`}
          role="tab"
          tabIndex={0}
          aria-selected={advancedMode}
          onClick={() => handleAdvancedToggle(true)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              handleAdvancedToggle(true);
            }
          }}
        >
          Advanced Settings
        </span>
      </div>

      {advancedMode && (
        <p className="settings-mode-notice">
          These settings override the quality tier presets. Switching back to Basic will save your
          advanced configuration but apply your last Basic settings instead.
        </p>
      )}

      {/* ── Quality ── */}
      <h3 className="settings-subsection-title">Quality</h3>
      <p className="settings-section-description">
        Higher quality uses more bandwidth. Premium tiers require a subscription.
      </p>

      <div className="settings-tier-slider-container">
        <div className="settings-tier-labels">
          {TIER_ORDER.map((tier, i) => {
            const config = AUDIO_QUALITY_TIERS[tier];
            const locked = isTierLocked(tier);
            return (
              <span
                key={tier}
                className={`settings-tier-label ${tierIndex === i ? 'active' : ''} ${locked ? 'settings-tier-label-locked' : ''}`}
                role="tab"
                tabIndex={0}
                aria-selected={tierIndex === i}
                // O1: locked tiers stay focusable + aria-disabled (never
                // `disabled`/`pointer-events:none`); selecting one snaps back.
                {...(locked ? { 'aria-disabled': 'true' } : {})}
                onClick={() => selectTierGated(tier)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault();
                    e.currentTarget.click();
                  }
                }}
              >
                {config.label}
                {locked && (
                  <span
                    className="settings-tier-lock-glyph"
                    aria-label="Premium feature"
                    role="img"
                  >
                    {'\u{1F512}'}
                  </span>
                )}
              </span>
            );
          })}
        </div>
        <div className="settings-tier-track">
          <div className="settings-tier-ticks">
            {TIER_ORDER.map((tier, i) => (
              <span
                key={tier}
                className={`settings-tier-tick ${tierIndex === i ? 'active' : ''}`}
              />
            ))}
          </div>
          <input
            type="range"
            className="settings-tier-slider"
            min={0}
            max={TIER_ORDER.length - 1}
            step={1}
            value={tierIndex}
            onChange={handleTierSlider}
          />
        </div>
        <div className="settings-tier-kbps-container">
          <span
            className="settings-tier-kbps-label"
            style={{
              left: `calc(${100 / 14 + (tierIndex / (TIER_ORDER.length - 1)) * (100 - 200 / 14)}%)`,
            }}
          >
            {Math.round(AUDIO_QUALITY_TIERS[qualityTier].maxBitrate / 1000)} kbps
          </span>
        </div>
        {!advancedMode && (
          <div className="settings-tier-description">
            {TIER_DESCRIPTIONS_BASIC[qualityTier].split('\n').map((line) => (
              <span key={line}>{line}</span>
            ))}
            {AUDIO_QUALITY_TIERS[qualityTier].premium && (
              <span className="settings-quality-premium-badge">Premium</span>
            )}
          </div>
        )}

        {tierLockHinted && (
          <output className="settings-tier-lock-popover">
            <span className="settings-tier-lock-popover-text">
              High-fidelity tiers need a subscription.
            </span>
            <PremiumChip
              label="High / Hi-Fi / Studio"
              onActivate={audioTierGate.onActivate}
              id={audioTierGate.describedById}
            />
          </output>
        )}
      </div>

      {/* ── Processing ── */}
      <h3 className="settings-subsection-title">Processing</h3>

      <div className={`settings-row ${musicMode ? 'settings-row-disabled' : ''}`}>
        <div className="settings-row-info">
          <span className="settings-row-label">Noise Cancellation</span>
          <span className="settings-row-hint">
            {processingHint(
              musicMode,
              noiseCancellation,
              'Locked by Music Mode. Noise cancellation is forced off to preserve full-bandwidth audio fidelity.',
              "Enabled. Background noise from your microphone is actively filtered out by the system's noise suppression. May slightly reduce audio fidelity in noisy environments.",
              'Disabled. No noise filtering is applied \u2014 all ambient sound passes through unprocessed.'
            )}
          </span>
        </div>
        <ToggleSwitch
          checked={!musicMode && noiseCancellation}
          onChange={(v) => {
            if (!musicMode) setDraftAudioSetting('noiseCancellation', v);
          }}
          disabled={musicMode}
        />
      </div>

      <div className={`settings-row ${musicMode ? 'settings-row-disabled' : ''}`}>
        <div className="settings-row-info">
          <span className="settings-row-label">Echo Cancellation</span>
          <span className="settings-row-hint">
            {processingHint(
              musicMode,
              echoCancellation,
              'Locked by Music Mode. Echo cancellation is forced off to preserve unprocessed audio fidelity.',
              'Enabled. Acoustic echo cancellation prevents your speakers from feeding back into your microphone, allowing speaker use without headphones.',
              'Disabled. No echo cancellation is applied. Use headphones to prevent feedback loops.'
            )}
          </span>
        </div>
        <ToggleSwitch
          checked={!musicMode && echoCancellation}
          onChange={(v) => {
            if (!musicMode) setDraftAudioSetting('echoCancellation', v);
          }}
          disabled={musicMode}
        />
      </div>

      <div className={`settings-row ${musicMode ? 'settings-row-disabled' : ''}`}>
        <div className="settings-row-info">
          <span className="settings-row-label">Auto Gain Control</span>
          <span className="settings-row-hint">
            {processingHint(
              musicMode,
              autoGainControl,
              'Locked by Music Mode. Automatic gain control is forced off to prevent dynamic compression of your audio signal.',
              'Enabled. Automatically normalizes your microphone volume \u2014 making quiet speech louder and loud speech softer.',
              'Disabled. Your microphone level is not automatically adjusted. Manual volume control via the input slider is recommended.'
            )}
          </span>
        </div>
        <ToggleSwitch
          checked={!musicMode && autoGainControl}
          onChange={(v) => {
            if (!musicMode) setDraftAudioSetting('autoGainControl', v);
          }}
          disabled={musicMode}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Input Noise Gate</span>
          <span className="settings-row-hint">
            {noiseGateMode === 'manual'
              ? `Enabled. Input below ${noiseGateLevel} dBFS is muted with a hard cutoff. Sounds above this threshold pass through unaffected.`
              : 'Disabled. No hard cutoff is applied to your input. Relies on noise cancellation and auto gain control if those are enabled.'}
          </span>
        </div>
        <ToggleSwitch
          checked={noiseGateMode === 'manual'}
          onChange={(v) => setDraftAudioSetting('noiseGateMode', v ? 'manual' : 'auto')}
        />
      </div>

      {noiseGateMode === 'manual' && (
        <div className="settings-row settings-row-child">
          <div className="settings-row-info">
            <span className="settings-row-label">Gate Threshold</span>
            <span className="settings-row-hint">{gateThresholdHint(noiseGateLevel)}</span>
          </div>
          <div className="settings-slider-wrapper">
            <span className="settings-slider-value">{noiseGateLevel} dBFS</span>
            <input
              type="range"
              className="settings-slider"
              min={-80}
              max={-20}
              value={noiseGateLevel}
              onChange={(e) => setDraftAudioSetting('noiseGateLevel', Number(e.target.value))}
            />
          </div>
        </div>
      )}

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Boost Quiet Users</span>
          <span className="settings-row-hint">
            {quietBoost
              ? 'Enabled. Participants whose audio falls below the threshold are dynamically amplified to a comfortable level. Normal-volume users are unaffected.'
              : 'Disabled. All participants play at their natural volume level with no receiver-side amplification.'}
          </span>
        </div>
        <ToggleSwitch
          checked={quietBoost}
          onChange={(v) => setDraftAudioSetting('quietBoost', v)}
        />
      </div>

      {quietBoost && (
        <div className="settings-row settings-row-child">
          <div className="settings-row-info">
            <span className="settings-row-label">Boost Threshold</span>
            <span className="settings-row-hint">{boostThresholdHint(quietBoostThreshold)}</span>
          </div>
          <div className="settings-slider-wrapper">
            <span className="settings-slider-value">{quietBoostThreshold} dBFS</span>
            <input
              type="range"
              className="settings-slider"
              min={-50}
              max={-20}
              value={quietBoostThreshold}
              onChange={(e) => setDraftAudioSetting('quietBoostThreshold', Number(e.target.value))}
            />
          </div>
        </div>
      )}

      {/* ── Advanced-only: Opus Codec, Error Correction, Transport ── */}
      {advancedMode && <AudioOpusSection qualityTier={qualityTier} />}
    </CollapsibleSection>
  );
};

export default AudioConfigSection;
