import React, { useCallback, useEffect, useRef, useState } from 'react';
import { MessageSquare, Volume2, PanelBottom, PanelRight } from 'lucide-react';
import MessageList from '../Chat/MessageList';
import MessageInput from '../Chat/MessageInput';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useDMStore } from '../../stores/chat/dmStore';
import { usePrivacyStore } from '../../stores/ui/privacyStore';
import { useTTSSettingsStore } from '../../stores/audio/ttsSettingsStore';
import { useMessageFetch } from '../../hooks/messaging/useMessageFetch';
import { useReadMarker } from '../../hooks/messaging/useReadMarker';
import { apiFetch } from '../../services/system/apiClient';
import { useChatController } from '../../hooks/messaging/useChatController';
import { useVoiceTextChatTarget } from '../../hooks/voice/useVoiceTextChatTarget';
import './VoiceTextChat.css';

const VoiceTextChat: React.FC = () => {
  const voiceTextChatLayout = useVoiceStore((s) => s.voiceTextChatLayout);
  const toggleVoiceTextChatLayout = useVoiceStore((s) => s.toggleVoiceTextChatLayout);
  const user = useUserStore((s) => s.user);
  const dmPrivacyLevel = usePrivacyStore((s) => s.settings.dmPrivacyLevel);

  // DM-vs-server target resolution + subscription wiring lives in the hook
  // (#1873) so this component stays within the S3776 cognitive-complexity bound.
  const { isDMCall, targetId, targetName, fetchType, ctx } = useVoiceTextChatTarget();

  const {
    sendMessage,
    editMessage,
    deleteMessage,
    replyingTo,
    handleReply,
    cancelReply,
    canPin,
    handlePinToggle,
    sendTyping,
    chatContext,
  } = useChatController(ctx);

  const ttsEnabled = useTTSSettingsStore((s) => s.ttsEnabled);
  const [isSpeaking, setIsSpeaking] = useState(false);

  // Poll speechSynthesis.speaking for TTS indicator
  useEffect(() => {
    if (!ttsEnabled || !globalThis.speechSynthesis) return;
    const interval = setInterval(() => {
      setIsSpeaking(globalThis.speechSynthesis.speaking);
    }, 250);
    return () => clearInterval(interval);
  }, [ttsEnabled]);

  // Shared fetch/decrypt/paginate logic
  // The open-time read for a DM call: DMChatArea's ran when the conversation
  // was opened, but a backlog that arrived while this drawer was closed is
  // hydration when it reopens, and hydration is never "seen". Once per
  // target ENTRY: the guard clears when the target changes, so coming back
  // to a conversation after another target (a DM, or a linked channel, which
  // never writes the guard) reads its backlog again as opening it would,
  // while a reconnect refetch of the same target does not. The
  // linked-channel case is ChannelList's voice-linked effect.
  const openReadTargetRef = useRef<string | null>(null);
  useEffect(() => {
    openReadTargetRef.current = null;
  }, [targetId]);
  // Read claims per target — open-time attempts and queued live markers —
  // DMChatArea's fence: an open-time failure landing after a newer claim
  // must not restore a count that claim clears. Keyed by target, not one
  // number: no mount site gives this panel a key, so one instance survives a
  // target change, and a claim on the next target must not fence the
  // previous target's rollback.
  const readGensRef = useRef(new Map<string, number>());
  const readPath = isDMCall
    ? `/api/v1/dm/conversations/${targetId}/read`
    : `/api/v1/channels/${targetId}/read`;
  const handleFetchComplete = useCallback(() => {
    if (!isDMCall || !targetId || openReadTargetRef.current === targetId) return;
    openReadTargetRef.current = targetId;
    const previousUnread =
      useDMStore.getState().conversations.find((c) => c.id === targetId)?.unreadCount ?? 0;
    // A new attempt claims the marker as a queued marker does: an older
    // attempt still in flight (A → B → A before its read returned) must not
    // restore a baseline this one superseded when it fails late. If every
    // claim fails the count stays low, the safe direction.
    const gens = readGensRef.current;
    const gen = (gens.get(targetId) ?? 0) + 1;
    gens.set(targetId, gen);
    useDMStore.getState().clearUnread(targetId);
    apiFetch(readPath, { method: 'POST' })
      .then((res) => {
        if (!res.ok) throw new Error(`open-time read rejected: HTTP ${res.status}`);
      })
      .catch((error: unknown) => {
        console.error(
          '[VoiceTextChat] Failed to mark conversation as read:',
          error instanceof Error ? error.message : String(error)
        );
        if (openReadTargetRef.current === targetId) openReadTargetRef.current = null;
        if ((readGensRef.current.get(targetId) ?? 0) !== gen) return; // a newer marker made the server current
        // The server still counts these: put them back, as DMChatArea does.
        useDMStore.getState().incrementUnread(targetId, previousUnread);
      });
  }, [isDMCall, targetId, readPath]);

  const { messages, isLoading, hasMore, error, handleLoadMore } = useMessageFetch(targetId, {
    type: fetchType,
    onFetchComplete: handleFetchComplete,
  });

  const currentUserId = user?.id || '';

  // Advance the read marker while the panel is open (#3289): a message read
  // as it arrives here. Same shape as ChatView and DMChatArea: debounced,
  // flushed when the list stops following, and a non-2xx is rejected so the
  // hook logs it. A DM call's local unread count is cleared before marking,
  // as DMChatArea does — the active conversation may be another one, so the
  // WebSocket handler keeps counting this one.
  const { markSeen, flush: flushSeen } = useReadMarker(async () => {
    if (!targetId) return;
    const res = await apiFetch(readPath, { method: 'POST' });
    if (!res.ok) throw new Error(`read marker rejected: HTTP ${res.status}`);
  }, targetId);
  const handleLatestSeen = useCallback(() => {
    if (isDMCall && targetId) {
      const unread =
        useDMStore.getState().conversations.find((c) => c.id === targetId)?.unreadCount ?? 0;
      if (unread > 0) useDMStore.getState().clearUnread(targetId);
      const gens = readGensRef.current;
      gens.set(targetId, (gens.get(targetId) ?? 0) + 1); // queued, see readGensRef
    }
    markSeen();
  }, [isDMCall, targetId, markSeen]);

  const handleSendMessage = (
    content: string,
    mentionMeta?: string,
    replyToId?: string,
    attachmentIds?: string[],
    attachments?: import('../../types/chat').AttachmentSummary[]
  ) => {
    if (!targetId) return;
    sendMessage(content, { mentionMeta, replyToId, attachmentIds, attachments });
  };

  if (!targetId) {
    return (
      <div className="voice-text-chat voice-text-chat--empty">
        <MessageSquare size={20} />
        <span>{isDMCall ? 'No conversation' : 'No text channel linked'}</span>
      </div>
    );
  }

  return (
    <div className="voice-text-chat">
      <div className="voice-text-chat__header">
        <MessageSquare size={14} />
        <span className="voice-text-chat__title">{targetName} Text Chat</span>
        <button
          type="button"
          className="voice-text-chat__layout-toggle"
          onClick={toggleVoiceTextChatLayout}
          title={
            voiceTextChatLayout === 'horizontal'
              ? 'Switch to side layout'
              : 'Switch to bottom layout'
          }
        >
          {voiceTextChatLayout === 'horizontal' ? (
            <PanelRight size={14} />
          ) : (
            <PanelBottom size={14} />
          )}
        </button>
      </div>

      {error && <div className="voice-text-chat__error">{error}</div>}

      <div className="voice-text-chat__messages">
        <MessageList
          key={targetId}
          messages={messages}
          currentUserId={currentUserId}
          persistenceKey={targetId}
          chatContext={chatContext}
          channelName={targetName}
          isLoading={isLoading}
          hasMore={hasMore}
          onLoadMore={handleLoadMore}
          onEditMessage={editMessage}
          onDeleteMessage={deleteMessage}
          onReply={handleReply}
          onPinToggle={handlePinToggle}
          canPin={canPin}
          onLatestSeen={handleLatestSeen}
          onLatestLeft={flushSeen}
        />
      </div>

      {isSpeaking && (
        <div className="voice-text-chat__tts-indicator">
          <Volume2 size={12} />
          <span>Speaking...</span>
        </div>
      )}

      {isDMCall && dmPrivacyLevel === 0 ? (
        // Preserve the DM privacy-disabled behavior from DMChatArea (#1873):
        // when the local user has globally disabled DMs, the voice text panel
        // shows the same notice instead of a composer. Server voice is unaffected.
        <div className="dm-disabled-notice">
          All DMs have been disabled. Change your privacy settings to restore DMs.
        </div>
      ) : (
        <div className="voice-text-chat__input">
          <MessageInput
            onSendMessage={handleSendMessage}
            onTyping={sendTyping}
            channelName={targetName}
            disabled={!currentUserId}
            placeholder={`Message ${targetName} text chat...`}
            replyingTo={replyingTo}
            onCancelReply={cancelReply}
          />
        </div>
      )}
    </div>
  );
};

export default VoiceTextChat;
