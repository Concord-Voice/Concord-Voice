import { render, screen } from '../../../test-utils';

// Mock FriendsList to prevent complex rendering
vi.mock('@/renderer/components/DirectMessages/FriendsList', () => ({
  default: () => <div data-testid="friends-list">Friends List</div>,
}));

// Mock useResizablePanel
vi.mock('@/renderer/hooks/useResizablePanel', () => ({
  useResizablePanel: () => ({
    width: 260,
    onMouseDown: vi.fn(),
    onKeyDown: vi.fn(),
  }),
}));

import FriendsFlexSpace from '@/renderer/components/DirectMessages/FriendsFlexSpace';

describe('FriendsFlexSpace', () => {
  it('renders friends list', () => {
    render(<FriendsFlexSpace />);
    expect(screen.getByTestId('friends-list')).toBeInTheDocument();
  });

  it('renders resize handle', () => {
    const { container } = render(<FriendsFlexSpace />);
    expect(container.querySelector('.layout-resize-handle')).toBeInTheDocument();
  });

  it('resize handle is keyboard-accessible with aria-label', () => {
    render(<FriendsFlexSpace />);
    const handle = screen.getByLabelText('Resize friends panel');
    expect(handle).toBeInTheDocument();
    expect(handle).toHaveAttribute('tabindex', '0');
  });
});
