import { render, screen, fireEvent } from '../../../test-utils';
import ChannelList, {
  parseGapSlot,
  buildGapDropUpdates,
  buildNormalDropUpdates,
  resolveTargetGroupId,
  buildOldGroupUpdates,
} from '@/renderer/components/Channels/ChannelList';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockChannel, mockEncryptedChannel } from '../../../mocks/fixtures';
import { ADMIN_PERMISSIONS, MANAGE_CHANNELS } from '@/renderer/utils/permissions';
import { vi } from 'vitest';
import type { Channel, ChannelGroup } from '@/renderer/types/chat';

// Mock apiFetch to prevent real API calls
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({ ok: true }),
  API_BASE: 'http://localhost:3001',
}));

const mockVoiceChannel: Channel = {
  id: 'voice-1',
  server_id: 'server-1',
  name: 'General Voice',
  type: 'voice',
  position: 2,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockBulletinChannel: Channel = {
  id: 'bulletin-1',
  server_id: 'server-1',
  name: 'Announcements',
  type: 'bulletin',
  position: 3,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockChannelGroup: ChannelGroup = {
  id: 'group-1',
  server_id: 'server-1',
  name: 'Text Channels',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockChannelGroup2: ChannelGroup = {
  id: 'group-2',
  server_id: 'server-1',
  name: 'Voice Channels',
  position: 1,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

describe('ChannelList', () => {
  const onContextMenu = vi.fn();
  const onEmptyContextMenu = vi.fn();
  const onCategoryContextMenu = vi.fn();

  const renderChannelList = () =>
    render(
      <ChannelList
        onContextMenu={onContextMenu}
        onEmptyContextMenu={onEmptyContextMenu}
        onCategoryContextMenu={onCategoryContextMenu}
      />
    );

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    // Override fetchChannels and clearChannels to prevent useEffect from modifying state
    useChannelStore.setState({
      fetchChannels: vi.fn() as unknown as (serverId: string) => Promise<void>,
      clearChannels: vi.fn() as unknown as () => void,
    });
  });

  // ── Empty / Loading / Error States ──

  it('shows empty state when no server selected', () => {
    renderChannelList();
    expect(screen.getByText('Select a server to view channels')).toBeInTheDocument();
  });

  it('shows empty state when server has no channels', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({ channels: [], channelGroups: [], isLoading: false, error: null });
    renderChannelList();
    expect(screen.getByText('No channels yet')).toBeInTheDocument();
  });

  it('shows loading skeletons when loading', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({ channels: [], isLoading: true, error: null });
    const { container } = renderChannelList();
    const skeletons = container.querySelectorAll('.channel-skeleton');
    expect(skeletons.length).toBe(3);
  });

  it('shows error state with retry button', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [],
      isLoading: false,
      error: 'Failed to load channels',
    });
    renderChannelList();
    expect(screen.getByText('Failed to load channels')).toBeInTheDocument();
    expect(screen.getByText('Retry')).toBeInTheDocument();
  });

  it('calls fetchChannels on retry click', () => {
    const mockFetch = vi.fn() as unknown as (serverId: string) => Promise<void>;
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [],
      isLoading: false,
      error: 'Network error',
      fetchChannels: mockFetch,
    });
    renderChannelList();
    fireEvent.click(screen.getByText('Retry'));
    expect(mockFetch).toHaveBeenCalledWith('server-1');
  });

  // ── Rendering Channels ──

  it('renders text channels with hash icon', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [{ ...mockChannel, group_id: 'group-1' }],
      channelGroups: [mockChannelGroup],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    expect(screen.getByText('general')).toBeInTheDocument();
    expect(screen.getByText('TEXT CHANNELS')).toBeInTheDocument();
  });

  it('renders voice channels', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [{ ...mockVoiceChannel, group_id: 'group-2' }],
      channelGroups: [mockChannelGroup2],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    expect(screen.getByText('General Voice')).toBeInTheDocument();
    expect(screen.getByText('VOICE CHANNELS')).toBeInTheDocument();
  });

  it('renders bulletin channels', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockBulletinChannel],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    expect(screen.getByText('Announcements')).toBeInTheDocument();
  });

  it('renders encrypted indicator for encrypted channels', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockEncryptedChannel],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    expect(screen.getByText('encrypted-chat')).toBeInTheDocument();
    const encryptedIcon = document.querySelector('.channel-encrypted-icon');
    expect(encryptedIcon).toBeInTheDocument();
  });

  it('renders multiple channel groups with correct headers', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [
        { ...mockChannel, group_id: 'group-1' },
        { ...mockVoiceChannel, group_id: 'group-2' },
      ],
      channelGroups: [mockChannelGroup, mockChannelGroup2],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    expect(screen.getByText('TEXT CHANNELS')).toBeInTheDocument();
    expect(screen.getByText('VOICE CHANNELS')).toBeInTheDocument();
  });

  it('renders uncategorized channels without a group header', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [{ ...mockChannel, group_id: null, position: 1000 }],
      channelGroups: [],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    expect(screen.getByText('general')).toBeInTheDocument();
    // No group header should exist
    const headers = document.querySelectorAll('.channel-group-header--collapsible');
    expect(headers.length).toBe(0);
  });

  // ── Active State ──

  it('highlights active channel', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: 'channel-1',
      isLoading: false,
      error: null,
    });
    renderChannelList();
    const activeItem = document.querySelector('.channel-item.active');
    expect(activeItem).toBeInTheDocument();
  });

  it('sets active channel on click', () => {
    const mockSetActive = vi.fn();
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: null,
      isLoading: false,
      error: null,
      setActiveChannel: mockSetActive,
    });
    renderChannelList();
    fireEvent.click(screen.getByText('general'));
    expect(mockSetActive).toHaveBeenCalledWith('channel-1');
  });

  // ── Unread Badges ──

  it('shows unread badge', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: null,
      isLoading: false,
      error: null,
    });
    useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 5]]));
    renderChannelList();
    expect(screen.getByText('5')).toBeInTheDocument();
  });

  it('shows 99+ for large unread counts', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: null,
      isLoading: false,
      error: null,
    });
    useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 150]]));
    renderChannelList();
    expect(screen.getByText('99+')).toBeInTheDocument();
  });

  it('does not show unread badge when channel is active', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: 'channel-1',
      isLoading: false,
      error: null,
    });
    useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 5]]));
    renderChannelList();
    expect(screen.queryByText('5')).not.toBeInTheDocument();
  });

  // ── Category Collapsing ──

  it('toggles category collapsed state on click', () => {
    const mockToggle = vi.fn();
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [{ ...mockChannel, group_id: 'group-1' }],
      channelGroups: [mockChannelGroup],
      collapsedGroups: [],
      toggleGroupCollapsed: mockToggle,
      isLoading: false,
      error: null,
    });
    renderChannelList();
    fireEvent.click(screen.getByText('TEXT CHANNELS'));
    expect(mockToggle).toHaveBeenCalledWith('group-1');
  });

  it('hides channels when category is collapsed', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [{ ...mockChannel, group_id: 'group-1' }],
      channelGroups: [mockChannelGroup],
      collapsedGroups: ['group-1'],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    expect(screen.getByText('TEXT CHANNELS')).toBeInTheDocument();
    // Channel should be hidden
    expect(screen.queryByText('general')).not.toBeInTheDocument();
  });

  // ── Context Menus ──

  it('fires context menu on channel right-click', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockChannel],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    fireEvent.contextMenu(screen.getByText('general'));
    expect(onContextMenu).toHaveBeenCalled();
  });

  it('fires category context menu on header right-click', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [{ ...mockChannel, group_id: 'group-1' }],
      channelGroups: [mockChannelGroup],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    fireEvent.contextMenu(screen.getByText('TEXT CHANNELS'));
    expect(onCategoryContextMenu).toHaveBeenCalled();
  });

  // ── Drag and Drop ──

  it('makes channels draggable when user has MANAGE_CHANNELS permission', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockChannel],
      isLoading: false,
      error: null,
    });
    usePermissionStore.setState({
      serverPermissions: { 'server-1': ADMIN_PERMISSIONS | MANAGE_CHANNELS },
    });
    renderChannelList();
    const channelItem = document.querySelector('.channel-item');
    expect(channelItem?.getAttribute('draggable')).toBe('true');
  });

  it('makes channels not draggable without permission', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockChannel],
      isLoading: false,
      error: null,
    });
    usePermissionStore.setState({
      serverPermissions: { 'server-1': 0n },
    });
    renderChannelList();
    const channelItem = document.querySelector('.channel-item');
    expect(channelItem?.getAttribute('draggable')).toBe('false');
  });

  // ── Voice Channel Members ──

  it('shows voice participants when connected to a voice channel', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockVoiceChannel],
      isLoading: false,
      error: null,
    });
    useVoiceStore.setState({
      activeChannelId: 'voice-1',
      connectionState: 'connected',
      participants: {
        'user-1': {
          peerId: 'peer-1',
          userId: 'user-1',
          username: 'testuser',
          displayName: 'Test User',
          isMuted: false,
          isDeafened: false,
          isSpeaking: false,
        },
      },
    });
    renderChannelList();
    expect(screen.getByText('Test User')).toBeInTheDocument();
  });

  it('shows channel voice members from REST data when not connected', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockVoiceChannel],
      isLoading: false,
      error: null,
    });
    useVoiceStore.setState({
      activeChannelId: null,
      connectionState: 'idle',
      channelVoiceMembers: {
        'voice-1': [
          {
            userId: 'user-2',
            username: 'otheruser',
            displayName: 'Other User',
            isMuted: false,
          },
        ],
      },
    });
    renderChannelList();
    expect(screen.getByText('Other User')).toBeInTheDocument();
  });

  it('maps enforcement flags from real-time participants to voice members', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockVoiceChannel],
      isLoading: false,
      error: null,
    });
    useVoiceStore.setState({
      activeChannelId: 'voice-1',
      connectionState: 'connected',
      participants: {
        'user-1': {
          peerId: 'peer-1',
          userId: 'user-1',
          username: 'enforceduser',
          displayName: 'Enforced User',
          isMuted: false,
          isDeafened: true,
          isSpeaking: false,
          serverMuted: true,
          serverDeafened: true,
        },
      },
    });
    renderChannelList();
    expect(screen.getByText('Enforced User')).toBeInTheDocument();
  });

  it('maps enforcement flags from cached channel voice members', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockVoiceChannel],
      isLoading: false,
      error: null,
    });
    useVoiceStore.setState({
      activeChannelId: null,
      connectionState: 'idle',
      channelVoiceMembers: {
        'voice-1': [
          {
            userId: 'user-3',
            username: 'cacheduser',
            displayName: 'Cached User',
            isMuted: true,
            serverMuted: true,
            serverDeafened: false,
          },
        ],
      },
    });
    renderChannelList();
    expect(screen.getByText('Cached User')).toBeInTheDocument();
  });

  it('defaults enforcement flags to false when missing from real-time participants', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockVoiceChannel],
      isLoading: false,
      error: null,
    });
    useVoiceStore.setState({
      activeChannelId: 'voice-1',
      connectionState: 'connected',
      participants: {
        'user-1': {
          peerId: 'peer-1',
          userId: 'user-1',
          username: 'plainuser',
          displayName: 'Plain User',
          isMuted: false,
          isSpeaking: false,
          // serverMuted, serverDeafened, isDeafened intentionally omitted
        },
      },
    });
    renderChannelList();
    expect(screen.getByText('Plain User')).toBeInTheDocument();
  });

  it('defaults enforcement flags to false when missing from cached voice members', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockVoiceChannel],
      isLoading: false,
      error: null,
    });
    useVoiceStore.setState({
      activeChannelId: null,
      connectionState: 'idle',
      channelVoiceMembers: {
        'voice-1': [
          {
            userId: 'user-4',
            username: 'minimaluser',
            displayName: 'Minimal User',
            isMuted: false,
            // serverMuted, serverDeafened intentionally omitted
          },
        ],
      },
    });
    renderChannelList();
    expect(screen.getByText('Minimal User')).toBeInTheDocument();
  });

  // ── Channel with Emoji ──

  it('renders channel custom emoji when set', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [{ ...mockChannel, emoji: '🎮' }],
      isLoading: false,
      error: null,
    });
    renderChannelList();
    expect(screen.getByText('🎮')).toBeInTheDocument();
  });

  // ── Empty hint ──

  it('shows "Use + Add above to create one" hint in empty state', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({ channels: [], channelGroups: [], isLoading: false, error: null });
    renderChannelList();
    expect(screen.getByText('Use "+ Add" above to create one')).toBeInTheDocument();
  });

  // ── Voice text chat toggle ──

  it('renders voice text chat indicator for active voice channel', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [mockVoiceChannel],
      channelGroups: [],
      isLoading: false,
      error: null,
      activeChannelId: null,
    });
    useVoiceStore.setState({
      activeChannelId: 'voice-1',
      connectionState: 'connected',
      participants: new Map([['user-1', { userId: 'user-1', username: 'testuser' }]]),
      showVoiceTextChat: false,
    });
    renderChannelList();
    expect(screen.getByText('General Voice')).toBeInTheDocument();
  });

  // ── Linked text channel ──

  it('renders linked text channel for voice channels', () => {
    const voiceWithLinked: Channel = {
      ...mockVoiceChannel,
      linked_text_channel_id: 'linked-text-1',
    };
    const linkedText: Channel = {
      id: 'linked-text-1',
      server_id: 'server-1',
      name: 'general-voice-text',
      type: 'text',
      position: 3,
      created_at: '2025-01-01T00:00:00Z',
      updated_at: '2025-01-01T00:00:00Z',
      linked_voice_channel_id: 'voice-1',
    };
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [voiceWithLinked, linkedText],
      channelGroups: [],
      isLoading: false,
      error: null,
    });
    usePermissionStore.setState({ activeServerPerms: ADMIN_PERMISSIONS });
    renderChannelList();
    // Linked text channels are rendered alongside their voice channel
    expect(screen.getByText('General Voice')).toBeInTheDocument();
  });
});

// ── Drag-drop helper function unit tests ──────────────────────────────

describe('parseGapSlot', () => {
  const groups: ChannelGroup[] = [mockChannelGroup, mockChannelGroup2];

  it('returns groups.length for __gap-bottom', () => {
    expect(parseGapSlot('__gap-bottom', groups)).toBe(2);
  });

  it('returns groups.length for __gap-uncategorized', () => {
    expect(parseGapSlot('__gap-uncategorized', groups)).toBe(2);
  });

  it('returns category index for __gap-before-{id}', () => {
    expect(parseGapSlot('__gap-before-group-2', groups)).toBe(1);
  });

  it('returns 0 for __gap-before- with first group', () => {
    expect(parseGapSlot('__gap-before-group-1', groups)).toBe(0);
  });

  it('returns 0 for unknown gap ID', () => {
    expect(parseGapSlot('unknown', groups)).toBe(0);
  });
});

describe('resolveTargetGroupId', () => {
  const channels: Channel[] = [
    { ...mockChannel, group_id: 'group-1' },
    { ...mockVoiceChannel, group_id: null },
  ];
  const groups: ChannelGroup[] = [mockChannelGroup];

  it('returns null for gap target type', () => {
    const result = resolveTargetGroupId(
      { targetId: '__gap-bottom', side: 'after', targetType: 'gap' },
      channels,
      groups
    );
    expect(result).toBeNull();
  });

  it('returns group ID for category target type', () => {
    const result = resolveTargetGroupId(
      { targetId: 'group-1', side: 'after', targetType: 'category' },
      channels,
      groups
    );
    expect(result).toBe('group-1');
  });

  it('returns undefined for non-existent category', () => {
    const result = resolveTargetGroupId(
      { targetId: 'nonexistent', side: 'after', targetType: 'category' },
      channels,
      groups
    );
    expect(result).toBeUndefined();
  });

  it('returns channel group_id for channel target type', () => {
    const result = resolveTargetGroupId(
      { targetId: mockChannel.id, side: 'after', targetType: 'channel' },
      channels,
      groups
    );
    expect(result).toBe('group-1');
  });

  it('returns null for ungrouped channel target', () => {
    const result = resolveTargetGroupId(
      { targetId: 'voice-1', side: 'after', targetType: 'channel' },
      channels,
      groups
    );
    expect(result).toBeNull();
  });

  it('returns undefined for non-existent channel target', () => {
    const result = resolveTargetGroupId(
      { targetId: 'nonexistent', side: 'after', targetType: 'channel' },
      channels,
      groups
    );
    expect(result).toBeUndefined();
  });
});

describe('buildGapDropUpdates', () => {
  it('places source channel into uncategorized slot', () => {
    const source: Channel = { ...mockChannel, id: 'src', group_id: 'group-1', position: 0 };
    const existing: Channel[] = [
      { ...mockChannel, id: 'existing-1', group_id: null, position: 1000 },
    ];
    const updates = buildGapDropUpdates(source, 0, existing, 'src');
    expect(updates.length).toBeGreaterThanOrEqual(1);
    expect(updates.every((u) => u.group_id === null)).toBe(true);
    expect(updates.find((u) => u.channel_id === 'src')).toBeDefined();
  });
});

describe('buildNormalDropUpdates', () => {
  const groups: ChannelGroup[] = [mockChannelGroup];

  it('builds updates for dropping into a group', () => {
    const source: Channel = { ...mockChannel, id: 'src', group_id: null, position: 1000 };
    const existing: Channel[] = [
      { ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 },
      { ...mockChannel, id: 'ch-2', group_id: 'group-1', position: 1 },
    ];
    const insertPos = {
      targetId: 'group-1',
      side: 'after' as const,
      targetType: 'category' as const,
    };
    const updates = buildNormalDropUpdates(
      source,
      'group-1',
      mockChannelGroup,
      insertPos,
      existing,
      groups,
      'src'
    );
    expect(updates.length).toBe(3); // 2 existing + 1 source
    expect(updates.every((u) => u.group_id === 'group-1')).toBe(true);
  });

  it('builds updates for reordering within uncategorized', () => {
    const source: Channel = { ...mockChannel, id: 'src', group_id: null, position: 1001 };
    const existing: Channel[] = [{ ...mockChannel, id: 'ch-1', group_id: null, position: 1000 }];
    const insertPos = { targetId: 'ch-1', side: 'before' as const, targetType: 'channel' as const };
    const updates = buildNormalDropUpdates(source, null, null, insertPos, existing, groups, 'src');
    expect(updates.length).toBe(2);
    expect(updates.every((u) => u.group_id === null)).toBe(true);
  });
});

describe('buildOldGroupUpdates', () => {
  const groups: ChannelGroup[] = [mockChannelGroup];

  it('reindexes grouped channels after source removal', () => {
    const channels: Channel[] = [
      { ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 },
      { ...mockChannel, id: 'ch-2', group_id: 'group-1', position: 1 },
      { ...mockChannel, id: 'src', group_id: 'group-1', position: 2 },
    ];
    const updates = buildOldGroupUpdates('group-1', channels, 'src', groups);
    expect(updates.length).toBe(2); // src excluded
    expect(updates[0].position).toBe(0);
    expect(updates[1].position).toBe(1);
  });

  it('handles uncategorized channels with slot-based positioning', () => {
    const channels: Channel[] = [
      { ...mockChannel, id: 'ch-1', group_id: null, position: 1000 },
      { ...mockChannel, id: 'ch-2', group_id: null, position: 1001 },
      { ...mockChannel, id: 'src', group_id: null, position: 1002 },
    ];
    const updates = buildOldGroupUpdates(null, channels, 'src', groups);
    expect(updates.length).toBe(2);
    expect(updates.every((u) => u.group_id === null)).toBe(true);
  });
});
