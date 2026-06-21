import React, { useState, useEffect, useCallback, useMemo } from 'react';
import { Lock } from 'lucide-react';
import { MessageWithStatus, type ChatContextType } from '../../types/chat';
import { useMemberStore } from '../../stores/memberStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { useFriendOrgStore } from '../../stores/friendOrgStore';
import { resolveUserAccentColors } from '../../utils/schemeColors';
import MessageAvatar from './MessageAvatar';
import MessageActions from './MessageActions';
import ReactionBar from './ReactionBar';
import ReplyPreviewBar from './ReplyPreviewBar';
import MessageContextMenu from './MessageContextMenu';
import LazyEmojiPicker from '../EmojiPicker/LazyEmojiPicker';
import { toggleReaction } from '../../services/reactionService';
import DeleteMessageModal from './DeleteMessageModal';
import {
  getEmojiOnlyCount,
  getEmojiSizeClass,
  renderEmoji,
  type MentionLookup,
} from './messageUtils';
import MarkdownContent from '../Markdown/MarkdownContent';
import AttachmentDisplay from './AttachmentDisplay';
import GifEmbed from './GifEmbed';
import { InviteEmbed } from './InviteEmbed';
import { messageInviteCodes } from '@/renderer/utils/inviteUrl';
import { useMessageProfileCard } from '../../hooks/useMessageProfileCard';
import MessageProfileCardHost from './MessageProfileCardHost';
import { useSettingsStore } from '../../stores/settingsStore';
import { usePrivacyStore } from '../../stores/privacyStore';
import './Message.css';

// Stable empty references to avoid infinite re-renders when DM context skips server data
const EMPTY_MEMBERS: never[] = [];
const EMPTY_ROLES: Record<string, never[]> = {};

export interface MessageProps {
  message: MessageWithStatus;
  currentUserId: string;
  chatContext?: ChatContextType;
  onEdit?: (messageId: string, newContent: string) => void;
  onDelete?: (messageId: string) => void;
  onReply?: (message: MessageWithStatus) => void;
  onScrollToMessage?: (messageId: string) => void;
  onPinToggle?: (message: MessageWithStatus) => void;
  canPin?: boolean;
  showAvatar?: boolean;
  shiftHeld?: boolean;
}

// Messages with pending/sent status still have temporary client-generated IDs.
// Only allow edit/delete once the message has a server-assigned UUID.
const isServerMessage = (msg: MessageWithStatus) => !msg.status || msg.status === 'delivered';

function formatTimestamp(timestamp: string): string {
  const date = new Date(timestamp);
  const now = new Date();
  const isToday = date.toDateString() === now.toDateString();

  if (isToday) {
    return date.toLocaleTimeString('en-US', {
      hour: 'numeric',
      minute: '2-digit',
      hour12: true,
    });
  }
  return date.toLocaleDateString('en-US', {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
    hour12: true,
  });
}

/** Inline edit box with save/cancel actions. */
function MessageEditBox({
  content,
  originalContent,
  onChange,
  onKeyDown,
  onSubmit,
  onCancel,
}: Readonly<{
  content: string;
  originalContent: string;
  onChange: (value: string) => void;
  onKeyDown: (e: React.KeyboardEvent) => void;
  onSubmit: () => void;
  onCancel: () => void;
}>) {
  return (
    <div className="message-edit-box">
      <textarea
        className="message-edit-input"
        value={content}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={onKeyDown}
        autoFocus
        rows={3}
      />
      <div className="message-edit-actions">
        <button className="btn-edit-cancel" onClick={onCancel}>
          Cancel
        </button>
        <button
          className="btn-edit-save"
          onClick={onSubmit}
          disabled={!content.trim() || content === originalContent}
        >
          Save
        </button>
      </div>
    </div>
  );
}

/** Inline reply context shown above the message content. */
function InlineReplyPreview({
  message,
  onScrollToMessage,
}: Readonly<{
  message: MessageWithStatus;
  onScrollToMessage?: (id: string) => void;
}>) {
  const handleClick =
    message.replied_to && onScrollToMessage
      ? () => onScrollToMessage(message.replied_to?.id ?? '')
      : undefined;

  return (
    <ReplyPreviewBar
      repliedTo={message.replied_to ?? null}
      isDeleted={!message.replied_to}
      variant="inline"
      onClick={handleClick}
    />
  );
}

/** Message header: avatar username, timestamp, edited badge, role display. */
function MessageHeader({
  message,
  senderRoleEmoji,
  senderRoleColor,
  onOpenProfile,
}: Readonly<{
  message: MessageWithStatus;
  senderRoleEmoji: string | null;
  senderRoleColor: string | null;
  onOpenProfile: (position: { x: number; y: number }) => void;
}>) {
  return (
    <div className="message-header">
      {senderRoleEmoji && <span className="message-role-emoji">{senderRoleEmoji}</span>}
      <button
        type="button"
        className="message-username"
        style={senderRoleColor ? { color: senderRoleColor } : undefined}
        onClick={(e) => onOpenProfile({ x: e.clientX, y: e.clientY })}
      >
        {message.display_name || message.username}
      </button>
      <span className="message-timestamp">{formatTimestamp(message.created_at)}</span>
      {message.edited_at && <span className="message-edited">(edited)</span>}
      {message.pinned_at && (
        <span className="message-pinned-indicator" title="Pinned message">
          <svg width="12" height="12" viewBox="0 0 16 16" fill="none">
            <path
              d="M9 2L5 8h3v6l4-6H9V2z"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </span>
      )}
    </div>
  );
}

/** Submits an edit if the content has meaningfully changed. Returns true if submitted. */
function trySubmitEdit(
  editContent: string,
  originalContent: string,
  messageId: string,
  onEdit?: (id: string, content: string) => void
): boolean {
  const trimmed = editContent.trim();
  if (!trimmed || trimmed === originalContent || !onEdit) return false;
  onEdit(messageId, trimmed);
  return true;
}

/** Handles keyboard shortcuts in the message edit textarea. */
/** Renders the message actions bar, context menu, delete modal, and reaction picker. */
function MessageOverlays({
  message,
  canModify,
  isEditing,
  isOwnMessage,
  shiftHeld,
  showDeleteModal,
  setShowDeleteModal,
  setIsEditing,
  contextMenu,
  setContextMenu,
  reactionPicker,
  setReactionPicker,
  onEdit,
  onDelete,
  onReply,
  onPinToggle,
  canPin,
}: Readonly<{
  message: MessageWithStatus;
  canModify: boolean;
  isEditing: boolean;
  isOwnMessage: boolean;
  shiftHeld: boolean;
  showDeleteModal: boolean;
  setShowDeleteModal: (v: boolean) => void;
  setIsEditing: (v: boolean) => void;
  contextMenu: { x: number; y: number } | null;
  setContextMenu: (v: { x: number; y: number } | null) => void;
  reactionPicker: { x: number; y: number } | null;
  setReactionPicker: (v: { x: number; y: number } | null) => void;
  onEdit?: (messageId: string, newContent: string) => void;
  onDelete?: (messageId: string) => void;
  onReply?: (message: MessageWithStatus) => void;
  onPinToggle?: (message: MessageWithStatus) => void;
  canPin?: boolean;
}>) {
  // Pre-compute conditional callbacks to keep JSX complexity flat
  const editHandler = canModify && onEdit ? () => setIsEditing(true) : undefined;
  const deleteHandler = canModify ? onDelete : undefined;
  const requestDeleteHandler = canModify ? () => setShowDeleteModal(true) : undefined;
  const replyHandler = onReply ? () => onReply(message) : undefined;
  const pinHandler = onPinToggle ? () => onPinToggle(message) : undefined;
  const hasReactions = Boolean(message.reactions && message.reactions.length > 0);

  return (
    <>
      <MessageActions
        messageId={message.id}
        canModify={canModify}
        isEditing={isEditing}
        shiftHeld={shiftHeld}
        onEdit={editHandler}
        onDelete={deleteHandler}
        onRequestDelete={requestDeleteHandler}
        onReply={replyHandler}
        onPin={pinHandler}
        canPin={canPin}
        isPinned={!!message.pinned_at}
        onReaction={(pos) => setReactionPicker(pos)}
      />

      {hasReactions && <ReactionBar messageId={message.id} reactions={message.reactions ?? []} />}

      {onDelete && (
        <DeleteMessageModal
          isOpen={showDeleteModal}
          onClose={() => setShowDeleteModal(false)}
          onConfirm={() => {
            onDelete(message.id);
            setShowDeleteModal(false);
          }}
        />
      )}

      {contextMenu && (
        <MessageContextMenu
          message={message}
          position={contextMenu}
          isOwnMessage={isOwnMessage}
          canModify={canModify}
          onClose={() => setContextMenu(null)}
          onEdit={() => setIsEditing(true)}
          onDelete={() => {
            if (onDelete) {
              setShowDeleteModal(true);
            }
          }}
          onReaction={() => setReactionPicker(contextMenu)}
          onReply={replyHandler}
          onPin={pinHandler}
          isPinned={!!message.pinned_at}
          canPin={canPin}
        />
      )}

      {reactionPicker && (
        <LazyEmojiPicker
          onSelect={(emoji: string) => {
            setReactionPicker(null);
            toggleReaction(message.id, emoji).catch(() => {});
          }}
          onClose={() => setReactionPicker(null)}
          mode="popover"
          position={reactionPicker}
        />
      )}
    </>
  );
}

function handleEditKeyDown(e: React.KeyboardEvent, onSubmit: () => void, onCancel: () => void) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    onSubmit();
  } else if (e.key === 'Escape') {
    onCancel();
  }
}

function getTopDisplayRole(
  senderMember:
    | {
        roles?: Array<{
          display_separately?: boolean;
          position: number;
          role_emoji?: string;
          role_color?: string;
        }>;
      }
    | undefined
) {
  if (!senderMember?.roles?.length) return null;
  return (
    [...senderMember.roles]
      .filter((r) => r.display_separately)
      .sort((a, b) => b.position - a.position)[0] ?? null
  );
}

function buildMentionLookup(
  members: Array<{ user_id: string; display_name?: string; username: string }>,
  allServerRoles: Record<string, Array<{ id: string; name: string }>>
): MentionLookup {
  const users = new Map<string, string>();
  for (const m of members) {
    users.set(m.user_id, m.display_name || m.username);
  }
  const roles = new Map<string, string>();
  for (const serverRoles of Object.values(allServerRoles)) {
    for (const r of serverRoles) {
      roles.set(r.id, r.name);
    }
  }
  return { users, roles };
}

/** Renders the message text content with emoji sizing, encryption indicators, and mention highlighting. */
function MessageTextContent({
  message,
  showAvatar,
  mentionLookup,
}: Readonly<{
  message: MessageWithStatus;
  showAvatar: boolean;
  mentionLookup: MentionLookup;
}>) {
  const emojiCount =
    !message.pendingKeys && !message.decryptFailed ? getEmojiOnlyCount(message.content) : 0;
  const emojiClass = getEmojiSizeClass(emojiCount);

  let contentNode: React.ReactNode;
  if (message.pendingKeys) {
    contentNode = (
      <span className="decrypt-failed pending-keys">
        <Lock size={14} />
        Waiting for encryption keys...
      </span>
    );
  } else if (message.decryptFailed) {
    contentNode = (
      <span className="decrypt-failed">
        <Lock size={14} />
        Unable to decrypt this message
      </span>
    );
  } else if (emojiCount > 0) {
    // Emoji-only path — keeps jumbo sizing via the pre-existing renderEmoji helper
    contentNode = renderEmoji(message.content);
  } else {
    // Normal markdown rendering with real message fields for memo keying
    contentNode = (
      <MarkdownContent
        id={message.id}
        content={message.content}
        editedAt={message.edited_at ?? null}
        mentionLookup={mentionLookup}
      />
    );
  }

  return (
    <div className={'message-text' + (emojiClass ? ' ' + emojiClass : '')}>
      {contentNode}
      {!showAvatar && message.edited_at && <span className="message-edited-inline">(edited)</span>}
    </div>
  );
}

const Message: React.FC<MessageProps> = ({
  message,
  currentUserId,
  chatContext = 'channel',
  onEdit,
  onDelete,
  onReply,
  onScrollToMessage,
  onPinToggle,
  canPin,
  showAvatar = true,
  shiftHeld = false,
}) => {
  const isDM = chatContext === 'dm';
  const reduceAnimations = useSettingsStore((s) => s.appearance.reduceAnimations);
  const loadGifsAutomatically = usePrivacyStore((s) => s.settings.loadGifsAutomatically);
  const [isEditing, setIsEditing] = useState(false);
  const [editContent, setEditContent] = useState(message.content);
  const [showDeleteModal, setShowDeleteModal] = useState(false);
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number } | null>(null);
  const [reactionPicker, setReactionPicker] = useState<{ x: number; y: number } | null>(null);

  // Sync editContent when message content changes externally (not while editing)
  useEffect(() => {
    if (!isEditing) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editContent from message.content when message is updated externally (not while user is editing); not a render loop
      setEditContent(message.content);
    }
  }, [message.content, isEditing]);

  const isOwnMessage = message.user_id === currentUserId;
  const canModify = isOwnMessage && isServerMessage(message);

  // Gate inside selectors so DM messages return stable empty refs and don't
  // re-render on unrelated server member/role store updates.
  const members = useMemberStore((state) => (isDM ? EMPTY_MEMBERS : state.members));
  const allServerRoles = usePermissionStore((state) => (isDM ? EMPTY_ROLES : state.serverRoles));

  const mentionLookup = useMemo(
    () => buildMentionLookup(members, allServerRoles),
    [members, allServerRoles]
  );

  const senderMember = isDM ? undefined : members.find((m) => m.user_id === message.user_id);
  const senderColors = resolveUserAccentColors(senderMember?.color_scheme);

  const topDisplayRole = getTopDisplayRole(senderMember);
  const senderRoleEmoji = topDisplayRole?.role_emoji ?? null;
  const senderRoleColor = topDisplayRole?.role_color ?? null;

  // DM author color tint (#324): in a DM, resolve the author's friend-category
  // color from friendOrgStore by the author's userId. Display-only — never
  // touches ciphertext or keys. Server roles are already suppressed in DM via
  // the #543 chatContext seam, so this tint only applies when isDM and does not
  // disturb the server-role boundary for channel/voice contexts.
  const friendCatColor = useFriendOrgStore((s) =>
    isDM ? (s.categories.find((c) => c.memberIds.includes(message.user_id))?.color ?? null) : null
  );
  const usernameColor = senderRoleColor ?? friendCatColor;

  const handleContextMenu = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setContextMenu({ x: e.clientX, y: e.clientY });
  }, []);

  const handleEditSubmit = useCallback(() => {
    if (trySubmitEdit(editContent, message.content, message.id, onEdit)) {
      setIsEditing(false);
    }
  }, [editContent, message.content, message.id, onEdit]);

  const handleEditCancel = useCallback(() => {
    setEditContent(message.content);
    setIsEditing(false);
  }, [message.content]);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => handleEditKeyDown(e, handleEditSubmit, handleEditCancel),
    [handleEditSubmit, handleEditCancel]
  );

  // Shared profile-card state for this message — the avatar and the username
  // both open the same card (which carries the Send Friend Request action, #226).
  // The hook is pure state (no JSX); MessageProfileCardHost renders the card +
  // modal subtree below. Split lets the hook stay in hooks/ without importing
  // from components/ (Sonar typescript:S6804).
  const profileCardState = useMessageProfileCard({
    message,
    currentUserId,
    chatContext,
  });
  const { openProfileAt } = profileCardState;

  const showReplyPreview = Boolean(message.replied_to || message.reply_to_id);

  const inviteCodes = useMemo(
    () =>
      messageInviteCodes(message.content, {
        pendingKeys: message.pendingKeys,
        decryptFailed: message.decryptFailed,
      }),
    [message.content, message.pendingKeys, message.decryptFailed]
  );

  const messageClassName = [
    'message',
    isOwnMessage && 'own-message',
    !showAvatar && 'message-grouped',
    message.pinned_at && 'pinned',
  ]
    .filter(Boolean)
    .join(' ');

  return (
    <article className={messageClassName} onContextMenu={handleContextMenu}>
      <MessageProfileCardHost state={profileCardState} />
      <MessageAvatar
        message={message}
        onOpenProfile={openProfileAt}
        showAvatar={showAvatar}
        senderColors={senderColors}
      />
      <div className="message-content-wrapper">
        {showReplyPreview && (
          <InlineReplyPreview message={message} onScrollToMessage={onScrollToMessage} />
        )}
        {showAvatar && (
          <MessageHeader
            message={message}
            senderRoleEmoji={senderRoleEmoji}
            senderRoleColor={usernameColor}
            onOpenProfile={openProfileAt}
          />
        )}

        {isEditing ? (
          <MessageEditBox
            content={editContent}
            originalContent={message.content}
            onChange={setEditContent}
            onKeyDown={handleKeyDown}
            onSubmit={handleEditSubmit}
            onCancel={handleEditCancel}
          />
        ) : (
          <MessageTextContent
            message={message}
            showAvatar={showAvatar}
            mentionLookup={mentionLookup}
          />
        )}

        {message.gif_slug && (
          <GifEmbed
            slug={message.gif_slug}
            reduceMotion={reduceAnimations}
            loadAutomatically={loadGifsAutomatically}
          />
        )}

        {inviteCodes.map((code) => (
          <InviteEmbed key={code} code={code} />
        ))}

        {message.attachments && message.attachments.length > 0 && (
          <AttachmentDisplay
            attachments={message.attachments}
            channelId={message.channel_id}
            messageBody={message.pendingKeys || message.decryptFailed ? '' : message.content}
          />
        )}

        <MessageOverlays
          message={message}
          canModify={canModify}
          isEditing={isEditing}
          isOwnMessage={isOwnMessage}
          shiftHeld={shiftHeld}
          showDeleteModal={showDeleteModal}
          setShowDeleteModal={setShowDeleteModal}
          setIsEditing={setIsEditing}
          contextMenu={contextMenu}
          setContextMenu={setContextMenu}
          reactionPicker={reactionPicker}
          setReactionPicker={setReactionPicker}
          onEdit={onEdit}
          onDelete={onDelete}
          onReply={onReply}
          onPinToggle={onPinToggle}
          canPin={canPin}
        />
      </div>
    </article>
  );
};

export default React.memo(Message);
