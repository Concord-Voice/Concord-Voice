import { render, screen, fireEvent } from '../../../test-utils';
import ToggleSwitch from '@/renderer/components/Settings/ToggleSwitch';

describe('ToggleSwitch', () => {
  const mockOnChange = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders a checkbox input', () => {
    render(<ToggleSwitch checked={false} onChange={mockOnChange} />);
    expect(screen.getByRole('checkbox')).toBeInTheDocument();
  });

  it('toggles on click', () => {
    render(<ToggleSwitch checked={false} onChange={mockOnChange} />);
    fireEvent.click(screen.getByRole('checkbox'));
    expect(mockOnChange).toHaveBeenCalledWith(true);
  });

  it('uses aria-label by default', () => {
    const { container } = render(<ToggleSwitch checked={false} onChange={mockOnChange} />);
    const label = container.querySelector('.settings-toggle');
    expect(label).toHaveAttribute('aria-label', 'Toggle');
  });

  it('uses custom aria-label', () => {
    const { container } = render(
      <ToggleSwitch checked={false} onChange={mockOnChange} label="Dark mode" />
    );
    const label = container.querySelector('.settings-toggle');
    expect(label).toHaveAttribute('aria-label', 'Dark mode');
  });

  it('uses aria-labelledby when provided', () => {
    const { container } = render(
      <ToggleSwitch checked={false} onChange={mockOnChange} ariaLabelledBy="my-label-id" />
    );
    const label = container.querySelector('.settings-toggle');
    expect(label).toHaveAttribute('aria-labelledby', 'my-label-id');
    expect(label).not.toHaveAttribute('aria-label');
  });

  it('disables checkbox when disabled', () => {
    render(<ToggleSwitch checked={false} onChange={mockOnChange} disabled={true} />);
    expect(screen.getByRole('checkbox')).toBeDisabled();
  });

  it('forwards the id prop to the native checkbox (focus target for back-links)', () => {
    const { container } = render(
      <ToggleSwitch id="toggle-dyslexic-support" checked={false} onChange={mockOnChange} />
    );
    const input = container.querySelector('input[type="checkbox"]');
    expect(input).toHaveAttribute('id', 'toggle-dyslexic-support');
  });
});
