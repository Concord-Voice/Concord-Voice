import React, { useState, useEffect, useCallback, useMemo, useId, useRef } from 'react';
import { Monitor, X, Volume2, VolumeX } from 'lucide-react';
import {
  useVideoSettingsStore,
  type ScreenContentType,
  type ScreenShareOptions,
} from '../../stores/voice/videoSettingsStore';
import CustomSelect from '../ui/CustomSelect';
import { getFocusable } from '../ui/Modal';
import { errorMessage } from '../../utils/runtime/redactError';
import { useSubscriptionStore } from '../../stores/auth/subscriptionStore';
import { effectiveStreamAxis, clampScreenCapture } from '../../utils/policy/videoLimits';
import { resolveScreenDims } from '../../utils/ui/screenResolution';
import { canCarryScreenAudio, verdictOffersAudio } from '../../utils/policy/screenAudioCapability';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { groupDesktopSources, type GroupedSources } from '../../utils/ui/groupDesktopSources';
import './ScreenSharePicker.css';

interface DesktopSource {
  id: string;
  name: string;
  thumbnail: string;
  appIcon: string | null;
}

// Applications and Windows were one concept shown twice: an application IS its
// windows, and the split existed because the grouping code existed, not because
// a user ever needed to choose between the two views.
type TabId = 'screens' | 'windows';

const TABS: { id: TabId; label: string }[] = [
  { id: 'screens', label: 'Screens' },
  { id: 'windows', label: 'Windows' },
];

/** Discrete fps choices the screen-share picker offers (ascending). */
const SCREEN_FPS_OPTIONS = [5, 15, 30, 60] as const;

/**
 * Why the Stream Audio control is in the state it is in. This string is the ONLY
 * explanation a user gets for why an application share is silent, so it is a named
 * function rather than a ternary buried in a JSX attribute (#2161, ADR-0043).
 */
function audioToggleHint(
  selected: string | null,
  audioCapable: boolean,
  platform: string | null
): string {
  if (selected === null) return 'Choose what to share first';
  // "on this screen" would be a PRIVACY claim we cannot keep. Electron's desktop audio
  // capture is a whole-system loopback that ignores the chosen source, so on a
  // multi-monitor machine a user picking the second monitor still broadcasts sound from
  // apps on the first. Say what is actually sent.
  if (audioCapable)
    return 'Share your computer\u2019s sound \u2014 everything playing, not only this screen';
  if (platform === 'linux') return 'Sharing computer sound is not supported on Linux yet';
  return 'Application audio is not available yet \u2014 sharing a whole screen carries your whole computer\u2019s sound';
}

/**
 * One selectable capture target. Module-level rather than inline so the three tabs
 * cannot drift apart visually, and so it is not re-created on every picker render.
 */
const SourceTile: React.FC<{
  source: DesktopSource;
  selected: boolean;
  onSelect: (id: string) => void;
}> = ({ source, selected, onSelect }) => (
  <button
    type="button"
    className={`screen-picker__source ${selected ? 'screen-picker__source--selected' : ''}`}
    aria-pressed={selected}
    onClick={() => onSelect(source.id)}
  >
    <img src={source.thumbnail} alt="" className="screen-picker__thumbnail" />
    <div className="screen-picker__source-info">
      {source.appIcon && <img src={source.appIcon} alt="" className="screen-picker__app-icon" />}
      <span className="screen-picker__source-name">{source.name}</span>
    </div>
  </button>
);

/** Grid of selectable sources, or an explanation of why there are none. */
const SourceGrid: React.FC<{
  sources: DesktopSource[];
  /** Omitted where the caller cannot produce an empty list (an app group always has windows). */
  emptyText?: string;
  selected: string | null;
  onSelect: (id: string) => void;
}> = ({ sources, emptyText, selected, onSelect }) => {
  // Early returns rather than a nested ternary: the empty-with-no-message case is a
  // third outcome, and expressing three outcomes as one expression is what tripped
  // S3358 here in the first place.
  if (sources.length === 0) {
    if (!emptyText) return null;
    return <p className="screen-picker__empty">{emptyText}</p>;
  }
  return (
    <div className="screen-picker__grid">
      {sources.map((source) => (
        <SourceTile
          key={source.id}
          source={source}
          selected={selected === source.id}
          onSelect={onSelect}
        />
      ))}
    </div>
  );
};

/**
 * The body of the active tab. Module-level so the picker itself stays under the
 * cognitive-complexity ceiling, and so each tab's empty state is stated once.
 */
const SourcePanel: React.FC<{
  tab: TabId;
  grouped: GroupedSources;
  selected: string | null;
  onSelect: (id: string) => void;
}> = ({ tab, grouped, selected, onSelect }) => {
  if (tab === 'screens') {
    return (
      <SourceGrid
        sources={grouped.screens}
        emptyText="No screens available to share."
        selected={selected}
        onSelect={onSelect}
      />
    );
  }
  // Both tabs now render the same flat grid. Applications used to render one
  // window per row while Windows rendered a grid, which made the same content
  // look like two different kinds of thing.
  return (
    <SourceGrid
      sources={grouped.windows}
      emptyText="No open windows to share."
      selected={selected}
      onSelect={onSelect}
    />
  );
};

interface ScreenSharePickerProps {
  onSelect: (sourceId: string, options: ScreenShareOptions) => void;
  onCancel: () => void;
  /**
   * The source already being shared, when the picker is reopened to switch. Marks
   * that tile as selected so a multi-monitor or many-window list shows which one is
   * live -- without it every tile renders unselected and the two adjacent monitors
   * you are choosing between look identical.
   */
  currentSourceId?: string | null;
}

const ScreenSharePicker: React.FC<ScreenSharePickerProps> = ({
  onSelect,
  onCancel,
  currentSourceId = null,
}) => {
  const [sources, setSources] = useState<DesktopSource[]>([]);
  const [loading, setLoading] = useState(true);
  const [selected, setSelected] = useState<string | null>(currentSourceId);
  // Open on the tab that actually contains the pre-selection, or the marking is
  // invisible. Every window is in the Windows tab, so a `window:` id always
  // resolves -- there is no longer a second tab it could have belonged to.
  const [tab, setTab] = useState<TabId>(
    currentSourceId?.startsWith('window:') ? 'windows' : 'screens'
  );

  // Read persisted defaults from the video settings store
  const savedResolution = useVideoSettingsStore((s) => s.screenResolution);
  const savedFrameRate = useVideoSettingsStore((s) => s.screenFrameRate);
  const savedContentType = useVideoSettingsStore((s) => s.screenContentType);
  const setSavedStreamAudio = useVideoSettingsStore((s) => s.setScreenStreamAudio);
  const savedStreamAudio = useVideoSettingsStore((s) => s.screenStreamAudio);
  // Mid-share the picker is a SWITCH dialog, so the honest default is what the share is
  // doing now — not the persisted preference. Seeding from the preference silently
  // re-enabled audio a user had turned off with the live toggle.
  const isSharing = useVoiceStore((s) => s.isScreenSharing);
  const isScreenAudioOn = useVoiceStore((s) => s.isScreenAudioOn);
  // ...but only when the live target COULD carry audio. On a window share
  // `isScreenAudioOn` is false because the platform forces it, not because the user
  // chose it, and reading that as an opt-out left the toggle off after switching to a
  // whole screen -- then persisted the false, quietly clearing a default-on preference.
  const isScreenAudioCapable = useVoiceStore((s) => s.isScreenAudioCapable);
  const seedStreamAudio = isSharing && isScreenAudioCapable ? isScreenAudioOn : savedStreamAudio;
  const [streamAudio, setStreamAudio] = useState<boolean>(seedStreamAudio);

  // Local transient state — initialized from saved defaults, not persisted on change
  const [resolution, setResolution] = useState<string>(savedResolution);
  const [frameRate, setFrameRate] = useState<number>(savedFrameRate);
  const [contentType, setContentType] = useState<ScreenContentType>(savedContentType);
  const [dirty, setDirty] = useState(false);

  // Sync from store until the user makes a local change (handles async rehydration)
  useEffect(() => {
    if (!dirty) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: syncs resolution from store when settings rehydrate and no local change has been made; not a render loop
      setResolution(savedResolution);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: syncs frameRate from store when settings rehydrate and no local change has been made; not a render loop
      setFrameRate(savedFrameRate);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: syncs contentType from store when settings rehydrate and no local change has been made; not a render loop
      setContentType(savedContentType);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: syncs streamAudio from store when settings rehydrate and no local change has been made; not a render loop
      setStreamAudio(seedStreamAudio);
    }
  }, [dirty, savedResolution, savedFrameRate, savedContentType, seedStreamAudio]);

  useEffect(() => {
    const fetchSources = async () => {
      try {
        if (globalThis.electron?.getDesktopSources) {
          const result = await globalThis.electron.getDesktopSources();
          setSources(result);
        }
      } catch (err) {
        console.error('Failed to get desktop sources:', errorMessage(err));
      } finally {
        setLoading(false);
      }
    };
    fetchSources();
  }, []);

  const dialogRef = useRef<HTMLDialogElement>(null);
  const overlayRef = useRef<HTMLDivElement>(null);

  // Dismiss only on a click that landed on the BACKDROP itself. The previous
  // shape put `onClick={(e) => e.stopPropagation()}` on the picker container so
  // an inside click would not bubble out to this handler — which meant hanging a
  // mouse listener on a non-interactive element (sonar typescript:S6847/S1082),
  // and implying a keyboard affordance that has no meaning: there is no keyboard
  // equivalent of "clicked the backdrop". Identity-checking the target is what
  // Modal.tsx already does, and it needs no listener on the dialog.
  const handleOverlayClick = (e: React.MouseEvent) => {
    if (e.target === overlayRef.current) onCancel();
  };
  const titleId = useId();

  // Escape stays a hand-rolled listener. A native <dialog> runs the browser's
  // cancel action -- and so closes itself on Escape -- only when opened with
  // showModal(); this one uses the declarative `open` attribute, exactly as
  // Modal.tsx does, and a declaratively-open dialog is NON-modal. Deleting this
  // on the belief that <dialog> handles Escape would silently break it.
  const handleKeyDown = useCallback(
    (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCancel();
    },
    [onCancel]
  );

  useEffect(() => {
    document.addEventListener('keydown', handleKeyDown);
    return () => document.removeEventListener('keydown', handleKeyDown);
  }, [handleKeyDown]);

  // Focus custody (WCAG 2.4.3). The picker is mounted only while open, so mount
  // and unmount ARE open and close.
  useEffect(() => {
    const invoker = document.activeElement as HTMLElement | null;
    dialogRef.current?.focus();
    return () => invoker?.focus?.();
  }, []);

  // Tab containment (WCAG 2.1.2). Document-level rather than an onKeyDown on the
  // dialog, because the case that matters is focus having ALREADY escaped -- an
  // element-scoped handler cannot see a keystroke aimed at the bar behind.
  //
  // No modal-stack / isTopmost gate, unlike Modal.tsx: that exists to stop nested
  // modals fighting over the trap, and the picker is opened from the voice bar
  // rather than from inside another modal. If it ever gains a child modal, this
  // needs the same gate.
  useEffect(() => {
    const handleTab = (e: KeyboardEvent) => {
      if (e.key !== 'Tab') return;
      const container = dialogRef.current;
      if (!container) return;
      const focusables = getFocusable(container);
      if (focusables.length === 0) {
        e.preventDefault();
        container.focus();
        return;
      }
      const first = focusables[0];
      const last = focusables.at(-1) ?? first;
      const active = document.activeElement;
      if (e.shiftKey) {
        if (active === first || active === container || !container.contains(active)) {
          e.preventDefault();
          last.focus();
        }
      } else if (active === last || active === container || !container.contains(active)) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', handleTab);
    return () => document.removeEventListener('keydown', handleTab);
  }, []);

  // null until resolved; canCarryScreenAudio treats null as the permissive dev/web case.
  const [platform, setPlatform] = useState<string | null>(null);
  useEffect(() => {
    (globalThis.electron?.getPlatform?.() ?? Promise.resolve(null))
      .then((p) => setPlatform(p ?? null))
      .catch(() => setPlatform(null));
  }, []);

  const { screens, windows } = useMemo(() => groupDesktopSources(sources), [sources]);

  // Capability is target AND platform (#2161, ADR-0043) — the prefix alone offered an
  // enabled, default-on control on Linux, where this capture path has no loopback and
  // silently falls back to video. Shared with the service so the two cannot drift.
  //
  // Routed through `verdictOffersAudio` rather than compared here, because the equality
  // test this line used to carry defended exactly one direction. `=== 'system-loopback'`
  // does stop a future verdict from lighting the control up claiming a capture shape the
  // service cannot request — but it also compiles silently when the union widens, and then
  // leaves the control DARK on a machine that can carry audio. Widening to `'per-process'`
  // produced zero compile errors here while the other three consumers were converted to
  // exhaustive switches (#3198 Phase-8 review). The helper is exhaustive, so the next rung
  // is a compile error rather than a silent default.
  const audioCapable = verdictOffersAudio(canCarryScreenAudio(selected, platform));

  // ── #2163: tier the per-share picker to the stream entitlement ──────────
  // The produce boundary clamps screen capture to the entitlement's tiered
  // pixel-rate; without mirroring that here the picker would offer an fps the
  // capture silently drops (e.g. free 1080p60 becomes 1080p30). Derive the fps ceiling
  // from the SAME effectiveStreamAxis gate the produce boundary uses, reading the full
  // subscription snapshot (not just the entitlement) so the ceiling FAILS OPEN exactly
  // when produce does — pre-hydrate, or a degraded premium (#2172). Otherwise a premium
  // user sharing before login-hydrate would be clamped against the pre-hydrate free floor.
  const hydrated = useSubscriptionStore((s) => s.hydrated);
  const degraded = useSubscriptionStore((s) => s.degraded);
  const entitlement = useSubscriptionStore((s) => s.entitlement);
  const streamLimit = useMemo(
    () => effectiveStreamAxis({ hydrated, degraded, entitlement }),
    [hydrated, degraded, entitlement]
  );

  // null = getDisplayInfo has not resolved yet (distinct from a loaded-but-empty []).
  const [displayInfo, setDisplayInfo] = useState<{ width: number; height: number }[] | null>(null);
  useEffect(() => {
    // The optional chain already short-circuits (verified: does NOT throw) when the
    // getDisplayInfo bridge is absent, but spell the fallback explicitly: a missing
    // bridge (dev/web) resolves to [] → the 4K-tiered default, so display == capture
    // there, rather than leaving 'source' permanently failed-open. A PENDING promise
    // (packaged, loading) keeps displayInfo null → race fail-open, since produce will
    // resolve the real dims. Reads unambiguously (#2172 Codex).
    (globalThis.electron?.getDisplayInfo?.() ?? Promise.resolve([]))
      .then((displays) =>
        setDisplayInfo((displays ?? []).map((d) => ({ width: d.width, height: d.height })))
      )
      .catch(() => setDisplayInfo([]));
  }, []);

  // 'source' resolves to the largest display (matches produceScreen's
  // resolveCaptureDims); 4K fallback when display info is unavailable.
  const sourceDims = useMemo(() => {
    if (!displayInfo || displayInfo.length === 0) return { w: 3840, h: 2160 };
    const best = displayInfo.reduce(
      (b, d) => (d.width * d.height > b.width * b.height ? d : b),
      displayInfo[0]
    );
    return { w: best.width, h: best.height };
  }, [displayInfo]);

  // Highest fps a resolution can actually deliver under the stream entitlement
  // (tiered pixel-rate). Premium/native returns Infinity (no marking, no snap).
  // For 'source' the real display dims are only known after getDisplayInfo resolves;
  // until then fail OPEN (no tiering) instead of tiering against the 4K fallback —
  // otherwise a free user on a small display who shares Native during the async load
  // is truncated below what the authoritative produce-boundary clamp (which resolves
  // the real dims) would allow (#2172 Codex). Fixed resolutions never consult display.
  const fpsCeilingFor = useCallback(
    (res: string): number => {
      if (res === 'source' && displayInfo === null) return Infinity;
      const dims = resolveScreenDims(res, sourceDims);
      return clampScreenCapture(dims.w, dims.h, streamLimit.fps, streamLimit).fps;
    },
    [sourceDims, streamLimit, displayInfo]
  );
  const fpsCeiling = useMemo(() => fpsCeilingFor(resolution), [fpsCeilingFor, resolution]);
  // Clamp the transient fps to the tiered ceiling — but do NOT snap it down to a
  // listed option. This value flows into produceScreen (handleConfirm), which is the
  // AUTHORITATIVE entitlement clamp; snapping here to the {5,15,30,60} option list
  // would silently halve a premium user's persisted 120/90/75fps share to 60 (#2172).
  // The <select> shows this exact value — injected as its own option below when it is
  // not one of the discrete choices — so the display and the capture never disagree.
  const effectiveFrameRate = frameRate > 0 ? Math.min(frameRate, fpsCeiling) : frameRate;
  // Base fps choices, premium-marking any above the tiered ceiling; then inject the
  // effective (ceiling-clamped) value when it is not already listed — a wide 'source'
  // clamps to e.g. 22fps and a premium 120/90/75 must stay selectable without a blank
  // <select> and without truncating the captured value (#2163 / #2172).
  const fpsOptions = useMemo(() => {
    const base = SCREEN_FPS_OPTIONS.map((n) => ({
      value: String(n),
      label: n > fpsCeiling ? `${n} FPS \u{1F512} Premium` : `${n} FPS`,
    }));
    if (effectiveFrameRate > 0 && !base.some((o) => o.value === String(effectiveFrameRate))) {
      base.push({ value: String(effectiveFrameRate), label: `${effectiveFrameRate} FPS` });
    }
    return base;
  }, [fpsCeiling, effectiveFrameRate]);

  // #2172: 'Source Native' promises the display's full resolution, but for a free user on
  // an above-cap display produceScreen clamps the capture height down (e.g. 4K/1440p to
  // 1080p), so the shared video is not native. Mark the option Premium in that case so the
  // label matches what capture produces, mirroring the Settings native-exceeds gate. Uses
  // the SAME effectiveStreamAxis as the fps tiering, so it fails OPEN pre-hydrate or for a
  // degraded premium (streamLimit height Infinity, no clamp, no marker) and while the real
  // source dims are still loading (displayInfo === null). Display-only: 'source' is still
  // sent unchanged and the produce boundary stays authoritative (no resolution snap-down).
  const sourceIsClamped = useMemo(
    () =>
      displayInfo !== null &&
      clampScreenCapture(sourceDims.w, sourceDims.h, streamLimit.fps, streamLimit).height <
        sourceDims.h,
    [displayInfo, sourceDims, streamLimit]
  );
  const resolutionOptions = useMemo(
    () => [
      {
        value: 'source',
        label: sourceIsClamped ? 'Source Native \u{1F512} Premium' : 'Source Native',
      },
      { value: '1080p', label: '1080p' },
      { value: '720p', label: '720p' },
    ],
    [sourceIsClamped]
  );

  const handleScreenResolutionChange = (v: string) => {
    setResolution(v);
    setDirty(true);
    // Snap an over-cap fps down when the new resolution's tiered ceiling drops
    // (mirrors VideoConfigSection.handleScreenResolutionChange).
    const ceiling = fpsCeilingFor(v);
    if (frameRate > 0 && frameRate > ceiling) setFrameRate(ceiling);
  };

  const handleScreenFrameRateChange = (v: string) => {
    // Selecting a premium-marked (over-cap) fps snaps back to the tier ceiling.
    setFrameRate(Math.min(Number(v), fpsCeiling));
    setDirty(true);
  };

  const tabCounts: Record<TabId, number> = {
    screens: screens.length,
    windows: windows.length,
  };
  const countFor = (id: TabId): number => tabCounts[id];

  // WAI-ARIA tabs: Left/Right move between tabs and wrap; Home/End jump to the ends.
  const handleTabKeyDown = (e: React.KeyboardEvent, id: TabId) => {
    const keys: Record<string, number> = {
      ArrowLeft: -1,
      ArrowRight: 1,
    };
    let nextIndex: number | null = null;
    if (e.key in keys) {
      nextIndex = (TABS.findIndex((t) => t.id === id) + keys[e.key] + TABS.length) % TABS.length;
    } else if (e.key === 'Home') {
      nextIndex = 0;
    } else if (e.key === 'End') {
      nextIndex = TABS.length - 1;
    }
    if (nextIndex === null) return;
    e.preventDefault();
    const next = TABS[nextIndex].id;
    setTab(next);
    document.getElementById(`screen-picker-tab-${next}`)?.focus();
  };

  const handleConfirm = () => {
    if (!selected) return;
    // Gated on the TARGET KIND, not just the toggle: a window/app target cannot carry
    // scoped audio (#2161, ADR-0043). captureScreenElectron enforces this too, but
    // sending `true` here would make the picker's own stated intent wrong.
    // Persist the choice: without a production writer the preference sat at its default
    // forever and the toggle reset to On every time the dialog opened. Only recorded for
    // an audio-capable target, so a window share does not rewrite the user's preference.
    if (audioCapable) setSavedStreamAudio(streamAudio);
    onSelect(selected, {
      resolution,
      frameRate: effectiveFrameRate,
      contentType,
      streamAudio: audioCapable && streamAudio,
    });
  };

  return (
    <div className="screen-picker-overlay" ref={overlayRef} onClick={handleOverlayClick}>
      {/* Native <dialog> with the declarative `open`, mirroring Modal.tsx: it
          carries the implicit dialog role and AT semantics while staying in
          normal flow so the overlay's flex centring still works. aria-modal
          asserts the modality that `open` alone does not provide; the overlay
          and the Tab trap above are what actually enforce it. */}
      <dialog
        className="screen-picker"
        ref={dialogRef}
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        open
      >
        <div className="screen-picker__header">
          <h3 className="screen-picker__title" id={titleId}>
            <Monitor size={18} />
            Share Your Screen
          </h3>
          <button className="screen-picker__close" onClick={onCancel}>
            <X size={18} />
          </button>
        </div>

        {loading ? (
          <div className="screen-picker__loading">Loading sources...</div>
        ) : (
          <>
            <div className="screen-picker__tabs" role="tablist" aria-label="Share source type">
              {TABS.map(({ id, label }) => (
                <button
                  key={id}
                  id={`screen-picker-tab-${id}`}
                  role="tab"
                  type="button"
                  aria-selected={tab === id}
                  aria-controls={`screen-picker-panel-${id}`}
                  // Roving tabindex: only the active tab is in the tab order, and the
                  // arrow keys move between tabs. This is the WAI-ARIA tabs pattern;
                  // without it a keyboard user tabs through every tab button.
                  tabIndex={tab === id ? 0 : -1}
                  className={`screen-picker__tab ${tab === id ? 'screen-picker__tab--active' : ''}`}
                  onClick={() => setTab(id)}
                  onKeyDown={(e) => handleTabKeyDown(e, id)}
                >
                  {label}
                  <span className="screen-picker__tab-count">{countFor(id)}</span>
                </button>
              ))}
            </div>

            <div
              className="screen-picker__content"
              role="tabpanel"
              id={`screen-picker-panel-${tab}`}
              aria-labelledby={`screen-picker-tab-${tab}`}
            >
              <SourcePanel
                tab={tab}
                grouped={{ screens, windows }}
                selected={selected}
                onSelect={setSelected}
              />
            </div>
          </>
        )}

        <div className="screen-picker__quality">
          <div className="screen-picker__quality-row">
            <span className="screen-picker__quality-label" id="screen-audio-label">
              Stream Audio
            </span>
            {/*
              Disabled for window/application targets, and the title says WHY. This is the
              one place a user learns that application audio is a platform limitation
              rather than a missing checkbox -- Electron's desktop audio capture is a
              whole-system loopback that ignores the chosen source (#2161), so scoping it
              to one app needs a native addon (ADR-0043). Silence with no explanation is
              what made this confusing in the first place.
            */}
            <button
              type="button"
              className={`screen-picker__audio-toggle ${
                audioCapable && streamAudio ? 'screen-picker__audio-toggle--on' : ''
              }`}
              aria-labelledby="screen-audio-label"
              aria-pressed={audioCapable && streamAudio}
              disabled={!audioCapable}
              title={audioToggleHint(selected, audioCapable, platform)}
              onClick={() => {
                setStreamAudio((on) => !on);
                setDirty(true);
              }}
            >
              {audioCapable && streamAudio ? <Volume2 size={16} /> : <VolumeX size={16} />}
              <span>{audioCapable && streamAudio ? 'On' : 'Off'}</span>
            </button>
          </div>
          <div className="screen-picker__quality-row">
            <label htmlFor="screen-resolution" className="screen-picker__quality-label">
              Resolution
            </label>
            <CustomSelect
              id="screen-resolution"
              className="screen-picker__quality-select"
              // #2172: 'Source Native' carries a Premium marker when the display exceeds
              // the tiered height cap (capture clamps below native); see resolutionOptions.
              options={resolutionOptions}
              value={resolution}
              onChange={handleScreenResolutionChange}
            />
          </div>
          <div className="screen-picker__quality-row">
            <label htmlFor="screen-framerate" className="screen-picker__quality-label">
              Frame Rate
            </label>
            <CustomSelect
              id="screen-framerate"
              className="screen-picker__quality-select"
              // #2163: fps options above the resolution's tiered ceiling carry a
              // premium marker and snap back on selection, so the picker never
              // offers an fps the capture will silently drop; the effective value is
              // injected as its own option so display == capture (#2172).
              options={fpsOptions}
              value={String(effectiveFrameRate)}
              onChange={handleScreenFrameRateChange}
            />
          </div>
          <div className="screen-picker__quality-row">
            <label htmlFor="screen-content-type" className="screen-picker__quality-label">
              Content
            </label>
            <CustomSelect
              id="screen-content-type"
              className="screen-picker__quality-select"
              options={[
                { value: 'auto', label: 'Auto' },
                { value: 'motion', label: 'Motion (video)' },
                { value: 'detail', label: 'Detail (text/code)' },
              ]}
              value={contentType}
              onChange={(v) => {
                setContentType(v as ScreenContentType);
                setDirty(true);
              }}
            />
          </div>
        </div>

        <div className="screen-picker__footer">
          <button className="screen-picker__btn screen-picker__btn--cancel" onClick={onCancel}>
            Cancel
          </button>
          <button
            className="screen-picker__btn screen-picker__btn--confirm"
            onClick={handleConfirm}
            disabled={!selected}
          >
            Share
          </button>
        </div>
      </dialog>
    </div>
  );
};

export default ScreenSharePicker;
