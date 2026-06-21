import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import ChannelSwitcher from '@/renderer/components/ui/ChannelSwitcher';

vi.mock('@/renderer/components/ui/ChannelSwitcher.css', () => ({}));

const mockNavigate = vi.fn();
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom');
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockCloseChannelSwitcher = vi.fn();
let mockChannelSwitcherOpen = true;

vi.mock('@/renderer/stores/keyboardShortcutStore', () => ({
  useKeyboardShortcutStore: (selector: (s: Record<string, unknown>) => unknown) =>
    selector({
      channelSwitcherOpen: mockChannelSwitcherOpen,
      closeChannelSwitcher: mockCloseChannelSwitcher,
    }),
}));

const mockSetActiveChannel = vi.fn();

vi.mock('@/renderer/stores/channelStore', () => {
  const store = {
    channels: [
      { id: 'ch-1', name: 'general', type: 'text' },
      { id: 'ch-2', name: 'secret', type: 'text' },
      { id: 'ch-3', name: 'voice-room', type: 'voice' },
    ],
    setActiveChannel: (...args: unknown[]) => mockSetActiveChannel(...args),
    currentServerId: 'srv-1',
  };
  return {
    useChannelStore: Object.assign((selector: (s: typeof store) => unknown) => selector(store), {
      getState: () => store,
    }),
  };
});

const mockSetActiveConversation = vi.fn();

vi.mock('@/renderer/stores/dmStore', () => ({
  useDMStore: (selector: (s: Record<string, unknown>) => unknown) =>
    selector({
      conversations: [
        {
          id: 'dm-1',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [
            { userId: 'me', username: 'myself', displayName: 'Me' },
            { userId: 'u2', username: 'alice', displayName: 'Alice' },
          ],
        },
        {
          id: 'dm-2',
          isGroup: true,
          isPersonal: false,
          name: 'Project Chat',
          participants: [
            { userId: 'me', username: 'myself' },
            { userId: 'u3', username: 'bob' },
          ],
        },
        {
          id: 'dm-personal',
          isGroup: false,
          isPersonal: true,
          name: null,
          participants: [{ userId: 'me', username: 'myself' }],
        },
      ],
      setActiveConversation: (...args: unknown[]) => mockSetActiveConversation(...args),
    }),
}));

vi.mock('@/renderer/stores/serverStore', () => ({
  useServerStore: (selector: (s: Record<string, unknown>) => unknown) =>
    selector({
      servers: [{ id: 'srv-1', name: 'Test Server' }],
    }),
}));

vi.mock('@/renderer/stores/userStore', () => ({
  useUserStore: (selector: (s: Record<string, unknown>) => unknown) =>
    selector({ user: { id: 'me' } }),
}));

describe('ChannelSwitcher', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockChannelSwitcherOpen = true;
  });

  it('returns null when channelSwitcherOpen is false', () => {
    mockChannelSwitcherOpen = false;
    const { container } = render(<ChannelSwitcher />);
    expect(container.innerHTML).toBe('');
  });

  it('renders search input when open', () => {
    render(<ChannelSwitcher />);
    expect(screen.getByPlaceholderText('Search channels and DMs...')).toBeInTheDocument();
  });

  it('renders channel items from channelStore', () => {
    render(<ChannelSwitcher />);
    // text channels should appear; voice channel should not
    expect(screen.getByText('general')).toBeInTheDocument();
    expect(screen.getByText('secret')).toBeInTheDocument();
    expect(screen.queryByText('voice-room')).not.toBeInTheDocument();
  });

  it('renders DM items from dmStore', () => {
    render(<ChannelSwitcher />);
    // 1:1 DM shows other participant's displayName
    expect(screen.getByText('Alice')).toBeInTheDocument();
    // Group DM shows its name
    expect(screen.getByText('Project Chat')).toBeInTheDocument();
    // Personal DM is excluded
    expect(screen.queryByText('myself')).not.toBeInTheDocument();
  });

  it('typing filters the list', () => {
    render(<ChannelSwitcher />);
    const input = screen.getByPlaceholderText('Search channels and DMs...');
    fireEvent.change(input, { target: { value: 'gen' } });

    expect(screen.getByText('general')).toBeInTheDocument();
    expect(screen.queryByText('secret')).not.toBeInTheDocument();
    expect(screen.queryByText('Alice')).not.toBeInTheDocument();
  });

  it('shows "No results found" when nothing matches', () => {
    render(<ChannelSwitcher />);
    const input = screen.getByPlaceholderText('Search channels and DMs...');
    fireEvent.change(input, { target: { value: 'zzzznonexistent' } });

    expect(screen.getByText('No results found')).toBeInTheDocument();
  });

  it('first item has selected class by default', () => {
    render(<ChannelSwitcher />);
    const items = document.querySelectorAll('.channel-switcher-item');
    expect(items[0]).toHaveClass('selected');
    expect(items[1]).not.toHaveClass('selected');
  });

  it('Enter selects channel and calls setActiveChannel + closeSwitcher', () => {
    render(<ChannelSwitcher />);
    const input = screen.getByPlaceholderText('Search channels and DMs...');

    // First item is 'general' (a channel) -- press Enter
    fireEvent.keyDown(input, { key: 'Enter' });

    expect(mockSetActiveChannel).toHaveBeenCalledWith('ch-1');
    expect(mockNavigate).toHaveBeenCalledWith('/app');
    expect(mockCloseChannelSwitcher).toHaveBeenCalled();
  });

  it('Escape closes without action', () => {
    render(<ChannelSwitcher />);
    const input = screen.getByPlaceholderText('Search channels and DMs...');

    fireEvent.keyDown(input, { key: 'Escape' });

    expect(mockCloseChannelSwitcher).toHaveBeenCalled();
    expect(mockSetActiveChannel).not.toHaveBeenCalled();
    expect(mockSetActiveConversation).not.toHaveBeenCalled();
  });
});
