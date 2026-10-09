import { useCallback, useState } from 'react';
import {
  useVoiceStore,
  AUDIO_QUALITY_TIERS,
  type AudioQualityTier,
} from '../../stores/voice/voiceStore';
import {
  effectiveNoiseGateMode,
  useAudioSettingsStore,
  type NoiseGateMode,
} from '../../stores/audio/audioSettingsStore';
import {
  useDraftAudioSetting,
  setDraftAudioSetting,
  batchSetAudioDrafts,
  useStashAndSwapAudioMode,
} from '../../hooks/ui/useDraftSettings';
import { useEntitlement } from '../../hooks/ui/useEntitlement';
import { useGateActivation } from '../../hooks/ui/useGateActivation';
import PremiumChip from '../common/PremiumChip';
import DeviceSelector from '../Voice/DeviceSelector';
import { useMicTest } from '../../hooks/device/useMicTest';
import { useOutputTest } from '../../hooks/device/useOutputTest';
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

function setProcessingToggle(
  key: 'noiseCancellation' | 'echoCancellation' | 'autoGainControl',
  enabled: boolean,
  musicMode: boolean
): void {
  if (!musicMode) setDraftAudioSetting(key, enabled);
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

function noiseGateHint(mode: NoiseGateMode): string {
  switch (mode) {
    case 'dynamic':
      return 'Dynamic learns from microphone levels during a call or Test. When levels overlap, it favors letting sound through.';
    case 'manualCalibrate':
      return 'Manual Calibrate uses the fixed dBFS threshold below to decide when the gate opens.';
    case 'autoCalibrate':
      return 'Off leaves the gate open.';
    case 'off':
      return 'Off leaves the gate open.';
  }
}

function dynamicGateStatusMessage(
  error: string | null,
  status: ReturnType<typeof useVoiceStore.getState>['dynamicGateStatus']
): string {
  if (error) return error;
  if (status?.state === 'adjusted') {
    return 'Adjusted — Dynamic can change this threshold as levels change.';
  }
  if (status?.state === 'uncertain') {
    return 'Uncertain — microphone levels are too close to separate reliably, so the gate stays permissive. Try moving the microphone closer.';
  }
  return 'Learning — speak normally while the gate samples microphone levels.';
}

interface QualityTierLabelsProps {
  tierIndex: number;
  isTierLocked: (tier: AudioQualityTier) => boolean;
  selectTierGated: (tier: AudioQualityTier) => void;
}

const QualityTierLabels: React.FC<QualityTierLabelsProps> = ({
  tierIndex,
  isTierLocked,
  selectTierGated,
}) => (
  <div className="settings-tier-labels">
    {TIER_ORDER.map((tier, i) => {
      const config = AUDIO_QUALITY_TIERS[tier];
      const locked = isTierLocked(tier);
      return (
        <button
          type="button"
          key={tier}
          className={`settings-tier-label ${tierIndex === i ? 'active' : ''} ${locked ? 'settings-tier-label-locked' : ''}`}
          aria-pressed={tierIndex === i}
          {...(locked ? { 'aria-disabled': 'true' } : {})}
          onClick={() => selectTierGated(tier)}
        >
          {config.label}
          {locked && (
            <span className="settings-tier-lock-glyph" aria-label="Premium feature" role="img">
              {'\u{1F512}'}
            </span>
          )}
        </button>
      );
    })}
  </div>
);

interface AudioDeviceSettingsProps {
  localIsTesting: boolean;
  micTest: ReturnType<typeof useMicTest>;
  outputTest: ReturnType<typeof useOutputTest>;
  outputVolume: number;
}

const AudioDeviceSettings: React.FC<AudioDeviceSettingsProps> = ({
  localIsTesting,
  micTest,
  outputTest,
  outputVolume,
}) => (
  <div className="settings-audio-devices">
    <div className="settings-audio-device-group">
      <h3 className="settings-subsection-title">Input</h3>
      <div className="settings-device-row">
        <DeviceSelector kind="audioinput" />
      </div>
      <div className="settings-mic-test-row">
        <button
          className={`settings-mic-test-btn${micTest.isTesting ? ' testing' : ''}`}
          onClick={micTest.isTesting ? micTest.stopTest : micTest.startTest}
          disabled={localIsTesting && !micTest.isTesting}
          title={localIsTesting && !micTest.isTesting ? 'Another audio test is running' : undefined}
        >
          {micTest.isTesting ? 'Stop Testing' : 'Test'}
        </button>
        {micTest.error && <span className="settings-mic-test-error">{micTest.error}</span>}
      </div>
      <p className="settings-row-hint">
        Microphone Test uses applied settings until you select Apply. Its meter shows sample peaks
        before the noise gate, so it still responds while the gate is closed.
      </p>
      {micTest.isTesting && (
        <div className="settings-mic-meter-container">
          <div className="settings-mic-meter-track">
            <div
              className="settings-mic-meter-fill"
              style={{
                width: `${Math.min(100, Math.max(0, ((micTest.dbfsLevel + 80) / 80) * 100))}%`,
              }}
            />
          </div>
          <div className="settings-mic-meter-ticks">
            <span>-80</span>
            <span>-60</span>
            <span>-40</span>
            <span>-20</span>
            <span>0 dBFS</span>
          </div>
          {micTest.inputOverloaded && (
            <p className="settings-mic-test-error" role="alert">
              Input reached or exceeded 0 dBFS before the noise gate. This may indicate clipping;
              check your microphone level and upstream audio settings.
            </p>
          )}
        </div>
      )}
    </div>

    <div className="settings-audio-device-group">
      <h3 className="settings-subsection-title">Output</h3>
      <div className="settings-device-row">
        <DeviceSelector kind="audiooutput" />
      </div>
      <div className="settings-volume-row">
        <div className="settings-row-info">
          <span className="settings-volume-label">Output Volume</span>
          <span className="settings-row-hint">
            Scales all incoming audio from muted (left) to 2x boost (right). Values above 100% may
            introduce clipping.
          </span>
        </div>
        <div className="settings-slider-wrapper">
          <span className="settings-slider-value">{outputVolume}%</span>
          <input
            type="range"
            className="settings-volume-slider"
            min={0}
            max={200}
            step={1}
            value={outputVolume}
            aria-label="Output Volume"
            onChange={(event) =>
              setDraftAudioSetting('outputVolume', Number(event.currentTarget.value))
            }
          />
        </div>
      </div>
      <div className="settings-output-test-row">
        <button
          className={`settings-output-test-btn${outputTest.isTesting ? ' testing' : ''}`}
          onClick={outputTest.playTestTone}
          disabled={outputTest.isTesting || (localIsTesting && !outputTest.isTesting)}
          title={
            localIsTesting && !outputTest.isTesting ? 'Another audio test is running' : undefined
          }
        >
          {outputTest.isTesting ? 'Playing...' : 'Test'}
        </button>
        {outputTest.error && <span className="settings-output-test-error">{outputTest.error}</span>}
      </div>
    </div>
  </div>
);

interface NoiseGateSettingsProps {
  appliedGateMode: NoiseGateMode;
  appliedAgc: boolean;
  appliedMusicMode: boolean;
  autoGainControl: boolean;
  noiseGateMode: NoiseGateMode;
  noiseGateLevel: number;
  musicMode: boolean;
  isTesting: boolean;
  activeGateStatus: ReturnType<typeof useVoiceStore.getState>['dynamicGateStatus'];
  gateError: string | null;
}

const NoiseGateSettings: React.FC<NoiseGateSettingsProps> = ({
  appliedGateMode,
  appliedAgc,
  appliedMusicMode,
  autoGainControl,
  noiseGateMode,
  noiseGateLevel,
  musicMode,
  isTesting,
  activeGateStatus,
  gateError,
}) => {
  const effectiveAgc = !musicMode && autoGainControl;
  const appliedDynamic =
    effectiveNoiseGateMode({
      noiseGateMode: appliedGateMode,
      autoGainControl: appliedAgc,
      musicMode: appliedMusicMode,
    }) === 'dynamic';
  const pendingGateChange =
    noiseGateMode !== appliedGateMode ||
    autoGainControl !== appliedAgc ||
    musicMode !== appliedMusicMode;

  return (
    <>
      <div className="settings-row">
        <div className="settings-row-info">
          <label className="settings-row-label" htmlFor="settings-noise-gate-mode">
            Noise Gate
          </label>
          <span className="settings-row-hint">
            {noiseGateHint(noiseGateMode)}
            {musicMode && ' Music Mode disables Auto Gain Control.'}
          </span>
        </div>
        <select
          id="settings-noise-gate-mode"
          className="settings-select"
          value={noiseGateMode}
          onChange={(event) =>
            setDraftAudioSetting('noiseGateMode', event.currentTarget.value as NoiseGateMode)
          }
        >
          <option value="dynamic">Dynamic</option>
          {!effectiveAgc && <option value="manualCalibrate">Manual Calibrate</option>}
          <option value="off">Off</option>
        </select>
      </div>

      {pendingGateChange && (
        <p className="settings-row-hint">
          {noiseGateMode === 'dynamic' && appliedGateMode === 'manualCalibrate' && effectiveAgc
            ? 'Switching to Dynamic takes effect when you select Apply.'
            : 'Noise Gate changes take effect when you select Apply.'}
        </p>
      )}

      {appliedDynamic && (isTesting || activeGateStatus || gateError) ? (
        <div className="settings-row-hint">
          <span role="status" aria-live="polite">
            {dynamicGateStatusMessage(gateError, activeGateStatus)}
          </span>
          {!gateError && activeGateStatus?.state === 'adjusted' && (
            <span>
              {' '}
              Current threshold: {String(activeGateStatus.thresholdDbfs).replace('-', '−')} dBFS.
            </span>
          )}
        </div>
      ) : (
        appliedDynamic &&
        noiseGateMode === 'dynamic' && (
          <p className="settings-row-hint">The gate learns during a call or microphone Test</p>
        )
      )}

      {noiseGateMode === 'manualCalibrate' && (
        <div className="settings-row settings-row-child">
          <div className="settings-row-info">
            <label className="settings-row-label" htmlFor="settings-noise-gate-threshold">
              Gate Threshold
            </label>
            <span className="settings-row-hint">Set the threshold yourself.</span>
          </div>
          <div className="settings-slider-wrapper">
            <span className="settings-slider-value">{noiseGateLevel} dBFS</span>
            <input
              id="settings-noise-gate-threshold"
              type="range"
              className="settings-slider"
              min={-80}
              max={-20}
              value={noiseGateLevel}
              onChange={(event) =>
                setDraftAudioSetting('noiseGateLevel', Number(event.currentTarget.value))
              }
            />
          </div>
        </div>
      )}
    </>
  );
};

interface AudioQualitySettingsProps {
  qualityTier: AudioQualityTier;
  setQualityTier: (tier: AudioQualityTier) => void;
  advancedMode: boolean;
}

const AudioQualitySettings: React.FC<AudioQualitySettingsProps> = ({
  qualityTier,
  setQualityTier,
  advancedMode,
}) => {
  const allowedAudioTiers = useEntitlement((entitlement) => entitlement.allowedAudioTiers);
  const audioTierGate = useGateActivation('audio-tier');
  const [tierLockHinted, setTierLockHinted] = useState(false);
  const tierIndex = TIER_ORDER.indexOf(qualityTier);

  const isTierLocked = useCallback(
    (tier: AudioQualityTier): boolean =>
      AUDIO_QUALITY_TIERS[tier]?.premium === true && !allowedAudioTiers.includes(tier),
    [allowedAudioTiers]
  );
  const highestFreeTier: AudioQualityTier =
    [...TIER_ORDER].reverse().find((tier) => allowedAudioTiers.includes(tier)) ?? 'standard';

  const applyTier = useCallback(
    (tier: AudioQualityTier) => {
      setQualityTier(tier);
      if (!useAudioSettingsStore.getState().advancedMode) {
        const config = AUDIO_QUALITY_TIERS[tier];
        // Basic FEC headroom and DTX are resolved from the tier at runtime. Do not
        // write the Advanced toggles here or a tier change can silently switch them on.
        batchSetAudioDrafts({
          inlineFec: config.opusFec,
          frameSize: 0,
          stereoOverride: null,
        });
      }
    },
    [setQualityTier]
  );
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
  const handleTierSlider = useCallback(
    (event: React.ChangeEvent<HTMLInputElement>) => {
      const index = Number(event.target.value);
      if (index >= 0 && index < TIER_ORDER.length) {
        selectTierGated(TIER_ORDER[index]);
      }
    },
    [selectTierGated]
  );

  return (
    <>
      <h3 className="settings-subsection-title">Quality</h3>
      <p className="settings-section-description">
        Higher quality uses more bandwidth. Premium tiers require a subscription.
      </p>

      <div className="settings-tier-slider-container">
        <QualityTierLabels
          tierIndex={tierIndex}
          isTierLocked={isTierLocked}
          selectTierGated={selectTierGated}
        />
        <div className="settings-tier-track">
          <div className="settings-tier-ticks">
            {TIER_ORDER.map((tier, index) => (
              <span
                key={tier}
                className={`settings-tier-tick ${tierIndex === index ? 'active' : ''}`}
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
            aria-label="Audio quality"
            aria-valuetext={AUDIO_QUALITY_TIERS[TIER_ORDER[tierIndex]].label}
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
    </>
  );
};

// ─── Component ───────────────────────────────────────────────────────────────

const AudioConfigSection: React.FC = () => {
  const qualityTier = useVoiceStore((s) => s.qualityTier);
  const setQualityTier = useVoiceStore((s) => s.setQualityTier);

  const advancedMode = useAudioSettingsStore((s) => s.advancedMode);
  const appliedGateMode = useAudioSettingsStore((s) => s.noiseGateMode);
  const appliedAgc = useAudioSettingsStore((s) => s.autoGainControl);
  const appliedMusicMode = useAudioSettingsStore((s) => s.musicMode);
  const liveGateStatus = useVoiceStore((s) => s.dynamicGateStatus);
  const liveGateError = useVoiceStore((s) =>
    s.joinError?.startsWith('Microphone processing ') ? s.joinError : null
  );

  const stashAndSwapAudioMode = useStashAndSwapAudioMode();

  // Audio processing settings (drafted)
  const noiseCancellation = useDraftAudioSetting('noiseCancellation');
  const echoCancellation = useDraftAudioSetting('echoCancellation');
  const autoGainControl = useDraftAudioSetting('autoGainControl');
  const noiseGateMode = useDraftAudioSetting('noiseGateMode');
  const noiseGateLevel = useDraftAudioSetting('noiseGateLevel');
  const quietBoost = useDraftAudioSetting('quietBoost');
  const quietBoostThreshold = useDraftAudioSetting('quietBoostThreshold');
  const musicMode = useDraftAudioSetting('musicMode');
  const processingRowClassName = musicMode ? 'settings-row settings-row-disabled' : 'settings-row';
  const inputVolume = useDraftAudioSetting('inputVolume');
  const outputVolume = useDraftAudioSetting('outputVolume');
  const localIsTesting = useVoiceStore((s) => s.localIsTesting);
  const micTest = useMicTest();
  const outputTest = useOutputTest();
  const testGateError = micTest.error
    ? `${micTest.error}. Check your microphone and try again.`
    : null;
  const gateError = !micTest.isTesting && liveGateError ? liveGateError : testGateError;
  const handleAdvancedToggle = useCallback(
    (enabled: boolean) => {
      useAudioSettingsStore.getState().setAdvancedMode(enabled); // immediate — UI toggle
      stashAndSwapAudioMode(enabled, qualityTier);
    },
    [qualityTier, stashAndSwapAudioMode]
  );

  return (
    <CollapsibleSection
      id="section-audio-config"
      title="Audio Configuration"
      onCollapse={() => {
        micTest.stopTest();
        outputTest.stopTest();
      }}
    >
      <AudioDeviceSettings
        localIsTesting={localIsTesting}
        micTest={micTest}
        outputTest={outputTest}
        outputVolume={outputVolume}
      />
      <p className="settings-section-description">
        Device selections apply immediately. Processing changes apply when you select Apply.
      </p>

      {/* ── Mode Toggle ── native radios: this persists a setting, it does not
          switch views, so a tablist would announce a tabpanel that isn't there. */}
      <fieldset className="settings-mode-toggle">
        <legend className="settings-mode-legend">Audio settings mode</legend>
        <label className="settings-mode-pill">
          <input
            type="radio"
            name="audio-settings-mode"
            className="settings-mode-radio"
            checked={!advancedMode}
            onChange={() => handleAdvancedToggle(false)}
          />
          {'Basic Settings'}
        </label>
        <label className="settings-mode-pill">
          <input
            type="radio"
            name="audio-settings-mode"
            className="settings-mode-radio"
            checked={advancedMode}
            onChange={() => handleAdvancedToggle(true)}
          />
          {'Advanced Settings'}
        </label>
      </fieldset>

      {advancedMode && (
        <p className="settings-mode-notice">
          These settings override the quality tier presets. Switching back to Basic will save your
          advanced configuration but apply your last Basic settings instead.
        </p>
      )}

      <AudioQualitySettings
        qualityTier={qualityTier}
        setQualityTier={setQualityTier}
        advancedMode={advancedMode}
      />

      {/* ── Processing ── */}
      <h3 className="settings-subsection-title">Processing</h3>

      <div className={processingRowClassName}>
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
          onChange={(v) => setProcessingToggle('noiseCancellation', v, musicMode)}
          disabled={musicMode}
        />
      </div>

      <div className={processingRowClassName}>
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
          onChange={(v) => setProcessingToggle('echoCancellation', v, musicMode)}
          disabled={musicMode}
        />
      </div>

      <div className={processingRowClassName}>
        <div className="settings-row-info">
          <span className="settings-row-label">Auto Gain Control</span>
          <span className="settings-row-hint">
            {processingHint(
              musicMode,
              autoGainControl,
              'Locked by Music Mode. Automatic gain control is forced off to prevent dynamic compression of your audio signal.',
              'Enabled. Requests automatic microphone leveling and limits outgoing sample peaks to −6 dBFS. As a starting point, aim for speech peaks around −18 to −12 dBFS; repeated peaks above −12 dBFS may indicate a high input level. This cannot repair clipping at the microphone. Positive saved Microphone Level values use unity; 0% stays silent.',
              'Disabled. Your microphone level is not automatically adjusted. Adjust it with Microphone Level below.'
            )}
          </span>
        </div>
        <ToggleSwitch
          checked={!musicMode && autoGainControl}
          onChange={(v) => setProcessingToggle('autoGainControl', v, musicMode)}
          disabled={musicMode}
        />
      </div>

      {(musicMode || !autoGainControl) && (
        <div className="settings-volume-row settings-row-child">
          <div className="settings-row-info">
            <label className="settings-volume-label" htmlFor="settings-microphone-level">
              Microphone Level
            </label>
            <span className="settings-row-hint">
              100% is unity gain; 0% is silence and 200% is 2x. Boosting can amplify noise and clip
              audio. After changing the level from 100%, retest the noise gate at its saved
              threshold; the threshold is not adjusted automatically.
            </span>
          </div>
          <div className="settings-slider-wrapper">
            <span className="settings-slider-value">{inputVolume}%</span>
            <input
              id="settings-microphone-level"
              type="range"
              className="settings-volume-slider"
              min={0}
              max={200}
              step={1}
              value={inputVolume}
              aria-label="Microphone Level"
              onChange={(event) =>
                setDraftAudioSetting('inputVolume', Number(event.currentTarget.value))
              }
            />
          </div>
        </div>
      )}

      <NoiseGateSettings
        appliedGateMode={appliedGateMode}
        appliedAgc={appliedAgc}
        appliedMusicMode={appliedMusicMode}
        autoGainControl={autoGainControl}
        noiseGateMode={noiseGateMode}
        noiseGateLevel={noiseGateLevel}
        musicMode={musicMode}
        isTesting={micTest.isTesting}
        activeGateStatus={micTest.isTesting ? micTest.dynamicGateStatus : liveGateStatus}
        gateError={gateError}
      />

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
