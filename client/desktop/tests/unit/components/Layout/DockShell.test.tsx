import { useState, type ComponentProps, type ReactNode } from 'react';
import { act, fireEvent, render, screen, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useLayoutStore, type SidebarProfiles } from '@/renderer/stores/layoutStore';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { DockOverlayProvider, DockShell } from '@/renderer/components/Layout/DockShell';
import { AttributedPopover } from '@/renderer/components/Layout/AttributedPopover';

const profiles = (width = 240, pinned = true): SidebarProfiles => ({
  dm: {
    left: { width, pinned },
    right: { width, pinned },
  },
  server: {
    left: { width, pinned },
    right: { width, pinned },
  },
});

const renderDock = (props: Partial<ComponentProps<typeof DockShell>> = {}) =>
  render(
    <DockOverlayProvider>
      <DockShell
        context="dm"
        side="right"
        label="Friends"
        header={<span>Friends</span>}
        renderBody={(compact) => <span>{compact ? 'compact' : 'standard'}</span>}
        {...props}
      />
    </DockOverlayProvider>
  );

const hiddenOrInertAncestor = (element: HTMLElement): Element | null =>
  element.closest('[aria-hidden="true"], [inert]');

describe('DockShell', () => {
  beforeEach(() => {
    resetAllStores();
    useLayoutStore.setState({
      sidebarProfiles: profiles(),
      sidebarLayoutsDecoupled: true,
      interfaceLocked: false,
    });
    useSettingsStore.setState((state) => ({
      appearance: { ...state.appearance, reduceAnimations: false },
    }));
  });

  it.each([
    [56, 'compact'],
    [179, 'compact'],
    [180, 'standard'],
  ] as const)('renders %ipx as the %s presentation', (width, presentation) => {
    useLayoutStore.setState({ sidebarProfiles: profiles(width) });

    const { container } = renderDock();

    expect(screen.getByText(presentation)).toBeVisible();
    expect(container.querySelector('.dock-shell__surface')).toHaveStyle({ width: `${width}px` });
  });

  it.each([
    ['left', 'Threads'],
    ['right', 'Friends'],
  ] as const)('stacks the compact %s pin control vertically', (side, label) => {
    useLayoutStore.setState({ sidebarProfiles: profiles(56) });

    const { container } = renderDock({ side, label, header: <span>{label}</span> });
    const pin = screen.getByRole('button', { name: `Unpin ${label} sidebar` });

    expect(pin).toHaveAttribute('title', `Unpin ${label} sidebar`);
    expect(pin.parentElement).toHaveAttribute('data-layout', 'vertical');
    expect(container.querySelector('.dock-shell__surface')).toHaveAttribute(
      'data-presentation',
      'compact'
    );
  });

  it('keeps a pin-only header isolated from server header styling (#1750)', () => {
    const { container } = renderDock({ header: null });

    const header = container.querySelector('.dock-shell__header');
    expect(header).toHaveClass('dock-shell__header--actions-only');
    expect(header).not.toHaveClass('channels-sidebar-header');
  });

  it('rotates only the pin glyph so the button hitbox remains axis-aligned (#1750)', () => {
    renderDock();

    const pin = screen.getByRole('button', { name: 'Unpin Friends sidebar' });
    expect(pin.querySelector('.dock-shell__pin-icon')).toBeInTheDocument();
  });

  it.each([
    ['left', 'left'],
    ['right', 'right'],
  ] as const)('renders a mirrored %s-side shell', (side, expectedSide) => {
    const { container } = renderDock({ side });

    expect(container.querySelector('.dock-shell')).toHaveAttribute('data-side', expectedSide);
  });

  it('exposes state-specific pin names and pressed state', () => {
    renderDock();
    const unpin = screen.getByRole('button', { name: 'Unpin Friends sidebar' });
    expect(unpin).toHaveAttribute('aria-pressed', 'true');

    fireEvent.click(unpin);

    expect(screen.getByRole('button', { name: 'Open Friends sidebar' })).toBeVisible();
    fireEvent.focus(screen.getByRole('button', { name: 'Open Friends sidebar' }));
    expect(screen.getByText('standard')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Pin Friends sidebar' })).toHaveAttribute(
      'aria-pressed',
      'false'
    );
  });

  it('exposes a focusable adjustable separator and commits keyboard resize steps', () => {
    useLayoutStore.setState({ sidebarProfiles: profiles(179) });
    renderDock({ side: 'left', label: 'Threads' });

    const separator = screen.getByRole('separator', { name: 'Resize Threads sidebar' });
    expect(separator).toHaveAttribute('tabindex', '0');
    expect(separator).toHaveAttribute('aria-orientation', 'vertical');
    expect(separator).toHaveAttribute('aria-valuemin', '56');
    expect(separator).toHaveAttribute('aria-valuemax', '400');
    expect(separator).toHaveAttribute('aria-valuenow', '179');

    separator.focus();
    expect(separator).toHaveFocus();

    fireEvent.keyDown(separator, { key: 'ArrowRight' });
    expect(useLayoutStore.getState().sidebarProfiles.dm.left.width).toBe(189);
    expect(separator).toHaveAttribute('aria-valuenow', '189');

    fireEvent.keyDown(separator, { key: 'ArrowRight', shiftKey: true });
    expect(useLayoutStore.getState().sidebarProfiles.dm.left.width).toBe(239);
    expect(separator).toHaveAttribute('aria-valuenow', '239');
  });

  it('opens on hover and closes after pointer leave', () => {
    vi.useFakeTimers();
    useLayoutStore.setState({ sidebarProfiles: profiles(240, false) });
    const { container } = renderDock();

    fireEvent.mouseEnter(screen.getByRole('button', { name: 'Open Friends sidebar' }));
    expect(container.querySelector('.dock-shell__surface')).toHaveAttribute('data-state', 'open');

    fireEvent.mouseLeave(container.querySelector('.dock-shell__surface') as Element);
    act(() => vi.runAllTimers());
    expect(container.querySelector('.dock-shell__surface')).toHaveAttribute('data-state', 'closed');
    vi.useRealTimers();
  });

  it('stays open while focus is inside the overlay', () => {
    vi.useFakeTimers();
    useLayoutStore.setState({ sidebarProfiles: profiles(240, false) });
    const { container } = renderDock({ header: <button type="button">Inside</button> });

    fireEvent.focus(screen.getByRole('button', { name: 'Open Friends sidebar' }));
    screen.getByRole('button', { name: 'Inside' }).focus();
    fireEvent.mouseLeave(container.querySelector('.dock-shell__surface') as Element);
    act(() => vi.runAllTimers());

    expect(container.querySelector('.dock-shell__surface')).toHaveAttribute('data-state', 'open');
    vi.useRealTimers();
  });

  it('keeps an owned attributed popover actionable inside an unpinned overlay', () => {
    vi.useFakeTimers();
    const action = vi.fn();
    const PopoverBody = () => {
      const [anchor, setAnchor] = useState<HTMLElement | null>(null);
      return (
        <>
          <button
            id="owned-trigger"
            type="button"
            onClick={(event) => setAnchor(event.currentTarget)}
          >
            Online
          </button>
          <AttributedPopover
            id="owned-popover"
            anchor={anchor}
            label="Online — 1"
            open={anchor !== null}
            onClose={() => setAnchor(null)}
          >
            <button type="button" onClick={action}>
              Message Alice
            </button>
          </AttributedPopover>
        </>
      );
    };
    useLayoutStore.setState({
      sidebarProfiles: profiles(240, false),
      interfaceLocked: true,
    });
    const { container } = renderDock({
      header: null,
      renderBody: () => <PopoverBody />,
    });
    const surface = container.querySelector('.dock-shell__surface') as HTMLElement;
    const lip = screen.getByRole('button', { name: 'Open Friends sidebar' });
    fireEvent.focus(lip);
    const trigger = screen.getByRole('button', { name: 'Online' });
    fireEvent.focus(trigger);
    fireEvent.click(trigger);

    const popoverAction = screen.getByRole('button', { name: 'Message Alice' });
    fireEvent.mouseLeave(surface);
    popoverAction.focus();
    fireEvent.mouseDown(popoverAction);
    fireEvent.click(popoverAction);
    act(() => vi.runAllTimers());

    expect(action).toHaveBeenCalledOnce();
    expect(screen.getByRole('region', { name: 'Online — 1' })).toBeVisible();
    expect(surface).toHaveAttribute('data-state', 'open');
    vi.useRealTimers();
  });

  it('keeps an activated lip held open until Escape and restores visible focus after close', () => {
    vi.useFakeTimers();
    useLayoutStore.setState({ sidebarProfiles: profiles(240, false) });
    const { container } = renderDock({ header: <button type="button">Inside</button> });
    const lip = screen.getByRole('button', { name: 'Open Friends sidebar' });
    const surface = container.querySelector('.dock-shell__surface') as HTMLElement;

    fireEvent.click(lip);
    screen.getByRole('button', { name: 'Inside' }).focus();
    fireEvent.mouseLeave(container.querySelector('.dock-shell__surface') as Element);
    act(() => vi.runAllTimers());
    expect(container.querySelector('.dock-shell__surface')).toHaveAttribute('data-state', 'open');

    const focusSnapshots: Array<{
      lipAriaHidden: string | null;
      surfaceAriaHidden: string | null;
      surfaceInert: boolean;
    }> = [];
    lip.addEventListener('focus', () => {
      focusSnapshots.push({
        lipAriaHidden: lip.getAttribute('aria-hidden'),
        surfaceAriaHidden: surface.getAttribute('aria-hidden'),
        surfaceInert: surface.hasAttribute('inert'),
      });
    });

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(surface).toHaveAttribute('data-state', 'closed');
    expect(surface).toHaveAttribute('aria-hidden', 'true');
    expect(surface).toHaveAttribute('inert');
    expect(lip).toHaveFocus();
    expect(hiddenOrInertAncestor(lip)).toBeNull();
    expect(focusSnapshots).toEqual([
      {
        lipAriaHidden: 'false',
        surfaceAriaHidden: 'true',
        surfaceInert: true,
      },
    ]);
    vi.useRealTimers();
  });

  it('dismisses a held overlay on outside interaction', () => {
    useLayoutStore.setState({ sidebarProfiles: profiles(240, false) });
    const { container } = renderDock();

    fireEvent.click(screen.getByRole('button', { name: 'Open Friends sidebar' }));
    fireEvent.mouseDown(document.body);

    expect(container.querySelector('.dock-shell__surface')).toHaveAttribute('data-state', 'closed');
  });

  it('keeps keyboard focus on an exposed lip when focus opens the overlay', () => {
    useLayoutStore.setState({ sidebarProfiles: profiles(240, false) });
    const { container } = renderDock({ header: <button type="button">Inside</button> });
    const surface = container.querySelector('.dock-shell__surface') as HTMLElement;
    const lip = screen.getByRole('button', { name: 'Open Friends sidebar' });

    expect(surface).toHaveAttribute('inert');

    act(() => lip.focus());

    expect(lip).toHaveFocus();
    expect(surface).toHaveAttribute('data-state', 'open');
    expect(lip).toHaveAttribute('aria-hidden', 'false');
    expect(lip).toHaveClass('dock-shell__lip--focused');
    expect(lip).not.toHaveClass('dock-shell__lip--covered');
    expect(hiddenOrInertAncestor(lip)).toBeNull();
    expect(surface).toHaveAttribute('aria-hidden', 'false');
    expect(surface).not.toHaveAttribute('inert');
  });

  it('restores focus to the visible lip after unpinning', () => {
    const { container } = renderDock();
    const surface = container.querySelector('.dock-shell__surface') as HTMLElement;
    const unpin = screen.getByRole('button', { name: 'Unpin Friends sidebar' });

    unpin.focus();
    fireEvent.click(unpin);

    const lip = screen.getByRole('button', { name: 'Open Friends sidebar' });
    expect(lip).toHaveFocus();
    expect(hiddenOrInertAncestor(lip)).toBeNull();
    expect(surface).toHaveAttribute('aria-hidden', 'true');
    expect(surface).toHaveAttribute('inert');
  });

  it('keeps only one opposite-side overlay open', () => {
    useLayoutStore.setState({ sidebarProfiles: profiles(240, false) });
    const body =
      (label: string): ((compact: boolean) => ReactNode) =>
      () => <span>{label} body</span>;
    const { container } = render(
      <DockOverlayProvider>
        <DockShell
          context="dm"
          side="left"
          label="Threads"
          header={<span>Threads</span>}
          renderBody={body('Threads')}
        />
        <DockShell
          context="dm"
          side="right"
          label="Friends"
          header={<span>Friends</span>}
          renderBody={body('Friends')}
        />
      </DockOverlayProvider>
    );

    fireEvent.focus(screen.getByRole('button', { name: 'Open Threads sidebar' }));
    expect(container.querySelector('[data-side="left"] .dock-shell__surface')).toHaveAttribute(
      'data-state',
      'open'
    );

    fireEvent.focus(screen.getByRole('button', { name: 'Open Friends sidebar' }));
    expect(container.querySelector('[data-side="left"] .dock-shell__surface')).toHaveAttribute(
      'data-state',
      'closed'
    );
    expect(container.querySelector('[data-side="right"] .dock-shell__surface')).toHaveAttribute(
      'data-state',
      'open'
    );
  });

  it('force-pins without mutating the retained preference', () => {
    useLayoutStore.setState({ sidebarProfiles: profiles(240, false) });
    const { container } = renderDock({ forcePinned: true });

    expect(container.querySelector('.dock-shell__surface')).toHaveAttribute('data-mode', 'pinned');
    expect(screen.queryByRole('button', { name: 'Unpin Friends sidebar' })).not.toBeInTheDocument();
    expect(useLayoutStore.getState().sidebarProfiles.dm.right.pinned).toBe(false);
  });

  it('Interface Lock removes mutation controls but lets the lip peek', () => {
    useLayoutStore.setState({
      sidebarProfiles: profiles(240, false),
      interfaceLocked: true,
    });
    const { container } = renderDock();
    const lip = screen.getByRole('button', { name: 'Open Friends sidebar' });

    fireEvent.click(lip);

    expect(container.querySelector('.dock-shell__surface')).toHaveAttribute('data-state', 'open');
    expect(screen.queryByRole('button', { name: 'Pin Friends sidebar' })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('separator', { name: 'Resize Friends sidebar' })
    ).not.toBeInTheDocument();
    expect(useLayoutStore.getState().sidebarProfiles.dm.right.pinned).toBe(false);
  });

  it('cleans up a pending pointer-leave timer on unmount', () => {
    vi.useFakeTimers();
    useLayoutStore.setState({ sidebarProfiles: profiles(240, false) });
    const { container, unmount } = renderDock();

    fireEvent.mouseEnter(screen.getByRole('button', { name: 'Open Friends sidebar' }));
    fireEvent.mouseLeave(container.querySelector('.dock-shell__surface') as Element);
    expect(vi.getTimerCount()).toBe(1);

    unmount();
    expect(vi.getTimerCount()).toBe(0);
    vi.useRealTimers();
  });

  it('marks nonessential motion disabled when reduced animations are enabled', () => {
    useSettingsStore.setState((state) => ({
      appearance: { ...state.appearance, reduceAnimations: true },
    }));

    const { container } = renderDock();

    expect(container.querySelector('.dock-shell')).toHaveAttribute('data-reduce-motion', 'true');
  });
});
