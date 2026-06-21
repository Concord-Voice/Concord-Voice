import { render, screen } from '../../../test-utils';
import PasswordStrength from '@/renderer/components/Auth/PasswordStrength';

describe('PasswordStrength', () => {
  it('renders nothing for empty password', () => {
    const { container } = render(<PasswordStrength password="" />);
    expect(container.innerHTML).toBe('');
  });

  it('shows Very Weak for short password', () => {
    render(<PasswordStrength password="abc" />);
    expect(screen.getByText('Very Weak')).toBeInTheDocument();
  });

  it('shows feedback for short password', () => {
    render(<PasswordStrength password="abc" />);
    expect(screen.getByText(/at least 12 characters/i)).toBeInTheDocument();
  });

  it('shows stronger rating for good password', () => {
    render(<PasswordStrength password="MyS3cur3Phr@se!" />);
    const label = screen.getByText(/good|strong/i);
    expect(label).toBeInTheDocument();
  });

  it('shows Legendary for very strong password', () => {
    render(<PasswordStrength password="MyS3cur3P@ssw0rd!2025ExtraLong" />);
    expect(screen.getByText(/legendary/i)).toBeInTheDocument();
  });
});
