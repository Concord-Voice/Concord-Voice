import { act, render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi, type Mock } from 'vitest';
import { useFriendStore } from '@/renderer/stores/chat/friendStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';

// The Recovery Circle save (#10; plan 2026-10-07 §3 "Preparation"). The confirm
// step takes its capture and prepares the shares when it opens, the primary
// stays aria-disabled until they are ready, and the click only transmits. The
// requirements read (`GET /mfa/step-up`) decides which fields exist, so every
// case serves it by PATH through `serve`, never by call order.
//
// "Mutant:" comments name the production change each case exists to turn red.

// Mock Shamir secret sharing
vi.mock('@/renderer/utils/crypto/shamir', () => ({
  split: vi.fn().mockImplementation((_secret: Uint8Array, n: number) => {
    return Array.from({ length: n }, (_, i) => ({
      index: i + 1,
      data: new Uint8Array([1, 2, 3]),
    }));
  }),
}));

// Mock crypto utilities
vi.mock('@/renderer/utils/crypto/crypto', () => ({
  base64ToArrayBuffer: vi.fn().mockReturnValue(new ArrayBuffer(32)),
  arrayBufferToBase64: vi.fn().mockReturnValue('bW9jay1iYXNlNjQ='),
}));

// Mock e2eeService
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    getWrappingKey: vi.fn().mockReturnValue('mock-wrapping-key'),
    getWrappedPrivateKey: vi.fn().mockReturnValue('bW9jay1kYXRh'),
  },
}));

// Mock apiFetch
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

// Mock Web Crypto
Object.defineProperty(globalThis, 'crypto', {
  value: {
    subtle: {
      unwrapKey: vi.fn().mockResolvedValue('mock-private-key'),
      exportKey: vi.fn().mockResolvedValue(new ArrayBuffer(64)),
      importKey: vi.fn().mockResolvedValue('mock-imported-key'),
      generateKey: vi.fn().mockResolvedValue('mock-aes-key'),
      encrypt: vi.fn().mockResolvedValue(new ArrayBuffer(32)),
    },
    getRandomValues: (arr: Uint8Array) => arr,
  },
  writable: true,
  configurable: true,
});

import RecoveryCircle from '@/renderer/components/Settings/RecoveryCircle';
import { split } from '@/renderer/utils/crypto/shamir';
import { e2eeService } from '@/renderer/services/e2ee/e2eeService';

// ── API double ───────────────────────────────────────────────────────────

const READ_PATH = '/api/v1/mfa/step-up';
const PUT_PATH = '/api/v1/mfa/recovery-circle';
const FIXTURE_PW = 'mypassword';
const FIXTURE_OTP = '123456';

interface Call {
  path: string;
  method: string;
  body: Record<string, unknown> | null;
  /** The `ApiRequestContext` the request was admitted against, if any. */
  context: unknown;
  signal: AbortSignal | null | undefined;
}
const calls: Call[] = [];

function json(body: unknown, status = 200): Response {
  return { ok: status >= 200 && status < 300, status, json: async () => body } as Response;
}

const readOffers = (methods: string[], backup = false) =>
  json({ methods, default_method: methods[0] ?? null, backup_code_available: backup });

interface Scenario {
  read?: () => Response | Promise<Response>;
  publicKey?: (userId: string) => Response | Promise<Response>;
  put?: () => Response | Promise<Response>;
}

/** Routes by path. Nothing here depends on the order requests arrive in. */
function serve(scenario: Scenario = {}) {
  mockApiFetch.mockImplementation(
    async (path: string, init?: RequestInit, opts?: { context?: unknown }) => {
      calls.push({
        path,
        method: init?.method ?? 'GET',
        body: typeof init?.body === 'string' ? JSON.parse(init.body) : null,
        context: opts?.context,
        signal: init?.signal,
      });
      if (path === READ_PATH) return (scenario.read ?? (() => readOffers([])))();
      const publicKeyOf = /^\/api\/v1\/users\/([^/]+)\/public-key$/.exec(path);
      if (publicKeyOf) {
        return (scenario.publicKey ?? (() => json({ public_key: 'bW9jay1wdWJsaWMta2V5' })))(
          publicKeyOf[1]
        );
      }
      if (path === PUT_PATH) return (scenario.put ?? (() => json({})))();
      throw new Error(`unexpected request ${path}`);
    }
  );
}

const putCalls = () => calls.filter((c) => c.path === PUT_PATH);
const publicKeyCalls = () => calls.filter((c) => c.path.endsWith('/public-key'));

const mockFriends = [
  {
    id: 'f1',
    userId: 'user-2',
    username: 'alice',
    displayName: 'Alice',
    status: 'online' as const,
  },
  { id: 'f2', userId: 'user-3', username: 'bob', displayName: 'Bob', status: 'offline' as const },
  {
    id: 'f3',
    userId: 'user-4',
    username: 'charlie',
    displayName: undefined,
    status: 'online' as const,
  },
  {
    id: 'f4',
    userId: 'user-5',
    username: 'diana',
    displayName: 'Diana',
    status: 'online' as const,
  },
];

describe('RecoveryCircle', () => {
  const onComplete = vi.fn();
  const onCancel = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    mockApiFetch.mockReset();
    calls.length = 0;
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    serve();
    // Set up friend store with mock friends
    useFriendStore.setState({
      friends: mockFriends,
      fetchFriends: vi.fn(),
    });
  });

  // ── Initial Rendering (Select Step) ────────────────────────────────────

  it('renders setup wizard title', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Set Up Recovery Circle')).toBeInTheDocument();
  });

  it('renders explanatory text about Shamir Secret Sharing', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText(/Shamir/)).toBeInTheDocument();
  });

  it('renders friend list with checkboxes', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.getByText('Bob')).toBeInTheDocument();
    expect(screen.getByText('@charlie')).toBeInTheDocument();
  });

  it('shows display name when available, username otherwise', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    // charlie has no displayName
    expect(screen.getByText('charlie')).toBeInTheDocument();
  });

  it('renders threshold slider', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByLabelText(/Recovery threshold/)).toBeInTheDocument();
  });

  it('renders Continue and Cancel buttons', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Continue')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();
  });

  it('disables Continue until at least 2 contacts are selected', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Continue')).toBeDisabled();
  });

  it('calls onCancel when Cancel is clicked', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(onCancel).toHaveBeenCalled();
  });

  it('calls fetchFriends on mount', () => {
    const fetchFriends = vi.fn();
    useFriendStore.setState({ fetchFriends });
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(fetchFriends).toHaveBeenCalled();
  });

  // ── Contact Selection ──────────────────────────────────────────────────

  it('enables Continue when 3 contacts are selected (meeting default threshold)', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]); // Alice
    fireEvent.click(checkboxes[1]); // Bob
    fireEvent.click(checkboxes[2]); // Charlie

    expect(screen.getByText('Continue')).not.toBeDisabled();
  });

  it('toggles contact selection off when clicked again', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]); // Select Alice
    fireEvent.click(checkboxes[1]); // Select Bob
    fireEvent.click(checkboxes[2]); // Select Charlie (threshold=3, need 3 selected)
    expect(screen.getByText('Continue')).not.toBeDisabled();

    fireEvent.click(checkboxes[2]); // Deselect Charlie — now only 2 < threshold(3)
    expect(screen.getByText('Continue')).toBeDisabled();
  });

  it('limits selection to 7 contacts', () => {
    // Add more friends to test the limit
    const manyFriends = Array.from({ length: 10 }, (_, i) => ({
      id: `f${i}`,
      userId: `user-${i + 10}`,
      username: `friend${i}`,
      displayName: `Friend ${i}`,
      status: 'online' as const,
    }));
    useFriendStore.setState({ friends: manyFriends });

    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    // Select 7 contacts
    for (let i = 0; i < 7; i++) {
      fireEvent.click(checkboxes[i]);
    }

    // 8th selection should not add
    fireEvent.click(checkboxes[7]);

    // Count checked checkboxes
    const checked = checkboxes.filter((cb) => (cb as HTMLInputElement).checked);
    expect(checked.length).toBe(7);
  });

  it('shows "No friends found" when friend list is empty', () => {
    useFriendStore.setState({ friends: [] });
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText(/No friends found/)).toBeInTheDocument();
  });

  // ── Threshold Slider ───────────────────────────────────────────────────

  it('adjusts threshold when slider is changed', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    // Select 3 contacts first
    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    fireEvent.click(checkboxes[2]);

    const slider = screen.getByRole('slider');
    fireEvent.change(slider, { target: { value: '2' } });

    expect(screen.getByText(/Recovery threshold: 2 of 3/)).toBeInTheDocument();
  });

  it('disables threshold slider when fewer than 2 contacts selected', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    const slider = screen.getByRole('slider');
    expect(slider).toBeDisabled();
  });

  // ── Confirm Step ───────────────────────────────────────────────────────

  /** Selects `count` contacts, lowers the threshold to 2 and opens the confirm step. */
  const openConfirm = (count = 2) => {
    const checkboxes = screen.getAllByRole('checkbox');
    for (let i = 0; i < count; i++) fireEvent.click(checkboxes[i]);
    if (count === 2) fireEvent.change(screen.getByRole('slider'), { target: { value: '2' } });
    fireEvent.click(screen.getByText('Continue'));
  };

  const primary = () => screen.getByRole('button', { name: 'Create Recovery Circle' });
  const typePassword = (value = FIXTURE_PW) =>
    fireEvent.change(screen.getByLabelText('Password'), { target: { value } });
  /** Resolves once the primary can act: the read landed and the shares are prepared. */
  const untilActionable = () =>
    waitFor(() => expect(primary()).not.toHaveAttribute('aria-disabled'));

  it('transitions to confirm step when Continue is clicked', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    openConfirm(3);

    expect(screen.getByText('Confirm Recovery Circle')).toBeInTheDocument();
    expect(screen.getByText(/3 of 3/)).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toBeInTheDocument();
  });

  it('shows Create Recovery Circle button on confirm step', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    openConfirm(3);
    expect(primary()).toBeInTheDocument();
  });

  it('keeps Create Recovery Circle aria-disabled, not natively disabled, with no password', async () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    openConfirm(3);
    await waitFor(() => expect(publicKeyCalls()).toHaveLength(3));

    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    expect(primary()).not.toBeDisabled();
    fireEvent.click(primary());
    expect(putCalls()).toHaveLength(0);
  });

  it('goes back to select step when Back is clicked', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    openConfirm(3);

    expect(screen.getByText('Confirm Recovery Circle')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Back'));
    expect(screen.getByText('Set Up Recovery Circle')).toBeInTheDocument();
  });

  // ── The factor the account has (plan §1.3) ─────────────────────────────

  describe('which credential fields the step shows', () => {
    // Mutant: an unconditional code field. With `methods: []` the account has
    // no inline factor, so a required code box can never be satisfied.
    it('shows no code field when the read returns no methods, and saves on the password alone', async () => {
      serve({ read: () => readOffers([]) });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      await untilActionable();

      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Backup code')).not.toBeInTheDocument();
      fireEvent.click(primary());

      await screen.findByText('Recovery Circle Configured');
      expect(putCalls()).toHaveLength(1);
      expect(putCalls()[0].body).not.toHaveProperty('mfa_code');
      expect(putCalls()[0].body).toMatchObject({ password: FIXTURE_PW });
    });

    it('shows the code field only once the read is answered', async () => {
      const read = deferred<Response>();
      serve({ read: () => read.promise });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();

      expect(screen.getByLabelText('Password')).toBeInTheDocument();
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();

      await act(async () => read.resolve(readOffers(['totp'])));
      expect(await screen.findByLabelText('Authenticator app code')).toBeInTheDocument();
    });

    it('sends the authenticator code as mfa_code and holds the primary until it is complete', async () => {
      serve({ read: () => readOffers(['totp']) });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      const code = await screen.findByLabelText('Authenticator app code');
      await waitFor(() => expect(publicKeyCalls()).toHaveLength(2));
      // Prepared and password typed, but the code is missing.
      expect(primary()).toHaveAttribute('aria-disabled', 'true');
      fireEvent.click(primary());
      expect(putCalls()).toHaveLength(0);

      fireEvent.change(code, { target: { value: FIXTURE_OTP } });
      await untilActionable();
      fireEvent.click(primary());

      await screen.findByText('Recovery Circle Configured');
      expect(putCalls()[0].body).toMatchObject({ password: FIXTURE_PW, mfa_code: FIXTURE_OTP });
    });

    it('offers a backup code beside TOTP when the read reports one', async () => {
      serve({ read: () => readOffers(['totp'], true) });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();

      expect(
        await screen.findByRole('button', { name: 'Use a backup code instead' })
      ).toBeInTheDocument();
    });

    it('sends a typed backup code as mfa_code', async () => {
      serve({ read: () => readOffers(['totp'], true) });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      fireEvent.click(await screen.findByRole('button', { name: 'Use a backup code instead' }));
      fireEvent.change(screen.getByLabelText('Backup code'), { target: { value: 'EXCI3G5F' } });
      await untilActionable();
      fireEvent.click(primary());

      await screen.findByText('Recovery Circle Configured');
      expect(putCalls()[0].body).toMatchObject({ mfa_code: 'EXCI3G5F' });
    });

    it('falls back to the password alone when the read fails', async () => {
      serve({ read: () => json({}, 503) });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      await untilActionable();

      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    });
  });

  // ── Preparation before the activation (C9, C82, D11) ───────────────────

  describe('preparation', () => {
    it('keeps the primary down and sends no PUT until the shares are prepared', async () => {
      const key = deferred<Response>();
      serve({ publicKey: () => key.promise });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      await waitFor(() => expect(publicKeyCalls()).toHaveLength(2));
      // The read has landed and the password is in: only preparation is missing.
      await screen.findByLabelText('Password');

      expect(primary()).toHaveAttribute('aria-disabled', 'true');
      fireEvent.click(primary());
      expect(putCalls()).toHaveLength(0);
      expect(screen.getByRole('status')).toHaveTextContent('Getting things ready…');

      await act(async () => key.resolve(json({ public_key: 'bW9jay1wdWJsaWMta2V5' })));
      await untilActionable();
      expect(putCalls()).toHaveLength(0);
      fireEvent.click(primary());
      await screen.findByText('Recovery Circle Configured');
    });

    // Mutant: `submit` awaits preparation itself. Nothing would be fetched when
    // the step opens, so the fetches below would not be out before any
    // credential is typed and any activation is tried; and the PUT would trail
    // them by a round trip instead of following settled work.
    it('prepares at step entry, so the PUT follows every public-key fetch', async () => {
      const key = deferred<Response>();
      serve({ publicKey: () => key.promise });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();

      // Both fetches are out with no password typed and nothing activated.
      await waitFor(() => expect(publicKeyCalls()).toHaveLength(2));
      expect(putCalls()).toHaveLength(0);

      // An activation attempted while they are held sends nothing and starts
      // no preparation of its own.
      typePassword();
      await screen.findByLabelText('Password');
      fireEvent.click(primary());
      expect(putCalls()).toHaveLength(0);
      expect(publicKeyCalls()).toHaveLength(2);

      await act(async () => key.resolve(json({ public_key: 'bW9jay1wdWJsaWMta2V5' })));
      await untilActionable();
      expect(putCalls()).toHaveLength(0);

      fireEvent.click(primary());
      await screen.findByText('Recovery Circle Configured');

      // The activation fetched nothing more, and the PUT came after every fetch.
      expect(publicKeyCalls()).toHaveLength(2);
      const order = calls.filter((c) => c.path !== READ_PATH).map((c) => c.path);
      expect(order.at(-1)).toBe(PUT_PATH);
      expect(putCalls()).toHaveLength(1);
    });

    // C82. Mutant: `run` takes its own capture instead of the step's. The PUT
    // would then be admitted against a different object than the fetches whose
    // output it carries.
    it('sends the public-key fetches and the PUT under one capture', async () => {
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      await untilActionable();
      fireEvent.click(primary());
      await screen.findByText('Recovery Circle Configured');

      const [put] = putCalls();
      expect(put.context).toBeDefined();
      expect(publicKeyCalls()).toHaveLength(2);
      for (const fetched of publicKeyCalls()) expect(fetched.context).toBe(put.context);
    });

    // Mutant: a request sent outside the capture. Nothing may go out as the
    // account that signed in after the step opened.
    it('sends nothing when the account changed after the step opened', async () => {
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      await untilActionable();

      act(() => useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 })));
      fireEvent.click(primary());

      await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Sign in again'));
      expect(putCalls()).toHaveLength(0);
      expect(screen.queryByText('Recovery Circle Configured')).not.toBeInTheDocument();
    });

    it('returns to the select step with the reason when the keys cannot be unlocked', async () => {
      (e2eeService.getWrappingKey as ReturnType<typeof vi.fn>).mockReturnValueOnce(null);
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();

      expect(await screen.findByText('E2EE keys not available')).toBeInTheDocument();
      expect(screen.getByText('Set Up Recovery Circle')).toBeInTheDocument();
      expect(putCalls()).toHaveLength(0);
    });

    it('returns to the select step when a contact public key cannot be fetched', async () => {
      serve({ publicKey: () => json({ error: 'User not found' }, 404) });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();

      expect(await screen.findByText('Failed to fetch public key for contact')).toBeInTheDocument();
      expect(screen.getByText('Set Up Recovery Circle')).toBeInTheDocument();
      expect(putCalls()).toHaveLength(0);
    });

    it('prepares again when the step is reopened', async () => {
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      await waitFor(() => expect(publicKeyCalls()).toHaveLength(2));
      fireEvent.click(screen.getByText('Back'));
      fireEvent.click(screen.getByText('Continue'));
      await waitFor(() => expect(publicKeyCalls()).toHaveLength(4));
    });
  });

  // ── Setup Flow ─────────────────────────────────────────────────────────

  it('shows loading state during setup', async () => {
    // The PUT never answers.
    serve({ put: () => new Promise<Response>(() => {}) });
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    openConfirm();
    typePassword();
    await untilActionable();
    fireEvent.click(primary());

    await waitFor(() => {
      expect(screen.getByText('Setting up...')).toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: 'Back' })).toBeDisabled();
  });

  it('shows done step after successful setup', async () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    openConfirm();
    typePassword();
    await untilActionable();
    fireEvent.click(primary());

    await waitFor(() => {
      expect(screen.getByText('Recovery Circle Configured')).toBeInTheDocument();
      expect(screen.getByText('Recovery Circle Active')).toBeInTheDocument();
    });
    expect(putCalls()[0].body).toMatchObject({
      threshold_k: 2,
      total_shares_n: 2,
      shares: [
        expect.objectContaining({ contact_id: 'user-2', share_index: 1 }),
        expect.objectContaining({ contact_id: 'user-3', share_index: 2 }),
      ],
    });
  });

  it('calls onComplete when Done is clicked on success', async () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    openConfirm();
    typePassword();
    await untilActionable();
    fireEvent.click(primary());

    fireEvent.click(await screen.findByText('Done'));
    expect(onComplete).toHaveBeenCalled();
  });

  // ── Error States ───────────────────────────────────────────────────────

  describe('refusals of the save', () => {
    const save = async () => {
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword('wrongpw');
      await untilActionable();
      fireEvent.click(primary());
    };

    it('a wrong password is a field error, empties the password and stays on the step', async () => {
      serve({ put: () => json({ error: 'Invalid password' }, 403) });
      await save();

      expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
      expect(screen.getByLabelText('Password')).toHaveValue('');
      expect(screen.getByText('Confirm Recovery Circle')).toBeInTheDocument();
    });

    it('shows the server text for an answer the seam does not classify', async () => {
      serve({ put: () => json({ error: 'Password incorrect' }, 500) });
      await save();

      expect(await screen.findByText('Password incorrect')).toBeInTheDocument();
      expect(screen.queryByText('Recovery Circle Configured')).not.toBeInTheDocument();
    });

    it('says so when the server cannot be reached', async () => {
      serve({
        put: () => {
          throw new TypeError('Failed to fetch');
        },
      });
      await save();

      expect(
        await screen.findByText("Couldn't reach the server. Check your connection and try again.")
      ).toBeInTheDocument();
    });

    it('shows the rate-limit copy for a spent budget', async () => {
      serve({ put: () => json({ error: 'Too many verification attempts' }, 429) });
      await save();

      expect(
        await screen.findByText('Too many attempts. Try again in a few minutes.')
      ).toBeInTheDocument();
    });

    // Mutant: `enrollmentRequired` routed to a banner or to `failed`.
    it('an account with no inline factor is told to set one up, with no field to fill', async () => {
      serve({
        put: () => json({ error: 'Set up an app', mfa_enrollment_required: true }, 403),
      });
      await save();

      expect(
        await screen.findByText(
          'Set up an authenticator app or security key in Settings to do this.'
        )
      ).toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    });
  });

  it('displays threshold and contact count on done step', async () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    fireEvent.click(checkboxes[2]);
    fireEvent.change(screen.getByRole('slider'), { target: { value: '2' } });
    fireEvent.click(screen.getByText('Continue'));
    typePassword('pw');
    await untilActionable();
    fireEvent.click(primary());

    await waitFor(() => {
      expect(screen.getByText(/2 of 3/)).toBeInTheDocument();
    });
  });

  // ── Key material and an abandoned preparation (EE1, EE2) ───────────────

  describe('the key material preparation holds', () => {
    const subtle = () => crypto.subtle as unknown as Record<'exportKey' | 'encrypt', Mock>;

    afterEach(() => {
      subtle().encrypt.mockReset().mockResolvedValue(new ArrayBuffer(32));
    });

    // Mutant: no `fill(0)` after the split, or after a share is encrypted. The
    // private key's PKCS8 bytes, or a plaintext share, would outlive their use.
    it('zeroes the private key bytes after the split and each share once encrypted', async () => {
      subtle().exportKey.mockResolvedValueOnce(new Uint8Array(64).fill(7).buffer);
      const seen: { secret?: Uint8Array; secretAtSplit?: Uint8Array; shares: Uint8Array[] } = {
        shares: [],
      };
      vi.mocked(split).mockImplementationOnce((secret, n) => {
        seen.secret = secret;
        seen.secretAtSplit = secret.slice();
        const shares = Array.from({ length: n }, (_, i) => ({
          index: i + 1,
          data: new Uint8Array([1, 2, 3]),
        }));
        seen.shares = shares.map((share) => share.data);
        return shares;
      });
      const encryptedPlaintexts: Uint8Array[] = [];
      subtle().encrypt.mockImplementation(
        async (alg: { name: string }, _key, data: ArrayBuffer) => {
          if (alg.name === 'AES-GCM') encryptedPlaintexts.push(new Uint8Array(data.slice(0)));
          return new ArrayBuffer(32);
        }
      );

      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      await untilActionable();

      // Positive controls: the split saw the key and each share was encrypted
      // as it was, so the zeroing came after the bytes were used.
      expect(seen.secretAtSplit?.every((b) => b === 7)).toBe(true);
      expect(encryptedPlaintexts).toEqual([new Uint8Array([1, 2, 3]), new Uint8Array([1, 2, 3])]);
      expect(seen.secret?.every((b) => b === 0)).toBe(true);
      expect(seen.shares).toHaveLength(2);
      for (const share of seen.shares) expect(share.every((b) => b === 0)).toBe(true);
    });

    // Mutant: `wrapRawKey` without its `fill(0)`. Each share's raw AES key
    // would outlive the RSA-OAEP wrap that is its only use.
    it('zeroes each raw AES key once it is wrapped', async () => {
      const rawKeys: Uint8Array[] = [];
      subtle().exportKey.mockImplementation(async (format: string) => {
        if (format !== 'raw') return new ArrayBuffer(64);
        const raw = new Uint8Array(32).fill(9);
        rawKeys.push(raw);
        return raw.buffer;
      });
      const wrapped: Uint8Array[] = [];
      subtle().encrypt.mockImplementation(
        async (alg: { name: string }, _key, data: ArrayBuffer) => {
          if (alg.name === 'RSA-OAEP') wrapped.push(new Uint8Array(data.slice(0)));
          return new ArrayBuffer(32);
        }
      );

      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      await untilActionable();

      // Positive control: each wrap saw its key as it was before the zeroing.
      expect(wrapped).toHaveLength(2);
      for (const key of wrapped) expect(key.every((b) => b === 9)).toBe(true);
      expect(rawKeys).toHaveLength(2);
      for (const key of rawKeys) expect(key.every((b) => b === 0)).toBe(true);
      subtle().exportKey.mockReset().mockResolvedValue(new ArrayBuffer(64));
    });

    it('zeroes a share whose contact key could not be fetched', async () => {
      const shares: Uint8Array[] = [];
      vi.mocked(split).mockImplementationOnce((_secret, n) =>
        Array.from({ length: n }, (_, i) => {
          const data = new Uint8Array([1, 2, 3]);
          shares.push(data);
          return { index: i + 1, data };
        })
      );
      serve({ publicKey: () => json({ error: 'User not found' }, 404) });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();

      await screen.findByText('Failed to fetch public key for contact');
      await waitFor(() => expect(shares.every((s) => s.every((b) => b === 0))).toBe(true));
      expect(shares).toHaveLength(2);
    });

    /** Opens the confirm step with the private key's export held until it is released. */
    const openWithExportHeld = () => {
      const exported = deferred<ArrayBuffer>();
      subtle().exportKey.mockImplementationOnce(() => exported.promise);
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      return exported;
    };

    // Positive control: the same held export, released on the open step, fetches.
    it('a preparation released on the open step fetches every contact key', async () => {
      const exported = openWithExportHeld();
      expect(publicKeyCalls()).toHaveLength(0);

      await act(async () => exported.resolve(new ArrayBuffer(64)));
      await waitFor(() => expect(publicKeyCalls()).toHaveLength(2));
    });

    // Mutant: no abort in the effect cleanup, or no check between steps. The
    // step would go on fetching and encrypting for nobody.
    it('leaving mid-preparation fetches nothing more and reports nothing', async () => {
      const exported = openWithExportHeld();
      fireEvent.click(screen.getByText('Back'));

      await act(async () => exported.resolve(new ArrayBuffer(64)));
      await act(async () => {});

      expect(publicKeyCalls()).toHaveLength(0);
      expect(screen.getByText('Set Up Recovery Circle')).toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('leaving while the contact keys are being fetched cancels those fetches', async () => {
      const key = deferred<Response>();
      serve({ publicKey: () => key.promise });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      await waitFor(() => expect(publicKeyCalls()).toHaveLength(2));
      for (const call of publicKeyCalls()) expect(call.signal?.aborted).toBe(false);

      fireEvent.click(screen.getByText('Back'));
      for (const call of publicKeyCalls()) expect(call.signal?.aborted).toBe(true);

      await act(async () => key.resolve(json({ public_key: 'bW9jay1wdWJsaWMta2V5' })));
      expect(subtle().encrypt).not.toHaveBeenCalled();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });
  });

  // ── Refusal rendering and focus (CR3, FE3) ─────────────────────────────

  describe('how the confirm step answers', () => {
    const save = async () => {
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      await untilActionable();
      fireEvent.click(primary());
    };

    // Mutant: the password dropped on `passwordRequired` too. The server says
    // that only of an empty password, so nothing it refused is being dropped.
    it('a password_required answer keeps the typed password', async () => {
      serve({ put: () => json({ error: 'Password required', password_required: true }, 403) });
      await save();

      expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
      expect(screen.getByLabelText('Password')).toHaveValue(FIXTURE_PW);
    });

    // Mutant: the error back in a `--error-color` paragraph rather than the
    // shared banner the other hosts use.
    it('shows a refusal in the shared error banner', async () => {
      serve({ put: () => json({ error: 'Password incorrect' }, 500) });
      await save();

      const text = await screen.findByText('Password incorrect');
      expect(text.closest('[role="alert"]')).toHaveClass('mfa-setup-error-banner');
    });

    it('shows a preparation failure on the select step in the shared error banner', async () => {
      (e2eeService.getWrappingKey as ReturnType<typeof vi.fn>).mockReturnValueOnce(null);
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();

      const text = await screen.findByText('E2EE keys not available');
      expect(text.closest('[role="alert"]')).toHaveClass('mfa-setup-error-banner');
    });

    // Mutant: no focus on entry. Continue unmounts with the select step, so
    // focus would fall to <body>.
    it('Continue moves focus to the password field', () => {
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();

      expect(screen.getByLabelText('Password')).toHaveFocus();
    });

    // Mutant: no `headingRef`. The terminal state removes the password field,
    // and the stage would fall back to the nearest <dialog>, which this render
    // (like any host without one) does not have: focus would drop to <body>.
    it('a terminal answer moves focus to the step heading', async () => {
      serve({ put: () => json({ error: 'Set up an app', mfa_enrollment_required: true }, 403) });
      await save();

      await screen.findByText(
        'Set up an authenticator app or security key in Settings to do this.'
      );
      expect(screen.getByRole('heading', { name: 'Confirm Recovery Circle' })).toHaveFocus();
    });
  });

  // ── Focus when the confirm step closes ─────────────────────────────────
  //
  // Whatever held focus in the confirm step unmounts with it, and an element
  // that unmounts with focus sends it to <body>, inside a dialog or not. The
  // step it lands on gives its heading focus instead.

  describe('focus when the confirm step closes', () => {
    const heading = (name: string) => screen.getByRole('heading', { name });

    // The host's Modal owns focus when the wizard opens.
    it('opening the wizard takes no focus of its own', () => {
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      expect(heading('Set Up Recovery Circle')).not.toHaveFocus();
    });

    // Mutant: no heading ref on the select step.
    it('Back moves focus to the select step heading', () => {
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      // Positive control: focus was inside the step that is about to close.
      expect(screen.getByLabelText('Password')).toHaveFocus();

      fireEvent.click(screen.getByText('Back'));
      expect(heading('Set Up Recovery Circle')).toHaveFocus();
    });

    it('a failed preparation moves focus to the select step heading', async () => {
      serve({ publicKey: () => json({ error: 'User not found' }, 404) });
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();

      await screen.findByText('Failed to fetch public key for contact');
      expect(heading('Set Up Recovery Circle')).toHaveFocus();
    });

    // Mutant: no heading ref on the done step.
    it('a saved circle moves focus to the done step heading', async () => {
      render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
      openConfirm();
      typePassword();
      await untilActionable();
      fireEvent.click(primary());

      await screen.findByText('Recovery Circle Active');
      expect(heading('Recovery Circle Configured')).toHaveFocus();
    });
  });
});
