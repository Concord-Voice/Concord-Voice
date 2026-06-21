import { render, screen, fireEvent } from '../../../test-utils';

// Import after mocking
import ServerInput from '@/renderer/components/Auth/ServerInput';

describe('ServerInput', () => {
  const mockOnConnect = vi.fn();
  const mockOnBack = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
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

  it('adds https:// prefix when protocol is missing', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    expect(mockOnConnect).toHaveBeenCalledWith('https://concord.example.com');
  });

  it('accepts valid https URL', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    expect(mockOnConnect).toHaveBeenCalledWith('https://concord.example.com');
  });

  it('allows http:// for localhost', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'http://localhost:8080' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    expect(mockOnConnect).toHaveBeenCalledWith('http://localhost:8080');
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

  it('submits on Enter key', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    const input = screen.getByLabelText('Server URL');
    fireEvent.change(input, { target: { value: 'https://concord.example.com' } });
    fireEvent.keyPress(input, { key: 'Enter', charCode: 13 });
    expect(mockOnConnect).toHaveBeenCalled();
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
