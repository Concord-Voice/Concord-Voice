import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useRichPresenceStore } from '@/renderer/stores/richPresenceStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import PresenceSettingsSection from '@/renderer/components/Settings/PresenceSettingsSection';

const API_BASE = 'http://localhost:8080';
const PRESENCE_PATH = `${API_BASE}/api/v1/users/me/presence-settings`;

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

describe('PresenceSettingsSection', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    // Default GET handler so the mount fetch resolves.
    mswServer.use(
      http.get(PRESENCE_PATH, () =>
        HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' })
      )
    );
  });

  it('renders the three visibility options', async () => {
    render(<PresenceSettingsSection />);

    await waitFor(() => expect(useRichPresenceStore.getState().self.tier).toBe(0));

    expect(screen.getByRole('button', { name: 'Off' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Friends' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Servers' })).toBeInTheDocument();
  });

  it('PATCHes the tier when an option is selected', async () => {
    let received: Record<string, unknown> | null = null;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ custom_text_tier: 1, custom_text: '', custom_text_emoji: '' });
      })
    );

    render(<PresenceSettingsSection />);

    fireEvent.click(screen.getByRole('button', { name: 'Friends' }));

    await waitFor(() => expect(received).toEqual({ custom_text_tier: 1 }));
    expect(useRichPresenceStore.getState().self.tier).toBe(1);
  });

  it('hydrates the active tier from the GET response', async () => {
    mswServer.resetHandlers();
    mswServer.use(
      http.get(PRESENCE_PATH, () =>
        HttpResponse.json({ custom_text_tier: 2, custom_text: '', custom_text_emoji: '' })
      )
    );

    render(<PresenceSettingsSection />);

    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Servers' })).toHaveAttribute(
        'aria-pressed',
        'true'
      )
    );
  });
});
