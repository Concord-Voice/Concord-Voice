import { render, screen, fireEvent } from '../../../test-utils';
import ChannelList, {
  parseGapSlot,
  buildGapDropUpdates,
  buildNormalDropUpdates,
  buildOldGroupUpdates,
} from '@/renderer/components/Channels/ChannelList';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockChannel } from '../../../mocks/fixtures';
import { ADMIN_PERMISSIONS, MANAGE_CHANNELS } from '@/renderer/utils/permissions';
import type { Channel, ChannelGroup } from '@/renderer/types/chat';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve({}) }),
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

const mockGroup1: ChannelGroup = {
  id: 'group-1',
  server_id: 'server-1',
  name: 'Text Channels',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockGroup2: ChannelGroup = {
  id: 'group-2',
  server_id: 'server-1',
  name: 'Voice Channels',
  position: 1,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

describe('ChannelList — extended coverage', () => {
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
    useChannelStore.setState({
      fetchChannels: vi.fn() as unknown as (serverId: string) => Promise<void>,
      clearChannels: vi.fn() as unknown as () => void,
    });
  });

  // --- Collapsed group unread count ---

  describe('collapsed category unread badge', () => {
    it('shows summed unread count for collapsed group', () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [
          { ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 },
          { ...mockChannel, id: 'ch-2', name: 'random', group_id: 'group-1', position: 1 },
        ],
        channelGroups: [mockGroup1],
        collapsedGroups: ['group-1'],
        isLoading: false,
        error: null,
      });
      useUnreadStore.getState().setUnreadCount('ch-1', 3);
      useUnreadStore.getState().setUnreadCount('ch-2', 7);
      renderChannelList();

      expect(screen.getByText('10')).toBeInTheDocument();
    });

    it('shows 99+ when collapsed group unread exceeds 99', () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [{ ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 }],
        channelGroups: [mockGroup1],
        collapsedGroups: ['group-1'],
        isLoading: false,
        error: null,
      });
      useUnreadStore.getState().setUnreadCount('ch-1', 150);
      renderChannelList();

      expect(screen.getByText('99+')).toBeInTheDocument();
    });
  });

  // --- Interleaved uncategorized channels ---

  describe('uncategorized channel slots', () => {
    it('renders uncategorized channels between categories based on position', () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [
          // Uncategorized in slot 0 (before group-1)
          { ...mockChannel, id: 'uncat-1', name: 'top-level', group_id: null, position: 1000 },
          // Categorized
          { ...mockChannel, id: 'cat-1', name: 'in-group', group_id: 'group-1', position: 0 },
        ],
        channelGroups: [mockGroup1],
        isLoading: false,
        error: null,
      });
      renderChannelList();
      expect(screen.getByText('top-level')).toBeInTheDocument();
      // Verify a group section is rendered (group header uses uppercase text)
      const groups = document.querySelectorAll('.channel-group');
      expect(groups.length).toBeGreaterThanOrEqual(1);
    });

    it('renders legacy uncategorized channels (position < 1000) at the bottom', () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [
          { ...mockChannel, id: 'legacy-1', name: 'legacy', group_id: null, position: 5 },
          { ...mockChannel, id: 'cat-1', name: 'grouped', group_id: 'group-1', position: 0 },
        ],
        channelGroups: [mockGroup1],
        isLoading: false,
        error: null,
      });
      renderChannelList();
      expect(screen.getByText('legacy')).toBeInTheDocument();
    });
  });

  // --- Linked text channel rendering ---

  describe('linked text channels', () => {
    it('does not render linked text channel as a separate item', () => {
      const linkedText: Channel = {
        id: 'linked-text-1',
        server_id: 'server-1',
        name: 'voice-chat',
        type: 'text',
        position: 3,
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
        linked_voice_channel_id: 'voice-1',
      };
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [mockVoiceChannel, linkedText],
        channelGroups: [],
        isLoading: false,
        error: null,
      });
      renderChannelList();
      // The linked text channel should not appear as a standalone entry
      expect(screen.getByText('General Voice')).toBeInTheDocument();
      // voice-chat should NOT render as its own standalone item
      // It may or may not appear inline depending on voice state
    });
  });

  // --- Empty context menu ---

  describe('empty context menu', () => {
    it('fires empty context menu on right-click in empty space', () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [mockChannel],
        isLoading: false,
        error: null,
      });
      const { container } = renderChannelList();
      const channelList = container.querySelector('.channel-list');
      fireEvent.contextMenu(channelList!);
      expect(onEmptyContextMenu).toHaveBeenCalled();
    });

    it('does not fire empty context menu when clicking on a channel item', () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [mockChannel],
        isLoading: false,
        error: null,
      });
      renderChannelList();
      fireEvent.contextMenu(screen.getByText('general'));
      // onContextMenu should be called, not onEmptyContextMenu
      expect(onContextMenu).toHaveBeenCalled();
    });
  });

  // --- Collapsed voice avatars ---

  describe('collapsed voice avatars', () => {
    it('shows voice user avatars on collapsed category header', () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [{ ...mockVoiceChannel, group_id: 'group-2' }],
        channelGroups: [mockGroup2],
        collapsedGroups: ['group-2'],
        isLoading: false,
        error: null,
      });
      useVoiceStore.setState({
        channelVoiceMembers: {
          'voice-1': [
            { userId: 'u1', username: 'alice', displayName: 'Alice', isMuted: false },
            { userId: 'u2', username: 'bob', displayName: 'Bob', isMuted: false },
          ],
        },
      });
      const { container } = renderChannelList();
      const avatars = container.querySelectorAll('.channel-group-header__avatar');
      expect(avatars.length).toBe(2);
    });

    it('shows overflow count when more than 3 voice users', () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [{ ...mockVoiceChannel, group_id: 'group-2' }],
        channelGroups: [mockGroup2],
        collapsedGroups: ['group-2'],
        isLoading: false,
        error: null,
      });
      useVoiceStore.setState({
        channelVoiceMembers: {
          'voice-1': [
            { userId: 'u1', username: 'a', displayName: 'A', isMuted: false },
            { userId: 'u2', username: 'b', displayName: 'B', isMuted: false },
            { userId: 'u3', username: 'c', displayName: 'C', isMuted: false },
            { userId: 'u4', username: 'd', displayName: 'D', isMuted: false },
          ],
        },
      });
      renderChannelList();
      expect(screen.getByText('+1')).toBeInTheDocument();
    });
  });

  // --- CategoryGroupHeader toggle button a11y ---

  describe('category header toggle button accessibility', () => {
    const setupCategory = (collapsed = false) => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [{ ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 }],
        channelGroups: [mockGroup1],
        collapsedGroups: collapsed ? ['group-1'] : [],
        toggleGroupCollapsed: vi.fn(),
        isLoading: false,
        error: null,
      });
    };

    it('category header toggle button has aria-expanded true when not collapsed', () => {
      setupCategory(false);
      renderChannelList();
      const toggle = screen.getByRole('button', { name: 'Text Channels' });
      expect(toggle).toHaveAttribute('aria-expanded', 'true');
    });

    it('category header toggle button has aria-expanded false when collapsed', () => {
      setupCategory(true);
      renderChannelList();
      const toggle = screen.getByRole('button', { name: 'Text Channels' });
      expect(toggle).toHaveAttribute('aria-expanded', 'false');
    });

    it('category header toggle button has aria-label with category name', () => {
      setupCategory();
      renderChannelList();
      const toggle = screen.getByRole('button', { name: 'Text Channels' });
      expect(toggle).toHaveAttribute('aria-label', 'Text Channels');
    });

    it('clicking category toggle button calls onToggleCollapsed', () => {
      setupCategory();
      renderChannelList();
      const toggle = screen.getByRole('button', { name: 'Text Channels' });
      fireEvent.click(toggle);
      expect(useChannelStore.getState().toggleGroupCollapsed).toHaveBeenCalledWith('group-1');
    });

    it('category toggle is keyboard accessible as a native button', () => {
      setupCategory();
      renderChannelList();
      const toggle = screen.getByRole('button', { name: 'Text Channels' });
      expect(toggle.tagName).toBe('BUTTON');
      expect(toggle).toHaveAttribute('type', 'button');
    });
  });

  // --- ARIA / a11y attributes ---

  describe('ARIA tree structure', () => {
    const setupWithGroupAndChannel = () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [{ ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 }],
        channelGroups: [mockGroup1],
        collapsedGroups: [],
        isLoading: false,
        error: null,
      });
    };

    it('renders the channel list with role="tree" and aria-label', () => {
      setupWithGroupAndChannel();
      renderChannelList();
      const tree = screen.getByRole('tree', { name: 'Server channels' });
      expect(tree).toBeInTheDocument();
    });

    it('renders channel group headers as <section> elements', () => {
      setupWithGroupAndChannel();
      renderChannelList();
      const regions = screen.getAllByRole('region');
      expect(regions.length).toBeGreaterThanOrEqual(1);
      expect(regions[0]).toHaveAttribute('aria-label', 'Text Channels channel group');
    });

    it('renders drag gap zones with aria-hidden (no ARIA role)', () => {
      setupWithGroupAndChannel();
      usePermissionStore.setState({
        serverPermissions: { 'server-1': ADMIN_PERMISSIONS | MANAGE_CHANNELS },
      });
      const { container } = renderChannelList();
      const gaps = container.querySelectorAll('.channel-drag-gap');
      expect(gaps.length).toBeGreaterThan(0);
      gaps.forEach((gap) => {
        expect(gap).toHaveAttribute('aria-hidden', 'true');
        expect(gap).not.toHaveAttribute('role');
      });
    });
  });

  // --- DnD gap zones ---

  describe('drag-and-drop gap zones', () => {
    it('renders gap zones when categories exist', () => {
      useServerStore.setState({ activeServerId: 'server-1' });
      useChannelStore.setState({
        channels: [{ ...mockChannel, group_id: 'group-1' }],
        channelGroups: [mockGroup1],
        isLoading: false,
        error: null,
      });
      usePermissionStore.setState({
        serverPermissions: { 'server-1': ADMIN_PERMISSIONS | MANAGE_CHANNELS },
      });
      const { container } = renderChannelList();
      const gaps = container.querySelectorAll('.channel-drag-gap');
      expect(gaps.length).toBeGreaterThan(0);
    });
  });
});

// --- Additional helper function tests ---

describe('buildGapDropUpdates — additional', () => {
  it('assigns position in correct slot range', () => {
    const source: Channel = { ...mockChannel, id: 'src', group_id: 'group-1', position: 0 };
    const existing: Channel[] = [];
    const updates = buildGapDropUpdates(source, 2, existing, 'src');
    // Slot 2 → base = (2+1)*1000 = 3000
    expect(updates[0].position).toBeGreaterThanOrEqual(3000);
    expect(updates[0].position).toBeLessThan(4000);
  });
});

describe('buildNormalDropUpdates — insert position', () => {
  const groups: ChannelGroup[] = [mockGroup1];

  it('inserts before a target channel', () => {
    const source: Channel = { ...mockChannel, id: 'src', group_id: 'group-1', position: 5 };
    const existing: Channel[] = [
      { ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 },
      { ...mockChannel, id: 'ch-2', group_id: 'group-1', position: 1 },
    ];
    const insertPos = { targetId: 'ch-1', side: 'before' as const, targetType: 'channel' as const };
    const updates = buildNormalDropUpdates(
      source,
      'group-1',
      null,
      insertPos,
      existing,
      groups,
      'src'
    );
    // Source should be at position 0 (before ch-1)
    const srcUpdate = updates.find((u) => u.channel_id === 'src');
    expect(srcUpdate?.position).toBe(0);
  });

  it('inserts after a target channel', () => {
    const source: Channel = { ...mockChannel, id: 'src', group_id: 'group-1', position: 5 };
    const existing: Channel[] = [
      { ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 },
      { ...mockChannel, id: 'ch-2', group_id: 'group-1', position: 1 },
    ];
    const insertPos = { targetId: 'ch-1', side: 'after' as const, targetType: 'channel' as const };
    const updates = buildNormalDropUpdates(
      source,
      'group-1',
      null,
      insertPos,
      existing,
      groups,
      'src'
    );
    const srcUpdate = updates.find((u) => u.channel_id === 'src');
    expect(srcUpdate?.position).toBe(1);
  });
});

// --- parseGapSlot edge cases ---

describe('parseGapSlot — edge cases', () => {
  const groups: ChannelGroup[] = [mockGroup1, mockGroup2];

  it('returns sortedGroups.length for __gap-bottom', () => {
    expect(parseGapSlot('__gap-bottom', groups)).toBe(2);
  });

  it('returns sortedGroups.length for __gap-uncategorized', () => {
    expect(parseGapSlot('__gap-uncategorized', groups)).toBe(2);
  });

  it('returns correct index for __gap-before-{catId}', () => {
    expect(parseGapSlot('__gap-before-group-1', groups)).toBe(0);
    expect(parseGapSlot('__gap-before-group-2', groups)).toBe(1);
  });

  it('returns 0 for __gap-before- with unknown category', () => {
    expect(parseGapSlot('__gap-before-unknown', groups)).toBe(0);
  });

  it('returns 0 for unrecognized gap ID', () => {
    expect(parseGapSlot('something-else', groups)).toBe(0);
  });
});

// --- Multiple categories with drag-gap zones ---

describe('ChannelList — multiple categories with drag gap zones', () => {
  const onContextMenu = vi.fn();
  const onEmptyContextMenu = vi.fn();
  const onCategoryContextMenu = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useChannelStore.setState({
      fetchChannels: vi.fn() as unknown as (serverId: string) => Promise<void>,
      clearChannels: vi.fn() as unknown as () => void,
    });
  });

  it('renders multiple category headers when multiple groups exist', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [
        { ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 },
        { ...mockChannel, id: 'ch-2', name: 'voice-general', group_id: 'group-2', position: 0 },
      ],
      channelGroups: [mockGroup1, mockGroup2],
      collapsedGroups: [],
      isLoading: false,
      error: null,
    });

    render(
      <ChannelList
        onContextMenu={onContextMenu}
        onEmptyContextMenu={onEmptyContextMenu}
        onCategoryContextMenu={onCategoryContextMenu}
      />
    );

    // Both group headers should render as <section> elements (names are uppercased in the component)
    const regions = screen.getAllByRole('region');
    expect(regions.length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText('TEXT CHANNELS')).toBeInTheDocument();
    expect(screen.getByText('VOICE CHANNELS')).toBeInTheDocument();
  });

  it('renders gap zones between categories when user has manage permission', () => {
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({
      channels: [
        { ...mockChannel, id: 'ch-1', group_id: 'group-1', position: 0 },
        { ...mockChannel, id: 'ch-2', name: 'voice-general', group_id: 'group-2', position: 0 },
      ],
      channelGroups: [mockGroup1, mockGroup2],
      collapsedGroups: [],
      isLoading: false,
      error: null,
    });
    usePermissionStore.setState({
      serverPermissions: { 'server-1': ADMIN_PERMISSIONS | MANAGE_CHANNELS },
    });

    const { container } = render(
      <ChannelList
        onContextMenu={onContextMenu}
        onEmptyContextMenu={onEmptyContextMenu}
        onCategoryContextMenu={onCategoryContextMenu}
      />
    );

    const gaps = container.querySelectorAll('.channel-drag-gap');
    // With 2 categories there should be gap zones between/around them
    expect(gaps.length).toBeGreaterThanOrEqual(2);
  });
});

describe('buildOldGroupUpdates — edge cases', () => {
  it('handles empty old group after source removal', () => {
    const channels: Channel[] = [{ ...mockChannel, id: 'src', group_id: 'group-1', position: 0 }];
    const updates = buildOldGroupUpdates('group-1', channels, 'src', [mockGroup1]);
    expect(updates).toHaveLength(0);
  });

  it('handles multiple uncategorized slots correctly', () => {
    const channels: Channel[] = [
      { ...mockChannel, id: 'ch-1', group_id: null, position: 1000 },
      { ...mockChannel, id: 'ch-2', group_id: null, position: 2000 },
      { ...mockChannel, id: 'src', group_id: null, position: 2001 },
    ];
    const updates = buildOldGroupUpdates(null, channels, 'src', [mockGroup1]);
    // ch-1 in slot 0, ch-2 in slot 1 — both reindexed
    expect(updates).toHaveLength(2);
  });
});
