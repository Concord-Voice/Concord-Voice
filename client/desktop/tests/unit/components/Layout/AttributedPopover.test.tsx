import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { AttributedPopover } from '@/renderer/components/Layout/AttributedPopover';

const anchors: HTMLElement[] = [];

const makeAnchor = () => {
  const anchor = document.createElement('button');
  document.body.append(anchor);
  anchors.push(anchor);
  return anchor;
};

const rect = (overrides: Partial<DOMRect> = {}): DOMRect =>
  ({
    x: 400,
    y: 300,
    left: 400,
    right: 444,
    top: 300,
    bottom: 344,
    width: 44,
    height: 44,
    toJSON: () => ({}),
    ...overrides,
  }) satisfies DOMRect;

describe('AttributedPopover', () => {
  beforeEach(() => {
    resetAllStores();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    delete document.documentElement.dataset.reduceAnimations;
    anchors.splice(0).forEach((anchor) => anchor.remove());
  });

  it('portals an attributed region and restores its anchor on Escape', () => {
    const anchor = makeAnchor();
    const onClose = vi.fn();
    anchor.focus();

    const { container } = render(
      <AttributedPopover
        id="friends-online"
        anchor={anchor}
        label="Online — 2"
        open
        onClose={onClose}
      >
        <button type="button">Alice</button>
      </AttributedPopover>
    );

    const surface = screen.getByRole('region', { name: 'Online — 2' });
    expect(surface).toBeVisible();
    expect(container).not.toContainElement(surface);

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledOnce();
    expect(anchor).toHaveFocus();
  });

  it('closes for an outside pointer but ignores pointers inside the portaled surface', () => {
    const anchor = makeAnchor();
    const onClose = vi.fn();
    render(
      <AttributedPopover
        id="friends-online"
        anchor={anchor}
        label="Online — 1"
        open
        onClose={onClose}
      >
        <button type="button">Alice</button>
      </AttributedPopover>
    );

    fireEvent.pointerDown(screen.getByRole('button', { name: 'Alice' }));
    expect(onClose).not.toHaveBeenCalled();

    fireEvent.pointerDown(anchor);
    expect(onClose).not.toHaveBeenCalled();

    fireEvent.pointerDown(document.body);
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('removes document listeners and restores focus when unmounted', () => {
    const anchor = makeAnchor();
    const onClose = vi.fn();
    const { unmount } = render(
      <AttributedPopover
        id="friends-online"
        anchor={anchor}
        label="Online — 1"
        open
        onClose={onClose}
      >
        <button type="button">Alice</button>
      </AttributedPopover>
    );
    screen.getByRole('button', { name: 'Alice' }).focus();

    unmount();
    expect(anchor).toHaveFocus();

    fireEvent.keyDown(document, { key: 'Escape' });
    fireEvent.pointerDown(document.body);
    expect(onClose).not.toHaveBeenCalled();
  });

  it('keeps focus and uses the latest close callback across a parent rerender', () => {
    const anchor = makeAnchor();
    const firstClose = vi.fn();
    const nextClose = vi.fn();
    const { rerender } = render(
      <AttributedPopover
        id="friends-online"
        anchor={anchor}
        label="Online — 1"
        open
        onClose={firstClose}
      >
        <button type="button">Alice</button>
      </AttributedPopover>
    );
    const friend = screen.getByRole('button', { name: 'Alice' });
    friend.focus();

    rerender(
      <AttributedPopover
        id="friends-online"
        anchor={anchor}
        label="Online — 1"
        open
        onClose={nextClose}
      >
        <button type="button">Alice</button>
      </AttributedPopover>
    );
    expect(friend).toHaveFocus();

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(firstClose).not.toHaveBeenCalled();
    expect(nextClose).toHaveBeenCalledOnce();
  });

  it('switches anchors without restoring the previous one and restores the current on Escape', () => {
    const onlineAnchor = makeAnchor();
    const offlineAnchor = makeAnchor();
    const onClose = vi.fn();
    const { rerender } = render(
      <AttributedPopover
        id="friends-online"
        anchor={onlineAnchor}
        label="Online — 1"
        open
        onClose={onClose}
      >
        <button type="button">Alice</button>
      </AttributedPopover>
    );
    const friend = screen.getByRole('button', { name: 'Alice' });
    friend.focus();

    rerender(
      <AttributedPopover
        id="friends-offline"
        anchor={offlineAnchor}
        label="Offline — 1"
        open
        onClose={onClose}
      >
        <button type="button">Bob</button>
      </AttributedPopover>
    );
    expect(friend).toHaveFocus();
    expect(onlineAnchor).not.toHaveFocus();

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledOnce();
    expect(offlineAnchor).toHaveFocus();
  });

  it('positions to the anchor left and clamps the surface and arrow to the viewport', async () => {
    const anchor = makeAnchor();
    const anchorRect = rect({ left: 250, right: 294, top: 130, bottom: 174, x: 250, y: 130 });
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function () {
      if (this === anchor) return anchorRect;
      if (this.classList.contains('attributed-popover')) {
        return rect({ width: 280, height: 180, right: 280, bottom: 180 });
      }
      return rect({ x: 0, y: 0, left: 0, right: 0, top: 0, bottom: 0, width: 0, height: 0 });
    });
    vi.stubGlobal('innerWidth', 320);
    vi.stubGlobal('innerHeight', 240);

    render(
      <AttributedPopover
        id="friends-online"
        anchor={anchor}
        label="Online — 1"
        open
        onClose={() => {}}
      >
        <button type="button">Alice</button>
      </AttributedPopover>
    );

    const surface = screen.getByRole('region', { name: 'Online — 1' });
    await waitFor(() => {
      expect(surface).toHaveStyle({ left: '8px', top: '52px' });
    });
    expect(Number.parseFloat(surface.style.left)).toBeLessThan(anchorRect.left);
    // Assert the INLINE value, not the computed one. jsdom 30 resolves calc()
    // against the viewport, so getComputedStyle (which is what toHaveStyle
    // reads) returns "752px" here — 768px default viewport height minus 16.
    // The component's contract is the declaration it writes, not the pixel
    // value a particular jsdom viewport happens to resolve it to.
    expect(surface.style.maxHeight).toBe('calc(100vh - 16px)');
    expect(surface.querySelector('.attributed-popover__arrow')).toHaveStyle({ top: '100px' });
  });

  it('mirrors outward positioning for a left-sidebar trigger', async () => {
    const anchor = makeAnchor();
    const anchorRect = rect({ left: 40, right: 84, top: 80, bottom: 124, x: 40, y: 80 });
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function () {
      if (this === anchor) return anchorRect;
      if (this.classList.contains('attributed-popover')) {
        return rect({ width: 200, height: 100, right: 200, bottom: 100 });
      }
      return rect({ x: 0, y: 0, left: 0, right: 0, top: 0, bottom: 0, width: 0, height: 0 });
    });
    vi.stubGlobal('innerWidth', 320);
    vi.stubGlobal('innerHeight', 240);

    render(
      <AttributedPopover
        id="thread-search"
        anchor={anchor}
        label="Search conversations"
        open
        placement="right"
        onClose={() => {}}
      >
        <input aria-label="Search conversations" />
      </AttributedPopover>
    );

    const surface = screen.getByRole('region', { name: 'Search conversations' });
    await waitFor(() => expect(surface).toHaveStyle({ left: '96px', top: '80px' }));
    expect(Number.parseFloat(surface.style.left)).toBeGreaterThan(anchorRect.right);
    expect(surface).toHaveAttribute('data-placement', 'right');
  });

  it('repositions on viewport resize and capture-phase scroll, then removes both listeners', () => {
    const anchor = makeAnchor();
    let anchorTop = 100;
    const anchorRect = vi
      .spyOn(anchor, 'getBoundingClientRect')
      .mockImplementation(() => rect({ top: anchorTop, bottom: anchorTop + 44, y: anchorTop }));
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function () {
      if (this.classList.contains('attributed-popover')) {
        return rect({ width: 200, height: 100, right: 200, bottom: 100 });
      }
      return rect();
    });

    const { unmount } = render(
      <AttributedPopover
        id="friends-online"
        anchor={anchor}
        label="Online — 1"
        open
        onClose={() => {}}
      >
        <button type="button">Alice</button>
      </AttributedPopover>
    );
    const surface = screen.getByRole('region', { name: 'Online — 1' });
    expect(surface).toHaveStyle({ top: '100px' });

    anchorTop = 140;
    fireEvent.resize(window);
    expect(surface).toHaveStyle({ top: '140px' });

    anchorTop = 180;
    fireEvent.scroll(document.body);
    expect(surface).toHaveStyle({ top: '180px' });

    unmount();
    const callsAfterUnmount = anchorRect.mock.calls.length;
    fireEvent.resize(window);
    fireEvent.scroll(document.body);
    expect(anchorRect).toHaveBeenCalledTimes(callsAfterUnmount);
  });

  it('uses the existing reduced-motion preference for its entrance', () => {
    document.documentElement.dataset.reduceAnimations = 'true';
    const anchor = makeAnchor();
    render(
      <AttributedPopover
        id="friends-online"
        anchor={anchor}
        label="Online — 1"
        open
        onClose={() => {}}
      >
        <button type="button">Alice</button>
      </AttributedPopover>
    );

    expect(screen.getByRole('region', { name: 'Online — 1' })).toHaveAttribute(
      'data-reduce-motion',
      'true'
    );
  });
});
