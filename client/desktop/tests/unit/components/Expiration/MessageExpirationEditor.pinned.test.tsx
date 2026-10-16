import { act, render, screen, waitFor, within } from '../../../test-utils';
import userEvent from '@testing-library/user-event';
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { server } from '../../../mocks/server';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import {
  updateExpirationPolicy,
  type ExpirationMutationResult,
  type ExpirationPolicy,
  type ExpirationPolicyReadResult,
  type ExpirationRequest,
} from '@/renderer/services/messaging/expirationPolicyApi';
import {
  CODE_LABEL,
  DIALOG_TITLE,
  FIXTURE_OTP,
  GATED_API_BASE,
  MFA_REQUIRED,
  stubStepUpRead,
} from '../../../helpers/gatedRoute';
import { stubGatedWrite } from '../../../helpers/gatedWriteRoute';
import MessageExpirationEditor from '@/renderer/components/Expiration/MessageExpirationEditor';

// #3459: the pin switch in the editor. The existing editor suite covers the window flow; this one
// varies what the pin work changed, and every send-assertion looks at the REQUEST the editor
// handed to the network seam, which is where the value is finally spent.

const base: ExpirationPolicy = {
  windowSeconds: 86400,
  updatedAt: '2026-09-08T05:00:00Z',
  revision: 4,
  backfillPending: false,
};
const kept: ExpirationPolicy = { ...base, includePinned: false };
const expiring: ExpirationPolicy = { ...base, includePinned: true };
const off: ExpirationPolicy = { ...base, windowSeconds: null, includePinned: false };

const ON_STATUS = 'Pinned messages now expire. Any whose timer has run out are being deleted.';
const OFF_STATUS =
  'Pinned messages are now kept. Any whose timer had already run out may still be deleted in the next few minutes.';

const noCapability = { auth: { oauthProviders: [] }, features: {} };

type EditorProps = React.ComponentProps<typeof MessageExpirationEditor>;

function setup(
  policy: ExpirationPolicy | null,
  extra: Partial<EditorProps> & { refreshed?: ExpirationPolicy } = {}
) {
  const { refreshed, ...rest } = extra;
  const onApplyPolicy = vi.fn(
    async (_request: ExpirationRequest, _context?: unknown): Promise<ExpirationMutationResult> => ({
      kind: 'ok',
      policy: { ...(policy ?? base), revision: 5 },
    })
  );
  const onRefresh = vi.fn(async (): Promise<ExpirationPolicyReadResult> => ({
    kind: 'fresh',
    policy: refreshed ?? (policy as ExpirationPolicy),
  }));
  const view = render(
    <MessageExpirationEditor
      scope={{ kind: 'channel', id: 'channel-1' }}
      policy={policy}
      policyState="ready"
      canEdit
      lockedDescription="Only moderators can change this timer."
      onRefresh={onRefresh}
      onApplyPolicy={onApplyPolicy}
      onClose={vi.fn()}
      {...rest}
    />
  );
  const sent = (): ExpirationRequest => onApplyPolicy.mock.calls[0][0];
  return { onApplyPolicy, onRefresh, sent, ...view };
}

const pinBox = () => screen.queryByRole('checkbox', { name: 'Include pinned messages' });
const applyButton = () => screen.queryByRole('button', { name: 'Apply' });
const statusRegion = () => document.querySelector('.message-expiration-status');
const acknowledge = (user: ReturnType<typeof userEvent.setup>) =>
  user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));

beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }));
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
  vi.clearAllMocks();
  server.resetHandlers();
  useClientConfigStore.setState({ serverCapabilities: null });
});

describe('the pin switch in context (render gate)', () => {
  it('starts at the server value, checked for expiring and unchecked for kept', () => {
    const first = setup(expiring);
    expect(pinBox()).toBeChecked();
    first.unmount();
    setup(kept);
    expect(pinBox()).not.toBeChecked();
  });

  it.each([
    ['the field is absent (a server that predates #3459)', base, 'ready' as const],
    ['no timer is applied', off, 'ready' as const],
    ['the first read is still loading', null, 'loading' as const],
    ['the policy is unavailable', expiring, 'unavailable' as const],
  ])('draws no switch when %s', (_name, policy, state) => {
    setup(policy, { policyState: state });
    expect(pinBox()).not.toBeInTheDocument();
  });

  it('draws no switch for a server CONFIRMED not to take the field, and draws it when told yes', () => {
    useClientConfigStore.setState({ serverCapabilities: noCapability });
    const first = setup(expiring);
    expect(pinBox()).not.toBeInTheDocument();
    first.unmount();
    act(() =>
      useClientConfigStore.setState({
        serverCapabilities: {
          auth: { oauthProviders: [] },
          features: { expirationPinnedPolicySupported: true },
        },
      })
    );
    setup(expiring);
    expect(pinBox()).toBeInTheDocument();
  });

  it('keeps a focused switch through a refresh, locked rather than unmounted (#3459 Codex review)', () => {
    const view = setup(expiring);
    act(() => pinBox()!.focus());
    view.rerender(
      <MessageExpirationEditor
        scope={{ kind: 'channel', id: 'channel-1' }}
        policy={expiring}
        policyState="loading"
        canEdit
        lockedDescription="Only moderators can change this timer."
        onRefresh={vi.fn()}
        onApplyPolicy={vi.fn()}
        onClose={vi.fn()}
      />
    );
    expect(pinBox()).toHaveFocus();
    expect(pinBox()).toHaveAttribute('aria-disabled', 'true');
  });

  it('moves focus to the active stop when the capability read removes a focused switch', () => {
    setup(expiring);
    act(() => pinBox()!.focus());
    act(() => useClientConfigStore.setState({ serverCapabilities: noCapability }));
    expect(pinBox()).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '24 hours' })).toHaveFocus();
  });

  it('fails OPEN while the capabilities are unknown (loading or a failed fetch)', () => {
    useClientConfigStore.setState({ serverCapabilities: null });
    setup(expiring);
    expect(pinBox()).toBeInTheDocument();
  });

  it('appears, unchecked, once a window is STAGED over an applied Off', async () => {
    const user = userEvent.setup();
    setup(off);
    expect(pinBox()).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '24 hours' }));
    expect(pinBox()).toBeInTheDocument();
    expect(pinBox()).not.toBeChecked();
  });

  it('is aria-disabled and inert for someone who cannot edit, with the reason attached', async () => {
    const user = userEvent.setup();
    setup(expiring, { canEdit: false });
    const box = pinBox()!;
    expect(box).toHaveAttribute('aria-disabled', 'true');
    expect(box).not.toBeDisabled();
    expect(box).toHaveAccessibleDescription(/Only moderators can change this timer\./);
    await user.click(box);
    expect(box).toBeChecked();
    expect(applyButton()).not.toBeInTheDocument();
  });
});

describe('staging the switch', () => {
  it('stages without sending, and putting it back drops the draft', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy } = setup(expiring);
    await user.click(pinBox()!);
    expect(pinBox()).not.toBeChecked();
    expect(applyButton()).toBeInTheDocument();
    await user.click(pinBox()!);
    expect(pinBox()).toBeChecked();
    expect(applyButton()).not.toBeInTheDocument();
    expect(onApplyPolicy).not.toHaveBeenCalled();
  });

  it('Discard drops a staged switch along with a staged window', async () => {
    const user = userEvent.setup();
    setup(expiring);
    await user.click(pinBox()!);
    await user.click(screen.getByRole('button', { name: '7 days' }));
    await user.click(screen.getByRole('button', { name: 'Discard' }));
    expect(applyButton()).not.toBeInTheDocument();
    expect(pinBox()).toBeChecked();
  });

  it('drops a staged switch when Off is picked, and the switch goes with the window', async () => {
    const user = userEvent.setup();
    setup(expiring);
    await user.click(pinBox()!);
    await user.click(screen.getByRole('button', { name: 'Off' }));
    expect(pinBox()).not.toBeInTheDocument();
  });
});

describe('Apply routing', () => {
  it('OFF-only applies at once: no confirmation, new_only, include_pinned false, behind the same fresh read', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy, onRefresh, sent } = setup(expiring);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));
    expect(sent()).toEqual({
      mode: 'set',
      window_seconds: 86400,
      retroactive: 'new_only',
      include_pinned: false,
    });
    expect(onRefresh).toHaveBeenCalled();
    await waitFor(() => expect(statusRegion()).toHaveTextContent(OFF_STATUS));
  });

  it('locks the switch and the stops while a direct apply is out (#3459 Codex review)', async () => {
    // A change staged while the request is out would orphan its answer: the editor ignores a
    // response once the operation moved on, while the store still commits it.
    const user = userEvent.setup();
    let answer: (result: ExpirationMutationResult) => void = () => undefined;
    const onApplyPolicy = vi.fn(
      () =>
        new Promise<ExpirationMutationResult>((resolve) => {
          answer = resolve;
        })
    );
    setup(expiring, { onApplyPolicy });
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));

    expect(pinBox()).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: '7 days' })).toHaveAttribute('aria-disabled', 'true');
    await user.click(pinBox()!);
    await user.click(screen.getByRole('button', { name: '7 days' }));
    expect(pinBox()).not.toBeChecked();
    expect(screen.getByRole('button', { name: '7 days' })).not.toHaveClass('pending');

    await act(async () => answer({ kind: 'ok', policy: { ...kept, revision: 5 } }));
    await waitFor(() => expect(statusRegion()).toHaveTextContent(OFF_STATUS));
  });

  describe('a superseded change is not left announced (#3459 Codex review)', () => {
    function heldEditor(policy: ExpirationPolicy, onApplyPolicy: EditorProps['onApplyPolicy']) {
      return (
        <MessageExpirationEditor
          scope={{ kind: 'channel', id: 'channel-1' }}
          policy={policy}
          policyState="ready"
          canEdit
          lockedDescription="x"
          onRefresh={async () => ({ kind: 'fresh', policy: expiring })}
          onApplyPolicy={onApplyPolicy}
          onClose={vi.fn()}
        />
      );
    }

    it('clears the announcement when a newer revision lands after it', async () => {
      const user = userEvent.setup();
      const onApplyPolicy = vi.fn(async (): Promise<ExpirationMutationResult> => ({
        kind: 'ok',
        policy: { ...kept, revision: 5 },
      }));
      const view = render(heldEditor(expiring, onApplyPolicy));
      await user.click(pinBox()!);
      await user.click(applyButton()!);
      await waitFor(() => expect(statusRegion()).toHaveTextContent(OFF_STATUS));
      view.rerender(heldEditor({ ...kept, revision: 5 }, onApplyPolicy));
      expect(statusRegion()).toHaveTextContent(OFF_STATUS);

      // Another manager turns pinned expiry back on.
      view.rerender(heldEditor({ ...expiring, revision: 6 }, onApplyPolicy));
      await waitFor(() => expect(statusRegion()).toHaveTextContent(''));
      expect(statusRegion()?.textContent).toBe('');
    });

    it('says nothing when a newer revision was already shown before the answer landed', async () => {
      const user = userEvent.setup();
      let answer: (result: ExpirationMutationResult) => void = () => undefined;
      const onApplyPolicy = vi.fn(
        () =>
          new Promise<ExpirationMutationResult>((resolve) => {
            answer = resolve;
          })
      );
      const view = render(heldEditor(expiring, onApplyPolicy));
      await user.click(pinBox()!);
      await user.click(applyButton()!);
      await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));

      view.rerender(heldEditor({ ...expiring, revision: 6 }, onApplyPolicy));
      await act(async () => answer({ kind: 'ok', policy: { ...kept, revision: 5 } }));
      expect(statusRegion()?.textContent).toBe('');
    });

    it('clears the announcement when the same revision loses its pin claim', async () => {
      const user = userEvent.setup();
      const onApplyPolicy = vi.fn(async (): Promise<ExpirationMutationResult> => ({
        kind: 'ok',
        policy: { ...kept, revision: 5 },
      }));
      const view = render(heldEditor(expiring, onApplyPolicy));
      await user.click(pinBox()!);
      await user.click(applyButton()!);
      await waitFor(() => expect(statusRegion()).toHaveTextContent(OFF_STATUS));

      // A read from a server without the field drops the claim at the same revision.
      const { includePinned: _dropped, ...withoutClaim } = { ...kept, revision: 5 };
      view.rerender(heldEditor(withoutClaim, onApplyPolicy));
      expect(statusRegion()?.textContent).toBe('');
    });
  });

  it('returns focus to the switch after a direct apply, never <body>', async () => {
    const user = userEvent.setup();
    setup(expiring);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await waitFor(() => expect(applyButton()).not.toBeInTheDocument());
    await waitFor(() => expect(pinBox()).toHaveFocus());
  });

  it('OFF-only still passes the fence: a policy that moved under it sends nothing and says so', async () => {
    const user = userEvent.setup();
    // The fresh read no longer carries the claim (a rollback at an unchanged revision).
    const { onApplyPolicy } = setup(expiring, { refreshed: base });
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    expect(
      await screen.findByText('The policy changed. Review the refreshed timer before continuing.')
    ).toBeInTheDocument();
    expect(onApplyPolicy).not.toHaveBeenCalled();
  });

  it('a failed direct apply is said in the editor and keeps the draft to retry', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy } = setup(expiring);
    onApplyPolicy.mockResolvedValueOnce({ kind: 'rejected', reason: 'unavailable' });
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    expect(await screen.findByRole('alert')).toHaveTextContent('This change could not be applied.');
    expect(applyButton()).toBeInTheDocument();
    expect(applyButton()).not.toHaveAttribute('aria-disabled', 'true');
    expect(statusRegion()).toBeEmptyDOMElement();
  });

  it('ON-only opens its own confirmation: no radios, an acknowledgement, new_only, then announces', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy, sent } = setup(kept);
    await user.click(pinBox()!);
    await user.click(applyButton()!);

    const dialog = await screen.findByRole('dialog', { name: 'Expire pinned messages too' });
    expect(within(dialog).queryAllByRole('radio')).toHaveLength(0);
    expect(dialog).toHaveTextContent('Pinned messages will expire like every other message.');
    // Pins-only ON is new_only: a pin gets back the stamp it was parked with, so the copy names
    // pins whose own timer ran out, never pins older than the current window (#3459 Codex review).
    expect(dialog).toHaveTextContent(
      'Any pinned message whose timer has already run out is deleted right away.'
    );
    expect(dialog).not.toHaveTextContent('older than');
    const confirm = within(dialog).getByRole('button', { name: 'Expire pinned messages' });
    expect(confirm).toBeDisabled();
    await acknowledge(user);
    expect(confirm).toBeEnabled();
    await user.click(confirm);

    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));
    expect(sent()).toEqual({
      mode: 'set',
      window_seconds: 86400,
      retroactive: 'new_only',
      include_pinned: true,
    });
    await waitFor(() => expect(statusRegion()).toHaveTextContent(ON_STATUS));
    await waitFor(() => expect(pinBox()).toHaveFocus());
  });

  it('ON-only Cancel keeps the staged switch and sends nothing', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy } = setup(kept);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await screen.findByRole('dialog', { name: 'Expire pinned messages too' });
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Expire pinned messages too' })).toBeNull()
    );
    expect(pinBox()).toBeChecked();
    expect(applyButton()).toBeInTheDocument();
    expect(onApplyPolicy).not.toHaveBeenCalled();
  });

  it('ON-only is retired when the policy changes under the open confirmation', async () => {
    const user = userEvent.setup();
    const view = setup(kept);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await screen.findByRole('dialog', { name: 'Expire pinned messages too' });
    // Same revision, claim erased: only the identity's includePinned term can notice.
    view.rerender(
      <MessageExpirationEditor
        scope={{ kind: 'channel', id: 'channel-1' }}
        policy={base}
        policyState="ready"
        canEdit
        lockedDescription="x"
        onRefresh={view.onRefresh}
        onApplyPolicy={view.onApplyPolicy}
        onClose={vi.fn()}
      />
    );
    // NO dialog at all, not merely "not that title": with the identity term missing, the
    // confirmation stays open and merely retitles itself ('Change message expiration') once the
    // staged switch no longer applies, so a title-scoped assertion passes for the wrong reason.
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(applyButton()).not.toBeInTheDocument();
    expect(view.onApplyPolicy).not.toHaveBeenCalled();
  });

  it('ON-only is withdrawn when the capability read confirms the server lacks it (#3459 Codex review)', async () => {
    // Staged while capabilities were unknown (fail-open), then the read says unsupported: the
    // staged switch no longer applies, and an open dialog would retitle itself into a timer
    // confirmation whose Confirm sends nothing and closes as if it had applied.
    const user = userEvent.setup();
    const { onApplyPolicy } = setup(kept);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await screen.findByRole('dialog', { name: 'Expire pinned messages too' });
    act(() => useClientConfigStore.setState({ serverCapabilities: noCapability }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(applyButton()).not.toBeInTheDocument();
    expect(onApplyPolicy).not.toHaveBeenCalled();
  });

  it('ON-only stays open while its own pre-send refresh holds the policy in loading (#3459 Codex review)', async () => {
    // The real refresh sets policyState to 'loading', which empties the staged switch for its
    // duration. The confirmation must not read that as a withdrawn change.
    const user = userEvent.setup();
    const view = setup(kept);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await screen.findByRole('dialog', { name: 'Expire pinned messages too' });
    view.rerender(
      <MessageExpirationEditor
        scope={{ kind: 'channel', id: 'channel-1' }}
        policy={kept}
        policyState="loading"
        canEdit
        lockedDescription="Only moderators can change this timer."
        onRefresh={view.onRefresh}
        onApplyPolicy={view.onApplyPolicy}
        onClose={vi.fn()}
      />
    );
    expect(screen.getByRole('dialog', { name: 'Expire pinned messages too' })).toBeInTheDocument();
  });

  it('ON-only refuses to send when the fresh read disagrees about pins', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy } = setup(kept, { refreshed: base });
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await screen.findByRole('dialog', { name: 'Expire pinned messages too' });
    await acknowledge(user);
    await user.click(screen.getByRole('button', { name: 'Expire pinned messages' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(onApplyPolicy).not.toHaveBeenCalled();
    expect(screen.getByRole('alert')).toHaveTextContent('The policy changed.');
  });
});

describe('a window change keeps its confirmation and sends include_pinned ONLY when changed', () => {
  async function changeWindowTo7Days(user: ReturnType<typeof userEvent.setup>) {
    await user.click(screen.getByRole('button', { name: '7 days' }));
    await user.click(applyButton()!);
    const dialog = await screen.findByRole('dialog', { name: 'Change message expiration' });
    await user.click(within(dialog).getByRole('radio', { name: 'Only new messages' }));
    await acknowledge(user);
    return dialog;
  }
  const send = (user: ReturnType<typeof userEvent.setup>) =>
    user.click(screen.getByRole('button', { name: 'Apply timer' }));

  it.each([
    ['kept', kept, 'Pinned messages are kept.'],
    ['expiring', expiring, 'Pinned messages expire like all other messages.'],
  ])('with pins %s: one pin line, and NO include_pinned on the wire', async (_n, policy, line) => {
    const user = userEvent.setup();
    const { onApplyPolicy, sent } = setup(policy);
    const dialog = await changeWindowTo7Days(user);
    expect(within(dialog).getByText(line)).toBeInTheDocument();
    await send(user);
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));
    expect(sent()).toEqual({ mode: 'set', window_seconds: 604800, retroactive: 'new_only' });
    expect(Object.keys(sent())).not.toContain('include_pinned');
    // A window-only change says nothing about pins afterwards.
    expect(statusRegion()).toBeEmptyDOMElement();
  });

  it('with the field absent (old server): no pin line and no field, exactly the pre-#3459 request', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy, sent } = setup(base);
    const dialog = await changeWindowTo7Days(user);
    expect(dialog).not.toHaveTextContent(/Pinned messages/);
    await send(user);
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));
    expect(Object.keys(sent()).sort()).toEqual(['mode', 'retroactive', 'window_seconds']);
  });

  it('a new timer over Off with the switch untouched sends no field (the server stores false)', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy, sent } = setup(off);
    await user.click(screen.getByRole('button', { name: '24 hours' }));
    await user.click(applyButton()!);
    const dialog = await screen.findByRole('dialog', { name: 'Change message expiration' });
    expect(within(dialog).getByText('Pinned messages are kept.')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('radio', { name: 'Only new messages' }));
    await acknowledge(user);
    await send(user);
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));
    expect(Object.keys(sent())).not.toContain('include_pinned');
  });

  it('a window change TOGETHER with turning pins on sends both, and the pin line says what that deletes', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy, sent } = setup(kept);
    await user.click(screen.getByRole('button', { name: '7 days' }));
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    const dialog = await screen.findByRole('dialog', { name: 'Change message expiration' });
    // The pin line follows the retroactive choice: only "Apply to existing messages" restamps
    // existing pins under 7 days; "Only new messages" gives each pin back its own stamp.
    const ownTimer =
      'Pinned messages will expire too, and any whose timer has already run out are deleted right away.';
    const newWindow =
      'Pinned messages will expire too, and any older than 7 days are deleted right away.';
    expect(within(dialog).getByText(ownTimer)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('radio', { name: 'Apply to existing messages' }));
    expect(within(dialog).getByText(newWindow)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('radio', { name: 'Only new messages' }));
    expect(within(dialog).getByText(ownTimer)).toBeInTheDocument();
    await acknowledge(user);
    await send(user);
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));
    expect(sent()).toEqual({
      mode: 'set',
      window_seconds: 604800,
      retroactive: 'new_only',
      include_pinned: true,
    });
    await waitFor(() => expect(statusRegion()).toHaveTextContent(ON_STATUS));
  });

  it('from Off promises no immediate deletion: the sweep, not the PATCH, deletes (#3459 Codex review)', async () => {
    const user = userEvent.setup();
    setup(off);
    await user.click(screen.getByRole('button', { name: '7 days' }));
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    const dialog = await screen.findByRole('dialog', { name: 'Change message expiration' });
    expect(within(dialog).getByText('Pinned messages will expire too.')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('radio', { name: 'Apply to existing messages' }));
    expect(
      within(dialog).getByText(
        'Pinned messages will expire too, and any older than 7 days are deleted within the next few minutes.'
      )
    ).toBeInTheDocument();
    expect(dialog).not.toHaveTextContent('right away');
  });

  it('Off after a staged switch sends a clear with no include_pinned (a clear is refused the key)', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy, sent } = setup(expiring);
    await user.click(pinBox()!);
    await user.click(screen.getByRole('button', { name: 'Off' }));
    await user.click(applyButton()!);
    const dialog = await screen.findByRole('dialog', { name: 'Turn off message expiration' });
    expect(dialog).not.toHaveTextContent(/Pinned messages/);
    await user.click(within(dialog).getByRole('radio', { name: 'Keep scheduled deletions' }));
    await acknowledge(user);
    await user.click(screen.getByRole('button', { name: 'Turn off timer' }));
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));
    expect(sent()).toEqual({ mode: 'clear', retroactive: 'leave_pending' });
  });

  it('a switch staged before the capability was withdrawn is NOT sent just because it is no longer drawn', async () => {
    const user = userEvent.setup();
    const { onApplyPolicy, sent } = setup(kept);
    await user.click(screen.getByRole('button', { name: '7 days' }));
    await user.click(pinBox()!);
    act(() => useClientConfigStore.setState({ serverCapabilities: noCapability }));
    expect(pinBox()).not.toBeInTheDocument();
    await user.click(applyButton()!);
    const dialog = await screen.findByRole('dialog', { name: 'Change message expiration' });
    expect(dialog).not.toHaveTextContent(/Pinned messages/);
    await user.click(within(dialog).getByRole('radio', { name: 'Only new messages' }));
    await acknowledge(user);
    await send(user);
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));
    expect(Object.keys(sent())).not.toContain('include_pinned');
  });
});

describe('the status region', () => {
  it('is mounted and empty before anything changes, with role=status', () => {
    setup(expiring);
    const region = statusRegion();
    expect(region).toBeInTheDocument();
    // An <output> carries the status role natively (Sonar S6819 prefers it to role="status").
    expect(region?.tagName).toBe('OUTPUT');
    expect(screen.getAllByRole('status')).toContain(region);
    expect(region).toBeEmptyDOMElement();
  });

  it('stays mounted for a policy with no switch at all', () => {
    setup(base);
    expect(statusRegion()).toBeInTheDocument();
  });

  it('is cleared by the next staging action', async () => {
    const user = userEvent.setup();
    setup(expiring);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await waitFor(() => expect(statusRegion()).toHaveTextContent(OFF_STATUS));
    await user.click(pinBox()!);
    expect(statusRegion()).toBeEmptyDOMElement();
  });
});

describe('the verification dialog says what is being verified (#3459)', () => {
  const route = `${GATED_API_BASE}/api/v1/channels/channel-1/expiration`;
  const answer = {
    window_seconds: 86400,
    updated_at: base.updatedAt,
    revision: 5,
    backfill_pending: false,
    include_pinned: true,
  };

  function ApiEditor({ policy }: { policy: ExpirationPolicy }) {
    return (
      <MessageExpirationEditor
        scope={{ kind: 'channel', id: 'channel-1' }}
        policy={policy}
        policyState="ready"
        canEdit
        lockedDescription="x"
        onRefresh={async () => ({ kind: 'fresh', policy })}
        onApplyPolicy={(request, context) =>
          updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request, context)
        }
        onClose={vi.fn()}
      />
    );
  }

  beforeEach(() => {
    useAuthStore.getState().setAccessToken('mock-token');
    stubStepUpRead();
  });

  it('names the pins, keeps the purpose, freezes include_pinned and re-sends it with the code', async () => {
    const user = userEvent.setup();
    const writes = stubGatedWrite({
      method: 'patch',
      url: route,
      first: MFA_REQUIRED,
      retries: [{ status: 200, body: answer }],
    });
    render(<ApiEditor policy={kept} />);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await screen.findByRole('dialog', { name: 'Expire pinned messages too' });
    await acknowledge(user);
    await user.click(screen.getByRole('button', { name: 'Expire pinned messages' }));

    const dialog = await screen.findByRole('dialog', { name: DIALOG_TITLE });
    expect(dialog).toHaveTextContent(
      'This server asks you to verify before pinned messages start to expire.'
    );
    expect(writes[0].json).toEqual({
      mode: 'set',
      window_seconds: 86400,
      retroactive: 'new_only',
      include_pinned: true,
    });
    await user.type(await screen.findByLabelText(CODE_LABEL), FIXTURE_OTP);
    await user.click(within(dialog).getByRole('button', { name: 'Expire pinned messages' }));
    await waitFor(() => expect(writes).toHaveLength(2));
    expect(writes[1].json).toEqual({
      mode: 'set',
      window_seconds: 86400,
      retroactive: 'new_only',
      include_pinned: true,
      mfa_code: FIXTURE_OTP,
    });
  });

  it('retires the pin-enable dialog when another manager moves the policy, sending nothing', async () => {
    const user = userEvent.setup();
    const writes = stubGatedWrite({
      method: 'patch',
      url: route,
      first: MFA_REQUIRED,
      retries: [{ status: 200, body: answer }],
    });
    const view = render(<ApiEditor policy={kept} />);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await screen.findByRole('dialog', { name: 'Expire pinned messages too' });
    await acknowledge(user);
    await user.click(screen.getByRole('button', { name: 'Expire pinned messages' }));
    await screen.findByRole('dialog', { name: DIALOG_TITLE });
    expect(writes).toHaveLength(1);

    // Another manager shortened the window: a new revision under the open dialog.
    view.rerender(<ApiEditor policy={{ ...kept, windowSeconds: 3600, revision: 5 }} />);

    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).not.toBeInTheDocument()
    );
    expect(writes).toHaveLength(1);
  });

  it('opens no dialog when the policy moves while the first send is still out (#3459 Codex review)', async () => {
    const user = userEvent.setup();
    const writes = stubGatedWrite({ method: 'patch', url: route, first: MFA_REQUIRED });
    let release: () => void = () => undefined;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    function HeldEditor({ policy }: { policy: ExpirationPolicy }) {
      return (
        <MessageExpirationEditor
          scope={{ kind: 'channel', id: 'channel-1' }}
          policy={policy}
          policyState="ready"
          canEdit
          lockedDescription="x"
          onRefresh={async () => ({ kind: 'fresh', policy })}
          onApplyPolicy={async (request, context) => {
            await held;
            return updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request, context);
          }}
          onClose={vi.fn()}
        />
      );
    }
    const view = render(<HeldEditor policy={kept} />);
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await screen.findByRole('dialog', { name: 'Expire pinned messages too' });
    await acknowledge(user);
    await user.click(screen.getByRole('button', { name: 'Expire pinned messages' }));

    // Another manager moves the window before the refusal comes back.
    view.rerender(<HeldEditor policy={{ ...kept, windowSeconds: 3600, revision: 5 }} />);
    await act(async () => release());
    await waitFor(() => expect(writes).toHaveLength(1));

    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).not.toBeInTheDocument()
    );
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(writes).toHaveLength(1);
  });

  it('names both when the window moves too, and keeps the original words for a shortening alone', async () => {
    const user = userEvent.setup();
    stubGatedWrite({ method: 'patch', url: route, first: MFA_REQUIRED });
    const first = render(<ApiEditor policy={kept} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await user.click(await screen.findByRole('radio', { name: 'Only new messages' }));
    await acknowledge(user);
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    expect(
      await screen.findByText(
        'This server asks you to verify before you change how long messages are kept and let pinned messages expire.'
      )
    ).toBeInTheDocument();
    first.unmount();

    server.resetHandlers();
    stubStepUpRead();
    stubGatedWrite({ method: 'patch', url: route, first: MFA_REQUIRED });
    render(<ApiEditor policy={kept} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(applyButton()!);
    await user.click(await screen.findByRole('radio', { name: 'Only new messages' }));
    await acknowledge(user);
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    expect(
      await screen.findByText(
        'This server asks you to verify before you shorten how long messages are kept.'
      )
    ).toBeInTheDocument();
  });
});

// The ambiguous-commit ("partial") settle. A 503 whose body is the candidate policy does not say
// the change landed, so the editor must not tell the user what it "now" does. The `ok` arm
// announces (pinned above); the `partial` arm re-reads and either accepts quietly or asks for a
// refresh, and in neither case speaks in the role="status" region.
describe('an ambiguous commit does not announce in the status region', () => {
  const candidate: ExpirationPolicy = { ...expiring, includePinned: false, revision: 5 };

  // Mutant killed: `announceInclude(request)` added to the `partial` arm of settlePolicyResult.
  it.each([
    ['the re-read confirms the candidate', candidate, false],
    ['the re-read still shows the old policy', expiring, true],
  ])('stays silent when %s', async (_name, rereadPolicy, expectsRefreshPrompt) => {
    const user = userEvent.setup();
    let reads = 0;
    const onRefresh = vi.fn(async (): Promise<ExpirationPolicyReadResult> => {
      reads += 1;
      // read 1 is the fence before the send; read 2 is the re-read after the 503.
      return { kind: 'fresh', policy: reads === 1 ? expiring : rereadPolicy };
    });
    const onApplyPolicy = vi.fn(async (): Promise<ExpirationMutationResult> => ({
      kind: 'partial',
      candidate,
    }));
    setup(expiring, { onRefresh, onApplyPolicy });
    await user.click(pinBox()!);
    await user.click(applyButton()!);
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(onRefresh).toHaveBeenCalledTimes(2));
    if (expectsRefreshPrompt) {
      // Positive gate on the post-await write, so the check below samples a settled DOM.
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Refresh the policy before continuing.'
      );
    } else {
      await act(async () => {});
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    }
    expect(statusRegion()).toBeEmptyDOMElement();
  });
});
