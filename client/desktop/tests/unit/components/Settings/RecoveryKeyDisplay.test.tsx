import { render, screen, fireEvent } from '../../../test-utils';
import { vi } from 'vitest';
import RecoveryKeyDisplay from '@/renderer/components/Settings/RecoveryKeyDisplay';

describe('RecoveryKeyDisplay', () => {
  const onConfirm = vi.fn();
  const onSkip = vi.fn();
  const recoveryKey = 'ABCD-EFGH-IJKL-MNOP-QRST-UVWX';

  beforeEach(() => {
    vi.clearAllMocks();
  });

  // ── Initial Rendering ──────────────────────────────────────────────────

  it('renders the recovery key', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);
    expect(screen.getByText(recoveryKey)).toBeInTheDocument();
  });

  it('renders warning about recovery key importance', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);
    expect(screen.getByText(/account recovery key/)).toBeInTheDocument();
    expect(screen.getByText(/will not be shown again/)).toBeInTheDocument();
  });

  it('renders Copy and Download buttons', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);
    expect(screen.getByText('Copy')).toBeInTheDocument();
    expect(screen.getByText('Download .txt')).toBeInTheDocument();
  });

  it('renders checkbox for confirming key is saved', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);
    expect(screen.getByText('I have saved my recovery key in a safe place')).toBeInTheDocument();
  });

  it('renders Continue button (disabled by default)', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);
    const continueBtn = screen.getByText('Continue');
    expect(continueBtn).toBeDisabled();
  });

  it('renders Skip for now link when onSkip is provided', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);
    expect(screen.getByText('Skip for now')).toBeInTheDocument();
  });

  it('does not render Skip for now when onSkip is not provided', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} />);
    expect(screen.queryByText('Skip for now')).not.toBeInTheDocument();
  });

  // ── Checkbox & Continue ────────────────────────────────────────────────

  it('enables Continue after checkbox is checked', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);

    const checkbox = screen.getByRole('checkbox');
    fireEvent.click(checkbox);

    expect(screen.getByText('Continue')).not.toBeDisabled();
  });

  it('disables Continue if checkbox is unchecked again', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);

    const checkbox = screen.getByRole('checkbox');
    fireEvent.click(checkbox);
    expect(screen.getByText('Continue')).not.toBeDisabled();

    fireEvent.click(checkbox);
    expect(screen.getByText('Continue')).toBeDisabled();
  });

  it('calls onConfirm when Continue is clicked after checking checkbox', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);

    fireEvent.click(screen.getByRole('checkbox'));
    fireEvent.click(screen.getByText('Continue'));
    expect(onConfirm).toHaveBeenCalled();
  });

  it('disables checkbox when disabled prop is true', () => {
    render(
      <RecoveryKeyDisplay
        recoveryKey={recoveryKey}
        onConfirm={onConfirm}
        onSkip={onSkip}
        disabled
      />
    );

    expect(screen.getByRole('checkbox')).toBeDisabled();
  });

  it('disables Continue when disabled prop is true even if checkbox is checked', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} disabled />);
    // Continue should be disabled regardless
    expect(screen.getByText('Continue')).toBeDisabled();
  });

  // ── Copy ───────────────────────────────────────────────────────────────

  it('copies recovery key to clipboard via navigator.clipboard', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} />);

    fireEvent.click(screen.getByText('Copy'));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(recoveryKey);
  });

  it('uses electron.writeClipboard when available', () => {
    const mockWriteClipboard = vi.fn();
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        writeClipboard: mockWriteClipboard,
      },
      writable: true,
    });

    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} />);

    fireEvent.click(screen.getByText('Copy'));
    expect(mockWriteClipboard).toHaveBeenCalledWith(recoveryKey);
  });

  // ── Download ───────────────────────────────────────────────────────────

  it('triggers download when Download .txt is clicked', () => {
    const mockClick = vi.fn();
    const origCreateObjectURL = URL.createObjectURL;
    const origRevokeObjectURL = URL.revokeObjectURL;
    URL.createObjectURL = vi.fn().mockReturnValue('blob:mock-url');
    URL.revokeObjectURL = vi.fn();

    // Intercept anchor creation before render to avoid infinite recursion
    const origCreate = document.createElement.bind(document);
    const createSpy = vi
      .spyOn(document, 'createElement')
      .mockImplementation((tagName: string, options?: ElementCreationOptions) => {
        const el = origCreate(tagName, options);
        if (tagName === 'a') {
          el.click = mockClick;
        }
        return el;
      });

    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} />);

    fireEvent.click(screen.getByText('Download .txt'));

    expect(URL.createObjectURL).toHaveBeenCalled();
    expect(mockClick).toHaveBeenCalled();
    expect(URL.revokeObjectURL).toHaveBeenCalled();

    createSpy.mockRestore();
    URL.createObjectURL = origCreateObjectURL;
    URL.revokeObjectURL = origRevokeObjectURL;
  });

  // ── Skip Flow ──────────────────────────────────────────────────────────

  it('shows skip warning when "Skip for now" is clicked', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);

    fireEvent.click(screen.getByText('Skip for now'));

    expect(screen.getByText(/Are you sure?/)).toBeInTheDocument();
    expect(screen.getByText(/permanently losing all encrypted/)).toBeInTheDocument();
  });

  it('shows Go Back and Skip Without Recovery Key buttons on skip warning', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);

    fireEvent.click(screen.getByText('Skip for now'));

    expect(screen.getByText('Go Back')).toBeInTheDocument();
    expect(screen.getByText('Skip Without Recovery Key')).toBeInTheDocument();
  });

  it('returns to main view when Go Back is clicked on skip warning', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);

    fireEvent.click(screen.getByText('Skip for now'));
    expect(screen.getByText(/Are you sure?/)).toBeInTheDocument();

    fireEvent.click(screen.getByText('Go Back'));
    // Should show the recovery key again
    expect(screen.getByText(recoveryKey)).toBeInTheDocument();
    expect(screen.queryByText(/Are you sure?/)).not.toBeInTheDocument();
  });

  it('calls onSkip when "Skip Without Recovery Key" is clicked', () => {
    render(<RecoveryKeyDisplay recoveryKey={recoveryKey} onConfirm={onConfirm} onSkip={onSkip} />);

    fireEvent.click(screen.getByText('Skip for now'));
    fireEvent.click(screen.getByText('Skip Without Recovery Key'));

    expect(onSkip).toHaveBeenCalled();
  });
});
