import { render, screen, fireEvent } from '../../../test-utils';
import MemberItem from '@/renderer/components/Members/MemberItem';
import { vi } from 'vitest';
import type { ServerMember, PresenceStatus } from '@/renderer/stores/memberStore';
import { useRichPresenceStore } from '@/renderer/stores/richPresenceStore';

vi.mock('@/renderer/utils/schemeColors', () => ({
  resolveUserAccentColors: vi.fn(() => null),
}));

function makeMember(overrides: Partial<ServerMember> = {}): ServerMember {
  return {
    user_id: 'u1',
    username: 'testuser',
    role: 'member' as const,
    joined_at: '2025-01-01T00:00:00Z',
    roles: [],
    ...overrides,
  };
}

const defaultProps = {
  member: makeMember(),
  status: 'online' as PresenceStatus,
  onClick: vi.fn(),
  onContextMenu: vi.fn(),
};

describe('MemberItem', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useRichPresenceStore.getState().reset();
  });

  it('renders as a button element', () => {
    render(<MemberItem {...defaultProps} />);
    expect(screen.getByRole('button')).toBeInTheDocument();
  });

  it('displays member username', () => {
    render(<MemberItem {...defaultProps} />);
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  it('displays display_name when available', () => {
    const member = makeMember({ display_name: 'Test Display' });
    render(<MemberItem {...defaultProps} member={member} />);
    expect(screen.getByText('Test Display')).toBeInTheDocument();
  });

  it('calls onClick when clicked', () => {
    const onClick = vi.fn();
    render(<MemberItem {...defaultProps} onClick={onClick} />);
    fireEvent.click(screen.getByRole('button'));
    expect(onClick).toHaveBeenCalledTimes(1);
    expect(onClick).toHaveBeenCalledWith(expect.any(Object), defaultProps.member);
  });

  it('applies offline class for offline status', () => {
    render(<MemberItem {...defaultProps} status="offline" />);
    const button = screen.getByRole('button');
    expect(button).toHaveClass('member-item', 'offline');
  });

  it('shows avatar image when avatar_url is set', () => {
    const member = makeMember({ avatar_url: 'https://example.com/avatar.png' });
    render(<MemberItem {...defaultProps} member={member} />);
    const img = screen.getByRole('img');
    expect(img).toHaveAttribute('src', 'https://example.com/avatar.png');
    expect(img).toHaveAttribute('alt', 'testuser');
  });

  it('shows initial letter when no avatar_url', () => {
    render(<MemberItem {...defaultProps} />);
    expect(screen.getByText('T')).toBeInTheDocument();
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
  });

  it('is keyboard focusable', () => {
    render(<MemberItem {...defaultProps} />);
    const button = screen.getByRole('button');
    button.focus();
    expect(button).toHaveFocus();
    // Verify no tabIndex={-1} which would prevent keyboard focus
    expect(button.getAttribute('tabindex')).not.toBe('-1');
  });

  it('stops propagation on mouseDown', () => {
    render(<MemberItem {...defaultProps} />);
    const button = screen.getByRole('button');
    const event = new MouseEvent('mousedown', { bubbles: true });
    const spy = vi.spyOn(event, 'stopPropagation');
    button.dispatchEvent(event);
    expect(spy).toHaveBeenCalled();
  });

  it('applies role color when member has display role', () => {
    const member = makeMember({
      roles: [
        { id: 'r1', name: 'Admin', role_color: '#ff0000', position: 10, display_separately: true },
        { id: 'r2', name: 'Mod', role_color: '#00ff00', position: 5, display_separately: true },
      ],
    });
    render(<MemberItem {...defaultProps} member={member} />);
    const username = screen.getByText('testuser');
    expect(username).toHaveStyle({ color: '#ff0000' }); // highest position role
  });

  it('does not apply role color when no display roles', () => {
    const member = makeMember({
      roles: [
        {
          id: 'r1',
          name: 'Everyone',
          role_color: '#ff0000',
          position: 0,
          display_separately: false,
        },
      ],
    });
    render(<MemberItem {...defaultProps} member={member} />);
    const username = screen.getByText('testuser');
    expect(username.style.color).toBe('');
  });

  it('calls onContextMenu on right-click', () => {
    const onContextMenu = vi.fn();
    render(<MemberItem {...defaultProps} onContextMenu={onContextMenu} />);
    fireEvent.contextMenu(screen.getByRole('button'));
    expect(onContextMenu).toHaveBeenCalledTimes(1);
  });

  it('renders custom-text status (emoji + text) when present in the store (#1233)', () => {
    useRichPresenceStore.getState().setCustomText('u1', { emoji: '🎮', text: 'Gaming' });
    render(<MemberItem {...defaultProps} member={makeMember({ user_id: 'u1' })} />);
    expect(screen.getByText('Gaming')).toBeInTheDocument();
    expect(screen.getByText('🎮')).toBeInTheDocument();
  });

  it('renders custom-text status without an emoji when none is set (#1233)', () => {
    useRichPresenceStore.getState().setCustomText('u1', { text: 'Focusing' });
    const { container } = render(
      <MemberItem {...defaultProps} member={makeMember({ user_id: 'u1' })} />
    );
    expect(screen.getByText('Focusing')).toBeInTheDocument();
    expect(container.querySelector('.member-custom-status-emoji')).not.toBeInTheDocument();
  });

  it('renders nothing for custom status when the store has no entry (#1233)', () => {
    const { container } = render(
      <MemberItem {...defaultProps} member={makeMember({ user_id: 'u1' })} />
    );
    expect(container.querySelector('.member-custom-status')).not.toBeInTheDocument();
  });
});
