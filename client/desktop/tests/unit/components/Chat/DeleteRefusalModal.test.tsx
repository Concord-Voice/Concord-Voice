import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import DeleteRefusalModal from '@/renderer/components/Chat/DeleteRefusalModal';
import type { DeleteRefusalState } from '@/renderer/hooks/messaging/useChatController';
import type { DeleteRefusalView } from '@/renderer/services/messaging/deleteRefusal';
import type { StepUpPurpose } from '@/renderer/components/Auth/stepUpPurpose';

// #3455 T7/T8/T9: the single-message delete refusal dialog. It is controlled by
// the hook's one refusal slot, so these tests drive it with a slot value and
// assert what a person sees, types and hears.

// The security-key path is the only observable carrier of `purpose`.
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

const FIXTURE_PW = 'hunter2-fixture';
const CODE = '123456';

function slot(
  view: DeleteRefusalView,
  extra: Partial<DeleteRefusalState> = {}
): DeleteRefusalState {
  return {
    messageId: 'm1',
    view,
    submitting: false,
    openedAt: Date.now(),
    promptKey: 0,
    ...extra,
  };
}

const CONFIRM: DeleteRefusalView = { view: 'confirm', methods: ['totp'] };
const PASSWORD: DeleteRefusalView = { view: 'password' };

interface Props {
  refusal: DeleteRefusalState | null;
  onConfirm?: (step: { mfaCode?: string; currentPassword?: string }) => void;
  onDismiss?: () => void;
  purpose?: StepUpPurpose;
  surfaceId?: string;
}

const SURFACE = 'surface-main';

function ui({
  refusal,
  onConfirm = vi.fn(),
  onDismiss = vi.fn(),
  purpose = 'messages.delete',
  surfaceId = SURFACE,
}: Props) {
  return (
    <DeleteRefusalModal
      refusal={refusal}
      onConfirm={onConfirm}
      onDismiss={onDismiss}
      purpose={purpose}
      surfaceId={surfaceId}
    />
  );
}

async function typeCode(user: ReturnType<typeof userEvent.setup>, code = CODE) {
  await user.click(screen.getByRole('textbox', { name: 'Digit 1' }));
  await user.keyboard(code);
}

function digitValues(): string[] {
  return screen.getAllByRole('textbox').map((el) => (el as HTMLInputElement).value);
}

function confirmButton() {
  return screen.getByRole('button', { name: /^Confirm/ });
}

// Nodes a test appends to the document outside React; removed after each test.
const extraNodes: HTMLElement[] = [];

describe('DeleteRefusalModal', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockApiFetch.mockReset();
    resetAllStores();
  });

  afterEach(() => {
    vi.useRealTimers();
    for (const el of extraNodes.splice(0)) el.remove();
  });

  describe('views', () => {
    it('renders nothing for an empty slot', () => {
      render(ui({ refusal: null }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('confirm: an MFA prompt inside a dialog titled for the person, with no error text', () => {
      render(ui({ refusal: slot(CONFIRM) }));

      const dialog = screen.getByRole('dialog', { name: "Confirm it's you" });
      expect(dialog).toHaveTextContent(
        "You've deleted several messages quickly. Confirm it's you to keep going."
      );
      expect(screen.getByText('MFA Verification')).toBeInTheDocument();
      expect(screen.getAllByRole('textbox')).toHaveLength(6);
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('confirm: shows the per-attempt error the mapper supplied', () => {
      render(
        ui({ refusal: slot({ ...CONFIRM, error: "That didn't work. Try again with a new code." }) })
      );
      expect(screen.getByRole('alert')).toHaveTextContent(
        "That didn't work. Try again with a new code."
      );
    });

    it('password: a labelled password field, not an MFA prompt', () => {
      render(ui({ refusal: slot(PASSWORD) }));

      expect(screen.getByRole('dialog', { name: "Confirm it's you" })).toBeInTheDocument();
      expect(screen.getByLabelText('Password')).toHaveAttribute('type', 'password');
      expect(screen.queryByText('MFA Verification')).not.toBeInTheDocument();
      expect(screen.getByLabelText('Password')).not.toHaveAttribute('aria-invalid');
    });

    it('password: a refused password is announced and tied to the field', () => {
      render(ui({ refusal: slot({ view: 'password', error: 'That password is not correct.' }) }));

      expect(screen.getByRole('alert')).toHaveTextContent('That password is not correct.');
      expect(screen.getByLabelText('Password')).toHaveAttribute('aria-invalid', 'true');
      expect(screen.getByLabelText('Password')).toHaveAccessibleDescription(
        'That password is not correct.'
      );
    });

    it('wait: names the problem, offers Close only, and has no form', () => {
      render(ui({ refusal: slot({ view: 'wait', reason: 'requests' }) }));

      expect(screen.getByRole('dialog', { name: 'Deleting too quickly' })).toHaveTextContent(
        'Try again shortly.'
      );
      expect(screen.queryByRole('button', { name: /^Confirm/ })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument();
      expect(screen.getAllByRole('button', { name: 'Close' }).length).toBeGreaterThan(0);
    });

    // Seen in the running app (#3455 captures): a spent step-up budget, after
    // five wrong passwords or codes, rendered as "Deleting too quickly — Try
    // again shortly.", blaming the delete rate for a verification lockout that
    // lasts up to 15 minutes. The two 429 reasons must read differently.
    it('wait (verification): names the spent attempts, not the delete rate', () => {
      render(ui({ refusal: slot({ view: 'wait', reason: 'verification' }) }));

      const dialog = screen.getByRole('dialog', { name: 'Too many attempts' });
      expect(dialog, 'verification wait names the attempts').toHaveTextContent(
        'Too many verification attempts.'
      );
      expect(dialog, 'the budget window is minutes, not a moment').toHaveTextContent(
        'Try again in a few minutes.'
      );
      expect(dialog, 'never blames the delete rate').not.toHaveTextContent('Deleting too quickly');
      expect(dialog).not.toHaveTextContent('Try again shortly.');
    });

    it('unavailable: says nothing happened and gives no countdown', () => {
      render(ui({ refusal: slot({ view: 'unavailable' }) }));

      expect(screen.getByRole('dialog', { name: "Can't delete right now" })).toHaveTextContent(
        'Deleting messages is temporarily unavailable. Try again in a moment.'
      );
      expect(screen.queryByText(/\d+s\b/)).not.toBeInTheDocument();
    });

    it('failed: shows the server text when there is some', () => {
      render(ui({ refusal: slot({ view: 'failed', message: 'Delete not allowed' }) }));
      expect(
        screen.getByRole('dialog', { name: "Couldn't delete that message" })
      ).toHaveTextContent('Delete not allowed');
    });

    it('failed: falls back to generic copy when there is none', () => {
      render(ui({ refusal: slot({ view: 'failed' }) }));
      expect(screen.getByRole('dialog')).toHaveTextContent('Something went wrong. Try again.');
    });

    it('Close on a Close-only view dismisses', async () => {
      const user = userEvent.setup();
      const onDismiss = vi.fn();
      render(ui({ refusal: slot({ view: 'unavailable' }), onDismiss }));

      const close = screen.getAllByRole('button', { name: 'Close' });
      await user.click(close.at(-1) as HTMLElement);
      expect(onDismiss).toHaveBeenCalledTimes(1);
    });
  });

  describe('confirming with a code', () => {
    it('keeps Confirm disabled until a complete code exists, then sends only the code', async () => {
      const user = userEvent.setup();
      const onConfirm = vi.fn();
      render(ui({ refusal: slot(CONFIRM), onConfirm }));

      expect(confirmButton()).toBeDisabled();
      // The prompt autofocuses digit 1 and each digit advances focus.
      await user.keyboard('123');
      expect(confirmButton()).toBeDisabled();

      await user.keyboard('456');
      expect(confirmButton()).toBeEnabled();
      await user.click(confirmButton());

      expect(onConfirm).toHaveBeenCalledTimes(1);
      expect(onConfirm).toHaveBeenCalledWith({ mfaCode: CODE });
    });

    it('editing a completed code disables Confirm again', async () => {
      const user = userEvent.setup();
      render(ui({ refusal: slot(CONFIRM) }));
      await typeCode(user);
      expect(confirmButton()).toBeEnabled();

      await user.click(screen.getByRole('textbox', { name: 'Digit 6' }));
      await user.keyboard('{Backspace}');
      expect(confirmButton()).toBeDisabled();
    });

    it('submitting the form with no code sends nothing', async () => {
      const user = userEvent.setup();
      const onConfirm = vi.fn();
      render(ui({ refusal: slot(CONFIRM), onConfirm }));

      await user.click(screen.getByRole('textbox', { name: 'Digit 1' }));
      await user.keyboard('{Enter}');
      expect(onConfirm).not.toHaveBeenCalled();
    });

    it('an invalid code remounts the prompt empty (promptKey bump) and clears Confirm', async () => {
      const user = userEvent.setup();
      const { rerender } = render(ui({ refusal: slot(CONFIRM) }));
      await typeCode(user);
      expect(digitValues()).toEqual([...CODE]);
      expect(confirmButton()).toBeEnabled();

      rerender(
        ui({
          refusal: slot(
            { ...CONFIRM, error: "That didn't work. Try again with a new code." },
            { promptKey: 1 }
          ),
        })
      );

      expect(digitValues()).toEqual(['', '', '', '', '', '']);
      expect(confirmButton()).toBeDisabled();
      expect(screen.getByRole('alert')).toHaveTextContent("That didn't work");
    });

    it('a refusal that does NOT bump the key leaves a typed code in place', async () => {
      const user = userEvent.setup();
      const { rerender } = render(ui({ refusal: slot(CONFIRM) }));
      await typeCode(user);

      rerender(ui({ refusal: slot(CONFIRM, { openedAt: Date.now() + 1 }) }));
      expect(digitValues()).toEqual([...CODE]);
    });
  });

  describe('confirming with a password', () => {
    it('keeps Confirm disabled until a password is typed, then sends only the password', async () => {
      const user = userEvent.setup();
      const onConfirm = vi.fn();
      render(ui({ refusal: slot(PASSWORD), onConfirm }));

      expect(confirmButton()).toBeDisabled();
      await user.type(screen.getByLabelText('Password'), FIXTURE_PW);
      expect(confirmButton()).toBeEnabled();
      await user.click(confirmButton());

      expect(onConfirm).toHaveBeenCalledWith({ currentPassword: FIXTURE_PW });
    });

    it('Enter in the field submits it', async () => {
      const user = userEvent.setup();
      const onConfirm = vi.fn();
      render(ui({ refusal: slot(PASSWORD), onConfirm }));

      await user.type(screen.getByLabelText('Password'), `${FIXTURE_PW}{Enter}`);
      expect(onConfirm).toHaveBeenCalledWith({ currentPassword: FIXTURE_PW });
    });

    it('an invalid password clears the field', async () => {
      const user = userEvent.setup();
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      await user.type(screen.getByLabelText('Password'), FIXTURE_PW);
      expect(screen.getByLabelText('Password')).toHaveValue(FIXTURE_PW);

      rerender(
        ui({
          refusal: slot(
            { view: 'password', error: 'That password is not correct.' },
            { promptKey: 1 }
          ),
        })
      );

      expect(screen.getByLabelText('Password')).toHaveValue('');
      expect(confirmButton()).toBeDisabled();
    });

    it('a different message starts from an empty password field', async () => {
      const user = userEvent.setup();
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      await user.type(screen.getByLabelText('Password'), FIXTURE_PW);

      rerender(ui({ refusal: slot(PASSWORD, { messageId: 'm2' }) }));
      expect(screen.getByLabelText('Password')).toHaveValue('');
    });

    it('keeps the typed password while the attempt is merely in flight', async () => {
      const user = userEvent.setup();
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      await user.type(screen.getByLabelText('Password'), FIXTURE_PW);

      rerender(ui({ refusal: slot(PASSWORD, { submitting: true }) }));
      expect(screen.getByLabelText('Password')).toHaveValue(FIXTURE_PW);
      expect(screen.getByLabelText('Password')).toBeDisabled();
    });
  });

  describe('while submitting', () => {
    it.each([
      ['confirm', CONFIRM],
      ['password', PASSWORD],
    ] as const)('%s: is not dismissable and says so', async (_, view) => {
      const user = userEvent.setup();
      const onDismiss = vi.fn();
      render(ui({ refusal: slot(view, { submitting: true }), onDismiss }));

      expect(screen.getByRole('button', { name: 'Confirming…' })).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
      // ui/Modal drops its header X when dismissable is false.
      expect(screen.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument();

      await user.keyboard('{Escape}');
      expect(onDismiss).not.toHaveBeenCalled();
    });

    it('is dismissable by Escape, the header X and Cancel when idle', async () => {
      const user = userEvent.setup();
      const onDismiss = vi.fn();
      render(ui({ refusal: slot(PASSWORD), onDismiss }));

      await user.keyboard('{Escape}');
      expect(onDismiss).toHaveBeenCalledTimes(1);
      await user.click(screen.getByRole('button', { name: 'Close' }));
      expect(onDismiss).toHaveBeenCalledTimes(2);
      await user.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(onDismiss).toHaveBeenCalledTimes(3);
    });

    it('does not submit a second time', async () => {
      const onConfirm = vi.fn();
      render(ui({ refusal: slot(PASSWORD, { submitting: true }), onConfirm }));
      expect(screen.getByRole('button', { name: 'Confirming…' })).toBeDisabled();
      expect(onConfirm).not.toHaveBeenCalled();
    });
  });

  describe('countdown', () => {
    function liveRegion() {
      return screen.getByRole('status');
    }

    it('ticks in the body, writing to the live region only at zero', () => {
      vi.useFakeTimers();
      render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'requests', retryAfterSeconds: 3 },
            { openedAt: Date.now() }
          ),
        })
      );

      expect(screen.getByText('Try again in 3s.')).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('');
      // The ticking text is outside every live region.
      expect(screen.getByText('Try again in 3s.').closest('[aria-live]')).toBeNull();

      act(() => {
        vi.advanceTimersByTime(1000);
      });
      expect(screen.getByText('Try again in 2s.')).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('');

      act(() => {
        vi.advanceTimersByTime(1000);
      });
      expect(screen.getByText('Try again in 1s.')).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('');

      act(() => {
        vi.advanceTimersByTime(1000);
      });
      expect(screen.getByText('You can try again now.', { selector: 'p' })).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('You can try again now.');
      expect(liveRegion()).toHaveAttribute('aria-live', 'polite');
    });

    it('counts from the wall clock, so a throttled timer cannot drift it', () => {
      vi.useFakeTimers();
      render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'requests', retryAfterSeconds: 60 },
            { openedAt: Date.now() }
          ),
        })
      );

      // One long jump in a single tick, as a backgrounded window would see.
      act(() => {
        vi.advanceTimersByTime(45_000);
      });
      expect(screen.getByText('Try again in 15s.')).toBeInTheDocument();
    });

    it('anchors on the response that carried the header, not on when the dialog mounted', () => {
      vi.useFakeTimers();
      const arrived = Date.now() - 10_000;
      render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'requests', retryAfterSeconds: 30 },
            { openedAt: arrived }
          ),
        })
      );
      expect(screen.getByText('Try again in 20s.')).toBeInTheDocument();
    });

    it('a header already elapsed shows zero at once, and announces it', () => {
      vi.useFakeTimers();
      render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'verification', retryAfterSeconds: 5 },
            { openedAt: Date.now() - 60_000 }
          ),
        })
      );
      expect(screen.getByText('You can try again now.', { selector: 'p' })).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('You can try again now.');
    });

    it('a wait with no header shows a static line and never announces', () => {
      vi.useFakeTimers();
      render(ui({ refusal: slot({ view: 'wait', reason: 'requests' }) }));
      act(() => {
        vi.advanceTimersByTime(120_000);
      });
      expect(screen.getByText('Try again shortly.')).toBeInTheDocument();
      expect(liveRegion()).toHaveTextContent('');
    });

    it('a failed view appends the countdown, and drops it at zero', () => {
      vi.useFakeTimers();
      render(
        ui({
          refusal: slot(
            { view: 'failed', message: 'Delete not allowed', retryAfterSeconds: 2 },
            { openedAt: Date.now() }
          ),
        })
      );
      expect(screen.getByRole('dialog')).toHaveTextContent(
        'Delete not allowed You can try again in 2s.'
      );

      act(() => {
        vi.advanceTimersByTime(2000);
      });
      expect(screen.getByRole('dialog')).not.toHaveTextContent('You can try again in');
      expect(screen.getByRole('dialog')).toHaveTextContent('Delete not allowed');
    });

    it('stops ticking once the dialog closes', () => {
      vi.useFakeTimers();
      const { rerender } = render(
        ui({
          refusal: slot(
            { view: 'wait', reason: 'requests', retryAfterSeconds: 30 },
            { openedAt: Date.now() }
          ),
        })
      );
      const ticking = vi.getTimerCount();
      expect(ticking).toBeGreaterThan(0);

      rerender(ui({ refusal: null }));
      expect(vi.getTimerCount()).toBeLessThan(ticking);
    });
  });

  describe('focus', () => {
    // A chat panel root, as the owners render it: `data-chat-surface` on an element in the document.
    function addSurface(id: string) {
      const root = document.createElement('div');
      root.dataset.chatSurface = id;
      document.body.appendChild(root);
      extraNodes.push(root);
      return root;
    }

    function addRow(id: string, surface: HTMLElement = surfaceRoot) {
      const row = document.createElement('div');
      row.dataset.messageId = id;
      row.tabIndex = -1;
      surface.appendChild(row);
      return row;
    }

    function addComposer(surface: HTMLElement = surfaceRoot) {
      const composer = document.createElement('textarea');
      composer.className = 'message-input-textarea';
      surface.appendChild(composer);
      return composer;
    }

    let surfaceRoot: HTMLElement;
    beforeEach(() => {
      surfaceRoot = addSurface(SURFACE);
    });

    it('moves focus into the first field when a challenge opens', () => {
      render(ui({ refusal: slot(PASSWORD) }));
      expect(screen.getByLabelText('Password')).toHaveFocus();
    });

    it('moves focus to Close on a Close-only view', () => {
      render(ui({ refusal: slot({ view: 'unavailable' }) }));
      const focused = document.activeElement as HTMLElement;
      expect(focused).toHaveTextContent('Close');
      expect(focused.className).toContain('delete-refusal-modal__cancel');
    });

    it('returns focus to the message row after close', () => {
      const row = addRow('m1');
      const composer = addComposer();
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      expect(row).not.toHaveFocus();

      rerender(ui({ refusal: null }));
      expect(row).toHaveFocus();
      expect(composer).not.toHaveFocus();
    });

    it('falls back to the composer when the row is gone', () => {
      const composer = addComposer();
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));

      rerender(ui({ refusal: null }));
      expect(composer).toHaveFocus();
    });

    it('never lands on body when neither exists', () => {
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      expect(() => rerender(ui({ refusal: null }))).not.toThrow();
    });

    it('finds a row whose id contains a quote, without treating it as selector syntax', () => {
      const row = addRow('m"1]');
      const { rerender } = render(ui({ refusal: slot(PASSWORD, { messageId: 'm"1]' }) }));

      rerender(ui({ refusal: null }));
      expect(row).toHaveFocus();
    });

    it('returns focus to the row of the message that was refused, not another row', () => {
      const other = addRow('m2');
      const row = addRow('m1');
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));

      rerender(ui({ refusal: null }));
      expect(row).toHaveFocus();
      expect(other).not.toHaveFocus();
    });

    it('never lands on another panel that shows the same message', () => {
      // The other panel comes FIRST in the document, so a document-wide lookup would find it.
      const otherPanel = addSurface('surface-panel');
      const otherRow = addRow('m1', otherPanel);
      const otherComposer = addComposer(otherPanel);
      const row = addRow('m1');
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      // Gate: the modal is open and neither target has focus yet.
      expect(screen.getByLabelText('Password')).toHaveFocus();

      rerender(ui({ refusal: null }));

      expect(row).toHaveFocus();
      expect(otherRow).not.toHaveFocus();
      expect(otherComposer).not.toHaveFocus();
    });

    it('falls back to its own composer, not another panel composer, when its row is gone', () => {
      const otherPanel = addSurface('surface-panel');
      const otherRow = addRow('m1', otherPanel);
      const otherComposer = addComposer(otherPanel);
      const composer = addComposer();
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      expect(screen.getByLabelText('Password')).toHaveFocus();

      rerender(ui({ refusal: null }));

      expect(composer).toHaveFocus();
      expect(otherRow).not.toHaveFocus();
      expect(otherComposer).not.toHaveFocus();
    });

    it('moves focus nowhere when its own panel has neither row nor composer', () => {
      const otherPanel = addSurface('surface-panel');
      const otherRow = addRow('m1', otherPanel);
      const otherComposer = addComposer(otherPanel);
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      expect(screen.getByLabelText('Password')).toHaveFocus();

      rerender(ui({ refusal: null }));

      expect(otherRow).not.toHaveFocus();
      expect(otherComposer).not.toHaveFocus();
    });

    it('does not steal focus for a modal that never opened', () => {
      const composer = addComposer();
      render(ui({ refusal: null }));
      expect(composer).not.toHaveFocus();
    });

    it('re-focuses the Close button when the challenge gives way to a Close-only view', () => {
      const { rerender } = render(ui({ refusal: slot(PASSWORD) }));
      rerender(ui({ refusal: slot({ view: 'wait', reason: 'verification' }) }));
      expect(document.activeElement).toHaveTextContent('Close');
    });
  });

  describe('purpose', () => {
    it.each(['messages.delete', 'dm.message_delete'] as const)(
      'binds a security-key token to %s',
      async (purpose) => {
        mockApiFetch.mockReturnValueOnce(new Promise(() => {}));
        const user = userEvent.setup();
        render(ui({ refusal: slot({ view: 'confirm', methods: ['webauthn'] }), purpose }));

        await user.click(screen.getByRole('button', { name: 'Verify with security key' }));

        const [url, init] = mockApiFetch.mock.calls[0];
        expect(url).toBe('/api/v1/mfa/webauthn/verify-inline/begin');
        expect(JSON.parse(init.body)).toEqual({ purpose });
      }
    );
  });

  describe('no --danger (T8)', () => {
    const VIEWS: DeleteRefusalView[] = [
      CONFIRM,
      { ...CONFIRM, error: 'Try again' },
      PASSWORD,
      { view: 'password', error: 'That password is not correct.' },
      { view: 'wait', reason: 'requests', retryAfterSeconds: 3 },
      { view: 'unavailable' },
      { view: 'failed', message: 'Nope' },
    ];

    it.each(VIEWS.map((view) => [view.view, view] as const))(
      'the %s view carries no danger class',
      (_, view) => {
        render(ui({ refusal: slot(view) }));
        const dialog = screen.getByRole('dialog');
        for (const el of [dialog, ...dialog.querySelectorAll('*')]) {
          expect(el.getAttribute('class') ?? '').not.toMatch(/danger/i);
        }
      }
    );

    it('the stylesheet paints nothing with --danger', () => {
      const css = readFileSync(
        resolve(__dirname, '../../../../src/renderer/components/Chat/DeleteRefusalModal.css'),
        'utf-8'
      ).replaceAll(/\/\*[\s\S]*?\*\//g, '');
      expect(css).not.toMatch(/danger/i);
    });
  });
});
