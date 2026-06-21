import { render, screen, fireEvent } from '../../../test-utils';
import BackupCodeDisplay from '@/renderer/components/Settings/BackupCodeDisplay';

describe('BackupCodeDisplay', () => {
  const mockCodes = [
    'AAAA1111',
    'BBBB2222',
    'CCCC3333',
    'DDDD4444',
    'EEEE5555',
    'FFFF6666',
    'GGGG7777',
    'HHHH8888',
  ];
  const onConfirm = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders all 8 backup codes', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    for (const code of mockCodes) {
      expect(screen.getByText(code)).toBeInTheDocument();
    }
  });

  it('renders numbered labels for each code', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    for (let i = 1; i <= 8; i++) {
      expect(screen.getByText(`${i}.`)).toBeInTheDocument();
    }
  });

  it('renders warning message', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    expect(
      screen.getByText('These codes will not be shown again. Each code can only be used once.')
    ).toBeInTheDocument();
  });

  it('renders Copy All button', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    expect(screen.getByText('Copy All')).toBeInTheDocument();
  });

  it('renders Download .txt button', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    expect(screen.getByText('Download .txt')).toBeInTheDocument();
  });

  it('renders confirmation checkbox', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    expect(screen.getByText('I have saved my backup codes in a safe place')).toBeInTheDocument();
    expect(screen.getByRole('checkbox')).not.toBeChecked();
  });

  it('Activate MFA button is disabled until checkbox is checked', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    const activateBtn = screen.getByText('Activate MFA');
    expect(activateBtn).toBeDisabled();
  });

  it('Activate MFA button is enabled after checkbox is checked', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    fireEvent.click(screen.getByRole('checkbox'));
    expect(screen.getByText('Activate MFA')).not.toBeDisabled();
  });

  it('calls onConfirm when Activate MFA is clicked', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    fireEvent.click(screen.getByRole('checkbox'));
    fireEvent.click(screen.getByText('Activate MFA'));
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it('does not call onConfirm without checking the checkbox first', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    // Button is disabled, but verify clicking does nothing
    fireEvent.click(screen.getByText('Activate MFA'));
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('Copy All copies formatted text to clipboard', async () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} />);
    fireEvent.click(screen.getByText('Copy All'));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(
      mockCodes.map((c, i) => `${i + 1}. ${c}`).join('\n')
    );
  });

  it('checkbox is disabled when disabled prop is true', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} disabled />);
    expect(screen.getByRole('checkbox')).toBeDisabled();
  });

  it('Activate MFA button is disabled when disabled prop is true even if checkbox checked', () => {
    render(<BackupCodeDisplay codes={mockCodes} onConfirm={onConfirm} disabled />);
    // Can't check checkbox since it's disabled, but verify the button is disabled
    expect(screen.getByText('Activate MFA')).toBeDisabled();
  });
});
