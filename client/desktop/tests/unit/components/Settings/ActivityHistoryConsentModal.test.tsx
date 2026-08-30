import { useState } from 'react';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import ActivityHistoryConsentModal from '@/renderer/components/Settings/ActivityHistoryConsentModal';
import type { PresenceHistoryRequiredConsent } from '@/renderer/services/system/presenceHistoryService';
import { vi } from 'vitest';

const FIRST_HASH = 'a'.repeat(64);
const UPDATED_HASH = 'b'.repeat(64);

const DISCLOSURE: PresenceHistoryRequiredConsent = {
  version: 4,
  copyHash: FIRST_HASH,
  operatorName: 'Concord Voice, Inc.',
  requiredText:
    'Activity History stores your Custom Status on this server so you can review it later.',
  details: [
    'Visibility settings still control who can see your current status.',
    'No prior activity is backfilled when you enable history.',
    'Expired records are removed from the API immediately.',
  ],
  privacyPolicyUrl: 'https://concordvoice.com/privacy#activity-history',
  acknowledgementLabel:
    'I understand and consent to server-readable Activity History under the terms above.',
};

const UPDATED_DISCLOSURE: PresenceHistoryRequiredConsent = {
  ...DISCLOSURE,
  version: 5,
  copyHash: UPDATED_HASH,
  requiredText: 'Updated Activity History terms apply to future Custom Status changes.',
};

interface RenderConsentOptions {
  mode?: 'enable' | 'reconsent';
  disclosure?: PresenceHistoryRequiredConsent;
  retentionDays?: 7 | 30 | 90 | 365;
  onClose?: () => void;
  onSubmit?: ReturnType<typeof vi.fn>;
}

function renderConsent({
  mode = 'enable',
  disclosure = DISCLOSURE,
  retentionDays = 30,
  onClose = vi.fn(),
  onSubmit = vi.fn().mockResolvedValue(undefined),
}: RenderConsentOptions = {}) {
  return render(
    <ActivityHistoryConsentModal
      isOpen={true}
      mode={mode}
      disclosure={disclosure}
      retentionDays={retentionDays}
      onClose={onClose}
      onSubmit={onSubmit}
    />
  );
}

describe('ActivityHistoryConsentModal', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders every disclosure field verbatim with associated native controls', () => {
    renderConsent();

    expect(screen.getByRole('dialog', { name: 'Enable Activity History' })).toBeInTheDocument();
    expect(screen.getByText(DISCLOSURE.operatorName)).toBeInTheDocument();
    expect(screen.getByText(DISCLOSURE.requiredText)).toBeInTheDocument();
    for (const detail of DISCLOSURE.details) {
      expect(screen.getByText(detail)).toBeInTheDocument();
    }

    const policyLink = screen.getByRole('link', {
      name: `${DISCLOSURE.operatorName} privacy policy`,
    });
    expect(policyLink).toHaveAttribute('href', DISCLOSURE.privacyPolicyUrl);
    expect(policyLink).toHaveAttribute('target', '_blank');

    const acknowledgement = screen.getByRole('checkbox', {
      name: DISCLOSURE.acknowledgementLabel,
    });
    expect(acknowledgement).not.toBeChecked();
    expect(acknowledgement).toHaveAttribute('type', 'checkbox');

    const retention = screen.getByRole('combobox', { name: 'Keep Activity History for' });
    expect(retention).toHaveValue('30');
    expect(retention).toHaveDisplayValue('30 days');
    expect(screen.getByRole('button', { name: 'Enable Activity History' })).toBeDisabled();
  });

  it('submits the selected retention and exact disclosure identity only after acknowledgement', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    renderConsent({ onSubmit });

    await user.selectOptions(
      screen.getByRole('combobox', { name: 'Keep Activity History for' }),
      '90'
    );
    await user.click(screen.getByRole('checkbox', { name: DISCLOSURE.acknowledgementLabel }));
    await user.click(screen.getByRole('button', { name: 'Enable Activity History' }));

    expect(onSubmit).toHaveBeenCalledWith({
      retentionDays: 90,
      consentVersion: DISCLOSURE.version,
      consentCopyHash: DISCLOSURE.copyHash,
    });
  });

  it('uses re-consent language without replacing the server disclosure', () => {
    renderConsent({ mode: 'reconsent', disclosure: UPDATED_DISCLOSURE, retentionDays: 7 });

    expect(
      screen.getByRole('dialog', { name: 'Review updated Activity History terms' })
    ).toBeInTheDocument();
    expect(screen.getByText(UPDATED_DISCLOSURE.requiredText)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Accept updated terms' })).toBeDisabled();
    expect(screen.getByRole('combobox', { name: 'Keep Activity History for' })).toHaveValue('7');
  });

  it('does not close optimistically and exposes a submission failure for retry', async () => {
    const user = userEvent.setup();
    let rejectSubmission: ((error: Error) => void) | undefined;
    const onSubmit = vi.fn(
      () =>
        new Promise<void>((_resolve, reject) => {
          rejectSubmission = reject;
        })
    );
    const onClose = vi.fn();
    renderConsent({ onSubmit, onClose });

    await user.click(screen.getByRole('checkbox', { name: DISCLOSURE.acknowledgementLabel }));
    await user.click(screen.getByRole('button', { name: 'Enable Activity History' }));

    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Enabling Activity History…' })).toBeDisabled();

    rejectSubmission?.(new Error('The server could not save Activity History settings.'));

    expect(
      await screen.findByRole('alert', {
        name: 'Activity History settings could not be saved. Try again.',
      })
    ).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Enable Activity History' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Enable Activity History' })).toBeEnabled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('cancels through Modal and restores focus to the invoking control', async () => {
    const user = userEvent.setup();

    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>
            Turn on Activity History
          </button>
          <ActivityHistoryConsentModal
            isOpen={open}
            mode="enable"
            disclosure={DISCLOSURE}
            retentionDays={30}
            onClose={() => setOpen(false)}
            onSubmit={vi.fn().mockResolvedValue(undefined)}
          />
        </>
      );
    }

    render(<Harness />);
    const invoker = screen.getByRole('button', { name: 'Turn on Activity History' });
    await user.click(invoker);

    expect(screen.getByRole('heading', { name: DISCLOSURE.requiredText })).toHaveFocus();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(invoker).toHaveFocus();
  });

  it('resets acknowledgement and returns focus to the disclosure heading when terms change', async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <ActivityHistoryConsentModal
        isOpen={true}
        mode="reconsent"
        disclosure={DISCLOSURE}
        retentionDays={30}
        onClose={vi.fn()}
        onSubmit={vi.fn().mockResolvedValue(undefined)}
      />
    );

    const acknowledgement = screen.getByRole('checkbox', {
      name: DISCLOSURE.acknowledgementLabel,
    });
    await user.click(acknowledgement);
    expect(acknowledgement).toBeChecked();
    screen.getByRole('combobox', { name: 'Keep Activity History for' }).focus();

    rerender(
      <ActivityHistoryConsentModal
        isOpen={true}
        mode="reconsent"
        disclosure={UPDATED_DISCLOSURE}
        retentionDays={30}
        onClose={vi.fn()}
        onSubmit={vi.fn().mockResolvedValue(undefined)}
      />
    );

    await waitFor(() => {
      expect(
        screen.getByRole('checkbox', { name: UPDATED_DISCLOSURE.acknowledgementLabel })
      ).not.toBeChecked();
      expect(screen.getByRole('heading', { name: UPDATED_DISCLOSURE.requiredText })).toHaveFocus();
    });
  });
});
