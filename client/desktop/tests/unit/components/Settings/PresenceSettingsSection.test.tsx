import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useFriendStore, type Friend } from '@/renderer/stores/chat/friendStore';
import { useRichPresenceStore } from '@/renderer/stores/ui/richPresenceStore';
import { presenceOverrideSyncService } from '@/renderer/services/system/presenceOverrideSync';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import PresenceSettingsSection from '@/renderer/components/Settings/PresenceSettingsSection';

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    encryptPreferences: vi.fn().mockResolvedValue('encrypted-exceptions'),
    decryptPreferences: vi.fn(),
  },
}));

const API_BASE = 'http://localhost:8080';
const PRESENCE_PATH = `${API_BASE}/api/v1/users/me/presence-settings`;
const OVERRIDE_PATH = `${API_BASE}/api/v1/users/me/presence-overrides/custom_text`;
const UUID_A = '11111111-1111-4111-8111-111111111111';

const friend = (userId: string, username: string, displayName: string): Friend => ({
  id: `friendship-${username}`,
  userId,
  username,
  displayName,
  status: 'online',
});

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

describe('PresenceSettingsSection', () => {
  beforeEach(() => {
    resetAllStores();
    presenceOverrideSyncService.reset();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useFriendStore.setState({ friendsHydrated: true, isLoading: false, error: null });
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

  it('PATCHes only the tier endpoint when an option is selected', async () => {
    let received: Record<string, unknown> | null = null;
    let overridePuts = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ custom_text_tier: 1, custom_text: '', custom_text_emoji: '' });
      }),
      http.put(OVERRIDE_PATH, () => {
        overridePuts += 1;
        return HttpResponse.json({ version: 1 });
      })
    );

    render(<PresenceSettingsSection />);

    fireEvent.click(screen.getByRole('button', { name: 'Friends' }));

    await waitFor(() => expect(received).toEqual({ custom_text_tier: 1 }));
    expect(useRichPresenceStore.getState().self.tier).toBe(1);
    expect(overridePuts).toBe(0);
  });

  it('renders exceptions beneath the tier row', () => {
    render(<PresenceSettingsSection />);

    const tierRow = screen.getByText('Who Can See Your Custom Status').closest('.settings-row');
    const exceptions = screen.getByText('Exceptions - 0 people').closest('details');
    expect(tierRow).toBeInTheDocument();
    expect(exceptions).toBeInTheDocument();
    expect(tierRow?.compareDocumentPosition(exceptions!)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
  });

  it('does not mount Activity History in Custom Status', () => {
    render(<PresenceSettingsSection />);
    expect(screen.queryByRole('heading', { name: 'Activity History' })).not.toBeInTheDocument();
  });

  it('PUTs only the override endpoint when exceptions are saved', async () => {
    useFriendStore.setState({ friends: [friend(UUID_A, 'alex', 'Alex Rivera')] });
    let overridePuts = 0;
    let tierPatches = 0;
    let overrideBody: unknown;
    mswServer.use(
      http.put(OVERRIDE_PATH, async ({ request }) => {
        overridePuts += 1;
        overrideBody = await request.json();
        return HttpResponse.json({ version: 1 });
      }),
      http.patch(PRESENCE_PATH, () => {
        tierPatches += 1;
        return HttpResponse.json({ custom_text_tier: 1 });
      })
    );
    render(<PresenceSettingsSection />);

    fireEvent.click(screen.getByText('Exceptions - 0 people'));
    fireEvent.click(screen.getByRole('button', { name: 'Add exceptions' }));
    fireEvent.click(screen.getByRole('checkbox', { name: /Alex Rivera/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));

    await waitFor(() => expect(overridePuts).toBe(1));
    expect(tierPatches).toBe(0);
    expect(await screen.findByText('Saved Custom Status exceptions.')).toBeInTheDocument();
    expect(overrideBody).toEqual({
      encrypted_data: 'encrypted-exceptions',
      expected_version: 0,
      excluded_user_ids: [UUID_A],
    });
  });

  it('unmounts the exception dialog before opening categories and returns focus after close', async () => {
    render(<PresenceSettingsSection />);
    fireEvent.click(screen.getByText('Exceptions - 0 people'));
    const add = screen.getByRole('button', { name: 'Add exceptions' });
    fireEvent.click(add);

    expect(screen.getByRole('dialog', { name: 'Custom Status exceptions' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Manage categories' }));

    expect(await screen.findByRole('dialog', { name: 'Manage Categories' })).toBeInTheDocument();
    expect(
      screen.queryByRole('dialog', { name: 'Custom Status exceptions' })
    ).not.toBeInTheDocument();
    expect(screen.getAllByRole('dialog')).toHaveLength(1);

    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(add).toHaveFocus());
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
