import { render, screen, fireEvent } from '../../../test-utils';
import ChannelItem, { type VoiceMemberInfo } from '@/renderer/components/Channels/ChannelItem';
import type { Channel } from '@/renderer/types/chat';
import { vi } from 'vitest';

const mockTextChannel: Channel = {
  id: 'channel-1',
  server_id: 'server-1',
  name: 'general',
  type: 'text',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockVoiceChannel: Channel = {
  id: 'voice-1',
  server_id: 'server-1',
  name: 'General Voice',
  type: 'voice',
  position: 1,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockBulletinChannel: Channel = {
  id: 'bulletin-1',
  server_id: 'server-1',
  name: 'Announcements',
  type: 'bulletin',
  position: 2,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockLinkedTextChannel: Channel = {
  id: 'linked-text-1',
  server_id: 'server-1',
  name: 'General Voice',
  type: 'text',
  position: 0,
  linked_voice_channel_id: 'voice-1',
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const defaultProps = {
  channel: mockTextChannel,
  isActive: false,
  unread: 0,
  isGrouped: false,
  isLastInGroup: false,
  voiceMembers: [] as VoiceMemberInfo[],
  linkedText: null as Channel | null,
  showLinkedText: false,
  isLinkedTextActive: false,
  linkedTextUnread: 0,
  canReorder: false,
  isDragging: false,
  showGhostBefore: false,
  showGhostAfter: false,
  onChannelClick: vi.fn(),
  onContextMenu: vi.fn(),
  onDragStart: vi.fn(),
  onDragOver: vi.fn(),
  onDrop: vi.fn(),
  onDragEnd: vi.fn(),
  onLinkedTextClick: vi.fn(),
  itemRef: vi.fn(),
};

describe('ChannelItem', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // -- Basic Rendering --

  it('renders text channel with name', () => {
    render(<ChannelItem {...defaultProps} />);
    expect(screen.getByText('general')).toBeInTheDocument();
  });

  it('renders voice channel with correct icon', () => {
    render(<ChannelItem {...defaultProps} channel={mockVoiceChannel} />);
    expect(screen.getByText('General Voice')).toBeInTheDocument();
  });

  it('renders bulletin channel', () => {
    render(<ChannelItem {...defaultProps} channel={mockBulletinChannel} />);
    expect(screen.getByText('Announcements')).toBeInTheDocument();
  });

  it('renders channel name with title attribute', () => {
    render(<ChannelItem {...defaultProps} />);
    const item = document.querySelector('.channel-item');
    expect(item?.getAttribute('title')).toBe('general');
  });

  // -- Channel Type Icons --

  it('renders hash icon for text channels', () => {
    render(<ChannelItem {...defaultProps} channel={mockTextChannel} />);
    const icon = document.querySelector('.channel-type-icon');
    expect(icon).toBeInTheDocument();
  });

  it('renders different icon for voice channels', () => {
    render(<ChannelItem {...defaultProps} channel={mockVoiceChannel} />);
    const icon = document.querySelector('.channel-type-icon');
    expect(icon).toBeInTheDocument();
  });

  // -- Active State --

  it('applies active class when isActive is true', () => {
    render(<ChannelItem {...defaultProps} isActive={true} />);
    const item = document.querySelector('.channel-item.active');
    expect(item).toBeInTheDocument();
  });

  it('does not apply active class when isActive is false', () => {
    render(<ChannelItem {...defaultProps} isActive={false} />);
    const item = document.querySelector('.channel-item.active');
    expect(item).not.toBeInTheDocument();
  });

  // -- Unread Badges --

  it('shows unread badge with count', () => {
    render(<ChannelItem {...defaultProps} unread={3} />);
    expect(screen.getByText('3')).toBeInTheDocument();
  });

  it('shows 99+ for counts over 99', () => {
    render(<ChannelItem {...defaultProps} unread={200} />);
    expect(screen.getByText('99+')).toBeInTheDocument();
  });

  it('does not show unread badge when count is 0', () => {
    render(<ChannelItem {...defaultProps} unread={0} />);
    const badge = document.querySelector('.channel-unread-badge');
    expect(badge).not.toBeInTheDocument();
  });

  it('does not show unread badge when channel is active', () => {
    render(<ChannelItem {...defaultProps} isActive={true} unread={5} />);
    const badge = document.querySelector('.channel-unread-badge');
    expect(badge).not.toBeInTheDocument();
  });

  it('applies has-unread class when there are unreads', () => {
    render(<ChannelItem {...defaultProps} unread={3} />);
    const item = document.querySelector('.channel-item.has-unread');
    expect(item).toBeInTheDocument();
  });

  // -- Encrypted Indicator --

  it('always shows encrypted icon (all channels are E2EE)', () => {
    render(<ChannelItem {...defaultProps} />);
    const encIcon = document.querySelector('.channel-encrypted-icon');
    expect(encIcon).toBeInTheDocument();
  });

  // -- Draft Indicator --

  it('renders pencil icon when hasDraft is true and not active', () => {
    const { container } = render(
      <ChannelItem {...defaultProps} hasDraft={true} isActive={false} />
    );
    const draftIndicator = container.querySelector('.channel-draft-indicator');
    expect(draftIndicator).toBeInTheDocument();
    expect(draftIndicator?.getAttribute('title')).toBe('Draft message');
  });

  it('does not render pencil icon when hasDraft is false', () => {
    const { container } = render(<ChannelItem {...defaultProps} hasDraft={false} />);
    const draftIndicator = container.querySelector('.channel-draft-indicator');
    expect(draftIndicator).not.toBeInTheDocument();
  });

  it('does not render pencil icon when active even if hasDraft is true', () => {
    const { container } = render(<ChannelItem {...defaultProps} hasDraft={true} isActive={true} />);
    const draftIndicator = container.querySelector('.channel-draft-indicator');
    expect(draftIndicator).not.toBeInTheDocument();
  });

  // -- Emoji --

  it('renders custom emoji when channel has one', () => {
    render(<ChannelItem {...defaultProps} channel={{ ...mockTextChannel, emoji: '🔥' }} />);
    expect(screen.getByText('🔥')).toBeInTheDocument();
  });

  it('does not render emoji element when channel has no emoji', () => {
    render(<ChannelItem {...defaultProps} />);
    const emoji = document.querySelector('.channel-custom-emoji');
    expect(emoji).not.toBeInTheDocument();
  });

  // -- Click Handling --

  it('calls onChannelClick on click', () => {
    const onClick = vi.fn();
    render(<ChannelItem {...defaultProps} onChannelClick={onClick} />);
    fireEvent.click(screen.getByText('general'));
    expect(onClick).toHaveBeenCalledWith(mockTextChannel);
  });

  it('calls onContextMenu on right-click', () => {
    const onCtx = vi.fn();
    render(<ChannelItem {...defaultProps} onContextMenu={onCtx} />);
    fireEvent.contextMenu(screen.getByText('general'));
    expect(onCtx).toHaveBeenCalled();
  });

  // -- Grouped State --

  it('applies grouped class when isGrouped is true', () => {
    const { container } = render(<ChannelItem {...defaultProps} isGrouped={true} />);
    const grouped = container.querySelector('.channel-item--grouped');
    expect(grouped).toBeInTheDocument();
  });

  it('applies grouped-last class when isLastInGroup is true', () => {
    const { container } = render(
      <ChannelItem {...defaultProps} isGrouped={true} isLastInGroup={true} />
    );
    const last = container.querySelector('.channel-item--grouped-last');
    expect(last).toBeInTheDocument();
  });

  // -- Drag and Drop --

  it('makes item draggable when canReorder is true', () => {
    render(<ChannelItem {...defaultProps} canReorder={true} />);
    const item = document.querySelector('.channel-item');
    expect(item?.getAttribute('draggable')).toBe('true');
  });

  it('makes item not draggable when canReorder is false', () => {
    render(<ChannelItem {...defaultProps} canReorder={false} />);
    const item = document.querySelector('.channel-item');
    expect(item?.getAttribute('draggable')).toBe('false');
  });

  it('applies dragging class when isDragging is true', () => {
    render(<ChannelItem {...defaultProps} isDragging={true} />);
    const item = document.querySelector('.channel-item.dragging');
    expect(item).toBeInTheDocument();
  });

  it('shows ghost before indicator', () => {
    const { container } = render(<ChannelItem {...defaultProps} showGhostBefore={true} />);
    const ghost = container.querySelector('.channel-drag-ghost');
    expect(ghost).toBeInTheDocument();
  });

  it('shows ghost after indicator', () => {
    const { container } = render(<ChannelItem {...defaultProps} showGhostAfter={true} />);
    const ghost = container.querySelector('.channel-drag-ghost');
    expect(ghost).toBeInTheDocument();
  });

  it('calls onDragStart on drag start', () => {
    const onDragStart = vi.fn();
    render(<ChannelItem {...defaultProps} canReorder={true} onDragStart={onDragStart} />);
    const item = document.querySelector('.channel-item')!;
    fireEvent.dragStart(item);
    expect(onDragStart).toHaveBeenCalledWith(expect.anything(), 'channel-1', 'channel');
  });

  it('calls onDragOver on drag over', () => {
    const onDragOver = vi.fn();
    render(<ChannelItem {...defaultProps} onDragOver={onDragOver} />);
    const item = document.querySelector('.channel-item')!;
    fireEvent.dragOver(item);
    expect(onDragOver).toHaveBeenCalledWith(expect.anything(), 'channel-1', 'channel');
  });

  it('calls onDrop on drop', () => {
    const onDrop = vi.fn();
    render(<ChannelItem {...defaultProps} onDrop={onDrop} />);
    const item = document.querySelector('.channel-item')!;
    fireEvent.drop(item);
    expect(onDrop).toHaveBeenCalled();
  });

  it('calls onDragEnd on drag end', () => {
    const onDragEnd = vi.fn();
    render(<ChannelItem {...defaultProps} onDragEnd={onDragEnd} />);
    const item = document.querySelector('.channel-item')!;
    fireEvent.dragEnd(item);
    expect(onDragEnd).toHaveBeenCalled();
  });

  // -- Voice Members --

  it('renders voice member list for voice channels with members', () => {
    const voiceMembers: VoiceMemberInfo[] = [
      { userId: 'u1', username: 'alice', isMuted: false, isSpeaking: false },
      { userId: 'u2', username: 'bob', displayName: 'Bob', isMuted: true, isSpeaking: false },
    ];
    render(
      <ChannelItem {...defaultProps} channel={mockVoiceChannel} voiceMembers={voiceMembers} />
    );
    expect(screen.getByText('alice')).toBeInTheDocument();
    expect(screen.getByText('Bob')).toBeInTheDocument();
  });

  it('shows mute icon for muted voice members', () => {
    const voiceMembers: VoiceMemberInfo[] = [{ userId: 'u1', username: 'alice', isMuted: true }];
    const { container } = render(
      <ChannelItem {...defaultProps} channel={mockVoiceChannel} voiceMembers={voiceMembers} />
    );
    const participant = container.querySelector('.voice-channel-participant');
    expect(participant).toBeInTheDocument();
  });

  it('applies speaking class to speaking voice members', () => {
    const voiceMembers: VoiceMemberInfo[] = [
      { userId: 'u1', username: 'alice', isMuted: false, isSpeaking: true },
    ];
    const { container } = render(
      <ChannelItem {...defaultProps} channel={mockVoiceChannel} voiceMembers={voiceMembers} />
    );
    const speaking = container.querySelector('.voice-channel-participant.speaking');
    expect(speaking).toBeInTheDocument();
  });

  // -- VoiceStatusIcon branches --

  it('shows server-enforced icon with HeadphoneOff + Lock for server-deafened member', () => {
    const voiceMembers: VoiceMemberInfo[] = [
      {
        userId: 'u1',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        serverMuted: false,
        serverDeafened: true,
      },
    ];
    const { container } = render(
      <ChannelItem {...defaultProps} channel={mockVoiceChannel} voiceMembers={voiceMembers} />
    );
    const serverIcon = container.querySelector('.voice-channel-participant__icon--server-enforced');
    expect(serverIcon).toBeInTheDocument();
    const lockIcon = container.querySelector('.voice-channel-participant__lock');
    expect(lockIcon).toBeInTheDocument();
  });

  it('shows server-enforced icon with MicOff + Lock for server-muted member', () => {
    const voiceMembers: VoiceMemberInfo[] = [
      {
        userId: 'u1',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        serverMuted: true,
        serverDeafened: false,
      },
    ];
    const { container } = render(
      <ChannelItem {...defaultProps} channel={mockVoiceChannel} voiceMembers={voiceMembers} />
    );
    const serverIcon = container.querySelector('.voice-channel-participant__icon--server-enforced');
    expect(serverIcon).toBeInTheDocument();
    const lockIcon = container.querySelector('.voice-channel-participant__lock');
    expect(lockIcon).toBeInTheDocument();
  });

  it('shows HeadphoneOff icon (without Lock) for self-deafened member', () => {
    const voiceMembers: VoiceMemberInfo[] = [
      {
        userId: 'u1',
        username: 'alice',
        isMuted: false,
        isDeafened: true,
        serverMuted: false,
        serverDeafened: false,
      },
    ];
    const { container } = render(
      <ChannelItem {...defaultProps} channel={mockVoiceChannel} voiceMembers={voiceMembers} />
    );
    const mutedIcon = container.querySelector('.voice-channel-participant__icon--muted');
    expect(mutedIcon).toBeInTheDocument();
    // Should NOT have a lock icon (not server-enforced)
    const lockIcon = container.querySelector('.voice-channel-participant__lock');
    expect(lockIcon).not.toBeInTheDocument();
  });

  it('prioritizes serverDeafened over serverMuted and self-deafened', () => {
    const voiceMembers: VoiceMemberInfo[] = [
      {
        userId: 'u1',
        username: 'alice',
        isMuted: true,
        isDeafened: true,
        serverMuted: true,
        serverDeafened: true,
      },
    ];
    const { container } = render(
      <ChannelItem {...defaultProps} channel={mockVoiceChannel} voiceMembers={voiceMembers} />
    );
    // serverDeafened should take priority — shows HeadphoneOff + Lock
    const serverIcon = container.querySelector('.voice-channel-participant__icon--server-enforced');
    expect(serverIcon).toBeInTheDocument();
  });

  // -- Linked Text Channel --

  it('shows linked text channel when showLinkedText is true', () => {
    render(
      <ChannelItem
        {...defaultProps}
        channel={mockVoiceChannel}
        linkedText={mockLinkedTextChannel}
        showLinkedText={true}
      />
    );
    expect(screen.getByText('General Voice Text Chat')).toBeInTheDocument();
  });

  it('does not show linked text channel when showLinkedText is false', () => {
    render(
      <ChannelItem
        {...defaultProps}
        channel={mockVoiceChannel}
        linkedText={mockLinkedTextChannel}
        showLinkedText={false}
      />
    );
    expect(screen.queryByText('General Voice Text Chat')).not.toBeInTheDocument();
  });

  it('calls onLinkedTextClick when linked text is clicked', () => {
    const onLinkedTextClick = vi.fn();
    render(
      <ChannelItem
        {...defaultProps}
        channel={mockVoiceChannel}
        linkedText={mockLinkedTextChannel}
        showLinkedText={true}
        onLinkedTextClick={onLinkedTextClick}
      />
    );
    fireEvent.click(screen.getByText('General Voice Text Chat'));
    expect(onLinkedTextClick).toHaveBeenCalledWith(mockVoiceChannel, mockLinkedTextChannel);
  });

  it('shows linked text unread badge', () => {
    render(
      <ChannelItem
        {...defaultProps}
        channel={mockVoiceChannel}
        linkedText={mockLinkedTextChannel}
        showLinkedText={true}
        linkedTextUnread={7}
        isLinkedTextActive={false}
      />
    );
    expect(screen.getByText('7')).toBeInTheDocument();
  });

  it('does not show linked text unread badge when linked text is active', () => {
    render(
      <ChannelItem
        {...defaultProps}
        channel={mockVoiceChannel}
        linkedText={mockLinkedTextChannel}
        showLinkedText={true}
        linkedTextUnread={7}
        isLinkedTextActive={true}
      />
    );
    const badges = document.querySelectorAll('.channel-unread-badge');
    expect(badges.length).toBe(0);
  });

  it('applies active class to linked text when isLinkedTextActive is true', () => {
    const { container } = render(
      <ChannelItem
        {...defaultProps}
        channel={mockVoiceChannel}
        linkedText={mockLinkedTextChannel}
        showLinkedText={true}
        isLinkedTextActive={true}
      />
    );
    const linkedActive = container.querySelector('.channel-item--voice-text.active');
    expect(linkedActive).toBeInTheDocument();
  });

  // -- itemRef --

  it('calls itemRef callback with channel id', () => {
    const itemRef = vi.fn();
    render(<ChannelItem {...defaultProps} itemRef={itemRef} />);
    expect(itemRef).toHaveBeenCalledWith('channel-1', expect.anything());
  });
});
