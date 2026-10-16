import { render, screen } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';

vi.mock('@/renderer/hooks/ui/useAgeStatus', () => ({
  useAgeStatus: () => ({ state: 'unverified' }),
}));

vi.mock('@/renderer/components/Profile/PresenceHistorySection', () => ({
  default: ({ userId }: { userId: string | null }) => (
    <section aria-label="Self Activity History" data-user-id={userId ?? ''} />
  ),
}));

import AccountSection from '@/renderer/components/Settings/AccountSection';

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('AccountSection', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    server.use(
      http.get('http://localhost:8080/api/v1/subscriptions/me', () =>
        HttpResponse.json({ tier: 'free', status: 'none' })
      )
    );
  });

  it('renders the NSFW Content Access collapsible section containing the gate', () => {
    render(<AccountSection />);
    expect(screen.getByText('NSFW Content Access')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /verify age/i })).toBeInTheDocument();
  });

  it('renders profile, subscription, and NSFW content as distinct ordered sections', () => {
    render(<AccountSection />);
    const account = document.getElementById('section-profile')!;
    const subscription = document.getElementById('section-subscription')!;
    const nsfw = document.getElementById('section-nsfw-content')!;
    expect(account).not.toBeNull();
    expect(subscription).not.toBeNull();
    expect(nsfw).not.toBeNull();
    expect(
      account.compareDocumentPosition(subscription) & Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy();
    expect(
      subscription.compareDocumentPosition(nsfw) & Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy();
    expect(document.getElementById('section-password')).toBeNull();
    expect(screen.queryByLabelText('Self Activity History')).not.toBeInTheDocument();
  });

  it('renders every section collapsed on first visit (#2365)', () => {
    render(<AccountSection />);
    const ids = ['section-profile', 'section-subscription', 'section-nsfw-content'];
    for (const id of ids) {
      expect(document.getElementById(id), `${id} should be rendered`).not.toBeNull();
    }
    // Sweep every rendered section, not only the listed ids, so a new section that ships
    // open is caught too.
    const open = Array.from(document.querySelectorAll('details.settings-collapsible[open]')).map(
      (el) => el.id || '(no id)'
    );
    expect(
      open,
      `expected no Settings section to be open on first visit, but these are open: ${open.join(', ')}`
    ).toEqual([]);
  });
});
