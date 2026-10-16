import { render, screen, userEvent, within } from '../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PrivacySection from '@/renderer/components/Settings/PrivacySection';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';

const API_BASE = 'http://localhost:8080/api/v1';
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());
beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useUserStore.setState({
    user: { id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', username: 'pilot' },
  });
  useClientConfigStore.getState().setActivityHistoryCapability({ status: 'confirmed-unsupported' });
  server.use(
    http.post(`${API_BASE}/klipy/customer-id`, () =>
      HttpResponse.json({ customer_id: 'mock-klipy-customer-id' })
    ),
    http.get(`${API_BASE}/friends`, () => HttpResponse.json({ friends: [] })),
    http.get(`${API_BASE}/users/me/privacy`, () => HttpResponse.json({ privacy: {} })),
    http.get(`${API_BASE}/users/me/presence-settings`, () =>
      HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' })
    ),
    http.get(
      `${API_BASE}/users/me/presence-history/settings`,
      () => new HttpResponse(null, { status: 404 })
    ),
    http.get(`${API_BASE}/users/me/presence-history`, () => new HttpResponse(null, { status: 404 }))
  );
});

it('groups privacy choices, Rich Presence and Activity History without credential or OS permission settings', async () => {
  render(<PrivacySection />);
  const privacy = document.getElementById('section-privacy-settings') as HTMLDetailsElement;
  expect(privacy).toBeInstanceOf(HTMLDetailsElement);
  await userEvent.setup().click(within(privacy).getByText('Privacy Controls'));
  expect(
    await screen.findByRole('switch', { name: /require authentication before purging/i })
  ).toBeInTheDocument();
  expect(document.getElementById('section-presence-settings')).toBeInstanceOf(HTMLDetailsElement);
  expect(document.getElementById('section-presence-history')).toBeInstanceOf(HTMLDetailsElement);
  expect(document.getElementById('section-password')).toBeNull();
  expect(document.getElementById('section-mfa')).toBeNull();
  expect(document.getElementById('section-active-sessions')).toBeNull();
  expect(document.getElementById('section-system-permissions')).toBeNull();
});
