/**
 * Ban and kick-with-purge are SWAP hosts of the dangerous-action step-up (#3456
 * §3.3, §3.4, D-5): on a gate refusal the `ConfirmActionModal` is replaced by
 * `DangerousActionStepUpDialog`, which re-sends the frozen request with `mfa_code`.
 * `ModerationDialog` is one component mounted by two surfaces, so every case runs
 * against both, through real inputs and the real dialog.
 */
import { render, screen, fireEvent, waitFor, within, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { server as mswServer } from '../../../mocks/server';
import { mockUser, mockServer } from '../../../mocks/fixtures';
import { http, HttpResponse } from 'msw';
import {
  CODE_LABEL,
  DIALOG_TITLE,
  ENROLMENT_REQUIRED,
  ENROLMENT_TEXT,
  FIXTURE_OTP,
  GATED_API_BASE,
  SETUP_LINK,
  codesSent,
  stubGatedRoute,
  stubStepUpRead,
  type GatedReply,
} from '../../../helpers/gatedRoute';
import { PURGE_CHECKBOX, moderationMember } from '../../../helpers/moderationPurge';
import { answerCapabilityRefresh } from '../../../helpers/capabilityRefresh';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { PIN_CLAIM_UNCONFIRMED_MESSAGE } from '@/renderer/components/Members/purgeOnModeration';
import MemberList from '@/renderer/components/Members/MemberList';
import MemberListPanel from '@/renderer/components/Servers/MemberListPanel';

// Dynamic import inside the factory: `vi.mock` is hoisted above the imports.
vi.mock('@/renderer/components/Members/MemberContextMenu', async () => {
  const { memberContextMenuDouble } = await import('../../../helpers/moderationPurge');
  return memberContextMenuDouble();
});

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

type Which = 'ban' | 'kick';

interface Host {
  name: string;
  serverId: string;
  /** The region `focusFallback` aims at; focus must stay inside it. */
  rootSelector: string;
  returnTo: unknown;
  mount: () => Promise<void>;
  open: (which: Which) => Promise<void>;
  /** Whether the roster still lists Alice; null where the roster is a prop. */
  isListed: () => boolean | null;
}

const PANEL_SERVER_ID = 's1';

const hosts: Host[] = [
  {
    name: 'MemberList',
    serverId: mockServer.id,
    rootSelector: '.member-list',
    returnTo: { kind: 'chat' },
    async mount() {
      useAuthStore.getState().setAccessToken('mock-token');
      useUserStore.setState({ user: mockUser });
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().setActiveServer(mockServer.id);
      usePermissionStore.setState({ serverPermissions: {}, serverRoles: {} });
      mswServer.use(
        http.get(`${GATED_API_BASE}/api/v1/servers/${mockServer.id}/members`, () =>
          HttpResponse.json({ members: [moderationMember] })
        )
      );
      render(<MemberList />);
      await screen.findByText('Alice');
    },
    async open(which) {
      fireEvent.contextMenu(screen.getByText('Alice'));
      await userEvent.click(screen.getByRole('button', { name: `Open ${which} dialog` }));
    },
    isListed: () =>
      useMemberStore.getState().members.some((m) => m.user_id === moderationMember.user_id),
  },
  {
    name: 'MemberListPanel',
    serverId: PANEL_SERVER_ID,
    rootSelector: '.members-list',
    returnTo: { kind: 'serverSettings', serverId: PANEL_SERVER_ID, section: 'members' },
    async mount() {
      useAuthStore.getState().setAccessToken('mock-token');
      render(
        <MemberListPanel
          members={[moderationMember]}
          assignableRoles={[]}
          onToggleRole={vi.fn()}
          serverId={PANEL_SERVER_ID}
          ownerUserId="owner-1"
        />
      );
    },
    async open(which) {
      await userEvent.click(screen.getByRole('button', { name: 'Open context menu for Alice' }));
      await userEvent.click(screen.getByRole('button', { name: `Open ${which} dialog` }));
    },
    isListed: () => null,
  },
];

const route = (host: Host, which: Which) =>
  `${GATED_API_BASE}/api/v1/servers/${host.serverId}/${which === 'ban' ? 'bans' : 'members'}/u1`;

const stubRoute = (
  host: Host,
  which: Which,
  options: { first?: GatedReply; retries?: readonly GatedReply[] } = {}
) =>
  stubGatedRoute({
    method: which === 'ban' ? 'post' : 'delete',
    url: route(host, which),
    ...options,
  });

const stepUpDialog = () => screen.getByRole('dialog', { name: DIALOG_TITLE });
const expectNoStepUpDialog = () =>
  expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();

const purged = (verb: string): GatedReply => ({
  status: 200,
  body: { message: verb, purge: { requested: true, status: 'completed', purged_count: 3 } },
});

describe.each(hosts)('moderation step-up swap on $name (#3456)', (host) => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    stubStepUpRead();
  });

  // Mutation: `isOpen={target !== null && pending === null}` -> `isOpen={target !== null}` in ModerationDialog keeps the confirmation open beside the dialog (red).
  it('swaps a code-less ban refusal for the step-up dialog in place of the confirmation', async () => {
    const requests = stubRoute(host, 'ban');
    await host.mount();
    await host.open('ban');

    await userEvent.click(screen.getByRole('button', { name: 'Ban' }));

    expect(await screen.findByLabelText(CODE_LABEL)).toBeInTheDocument();
    expect(stepUpDialog()).toHaveTextContent('verify before you ban Alice.');
    // The confirmation gave way: no checkbox, no second Ban button, no error alert.
    expect(screen.queryByRole('checkbox', { name: PURGE_CHECKBOX })).toBeNull();
    expect(within(stepUpDialog()).getByRole('button', { name: 'Ban' })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(requests).toHaveLength(1);
  });

  // Mutation: `handOff({ ..., target, alsoPurge })` -> `alsoPurge: false` drops the purge clause and the primary's label (red).
  it('re-sends the same ban once with the code, keeping the purge choice, then runs the success path once', async () => {
    const requests = stubRoute(host, 'ban', { retries: [purged('banned')] });
    await host.mount();
    await host.open('ban');
    await userEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    await userEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await userEvent.type(await screen.findByLabelText(CODE_LABEL), FIXTURE_OTP);
    expect(stepUpDialog()).toHaveTextContent('before you ban Alice and purge their messages.');
    await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() => expectNoStepUpDialog());
    expect(requests.map((r) => r.body)).toEqual([
      { purge_messages: true, include_pinned: false },
      { purge_messages: true, include_pinned: false, mfa_code: FIXTURE_OTP },
    ]);
    expect(codesSent(requests)).toEqual([FIXTURE_OTP]);
    expect(screen.getByRole('status').textContent).toBe(
      'Alice was banned and their messages were purged.'
    );
    expect(host.isListed()).not.toBe(true);
    // The confirmation does not come back behind the closed dialog.
    expect(screen.queryByRole('checkbox', { name: PURGE_CHECKBOX })).toBeNull();
  });

  // Mutation: `moderationRequest`'s `purge_messages: alsoPurge` -> `true` re-sends a purge nobody asked for (red).
  it('re-sends an unpurged ban with purge_messages false and the code', async () => {
    const requests = stubRoute(host, 'ban');
    await host.mount();
    await host.open('ban');
    await userEvent.click(screen.getByRole('button', { name: 'Ban' }));

    await userEvent.type(await screen.findByLabelText(CODE_LABEL), FIXTURE_OTP);
    await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Ban' }));

    await waitFor(() => expectNoStepUpDialog());
    expect(requests.map((r) => r.body)).toEqual([
      { purge_messages: false, include_pinned: false },
      { purge_messages: false, include_pinned: false, mfa_code: FIXTURE_OTP },
    ]);
    // No purge was requested, so there is nothing to announce.
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  // Mutation: `sendFirst(request, failure, action === 'ban' || alsoPurge)` -> `action === 'ban'` leaves a purging kick ungated, so the refusal becomes an error alert (red).
  it('swaps and re-sends a kick with purge through members.kick_purge', async () => {
    const requests = stubRoute(host, 'kick', { retries: [purged('kicked')] });
    await host.mount();
    await host.open('kick');
    await userEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    await userEvent.click(screen.getByRole('button', { name: 'Kick and purge' }));

    await userEvent.type(await screen.findByLabelText(CODE_LABEL), FIXTURE_OTP);
    expect(stepUpDialog()).toHaveTextContent('before you kick Alice and purge their messages.');
    await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Kick and purge' }));

    await waitFor(() => expectNoStepUpDialog());
    expect(requests.map((r) => r.body)).toEqual([
      { purge_messages: true, include_pinned: false },
      { purge_messages: true, include_pinned: false, mfa_code: FIXTURE_OTP },
    ]);
    expect(screen.getByRole('status').textContent).toBe(
      'Alice was kicked and their messages were purged.'
    );
    expect(host.isListed()).not.toBe(true);
  });

  // Baseline: a 403 that is not a gate refusal is the host's own error; no single production flip isolates it, the next test carries the gating mutation.
  it('never gates a plain kick: a refusal that is not a gate keeps the existing error', async () => {
    const requests = stubRoute(host, 'kick', {
      first: { status: 403, body: { error: 'Forbidden' } },
    });
    await host.mount();
    await host.open('kick');

    await userEvent.click(screen.getByRole('button', { name: 'Kick' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('Forbidden');
    expectNoStepUpDialog();
    expect(screen.getByRole('button', { name: 'Kick' })).toBeInTheDocument();
    expect(requests).toHaveLength(1);
  });

  // Mutation: `action === 'ban' || alsoPurge` -> `true` in moderateMember, or dropping `&& gated` in `verificationRefusal`, opens the dialog for a plain kick (red).
  it('does not read mfa_required as a prompt on a plain kick', async () => {
    const requests = stubRoute(host, 'kick');
    await host.mount();
    await host.open('kick');

    await userEvent.click(screen.getByRole('button', { name: 'Kick' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('MFA verification required');
    expectNoStepUpDialog();
    expect(screen.queryByLabelText(CODE_LABEL)).toBeNull();
    expect(requests).toHaveLength(1);
  });

  // Mutation: `verificationRefusal` returning null for `enrollmentRequired` leaves a plain kick on the generic error (red).
  it('shows the enrolment state, with the link and no code field, when a plain kick is refused for lack of enrolment', async () => {
    const requests = stubRoute(host, 'kick', { first: ENROLMENT_REQUIRED });
    await host.mount();
    await host.open('kick');

    await userEvent.click(screen.getByRole('button', { name: 'Kick' }));

    expect(await screen.findByText(ENROLMENT_TEXT)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: SETUP_LINK })).toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).toBeNull();
    expect(requests).toHaveLength(1);
  });

  // Mutation: `onSetUpVerification`'s `closeHost: endStepUp` removed, or `returnTo` changed, leaves the dialog up or the return pointing elsewhere (red).
  it('opens verification setup from the enrolment state, abandons the ban and sends nothing more', async () => {
    const requests = stubRoute(host, 'ban', { first: ENROLMENT_REQUIRED });
    await host.mount();
    await host.open('ban');
    await userEvent.click(screen.getByRole('button', { name: 'Ban' }));

    await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));

    await waitFor(() => expect(useSettingsOverlayStore.getState().open).toBe('app'));
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual(host.returnTo);
    expectNoStepUpDialog();
    expect(requests).toHaveLength(1);
    expect(host.isListed()).not.toBe(false);
  });

  // Mutation: omitting `&& !ending` from the confirmation's `isOpen` (the useStepUpHandoff contract) reopens it for one commit and strands focus on <body> (red until fixed).
  it('cancels without sending anything more and returns focus into the member list, never body', async () => {
    const requests = stubRoute(host, 'ban');
    await host.mount();
    await host.open('ban');
    await userEvent.click(screen.getByRole('button', { name: 'Ban' }));
    await screen.findByLabelText(CODE_LABEL);

    await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expectNoStepUpDialog());
    expect(requests).toHaveLength(1);
    expect(screen.queryByRole('checkbox', { name: PURGE_CHECKBOX })).toBeNull();
    expect(host.isListed()).not.toBe(false);
    await waitFor(() => expect(document.activeElement).not.toBe(document.body));
    expect(document.querySelector(host.rootSelector)).toContainElement(
      document.activeElement as HTMLElement
    );
  });

  // Mutation: `setAlsoPurge(false)` dropped from the handoff's `onEnd` carries a checked box into the next ban (red).
  it('forgets the purge choice once the step-up is cancelled', async () => {
    stubRoute(host, 'ban');
    await host.mount();
    await host.open('ban');
    await userEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    await userEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));
    await userEvent.click(
      within(await screen.findByRole('dialog', { name: DIALOG_TITLE })).getByRole('button', {
        name: 'Cancel',
      })
    );
    await waitFor(() => expectNoStepUpDialog());

    await host.open('ban');

    expect(screen.getByRole('checkbox', { name: PURGE_CHECKBOX })).not.toBeChecked();
  });
});

// #3458 meets #3456: the pinned-message choice rides the frozen request, and the
// verified re-send rechecks the pin claim exactly as the first send did.
describe.each(hosts)('moderation step-up with pinned messages on $name', (host) => {
  const PINNED = { name: 'Include pinned messages' };
  const setKeepsPinned = (value: boolean) =>
    useClientConfigStore.setState({
      serverCapabilities: {
        auth: { oauthProviders: [] },
        features: value ? { purgeKeepsPinned: true } : {},
      },
    });

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    stubStepUpRead();
  });
  afterEach(() => vi.restoreAllMocks());

  // Mutation: \`handOff\` without \`pinMode\`, or the request built before the choice, re-sends
  // a purge that keeps pins the user asked to include (red).
  it('re-sends the pinned choice the confirmation showed, with the code', async () => {
    answerCapabilityRefresh();
    const requests = stubRoute(host, 'ban', { retries: [purged('banned')] });
    await host.mount();
    // After mounting: a host's server selection can reset the cached capabilities.
    setKeepsPinned(true);
    await host.open('ban');
    await userEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    await userEvent.click(await screen.findByRole('checkbox', PINNED));
    await userEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await userEvent.type(await screen.findByLabelText(CODE_LABEL), FIXTURE_OTP);
    expect(stepUpDialog()).toHaveTextContent(
      'before you ban Alice and purge their messages, pinned messages included.'
    );
    await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() => expectNoStepUpDialog());
    expect(requests.map((r) => r.body)).toEqual([
      { purge_messages: true, include_pinned: true },
      { purge_messages: true, include_pinned: true, mfa_code: FIXTURE_OTP },
    ]);
  });

  // Mutation: the re-send dispatched without \`sendPinClaim\` (a server rolled back since the
  // first send would delete the pins the dialog promised to keep) (red).
  it('sends nothing on the re-send once the server stops keeping pins, and says so', async () => {
    const refresh = answerCapabilityRefresh();
    const requests = stubRoute(host, 'ban', { retries: [purged('banned')] });
    await host.mount();
    setKeepsPinned(true);
    await host.open('ban');
    await userEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    await userEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));
    await userEvent.type(await screen.findByLabelText(CODE_LABEL), FIXTURE_OTP);

    // The server rolled back: the recheck answers without the capability.
    refresh.mockImplementation(async () => setKeepsPinned(false));
    await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Ban and purge' }));

    expect(
      await within(stepUpDialog()).findByText(PIN_CLAIM_UNCONFIRMED_MESSAGE)
    ).toBeInTheDocument();
    expect(requests).toHaveLength(1);
  });
});
