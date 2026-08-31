import { act, render, screen, fireEvent, waitFor, within } from '../../../test-utils';
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

const presenceSettingsResponse = (overrides: Record<string, unknown> = {}) => ({
  master_enabled: true,
  server_voice_tier: 1,
  server_voice_show_details: true,
  private_call_tier: 0,
  private_call_show_details: false,
  custom_text_tier: 0,
  custom_text: null,
  custom_text_emoji: null,
  ...overrides,
});

const friend = (userId: string, username: string, displayName: string): Friend => ({
  id: `friendship-${username}`,
  userId,
  username,
  displayName,
  status: 'online',
});

const privateCallScope = () => {
  const heading = screen.getByRole('heading', { name: 'Private Call' });
  return heading.closest('article, section, [role="group"]') ?? heading.parentElement!;
};

const customStatusScope = () => screen.getByRole('group', { name: 'Custom status visibility' });

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
    mswServer.use(http.get(PRESENCE_PATH, () => HttpResponse.json(presenceSettingsResponse())));
  });

  it('renders the three visibility options', async () => {
    render(<PresenceSettingsSection />);

    await waitFor(() => expect(useRichPresenceStore.getState().self.tier).toBe(0));

    const customStatus = within(customStatusScope());
    expect(customStatus.getByRole('button', { name: 'Off' })).toBeInTheDocument();
    expect(customStatus.getByRole('button', { name: 'Friends' })).toBeInTheDocument();
    expect(customStatus.getByRole('button', { name: 'Servers' })).toBeInTheDocument();
  });

  it('explains the eligible friends-of-friends audience for Friends and Servers', async () => {
    mswServer.use(
      http.get(PRESENCE_PATH, () =>
        HttpResponse.json(presenceSettingsResponse({ custom_text_tier: 1 }))
      ),
      http.patch(PRESENCE_PATH, () =>
        HttpResponse.json(presenceSettingsResponse({ custom_text_tier: 2 }))
      )
    );
    render(<PresenceSettingsSection />);

    const customStatus = within(customStatusScope());
    expect(
      await screen.findByText(
        'Only your friends and eligible friends-of-friends can see your custom status.'
      )
    ).toBeInTheDocument();

    fireEvent.click(await customStatus.findByRole('button', { name: 'Servers' }));
    expect(
      await screen.findByText(
        'Your friends, eligible friends-of-friends, and members of servers you share can see your custom status.'
      )
    ).toBeInTheDocument();
  });

  it('PATCHes only the tier endpoint when an option is selected', async () => {
    let received: Record<string, unknown> | null = null;
    let overridePuts = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(presenceSettingsResponse({ custom_text_tier: 1 }));
      }),
      http.put(OVERRIDE_PATH, () => {
        overridePuts += 1;
        return HttpResponse.json({ version: 1 });
      })
    );

    render(<PresenceSettingsSection />);

    const customStatus = within(customStatusScope());
    await waitFor(() =>
      expect(customStatus.getByRole('button', { name: 'Friends', pressed: false })).toBeEnabled()
    );
    fireEvent.click(customStatus.getByRole('button', { name: 'Friends', pressed: false }));

    await waitFor(() => expect(useRichPresenceStore.getState().self.tier).toBe(1));
    expect(received).toEqual({ custom_text_tier: 1 });
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
        return HttpResponse.json(presenceSettingsResponse({ custom_text_tier: 1 }));
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
        HttpResponse.json(presenceSettingsResponse({ custom_text_tier: 2 }))
      )
    );

    render(<PresenceSettingsSection />);

    await waitFor(() =>
      expect(within(customStatusScope()).getByRole('button', { name: 'Servers' })).toHaveAttribute(
        'aria-pressed',
        'true'
      )
    );
  });

  it('renders Rich Presence controls with the exact activity and tier labels', async () => {
    render(<PresenceSettingsSection />);

    expect(await screen.findByRole('heading', { name: 'Rich Presence' })).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Share Rich Presence' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Server Voice' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Private Call' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Friends in server' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'All in server' })).toBeInTheDocument();
    const privateCall = within(privateCallScope());
    expect(privateCall.getByRole('button', { name: 'Friends' })).toBeInTheDocument();
    expect(privateCall.getByRole('button', { name: 'Servers' })).toBeInTheDocument();
    expect(privateCall.getByRole('switch', { name: /Private Call.*details/i })).toBeInTheDocument();
  });

  it('shows authoritative defaults and truthful previews after hydration', async () => {
    render(<PresenceSettingsSection />);

    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Friends in server' })).toHaveAttribute(
        'aria-pressed',
        'true'
      )
    );
    expect(screen.getByText(/Friends—and eligible friends-of-friends/)).toBeInTheDocument();
    expect(screen.getByText('In voice in #General on Concord')).toBeInTheDocument();
    expect(screen.getByText('People currently in this private call.')).toBeInTheDocument();
    expect(screen.getByText('In a private call')).toBeInTheDocument();
    expect(screen.queryByText('Nobody')).not.toBeInTheDocument();
  });

  it.each([
    {
      name: 'Server Voice Off',
      settings: { server_voice_tier: 0, server_voice_show_details: false },
      card: 'Server Voice',
      audience: 'Nobody',
      preview: 'Nothing is broadcast.',
    },
    {
      name: 'Server Voice All in server',
      settings: { server_voice_tier: 2, server_voice_show_details: true },
      card: 'Server Voice',
      audience: 'People in this server who can view this voice channel.',
      preview: 'In voice in #General on Concord',
    },
    {
      name: 'Private Call Friends',
      settings: { private_call_tier: 1, private_call_show_details: false },
      card: 'Private Call',
      audience: 'People currently in this call, plus your friends and eligible friends-of-friends.',
      preview: 'In a private call',
    },
    {
      name: 'Private Call Servers',
      settings: { private_call_tier: 2, private_call_show_details: true },
      card: 'Private Call',
      audience:
        'People currently in this call, plus your friends, eligible friends-of-friends, and people who share a server with you.',
      preview: 'In a group private call (participant count shown)',
    },
  ])('maps the $name preview without counts', async ({ settings, card, audience, preview }) => {
    mswServer.resetHandlers();
    mswServer.use(
      http.get(PRESENCE_PATH, () => HttpResponse.json(presenceSettingsResponse(settings)))
    );
    const { unmount } = render(<PresenceSettingsSection />);
    const cardScope = within(screen.getByRole('heading', { name: card }).closest('section')!);
    await waitFor(() => expect(cardScope.getByText(audience)).toBeInTheDocument());
    expect(cardScope.getByText(preview)).toBeInTheDocument();
    expect(screen.queryByText(/participant count:|\(\d+\)/i)).not.toBeInTheDocument();
    unmount();
  });

  it('explains that Private Call Off still exposes presence to current participants', async () => {
    render(<PresenceSettingsSection />);

    const privateCall = within(privateCallScope());
    expect(
      await privateCall.findByText('People currently in this private call.')
    ).toBeInTheDocument();
    expect(privateCall.queryByText('Nobody')).not.toBeInTheDocument();
  });

  it.each([0, 1, 2] as const)(
    'shows the Private Call server-sharing warning at tier %s',
    async (tier) => {
      mswServer.resetHandlers();
      mswServer.use(
        http.get(PRESENCE_PATH, () =>
          HttpResponse.json(presenceSettingsResponse({ private_call_tier: tier }))
        )
      );
      render(<PresenceSettingsSection />);
      const privateCall = within(privateCallScope());
      await waitFor(() =>
        expect(privateCall.getByRole('button', { name: 'Servers' })).toHaveAttribute(
          'aria-pressed',
          tier === 2 ? 'true' : 'false'
        )
      );
      const warningText = screen.queryByText(/Choosing Servers lets people who share a server/);
      const warning = warningText?.closest('p');
      expect(warning).toBeInTheDocument();
      expect(warning).toHaveAttribute('id', 'private-call-servers-warning');
      expect(privateCall.getByRole('button', { name: 'Servers' })).toHaveAttribute(
        'aria-describedby',
        'private-call-servers-warning'
      );
    }
  );

  it('keeps category choices enabled and previews truthful when the master switch is off', async () => {
    let body: Record<string, unknown> | undefined;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(presenceSettingsResponse({ master_enabled: false }));
      })
    );
    render(<PresenceSettingsSection />);

    const master = await screen.findByRole('switch', { name: 'Share Rich Presence' });
    await waitFor(() => expect(master).toBeEnabled());
    fireEvent.click(master);

    await waitFor(() => expect(body).toEqual({ master_enabled: false }));
    await waitFor(() => expect(master).not.toBeChecked());
    expect(screen.getAllByRole('button', { name: 'Friends in server' })[0]).toBeEnabled();
    expect(screen.getAllByText('Nobody')).toHaveLength(2);
    expect(screen.getAllByText('Nothing is broadcast.')).toHaveLength(2);
  });

  it('sends isolated category and detail PATCH bodies through the settings endpoint', async () => {
    const bodies: Record<string, unknown>[] = [];
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json(presenceSettingsResponse());
      })
    );
    render(<PresenceSettingsSection />);

    const allInServer = await screen.findByRole('button', { name: 'All in server' });
    await waitFor(() => expect(allInServer).toBeEnabled());
    fireEvent.click(allInServer);
    await waitFor(() => expect(bodies).toContainEqual({ server_voice_tier: 2 }));
    const serverDetails = screen.getByRole('switch', { name: /Server Voice.*details/i });
    await waitFor(() => expect(serverDetails).toBeEnabled());
    fireEvent.click(serverDetails);
    await waitFor(() => expect(bodies).toContainEqual({ server_voice_show_details: false }));
  });

  it('activates Private Call Servers directly and keeps its warning adjacent', async () => {
    let body: Record<string, unknown> | undefined;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(presenceSettingsResponse({ private_call_tier: 2 }));
      })
    );
    render(<PresenceSettingsSection />);

    const servers = within(privateCallScope()).getByRole('button', { name: 'Servers' });
    await waitFor(() => expect(servers).toBeEnabled());
    fireEvent.click(servers);
    await waitFor(() => expect(body).toEqual({ private_call_tier: 2 }));
    expect(screen.getByText(/Choosing Servers lets people who share a server/)).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('keeps controls unavailable during initial hydration', async () => {
    let resolveGet!: (response: Response) => void;
    const pendingGet = new Promise<Response>((resolve) => {
      resolveGet = resolve;
    });
    mswServer.use(http.get(PRESENCE_PATH, () => pendingGet));
    render(<PresenceSettingsSection />);

    expect(screen.getByRole('switch', { name: 'Share Rich Presence' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Friends in server' })).toBeDisabled();
    resolveGet(new Response(JSON.stringify(presenceSettingsResponse()), { status: 200 }));
    await screen.findByRole('button', { name: 'Friends in server', pressed: true });
  });

  it('offers one hydration retry after a failed or malformed GET', async () => {
    let attempts = 0;
    mswServer.use(
      http.get(PRESENCE_PATH, () => {
        attempts += 1;
        return attempts === 1
          ? HttpResponse.json({ error: 'load failed' }, { status: 500 })
          : HttpResponse.json(presenceSettingsResponse());
      })
    );
    render(<PresenceSettingsSection />);

    expect(await screen.findByRole('alert')).toHaveTextContent('load failed');
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(attempts).toBe(2));
    expect(
      await screen.findByRole('button', { name: 'Friends in server', pressed: true })
    ).toBeInTheDocument();
  });

  it('fails closed after hydration failure until an explicit retry succeeds', async () => {
    let attempts = 0;
    const seeded = useRichPresenceStore.getState().presenceSettings;
    useRichPresenceStore.setState({ confirmedPresenceSettings: seeded });
    mswServer.resetHandlers();
    mswServer.use(
      http.get(PRESENCE_PATH, () => {
        attempts += 1;
        return attempts === 1
          ? HttpResponse.json({ error: 'load failed' }, { status: 500 })
          : HttpResponse.json(presenceSettingsResponse({ custom_text_tier: 2 }));
      })
    );

    render(<PresenceSettingsSection />);

    expect(await screen.findByRole('alert')).toHaveTextContent('load failed');
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Share Rich Presence' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Friends in server' })).toBeDisabled();

    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(attempts).toBe(2));
    await waitFor(() =>
      expect(useRichPresenceStore.getState().confirmedPresenceSettings).not.toBeNull()
    );
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'All in server' })).toBeEnabled()
    );
    expect(within(customStatusScope()).getByRole('button', { name: 'Servers' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });

  it('disables hydration retry while a presence settings save is pending', async () => {
    let getCount = 0;
    mswServer.resetHandlers();
    mswServer.use(
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return HttpResponse.json(presenceSettingsResponse());
      })
    );
    useRichPresenceStore.setState({
      confirmedPresenceSettings: null,
      presenceSettingsError: 'Failed to load Rich Presence settings',
      presenceSettingsSaving: true,
    });

    render(<PresenceSettingsSection />);

    const retry = screen.getByRole('button', { name: 'Try again' });
    expect(retry).toBeDisabled();
    fireEvent.click(retry);
    expect(getCount).toBe(0);

    useRichPresenceStore.setState({ presenceSettingsSaving: false });
    await waitFor(() => expect(retry).toBeEnabled());
    fireEvent.click(retry);
    fireEvent.click(retry);
    await waitFor(() => expect(getCount).toBe(1));
  });

  it('disables hydration retry while custom status reconciliation is pending', async () => {
    let getCount = 0;
    mswServer.resetHandlers();
    mswServer.use(
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return HttpResponse.json(presenceSettingsResponse());
      })
    );
    useAuthStore.getState().setAccessToken(null);
    useRichPresenceStore.setState({
      confirmedPresenceSettings: null,
      presenceSettingsError: 'Failed to load Rich Presence settings',
      customStatusSaving: true,
    });

    render(<PresenceSettingsSection />);

    const retry = screen.getByRole('button', { name: 'Try again' });
    expect(retry).toBeDisabled();
    fireEvent.click(retry);
    expect(getCount).toBe(0);
    useRichPresenceStore.setState({ customStatusSaving: false });
    await waitFor(() => expect(retry).toBeEnabled());
    fireEvent.click(retry);
    await waitFor(() => expect(getCount).toBe(1));
  });

  it('treats malformed hydration JSON as a retryable load error', async () => {
    mswServer.use(http.get(PRESENCE_PATH, () => HttpResponse.json({ custom_text_tier: 0 })));
    render(<PresenceSettingsSection />);

    expect(await screen.findByRole('alert')).toHaveTextContent(/invalid|load/i);
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });

  it('changes only the example output when a category detail toggle changes', async () => {
    mswServer.use(
      http.get(PRESENCE_PATH, () =>
        HttpResponse.json(presenceSettingsResponse({ server_voice_show_details: false }))
      ),
      http.patch(PRESENCE_PATH, () =>
        HttpResponse.json(presenceSettingsResponse({ server_voice_show_details: true }))
      )
    );
    render(<PresenceSettingsSection />);

    const audience = await screen.findByText(/Friends—and eligible friends-of-friends/);
    expect(screen.getByText('In voice')).toBeInTheDocument();
    const serverDetails = screen.getByRole('switch', { name: /Server Voice.*details/i });
    await waitFor(() => expect(serverDetails).toBeEnabled());
    fireEvent.click(serverDetails);
    await screen.findByText('In voice in #General on Concord');
    expect(audience).toBeInTheDocument();
  });

  it('reconciles a 5xx PATCH and recovers the controls', async () => {
    mswServer.use(
      http.get(PRESENCE_PATH, () => HttpResponse.json(presenceSettingsResponse())),
      http.patch(PRESENCE_PATH, () => HttpResponse.json({ error: 'save failed' }, { status: 500 }))
    );
    render(<PresenceSettingsSection />);

    const friends = await screen.findByRole('button', { name: 'Friends in server' });
    const all = screen.getByRole('button', { name: 'All in server' });
    await waitFor(() => expect(all).toBeEnabled());
    fireEvent.click(all);
    expect(all).toBeDisabled();
    await waitFor(() =>
      expect(useRichPresenceStore.getState().presenceSettings.serverVoiceTier).toBe(1)
    );
    await waitFor(() => {
      expect(friends).toHaveAttribute('aria-pressed', 'true');
      expect(all).toBeEnabled();
    });
  });

  it('does not rehydrate when same-session credentials rotate', async () => {
    let getCount = 0;
    mswServer.resetHandlers();
    mswServer.use(
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return HttpResponse.json(presenceSettingsResponse());
      })
    );
    render(<PresenceSettingsSection />);

    await waitFor(() => expect(getCount).toBe(1));
    const generation = useAuthStore.getState().authGeneration;
    await act(async () => {
      expect(
        useAuthStore.getState().rotateAuthCredentials(generation, 'rotated-token', 'session-a')
      ).toBe(true);
    });
    expect(useAuthStore.getState().authGeneration).toBe(generation);
    expect(getCount).toBe(1);
  });
});
