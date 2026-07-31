import { render, screen, fireEvent, waitFor, userEvent } from '../../../test-utils';
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import type { AgeStatus } from '@/renderer/hooks/useAgeStatus';
import { useSettingsStore } from '@/renderer/stores/settingsStore';

const { mockSubmit, ageStatusRef } = vi.hoisted(() => ({
  mockSubmit: vi.fn(),
  // Mutable holder so individual tests can swap the AgeStatus union variant.
  // Default 'unverified' → the DOB form renders (what most tests below exercise).
  ageStatusRef: { current: { state: 'unverified' } as AgeStatus },
}));

// Module-boundary mock (NOT a fetch mock — satisfies [internal]rules/tests.md). The
// service has its own tests; here we isolate the component. evaluateAge is a SEPARATE
// module and stays REAL so the outcome-branch logic is exercised end-to-end. The
// useAgeStatus hook has its own MSW-backed tests; the rehydrate path is covered
// end-to-end in NsfwContentGate.verifiedStatePersists.test.tsx (#1763).
vi.mock('@/renderer/services/ageClaim/ageClaimService', () => ({
  submitSignedAgeClaim: (input: unknown) => mockSubmit(input),
}));
vi.mock('@/renderer/hooks/useAgeStatus', () => ({
  useAgeStatus: () => ageStatusRef.current,
}));

import NsfwContentGate from '@/renderer/components/Settings/NsfwContentGate';

const FUTURE_COPY =
  'This saves your preference for future NSFW-marked channels. NSFW-marked channels are not available yet.';

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
    ageStatusRef.current = { state: 'unverified' };
    mockSubmit.mockReset();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it.each([
    [{ state: 'loading' }, 'Checking your verification status…'],
    [{ state: 'unverified' }, 'Verify your age before you can change this preference.'],
    [
      { state: 'verified', validAge: false, nsfwAuth: false },
      "Age verification did not meet Concord's minimum age requirement.",
    ],
    [
      { state: 'verified', validAge: true, nsfwAuth: false },
      'Age verified · Not eligible for NSFW content',
    ],
  ] as const)('keeps the preference disabled for %o', (ageStatus, copy) => {
    ageStatusRef.current = ageStatus;
    render(<NsfwContentGate />);

    expect(screen.getByText(copy)).toBeInTheDocument();
    expect(screen.getByText(FUTURE_COPY)).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).not.toBeChecked();

    if (ageStatus.state === 'unverified') {
      expect(screen.getByRole('spinbutton', { name: /year/i })).toBeInTheDocument();
    } else {
      expect(screen.queryByRole('spinbutton', { name: /year/i })).not.toBeInTheDocument();
    }
  });

  // The switch is aria-disabled (focusable, so AT can reach the explanation) rather than
  // HTML-disabled, so the browser no longer blocks the click. Enforcement moved to the
  // component's handler guard — this proves the guard, not the attribute, is what holds.
  it('refuses to store the preference when the user is not age-eligible', async () => {
    ageStatusRef.current = { state: 'verified', validAge: true, nsfwAuth: false };
    render(<NsfwContentGate />);

    const toggle = screen.getByRole('switch', { name: 'Allow NSFW content' });
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    // Reachable by keyboard/AT — the whole point of not using HTML `disabled`.
    expect(toggle).toBeEnabled();

    await userEvent.click(toggle);

    expect(useSettingsStore.getState().allowNsfwContent).toBe(false);
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).not.toBeChecked();
  });

  it('explains why a verified under-18 user cannot opt in', () => {
    ageStatusRef.current = { state: 'verified', validAge: true, nsfwAuth: false };
    render(<NsfwContentGate />);

    expect(screen.getByText('Age verified · Not eligible for NSFW content')).toBeInTheDocument();
    expect(
      screen.getByText('You must be 18 or older to enable this preference.')
    ).toBeInTheDocument();
  });

  it('keeps every switch description target mounted', () => {
    render(<NsfwContentGate />);

    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toHaveAttribute(
      'aria-describedby',
      'allow-nsfw-content-status allow-nsfw-content-reason allow-nsfw-content-future'
    );
    expect(document.getElementById('allow-nsfw-content-status')).toBeInTheDocument();
    expect(document.getElementById('allow-nsfw-content-reason')).toBeInTheDocument();
    expect(document.getElementById('allow-nsfw-content-future')).toBeInTheDocument();
  });

  it('stores a verified adult explicit opt-in', async () => {
    const user = userEvent.setup();
    ageStatusRef.current = { state: 'verified', validAge: true, nsfwAuth: true };
    render(<NsfwContentGate />);

    expect(screen.getByText('Age verified · Eligible for NSFW content')).toBeInTheDocument();
    expect(screen.getByText(FUTURE_COPY)).toBeInTheDocument();
    const preference = screen.getByRole('switch', { name: 'Allow NSFW content' });
    expect(preference).toBeEnabled();
    expect(preference).not.toBeChecked();
    expect(screen.queryByRole('spinbutton', { name: /year/i })).not.toBeInTheDocument();

    await user.click(preference);

    expect(preference).toBeChecked();
    expect(useSettingsStore.getState().allowNsfwContent).toBe(true);
  });

  it('masks stored intent while ineligible and restores it when eligibility returns', () => {
    useSettingsStore.getState().setAllowNsfwContent(true);
    ageStatusRef.current = { state: 'verified', validAge: true, nsfwAuth: true };
    const { rerender } = render(<NsfwContentGate />);

    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toBeChecked();

    ageStatusRef.current = { state: 'unverified' };
    rerender(<NsfwContentGate />);

    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).not.toBeChecked();
    expect(useSettingsStore.getState().allowNsfwContent).toBe(true);

    ageStatusRef.current = { state: 'verified', validAge: true, nsfwAuth: true };
    rerender(<NsfwContentGate />);

    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toBeChecked();
    expect(useSettingsStore.getState().allowNsfwContent).toBe(true);
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
    expect(screen.getByText(FUTURE_COPY)).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    expect(screen.queryByRole('spinbutton', { name: /year/i })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /cancel/i }));
    expect(mockSubmit).not.toHaveBeenCalled();
    expect(screen.getByRole('spinbutton', { name: /year/i })).toHaveValue(2000); // retained
  });

  it('enables the preference after adult verification without automatically opting in', async () => {
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
    expect(await screen.findByText('Age verified · Eligible for NSFW content')).toBeInTheDocument();
    expect(screen.getByText(FUTURE_COPY)).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toHaveAttribute(
      'aria-disabled',
      'false'
    );
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).not.toBeChecked();
    expect(useSettingsStore.getState().allowNsfwContent).toBe(false);
    expect(screen.queryByRole('spinbutton', { name: /year/i })).not.toBeInTheDocument();
  });

  it('moves focus to the persistent status output through the signed terminal outcome', async () => {
    const user = userEvent.setup();
    type AdultVerdict = { ok: true; validAge: true; nsfwAuth: true };
    let resolveSubmit!: (verdict: AdultVerdict) => void;
    mockSubmit.mockReturnValue(
      new Promise<AdultVerdict>((resolve) => {
        resolveSubmit = resolve;
      })
    );
    render(<NsfwContentGate />);

    await user.type(screen.getByRole('spinbutton', { name: /year/i }), '2000');
    await user.type(screen.getByRole('spinbutton', { name: /month/i }), '1');
    await user.type(screen.getByRole('spinbutton', { name: /day/i }), '1');
    await user.click(screen.getByRole('button', { name: /verify age/i }));
    await user.click(screen.getByRole('button', { name: /submit/i }));

    const statusOutput = screen.getByRole('status');
    expect(statusOutput).toHaveTextContent('Submitting age verification…');
    expect.soft(statusOutput).toHaveAttribute('tabindex', '-1');
    expect.soft(statusOutput).toHaveFocus();

    resolveSubmit({ ok: true, validAge: true, nsfwAuth: true });

    await waitFor(() =>
      expect(statusOutput).toHaveTextContent('Age verified · Eligible for NSFW content')
    );
    expect(screen.getByRole('status')).toBe(statusOutput);
    expect(statusOutput).toHaveFocus();
  });

  it('shows verified-but-locked for a 16–17 year old', async () => {
    mockSubmit.mockResolvedValue({ ok: true, validAge: true, nsfwAuth: false });
    render(<NsfwContentGate />);
    enterDob('2009', '1', '1'); // age 17 at 2026-06-20
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));
    expect(
      await screen.findByText('Age verified · Not eligible for NSFW content')
    ).toBeInTheDocument();
    expect(
      screen.getByText('You must be 18 or older to enable this preference.')
    ).toBeInTheDocument();
    expect(screen.getByText(FUTURE_COPY)).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).not.toBeChecked();
    expect(screen.queryByRole('spinbutton', { name: /year/i })).not.toBeInTheDocument();
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
    expect(screen.getByText(FUTURE_COPY)).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).not.toBeChecked();
    expect(screen.queryByRole('spinbutton', { name: /year/i })).not.toBeInTheDocument();
  });

  it('shows the disabled screen on the account_disabled re-submit edge case', async () => {
    mockSubmit.mockResolvedValue({ ok: false, code: 'account_disabled' });
    render(<NsfwContentGate />);
    enterDob('2000', '1', '1'); // adult, but server says already disabled
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/disabled/i);
    expect(screen.getByText(FUTURE_COPY)).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).not.toBeChecked();
    expect(screen.queryByRole('spinbutton', { name: /year/i })).not.toBeInTheDocument();
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
    expect(screen.getByText(FUTURE_COPY)).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    expect(screen.getByRole('switch', { name: 'Allow NSFW content' })).not.toBeChecked();
  });

  it('never writes the raw DOB to web storage (submit path actually exercised)', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    mockSubmit.mockResolvedValue({ ok: true, validAge: true, nsfwAuth: true });
    render(<NsfwContentGate />);
    enterDob('2000', '7', '4');
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));
    await screen.findByText('Age verified · Eligible for NSFW content');
    // Positive precondition so the no-write assertion is not vacuous: the submit ran.
    expect(mockSubmit).toHaveBeenCalledTimes(1);
    const wrote = setItem.mock.calls.flat().join('|');
    expect(wrote).not.toContain('2000');
    expect(wrote).not.toContain('-07-04');
  });
});
