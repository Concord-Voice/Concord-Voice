import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';
import FeedbackModal from '@/renderer/components/User/FeedbackModal';

// Mock the connection store the systemInfoService imports
vi.mock('@/renderer/stores/connectionStore', () => ({
  useConnectionStore: { getState: () => ({ phase: 'stable' }) },
}));

// Mock apiClient — both apiFetch and safeJson
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: async <T,>(res: Response): Promise<T> => (await res.json()) as T,
}));

describe('FeedbackModal', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe('rendering', () => {
    it('does not render when closed', () => {
      render(<FeedbackModal isOpen={false} onClose={vi.fn()} />);
      expect(screen.queryByText('Send Feedback')).not.toBeInTheDocument();
    });

    it('renders the modal title when open', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      expect(screen.getByText('Send Feedback')).toBeInTheDocument();
    });

    it('shows both mode tabs', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      expect(screen.getByRole('tab', { name: 'Bug Report' })).toBeInTheDocument();
      expect(screen.getByRole('tab', { name: 'Feature Request' })).toBeInTheDocument();
    });
  });

  describe('default mode', () => {
    it('defaults to Bug Report mode', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      const bugTab = screen.getByRole('tab', { name: 'Bug Report' });
      expect(bugTab).toHaveAttribute('aria-selected', 'true');
    });

    it('honors initialMode prop', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} initialMode="feature" />);
      const featureTab = screen.getByRole('tab', { name: 'Feature Request' });
      expect(featureTab).toHaveAttribute('aria-selected', 'true');
    });
  });

  describe('mode switching', () => {
    it('switches active tab on click', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      const featureTab = screen.getByRole('tab', { name: 'Feature Request' });
      fireEvent.click(featureTab);
      expect(featureTab).toHaveAttribute('aria-selected', 'true');
    });

    it('shows the bug form in bug mode and the feature form in feature mode', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      // Bug mode renders BugReportPanel (#159); its submit button is the marker.
      expect(screen.getByRole('button', { name: 'Submit Bug Report' })).toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: 'Submit Feature Request' })
      ).not.toBeInTheDocument();

      fireEvent.click(screen.getByRole('tab', { name: 'Feature Request' }));
      // Feature mode renders FeatureRequestPanel (#160).
      expect(screen.getByRole('button', { name: 'Submit Feature Request' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Submit Bug Report' })).not.toBeInTheDocument();
    });
  });

  describe('close handling', () => {
    it('calls onClose when Close button clicked', () => {
      const onClose = vi.fn();
      render(<FeedbackModal isOpen={true} onClose={onClose} />);
      fireEvent.click(screen.getByText('Close'));
      expect(onClose).toHaveBeenCalled();
    });
  });

  describe('accessibility — WAI-ARIA tabs', () => {
    it('wires each tab to the panel (aria-controls) and the panel to the active tab (aria-labelledby)', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      const bugTab = screen.getByRole('tab', { name: 'Bug Report' });
      const panel = screen.getByRole('tabpanel');
      expect(bugTab.id).toBeTruthy();
      expect(panel.id).toBeTruthy();
      expect(bugTab).toHaveAttribute('aria-controls', panel.id);
      expect(panel).toHaveAttribute('aria-labelledby', bugTab.id);
    });

    it('uses roving tabindex (active tab 0, inactive -1)', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      expect(screen.getByRole('tab', { name: 'Bug Report' })).toHaveAttribute('tabindex', '0');
      expect(screen.getByRole('tab', { name: 'Feature Request' })).toHaveAttribute(
        'tabindex',
        '-1'
      );
    });

    it('ArrowRight moves to the next tab and wraps', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      fireEvent.keyDown(screen.getByRole('tab', { name: 'Bug Report' }), { key: 'ArrowRight' });
      expect(screen.getByRole('tab', { name: 'Feature Request' })).toHaveAttribute(
        'aria-selected',
        'true'
      );
      // Wrap back to bug from feature.
      fireEvent.keyDown(screen.getByRole('tab', { name: 'Feature Request' }), {
        key: 'ArrowRight',
      });
      expect(screen.getByRole('tab', { name: 'Bug Report' })).toHaveAttribute(
        'aria-selected',
        'true'
      );
    });

    it('ArrowLeft moves to the previous tab and wraps', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      // From bug, ArrowLeft wraps to feature (last).
      fireEvent.keyDown(screen.getByRole('tab', { name: 'Bug Report' }), { key: 'ArrowLeft' });
      expect(screen.getByRole('tab', { name: 'Feature Request' })).toHaveAttribute(
        'aria-selected',
        'true'
      );
    });

    it('ignores non-navigation keys on a tab (no mode change)', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      const bugTab = screen.getByRole('tab', { name: 'Bug Report' });
      fireEvent.keyDown(bugTab, { key: 'a' });
      expect(bugTab).toHaveAttribute('aria-selected', 'true');
      expect(screen.getByRole('tab', { name: 'Feature Request' })).toHaveAttribute(
        'aria-selected',
        'false'
      );
    });

    it('Home / End jump to the first / last tab', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      fireEvent.keyDown(screen.getByRole('tab', { name: 'Bug Report' }), { key: 'End' });
      expect(screen.getByRole('tab', { name: 'Feature Request' })).toHaveAttribute(
        'aria-selected',
        'true'
      );
      fireEvent.keyDown(screen.getByRole('tab', { name: 'Feature Request' }), { key: 'Home' });
      expect(screen.getByRole('tab', { name: 'Bug Report' })).toHaveAttribute(
        'aria-selected',
        'true'
      );
    });

    it('exposes a polite live region for the submit status surface', () => {
      const { container } = render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      expect(container.querySelector('[aria-live="polite"]')).toBeInTheDocument();
    });
  });

  describe('mode switch resets the submit surface (#159 review)', () => {
    it('switching tabs after a successful submit returns to a fresh form', async () => {
      mockApiFetch.mockResolvedValueOnce(
        new Response(JSON.stringify({ dev: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      // Submit a bug report (default mode) to reach the success surface.
      fireEvent.change(screen.getByLabelText(/Title/), { target: { value: 'A bug' } });
      fireEvent.change(screen.getByLabelText(/Description/), { target: { value: 'It broke.' } });
      fireEvent.click(screen.getByRole('button', { name: 'Submit Bug Report' }));
      await waitFor(() =>
        expect(screen.getByText(/Thank you for the feedback/)).toBeInTheDocument()
      );

      // Switching to feature mode must clear the success surface and show the form.
      fireEvent.click(screen.getByRole('tab', { name: 'Feature Request' }));
      expect(screen.queryByText(/Thank you for the feedback/)).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Submit Feature Request' })).toBeInTheDocument();
    });

    it('preserves an in-flight submitting state when switching tabs mid-submit', async () => {
      // The reset must NOT clobber a submitting state — the pipe is still
      // running and will resolve into success/error on its own.
      let releaseFetch!: (r: Response) => void;
      mockApiFetch.mockReturnValue(
        new Promise<Response>((resolve) => {
          releaseFetch = resolve;
        })
      );
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      // Start a bug submit and leave the fetch pending.
      fireEvent.change(screen.getByLabelText(/Title/), { target: { value: 'A bug' } });
      fireEvent.change(screen.getByLabelText(/Description/), { target: { value: 'It broke.' } });
      fireEvent.click(screen.getByRole('button', { name: 'Submit Bug Report' }));
      // "Submitting…" shows in both the modal status region and on the bug
      // panel's button, so match with getAllByText.
      await waitFor(() => expect(screen.getAllByText('Submitting…').length).toBeGreaterThan(0));

      // Switch tabs mid-submit — submitting state must survive the mode change.
      // (In feature mode + submitting, "Submitting…" appears both in the modal's
      // status region and on the FeatureRequestPanel submit button — either
      // proves the state was preserved, not reset to idle.)
      fireEvent.click(screen.getByRole('tab', { name: 'Feature Request' }));
      expect(screen.getAllByText('Submitting…').length).toBeGreaterThan(0);

      // Release the request so the component can settle (avoids act() warnings).
      releaseFetch(
        new Response(JSON.stringify({ dev: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
      await waitFor(() =>
        expect(screen.getByText(/Thank you for the feedback/)).toBeInTheDocument()
      );
    });
  });

  describe('bug submission integration (real BugReportPanel → submit → apiFetch)', () => {
    // End-to-end wiring test: fills the REAL BugReportPanel form inside the
    // modal and submits, asserting the assembled payload reaches apiFetch.
    // Locks the BugReportPanel → modal.submit → apiFetch seam that the
    // per-component unit tests each only cover one side of. No logs box
    // checked, so systemInfoService.collect() is never invoked (no extra
    // mocks needed).
    it('routes a bug report from the real form to POST /api/v1/feedback', async () => {
      mockApiFetch.mockResolvedValueOnce(
        new Response(JSON.stringify({ dev: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      // Default bug mode renders the real BugReportPanel.
      fireEvent.change(screen.getByLabelText(/Title/), { target: { value: 'Crash on send' } });
      fireEvent.change(screen.getByLabelText(/Description/), {
        target: { value: 'It crashed when I clicked send.' },
      });
      fireEvent.click(screen.getByRole('button', { name: 'Submit Bug Report' }));

      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(1));
      const [path, init] = mockApiFetch.mock.calls[0];
      expect(path).toBe('/api/v1/feedback');
      expect(init.method).toBe('POST');
      const body = JSON.parse(init.body as string);
      expect(body.type).toBe('bug');
      expect(body.title).toBe('Crash on send');
      expect(body.description).toBe('It crashed when I clicked send.');
      expect(body.diagnostics).toBeUndefined(); // logs box left unchecked

      // Success surface renders through the whole stack.
      expect(await screen.findByText(/Thank you for the feedback/)).toBeInTheDocument();
    });
  });

  describe('feature submission integration (real FeatureRequestPanel → submit → apiFetch)', () => {
    // Mirror of the bug integration test for the feature seam: fills the REAL
    // FeatureRequestPanel form (incl. the optional category) and submits,
    // asserting the assembled feature payload reaches apiFetch. Feature
    // requests never collect diagnostics, so no system-info mocks are needed.
    it('routes a feature request (with category) from the real form to POST /api/v1/feedback', async () => {
      mockApiFetch.mockResolvedValueOnce(
        new Response(JSON.stringify({ dev: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} initialMode="feature" />);
      fireEvent.change(screen.getByLabelText(/Title/), { target: { value: 'Dark mode toggle' } });
      fireEvent.change(screen.getByLabelText(/What would you like/), {
        target: { value: 'Please add a dark mode.' },
      });
      fireEvent.change(screen.getByLabelText(/Category/), { target: { value: 'New Feature' } });
      fireEvent.click(screen.getByRole('button', { name: 'Submit Feature Request' }));

      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(1));
      const [path, init] = mockApiFetch.mock.calls[0];
      expect(path).toBe('/api/v1/feedback');
      expect(init.method).toBe('POST');
      const body = JSON.parse(init.body as string);
      expect(body.type).toBe('feature');
      expect(body.title).toBe('Dark mode toggle');
      expect(body.description).toBe('Please add a dark mode.');
      expect(body.category).toBe('New Feature');
      expect(body.diagnostics).toBeUndefined(); // feature requests carry none

      expect(await screen.findByText(/Thank you for the feedback/)).toBeInTheDocument();
    });
  });

  describe('Modal width', () => {
    it('mounts inside the xlarge modal container (#158 wide layout)', () => {
      render(<FeedbackModal isOpen={true} onClose={vi.fn()} />);
      // The modal wrapper applies `modal-${width}` — xlarge is the new
      // 660px variant added on this branch. Asserting the class catches a
      // regression that changed the size prop without changing the spec.
      expect(document.querySelector('.modal-xlarge')).toBeInTheDocument();
    });
  });
});
