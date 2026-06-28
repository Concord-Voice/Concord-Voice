import { render, screen, fireEvent } from '../../../test-utils';
import { useUserStore } from '@/renderer/stores/userStore';
import { mockUser } from '../../../mocks/fixtures';

// Mock PasswordStrength
vi.mock('@/renderer/components/Auth/PasswordStrength', () => ({
  default: ({ password }: { password: string }) => (
    <div data-testid="password-strength">{password ? 'strength-shown' : 'no-password'}</div>
  ),
}));

// Mock LoadingSpinner
vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({
  default: () => <div data-testid="loading-spinner">Loading...</div>,
}));

// NsfwContentGate (the sibling section in AccountSection) reads age status.
vi.mock('@/renderer/hooks/useAgeStatus', () => ({
  useAgeStatus: () => ({ nsfwAuth: 'unknown' }),
}));

import AccountSection from '@/renderer/components/Settings/AccountSection';

// Profile/password form behavior, exercised through their new host AccountSection ▸
// My Profile (#1773). Was tests/unit/components/Profile/ProfilePage.test.tsx before the
// standalone page was removed; retained here so ProfileInfoForm + PasswordChangeForm
// (an OWASP A07 surface) coverage does not regress during the move.
describe('AccountSection ▸ My Profile', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useUserStore.setState({
      user: {
        ...mockUser,
        display_name: 'Test Display',
        bio: 'Test bio here',
        links: ['https://example.com'],
      },
      isLoading: false,
    });
  });

  it('renders the My Profile section title', () => {
    render(<AccountSection />);
    expect(screen.getByText('My Profile')).toBeInTheDocument();
  });

  it('renders profile information section', () => {
    render(<AccountSection />);
    expect(screen.getByText('Profile Information')).toBeInTheDocument();
  });

  it('renders username field with value', () => {
    render(<AccountSection />);
    expect(screen.getByText('Username')).toBeInTheDocument();
    const input = screen.getByDisplayValue('testuser');
    expect(input).toBeInTheDocument();
  });

  it('renders display name field with value', () => {
    render(<AccountSection />);
    expect(screen.getByText('Display Name')).toBeInTheDocument();
    const input = screen.getByDisplayValue('Test Display');
    expect(input).toBeInTheDocument();
  });

  it('renders bio field with value', () => {
    render(<AccountSection />);
    expect(screen.getByText('About')).toBeInTheDocument();
    const textarea = screen.getByDisplayValue('Test bio here');
    expect(textarea).toBeInTheDocument();
  });

  it('renders links section', () => {
    render(<AccountSection />);
    expect(screen.getByText('Links')).toBeInTheDocument();
    const linkInput = screen.getByDisplayValue('https://example.com');
    expect(linkInput).toBeInTheDocument();
  });

  it('renders avatar upload area', () => {
    render(<AccountSection />);
    expect(screen.getByLabelText('Upload avatar')).toBeInTheDocument();
  });

  it('renders avatar initial when no avatar URL', () => {
    useUserStore.setState({
      user: { ...mockUser, avatar_url: undefined, display_name: null, bio: null, links: [] },
    });
    render(<AccountSection />);
    expect(screen.getByText('T')).toBeInTheDocument(); // First letter of "testuser"
  });

  it('renders password change section', () => {
    render(<AccountSection />);
    // "Change Password" appears as h2 title AND as submit button text
    const elements = screen.getAllByText('Change Password');
    expect(elements.length).toBeGreaterThanOrEqual(2);
  });

  it('renders current password field', () => {
    render(<AccountSection />);
    expect(screen.getByPlaceholderText('Enter your current password')).toBeInTheDocument();
  });

  it('renders new password field', () => {
    render(<AccountSection />);
    expect(screen.getByPlaceholderText('Enter your new password')).toBeInTheDocument();
  });

  it('renders confirm password field', () => {
    render(<AccountSection />);
    expect(screen.getByPlaceholderText('Confirm your new password')).toBeInTheDocument();
  });

  it('renders save and cancel buttons', () => {
    render(<AccountSection />);
    const saveButtons = screen.getAllByText('Save Changes');
    expect(saveButtons.length).toBeGreaterThanOrEqual(1);
  });

  it('shows character count for username', () => {
    render(<AccountSection />);
    // "testuser" = 8 chars out of 32; hint includes cooldown notice
    expect(screen.getByText(/8\/32 characters/)).toBeInTheDocument();
  });

  it('shows character count for bio', () => {
    render(<AccountSection />);
    // "Test bio here" = 13 chars out of 500
    expect(screen.getByText('13/500 characters')).toBeInTheDocument();
  });

  it('updates username on input change', () => {
    render(<AccountSection />);
    const input = screen.getByDisplayValue('testuser');
    fireEvent.change(input, { target: { value: 'newname' } });
    expect(screen.getByDisplayValue('newname')).toBeInTheDocument();
  });

  it('renders add link button when under max links', () => {
    render(<AccountSection />);
    expect(screen.getByText('+ Add link')).toBeInTheDocument();
  });

  it('renders member since date', () => {
    render(<AccountSection />);
    const memberSince = document.querySelector('.profile-member-since');
    expect(memberSince).toBeInTheDocument();
    expect(memberSince?.textContent).toMatch(/Member since/);
  });

  it('adds a new link when Add Link is clicked', () => {
    render(<AccountSection />);
    fireEvent.click(screen.getByText('+ Add link'));
    // Should now have 2 link inputs (1 existing + 1 new)
    const linkInputs = screen.getAllByPlaceholderText('https://example.com');
    expect(linkInputs.length).toBe(2);
  });

  it('removes a link when remove button is clicked', () => {
    render(<AccountSection />);
    const removeBtn = screen.getByLabelText('Remove link');
    fireEvent.click(removeBtn);
    // Link input should be gone
    expect(screen.queryByDisplayValue('https://example.com')).not.toBeInTheDocument();
  });

  it('updates display name on input change', () => {
    render(<AccountSection />);
    const input = screen.getByDisplayValue('Test Display');
    fireEvent.change(input, { target: { value: 'New Name' } });
    expect(screen.getByDisplayValue('New Name')).toBeInTheDocument();
  });

  it('updates bio on textarea change', () => {
    render(<AccountSection />);
    const textarea = screen.getByDisplayValue('Test bio here');
    fireEvent.change(textarea, { target: { value: 'New bio' } });
    expect(screen.getByDisplayValue('New bio')).toBeInTheDocument();
  });

  it('updates current password on input change', () => {
    render(<AccountSection />);
    const input = screen.getByPlaceholderText('Enter your current password');
    fireEvent.change(input, { target: { value: 'mypassword' } });
    expect(screen.getByDisplayValue('mypassword')).toBeInTheDocument();
  });

  it('updates new password on input change', () => {
    render(<AccountSection />);
    const input = screen.getByPlaceholderText('Enter your new password');
    fireEvent.change(input, { target: { value: 'newpass123456' } });
    expect(screen.getByDisplayValue('newpass123456')).toBeInTheDocument();
  });

  it('updates confirm password on input change', () => {
    render(<AccountSection />);
    const input = screen.getByPlaceholderText('Confirm your new password');
    fireEvent.change(input, { target: { value: 'newpass123456' } });
    expect(screen.getByDisplayValue('newpass123456')).toBeInTheDocument();
  });

  it('shows password strength indicator', () => {
    render(<AccountSection />);
    const input = screen.getByPlaceholderText('Enter your new password');
    fireEvent.change(input, { target: { value: 'newpass123456' } });
    expect(screen.getByTestId('password-strength')).toBeInTheDocument();
    expect(screen.getByText('strength-shown')).toBeInTheDocument();
  });

  it('updates link value on change', () => {
    render(<AccountSection />);
    const linkInput = screen.getByDisplayValue('https://example.com');
    fireEvent.change(linkInput, { target: { value: 'https://newsite.com' } });
    expect(screen.getByDisplayValue('https://newsite.com')).toBeInTheDocument();
  });

  it('renders remove avatar button when avatar exists', () => {
    useUserStore.setState({
      user: {
        ...mockUser,
        avatar_url: 'https://example.com/avatar.png',
        display_name: 'Test Display',
        bio: 'Test bio here',
        links: [],
      },
    });
    render(<AccountSection />);
    expect(screen.getByText('Remove avatar')).toBeInTheDocument();
  });

  it('handles remove avatar click', () => {
    useUserStore.setState({
      user: {
        ...mockUser,
        avatar_url: 'https://example.com/avatar.png',
        display_name: 'Test Display',
        bio: 'Test bio here',
        links: [],
      },
    });
    render(<AccountSection />);
    fireEvent.click(screen.getByText('Remove avatar'));
    // After removing, avatar initial should show
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('shows links count', () => {
    render(<AccountSection />);
    expect(screen.getByText('1/5 links')).toBeInTheDocument();
  });

  it('validates empty username on submit', async () => {
    render(<AccountSection />);
    const input = screen.getByDisplayValue('testuser');
    fireEvent.change(input, { target: { value: '' } });
    // Submit the profile form
    const saveBtn = screen.getAllByText('Save Changes')[0];
    fireEvent.click(saveBtn);
    expect(screen.getByText('Username is required')).toBeInTheDocument();
  });

  it('validates short username on submit', async () => {
    render(<AccountSection />);
    const input = screen.getByDisplayValue('testuser');
    fireEvent.change(input, { target: { value: 'ab' } });
    const saveBtn = screen.getAllByText('Save Changes')[0];
    fireEvent.click(saveBtn);
    expect(screen.getByText('Username must be at least 3 characters')).toBeInTheDocument();
  });

  it('validates password required fields on submit', () => {
    render(<AccountSection />);
    // Find the password Change Password submit button (it's a button, not submit in a separate form)
    const changePwdButtons = screen.getAllByText('Change Password');
    // The last one is the submit button
    const submitBtn = changePwdButtons[changePwdButtons.length - 1];
    fireEvent.click(submitBtn);
    expect(screen.getByText('Current password is required')).toBeInTheDocument();
  });

  it('validates password mismatch on submit', () => {
    render(<AccountSection />);
    fireEvent.change(screen.getByPlaceholderText('Enter your current password'), {
      target: { value: 'oldpass123456' },
    });
    fireEvent.change(screen.getByPlaceholderText('Enter your new password'), {
      target: { value: 'newpass123456' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'mismatch123456' },
    });
    const changePwdButtons = screen.getAllByText('Change Password');
    const submitBtn = changePwdButtons[changePwdButtons.length - 1];
    fireEvent.click(submitBtn);
    expect(screen.getByText('Passwords do not match')).toBeInTheDocument();
  });

  it('validates short new password', () => {
    render(<AccountSection />);
    fireEvent.change(screen.getByPlaceholderText('Enter your current password'), {
      target: { value: 'oldpass123456' },
    });
    fireEvent.change(screen.getByPlaceholderText('Enter your new password'), {
      target: { value: 'short' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'short' },
    });
    const changePwdButtons = screen.getAllByText('Change Password');
    const submitBtn = changePwdButtons[changePwdButtons.length - 1];
    fireEvent.click(submitBtn);
    expect(screen.getByText('Password must be at least 12 characters')).toBeInTheDocument();
  });

  it('hides add link button when at max links', () => {
    useUserStore.setState({
      user: {
        ...mockUser,
        display_name: 'Test Display',
        bio: 'Test bio here',
        links: [
          'https://a.com',
          'https://b.com',
          'https://c.com',
          'https://d.com',
          'https://e.com',
        ],
      },
    });
    render(<AccountSection />);
    expect(screen.queryByText('+ Add link')).not.toBeInTheDocument();
    expect(screen.getByText('5/5 links')).toBeInTheDocument();
  });

  // --- Header image tests ---

  it('renders header image upload area', () => {
    render(<AccountSection />);
    expect(screen.getByLabelText('Upload header image')).toBeInTheDocument();
  });

  it('shows placeholder text when no header image', () => {
    render(<AccountSection />);
    expect(screen.getByText('Click to upload a banner image')).toBeInTheDocument();
  });

  it('renders header image preview when header URL exists', () => {
    useUserStore.setState({
      user: {
        ...mockUser,
        header_image_url: 'data:image/png;base64,abc123',
        display_name: 'Test Display',
        bio: 'Test bio here',
        links: ['https://example.com'],
      },
    });
    render(<AccountSection />);
    const img = screen.getByAltText('Header preview');
    expect(img).toBeInTheDocument();
  });

  it('renders remove banner button when header image exists', () => {
    useUserStore.setState({
      user: {
        ...mockUser,
        header_image_url: 'data:image/png;base64,abc123',
        display_name: 'Test Display',
        bio: 'Test bio here',
        links: [],
      },
    });
    render(<AccountSection />);
    expect(screen.getByText('Remove banner')).toBeInTheDocument();
  });

  it('handles remove banner click', () => {
    useUserStore.setState({
      user: {
        ...mockUser,
        header_image_url: 'data:image/png;base64,abc123',
        display_name: 'Test Display',
        bio: 'Test bio here',
        links: [],
      },
    });
    render(<AccountSection />);
    fireEvent.click(screen.getByText('Remove banner'));
    // After removing, placeholder should show
    expect(screen.getByText('Click to upload a banner image')).toBeInTheDocument();
  });
});
