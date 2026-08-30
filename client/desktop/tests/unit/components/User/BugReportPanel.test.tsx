import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';
import BugReportPanel from '@/renderer/components/User/BugReportPanel';
import type { SystemInfo } from '@/renderer/services/system/systemInfoService';

// ── Mocks ──────────────────────────────────────────────────────────────────
const mockCollect = vi.fn();
vi.mock('@/renderer/services/system/systemInfoService', () => ({
  collect: () => mockCollect(),
}));

const mockGetEntries = vi.fn(() => []);
const mockFormatEntries = vi.fn(() => '2026-06-16T00:00:00Z  [warn]  sample sanitized log');
vi.mock('@/renderer/services/system/logBufferService', () => ({
  getEntries: () => mockGetEntries(),
  formatEntries: () => mockFormatEntries(),
}));

const SAMPLE_SYSTEM_INFO: SystemInfo = {
  appVersion: '0.1.63',
  platform: 'darwin',
  userAgent: 'jsdom',
  machineIdPrefix: '4c33734c',
  gpu: { vendor: 'Apple', renderer: 'M1 Pro' },
  display: { width: 3024, height: 1964, scaleFactor: 2, refreshRate: 120 },
  connectionPhase: 'stable',
};

function fillForm(title: string, description: string) {
  fireEvent.change(screen.getByLabelText(/Title/), { target: { value: title } });
  fireEvent.change(screen.getByLabelText(/Description/), { target: { value: description } });
}

describe('BugReportPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockCollect.mockResolvedValue(SAMPLE_SYSTEM_INFO);
    mockGetEntries.mockReturnValue([]);
    mockFormatEntries.mockReturnValue('2026-06-16T00:00:00Z  [warn]  sample sanitized log');
  });

  describe('rendering', () => {
    it('renders title, description, include-logs checkbox, and submit button', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      expect(screen.getByLabelText(/Title/)).toBeInTheDocument();
      expect(screen.getByLabelText(/Description/)).toBeInTheDocument();
      expect(screen.getByLabelText('Include diagnostic logs')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Submit Bug Report' })).toBeInTheDocument();
    });

    it('description placeholder carries the guided prompts', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      const textarea = screen.getByLabelText(/Description/) as HTMLTextAreaElement;
      expect(textarea.placeholder).toMatch(/What were you trying to do/);
      expect(textarea.placeholder).toMatch(/What happened instead/);
      expect(textarea.placeholder).toMatch(/Steps to reproduce/);
    });

    it('renders the always-visible diagnostics disclosure with the exact spec text', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      const disclosure = screen.getByText(/Includes your anonymous machine ID/);
      expect(disclosure).toBeInTheDocument();
      expect(disclosure.textContent).toMatch(/automatically.*stripped/);
      expect(disclosure.textContent).toMatch(/No message content, friend lists/);
    });

    it('discloses connection state — every field buildDiagnostics sends must be named', () => {
      // Consent-accuracy lock: connectionPhase IS transmitted (see the
      // diagnostics submission test below), so the disclosure must name it.
      // Guards against the disclosure drifting out of sync with the payload.
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      const disclosure = screen.getByText(/Includes your anonymous machine ID/);
      expect(disclosure.textContent).toMatch(/connection state/i);
    });

    it('wires the checkbox to the disclosure via aria-describedby', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      const checkbox = screen.getByLabelText('Include diagnostic logs');
      const describedBy = checkbox.getAttribute('aria-describedby');
      expect(describedBy).toBeTruthy();
      const disclosure = screen.getByText(/Includes your anonymous machine ID/);
      expect(disclosure.id).toBe(describedBy);
    });

    it('enforces maxLength on title (120) and description (5000)', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      expect(screen.getByLabelText(/Title/)).toHaveAttribute('maxLength', '120');
      expect(screen.getByLabelText(/Description/)).toHaveAttribute('maxLength', '5000');
    });
  });

  describe('validation', () => {
    it('disables submit when both fields are empty', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      expect(screen.getByRole('button', { name: 'Submit Bug Report' })).toBeDisabled();
    });

    it('disables submit when only the title is filled', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fireEvent.change(screen.getByLabelText(/Title/), { target: { value: 'Crash' } });
      expect(screen.getByRole('button', { name: 'Submit Bug Report' })).toBeDisabled();
    });

    it('disables submit when only the description is filled', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fireEvent.change(screen.getByLabelText(/Description/), { target: { value: 'It broke' } });
      expect(screen.getByRole('button', { name: 'Submit Bug Report' })).toBeDisabled();
    });

    it('treats whitespace-only input as empty (submit stays disabled)', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fillForm('   ', '   ');
      expect(screen.getByRole('button', { name: 'Submit Bug Report' })).toBeDisabled();
    });

    it('enables submit when both fields are valid', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fillForm('Crash on send', 'It crashed when I clicked send.');
      expect(screen.getByRole('button', { name: 'Submit Bug Report' })).toBeEnabled();
    });

    it('shows live character counters', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fireEvent.change(screen.getByLabelText(/Title/), { target: { value: 'abc' } });
      expect(screen.getByText('3/120')).toBeInTheDocument();
    });

    it('keeps submit disabled when the title exceeds the 120-char cap', () => {
      // jsdom does not enforce maxLength on programmatic value changes, so this
      // drives the `title.length <= TITLE_MAX` false branch that the browser
      // maxLength attribute would otherwise prevent — the defensive cap is real
      // and covered.
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fillForm('a'.repeat(121), 'A valid description.');
      expect(screen.getByRole('button', { name: 'Submit Bug Report' })).toBeDisabled();
    });

    it('keeps submit disabled when the description exceeds the 5000-char cap', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fillForm('Valid title', 'd'.repeat(5001));
      expect(screen.getByRole('button', { name: 'Submit Bug Report' })).toBeDisabled();
    });

    it('does not call onSubmit when the form is submitted while invalid', () => {
      // The submit button is disabled when invalid, but a programmatic form
      // submit must also be a no-op — covers the handleSubmit early-return
      // guard (`if (!titleValid || !descriptionValid ...) return`).
      const onSubmit = vi.fn();
      render(<BugReportPanel onSubmit={onSubmit} isSubmitting={false} />);
      const form = screen.getByRole('button', { name: 'Submit Bug Report' }).closest('form');
      if (form) fireEvent.submit(form);
      expect(onSubmit).not.toHaveBeenCalled();
    });
  });

  describe('submission', () => {
    it('submits without diagnostics when the log checkbox is unchecked', async () => {
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<BugReportPanel onSubmit={onSubmit} isSubmitting={false} />);
      fillForm('Crash on send', 'It crashed.');
      fireEvent.click(screen.getByRole('button', { name: 'Submit Bug Report' }));

      await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
      const payload = onSubmit.mock.calls[0][0];
      expect(payload.type).toBe('bug');
      expect(payload.title).toBe('Crash on send');
      expect(payload.description).toBe('It crashed.');
      expect(payload.diagnostics).toBeUndefined();
      // collect() must NOT run when the user did not opt in.
      expect(mockCollect).not.toHaveBeenCalled();
    });

    it('attaches diagnostics when the log checkbox is checked', async () => {
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<BugReportPanel onSubmit={onSubmit} isSubmitting={false} />);
      fillForm('Crash on send', 'It crashed.');
      fireEvent.click(screen.getByLabelText('Include diagnostic logs'));
      fireEvent.click(screen.getByRole('button', { name: 'Submit Bug Report' }));

      await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
      const payload = onSubmit.mock.calls[0][0];
      expect(payload.diagnostics).toBeDefined();
      expect(payload.diagnostics.appVersion).toBe('0.1.63');
      expect(payload.diagnostics.machineIdPrefix).toBe('4c33734c');
      expect(payload.diagnostics.platform).toBe('darwin');
      expect(payload.diagnostics.connectionPhase).toBe('stable');
      expect(payload.diagnostics.gpu).toEqual({ vendor: 'Apple', renderer: 'M1 Pro' });
      expect(payload.diagnostics.logs).toMatch(/sample sanitized log/);
      expect(mockCollect).toHaveBeenCalledTimes(1);
    });

    it('pseudonymizes UUIDs in the logs field before submitting (#2074)', async () => {
      const uuid = '550e8400-e29b-41d4-a716-446655440000';
      mockFormatEntries.mockReturnValue(`for channel ${uuid} again ${uuid}`);
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<BugReportPanel onSubmit={onSubmit} isSubmitting={false} />);
      fillForm('Crash on send', 'It crashed.');
      fireEvent.click(screen.getByLabelText('Include diagnostic logs'));
      fireEvent.click(screen.getByRole('button', { name: 'Submit Bug Report' }));

      await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
      const logs = onSubmit.mock.calls[0][0].diagnostics.logs;
      expect(logs).toContain('<id:1>');
      expect(logs).not.toContain(uuid);
      // The same UUID maps to the same ordinal within the report.
      expect(logs).toBe('for channel <id:1> again <id:1>');
    });

    it('trims title and description in the submitted payload', async () => {
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<BugReportPanel onSubmit={onSubmit} isSubmitting={false} />);
      fillForm('  Padded title  ', '  Padded body  ');
      fireEvent.click(screen.getByRole('button', { name: 'Submit Bug Report' }));

      await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
      const payload = onSubmit.mock.calls[0][0];
      expect(payload.title).toBe('Padded title');
      expect(payload.description).toBe('Padded body');
    });

    it('still submits (without diagnostics) if collect() rejects', async () => {
      mockCollect.mockRejectedValueOnce(new Error('probe failed'));
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<BugReportPanel onSubmit={onSubmit} isSubmitting={false} />);
      fillForm('Crash', 'Body');
      fireEvent.click(screen.getByLabelText('Include diagnostic logs'));
      fireEvent.click(screen.getByRole('button', { name: 'Submit Bug Report' }));

      await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
      const payload = onSubmit.mock.calls[0][0];
      // Diagnostics assembly failed → report still goes out, just without them.
      expect(payload.diagnostics).toBeUndefined();
      expect(payload.title).toBe('Crash');
    });
  });

  describe('logs preview (#2078)', () => {
    it('opens the preview modal showing the exact buildDiagnostics logs', async () => {
      // The preview calls the SAME buildDiagnostics() the submit path uses, so
      // the pseudonymized log line (UUID → <id:1>) proves preview == payload.
      const uuid = '550e8400-e29b-41d4-a716-446655440000';
      mockFormatEntries.mockReturnValue(`for channel ${uuid}`);
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);

      fireEvent.click(screen.getByRole('button', { name: /what's in the logs/i }));

      expect(await screen.findByText(/for channel <id:1>/)).toBeInTheDocument();
      expect(screen.queryByText(/550e8400-/)).toBeNull();
    });

    it('opening/closing the preview does not change the include-logs checkbox', async () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);
      const checkbox = screen.getByLabelText('Include diagnostic logs');
      expect(checkbox).not.toBeChecked();

      fireEvent.click(screen.getByRole('button', { name: /what's in the logs/i }));
      await screen.findByRole('heading', { name: /what's in the logs/i });

      // Topmost modal is the preview → Escape closes it; the panel stays.
      fireEvent.keyDown(document, { key: 'Escape' });

      await waitFor(() =>
        expect(screen.queryByRole('heading', { name: /what's in the logs/i })).toBeNull()
      );
      expect(checkbox).not.toBeChecked();
    });

    it('surfaces a graceful error state when diagnostics collection fails', async () => {
      mockCollect.mockRejectedValueOnce(new Error('probe failed'));
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={false} />);

      fireEvent.click(screen.getByRole('button', { name: /what's in the logs/i }));

      expect(await screen.findByText(/couldn't collect diagnostics/i)).toBeInTheDocument();
    });

    it('closing the preview with X does NOT submit the bug report (Gitar HIGH, #2086)', async () => {
      // Regression lock: `ui/Modal` renders inline (no portal) and its close (X)
      // button has no explicit `type`, so if the modal were a descendant of the
      // <form> the default `type="submit"` X-click would submit the report. The
      // modal is now a form sibling; clicking X must only close it. The existing
      // ESC-close test above did NOT cover this button-click submit path.
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<BugReportPanel onSubmit={onSubmit} isSubmitting={false} />);

      fireEvent.click(screen.getByRole('button', { name: /what's in the logs/i }));
      await screen.findByRole('heading', { name: /what's in the logs/i });

      fireEvent.click(screen.getByRole('button', { name: /close/i }));

      await waitFor(() =>
        expect(screen.queryByRole('heading', { name: /what's in the logs/i })).toBeNull()
      );
      // The X close must not have submitted the form.
      expect(onSubmit).not.toHaveBeenCalled();
    });
  });

  describe('submitting state', () => {
    it('disables the fields and button while isSubmitting', () => {
      render(<BugReportPanel onSubmit={vi.fn()} isSubmitting={true} />);
      expect(screen.getByLabelText(/Title/)).toBeDisabled();
      expect(screen.getByLabelText(/Description/)).toBeDisabled();
      expect(screen.getByLabelText('Include diagnostic logs')).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Submitting…' })).toBeDisabled();
    });

    it('does not call onSubmit a second time while a submit is in flight', () => {
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      // isSubmitting=true means the button is disabled; a programmatic form
      // submit (Enter) must also be a no-op.
      render(<BugReportPanel onSubmit={onSubmit} isSubmitting={true} />);
      const form = screen.getByRole('button', { name: 'Submitting…' }).closest('form');
      if (form) fireEvent.submit(form);
      expect(onSubmit).not.toHaveBeenCalled();
    });

    it('guards against a double-submit during the diagnostics-collection window', async () => {
      // The parent isSubmitting prop only flips after onSubmit runs, but with
      // logs checked we await collect() BEFORE onSubmit. Two rapid submits in
      // that window must still produce exactly ONE onSubmit (synchronous ref
      // guard), not two POSTs / two GitHub issues.
      let releaseCollect!: (v: SystemInfo) => void;
      mockCollect.mockReturnValue(
        new Promise<SystemInfo>((resolve) => {
          releaseCollect = resolve;
        })
      );
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<BugReportPanel onSubmit={onSubmit} isSubmitting={false} />);
      fillForm('Crash', 'Body');
      fireEvent.click(screen.getByLabelText('Include diagnostic logs'));

      const submitBtn = screen.getByRole('button', { name: 'Submit Bug Report' });
      // Both clicks land while collect() is still pending (isSubmitting=false).
      fireEvent.click(submitBtn);
      fireEvent.click(submitBtn);
      // Now let diagnostics collection resolve for the first (only) submit.
      releaseCollect(SAMPLE_SYSTEM_INFO);

      await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
      // Sanity: the single submit still carried diagnostics.
      expect(onSubmit.mock.calls[0][0].diagnostics).toBeDefined();
    });
  });
});
