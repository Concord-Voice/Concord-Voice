import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen } from '../../test-utils';
import userEvent from '@testing-library/user-event';
import { resetAllStores } from '../../helpers/store-helpers';
import { ShareSegmentedControl } from '@/renderer/components/Voice/ShareSegmentedControl';

// TDD (red phase): ShareSegmentedControl.tsx does not exist yet. This test
// pins the interface it must satisfy — see
// [internal]skills/scaffold-component/SKILL.md and [internal]rules/tests.md.

describe('ShareSegmentedControl', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('exposes Switch and Stop as two buttons inside a "Screen share" group', () => {
    render(<ShareSegmentedControl onSwitch={vi.fn()} onStop={vi.fn()} />);

    const group = screen.getByRole('group', { name: 'Screen share' });
    expect(group).toBeInTheDocument();

    const switchBtn = screen.getByRole('button', { name: 'Switch' });
    const stopBtn = screen.getByRole('button', { name: 'Stop' });
    expect(group).toContainElement(switchBtn);
    expect(group).toContainElement(stopBtn);
  });

  it('clicking Switch calls onSwitch only — not onStop', async () => {
    const user = userEvent.setup();
    const onSwitch = vi.fn();
    const onStop = vi.fn();
    render(<ShareSegmentedControl onSwitch={onSwitch} onStop={onStop} />);

    await user.click(screen.getByRole('button', { name: 'Switch' }));

    expect(onSwitch).toHaveBeenCalledTimes(1);
    expect(onStop).not.toHaveBeenCalled();
  });

  it('clicking Stop calls onStop only — not onSwitch', async () => {
    const user = userEvent.setup();
    const onSwitch = vi.fn();
    const onStop = vi.fn();
    render(<ShareSegmentedControl onSwitch={onSwitch} onStop={onStop} />);

    await user.click(screen.getByRole('button', { name: 'Stop' }));

    expect(onStop).toHaveBeenCalledTimes(1);
    expect(onSwitch).not.toHaveBeenCalled();
  });

  it('both halves are independently reachable by Tab, in DOM order (Switch then Stop)', async () => {
    const user = userEvent.setup();
    render(<ShareSegmentedControl onSwitch={vi.fn()} onStop={vi.fn()} />);

    const switchBtn = screen.getByRole('button', { name: 'Switch' });
    const stopBtn = screen.getByRole('button', { name: 'Stop' });

    // Neither half starts focused.
    expect(switchBtn).not.toHaveFocus();
    expect(stopBtn).not.toHaveFocus();

    await user.tab();
    expect(switchBtn).toHaveFocus();

    await user.tab();
    expect(stopBtn).toHaveFocus();
  });

  it('Stop carries the tinted-destructive class; Switch does not', () => {
    render(<ShareSegmentedControl onSwitch={vi.fn()} onStop={vi.fn()} />);

    const switchBtn = screen.getByRole('button', { name: 'Switch' });
    const stopBtn = screen.getByRole('button', { name: 'Stop' });

    expect(stopBtn.className).toContain('voice-controls__btn--danger-soft');
    expect(switchBtn.className).not.toContain('voice-controls__btn--danger-soft');

    // Full class lists, per the interface contract.
    expect(switchBtn.className.split(' ').sort()).toEqual(
      ['voice-controls__btn', 'share-segmented__half', 'share-segmented__half--switch'].sort()
    );
    expect(stopBtn.className.split(' ').sort()).toEqual(
      [
        'voice-controls__btn',
        'voice-controls__btn--danger-soft',
        'share-segmented__half',
        'share-segmented__half--stop',
      ].sort()
    );
  });

  it('both halves are always enabled (share is live whenever this renders)', () => {
    render(<ShareSegmentedControl onSwitch={vi.fn()} onStop={vi.fn()} />);

    expect(screen.getByRole('button', { name: 'Switch' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Stop' })).toBeEnabled();
  });
});
