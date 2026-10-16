import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { act, fireEvent, render, screen, waitFor, within } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import SettingsPage from '@/renderer/components/Settings/SettingsPage';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import {
  captureRuntimeServerSelection,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';

const API_BASE = 'http://localhost:8080/api/v1';
const TOKEN = 'mock-token';
const patches: Array<{ body: unknown; authorization: string | null }> = [];
let confirmedPrivacy: Record<string, unknown>;
let patchFailure: 'refused' | 'network' | null = null;

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
afterEach(() => {
  usePrivacyStore.getState().clearPrivacy();
  vi.clearAllTimers();
  vi.useRealTimers();
  server.resetHandlers();
});

beforeEach(() => {
  resetAllStores();
  setRuntimeServerBase('http://localhost:8080');
  useAuthStore.getState().setAccessToken(TOKEN);
  useAuthStore.setState({ sessionId: 'privacy-repro-session' });
  useUserStore.setState({
    user: { id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', username: 'pilot' },
  });
  useClientConfigStore.getState().setActivityHistoryCapability({ status: 'confirmed-unsupported' });
  patches.length = 0;
  patchFailure = null;
  confirmedPrivacy = {
    messages_friends_only: true,
    messages_server_members: true,
    dm_privacy_level: 2,
    dm_friends_of_friends: false,
    auto_accept_friend_codes: false,
    searchable_by_username: false,
    searchable_by_email: false,
    searchable_by_phone: false,
    allow_embedded_content: false,
    load_gifs_automatically: false,
    share_personalization_with_gif_provider: true,
    require_auth_before_purge: true,
    allow_friend_requests_from: 'everyone',
  };
  server.use(
    http.get(`${API_BASE}/users/me/privacy`, () =>
      HttpResponse.json({ privacy: confirmedPrivacy })
    ),
    http.patch(`${API_BASE}/users/me/privacy`, async ({ request }) => {
      const body = await request.json();
      patches.push({ body, authorization: request.headers.get('Authorization') });
      if (patchFailure === 'refused') {
        return HttpResponse.json({ error: 'Privacy change refused' }, { status: 403 });
      }
      if (patchFailure === 'network') return HttpResponse.error();
      confirmedPrivacy = { ...confirmedPrivacy, ...(body as Record<string, unknown>) };
      return HttpResponse.json({ privacy: confirmedPrivacy });
    }),
    http.post(`${API_BASE}/klipy/customer-id`, () =>
      HttpResponse.json({ customer_id: 'mock-klipy-customer-id' })
    ),
    http.get(`${API_BASE}/friends`, () => HttpResponse.json({ friends: [] })),
    http.get(`${API_BASE}/users/me/presence-settings`, () =>
      HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' })
    ),
    http.get(
      `${API_BASE}/users/me/presence-history/settings`,
      () => new HttpResponse(null, { status: 404 })
    ),
    http.get(
      `${API_BASE}/users/me/presence-history`,
      () => new HttpResponse(null, { status: 404 })
    ),
    http.get(`${API_BASE}/sessions`, () =>
      HttpResponse.json({ sessions: [], past_sessions: [], revocation_mode: 'secure' })
    ),
    http.get(`${API_BASE}/mfa/status`, () =>
      HttpResponse.json({
        methods: [],
        recovery_only_methods: [],
        recovery_hardened: false,
        backup_codes_remaining: 0,
        backup_email: '',
      })
    ),
    http.get(`${API_BASE}/mfa/webauthn/credentials`, () => HttpResponse.json({ credentials: [] })),
    http.get(`${API_BASE}/users/me/security`, () =>
      HttpResponse.json({ password_login_disabled: false, trust_sso_security: false })
    ),
    http.get(`${API_BASE}/users/me/sso-identities`, () => HttpResponse.json({ identities: [] })),
    http.get(`${API_BASE}/mfa/recovery-key`, () => HttpResponse.json({ enabled: false })),
    http.get(`${API_BASE}/mfa/trusted-devices`, () => HttpResponse.json({ devices: [] })),
    http.get(`${API_BASE}/mfa/recovery-circle`, () => HttpResponse.json({ members: [] })),
    http.get(`${API_BASE}/mfa/recovery-requests`, () => HttpResponse.json({ requests: [] }))
  );
});

function authSnapshot() {
  const { accessToken, authGeneration, sessionId } = useAuthStore.getState();
  return { accessToken, authGeneration, sessionId };
}

const cases = [
  {
    label: 'DM privacy',
    group: 'Who Can DM You',
    current: 'Friends + Server',
    body: { dm_privacy_level: 0 },
  },
  {
    label: 'friend requests',
    group: 'Who Can Send You Friend Requests',
    current: 'Everyone',
    body: { allow_friend_requests_from: 'nobody' },
  },
] as const;

async function selectStrictPrivacy(groupName: string, current: string) {
  render(<SettingsPage />);
  fireEvent.click(screen.getByRole('button', { name: 'Privacy', exact: true }));
  const disclosure = document.getElementById('section-privacy-settings');
  expect(disclosure).toBeInstanceOf(HTMLDetailsElement);
  const summary = disclosure?.querySelector('summary');
  if (!summary) throw new Error('Real Privacy disclosure has no summary');
  if (!(disclosure as HTMLDetailsElement).open) fireEvent.click(summary);
  const group = await screen.findByRole('group', { name: groupName, exact: true });
  await waitFor(() =>
    expect(within(group).getByRole('button', { name: current, exact: true })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
  );
  const picked = within(group).getByRole('button', { name: 'No One', exact: true });
  expect(picked).toBeVisible();
  expect(picked).toBeEnabled();
  const originalAuth = authSnapshot();
  const originalServer = captureRuntimeServerSelection();
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  fireEvent.click(picked);
  expect(picked).toHaveAttribute('aria-pressed', 'true');
  expect(patches).toEqual([]);
  return { originalAuth, originalServer };
}

describe('privacy selections across Settings panes', () => {
  it.each(cases)(
    'control: $label sends the real PATCH while the pane stays mounted',
    async ({ group, current, body }) => {
      const { originalAuth, originalServer } = await selectStrictPrivacy(group, current);
      await act(async () => vi.advanceTimersByTimeAsync(350));
      vi.useRealTimers();
      await waitFor(() => expect(patches).toEqual([{ body, authorization: `Bearer ${TOKEN}` }]));
      expect(authSnapshot()).toEqual(originalAuth);
      expect(captureRuntimeServerSelection()).toEqual(originalServer);
      expect(confirmedPrivacy).toMatchObject(body);
    }
  );

  it.each(cases)(
    'regression: $label still sends the real PATCH after immediately choosing Security',
    async ({ group, current, body }) => {
      const { originalAuth, originalServer } = await selectStrictPrivacy(group, current);
      await act(async () =>
        fireEvent.click(screen.getByRole('button', { name: 'Security', exact: true }))
      );
      expect(document.getElementById('section-privacy-settings')).toBeNull();
      expect(document.getElementById('section-password')).toBeInstanceOf(HTMLDetailsElement);
      expect(screen.getByRole('button', { name: 'Security', exact: true })).toHaveAttribute(
        'aria-current',
        'page'
      );
      await act(async () => vi.advanceTimersByTimeAsync(350));
      vi.useRealTimers();
      // Establish unchanged ownership BEFORE the expected failing assertion: this
      // is a pane pivot, not an account/server/logout cancellation scenario.
      expect(authSnapshot()).toEqual(originalAuth);
      expect(captureRuntimeServerSelection()).toEqual(originalServer);
      await waitFor(() => expect(patches).toEqual([{ body, authorization: `Bearer ${TOKEN}` }]));
      expect(confirmedPrivacy).toMatchObject(body);
      await returnToPrivacy();
      await waitFor(() =>
        expect(
          within(screen.getByRole('group', { name: group, exact: true })).getByRole('button', {
            name: 'No One',
            exact: true,
          })
        ).toHaveAttribute('aria-pressed', 'true')
      );
    }
  );

  it('coalesces rapid selections independently while the pane changes', async () => {
    await selectStrictPrivacy('Who Can DM You', 'Friends + Server');
    const dm = screen.getByRole('group', { name: 'Who Can DM You', exact: true });
    const friends = screen.getByRole('group', {
      name: 'Who Can Send You Friend Requests',
      exact: true,
    });
    fireEvent.click(within(dm).getByRole('button', { name: 'Everyone', exact: true }));
    fireEvent.click(within(dm).getByRole('button', { name: 'Friends', exact: true }));
    fireEvent.click(within(friends).getByRole('button', { name: 'No One', exact: true }));
    fireEvent.click(within(friends).getByRole('button', { name: 'Mutual Servers', exact: true }));
    expect(within(friends).getByText(/share a server or a friend with you/)).toBeInTheDocument();
    expect(
      within(friends).getByText(/Someone with a valid friend code can too/)
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Security', exact: true }));
    await act(async () => vi.advanceTimersByTimeAsync(350));
    vi.useRealTimers();
    await waitFor(() => expect(patches).toHaveLength(2));
    expect(patches.map(({ body }) => body)).toEqual(
      expect.arrayContaining([
        { dm_privacy_level: 1 },
        { allow_friend_requests_from: 'mutual_servers' },
      ])
    );
    await returnToPrivacy();
    await waitFor(() =>
      expect(
        within(screen.getByRole('group', { name: 'Who Can DM You', exact: true })).getByRole(
          'button',
          { name: 'Friends', exact: true }
        )
      ).toHaveAttribute('aria-pressed', 'true')
    );
    await waitFor(() =>
      expect(
        within(
          screen.getByRole('group', { name: 'Who Can Send You Friend Requests', exact: true })
        ).getByRole('button', { name: 'Mutual Servers', exact: true })
      ).toHaveAttribute('aria-pressed', 'true')
    );
    expect(patches).toHaveLength(2);
  });

  it.each([
    {
      group: 'Who Can DM You',
      current: 'Friends + Server',
      selected: 'Friends',
      body: { dm_privacy_level: 1 },
    },
    {
      group: 'Who Can Send You Friend Requests',
      current: 'Everyone',
      selected: 'Mutual Servers',
      body: { allow_friend_requests_from: 'mutual_servers' },
    },
  ])(
    'saves the $group range-input selection through the real caller',
    async ({ group, current, selected, body }) => {
      await selectStrictPrivacy(group, current);
      const control = screen.getByRole('group', { name: group, exact: true });
      fireEvent.change(within(control).getByRole('slider'), { target: { value: '1' } });
      expect(within(control).getByRole('button', { name: selected, exact: true })).toHaveAttribute(
        'aria-pressed',
        'true'
      );
      expect(patches).toEqual([]);
      await act(async () => vi.advanceTimersByTimeAsync(350));
      vi.useRealTimers();
      await waitFor(() => expect(patches).toEqual([{ body, authorization: `Bearer ${TOKEN}` }]));
      await waitFor(() => expect(confirmedPrivacy).toMatchObject(body));
      expect(patches).toHaveLength(1);
    }
  );

  describe.each(['refused', 'network'] as const)('%s after leaving Privacy', (failure) => {
    it.each(cases)(
      '$label shows the confirmed choice and its error on return',
      async ({ group, current, body }) => {
        patchFailure = failure;
        await selectStrictPrivacy(group, current);
        fireEvent.click(screen.getByRole('button', { name: 'Security', exact: true }));
        await act(async () => vi.advanceTimersByTimeAsync(350));
        vi.useRealTimers();
        await waitFor(() => expect(patches).toEqual([{ body, authorization: `Bearer ${TOKEN}` }]));
        await returnToPrivacy();
        await waitFor(() =>
          expect(
            within(screen.getByRole('group', { name: group, exact: true })).getByRole('alert')
          ).toHaveTextContent(
            failure === 'refused' ? 'Privacy change refused' : /failed|fetch|network/i
          )
        );
        const returned = screen.getByRole('group', { name: group, exact: true });
        await waitFor(() =>
          expect(
            within(returned).getByRole('button', { name: current, exact: true })
          ).toHaveAttribute('aria-pressed', 'true')
        );
        expect(
          within(returned).getByRole('button', { name: 'No One', exact: true })
        ).toHaveAttribute('aria-pressed', 'false');
        expect(patches).toHaveLength(1);

        // A deliberate new selection can recover; returning to the pane must not
        // replay the failed request on the user's behalf.
        patchFailure = null;
        vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
        fireEvent.click(within(returned).getByRole('button', { name: 'No One', exact: true }));
        await act(async () => vi.advanceTimersByTimeAsync(350));
        vi.useRealTimers();
        await waitFor(() =>
          expect(
            within(screen.getByRole('group', { name: group, exact: true })).getByRole('button', {
              name: 'No One',
              exact: true,
            })
          ).toHaveAttribute('aria-pressed', 'true')
        );
        await waitFor(() => expect(confirmedPrivacy).toMatchObject(body));
        await waitFor(() => expect(patches).toHaveLength(2));
        expect(
          within(screen.getByRole('group', { name: group, exact: true })).queryByRole('alert')
        ).toBeNull();
      }
    );
  });
});

async function returnToPrivacy() {
  fireEvent.click(screen.getByRole('button', { name: 'Privacy', exact: true }));
  const disclosure = document.getElementById('section-privacy-settings') as HTMLDetailsElement;
  expect(disclosure).toBeInstanceOf(HTMLDetailsElement);
  const summary = disclosure.querySelector('summary');
  if (!summary) throw new Error('Real Privacy disclosure has no summary');
  if (!disclosure.open) fireEvent.click(summary);
  await screen.findByRole('group', { name: 'Who Can DM You', exact: true });
}
