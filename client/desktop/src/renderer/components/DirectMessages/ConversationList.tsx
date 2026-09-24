import { useState, useEffect, useRef, useCallback, useId } from 'react';
import { MessageSquare, Users, BookOpen, MessagesSquare, PenLine, Search } from 'lucide-react';
import {
  useDMStore,
  type DMConversation,
  type DMParticipant,
  type DMLastMessage,
} from '../../stores/chat/dmStore';
import {
  useNotificationPrefsStore,
  isEntryCurrentlyMuted,
} from '../../stores/ui/notificationPrefsStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useAuthStore } from '../../stores/auth/authStore';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useFriendStore, type Friend } from '../../stores/chat/friendStore';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { E2EEKeyUnavailableError } from '../../services/e2ee/e2eeErrors';
import { subscribeSearchScopeInvalidations } from '../../services/messaging/searchService';
import { useDraftMessageStore } from '../../stores/chat/draftMessageStore';
import { errorMessage } from '../../utils/runtime/redactError';
import { resolveMediaUrl } from '../../utils/ui/resolveMediaUrl';
import { describeMessagePreview, truncateSenderName } from '../../utils/messaging/messagePreview';
import { resolveUserAccentColors } from '../../utils/ui/schemeColors';
import CreateGroupModal from './CreateGroupModal';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import { DIRECT_MESSAGES_CONTEXT_AREA } from '../ui/ContextMenuProvider';
import DMConversationContextMenu from './DMConversationContextMenu';
import DMThreadRemovalDialog, { type DMThreadRemovalTarget } from './DMThreadRemovalDialog';
import DMProfileModal from './DMProfileModal';
import PurgeMessagesModal from '../Purge/PurgeMessagesModal';
import Modal from '../ui/Modal';
import { useExpirationPolicy } from '../../hooks/messaging/useExpirationPolicy';
import MessageExpirationEditor from '../Expiration/MessageExpirationEditor';
import { AttributedPopover } from '../Layout/AttributedPopover';
import './DirectMessages.css';

interface ConversationListProps {
  compact?: boolean;
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

interface ConversationItemProps {
  conv: DMConversation;
  compact: boolean;
  currentUserId: string;
  friends: Friend[];
  muted: boolean;
  hasDraft: boolean;
  activeCallParticipantIds?: string[];
  isInOwnCall: boolean;
  decryptedPreview?: DecryptedPreview;
  selected: boolean;
  onSelectThread: (id: string) => void;
  onOpenContextMenu: (value: {
    conversation: DMConversation;
    position: { x: number; y: number };
  }) => void;
}

interface ConversationItemViewProps {
  conv: DMConversation;
  currentUserId: string;
  other?: DMParticipant;
  status: string;
  name: string;
  showUnread: boolean;
  hasDraft: boolean;
  boundedUnread: string | number;
  callLabel: string | null;
  activeCallParticipantIds?: string[];
  selected: boolean;
  onSelect: () => void;
  onContextMenu: (event: React.MouseEvent) => void;
}

function getConversationCallLabel(
  conv: DMConversation,
  activeCallParticipantIds: string[] | undefined,
  isInOwnCall: boolean
): string | null {
  if (conv.isGroup && activeCallParticipantIds) {
    return `${activeCallParticipantIds.length} of ${conv.participants.length} in call`;
  }
  if (isInOwnCall) return 'In voice call';
  return null;
}

/** Tri-state preview cache entry. `empty` may only be asserted when a decrypt
 *  actually succeeded and yielded blank text — never inferred from a failure. */
type PreviewStatus = 'text' | 'empty' | 'failed';
interface DecryptedPreview {
  status: PreviewStatus;
  text: string;
  cipher: string;
}

/**
 * The one place that decides whether a KNOWN plaintext counts as text or as
 * proven-empty. Both callers — the optimistic-send branch and the decrypt
 * commit — reached this conclusion separately before; a single rule keeps them
 * from drifting. It deliberately cannot return `'failed'`: emptiness is only
 * ever asserted about a plaintext the client actually holds, never inferred
 * from a decryption that did not happen or did not succeed.
 */
function statusForPlaintext(plaintext: string | null | undefined): PreviewStatus {
  return plaintext?.trim() ? 'text' : 'empty';
}

/**
 * Total by construction (§4.4): self -> 'You'; else the participant's display
 * name or username; else the message's own username; else 'Someone'. Never
 * `undefined`, never a UUID. A departed group-DM member lands on rung 3 or 4.
 * No `users` JOIN is added — the client already holds participants[] on the
 * same payload.
 */
function previewSenderName(
  conv: DMConversation,
  currentUserId: string,
  last: DMLastMessage
): string {
  if (last.userId === currentUserId) return 'You';
  const participant = conv.participants.find((p) => p.userId === last.userId);
  // Truthiness rather than `??`: a blank displayName must fall through to
  // username, or the ladder stops being total.
  const fromRoster = participant?.displayName?.trim() || participant?.username?.trim();
  if (fromRoster) return truncateSenderName(fromRoster);
  const fromMessage = last.username?.trim();
  if (fromMessage) return truncateSenderName(fromMessage);
  return 'Someone';
}

/**
 * A preview split at the trust boundary. `sender` is ATTACKER-CONTROLLED display
 * text — another user's `display_name`, which no layer sanitizes — and `body` is
 * the app's own words. They are returned separately, and rendered into separate
 * elements, because one concatenated string gives the reader nothing that marks
 * where the untrusted half ends: a display name of exactly `You sent a Photo`
 * (16 graphemes, so `truncateSenderName` returns it verbatim) composes to
 * `You sent a Photo sent a Photo`, and `.conversation-preview`'s
 * `text-overflow: ellipsis` clips the real clause away — leaving a complete,
 * well-formed attribution of someone else's attachment to the reader themselves.
 * The capability is new to this change: before it the preview carried no name.
 */
interface PreviewParts {
  readonly sender?: string;
  readonly body: string;
}

function getConversationPreview(
  conv: DMConversation,
  currentUserId: string,
  decryptedPreview: DecryptedPreview | undefined
): PreviewParts {
  const last = conv.lastMessage;
  if (!last) return { body: '' };

  // A cache entry is about ONE ciphertext. When lastMessage advances the entry
  // still describes the PREVIOUS message until its decrypt lands, so reading it
  // unconditionally renders the old text under the new message's sender and
  // type — `<previous text> · Photo`. An entry for another ciphertext is not
  // stale data to display, it is no data.
  const usable = decryptedPreview?.cipher === last.content ? decryptedPreview : undefined;

  // Writer 1: an optimistic own-send carries its plaintext and bypasses the
  // decrypt cache entirely, so it stays ahead of the status branch (R13).
  // It is checked FIRST because for an own-send `last.content` holds plaintext,
  // not ciphertext, so the gate above can never match on that path.
  const hasPlaintextPreview = last.plaintextPreview !== undefined;
  const status: PreviewStatus = hasPlaintextPreview
    ? statusForPlaintext(last.plaintextPreview)
    : (usable?.status ?? 'failed');
  const plaintext = hasPlaintextPreview ? (last.plaintextPreview as string) : (usable?.text ?? '');

  const described = describeMessagePreview({
    content: plaintext,
    gifSlug: hasPlaintextPreview ? last.gifSlug : undefined,
    attachmentType: last.attachmentType,
    attachmentMime: last.attachmentMime,
    callEventPayload: last.callEventPayload,
    currentUserId,
  });

  // Call events are server-authored plaintext and never decrypted.
  if (described.kind === 'call') return { body: described.text ?? '' };

  // A type label may assert "this is a Photo with no text" only when the client
  // has PROVEN the text is empty by decrypting it. A genuine key miss renders
  // the placeholder and never a type.
  if (status === 'failed') return { body: 'Encrypted message' };

  if (described.kind === 'text') {
    return {
      body: described.suffix
        ? `${described.text ?? ''} · ${described.suffix}`
        : (described.text ?? ''),
    };
  }
  if (described.phrase) {
    return {
      sender: previewSenderName(conv, currentUserId, last),
      body: ` sent ${described.phrase}`,
    };
  }
  return { body: '' };
}

const CompactConversationItem: React.FC<ConversationItemViewProps> = ({
  conv,
  currentUserId,
  other,
  status,
  name,
  showUnread,
  hasDraft,
  boundedUnread,
  callLabel,
  selected,
  onSelect,
  onContextMenu,
}) => {
  const ariaLabel = [
    name,
    showUnread ? `${boundedUnread} unread` : null,
    hasDraft ? 'Draft' : null,
    callLabel,
  ]
    .filter(Boolean)
    .join(', ');

  return (
    <button
      type="button"
      className={`conversation-item conversation-item--compact${selected ? ' active' : ''}${showUnread ? ' unread' : ''}`}
      data-conversation-id={conv.id}
      aria-label={ariaLabel}
      title={name}
      onClick={onSelect}
      onContextMenu={onContextMenu}
    >
      <ConversationAvatar conv={conv} currentUserId={currentUserId} other={other} status={status} />
      {hasDraft && <PenLine className="conversation-compact-draft" size={12} aria-hidden="true" />}
      {showUnread && (
        <span className="conversation-unread-badge" aria-hidden="true">
          {boundedUnread}
        </span>
      )}
    </button>
  );
};

const ConversationCallBadge: React.FC<
  Pick<ConversationItemViewProps, 'conv' | 'activeCallParticipantIds' | 'callLabel'>
> = ({ conv, activeCallParticipantIds, callLabel }) => {
  if (conv.isGroup && activeCallParticipantIds) {
    return (
      <span
        className="conversation-in-call-badge group"
        title={callLabel ?? undefined}
        aria-label={callLabel ?? undefined}
      >
        {activeCallParticipantIds.length} of {conv.participants.length} in call
      </span>
    );
  }
  if (!callLabel) return null;
  return (
    <span className="conversation-in-call-badge" title={callLabel} aria-label={callLabel}>
      🔊
    </span>
  );
};

interface StandardConversationItemProps extends ConversationItemViewProps {
  preview: PreviewParts;
  lastTime: string;
}

const StandardConversationItem: React.FC<StandardConversationItemProps> = ({
  conv,
  currentUserId,
  other,
  status,
  name,
  showUnread,
  hasDraft,
  boundedUnread,
  callLabel,
  activeCallParticipantIds,
  selected,
  preview,
  lastTime,
  onSelect,
  onContextMenu,
}) => {
  const previewId = useId();
  return (
    <button
      type="button"
      className={`conversation-item${selected ? ' active' : ''}${showUnread ? ' unread' : ''}`}
      data-conversation-id={conv.id}
      onClick={onSelect}
      onContextMenu={onContextMenu}
      onKeyDown={(event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          onSelect();
        }
      }}
      // `aria-label` is the accessible NAME, and AT re-announces a focused element
      // whenever its name changes — so the preview (which changes on every incoming
      // message) must never join it, or a focused row would re-interrupt mid-read.
      // `aria-describedby` instead points at an accessible DESCRIPTION, which is read
      // once on focus and is not monitored for changes, so the preview can update
      // freely without re-announcing. Do not "simplify" this into `aria-labelledby`
      // or fold the preview into `aria-label`.
      aria-label={name}
      aria-describedby={preview.sender || preview.body ? previewId : undefined}
    >
      <ConversationAvatar conv={conv} currentUserId={currentUserId} other={other} status={status} />

      <div className="conversation-content">
        <div className="conversation-top-row">
          <span className="conversation-name">{name}</span>
          <span className="conversation-time">{getRelativeTime(lastTime)}</span>
        </div>
        <div className="conversation-bottom-row">
          <span className="conversation-preview" id={previewId}>
            {/* The untrusted half gets its own element so the boundary survives
                into the rendered text: distinct weight and colour, plus
                `unicode-bidi: isolate` as the second belt behind
                truncateSenderName's control-character strip. */}
            {preview.sender ? (
              <span className="conversation-preview-sender">{preview.sender}</span>
            ) : null}
            {preview.body}
          </span>
          <div className="conversation-badges">
            {/* Group conversations surface joinable calls even when the local user has not joined;
                1:1 conversations keep the local in-call indicator. */}
            <ConversationCallBadge
              conv={conv}
              activeCallParticipantIds={activeCallParticipantIds}
              callLabel={callLabel}
            />
            {hasDraft && (
              <span className="conversation-draft-indicator" title="Draft message">
                <PenLine size={12} />
              </span>
            )}
            {showUnread && <span className="conversation-unread-badge">{boundedUnread}</span>}
          </div>
        </div>
      </div>
    </button>
  );
};

const ConversationItem: React.FC<ConversationItemProps> = ({
  conv,
  compact,
  currentUserId,
  friends,
  muted,
  hasDraft,
  activeCallParticipantIds,
  isInOwnCall,
  decryptedPreview,
  selected,
  onSelectThread,
  onOpenContextMenu,
}) => {
  const name = getConversationName(conv, currentUserId);
  const other = conv.isGroup
    ? undefined
    : conv.participants.find((participant) => participant.userId !== currentUserId);
  const friendStatus = other
    ? friends.find((friend) => friend.userId === other.userId)?.status
    : undefined;
  const status = friendStatus ?? other?.status ?? 'offline';
  const showUnread = conv.unreadCount > 0 && !muted;
  const boundedUnread = conv.unreadCount > 99 ? '99+' : conv.unreadCount;
  const callLabel = getConversationCallLabel(conv, activeCallParticipantIds, isInOwnCall);
  const onSelect = () => onSelectThread(conv.id);
  const onContextMenu = (event: React.MouseEvent) => {
    event.preventDefault();
    // Chromium dispatches `contextmenu` for Shift+F10 and the ContextMenu key on a focused
    // element, but with no pointer there are no meaningful client coordinates — they arrive as
    // 0, which would pin the menu to the top-left corner of the window instead of to the row
    // the user is on. Fall back to the row's own box so the keyboard path lands where the mouse
    // path would. `> 0` rather than a null check: 0 is the value actually observed, and a
    // genuine click at the viewport origin cannot land on a conversation row.
    const rect = event.currentTarget.getBoundingClientRect();
    onOpenContextMenu({
      conversation: conv,
      position: {
        x: event.clientX > 0 ? event.clientX : rect.left + 8,
        y: event.clientY > 0 ? event.clientY : rect.bottom,
      },
    });
  };
  const sharedProps: ConversationItemViewProps = {
    conv,
    currentUserId,
    other,
    status,
    name,
    showUnread,
    hasDraft,
    boundedUnread,
    callLabel,
    activeCallParticipantIds,
    selected,
    onSelect,
    onContextMenu,
  };

  if (compact) return <CompactConversationItem {...sharedProps} />;
  return (
    <StandardConversationItem
      {...sharedProps}
      preview={getConversationPreview(conv, currentUserId, decryptedPreview)}
      lastTime={conv.lastMessage?.createdAt || conv.createdAt}
    />
  );
};

const ConversationList: React.FC<ConversationListProps> = ({
  compact = false,
  selectedThreadId,
  onSelectThread,
}) => {
  const [search, setSearch] = useState('');
  const searchInputRef = useRef<HTMLInputElement>(null);
  const searchTriggerRef = useRef<HTMLButtonElement>(null);
  const [searchAnchor, setSearchAnchor] = useState<HTMLElement | null>(null);
  const searchTriggerId = useId();
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
  // Purge target — same lifted-state pattern; the purge dialog outlives the
  // context menu that opened it (#1354).
  const [purgeTarget, setPurgeTarget] = useState<DMConversation | null>(null);
  const [expirationTarget, setExpirationTarget] = useState<DMConversation | null>(null);
  const [unfriendTarget, setUnfriendTarget] = useState<DMConversation | null>(null);
  // DM Profile modal target (#1208). Same lifted-state pattern as block/unfriend
  // so the modal continues rendering after the context menu unmounts.
  const [profileTarget, setProfileTarget] = useState<DMConversation | null>(null);
  const [removalTarget, setRemovalTarget] = useState<DMThreadRemovalTarget | null>(null);
  const removalTargetRef = useRef<DMThreadRemovalTarget | null>(null);
  removalTargetRef.current = removalTarget;
  const focusAfterRemovalRef = useRef<string | null>(null);
  const blockUser = useFriendStore((s) => s.blockUser);
  const removeFriend = useFriendStore((s) => s.removeFriend);
  const friends = useFriendStore((s) => s.friends);
  const conversations = useDMStore((s) => s.conversations);
  // Muted DMs (#84) suppress the unread indicator at render time — the count
  // stays intact so un-muting reveals it instantly (epic #1029 close audit).
  const mutedDMs = useNotificationPrefsStore((s) => s.mutedDMs);
  const fetchConversations = useDMStore((s) => s.fetchConversations);
  const openPersonalThread = useDMStore((s) => s.openPersonalThread);
  const currentUserId = useUserStore((s) => s.user?.id) || '';
  const authGeneration = useAuthStore((s) => s.authGeneration);
  const currentExpirationTarget = expirationTarget
    ? (conversations.find((conversation) => conversation.id === expirationTarget.id) ?? null)
    : null;

  useEffect(() => {
    const conversationId = focusAfterRemovalRef.current;
    if (removalTarget !== null || conversationId === null) return;
    focusAfterRemovalRef.current = null;
    const survivingInvoker = Array.from(
      document.querySelectorAll<HTMLButtonElement>('.conversation-item[data-conversation-id]')
    ).find((element) => element.dataset.conversationId === conversationId);
    const focusTarget =
      (survivingInvoker?.isConnected ? survivingInvoker : null) ??
      (compact ? searchTriggerRef.current : searchInputRef.current);
    focusTarget?.focus();
  }, [compact, removalTarget]);

  useEffect(() => {
    focusAfterRemovalRef.current = null;
    if (!removalTargetRef.current) return;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- an account-scoped destructive target must not survive its auth owner
    setRemovalTarget(null);
  }, [authGeneration]);
  const expiration = useExpirationPolicy(
    currentExpirationTarget &&
      !currentExpirationTarget.isGroup &&
      !currentExpirationTarget.isPersonal &&
      currentExpirationTarget.participants.some(
        (participant) => participant.userId === currentUserId
      )
      ? { kind: 'dm', id: currentExpirationTarget.id }
      : null
  );

  useEffect(() => {
    if (
      expirationTarget &&
      (!currentExpirationTarget ||
        currentExpirationTarget.isGroup ||
        currentExpirationTarget.isPersonal ||
        !currentExpirationTarget.participants.some(
          (participant) => participant.userId === currentUserId
        ))
    )
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- close an expired context-menu target after account or list reconciliation
      setExpirationTarget(null);
  }, [currentExpirationTarget, currentUserId, expirationTarget]);

  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- a new active conversation or auth owner invalidates a lifted timer modal
    setExpirationTarget(null);
  }, [authGeneration, selectedThreadId]);

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

  // Tri-state cache of decrypted last message previews (conversationId → entry).
  const [decryptedPreviews, setDecryptedPreviews] = useState<Record<string, DecryptedPreview>>({});
  const decryptingRef = useRef<Set<string>>(new Set());

  // No mount fetch here: ServerBar owns it (#2363). ServerBar renders in this
  // view AND in the persistent chrome, so its mount coverage strictly contains
  // this component's — and its effect re-runs on every accessToken rotation,
  // where this one fired once, unauthenticated, if the token had not arrived.
  // Keeping both cost TWO sequential full-list requests on every DM-view entry:
  // the second caller does not no-op, it sets conversationRefetchQueued and the
  // first request's `finally` immediately issues it (CODEX P2).

  const forgetPreview = useCallback((scope: string) => {
    // The in-flight set is keyed `${convId}:${ciphertext}` (see decryptPreview),
    // so dropping one conversation means dropping every key under its prefix.
    // Deleting from a Set while iterating it is well-defined.
    for (const key of decryptingRef.current) {
      if (key === scope || key.startsWith(`${scope}:`)) decryptingRef.current.delete(key);
    }
    setDecryptedPreviews((current) => {
      if (!(scope in current)) return current;
      const next = { ...current };
      delete next[scope];
      return next;
    });
  }, []);

  useEffect(
    () =>
      subscribeSearchScopeInvalidations((scope) => {
        if (scope === null) {
          decryptingRef.current.clear();
          setDecryptedPreviews({});
          return;
        }
        forgetPreview(scope);
      }),
    [forgetPreview]
  );

  // A purge deletes the conversation's last message, but dropping the decrypted
  // cache alone does not close it: `lastMessage` still holds the purged
  // ciphertext, so the decrypt effect below re-decrypts on the very next pass
  // (its `conversationStillOwnsCiphertext` guard passes — nothing removed the
  // ciphertext) and the purged text renders again. Clearing that stored
  // `lastMessage` is what closes the loop, and it belongs to the shared
  // `messages-purged` lane in `useWebSocketMessages` — that lane runs whether or
  // not this list is mounted, so an unmounted list cannot leave a purged preview
  // to reappear. What stays here is the view-local decrypted cache plus the
  // refetch that re-establishes the authoritative post-purge preview, which
  // every DM read already filters through the server's purge.HiddenRangeFilter.
  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent).detail as { scopeId?: string | null } | undefined;
      const scopeId = detail?.scopeId;
      // A null scope is the server-wide purge signal — it can never name a DM.
      if (typeof scopeId !== 'string') return;
      const known = useDMStore.getState().conversations;
      if (!known.some((conversation) => conversation.id === scopeId)) return;
      forgetPreview(scopeId);
      void fetchConversations();
    };
    globalThis.addEventListener('messages-purged', handler);
    return () => globalThis.removeEventListener('messages-purged', handler);
  }, [fetchConversations, forgetPreview]);

  // Decrypt last message previews for encrypted conversations
  const decryptPreview = useCallback(async (convId: string, ciphertext: string) => {
    // Keyed by conversation AND ciphertext. Keyed by conversation alone, a new
    // message arriving while the previous decrypt was in flight was DROPPED:
    // the effect called through, returned here immediately, and the in-flight
    // decrypt then failed its ownership check and committed nothing — so
    // nothing re-armed the new ciphertext until some unrelated store update.
    const inFlightKey = `${convId}:${ciphertext}`;
    if (decryptingRef.current.has(inFlightKey)) return;
    decryptingRef.current.add(inFlightKey);
    try {
      if (!e2eeService.isInitialized) return;
      const operationGuard = e2eeService.createChannelOperationGuard(convId);

      // NARROW inner try: it captures the DECRYPT's own outcome and nothing
      // else. assertCurrent() throws deliberately when this operation has been
      // superseded, and that throw must NOT be readable as a decryption
      // failure — recording 'failed' for a superseded operation would re-arm
      // the preview-resurrection class the guard exists to prevent (R9/C3).
      let plaintext: string | null = null;
      let decryptFailed = false;
      let keyStillPending = false;
      try {
        plaintext = await e2eeService.decryptForChannel(convId, ciphertext);
      } catch (err) {
        decryptFailed = true;
        // A key that has not ARRIVED yet is a transient state, not a verdict.
        // The error object is inspected, never logged — see C4 below.
        keyStillPending = err instanceof E2EEKeyUnavailableError && err.pending;
      }

      // Committing 'failed' for a pending key would satisfy the effect's
      // re-decrypt predicate (the cached cipher now matches), latching the
      // placeholder for the rest of the connection. `main` retried precisely
      // because it cached nothing, so this restores that behaviour for the one
      // case that is genuinely retryable. Anything unrecognised falls through
      // to 'failed', which is the fail-closed direction.
      if (keyStillPending) return;

      // Both fences run OUTSIDE that try. Either one throwing leaves the cache
      // untouched: a superseded operation commits nothing, not even 'failed'.
      operationGuard.assertCurrent();
      const conversationStillOwnsCiphertext = useDMStore
        .getState()
        .conversations.some(
          (conversation) =>
            conversation.id === convId && conversation.lastMessage?.content === ciphertext
        );
      if (!conversationStillOwnsCiphertext) return;

      // 'empty' is asserted only from a decrypt that actually SUCCEEDED and
      // yielded blank text. Emptiness is never inferred from a failure.
      const status: PreviewStatus = decryptFailed ? 'failed' : statusForPlaintext(plaintext);
      setDecryptedPreviews((prev) => ({
        ...prev,
        [convId]: { status, text: plaintext ?? '', cipher: ciphertext },
      }));
    } catch {
      // Superseded operation or guard construction failure — commit nothing.
      // No log line: correlating a conversation id with a decrypt outcome is
      // exactly the signal C4 forbids.
    } finally {
      decryptingRef.current.delete(inFlightKey);
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

  // A key that had not ARRIVED yet leaves no cache entry at all — decryptPreview
  // deliberately commits nothing for a retryable miss, so the placeholder does
  // not latch. But the effect above is driven by `conversations` and
  // `decryptedPreviews`, and the arriving key changes neither, so nothing
  // re-armed the attempt until some unrelated store update happened to fire.
  // The same event `useMessageFetch` already listens for is the signal.
  useEffect(() => {
    const handler = (event: Event) => {
      const deliveredId = (event as CustomEvent<{ channelId?: string }>).detail?.channelId;
      if (!deliveredId) return;
      const conv = useDMStore.getState().conversations.find((c) => c.id === deliveredId);
      const last = conv?.lastMessage;
      // An own-send carries its plaintext and never wanted a decrypt. The
      // in-flight set is keyed by conversation AND ciphertext, so a delivery
      // landing mid-decrypt cannot start a second attempt at the same one.
      if (!last?.content || last.plaintextPreview !== undefined) return;
      decryptPreview(deliveredId, last.content);
    };
    globalThis.addEventListener('e2ee-key-delivered', handler);
    return () => globalThis.removeEventListener('e2ee-key-delivered', handler);
  }, [decryptPreview]);

  // Separate personal thread from regular conversations
  const personalThread = conversations.find((c) => c.isPersonal);

  const filtered = conversations
    .filter((c) => !c.isPersonal)
    .filter((c) =>
      getConversationName(c, currentUserId).toLowerCase().includes(search.toLowerCase())
    );
  const activeSearchAnchor = compact && searchAnchor?.isConnected ? searchAnchor : null;

  const handleOpenPersonalThread = async () => {
    try {
      const conv = await openPersonalThread();
      onSelectThread(conv.id);
    } catch (err) {
      console.error('Failed to open personal thread:', errorMessage(err));
    }
  };

  const contextMenuElement = contextMenu ? (
    <DMConversationContextMenu
      conversation={contextMenu.conversation}
      currentUserId={currentUserId}
      position={contextMenu.position}
      onClose={() => setContextMenu(null)}
      onBlockUser={(conv) => setBlockTarget(conv)}
      onUnfriend={(conv) => setUnfriendTarget(conv)}
      onViewProfile={(conv) => setProfileTarget(conv)}
      onHideThread={(conv) => {
        focusAfterRemovalRef.current = conv.id;
        setRemovalTarget({ conversation: conv, action: 'hide' });
      }}
      onClearHistory={(conv) => {
        focusAfterRemovalRef.current = conv.id;
        setRemovalTarget({ conversation: conv, action: 'clear' });
      }}
      onLeaveGroup={(conv) => {
        focusAfterRemovalRef.current = conv.id;
        setRemovalTarget({ conversation: conv, action: 'leave' });
      }}
      onPurgeMessages={(conv) => setPurgeTarget(conv)}
      onMessageExpiration={(conv) => setExpirationTarget(conv)}
    />
  ) : null;

  return (
    <div
      className={`conversation-list${compact ? ' conversation-list--compact' : ''}`}
      data-context-area={DIRECT_MESSAGES_CONTEXT_AREA}
    >
      {compact && (
        <>
          <button
            ref={searchTriggerRef}
            id={`${searchTriggerId}-thread-search`}
            type="button"
            className="conversation-item conversation-item--compact conversation-search-trigger"
            aria-label="Search conversations"
            title="Search conversations"
            aria-expanded={activeSearchAnchor !== null}
            aria-controls={`${searchTriggerId}-thread-search-popover`}
            onClick={(event) =>
              setSearchAnchor((current) =>
                current === event.currentTarget ? null : event.currentTarget
              )
            }
          >
            <Search size={20} />
          </button>
          <AttributedPopover
            id={`${searchTriggerId}-thread-search-popover`}
            anchor={activeSearchAnchor}
            label="Search conversations"
            open={activeSearchAnchor !== null}
            placement="right"
            onClose={() => setSearchAnchor(null)}
          >
            <div className="conversation-search conversation-search--popover">
              <input
                ref={searchInputRef}
                type="text"
                placeholder="Search conversations..."
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                autoFocus
              />
            </div>
          </AttributedPopover>
          <button
            id={`${searchTriggerId}-create-group`}
            type="button"
            className="conversation-item conversation-item--compact"
            onClick={() => setIsCreateGroupOpen(true)}
            aria-label="Create Group DM"
            title="Create Group DM"
          >
            <MessagesSquare size={20} aria-hidden="true" />
          </button>
        </>
      )}
      {!compact && (
        <div className="conversation-search">
          <input
            ref={searchInputRef}
            type="text"
            placeholder="Search conversations..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          <button
            id={`${searchTriggerId}-create-group`}
            type="button"
            className="create-group-btn"
            onClick={() => setIsCreateGroupOpen(true)}
            aria-label="Create Group DM"
            title="Create Group DM"
          >
            <MessagesSquare size={16} />
          </button>
        </div>
      )}

      {/* Pinned Personal Thread */}
      <button
        type="button"
        className={`conversation-item personal-thread${compact ? ' conversation-item--compact' : ''}${
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
        title={compact ? 'Personal Thread' : undefined}
      >
        <div className="conversation-avatar personal-thread-avatar">
          <BookOpen size={14} />
        </div>
        {!compact && (
          <span className="conversation-name personal-thread-name">Personal Thread</span>
        )}
      </button>
      <div className="personal-thread-divider" />

      {filtered.length === 0
        ? !compact && (
            <div className="conversation-list-empty">
              <MessageSquare size={32} />
              <p>No conversations yet</p>
            </div>
          )
        : filtered.map((conv) => (
            <ConversationItem
              key={conv.id}
              conv={conv}
              compact={compact}
              currentUserId={currentUserId}
              friends={friends}
              muted={isEntryCurrentlyMuted(mutedDMs.get(conv.id))}
              hasDraft={Boolean(drafts[conv.id])}
              activeCallParticipantIds={activeDMCalls[conv.id]?.participantIds}
              isInOwnCall={inCallForThisConv === conv.id}
              decryptedPreview={decryptedPreviews[conv.id]}
              selected={selectedThreadId === conv.id}
              onSelectThread={onSelectThread}
              onOpenContextMenu={setContextMenu}
            />
          ))}
      <CreateGroupModal isOpen={isCreateGroupOpen} onClose={() => setIsCreateGroupOpen(false)} />
      {contextMenuElement}

      {removalTarget && (
        <DMThreadRemovalDialog
          target={removalTarget}
          onClose={() => setRemovalTarget(null)}
          onRemoved={() => {
            setRemovalTarget(null);
          }}
        />
      )}

      {currentExpirationTarget && (
        <Modal isOpen={true} onClose={() => setExpirationTarget(null)} title="Message expiration">
          <MessageExpirationEditor
            scope={{ kind: 'dm', id: currentExpirationTarget.id }}
            policy={expiration.policy}
            policyState={expiration.policyState}
            canEdit={expiration.canEdit}
            lockedDescription={expiration.lockedDescription}
            onRefresh={expiration.onRefresh}
            onApplyPolicy={expiration.onApplyPolicy}
            onClose={() => setExpirationTarget(null)}
          />
        </Modal>
      )}

      {/* Block-user confirmation (#984). The peer is computed from the
          conversation's participants by filtering out the current user. The
          confirmation copy uses the peer's display name (or username
          fallback) to make the destructive action unambiguous. */}
      {/* Purge Messages — the modal's consequence copy depends on the group
          role because the backend behaviour does: an admin's purge removes
          messages for everyone, a member's removes only their own and
          receiver-hides the rest. Unknown role falls back to 'member', the
          shape that promises less. */}
      {purgeTarget && (
        <PurgeMessagesModal
          context={purgeTarget.isGroup ? 'group' : 'dm'}
          isOpen={purgeTarget !== null}
          scopeId={purgeTarget.id}
          scopeName={getConversationName(purgeTarget, currentUserId)}
          role={
            purgeTarget.participants.find((p) => p.userId === currentUserId)?.role === 'admin'
              ? 'admin'
              : 'member'
          }
          onClose={() => setPurgeTarget(null)}
        />
      )}

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
