import { render, screen, fireEvent } from '../../../test-utils';
import ConnectionSelector from '@/renderer/components/Auth/ConnectionSelector';

describe('ConnectionSelector', () => {
  const mockOnSelect = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders all connection options', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    expect(screen.getByText('Sign In to Existing Account')).toBeInTheDocument();
    expect(screen.getByText('Create New Account')).toBeInTheDocument();
    expect(screen.getByText('Connect to Self-Hosted Server')).toBeInTheDocument();
  });

  it('disables continue button when no option selected', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    expect(screen.getByText('Continue')).toBeDisabled();
  });

  it('enables continue button after selecting an option', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    fireEvent.click(screen.getByText('Sign In to Existing Account'));
    expect(screen.getByText('Continue')).not.toBeDisabled();
  });

  it('calls onSelect with hosted-login when sign in is chosen', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    fireEvent.click(screen.getByText('Sign In to Existing Account'));
    fireEvent.click(screen.getByText('Continue'));
    expect(mockOnSelect).toHaveBeenCalledWith('hosted-login');
  });

  it('calls onSelect with hosted when create account is chosen', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    fireEvent.click(screen.getByText('Create New Account'));
    fireEvent.click(screen.getByText('Continue'));
    expect(mockOnSelect).toHaveBeenCalledWith('hosted');
  });

  it('calls onSelect with self-hosted when self-hosted is chosen', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    fireEvent.click(screen.getByText('Connect to Self-Hosted Server'));
    fireEvent.click(screen.getByText('Continue'));
    expect(mockOnSelect).toHaveBeenCalledWith('self-hosted');
  });

  it('does not call onSelect when continue clicked without selection', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    fireEvent.click(screen.getByText('Continue'));
    expect(mockOnSelect).not.toHaveBeenCalled();
  });

  it('adds selected class to chosen option', () => {
    const { container } = render(<ConnectionSelector onSelect={mockOnSelect} />);
    fireEvent.click(screen.getByText('Create New Account'));
    const options = container.querySelectorAll('.connection-option');
    const selectedOptions = container.querySelectorAll('.connection-option.selected');
    expect(options.length).toBe(3);
    expect(selectedOptions.length).toBe(1);
  });

  it('switches selection between modes', () => {
    const { container } = render(<ConnectionSelector onSelect={mockOnSelect} />);
    fireEvent.click(screen.getByText('Sign In to Existing Account'));
    expect(container.querySelectorAll('.connection-option.selected')).toHaveLength(1);

    fireEvent.click(screen.getByText('Connect to Self-Hosted Server'));
    expect(container.querySelectorAll('.connection-option.selected')).toHaveLength(1);

    fireEvent.click(screen.getByText('Continue'));
    expect(mockOnSelect).toHaveBeenCalledWith('self-hosted');
  });

  it('renders footer link', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    expect(screen.getByText('Learn about self-hosting')).toBeInTheDocument();
  });

  it('renders logo image', () => {
    const { container } = render(<ConnectionSelector onSelect={mockOnSelect} />);
    const logo = container.querySelector('.connection-logo');
    expect(logo).toBeInTheDocument();
    expect(logo?.tagName).toBe('IMG');
  });

  it('renders three radio inputs', () => {
    const { container } = render(<ConnectionSelector onSelect={mockOnSelect} />);
    const radios = container.querySelectorAll('input[type="radio"]');
    expect(radios).toHaveLength(3);
  });

  it('renders self-hosting link as a button', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    expect(screen.getByRole('button', { name: /learn about self-hosting/i })).toBeInTheDocument();
  });

  it('opens self-hosting help in the external browser', () => {
    const openExternal = vi.fn();
    const electron = globalThis.electron as { openExternal?: typeof openExternal } | undefined;
    const originalOpenExternal = electron?.openExternal;
    if (electron) {
      electron.openExternal = openExternal;
    }

    render(<ConnectionSelector onSelect={mockOnSelect} />);
    const button = screen.getByRole('button', { name: /learn about self-hosting/i });
    expect(button).not.toBeDisabled();

    fireEvent.click(button);
    expect(openExternal).toHaveBeenCalledWith('https://concordvoice.com/self-hosting');

    if (electron) {
      if (originalOpenExternal) {
        electron.openExternal = originalOpenExternal;
      } else {
        delete electron.openExternal;
      }
    }
  });

  it('labels have accessible text', () => {
    render(<ConnectionSelector onSelect={mockOnSelect} />);
    const labels = screen.getAllByLabelText(/.+/);
    expect(labels.length).toBeGreaterThanOrEqual(3);
  });
});
