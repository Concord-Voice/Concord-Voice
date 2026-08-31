import { render, screen, fireEvent, waitFor, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useRichPresenceStore } from '@/renderer/stores/ui/richPresenceStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import { deferred } from '../../../helpers/deferred';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import type { PresenceSettings } from '@/renderer/stores/ui/richPresenceStore';

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

const presenceResponse = (overrides: Record<string, unknown> = {}) => ({
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

const confirmedSettings: PresenceSettings = {
  masterEnabled: true,
  serverVoiceTier: 1,
  serverVoiceShowDetails: true,
  privateCallTier: 0,
  privateCallShowDetails: false,
  customTextTier: 0,
};

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => {
  mswServer.resetHandlers();
  resetRuntimeServerBase();
});

describe('CustomStatusPopover', () => {
  const onClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('does not submit when auth changes during custom status ticket capture', async () => {
    let patchCount = 0;
    let invalidated = false;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return HttpResponse.json({ error: 'stale ticket submitted' }, { status: 400 });
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    const generation = useAuthStore.getState().authGeneration;
    let unsubscribe: (() => void) | undefined;
    unsubscribe = useRichPresenceStore.subscribe((state, previous) => {
      if (previous.customStatusSaving || !state.customStatusSaving) return;
      invalidated = true;
      useAuthStore
        .getState()
        .beginAuthLifecycleIfCurrent(generation, 'successor-token', 'successor-session');
      unsubscribe?.();
    });

    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'stale ticket' } });
    fireEvent.click(getSaveBtn());

    await waitFor(() => expect(invalidated).toBe(true));
    expect(patchCount).toBe(0);
    expect(onClose).not.toHaveBeenCalled();
  });

  const getInput = () =>
    screen.getByRole('textbox', { name: /custom status text/i }) as HTMLInputElement;
  const getSaveBtn = () => screen.getByRole('button', { name: /^save$/i }) as HTMLButtonElement;
  const getClearBtn = () => screen.getByRole('button', { name: /^clear$/i });

  it('renders as a native dialog (implicit role="dialog")', () => {
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
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
        return HttpResponse.json(
          presenceResponse({ custom_text: 'Coffee break', custom_text_emoji: '' })
        );
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
        return HttpResponse.json(
          presenceResponse({ custom_text: 'Launching', custom_text_emoji: '🚀' })
        );
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

  it('updates the accessible remaining-code-point counter after input changes', () => {
    render(<CustomStatusPopover onClose={onClose} />);

    // Empty → 140 remaining
    const input = getInput();
    const counter = screen.getByText('140 code points remaining');
    expect(input).toHaveAttribute('aria-describedby', 'custom-status-counter');
    expect(counter).toHaveAttribute('id', 'custom-status-counter');
    expect(counter).toHaveAttribute('aria-live', 'polite');
    expect(counter).toHaveAttribute('aria-atomic', 'true');

    fireEvent.change(input, { target: { value: 'hello' } });
    expect(screen.getByText('135 code points remaining')).toBeInTheDocument();
  });

  it('allows and saves 140 typed astral code points (#2239)', async () => {
    const text = '😀'.repeat(140);
    let received: Record<string, unknown> | null = null;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(presenceResponse({ custom_text: text, custom_text_emoji: '' }));
      })
    );
    const user = userEvent.setup();

    render(<CustomStatusPopover onClose={onClose} />);
    const input = getInput();
    const save = getSaveBtn();

    await user.type(input, text);

    expect(input).toHaveValue(text);
    expect(input).toHaveAttribute('maxlength', '282');
    expect(input).toHaveAttribute('aria-invalid', 'false');
    expect(screen.getByText('0 code points remaining')).toBeInTheDocument();
    expect(save).not.toBeDisabled();

    await user.click(save);

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(received).toEqual({ custom_text: text, custom_text_emoji: '' });
  });

  it('marks 141 pasted astral code points invalid and disables Save (#2239)', async () => {
    const text = '😀'.repeat(141);
    const user = userEvent.setup();

    render(<CustomStatusPopover onClose={onClose} />);
    const input = getInput();
    await user.click(input);
    await user.paste(text);

    expect(input).toHaveValue(text);
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByText('1 code point over limit')).toHaveClass('over-limit');
    expect(getSaveBtn()).toBeDisabled();
  });

  it('bounds oversized pasted input without hiding the over-limit state (#2239)', async () => {
    const user = userEvent.setup();

    render(<CustomStatusPopover onClose={onClose} />);
    const input = getInput();
    await user.click(input);
    await user.paste('x'.repeat(1_000));

    expect(input).toHaveValue('x'.repeat(282));
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByText('142 code points over limit')).toHaveClass('over-limit');
    expect(getSaveBtn()).toBeDisabled();
  });

  it('marks a hydrated 141-code-point value invalid and disables Save (#2239)', () => {
    const text = '😀'.repeat(141);
    useRichPresenceStore.getState().setSelfPresence({ customText: text });

    render(<CustomStatusPopover onClose={onClose} />);

    expect(getInput()).toHaveValue(text);
    expect(getInput()).toHaveAttribute('aria-invalid', 'true');
    expect(getSaveBtn()).toBeDisabled();
    const counter = screen.getByText('1 code point over limit');
    expect(counter).toHaveClass('over-limit');
  });

  it('counts combining sequences by code point, not grapheme cluster (#2239)', () => {
    const atLimit = 'e\u0301'.repeat(70);
    render(<CustomStatusPopover onClose={onClose} />);
    const input = getInput();

    fireEvent.change(input, { target: { value: atLimit } });
    expect(screen.getByText('0 code points remaining')).toBeInTheDocument();
    expect(input).toHaveAttribute('aria-invalid', 'false');
    expect(getSaveBtn()).not.toBeDisabled();

    fireEvent.change(input, { target: { value: `${atLimit}e` } });
    expect(screen.getByText('1 code point over limit')).toHaveClass('over-limit');
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(getSaveBtn()).toBeDisabled();
  });

  it('saves on Enter keydown in the input', async () => {
    let received: Record<string, unknown> | null = null;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(
          presenceResponse({ custom_text: 'Typing', custom_text_emoji: '' })
        );
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
        return HttpResponse.json(presenceResponse({ custom_text: null, custom_text_emoji: null }));
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
        return HttpResponse.json(presenceResponse({ custom_text: null, custom_text_emoji: null }));
      })
    );

    useRichPresenceStore.getState().setSelfPresence({ customText: 'Busy' });

    render(<CustomStatusPopover onClose={onClose} />);

    fireEvent.click(getClearBtn());

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(received).toEqual({ custom_text: '', custom_text_emoji: '' });
    expect(useRichPresenceStore.getState().self.customText).toBeUndefined();
  });

  it('uses every parsed server value for self, current settings, and confirmed settings', async () => {
    mswServer.use(
      http.patch(PRESENCE_PATH, () =>
        HttpResponse.json(
          presenceResponse({
            custom_text_tier: 2,
            custom_text: 'server value',
            custom_text_emoji: '🛰️',
          })
        )
      )
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'request value' } });
    fireEvent.click(getSaveBtn());

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    const state = useRichPresenceStore.getState();
    expect(state.self).toMatchObject({
      tier: 2,
      customText: 'server value',
      customTextEmoji: '🛰️',
    });
    expect(state.presenceSettings).toMatchObject({
      serverVoiceTier: 1,
      serverVoiceShowDetails: true,
      privateCallTier: 0,
      privateCallShowDetails: false,
      customTextTier: 2,
      customText: 'server value',
      customTextEmoji: '🛰️',
    });
    expect(state.confirmedPresenceSettings).toMatchObject({
      serverVoiceTier: 1,
      serverVoiceShowDetails: true,
      privateCallTier: 0,
      privateCallShowDetails: false,
      customTextTier: 2,
      customText: 'server value',
      customTextEmoji: '🛰️',
    });
  });

  it.each([
    ['Save', 'Failed to update status'],
    ['Clear', 'Failed to clear status'],
  ] as const)(
    'reconciles a schema-invalid 2xx %s and keeps the popover open',
    async (action, error) => {
      let gets = 0;
      mswServer.use(
        http.get(PRESENCE_PATH, () => {
          gets += 1;
          return HttpResponse.json(
            presenceResponse({ custom_text: 'authoritative', custom_text_emoji: null })
          );
        }),
        http.patch(PRESENCE_PATH, () => HttpResponse.json({ custom_text_tier: 0 }))
      );
      useRichPresenceStore.setState({
        presenceSettings: confirmedSettings,
        confirmedPresenceSettings: confirmedSettings,
      });
      render(<CustomStatusPopover onClose={onClose} />);
      if (action === 'Save') fireEvent.change(getInput(), { target: { value: 'request' } });
      fireEvent.click(action === 'Save' ? getSaveBtn() : getClearBtn());

      expect(await screen.findByText(/invalid Rich Presence settings/)).toBeInTheDocument();
      await waitFor(() => expect(gets).toBe(1));
      expect(onClose).not.toHaveBeenCalled();
      expect(screen.getByText(/invalid Rich Presence settings/)).toBeInTheDocument();
      await waitFor(() =>
        expect(useRichPresenceStore.getState().self.customText).toBe('authoritative')
      );
    }
  );

  it.each([
    [
      'Save',
      'Failed to update status',
      () => HttpResponse.json({ error: 'save uncertain' }, { status: 500 }),
    ],
    ['Clear', 'Failed to clear status', () => HttpResponse.error()],
  ] as const)(
    'reconciles ambiguous %s failures and leaves the error visible',
    async (action, error, response) => {
      let gets = 0;
      mswServer.use(
        http.get(PRESENCE_PATH, () => {
          gets += 1;
          return HttpResponse.json(
            presenceResponse({ custom_text: 'server truth', custom_text_emoji: null })
          );
        }),
        http.patch(PRESENCE_PATH, response)
      );
      useRichPresenceStore.setState({
        presenceSettings: confirmedSettings,
        confirmedPresenceSettings: confirmedSettings,
      });
      render(<CustomStatusPopover onClose={onClose} />);
      if (action === 'Save') fireEvent.change(getInput(), { target: { value: 'request' } });
      fireEvent.click(action === 'Save' ? getSaveBtn() : getClearBtn());

      expect(await screen.findByText(/save uncertain|Failed to fetch/)).toBeInTheDocument();
      await waitFor(() => expect(gets).toBe(1));
      expect(onClose).not.toHaveBeenCalled();
      await waitFor(() =>
        expect(useRichPresenceStore.getState().self.customText).toBe('server truth')
      );
      await waitFor(() => {
        const actionButton = action === 'Save' ? getSaveBtn() : getClearBtn();
        expect(actionButton).not.toBeDisabled();
      });
    }
  );

  it.each([
    ['Save', { custom_text: 'request', custom_text_emoji: '🚀' }],
    ['Clear', { custom_text: null, custom_text_emoji: null }],
  ] as const)(
    'closes after an ambiguous %s is confirmed by matching GET',
    async (action, truth) => {
      let patchCount = 0;
      let getCount = 0;
      mswServer.use(
        http.patch(PRESENCE_PATH, () => {
          patchCount += 1;
          return HttpResponse.json({ error: 'save uncertain' }, { status: 503 });
        }),
        http.get(PRESENCE_PATH, () => {
          getCount += 1;
          return HttpResponse.json(presenceResponse(truth));
        })
      );
      useRichPresenceStore.setState({
        presenceSettings: confirmedSettings,
        confirmedPresenceSettings: confirmedSettings,
      });
      if (action === 'Clear') {
        useRichPresenceStore
          .getState()
          .setSelfPresence({ customText: 'existing', customTextEmoji: '🚀' });
      }

      render(<CustomStatusPopover onClose={onClose} />);
      if (action === 'Save') {
        fireEvent.change(getInput(), { target: { value: 'request' } });
        fireEvent.click(screen.getByRole('button', { name: /choose emoji/i }));
        fireEvent.click(screen.getByRole('button', { name: 'Pick rocket' }));
      }
      fireEvent.click(action === 'Save' ? getSaveBtn() : getClearBtn());

      await waitFor(() => expect(getCount).toBe(1));
      await waitFor(() =>
        expect(useRichPresenceStore.getState().confirmedPresenceSettings).not.toBeNull()
      );
      expect(patchCount).toBe(1);
      await waitFor(() => expect(onClose).toHaveBeenCalled());
    }
  );

  it('keeps a matching ambiguous save open when reconciliation fails', async () => {
    let patchCount = 0;
    let getCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return HttpResponse.json({ error: 'save uncertain' }, { status: 503 });
      }),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return HttpResponse.json({ error: 'load failed' }, { status: 503 });
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'request',
      customTextEmoji: '🚀',
    });

    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.click(getSaveBtn());

    expect(await screen.findByText('save uncertain')).toBeInTheDocument();
    await waitFor(() => expect(getCount).toBe(1));
    await waitFor(() =>
      expect(useRichPresenceStore.getState().confirmedPresenceSettings).toBeNull()
    );
    expect(patchCount).toBe(1);
    expect(screen.getByRole('alert')).toHaveTextContent('save uncertain');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('does not close after matching reconciliation when auth changes during store apply', async () => {
    let getCount = 0;
    let patchCount = 0;
    let invalidated = false;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return HttpResponse.json({ error: 'save uncertain' }, { status: 503 });
      }),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return HttpResponse.json(presenceResponse({ custom_text: 'request' }));
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    useRichPresenceStore.getState().setSelfPresence({ customText: 'request' });
    const generation = useAuthStore.getState().authGeneration;
    let unsubscribe: (() => void) | undefined;
    unsubscribe = useRichPresenceStore.subscribe((state, previous) => {
      if (previous.confirmedPresenceSettings !== null || state.confirmedPresenceSettings === null) {
        return;
      }
      invalidated = true;
      useAuthStore
        .getState()
        .beginAuthLifecycleIfCurrent(generation, 'successor-token', 'successor-session');
      unsubscribe?.();
    });

    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.click(getSaveBtn());

    await waitFor(() => expect(getCount).toBe(1));
    await waitFor(() => expect(invalidated).toBe(true));
    expect(invalidated).toBe(true);
    expect(patchCount).toBe(1);
    expect(useRichPresenceStore.getState().self.customText).toBe('request');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('does not close when a queued category mutation invalidates matching reconciliation', async () => {
    const categoryResponse = deferred<Response>();
    let getCount = 0;
    let patchCount = 0;
    let categoryStarted = false;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        if (patchCount === 1) {
          return HttpResponse.json({ error: 'save uncertain' }, { status: 503 });
        }
        categoryStarted = true;
        return categoryResponse.promise;
      }),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return HttpResponse.json(presenceResponse({ custom_text: 'request' }));
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    useRichPresenceStore.getState().setSelfPresence({ customText: 'request' });
    let unsubscribe: (() => void) | undefined;
    unsubscribe = useRichPresenceStore.subscribe((state, previous) => {
      if (previous.confirmedPresenceSettings !== null || state.confirmedPresenceSettings === null) {
        return;
      }
      queueMicrotask(() => {
        void useRichPresenceStore.getState().updatePresenceSettings({ serverVoiceTier: 2 });
      });
      unsubscribe?.();
    });

    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.click(getSaveBtn());
    await waitFor(() => expect(getCount).toBe(1));
    await waitFor(() => expect(categoryStarted).toBe(true));
    expect(patchCount).toBe(2);
    try {
      expect(onClose).not.toHaveBeenCalled();
      expect(useRichPresenceStore.getState().presenceSettingsSaving).toBe(true);
    } finally {
      categoryResponse.resolve(
        new Response(JSON.stringify(presenceResponse({ server_voice_tier: 2 })), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
      await waitFor(() =>
        expect(useRichPresenceStore.getState().presenceSettingsSaving).toBe(false)
      );
    }
  });

  it('keeps an ambiguous save unconfirmed when reconciliation GET fails', async () => {
    mswServer.use(
      http.get(PRESENCE_PATH, () => HttpResponse.json({ error: 'load failed' }, { status: 503 })),
      http.patch(PRESENCE_PATH, () =>
        HttpResponse.json({ error: 'save uncertain' }, { status: 500 })
      )
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'request' } });
    fireEvent.click(getSaveBtn());
    await screen.findByText('save uncertain');
    expect(screen.getByRole('alert')).toHaveTextContent('save uncertain');
    await waitFor(() =>
      expect(useRichPresenceStore.getState().confirmedPresenceSettings).toBeNull()
    );
    expect(onClose).not.toHaveBeenCalled();
    expect(getSaveBtn()).toBeDisabled();
    expect(getClearBtn()).toBeDisabled();
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
  });

  it.each(['Save', 'Clear'] as const)(
    'does not retry an ambiguous %s after reconciliation fails, but permits recovery',
    async (action) => {
      let patchCount = 0;
      let getCount = 0;
      const recoveryResponse = deferred<Response>();
      mswServer.use(
        http.patch(PRESENCE_PATH, () => {
          patchCount += 1;
          return HttpResponse.json({ error: 'save uncertain' }, { status: 503 });
        }),
        http.get(PRESENCE_PATH, () => {
          getCount += 1;
          if (getCount === 1) return HttpResponse.json({ error: 'load failed' }, { status: 503 });
          return recoveryResponse.promise;
        })
      );
      useRichPresenceStore.setState({
        presenceSettings: confirmedSettings,
        confirmedPresenceSettings: confirmedSettings,
      });
      render(<CustomStatusPopover onClose={onClose} />);
      if (action === 'Save') fireEvent.change(getInput(), { target: { value: 'request' } });
      const actionButton = action === 'Save' ? getSaveBtn() : getClearBtn();
      fireEvent.click(actionButton);

      await screen.findByText('save uncertain');
      await waitFor(() => expect(getSaveBtn()).toBeDisabled());
      expect(getClearBtn()).toBeDisabled();
      expect(patchCount).toBe(1);
      expect(useRichPresenceStore.getState().confirmedPresenceSettings).toBeNull();

      fireEvent.click(screen.getByRole('button', { name: /try again/i }));
      await waitFor(() => expect(getCount).toBe(2));
      expect(screen.getByRole('button', { name: /retrying/i })).toBeInTheDocument();
      expect(getSaveBtn()).toBeDisabled();
      expect(getClearBtn()).toBeDisabled();
      fireEvent.click(actionButton);
      expect(patchCount).toBe(1);
      recoveryResponse.resolve(
        new Response(JSON.stringify(presenceResponse({ custom_text: 'recovered' })), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
      await waitFor(() => expect(actionButton).toBeEnabled());
      expect(useRichPresenceStore.getState().confirmedPresenceSettings?.customText).toBe(
        'recovered'
      );
      expect(useRichPresenceStore.getState().presenceSettingsError).toBeNull();
    }
  );

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

  it('clears a category rejection error after a fully current direct status succeeds', async () => {
    let patchCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        patchCount += 1;
        const body = (await request.json()) as { custom_text?: string };
        if (patchCount === 1) {
          expect(body).toEqual({ server_voice_tier: 2 });
          return HttpResponse.json({ error: 'category rejected' }, { status: 400 });
        }
        return HttpResponse.json(presenceResponse({ custom_text: body.custom_text }));
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });

    await useRichPresenceStore.getState().updatePresenceSettings({ serverVoiceTier: 2 });
    expect(useRichPresenceStore.getState().presenceSettingsError).toBe('category rejected');
    expect(useRichPresenceStore.getState().confirmedPresenceSettings).toEqual(confirmedSettings);

    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'current status' } });
    fireEvent.click(getSaveBtn());

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(patchCount).toBe(2);
    expect(useRichPresenceStore.getState().confirmedPresenceSettings?.customText).toBe(
      'current status'
    );
    expect(useRichPresenceStore.getState().presenceSettingsError).toBeNull();
  });

  it.each(['Save', 'Clear'] as const)(
    'does not reconcile a definite 4xx %s failure',
    async (action) => {
      let getCount = 0;
      mswServer.use(
        http.get(PRESENCE_PATH, () => {
          getCount += 1;
          return HttpResponse.json(presenceResponse());
        }),
        http.patch(PRESENCE_PATH, () => HttpResponse.json({ error: 'rejected' }, { status: 400 }))
      );
      if (action === 'Clear')
        useRichPresenceStore.getState().setSelfPresence({ customText: 'Busy' });
      render(<CustomStatusPopover onClose={onClose} />);
      if (action === 'Save') fireEvent.change(getInput(), { target: { value: 'request' } });
      fireEvent.click(action === 'Save' ? getSaveBtn() : getClearBtn());
      await screen.findByText('rejected');
      expect(getCount).toBe(0);
      expect(onClose).not.toHaveBeenCalled();
    }
  );

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
        return HttpResponse.json(presenceResponse({ custom_text: 'Busy', custom_text_emoji: '' }));
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
        return HttpResponse.json(presenceResponse({ custom_text: '', custom_text_emoji: '' }));
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

  it.each(['Save', 'Clear'] as const)(
    'keeps a newer category mutation while applying the direct %s response fields',
    async (action) => {
      const directResponse = deferred<Response>();
      const categoryResponse = deferred<Response>();
      let patchCount = 0;
      mswServer.use(
        http.patch(PRESENCE_PATH, async () => {
          patchCount += 1;
          if (patchCount === 1) return directResponse.promise;
          return categoryResponse.promise;
        })
      );
      useRichPresenceStore.setState({
        presenceSettings: confirmedSettings,
        confirmedPresenceSettings: confirmedSettings,
      });
      render(<CustomStatusPopover onClose={onClose} />);
      if (action === 'Save') fireEvent.change(getInput(), { target: { value: 'direct text' } });
      const actionButton = action === 'Save' ? getSaveBtn() : getClearBtn();
      fireEvent.click(actionButton);
      await waitFor(() => expect(actionButton).toBeDisabled());

      const categoryMutation = useRichPresenceStore
        .getState()
        .updatePresenceSettings({ serverVoiceTier: 2 });
      await waitFor(() =>
        expect(useRichPresenceStore.getState().presenceSettingsSaving).toBe(true)
      );
      directResponse.resolve(
        new Response(
          JSON.stringify(
            presenceResponse({
              server_voice_tier: 1,
              private_call_show_details: false,
              custom_text_tier: 2,
              custom_text: action === 'Clear' ? null : 'direct response',
              custom_text_emoji: action === 'Clear' ? null : '🛰️',
            })
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );

      await waitFor(() => expect(actionButton).toBeDisabled());
      expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
      const state = useRichPresenceStore.getState();
      expect(patchCount).toBe(2);
      expect(state.presenceSettings.serverVoiceTier).toBe(2);
      expect(state.presenceSettings.privateCallShowDetails).toBe(false);
      expect(state.presenceSettings.customText).toBe(
        action === 'Clear' ? undefined : 'direct response'
      );
      expect(state.self.tier).toBe(0);
      if (action === 'Clear') {
        expect(state.self.customText).toBeUndefined();
        expect(state.self.customTextEmoji).toBeUndefined();
      } else {
        expect(state.self).toMatchObject({ customText: 'direct response', customTextEmoji: '🛰️' });
      }
      expect(state.confirmedPresenceSettings).toBeNull();
      expect(state.presenceSettingsSaving).toBe(true);
      expect(state.presenceSettingsError).toBe(
        'Settings changed while saving your status. Reload settings to continue.'
      );
      expect(onClose).not.toHaveBeenCalled();

      categoryResponse.resolve(
        new Response(
          JSON.stringify(
            presenceResponse({
              server_voice_tier: 2,
              private_call_show_details: true,
              custom_text: 'category response',
              custom_text_emoji: '🧭',
            })
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
      await categoryMutation;
      const finalState = useRichPresenceStore.getState();
      expect(finalState.presenceSettings.privateCallShowDetails).toBe(true);
      if (action === 'Clear') {
        expect(finalState.self.customText).toBeUndefined();
        expect(finalState.self.customTextEmoji).toBeUndefined();
      } else {
        expect(finalState.self).toMatchObject({
          customText: 'direct response',
          customTextEmoji: '🛰️',
        });
      }
      expect(finalState.confirmedPresenceSettings).toBeNull();
      expect(finalState.presenceSettingsError).toBe(
        'Settings changed while saving your status. Reload settings to continue.'
      );
    }
  );

  it('does not confirm category success while a direct Save is still pending', async () => {
    const directResponse = deferred<Response>();
    let patchCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        if (patchCount === 1) return directResponse.promise;
        return HttpResponse.json(
          presenceResponse({ server_voice_tier: 2, custom_text: 'category response' })
        );
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'direct response' } });
    const actionButton = getSaveBtn();
    fireEvent.click(actionButton);
    await waitFor(() => expect(actionButton).toBeDisabled());

    const categoryMutation = useRichPresenceStore
      .getState()
      .updatePresenceSettings({ serverVoiceTier: 2 });
    await categoryMutation;
    const duringDirect = useRichPresenceStore.getState();
    expect(patchCount).toBe(2);
    expect(duringDirect.presenceSettings.serverVoiceTier).toBe(2);
    expect(duringDirect.confirmedPresenceSettings).toBeNull();
    expect(duringDirect.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );

    directResponse.resolve(
      new Response(
        JSON.stringify(
          presenceResponse({ custom_text: 'direct response', custom_text_emoji: '🛰️' })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(actionButton).toBeDisabled());
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
    const finalState = useRichPresenceStore.getState();
    expect(finalState.presenceSettings.serverVoiceTier).toBe(2);
    expect(finalState.self).toMatchObject({
      customText: 'direct response',
      customTextEmoji: '🛰️',
    });
    expect(finalState.confirmedPresenceSettings).toBeNull();
    expect(finalState.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );
    expect(onClose).not.toHaveBeenCalled();
  });

  it('keeps retry feedback when direct success arrives before the newer category response', async () => {
    const directResponse = deferred<Response>();
    const categoryResponse = deferred<Response>();
    let patchCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return patchCount === 1 ? directResponse.promise : categoryResponse.promise;
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'custom A' } });
    const actionButton = getSaveBtn();
    fireEvent.click(actionButton);
    await waitFor(() => expect(actionButton).toBeDisabled());

    const categoryMutation = useRichPresenceStore
      .getState()
      .updatePresenceSettings({ serverVoiceTier: 2 });
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'custom B',
      customTextEmoji: '🌱',
    });
    await waitFor(() => expect(patchCount).toBe(2));
    directResponse.resolve(
      new Response(
        JSON.stringify(
          presenceResponse({
            server_voice_tier: 1,
            custom_text: 'custom A',
            custom_text_emoji: '🛰️',
          })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(actionButton).toBeDisabled());
    const retry = screen.getByRole('button', { name: /try again/i });
    expect(retry).toBeDisabled();
    const duringCategory = useRichPresenceStore.getState();
    expect(duringCategory.presenceSettings.serverVoiceTier).toBe(2);
    expect(duringCategory.self).toMatchObject({ customText: 'custom B', customTextEmoji: '🌱' });
    expect(duringCategory.confirmedPresenceSettings).toBeNull();
    expect(duringCategory.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );
    expect(onClose).not.toHaveBeenCalled();

    categoryResponse.resolve(
      new Response(
        JSON.stringify(
          presenceResponse({
            server_voice_tier: 2,
            custom_text: 'category response',
            custom_text_emoji: '🧭',
          })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await categoryMutation;
    const finalState = useRichPresenceStore.getState();
    expect(finalState.presenceSettings.serverVoiceTier).toBe(2);
    expect(finalState.self).toMatchObject({ customText: 'custom B', customTextEmoji: '🌱' });
    expect(finalState.confirmedPresenceSettings).toBeNull();
    expect(finalState.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );
  });

  it('coalesces immediate recovery retries after a presence settings save', async () => {
    const recoveryResponse = deferred<Response>();
    let getCount = 0;
    mswServer.use(
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return recoveryResponse.promise;
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: null,
      presenceSettingsError:
        'Settings changed while saving your status. Reload settings to continue.',
      presenceSettingsSaving: true,
    });

    render(<CustomStatusPopover onClose={onClose} />);

    const retry = screen.getByRole('button', { name: /try again/i });
    expect(retry).toBeDisabled();
    useRichPresenceStore.setState({ presenceSettingsSaving: false });
    await waitFor(() => expect(retry).toBeEnabled());

    fireEvent.click(retry);
    fireEvent.click(retry);
    await waitFor(() => expect(getCount).toBe(1));

    recoveryResponse.resolve(
      new Response(JSON.stringify(presenceResponse()), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  });

  it('does not retry an ambiguous direct write once both axes are stale', async () => {
    const directResponse = deferred<Response>();
    const categoryResponse = deferred<Response>();
    let patchCount = 0;
    let getCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return patchCount === 1 ? directResponse.promise : categoryResponse.promise;
      }),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return HttpResponse.json(presenceResponse());
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'custom A' } });
    const actionButton = getSaveBtn();
    fireEvent.click(actionButton);
    await waitFor(() => expect(actionButton).toBeDisabled());

    const categoryMutation = useRichPresenceStore
      .getState()
      .updatePresenceSettings({ serverVoiceTier: 2 });
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'custom B',
      customTextEmoji: '🌱',
    });
    await waitFor(() => expect(patchCount).toBe(2));
    directResponse.resolve(HttpResponse.json({ error: 'uncertain' }, { status: 503 }));
    await screen.findByText('uncertain');
    expect(getCount).toBe(0);

    categoryResponse.resolve(
      new Response(JSON.stringify(presenceResponse({ server_voice_tier: 2 })), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await categoryMutation;
    await waitFor(() => expect(actionButton).toBeDisabled());
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
    const state = useRichPresenceStore.getState();
    expect(patchCount).toBe(2);
    expect(state.presenceSettings.serverVoiceTier).toBe(2);
    expect(state.self).toMatchObject({ customText: 'custom B', customTextEmoji: '🌱' });
    expect(state.confirmedPresenceSettings).toBeNull();
    expect(state.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );
  });

  it.each(['Save', 'Clear'] as const)(
    'does not overwrite a newer custom successor with a direct %s response',
    async (action) => {
      const response = deferred<Response>();
      mswServer.use(http.patch(PRESENCE_PATH, () => response.promise));
      useRichPresenceStore.getState().setSelfPresence({
        customText: 'predecessor',
        customTextEmoji: '🚀',
      });
      render(<CustomStatusPopover onClose={onClose} />);
      if (action === 'Save') fireEvent.change(getInput(), { target: { value: 'request' } });
      const actionButton = action === 'Save' ? getSaveBtn() : getClearBtn();
      fireEvent.click(actionButton);
      await waitFor(() => expect(actionButton).toBeDisabled());

      useRichPresenceStore.getState().setSelfPresence({
        customText: 'successor',
        customTextEmoji: '🌱',
      });
      response.resolve(
        new Response(
          JSON.stringify(
            presenceResponse({ custom_text: 'predecessor response', custom_text_emoji: '🚀' })
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );

      await waitFor(() => expect(actionButton).toBeDisabled());
      expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
      const state = useRichPresenceStore.getState();
      expect(state.self).toMatchObject({ customText: 'successor', customTextEmoji: '🌱' });
      expect(state.confirmedPresenceSettings).toBeNull();
      expect(state.presenceSettingsError).toBe(
        'Settings changed while saving your status. Reload settings to continue.'
      );
      expect(onClose).not.toHaveBeenCalled();
    }
  );

  it('preserves a newer custom successor during ambiguous reconciliation', async () => {
    const getResponse = deferred<Response>();
    let getCount = 0;
    let patchCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return HttpResponse.json({ error: 'uncertain' }, { status: 503 });
      }),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return getResponse.promise;
      })
    );
    useRichPresenceStore.getState().setSelfPresence({ customText: 'predecessor' });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'request' } });
    const actionButton = getSaveBtn();
    fireEvent.click(actionButton);
    await screen.findByText('uncertain');
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'successor',
      customTextEmoji: '🌱',
    });
    getResponse.resolve(
      new Response(
        JSON.stringify(presenceResponse({ custom_text: 'stale GET', custom_text_emoji: '🚀' })),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(actionButton).toBeDisabled());
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
    const state = useRichPresenceStore.getState();
    expect(patchCount).toBe(1);
    expect(getCount).toBe(1);
    expect(state.self).toMatchObject({ customText: 'successor', customTextEmoji: '🌱' });
    expect(state.confirmedPresenceSettings).toBeNull();
  });

  it('fences an older ambiguous direct write from a newer direct write', async () => {
    const firstGet = deferred<Response>();
    let patchCount = 0;
    let getCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        if (patchCount === 1) return HttpResponse.json({ error: 'A uncertain' }, { status: 503 });
        return HttpResponse.json({ error: 'second request must be blocked' }, { status: 400 });
      }),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return firstGet.promise;
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });

    const firstClose = vi.fn();
    render(<CustomStatusPopover onClose={firstClose} />);
    fireEvent.change(getInput(), { target: { value: 'direct A' } });
    const firstSave = getSaveBtn();
    fireEvent.click(firstSave);
    await screen.findByText('A uncertain');
    await waitFor(() => expect(getCount).toBe(1));

    const secondClose = vi.fn();
    render(<CustomStatusPopover onClose={secondClose} />);
    const inputs = screen.getAllByRole('textbox', { name: /custom status text/i });
    const secondInput = inputs[inputs.length - 1];
    fireEvent.change(secondInput, { target: { value: 'direct B' } });
    const saves = screen.getAllByRole('button', { name: /^save$/i });
    const secondSave = saves[saves.length - 1];
    fireEvent.click(secondSave);
    await waitFor(() => expect(secondSave).toBeDisabled());

    firstGet.resolve(
      new Response(
        JSON.stringify(presenceResponse({ custom_text: 'stale A', custom_text_emoji: '🅰️' })),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(secondSave).toBeEnabled());

    const state = useRichPresenceStore.getState();
    expect(patchCount).toBe(1);
    expect(getCount).toBe(1);
    expect(state.self).toMatchObject({ customText: 'stale A', customTextEmoji: '🅰️' });
    expect(state.presenceSettings).toMatchObject({
      customText: 'stale A',
      customTextEmoji: '🅰️',
    });
    expect(state.confirmedPresenceSettings).toMatchObject({
      customText: 'stale A',
      customTextEmoji: '🅰️',
    });
    expect(firstClose).not.toHaveBeenCalled();
    expect(secondClose).not.toHaveBeenCalled();
  });

  it('serializes direct submissions across popover instances', async () => {
    const firstPatch = deferred<Response>();
    let patchCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return firstPatch.promise;
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={vi.fn()} />);
    const firstInput = getInput();
    fireEvent.change(firstInput, { target: { value: 'direct A' } });
    const firstSave = getSaveBtn();
    fireEvent.click(firstSave);
    await waitFor(() => expect(firstSave).toBeDisabled());

    render(<CustomStatusPopover onClose={vi.fn()} />);
    const inputs = screen.getAllByRole('textbox', { name: /custom status text/i });
    fireEvent.change(inputs[inputs.length - 1], { target: { value: 'direct B' } });
    const saves = screen.getAllByRole('button', { name: /^save$/i });
    const secondSave = saves[saves.length - 1];
    expect(secondSave).toBeDisabled();
    fireEvent.click(secondSave);
    expect(patchCount).toBe(1);

    firstPatch.resolve(
      new Response(
        JSON.stringify(presenceResponse({ custom_text: 'direct A', custom_text_emoji: '🅰️' })),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(firstSave).toBeEnabled());
    await waitFor(() => expect(secondSave).toBeEnabled());
    expect(patchCount).toBe(1);
    expect(useRichPresenceStore.getState().confirmedPresenceSettings).toMatchObject({
      customText: 'direct A',
      customTextEmoji: '🅰️',
    });
  });

  it('does not invoke callbacks when a pending editor is unmounted', async () => {
    const response = deferred<Response>();
    const predecessorClose = vi.fn();
    const successorClose = vi.fn();
    mswServer.use(http.patch(PRESENCE_PATH, () => response.promise));
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });

    const predecessor = render(<CustomStatusPopover onClose={predecessorClose} />);
    fireEvent.change(getInput(), { target: { value: 'pending status' } });
    const predecessorSave = getSaveBtn();
    fireEvent.click(predecessorSave);
    await waitFor(() => expect(predecessorSave).toBeDisabled());
    predecessor.unmount();

    render(<CustomStatusPopover onClose={successorClose} />);
    response.resolve(
      new Response(JSON.stringify(presenceResponse({ custom_text: 'pending status' })), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await waitFor(() => expect(useRichPresenceStore.getState().customStatusSaving).toBe(false));

    expect(predecessorClose).not.toHaveBeenCalled();
    expect(successorClose).not.toHaveBeenCalled();
  });

  it('refreshes a successor reopened after Escape when a pending save completes', async () => {
    const response = deferred<Response>();
    const predecessorClose = vi.fn();
    const successorClose = vi.fn();
    mswServer.use(http.patch(PRESENCE_PATH, () => response.promise));
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'initial status',
      customTextEmoji: '🙂',
    });

    const predecessor = render(<CustomStatusPopover onClose={predecessorClose} />);
    fireEvent.change(getInput(), { target: { value: 'predecessor status' } });
    fireEvent.click(screen.getByRole('button', { name: /choose emoji/i }));
    fireEvent.click(screen.getByRole('button', { name: 'Pick rocket' }));
    const predecessorSave = getSaveBtn();
    fireEvent.click(predecessorSave);
    await waitFor(() => expect(predecessorSave).toBeDisabled());

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(predecessorClose).toHaveBeenCalled();
    predecessor.unmount();

    render(<CustomStatusPopover onClose={successorClose} />);
    response.resolve(
      new Response(
        JSON.stringify(presenceResponse({ custom_text: 'server truth', custom_text_emoji: '🛰️' })),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );

    await waitFor(() => expect(useRichPresenceStore.getState().customStatusSaving).toBe(false));
    expect(useRichPresenceStore.getState().self).toMatchObject({
      customText: 'server truth',
      customTextEmoji: '🛰️',
    });
    expect(screen.getByRole('dialog', { name: 'Set custom status' })).toBeInTheDocument();
    expect(successorClose).not.toHaveBeenCalled();
    await waitFor(() => {
      expect(getInput()).toHaveValue('server truth');
      expect(screen.getByRole('button', { name: /choose emoji/i })).toHaveTextContent('🛰️');
    });
  });

  it('preserves a successor text draft while refreshing untouched emoji', async () => {
    const response = deferred<Response>();
    const predecessorClose = vi.fn();
    const successorClose = vi.fn();
    mswServer.use(http.patch(PRESENCE_PATH, () => response.promise));
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'initial status',
      customTextEmoji: '🙂',
    });

    const predecessor = render(<CustomStatusPopover onClose={predecessorClose} />);
    fireEvent.change(getInput(), { target: { value: 'predecessor status' } });
    fireEvent.click(screen.getByRole('button', { name: /choose emoji/i }));
    fireEvent.click(screen.getByRole('button', { name: 'Pick rocket' }));
    const predecessorSave = getSaveBtn();
    fireEvent.click(predecessorSave);
    await waitFor(() => expect(predecessorSave).toBeDisabled());

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(predecessorClose).toHaveBeenCalled();
    predecessor.unmount();

    render(<CustomStatusPopover onClose={successorClose} />);
    fireEvent.change(getInput(), { target: { value: 'successor draft' } });
    response.resolve(
      new Response(
        JSON.stringify(presenceResponse({ custom_text: 'server truth', custom_text_emoji: '🛰️' })),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );

    await waitFor(() => expect(useRichPresenceStore.getState().customStatusSaving).toBe(false));
    expect(useRichPresenceStore.getState().self).toMatchObject({
      customText: 'server truth',
      customTextEmoji: '🛰️',
    });
    await waitFor(() => {
      expect(getInput()).toHaveValue('successor draft');
      expect(screen.getByRole('button', { name: /choose emoji/i })).toHaveTextContent('🛰️');
    });
    expect(successorClose).not.toHaveBeenCalled();
  });

  it('submits refreshed server values when the predecessor unlocks before passive effects run', async () => {
    const response = deferred<Response>();
    let patchCount = 0;
    let successorBody: Record<string, unknown> | null = null;
    const predecessorClose = vi.fn();
    const successorClose = vi.fn();
    mswServer.use(
      http.patch(PRESENCE_PATH, async ({ request }) => {
        patchCount += 1;
        if (patchCount === 1) return response.promise;
        successorBody = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(
          presenceResponse({ custom_text: 'server truth', custom_text_emoji: '🛰️' })
        );
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'predecessor-era value',
      customTextEmoji: '🚀',
    });

    const predecessor = render(<CustomStatusPopover onClose={predecessorClose} />);
    fireEvent.change(getInput(), { target: { value: 'pending predecessor' } });
    const predecessorSave = getSaveBtn();
    fireEvent.click(predecessorSave);
    await waitFor(() => expect(predecessorSave).toBeDisabled());
    fireEvent.keyDown(document, { key: 'Escape' });
    predecessor.unmount();

    render(<CustomStatusPopover onClose={successorClose} />);
    const successorSave = getSaveBtn();
    let successorSubmitted = false;
    const unlockObserver = new MutationObserver(() => {
      if (!successorSave.disabled && !successorSubmitted) {
        successorSubmitted = true;
        successorSave.click();
      }
    });
    unlockObserver.observe(successorSave, {
      attributes: true,
      attributeFilter: ['disabled'],
    });
    response.resolve(
      new Response(
        JSON.stringify(presenceResponse({ custom_text: 'server truth', custom_text_emoji: '🛰️' })),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );

    await waitFor(() => expect(patchCount).toBe(2));
    unlockObserver.disconnect();
    expect(successorBody).toEqual({
      custom_text: 'server truth',
      custom_text_emoji: '🛰️',
    });
    expect(useRichPresenceStore.getState().self).toMatchObject({
      customText: 'server truth',
      customTextEmoji: '🛰️',
    });
    expect(successorClose).toHaveBeenCalled();
  });

  it('reconciles an errored PATCH after unmount without leaking error or callbacks', async () => {
    const patchResponse = deferred<Response>();
    let getCount = 0;
    const predecessorClose = vi.fn();
    const successorClose = vi.fn();
    mswServer.use(
      http.patch(PRESENCE_PATH, () => patchResponse.promise),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return HttpResponse.json(presenceResponse({ custom_text: 'server truth' }));
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });

    const predecessor = render(<CustomStatusPopover onClose={predecessorClose} />);
    fireEvent.change(getInput(), { target: { value: 'pending status' } });
    const predecessorSave = getSaveBtn();
    fireEvent.click(predecessorSave);
    await waitFor(() => expect(predecessorSave).toBeDisabled());
    predecessor.unmount();

    render(<CustomStatusPopover onClose={successorClose} />);
    patchResponse.resolve(Response.error());
    await waitFor(() => expect(getCount).toBe(1));
    await waitFor(() => expect(useRichPresenceStore.getState().customStatusSaving).toBe(false));

    expect(useRichPresenceStore.getState().presenceSettingsError).toBeNull();
    expect(useRichPresenceStore.getState().self.customText).toBe('server truth');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(predecessorClose).not.toHaveBeenCalled();
    expect(successorClose).not.toHaveBeenCalled();
  });

  it('preserves live custom ownership across a concurrent hydrate while direct A is active', async () => {
    const directResponse = deferred<Response>();
    let getCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => directResponse.promise),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return HttpResponse.json(
          presenceResponse({
            master_enabled: false,
            server_voice_tier: 2,
            custom_text_tier: 0,
            custom_text: null,
            custom_text_emoji: null,
          })
        );
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'live custom',
      customTextEmoji: '🌱',
    });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'custom A' } });
    const actionButton = getSaveBtn();
    fireEvent.click(actionButton);
    await waitFor(() => expect(actionButton).toBeDisabled());

    await useRichPresenceStore.getState().hydratePresenceSettings();
    const hydrated = useRichPresenceStore.getState();
    expect(getCount).toBe(1);
    expect(hydrated.presenceSettings.masterEnabled).toBe(false);
    expect(hydrated.presenceSettings.serverVoiceTier).toBe(2);
    expect(hydrated.self).toMatchObject({ customText: 'live custom', customTextEmoji: '🌱' });
    expect(hydrated.confirmedPresenceSettings).toBeNull();
    expect(hydrated.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );

    directResponse.resolve(
      new Response(
        JSON.stringify(
          presenceResponse({
            master_enabled: true,
            server_voice_tier: 1,
            custom_text_tier: 2,
            custom_text: 'custom A',
            custom_text_emoji: '🛰️',
          })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(actionButton).toBeDisabled());
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
    const finalState = useRichPresenceStore.getState();
    expect(finalState.presenceSettings.serverVoiceTier).toBe(2);
    expect(finalState.self).toMatchObject({
      tier: 0,
      customText: 'custom A',
      customTextEmoji: '🛰️',
    });
    expect(finalState.confirmedPresenceSettings).toBeNull();
    expect(finalState.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );
    expect(onClose).not.toHaveBeenCalled();
  });

  it('keeps retry feedback when both ownership axes become stale', async () => {
    const directResponse = deferred<Response>();
    const categoryResponse = deferred<Response>();
    let patchCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return patchCount === 1 ? directResponse.promise : categoryResponse.promise;
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'custom A' } });
    const actionButton = getSaveBtn();
    fireEvent.click(actionButton);
    await waitFor(() => expect(actionButton).toBeDisabled());

    const categoryMutation = useRichPresenceStore
      .getState()
      .updatePresenceSettings({ serverVoiceTier: 2 });
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'custom B',
      customTextEmoji: '🌱',
    });
    await waitFor(() => expect(patchCount).toBe(2));
    categoryResponse.resolve(
      new Response(JSON.stringify(presenceResponse({ server_voice_tier: 2 })), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await categoryMutation;
    directResponse.resolve(
      new Response(
        JSON.stringify(
          presenceResponse({
            server_voice_tier: 1,
            custom_text: 'custom A',
            custom_text_emoji: '🛰️',
          })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(actionButton).toBeDisabled());
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
    const state = useRichPresenceStore.getState();
    expect(state.presenceSettings.serverVoiceTier).toBe(2);
    expect(state.self).toMatchObject({ customText: 'custom B', customTextEmoji: '🌱' });
    expect(state.confirmedPresenceSettings).toBeNull();
    expect(state.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );
    expect(
      screen.getByText('Settings changed while saving your status. Reload settings to continue.')
    ).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('keeps the cross-instance submission lock through ambiguous reconciliation', async () => {
    const firstGet = deferred<Response>();
    let patchCount = 0;
    let getCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return HttpResponse.json({ error: 'uncertain' }, { status: 503 });
      }),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return firstGet.promise;
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={vi.fn()} />);
    fireEvent.change(getInput(), { target: { value: 'direct A' } });
    const firstSave = getSaveBtn();
    fireEvent.click(firstSave);
    await screen.findByText('uncertain');
    await waitFor(() => expect(getCount).toBe(1));

    render(<CustomStatusPopover onClose={vi.fn()} />);
    const saves = screen.getAllByRole('button', { name: /^save$/i });
    const secondSave = saves[saves.length - 1];
    expect(secondSave).toBeDisabled();
    fireEvent.click(secondSave);
    expect(patchCount).toBe(1);

    firstGet.resolve(
      new Response(
        JSON.stringify(presenceResponse({ custom_text: 'direct A', custom_text_emoji: '🅰️' })),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(secondSave).toBeEnabled());
    expect(patchCount).toBe(1);
    expect(useRichPresenceStore.getState().confirmedPresenceSettings).toMatchObject({
      customText: 'direct A',
      customTextEmoji: '🅰️',
    });
  });

  it('keeps a newer category response when an older direct response arrives afterward', async () => {
    const categoryResponse = deferred<Response>();
    const directResponse = deferred<Response>();
    let patchCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        return patchCount === 1 ? categoryResponse.promise : directResponse.promise;
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    const categoryMutation = useRichPresenceStore
      .getState()
      .updatePresenceSettings({ serverVoiceTier: 2 });
    await waitFor(() => expect(useRichPresenceStore.getState().presenceSettingsSaving).toBe(true));

    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'direct text' } });
    const actionButton = getSaveBtn();
    fireEvent.click(actionButton);
    await waitFor(() => expect(actionButton).toBeDisabled());

    categoryResponse.resolve(
      new Response(
        JSON.stringify(
          presenceResponse({
            server_voice_tier: 2,
            custom_text_tier: 0,
            custom_text: 'category text',
            custom_text_emoji: '🧭',
          })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await categoryMutation;
    directResponse.resolve(
      new Response(
        JSON.stringify(
          presenceResponse({
            server_voice_tier: 1,
            custom_text_tier: 2,
            custom_text: 'direct text',
            custom_text_emoji: '🛰️',
          })
        ),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(actionButton).toBeDisabled());
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();

    const state = useRichPresenceStore.getState();
    expect(patchCount).toBe(2);
    expect(state.presenceSettings.serverVoiceTier).toBe(2);
    expect(state.presenceSettings.customTextTier).toBe(0);
    expect(state.self).toMatchObject({ tier: 0, customText: 'direct text', customTextEmoji: '🛰️' });
    expect(state.confirmedPresenceSettings).toBeNull();
    expect(state.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );
    expect(onClose).not.toHaveBeenCalled();
  });

  it('fences an old ambiguous reconciliation GET from a newer hydration', async () => {
    const oldGet = deferred<Response>();
    let getCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => HttpResponse.json({ error: 'uncertain' }, { status: 503 })),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        if (getCount === 1) return oldGet.promise;
        return HttpResponse.json(presenceResponse({ server_voice_tier: 2 }));
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'request' } });
    const actionButton = getSaveBtn();
    fireEvent.click(actionButton);
    await screen.findByText('uncertain');
    await waitFor(() => expect(getCount).toBe(1));

    const newerHydration = useRichPresenceStore.getState().hydratePresenceSettings();
    await waitFor(() => expect(getCount).toBe(2));
    await newerHydration;
    oldGet.resolve(
      new Response(JSON.stringify(presenceResponse({ server_voice_tier: 1 })), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await waitFor(() => expect(actionButton).toBeDisabled());
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();

    const state = useRichPresenceStore.getState();
    expect(getCount).toBe(2);
    expect(state.presenceSettings.serverVoiceTier).toBe(2);
    expect(state.confirmedPresenceSettings).toBeNull();
    expect(state.presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );
  });

  it('preserves a newer category mutation during ambiguous reconciliation', async () => {
    const directResponse = deferred<Response>();
    const getResponse = deferred<Response>();
    let getCount = 0;
    let patchCount = 0;
    mswServer.use(
      http.patch(PRESENCE_PATH, () => {
        patchCount += 1;
        if (patchCount === 1) {
          return directResponse.promise;
        }
        return HttpResponse.json(
          presenceResponse({ server_voice_tier: 2, custom_text: 'category response' })
        );
      }),
      http.get(PRESENCE_PATH, () => {
        getCount += 1;
        return getResponse.promise;
      })
    );
    useRichPresenceStore.setState({
      presenceSettings: confirmedSettings,
      confirmedPresenceSettings: confirmedSettings,
    });
    render(<CustomStatusPopover onClose={onClose} />);
    fireEvent.change(getInput(), { target: { value: 'request' } });
    const actionButton = getSaveBtn();
    fireEvent.click(actionButton);
    await waitFor(() => expect(actionButton).toBeDisabled());
    const categoryMutation = useRichPresenceStore
      .getState()
      .updatePresenceSettings({ serverVoiceTier: 2 });
    await waitFor(() => expect(useRichPresenceStore.getState().presenceSettingsSaving).toBe(true));
    // Complete the newer category mutation before the direct request becomes ambiguous.
    await waitFor(() => expect(patchCount).toBe(2));
    directResponse.resolve(HttpResponse.json({ error: 'uncertain' }, { status: 503 }));
    await screen.findByText('uncertain');
    await categoryMutation;
    await waitFor(() => expect(getCount).toBe(1));
    getResponse.resolve(
      new Response(
        JSON.stringify(presenceResponse({ server_voice_tier: 1, custom_text: 'stale GET' })),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );

    await waitFor(() => expect(actionButton).toBeDisabled());
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
    const state = useRichPresenceStore.getState();
    expect(patchCount).toBe(2);
    expect(getCount).toBe(1);
    expect(state.presenceSettings.serverVoiceTier).toBe(2);
    expect(state.confirmedPresenceSettings).toBeNull();
  });

  it.each([
    ['Save', 'auth adoption'],
    ['Save', 'runtime selection'],
    ['Clear', 'auth adoption'],
    ['Clear', 'runtime selection'],
  ] as const)('%s ignores a stale %s PATCH continuation', async (action, transition) => {
    const response = deferred<Response>();
    mswServer.use(http.patch(PRESENCE_PATH, () => response.promise));
    useRichPresenceStore.getState().setSelfPresence({
      customText: action === 'Clear' ? 'before clear' : 'before save',
      customTextEmoji: '🚀',
    });

    render(<CustomStatusPopover onClose={onClose} />);
    const actionButton = action === 'Save' ? getSaveBtn() : getClearBtn();
    if (action === 'Save') {
      fireEvent.change(getInput(), { target: { value: 'predecessor text' } });
      fireEvent.click(actionButton);
    } else {
      fireEvent.click(actionButton);
    }
    await waitFor(() => expect(actionButton).toBeDisabled());

    if (transition === 'auth adoption') {
      const generation = useAuthStore.getState().authGeneration;
      useAuthStore
        .getState()
        .beginAuthLifecycleIfCurrent(generation, 'successor-token', 'successor-session');
    } else {
      setRuntimeServerBase('https://successor.example');
    }
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'successor text',
      customTextEmoji: '🌱',
    });
    const snapshots = {
      presenceSettings: { ...useRichPresenceStore.getState().presenceSettings },
      confirmedPresenceSettings: useRichPresenceStore.getState().confirmedPresenceSettings,
    };
    response.resolve(
      new Response(
        JSON.stringify({
          master_enabled: true,
          server_voice_tier: 1,
          server_voice_show_details: true,
          private_call_tier: 0,
          private_call_show_details: false,
          custom_text_tier: 0,
          custom_text: action === 'Clear' ? null : 'predecessor text',
          custom_text_emoji: action === 'Clear' ? null : '🚀',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await waitFor(() => expect(actionButton).toBeEnabled());

    expect(useRichPresenceStore.getState().self).toMatchObject({
      customText: 'successor text',
      customTextEmoji: '🌱',
    });
    expect(useRichPresenceStore.getState().presenceSettings).toEqual(snapshots.presenceSettings);
    expect(useRichPresenceStore.getState().confirmedPresenceSettings).toEqual(
      snapshots.confirmedPresenceSettings
    );
    expect(onClose).not.toHaveBeenCalled();
  });
});
