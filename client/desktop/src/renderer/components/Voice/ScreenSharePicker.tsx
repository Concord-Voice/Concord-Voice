import React, { useState, useEffect, useCallback, useMemo } from 'react';
import { Monitor, X } from 'lucide-react';
import {
  useVideoSettingsStore,
  type ScreenContentType,
  type ScreenShareOptions,
} from '../../stores/videoSettingsStore';
import CustomSelect from '../ui/CustomSelect';
import { errorMessage } from '../../utils/redactError';
import { useSubscriptionStore } from '../../stores/subscriptionStore';
import { effectiveStreamAxis, clampScreenCapture } from '../../utils/videoLimits';
import { resolveScreenDims } from '../../utils/screenResolution';
import './ScreenSharePicker.css';

interface DesktopSource {
  id: string;
  name: string;
  thumbnail: string;
  appIcon: string | null;
}

/** Discrete fps choices the screen-share picker offers (ascending). */
const SCREEN_FPS_OPTIONS = [5, 15, 30, 60] as const;

interface ScreenSharePickerProps {
  onSelect: (sourceId: string, options: ScreenShareOptions) => void;
  onCancel: () => void;
}

const ScreenSharePicker: React.FC<ScreenSharePickerProps> = ({ onSelect, onCancel }) => {
  const [sources, setSources] = useState<DesktopSource[]>([]);
  const [loading, setLoading] = useState(true);
  const [selected, setSelected] = useState<string | null>(null);

  // Read persisted defaults from the video settings store
  const savedResolution = useVideoSettingsStore((s) => s.screenResolution);
  const savedFrameRate = useVideoSettingsStore((s) => s.screenFrameRate);
  const savedContentType = useVideoSettingsStore((s) => s.screenContentType);

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
    }
  }, [dirty, savedResolution, savedFrameRate, savedContentType]);

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

  // Close on Escape
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

  const screens = sources.filter((s) => s.id.startsWith('screen:'));
  const windows = sources.filter((s) => s.id.startsWith('window:'));

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

  const handleConfirm = () => {
    if (!selected) return;
    onSelect(selected, { resolution, frameRate: effectiveFrameRate, contentType });
  };

  return (
    <div className="screen-picker-overlay" onClick={onCancel}>
      <div className="screen-picker" onClick={(e) => e.stopPropagation()}>
        <div className="screen-picker__header">
          <h3 className="screen-picker__title">
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
          <div className="screen-picker__content">
            {screens.length > 0 && (
              <div className="screen-picker__section">
                <h4 className="screen-picker__section-title">Screens</h4>
                <div className="screen-picker__grid">
                  {screens.map((source) => (
                    <button
                      key={source.id}
                      className={`screen-picker__source ${
                        selected === source.id ? 'screen-picker__source--selected' : ''
                      }`}
                      onClick={() => setSelected(source.id)}
                    >
                      <img
                        src={source.thumbnail}
                        alt={source.name}
                        className="screen-picker__thumbnail"
                      />
                      <span className="screen-picker__source-name">{source.name}</span>
                    </button>
                  ))}
                </div>
              </div>
            )}

            {windows.length > 0 && (
              <div className="screen-picker__section">
                <h4 className="screen-picker__section-title">Windows</h4>
                <div className="screen-picker__grid">
                  {windows.map((source) => (
                    <button
                      key={source.id}
                      className={`screen-picker__source ${
                        selected === source.id ? 'screen-picker__source--selected' : ''
                      }`}
                      onClick={() => setSelected(source.id)}
                    >
                      <img
                        src={source.thumbnail}
                        alt={source.name}
                        className="screen-picker__thumbnail"
                      />
                      <div className="screen-picker__source-info">
                        {source.appIcon && (
                          <img src={source.appIcon} alt="" className="screen-picker__app-icon" />
                        )}
                        <span className="screen-picker__source-name">{source.name}</span>
                      </div>
                    </button>
                  ))}
                </div>
              </div>
            )}
          </div>
        )}

        <div className="screen-picker__quality">
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
      </div>
    </div>
  );
};

export default ScreenSharePicker;
