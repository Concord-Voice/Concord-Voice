import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import SearchPanel from '@/renderer/components/Chat/SearchPanel';
import { vi } from 'vitest';

// Mock useChannelSearch hook
const mockSearch = vi.fn();
const mockCancel = vi.fn();
let mockResults: Array<{
  id: string;
  content: string;
  username: string;
  display_name?: string;
  created_at: string;
  [key: string]: unknown;
}> = [];
let mockIsSearching = false;
let mockProgress: { checked: number; total: number | null } | null = null;

vi.mock('@/renderer/hooks/useChannelSearch', () => ({
  useChannelSearch: () => ({
    results: mockResults,
    isSearching: mockIsSearching,
    progress: mockProgress,
    search: mockSearch,
    cancel: mockCancel,
  }),
}));

describe('SearchPanel', () => {
  const defaultProps = {
    channelId: 'channel-1',
    isOpen: true,
    onClose: vi.fn(),
    onScrollToMessage: vi.fn(),
  };

  beforeEach(() => {
    vi.clearAllMocks();
    mockResults = [];
    mockIsSearching = false;
    mockProgress = null;
  });

  it('returns null when not open', () => {
    const { container } = render(<SearchPanel {...defaultProps} isOpen={false} />);
    expect(container.querySelector('.search-panel-backdrop')).not.toBeInTheDocument();
  });

  it('renders search input', () => {
    render(<SearchPanel {...defaultProps} />);
    expect(screen.getByPlaceholderText('Search messages...')).toBeInTheDocument();
  });

  it('calls search on input change', async () => {
    render(<SearchPanel {...defaultProps} />);
    const input = screen.getByPlaceholderText('Search messages...');
    fireEvent.change(input, { target: { value: 'hello' } });

    // Debounced — wait for it
    await waitFor(() => {
      expect(mockSearch).toHaveBeenCalledWith('hello');
    });
  });

  it('shows results when available', () => {
    mockResults = [
      {
        id: 'msg-1',
        content: 'Hello world deployment guide',
        username: 'testuser',
        display_name: 'Test User',
        created_at: '2025-01-01T12:00:00Z',
        channel_id: 'channel-1',
        user_id: 'user-1',
        updated_at: '2025-01-01T12:00:00Z',
      },
    ];

    render(<SearchPanel {...defaultProps} />);
    expect(screen.getByText('Test User')).toBeInTheDocument();
    expect(screen.getByText(/Hello world deployment guide/)).toBeInTheDocument();
  });

  it('Jump button calls onScrollToMessage and closes', () => {
    mockResults = [
      {
        id: 'msg-1',
        content: 'Test message',
        username: 'testuser',
        created_at: '2025-01-01T12:00:00Z',
        channel_id: 'channel-1',
        user_id: 'user-1',
        updated_at: '2025-01-01T12:00:00Z',
      },
    ];

    render(<SearchPanel {...defaultProps} />);
    fireEvent.click(screen.getByText('Jump'));
    expect(defaultProps.onScrollToMessage).toHaveBeenCalledWith('msg-1');
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('shows progress during search', () => {
    mockIsSearching = true;
    mockProgress = { checked: 400, total: null };

    render(<SearchPanel {...defaultProps} />);
    expect(screen.getByText(/400 messages checked/)).toBeInTheDocument();
  });

  it('shows empty state when no results found', () => {
    mockResults = [];
    mockIsSearching = false;

    render(<SearchPanel {...defaultProps} />);

    // Type a query first so the empty state shows
    const input = screen.getByPlaceholderText('Search messages...');
    fireEvent.change(input, { target: { value: 'nonexistent' } });

    expect(screen.getByText('No results found')).toBeInTheDocument();
  });

  it('closes on backdrop dismiss click', () => {
    render(<SearchPanel {...defaultProps} />);
    const dismiss = document.querySelector('.search-panel-backdrop-dismiss') as HTMLElement;
    fireEvent.click(dismiss);
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('shows server-wide toggle when showServerWideToggle is true', () => {
    render(<SearchPanel {...defaultProps} showServerWideToggle={true} />);
    expect(screen.getByText('Search all channels in this server')).toBeInTheDocument();
  });

  it('hides server-wide toggle when showServerWideToggle is false', () => {
    render(<SearchPanel {...defaultProps} showServerWideToggle={false} />);
    expect(screen.queryByText('Search all channels in this server')).not.toBeInTheDocument();
  });

  it('closes on close button click', () => {
    render(<SearchPanel {...defaultProps} />);
    fireEvent.click(screen.getByLabelText('Close'));
    expect(defaultProps.onClose).toHaveBeenCalled();
  });
});
