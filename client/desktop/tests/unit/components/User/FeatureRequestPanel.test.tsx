import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';
import FeatureRequestPanel from '@/renderer/components/User/FeatureRequestPanel';

function fillForm(title: string, description: string) {
  fireEvent.change(screen.getByLabelText(/Title/), { target: { value: title } });
  fireEvent.change(screen.getByLabelText(/What would you like/), {
    target: { value: description },
  });
}

const SUBMIT = { name: 'Submit Feature Request' } as const;

describe('FeatureRequestPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe('rendering', () => {
    it('renders title, description, category select, and submit button', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      expect(screen.getByLabelText(/Title/)).toBeInTheDocument();
      expect(screen.getByLabelText(/What would you like/)).toBeInTheDocument();
      expect(screen.getByLabelText(/Category/)).toBeInTheDocument();
      expect(screen.getByRole('button', SUBMIT)).toBeInTheDocument();
    });

    it('description placeholder carries the guided prompts', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      const textarea = screen.getByLabelText(/What would you like/) as HTMLTextAreaElement;
      expect(textarea.placeholder).toMatch(/Describe the feature, fix, or change/);
      expect(textarea.placeholder).toMatch(/Why is this important to you/);
      expect(textarea.placeholder).toMatch(/How would this improve your experience/);
    });

    it('offers the five spec categories plus an empty default', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      const select = screen.getByLabelText(/Category/) as HTMLSelectElement;
      const values = Array.from(select.options).map((o) => o.value);
      expect(values).toEqual([
        '',
        'New Feature',
        'Improvement to Existing Feature',
        'UI/UX Change',
        'Performance',
        'Other',
      ]);
      // Default is the empty (no-selection) option.
      expect(select.value).toBe('');
    });

    it('enforces maxLength on title (120) and description (5000)', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      expect(screen.getByLabelText(/Title/)).toHaveAttribute('maxLength', '120');
      expect(screen.getByLabelText(/What would you like/)).toHaveAttribute('maxLength', '5000');
    });
  });

  describe('validation', () => {
    it('disables submit when both fields are empty', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      expect(screen.getByRole('button', SUBMIT)).toBeDisabled();
    });

    it('disables submit when only the title is filled', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fireEvent.change(screen.getByLabelText(/Title/), { target: { value: 'Dark mode' } });
      expect(screen.getByRole('button', SUBMIT)).toBeDisabled();
    });

    it('disables submit when only the description is filled', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fireEvent.change(screen.getByLabelText(/What would you like/), {
        target: { value: 'Please add it' },
      });
      expect(screen.getByRole('button', SUBMIT)).toBeDisabled();
    });

    it('treats whitespace-only input as empty (submit stays disabled)', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fillForm('   ', '   ');
      expect(screen.getByRole('button', SUBMIT)).toBeDisabled();
    });

    it('enables submit when both required fields are valid (category optional)', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fillForm('Dark mode toggle', 'Please add a dark mode.');
      expect(screen.getByRole('button', SUBMIT)).toBeEnabled();
    });

    it('shows live character counters', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fireEvent.change(screen.getByLabelText(/Title/), { target: { value: 'abc' } });
      expect(screen.getByText('3/120')).toBeInTheDocument();
    });

    it('keeps submit disabled when the title exceeds the 120-char cap', () => {
      // jsdom does not enforce maxLength on programmatic value changes, so this
      // drives the defensive `title.length <= TITLE_MAX` false branch.
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fillForm('a'.repeat(121), 'A valid description.');
      expect(screen.getByRole('button', SUBMIT)).toBeDisabled();
    });

    it('keeps submit disabled when the description exceeds the 5000-char cap', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={false} />);
      fillForm('Valid title', 'd'.repeat(5001));
      expect(screen.getByRole('button', SUBMIT)).toBeDisabled();
    });

    it('does not call onSubmit when the form is submitted while invalid', () => {
      const onSubmit = vi.fn();
      render(<FeatureRequestPanel onSubmit={onSubmit} isSubmitting={false} />);
      const form = screen.getByRole('button', SUBMIT).closest('form');
      if (form) fireEvent.submit(form);
      expect(onSubmit).not.toHaveBeenCalled();
    });
  });

  describe('submission', () => {
    it('submits type/title/description and omits category when none is selected', async () => {
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<FeatureRequestPanel onSubmit={onSubmit} isSubmitting={false} />);
      fillForm('Dark mode toggle', 'Please add a dark mode.');
      fireEvent.click(screen.getByRole('button', SUBMIT));

      await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
      const payload = onSubmit.mock.calls[0][0];
      expect(payload.type).toBe('feature');
      expect(payload.title).toBe('Dark mode toggle');
      expect(payload.description).toBe('Please add a dark mode.');
      expect(payload.category).toBeUndefined();
      // Feature requests never carry diagnostics.
      expect(payload.diagnostics).toBeUndefined();
    });

    it('includes the selected category in the payload', async () => {
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<FeatureRequestPanel onSubmit={onSubmit} isSubmitting={false} />);
      fillForm('Dark mode toggle', 'Please add a dark mode.');
      fireEvent.change(screen.getByLabelText(/Category/), {
        target: { value: 'Improvement to Existing Feature' },
      });
      fireEvent.click(screen.getByRole('button', SUBMIT));

      await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
      expect(onSubmit.mock.calls[0][0].category).toBe('Improvement to Existing Feature');
    });

    it('trims title and description in the submitted payload', async () => {
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<FeatureRequestPanel onSubmit={onSubmit} isSubmitting={false} />);
      fillForm('  Padded title  ', '  Padded body  ');
      fireEvent.click(screen.getByRole('button', SUBMIT));

      await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
      const payload = onSubmit.mock.calls[0][0];
      expect(payload.title).toBe('Padded title');
      expect(payload.description).toBe('Padded body');
    });
  });

  describe('submitting state', () => {
    it('disables the fields, category, and button while isSubmitting', () => {
      render(<FeatureRequestPanel onSubmit={vi.fn()} isSubmitting={true} />);
      expect(screen.getByLabelText(/Title/)).toBeDisabled();
      expect(screen.getByLabelText(/What would you like/)).toBeDisabled();
      expect(screen.getByLabelText(/Category/)).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Submitting…' })).toBeDisabled();
    });

    it('does not call onSubmit while a submit is already in flight', () => {
      const onSubmit = vi.fn().mockResolvedValue(undefined);
      render(<FeatureRequestPanel onSubmit={onSubmit} isSubmitting={true} />);
      const form = screen.getByRole('button', { name: 'Submitting…' }).closest('form');
      if (form) fireEvent.submit(form);
      expect(onSubmit).not.toHaveBeenCalled();
    });
  });
});
