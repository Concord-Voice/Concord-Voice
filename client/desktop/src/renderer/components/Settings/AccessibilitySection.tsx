import React, { useState, useEffect, useRef } from 'react';
import {
  useDraftTtsSetting,
  setDraftTtsSetting,
  useDraftAppearance,
  setDraftAppearanceSetting,
} from '../../hooks/useDraftSettings';
import { UI_SCALE_MIN, UI_SCALE_MAX, type AppearanceSettings } from '../../stores/settingsStore';
import ToggleSwitch from './ToggleSwitch';
import CollapsibleSection from './CollapsibleSection';
import CustomSelect from '../ui/CustomSelect';
import SettingsPreviewPanel from './SettingsPreviewPanel';
import {
  getVoices as getTTSVoices,
  preview as previewTTS,
  stop as stopTTS,
} from '../../services/ttsService';

const fontSizes: { value: AppearanceSettings['fontSize']; label: string }[] = [
  { value: 'small', label: 'Small' },
  { value: 'default', label: 'Default' },
  { value: 'large', label: 'Large' },
];

// ─── Display Section ─────────────────────────────────────────────────────────
// Moved from Appearance (#489) — font size + compact + reduce animations are
// accessibility concerns. Added: UI Scale (continuous, coexists with the
// discrete font-size selector via the compounding calc() in index.css) and
// High Contrast (token overrides for text/border).

const DisplaySection: React.FC = () => {
  const appearance = useDraftAppearance();

  return (
    <CollapsibleSection id="section-display" title="Display">
      <div className="form-group">
        <span className="form-label">Font Size</span>
        <div className="font-size-selector">
          {fontSizes.map((fs) => (
            <button
              key={fs.value}
              className={`font-size-option ${fs.value} ${appearance.fontSize === fs.value ? 'selected' : ''}`}
              onClick={() => setDraftAppearanceSetting('fontSize', fs.value)}
            >
              {fs.label}
            </button>
          ))}
        </div>
      </div>

      <div className="form-group">
        <span className="form-label">UI Scale</span>
        <p className="settings-section-description">
          Adjust the size of the entire interface. Compounds with Font Size — so "Large" + 1.2×
          makes everything ≈ 1.4× the baseline.
        </p>
        <div className="ui-scale-slider-row">
          <input
            type="range"
            min={UI_SCALE_MIN}
            max={UI_SCALE_MAX}
            step={0.05}
            value={appearance.uiScale}
            onChange={(e) =>
              setDraftAppearanceSetting('uiScale', Number.parseFloat(e.target.value))
            }
            aria-label="UI Scale"
            className="ui-scale-slider"
          />
          <span className="ui-scale-value">{Math.round(appearance.uiScale * 100)}%</span>
          <button
            type="button"
            className="ui-scale-reset-btn"
            onClick={() => setDraftAppearanceSetting('uiScale', 1)}
            disabled={appearance.uiScale === 1}
            aria-label="Reset UI scale to 100%"
          >
            Reset
          </button>
        </div>
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">High Contrast</span>
          <span className="settings-row-hint">
            {appearance.highContrast
              ? 'Enabled. The active color scheme is replaced with a maximum-contrast palette — black/white backgrounds, saturated yellow/cyan (dark) or blue/purple (light) accents, and thick focus rings. Per-user color identity is suppressed across the app while enabled.'
              : 'Disabled. Standard color hierarchy with subtle text variants and softer borders.'}
          </span>
        </div>
        <ToggleSwitch
          checked={appearance.highContrast}
          onChange={(v) => setDraftAppearanceSetting('highContrast', v)}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Compact Mode</span>
          <span className="settings-row-hint">
            {appearance.compactMode
              ? 'Enabled. Padding and spacing are reduced throughout the interface, fitting more content on screen.'
              : 'Disabled. Standard padding and spacing for a comfortable, spacious layout.'}
          </span>
        </div>
        <ToggleSwitch
          checked={appearance.compactMode}
          onChange={(v) => setDraftAppearanceSetting('compactMode', v)}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Reduce Animations</span>
          <span className="settings-row-hint">
            {appearance.reduceAnimations
              ? 'Enabled. Motion and transitions are minimized throughout the interface for a snappier experience.'
              : 'Disabled. Standard animations and transitions are used for a fluid interface.'}
          </span>
        </div>
        <ToggleSwitch
          checked={appearance.reduceAnimations}
          onChange={(v) => setDraftAppearanceSetting('reduceAnimations', v)}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label" id="label-dyslexic-support">
            Enable Dyslexic Support
          </span>
          <span className="settings-row-hint">
            {appearance.dyslexicSupport
              ? 'Enabled. OpenDyslexic overrides all font choices (including theme fonts) across the app, and the Appearance font picker is locked.'
              : 'Disabled. Use the Appearance ▸ Fonts picker to choose a font.'}
          </span>
        </div>
        <ToggleSwitch
          id="toggle-dyslexic-support"
          ariaLabelledBy="label-dyslexic-support"
          checked={appearance.dyslexicSupport}
          onChange={(v) => setDraftAppearanceSetting('dyslexicSupport', v)}
        />
      </div>
    </CollapsibleSection>
  );
};

// ─── Text-to-Speech Section ─────────────────────────────────────────────────

type PreviewState = 'idle' | 'speaking' | 'error';

const TTS_PREVIEW_TIMEOUT_MS = 15_000;

function getPreviewUnavailableHint(
  speechAvailable: boolean,
  voicesLoaded: boolean,
  voicesLength: number
): string | null {
  if (!speechAvailable) return 'Text-to-speech is not available on this system.';
  if (!voicesLoaded) return 'Loading text-to-speech voices...';
  if (voicesLength === 0) return 'No text-to-speech voices are available on this system.';
  return null;
}

function getPreviewHint(
  previewUnavailableHint: string | null,
  previewState: PreviewState,
  ttsVolume: number
): string {
  if (previewUnavailableHint) return previewUnavailableHint;
  if (previewState === 'speaking') return 'Speaking preview...';
  if (previewState === 'error') {
    return 'Preview could not play. Check your system text-to-speech and output settings.';
  }
  if (ttsVolume === 0) return 'Preview is muted because TTS volume is set to 0%.';
  return 'Preview uses the selected voice, speed, and volume.';
}

const TTSSection: React.FC = () => {
  const ttsEnabled = useDraftTtsSetting('ttsEnabled');
  const ttsVoice = useDraftTtsSetting('ttsVoice');
  const ttsRate = useDraftTtsSetting('ttsRate');
  const ttsVolume = useDraftTtsSetting('ttsVolume');
  const [voices, setVoices] = useState<SpeechSynthesisVoice[]>([]);
  const [voicesLoaded, setVoicesLoaded] = useState(false);
  const [previewState, setPreviewState] = useState<PreviewState>('idle');
  const previewTimeoutRef = useRef<number | null>(null);
  const mountedRef = useRef(true);

  const clearPreviewTimeout = () => {
    if (previewTimeoutRef.current === null) return;
    globalThis.clearTimeout(previewTimeoutRef.current);
    previewTimeoutRef.current = null;
  };

  const finishPreview = (state: PreviewState) => {
    clearPreviewTimeout();
    if (mountedRef.current) setPreviewState(state);
  };

  useEffect(() => {
    const loadVoices = () => {
      const available = getTTSVoices();
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: updates voices list from SpeechSynthesis API on mount and when voices change; not a render loop
      setVoices(available);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: marks the one-shot Web Speech voices probe complete
      setVoicesLoaded(true);
    };
    loadVoices();
    globalThis.speechSynthesis?.addEventListener('voiceschanged', loadVoices);
    return () => globalThis.speechSynthesis?.removeEventListener('voiceschanged', loadVoices);
  }, []);

  useEffect(() => {
    return () => {
      mountedRef.current = false;
      clearPreviewTimeout();
      stopTTS();
    };
  }, []);

  const speechAvailable = globalThis.speechSynthesis !== undefined;
  const previewUnavailableHint = getPreviewUnavailableHint(
    speechAvailable,
    voicesLoaded,
    voices.length
  );
  const previewHint = getPreviewHint(previewUnavailableHint, previewState, ttsVolume);
  const previewDisabled = Boolean(previewUnavailableHint) || previewState === 'speaking';

  const handlePreview = () => {
    if (previewDisabled) return;

    setPreviewState('speaking');
    clearPreviewTimeout();
    previewTimeoutRef.current = window.setTimeout(() => {
      previewTimeoutRef.current = null;
      if (mountedRef.current) setPreviewState('idle');
    }, TTS_PREVIEW_TIMEOUT_MS);

    const started = previewTTS({
      voiceURI: ttsVoice || null,
      rate: ttsRate,
      volume: ttsVolume,
      onEnd: () => finishPreview('idle'),
      onError: () => finishPreview('error'),
    });
    if (!started) {
      finishPreview('error');
    }
  };

  return (
    <CollapsibleSection id="section-tts" title="Text-to-Speech">
      <p className="settings-section-description">
        Read voice text chat messages aloud while you&apos;re in a voice channel.
      </p>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Enable TTS Playback</span>
          <span className="settings-row-hint">
            {ttsEnabled
              ? 'Enabled. Incoming voice text chat messages are read aloud while you are in a voice channel.'
              : 'Disabled. Voice text chat messages are displayed as text only.'}
          </span>
        </div>
        <ToggleSwitch checked={ttsEnabled} onChange={(v) => setDraftTtsSetting('ttsEnabled', v)} />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Voice</span>
          <span className="settings-row-hint">
            {`Select which text-to-speech voice reads messages aloud. Currently using ${ttsVoice ? (voices.find((v) => v.voiceURI === ttsVoice)?.name ?? ttsVoice) : 'System Default'}.`}
          </span>
        </div>
        <CustomSelect
          className="settings-select"
          options={[
            { value: '', label: 'System Default' },
            ...voices.map((v) => ({
              value: v.voiceURI,
              label: `${v.name} (${v.lang})`,
            })),
          ]}
          value={ttsVoice ?? ''}
          onChange={(v) => setDraftTtsSetting('ttsVoice', v || null)}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Speed</span>
          <span className="settings-row-hint">
            Playback speed for text-to-speech. Left (0.5x) for slow speech. Right (2.0x) for
            rapid-fire reading.
          </span>
        </div>
        <div className="settings-slider-wrapper">
          <span className="settings-slider-value">{ttsRate.toFixed(1)}x</span>
          <input
            type="range"
            className="settings-slider"
            min={0.5}
            max={2}
            step={0.1}
            value={ttsRate}
            onChange={(e) => setDraftTtsSetting('ttsRate', Number(e.target.value))}
          />
        </div>
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Volume</span>
          <span className="settings-row-hint">
            Volume level for text-to-speech playback. Left is muted. Right is full volume.
          </span>
        </div>
        <div className="settings-slider-wrapper">
          <span className="settings-slider-value">{Math.round(ttsVolume * 100)}%</span>
          <input
            type="range"
            className="settings-slider"
            min={0}
            max={1}
            step={0.05}
            value={ttsVolume}
            onChange={(e) => setDraftTtsSetting('ttsVolume', Number(e.target.value))}
          />
        </div>
      </div>

      <div className="settings-row">
        <button
          type="button"
          className="settings-btn-secondary"
          onClick={handlePreview}
          disabled={previewDisabled}
          aria-describedby="tts-preview-hint"
        >
          {previewState === 'speaking' ? 'Speaking...' : 'Preview'}
        </button>
        <span id="tts-preview-hint" className="settings-row-hint">
          {previewHint}
        </span>
      </div>
    </CollapsibleSection>
  );
};

// ─── Accessibility Section ──────────────────────────────────────────────────

const AccessibilitySection: React.FC = () => (
  <>
    <SettingsPreviewPanel />
    <DisplaySection />
    <TTSSection />
  </>
);

export default AccessibilitySection;
