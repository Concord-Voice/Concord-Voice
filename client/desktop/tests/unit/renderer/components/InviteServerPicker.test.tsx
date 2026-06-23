import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, screen, fireEvent, act } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useServerStore } from '@/renderer/stores/serverStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { INVITE } from '@/renderer/utils/permissions';
import { InviteServerPicker } from '@/renderer/components/Chat/InviteServerPicker';

describe('InviteServerPicker', () => {
  beforeEach(() => {
    resetAllStores();
    useServerStore.setState({
      servers: [
        { id: 's1', name: 'Has Invite', icon_url: null },
        { id: 's2', name: 'No Invite', icon_url: null },
      ] as never,
    });
    usePermissionStore.setState({ serverPermissions: { s1: INVITE, s2: 0n } as never });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('lists only servers the user can invite to', () => {
    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);
    expect(screen.getByText('Has Invite')).toBeInTheDocument();
    expect(screen.queryByText('No Invite')).not.toBeInTheDocument();
  });

  it('renders through the shared context-menu layer at the requested viewport position', () => {
    const { container } = render(
      <InviteServerPicker position={{ x: 16, y: 32 }} onPick={vi.fn()} onClose={vi.fn()} />
    );

    expect(container.querySelector('.ctx-menu-overlay')).toBeInTheDocument();
    expect(container.querySelector('.ctx-menu')).toHaveStyle({ left: '16px', top: '32px' });
  });

  it('calls onPick with the chosen server id', () => {
    const onPick = vi.fn();
    render(<InviteServerPicker onPick={onPick} onClose={vi.fn()} />);
    fireEvent.click(screen.getByText('Has Invite'));
    expect(onPick).toHaveBeenCalledWith('s1');
  });

  it('shows an empty state when no server is invitable', () => {
    usePermissionStore.setState({ serverPermissions: { s1: 0n, s2: 0n } as never });
    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);
    expect(screen.getByText(/no servers you can invite to/i)).toBeInTheDocument();
  });

  it('calls onClose on Escape', () => {
    vi.useFakeTimers();
    const onClose = vi.fn();
    render(<InviteServerPicker onPick={vi.fn()} onClose={onClose} />);
    fireEvent.keyDown(document, { key: 'Escape' });
    act(() => {
      vi.advanceTimersByTime(200);
    });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
