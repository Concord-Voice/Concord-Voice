import React, { useEffect, useRef, useCallback, useMemo } from 'react';
import { Lock, Users, ChevronUp, ChevronDown } from 'lucide-react';
import { createResizeKeyHandler } from '../../utils/resizeKeyboard';
import { useVoiceStore } from '../../stores/voiceStore';
import { useChannelStore } from '../../stores/channelStore';
// voiceService is loaded on-demand via dynamic import() — see voiceService.ts
import { UserFrameGrid } from './ParticipantGrid';
import UserFrameBar from './UserFrameBar';
import VoiceStage from './VoiceStage';
import StreamBar from './StreamBar';
import { TuneInOverlay } from './TuneInButton';
import VoiceControls from './VoiceControls';
import VoiceTextChat from './VoiceTextChat';
import './VoiceView.css';

interface VoiceViewProps {
  channelId: string;
  channelName: string;
}

const MIN_CHAT_HEIGHT = 150;
const MAX_CHAT_RATIO = 0.7;
const MIN_CHAT_WIDTH = 250;
const MAX_CHAT_WIDTH_RATIO = 0.5;
const MIN_SECTION_HEIGHT = 80;

/** Compute new chat panel size during a drag resize. */
function computeChatResizeValue(
  isVertical: boolean,
  containerRect: DOMRect,
  startPos: number,
  movePos: number,
  startSize: number
): number {
  const delta = startPos - movePos;
  if (isVertical) {
    const maxWidth = containerRect.width * MAX_CHAT_WIDTH_RATIO;
    return Math.min(maxWidth, Math.max(MIN_CHAT_WIDTH, startSize + delta));
  }
  const maxHeight = containerRect.height * MAX_CHAT_RATIO;
  return Math.min(maxHeight, Math.max(MIN_CHAT_HEIGHT, startSize + delta));
}

/* ── Extracted sub-components (reduces cognitive complexity — S3776) ── */

/** Compute the join prompt subtitle text. */
function getJoinSubtitle(isConnecting: boolean, memberCount: number): string {
  if (isConnecting) return 'Connecting...';
  if (memberCount > 0) {
    const plural = memberCount === 1 ? '' : 's';
    return `${memberCount} user${plural} in this channel`;
  }
  return 'No one is in this voice channel yet.';
}

interface JoinPromptProps {
  channelName: string;
  isConnecting: boolean;
  memberCount: number;
  onJoin: () => void;
}

/** Displayed when the user is not connected to the voice channel. */
const JoinPrompt: React.FC<JoinPromptProps> = ({
  channelName,
  isConnecting,
  memberCount,
  onJoin,
}) => (
  <div className="voice-view">
    <div className="voice-view__join">
      <div className="voice-view__join-icon">
        <Users size={48} />
      </div>
      <h2 className="voice-view__join-title">{channelName}</h2>
      <p className="voice-view__join-subtitle">{getJoinSubtitle(isConnecting, memberCount)}</p>
      <button className="voice-view__join-btn" onClick={onJoin} disabled={isConnecting}>
        {isConnecting ? 'Connecting...' : 'Join Voice'}
      </button>
    </div>
  </div>
);

interface HeaderBadgesProps {
  participantCount: number;
  effectiveQualityTier: string;
  decoderHealth: string;
}

/** Header metadata badges: E2EE, participant count, quality tier, decoder health. */
const HeaderBadges: React.FC<HeaderBadgesProps> = ({
  participantCount,
  effectiveQualityTier,
  decoderHealth,
}) => (
  <div className="voice-view__header-meta">
    <span className="voice-view__badge voice-view__badge--encrypted">
      <Lock size={12} />
      E2EE
    </span>
    <span className="voice-view__badge">
      <Users size={12} />
      {participantCount}
    </span>
    <span className="voice-view__badge voice-view__badge--quality">{effectiveQualityTier}</span>
    {decoderHealth !== 'green' && (
      <span className={`voice-view__badge voice-view__badge--decoder-${decoderHealth}`}>
        {decoderHealth === 'yellow' ? 'Decode Warning' : 'Decode Overload'}
      </span>
    )}
  </div>
);

interface ScreenShareLayoutProps {
  showUserFrameBar: boolean;
  userFrameBarHeight: number;
  onUserFrameResize: (e: React.MouseEvent) => void;
  userFrameResizeKeyHandler: (e: React.KeyboardEvent) => void;
  toggleUserFrameBar: () => void;
  showStreamSection: boolean;
  streamBarHeight: number;
  onStreamBarResize: (e: React.MouseEvent) => void;
  streamBarResizeKeyHandler: (e: React.KeyboardEvent) => void;
  toggleStreamBar: () => void;
  showStreamBar: boolean;
}

/** Three-section layout when screen shares are active (user frames, stage, stream bar). */
const ScreenShareLayout: React.FC<ScreenShareLayoutProps> = ({
  showUserFrameBar,
  userFrameBarHeight,
  onUserFrameResize,
  userFrameResizeKeyHandler,
  toggleUserFrameBar,
  showStreamSection,
  streamBarHeight,
  onStreamBarResize,
  streamBarResizeKeyHandler,
  toggleStreamBar,
  showStreamBar,
}) => (
  <>
    {showUserFrameBar && <UserFrameBar height={userFrameBarHeight} />}

    <div className="voice-view__section-handle">
      <button
        type="button"
        className="voice-view__section-handle-grip"
        onMouseDown={onUserFrameResize}
        onKeyDown={userFrameResizeKeyHandler}
        tabIndex={0}
        aria-label="Resize user frame bar"
      />
      <button
        className="voice-view__section-toggle"
        onClick={toggleUserFrameBar}
        title={showUserFrameBar ? 'Hide user frames' : 'Show user frames'}
      >
        {showUserFrameBar ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
      </button>
    </div>

    <VoiceStage />

    {showStreamSection && (
      <div className="voice-view__section-handle">
        <button
          type="button"
          className="voice-view__section-handle-grip"
          onMouseDown={onStreamBarResize}
          onKeyDown={streamBarResizeKeyHandler}
          tabIndex={0}
          aria-label="Resize stream bar"
        />
        <button
          className="voice-view__section-toggle"
          onClick={toggleStreamBar}
          title={showStreamBar ? 'Hide stream bar' : 'Show stream bar'}
        >
          {showStreamBar ? <ChevronDown size={14} /> : <ChevronUp size={14} />}
        </button>
      </div>
    )}

    {showStreamSection && showStreamBar && <StreamBar height={streamBarHeight} />}
  </>
);

const VoiceView: React.FC<VoiceViewProps> = ({ channelId, channelName }) => {
  const activeChannelId = useVoiceStore((s) => s.activeChannelId);
  const connectionState = useVoiceStore((s) => s.connectionState);
  const participants = useVoiceStore((s) => s.participants);
  const channelVoiceMembers = useVoiceStore((s) => s.channelVoiceMembers);
  const effectiveQualityTier = useVoiceStore((s) => s.effectiveQualityTier);
  const decoderHealth = useVoiceStore((s) => s.decoderHealth);
  const showVoiceTextChat = useVoiceStore((s) => s.showVoiceTextChat);
  const voiceTextChatHeight = useVoiceStore((s) => s.voiceTextChatHeight);
  const setVoiceTextChatHeight = useVoiceStore((s) => s.setVoiceTextChatHeight);
  const voiceTextChatLayout = useVoiceStore((s) => s.voiceTextChatLayout);
  const voiceTextChatWidth = useVoiceStore((s) => s.voiceTextChatWidth);
  const setVoiceTextChatWidth = useVoiceStore((s) => s.setVoiceTextChatWidth);
  const tunedInScreenShares = useVoiceStore((s) => s.tunedInScreenShares);
  const availableScreenShares = useVoiceStore((s) => s.availableScreenShares);
  const showUserFrameBar = useVoiceStore((s) => s.showUserFrameBar);
  const showStreamBar = useVoiceStore((s) => s.showStreamBar);
  const userFrameBarHeight = useVoiceStore((s) => s.userFrameBarHeight);
  const streamBarHeight = useVoiceStore((s) => s.streamBarHeight);
  const setUserFrameBarHeight = useVoiceStore((s) => s.setUserFrameBarHeight);
  const setStreamBarHeight = useVoiceStore((s) => s.setStreamBarHeight);
  const toggleUserFrameBar = useVoiceStore((s) => s.toggleUserFrameBar);
  const toggleStreamBar = useVoiceStore((s) => s.toggleStreamBar);
  const stageLayout = useVoiceStore((s) => s.stageLayout);
  const isScreenSharing = useVoiceStore((s) => s.isScreenSharing);
  const keepActiveWhileUnfocused = useVoiceStore((s) => s.keepActiveWhileUnfocused);

  const getLinkedTextChannel = useChannelStore((s) => s.getLinkedTextChannel);
  const hasLinkedText = !!getLinkedTextChannel(channelId);

  const hasJoinedRef = useRef(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const resizingRef = useRef(false);
  const participantCount = Object.keys(participants).length;

  const hasScreenShares = Object.keys(tunedInScreenShares).length > 0;

  const isConnected = activeChannelId === channelId && connectionState === 'connected';
  const isConnecting = connectionState === 'connecting';

  const handleJoin = async () => {
    if (hasJoinedRef.current) return;
    hasJoinedRef.current = true;
    try {
      const { voiceService } = await import('../../services/voiceService');
      await voiceService.joinChannel(channelId);
    } catch {
      hasJoinedRef.current = false;
    }
  };

  useEffect(() => {
    hasJoinedRef.current = isConnected;
  }, [channelId, isConnected]);

  // Auto-pause: hide local stream previews when window loses focus to save
  // GPU/decode resources. The producer keeps running — other participants
  // still see the stream. Only the local rendering is paused.
  useEffect(() => {
    if (!isConnected || !isScreenSharing) {
      useVoiceStore.getState().setLocalStreamPaused(false);
      return;
    }

    const handleBlur = () => {
      if (keepActiveWhileUnfocused) return;
      useVoiceStore.getState().setLocalStreamPaused(true);
    };

    const handleFocus = () => {
      if (!useVoiceStore.getState().localStreamPaused) return;
      useVoiceStore.getState().setLocalStreamPaused(false);
    };

    globalThis.addEventListener('blur', handleBlur);
    globalThis.addEventListener('focus', handleFocus);
    return () => {
      globalThis.removeEventListener('blur', handleBlur);
      globalThis.removeEventListener('focus', handleFocus);
      // Ensure we unpause if this effect cleans up (e.g. screen share stops)
      useVoiceStore.getState().setLocalStreamPaused(false);
    };
  }, [isConnected, isScreenSharing, keepActiveWhileUnfocused]);

  // Text chat resize (layout-aware: vertical uses X axis, horizontal uses Y axis)
  const handleChatResizeStart = useCallback(
    (e: React.MouseEvent) => {
      e.preventDefault();
      resizingRef.current = true;
      const isVertical = voiceTextChatLayout === 'vertical';
      const startPos = isVertical ? e.clientX : e.clientY;
      const startSize = isVertical ? voiceTextChatWidth : voiceTextChatHeight;
      const setter = isVertical ? setVoiceTextChatWidth : setVoiceTextChatHeight;

      document.body.style.cursor = isVertical ? 'col-resize' : 'row-resize';
      document.body.style.userSelect = 'none';

      const onMouseMove = (moveEvent: MouseEvent) => {
        if (!resizingRef.current || !containerRef.current) return;
        const rect = containerRef.current.getBoundingClientRect();
        const movePos = isVertical ? moveEvent.clientX : moveEvent.clientY;
        setter(computeChatResizeValue(isVertical, rect, startPos, movePos, startSize));
      };

      const onMouseUp = () => {
        resizingRef.current = false;
        document.body.style.cursor = '';
        document.body.style.userSelect = '';
        document.removeEventListener('mousemove', onMouseMove);
        document.removeEventListener('mouseup', onMouseUp);
      };

      document.addEventListener('mousemove', onMouseMove);
      document.addEventListener('mouseup', onMouseUp);
    },
    [
      voiceTextChatLayout,
      voiceTextChatHeight,
      voiceTextChatWidth,
      setVoiceTextChatHeight,
      setVoiceTextChatWidth,
    ]
  );

  // Section resize factory (for UserFrameBar / StreamBar)
  const makeSectionResizeStart = useCallback(
    (section: 'top' | 'bottom', currentHeight: number, setter: (h: number) => void) =>
      (e: React.MouseEvent) => {
        e.preventDefault();
        const startY = e.clientY;
        const startHeight = currentHeight;

        const onMouseMove = (moveEvent: MouseEvent) => {
          const delta = section === 'top' ? moveEvent.clientY - startY : startY - moveEvent.clientY;
          const newHeight = Math.max(MIN_SECTION_HEIGHT, startHeight + delta);
          setter(newHeight);
        };

        const onMouseUp = () => {
          document.removeEventListener('mousemove', onMouseMove);
          document.removeEventListener('mouseup', onMouseUp);
        };

        document.addEventListener('mousemove', onMouseMove);
        document.addEventListener('mouseup', onMouseUp);
      },
    []
  );

  // Keyboard handlers for resize handles
  const userFrameResizeKeyHandler = useMemo(
    () =>
      createResizeKeyHandler({
        axis: 'vertical',
        direction: 'grow',
        min: MIN_SECTION_HEIGHT,
        max: 600,
        getValue: () => userFrameBarHeight,
        setValue: setUserFrameBarHeight,
      }),
    [userFrameBarHeight, setUserFrameBarHeight]
  );

  const streamBarResizeKeyHandler = useMemo(
    () =>
      createResizeKeyHandler({
        axis: 'vertical',
        direction: 'shrink',
        min: MIN_SECTION_HEIGHT,
        max: 600,
        getValue: () => streamBarHeight,
        setValue: setStreamBarHeight,
      }),
    [streamBarHeight, setStreamBarHeight]
  );

  const chatVerticalResizeKeyHandler = useMemo(
    () =>
      createResizeKeyHandler({
        axis: 'horizontal',
        direction: 'shrink',
        min: MIN_CHAT_WIDTH,
        max: 1200,
        getValue: () => voiceTextChatWidth,
        setValue: setVoiceTextChatWidth,
      }),
    [voiceTextChatWidth, setVoiceTextChatWidth]
  );

  const chatHorizontalResizeKeyHandler = useMemo(
    () =>
      createResizeKeyHandler({
        axis: 'vertical',
        direction: 'shrink',
        min: MIN_CHAT_HEIGHT,
        max: 1200,
        getValue: () => voiceTextChatHeight,
        setValue: setVoiceTextChatHeight,
      }),
    [voiceTextChatHeight, setVoiceTextChatHeight]
  );

  // Not connected — show join prompt
  if (!isConnected) {
    const memberCount = channelVoiceMembers[channelId]?.length || 0;
    return (
      <JoinPrompt
        channelName={channelName}
        isConnecting={isConnecting}
        memberCount={memberCount}
        onJoin={handleJoin}
      />
    );
  }

  const showChat = showVoiceTextChat && hasLinkedText;
  const isVerticalLayout = showChat && voiceTextChatLayout === 'vertical';
  const tunedInCount = Object.keys(tunedInScreenShares).length;
  const nonDominantCount = tunedInCount - 1;
  const showStreamSection = stageLayout === 'focus' && nonDominantCount > 0;

  return (
    <div className="voice-view" ref={containerRef}>
      {/* Header */}
      <div className="voice-view__header">
        <h2 className="voice-view__channel-name">{channelName}</h2>
        <HeaderBadges
          participantCount={participantCount}
          effectiveQualityTier={effectiveQualityTier}
          decoderHealth={decoderHealth}
        />
      </div>

      {/* Content wrapper: voice area + optional text chat (switches flex direction) */}
      <div
        className={`voice-view__content ${isVerticalLayout ? 'voice-view__content--vertical' : 'voice-view__content--horizontal'}`}
      >
        {/* Voice area — one resizable entity vs text chat */}
        <div className="voice-view__voice-area">
          {hasScreenShares ? (
            <ScreenShareLayout
              showUserFrameBar={showUserFrameBar}
              userFrameBarHeight={userFrameBarHeight}
              onUserFrameResize={makeSectionResizeStart(
                'top',
                userFrameBarHeight,
                setUserFrameBarHeight
              )}
              userFrameResizeKeyHandler={userFrameResizeKeyHandler}
              toggleUserFrameBar={toggleUserFrameBar}
              showStreamSection={showStreamSection}
              streamBarHeight={streamBarHeight}
              onStreamBarResize={makeSectionResizeStart(
                'bottom',
                streamBarHeight,
                setStreamBarHeight
              )}
              streamBarResizeKeyHandler={streamBarResizeKeyHandler}
              toggleStreamBar={toggleStreamBar}
              showStreamBar={showStreamBar}
            />
          ) : (
            <UserFrameGrid />
          )}

          {/* Available screen shares — Tune In buttons */}
          {availableScreenShares.length > 0 && <TuneInOverlay />}
        </div>

        {/* Text chat panel — vertical (side-by-side) layout only */}
        {showChat && isVerticalLayout && (
          <>
            <button
              type="button"
              className="voice-text-chat-resize voice-text-chat-resize--vertical"
              onMouseDown={handleChatResizeStart}
              onKeyDown={chatVerticalResizeKeyHandler}
              tabIndex={0}
              aria-label="Resize voice text chat"
            >
              <div className="voice-text-chat-resize__grip" />
            </button>
            <div
              className="voice-view__text-chat voice-view__text-chat--vertical"
              style={{ width: voiceTextChatWidth }}
            >
              <VoiceTextChat />
            </div>
          </>
        )}
      </div>

      {/* Controls bar — attached to voice area, above horizontal text chat */}
      <VoiceControls />

      {/* Text chat panel — horizontal (bottom) layout only */}
      {showChat && !isVerticalLayout && (
        <>
          <button
            type="button"
            className="voice-text-chat-resize"
            onMouseDown={handleChatResizeStart}
            onKeyDown={chatHorizontalResizeKeyHandler}
            tabIndex={0}
            aria-label="Resize voice text chat"
          >
            <div className="voice-text-chat-resize__grip" />
          </button>
          <div className="voice-view__text-chat" style={{ height: voiceTextChatHeight }}>
            <VoiceTextChat />
          </div>
        </>
      )}
    </div>
  );
};

export default VoiceView;
