import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';

// Import after mocking
import ServerInput, {
  FAIL_COPY,
  HOST_COPY,
  REFUSAL_COPY,
  STATIC_COPY,
} from '@/renderer/components/Auth/ServerInput';

// §8.4 forbidden mechanism vocabulary — one shared constant so both assertions below
// cover the same list. Word-bounded, so "reserves"/"reserved" is not a false hit on
// "resolve" and "administrator" is not one on "agent".
const FORBIDDEN =
  /\b(DNS|lookup|resolve|resolution|denylist|blocklist|tier|CIDR|RFC1918|private network|loopback|origin|IPC|SSRF|socket|agent|redirect hop)\b/i;

const SAMPLE_HOST = 'example.test';

// Every string the component can render, host-substituted. Driving the lock over this
// set rather than over one rendered outcome is the point: the previous version rendered
// exactly one state (origin_not_approved) and scanned it once, leaving the six refusal
// strings, every failure string, the three probing strings and the three invalid strings
// unguarded.
const ALL_COPY: string[] = [
  ...new Set([
    ...Object.values(STATIC_COPY),
    ...Object.values(REFUSAL_COPY).filter((s): s is string => typeof s === 'string'),
    ...Object.values(FAIL_COPY).flatMap((f) => (f ? [f(SAMPLE_HOST)] : [])),
    ...Object.values(HOST_COPY).map((f) => f(SAMPLE_HOST)),
  ]),
];

describe('ServerInput', () => {
  const mockOnConnect = vi.fn();
  const mockOnBack = vi.fn();
  const mockProbeServer = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    globalThis.electron = {
      ...(globalThis.electron ?? {}),
      selfHosted: {
        probeServer: mockProbeServer,
      },
    } as typeof globalThis.electron;
    mockProbeServer.mockResolvedValue({
      status: 'ok',
      apiBase: 'https://concord.example.com',
      clientConfig: {},
      capabilities: {},
    });
  });

  afterEach(() => {
    globalThis.electron = undefined as unknown as typeof globalThis.electron;
  });

  it('renders the server input form', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    expect(screen.getByText('Connect to Self-Hosted Server')).toBeInTheDocument();
    expect(screen.getByLabelText('Server URL')).toBeInTheDocument();
    expect(screen.getByText('Connect to Server')).toBeInTheDocument();
  });

  it('enables connect button when URL is entered', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    expect(screen.getByText('Connect to Server')).toBeDisabled();
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'example.com' },
    });
    expect(screen.getByText('Connect to Server')).not.toBeDisabled();
  });

  it('adds https:// prefix when protocol is missing', async () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    await waitFor(() => {
      expect(mockProbeServer).toHaveBeenCalledWith('https://concord.example.com');
      expect(mockOnConnect).toHaveBeenCalledWith('https://concord.example.com');
    });
  });

  it('accepts valid https URL', async () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    await waitFor(() => expect(mockOnConnect).toHaveBeenCalledWith('https://concord.example.com'));
  });

  it('allows http:// for localhost', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'ok',
      apiBase: 'http://localhost:8080',
      clientConfig: {},
      capabilities: {},
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'http://localhost:8080' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    await waitFor(() => expect(mockOnConnect).toHaveBeenCalledWith('http://localhost:8080'));
  });

  // Main's isHttpAllowedHost accepts the whole loopback class, ::1 included, and its own
  // test asserts it. The renderer's `startsWith('127.')` gate refused this before the
  // fix — a client-side block on a host the app supports.
  it('allows http:// for the IPv6 loopback literal', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'ok',
      apiBase: 'http://[::1]:8080',
      clientConfig: {},
      capabilities: {},
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'http://[::1]:8080' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    await waitFor(() => expect(mockProbeServer).toHaveBeenCalledWith('http://[::1]:8080'));
  });

  it('shows a host-scoped schema failure and does not connect when discovery fails', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'capabilities_failed',
      message: 'Could not load capabilities.',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText("concord.example.com didn't respond like a Concord server.")
    ).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  // An unmapped code falls through to `result.message`, which main sends empty for
  // this outcome — a silent no-op. FAIL_COPY must carry it.
  it('renders recoverable copy when main refuses a concurrent probe', async () => {
    mockProbeServer.mockResolvedValueOnce({ status: 'error', code: 'busy', message: '' });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    const text = await screen.findByText(/already checking a server/i);
    expect(text).toBeInTheDocument();
    expect(text.textContent ?? '').not.toMatch(FORBIDDEN);
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('renders recoverable copy when main refuses past the approval-prompt budget', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'too_many_prompts',
      message: '',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(await screen.findByText(/too many connection attempts/i)).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('rejects http:// for non-localhost', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'http://insecure.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    expect(
      screen.getByText('HTTPS is required for security (except localhost)')
    ).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  // B2: the pre-fix gate was `fullUrl.includes('localhost')`, so this host passed the
  // renderer and was only stopped by main's exact match. Compare parsed.hostname.
  it('rejects an http:// host that merely contains "localhost" (B2)', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'http://localhost.attacker.example' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    expect(
      screen.getByText('HTTPS is required for security (except localhost)')
    ).toBeInTheDocument();
    expect(mockProbeServer).not.toHaveBeenCalled();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  // Same defect class as B2, one line down: `startsWith('127.')` admitted this host to
  // the IPC. Match the full dotted quad instead.
  it('rejects an http:// host that merely starts with "127." (B2, prefix form)', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'http://127.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    expect(
      screen.getByText('HTTPS is required for security (except localhost)')
    ).toBeInTheDocument();
    expect(mockProbeServer).not.toHaveBeenCalled();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('rejects invalid URL', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'not a valid url @@@' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    expect(screen.getByText('Invalid server URL')).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('calls onBack when back button clicked', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.click(screen.getByText('← Back to Connection Options'));
    expect(mockOnBack).toHaveBeenCalled();
  });

  // B3: onKeyPress is deprecated in React 19, so the handler moved to onKeyDown.
  it('submits on Enter key', async () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    const input = screen.getByLabelText('Server URL');
    fireEvent.change(input, { target: { value: 'https://concord.example.com' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(mockOnConnect).toHaveBeenCalled());
  });

  it('disables connect button when URL is empty', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    expect(screen.getByText('Connect to Server')).toBeDisabled();
  });

  // B1: the panel claimed "All connections are encrypted with HTTPS", which is false on
  // the localhost-over-HTTP path this very component permits — and it was a green
  // reassurance sitting directly above the trust-ceremony trigger.
  it('does not render the false HTTPS reassurance panel (B1)', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    expect(screen.queryByText('Secure Connection')).not.toBeInTheDocument();
    expect(
      screen.queryByText('All connections are encrypted with HTTPS to protect your privacy')
    ).not.toBeInTheDocument();
  });

  it('shows the renderer-owned refusal copy for a tier-1 address, never the raw token', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'address_not_allowed',
      message: 'metadata_link_local',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://a.example' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(await screen.findByText(/can never connect to that address/i)).toBeInTheDocument();
    expect(screen.queryByText(/metadata_link_local/)).not.toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('falls back to the generic refusal copy for an unrecognised reason token', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'address_not_allowed',
      message: 'some_future_class',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://a.example' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText(/reserved address that can't host a server/i)
    ).toBeInTheDocument();
    expect(screen.queryByText(/some_future_class/)).not.toBeInTheDocument();
  });

  // A plain object literal inherits from Object.prototype, so this token resolved to an
  // inherited FUNCTION — not nullish, so `?? GENERIC_REFUSAL` never fired, React dropped
  // the function, and the user got a ⛔ glyph with no text at all.
  it('falls back to the generic refusal for a prototype-named reason token', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'address_not_allowed',
      message: 'constructor',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://a.example' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText(/reserved address that can't host a server/i)
    ).toBeInTheDocument();
  });

  // Same prototype hazard on the failure table, but worse: `fail(host)` invoked
  // Object.prototype.toString and rendered the literal "[object Undefined]".
  it('falls back to the generic failure for a prototype-named error code', async () => {
    mockProbeServer.mockResolvedValueOnce({ status: 'error', code: 'toString', message: '' });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText(
        "Couldn't connect to concord.example.com. Check the address and try again."
      )
    ).toBeInTheDocument();
    expect(screen.queryByText(/\[object /)).not.toBeInTheDocument();
  });

  // Main authors this string in selfHostedProbe.ts, which no renderer copy rule governs.
  // An https URL carrying embedded userinfo still parses as https in the renderer, so the
  // probe fires and main's raw refusal reached the user — live, not hypothetical.
  it('never renders main-authored copy for a code FAIL_COPY does not cover', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'credentials_not_allowed',
      message: 'Server URLs must not include usernames or passwords.',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      // Synthetic userinfo in a test fixture, not a credential.
      target: { value: 'https://someone:placeholder@concord.test' }, // pragma: allowlist secret
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText("Couldn't connect to concord.test. Check the address and try again.")
    ).toBeInTheDocument();
    expect(screen.queryByText(/usernames or passwords/i)).not.toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('returns focus to the input and shows a neutral note on decline', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'approval_declined',
      message: 'Connection cancelled.',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    const input = screen.getByLabelText('Server URL');
    fireEvent.change(input, { target: { value: 'https://concord.lan' } });
    input.blur();
    expect(input).not.toHaveFocus();

    fireEvent.click(screen.getByText('Connect to Server'));

    await waitFor(() => expect(screen.getByText(/was not added/i)).toBeInTheDocument());
    expect(input).toHaveFocus();
    expect(input).not.toHaveClass('error');
  });

  // Task 5b: tier 2 is the approvable self-hosting case, so this is recoverable and
  // neutral — never the terminal "can never connect" copy, and no error border.
  it('treats origin_not_approved as a recoverable, neutral state', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'origin_not_approved',
      message: '',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    const input = screen.getByLabelText('Server URL');
    fireEvent.change(input, { target: { value: 'https://concord.lan' } });
    input.blur();

    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText(
        "concord.lan hasn't been approved on this device yet. " +
          'Try connecting again to approve it.'
      )
    ).toBeInTheDocument();
    expect(screen.queryByText(/can never connect/i)).not.toBeInTheDocument();
    expect(input).not.toHaveClass('error');
    expect(input).toHaveFocus();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  // ── Coverage for the paths SonarCloud flagged on PR #2668 ────────────────────
  // The ten-state machine has more branches than the original suite exercised.
  // Each case below covers a distinct uncovered path, not a variation of one.

  it('rejects a non-http(s) scheme before any IPC call', () => {
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'ftp://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(screen.getByText('Invalid server URL')).toBeInTheDocument();
    // The guard is client-side shape validation — main is never consulted.
    expect(mockProbeServer).not.toHaveBeenCalled();
  });

  it('reports an unreachable host with the typed host echoed back', async () => {
    mockProbeServer.mockResolvedValueOnce({ status: 'error', code: 'unreachable', message: '' });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    // The host is echoed so the user can cross-check their own typo.
    expect(
      await screen.findByText(
        "Couldn't reach concord.example.com. Check the address and try again."
      )
    ).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('reports a client-config schema failure', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'client_config_failed',
      message: '',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText("concord.example.com didn't respond like a Concord server.")
    ).toBeInTheDocument();
  });

  it('reports a failed approval write without minting anything', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'approval_not_saved',
      message: '',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText("Concord couldn't save your choice. Try again.")
    ).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('degrades gracefully when the preload bridge is missing', async () => {
    mockProbeServer.mockResolvedValueOnce(undefined);
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText('Self-hosted server discovery is unavailable in this app version.')
    ).toBeInTheDocument();
    expect(mockOnConnect).not.toHaveBeenCalled();
  });

  it('surfaces a thrown IPC rejection as a recoverable failure, not an unhandled error', async () => {
    mockProbeServer.mockRejectedValueOnce(new Error('ipc exploded'));
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));

    expect(
      await screen.findByText(
        "Couldn't reach concord.example.com. Check the address and try again."
      )
    ).toBeInTheDocument();
    // The button must return to its resting state — a thrown IPC must not strand the form.
    await waitFor(() => expect(screen.getByText('Connect to Server')).toBeEnabled());
  });

  it('escalates through the probing states and never reverts once approval-pending is reached', async () => {
    vi.useFakeTimers();
    try {
      // A probe that never settles, so only the timers drive the state machine.
      mockProbeServer.mockReturnValueOnce(new Promise(() => {}));
      render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
      fireEvent.change(screen.getByLabelText('Server URL'), {
        target: { value: 'https://concord.example.com' },
      });
      fireEvent.click(screen.getByText('Connect to Server'));

      // <250ms: deliberately no loading state, so a fast localhost probe does not flash.
      expect(screen.queryByText('Checking server…')).not.toBeInTheDocument();

      // findBy* polls on real timers and would hang here, so advance explicitly and
      // assert synchronously. act() flushes the React state update the timer queued.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(300);
      });
      expect(screen.getByText('Checking server…')).toBeInTheDocument();

      // 3s: the hedged copy. The renderer cannot know a dialog is open (PR-1 adds no
      // event), so it says "may include" — and drops the spinner, because the system
      // is not working, it is waiting on a human.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });
      expect(screen.getByText(/may include a one-time confirmation/i)).toBeInTheDocument();
      // The slow copy CONTAINS the approval-pending copy, so the assertion above matches
      // in both states. Pin the state by asserting the slow fragment is still absent —
      // otherwise swapping the two constants would leave this test green.
      expect(screen.queryByText(/still checking/i)).not.toBeInTheDocument();

      // 4s: the slow note appends. approval-pending must not revert to probing-visible.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1500);
      });
      expect(screen.getByText(/still checking/i)).toBeInTheDocument();
      expect(screen.queryByText('Checking server…')).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  // The only thing stopping the three escalation timers firing against an unmounted tree
  // is `useEffect(() => clearTimers, [])`, and nothing exercised it.
  it('clears its escalation timers when unmounted mid-probe', async () => {
    vi.useFakeTimers();
    try {
      mockProbeServer.mockReturnValueOnce(new Promise(() => {}));
      const { unmount } = render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
      // render() itself leaves a timer behind (React/testing-library scheduler), so the
      // count is measured against that baseline rather than an absolute zero.
      const baseline = vi.getTimerCount();
      fireEvent.change(screen.getByLabelText('Server URL'), {
        target: { value: 'https://concord.example.com' },
      });
      fireEvent.click(screen.getByText('Connect to Server'));
      expect(vi.getTimerCount()).toBe(baseline + 3);

      unmount();
      expect(vi.getTimerCount()).toBeLessThanOrEqual(baseline);

      // And nothing throws when the clock runs past every escalation deadline.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000);
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps the connect button focusable and guarded while a probe is in flight', async () => {
    mockProbeServer.mockReturnValueOnce(new Promise(() => {}));
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.example.com' },
    });
    const connect = screen.getByText('Connect to Server');
    connect.focus();
    fireEvent.click(connect);

    const busyButton = await screen.findByText('Connecting…');
    // A native `disabled` here would blur the button the keyboard user just activated
    // and drop focus to <body> for the whole probe — never restored on refused/failed.
    expect(busyButton).not.toBeDisabled();
    expect(busyButton).toHaveAttribute('aria-disabled', 'true');
    expect(busyButton).toHaveAttribute('aria-busy', 'true');
    expect(busyButton).toHaveFocus();

    // aria-disabled is advisory, so the activation guard is what actually holds.
    fireEvent.click(busyButton);
    fireEvent.click(screen.getByText('← Back to Connection Options'));
    expect(mockProbeServer).toHaveBeenCalledTimes(1);
    expect(mockOnBack).not.toHaveBeenCalled();
  });

  it('binds the status region to the input and flags the error state programmatically', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'capabilities_failed',
      message: '',
    });
    render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    const input = screen.getByLabelText('Server URL');
    // The red border is colour-only; aria-invalid is the non-visual half.
    expect(input).toHaveAttribute('aria-describedby', 'server-url-status');
    expect(input).not.toHaveAttribute('aria-invalid');

    fireEvent.change(input, { target: { value: 'https://concord.example.com' } });
    fireEvent.click(screen.getByText('Connect to Server'));

    await screen.findByText("concord.example.com didn't respond like a Concord server.");
    expect(input).toHaveAttribute('aria-invalid', 'true');
    const region = document.getElementById('server-url-status');
    expect(region).toHaveTextContent("didn't respond like a Concord server");
  });

  it('never renders a mechanism term in any message it owns', async () => {
    mockProbeServer.mockResolvedValueOnce({
      status: 'error',
      code: 'origin_not_approved',
      message: '',
    });
    const { container } = render(<ServerInput onConnect={mockOnConnect} onBack={mockOnBack} />);
    fireEvent.change(screen.getByLabelText('Server URL'), {
      target: { value: 'https://concord.lan' },
    });
    fireEvent.click(screen.getByText('Connect to Server'));
    await waitFor(() => expect(screen.getByText(/hasn't been approved/i)).toBeInTheDocument());

    expect(container.textContent ?? '').not.toMatch(FORBIDDEN);
  });

  // The render-level check above covers one outcome out of ten. This drives the same
  // vocabulary rule over every string the component owns, so the next added string is
  // guarded without adding a render for it.
  describe('§8.4 vocabulary lock over the whole copy surface', () => {
    it('has a non-trivial copy surface to assert over', () => {
      expect(ALL_COPY.length).toBeGreaterThanOrEqual(15);
    });

    it.each(ALL_COPY)('carries no mechanism vocabulary: %s', (copy) => {
      expect(copy).not.toMatch(FORBIDDEN);
    });
  });
});
