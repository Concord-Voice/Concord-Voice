import React, { useState, useEffect, useCallback, useRef } from 'react';
import { createPortal } from 'react-dom';
import { errorMessage } from '../../utils/runtime/redactError';
import {
  ExternalLink,
  HeadphoneOff,
  Headphones,
  MessageSquare,
  MessageSquareOff,
  Mic,
  MicOff,
  Monitor,
  MoreHorizontal,
  PhoneOff,
  Pin,
  PinOff,
  Tv,
  Video,
  VideoOff,
  Volume2,
  VolumeX,
} from 'lucide-react';
import { ShareSegmentedControl } from './ShareSegmentedControl';
import {
  useVoiceStore,
  type ActiveScreenShare,
  MAX_TUNED_SCREEN_SHARES,
} from '../../stores/voice/voiceStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useChannelStore } from '../../stores/chat/channelStore';
import { useOsPermissionStore } from '../../stores/voice/osPermissionStore';
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
function screenAudioTitle(capable: boolean, on: boolean): string {
  if (!capable) {
    return 'This share cannot carry computer sound \u2014 share a whole screen, on Windows or macOS';
  }
  // "this screen's audio" is a claim the loopback cannot keep — it ignores the selected
  // source, so a multi-monitor user broadcasts the other screen's applications too. Same
  // correction as the picker hint; this separate live-control tooltip had been missed.
  return on
    ? 'Stop sharing your computer\u2019s sound'
    : 'Share your computer\u2019s sound \u2014 everything playing, not only this screen';
}

function deafenTitle(serverDeafened: boolean, selfDeafened: boolean): string {
  if (serverDeafened) return 'Server-deafened by a moderator';
  if (selfDeafened) return 'Undeafen';
  return 'Deafen';
}

/** Lazily import and return the voice service. */
async function getVoiceService() {
  const { voiceService } = await import('../../services/voice/voiceService');
  return voiceService;
}

/** Attach mousedown + keydown listeners that dismiss a popup when clicking outside or pressing Escape. */
function attachDismissListeners(
  anchorRef: React.RefObject<HTMLElement | null>,
  popupRef: React.RefObject<HTMLElement | null>,
  dismiss: (viaKeyboard: boolean) => void
): () => void {
  const handleClick = (e: MouseEvent) => {
    const target = e.target as Node;
    if (anchorRef.current?.contains(target)) return;
    if (popupRef.current?.contains(target)) return;
    dismiss(false);
  };
  const handleKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'Escape') dismiss(true);
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

interface UtilityMenuProps {
  utilityMenuRef: React.RefObject<HTMLDivElement | null>;
  style: React.CSSProperties;
  tunedInIds: string[];
  activeScreenShares: Record<string, ActiveScreenShare>;
  onOpenPip: (mode: 'frames' | 'screen', producerId?: string) => void;
  /** Live value of the persisted keep-active preference. */
  keepActive: boolean;
  onToggleKeepActive: () => void;
  /** Only meaningful while sharing, so the item is hidden otherwise. */
  showKeepActive: boolean;
  /** False on a non-Electron build, where the pop-out targets do not exist. */
  showPipItems: boolean;
}

/** Portaled PiP menu — lists pop-out targets for user frames and active screen shares. */
const UtilityMenu: React.FC<UtilityMenuProps> = ({
  utilityMenuRef,
  style,
  tunedInIds,
  activeScreenShares,
  onOpenPip,
  keepActive,
  onToggleKeepActive,
  showKeepActive,
  showPipItems,
}) => (
  <div ref={utilityMenuRef} className="voice-controls__utility-menu" style={style}>
    {showPipItems && (
      <button className="voice-controls__utility-menu-item" onClick={() => onOpenPip('frames')}>
        Pop Out User Frames
      </button>
    )}
    {showPipItems &&
      tunedInIds.map((producerId) => {
        // Resolve the owner via the producerId → owner metadata seam (#2088)
        const meta = activeScreenShares[producerId];
        const name = meta?.displayName || meta?.username || 'User';
        return (
          <button
            key={producerId}
            className="voice-controls__utility-menu-item"
            onClick={() => onOpenPip('screen', producerId)}
          >
            Pop Out {name}&apos;s Screen
          </button>
        );
      })}
    {/* A durable preference (localStorage `concord:keep-active-unfocused`), not a
        per-moment action — which is the whole reason it left the bar. Plain button
        with aria-pressed rather than menuitemcheckbox: this container is a div of
        buttons, not a role="menu", so the checkbox role would promise keyboard
        semantics nothing here implements. */}
    {showKeepActive && (
      <button
        type="button"
        className="voice-controls__utility-menu-item"
        onClick={onToggleKeepActive}
        aria-pressed={keepActive}
      >
        {keepActive ? '\u2713 ' : ''}Keep stream active when unfocused
      </button>
    )}
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
    <button
      className={classes}
      onClick={() => {
        // Load-bearing, exactly as in FontSection: aria-disabled does not stop
        // activation the way `disabled` does, so the guard IS the enforcement.
        if (locked) return;
        onClick();
      }}
      title={title}
      aria-disabled={locked}
    >
      {isActive ? activeIcon : inactiveIcon}
      <span className="voice-controls__btn-label">{isActive ? activeLabel : inactiveLabel}</span>
    </button>
  );
};

/**
 * Tune Everywhere toggle + Tile / Front 'n Center view switch (voiceView
 * context only). File-private extraction keeps VoiceControls under the
 * S3776 cognitive-complexity gate.
 *
 * Offer tune-in while it is legal; otherwise offer tune-out whenever any
 * remote stream is tuned in — never a dead disabled control (the retired
 * dock kept Tune Out All reachable in every mixed/at-cap state).
 */
const StreamControls: React.FC<{ context: 'voiceView' | 'persistent' }> = ({ context }) => {
  const activeScreenShares = useVoiceStore((s) => s.activeScreenShares);
  const tunedInScreenShares = useVoiceStore((s) => s.tunedInScreenShares);

  const remoteShares = Object.values(activeScreenShares).filter((s) => !s.isLocal);
  const remoteUntuned = remoteShares.filter((s) => !(s.producerId in tunedInScreenShares));
  const remoteTuned = remoteShares.filter((s) => s.producerId in tunedInScreenShares);
  const tunedInCount = Object.keys(tunedInScreenShares).length;
  const atShareCap = tunedInCount >= MAX_TUNED_SCREEN_SHARES;
  const canTuneInMore = remoteUntuned.length > 0 && !atShareCap;
  const offerTuneOut = !canTuneInMore && remoteTuned.length > 0;
  // Degenerate only (cap reached with zero remote tune-ins): unreachable with
  // MAX_TUNED_SCREEN_SHARES > 1 since the local sentinel is a single entry.
  const tuneEverywhereLocked = !canTuneInMore && !offerTuneOut;

  const handleTuneEverywhere = useCallback(async () => {
    try {
      const { voiceService } = await import('../../services/voice/voiceService');
      if (canTuneInMore) {
        await voiceService.tuneInAllScreenShares();
      } else {
        await voiceService.tuneOutAllScreenShares();
      }
    } catch (err) {
      console.error('Tune Everywhere failed:', errorMessage(err));
    }
  }, [canTuneInMore]);

  if (context !== 'voiceView') return null;

  const tuneEverywhereTitle = (() => {
    if (tuneEverywhereLocked) return `Maximum ${MAX_TUNED_SCREEN_SHARES} screen shares`;
    if (!offerTuneOut) return 'Tune in to every stream';
    return remoteUntuned.length > 0
      ? `Maximum ${MAX_TUNED_SCREEN_SHARES} screen shares — tune out of every stream`
      : 'Tune out of every stream';
  })();

  return (
    <>
      {/* Tune Everywhere — global stream tune toggle (replaces the old
          Tune In All / Tune Out All dock buttons) */}
      {remoteShares.length > 0 && (
        <MediaButton
          isActive={offerTuneOut}
          onClick={handleTuneEverywhere}
          title={tuneEverywhereTitle}
          activeIcon={<Tv size={18} />}
          inactiveIcon={<Tv size={18} />}
          activeLabel="Tune Out Everywhere"
          inactiveLabel="Tune In Everywhere"
          locked={tuneEverywhereLocked}
        />
      )}
      {/* The Tile ↔ Front 'n Center switch now lives as a floating overlay in
          the voice area (VoiceViewSwitch), not on this bar. */}
    </>
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
  const isScreenAudioOn = useVoiceStore((s) => s.isScreenAudioOn);
  // Published by voiceService, which is the only place that knows both the live source
  // id and the platform. Read, never derived here.
  const isScreenAudioCapable = useVoiceStore((s) => s.isScreenAudioCapable);
  const showVoiceTextChat = useVoiceStore((s) => s.showVoiceTextChat);
  const toggleVoiceTextChat = useVoiceStore((s) => s.toggleVoiceTextChat);
  const activeScreenShares = useVoiceStore((s) => s.activeScreenShares);
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
  // Resolved when the picker OPENS rather than read during render: voiceService is a
  // dynamic import here, so there is no synchronous handle to it in the JSX.
  const [pickerCurrentSource, setPickerCurrentSource] = useState<string | null>(null);
  const [showUtilityMenu, setShowUtilityMenu] = useState(false);

  const controlsRef = useRef<HTMLDivElement>(null);
  const utilityWrapRef = useRef<HTMLDivElement>(null);
  const utilityMenuRef = useRef<HTMLDivElement>(null);

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

  /** Open the picker while a share is live, to change target without stopping (R6). */
  const handleSwitchScreen = () => {
    // RESOLVE FIRST, THEN MOUNT. `ScreenSharePicker` reads `currentSourceId` in a
    // `useState` initializer, so it is consumed once at mount and never again --
    // opening the picker before the id arrives means the pre-selection can never
    // land, and the late setState could also restore a value a cancel had cleared.
    // The wait is a resolved-module microtask in practice: you can only reach this
    // button while a share is live, which means voiceService is already imported.
    void getVoiceService()
      .then((svc) => setPickerCurrentSource(svc.getCurrentScreenSourceId()))
      // A failed resolve costs the pre-selection, never the picker: the `finally`
      // opens it either way, because choosing a source is what the button is for.
      .catch(() => setPickerCurrentSource(null))
      .finally(() => setShowScreenPicker(true));
  };

  const handleToggleScreenAudio = async () => {
    try {
      await (await getVoiceService()).setScreenAudioEnabled(!isScreenAudioOn);
    } catch (err) {
      console.error('Failed to toggle screen audio:', errorMessage(err));
    }
  };

  const handleScreenSourceSelected = async (
    sourceId: string,
    options?: import('../../stores/voice/videoSettingsStore').ScreenShareOptions
  ) => {
    setShowScreenPicker(false);
    setPickerCurrentSource(null);
    try {
      const svc = await getVoiceService();
      // Already sharing means the user picked a NEW target for a live share. Switching
      // keeps the producer id, so viewers stay tuned in; toggleScreenShare here would
      // start a second share on top of the first.
      if (isScreenSharing) await svc.switchScreenSource(sourceId, options);
      else await svc.toggleScreenShare(sourceId, options);
    } catch (err) {
      console.error('Failed to start screen share:', errorMessage(err));
      // Surface cap-exceeded (and other start failures) as a slot toast — parity
      // with the camera path's setVideoSlotError (#1542). errorMessage() returns
      // the server message ("Screen share limit reached (max N)").
      setVideoSlotError(errorMessage(err));
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
    if (!showUtilityMenu) return;
    return attachDismissListeners(utilityWrapRef, utilityMenuRef, (viaKeyboard) => {
      setShowUtilityMenu(false);
      // WCAG 2.4.3. Escape unmounts the focused item, so without this focus
      // falls back to <body> and a keyboard user loses their place in the bar.
      // Deliberately NOT done for an outside click, which would yank focus away
      // from whatever the user just clicked.
      if (viaKeyboard) utilityWrapRef.current?.querySelector('button')?.focus();
    });
  }, [showUtilityMenu]);

  const handleOpenPip = useCallback(async (mode: 'frames' | 'screen', producerId?: string) => {
    if (!globalThis.electron?.openPipWindow) return;
    await globalThis.electron.openPipWindow(buildPipOptions(mode, producerId));
    setShowUtilityMenu(false);
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
          {/* Four clusters — Self, Share, Utility, Danger. The bar had eleven
              equal-weight pills in one undifferentiated row; grouping by what a
              control ACTS ON is what makes it scannable. Spacing carries the
              grouping (Proximity); see the CSS for why there is no divider. */}
          <div className="voice-controls__cluster">
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
          </div>

          {/* Everything scoped to the outgoing share. The two toggles below are
              literally gated on isScreenSharing, so they cannot belong anywhere
              else — yet Chat used to sit between them and the Share button. */}
          <div className="voice-controls__cluster">
            {/* Idle: one button that starts a share. Live: Switch and Stop as one
                object — they act on the same share. */}
            {isScreenSharing ? (
              <ShareSegmentedControl onSwitch={handleSwitchScreen} onStop={handleToggleScreen} />
            ) : (
              <MediaButton
                isActive={false}
                onClick={handleToggleScreen}
                title="Share Screen"
                activeIcon={<Monitor size={18} />}
                inactiveIcon={<Monitor size={18} />}
                activeLabel="Screen"
                inactiveLabel="Screen"
              />
            )}

            {isScreenSharing && (
              <MediaButton
                isActive={isScreenAudioOn}
                onClick={handleToggleScreenAudio}
                locked={!isScreenAudioCapable}
                title={screenAudioTitle(isScreenAudioCapable, isScreenAudioOn)}
                activeIcon={<Volume2 size={18} />}
                inactiveIcon={<VolumeX size={18} />}
                activeLabel="Sound shared"
                inactiveLabel="Share sound"
              />
            )}
          </div>

          {/* Acts on how YOU view the call, not on what you send. Every member is
              conditional, so this cluster can render empty — see the :empty rule. */}
          <div className="voice-controls__cluster">
            {/* Tune Everywhere + view-mode switch (voiceView context only) */}
            <StreamControls context={context} />

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

            {/* `||`, not `&&`: the menu now also carries the keep-active preference,
                which is share-scoped and has nothing to do with Electron. Gating the
                trigger on hasElectronPip alone would make that preference
                unreachable on any build without pop-out windows. */}
            {(hasElectronPip || isScreenSharing) && (
              <div ref={utilityWrapRef} className="voice-controls__utility-wrap">
                <button
                  className="voice-controls__btn"
                  onClick={() => setShowUtilityMenu((v) => !v)}
                  title="More controls"
                  aria-expanded={showUtilityMenu}
                >
                  <MoreHorizontal size={18} />
                  <span className="voice-controls__btn-label">More</span>
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
          </div>

          {/* Alone, so the one irreversible action in the bar is never adjacent to
              a toggle someone meant to press. */}
          <div className="voice-controls__cluster">
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
      </div>

      {/* Portaled slot error — escapes overflow:hidden ancestors */}
      {videoSlotError &&
        createPortal(
          <div
            className="voice-controls__slot-error"
            role="alert"
            style={getPortalStyle(controlsRef)}
          >
            {videoSlotError}
          </div>,
          document.body
        )}

      {/* Portaled PiP menu — escapes overflow:hidden ancestors */}
      {showUtilityMenu &&
        createPortal(
          <UtilityMenu
            utilityMenuRef={utilityMenuRef}
            style={getPortalStyle(utilityWrapRef)}
            tunedInIds={tunedInIds}
            activeScreenShares={activeScreenShares}
            onOpenPip={handleOpenPip}
            keepActive={keepActiveWhileUnfocused}
            onToggleKeepActive={() => setKeepActiveWhileUnfocused(!keepActiveWhileUnfocused)}
            showKeepActive={isScreenSharing}
            showPipItems={hasElectronPip}
          />,
          document.body
        )}

      {showScreenPicker &&
        createPortal(
          <ScreenSharePicker
            onSelect={handleScreenSourceSelected}
            onCancel={() => {
              setShowScreenPicker(false);
              // Cleared on close, not on open: the start-a-share path lives in a
              // module-level helper with no access to this state, so clearing here
              // is the only point every open path passes through first.
              setPickerCurrentSource(null);
            }}
            currentSourceId={pickerCurrentSource}
          />,
          document.body
        )}
    </>
  );
};

export default VoiceControls;
