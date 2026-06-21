import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';

const { mockSubmit, mockStatus } = vi.hoisted(() => ({
  mockSubmit: vi.fn(),
  mockStatus: { nsfwAuth: 'unknown' as boolean | 'unknown' },
}));

// Module-boundary mock (NOT a fetch mock — satisfies [internal]rules/tests.md). The
// service has its own tests; here we isolate the component. evaluateAge is a SEPARATE
// module and stays REAL so the outcome-branch logic is exercised end-to-end.
vi.mock('@/renderer/services/ageClaim/ageClaimService', () => ({
  submitSignedAgeClaim: (input: unknown) => mockSubmit(input),
}));
vi.mock('@/renderer/hooks/useAgeStatus', () => ({
  useAgeStatus: () => mockStatus,
}));

import NsfwContentGate from '@/renderer/components/Settings/NsfwContentGate';

function enterDob(year: string, month: string, day: string) {
  fireEvent.change(screen.getByRole('spinbutton', { name: /year/i }), { target: { value: year } });
  fireEvent.change(screen.getByRole('spinbutton', { name: /month/i }), {
    target: { value: month },
  });
  fireEvent.change(screen.getByRole('spinbutton', { name: /day/i }), { target: { value: day } });
}

describe('NsfwContentGate', () => {
  beforeEach(() => {
    // Fake ONLY Date so new Date() is deterministic; leave timers/microtasks real so
    // async/await + waitFor work normally.
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-06-20T00:00:00Z'));
    resetAllStores();
    mockStatus.nsfwAuth = 'unknown';
    mockSubmit.mockReset();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('skips the gate when nsfw_auth is already satisfied', () => {
    mockStatus.nsfwAuth = true;
    render(<NsfwContentGate />);
    expect(screen.getByText(/already verified/i)).toBeInTheDocument();
    expect(screen.queryByRole('spinbutton', { name: /year/i })).not.toBeInTheDocument();
  });

  it('disables submit until a valid, non-future, real date is entered', () => {
    render(<NsfwContentGate />);
    const verify = () => screen.getByRole('button', { name: /verify age/i });
    expect(verify()).toBeDisabled(); // empty
    enterDob('2007', '2', '31'); // impossible (Feb 31)
    expect(verify()).toBeDisabled();
    enterDob('2099', '1', '1'); // future
    expect(verify()).toBeDisabled();
    enterDob('2000', '1', '1'); // valid adult
    expect(verify()).toBeEnabled();
  });

  it('rejects an in-year future date (exercises the intra-year future guard)', () => {
    render(<NsfwContentGate />);
    enterDob('2026', '12', '31'); // same year as now (2026-06-20) but in the future
    expect(screen.getByRole('button', { name: /verify age/i })).toBeDisabled();
  });

  it('echoes the entered date on the confirm step and cancels without submitting', () => {
    render(<NsfwContentGate />);
    enterDob('2000', '3', '5');
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    expect(screen.getByText('2000-03-05')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /cancel/i }));
    expect(mockSubmit).not.toHaveBeenCalled();
    expect(screen.getByRole('spinbutton', { name: /year/i })).toHaveValue(2000); // retained
  });

  it('unlocks NSFW for an adult (>=18) and submits the exact birthdate signal', async () => {
    mockSubmit.mockResolvedValue({ ok: true, validAge: true, nsfwAuth: true });
    render(<NsfwContentGate />);
    enterDob('2000', '1', '1');
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));
    await waitFor(() =>
      expect(mockSubmit).toHaveBeenCalledWith({
        signal: { kind: 'birthdate', year: 2000, month: 1, day: 1 },
      })
    );
    expect(await screen.findByText(/now enabled/i)).toBeInTheDocument();
  });

  it('shows verified-but-locked for a 16–17 year old', async () => {
    mockSubmit.mockResolvedValue({ ok: true, validAge: true, nsfwAuth: false });
    render(<NsfwContentGate />);
    enterDob('2009', '1', '1'); // age 17 at 2026-06-20
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));
    expect(await screen.findByText(/remains locked/i)).toBeInTheDocument();
  });

  it('shows the disabled terminal screen for a sub-16 user (service returns validAge=false)', async () => {
    // First-time disable: server returns 200 + side-effect disable; the service surfaces the
    // signed verdict (validAge=false), NOT an account_disabled code.
    mockSubmit.mockResolvedValue({ ok: true, validAge: false, nsfwAuth: false });
    render(<NsfwContentGate />);
    enterDob('2015', '1', '1'); // a sub-16 birthdate
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/disabled/i);
  });

  it('shows the disabled screen on the account_disabled re-submit edge case', async () => {
    mockSubmit.mockResolvedValue({ ok: false, code: 'account_disabled' });
    render(<NsfwContentGate />);
    enterDob('2000', '1', '1'); // adult, but server says already disabled
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/disabled/i);
  });

  it('surfaces a retryable error and re-renders the form on a non-disable failure', async () => {
    mockSubmit.mockResolvedValue({ ok: false, code: 'unavailable' });
    render(<NsfwContentGate />);
    enterDob('2000', '1', '1');
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));
    expect(await screen.findByText(/couldn't reach the server/i)).toBeInTheDocument();
    const year = screen.getByRole('spinbutton', { name: /year/i });
    expect(year).toBeInTheDocument();
    expect(year).toHaveValue(null); // DOB cleared after submit, even on error (privacy)
  });

  it('never writes the raw DOB to web storage (submit path actually exercised)', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    mockSubmit.mockResolvedValue({ ok: true, validAge: true, nsfwAuth: true });
    render(<NsfwContentGate />);
    enterDob('2000', '7', '4');
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));
    await screen.findByText(/now enabled/i);
    // Positive precondition so the no-write assertion is not vacuous: the submit ran.
    expect(mockSubmit).toHaveBeenCalledTimes(1);
    const wrote = setItem.mock.calls.flat().join('|');
    expect(wrote).not.toContain('2000');
    expect(wrote).not.toContain('-07-04');
  });
});
