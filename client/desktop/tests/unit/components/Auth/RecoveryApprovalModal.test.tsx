import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';

// Mock crypto utilities
vi.mock('@/renderer/utils/crypto', () => ({
  generateECDHKeyPair: vi.fn().mockResolvedValue({
    publicKey: 'mock-pub',
    privateKey: 'mock-priv',
  }),
  exportECDHPublicKey: vi.fn().mockResolvedValue('mock-exported-pub-key'),
  importECDHPublicKey: vi.fn().mockResolvedValue('mock-imported-pub-key'),
  deriveSharedSecret: vi.fn().mockResolvedValue('mock-shared-key'),
  encryptWithSharedSecret: vi.fn().mockResolvedValue('mock-encrypted-payload'),
  base64ToArrayBuffer: vi.fn().mockReturnValue(new ArrayBuffer(32)),
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
const mockExportKey = vi.fn().mockResolvedValue(new ArrayBuffer(64));

Object.defineProperty(globalThis, 'crypto', {
  value: {
    subtle: {
      unwrapKey: mockUnwrapKey,
      exportKey: mockExportKey,
    },
    getRandomValues: (arr: Uint8Array) => arr,
  },
  writable: true,
  configurable: true,
});

import RecoveryApprovalModal from '@/renderer/components/Auth/RecoveryApprovalModal';
import { e2eeService } from '@/renderer/services/e2eeService';

describe('RecoveryApprovalModal', () => {
  const defaultProps = {
    requestId: 'req-456',
    requesterEphemeralKey: 'bW9jay1lcGhlbWVyYWw=',
    createdAt: '2026-03-20T12:00:00Z',
    onClose: vi.fn(),
  };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  // ── Initial Rendering ──────────────────────────────────────────────────

  it('renders account recovery request modal', () => {
    render(<RecoveryApprovalModal {...defaultProps} />);
    expect(screen.getByText('Account Recovery Request')).toBeInTheDocument();
  });

  it('displays the creation date of the request', () => {
    render(<RecoveryApprovalModal {...defaultProps} />);
    expect(screen.getByText(/recovery request was created on/)).toBeInTheDocument();
  });

  it('renders Approve Recovery and Reject buttons', () => {
    render(<RecoveryApprovalModal {...defaultProps} />);
    expect(screen.getByText('Approve Recovery')).toBeInTheDocument();
    expect(screen.getByText('Reject')).toBeInTheDocument();
  });

  it('mentions encrypted channel in the description', () => {
    render(<RecoveryApprovalModal {...defaultProps} />);
    expect(screen.getByText(/securely transfer your private key/)).toBeInTheDocument();
  });

  // ── Reject Flow ────────────────────────────────────────────────────────

  it('sends reject request and calls onClose', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Reject'));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/mfa/recovery-requests/req-456/respond',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ action: 'reject' }),
        })
      );
    });

    await waitFor(() => {
      expect(defaultProps.onClose).toHaveBeenCalled();
    });
  });

  it('calls onClose even if reject API call fails', async () => {
    mockApiFetch.mockRejectedValueOnce(new Error('network'));

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Reject'));

    await waitFor(() => {
      expect(defaultProps.onClose).toHaveBeenCalled();
    });
  });

  // ── Approve Flow ───────────────────────────────────────────────────────

  it('shows loading state when approving', async () => {
    mockApiFetch.mockReturnValue(new Promise(() => {}));

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(screen.getByText('Approving...')).toBeInTheDocument();
    });
  });

  it('disables buttons while loading', async () => {
    mockApiFetch.mockReturnValue(new Promise(() => {}));

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(screen.getByText('Approving...')).toBeDisabled();
      expect(screen.getByText('Reject')).toBeDisabled();
    });
  });

  it('shows success screen after successful approval', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(screen.getByText('Recovery Approved')).toBeInTheDocument();
      expect(
        screen.getByText(/encrypted private key has been securely transferred/)
      ).toBeInTheDocument();
    });
  });

  it('shows Close button on success and calls onClose when clicked', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(screen.getByText('Close')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('Close'));
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('sends approval to correct API endpoint with action:approve', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/mfa/recovery-requests/req-456/respond',
        expect.objectContaining({
          method: 'POST',
          body: expect.stringContaining('"action":"approve"'),
        })
      );
    });
  });

  it('includes encrypted_payload and responder_public_key in approval', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      const body = JSON.parse((mockApiFetch.mock.calls[0][1] as { body: string }).body);
      expect(body.encrypted_payload).toBe('mock-encrypted-payload');
      expect(body.responder_public_key).toBe('mock-exported-pub-key');
    });
  });

  // ── Error States ───────────────────────────────────────────────────────

  it('shows error when E2EE keys are not available', async () => {
    (e2eeService.getWrappingKey as ReturnType<typeof vi.fn>).mockReturnValueOnce(null);

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(
        screen.getByText('E2EE keys not available. Please ensure you are logged in.')
      ).toBeInTheDocument();
    });
  });

  it('shows error when wrapped private key is null', async () => {
    (e2eeService.getWrappedPrivateKey as ReturnType<typeof vi.fn>).mockReturnValueOnce(null);

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(screen.getByText(/E2EE keys not available/)).toBeInTheDocument();
    });
  });

  it('shows error when API request fails', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Request expired' }),
    });

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(screen.getByText('Request expired')).toBeInTheDocument();
    });
  });

  it('shows error when crypto unwrapKey fails', async () => {
    mockUnwrapKey.mockRejectedValueOnce(new Error('Decryption failed'));

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(screen.getByText('Decryption failed')).toBeInTheDocument();
    });
  });

  it('re-enables buttons after error', async () => {
    (e2eeService.getWrappingKey as ReturnType<typeof vi.fn>).mockReturnValueOnce(null);

    render(<RecoveryApprovalModal {...defaultProps} />);
    fireEvent.click(screen.getByText('Approve Recovery'));

    await waitFor(() => {
      expect(screen.getByText(/E2EE keys not available/)).toBeInTheDocument();
    });

    expect(screen.getByText('Approve Recovery')).not.toBeDisabled();
    expect(screen.getByText('Reject')).not.toBeDisabled();
  });
});
