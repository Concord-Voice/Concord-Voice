import React, { useRef, useCallback, useEffect, useMemo } from 'react';
import { useVoiceStore } from '../../stores/voiceStore';
import { createResizeKeyHandler } from '../../utils/resizeKeyboard';
import { useChannelStore } from '../../stores/channelStore';
import VoiceControls from './VoiceControls';
import VoiceTextChat from './VoiceTextChat';
import './PersistentVoiceBar.css';

const MIN_CHAT_HEIGHT = 150;
const MAX_CHAT_RATIO = 0.7;

/**
 * Persistent voice controls bar rendered at the bottom of the chat area
 * when the user is in voice but viewing a different channel.
 *
 * Includes: full VoiceControls, optional text chat drawer, pin/unpin,
 * and pop-out to Electron PiP window.
 *
 * In vertical layout mode, the text chat panel is rendered by MainView
 * as a side column instead of inside this bar.
 */
const PersistentVoiceBar: React.FC = () => {
  const activeChannelId = useVoiceStore((s) => s.activeChannelId);
  const voiceControlsPinned = useVoiceStore((s) => s.voiceControlsPinned);
  const voiceControlsPoppedOut = useVoiceStore((s) => s.voiceControlsPoppedOut);
  const setVoiceControlsPoppedOut = useVoiceStore((s) => s.setVoiceControlsPoppedOut);
  const showVoiceTextChat = useVoiceStore((s) => s.showVoiceTextChat);
  const voiceTextChatLayout = useVoiceStore((s) => s.voiceTextChatLayout);
  const persistentTextChatHeight = useVoiceStore((s) => s.persistentTextChatHeight);
  const setPersistentTextChatHeight = useVoiceStore((s) => s.setPersistentTextChatHeight);

  const getLinkedTextChannel = useChannelStore((s) => s.getLinkedTextChannel);
  const hasLinkedText = !!(activeChannelId && getLinkedTextChannel(activeChannelId));
  const isVertical = voiceTextChatLayout === 'vertical';
  // In vertical mode, the text chat is rendered as a side panel by MainView
  const showChat = showVoiceTextChat && hasLinkedText && !isVertical;

  const containerRef = useRef<HTMLDivElement>(null);
  const resizingRef = useRef(false);

  // Electron PiP for pop-out controls
  const handlePopOut = useCallback(async () => {
    if (!globalThis.electron?.openPipWindow) return;

    await globalThis.electron.openPipWindow({
      id: 'controls-main',
      width: 480,
      height: 80,
      title: 'Concord Voice Controls',
    });
    setVoiceControlsPoppedOut(true);
  }, [setVoiceControlsPoppedOut]);

  // Listen for PiP close to restore inline bar
  useEffect(() => {
    if (!voiceControlsPoppedOut) return;
    const cleanup = globalThis.electron?.onPipClosed?.((closedId: string) => {
      if (closedId === 'controls-main') {
        setVoiceControlsPoppedOut(false);
      }
    });
    return () => {
      cleanup?.();
    };
  }, [voiceControlsPoppedOut, setVoiceControlsPoppedOut]);

  // Resize handler for text chat drawer (horizontal only)
  const handleChatResizeStart = useCallback(
    (e: React.MouseEvent) => {
      e.preventDefault();
      resizingRef.current = true;
      const startY = e.clientY;
      const startHeight = persistentTextChatHeight;

      document.body.style.cursor = 'row-resize';
      document.body.style.userSelect = 'none';

      const onMouseMove = (moveEvent: MouseEvent) => {
        if (!resizingRef.current || !containerRef.current) return;
        const parentRect = containerRef.current.parentElement?.getBoundingClientRect();
        const maxHeight = parentRect ? parentRect.height * MAX_CHAT_RATIO : 600;
        const delta = startY - moveEvent.clientY;
        const newHeight = Math.min(maxHeight, Math.max(MIN_CHAT_HEIGHT, startHeight + delta));
        setPersistentTextChatHeight(newHeight);
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
    [persistentTextChatHeight, setPersistentTextChatHeight]
  );

  const chatResizeKeyHandler = useMemo(
    () =>
      createResizeKeyHandler({
        axis: 'vertical',
        direction: 'shrink',
        min: MIN_CHAT_HEIGHT,
        max: 1200,
        getValue: () => persistentTextChatHeight,
        setValue: setPersistentTextChatHeight,
      }),
    [persistentTextChatHeight, setPersistentTextChatHeight]
  );

  // When popped out to Electron PiP, hide the inline bar
  if (voiceControlsPoppedOut) {
    return null;
  }

  return (
    <div
      ref={containerRef}
      className={`persistent-voice-bar ${voiceControlsPinned ? 'persistent-voice-bar--pinned' : 'persistent-voice-bar--unpinned'}`}
    >
      {/* Text chat drawer (above controls) */}
      {showChat && voiceControlsPinned && (
        <>
          <button
            type="button"
            className="persistent-voice-bar__chat-resize"
            onMouseDown={handleChatResizeStart}
            onKeyDown={chatResizeKeyHandler}
            tabIndex={0}
            aria-label="Resize voice text chat"
          >
            <div className="persistent-voice-bar__chat-resize-grip" />
          </button>
          <div
            className="persistent-voice-bar__chat-drawer"
            style={{ height: persistentTextChatHeight }}
          >
            <VoiceTextChat />
          </div>
        </>
      )}

      {/* Full controls bar */}
      <VoiceControls
        context="persistent"
        onPopOut={
          typeof globalThis.electron?.openPipWindow === 'function' ? handlePopOut : undefined
        }
      />
    </div>
  );
};

export default PersistentVoiceBar;
