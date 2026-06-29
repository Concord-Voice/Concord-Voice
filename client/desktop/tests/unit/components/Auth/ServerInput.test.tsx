import { render, screen, fireEvent, waitFor } from '../../../test-utils';

// Import after mocking
import ServerInput from '@/renderer/components/Auth/ServerInput';

describe('ServerInput', () => {
  const mockOnConnect = vi.fn();
  const mockOnBack = vi.fn();
  const mockProbeServer = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    globalThis.electron = {
      ...(globalThis.electron ?? {}),
      selfHosted: {
        probeServer: mockProbeServer,
      },
    } as typeof globalThis.electron;
    mockProbeServer.mockResolvedValue({
      status: 'ok',
      apiBase: 'https://concord.example.com',
      clientConfig: {},
      capabilities: {},
    });
  });

  afterEach(() => {
    globalThis.electron = undefined as unknown as typeof globalThis.electron;
  });

  it('renders the server input form', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    expect(screen.getByText('Connect to Self-Hosted Server')).toBeInTheDocument();
    expect(screen.getByLabelText('Server URL')).toBeInTheDocument();
    expect(screen.getByText('Connect to Server')).toBeInTheDocument();
  });

  it('enables connect button when URL is entered', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    expect(screen.getByText('Connect to Server')).toBeDisabled();
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'example.com' },
    });
    expect(screen.getByText('Connect to Server')).not.toBeDisabled();
  });

  it('adds https:// prefix when protocol is missing', async () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    await waitFor(() => {
      expect(mockProbeServer).toHaveBeenCalledWith('https://concord.example.com');
      expect(mockOnConnect).toHaveBeenCalledWith('https://concord.example.com');
    });
  });

  it('accepts valid https URL', async () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    await waitFor(() => expect(mockOnConnect).toHaveBeenCalledWith('https://concord.example.com'));
  });

  it('allows http:// for localhost', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'ok',
      apiBase: 'http://localhost:8080',
      clientConfig: {},
      capabilities: {},
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'http://localhost:8080' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    await waitFor(() => expect(mockOnConnect).toHaveBeenCalledWith('http://localhost:8080'));
  });

  it('shows the probe error message and does not connect when discovery fails', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'capabilities_failed',
      message: 'Could not load capabilities.',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(await screen.findByText('Could not load capabilities.')).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('rejects http:// for non-localhost', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'http://insecure.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    expect(
      screen.getByText('HTTPS is required for security (except localhost)')
    ).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('rejects invalid URL', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'not a valid url @@@' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    expect(screen.getByText('Invalid server URL')).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('calls onBack when back button clicked', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.click(screen.getByText('← Back to Connection Options'));
    expect(mockOnBack).toHaveBeenCalled();
  });

  it('submits on Enter key', async () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    const input = screen.getByLabelText('Server URL');
    fireEvent.change(input, { target: { value: 'https://concord.example.com' } });
    fireEvent.keyPress(input, { key: 'Enter', charCode: 13 });
    await waitFor(() => expect(mockOnConnect).toHaveBeenCalled());
  });

  it('disables connect button when URL is empty', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    expect(screen.getByText('Connect to Server')).toBeDisabled();
  });

  it('shows security info', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    expect(screen.getByText('Secure Connection')).toBeInTheDocument();
  });
});
