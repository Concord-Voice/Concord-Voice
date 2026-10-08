import { render, screen, fireEvent, waitFor, within, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { usePermissionStore, type ChannelOverride } from '@/renderer/stores/chat/permissionStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import CategorySettingsModal from '@/renderer/components/Channels/CategorySettingsModal';
import type { ChannelGroup } from '@/renderer/types/chat';
import type { Role } from '@/renderer/types/server';
import {
  CODE_LABEL,
  DIALOG_TITLE,
  ENROLMENT_REQUIRED,
  ENROLMENT_TEXT,
  FIXTURE_OTP,
  GATED_API_BASE,
  MFA_REQUIRED,
  SETUP_LINK,
  stubStepUpRead,
  type GatedReply,
} from '../../../helpers/gatedRoute';

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

const category: ChannelGroup = {
  id: 'cat-1',
  server_id: 'server-1',
  name: 'General',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};
const role: Role = {
  id: 'role-1',
  server_id: 'server-1',
  name: 'Moderator',
  color: '#ff0000',
  position: 1,
  permissions: '0',
  is_default: false,
  is_managed: false,
  display_separately: false,
  mentionable: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};
const existing: ChannelOverride = {
  id: 'override-1',
  channel_id: 'cat-1',
  target_type: 'role',
  target_id: 'role-1',
  allow: '1',
  deny: '2',
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const ROUTE = `${GATED_API_BASE}/api/v1/categories/cat-1/overrides`;

function stubPut(first: GatedReply, retries: GatedReply[] = [{ status: 200, body: {} }]) {
  const bodies: Record<string, unknown>[] = [];
  let i = 0;
  mswServer.use(
    http.put(ROUTE, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      bodies.push(body);
      const reply = 'mfa_code' in body ? retries[Math.min(i++, retries.length - 1)] : first;
      return HttpResponse.json(reply.body ?? {}, { status: reply.status });
    })
  );
  return bodies;
}

describe('override upsert step-up through the category editor', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    usePermissionStore.setState({
      fetchCategoryOverrides: vi.fn().mockResolvedValue(undefined),
      fetchRoles: vi.fn().mockResolvedValue(true),
      serverRoles: { 'server-1': [role] },
      channelOverrides: { 'category:cat-1': [existing] },
    });
    stubStepUpRead();
  });

  const renderModal = () =>
    render(
      <CategorySettingsModal isOpen category={category} serverId="server-1" onClose={vi.fn()} />
    );
  const saveEdit = async () => {
    await userEvent.click(document.querySelector('.override-item-select')!);
    await userEvent.click(screen.getByRole('button', { name: 'Save Override' }));
  };

  // Mutation: dropping `setSelectedOverrideId(null)` from handleStepUpSuccess in OverridePanel.tsx leaves the editor open (red).
  it('edit: refusal opens the dialog, resends same body plus code, closes editor', async () => {
    const bodies = stubPut(MFA_REQUIRED);
    renderModal();
    await saveEdit();
    const input = await screen.findByLabelText(CODE_LABEL);
    expect(screen.getByRole('dialog', { name: DIALOG_TITLE })).toBeInTheDocument();
    expect(document.querySelector('.override-error[role="alert"]')).toBeNull();
    await userEvent.type(input, FIXTURE_OTP);
    await userEvent.click(
      within(screen.getByRole('dialog', { name: DIALOG_TITLE })).getByRole('button', {
        name: 'Save Override',
      })
    );
    await waitFor(() => expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull());
    expect(bodies).toEqual([
      { target_type: 'role', target_id: 'role-1', allow: '1', deny: '2' },
      { target_type: 'role', target_id: 'role-1', allow: '1', deny: '2', mfa_code: FIXTURE_OTP },
    ]);
    expect(screen.queryByText(/Editing:/)).toBeNull();
    expect(document.activeElement).not.toBe(document.body);
  });

  // Mutation: dropping `setIsSavingOverride(false)` from endStepUp in OverridePanel.tsx leaves Save disabled after Cancel (red).
  it('edit: cancel leaves the editor, no alert, controls usable', async () => {
    stubPut(MFA_REQUIRED);
    renderModal();
    await saveEdit();
    await screen.findByLabelText(CODE_LABEL);
    await userEvent.click(
      within(screen.getByRole('dialog', { name: DIALOG_TITLE })).getByRole('button', {
        name: 'Cancel',
      })
    );
    await waitFor(() => expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull());
    expect(screen.getByText(/Editing:/)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.getByRole('button', { name: 'Save Override' })).not.toBeDisabled();
    expect(document.activeElement).not.toBe(document.body);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Save Override' }));
  });

  // Mutation: replacing `closeHost: endStepUp` with a no-op in OverridePanel.tsx leaves the dialog standing over settings (red).
  it('edit: enrolment refusal shows enrolment state and the setup link opens settings', async () => {
    stubPut(ENROLMENT_REQUIRED);
    renderModal();
    await saveEdit();
    expect(await screen.findByText(ENROLMENT_TEXT)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: SETUP_LINK }));
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull());
    expect(screen.getByRole('button', { name: 'Save Override' })).not.toBeDisabled();
  });

  // Mutation: widening the edit-site gate `current && refusal !== null` to `current` in OverridePanel.tsx opens the dialog for a plain 403 (red).
  it('edit: an ordinary refusal keeps the existing alert and no dialog', async () => {
    stubPut({ status: 403, body: { error: 'You cannot do that' } });
    renderModal();
    await saveEdit();
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Failed to save permission override'
    );
    expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
  });

  // Mutation: dropping `resetAddForm()` from handleStepUpSuccess in OverridePanel.tsx leaves the chosen target in place (red).
  it('add: refusal opens dialog; success resets the form', async () => {
    const bodies = stubPut(MFA_REQUIRED);
    renderModal();
    const target = screen.getByLabelText('Override target') as HTMLSelectElement;
    fireEvent.change(target, { target: { value: 'role-1' } });
    await userEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    const input = await screen.findByLabelText(CODE_LABEL);
    await userEvent.type(input, FIXTURE_OTP);
    const dialog = screen.getByRole('dialog', { name: DIALOG_TITLE });
    await userEvent.click(within(dialog).getByRole('button', { name: 'Add Override' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull());
    expect(bodies).toHaveLength(2);
    expect(bodies[1]).toMatchObject({ target_id: 'role-1', mfa_code: FIXTURE_OTP });
    expect((screen.getByLabelText('Override target') as HTMLSelectElement).value).toBe('');
    expect(document.activeElement).not.toBe(document.body);
  });

  // Mutation: dropping `capture={stepUp?.context}` from the dialog in OverridePanel.tsx re-sends against the dialog's own capture, not the refused add's (red).
  it('add: the code re-sends against the capture the refused add went out with', async () => {
    stubPut(MFA_REQUIRED);
    const upsert = vi.fn(usePermissionStore.getState().upsertCategoryOverride);
    usePermissionStore.setState({ upsertCategoryOverride: upsert });
    renderModal();
    fireEvent.change(screen.getByLabelText('Override target'), { target: { value: 'role-1' } });
    await userEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    await userEvent.type(await screen.findByLabelText(CODE_LABEL), FIXTURE_OTP);
    const dialog = screen.getByRole('dialog', { name: DIALOG_TITLE });
    await userEvent.click(within(dialog).getByRole('button', { name: 'Add Override' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull());

    expect(upsert).toHaveBeenCalledTimes(2);
    const first = await upsert.mock.results[0].value;
    expect(first).toMatchObject({ ok: false, kind: 'refused', status: 403 });
    // The same capture, not an equal one: the dialog never took its own.
    expect(upsert.mock.calls[1][2]?.context).toBe(first.context);
  });

  // Mutation: dropping the apiRequestContextIsCurrent check from permissionWriteStepUp.stepUpSeedOf opens the dialog for whoever signed in while the save was out (red).
  it('edit: a refusal that lands after the account changed opens no dialog', async () => {
    const bodies: unknown[] = [];
    mswServer.use(
      http.put(ROUTE, async ({ request }) => {
        bodies.push(await request.json());
        useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
        return HttpResponse.json(MFA_REQUIRED.body, { status: MFA_REQUIRED.status });
      })
    );
    renderModal();
    await saveEdit();
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Failed to save permission override'
    );
    expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
    expect(bodies).toHaveLength(1);
  });
});
