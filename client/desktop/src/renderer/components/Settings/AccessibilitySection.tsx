import React, { useState, useEffect, useRef, useId } from 'react';
import {
  useDraftTtsSetting,
  setDraftTtsSetting,
  useDraftAppearance,
  setDraftAppearanceSetting,
} from '../../hooks/ui/useDraftSettings';
import { UI_SCALE_MIN, UI_SCALE_MAX, useSettingsStore } from '../../stores/ui/settingsStore';
import {
  UI_SCALE_LEGACY_MAX,
  UI_SCALE_LEGACY_MIN,
  hasZoomBridge,
  legacyUiScale,
} from '../../utils/ui/uiZoom';
import ToggleSwitch from './ToggleSwitch';
import CollapsibleSection from './CollapsibleSection';
import CustomSelect from '../ui/CustomSelect';
import GifPlaybackControl from './GifPlaybackControl';
import SettingsPreviewPanel from './SettingsPreviewPanel';
import {
  getVoices as getTTSVoices,
  preview as previewTTS,
  stop as stopTTS,
} from '../../services/system/ttsService';

// ─── Display Section ─────────────────────────────────────────────────────────
// Moved from Appearance (#489). Added there: UI Scale (continuous, compounding
// with the discrete Font Size via the calc() in index.css) and High Contrast
// (token overrides for text/border). Font Size itself went back to Appearance ▸
// Application Font in #2367: the three discrete steps are an everyday display
// preference that people look for beside the typeface, while the continuous
// slider — the fine-grained, accessibility-oriented control — stays here.
//
// #2367 part 2: on a shell with the zoom bridge the slider drives real page zoom
// over 50–200 %, capped by window width; on an older shell it keeps the legacy
// 0.85–1.3 `--ui-scale` range. See utils/ui/uiZoom.ts.

const toPercent = (factor: number): number => Math.round(factor * 100);

const DisplaySection: React.FC = () => {
  const appearance = useDraftAppearance();
  const limitHintId = useId();
  // Process-static: a shell either exposes the bridge or it does not.
  const zoomBridge = hasZoomBridge();
  const appliedUiZoom = useSettingsStore((s) => s.appliedUiZoom);
  const chosen = appearance.uiScale;
  // On a legacy shell show what `--ui-scale` actually carries, so a value stored
  // by a zoom-capable shell (up to 2.0) never reads as more than is applied.
  const shown = zoomBridge ? chosen : legacyUiScale(chosen);
  // Compared in whole percent: the width cap is continuous, and "limited to
  // 200 %, widen the window to use 200 %" would be a lie told by rounding.
  const limitHint =
    zoomBridge && appliedUiZoom !== null && toPercent(appliedUiZoom) < toPercent(chosen)
      ? `Limited to ${toPercent(appliedUiZoom)}% at this window size — widen the window to use ${toPercent(chosen)}%.`
      : null;

  // A drag moves only a local value; the draft — and so the page zoom — is written
  // on the native `change` event, which fires on pointer release and on each
  // keyboard step. Zooming on every `input` event re-lays out the slider under a
  // stationary pointer: measured in Electron 44, a 200 px drag then ran the value
  // backwards twice and stopped at 1.35 instead of 1.75. React's onChange cannot
  // carry the commit: it is the `input` event, and on release the value has not
  // changed since the last one, so React does not fire it at all.
  //
  // `base` is the stored value the drag started from. Chromium fires no `change`
  // when a drag ends where it began, so a drag can be left behind uncommitted;
  // keying it to `base` makes it ignored the moment the store moves on (Reset,
  // Revert), instead of pinning the thumb to a stale value.
  const [drag, setDrag] = useState<{ value: number; base: number } | null>(null);
  const sliderRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    const el = sliderRef.current;
    if (!el) return;
    const commit = () => {
      setDraftAppearanceSetting('uiScale', Number.parseFloat(el.value));
      setDrag(null);
    };
    el.addEventListener('change', commit);
    return () => el.removeEventListener('change', commit);
  }, []);
  const sliderValue = drag !== null && drag.base === shown ? drag.value : shown;

  return (
    <CollapsibleSection id="section-display" title="Display">
      <div className="form-group">
        <span className="form-label">UI Scale</span>
        <p className="settings-section-description">
          Adjust the size of the entire interface. Compounds with Font Size (Appearance ▸
          Application Font) — so "Large" + 1.2× makes everything ≈ 1.4× the baseline.
        </p>
        <div className="ui-scale-slider-row">
          <input
            type="range"
            min={zoomBridge ? UI_SCALE_MIN : UI_SCALE_LEGACY_MIN}
            max={zoomBridge ? UI_SCALE_MAX : UI_SCALE_LEGACY_MAX}
            step={0.05}
            ref={sliderRef}
            value={sliderValue}
            onChange={(e) => {
              // Only a drag step. A `change` that React also reports (a keyboard step
              // that moved the value) has already been committed by the listener
              // above; re-storing it here would pin the thumb against later store
              // updates such as Reset.
              if (e.nativeEvent.type === 'input') {
                setDrag({ value: Number.parseFloat(e.target.value), base: shown });
              }
            }}
            aria-label="UI Scale"
            aria-describedby={zoomBridge ? limitHintId : undefined}
            className="ui-scale-slider"
          />
          <span className="ui-scale-value">{toPercent(sliderValue)}%</span>
          <button
            type="button"
            className="ui-scale-reset-btn"
            onClick={() => setDraftAppearanceSetting('uiScale', 1)}
            disabled={chosen === 1}
            aria-label="Reset UI scale to 100%"
          >
            Reset
          </button>
        </div>
        {/* Always mounted on a zoom-capable shell, empty until the width cap
            bites: a live region must exist BEFORE its text changes to be
            announced, and the text changes as a side effect of resizing the
            window, not of touching this control. Text, not colour. */}
        {zoomBridge && (
          <output id={limitHintId} className="settings-row-hint ui-scale-limit-hint">
            {limitHint}
          </output>
        )}
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

      {/* Sits directly under Reduce Animations because 'auto' FOLLOWS it: the
          two read as one decision, and separating them would hide the follow.
          Display now holds 7 controls — an 8th forces chunking or the deferred
          Media section (spec §5 residual 8). */}
      <GifPlaybackControl
        mode={appearance.gifPlayback}
        reduceAnimations={appearance.reduceAnimations}
        onChange={(v) => setDraftAppearanceSetting('gifPlayback', v)}
      />

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label" id="label-dyslexic-support">
            Enable Dyslexic Support
          </span>
          <span className="settings-row-hint">
            {appearance.dyslexicSupport
              ? 'Enabled. OpenDyslexic overrides all font choices (including theme fonts) across the app, and the Appearance font picker is locked.'
              : 'Disabled. Use the Appearance ▸ Application Font picker to choose a font.'}
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
