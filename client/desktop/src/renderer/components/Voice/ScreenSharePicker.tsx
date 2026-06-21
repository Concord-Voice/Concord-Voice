import React, { useState, useEffect, useCallback } from 'react';
import { Monitor, X } from 'lucide-react';
import {
  useVideoSettingsStore,
  type ScreenContentType,
  type ScreenShareOptions,
} from '../../stores/videoSettingsStore';
import CustomSelect from '../ui/CustomSelect';
import { errorMessage } from '../../utils/redactError';
import './ScreenSharePicker.css';

interface DesktopSource {
  id: string;
  name: string;
  thumbnail: string;
  appIcon: string | null;
}

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

  const handleConfirm = () => {
    if (!selected) return;
    onSelect(selected, { resolution, frameRate, contentType });
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
              options={[
                { value: 'source', label: 'Source Native' },
                { value: '1080p', label: '1080p' },
                { value: '720p', label: '720p' },
              ]}
              value={resolution}
              onChange={(v) => {
                setResolution(v);
                setDirty(true);
              }}
            />
          </div>
          <div className="screen-picker__quality-row">
            <label htmlFor="screen-framerate" className="screen-picker__quality-label">
              Frame Rate
            </label>
            <CustomSelect
              id="screen-framerate"
              className="screen-picker__quality-select"
              options={[
                { value: '5', label: '5 FPS' },
                { value: '15', label: '15 FPS' },
                { value: '30', label: '30 FPS' },
                { value: '60', label: '60 FPS' },
              ]}
              value={String(frameRate)}
              onChange={(v) => {
                setFrameRate(Number(v));
                setDirty(true);
              }}
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
