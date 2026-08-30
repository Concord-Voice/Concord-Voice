import { render, screen } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';

vi.mock('@/renderer/hooks/ui/useAgeStatus', () => ({
  useAgeStatus: () => ({ state: 'unverified' }),
}));

vi.mock('@/renderer/components/Profile/PresenceHistorySection', () => ({
  default: ({ userId }: { userId: string | null }) => (
    <section aria-label="Self Activity History" data-user-id={userId ?? ''} />
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

  it('renders profile, password, and NSFW content as distinct ordered sections', () => {
    render(<AccountSection />);
    const account = document.getElementById('section-profile')!;
    const password = document.getElementById('section-password')!;
    const nsfw = document.getElementById('section-nsfw-content')!;
    expect(account).not.toBeNull();
    expect(password).not.toBeNull();
    expect(nsfw).not.toBeNull();
    expect(
      account.compareDocumentPosition(password) & Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy();
    expect(password.compareDocumentPosition(nsfw) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(document.getElementById('section-subscription')).toBeNull();
    expect(screen.queryByLabelText('Self Activity History')).not.toBeInTheDocument();
  });
});
