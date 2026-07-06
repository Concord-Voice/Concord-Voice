import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '../../../test-utils';
import DiagnosticsPreviewModal from '@/renderer/components/User/DiagnosticsPreviewModal';
import type { FeedbackDiagnostics } from '@/renderer/components/User/feedbackTypes';

// The shared `render` helper (test-utils) wraps children in the real
// `ModalProvider`, which `ui/Modal` requires for its depth-tracked nesting +
// topmost-ESC behavior.
//
// Note: dialog-role / aria-modal / focus-trap assertions are intentionally
// deferred — `ui/Modal` provides only ESC-returns-to-parent + a native close
// control today; full role="dialog"/aria-modal/focus-trap is a shared-ui/Modal
// a11y hardening follow-up, so this suite does not assert them.

const BUNDLE: FeedbackDiagnostics = {
  appVersion: '0.2.24',
  platform: 'darwin',
  machineIdPrefix: 'ab12cd34',
  gpu: { vendor: 'Apple', renderer: 'M3' },
  display: { width: 2560, height: 1440, scaleFactor: 2 },
  connectionPhase: 'connected',
  logs: '2026-07-06T00:00:00.000Z  [warn]  for channel <id:1>',
};

describe('DiagnosticsPreviewModal', () => {
  beforeEach(() => vi.clearAllMocks());

  it('renders nothing when closed', () => {
    render(<DiagnosticsPreviewModal isOpen={false} onClose={() => {}} diagnostics={BUNDLE} />);
    expect(screen.queryByRole('heading', { name: /what's in the logs/i })).toBeNull();
  });

  it('renders every diagnostics field and the logs block', () => {
    render(<DiagnosticsPreviewModal isOpen onClose={() => {}} diagnostics={BUNDLE} />);
    expect(screen.getByText('0.2.24')).toBeInTheDocument();
    expect(screen.getByText('darwin')).toBeInTheDocument();
    expect(screen.getByText('ab12cd34')).toBeInTheDocument();
    expect(screen.getByText(/Apple \/ M3/)).toBeInTheDocument();
    expect(screen.getByText(/2560×1440 @ 2x/)).toBeInTheDocument();
    expect(screen.getByText('connected')).toBeInTheDocument();
    expect(screen.getByText(/for channel <id:1>/)).toBeInTheDocument();
  });

  it('shows the empty-logs placeholder when logs are blank', () => {
    render(
      <DiagnosticsPreviewModal isOpen onClose={() => {}} diagnostics={{ ...BUNDLE, logs: '' }} />
    );
    expect(screen.getByText('(no logs captured yet)')).toBeInTheDocument();
  });

  it('shows a loading state', () => {
    render(<DiagnosticsPreviewModal isOpen onClose={() => {}} diagnostics={null} loading />);
    expect(screen.getByText(/collecting diagnostics/i)).toBeInTheDocument();
  });

  it('shows a graceful error state', () => {
    render(<DiagnosticsPreviewModal isOpen onClose={() => {}} diagnostics={null} error />);
    expect(screen.getByText(/couldn't collect diagnostics/i)).toBeInTheDocument();
  });

  it('handles missing gpu/display without crashing', () => {
    render(
      <DiagnosticsPreviewModal
        isOpen
        onClose={() => {}}
        diagnostics={{ ...BUNDLE, gpu: undefined, display: undefined }}
      />
    );
    expect(screen.getAllByText('(unavailable)')).toHaveLength(2);
  });
});
