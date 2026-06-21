import React, { useState, useEffect, useCallback, useRef } from 'react';
import { createPortal } from 'react-dom';
import { errorMessage } from '../../utils/redactError';
import {
  Mic,
  MicOff,
  Headphones,
  HeadphoneOff,
  Video,
  VideoOff,
  Monitor,
  MonitorOff,
  PhoneOff,
  MessageSquare,
  MessageSquareOff,
  PictureInPicture2,
  Eye,
  EyeOff,
  Pin,
  PinOff,
  ExternalLink,
} from 'lucide-react';
import { useVoiceStore } from '../../stores/voiceStore';
import { useUserStore } from '../../stores/userStore';
import { useChannelStore } from '../../stores/channelStore';
import { useOsPermissionStore } from '../../stores/osPermissionStore';
// voiceService is loaded on-demand via dynamic import() — see voiceService.ts
import ScreenSharePicker from './ScreenSharePicker';
import './VoiceControls.css';

/**
 * JIT permission pre-check: verify an OS permission before starting a media action.
 * Returns true if permission was denied (caller should abort).
 */
async function checkPermissionOrWarn(
  permType: 'camera' | 'screen',
  errorMessage: string
): Promise<boolean> {
  const permStore = useOsPermissionStore.getState();
  const status = await permStore.checkOne(permType);
  if (status === 'denied' || status === 'restricted') {
    useVoiceStore.getState().setVideoSlotError(errorMessage);
    permStore.openSettings(permType);
    return true;
  }
  return false;
}

/** Compute mute button tooltip based on enforcement state. */
function muteTitle(serverMuted: boolean, selfMuted: boolean): string {
  if (serverMuted) return 'Server-muted by a moderator';
  if (selfMuted) return 'Unmute';
  return 'Mute';
}

/** Compute deafen button tooltip based on enforcement state. */
function deafenTitle(serverDeafened: boolean, selfDeafened: boolean): string {
  if (serverDeafened) return 'Server-deafened by a moderator';
  if (selfDeafened) return 'Undeafen';
  return 'Deafen';
}

/** Lazily import and return the voice service. */
async function getVoiceService() {
  const { voiceService } = await import('../../services/voiceService');
  return voiceService;
}

/** Attach mousedown + keydown listeners that dismiss a popup when clicking outside or pressing Escape. */
function attachDismissListeners(
  anchorRef: React.RefObject<HTMLElement | null>,
  popupRef: React.RefObject<HTMLElement | null>,
  dismiss: () => void
): () => void {
  const handleClick = (e: MouseEvent) => {
    const target = e.target as Node;
    if (anchorRef.current?.contains(target)) return;
    if (popupRef.current?.contains(target)) return;
    dismiss();
  };
  const handleKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'Escape') dismiss();
  };
  document.addEventListener('mousedown', handleClick);
  document.addEventListener('keydown', handleKeyDown);
  return () => {
    document.removeEventListener('mousedown', handleClick);
    document.removeEventListener('keydown', handleKeyDown);
  };
}

/** Compute position:fixed style to render a portal element above an anchor ref. */
function getPortalStyle(ref: React.RefObject<HTMLElement | null>): React.CSSProperties {
  const base: React.CSSProperties = {
    position: 'fixed',
    transform: 'translateX(-50%)',
    zIndex: 99999,
  };
  const rect = ref.current?.getBoundingClientRect();
  if (!rect) return { ...base, bottom: 0, left: '50%' };
  return {
    ...base,
    bottom: globalThis.innerHeight - rect.top,
    left: rect.left + rect.width / 2,
  };
}

/** Build PiP window options for the given mode. */
function buildPipOptions(mode: 'frames' | 'screen', producerId?: string) {
  const isFrames = mode === 'frames';
  return {
    id: isFrames ? 'frames-main' : `screen-${producerId || ''}`,
    width: isFrames ? 320 : 400,
    height: isFrames ? 240 : 300,
  };
}

/** Start or stop screen sharing, showing the picker on Electron. */
async function toggleScreenShareAction(
  isScreenSharing: boolean,
  setShowScreenPicker: (v: boolean) => void
): Promise<void> {
  if (isScreenSharing) {
    await (await getVoiceService()).toggleScreenShare();
    return;
  }

  const denied = await checkPermissionOrWarn(
    'screen',
    'Screen recording access denied. On macOS, enable Screen Recording in ' +
      'System Settings > Privacy & Security, then restart Concord.'
  );
  if (denied) return;

  if (typeof globalThis.electron?.getDesktopSources === 'function') {
    setShowScreenPicker(true);
  } else {
    await (await getVoiceService()).toggleScreenShare();
  }
}

/* ── Extracted sub-components (reduces cognitive complexity — S3776) ── */

interface PipMenuProps {
  pipMenuRef: React.RefObject<HTMLDivElement | null>;
  style: React.CSSProperties;
  tunedInIds: string[];
  participants: Record<
    string,
    { isScreenSharing?: boolean; displayName?: string; username?: string }
  >;
  onOpenPip: (mode: 'frames' | 'screen', producerId?: string) => void;
}

/** Portaled PiP menu — lists pop-out targets for user frames and active screen shares. */
const PipMenu: React.FC<PipMenuProps> = ({
  pipMenuRef,
  style,
  tunedInIds,
  participants,
  onOpenPip,
}) => (
  <div ref={pipMenuRef} className="voice-controls__pip-menu" style={style}>
    <button className="voice-controls__pip-menu-item" onClick={() => onOpenPip('frames')}>
      Pop Out User Frames
    </button>
    {tunedInIds.map((producerId) => {
      const sharer = Object.values(participants).find((p) => p.isScreenSharing);
      const name = sharer?.displayName || sharer?.username || 'User';
      return (
        <button
          key={producerId}
          className="voice-controls__pip-menu-item"
          onClick={() => onOpenPip('screen', producerId)}
        >
          Pop Out {name}&apos;s Screen
        </button>
      );
    })}
  </div>
);

interface MediaButtonProps {
  isActive: boolean;
  onClick: () => void;
  title: string;
  activeIcon: React.ReactNode;
  inactiveIcon: React.ReactNode;
  activeLabel: string;
  inactiveLabel: string;
  locked?: boolean;
}

/** Generic media toggle button — mic, deafen, video, screen share. */
const MediaButton: React.FC<MediaButtonProps> = ({
  isActive,
  onClick,
  title,
  activeIcon,
  inactiveIcon,
  activeLabel,
  inactiveLabel,
  locked = false,
}) => {
  const classes = [
    'voice-controls__btn',
    isActive ? 'voice-controls__btn--active' : '',
    locked ? 'voice-controls__btn--locked' : '',
  ]
    .filter(Boolean)
    .join(' ');

  return (
    <button className={classes} onClick={onClick} title={title} disabled={locked}>
      {isActive ? activeIcon : inactiveIcon}
      <span className="voice-controls__btn-label">{isActive ? activeLabel : inactiveLabel}</span>
    </button>
  );
};

interface VoiceControlsProps {
  /** 'voiceView' = inside VoiceView, 'persistent' = navigated-away bar */
  context?: 'voiceView' | 'persistent';
  /** Callback to pop out the controls bar (managed by parent PersistentVoiceBar) */
  onPopOut?: () => void;
}

const VoiceControls: React.FC<VoiceControlsProps> = ({ context = 'voiceView', onPopOut }) => {
  const activeChannelId = useVoiceStore((s) => s.activeChannelId);
  const connectionState = useVoiceStore((s) => s.connectionState);
  const isMuted = useVoiceStore((s) => s.isMuted);
  const isDeafened = useVoiceStore((s) => s.isDeafened);
  const isVideoOn = useVoiceStore((s) => s.isVideoOn);
  const isScreenSharing = useVoiceStore((s) => s.isScreenSharing);
  const showVoiceTextChat = useVoiceStore((s) => s.showVoiceTextChat);
  const toggleVoiceTextChat = useVoiceStore((s) => s.toggleVoiceTextChat);
  const participants = useVoiceStore((s) => s.participants);
  const videoSlotError = useVoiceStore((s) => s.videoSlotError);
  const setVideoSlotError = useVoiceStore((s) => s.setVideoSlotError);
  const tunedInScreenShares = useVoiceStore((s) => s.tunedInScreenShares);
  const keepActiveWhileUnfocused = useVoiceStore((s) => s.keepActiveWhileUnfocused);
  const setKeepActiveWhileUnfocused = useVoiceStore((s) => s.setKeepActiveWhileUnfocused);
  const voiceControlsPinned = useVoiceStore((s) => s.voiceControlsPinned);
  const toggleVoiceControlsPinned = useVoiceStore((s) => s.toggleVoiceControlsPinned);

  const localUserId = useUserStore((s) => s.user?.id);
  const localParticipant = useVoiceStore((s) =>
    localUserId ? s.participants[localUserId] : undefined
  );
  const isServerMuted = localParticipant?.serverMuted || false;
  const isServerDeafened = localParticipant?.serverDeafened || false;

  const getLinkedTextChannel = useChannelStore((s) => s.getLinkedTextChannel);
  const hasLinkedText = !!(activeChannelId && getLinkedTextChannel(activeChannelId));

  const [showScreenPicker, setShowScreenPicker] = useState(false);
  const [showPipMenu, setShowPipMenu] = useState(false);

  const controlsRef = useRef<HTMLDivElement>(null);
  const pipWrapRef = useRef<HTMLDivElement>(null);
  const pipMenuRef = useRef<HTMLDivElement>(null);

  // Electron PiP — check if electron API is available
  const hasElectronPip = !!globalThis.electron?.openPipWindow;

  const handleToggleMute = async () => {
    (await getVoiceService()).toggleMute();
  };

  const handleToggleDeafen = async () => {
    (await getVoiceService()).toggleDeafen();
  };

  const handleToggleVideo = async () => {
    const needsPermission = !isVideoOn;
    const denied =
      needsPermission &&
      (await checkPermissionOrWarn(
        'camera',
        'Camera access denied. Grant permission in System Settings > Privacy & Security.'
      ));
    if (denied) return;
    await (await getVoiceService()).toggleVideo();
  };

  const handleToggleScreen = async () => {
    try {
      await toggleScreenShareAction(isScreenSharing, setShowScreenPicker);
    } catch (err) {
      console.error('Failed to toggle screen share:', errorMessage(err));
    }
  };

  const handleScreenSourceSelected = async (
    sourceId: string,
    options?: import('../../stores/videoSettingsStore').ScreenShareOptions
  ) => {
    setShowScreenPicker(false);
    try {
      await (await getVoiceService()).toggleScreenShare(sourceId, options);
    } catch (err) {
      console.error('Failed to start screen share:', errorMessage(err));
    }
  };

  const handleLeave = async () => {
    await (await getVoiceService()).leaveChannel();
  };

  // Auto-dismiss video slot error after 5 seconds
  useEffect(() => {
    if (!videoSlotError) return;
    const timer = setTimeout(() => setVideoSlotError(null), 5000);
    return () => clearTimeout(timer);
  }, [videoSlotError, setVideoSlotError]);

  // Close PiP menu on click outside or Escape
  useEffect(() => {
    if (!showPipMenu) return;
    return attachDismissListeners(pipWrapRef, pipMenuRef, () => setShowPipMenu(false));
  }, [showPipMenu]);

  const handleOpenPip = useCallback(async (mode: 'frames' | 'screen', producerId?: string) => {
    if (!globalThis.electron?.openPipWindow) return;
    await globalThis.electron.openPipWindow(buildPipOptions(mode, producerId));
    setShowPipMenu(false);
  }, []);

  const tunedInIds = Object.keys(tunedInScreenShares);

  if (connectionState === 'disconnected') return null;

  return (
    <>
      <div ref={controlsRef} className="voice-controls voice-controls--full">
        {/* Pin/Unpin button — persistent context only */}
        {context === 'persistent' && (
          <div className="voice-controls__persistent-actions">
            <button
              className="voice-controls__pin-btn"
              onClick={toggleVoiceControlsPinned}
              title={voiceControlsPinned ? 'Unpin controls' : 'Pin controls'}
            >
              {voiceControlsPinned ? <PinOff size={14} /> : <Pin size={14} />}
            </button>
          </div>
        )}

        <div className="voice-controls__buttons">
          <MediaButton
            isActive={isMuted || isServerMuted}
            onClick={handleToggleMute}
            title={muteTitle(isServerMuted, isMuted)}
            activeIcon={<MicOff size={18} />}
            inactiveIcon={<Mic size={18} />}
            activeLabel={isServerMuted ? 'Muted' : 'Unmute'}
            inactiveLabel="Mute"
            locked={isServerMuted}
          />

          <MediaButton
            isActive={isDeafened || isServerDeafened}
            onClick={handleToggleDeafen}
            title={deafenTitle(isServerDeafened, isDeafened)}
            activeIcon={<HeadphoneOff size={18} />}
            inactiveIcon={<Headphones size={18} />}
            activeLabel={isServerDeafened ? 'Deafened' : 'Undeafen'}
            inactiveLabel="Deafen"
            locked={isServerDeafened}
          />

          <MediaButton
            isActive={isVideoOn}
            onClick={handleToggleVideo}
            title={isVideoOn ? 'Stop Video' : 'Start Video'}
            activeIcon={<VideoOff size={18} />}
            inactiveIcon={<Video size={18} />}
            activeLabel="Stop Video"
            inactiveLabel="Video"
          />

          <MediaButton
            isActive={isScreenSharing}
            onClick={handleToggleScreen}
            title={isScreenSharing ? 'Stop Sharing' : 'Share Screen'}
            activeIcon={<MonitorOff size={18} />}
            inactiveIcon={<Monitor size={18} />}
            activeLabel="Stop"
            inactiveLabel="Screen"
          />

          {hasLinkedText && (
            <button
              className={`voice-controls__btn ${showVoiceTextChat ? 'voice-controls__btn--chat-active' : ''}`}
              onClick={toggleVoiceTextChat}
              title={showVoiceTextChat ? 'Hide Text Chat' : 'Show Text Chat'}
            >
              {showVoiceTextChat ? <MessageSquareOff size={18} /> : <MessageSquare size={18} />}
              <span className="voice-controls__btn-label">Chat</span>
            </button>
          )}

          {isScreenSharing && (
            <MediaButton
              isActive={keepActiveWhileUnfocused}
              onClick={() => setKeepActiveWhileUnfocused(!keepActiveWhileUnfocused)}
              title={
                keepActiveWhileUnfocused
                  ? 'Stream stays active when unfocused'
                  : 'Stream pauses when unfocused'
              }
              activeIcon={<Eye size={18} />}
              inactiveIcon={<EyeOff size={18} />}
              activeLabel="Always On"
              inactiveLabel="Auto Pause"
            />
          )}

          {hasElectronPip && (
            <div ref={pipWrapRef} className="voice-controls__pip-wrap">
              <button
                className="voice-controls__btn"
                onClick={() => setShowPipMenu((v) => !v)}
                title="Picture-in-Picture"
              >
                <PictureInPicture2 size={18} />
                <span className="voice-controls__btn-label">PiP</span>
              </button>
            </div>
          )}

          {/* Pop-Out controls button — persistent context only */}
          {context === 'persistent' && onPopOut && (
            <button className="voice-controls__btn" onClick={onPopOut} title="Pop out controls">
              <ExternalLink size={18} />
              <span className="voice-controls__btn-label">Pop Out</span>
            </button>
          )}

          <button
            className="voice-controls__btn voice-controls__btn--danger"
            onClick={handleLeave}
            title="Leave Voice"
          >
            <PhoneOff size={18} />
            <span className="voice-controls__btn-label">Leave</span>
          </button>
        </div>
      </div>

      {/* Portaled slot error — escapes overflow:hidden ancestors */}
      {videoSlotError &&
        createPortal(
          <div className="voice-controls__slot-error" style={getPortalStyle(controlsRef)}>
            {videoSlotError}
          </div>,
          document.body
        )}

      {/* Portaled PiP menu — escapes overflow:hidden ancestors */}
      {showPipMenu &&
        createPortal(
          <PipMenu
            pipMenuRef={pipMenuRef}
            style={getPortalStyle(pipWrapRef)}
            tunedInIds={tunedInIds}
            participants={participants}
            onOpenPip={handleOpenPip}
          />,
          document.body
        )}

      {showScreenPicker && (
        <ScreenSharePicker
          onSelect={handleScreenSourceSelected}
          onCancel={() => setShowScreenPicker(false)}
        />
      )}
    </>
  );
};

export default VoiceControls;
