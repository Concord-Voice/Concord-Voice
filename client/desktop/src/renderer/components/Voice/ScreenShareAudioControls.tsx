import React from 'react';
import { Volume2, VolumeX } from 'lucide-react';
import { useAudioSettingsStore } from '../../stores/audio/audioSettingsStore';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useUserStore } from '../../stores/auth/userStore';
import { voiceService } from '../../services/voiceService';
import './ScreenShareAudioControls.css';

interface ScreenShareAudioControlsProps {
  sharerUserId: string;
}

const DEAFENED_TITLE = 'Server-deafened by a moderator';

/** Value-readout text: deafen wins over mute, which wins over the live volume. */
function valueLabel(serverDeafened: boolean, muted: boolean, volume: number): string {
  if (serverDeafened) return 'Deafened';
  if (muted) return 'Muted';
  return `${volume}%`;
}

/**
 * Per-stream screenshare audio controls: an independent volume slider (client
 * gain via perScreenShareVolume) and a mute toggle (server-side screen-audio
 * consumer pause via voiceService). Rendered only for a remote sharer with live
 * screen audio (#2162).
 *
 * When the local viewer is server-deafened (moderator-enforced), the media-plane
 * authoritatively refuses to resume ANY audio consumer (roomManager serverDeafened
 * guard), so the viewer hears silence regardless of this control's state. We
 * disable the mute/volume controls in that case so the UI does not claim audio is
 * playing while the server is refusing it (#2162).
 */
export const ScreenShareAudioControls: React.FC<ScreenShareAudioControlsProps> = ({
  sharerUserId,
}) => {
  const volume = useAudioSettingsStore((s) => s.perScreenShareVolume[sharerUserId] ?? 100);
  const setScreenShareVolume = useAudioSettingsStore((s) => s.setScreenShareVolume);
  const muted = useVoiceStore((s) => s.screenShareMuted[sharerUserId] ?? false);
  const localUserId = useUserStore((s) => s.user?.id);
  const serverDeafened = useVoiceStore((s) =>
    localUserId ? (s.participants[localUserId]?.serverDeafened ?? false) : false
  );

  const toggleMute = (): void => {
    if (muted) voiceService.unmuteScreenShare(sharerUserId);
    else voiceService.muteScreenShare(sharerUserId);
  };

  // Keep control interaction from bubbling to the stage's handlers: the click
  // handler that switches the focused stream, and the window-level ArrowLeft/
  // ArrowRight keydown listener that cycles shares in focus mode. Without the
  // keydown guard, arrow-key volume adjustment on the slider would also cycle
  // the dominant share (#2162). stopPropagation (not preventDefault) still lets
  // the slider adjust and the button activate. Attached to the NATIVE
  // interactive elements (button/input), not the wrapper div, so no a11y rule fires.
  const stop = (e: React.SyntheticEvent): void => e.stopPropagation();

  const muteLabel = muted ? 'Unmute screen audio' : 'Mute screen audio';
  // A server-deafened viewer hears nothing regardless of mute state, so the
  // icon shows silence and the value text reads "Deafened" (not "100%").
  const silenced = muted || serverDeafened;

  return (
    <div className="screenshare-audio-controls">
      <button
        type="button"
        className="screenshare-audio-controls__mute"
        disabled={serverDeafened}
        onMouseDown={stop}
        onKeyDown={stop}
        onClick={(e) => {
          stop(e);
          toggleMute();
        }}
        aria-pressed={muted}
        aria-label={serverDeafened ? DEAFENED_TITLE : muteLabel}
        title={serverDeafened ? DEAFENED_TITLE : muteLabel}
      >
        {silenced ? <VolumeX size={16} /> : <Volume2 size={16} />}
      </button>
      <input
        type="range"
        className="screenshare-audio-controls__slider"
        min={0}
        max={200}
        step={1}
        value={volume}
        disabled={silenced}
        onMouseDown={stop}
        onClick={stop}
        onKeyDown={stop}
        onChange={(e) => setScreenShareVolume(sharerUserId, Number(e.target.value))}
        aria-label="Screen share volume"
        title={serverDeafened ? DEAFENED_TITLE : undefined}
      />
      <span className="screenshare-audio-controls__value">
        {valueLabel(serverDeafened, muted, volume)}
      </span>
    </div>
  );
};
