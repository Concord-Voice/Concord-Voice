import { render, screen, fireEvent } from '../../../test-utils';
import TOTPInput from '@/renderer/components/Auth/TOTPInput';

describe('TOTPInput', () => {
  const onSubmit = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders 6 digit inputs', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    const inputs = screen.getAllByRole('textbox');
    expect(inputs).toHaveLength(6);
  });

  it('each input has an aria-label', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    for (let i = 1; i <= 6; i++) {
      expect(screen.getByLabelText(`Digit ${i}`)).toBeInTheDocument();
    }
  });

  it('auto-focuses the first input by default', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    expect(screen.getByLabelText('Digit 1')).toHaveFocus();
  });

  it('does not auto-focus when autoFocus is false', () => {
    render(<TOTPInput onSubmit={onSubmit} autoFocus={false} />);
    expect(screen.getByLabelText('Digit 1')).not.toHaveFocus();
  });

  it('advances focus to next input on digit entry', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    const digit1 = screen.getByLabelText('Digit 1');
    fireEvent.change(digit1, { target: { value: '1' } });
    expect(screen.getByLabelText('Digit 2')).toHaveFocus();
  });

  it('filters non-numeric characters', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    const digit1 = screen.getByLabelText('Digit 1');
    fireEvent.change(digit1, { target: { value: 'a' } });
    expect(digit1).toHaveValue('');
  });

  it('auto-submits when all 6 digits are filled', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    for (let i = 1; i <= 6; i++) {
      fireEvent.change(screen.getByLabelText(`Digit ${i}`), {
        target: { value: String(i) },
      });
    }
    expect(onSubmit).toHaveBeenCalledWith('123456');
  });

  it('handles paste of full 6-digit code', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    const container = screen.getByLabelText('Digit 1').closest('.totp-digits')!;
    fireEvent.paste(container, {
      clipboardData: { getData: () => '654321' },
    });
    expect(onSubmit).toHaveBeenCalledWith('654321');
  });

  it('handles paste of partial code', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    const container = screen.getByLabelText('Digit 1').closest('.totp-digits')!;
    fireEvent.paste(container, {
      clipboardData: { getData: () => '123' },
    });
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByLabelText('Digit 1')).toHaveValue('1');
    expect(screen.getByLabelText('Digit 2')).toHaveValue('2');
    expect(screen.getByLabelText('Digit 3')).toHaveValue('3');
  });

  it('strips non-numeric chars from pasted text', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    const container = screen.getByLabelText('Digit 1').closest('.totp-digits')!;
    fireEvent.paste(container, {
      clipboardData: { getData: () => '12-34-56' },
    });
    expect(onSubmit).toHaveBeenCalledWith('123456');
  });

  it('backspace moves focus to previous input', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    const digit1 = screen.getByLabelText('Digit 1');
    const digit2 = screen.getByLabelText('Digit 2');
    fireEvent.change(digit1, { target: { value: '1' } });
    // digit2 should now have focus; press backspace with empty value
    fireEvent.keyDown(digit2, { key: 'Backspace' });
    expect(digit1).toHaveFocus();
  });

  it('disables all inputs when disabled prop is true', () => {
    render(<TOTPInput onSubmit={onSubmit} disabled />);
    const inputs = screen.getAllByRole('textbox');
    for (const input of inputs) {
      expect(input).toBeDisabled();
    }
  });

  it('shows error message', () => {
    render(<TOTPInput onSubmit={onSubmit} error="Invalid code" />);
    expect(screen.getByText('Invalid code')).toBeInTheDocument();
  });

  it('adds error class to digit inputs when error exists', () => {
    render(<TOTPInput onSubmit={onSubmit} error="Invalid code" />);
    const inputs = screen.getAllByRole('textbox');
    for (const input of inputs) {
      expect(input).toHaveClass('totp-digit-error');
    }
  });

  it('renders backup code link when onBackupCode is provided', () => {
    const onBackup = vi.fn();
    render(<TOTPInput onSubmit={onSubmit} onBackupCode={onBackup} />);
    const link = screen.getByText('Use a backup code instead');
    expect(link).toBeInTheDocument();
    fireEvent.click(link);
    expect(onBackup).toHaveBeenCalledOnce();
  });

  it('does not render backup code link when onBackupCode is not provided', () => {
    render(<TOTPInput onSubmit={onSubmit} />);
    expect(screen.queryByText('Use a backup code instead')).not.toBeInTheDocument();
  });

  it('does not accept input when disabled', () => {
    render(<TOTPInput onSubmit={onSubmit} disabled />);
    const digit1 = screen.getByLabelText('Digit 1');
    fireEvent.change(digit1, { target: { value: '1' } });
    expect(digit1).toHaveValue('');
    expect(onSubmit).not.toHaveBeenCalled();
  });
});
