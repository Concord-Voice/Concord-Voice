import { render, screen, fireEvent } from '../../../test-utils';
import BackupCodeInput from '@/renderer/components/Auth/BackupCodeInput';

describe('BackupCodeInput', () => {
  const onSubmit = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders label and input', () => {
    render(<BackupCodeInput onSubmit={onSubmit} />);
    expect(screen.getByText('Enter one of your 8-character backup codes')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('XXXXXXXX')).toBeInTheDocument();
  });

  it('uppercases input as user types', () => {
    render(<BackupCodeInput onSubmit={onSubmit} />);
    const input = screen.getByPlaceholderText('XXXXXXXX');
    fireEvent.change(input, { target: { value: 'abcd1234' } });
    expect(input).toHaveValue('ABCD1234');
  });

  it('submits uppercased trimmed code on Enter key', () => {
    render(<BackupCodeInput onSubmit={onSubmit} />);
    const input = screen.getByPlaceholderText('XXXXXXXX');
    fireEvent.change(input, { target: { value: 'abcd1234' } });
    fireEvent.submit(input.closest('form')!);
    expect(onSubmit).toHaveBeenCalledWith('ABCD1234');
  });

  it('does not submit when code is less than 8 characters', () => {
    render(<BackupCodeInput onSubmit={onSubmit} />);
    fireEvent.change(screen.getByPlaceholderText('XXXXXXXX'), {
      target: { value: 'ABC' },
    });
    fireEvent.submit(screen.getByPlaceholderText('XXXXXXXX').closest('form')!);
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('shows error message', () => {
    render(<BackupCodeInput onSubmit={onSubmit} error="Invalid backup code" />);
    expect(screen.getByText('Invalid backup code')).toBeInTheDocument();
  });

  it('adds error class to input when error exists', () => {
    render(<BackupCodeInput onSubmit={onSubmit} error="Bad code" />);
    expect(screen.getByPlaceholderText('XXXXXXXX')).toHaveClass('error');
  });

  it('disables input when disabled', () => {
    render(<BackupCodeInput onSubmit={onSubmit} disabled />);
    expect(screen.getByPlaceholderText('XXXXXXXX')).toBeDisabled();
  });

  it('has maxLength of 8', () => {
    render(<BackupCodeInput onSubmit={onSubmit} />);
    expect(screen.getByPlaceholderText('XXXXXXXX')).toHaveAttribute('maxLength', '8');
  });
});
