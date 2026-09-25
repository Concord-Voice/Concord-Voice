import { render, screen, userEvent, fireEvent } from '../../../test-utils';
import BackupCodeInput from '@/renderer/components/Auth/BackupCodeInput';

// regression: a typed backup code was never sent unless Enter was pressed.
describe('BackupCodeInput auto-report (regression)', () => {
  const onSubmit = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('reports the uppercased 8-character code to onSubmit without pressing Enter', async () => {
    // regression: a typed backup code was never sent unless Enter was pressed
    const user = userEvent.setup();
    render(<BackupCodeInput onSubmit={onSubmit} />);
    const input = screen.getByPlaceholderText('XXXXXXXX');

    await user.type(input, 'exci3g5f');

    expect(onSubmit).toHaveBeenCalledWith('EXCI3G5F');
  });

  it('does not call onSubmit while fewer than 8 characters have been typed', async () => {
    // regression: a typed backup code was never sent unless Enter was pressed
    const user = userEvent.setup();
    render(<BackupCodeInput onSubmit={onSubmit} />);
    const input = screen.getByPlaceholderText('XXXXXXXX');

    await user.type(input, 'exci3g5');

    expect(onSubmit).not.toHaveBeenCalled();
  });

  // Q3: a parent that stores the code must see an edit that makes it incomplete.
  it('reports every edit to onCodeChange: the code once complete, empty while not', async () => {
    const user = userEvent.setup();
    const onCodeChange = vi.fn();
    render(<BackupCodeInput onSubmit={onSubmit} onCodeChange={onCodeChange} />);
    const input = screen.getByPlaceholderText('XXXXXXXX');

    await user.type(input, 'exci3g5f');
    expect(onCodeChange).toHaveBeenLastCalledWith('EXCI3G5F');
    await user.type(input, '{Backspace}');
    expect(onCodeChange).toHaveBeenLastCalledWith('');
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  // F6: a name from the visible instruction, and advance notice of the auto-submit.
  it('is named by its instruction and described by the auto-submit notice', () => {
    render(<BackupCodeInput onSubmit={onSubmit} />);
    const input = screen.getByRole('textbox', {
      name: 'Enter one of your 8-character backup codes',
    });
    expect(input).toHaveAccessibleDescription(
      'It is checked as soon as you type the 8th character.'
    );
  });

  it('ignores edits and Enter while disabled', () => {
    const onCodeChange = vi.fn();
    render(<BackupCodeInput onSubmit={onSubmit} onCodeChange={onCodeChange} disabled />);
    const input = screen.getByPlaceholderText('XXXXXXXX');
    fireEvent.change(input, { target: { value: 'EXCI3G5F' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);
    expect(onSubmit).not.toHaveBeenCalled();
    expect(onCodeChange).not.toHaveBeenCalled();
  });
});
