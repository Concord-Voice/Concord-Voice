import { render, screen, within } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';

vi.mock('@/renderer/hooks/useAgeStatus', () => ({
  useAgeStatus: () => ({ nsfwAuth: 'unknown' }),
}));

vi.mock('@/renderer/components/Profile/PresenceHistorySection', () => ({
  default: ({ userId }: { userId: string | null }) => (
    <section data-testid="presence-history-section" data-user-id={userId ?? ''}>
      <h3>Activity History</h3>
    </section>
  ),
}));

import AccountSection from '@/renderer/components/Settings/AccountSection';

describe('AccountSection', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('renders the NSFW Content Access collapsible section containing the gate', () => {
    render(<AccountSection />);
    expect(screen.getByText('NSFW Content Access')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /verify age/i })).toBeInTheDocument();
  });

  it('renders the My Profile subsection with both forms (#1773)', () => {
    render(<AccountSection />);
    expect(screen.getByText('My Profile')).toBeInTheDocument();
    expect(screen.getByText('Profile Information')).toBeInTheDocument();
    // "Change Password" appears as the section h2 title AND the submit button.
    expect(screen.getAllByText('Change Password').length).toBeGreaterThanOrEqual(2);
  });

  it('mounts self-only Activity History below the profile forms with the current user ID', () => {
    useUserStore.setState({
      user: { id: 'user-self', username: 'self' },
      isLoading: false,
    });

    render(<AccountSection />);

    const profileDetails = screen.getByText('My Profile').closest('details');
    expect(profileDetails).not.toBeNull();
    const history = within(profileDetails as HTMLElement).getByTestId('presence-history-section');
    expect(history).toHaveAttribute('data-user-id', 'user-self');
    expect(profileDetails).toContainElement(history);
    expect(
      within(profileDetails as HTMLElement).getByRole('heading', { name: 'Activity History' })
    ).toBeInTheDocument();
  });
});
