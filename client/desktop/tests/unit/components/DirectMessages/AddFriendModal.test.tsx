import { render, screen, fireEvent, act } from '../../../test-utils';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { usePrivacyStore } from '@/renderer/stores/privacyStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { vi } from 'vitest';

// Mock apiFetch — controllable per test
const mockApiFetch = vi.fn().mockResolvedValue({ ok: false });
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

// Mock CustomSelect
vi.mock('@/renderer/components/ui/CustomSelect', () => ({
  default: ({
    id,
    value,
    options,
    onChange,
  }: {
    id?: string;
    value: string;
    options: { value: string; label: string }[];
    onChange: (v: string) => void;
  }) => (
    <select
      data-testid={id || 'custom-select'}
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      {options.map((opt) => (
        <option key={opt.value} value={opt.value}>
          {opt.label}
        </option>
      ))}
    </select>
  ),
}));

import AddFriendModal from '@/renderer/components/DirectMessages/AddFriendModal';

describe('AddFriendModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useFriendStore.setState({
      friendCodes: [],
      previewFriendCode: vi
        .fn()
        .mockResolvedValue({ valid: true, username: 'alice', displayName: 'Alice' }),
      claimFriendCode: vi
        .fn()
        .mockResolvedValue({ status: 'pending', user: { username: 'alice' } }),
      generateFriendCode: vi.fn().mockResolvedValue({ code: 'ABCD1234' }),
      revokeFriendCode: vi.fn().mockResolvedValue(undefined),
      fetchFriendCodes: vi.fn().mockResolvedValue(undefined),
      searchUsers: vi.fn().mockResolvedValue([]),
      sendRequest: vi.fn().mockResolvedValue(undefined),
    });
    usePrivacyStore.setState({
      settings: {
        autoAcceptFriendCodes: false,
        searchable: true,
        searchableByEmail: false,
        searchableByPhone: false,
      },
    });
  });

  it('renders nothing when closed', () => {
    const { container } = render(<AddFriendModal isOpen={false} onClose={mockOnClose} />);
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders modal title when open', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const modalTitle = document.querySelector('.modal-title');
    expect(modalTitle?.textContent).toBe('Add Friend');
  });

  it('renders all sections', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('Add by Friend Code')).toBeInTheDocument();
    expect(screen.getByText('Share Your Friend Code')).toBeInTheDocument();
  });

  it('renders friend code input field', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByPlaceholderText('Enter 8-character code...')).toBeInTheDocument();
  });

  it('accepts friend code input', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'ABCD1234' } });
    expect(input.value).toBe('ABCD1234');
  });

  it('has maxLength of 8 on friend code input', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...');
    expect(input.getAttribute('maxLength')).toBe('8');
  });

  it('previews friend code when 8 characters entered', async () => {
    const mockPreview = vi.fn().mockResolvedValue({
      valid: true,
      username: 'alice',
      displayName: 'Alice',
    });
    useFriendStore.setState({ previewFriendCode: mockPreview });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...');
    fireEvent.change(input, { target: { value: 'ABCD1234' } });
    await vi.waitFor(() => {
      expect(mockPreview).toHaveBeenCalledWith('ABCD1234');
    });
  });

  it('shows preview with Send Friend Request button for valid code', async () => {
    const mockPreview = vi.fn().mockResolvedValue({
      valid: true,
      username: 'alice',
      displayName: 'Alice',
    });
    useFriendStore.setState({ previewFriendCode: mockPreview });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...');
    fireEvent.change(input, { target: { value: 'ABCD1234' } });
    await vi.waitFor(() => {
      expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
      expect(screen.getByText('Alice')).toBeInTheDocument();
    });
  });

  it('shows error for expired code', async () => {
    const mockPreview = vi.fn().mockResolvedValue({ valid: false });
    useFriendStore.setState({ previewFriendCode: mockPreview });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...');
    fireEvent.change(input, { target: { value: 'EXPIRED8' } });
    await vi.waitFor(() => {
      expect(screen.getByText('This code is expired or has been used')).toBeInTheDocument();
    });
  });

  it('claims friend code and shows success', async () => {
    const mockPreview = vi.fn().mockResolvedValue({
      valid: true,
      username: 'alice',
      displayName: 'Alice',
    });
    const mockClaim = vi.fn().mockResolvedValue({
      status: 'accepted',
      user: { username: 'alice' },
    });
    useFriendStore.setState({
      previewFriendCode: mockPreview,
      claimFriendCode: mockClaim,
    });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...');
    fireEvent.change(input, { target: { value: 'ABCD1234' } });
    await vi.waitFor(() => {
      expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Send Friend Request'));
    await vi.waitFor(() => {
      expect(screen.getByText('You are now friends with alice!')).toBeInTheDocument();
    });
  });

  it('renders search input', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(
      screen.getByPlaceholderText('Search by name or username (2+ characters)...')
    ).toBeInTheDocument();
  });

  it('does not search for queries less than 2 characters', () => {
    const mockSearch = vi.fn();
    useFriendStore.setState({ searchUsers: mockSearch });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Search by name or username (2+ characters)...');
    fireEvent.change(input, { target: { value: 'a' } });
    expect(mockSearch).not.toHaveBeenCalled();
    expect(screen.queryByText('No users found')).not.toBeInTheDocument();
  });

  it('searches for 2-character names', async () => {
    const mockSearch = vi
      .fn()
      .mockResolvedValue([{ id: 'u1', username: 'edie', displayName: 'Ed' }]);
    useFriendStore.setState({ searchUsers: mockSearch });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Search by name or username (2+ characters)...');
    fireEvent.change(input, { target: { value: 'ed' } });
    await vi.waitFor(() => {
      expect(mockSearch).toHaveBeenCalledWith('ed');
      expect(screen.getByText('Ed')).toBeInTheDocument();
    });
  });

  it('ignores stale search responses', async () => {
    const resolvers: Record<
      string,
      (results: Array<{ id: string; username: string; displayName: string }>) => void
    > = {};
    const mockSearch = vi.fn(
      (query: string) =>
        new Promise((resolve) => {
          resolvers[query] = resolve;
        })
    );
    useFriendStore.setState({ searchUsers: mockSearch });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Search by name or username (2+ characters)...');

    fireEvent.change(input, { target: { value: 'ed' } });
    await vi.waitFor(() => expect(mockSearch).toHaveBeenCalledWith('ed'));

    fireEvent.change(input, { target: { value: 'edi' } });
    await vi.waitFor(() => expect(mockSearch).toHaveBeenCalledWith('edi'));

    await act(async () => {
      resolvers.edi([{ id: 'u2', username: 'edie', displayName: 'Edie' }]);
    });
    expect(screen.getByText('Edie')).toBeInTheDocument();

    await act(async () => {
      resolvers.ed([]);
    });
    expect(screen.getByText('Edie')).toBeInTheDocument();
    expect(screen.queryByText('No users found')).not.toBeInTheDocument();
  });

  it('shows no-match state after a completed search with no results', async () => {
    const mockSearch = vi.fn().mockResolvedValue([]);
    useFriendStore.setState({ searchUsers: mockSearch });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Search by name or username (2+ characters)...');
    fireEvent.change(input, { target: { value: 'zz' } });
    await vi.waitFor(() => {
      expect(mockSearch).toHaveBeenCalledWith('zz');
      expect(screen.getByText('No users found')).toBeInTheDocument();
    });
  });

  it('shows search results after typing 2+ characters', async () => {
    const mockSearch = vi
      .fn()
      .mockResolvedValue([{ id: 'u1', username: 'charlie', displayName: 'Charlie' }]);
    useFriendStore.setState({ searchUsers: mockSearch });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Search by name or username (2+ characters)...');
    fireEvent.change(input, { target: { value: 'cha' } });
    await vi.waitFor(() => {
      expect(screen.getByText('Charlie')).toBeInTheDocument();
    });
  });

  it('renders Generate Code button', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('Generate Code')).toBeInTheDocument();
  });

  it('generates a friend code on Generate button click', async () => {
    const mockGenerate = vi.fn().mockResolvedValue({ code: 'NEWCODE1' });
    useFriendStore.setState({ generateFriendCode: mockGenerate });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Generate Code'));
    await vi.waitFor(() => {
      expect(mockGenerate).toHaveBeenCalled();
      expect(screen.getByText('NEWCODE1')).toBeInTheDocument();
    });
  });

  it('copies generated code to clipboard', async () => {
    const mockGenerate = vi.fn().mockResolvedValue({ code: 'COPYCODE' });
    useFriendStore.setState({ generateFriendCode: mockGenerate });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Generate Code'));
    await vi.waitFor(() => {
      expect(screen.getByText('COPYCODE')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Copy'));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith('COPYCODE');
  });

  it('renders expiry dropdown', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('Expires after')).toBeInTheDocument();
  });

  it('renders max uses dropdown', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('Max uses')).toBeInTheDocument();
  });

  it('renders auto-accept toggle', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('Auto-accept')).toBeInTheDocument();
  });

  it('renders active friend codes list', () => {
    useFriendStore.setState({
      friendCodes: [
        {
          id: 'fc-1',
          code: 'ACTIVE01',
          maxUses: 3,
          useCount: 1,
          expiresAt: null,
          autoAccept: false,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
      fetchFriendCodes: vi.fn().mockResolvedValue(undefined),
    });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('Active Codes')).toBeInTheDocument();
    expect(screen.getByText('ACTIVE01')).toBeInTheDocument();
    expect(screen.getByText('1/3 uses')).toBeInTheDocument();
  });

  it('revokes a friend code on revoke button click', async () => {
    const mockRevoke = vi.fn().mockResolvedValue(undefined);
    useFriendStore.setState({
      friendCodes: [
        {
          id: 'fc-3',
          code: 'REVOKE01',
          maxUses: 1,
          useCount: 0,
          expiresAt: null,
          autoAccept: false,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
      fetchFriendCodes: vi.fn().mockResolvedValue(undefined),
      revokeFriendCode: mockRevoke,
    });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const revokeBtn = document.querySelector('.add-friend-revoke-btn');
    expect(revokeBtn).toBeInTheDocument();
    fireEvent.click(revokeBtn!);
    await vi.waitFor(() => {
      expect(mockRevoke).toHaveBeenCalledWith('fc-3');
    });
  });

  it('does not show Active Codes section when no codes exist', () => {
    useFriendStore.setState({
      friendCodes: [],
      fetchFriendCodes: vi.fn().mockResolvedValue(undefined),
    });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.queryByText('Active Codes')).not.toBeInTheDocument();
  });

  it('fetches friend codes when modal opens', () => {
    const mockFetch = vi.fn().mockResolvedValue(undefined);
    useFriendStore.setState({ fetchFriendCodes: mockFetch });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    expect(mockFetch).toHaveBeenCalled();
  });

  it('shows server invite error when code is a valid server invite', async () => {
    const mockPreview = vi.fn().mockRejectedValue(new Error('Not found'));
    mockApiFetch.mockResolvedValue({ ok: true });
    useFriendStore.setState({ previewFriendCode: mockPreview });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...');
    fireEvent.change(input, { target: { value: 'SRVRINV1' } });
    await vi.waitFor(() => {
      expect(
        screen.getByText(
          'This looks like a server invite code, not a friend code. Use the Join Server button to use it.'
        )
      ).toBeInTheDocument();
    });
  });

  it('shows generic error when code is invalid and not a server invite', async () => {
    const mockPreview = vi.fn().mockRejectedValue(new Error('Code not found'));
    mockApiFetch.mockResolvedValue({ ok: false });
    useFriendStore.setState({ previewFriendCode: mockPreview });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...');
    fireEvent.change(input, { target: { value: 'INVALID1' } });
    await vi.waitFor(() => {
      expect(screen.getByText('Code not found')).toBeInTheDocument();
    });
  });

  it('shows fallback error when code throws non-Error and invite check also fails', async () => {
    const mockPreview = vi.fn().mockRejectedValue('unknown error');
    mockApiFetch.mockRejectedValue(new Error('network failure'));
    useFriendStore.setState({ previewFriendCode: mockPreview });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...');
    fireEvent.change(input, { target: { value: 'FAILCODE' } });
    await vi.waitFor(() => {
      expect(screen.getByText('Invalid code')).toBeInTheDocument();
    });
  });

  it('calls onClose when modal close is triggered', () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const closeBtn = document.querySelector('.modal-close-btn');
    if (closeBtn) {
      fireEvent.click(closeBtn);
      expect(mockOnClose).toHaveBeenCalled();
    }
  });

  it('shows pending status when claim result is pending', async () => {
    const mockPreview = vi.fn().mockResolvedValue({
      valid: true,
      username: 'bob',
      displayName: 'Bob',
    });
    const mockClaim = vi.fn().mockResolvedValue({
      status: 'pending',
      user: { username: 'bob' },
    });
    useFriendStore.setState({
      previewFriendCode: mockPreview,
      claimFriendCode: mockClaim,
    });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Enter 8-character code...');
    fireEvent.change(input, { target: { value: 'BOBCODE1' } });
    await vi.waitFor(() => {
      expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Send Friend Request'));
    await vi.waitFor(() => {
      expect(screen.getByText('Friend request sent to bob')).toBeInTheDocument();
    });
  });

  it('sends friend request from search results', async () => {
    const mockSearch = vi
      .fn()
      .mockResolvedValue([{ id: 'u1', username: 'diana', displayName: 'Diana' }]);
    const mockSendRequest = vi.fn().mockResolvedValue(undefined);
    useFriendStore.setState({ searchUsers: mockSearch, sendRequest: mockSendRequest });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const input = screen.getByPlaceholderText('Search by name or username (2+ characters)...');
    fireEvent.change(input, { target: { value: 'dia' } });
    await vi.waitFor(() => {
      expect(screen.getByText('Diana')).toBeInTheDocument();
    });
    const addBtn = screen.getByRole('button', { name: /Add Friend/i });
    fireEvent.click(addBtn);
    await vi.waitFor(() => {
      expect(mockSendRequest).toHaveBeenCalledWith('u1');
    });
  });

  it('logs redacted error when generateFriendCode fails', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useFriendStore.setState({
      generateFriendCode: vi.fn().mockRejectedValueOnce(new Error('boom')),
    });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);
    const generateBtn = screen.getByRole('button', { name: /Generate Code/i });
    fireEvent.click(generateBtn);
    await vi.waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to generate code:', 'boom');
    });
    consoleSpy.mockRestore();
  });
});

describe('AddFriendModal initialCode (#945)', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useFriendStore.setState({
      friendCodes: [],
      previewFriendCode: vi
        .fn()
        .mockResolvedValue({ valid: true, username: 'alice', displayName: 'Alice' }),
      claimFriendCode: vi
        .fn()
        .mockResolvedValue({ status: 'pending', user: { username: 'alice' } }),
      generateFriendCode: vi.fn().mockResolvedValue({ code: 'ABCD1234' }),
      revokeFriendCode: vi.fn().mockResolvedValue(undefined),
      fetchFriendCodes: vi.fn().mockResolvedValue(undefined),
      searchUsers: vi.fn().mockResolvedValue([]),
      sendRequest: vi.fn().mockResolvedValue(undefined),
    });
  });

  it('prefills and previews the code when opened with one', async () => {
    const mockPreview = vi
      .fn()
      .mockResolvedValue({ valid: true, username: 'alice', displayName: 'Alice' });
    useFriendStore.setState({ previewFriendCode: mockPreview });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} initialCode="AbCdEfGh" />);

    await vi.waitFor(() => {
      expect(screen.getByLabelText(/friend code/i)).toHaveValue('AbCdEfGh');
    });
    expect(mockPreview).toHaveBeenCalledWith('AbCdEfGh');
    await vi.waitFor(() => {
      expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
    });
  });

  it('focuses the code input and puts the caret at the end', async () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} initialCode="AbCdEfGh" />);

    await vi.waitFor(() => {
      expect(screen.getByLabelText(/friend code/i)).toHaveFocus();
    });
    const input = screen.getByLabelText(/friend code/i) as HTMLInputElement;
    expect(input.selectionStart).toBe(8);
    expect(input.selectionEnd).toBe(8);
  });

  it('does not auto-submit the prefilled code', async () => {
    const mockClaim = vi.fn().mockResolvedValue({ status: 'pending', user: { username: 'alice' } });
    useFriendStore.setState({ claimFriendCode: mockClaim });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} initialCode="AbCdEfGh" />);

    await vi.waitFor(() => {
      expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
    });
    expect(mockClaim).not.toHaveBeenCalled();
  });

  it('leaves the code input empty when no initialCode is supplied', () => {
    const mockPreview = vi.fn();
    useFriendStore.setState({ previewFriendCode: mockPreview });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} />);

    expect(screen.getByLabelText(/friend code/i)).toHaveValue('');
    expect(mockPreview).not.toHaveBeenCalled();
  });

  it('does not prefill while the modal is closed', () => {
    const mockPreview = vi.fn();
    useFriendStore.setState({ previewFriendCode: mockPreview });
    render(<AddFriendModal isOpen={false} onClose={mockOnClose} initialCode="AbCdEfGh" />);

    expect(mockPreview).not.toHaveBeenCalled();
  });

  it('announces the preview result to assistive technology', async () => {
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} initialCode="AbCdEfGh" />);

    await vi.waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent('Alice');
    });
  });

  it('announces a dead code as an alert', async () => {
    useFriendStore.setState({ previewFriendCode: vi.fn().mockResolvedValue({ valid: false }) });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} initialCode="DeAdC0dE" />);

    await vi.waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent('This code is expired or has been used');
    });
  });

  it('announces claim success to assistive technology', async () => {
    useFriendStore.setState({
      claimFriendCode: vi
        .fn()
        .mockResolvedValue({ status: 'accepted', user: { username: 'alice' } }),
    });
    render(<AddFriendModal isOpen={true} onClose={mockOnClose} initialCode="AbCdEfGh" />);

    await vi.waitFor(() => {
      expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Send Friend Request'));
    await vi.waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent('You are now friends with alice!');
    });
  });
});
