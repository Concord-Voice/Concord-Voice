import { render, screen, fireEvent } from '../../../test-utils';
import MessageAvatar from '@/renderer/components/Chat/MessageAvatar';
import { API_BASE } from '@/renderer/config';
import { mockMessage } from '../../../mocks/fixtures';
import { resetAllStores } from '../../../helpers/store-helpers';

// MessageAvatar is a pure trigger after #226 — it renders the avatar and calls
// onOpenProfile with a viewport position. The member-resolution + profile-card
// state it used to own now lives in useMessageProfileCard (see that hook's
// test for the resolution-precedence coverage).
describe('MessageAvatar', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders avatar image when avatar_url is present', () => {
    const messageWithAvatar = { ...mockMessage, avatar_url: 'https://example.com/avatar.png' };
    render(
      <MessageAvatar
        message={messageWithAvatar}
        showAvatar={true}
        senderColors={null}
        onOpenProfile={vi.fn()}
      />
    );
    const img = document.querySelector('.avatar-image') as HTMLImageElement;
    expect(img).toBeInTheDocument();
    expect(img.src).toBe('https://example.com/avatar.png');
  });

  it('absolutizes a relative /api/v1/media avatar_url against API_BASE (#1586)', () => {
    const messageRelative = { ...mockMessage, avatar_url: '/api/v1/media/avatars/abc' };
    render(
      <MessageAvatar
        message={messageRelative}
        showAvatar={true}
        senderColors={null}
        onOpenProfile={vi.fn()}
      />
    );
    const img = document.querySelector('.avatar-image') as HTMLImageElement;
    expect(img).toBeInTheDocument();
    expect(img.getAttribute('src')).toBe(`${API_BASE}/api/v1/media/avatars/abc`);
  });

  it('renders initials circle when no avatar_url', () => {
    render(
      <MessageAvatar
        message={mockMessage}
        showAvatar={true}
        senderColors={null}
        onOpenProfile={vi.fn()}
      />
    );
    const circle = document.querySelector('.avatar-circle');
    expect(circle).toBeInTheDocument();
    expect(circle!.textContent).toBe('T'); // "Test User" → "T"
  });

  it('applies sender gradient colors to avatar circle', () => {
    render(
      <MessageAvatar
        message={mockMessage}
        showAvatar={true}
        senderColors={{ gradient: 'linear-gradient(135deg, #ff0000, #00ff00)' }}
        onOpenProfile={vi.fn()}
      />
    );
    const circle = document.querySelector('.avatar-circle') as HTMLElement;
    expect(circle.style.background).toContain('linear-gradient');
  });

  it('renders gutter timestamp when showAvatar is false', () => {
    render(
      <MessageAvatar
        message={mockMessage}
        showAvatar={false}
        senderColors={null}
        onOpenProfile={vi.fn()}
      />
    );
    expect(document.querySelector('.message-gutter')).toBeInTheDocument();
    expect(document.querySelector('.message-avatar')).not.toBeInTheDocument();
  });

  it('uses username initial when no display_name', () => {
    const messageNoDisplay = {
      ...mockMessage,
      display_name: undefined as unknown as string,
      username: 'foobar',
    };
    render(
      <MessageAvatar
        message={messageNoDisplay}
        showAvatar={true}
        senderColors={null}
        onOpenProfile={vi.fn()}
      />
    );
    expect(document.querySelector('.avatar-circle')!.textContent).toBe('F');
  });

  it('calls onOpenProfile with the click position on avatar click', () => {
    const onOpenProfile = vi.fn();
    render(
      <MessageAvatar
        message={mockMessage}
        showAvatar={true}
        senderColors={null}
        onOpenProfile={onOpenProfile}
      />
    );
    fireEvent.click(screen.getByLabelText('View user profile'), { clientX: 42, clientY: 99 });
    expect(onOpenProfile).toHaveBeenCalledWith({ x: 42, y: 99 });
  });

  it('calls onOpenProfile from the element center on Enter keydown', () => {
    const onOpenProfile = vi.fn();
    render(
      <MessageAvatar
        message={mockMessage}
        showAvatar={true}
        senderColors={null}
        onOpenProfile={onOpenProfile}
      />
    );
    fireEvent.keyDown(screen.getByLabelText('View user profile'), { key: 'Enter' });
    // jsdom getBoundingClientRect returns zeros; assert the shape, not values.
    expect(onOpenProfile).toHaveBeenCalledWith(
      expect.objectContaining({ x: expect.any(Number), y: expect.any(Number) })
    );
  });

  it('does not call onOpenProfile when no avatar is shown', () => {
    const onOpenProfile = vi.fn();
    render(
      <MessageAvatar
        message={mockMessage}
        showAvatar={false}
        senderColors={null}
        onOpenProfile={onOpenProfile}
      />
    );
    expect(screen.queryByLabelText('View user profile')).not.toBeInTheDocument();
    expect(onOpenProfile).not.toHaveBeenCalled();
  });
});
