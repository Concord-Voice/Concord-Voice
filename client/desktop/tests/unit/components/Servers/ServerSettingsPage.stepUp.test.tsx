import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, userEvent, waitFor, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { bodiesTo, installStepUpApi, jsonResponse, readOffers } from '../../../helpers/stepUpApi';
import {
  CODE_LABEL,
  DIALOG_TITLE,
  ENROLMENT_REQUIRED,
  ENROLMENT_TEXT,
  FIXTURE_OTP,
  MFA_REQUIRED,
  SETUP_LINK,
  type GatedReply,
} from '../../../helpers/gatedRoute';
import { createMockWsService } from '../../../helpers/wsServiceMock';
import { moderationMember } from '../../../helpers/moderationPurge';

// Server Settings' save under MFA enforcement (#3456 §3.4 "Settings save", §3.6a). The page, the
// dialog, the factor hook, the refusal adapter and openVerificationSetup are real; only `apiFetch`
// (routed by path and method, as MfaEnforcementSetting.test.tsx does) and the WebSocket service are
// replaced.
//
// "Mutant:" comments name the production change each case exists to turn red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

const wsHolder = vi.hoisted(() => ({ current: null as unknown }));
vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => wsHolder.current,
}));

import ServerSettingsPage from '@/renderer/components/Servers/ServerSettingsPage';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useInviteStore } from '@/renderer/stores/chat/inviteStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { mockServer } from '../../../mocks/fixtures';
import {
  ADMIN_PERMISSIONS,
  BAN,
  INVITE,
  MANAGE_ROLES,
  MANAGE_ROLES_ASSIGN,
  MANAGE_SERVER,
} from '@/renderer/utils/policy/permissions';

const SERVER_PATH = '/api/v1/servers/server-1';
const SETTING_PATH = `${SERVER_PATH}/mfa-enforcement`;
const NEW_NAME = 'Renamed Server';
const DISCARD_TITLE = 'Discard unsaved changes?';
const SUCCESS = 'Server updated successfully!';

const SAVED_SERVER = {
  server: {
    name: NEW_NAME,
    icon_url: null,
    banner_url: null,
    allow_embedded_content: false,
    updated_at: '2025-02-02T00:00:00Z',
  },
};

interface Script {
  /** The answer to a PATCH, built per request from its parsed body. */
  patch: (body: Record<string, unknown>) => Response;
}

const asReply = ({ status, body }: GatedReply): Response => jsonResponse(status, body ?? {});

/** A PATCH script: a code-less save gets `first`; one carrying `mfa_code` gets `retry`. */
function gate(first: GatedReply, retry: Response = jsonResponse(200, SAVED_SERVER)): Script {
  return { patch: (body) => ('mfa_code' in body ? retry : asReply(first)) };
}

let script: Script;
let ws: ReturnType<typeof createMockWsService>;

function serve(next: Script, readMethods: string[] = ['totp']): void {
  script = next;
  installStepUpApi(mockApiFetch, {
    read: () => readOffers(readMethods),
    route: (path, init) => {
      if (path === SETTING_PATH) return jsonResponse(200, { enforce_mfa_dangerous_actions: false });
      if (path === SERVER_PATH && init.method === 'PATCH') {
        return script.patch(JSON.parse(init.body as string) as Record<string, unknown>);
      }
      return jsonResponse(404);
    },
  });
}

const patchBodies = () => bodiesTo(mockApiFetch, SERVER_PATH);
const nameField = () => screen.getByPlaceholderText('My Awesome Server') as HTMLInputElement;
const stepUpDialog = () => screen.queryByRole('dialog', { name: DIALOG_TITLE });
const pageSave = () => screen.getByRole('button', { name: 'Save Changes' });

async function renderPage() {
  const view = render(<ServerSettingsPage serverId="server-1" />);
  await screen.findByRole('heading', { name: 'Security' });
  return view;
}

/** Edits the name, saves, and waits for the verification dialog the refusal opens. */
async function saveRefused(name = NEW_NAME) {
  await renderPage();
  await userEvent.clear(nameField());
  await userEvent.type(nameField(), name);
  await userEvent.click(pageSave());
  return screen.findByRole('dialog', { name: DIALOG_TITLE });
}

beforeEach(() => {
  resetAllStores();
  // The wire always carries the flag; the shared fixture omits it, which would put it in every body.
  useServerStore.setState({ servers: [{ ...mockServer, allow_embedded_content: false }] });
  useInviteStore.setState({
    invites: {},
    fetchInvites: vi.fn().mockResolvedValue(undefined),
    createInvite: vi.fn().mockResolvedValue(null),
  });
  usePermissionStore.setState({
    serverPermissions: {
      'server-1': ADMIN_PERMISSIONS | MANAGE_SERVER | MANAGE_ROLES | MANAGE_ROLES_ASSIGN | INVITE,
    },
    serverRoles: {},
    fetchRoles: vi.fn().mockResolvedValue(undefined),
  });
  useMemberStore.setState({ members: [], fetchMembers: vi.fn().mockResolvedValue(undefined) });
  ws = createMockWsService();
  wsHolder.current = ws;
  useUserStore.setState({ user: { id: 'acct-1' } as never });
  serve(gate(MFA_REQUIRED));
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('a save the server asks to verify', () => {
  // Mutant: the page maps the refusal to `errors.general` (no dialog), or `stepUpSeed` returns null for mfa_required.
  it('opens the verification dialog over the page, with nothing yet saved', async () => {
    const dialog = await saveRefused();

    expect(within(dialog).getByLabelText(CODE_LABEL)).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Save Changes' })).toBeInTheDocument();
    expect(nameField()).toBeInTheDocument();
    expect(screen.queryByText(SUCCESS)).not.toBeInTheDocument();
    expect(patchBodies()).toEqual([{ name: NEW_NAME }]);
  });

  // Mutant: `resendServerUpdate` drops `mfa_code`, sends another body, or the dialog never calls `send` with the typed code.
  it('re-sends the identical body plus the typed code, then shows the success state', async () => {
    const dialog = await saveRefused();

    await userEvent.type(within(dialog).getByLabelText(CODE_LABEL), FIXTURE_OTP);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save Changes' }));

    expect(await screen.findByText(SUCCESS)).toBeInTheDocument();
    expect(patchBodies()).toEqual([{ name: NEW_NAME }, { name: NEW_NAME, mfa_code: FIXTURE_OTP }]);
    expect(stepUpDialog()).not.toBeInTheDocument();
    expect(useServerStore.getState().servers[0].name).toBe(NEW_NAME);
  });

  // Mutant: `onSaved` is not wired, or `setPendingSave(null)` is dropped, leaving the dialog up after a 200.
  it('closes the dialog and leaves the page editable after the re-send succeeds', async () => {
    const dialog = await saveRefused();

    await userEvent.type(within(dialog).getByLabelText(CODE_LABEL), FIXTURE_OTP);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save Changes' }));

    await waitFor(() => expect(stepUpDialog()).not.toBeInTheDocument());
    expect(nameField()).toBeEnabled();
    expect(nameField().value).toBe(NEW_NAME);
  });

  // Mutant: a wrong code closes the dialog or reports success; the failed re-send must keep the page unsaved.
  it('keeps the dialog up, and the page unsaved, when the code is refused', async () => {
    serve(gate(MFA_REQUIRED, jsonResponse(403, { error: 'Invalid MFA code' })));
    const dialog = await saveRefused();

    await userEvent.type(within(dialog).getByLabelText(CODE_LABEL), FIXTURE_OTP);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save Changes' }));

    await waitFor(() => expect(patchBodies()).toHaveLength(2));
    await act(async () => {});
    expect(stepUpDialog()).toBeInTheDocument();
    expect(screen.queryByText(SUCCESS)).not.toBeInTheDocument();
    expect(useServerStore.getState().servers[0].name).toBe(mockServer.name);
  });

  // Mutant: Cancel keeps `pendingSave`, so the dialog reopens or a later save re-sends the abandoned body.
  it('Cancel closes the dialog without a second send', async () => {
    const dialog = await saveRefused();

    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(stepUpDialog()).not.toBeInTheDocument());
    expect(patchBodies()).toHaveLength(1);
    expect(screen.queryByText(SUCCESS)).not.toBeInTheDocument();
  });
});

describe('a save that fails for another reason', () => {
  // Mutant: `stepUpSeed` seeds on any 4xx, or the page swallows `result.message`.
  it('keeps the existing error and opens no dialog for a plain failure', async () => {
    serve({ patch: () => jsonResponse(400, { error: 'Server name taken' }) });
    await renderPage();

    await userEvent.click(pageSave());

    expect(await screen.findByText('Server name taken')).toBeInTheDocument();
    expect(stepUpDialog()).not.toBeInTheDocument();
  });

  // Mutant: `stepUpSeed` accepts a bare 403 (a missing-permission refusal) as a verification request.
  it('treats a 403 that is not an MFA refusal as a plain failure', async () => {
    serve({ patch: () => jsonResponse(403, { error: 'Insufficient permissions' }) });
    await renderPage();

    await userEvent.click(pageSave());

    expect(await screen.findByText('Insufficient permissions')).toBeInTheDocument();
    expect(stepUpDialog()).not.toBeInTheDocument();
  });

  // Mutant: `updateServer` loses the `UPDATE_FAILED` fallback when the body carries no error text.
  it('words a failure with no body as the page always did', async () => {
    serve({ patch: () => new Response('<html>bad gateway</html>', { status: 502 }) });
    await renderPage();

    await userEvent.click(pageSave());

    expect(await screen.findByText('Failed to update server')).toBeInTheDocument();
    expect(stepUpDialog()).not.toBeInTheDocument();
  });
});

describe('"Set up verification" from the save dialog', () => {
  const setUp = (dialog: HTMLElement) =>
    userEvent.click(within(dialog).getByRole('button', { name: SETUP_LINK }));
  const saveRefusedForEnrolment = async () => {
    const dialog = await saveRefused();
    await within(dialog).findByText(ENROLMENT_TEXT);
    return dialog;
  };
  const discardDialog = () => screen.queryByRole('dialog', { name: DISCARD_TITLE });

  beforeEach(() => {
    // An account with no inline factor: the refusal is the enrolment one and the read offers none.
    serve(gate(ENROLMENT_REQUIRED), []);
  });

  // Mutant: `confirmDiscard` is not passed to openVerificationSetup, so the edits are lost unasked.
  it('asks to discard unsaved edits first, with the dialog still up behind the question', async () => {
    const dialog = await saveRefusedForEnrolment();

    await setUp(dialog);

    expect(await screen.findByRole('dialog', { name: DISCARD_TITLE })).toBeInTheDocument();
    expect(useSettingsOverlayStore.getState().open).toBeNull();
    expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();
  });

  // Mutant: openVerificationSetup closes the host, or opens Settings, before the discard answer (D-4).
  it('leaves the dialog and the page untouched when the discard is declined', async () => {
    const dialog = await saveRefusedForEnrolment();
    await setUp(dialog);
    const question = await screen.findByRole('dialog', { name: DISCARD_TITLE });

    await userEvent.click(within(question).getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(discardDialog()).not.toBeInTheDocument());
    await act(async () => {});
    expect(stepUpDialog()).toBeInTheDocument();
    expect(within(stepUpDialog() as HTMLElement).getByText(ENROLMENT_TEXT)).toBeInTheDocument();
    expect(nameField().value).toBe(NEW_NAME);
    expect(useSettingsOverlayStore.getState().open).toBeNull();
    expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();
    expect(patchBodies()).toHaveLength(1);
  });

  // Mutant: the return is recorded with another section or server, or `closeHost` is not the dialog's close.
  it('opens App Settings with a way back to this server once the discard is accepted', async () => {
    const dialog = await saveRefusedForEnrolment();
    await setUp(dialog);
    const question = await screen.findByRole('dialog', { name: DISCARD_TITLE });

    await userEvent.click(within(question).getByRole('button', { name: 'Discard Changes' }));

    await waitFor(() => expect(useSettingsOverlayStore.getState().open).toBe('app'));
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
      kind: 'serverSettings',
      serverId: 'server-1',
      section: 'general',
    });
    await waitFor(() => expect(stepUpDialog()).not.toBeInTheDocument());
    expect(patchBodies()).toHaveLength(1);
  });

  // Mutant: `hasUnsavedChanges` always true (a pristine form is asked about) or `confirmDiscard` always asks.
  it('does not ask when the form holds no edits', async () => {
    await renderPage();
    await userEvent.click(pageSave());
    const dialog = await screen.findByRole('dialog', { name: DIALOG_TITLE });
    await within(dialog).findByText(ENROLMENT_TEXT);

    await setUp(dialog);

    await waitFor(() => expect(useSettingsOverlayStore.getState().open).toBe('app'));
    expect(discardDialog()).not.toBeInTheDocument();
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
      kind: 'serverSettings',
      serverId: 'server-1',
      section: 'general',
    });
  });
});

// General's edits are page state, so they outlive a switch to Roles or Members, and "Set up
// verification" from either section leaves the page and them with it. Each section asks about
// them first, even with nothing of its own changed. The panels are real; only the network is not.
describe('"Set up verification" from Roles or Members, with General edited', () => {
  const ROLES_PATH = `${SERVER_PATH}/roles`;
  const BAN_PATH = `${SERVER_PATH}/bans/${moderationMember.user_id}`;

  beforeEach(() => {
    useMemberStore.setState({ members: [moderationMember] });
    usePermissionStore.setState((st) => ({
      serverPermissions: { 'server-1': (st.serverPermissions['server-1'] ?? 0n) | BAN },
    }));
    installStepUpApi(mockApiFetch, {
      read: () => readOffers([]),
      route: (path) => {
        if (path === SETTING_PATH)
          return jsonResponse(200, { enforce_mfa_dangerous_actions: false });
        if (path === ROLES_PATH || path === BAN_PATH) return asReply(ENROLMENT_REQUIRED);
        return jsonResponse(404);
      },
    });
  });

  async function editGeneralThenOpen(section: 'Roles' | 'Members') {
    await renderPage();
    await userEvent.clear(nameField());
    await userEvent.type(nameField(), NEW_NAME);
    await userEvent.click(screen.getByRole('button', { name: section }));
  }

  async function expectAskedFirst() {
    await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));
    expect(await screen.findByRole('dialog', { name: DISCARD_TITLE })).toBeInTheDocument();
    expect(useSettingsOverlayStore.getState().open).toBeNull();
    expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();
  }

  // Mutant: ServerSettingsPage stops passing `pageIsDirty` to RoleEditorPanel.
  it('a refused role create asks about the General edits', async () => {
    await editGeneralThenOpen('Roles');
    await userEvent.click(await screen.findByText('+ Create Role'));
    await expectAskedFirst();
  });

  // Mutant: ServerSettingsPage stops passing `confirmDiscard` to MemberListPanel, or the panel to ModerationDialog.
  it('a refused ban asks about the General edits', async () => {
    await editGeneralThenOpen('Members');
    await userEvent.click(
      await screen.findByRole('button', { name: 'Open context menu for Alice' })
    );
    // The menu's Ban opens the confirmation, whose own Ban sends.
    await userEvent.click(screen.getByRole('button', { name: 'Ban' }));
    const confirmation = await screen.findByRole('dialog');
    await userEvent.click(within(confirmation).getByRole('button', { name: 'Ban' }));
    await screen.findByText(ENROLMENT_TEXT);
    await expectAskedFirst();
  });
});

describe('the Security subsection', () => {
  // Mutant: MfaEnforcementSetting is mounted before the invite section, or outside General's form.
  it('is the last subsection of General, labelled Security', async () => {
    await renderPage();

    const titles = screen
      .getAllByRole('heading', { level: 2 })
      .map((h) => h.textContent)
      .filter((t) => t !== null);
    expect(titles).toEqual(['Server Info', 'Content Safety', 'Invite Code', 'Security']);
    const security = screen.getByRole('heading', { name: 'Security' });
    expect(security.closest('form')).not.toBeNull();
    expect(security.closest('#section-security')).not.toBeNull();
    expect(await screen.findByRole('switch', { name: 'Require MFA for dangerous actions' })).toBe(
      within(security.closest('#section-security') as HTMLElement).getByRole('switch')
    );
  });
});
