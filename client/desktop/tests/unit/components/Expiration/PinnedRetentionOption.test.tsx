import { act, render, screen } from '../../../test-utils';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import type { ExpirationPolicy } from '@/renderer/services/messaging/expirationPolicyApi';
import PinnedRetentionOption, {
  isPinnedRetentionAvailable,
} from '@/renderer/components/Expiration/PinnedRetentionOption';

const policy: ExpirationPolicy = {
  windowSeconds: 86400,
  updatedAt: '2026-09-08T05:00:00Z',
  revision: 4,
  backfillPending: false,
  includePinned: false,
};

const capabilities = (supported: boolean | undefined) => ({
  auth: { oauthProviders: [] },
  features: supported === undefined ? {} : { expirationPinnedPolicySupported: supported },
});

beforeEach(() => {
  resetAllStores();
  useClientConfigStore.setState({ serverCapabilities: null });
});

describe('isPinnedRetentionAvailable (the render gate, #3459)', () => {
  const open = {
    policy,
    policyState: 'ready' as const,
    effectiveWindow: 86400 as const,
    capabilityUnsupported: false,
  };

  it('is available when all four conditions hold', () => {
    expect(isPinnedRetentionAvailable(open)).toBe(true);
  });

  // One cell per condition, each with everything ELSE satisfied, so the cell can fail for
  // exactly one reason and deleting that condition turns exactly that cell red.
  it.each([
    ['the policy is still loading', { policyState: 'loading' as const }],
    ['the policy is unavailable', { policyState: 'unavailable' as const }],
    ['the field is absent', { policy: { ...policy, includePinned: undefined } }],
    ['there is no policy at all', { policy: null }],
    ['the capability is confirmed unsupported', { capabilityUnsupported: true }],
    ['no window is shown (Off)', { effectiveWindow: null }],
    ['the window is not yet known', { effectiveWindow: undefined }],
  ])('is NOT available when %s', (_name, override) => {
    expect(isPinnedRetentionAvailable({ ...open, ...override })).toBe(false);
  });

  it('is available for either boolean value of the field', () => {
    expect(
      isPinnedRetentionAvailable({ ...open, policy: { ...policy, includePinned: true } })
    ).toBe(true);
  });
});

describe('PinnedRetentionOption', () => {
  const renderOption = (over: Partial<React.ComponentProps<typeof PinnedRetentionOption>> = {}) => {
    const onChange = vi.fn();
    render(
      <PinnedRetentionOption
        policy={policy}
        policyState="ready"
        effectiveWindow={86400}
        checked={false}
        locked={false}
        onChange={onChange}
        {...over}
      />
    );
    return onChange;
  };
  const checkbox = () => screen.queryByRole('checkbox', { name: 'Include pinned messages' });

  it('renders a native checkbox named "Include pinned messages" and reports a change', async () => {
    const onChange = renderOption();
    const box = checkbox();
    expect(box).toBeInTheDocument();
    expect(box?.tagName).toBe('INPUT');
    await userEvent.setup().click(box!);
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it('reflects its checked prop', () => {
    renderOption({ checked: true });
    expect(checkbox()).toBeChecked();
  });

  it('renders nothing when the gate fails', () => {
    renderOption({ policy: { ...policy, includePinned: undefined } });
    expect(checkbox()).not.toBeInTheDocument();
  });

  it('renders nothing for a CONFIRMED-unsupported server', () => {
    useClientConfigStore.setState({ serverCapabilities: capabilities(undefined) });
    renderOption();
    expect(checkbox()).not.toBeInTheDocument();
  });

  it('shows for a server that states support, and when the capabilities fetch has not answered', () => {
    useClientConfigStore.setState({ serverCapabilities: capabilities(true) });
    const first = render(
      <PinnedRetentionOption
        policy={policy}
        policyState="ready"
        effectiveWindow={86400}
        checked={false}
        locked={false}
        onChange={vi.fn()}
      />
    );
    expect(checkbox()).toBeInTheDocument();
    first.unmount();
    // null = loading or a failed fetch: not told, so the control stays available.
    act(() => useClientConfigStore.setState({ serverCapabilities: null }));
    renderOption();
    expect(checkbox()).toBeInTheDocument();
  });

  it('is aria-disabled, never natively disabled, when locked, and ignores a change', async () => {
    const onChange = renderOption({ locked: true, lockDescriptionId: 'why' });
    const box = checkbox()!;
    expect(box).toHaveAttribute('aria-disabled', 'true');
    expect(box).not.toBeDisabled();
    box.focus();
    expect(box).toHaveFocus();
    await userEvent.setup().click(box);
    expect(onChange).not.toHaveBeenCalled();
    expect(box).not.toBeChecked();
  });

  it('is described by its hint, and by the lock explanation too when locked', () => {
    render(
      <>
        <p id="why">Only moderators can change this timer.</p>
        <PinnedRetentionOption
          policy={policy}
          policyState="ready"
          effectiveWindow={86400}
          checked={false}
          locked
          lockDescriptionId="why"
          onChange={vi.fn()}
        />
      </>
    );
    expect(checkbox()).toHaveAccessibleDescription(
      /kept for as long as they stay pinned.*Only moderators can change this timer\./
    );
  });

  it('is not aria-disabled when editable', () => {
    renderOption();
    expect(checkbox()).toHaveAttribute('aria-disabled', 'false');
  });
});
