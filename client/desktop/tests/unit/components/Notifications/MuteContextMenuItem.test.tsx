import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useNotificationPrefsStore } from '@/renderer/stores/notificationPrefsStore';
import ContextMenu from '@/renderer/components/ui/ContextMenu';
import MuteContextMenuItem from '@/renderer/components/Notifications/MuteContextMenuItem';

// Mock the service layer so the component's clicks fire predictable
// store updates without going to the network. The mock for setMutePreference
// reuses the store directly so the optimistic-update behavior the real
// service implements is preserved — tests then assert on the store rather
// than on whether the mock was called with the right args.
vi.mock('@/renderer/services/notificationPrefsService', async () => {
  const actual = await vi.importActual<
    typeof import('@/renderer/services/notificationPrefsService')
  >('@/renderer/services/notificationPrefsService');
  return {
    ...actual,
    setMutePreference: vi.fn(
      async (
        targetType: 'server' | 'channel' | 'dm',
        targetId: string,
        muted: boolean,
        mutedUntil: Date | null
      ) => {
        useNotificationPrefsStore.getState().setMute(targetType, targetId, muted, mutedUntil);
      }
    ),
  };
});

import { setMutePreference } from '@/renderer/services/notificationPrefsService';

const TARGET_ID = '11111111-1111-1111-1111-111111111111';

function renderItem(
  overrides: Partial<{
    targetType: 'server' | 'channel' | 'dm';
    kindLabel: string;
    onAction: () => void;
  }> = {}
) {
  const onAction = overrides.onAction ?? vi.fn();
  const ui = (
    <ContextMenu position={{ x: 10, y: 10 }} onClose={() => {}}>
      <MuteContextMenuItem
        targetType={overrides.targetType ?? 'server'}
        targetId={TARGET_ID}
        kindLabel={overrides.kindLabel ?? 'Server'}
        onAction={onAction}
      />
    </ContextMenu>
  );
  return { ...render(ui), onAction };
}

describe('MuteContextMenuItem', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders the "Mute <kind>" label when the target is unmuted', () => {
    renderItem({ kindLabel: 'Channel' });
    expect(screen.getByText('Mute Channel')).toBeInTheDocument();
    // No duration submenu visible until the trigger is clicked.
    expect(screen.queryByText('For 15 minutes')).not.toBeInTheDocument();
  });

  it('renders "Unmute <kind>" when the target is already muted', () => {
    // Seed the store with an active indefinite mute so the trigger reads as
    // already-muted on first render. This is the entry point for the
    // "click to unmute outright" path.
    useNotificationPrefsStore.getState().setMute('server', TARGET_ID, true, null);
    renderItem({ targetType: 'server', kindLabel: 'Server' });
    expect(screen.getByText('Unmute Server')).toBeInTheDocument();
  });

  it('opens the duration submenu when the unmuted trigger is clicked', () => {
    renderItem({ kindLabel: 'Server' });
    fireEvent.click(screen.getByText('Mute Server'));
    // All five durations should appear together — the picker is a single
    // submenu, not a progressive disclosure.
    expect(screen.getByText('For 15 minutes')).toBeInTheDocument();
    expect(screen.getByText('For 1 hour')).toBeInTheDocument();
    expect(screen.getByText('For 8 hours')).toBeInTheDocument();
    expect(screen.getByText('For 24 hours')).toBeInTheDocument();
    expect(screen.getByText('Until I turn it back on')).toBeInTheDocument();
  });

  it('fires setMutePreference with the right offset when a timed duration is picked', async () => {
    // Capture wall-clock at click time and assert mutedUntil falls in a
    // tight window around now + 1 hour. We deliberately don't fake timers
    // here — fake timers don't interact cleanly with RTL's act-based
    // re-render flushing for click handlers, and the helper under test
    // (mutedUntilFromDuration) is a pure offset against Date.now() so
    // real-clock assertion with a few-ms tolerance is enough.
    const before = Date.now();
    const { onAction } = renderItem({ targetType: 'channel', kindLabel: 'Channel' });
    fireEvent.click(screen.getByText('Mute Channel'));
    fireEvent.click(screen.getByText('For 1 hour'));

    await waitFor(() => {
      expect(setMutePreference).toHaveBeenCalledTimes(1);
    });
    const after = Date.now();
    const [type, id, muted, mutedUntil] = vi.mocked(setMutePreference).mock.calls[0];
    expect(type).toBe('channel');
    expect(id).toBe(TARGET_ID);
    expect(muted).toBe(true);
    // mutedUntil = clickTime + 3600000ms. clickTime is somewhere in
    // [before, after], so mutedUntil is in [before + 1h, after + 1h].
    const ts = (mutedUntil as Date).getTime();
    expect(ts).toBeGreaterThanOrEqual(before + 3_600_000);
    expect(ts).toBeLessThanOrEqual(after + 3_600_000);
    expect(onAction).toHaveBeenCalled();
  });

  it('passes null for mutedUntil when "indefinite" is picked', async () => {
    const { onAction } = renderItem({ targetType: 'dm', kindLabel: 'Conversation' });
    fireEvent.click(screen.getByText('Mute Conversation'));
    fireEvent.click(screen.getByText('Until I turn it back on'));

    await waitFor(() => {
      expect(setMutePreference).toHaveBeenCalledTimes(1);
    });
    const [type, id, muted, mutedUntil] = vi.mocked(setMutePreference).mock.calls[0];
    expect(type).toBe('dm');
    expect(id).toBe(TARGET_ID);
    expect(muted).toBe(true);
    // Indefinite mute = explicit null, not the absence of an arg. The store
    // distinguishes "this mute never expires" from "no entry exists."
    expect(mutedUntil).toBeNull();
    expect(onAction).toHaveBeenCalled();
  });

  it('unmute click fires setMutePreference(false, null) directly without a submenu', async () => {
    // Seed an active mute so the trigger is in unmute mode.
    useNotificationPrefsStore.getState().setMute('server', TARGET_ID, true, null);
    const { onAction } = renderItem({ targetType: 'server', kindLabel: 'Server' });

    fireEvent.click(screen.getByText('Unmute Server'));

    await waitFor(() => {
      expect(setMutePreference).toHaveBeenCalledTimes(1);
    });
    const [type, id, muted, mutedUntil] = vi.mocked(setMutePreference).mock.calls[0];
    // Unmute uses muted=false (NOT removeMute) so explicit-unmute rows
    // can defeat a parent server mute. mutedUntil is meaningless on an
    // unmuted row and must be null.
    expect(type).toBe('server');
    expect(id).toBe(TARGET_ID);
    expect(muted).toBe(false);
    expect(mutedUntil).toBeNull();
    expect(onAction).toHaveBeenCalled();
    // No submenu was opened — the unmute path is one-shot.
    expect(screen.queryByText('For 15 minutes')).not.toBeInTheDocument();
  });

  it('switches label live when the store transitions from unmuted to muted', () => {
    renderItem({ targetType: 'channel', kindLabel: 'Channel' });
    expect(screen.getByText('Mute Channel')).toBeInTheDocument();
    // Mutate the store directly (not via the trigger) to verify the
    // component is genuinely subscribed to the map, not relying on the
    // click handler to flip its internal state. Wrap in act() so React
    // flushes the resulting re-render before we assert.
    act(() => {
      useNotificationPrefsStore.getState().setMute('channel', TARGET_ID, true, null);
    });
    expect(screen.getByText('Unmute Channel')).toBeInTheDocument();
  });

  it('treats an expired timed mute as unmuted (Mute label shown)', () => {
    // A mute that already expired stays in the map until the sweep, but the
    // selector reports it as inactive. The trigger label must reflect that.
    const past = new Date(Date.now() - 60_000);
    useNotificationPrefsStore.getState().setMute('server', TARGET_ID, true, past);
    renderItem({ targetType: 'server', kindLabel: 'Server' });
    expect(screen.getByText('Mute Server')).toBeInTheDocument();
  });

  it('swallows a setMutePreference rejection on the unmute path with a console.warn', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    // Seed an active mute so the trigger renders as "Unmute" and the click
    // takes the direct setMutePreference(false, null) path (no submenu).
    useNotificationPrefsStore.getState().setMute('server', TARGET_ID, true, null);
    vi.mocked(setMutePreference).mockRejectedValueOnce(new Error('network down'));

    renderItem({ targetType: 'server', kindLabel: 'Server' });
    fireEvent.click(screen.getByText('Unmute Server'));

    await waitFor(() => {
      expect(warnSpy).toHaveBeenCalledTimes(1);
    });
    const args = warnSpy.mock.calls[0];
    expect(args[0]).toBe('Failed to unmute target:');
    expect(args[1]).toBe('server');
    expect(args[2]).toBe('network down');
    warnSpy.mockRestore();
  });

  it('swallows a setMutePreference rejection on the mute path with a console.warn', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.mocked(setMutePreference).mockRejectedValueOnce(new Error('rate limited'));

    renderItem({ targetType: 'channel', kindLabel: 'Channel' });
    // Open the duration submenu, then pick the indefinite option to fire mute(true).
    fireEvent.click(screen.getByText('Mute Channel'));
    fireEvent.click(screen.getByText('Until I turn it back on'));

    await waitFor(() => {
      expect(warnSpy).toHaveBeenCalledTimes(1);
    });
    const args = warnSpy.mock.calls[0];
    expect(args[0]).toBe('Failed to mute target:');
    expect(args[1]).toBe('channel');
    expect(args[2]).toBe('rate limited');
    warnSpy.mockRestore();
  });
});
