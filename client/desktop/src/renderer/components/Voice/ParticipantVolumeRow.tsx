import React from 'react';
import { useAudioSettingsStore } from '../../stores/audioSettingsStore';
import './ParticipantVolumeRow.css';

interface ParticipantVolumeRowProps {
  userId: string;
}

/**
 * Per-participant volume slider rendered inside the participant context menu.
 * Reads / writes `perParticipantVolume[userId]` on the audio settings store.
 * Missing keys default to 100 (unity — no adjustment relative to master).
 */
export const ParticipantVolumeRow: React.FC<ParticipantVolumeRowProps> = ({ userId }) => {
  const volume = useAudioSettingsStore((s) => s.perParticipantVolume[userId] ?? 100);
  const setParticipantVolume = useAudioSettingsStore((s) => s.setParticipantVolume);
  const clearParticipantVolume = useAudioSettingsStore((s) => s.clearParticipantVolume);

  // Stop click/mousedown from closing the context menu — the menu's outside-click
  // handler fires on mousedown, which would otherwise dismiss the menu when the
  // user drags the slider past the menu's bounding rect.
  const stop = (e: React.SyntheticEvent): void => {
    e.stopPropagation();
  };
  // For contextmenu specifically, also preventDefault so a right-click inside
  // the volume row doesn't summon the OS/Electron native context menu on top
  // of the app's participant menu.
  const stopAndPrevent = (e: React.SyntheticEvent): void => {
    e.stopPropagation();
    e.preventDefault();
  };

  return (
    <div
      className="participant-volume-row"
      onMouseDown={stop}
      onClick={stop}
      onContextMenu={stopAndPrevent}
    >
      <div className="participant-volume-row-header">
        <span className="participant-volume-row-label">Volume</span>
        <span className="participant-volume-row-value">{volume}%</span>
      </div>
      <input
        type="range"
        className="participant-volume-row-slider"
        min={0}
        max={200}
        step={1}
        value={volume}
        onChange={(e) => setParticipantVolume(userId, Number(e.target.value))}
        aria-label="Participant volume"
      />
      {volume !== 100 && (
        <button
          type="button"
          className="participant-volume-row-reset"
          onClick={() => clearParticipantVolume(userId)}
        >
          Reset to 100%
        </button>
      )}
    </div>
  );
};
