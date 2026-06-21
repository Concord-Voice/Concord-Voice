import React, { useEffect, useMemo, useState } from 'react';
import { MessageSquare, Volume2, PanelBottom, PanelRight } from 'lucide-react';
import MessageList from '../Chat/MessageList';
import MessageInput from '../Chat/MessageInput';
import { useChannelStore } from '../../stores/channelStore';
import { useVoiceStore } from '../../stores/voiceStore';
import { useUserStore } from '../../stores/userStore';
import { useTTSSettingsStore } from '../../stores/ttsSettingsStore';
import { useServerStore } from '../../stores/serverStore';
import { useChannelSubscription } from '../../hooks/useChannelSubscription';
import { useMessageFetch } from '../../hooks/useMessageFetch';
import { useChatController } from '../../hooks/useChatController';
import type { ChatContext } from '../../types/chat';
import './VoiceTextChat.css';

const VoiceTextChat: React.FC = () => {
  const activeChannelId = useVoiceStore((s) => s.activeChannelId);
  const voiceTextChatLayout = useVoiceStore((s) => s.voiceTextChatLayout);
  const toggleVoiceTextChatLayout = useVoiceStore((s) => s.toggleVoiceTextChatLayout);
  const getLinkedTextChannel = useChannelStore((s) => s.getLinkedTextChannel);

  const linkedChannel = activeChannelId ? getLinkedTextChannel(activeChannelId) : undefined;
  const channelId = linkedChannel?.id ?? null;

  const user = useUserStore((s) => s.user);
  const activeServerId = useServerStore((s) => s.activeServerId);

  const ctx: ChatContext = useMemo(
    () => ({
      type: 'voice' as const,
      id: channelId || '',
      serverId: activeServerId ?? undefined,
    }),
    [channelId, activeServerId]
  );

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

  // Subscribe to the linked text channel for real-time messages
  useChannelSubscription(channelId);

  // Shared fetch/decrypt/paginate logic
  const { messages, isLoading, hasMore, error, handleLoadMore } = useMessageFetch(channelId, {
    type: 'channel',
  });

  const currentUserId = user?.id || '';

  const handleSendMessage = (
    content: string,
    mentionMeta?: string,
    replyToId?: string,
    attachmentIds?: string[],
    attachments?: import('../../types/chat').AttachmentSummary[]
  ) => {
    if (!channelId) return;
    sendMessage(content, { mentionMeta, replyToId, attachmentIds, attachments });
  };

  if (!channelId || !linkedChannel) {
    return (
      <div className="voice-text-chat voice-text-chat--empty">
        <MessageSquare size={20} />
        <span>No text channel linked</span>
      </div>
    );
  }

  return (
    <div className="voice-text-chat">
      <div className="voice-text-chat__header">
        <MessageSquare size={14} />
        <span className="voice-text-chat__title">{linkedChannel.name} Text Chat</span>
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
          key={channelId}
          messages={messages}
          currentUserId={currentUserId}
          chatContext={chatContext}
          channelName={linkedChannel.name}
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

      <div className="voice-text-chat__input">
        <MessageInput
          onSendMessage={handleSendMessage}
          onTyping={sendTyping}
          channelName={linkedChannel.name}
          disabled={!currentUserId}
          placeholder={`Message ${linkedChannel.name} text chat...`}
          replyingTo={replyingTo}
          onCancelReply={cancelReply}
        />
      </div>
    </div>
  );
};

export default VoiceTextChat;
