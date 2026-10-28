import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { act, render, screen, userEvent, waitFor, within } from '../../../../test-utils';
import { resetAllStores } from '../../../../helpers/store-helpers';
import WingSeatPanel from '@/renderer/components/Settings/subscription/WingSeatPanel';
import SeatInviteCard from '@/renderer/components/Settings/subscription/SeatInviteCard';
import PlanCard from '@/renderer/components/Settings/subscription/PlanCard';
import { formatExpiry } from '@/renderer/components/Settings/subscription/subscriptionCopy';
import { useBillingStore, type SeatResult } from '@/renderer/stores/auth/billingStore';
import { useFriendStore, type Friend } from '@/renderer/stores/chat/friendStore';
import type { SubscriptionInfo } from '@/renderer/hooks/ui/useSubscription';

// Wing seat UI (#2033 PR-5, spec §13): the payer's well, the +1's invite card
// and P9 on the plan card.

const ORIGINAL = useBillingStore.getState();
const EXPIRES = '2026-10-31T12:00:00Z';
const NEXT_SWAP = new Date(Date.now() + 10 * 86_400_000).toISOString();

const friend = (userId: string, displayName: string): Friend => ({
  id: `f-${userId}`,
  userId,
  username: displayName.toLowerCase(),
  displayName,
  status: 'online',
});

let inviteSeat: Mock<(u: string, r: boolean) => Promise<SeatResult>>;
let removeSeat: Mock<() => Promise<SeatResult>>;
let leaveSeat: Mock<() => Promise<SeatResult>>;
let acceptInvite: Mock<(id: string) => Promise<SeatResult>>;
let declineInvite: Mock<(id: string) => Promise<SeatResult>>;

beforeEach(() => {
  resetAllStores();
  inviteSeat = vi.fn().mockResolvedValue({ ok: true });
  removeSeat = vi.fn().mockResolvedValue({ ok: true });
  leaveSeat = vi.fn().mockResolvedValue({ ok: true });
  acceptInvite = vi.fn().mockResolvedValue({ ok: true });
  declineInvite = vi.fn().mockResolvedValue({ ok: true });
  useBillingStore.setState({ inviteSeat, removeSeat, leaveSeat, acceptInvite, declineInvite });
  useFriendStore.setState({ friends: [friend('u-ada', 'Ada'), friend('u-bo', 'Bo')] });
});

afterEach(() => {
  useBillingStore.setState({
    inviteSeat: ORIGINAL.inviteSeat,
    removeSeat: ORIGINAL.removeSeat,
    leaveSeat: ORIGINAL.leaveSeat,
    acceptInvite: ORIGINAL.acceptInvite,
    declineInvite: ORIGINAL.declineInvite,
  });
});

describe('WingSeatPanel', () => {
  it('invites a picked friend from an empty well', async () => {
    const user = userEvent.setup();
    render(<WingSeatPanel seat={{ role: 'payer' }} />);

    await user.click(screen.getByRole('button', { name: 'Invite a friend' }));
    await user.click(screen.getByRole('radio', { name: 'Bo' }));
    await user.click(screen.getByRole('button', { name: 'Send invite' }));

    expect(inviteSeat).toHaveBeenCalledWith('u-bo', false);
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('sends nothing until a friend is picked', async () => {
    const user = userEvent.setup();
    render(<WingSeatPanel seat={{ role: 'payer' }} />);
    await user.click(screen.getByRole('button', { name: 'Invite a friend' }));
    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    expect(inviteSeat).not.toHaveBeenCalled();
  });

  it('shows one refusal for any ineligible friend, and the date for a cooldown', async () => {
    const user = userEvent.setup();
    inviteSeat
      .mockResolvedValueOnce({ ok: false, error: 'invitee_not_eligible' })
      .mockResolvedValueOnce({ ok: false, error: 'seat_cooldown', date: NEXT_SWAP });
    render(<WingSeatPanel seat={{ role: 'payer' }} />);

    await user.click(screen.getByRole('button', { name: 'Invite a friend' }));
    await user.click(screen.getByRole('radio', { name: 'Ada' }));
    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/still friends/);

    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(formatExpiry(NEXT_SWAP) as string);
  });

  it('asks the payer to add a friend when the list is empty', () => {
    useFriendStore.setState({ friends: [] });
    render(<WingSeatPanel seat={{ role: 'payer' }} />);
    expect(screen.getByText(/Add a friend first/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Invite a friend' })).not.toBeInTheDocument();
  });

  it('withdraws a pending invite', async () => {
    const user = userEvent.setup();
    render(
      <WingSeatPanel
        seat={{
          role: 'payer',
          seat_id: 's-1',
          state: 'pending',
          counterpart_user_id: 'u-ada',
          invite_expires_at: EXPIRES,
        }}
      />
    );
    expect(screen.getByText(/Invite sent to Ada/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Withdraw invite' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(
      Array.from(dialog.querySelectorAll('button')).find(
        (b) => b.textContent === 'Withdraw invite'
      ) as HTMLElement
    );
    expect(removeSeat).toHaveBeenCalledTimes(1);
  });

  it('removes an active +1, starting on Keep', async () => {
    const user = userEvent.setup();
    render(
      <WingSeatPanel
        seat={{ role: 'payer', seat_id: 's-1', state: 'active', counterpart_user_id: 'u-ada' }}
      />
    );
    expect(screen.getByText('Ada has Supersonic through your Wing.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Remove' }));
    const keep = await screen.findByRole('button', { name: 'Keep Ada' });
    expect(keep).toHaveFocus();
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(removeSeat).toHaveBeenCalledTimes(1);
  });

  it('keeps the dialog open with an error when Remove fails', async () => {
    const user = userEvent.setup();
    removeSeat.mockResolvedValue({ ok: false, error: 'failed' });
    render(
      <WingSeatPanel
        seat={{ role: 'payer', seat_id: 's-1', state: 'active', counterpart_user_id: 'u-ada' }}
      />
    );
    await user.click(screen.getByRole('button', { name: 'Remove' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(
      Array.from(dialog.querySelectorAll('button')).find(
        (b) => b.textContent === 'Remove'
      ) as HTMLElement
    );
    expect(await screen.findByRole('alert')).toHaveTextContent(/Couldn't update your Wing/);
  });

  it('blocks Change +1 during the cooldown and says when it ends', async () => {
    const user = userEvent.setup();
    render(
      <WingSeatPanel
        seat={{
          role: 'payer',
          seat_id: 's-1',
          state: 'active',
          counterpart_user_id: 'u-ada',
          next_swap_at: NEXT_SWAP,
        }}
      />
    );
    const change = screen.getByRole('button', { name: 'Change +1' });
    expect(change).toHaveAttribute('aria-disabled', 'true');
    expect(change).toHaveAccessibleDescription(
      `You can change your +1 again on ${formatExpiry(NEXT_SWAP)}.`
    );
    await user.click(change);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('lifts the Change +1 block when the cooldown ends', () => {
    vi.useFakeTimers();
    try {
      const fetchMe = vi.fn().mockResolvedValue(undefined);
      useBillingStore.setState({ fetchMe });
      const swapAt = new Date(Date.now() + 60_000).toISOString();
      render(
        <WingSeatPanel
          seat={{
            role: 'payer',
            seat_id: 's-1',
            state: 'active',
            counterpart_user_id: 'u-ada',
            next_swap_at: swapAt,
          }}
        />
      );
      const change = screen.getByRole('button', { name: 'Change +1' });
      expect(change).toHaveAttribute('aria-disabled', 'true');
      act(() => {
        vi.advanceTimersByTime(60_001);
      });
      expect(fetchMe).toHaveBeenCalledTimes(1);
      expect(change).toHaveAttribute('aria-disabled', 'false');
      useBillingStore.setState({ fetchMe: ORIGINAL.fetchMe });
    } finally {
      vi.useRealTimers();
    }
  });

  it('ignores a cooldown date already in the past', () => {
    render(
      <WingSeatPanel
        seat={{
          role: 'payer',
          seat_id: 's-1',
          state: 'active',
          counterpart_user_id: 'u-ada',
          next_swap_at: '2020-01-01T00:00:00Z',
        }}
      />
    );
    expect(screen.getByRole('button', { name: 'Change +1' })).toHaveAttribute(
      'aria-disabled',
      'false'
    );
  });

  it('Change +1 replaces the seat and leaves the current +1 off the list', async () => {
    const user = userEvent.setup();
    render(
      <WingSeatPanel
        seat={{ role: 'payer', seat_id: 's-1', state: 'active', counterpart_user_id: 'u-ada' }}
      />
    );
    await user.click(screen.getByRole('button', { name: 'Change +1' }));
    expect(screen.getByText(/can't change your \+1 again for 30 days/)).toBeInTheDocument();
    expect(screen.queryByRole('radio', { name: 'Ada' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('radio', { name: 'Bo' }));
    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    expect(inviteSeat).toHaveBeenCalledWith('u-bo', true);
  });

  it('offers someone else after an invite expired, and falls back to "your +1"', () => {
    render(
      <WingSeatPanel
        seat={{ role: 'payer', seat_id: 's-1', state: 'expired', counterpart_user_id: 'u-gone' }}
      />
    );
    expect(screen.getByText('Your invite to your +1 expired.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Invite someone else' })).toBeInTheDocument();
  });
});

describe('SeatInviteCard', () => {
  const invite = { invite_id: 'seat-1', payer_user_id: 'u-ada', expires_at: EXPIRES };

  it('accepts, waiting for the server', async () => {
    const user = userEvent.setup();
    render(<SeatInviteCard invite={invite} />);
    expect(screen.getByRole('region', { name: 'Wing invite' })).toHaveTextContent(
      'Ada invited you to their Wing.'
    );
    await user.click(screen.getByRole('button', { name: 'Accept' }));
    expect(acceptInvite).toHaveBeenCalledWith('seat-1');
  });

  it.each([
    ['invite_expired', undefined, 'This invite expired.'],
    ['invite_not_found', undefined, 'This invite is no longer available.'],
    ['already_seat_member', undefined, "You're already on a friend's Wing."],
    ['seat_cooldown', NEXT_SWAP, `Ada can't add a new +1 until ${formatExpiry(NEXT_SWAP)}.`],
    ['own_subscription_active', NEXT_SWAP, 'You already pay for Supersonic.'],
    ['failed', undefined, "Couldn't accept the invite. Try again."],
  ])('shows the %s refusal', async (error, date, copy) => {
    const user = userEvent.setup();
    acceptInvite.mockResolvedValue(date ? { ok: false, error, date } : { ok: false, error });
    render(<SeatInviteCard invite={invite} />);
    await user.click(screen.getByRole('button', { name: 'Accept' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(copy);
  });

  it('blocks Accept while the user pays for their own plan', async () => {
    const user = userEvent.setup();
    render(
      <SeatInviteCard
        invite={{ ...invite, blocked: 'own_subscription_active' }}
        ownPlanEnds={NEXT_SWAP}
      />
    );
    const accept = screen.getByRole('button', { name: 'Accept' });
    expect(accept).toHaveAttribute('aria-disabled', 'true');
    expect(accept).toHaveAccessibleDescription(
      `You already pay for Supersonic. You can accept after your plan ends on ${formatExpiry(NEXT_SWAP)}.`
    );
    await user.click(accept);
    expect(acceptInvite).not.toHaveBeenCalled();
  });

  it('declines optimistically', async () => {
    const user = userEvent.setup();
    render(<SeatInviteCard invite={invite} />);
    await user.click(screen.getByRole('button', { name: 'Decline' }));
    expect(screen.queryByRole('region', { name: 'Wing invite' })).not.toBeInTheDocument();
    expect(declineInvite).toHaveBeenCalledWith('seat-1');
  });

  it('brings the card back when Decline fails', async () => {
    const user = userEvent.setup();
    declineInvite.mockResolvedValue({ ok: false, error: 'failed' });
    render(<SeatInviteCard invite={invite} />);
    await user.click(screen.getByRole('button', { name: 'Decline' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "Couldn't decline the invite. Try again."
    );
    expect(screen.getByRole('region', { name: 'Wing invite' })).toBeInTheDocument();
  });

  it('falls back to "a friend" when the payer is not on the list', () => {
    render(<SeatInviteCard invite={{ ...invite, payer_user_id: 'u-gone' }} />);
    expect(screen.getByText(/a friend invited you to their Wing/)).toBeInTheDocument();
  });
});

describe('PlanCard P9 (Wing +1)', () => {
  const member = (overrides: Partial<SubscriptionInfo> = {}): SubscriptionInfo => ({
    tier: 'premium',
    status: 'active',
    source: 'seat',
    billing: 'available',
    seat: { role: 'member', seat_id: 's-1', state: 'active', counterpart_user_id: 'u-ada' },
    invites: [],
    loading: false,
    error: false,
    refetch: () => {},
    ...overrides,
  });

  it("names the payer's Wing and leaves after a confirm", async () => {
    const user = userEvent.setup();
    render(<PlanCard info={member()} />);
    expect(screen.getByText("Included with Ada's Wing")).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Leave Wing' }));
    expect(
      await screen.findByText("Leave Ada's Wing? You'll go back to Sonic right away.")
    ).toBeInTheDocument();
    const dialog = screen.getByRole('dialog');
    await user.click(
      Array.from(dialog.querySelectorAll('button')).find(
        (b) => b.textContent === 'Leave Wing'
      ) as HTMLElement
    );
    expect(leaveSeat).toHaveBeenCalledTimes(1);
  });

  it('keeps Leave Wing on a seat whose Wing is paused', () => {
    render(
      <PlanCard
        info={member({
          tier: 'free',
          source: undefined,
          seat: {
            role: 'member',
            seat_id: 's-1',
            state: 'active',
            counterpart_user_id: 'u-ada',
            suspended: true,
          },
        })}
      />
    );
    expect(screen.getByText("On hold until Ada's Wing is active again")).toBeInTheDocument();
    expect(screen.queryByText("Included with Ada's Wing")).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Leave Wing' })).toBeInTheDocument();
  });

  it('also shows a code that runs beside the seat', () => {
    render(<PlanCard info={member({ codeExpiry: EXPIRES })} />);
    expect(screen.getByText(/Also Supersonic from a code/)).toBeInTheDocument();
  });

  it("nests the payer's well under a live Wing", () => {
    render(
      <PlanCard
        info={member({
          source: 'stripe',
          seat: { role: 'payer' },
          stripe: {
            plan: 'wing',
            interval: 'year',
            status: 'active',
            current_period_end: EXPIRES,
            canceling: false,
            founders_rate: false,
          },
        })}
      />
    );
    expect(screen.getByRole('region', { name: 'Your +1' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Invite a friend' })).toBeInTheDocument();
  });
});
