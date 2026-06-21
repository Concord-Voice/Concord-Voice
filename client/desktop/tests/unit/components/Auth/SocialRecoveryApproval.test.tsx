import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';

// Mock crypto utilities
vi.mock('@/renderer/utils/crypto', () => ({
  base64ToArrayBuffer: vi.fn().mockImplementation((input: string) => {
    // For the myEncryptedShare prop, return the JSON payload as bytes
    // For other calls (k, iv, c fields), return generic buffer
    try {
      const decoded = atob(input);
      return new TextEncoder().encode(decoded).buffer;
    } catch {
      return new ArrayBuffer(32);
    }
  }),
  arrayBufferToBase64: vi.fn().mockReturnValue('bW9jay1iYXNlNjQ='),
  importECDHPublicKey: vi.fn().mockResolvedValue('mock-ecdh-pubkey'),
  generateECDHKeyPair: vi.fn().mockResolvedValue({
    publicKey: 'mock-pub',
    privateKey: 'mock-priv',
  }),
  deriveSharedSecret: vi.fn().mockResolvedValue('mock-shared-key'),
  encryptWithSharedSecret: vi.fn().mockResolvedValue('mock-encrypted'),
}));

// Mock e2eeService
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getWrappingKey: vi.fn().mockReturnValue('mock-wrapping-key'),
    getWrappedPrivateKey: vi.fn().mockReturnValue('bW9jay1kYXRh'),
  },
}));

// Mock apiFetch
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

// Mock Web Crypto
const mockUnwrapKey = vi.fn().mockResolvedValue('mock-private-key');
const mockDecrypt = vi.fn().mockResolvedValue(new ArrayBuffer(16));
const mockImportKey = vi.fn().mockResolvedValue('mock-aes-key');
const mockExportKey = vi.fn().mockResolvedValue(new ArrayBuffer(32));

Object.defineProperty(globalThis, 'crypto', {
  value: {
    subtle: {
      unwrapKey: mockUnwrapKey,
      decrypt: mockDecrypt,
      importKey: mockImportKey,
      exportKey: mockExportKey,
    },
    getRandomValues: (arr: Uint8Array) => arr,
  },
  writable: true,
  configurable: true,
});

import SocialRecoveryApproval from '@/renderer/components/Auth/SocialRecoveryApproval';
import { e2eeService } from '@/renderer/services/e2eeService';

describe('SocialRecoveryApproval', () => {
  const defaultProps = {
    requestId: 'req-123',
    requesterUsername: 'alice',
    requesterDisplayName: 'Alice Smith',
    requesterEphemeralKey: 'bW9jay1lcGhlbWVyYWw=',
    myEncryptedShare: btoa(
      JSON.stringify({
        k: 'bW9jay1rZXk=',
        iv: 'bW9jay1pdg==',
        c: 'bW9jay1jaXBoZXJ0ZXh0',
      })
    ),
    shareIndex: 1,
    onClose: vi.fn(),
  };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  // ── Initial Rendering ──────────────────────────────────────────────────

  it('renders social recovery request modal', () => {
    render(<SocialRecoveryApproval {...defaultProps} />);
    expect(screen.getByText('Social Recovery Request')).toBeInTheDocument();
  });

  it('displays requester display name', () => {
    render(<SocialRecoveryApproval {...defaultProps} />);
    expect(screen.getByText(/Alice Smith/)).toBeInTheDocument();
  });

  it('displays requester username', () => {
    render(<SocialRecoveryApproval {...defaultProps} />);
    expect(screen.getByText(/@alice/)).toBeInTheDocument();
  });

  it('falls back to username when no display name provided', () => {
    render(<SocialRecoveryApproval {...defaultProps} requesterDisplayName={undefined} />);
    const strongElements = document.querySelectorAll('strong');
    const text = Array.from(strongElements).map((el) => el.textContent);
    expect(text).toContain('alice');
  });

  it('renders Approve and Decline buttons', () => {
    render(<SocialRecoveryApproval {...defaultProps} />);
    expect(screen.getByText('Approve')).toBeInTheDocument();
    expect(screen.getByText('Decline')).toBeInTheDocument();
  });

  it('shows explanatory text about encrypted channel', () => {
    render(<SocialRecoveryApproval {...defaultProps} />);
    expect(screen.getByText(/securely transfer your share/)).toBeInTheDocument();
  });

  // ── Decline ────────────────────────────────────────────────────────────

  it('calls onClose when Decline is clicked', () => {
    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Decline'));
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  // ── Approve Flow ───────────────────────────────────────────────────────

  it('shows loading state when approving', async () => {
    mockApiFetch.mockReturnValue(new Promise(() => {})); // Never resolves

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(screen.getByText('Submitting...')).toBeInTheDocument();
    });
  });

  it('disables buttons while loading', async () => {
    mockApiFetch.mockReturnValue(new Promise(() => {}));

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(screen.getByText('Submitting...')).toBeDisabled();
      expect(screen.getByText('Decline')).toBeDisabled();
    });
  });

  it('shows success screen after successful approval', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(screen.getByText('Share Submitted')).toBeInTheDocument();
      expect(screen.getByText(/recovery share has been securely sent/)).toBeInTheDocument();
    });
  });

  it('shows Close button on success screen', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(screen.getByText('Close')).toBeInTheDocument();
    });
  });

  it('calls onClose when Close is clicked on success screen', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(screen.getByText('Close')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Close'));
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('sends approval to correct API endpoint', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/mfa/recovery-requests/social/req-123/respond',
        expect.objectContaining({
          method: 'POST',
        })
      );
    });
  });

  // ── Error States ───────────────────────────────────────────────────────

  it('shows error when E2EE keys are not available', async () => {
    (e2eeService.getWrappingKey as ReturnType<typeof vi.fn>).mockReturnValueOnce(null);

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(screen.getByText('E2EE keys not available')).toBeInTheDocument();
    });
  });

  it('shows error when API request fails', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Request expired' }),
    });

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(screen.getByText('Request expired')).toBeInTheDocument();
    });
  });

  it('shows generic error on unexpected failure', async () => {
    mockUnwrapKey.mockRejectedValueOnce(new Error('decrypt failed'));

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(screen.getByText('decrypt failed')).toBeInTheDocument();
    });
  });

  it('re-enables buttons after error', async () => {
    (e2eeService.getWrappingKey as ReturnType<typeof vi.fn>).mockReturnValueOnce(null);

    render(<SocialRecoveryApproval {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve'));

    await waitFor(() => {
      expect(screen.getByText('E2EE keys not available')).toBeInTheDocument();
    });

    expect(screen.getByText('Approve')).not.toBeDisabled();
    expect(screen.getByText('Decline')).not.toBeDisabled();
  });
});
