import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, act } from '../../test-utils';
import { resetAllStores } from '../../helpers/store-helpers';
import {
  AttestationFailedModal,
  type AttestationFailedModalProps,
} from '@/renderer/components/AttestationFailedModal';
import AttestationFailedModalHost from '@/renderer/components/AttestationFailedModal';
import { useAttestationFailureStore } from '@/renderer/stores/attestationFailureStore';

// ── Mock CSS import so Vitest doesn't choke on it ─────────────────────────
vi.mock('@/renderer/components/AttestationFailedModal.css', () => ({}));

// ── Electron bridge mock helpers ──────────────────────────────────────────
//
// setup.ts pre-installs a base window.electron object. We extend it with
// openExternal so we can assert calls. Pattern mirrors SafeLink.test.tsx.

const installOpenExternalMock = (openExternal: ReturnType<typeof vi.fn>): void => {
  const electron = (window as unknown as { electron?: Record<string, unknown> }).electron;
  if (!electron) {
    throw new Error('window.electron must be pre-installed by tests/setup.ts');
  }
  electron.openExternal = openExternal;
};

// ── Shared render helper for the presentational component ─────────────────

function renderPresentation(overrides: Partial<AttestationFailedModalProps> = {}) {
  const onDismiss = vi.fn();
  const props: AttestationFailedModalProps = {
    code: 'ATTESTATION_UNKNOWN_RELEASE',
    onDismiss,
    ...overrides,
  };
  render(<AttestationFailedModal {...props} />);
  return { onDismiss };
}

// ═════════════════════════════════════════════════════════════════════════════
// Presentational component tests
// ═════════════════════════════════════════════════════════════════════════════

describe('AttestationFailedModal (presentational)', () => {
  beforeEach(() => {
    resetAllStores();
    installOpenExternalMock(vi.fn());
  });

  // 1. Dialog role + accessible name
  it('renders a dialog with role="dialog", aria-modal="true", and title "Update Required"', () => {
    renderPresentation();
    const dialog = screen.getByRole('dialog');
    expect(dialog).toBeInTheDocument();
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    // The dialog is labelled by #attestation-modal-title which reads "Update Required"
    expect(screen.getByRole('heading', { name: 'Update Required' })).toBeInTheDocument();
  });

  // 2a. Shows requiredMinVersion when provided
  it('shows requiredMinVersion when provided', () => {
    renderPresentation({ requiredMinVersion: '0.2.0' });
    expect(screen.getByText(/Required minimum version:/i)).toBeInTheDocument();
    expect(screen.getByText('0.2.0')).toBeInTheDocument();
  });

  // 2b. Does NOT render version line when omitted
  it('does NOT render the version line when requiredMinVersion is omitted', () => {
    renderPresentation();
    expect(screen.queryByText(/Required minimum version:/i)).not.toBeInTheDocument();
  });

  // 3. Renders download link for a safe URL and calls openExternal on click
  it('renders "Download Official Client" link for https URL and calls openExternal on click', () => {
    const openExternal = vi.fn().mockResolvedValue({ ok: true });
    installOpenExternalMock(openExternal);

    renderPresentation({ downloadHelpUrl: 'https://concordvoice.com/download' });

    const link = screen.getByText('Download Official Client');
    expect(link).toBeInTheDocument();
    expect(link.tagName).toBe('A');
    expect(link).toHaveAttribute('href', 'https://concordvoice.com/download');
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');

    fireEvent.click(link);

    expect(openExternal).toHaveBeenCalledWith('https://concordvoice.com/download');
  });

  // 4a. SECURITY: javascript: URL renders no link
  it('SECURITY: renders NO download link for javascript: URL', () => {
    const openExternal = vi.fn();
    installOpenExternalMock(openExternal);

    renderPresentation({ downloadHelpUrl: 'javascript:alert(1)' });

    expect(screen.queryByText('Download Official Client')).not.toBeInTheDocument();
    expect(openExternal).not.toHaveBeenCalled();
  });

  // 4b. SECURITY: file: URL renders no link
  it('SECURITY: renders NO download link for file: URL', () => {
    const openExternal = vi.fn();
    installOpenExternalMock(openExternal);

    renderPresentation({ downloadHelpUrl: 'file:///etc/passwd' });

    expect(screen.queryByText('Download Official Client')).not.toBeInTheDocument();
    expect(openExternal).not.toHaveBeenCalled();
  });

  // 4c. Absent downloadHelpUrl renders no link
  it('renders NO download link when downloadHelpUrl is absent', () => {
    renderPresentation();
    expect(screen.queryByText('Download Official Client')).not.toBeInTheDocument();
  });

  // 5. Dismiss button calls onDismiss
  it('clicking "Dismiss" calls the onDismiss prop', () => {
    const { onDismiss } = renderPresentation();
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(onDismiss).toHaveBeenCalledOnce();
  });
});

// ═════════════════════════════════════════════════════════════════════════════
// Store-connected host tests
// ═════════════════════════════════════════════════════════════════════════════

describe('AttestationFailedModalHost (store-connected)', () => {
  beforeEach(() => {
    resetAllStores();
    installOpenExternalMock(vi.fn());
  });

  // 6. Renders nothing when store is in initial state
  it('renders nothing when store is in initial state (visible: false)', () => {
    render(<AttestationFailedModalHost />);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  // 7. Shows dialog after showFailure is called
  it('shows the dialog after showFailure is called', () => {
    render(<AttestationFailedModalHost />);

    act(() => {
      useAttestationFailureStore.getState().showFailure({
        code: 'ATTESTATION_UNKNOWN_RELEASE',
        requiredMinVersion: '0.2.0',
        downloadHelpUrl: 'https://concordvoice.com/download',
      });
    });

    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Update Required' })).toBeInTheDocument();
    expect(screen.getByText('0.2.0')).toBeInTheDocument();
    expect(screen.getByText('Download Official Client')).toBeInTheDocument();
  });

  // 8. Clicking Dismiss flips store back to visible: false
  it('clicking "Dismiss" hides the modal and resets the store to visible:false', () => {
    render(<AttestationFailedModalHost />);

    act(() => {
      useAttestationFailureStore.getState().showFailure({
        code: 'ATTESTATION_REVOKED',
      });
    });

    expect(screen.getByRole('dialog')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(useAttestationFailureStore.getState().visible).toBe(false);
  });
});
