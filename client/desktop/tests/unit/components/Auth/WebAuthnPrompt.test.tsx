import { render, screen, waitFor, fireEvent } from '../../../test-utils';
import { vi } from 'vitest';
import WebAuthnPrompt from '@/renderer/components/Auth/WebAuthnPrompt';

describe('WebAuthnPrompt', () => {
  const onSuccess = vi.fn();
  const onError = vi.fn();
  const onCancel = vi.fn();

  // Mock navigator.credentials
  const mockGet = vi.fn();
  const mockCreate = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    Object.defineProperty(navigator, 'credentials', {
      value: {
        get: mockGet,
        create: mockCreate,
      },
      writable: true,
      configurable: true,
    });
  });

  // ── Initial State ──────────────────────────────────────────────────────

  it('renders waiting state with security key message', () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );
    expect(screen.getByText('Waiting for security key...')).toBeInTheDocument();
    expect(screen.getByText(/Touch your security key/)).toBeInTheDocument();
  });

  it('renders key icon SVG in waiting state', () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );
    const svg = document.querySelector('svg');
    expect(svg).not.toBeNull();
    expect(svg?.getAttribute('width')).toBe('48');
  });

  // ── Request Options (Login) ────────────────────────────────────────────

  it('calls navigator.credentials.get with requestOptions', () => {
    const opts: PublicKeyCredentialRequestOptions = {
      challenge: new ArrayBuffer(32),
      rpId: 'localhost',
    };
    mockGet.mockReturnValue(new Promise(() => {}));

    render(<WebAuthnPrompt requestOptions={opts} onSuccess={onSuccess} onError={onError} />);

    expect(mockGet).toHaveBeenCalledWith(
      expect.objectContaining({
        publicKey: opts,
      })
    );
  });

  it('calls onSuccess when credential is returned from get()', async () => {
    const mockCredential = { id: 'cred-1', type: 'public-key' };
    mockGet.mockResolvedValue(mockCredential);

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    await waitFor(() => {
      expect(onSuccess).toHaveBeenCalledWith(mockCredential);
    });
  });

  // ── Creation Options (Registration) ────────────────────────────────────

  it('calls navigator.credentials.create with creationOptions', () => {
    const opts: PublicKeyCredentialCreationOptions = {
      challenge: new ArrayBuffer(32),
      rp: { name: 'Concord', id: 'localhost' },
      user: {
        id: new ArrayBuffer(16),
        name: 'test@example.com',
        displayName: 'Test User',
      },
      pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
    };
    mockCreate.mockReturnValue(new Promise(() => {}));

    render(<WebAuthnPrompt creationOptions={opts} onSuccess={onSuccess} onError={onError} />);

    expect(mockCreate).toHaveBeenCalledWith(
      expect.objectContaining({
        publicKey: opts,
      })
    );
  });

  it('calls onSuccess when credential is returned from create()', async () => {
    const mockCredential = { id: 'cred-2', type: 'public-key' };
    mockCreate.mockResolvedValue(mockCredential);

    render(
      <WebAuthnPrompt
        creationOptions={{
          challenge: new ArrayBuffer(32),
          rp: { name: 'Concord', id: 'localhost' },
          user: {
            id: new ArrayBuffer(16),
            name: 'test@example.com',
            displayName: 'Test User',
          },
          pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    await waitFor(() => {
      expect(onSuccess).toHaveBeenCalledWith(mockCredential);
    });
  });

  // ── Error States ───────────────────────────────────────────────────────

  it('shows error and calls onError when credential is null', async () => {
    mockGet.mockResolvedValue(null);

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    await waitFor(() => {
      expect(screen.getByText('No credential returned')).toBeInTheDocument();
      expect(onError).toHaveBeenCalledWith('No credential returned');
    });
  });

  it('shows timeout message on NotAllowedError', async () => {
    const err = new DOMException('User cancelled', 'NotAllowedError');
    mockGet.mockRejectedValue(err);

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    await waitFor(() => {
      expect(screen.getByText('Request cancelled or timed out. Try again.')).toBeInTheDocument();
      expect(onError).toHaveBeenCalledWith('Request cancelled or timed out. Try again.');
    });
  });

  it('silently handles AbortError (cleanup abort)', async () => {
    const err = new DOMException('Aborted', 'AbortError');
    mockGet.mockRejectedValue(err);

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    // Wait a bit for the promise to settle
    await new Promise((r) => setTimeout(r, 100));
    // Should not show error state
    expect(
      screen.queryByText('Request cancelled or timed out. Try again.')
    ).not.toBeInTheDocument();
    // onError should not be called with empty message
    expect(onError).not.toHaveBeenCalled();
  });

  it('shows generic error message for unknown errors', async () => {
    mockGet.mockRejectedValue(new Error('Unexpected WebAuthn failure'));

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    await waitFor(() => {
      expect(screen.getByText('Unexpected WebAuthn failure')).toBeInTheDocument();
      expect(onError).toHaveBeenCalledWith('Unexpected WebAuthn failure');
    });
  });

  it('shows fallback error message for non-Error throws', async () => {
    mockGet.mockRejectedValue('something');

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    await waitFor(() => {
      expect(screen.getByText('WebAuthn verification failed')).toBeInTheDocument();
    });
  });

  // ── Cancel / Try Another Method ────────────────────────────────────────

  it('shows "Try another method" button on error when onCancel is provided', async () => {
    mockGet.mockRejectedValue(new Error('failed'));

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
        onCancel={onCancel}
      />
    );

    await waitFor(() => {
      expect(screen.getByText('Try another method')).toBeInTheDocument();
    });
  });

  it('calls onCancel when "Try another method" is clicked', async () => {
    mockGet.mockRejectedValue(new Error('failed'));

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
        onCancel={onCancel}
      />
    );

    await waitFor(() => {
      expect(screen.getByText('Try another method')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('Try another method'));
    expect(onCancel).toHaveBeenCalled();
  });

  it('does not show "Try another method" when onCancel is not provided', async () => {
    mockGet.mockRejectedValue(new Error('failed'));

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    await waitFor(() => {
      expect(screen.getByText('failed')).toBeInTheDocument();
    });
    expect(screen.queryByText('Try another method')).not.toBeInTheDocument();
  });

  // ── WebAuthn Not Supported ─────────────────────────────────────────────

  it('shows unsupported error when navigator.credentials is absent', async () => {
    Object.defineProperty(navigator, 'credentials', {
      value: undefined,
      writable: true,
      configurable: true,
    });

    render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    await waitFor(() => {
      expect(screen.getByText('WebAuthn is not supported in this browser.')).toBeInTheDocument();
      expect(onError).toHaveBeenCalledWith('WebAuthn not supported');
    });
  });

  // ── Abort on Unmount ───────────────────────────────────────────────────

  it('aborts the ceremony when component unmounts', () => {
    // Track the signal passed to navigator.credentials.get
    let passedSignal: AbortSignal | undefined;
    mockGet.mockImplementation(({ signal }: { signal?: AbortSignal }) => {
      passedSignal = signal;
      return new Promise(() => {}); // Never resolves
    });

    const { unmount } = render(
      <WebAuthnPrompt
        requestOptions={{
          challenge: new ArrayBuffer(32),
          rpId: 'localhost',
        }}
        onSuccess={onSuccess}
        onError={onError}
      />
    );

    expect(passedSignal).toBeDefined();
    expect(passedSignal!.aborted).toBe(false);

    unmount();

    expect(passedSignal!.aborted).toBe(true);
  });
});
