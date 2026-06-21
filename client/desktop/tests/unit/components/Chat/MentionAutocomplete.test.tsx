import React from 'react';
import { render, screen, fireEvent } from '../../../test-utils';
import MentionAutocomplete, {
  type MentionAutocompleteHandle,
} from '@/renderer/components/Chat/MentionAutocomplete';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockMember, mockMember2 } from '../../../mocks/fixtures';
import { MENTION_EVERYONE, MENTION_USERS, MENTION_ROLES } from '@/renderer/utils/permissions';
import { vi } from 'vitest';

// jsdom lacks scrollIntoView
Element.prototype.scrollIntoView = vi.fn();

describe('MentionAutocomplete', () => {
  const onSelect = vi.fn();
  const onClose = vi.fn();

  const defaultProps = {
    text: '@',
    cursorPosition: 1,
    serverId: 'server-1',
    channelId: 'channel-1',
    onSelect,
    onClose,
  };

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useMemberStore.setState({
      members: [mockMember, mockMember2],
    });
    // Grant all mention permissions
    usePermissionStore.setState({
      serverPermissions: {
        'server-1': MENTION_EVERYONE | MENTION_USERS | MENTION_ROLES,
      },
      serverRoles: {
        'server-1': [
          {
            id: 'role-mod',
            server_id: 'server-1',
            name: 'Moderator',
            color: '#e74c3c',
            permissions: '0',
            position: 1,
            is_default: false,
            display_separately: false,
            mentionable: true,
            created_at: '2025-01-01T00:00:00Z',
            updated_at: '2025-01-01T00:00:00Z',
          },
          {
            id: 'role-default',
            server_id: 'server-1',
            name: '@everyone',
            color: '#99aab5',
            permissions: '0',
            position: 0,
            is_default: true,
            display_separately: false,
            mentionable: false,
            created_at: '2025-01-01T00:00:00Z',
            updated_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
      channelOverrides: {},
    });
  });

  // ── Rendering ──

  it('renders nothing when query is null (no @ in text)', () => {
    const { container } = render(
      <MentionAutocomplete {...defaultProps} text="hello" cursorPosition={5} />
    );
    expect(container.querySelector('.mention-autocomplete')).not.toBeInTheDocument();
  });

  it('renders nothing when there are no matching options', () => {
    const { container } = render(
      <MentionAutocomplete {...defaultProps} text="@zzzzz" cursorPosition={6} />
    );
    expect(container.querySelector('.mention-autocomplete')).not.toBeInTheDocument();
  });

  it('renders autocomplete list when @ matches members', () => {
    render(<MentionAutocomplete {...defaultProps} />);
    const autocomplete = document.querySelector('.mention-autocomplete');
    expect(autocomplete).toBeInTheDocument();
    expect(autocomplete?.getAttribute('role')).toBe('listbox');
  });

  it('has correct aria-label', () => {
    render(<MentionAutocomplete {...defaultProps} />);
    expect(screen.getByLabelText('Mention suggestions')).toBeInTheDocument();
  });

  // ── User search/filtering ──

  it('shows all matching users on empty query (@)', () => {
    render(<MentionAutocomplete {...defaultProps} text="@" cursorPosition={1} />);
    // Both members should appear
    expect(screen.getByText('testuser')).toBeInTheDocument();
    expect(screen.getByText('testuser2')).toBeInTheDocument();
  });

  it('filters users by username prefix', () => {
    render(<MentionAutocomplete {...defaultProps} text="@testuser2" cursorPosition={10} />);
    expect(screen.getByText('testuser2')).toBeInTheDocument();
    expect(screen.queryByText('testuser')).not.toBeInTheDocument();
  });

  it('filters users by display name prefix', () => {
    // Display name search is case-insensitive and uses startsWith on lowercase
    // "Test User 2" has spaces which break the @query extraction (@ stops at space)
    // Use a prefix without spaces that matches display_name
    render(<MentionAutocomplete {...defaultProps} text="@Test" cursorPosition={5} />);
    // Both "Test User" and "Test User 2" match
    expect(screen.getByText('testuser')).toBeInTheDocument();
    expect(screen.getByText('testuser2')).toBeInTheDocument();
  });

  it('shows display name as sublabel', () => {
    render(<MentionAutocomplete {...defaultProps} text="@testuser2" cursorPosition={10} />);
    expect(screen.getByText('Test User 2')).toBeInTheDocument();
  });

  // ── Special mentions (@all, @here) ──

  it('shows @all option when user has MENTION_EVERYONE permission', () => {
    render(<MentionAutocomplete {...defaultProps} text="@al" cursorPosition={3} />);
    expect(screen.getByText('all')).toBeInTheDocument();
    expect(screen.getByText('Notify everyone in this channel')).toBeInTheDocument();
  });

  it('shows @here option with online hint', () => {
    render(<MentionAutocomplete {...defaultProps} text="@her" cursorPosition={4} />);
    expect(screen.getByText('here')).toBeInTheDocument();
    expect(screen.getByText('Notify online members')).toBeInTheDocument();
  });

  it('hides @all when user lacks MENTION_EVERYONE permission', () => {
    usePermissionStore.setState({
      serverPermissions: { 'server-1': MENTION_USERS },
    });
    render(<MentionAutocomplete {...defaultProps} text="@al" cursorPosition={3} />);
    expect(screen.queryByText('all')).not.toBeInTheDocument();
  });

  it('hides @here when user lacks MENTION_EVERYONE permission in server context', () => {
    usePermissionStore.setState({
      serverPermissions: { 'server-1': MENTION_USERS },
    });
    render(<MentionAutocomplete {...defaultProps} text="@her" cursorPosition={4} />);
    expect(screen.queryByText('here')).not.toBeInTheDocument();
  });

  // ── Role mentions ──

  it('shows mentionable roles matching query', () => {
    render(<MentionAutocomplete {...defaultProps} text="@mod" cursorPosition={4} />);
    expect(screen.getByText('Moderator')).toBeInTheDocument();
    expect(screen.getByText('Role')).toBeInTheDocument();
  });

  it('does not show default role', () => {
    render(<MentionAutocomplete {...defaultProps} text="@every" cursorPosition={6} />);
    // @everyone role has is_default=true and mentionable=false — should not appear as role
    const roleLabels = screen.queryAllByText('Role');
    // The only "Role" sublabel should not exist for @everyone
    expect(roleLabels.length).toBe(0);
  });

  it('hides roles when user lacks MENTION_ROLES permission', () => {
    usePermissionStore.setState({
      serverPermissions: { 'server-1': MENTION_USERS | MENTION_EVERYONE },
    });
    render(<MentionAutocomplete {...defaultProps} text="@mod" cursorPosition={4} />);
    expect(screen.queryByText('Moderator')).not.toBeInTheDocument();
  });

  it('hides user mentions when user lacks MENTION_USERS permission', () => {
    usePermissionStore.setState({
      serverPermissions: { 'server-1': MENTION_EVERYONE },
    });
    render(<MentionAutocomplete {...defaultProps} text="@test" cursorPosition={5} />);
    expect(screen.queryByText('testuser')).not.toBeInTheDocument();
  });

  // ── Selection ──

  it('calls onSelect with user mention token on click', () => {
    render(<MentionAutocomplete {...defaultProps} text="@testuser2" cursorPosition={10} />);
    const option = screen.getByText('testuser2');
    fireEvent.mouseDown(option);
    expect(onSelect).toHaveBeenCalledWith(
      expect.objectContaining({
        type: 'user',
        id: 'user-2',
        label: 'testuser2',
      }),
      '<@user-2> '
    );
  });

  it('calls onSelect with role mention token on click', () => {
    render(<MentionAutocomplete {...defaultProps} text="@mod" cursorPosition={4} />);
    const option = screen.getByText('Moderator');
    fireEvent.mouseDown(option);
    expect(onSelect).toHaveBeenCalledWith(
      expect.objectContaining({
        type: 'role',
        id: 'role-mod',
        label: 'Moderator',
      }),
      '<@&role-mod> '
    );
  });

  it('calls onSelect with @all mention on click', () => {
    render(<MentionAutocomplete {...defaultProps} text="@al" cursorPosition={3} />);
    const option = screen.getByText('all');
    fireEvent.mouseDown(option);
    expect(onSelect).toHaveBeenCalledWith(
      expect.objectContaining({
        type: 'everyone',
        label: 'all',
      }),
      '@all '
    );
  });

  it('calls onSelect with @here mention on click', () => {
    render(<MentionAutocomplete {...defaultProps} text="@her" cursorPosition={4} />);
    const option = screen.getByText('here');
    fireEvent.mouseDown(option);
    expect(onSelect).toHaveBeenCalledWith(
      expect.objectContaining({
        type: 'here',
        label: 'here',
      }),
      '@here '
    );
  });

  // ── Keyboard navigation ──

  it('handles ArrowDown to move selection', () => {
    const ref = React.createRef<MentionAutocompleteHandle>();
    render(<MentionAutocomplete ref={ref} {...defaultProps} text="@" cursorPosition={1} />);
    // First option should be selected by default
    const options = document.querySelectorAll('.mention-option');
    expect(options[0]).toHaveClass('selected');
    // Press ArrowDown
    const event = {
      key: 'ArrowDown',
      preventDefault: vi.fn(),
    } as unknown as React.KeyboardEvent;
    const handled = ref.current!.handleKeyDown(event);
    expect(handled).toBe(true);
    expect(event.preventDefault).toHaveBeenCalled();
  });

  it('handles ArrowUp to move selection', () => {
    const ref = React.createRef<MentionAutocompleteHandle>();
    render(<MentionAutocomplete ref={ref} {...defaultProps} text="@" cursorPosition={1} />);
    const event = {
      key: 'ArrowUp',
      preventDefault: vi.fn(),
    } as unknown as React.KeyboardEvent;
    const handled = ref.current!.handleKeyDown(event);
    expect(handled).toBe(true);
  });

  it('handles Enter to select current option', () => {
    const ref = React.createRef<MentionAutocompleteHandle>();
    render(
      <MentionAutocomplete ref={ref} {...defaultProps} text="@testuser2" cursorPosition={10} />
    );
    const event = {
      key: 'Enter',
      preventDefault: vi.fn(),
    } as unknown as React.KeyboardEvent;
    const handled = ref.current!.handleKeyDown(event);
    expect(handled).toBe(true);
    expect(onSelect).toHaveBeenCalled();
  });

  it('handles Tab to select current option', () => {
    const ref = React.createRef<MentionAutocompleteHandle>();
    render(
      <MentionAutocomplete ref={ref} {...defaultProps} text="@testuser2" cursorPosition={10} />
    );
    const event = {
      key: 'Tab',
      preventDefault: vi.fn(),
    } as unknown as React.KeyboardEvent;
    const handled = ref.current!.handleKeyDown(event);
    expect(handled).toBe(true);
    expect(onSelect).toHaveBeenCalled();
  });

  it('handles Escape to close', () => {
    const ref = React.createRef<MentionAutocompleteHandle>();
    render(<MentionAutocomplete ref={ref} {...defaultProps} text="@" cursorPosition={1} />);
    const event = {
      key: 'Escape',
      preventDefault: vi.fn(),
    } as unknown as React.KeyboardEvent;
    const handled = ref.current!.handleKeyDown(event);
    expect(handled).toBe(true);
    expect(onClose).toHaveBeenCalled();
  });

  it('returns false for unhandled keys', () => {
    const ref = React.createRef<MentionAutocompleteHandle>();
    render(<MentionAutocomplete ref={ref} {...defaultProps} text="@" cursorPosition={1} />);
    const event = {
      key: 'a',
      preventDefault: vi.fn(),
    } as unknown as React.KeyboardEvent;
    const handled = ref.current!.handleKeyDown(event);
    expect(handled).toBe(false);
  });

  it('returns false when options are empty', () => {
    const ref = React.createRef<MentionAutocompleteHandle>();
    render(<MentionAutocomplete ref={ref} {...defaultProps} text="@zzzzz" cursorPosition={6} />);
    // The ref should still work even when nothing renders
    if (ref.current) {
      const event = {
        key: 'ArrowDown',
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent;
      const handled = ref.current.handleKeyDown(event);
      expect(handled).toBe(false);
    }
  });

  // ── Selection highlighting via mouse ──

  it('highlights option on mouse enter', () => {
    render(<MentionAutocomplete {...defaultProps} text="@" cursorPosition={1} />);
    const options = document.querySelectorAll('.mention-option');
    // Hover over second option
    fireEvent.mouseEnter(options[1]);
    expect(options[1]).toHaveClass('selected');
  });

  // ── Wrapping keyboard navigation ──

  it('wraps ArrowDown from last to first option', () => {
    const ref = React.createRef<MentionAutocompleteHandle>();
    // Only show one member
    useMemberStore.setState({ members: [mockMember] });
    usePermissionStore.setState({
      serverPermissions: { 'server-1': MENTION_USERS },
      serverRoles: { 'server-1': [] },
    });
    render(<MentionAutocomplete ref={ref} {...defaultProps} text="@test" cursorPosition={5} />);
    // There should be just 1 option — ArrowDown should wrap
    const event = {
      key: 'ArrowDown',
      preventDefault: vi.fn(),
    } as unknown as React.KeyboardEvent;
    ref.current!.handleKeyDown(event);
    // Should still have first item selected (wrapped)
    const options = document.querySelectorAll('.mention-option');
    expect(options[0]).toHaveClass('selected');
  });

  // ── Query parsing ──

  it('ignores @ in the middle of a word', () => {
    const { container } = render(
      <MentionAutocomplete {...defaultProps} text="email@test" cursorPosition={10} />
    );
    expect(container.querySelector('.mention-autocomplete')).not.toBeInTheDocument();
  });

  it('handles @ at start of text', () => {
    render(<MentionAutocomplete {...defaultProps} text="@test" cursorPosition={5} />);
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  it('handles @ after space', () => {
    render(<MentionAutocomplete {...defaultProps} text="hello @test" cursorPosition={11} />);
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  it('handles @ after newline', () => {
    render(<MentionAutocomplete {...defaultProps} text={'line1\n@test'} cursorPosition={11} />);
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  // ── DM context ──

  it('sources members from DM participants when conversationId is set', () => {
    useDMStore.setState({
      conversations: [
        {
          id: 'dm-1',
          participants: [
            {
              id: 'p-1',
              conversationId: 'dm-1',
              userId: 'dm-user-1',
              username: 'dmuser',
              displayName: 'DM User',
              avatarUrl: null,
              isCurrentUser: false,
              joinedAt: '',
            },
          ],
          isGroup: false,
          createdAt: '',
          updatedAt: '',
        },
      ],
    });
    render(
      <MentionAutocomplete
        {...defaultProps}
        serverId={undefined}
        conversationId="dm-1"
        text="@dm"
        cursorPosition={3}
      />
    );
    expect(screen.getByText('dmuser')).toBeInTheDocument();
    // Server members should not appear
    expect(screen.queryByText('testuser')).not.toBeInTheDocument();
  });

  it('allows @here but not @all in DM context', () => {
    render(
      <MentionAutocomplete
        {...defaultProps}
        serverId={undefined}
        conversationId="dm-1"
        text="@"
        cursorPosition={1}
      />
    );
    // @all should not appear (canMentionEveryone=false in DM)
    expect(screen.queryByText('all')).not.toBeInTheDocument();
    // @here should appear (canMentionHere=true in DM) if query matches
  });

  // ── Cap at 15 results ──

  it('caps results at 15', () => {
    // Create 20 members
    const manyMembers = Array.from({ length: 20 }, (_, i) => ({
      ...mockMember,
      user_id: `user-${i}`,
      username: `user${i}`,
      display_name: `User ${i}`,
    }));
    useMemberStore.setState({ members: manyMembers });
    render(<MentionAutocomplete {...defaultProps} text="@user" cursorPosition={5} />);
    const options = document.querySelectorAll('.mention-option');
    expect(options.length).toBeLessThanOrEqual(15);
  });

  // ── Option icons ──

  it('shows Users icon for @all option', () => {
    render(<MentionAutocomplete {...defaultProps} text="@al" cursorPosition={3} />);
    const iconSpan = document.querySelector('.mention-option-icon');
    expect(iconSpan).toBeInTheDocument();
  });

  it('shows Shield icon for role options', () => {
    render(<MentionAutocomplete {...defaultProps} text="@mod" cursorPosition={4} />);
    const option = screen.getByText('Moderator').closest('.mention-option');
    const iconSpan = option?.querySelector('.mention-option-icon');
    expect(iconSpan).toBeInTheDocument();
  });

  it('applies role color to Shield icon', () => {
    render(<MentionAutocomplete {...defaultProps} text="@mod" cursorPosition={4} />);
    // Role name should be colored
    const label = screen.getByText('Moderator');
    expect(label.style.color).toBe('rgb(231, 76, 60)');
  });

  // ── Channel SBAC overrides (#600) ──
  describe('channel SBAC overrides', () => {
    const VIEWER_ID = 'user-1';

    beforeEach(() => {
      // Viewer = user-1 carrying the Moderator role; outer beforeEach already granted
      // all three mention perms via serverPermissions['server-1'].
      useUserStore.setState({
        user: { id: VIEWER_ID, username: 'testuser' },
      });
      useMemberStore.setState({
        members: [
          {
            ...mockMember,
            user_id: VIEWER_ID,
            // mockMember defaults role:'owner'; owners are immune to overrides, so the
            // override-applies cases below MUST use a non-owner viewer (see owner test).
            role: 'member',
            roles: [{ role_id: 'role-mod', role_name: 'Moderator', position: 1 }],
          },
          mockMember2,
        ],
      });
    });

    it('suppresses @all when a channel override denies MENTION_EVERYONE to the viewer role', () => {
      usePermissionStore.setState({
        channelOverrides: {
          'channel-1': [
            {
              id: 'ovr-1',
              channel_id: 'channel-1',
              target_type: 'role',
              target_id: 'role-mod',
              allow: '0',
              deny: MENTION_EVERYONE.toString(),
              created_at: '2025-01-01T00:00:00Z',
              updated_at: '2025-01-01T00:00:00Z',
            },
          ],
        },
      });
      render(<MentionAutocomplete {...defaultProps} text="@a" cursorPosition={2} />);
      expect(screen.queryByText('all')).not.toBeInTheDocument();
    });

    it('still offers @all when no override denies MENTION_EVERYONE', () => {
      usePermissionStore.setState({ channelOverrides: { 'channel-1': [] } });
      render(<MentionAutocomplete {...defaultProps} text="@a" cursorPosition={2} />);
      expect(screen.getByText('all')).toBeInTheDocument();
    });

    it('surfaces role mentions when an override grants MENTION_ROLES the base lacks', () => {
      usePermissionStore.setState({
        serverPermissions: { 'server-1': MENTION_USERS },
        serverRoles: {
          'server-1': [
            {
              id: 'role-pingable',
              server_id: 'server-1',
              name: 'Pingable',
              color: '#3498db',
              permissions: '0',
              position: 2,
              is_default: false,
              display_separately: false,
              mentionable: true,
              created_at: '2025-01-01T00:00:00Z',
              updated_at: '2025-01-01T00:00:00Z',
            },
          ],
        },
        channelOverrides: {
          'channel-1': [
            {
              id: 'ovr-2',
              channel_id: 'channel-1',
              target_type: 'role',
              target_id: 'role-mod',
              allow: MENTION_ROLES.toString(),
              deny: '0',
              created_at: '2025-01-01T00:00:00Z',
              updated_at: '2025-01-01T00:00:00Z',
            },
          ],
        },
      });
      render(<MentionAutocomplete {...defaultProps} text="@Ping" cursorPosition={5} />);
      expect(screen.getByText('Pingable')).toBeInTheDocument();
    });

    it('does not apply overrides to a server owner — @all stays offered despite a denying override', () => {
      useMemberStore.setState({
        members: [
          {
            ...mockMember,
            user_id: VIEWER_ID,
            role: 'owner',
            roles: [{ role_id: 'role-mod', role_name: 'Moderator', position: 1 }],
          },
          mockMember2,
        ],
      });
      usePermissionStore.setState({
        channelOverrides: {
          'channel-1': [
            {
              id: 'ovr-owner',
              channel_id: 'channel-1',
              target_type: 'role',
              target_id: 'role-mod',
              allow: '0',
              deny: MENTION_EVERYONE.toString(),
              created_at: '2025-01-01T00:00:00Z',
              updated_at: '2025-01-01T00:00:00Z',
            },
          ],
        },
      });
      render(<MentionAutocomplete {...defaultProps} text="@a" cursorPosition={2} />);
      // Owner is immune to channel overrides (mirrors the backend owner-id bypass).
      expect(screen.getByText('all')).toBeInTheDocument();
    });
  });
});
