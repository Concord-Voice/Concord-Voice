import { useState, useEffect, useRef, useCallback } from 'react';
import { MessageSquare, Users, BookOpen, UserPlus, PenLine } from 'lucide-react';
import { useDMStore, type DMConversation, type DMParticipant } from '../../stores/dmStore';
import { useUserStore } from '../../stores/userStore';
import { useVoiceStore } from '../../stores/voiceStore';
import { useFriendStore } from '../../stores/friendStore';
import { e2eeService } from '../../services/e2eeService';
import { useDraftMessageStore } from '../../stores/draftMessageStore';
import { errorMessage } from '../../utils/redactError';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { resolveUserAccentColors } from '../../utils/schemeColors';
import CreateGroupModal from './CreateGroupModal';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import { DIRECT_MESSAGES_CONTEXT_AREA } from '../ui/ContextMenuProvider';
import DMConversationContextMenu from './DMConversationContextMenu';
import DMProfileModal from './DMProfileModal';
import './DirectMessages.css';

interface ConversationListProps {
  selectedThreadId: string | null;
  onSelectThread: (id: string) => void;
}

interface ConversationAvatarProps {
  conv: DMConversation;
  currentUserId: string;
  other?: DMParticipant;
  status: string;
}

/** Get display name for a conversation (other user's name for 1:1, group name for group) */
function getConversationName(conv: DMConversation, currentUserId: string): string {
  if (conv.isGroup) {
    return conv.name || conv.participants.map((p) => p.displayName || p.username).join(', ');
  }
  // 1:1 — show the other participant's name
  const other = conv.participants.find((p) => p.userId !== currentUserId);
  return other?.displayName || other?.username || 'Unknown';
}

/** Get the initial letter for the avatar */
function getInitial(conv: DMConversation, currentUserId: string): string {
  return getConversationName(conv, currentUserId).charAt(0).toUpperCase();
}

/** Get relative timestamp string */
function getRelativeTime(dateStr: string): string {
  const date = new Date(dateStr);
  const now = new Date();
  const diffMs = now.getTime() - date.getTime();
  const diffMin = Math.floor(diffMs / 60000);

  if (diffMin < 1) return 'Just now';
  if (diffMin < 60) return `${diffMin}m ago`;

  const diffHrs = Math.floor(diffMin / 60);
  if (diffHrs < 24) return `${diffHrs}h ago`;

  const diffDays = Math.floor(diffHrs / 24);
  if (diffDays === 1) return 'Yesterday';
  if (diffDays < 7) return `${diffDays}d ago`;

  return date.toLocaleDateString();
}

const ConversationAvatar: React.FC<ConversationAvatarProps> = ({
  conv,
  currentUserId,
  other,
  status,
}) => {
  const [failedAvatarUrl, setFailedAvatarUrl] = useState<string | null>(null);
  const avatarSrc = resolveMediaUrl(conv.isGroup ? conv.iconUrl : other?.avatarUrl);
  const showImage = avatarSrc && avatarSrc !== failedAvatarUrl;
  const fallback = conv.isGroup ? (
    <Users size={18} />
  ) : (
    <span
      className="conversation-avatar-initial"
      style={(() => {
        const colors = resolveUserAccentColors(other?.colorScheme);
        return colors ? { background: colors.gradient } : undefined;
      })()}
    >
      {getInitial(conv, currentUserId)}
    </span>
  );

  return (
    <div className={`conversation-avatar${conv.isGroup ? ' group' : ''}`}>
      {showImage ? (
        <img
          src={avatarSrc}
          alt=""
          className="conversation-avatar-img"
          onError={() => setFailedAvatarUrl(avatarSrc)}
        />
      ) : (
        fallback
      )}
      {!conv.isGroup && <span className={`member-status-dot ${status}`} />}
    </div>
  );
};

const ConversationList: React.FC<ConversationListProps> = ({
  selectedThreadId,
  onSelectThread,
}) => {
  const [search, setSearch] = useState('');
  const [isCreateGroupOpen, setIsCreateGroupOpen] = useState(false);
  const [contextMenu, setContextMenu] = useState<{
    conversation: DMConversation;
    position: { x: number; y: number };
  } | null>(null);
  // Modal state for destructive 1:1 DM context-menu actions (#984 expansion).
  // Lifted from DMConversationContextMenu so the modal continues rendering
  // after the menu's onClose() unmounts the menu — mirrors MemberListPanel's
  // Ban / Kick lifted-state pattern.
  const [blockTarget, setBlockTarget] = useState<DMConversation | null>(null);
  const [unfriendTarget, setUnfriendTarget] = useState<DMConversation | null>(null);
  // DM Profile modal target (#1208). Same lifted-state pattern as block/unfriend
  // so the modal continues rendering after the context menu unmounts.
  const [profileTarget, setProfileTarget] = useState<DMConversation | null>(null);
  const blockUser = useFriendStore((s) => s.blockUser);
  const removeFriend = useFriendStore((s) => s.removeFriend);
  const conversations = useDMStore((s) => s.conversations);
  const fetchConversations = useDMStore((s) => s.fetchConversations);
  const openPersonalThread = useDMStore((s) => s.openPersonalThread);
  const currentUserId = useUserStore((s) => s.user?.id) || '';

  // In-call indicator (#1209 plan task F5): returns the convId IF the
  // local user is currently in a DM voice call, else null. The selector
  // returns a string|null so the comparison `inCallForThisConv === conv.id`
  // re-renders only the matching list item when state transitions. Used for
  // the 1:1 🔊 badge.
  const inCallForThisConv = useVoiceStore((s) =>
    s.callState.kind === 'in-call' && s.isDMCall ? s.dmConversationId : null
  );
  // Multi-participant in-call rosters (#1219 R6). Unlike `inCallForThisConv`
  // (which only fires when the LOCAL user is in the call), this surfaces the
  // "N of M in call" indicator for any group call — including ones the user
  // has not joined — so the list advertises joinable calls.
  const activeDMCalls = useVoiceStore((s) => s.activeDMCalls);
  const drafts = useDraftMessageStore((s) => s.drafts);

  // Cache of decrypted last message previews (conversationId → {plaintext, ciphertext})
  const [decryptedPreviews, setDecryptedPreviews] = useState<
    Record<string, { text: string; cipher: string }>
  >({});
  const decryptingRef = useRef<Set<string>>(new Set());

  useEffect(() => {
    fetchConversations();
  }, [fetchConversations]);

  // Decrypt last message previews for encrypted conversations
  const decryptPreview = useCallback(async (convId: string, ciphertext: string) => {
    if (decryptingRef.current.has(convId)) return;
    decryptingRef.current.add(convId);
    try {
      if (!e2eeService.isInitialized) return;
      const plaintext = await e2eeService.decryptForChannel(convId, ciphertext);
      if (plaintext) {
        setDecryptedPreviews((prev) => ({
          ...prev,
          [convId]: { text: plaintext, cipher: ciphertext },
        }));
      }
    } catch {
      // Decryption failed — keep showing placeholder
    } finally {
      decryptingRef.current.delete(convId);
    }
  }, []);

  useEffect(() => {
    for (const conv of conversations) {
      if (conv.lastMessage?.content && !conv.lastMessage.plaintextPreview) {
        // Re-decrypt if no cache entry or the ciphertext changed (new message)
        const cached = decryptedPreviews[conv.id];
        if (!cached || cached.cipher !== conv.lastMessage.content) {
          decryptPreview(conv.id, conv.lastMessage.content);
        }
      }
    }
  }, [conversations, decryptedPreviews, decryptPreview]);

  // Separate personal thread from regular conversations
  const personalThread = conversations.find((c) => c.isPersonal);

  const filtered = conversations
    .filter((c) => !c.isPersonal)
    .filter((c) => {
      const name = getConversationName(c, currentUserId);
      return name.toLowerCase().includes(search.toLowerCase());
    });

  const handleOpenPersonalThread = async () => {
    try {
      const conv = await openPersonalThread();
      onSelectThread(conv.id);
    } catch (err) {
      console.error('Failed to open personal thread:', errorMessage(err));
    }
  };

  return (
    <div className="conversation-list" data-context-area={DIRECT_MESSAGES_CONTEXT_AREA}>
      <div className="conversation-search">
        <input
          type="text"
          placeholder="Search conversations..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <button
          type="button"
          className="create-group-btn"
          onClick={() => setIsCreateGroupOpen(true)}
          aria-label="Create Group DM"
          title="Create Group DM"
        >
          <UserPlus size={16} />
        </button>
      </div>

      {/* Pinned Personal Thread */}
      <button
        type="button"
        className={`conversation-item personal-thread${
          personalThread && selectedThreadId === personalThread.id ? ' active' : ''
        }`}
        onClick={handleOpenPersonalThread}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            handleOpenPersonalThread();
          }
        }}
        aria-label="Personal Thread"
      >
        <div className="conversation-avatar personal-thread-avatar">
          <BookOpen size={14} />
        </div>
        <span className="conversation-name personal-thread-name">Personal Thread</span>
      </button>
      <div className="personal-thread-divider" />

      {filtered.length === 0 ? (
        <div className="conversation-list-empty">
          <MessageSquare size={32} />
          <p>No conversations yet</p>
        </div>
      ) : (
        filtered.map((conv) => {
          const name = getConversationName(conv, currentUserId);
          const other = conv.isGroup
            ? null
            : conv.participants.find((p) => p.userId !== currentUserId);
          const status = other?.status || 'offline';
          const lastTime = conv.lastMessage?.createdAt || conv.createdAt;
          let preview = '';
          if (conv.lastMessage) {
            // Use optimistic local plaintext first, otherwise decrypted preview if available.
            preview =
              conv.lastMessage.plaintextPreview ||
              decryptedPreviews[conv.id]?.text ||
              'Encrypted message';
            // If no text content, show attachment type or generic fallback
            if (!preview || preview === 'Encrypted message') {
              if (conv.lastMessage.attachmentType) {
                preview = conv.lastMessage.attachmentType;
              }
            }
          }

          return (
            <button
              type="button"
              key={conv.id}
              className={`conversation-item${selectedThreadId === conv.id ? ' active' : ''}${conv.unreadCount > 0 ? ' unread' : ''}`}
              onClick={() => onSelectThread(conv.id)}
              onContextMenu={(e) => {
                e.preventDefault();
                setContextMenu({
                  conversation: conv,
                  position: { x: e.clientX, y: e.clientY },
                });
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault();
                  onSelectThread(conv.id);
                }
              }}
              aria-label={name}
            >
              <ConversationAvatar
                conv={conv}
                currentUserId={currentUserId}
                other={other ?? undefined}
                status={status}
              />

              <div className="conversation-content">
                <div className="conversation-top-row">
                  <span className="conversation-name">{name}</span>
                  <span className="conversation-time">{getRelativeTime(lastTime)}</span>
                </div>
                <div className="conversation-bottom-row">
                  <span className="conversation-preview">{preview}</span>
                  <div className="conversation-badges">
                    {/* In-call indicator. Group conversations with an active
                        voice call show a multi-participant "N of M in call"
                        tally (#1219 R6) — surfaced even for calls the local
                        user has NOT joined, so the list advertises joinable
                        calls. 1:1 conversations keep the 🔊 badge (#1209 F5),
                        shown only when the local user IS in the call.
                        M = the live `conv.participants.length` (this conv is
                        loaded — we're rendering it), NOT the stored roster
                        `total`, which can be 0 if the dm_voice_state_update
                        delta arrived before the conversation was in dmStore
                        (#1568 Gitar "N of 0 in call" edge-case fix). */}
                    {conv.isGroup && activeDMCalls[conv.id] ? (
                      <span
                        className="conversation-in-call-badge group"
                        title={`${activeDMCalls[conv.id].participantIds.length} of ${conv.participants.length} in call`}
                        aria-label={`${activeDMCalls[conv.id].participantIds.length} of ${conv.participants.length} in call`}
                      >
                        {activeDMCalls[conv.id].participantIds.length} of {conv.participants.length}{' '}
                        in call
                      </span>
                    ) : (
                      inCallForThisConv === conv.id && (
                        <span
                          className="conversation-in-call-badge"
                          title="In voice call"
                          aria-label="In voice call"
                        >
                          🔊
                        </span>
                      )
                    )}
                    {drafts[conv.id] && (
                      <span className="conversation-draft-indicator" title="Draft message">
                        <PenLine size={12} />
                      </span>
                    )}
                    {conv.unreadCount > 0 && (
                      <span className="conversation-unread-badge">
                        {conv.unreadCount > 99 ? '99+' : conv.unreadCount}
                      </span>
                    )}
                  </div>
                </div>
              </div>
            </button>
          );
        })
      )}
      <CreateGroupModal isOpen={isCreateGroupOpen} onClose={() => setIsCreateGroupOpen(false)} />
      {contextMenu && (
        <DMConversationContextMenu
          conversation={contextMenu.conversation}
          currentUserId={currentUserId}
          position={contextMenu.position}
          onClose={() => setContextMenu(null)}
          onBlockUser={(conv) => setBlockTarget(conv)}
          onUnfriend={(conv) => setUnfriendTarget(conv)}
          onViewProfile={(conv) => setProfileTarget(conv)}
        />
      )}

      {/* Block-user confirmation (#984). The peer is computed from the
          conversation's participants by filtering out the current user. The
          confirmation copy uses the peer's display name (or username
          fallback) to make the destructive action unambiguous. */}
      <ConfirmActionModal
        isOpen={blockTarget !== null}
        onClose={() => setBlockTarget(null)}
        title={(() => {
          if (!blockTarget) return 'Block User';
          const peer = blockTarget.participants.find((p) => p.userId !== currentUserId);
          return `Block ${peer?.displayName || peer?.username || 'User'}`;
        })()}
        message="They will no longer be able to send you friend requests, DM you, or see your profile. You can unblock them later from Settings."
        confirmLabel="Block"
        loadingLabel="Blocking..."
        onConfirm={async () => {
          if (!blockTarget) return;
          const peer = blockTarget.participants.find((p) => p.userId !== currentUserId);
          if (!peer) throw new Error('No peer found in 1:1 conversation');
          await blockUser(peer.userId);
        }}
      />

      {/* Unfriend confirmation (#984). Removes the friend relationship but
          does NOT block — the user can still receive friend requests from
          this person in the future. */}
      <ConfirmActionModal
        isOpen={unfriendTarget !== null}
        onClose={() => setUnfriendTarget(null)}
        title={(() => {
          if (!unfriendTarget) return 'Unfriend';
          const peer = unfriendTarget.participants.find((p) => p.userId !== currentUserId);
          return `Unfriend ${peer?.displayName || peer?.username || 'User'}`;
        })()}
        message="You can re-add them via a friend request later, but you will lose any shared friend status until they accept again."
        confirmLabel="Unfriend"
        loadingLabel="Removing..."
        onConfirm={async () => {
          if (!unfriendTarget) return;
          const peer = unfriendTarget.participants.find((p) => p.userId !== currentUserId);
          if (!peer) throw new Error('No peer found in 1:1 conversation');
          await removeFriend(peer.userId);
        }}
      />

      {/* DM Profile modal (#1208). Shown when a peer is set via the context-menu
          "View Profile" item. onVoiceCall is intentionally undefined — #1209's
          follow-up adds the callback that hooks into the voice-call subsystem.
          Block / Unfriend reuse the existing lifted-state confirmation modals. */}
      {profileTarget &&
        (() => {
          const peer = profileTarget.participants.find((p) => p.userId !== currentUserId);
          if (!peer) return null;
          return (
            <DMProfileModal
              isOpen={true}
              onClose={() => setProfileTarget(null)}
              peer={peer}
              conversation={profileTarget}
              onSendMessage={(conv) => {
                onSelectThread(conv.id);
                setProfileTarget(null);
              }}
              onBlockUser={(conv) => {
                setProfileTarget(null);
                setBlockTarget(conv);
              }}
              onUnfriend={(conv) => {
                setProfileTarget(null);
                setUnfriendTarget(conv);
              }}
              // onVoiceCall intentionally omitted — #1209 wires this.
            />
          );
        })()}
    </div>
  );
};

export default ConversationList;
