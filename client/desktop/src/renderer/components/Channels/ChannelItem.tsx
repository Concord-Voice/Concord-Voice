import React, { useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import {
  Hash,
  Volume2,
  Pin,
  Lock,
  Mic,
  MicOff,
  HeadphoneOff,
  MessageSquare,
  PenLine,
} from 'lucide-react';
import type { Channel } from '../../types/chat';

export interface VoiceMemberInfo {
  userId: string;
  displayName?: string;
  username: string;
  isMuted: boolean;
  isSpeaking?: boolean;
  serverMuted: boolean;
  serverDeafened: boolean;
  isDeafened: boolean;
}

interface ChannelItemProps {
  channel: Channel;
  isActive: boolean;
  unread: number;
  compact?: boolean;
  isMuted?: boolean;
  isGrouped: boolean;
  isLastInGroup: boolean;
  voiceMembers: VoiceMemberInfo[];
  linkedText: Channel | null;
  showLinkedText: boolean;
  isLinkedTextActive: boolean;
  linkedTextUnread: number;
  hasDraft?: boolean;
  canReorder: boolean;
  isDragging: boolean;
  showGhostBefore: boolean;
  showGhostAfter: boolean;
  onChannelClick: (channel: Channel) => void;
  onContextMenu: (e: React.MouseEvent, channel: Channel) => void;
  onDragStart: (e: React.DragEvent, id: string, type: 'channel' | 'category') => void;
  onDragOver: (e: React.DragEvent, id: string, type: 'channel' | 'category') => void;
  onDrop: (e: React.DragEvent) => void;
  onDragEnd: () => void;
  onLinkedTextClick: (voiceChannel: Channel, linkedText: Channel) => void;
  itemRef: (id: string, el: HTMLElement | null) => void;
  /**
   * Sidebar voice-participant interactivity (#487 Scope A/B). All optional so
   * existing call sites + tests that omit them keep the inert-name behavior:
   *  - click a participant name → open profile card,
   *  - right-click → open VoiceParticipantContextMenu,
   *  - native HTML5 drag of a participant name → move-to-another-channel.
   */
  onParticipantClick?: (e: React.MouseEvent, channelId: string, p: VoiceMemberInfo) => void;
  onParticipantContextMenu?: (e: React.MouseEvent, channelId: string, p: VoiceMemberInfo) => void;
  onParticipantDragStart?: (e: React.DragEvent, channelId: string, p: VoiceMemberInfo) => void;
  onParticipantDragEnd?: () => void;
  /** True while a voice participant is being dragged AND this row is a valid drop target. */
  isParticipantDropTarget?: boolean;
  /** The userId currently being dragged (dims its source name). */
  draggingParticipantUserId?: string | null;
  onParticipantDragOver?: (e: React.DragEvent, channel: Channel) => void;
  onParticipantDrop?: (e: React.DragEvent, channel: Channel) => void;
}

interface VoiceSubItemsProps {
  channel: Channel;
  isGrouped: boolean;
  isActive: boolean;
  showLinkedText: boolean;
  linkedText: Channel | null;
  isLinkedTextActive: boolean;
  linkedTextUnread: number;
  voiceMembers: VoiceMemberInfo[];
  onLinkedTextClick: (voiceChannel: Channel, linkedText: Channel) => void;
  onParticipantClick?: (e: React.MouseEvent, channelId: string, p: VoiceMemberInfo) => void;
  onParticipantContextMenu?: (e: React.MouseEvent, channelId: string, p: VoiceMemberInfo) => void;
  onParticipantDragStart?: (e: React.DragEvent, channelId: string, p: VoiceMemberInfo) => void;
  onParticipantDragEnd?: () => void;
  draggingParticipantUserId?: string | null;
}

function VoiceStatusIcon({ p }: Readonly<{ p: VoiceMemberInfo }>) {
  if (p.serverDeafened) {
    return (
      <span className="voice-channel-participant__icon voice-channel-participant__icon--server-enforced">
        <HeadphoneOff size={12} />
        <Lock size={6} className="voice-channel-participant__lock" />
      </span>
    );
  }
  if (p.serverMuted) {
    return (
      <span className="voice-channel-participant__icon voice-channel-participant__icon--server-enforced">
        <MicOff size={12} />
        <Lock size={6} className="voice-channel-participant__lock" />
      </span>
    );
  }
  if (p.isDeafened) {
    return <HeadphoneOff size={12} className="voice-channel-participant__icon--muted" />;
  }
  if (p.isMuted) {
    return <MicOff size={12} className="voice-channel-participant__icon--muted" />;
  }
  return <Mic size={12} />;
}

function VoiceSubItems({
  channel,
  isGrouped,
  isActive,
  showLinkedText,
  linkedText,
  isLinkedTextActive,
  linkedTextUnread,
  voiceMembers,
  onLinkedTextClick,
  onParticipantClick,
  onParticipantContextMenu,
  onParticipantDragStart,
  onParticipantDragEnd,
  draggingParticipantUserId,
}: Readonly<VoiceSubItemsProps>) {
  const interactive = !!(onParticipantClick || onParticipantContextMenu || onParticipantDragStart);
  return (
    <div
      className={`voice-channel-sub-container ${isGrouped ? 'voice-channel-sub-container--grouped' : ''} ${isActive ? 'voice-channel-sub-container--active' : ''}`}
    >
      {showLinkedText && linkedText && (
        <button
          type="button"
          className={`channel-item--voice-text ${isGrouped ? 'channel-item--voice-text-grouped' : ''} ${isLinkedTextActive ? 'active' : ''} ${linkedTextUnread > 0 ? 'has-unread' : ''}`}
          onClick={(e) => {
            e.stopPropagation();
            onLinkedTextClick(channel, linkedText);
          }}
          title={`${channel.name} Text Chat`}
        >
          <span className="channel-type-icon">
            <MessageSquare size={12} />
          </span>
          <span className="channel-name">{channel.name} Text Chat</span>
          {linkedTextUnread > 0 && !isLinkedTextActive && (
            <span className="channel-unread-badge">
              {linkedTextUnread > 99 ? '99+' : linkedTextUnread}
            </span>
          )}
        </button>
      )}
      {voiceMembers.length > 0 && (
        <div
          className={`voice-channel-participants ${isGrouped ? 'voice-channel-participants--grouped' : ''}`}
        >
          {voiceMembers.map((p) => (
            <div
              key={p.userId}
              className={`voice-channel-participant ${p.isSpeaking ? 'speaking' : ''}`}
            >
              <VoiceStatusIcon p={p} />
              {interactive ? (
                // Native <button> for the interactive participant name — gives
                // keyboard activation (Enter/Space), focusability, and button
                // semantics natively, so no role/tabIndex/onKeyDown is needed
                // (resolves S6819/S6845/S6848). Button-reset styling lives in CSS.
                // A native button is fully draggable in Chromium (HTML5 DnD).
                <button
                  type="button"
                  className={`voice-channel-participant-name voice-participant-name--interactive${draggingParticipantUserId === p.userId ? ' voice-participant-name--dragging' : ''}`}
                  draggable={!!onParticipantDragStart}
                  onClick={
                    onParticipantClick ? (e) => onParticipantClick(e, channel.id, p) : undefined
                  }
                  onContextMenu={
                    onParticipantContextMenu
                      ? (e) => onParticipantContextMenu(e, channel.id, p)
                      : undefined
                  }
                  onDragStart={
                    onParticipantDragStart
                      ? (e) => onParticipantDragStart(e, channel.id, p)
                      : undefined
                  }
                  onDragEnd={onParticipantDragEnd}
                >
                  {p.displayName || p.username}
                </button>
              ) : (
                <span className="voice-channel-participant-name">
                  {p.displayName || p.username}
                </span>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function getChannelTypeIcon(type: Channel['type'], size = 16) {
  switch (type) {
    case 'text':
      return <Hash size={size} />;
    case 'voice':
      return <Volume2 size={size} />;
    case 'bulletin':
      return <Pin size={size} />;
    default:
      return <Hash size={size} />;
  }
}

function getChannelTypeLabel(type: Channel['type']): string {
  switch (type) {
    case 'voice':
      return 'voice channel';
    case 'bulletin':
      return 'bulletin channel';
    default:
      return 'text channel';
  }
}

function formatUnreadCount(unread: number): string | number {
  return unread > 99 ? '99+' : unread;
}

interface CompactChannelDetailController {
  itemElementRef: React.RefObject<HTMLButtonElement | null>;
  detailRef: React.RefObject<HTMLDialogElement | null>;
  detailPosition: { top: number; left: number; maxHeight: number } | null;
  detailOpen: boolean;
  handleTriggerMouseEnter: () => void;
  handleTriggerMouseLeave: (event: React.MouseEvent<HTMLButtonElement>) => void;
  handleTriggerFocus: () => void;
  handleTriggerBlur: (event: React.FocusEvent<HTMLButtonElement>) => void;
  handleTriggerKeyDown: (event: React.KeyboardEvent<HTMLButtonElement>) => void;
  handleDetailMouseEnter: () => void;
  handleDetailMouseLeave: (event: React.MouseEvent<HTMLDialogElement>) => void;
  handleDetailFocus: () => void;
  handleDetailBlur: (event: React.FocusEvent<HTMLDialogElement>) => void;
  handleDetailKeyDown: (event: React.KeyboardEvent<HTMLDialogElement>) => void;
}

interface ChannelTriggerProps {
  channel: Channel;
  compact: boolean;
  isActive: boolean;
  isDragging: boolean;
  canReorder: boolean;
  itemClasses: string;
  compactLabel: string;
  detailId: string;
  triggerId: string;
  detail: CompactChannelDetailController;
  showDraft: boolean;
  showUnread: boolean;
  unread: number;
  voiceMembers: VoiceMemberInfo[];
  voiceCountLabel: string;
  itemRef: (id: string, element: HTMLElement | null) => void;
  onChannelClick: (channel: Channel) => void;
  onContextMenu: (event: React.MouseEvent, channel: Channel) => void;
  onDragStart: (event: React.DragEvent, id: string, type: 'channel' | 'category') => void;
  onDragOver: (event: React.DragEvent, id: string, type: 'channel' | 'category') => void;
  onDrop: (event: React.DragEvent) => void;
  onDragEnd: () => void;
  onParticipantDragOver?: (event: React.DragEvent, channel: Channel) => void;
  onParticipantDrop?: (event: React.DragEvent, channel: Channel) => void;
}

interface CompactChannelDetailProps {
  channel: Channel;
  detailId: string;
  triggerId: string;
  detail: CompactChannelDetailController;
  voiceSubItemsProps: VoiceSubItemsProps;
}

function getCompactChannelLabel({
  channel,
  isMuted,
  showUnread,
  unread,
  showDraft,
  voiceMembers,
  voiceCountLabel,
}: Readonly<{
  channel: Channel;
  isMuted: boolean;
  showUnread: boolean;
  unread: number;
  showDraft: boolean;
  voiceMembers: VoiceMemberInfo[];
  voiceCountLabel: string;
}>): string {
  return [
    channel.name,
    getChannelTypeLabel(channel.type),
    isMuted ? 'muted' : null,
    showUnread ? `${formatUnreadCount(unread)} unread` : null,
    showDraft ? 'draft' : null,
    channel.type === 'voice' && voiceMembers.length > 0 ? voiceCountLabel : null,
  ]
    .filter(Boolean)
    .join(', ');
}

function getChannelItemClasses({
  compact,
  isActive,
  showUnread,
  isMuted,
  isGrouped,
  isLastInGroup,
  isParticipantDropTarget,
}: Readonly<{
  compact: boolean;
  isActive: boolean;
  showUnread: boolean;
  isMuted: boolean;
  isGrouped: boolean;
  isLastInGroup: boolean;
  isParticipantDropTarget?: boolean;
}>): string {
  return [
    'channel-item',
    compact ? 'channel-item--compact' : '',
    isActive ? 'active' : '',
    showUnread ? 'has-unread' : '',
    isMuted ? 'channel-item--muted' : '',
    isGrouped ? 'channel-item--grouped' : '',
    isLastInGroup ? 'channel-item--grouped-last' : '',
    isParticipantDropTarget ? 'channel-item--participant-drop-target' : '',
  ]
    .filter(Boolean)
    .join(' ');
}

function useCompactChannelDetail(
  compact: boolean,
  channel: Channel,
  onChannelClick: (channel: Channel) => void
): CompactChannelDetailController {
  const itemElementRef = useRef<HTMLButtonElement>(null);
  const detailRef = useRef<HTMLDialogElement>(null);
  const suppressFocusOpenRef = useRef(false);
  const [detailHovered, setDetailHovered] = useState(false);
  const [detailFocused, setDetailFocused] = useState(false);
  const [detailPosition, setDetailPosition] = useState<{
    top: number;
    left: number;
    maxHeight: number;
  } | null>(null);

  const updateDetailPosition = () => {
    const item = itemElementRef.current;
    if (!item) return;
    const rect = item.getBoundingClientRect();
    const top = Math.max(8, Math.min(rect.top, globalThis.innerHeight - 52));
    setDetailPosition({
      top,
      left: rect.right,
      maxHeight: Math.max(44, globalThis.innerHeight - top - 8),
    });
  };

  const focusDetail = () => {
    updateDetailPosition();
    setDetailFocused(true);
    const detail = detailRef.current;
    if (!detail) return;
    const firstFocusable = detail.querySelector<HTMLElement>(
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
    );
    (firstFocusable ?? detail).focus({ preventScroll: true });
  };

  const closeDetailAndRestoreTrigger = () => {
    suppressFocusOpenRef.current = true;
    setDetailHovered(false);
    setDetailFocused(false);
    itemElementRef.current?.focus({ preventScroll: true });
  };

  const handleTriggerMouseEnter = () => {
    if (!compact) return;
    updateDetailPosition();
    setDetailHovered(true);
  };
  const handleTriggerMouseLeave = (event: React.MouseEvent<HTMLButtonElement>) => {
    if (!compact) return;
    const related = event.relatedTarget;
    if (related instanceof Node && detailRef.current?.contains(related)) return;
    setDetailHovered(false);
  };
  const handleTriggerFocus = () => {
    if (!compact) return;
    if (suppressFocusOpenRef.current) {
      suppressFocusOpenRef.current = false;
      return;
    }
    updateDetailPosition();
    setDetailFocused(true);
  };
  const handleTriggerBlur = (event: React.FocusEvent<HTMLButtonElement>) => {
    if (!compact) return;
    const related = event.relatedTarget;
    if (related instanceof Node && detailRef.current?.contains(related)) return;
    setDetailFocused(false);
  };
  const handleTriggerKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>) => {
    if (!compact) return;
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      event.stopPropagation();
      onChannelClick(channel);
      focusDetail();
      return;
    }
    if (event.key === 'Escape' && (detailHovered || detailFocused)) {
      event.preventDefault();
      event.stopPropagation();
      setDetailHovered(false);
      setDetailFocused(false);
    }
  };
  const handleDetailMouseEnter = () => setDetailHovered(true);
  const handleDetailMouseLeave = (event: React.MouseEvent<HTMLDialogElement>) => {
    const related = event.relatedTarget;
    if (related instanceof Node && itemElementRef.current?.contains(related)) return;
    setDetailHovered(false);
  };
  const handleDetailFocus = () => setDetailFocused(true);
  const handleDetailBlur = (event: React.FocusEvent<HTMLDialogElement>) => {
    const related = event.relatedTarget;
    if (
      related instanceof Node &&
      (event.currentTarget.contains(related) || itemElementRef.current?.contains(related))
    ) {
      return;
    }
    setDetailFocused(false);
  };
  const handleDetailKeyDown = (event: React.KeyboardEvent<HTMLDialogElement>) => {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    event.stopPropagation();
    closeDetailAndRestoreTrigger();
  };

  return {
    itemElementRef,
    detailRef,
    detailPosition,
    detailOpen: compact && (detailHovered || detailFocused),
    handleTriggerMouseEnter,
    handleTriggerMouseLeave,
    handleTriggerFocus,
    handleTriggerBlur,
    handleTriggerKeyDown,
    handleDetailMouseEnter,
    handleDetailMouseLeave,
    handleDetailFocus,
    handleDetailBlur,
    handleDetailKeyDown,
  };
}

const ChannelButtonContent: React.FC<
  Pick<
    ChannelTriggerProps,
    | 'channel'
    | 'compact'
    | 'showDraft'
    | 'showUnread'
    | 'unread'
    | 'voiceMembers'
    | 'voiceCountLabel'
  >
> = ({ channel, compact, showDraft, showUnread, unread, voiceMembers, voiceCountLabel }) => (
  <>
    <span className="channel-type-icon">{getChannelTypeIcon(channel.type, compact ? 14 : 16)}</span>
    {channel.emoji && <span className="channel-custom-emoji">{channel.emoji}</span>}
    <span className="channel-name">{channel.name}</span>
    <span className="channel-encrypted-icon" title="End-to-End Encrypted">
      <Lock size={12} />
    </span>
    {showDraft && (
      <span className="channel-draft-indicator" title="Draft message">
        <PenLine size={12} />
      </span>
    )}
    {showUnread && <span className="channel-unread-badge">{formatUnreadCount(unread)}</span>}
    {compact && channel.type === 'voice' && voiceMembers.length > 0 && (
      <span className="channel-item__voice-count" aria-label={voiceCountLabel}>
        {voiceMembers.length}
      </span>
    )}
  </>
);

const ChannelTrigger: React.FC<ChannelTriggerProps> = ({
  channel,
  compact,
  isActive,
  isDragging,
  canReorder,
  itemClasses,
  compactLabel,
  detailId,
  triggerId,
  detail,
  showDraft,
  showUnread,
  unread,
  voiceMembers,
  voiceCountLabel,
  itemRef,
  onChannelClick,
  onContextMenu,
  onDragStart,
  onDragOver,
  onDrop,
  onDragEnd,
  onParticipantDragOver,
  onParticipantDrop,
}) => (
  <button
    type="button"
    id={compact ? triggerId : undefined}
    ref={(element) => {
      detail.itemElementRef.current = element;
      itemRef(channel.id, element);
    }}
    className={`${itemClasses} ${isDragging ? 'dragging' : ''}`}
    aria-label={compact ? compactLabel : undefined}
    aria-current={compact && isActive ? 'page' : undefined}
    aria-expanded={compact ? detail.detailOpen : undefined}
    aria-controls={detail.detailOpen ? detailId : undefined}
    draggable={canReorder}
    onMouseEnter={detail.handleTriggerMouseEnter}
    onMouseLeave={detail.handleTriggerMouseLeave}
    onFocus={detail.handleTriggerFocus}
    onBlur={detail.handleTriggerBlur}
    onKeyDown={detail.handleTriggerKeyDown}
    onDragStart={(event) => onDragStart(event, channel.id, 'channel')}
    onDragOver={(event) => {
      if (
        onParticipantDragOver &&
        event.dataTransfer.types.includes('application/concord-voice-participant')
      ) {
        onParticipantDragOver(event, channel);
        return;
      }
      onDragOver(event, channel.id, 'channel');
    }}
    onDrop={(event) => {
      if (
        onParticipantDrop &&
        event.dataTransfer.types.includes('application/concord-voice-participant')
      ) {
        onParticipantDrop(event, channel);
        return;
      }
      onDrop(event);
    }}
    onDragEnd={onDragEnd}
    onClick={() => onChannelClick(channel)}
    onContextMenu={(event) => onContextMenu(event, channel)}
    title={channel.name}
  >
    <ChannelButtonContent
      channel={channel}
      compact={compact}
      showDraft={showDraft}
      showUnread={showUnread}
      unread={unread}
      voiceMembers={voiceMembers}
      voiceCountLabel={voiceCountLabel}
    />
  </button>
);

const CompactChannelDetail: React.FC<CompactChannelDetailProps> = ({
  channel,
  detailId,
  triggerId,
  detail,
  voiceSubItemsProps,
}) => {
  if (!detail.detailOpen || !detail.detailPosition) return null;
  const hasVoiceDetails =
    channel.type === 'voice' &&
    ((voiceSubItemsProps.showLinkedText && voiceSubItemsProps.linkedText) ||
      voiceSubItemsProps.voiceMembers.length > 0);

  return createPortal(
    <dialog
      id={detailId}
      ref={detail.detailRef}
      className="channel-item__compact-detail"
      data-dock-focus-owner={triggerId}
      aria-label={`${channel.name} channel details`}
      open
      tabIndex={-1}
      style={{ ...detail.detailPosition, margin: 0, padding: 0 }}
      onMouseEnter={detail.handleDetailMouseEnter}
      onMouseLeave={detail.handleDetailMouseLeave}
      onFocus={detail.handleDetailFocus}
      onBlur={detail.handleDetailBlur}
      onKeyDown={detail.handleDetailKeyDown}
    >
      <div className="channel-item__compact-detail-name">{channel.name}</div>
      {hasVoiceDetails && <VoiceSubItems {...voiceSubItemsProps} isGrouped={false} />}
    </dialog>,
    document.body
  );
};

const ChannelItem: React.FC<ChannelItemProps> = ({
  channel,
  isActive,
  unread,
  compact = false,
  isMuted = false,
  isGrouped,
  isLastInGroup,
  voiceMembers,
  linkedText,
  showLinkedText,
  isLinkedTextActive,
  linkedTextUnread,
  hasDraft,
  canReorder,
  isDragging,
  showGhostBefore,
  showGhostAfter,
  onChannelClick,
  onContextMenu,
  onDragStart,
  onDragOver,
  onDrop,
  onDragEnd,
  onLinkedTextClick,
  itemRef,
  onParticipantClick,
  onParticipantContextMenu,
  onParticipantDragStart,
  onParticipantDragEnd,
  isParticipantDropTarget,
  draggingParticipantUserId,
  onParticipantDragOver,
  onParticipantDrop,
}) => {
  const showUnread = unread > 0 && !isActive && !isMuted;
  const showDraft = Boolean(hasDraft && !isActive);
  const voiceCountLabel = `${voiceMembers.length} ${voiceMembers.length === 1 ? 'participant' : 'participants'}`;
  const compactLabel = getCompactChannelLabel({
    channel,
    isMuted,
    showUnread,
    unread,
    showDraft,
    voiceMembers,
    voiceCountLabel,
  });
  const detailId = `channel-compact-detail-${channel.id}`;
  const triggerId = `channel-compact-trigger-${channel.id}`;
  const detail = useCompactChannelDetail(compact, channel, onChannelClick);
  const itemClasses = getChannelItemClasses({
    compact,
    isActive,
    showUnread,
    isMuted,
    isGrouped,
    isLastInGroup,
    isParticipantDropTarget,
  });
  const voiceSubItemsProps: VoiceSubItemsProps = {
    channel,
    isGrouped,
    isActive,
    showLinkedText,
    linkedText,
    isLinkedTextActive,
    linkedTextUnread,
    voiceMembers,
    onLinkedTextClick,
    onParticipantClick,
    onParticipantContextMenu,
    onParticipantDragStart,
    onParticipantDragEnd,
    draggingParticipantUserId,
  };
  const showExpandedVoiceDetails =
    !compact && Boolean((showLinkedText && linkedText) || voiceMembers.length > 0);

  return (
    <React.Fragment>
      <div className={`channel-item-wrapper${isGrouped ? ' channel-item-wrapper--grouped' : ''}`}>
        {showGhostBefore && <div className="channel-drag-ghost" />}
        <ChannelTrigger
          channel={channel}
          compact={compact}
          isActive={isActive}
          isDragging={isDragging}
          canReorder={canReorder}
          itemClasses={itemClasses}
          compactLabel={compactLabel}
          detailId={detailId}
          triggerId={triggerId}
          detail={detail}
          showDraft={showDraft}
          showUnread={showUnread}
          unread={unread}
          voiceMembers={voiceMembers}
          voiceCountLabel={voiceCountLabel}
          itemRef={itemRef}
          onChannelClick={onChannelClick}
          onContextMenu={onContextMenu}
          onDragStart={onDragStart}
          onDragOver={onDragOver}
          onDrop={onDrop}
          onDragEnd={onDragEnd}
          onParticipantDragOver={onParticipantDragOver}
          onParticipantDrop={onParticipantDrop}
        />
        {showGhostAfter && <div className="channel-drag-ghost" />}
      </div>
      {showExpandedVoiceDetails && <VoiceSubItems {...voiceSubItemsProps} />}
      <CompactChannelDetail
        channel={channel}
        detailId={detailId}
        triggerId={triggerId}
        detail={detail}
        voiceSubItemsProps={voiceSubItemsProps}
      />
    </React.Fragment>
  );
};

export default React.memo(ChannelItem);
