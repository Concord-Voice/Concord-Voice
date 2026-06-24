import React, { useEffect, useState } from 'react';
import { MessageSquare, Volume2, PanelBottom, PanelRight } from 'lucide-react';
import MessageList from '../Chat/MessageList';
import MessageInput from '../Chat/MessageInput';
import { useVoiceStore } from '../../stores/voiceStore';
import { useUserStore } from '../../stores/userStore';
import { usePrivacyStore } from '../../stores/privacyStore';
import { useTTSSettingsStore } from '../../stores/ttsSettingsStore';
import { useMessageFetch } from '../../hooks/useMessageFetch';
import { useChatController } from '../../hooks/useChatController';
import { useVoiceTextChatTarget } from '../../hooks/useVoiceTextChatTarget';
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
  const { messages, isLoading, hasMore, error, handleLoadMore } = useMessageFetch(targetId, {
    type: fetchType,
  });

  const currentUserId = user?.id || '';

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
