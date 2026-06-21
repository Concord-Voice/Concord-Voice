import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useRichPresenceStore } from '@/renderer/stores/richPresenceStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';

// Mock the EmojiPicker to avoid loading the full emoji dataset. The mock exposes
// deterministic Select/Close buttons so the popover's onSelect/onClose wiring is
// exercised without the real picker's async data loading.
vi.mock('@/renderer/components/EmojiPicker/EmojiPicker', () => ({
  default: ({ onSelect, onClose }: { onSelect: (emoji: string) => void; onClose: () => void }) => (
    <div data-testid="emoji-picker">
      <button type="button" onClick={() => onSelect('🚀')} data-testid="picker-select">
        Pick rocket
      </button>
      <button type="button" onClick={onClose} data-testid="picker-close">
        Close picker
      </button>
    </div>
  ),
}));

const { default: CustomStatusPopover } =
  await import('@/renderer/components/User/CustomStatusPopover');

const API_BASE = 'http://localhost:8080';
const PRESENCE_PATH = `${API_BASE}/api/v1/users/me/presence-settings`;

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

describe('CustomStatusPopover', () => {
  const onClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  const getInput = () =>
    screen.getByRole('textbox', { name: /custom status text/i }) as HTMLInputElement;
  const getSaveBtn = () => screen.getByRole('button', { name: /^save$/i }) as HTMLButtonElement;
  const getClearBtn = () => screen.getByRole('button', { name: /^clear$/i });

  it('renders as a native dialog (implicit role="dialog")', () => {
    render(<CustomStatusPopover onClose={onClose} />);
    // The native <dialog> exposes role="dialog" implicitly, so the query that
    // worked against the old <div role="dialog"> still resolves.
    const dialog = screen.getByRole('dialog', { name: /set custom status/i });
    expect(dialog.tagName).toBe('DIALOG');
    expect(dialog).toHaveAttribute('open');
  });

  it('PATCHes presence-settings with the typed text on Save', async () => {
    let received: Record<string, unknown> | null = null;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({
          custom_text_tier: 0,
          custom_text: 'Coffee break',
          custom_text_emoji: '',
        });
      })
    );

    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.change(getInput(), { target: { value: 'Coffee break' } });
    fireEvent.click(getSaveBtn());

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(received).toEqual({ custom_text: 'Coffee break', custom_text_emoji: '' });
    expect(useRichPresenceStore.getState().self.customText).toBe('Coffee break');
  });

  it('includes the chosen emoji in the Save payload and store', async () => {
    let received: Record<string, unknown> | null = null;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({
          custom_text_tier: 0,
          custom_text: 'Launching',
          custom_text_emoji: '🚀',
        });
      })
    );

    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.change(getInput(), { target: { value: 'Launching' } });
    // Open the (mocked) emoji picker and select an emoji.
    fireEvent.click(screen.getByRole('button', { name: /choose emoji/i }));
    expect(screen.getByTestId('emoji-picker')).toBeInTheDocument();
    fireEvent.click(screen.getByTestId('picker-select'));
    // Picker closes after a selection.
    expect(screen.queryByTestId('emoji-picker')).not.toBeInTheDocument();

    fireEvent.click(getSaveBtn());

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(received).toEqual({ custom_text: 'Launching', custom_text_emoji: '🚀' });
    expect(useRichPresenceStore.getState().self.customTextEmoji).toBe('🚀');
  });

  it('closes the emoji picker via its onClose without selecting an emoji', () => {
    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.click(screen.getByRole('button', { name: /choose emoji/i }));
    expect(screen.getByTestId('emoji-picker')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('picker-close'));
    expect(screen.queryByTestId('emoji-picker')).not.toBeInTheDocument();
  });

  it('shows a Remove-emoji control once an emoji is chosen and clears it on click', () => {
    render(<CustomStatusPopover onClose={onClose} />);

    // No emoji yet → no Remove control.
    expect(screen.queryByRole('button', { name: /remove emoji/i })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /choose emoji/i }));
    fireEvent.click(screen.getByTestId('picker-select'));

    const remove = screen.getByRole('button', { name: /remove emoji/i });
    expect(remove).toBeInTheDocument();
    fireEvent.click(remove);
    expect(screen.queryByRole('button', { name: /remove emoji/i })).not.toBeInTheDocument();
  });

  it('updates the remaining-char counter as the user types', () => {
    render(<CustomStatusPopover onClose={onClose} />);

    // Empty → 140 remaining
    expect(screen.getByText('140')).toBeInTheDocument();

    fireEvent.change(getInput(), { target: { value: 'hello' } });
    expect(screen.getByText('135')).toBeInTheDocument();
  });

  it('marks the counter over-limit and disables Save when text exceeds 140 characters', () => {
    // Seed an over-limit value via the store so the popover initializes its
    // input above the cap (bypasses the <input maxLength> truncation a user
    // would normally hit), then assert the over-limit guard.
    useRichPresenceStore.getState().setSelfPresence({ customText: 'x'.repeat(141) });

    render(<CustomStatusPopover onClose={onClose} />);

    expect(getSaveBtn()).toBeDisabled();
    // remaining = 140 - 141 = -1, rendered with the over-limit class.
    const counter = screen.getByText('-1');
    expect(counter).toHaveClass('over-limit');
  });

  it('saves on Enter keydown in the input', async () => {
    let received: Record<string, unknown> | null = null;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({
          custom_text_tier: 0,
          custom_text: 'Typing',
          custom_text_emoji: '',
        });
      })
    );

    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.change(getInput(), { target: { value: 'Typing' } });
    fireEvent.keyDown(getInput(), { key: 'Enter' });

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(received).toEqual({ custom_text: 'Typing', custom_text_emoji: '' });
  });

  it('ignores non-Enter keydown in the input', () => {
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'Typing' } });
    fireEvent.keyDown(getInput(), { key: 'a' });
    expect(onClose).not.toHaveBeenCalled();
  });

  it('clears the status when Save is pressed with empty text', async () => {
    let received: Record<string, unknown> | null = null;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' });
      })
    );

    // Seed an existing status so we can verify it is cleared.
    useRichPresenceStore.getState().setSelfPresence({ customText: 'Busy', customTextEmoji: '🚀' });

    render(<CustomStatusPopover onClose={onClose} />);

    // Clear the input then Save.
    fireEvent.change(getInput(), { target: { value: '' } });
    fireEvent.click(getSaveBtn());

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(received).toEqual({ custom_text: '', custom_text_emoji: '' });
    expect(useRichPresenceStore.getState().self.customText).toBeUndefined();
    expect(useRichPresenceStore.getState().self.customTextEmoji).toBeUndefined();
  });

  it('clears the status via the Clear button', async () => {
    let received: Record<string, unknown> | null = null;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' });
      })
    );

    useRichPresenceStore.getState().setSelfPresence({ customText: 'Busy' });

    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.click(getClearBtn());

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(received).toEqual({ custom_text: '', custom_text_emoji: '' });
    expect(useRichPresenceStore.getState().self.customText).toBeUndefined();
  });

  it('shows an error and does NOT update the store when Save PATCH fails', async () => {
    mswServer.use(
      http.patch(PRESENCE_PATH, () =>
        HttpResponse.json({ error: 'Status update rejected' }, { status: 400 })
      )
    );

    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.change(getInput(), { target: { value: 'Coffee break' } });
    fireEvent.click(getSaveBtn());

    expect(await screen.findByText('Status update rejected')).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
    // Store must NOT be mutated on a failed save.
    expect(useRichPresenceStore.getState().self.customText).toBeUndefined();
    // Save is re-enabled after the failure (saving reset in finally).
    expect(getSaveBtn()).not.toBeDisabled();
  });

  it('falls back to a generic message when the Save error response has no body', async () => {
    mswServer.use(http.patch(PRESENCE_PATH, () => new HttpResponse(null, { status: 500 })));

    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.change(getInput(), { target: { value: 'Coffee break' } });
    fireEvent.click(getSaveBtn());

    expect(await screen.findByText('Failed to update status')).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('shows an error when the Clear PATCH fails', async () => {
    mswServer.use(
      http.patch(PRESENCE_PATH, () =>
        HttpResponse.json({ error: 'Clear rejected' }, { status: 400 })
      )
    );

    useRichPresenceStore.getState().setSelfPresence({ customText: 'Busy' });

    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.click(getClearBtn());

    expect(await screen.findByText('Clear rejected')).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
    // The seeded status remains because the clear failed.
    expect(useRichPresenceStore.getState().self.customText).toBe('Busy');
  });

  it('falls back to a generic message when the Clear error response has no body', async () => {
    mswServer.use(http.patch(PRESENCE_PATH, () => new HttpResponse(null, { status: 500 })));

    useRichPresenceStore.getState().setSelfPresence({ customText: 'Busy' });

    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.click(getClearBtn());

    expect(await screen.findByText('Failed to clear status')).toBeInTheDocument();
  });

  it('closes on Escape when the emoji picker is not open', () => {
    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('does NOT close on Escape while the emoji picker is open', () => {
    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.click(screen.getByRole('button', { name: /choose emoji/i }));
    expect(screen.getByTestId('emoji-picker')).toBeInTheDocument();

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).not.toHaveBeenCalled();
  });

  it('ignores non-Escape document keydown', () => {
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.keyDown(document, { key: 'Enter' });
    expect(onClose).not.toHaveBeenCalled();
  });

  it('only issues one Save PATCH when clicked twice while saving (re-entrancy guard)', async () => {
    let calls = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, async () => {
        calls += 1;
        // Delay so the second click lands while `saving` is still true.
        await new Promise((r) => setTimeout(r, 30));
        return HttpResponse.json({
          custom_text_tier: 0,
          custom_text: 'Busy',
          custom_text_emoji: '',
        });
      })
    );

    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'Busy' } });

    const save = getSaveBtn();
    fireEvent.click(save);
    // Second click hits the `saving` short-circuit (no second request).
    fireEvent.click(save);

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(calls).toBe(1);
  });

  it('only issues one Clear PATCH when clicked twice while saving (re-entrancy guard)', async () => {
    let calls = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, async () => {
        calls += 1;
        await new Promise((r) => setTimeout(r, 30));
        return HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' });
      })
    );

    useRichPresenceStore.getState().setSelfPresence({ customText: 'Busy' });
    render(<CustomStatusPopover onClose={onClose} />);

    const clear = getClearBtn();
    fireEvent.click(clear);
    // Second click hits the `saving` short-circuit in handleClear.
    fireEvent.click(clear);

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(calls).toBe(1);
  });
});
