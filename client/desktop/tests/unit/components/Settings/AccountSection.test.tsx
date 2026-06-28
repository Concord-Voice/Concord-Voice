import { render, screen } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/hooks/useAgeStatus', () => ({
  useAgeStatus: () => ({ nsfwAuth: 'unknown' }),
}));

import AccountSection from '@/renderer/components/Settings/AccountSection';

describe('AccountSection', () => {
  beforeEach(() => resetAllStores());

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
});
