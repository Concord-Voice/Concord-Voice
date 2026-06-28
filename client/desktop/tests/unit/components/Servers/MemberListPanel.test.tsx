import { render, screen, fireEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import MemberListPanel from '@/renderer/components/Servers/MemberListPanel';
import { vi } from 'vitest';
import type { ServerMember } from '@/renderer/stores/memberStore';
import type { Role } from '@/renderer/types/server';

// Mock heavy child dependencies so right-click surfaces a simple test double
vi.mock('@/renderer/components/Members/MemberContextMenu', () => ({
  default: ({
    member,
    onClose,
    onBan,
    onKick,
    onViewProfile,
  }: {
    member: ServerMember;
    onClose: () => void;
    onBan: (m: ServerMember) => void;
    onKick: (m: ServerMember) => void;
    onViewProfile: () => void;
  }) => (
    <div data-testid="ctx-menu">
      <span data-testid="ctx-member">{member.username}</span>
      <button onClick={onClose}>Close</button>
      <button data-testid="ctx-ban" onClick={() => onBan(member)}>
        Ban
      </button>
      <button data-testid="ctx-kick" onClick={() => onKick(member)}>
        Kick
      </button>
      <button data-testid="ctx-view-profile" onClick={onViewProfile}>
        View Profile
      </button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Members/UserProfileModal', () => ({
  default: ({ isOpen, member }: { isOpen: boolean; member: ServerMember }) =>
    isOpen ? <div data-testid="profile-modal">{member.username}</div> : null,
}));

vi.mock('@/renderer/components/ui/ConfirmActionModal', () => ({
  default: ({ isOpen, title }: { isOpen: boolean; title: string }) =>
    isOpen ? <div data-testid="confirm-modal">{title}</div> : null,
}));

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
  API_BASE: 'http://localhost:8080',
}));

const mockRoles: Role[] = [
  {
    id: 'role-default',
    server_id: 's1',
    name: '@everyone',
    color: '#99aab5',
    position: 0,
    permissions: '0',
    is_default: true,
    display_separately: false,
    mentionable: false,
    require_mfa: false,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  },
  {
    id: 'role-1',
    server_id: 's1',
    name: 'Moderator',
    color: '#ff0000',
    position: 1,
    permissions: '0',
    is_default: false,
    display_separately: false,
    mentionable: false,
    require_mfa: false,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  },
];

const assignableRoles = mockRoles.filter((r) => !r.is_default);

const mockMembers: ServerMember[] = [
  {
    user_id: 'u1',
    username: 'alice',
    display_name: 'Alice',
    role: 'member',
    joined_at: '2025-01-01T00:00:00Z',
    roles: [
      {
        role_id: 'role-1',
        role_name: 'Moderator',
        role_color: '#ff0000',
        position: 1,
      },
    ],
  },
  {
    user_id: 'u2',
    username: 'bob',
    role: 'member',
    joined_at: '2025-01-01T00:00:00Z',
    roles: [
      {
        role_id: 'role-default',
        role_name: '@everyone',
        role_color: '#99aab5',
        position: 0,
      },
    ],
  },
];

const defaultProps = {
  members: mockMembers,
  assignableRoles,
  onToggleRole: vi.fn(),
  serverId: 's1',
  ownerUserId: 'owner-1',
};

describe('MemberListPanel', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('renders column headers', () => {
    render(<MemberListPanel {...defaultProps} />);
    const header = document.querySelector('.member-list-header');
    expect(header).toBeInTheDocument();
    const spans = document.querySelectorAll('.member-list-header span');
    expect(spans[0]).toHaveTextContent('User');
    expect(spans[1]).toHaveTextContent('Roles');
  });

  it('renders member list with names and avatars', () => {
    render(<MemberListPanel {...defaultProps} />);
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.getByText('bob')).toBeInTheDocument();
    const avatarDivs = document.querySelectorAll('.member-avatar');
    expect(avatarDivs.length).toBe(2);
  });

  it('shows display_name when available, username otherwise', () => {
    render(<MemberListPanel {...defaultProps} />);
    const aliceName = screen.getByText('Alice');
    expect(aliceName).toHaveClass('member-name');
    const bobName = screen.getByText('bob');
    expect(bobName).toHaveClass('member-name');
  });

  it('shows @username below display_name', () => {
    render(<MemberListPanel {...defaultProps} />);
    expect(screen.getByText('@alice')).toBeInTheDocument();
    expect(screen.queryByText('@bob')).not.toBeInTheDocument();
  });

  it('shows roles inline as removable badges without needing to expand', () => {
    render(<MemberListPanel {...defaultProps} />);
    // Alice has Moderator — should be visible immediately as a removable badge
    const removableBadges = document.querySelectorAll('.role-badge--removable');
    expect(removableBadges.length).toBe(1);
    expect(removableBadges[0]).toHaveTextContent('Moderator');
  });

  it('shows empty state when no members', () => {
    render(<MemberListPanel {...defaultProps} members={[]} />);
    expect(screen.getByText('No members found.')).toBeInTheDocument();
  });

  it('shows Add Role button inline for members with unassigned roles', () => {
    render(<MemberListPanel {...defaultProps} />);
    // Bob has no assigned non-default roles -> Add Role button should be visible immediately
    const addBtns = document.querySelectorAll('.member-role-add-btn');
    expect(addBtns.length).toBeGreaterThan(0);
  });

  it('clicking Add Role and selecting a role calls onToggleRole(userId, roleId, false)', () => {
    render(<MemberListPanel {...defaultProps} />);
    // Bob's row should have Add Role button visible inline
    const bobRow = screen.getByText('bob').closest('.member-row')!;
    const addBtn = bobRow.querySelector('.member-role-add-btn');
    expect(addBtn).toBeInTheDocument();
    fireEvent.click(addBtn);

    // Dropdown should appear with Moderator
    const dropdownItem = document.querySelector('.member-role-dropdown__item');
    expect(dropdownItem).toHaveTextContent('Moderator');
    fireEvent.click(dropdownItem as HTMLElement);

    expect(defaultProps.onToggleRole).toHaveBeenCalledWith('u2', 'role-1', false);
  });

  it('Add Role dropdown renders via portal to document.body (#799 — escapes parent overflow)', () => {
    render(<MemberListPanel {...defaultProps} />);
    const bobRow = screen.getByText('bob').closest('.member-row')!;
    const addBtn = bobRow.querySelector('.member-role-add-btn') as HTMLElement;
    fireEvent.click(addBtn);

    const dropdown = document.querySelector('.member-role-dropdown--portal');
    expect(dropdown).toBeInTheDocument();
    // Portal target: dropdown must not be a descendant of the member-row that
    // owns the trigger button. If it WERE inside `.member-row`, parent's
    // `overflow: auto` would clip it — the bug this PR fixes.
    expect(bobRow.contains(dropdown)).toBe(false);
    expect(dropdown?.getAttribute('role')).toBe('menu');
    // Fixed positioning is what lets the portal sit above the modal chrome.
    expect((dropdown as HTMLElement).style.position).toBe('fixed');
  });

  it('Add Role dropdown closes on Escape (#799)', () => {
    render(<MemberListPanel {...defaultProps} />);
    const bobRow = screen.getByText('bob').closest('.member-row')!;
    const addBtn = bobRow.querySelector('.member-role-add-btn') as HTMLElement;
    fireEvent.click(addBtn);

    expect(document.querySelector('.member-role-dropdown--portal')).toBeInTheDocument();
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(document.querySelector('.member-role-dropdown--portal')).toBeNull();
  });

  it('Add Role dropdown closes when clicking outside (#799)', () => {
    render(<MemberListPanel {...defaultProps} />);
    const bobRow = screen.getByText('bob').closest('.member-row')!;
    const addBtn = bobRow.querySelector('.member-role-add-btn') as HTMLElement;
    fireEvent.click(addBtn);

    expect(document.querySelector('.member-role-dropdown--portal')).toBeInTheDocument();
    // Click on the body, NOT on the trigger or dropdown — should close.
    fireEvent.mouseDown(document.body);
    expect(document.querySelector('.member-role-dropdown--portal')).toBeNull();
  });

  it('Add Role dropdown closes on scroll (#799)', () => {
    render(<MemberListPanel {...defaultProps} />);
    const bobRow = screen.getByText('bob').closest('.member-row')!;
    const addBtn = bobRow.querySelector('.member-role-add-btn') as HTMLElement;
    fireEvent.click(addBtn);

    expect(document.querySelector('.member-role-dropdown--portal')).toBeInTheDocument();
    // Scroll on the document (capture-phase listener catches any ancestor scroll).
    fireEvent.scroll(document);
    expect(document.querySelector('.member-role-dropdown--portal')).toBeNull();
  });

  it('Add Role dropdown ignores clicks inside the dropdown itself (#799)', () => {
    render(<MemberListPanel {...defaultProps} />);
    const bobRow = screen.getByText('bob').closest('.member-row')!;
    const addBtn = bobRow.querySelector('.member-role-add-btn') as HTMLElement;
    fireEvent.click(addBtn);

    const dropdown = document.querySelector('.member-role-dropdown--portal') as HTMLElement;
    expect(dropdown).toBeInTheDocument();
    // mousedown inside the dropdown should NOT close it (only outside-clicks close).
    fireEvent.mouseDown(dropdown);
    expect(document.querySelector('.member-role-dropdown--portal')).toBeInTheDocument();
  });

  it('Add Role dropdown ignores clicks on the trigger button itself (#799 — toggle via onClick, not mousedown)', () => {
    render(<MemberListPanel {...defaultProps} />);
    const bobRow = screen.getByText('bob').closest('.member-row')!;
    const addBtn = bobRow.querySelector('.member-role-add-btn') as HTMLElement;
    fireEvent.click(addBtn);

    expect(document.querySelector('.member-role-dropdown--portal')).toBeInTheDocument();
    // The trigger's own click handler toggles the dropdown — the global mousedown
    // listener should ignore mousedown events on the trigger to avoid a stale-close
    // race between mousedown (close) and click (toggle-open).
    fireEvent.mouseDown(addBtn);
    expect(document.querySelector('.member-role-dropdown--portal')).toBeInTheDocument();
  });

  it('clicking Add Role twice toggles the dropdown closed (#799)', () => {
    render(<MemberListPanel {...defaultProps} />);
    const bobRow = screen.getByText('bob').closest('.member-row')!;
    const addBtn = bobRow.querySelector('.member-role-add-btn') as HTMLElement;
    fireEvent.click(addBtn);
    expect(document.querySelector('.member-role-dropdown--portal')).toBeInTheDocument();
    fireEvent.click(addBtn);
    expect(document.querySelector('.member-role-dropdown--portal')).toBeNull();
  });

  it('clicking remove button on a role badge calls onToggleRole(userId, roleId, true)', () => {
    render(<MemberListPanel {...defaultProps} />);
    // Alice's remove button is visible inline (no expand needed)
    const removeBtn = document.querySelector('.role-badge__remove');
    expect(removeBtn).toBeInTheDocument();
    fireEvent.click(removeBtn as HTMLElement);

    expect(defaultProps.onToggleRole).toHaveBeenCalledWith('u1', 'role-1', true);
  });

  it('shows "No roles available" when assignableRoles is empty', () => {
    render(<MemberListPanel {...defaultProps} assignableRoles={[]} />);
    // Each member row shows the message, so use getAllByText
    const msgs = screen.getAllByText('No roles available to assign.');
    expect(msgs.length).toBe(2);
  });

  it('renders avatar image when avatar_url is provided', () => {
    const memberWithAvatar: ServerMember[] = [
      {
        ...mockMembers[0],
        avatar_url: 'https://example.com/avatar.png',
      },
    ];
    render(<MemberListPanel {...defaultProps} members={memberWithAvatar} />);
    const img = document.querySelector('img.member-avatar') as HTMLImageElement;
    expect(img).toBeInTheDocument();
    expect(img.src).toBe('https://example.com/avatar.png');
  });

  it('does not have expand/collapse toggle buttons', () => {
    render(<MemberListPanel {...defaultProps} />);
    const toggles = document.querySelectorAll('.member-row-toggle');
    expect(toggles.length).toBe(0);
  });

  it('all member rows use grid layout with two cells', () => {
    render(<MemberListPanel {...defaultProps} />);
    const rows = document.querySelectorAll('.member-row');
    expect(rows.length).toBe(2);
    rows.forEach((row) => {
      expect(row.querySelector('.member-cell--user')).toBeInTheDocument();
      expect(row.querySelector('.member-cell--roles')).toBeInTheDocument();
    });
  });

  // ── Server enforcement badges ──

  it('shows Server Deafened badge when member is server_deafened', () => {
    const members: ServerMember[] = [
      {
        ...mockMembers[0],
        server_deafened: true,
        server_muted: false,
      },
    ];
    render(<MemberListPanel {...defaultProps} members={members} />);
    const badge = screen.getByTitle('Server Deafened');
    expect(badge).toBeInTheDocument();
    expect(badge).toHaveClass('member-enforcement-badge');
  });

  it('shows Server Muted badge when member is server_muted but not server_deafened', () => {
    const members: ServerMember[] = [
      {
        ...mockMembers[0],
        server_muted: true,
        server_deafened: false,
      },
    ];
    render(<MemberListPanel {...defaultProps} members={members} />);
    const badge = screen.getByTitle('Server Muted');
    expect(badge).toBeInTheDocument();
    expect(badge).toHaveClass('member-enforcement-badge');
  });

  it('shows no enforcement badge when both server_muted and server_deafened are false', () => {
    const members: ServerMember[] = [
      {
        ...mockMembers[0],
        server_muted: false,
        server_deafened: false,
      },
    ];
    render(<MemberListPanel {...defaultProps} members={members} />);
    expect(screen.queryByTitle('Server Muted')).not.toBeInTheDocument();
    expect(screen.queryByTitle('Server Deafened')).not.toBeInTheDocument();
  });

  it('shows Timed Out badge while timed_out_until is in the future', () => {
    const members: ServerMember[] = [
      {
        ...mockMembers[0],
        timed_out_until: '2999-01-01T00:00:00.000Z',
      },
    ];
    render(<MemberListPanel {...defaultProps} members={members} />);
    const badge = screen.getByTitle('Timed Out');
    expect(badge).toBeInTheDocument();
    expect(badge).toHaveClass('member-enforcement-badge--timeout');
  });

  // ── Context menu (right-click) ──

  it('right-click on a member row opens the context menu', () => {
    render(<MemberListPanel {...defaultProps} />);
    expect(screen.queryByTestId('ctx-menu')).not.toBeInTheDocument();
    const aliceRow = screen.getByText('Alice').closest('.member-row')!;
    fireEvent.contextMenu(aliceRow);
    expect(screen.getByTestId('ctx-menu')).toBeInTheDocument();
    expect(screen.getByTestId('ctx-member')).toHaveTextContent('alice');
  });

  it('context menu close button dismisses the menu', () => {
    render(<MemberListPanel {...defaultProps} />);
    const aliceRow = screen.getByText('Alice').closest('.member-row')!;
    fireEvent.contextMenu(aliceRow);
    expect(screen.getByTestId('ctx-menu')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Close'));
    expect(screen.queryByTestId('ctx-menu')).not.toBeInTheDocument();
  });

  it('clicking Ban in context menu opens the ban confirmation modal', () => {
    render(<MemberListPanel {...defaultProps} />);
    const aliceRow = screen.getByText('Alice').closest('.member-row')!;
    fireEvent.contextMenu(aliceRow);
    fireEvent.click(screen.getByTestId('ctx-ban'));
    expect(screen.getByTestId('confirm-modal')).toHaveTextContent('Ban Alice');
  });

  it('clicking Kick in context menu opens the kick confirmation modal', () => {
    render(<MemberListPanel {...defaultProps} />);
    const aliceRow = screen.getByText('Alice').closest('.member-row')!;
    fireEvent.contextMenu(aliceRow);
    fireEvent.click(screen.getByTestId('ctx-kick'));
    expect(screen.getByTestId('confirm-modal')).toHaveTextContent('Kick Alice');
  });

  it('clicking View Profile in context menu opens the profile modal', () => {
    render(<MemberListPanel {...defaultProps} />);
    const aliceRow = screen.getByText('Alice').closest('.member-row')!;
    fireEvent.contextMenu(aliceRow);
    fireEvent.click(screen.getByTestId('ctx-view-profile'));
    expect(screen.getByTestId('profile-modal')).toHaveTextContent('alice');
  });

  it('right-click on a different member updates the context menu target', () => {
    render(<MemberListPanel {...defaultProps} />);
    fireEvent.contextMenu(screen.getByText('Alice').closest('.member-row')!);
    expect(screen.getByTestId('ctx-member')).toHaveTextContent('alice');
    fireEvent.contextMenu(screen.getByText('bob').closest('.member-row')!);
    expect(screen.getByTestId('ctx-member')).toHaveTextContent('bob');
  });

  // ── Keyboard accessibility (member-row-kbd-trigger button) ──

  it('Enter key on the kbd-trigger button opens the context menu', () => {
    render(<MemberListPanel {...defaultProps} />);
    expect(screen.queryByTestId('ctx-menu')).not.toBeInTheDocument();
    const triggers = screen.getAllByRole('button', { name: /Open context menu for Alice/i });
    fireEvent.keyDown(triggers[0], { key: 'Enter' });
    expect(screen.getByTestId('ctx-menu')).toBeInTheDocument();
    expect(screen.getByTestId('ctx-member')).toHaveTextContent('alice');
  });

  it('Space key on the kbd-trigger button opens the context menu', () => {
    render(<MemberListPanel {...defaultProps} />);
    const triggers = screen.getAllByRole('button', { name: /Open context menu for Alice/i });
    fireEvent.keyDown(triggers[0], { key: ' ' });
    expect(screen.getByTestId('ctx-menu')).toBeInTheDocument();
  });

  it('non-activating key on the kbd-trigger button does not open the context menu', () => {
    render(<MemberListPanel {...defaultProps} />);
    const triggers = screen.getAllByRole('button', { name: /Open context menu for Alice/i });
    fireEvent.keyDown(triggers[0], { key: 'Tab' });
    expect(screen.queryByTestId('ctx-menu')).not.toBeInTheDocument();
  });

  it('clicking the kbd-trigger button opens the context menu', () => {
    render(<MemberListPanel {...defaultProps} />);
    const triggers = screen.getAllByRole('button', { name: /Open context menu for Alice/i });
    fireEvent.click(triggers[0]);
    expect(screen.getByTestId('ctx-menu')).toBeInTheDocument();
  });

  it('shows only deafened badge when both server_muted and server_deafened are true', () => {
    const members: ServerMember[] = [
      {
        ...mockMembers[0],
        server_muted: true,
        server_deafened: true,
      },
    ];
    render(<MemberListPanel {...defaultProps} members={members} />);
    expect(screen.getByTitle('Server Deafened')).toBeInTheDocument();
    expect(screen.queryByTitle('Server Muted')).not.toBeInTheDocument();
  });
});
