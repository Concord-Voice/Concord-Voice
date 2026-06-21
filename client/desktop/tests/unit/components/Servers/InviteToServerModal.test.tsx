import { render, screen, fireEvent } from '../../../test-utils';
import { useInviteStore } from '@/renderer/stores/inviteStore';
import { mockServer } from '../../../mocks/fixtures';
import type { ServerWithRole } from '@/renderer/types/server';
import { vi } from 'vitest';

// Mock LoadingSpinner
vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({
  default: ({ size, inline }: { size?: string; inline?: boolean }) => (
    <div data-testid="loading-spinner" data-size={size} data-inline={inline}>
      Loading...
    </div>
  ),
}));

// Mock CustomSelect
vi.mock('@/renderer/components/ui/CustomSelect', () => ({
  default: ({
    id,
    options,
    value,
    onChange,
    disabled,
    className,
  }: {
    id?: string;
    options: { value: string; label: string }[];
    value: string;
    onChange: (v: string) => void;
    disabled?: boolean;
    className?: string;
  }) => (
    <select
      id={id}
      data-testid={id}
      className={className}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={disabled}
    >
      {options.map((opt) => (
        <option key={opt.value} value={opt.value}>
          {opt.label}
        </option>
      ))}
    </select>
  ),
}));

import InviteToServerModal from '@/renderer/components/Servers/InviteToServerModal';

describe('InviteToServerModal', () => {
  const mockOnClose = vi.fn();
  const ownerServer: ServerWithRole = { ...mockServer, role: 'owner' };

  beforeEach(() => {
    vi.clearAllMocks();
    useInviteStore.setState({
      invites: {},
      isLoading: false,
      error: null,
      fetchInvites: vi.fn().mockResolvedValue(undefined),
      createInvite: vi.fn().mockResolvedValue(null),
    });
    // Mock clipboard
    Object.assign(navigator, {
      clipboard: {
        writeText: vi.fn().mockResolvedValue(undefined),
      },
    });
  });

  // ── Basic Rendering ──

  it('renders nothing when closed', () => {
    const { container } = render(
      <InviteToServerModal isOpen={false} server={ownerServer} onClose={mockOnClose} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders modal title when open', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.getByText('Invite to Server')).toBeInTheDocument();
  });

  it('renders server name', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.getByText('Test Server')).toBeInTheDocument();
  });

  it('shows server initial when no icon', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    const icon = document.querySelector('.invite-modal-server-icon span');
    expect(icon).toBeInTheDocument();
    expect(icon?.textContent).toBe('T');
  });

  it('shows server icon when icon_url exists', () => {
    const serverWithIcon = { ...ownerServer, icon_url: 'https://example.com/icon.png' };
    render(<InviteToServerModal isOpen={true} server={serverWithIcon} onClose={mockOnClose} />);
    const img = document.querySelector('.invite-modal-server-icon img');
    expect(img).toBeInTheDocument();
    expect(img).toHaveAttribute('src', 'https://example.com/icon.png');
  });

  // ── Section titles and descriptions ──

  it('shows Generate Invite Code section', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    // Section title (h4) and button both say "Generate Invite Code"
    const matches = screen.getAllByText('Generate Invite Code');
    expect(matches.length).toBeGreaterThanOrEqual(2);
    expect(
      screen.getByText('Create a code that others can use to join this server.')
    ).toBeInTheDocument();
  });

  // ── Options (expiry and max uses) ──

  it('renders expiry selector', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.getByText('Expires after')).toBeInTheDocument();
    const expirySelect = screen.getByTestId('invite-expires');
    expect(expirySelect).toBeInTheDocument();
  });

  it('renders max uses selector', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.getByText('Max uses')).toBeInTheDocument();
    const maxUsesSelect = screen.getByTestId('invite-max-uses');
    expect(maxUsesSelect).toBeInTheDocument();
  });

  it('defaults expiry to 1 day (86400)', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    const expirySelect = screen.getByTestId('invite-expires') as HTMLSelectElement;
    expect(expirySelect.value).toBe('86400');
  });

  it('defaults max uses to No limit (0)', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    const maxUsesSelect = screen.getByTestId('invite-max-uses') as HTMLSelectElement;
    expect(maxUsesSelect.value).toBe('0');
  });

  it('changes expiry option', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    const expirySelect = screen.getByTestId('invite-expires');
    fireEvent.change(expirySelect, { target: { value: '3600' } });
    expect((expirySelect as HTMLSelectElement).value).toBe('3600');
  });

  it('changes max uses option', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    const maxUsesSelect = screen.getByTestId('invite-max-uses');
    fireEvent.change(maxUsesSelect, { target: { value: '10' } });
    expect((maxUsesSelect as HTMLSelectElement).value).toBe('10');
  });

  // ── Generate button ──

  it('renders Generate Invite Code button', () => {
    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    const btn = document.querySelector('.invite-generate-btn');
    expect(btn).toBeInTheDocument();
    expect(btn?.textContent).toContain('Generate Invite Code');
  });

  it('calls createInvite with options on generate click', async () => {
    const mockCreateInvite = vi.fn().mockResolvedValue({
      id: 'inv-1',
      code: 'NEWCODE',
      server_id: 'server-1',
    });
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    fireEvent.click(document.querySelector('.invite-generate-btn')!);

    await vi.waitFor(() => {
      expect(mockCreateInvite).toHaveBeenCalledWith('server-1', {
        expires_in: 86400,
        max_uses: 0,
      });
    });
  });

  it('shows generated code after successful creation', async () => {
    const mockCreateInvite = vi.fn().mockResolvedValue({
      id: 'inv-1',
      code: 'TESTCODE123',
      server_id: 'server-1',
    });
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    fireEvent.click(document.querySelector('.invite-generate-btn')!);

    await vi.waitFor(() => {
      const codeInput = document.querySelector('.invite-code-value') as HTMLInputElement;
      expect(codeInput).toBeInTheDocument();
      expect(codeInput.value).toBe('TESTCODE123');
    });
  });

  it('auto-copies code to clipboard on generation', async () => {
    const mockCreateInvite = vi.fn().mockResolvedValue({
      id: 'inv-1',
      code: 'AUTOCOPY',
      server_id: 'server-1',
    });
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    fireEvent.click(document.querySelector('.invite-generate-btn')!);

    await vi.waitFor(() => {
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('AUTOCOPY');
    });
  });

  it('shows "Copied!" after auto-copy', async () => {
    const mockCreateInvite = vi.fn().mockResolvedValue({
      id: 'inv-1',
      code: 'COPYTEST',
      server_id: 'server-1',
    });
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    fireEvent.click(document.querySelector('.invite-generate-btn')!);

    await vi.waitFor(() => {
      expect(screen.getByText('Copied!')).toBeInTheDocument();
    });
  });

  it('shows error when createInvite returns null', async () => {
    const mockCreateInvite = vi.fn().mockResolvedValue(null);
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    fireEvent.click(document.querySelector('.invite-generate-btn')!);

    await vi.waitFor(() => {
      expect(screen.getByText('Failed to create invite')).toBeInTheDocument();
    });
  });

  it('shows error when createInvite throws', async () => {
    const mockCreateInvite = vi.fn().mockRejectedValue(new Error('Network error'));
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    fireEvent.click(document.querySelector('.invite-generate-btn')!);

    await vi.waitFor(() => {
      expect(screen.getByText('Failed to create invite')).toBeInTheDocument();
    });
  });

  it('shows "Generate New Code" after first code is generated', async () => {
    const mockCreateInvite = vi.fn().mockResolvedValue({
      id: 'inv-1',
      code: 'FIRSTCODE',
      server_id: 'server-1',
    });
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    fireEvent.click(document.querySelector('.invite-generate-btn')!);

    await vi.waitFor(() => {
      expect(screen.getByText('Generate New Code')).toBeInTheDocument();
    });
  });

  it('disables generate button while generating', async () => {
    // Create a promise that we control resolution of
    let resolveInvite: (value: unknown) => void;
    const mockCreateInvite = vi.fn().mockReturnValue(
      new Promise((resolve) => {
        resolveInvite = resolve;
      })
    );
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    const generateBtn = document.querySelector('.invite-generate-btn')!;
    fireEvent.click(generateBtn);

    // Button should show generating state
    await vi.waitFor(() => {
      expect(screen.getByText('Generating...')).toBeInTheDocument();
    });

    // Resolve
    resolveInvite!({ id: 'inv-1', code: 'DONE', server_id: 'server-1' });
    await vi.waitFor(() => {
      expect(screen.queryByText('Generating...')).not.toBeInTheDocument();
    });
  });

  // ── Copy button ──

  it('copy button copies generated code', async () => {
    const mockCreateInvite = vi.fn().mockResolvedValue({
      id: 'inv-1',
      code: 'COPYCODE',
      server_id: 'server-1',
    });
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    fireEvent.click(document.querySelector('.invite-generate-btn')!);

    await vi.waitFor(() => {
      const codeInput = document.querySelector('.invite-code-value') as HTMLInputElement;
      expect(codeInput).toBeInTheDocument();
      expect(codeInput.value).toBe('COPYCODE');
    });

    // Clear previous clipboard calls
    (navigator.clipboard.writeText as ReturnType<typeof vi.fn>).mockClear();

    // Click copy button
    const copyBtn = document.querySelector('.invite-code-copy-btn')!;
    fireEvent.click(copyBtn);

    await vi.waitFor(() => {
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('COPYCODE');
    });
  });

  // ── Existing invites ──

  it('shows active invites section when invites exist', () => {
    const futureDate = new Date(Date.now() + 86400000).toISOString();
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'EXISTINGCODE',
            created_by: 'user-1',
            max_uses: 10,
            use_count: 3,
            is_revoked: false,
            expires_at: futureDate,
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.getByText('Active Invites')).toBeInTheDocument();
    expect(screen.getByText('EXISTINGCODE')).toBeInTheDocument();
  });

  it('shows use count and max uses for active invites', () => {
    const futureDate = new Date(Date.now() + 86400000).toISOString();
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'ACTIVE1',
            created_by: 'user-1',
            max_uses: 10,
            use_count: 3,
            is_revoked: false,
            expires_at: futureDate,
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.getByText(/3 \/ 10 Uses/)).toBeInTheDocument();
  });

  it('shows infinity symbol for unlimited use invites', () => {
    const futureDate = new Date(Date.now() + 86400000).toISOString();
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'UNLIMITED',
            created_by: 'user-1',
            max_uses: 0,
            use_count: 5,
            is_revoked: false,
            expires_at: futureDate,
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    // Unicode infinity \u221E
    expect(screen.getByText(/5 \/ \u221E Uses/)).toBeInTheDocument();
  });

  it('does not show revoked invites', () => {
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'REVOKED',
            created_by: 'user-1',
            max_uses: 0,
            use_count: 0,
            is_revoked: true,
            expires_at: null,
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.queryByText('REVOKED')).not.toBeInTheDocument();
    expect(screen.queryByText('Active Invites')).not.toBeInTheDocument();
  });

  it('does not show expired invites', () => {
    const pastDate = new Date(Date.now() - 86400000).toISOString();
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'EXPIRED',
            created_by: 'user-1',
            max_uses: 0,
            use_count: 0,
            is_revoked: false,
            expires_at: pastDate,
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.queryByText('EXPIRED')).not.toBeInTheDocument();
  });

  it('does not show fully used invites', () => {
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'USEDMAX',
            created_by: 'user-1',
            max_uses: 5,
            use_count: 5,
            is_revoked: false,
            expires_at: null,
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.queryByText('USEDMAX')).not.toBeInTheDocument();
  });

  it('copy button on existing invite copies its code', async () => {
    const futureDate = new Date(Date.now() + 86400000).toISOString();
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'EXISTCOPY',
            created_by: 'user-1',
            max_uses: 0,
            use_count: 0,
            is_revoked: false,
            expires_at: futureDate,
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    const copyBtn = document.querySelector('.invite-existing-copy')!;
    fireEvent.click(copyBtn);

    await vi.waitFor(() => {
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('EXISTCOPY');
    });
  });

  // ── Expiry formatting ──

  it('shows "Never" for invite without expiry', () => {
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'NOEXPIRY',
            created_by: 'user-1',
            max_uses: 0,
            use_count: 0,
            is_revoked: false,
            expires_at: null,
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(screen.getByText(/Never/)).toBeInTheDocument();
  });

  it('shows remaining time for active invite', () => {
    const futureDate = new Date(Date.now() + 7200000).toISOString(); // 2 hours
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'TIMED',
            created_by: 'user-1',
            max_uses: 0,
            use_count: 0,
            is_revoked: false,
            expires_at: futureDate,
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    // Should show "Xh remaining"
    expect(screen.getByText(/remaining/)).toBeInTheDocument();
  });

  // ── Resets on open ──

  it('resets state when modal opens', () => {
    const mockFetchInvites = vi.fn().mockResolvedValue(undefined);
    useInviteStore.setState({ fetchInvites: mockFetchInvites });

    const { rerender } = render(
      <InviteToServerModal isOpen={false} server={ownerServer} onClose={mockOnClose} />
    );
    rerender(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    expect(mockFetchInvites).toHaveBeenCalledWith('server-1');
  });

  // ── Select code on click ──

  it('code input is read-only', async () => {
    const mockCreateInvite = vi.fn().mockResolvedValue({
      id: 'inv-1',
      code: 'READONLY',
      server_id: 'server-1',
    });
    useInviteStore.setState({ createInvite: mockCreateInvite });

    render(<InviteToServerModal isOpen={true} server={ownerServer} onClose={mockOnClose} />);
    fireEvent.click(document.querySelector('.invite-generate-btn')!);

    await vi.waitFor(() => {
      const input = document.querySelector('.invite-code-value') as HTMLInputElement;
      expect(input).toBeInTheDocument();
      expect(input.readOnly).toBe(true);
    });
  });
});
