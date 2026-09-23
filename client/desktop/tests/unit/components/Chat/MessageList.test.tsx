import { act, fireEvent, render, screen } from '../../../test-utils';
import MessageList from '@/renderer/components/Chat/MessageList';
import { mockMessage, mockMessage2 } from '../../../mocks/fixtures';
import { useChannelScrollStore } from '@/renderer/stores/chat/channelScrollStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { beforeEach, vi } from 'vitest';
import { StrictMode } from 'react';
import { render as bareRender } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock the Message component to simplify testing
vi.mock('@/renderer/components/Chat/Message', () => ({
  default: ({ message }: { message: any }) => <div data-testid="message">{message.content}</div>,
}));

describe('MessageList', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders messages', () => {
    render(<MessageList messages={[mockMessage, mockMessage2]} currentUserId="user-1" />);
    expect(screen.getByText('Hello, world!')).toBeInTheDocument();
    expect(screen.getByText('Hi there!')).toBeInTheDocument();
  });

  it('shows empty state when no messages', () => {
    render(<MessageList messages={[]} currentUserId="user-1" channelName="general" />);
    expect(screen.getByText(/welcome to #general/i)).toBeInTheDocument();
  });

  it('shows loading state', () => {
    render(<MessageList messages={[]} currentUserId="user-1" isLoading={true} />);
    expect(screen.getByText(/loading messages/i)).toBeInTheDocument();
  });

  it('shows date divider for first message', () => {
    render(<MessageList messages={[mockMessage]} currentUserId="user-1" />);
    // Messages from 2025-01-01 should show a date divider
    const dateDividers = document.querySelectorAll('.date-divider');
    expect(dateDividers.length).toBeGreaterThan(0);
  });

  it('shows date dividers between messages on different days', () => {
    const messageDay1 = {
      ...mockMessage,
      id: 'msg-day1',
      created_at: '2025-01-01T12:00:00Z',
      updated_at: '2025-01-01T12:00:00Z',
    };
    const messageDay2 = {
      ...mockMessage2,
      id: 'msg-day2',
      created_at: '2025-01-03T14:00:00Z',
      updated_at: '2025-01-03T14:00:00Z',
    };

    render(<MessageList messages={[messageDay1, messageDay2]} currentUserId="user-1" />);

    // Should show two date dividers (one for each distinct day)
    const dateDividers = document.querySelectorAll('.date-divider');
    expect(dateDividers.length).toBe(2);
  });

  it('renders container class for scroll area', () => {
    render(<MessageList messages={[mockMessage]} currentUserId="user-1" />);
    expect(document.querySelector('.message-list-container')).toBeInTheDocument();
  });

  it('shows default channel name when channelName is not provided', () => {
    render(<MessageList messages={[]} currentUserId="user-1" />);
    expect(screen.getByText(/welcome to #this channel/i)).toBeInTheDocument();
  });

  it('shows loading more indicator when loading with existing messages', () => {
    render(
      <MessageList
        messages={[mockMessage]}
        currentUserId="user-1"
        isLoading={true}
        hasMore={true}
      />
    );
    expect(screen.getByText(/loading more messages/i)).toBeInTheDocument();
  });

  it('observes the message list with ResizeObserver to re-pin scroll on growth', () => {
    const observe = vi.fn();
    const disconnect = vi.fn();
    let roCallback: ResizeObserverCallback | null = null;
    class MockRO {
      constructor(cb: ResizeObserverCallback) {
        roCallback = cb;
      }
      observe = observe;
      disconnect = disconnect;
      unobserve = vi.fn();
    }
    const OriginalRO = globalThis.ResizeObserver;
    (globalThis as any).ResizeObserver = MockRO;
    try {
      const { unmount } = render(
        <MessageList messages={[mockMessage, mockMessage2]} currentUserId="user-1" />
      );
      // Container + each child observed
      expect(observe).toHaveBeenCalled();
      // Trigger the callback to exercise the re-pin branch (no assertion needed —
      // covers the inner function lines).
      roCallback?.([] as unknown as ResizeObserverEntry[], {} as ResizeObserver);
      unmount();
      expect(disconnect).toHaveBeenCalled();
    } finally {
      (globalThis as any).ResizeObserver = OriginalRO;
    }
  });

  it('renders the correct number of message elements', () => {
    render(<MessageList messages={[mockMessage, mockMessage2]} currentUserId="user-1" />);
    const messageElements = screen.getAllByTestId('message');
    expect(messageElements).toHaveLength(2);
  });

  // ---- Bug #2: autoscroll fails on media load ----

  it('renders an inner content wrapper for the ResizeObserver to watch', () => {
    // The fix wraps messages in `.message-list-content` so the observer fires
    // when child rows grow (media loads) — without this wrapper, the scroll
    // container itself is fixed by flex and never resizes, so re-pin never
    // happens and the user has to manually scroll after sending media.
    render(<MessageList messages={[mockMessage]} currentUserId="user-1" />);
    expect(document.querySelector('.message-list-content')).toBeInTheDocument();
  });

  it('observes both the inner content wrapper and the scroll container', () => {
    const observed: Element[] = [];
    class MockRO {
      observe(el: Element) {
        observed.push(el);
      }
      disconnect = vi.fn();
      unobserve = vi.fn();
    }
    const OriginalRO = globalThis.ResizeObserver;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (globalThis as any).ResizeObserver = MockRO;
    try {
      render(<MessageList messages={[mockMessage]} currentUserId="user-1" />);
      // The content wrapper grows on media load; the scroll container shrinks
      // or grows when a sibling below it (composer, banners) changes height.
      // Both move the bottom without firing a scroll event.
      const classes = observed.map((el) => el.className).sort();
      expect(classes).toEqual(['message-list', 'message-list-content']);
    } finally {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (globalThis as any).ResizeObserver = OriginalRO;
    }
  });

  it('re-pins scroll to bottom when content grows and user is near bottom', () => {
    let roCallback: ResizeObserverCallback | null = null;
    class MockRO {
      constructor(cb: ResizeObserverCallback) {
        roCallback = cb;
      }
      observe = vi.fn();
      disconnect = vi.fn();
      unobserve = vi.fn();
    }
    const OriginalRO = globalThis.ResizeObserver;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (globalThis as any).ResizeObserver = MockRO;
    try {
      render(<MessageList messages={[mockMessage]} currentUserId="user-1" />);
      const list = document.querySelector('.message-list') as HTMLElement;
      // Stub layout: scroll container is 200px tall, content was 200px so
      // user is at the bottom. Then the GIF "loads" and content grows to
      // 600px — re-pin should set scrollTop = scrollHeight.
      Object.defineProperty(list, 'scrollHeight', { value: 600, writable: true });
      Object.defineProperty(list, 'clientHeight', { value: 200, writable: true });
      list.scrollTop = 0;
      // Fire the observer callback (the new helper reads isNearBottomRef
      // which defaults to true on mount, so re-pin should happen).
      roCallback?.([] as unknown as ResizeObserverEntry[], {} as ResizeObserver);
      expect(list.scrollTop).toBe(600);
    } finally {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (globalThis as any).ResizeObserver = OriginalRO;
    }
  });

  it('re-pins cold-loaded messages when late content grows (#2006)', () => {
    const callbacks: ResizeObserverCallback[] = [];
    class MockRO {
      constructor(cb: ResizeObserverCallback) {
        callbacks.push(cb);
      }
      observe = vi.fn();
      disconnect = vi.fn();
      unobserve = vi.fn();
    }
    const OriginalRO = globalThis.ResizeObserver;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (globalThis as any).ResizeObserver = MockRO;
    try {
      const { rerender } = render(
        <MessageList messages={[]} currentUserId="user-1" isLoading={true} />
      );

      rerender(<MessageList messages={[mockMessage]} currentUserId="user-1" isLoading={false} />);

      const list = document.querySelector('.message-list') as HTMLElement;
      Object.defineProperty(list, 'scrollHeight', { value: 600, writable: true });
      Object.defineProperty(list, 'clientHeight', { value: 200, writable: true });
      list.scrollTop = 0;

      callbacks.forEach((callback) =>
        callback([] as unknown as ResizeObserverEntry[], {} as ResizeObserver)
      );

      expect(list.scrollTop).toBe(600);
    } finally {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (globalThis as any).ResizeObserver = OriginalRO;
    }
  });

  it('keeps following latest when late content grows after Return to Latest (#2006)', () => {
    let roCallback: ResizeObserverCallback | null = null;
    class MockRO {
      constructor(cb: ResizeObserverCallback) {
        roCallback = cb;
      }
      observe = vi.fn();
      disconnect = vi.fn();
      unobserve = vi.fn();
    }
    const OriginalRO = globalThis.ResizeObserver;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (globalThis as any).ResizeObserver = MockRO;
    try {
      render(<MessageList messages={[mockMessage]} currentUserId="user-1" />);
      const list = document.querySelector('.message-list') as HTMLElement;
      Object.defineProperty(list, 'scrollHeight', { value: 1000, writable: true });
      Object.defineProperty(list, 'clientHeight', { value: 200, writable: true });
      Object.defineProperty(list, 'scrollTo', {
        configurable: true,
        value: vi.fn((options: ScrollToOptions) => {
          if (options.behavior === 'smooth') {
            // Native smooth scrolling emits intermediate scroll events before
            // it reaches the target. A late GIF resize can land in that window.
            list.scrollTop = 400;
            fireEvent.scroll(list);
            return;
          }
          list.scrollTop = options.top ?? list.scrollTop;
        }),
      });
      list.scrollTop = 100;
      fireEvent.scroll(list);

      fireEvent.click(screen.getByRole('button', { name: /return to latest/i }));
      expect(list.scrollTop).toBe(1000);

      Object.defineProperty(list, 'scrollHeight', { value: 1500, writable: true });
      roCallback?.([] as unknown as ResizeObserverEntry[], {} as ResizeObserver);

      expect(list.scrollTop).toBe(1500);
    } finally {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (globalThis as any).ResizeObserver = OriginalRO;
    }
  });

  it('offers Return to Latest whenever the list is no longer near bottom (#2006)', () => {
    render(<MessageList messages={[mockMessage]} currentUserId="user-1" />);
    const list = document.querySelector('.message-list') as HTMLElement;
    Object.defineProperty(list, 'scrollHeight', { value: 1000, writable: true });
    Object.defineProperty(list, 'clientHeight', { value: 200, writable: true });
    list.scrollTop = 600;

    fireEvent.scroll(list);

    expect(screen.getByRole('button', { name: /return to latest/i })).toBeVisible();
  });

  // ---- Scroll position preservation (WS3 #7, reworked for the lazy-media case) ----

  // Ten rows from another user, so every one of them counts as unread.
  const rows = Array.from({ length: 10 }, (_, i) => ({
    ...mockMessage,
    id: `msg-${i}`,
    user_id: 'user-2',
  }));

  /**
   * Stub the layout jsdom does not compute: the scroll container is
   * `clientHeight` tall and every message row is `rowHeight` tall, laid out
   * top-to-bottom. Rects are derived from the live scrollTop so a test can
   * scroll and re-measure the way a browser would. `rowHeight` is mutable so
   * a test can "resolve a GIF" by growing the rows after mount.
   */
  function installLayout(layout: { clientHeight: number; rowHeight: number }) {
    const proto = Element.prototype;
    const original = {
      rect: proto.getBoundingClientRect,
      scrollHeight: Object.getOwnPropertyDescriptor(proto, 'scrollHeight'),
      clientHeight: Object.getOwnPropertyDescriptor(proto, 'clientHeight'),
    };
    const rect = (top: number, bottom: number) =>
      ({
        top,
        bottom,
        left: 0,
        right: 0,
        width: 0,
        height: bottom - top,
        x: 0,
        y: top,
      }) as DOMRect;
    const isList = (el: Element) => el.classList.contains('message-list');
    Object.defineProperty(proto, 'scrollHeight', {
      configurable: true,
      get(this: Element) {
        return isList(this)
          ? this.querySelectorAll('[data-message-id]').length * layout.rowHeight
          : 0;
      },
    });
    Object.defineProperty(proto, 'clientHeight', {
      configurable: true,
      get(this: Element) {
        return isList(this) ? layout.clientHeight : 0;
      },
    });
    proto.getBoundingClientRect = function (this: Element) {
      if (!this.isConnected) return rect(0, 0);
      if (isList(this)) return rect(0, layout.clientHeight);
      const list = this.closest('.message-list');
      if (!list || !this.hasAttribute('data-message-id')) return rect(0, 0);
      const index = Array.from(list.querySelectorAll('[data-message-id]')).indexOf(this);
      const top = index * layout.rowHeight - list.scrollTop;
      return rect(top, top + layout.rowHeight);
    };
    return () => {
      proto.getBoundingClientRect = original.rect;
      if (original.scrollHeight)
        Object.defineProperty(proto, 'scrollHeight', original.scrollHeight);
      if (original.clientHeight)
        Object.defineProperty(proto, 'clientHeight', original.clientHeight);
    };
  }

  describe('scroll position preservation', () => {
    let restoreLayout: () => void = () => {};
    // 200px viewport over ten 100px rows: 1000px of content, bottom at 800.
    const layout = { clientHeight: 200, rowHeight: 100 };

    beforeEach(() => {
      layout.rowHeight = 100;
      layout.clientHeight = 200;
      restoreLayout = installLayout(layout);
      // Stores leak between tests otherwise: an unread count set by one case
      // would turn the next case's bare mount into a first-unread landing.
      resetAllStores();
      useChannelScrollStore.setState({ anchors: {} });
    });
    afterEach(() => restoreLayout());

    const getList = () => document.querySelector('.message-list') as HTMLElement;
    /** Swap in a ResizeObserver whose callback the test fires by hand. */
    function mockResizeObserver() {
      let cb: ResizeObserverCallback | null = null;
      class MockRO {
        constructor(callback: ResizeObserverCallback) {
          cb = callback;
        }
        observe = vi.fn();
        disconnect = vi.fn();
        unobserve = vi.fn();
      }
      const Original = globalThis.ResizeObserver;
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (globalThis as any).ResizeObserver = MockRO;
      return {
        fire: () => act(() => cb?.([] as unknown as ResizeObserverEntry[], {} as ResizeObserver)),
        restore: () => {
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          (globalThis as any).ResizeObserver = Original;
        },
      };
    }
    const returnToLatest = () => screen.queryByRole('button', { name: /return to latest/i });
    // jsdom has no Element.scrollTo; Return to Latest uses it.
    const stubScrollTo = (list: HTMLElement) =>
      Object.defineProperty(list, 'scrollTo', {
        configurable: true,
        value: vi.fn((options: ScrollToOptions) => {
          list.scrollTop = options.top ?? list.scrollTop;
        }),
      });

    it('returns to the bottom after Return to Latest even when media re-grows on remount', () => {
      // The reported soft-lock: come back to a remembered spot, click Return to
      // Latest, leave, come back while every GIF is still a skeleton, then the
      // GIFs resolve and grow. The list must stay pinned to the bottom instead
      // of freezing at the pre-growth offset.
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { unmount } = render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      const first = getList();
      stubScrollTo(first);
      expect(first.scrollTop).toBe(330);
      fireEvent.click(screen.getByRole('button', { name: /return to latest/i }));
      expect(first.scrollTop).toBe(1000);
      unmount();
      expect(useChannelScrollStore.getState().getAnchor('k')).toBeUndefined();

      const ro = mockResizeObserver();
      try {
        layout.rowHeight = 60; // skeletons: 600px of content
        render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
        const list = getList();
        expect(list.scrollTop).toBe(600);
        layout.rowHeight = 100; // GIFs resolved: 1000px of content
        ro.fire();
        expect(list.scrollTop).toBe(1000);
        expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
      } finally {
        ro.restore();
      }
    });

    it('clears any saved anchor when the user leaves from the bottom', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-2', offset: 0 });
      const { unmount } = render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      const list = getList();
      list.scrollTop = 800;
      fireEvent.scroll(list);
      unmount();
      expect(useChannelScrollStore.getState().getAnchor('k')).toBeUndefined();
    });

    it('saves the topmost visible message as the anchor when leaving from above the threshold', () => {
      const { unmount } = render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      const list = getList();
      // msg-3 spans 300..400; 30px of it sits above the top edge.
      list.scrollTop = 330;
      fireEvent.scroll(list);
      unmount();
      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });
    });

    it('restores the anchored message to where it was on remount', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(330);
      expect(screen.getByRole('button', { name: /return to latest/i })).toBeVisible();
    });

    it('restores a row that sat below the top edge (negative offset)', () => {
      // A date divider straddling the top edge leaves a gap above the first
      // row, so findTopAnchor records a negative offset for it.
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: -20 });
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(280);
    });

    it('anchors on a call-event row like any other', () => {
      const withCall = rows.map((m, i) =>
        i === 3
          ? {
              ...m,
              type: 'call_event' as const,
              call_event_payload: {
                started_at: '2026-01-01T00:00:00Z',
                status: 'completed' as const,
                duration_seconds: 5,
              },
            }
          : m
      );
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      render(<MessageList messages={withCall} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(330);
    });

    it('shows a message that arrives while the restored anchor already sits inside the band', () => {
      // Saved above the threshold at full heights; at skeleton heights the same
      // anchor can land inside the band. Not following, but the bottom is in
      // view, so an arrival is shown rather than counted behind a hidden button.
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-7', offset: 0 });
      const { rerender } = render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      const list = getList();
      expect(list.scrollTop).toBe(700); // 100px from the bottom: inside the band
      expect(returnToLatest()).not.toBeInTheDocument();
      rerender(
        <MessageList
          messages={[...rows, { ...mockMessage, id: 'msg-10', user_id: 'user-2' }]}
          currentUserId="user-1"
          persistenceKey="k"
        />
      );
      expect(list.scrollTop).toBe(1100);
      expect(document.querySelector('.new-message-badge')).toBeNull();
    });

    it('fetches the page above an anchor restored near the top, on the landing echo alone', () => {
      const onLoadMore = vi.fn();
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-0', offset: 20 });
      render(
        <MessageList
          messages={rows}
          currentUserId="user-1"
          persistenceKey="k"
          hasMore
          onLoadMore={onLoadMore}
        />
      );
      const list = getList();
      expect(list.scrollTop).toBe(20);
      fireEvent.scroll(list); // the echo of the landing scroll
      expect(onLoadMore).toHaveBeenCalledTimes(1);
      expect(returnToLatest()).toBeVisible();
    });

    it('lands once under StrictMode and keeps the echo of its first landing armed', () => {
      // Bare RTL render: the test-utils provider wrapper suppresses the dev
      // effect replay, and the replay is the point — it re-lands on the same
      // scrollTop, so it must not disarm the first landing's pending echo.
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { unmount } = bareRender(
        <StrictMode>
          <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
        </StrictMode>
      );
      const list = getList();
      expect(list.scrollTop).toBe(330);
      expect(returnToLatest()).toBeVisible();

      layout.clientHeight = 700; // transient: the echo samples a taller viewport
      fireEvent.scroll(list);
      expect(returnToLatest()).toBeVisible();
      layout.clientHeight = 200;

      unmount();
      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });
    });

    it('lands the new key at the bottom when the key changes on a mounted instance', () => {
      const ro = mockResizeObserver();
      try {
        const { rerender } = render(
          <MessageList messages={rows} currentUserId="user-1" persistenceKey="a" />
        );
        const list = getList();
        list.scrollTop = 330;
        fireEvent.scroll(list);
        expect(returnToLatest()).toBeVisible();

        rerender(<MessageList messages={rows} currentUserId="user-1" persistenceKey="b" />);
        expect(useChannelScrollStore.getState().getAnchor('a')).toEqual({
          messageId: 'msg-3',
          offset: 30,
        });
        expect(list.scrollTop).toBe(1000);
        expect(returnToLatest()).not.toBeInTheDocument();
        // Following again: media growth re-pins instead of offering the button.
        layout.rowHeight = 150;
        ro.fire();
        expect(list.scrollTop).toBe(1500);
        expect(returnToLatest()).not.toBeInTheDocument();
      } finally {
        ro.restore();
      }
    });

    it('lands afresh when the list empties and refills under the same key', () => {
      const { rerender } = render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      const list = getList();
      list.scrollTop = 330;
      fireEvent.scroll(list);
      expect(returnToLatest()).toBeVisible();

      // A refill is a refetch, so it carries the loading cycle; rows that land
      // after a purge with no loading cycle are live arrivals instead (see the
      // onLatestSeen purge test).
      rerender(<MessageList messages={[]} currentUserId="user-1" persistenceKey="k" isLoading />);
      rerender(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(1000);
      expect(returnToLatest()).not.toBeInTheDocument();
    });

    it('does not restore a settled anchor after a same-key empty refetch', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { rerender, unmount } = render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      const list = getList();
      expect(list.scrollTop).toBe(330);

      // The saved anchor is stale once the user has moved elsewhere while
      // this mounted instance remains settled.
      list.scrollTop = 500;
      fireEvent.scroll(list);
      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });

      rerender(<MessageList messages={[]} currentUserId="user-1" persistenceKey="k" isLoading />);
      rerender(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);

      expect(getList().scrollTop).toBe(500);
      expect(getList().scrollTop).not.toBe(330);
      unmount();
      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-5',
        offset: 0,
      });
    });

    it('finds the anchored row by attribute, so an id that breaks a selector still restores', () => {
      const odd = rows.map((m, i) => (i === 3 ? { ...m, id: 'msg"]3' } : m));
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg"]3', offset: 30 });
      render(<MessageList messages={odd} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(330);
    });

    it('leaves the saved anchor alone when the list is detached before cleanup', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { unmount } = render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      getList().remove();
      unmount();
      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });
    });

    it('restores the anchor even when messages arrive after an initial loading render', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { rerender } = render(
        <MessageList messages={[]} currentUserId="user-1" persistenceKey="k" isLoading />
      );
      rerender(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(330);
    });

    it('keeps a missing saved anchor pending until a later page contains it', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { rerender } = render(
        <MessageList
          messages={rows.filter((message) => message.id !== 'msg-3')}
          currentUserId="user-1"
          persistenceKey="k"
          hasMore
          isLoading
        />
      );
      const list = getList();
      expect(list.scrollTop).toBe(900);
      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });

      act(() => {
        rerender(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      });

      expect(list.scrollTop, 'later page must restore the saved row, not latest').toBe(330);
    });

    it('does not repin a pending anchor when a missing page starts loading', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { rerender } = render(
        <MessageList
          messages={rows.filter((message) => message.id !== 'msg-3')}
          currentUserId="user-1"
          persistenceKey="k"
          hasMore
        />
      );
      const list = getList();
      list.scrollTop = 0;
      fireEvent.scroll(list);
      expect(screen.getByRole('button', { name: /return to latest/i })).toBeVisible();

      act(() => {
        rerender(
          <MessageList
            messages={rows.filter((message) => message.id !== 'msg-3')}
            currentUserId="user-1"
            persistenceKey="k"
            hasMore
            isLoading
          />
        );
      });

      expect(list.scrollTop, 'loading an absent page must preserve user position').toBe(0);
      expect(screen.getByRole('button', { name: /return to latest/i })).toBeVisible();
    });

    it('waits for history readiness before restoring an anchor in cached rows', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const notReady = { isHistoryReady: false };
      const { rerender } = render(
        <MessageList {...notReady} messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      const list = getList();
      expect(list.scrollTop, 'cached rows must not restore before initial history settles').toBe(
        1000
      );

      act(() => {
        rerender(
          <MessageList
            {...{ isHistoryReady: true }}
            messages={rows}
            currentUserId="user-1"
            persistenceKey="k"
          />
        );
      });
      expect(list.scrollTop).toBe(330);
    });

    it('preserves a cached anchor when the initial history fetch fails without the row', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const cachedRows = rows.filter((message) => message.id !== 'msg-3');
      const { rerender } = render(
        <MessageList
          messages={cachedRows}
          currentUserId="user-1"
          persistenceKey="k"
          isHistoryReady={false}
          hasMore={false}
          isLoading={false}
        />
      );

      act(() => {
        rerender(
          <MessageList
            messages={cachedRows}
            currentUserId="user-1"
            persistenceKey="k"
            isHistoryReady
            hasMore={false}
            isLoading={false}
            hasInitialHistoryError
          />
        );
      });

      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });
      expect(getList().scrollTop, 'failed initial fetch must not discard the saved anchor').toBe(
        900
      );
    });

    it('clears a saved anchor after a successful empty initial history settles', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { rerender } = render(
        <MessageList
          messages={[]}
          currentUserId="user-1"
          persistenceKey="k"
          isHistoryReady={false}
          hasMore={false}
          isLoading
        />
      );

      act(() => {
        rerender(
          <MessageList
            messages={[]}
            currentUserId="user-1"
            persistenceKey="k"
            isHistoryReady
            hasMore={false}
            isLoading={false}
          />
        );
      });

      expect(useChannelScrollStore.getState().getAnchor('k')).toBeUndefined();
    });

    it('keeps a missing anchor while a pending-key replacement fetch is in flight', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const cachedRows = rows.filter((message) => message.id !== 'msg-3');
      const { rerender } = render(
        <MessageList
          messages={cachedRows}
          currentUserId="user-1"
          persistenceKey="k"
          isHistoryReady
          hasInitialHistoryError
          hasMore={false}
          isLoading={false}
        />
      );

      act(() => {
        rerender(
          <MessageList
            messages={cachedRows}
            currentUserId="user-1"
            persistenceKey="k"
            isHistoryReady={false}
            hasMore={false}
            isLoading={false}
          />
        );
      });

      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });
    });

    it('preserves the cached anchor when unmounted before history becomes ready', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const notReady = { isHistoryReady: false };
      const { unmount } = render(
        <MessageList {...notReady} messages={rows} currentUserId="user-1" persistenceKey="k" />
      );

      unmount();

      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });
    });

    it('preserves a pending anchor across cleanup while history can continue', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { unmount } = render(
        <MessageList
          messages={rows.filter((message) => message.id !== 'msg-3')}
          currentUserId="user-1"
          persistenceKey="k"
          hasMore
        />
      );

      unmount();

      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });
    });

    it('clears a missing anchor only after history is exhausted', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { rerender } = render(
        <MessageList
          messages={rows.filter((message) => message.id !== 'msg-3')}
          currentUserId="user-1"
          persistenceKey="k"
          hasMore
          isLoading
        />
      );
      const list = getList();
      expect(useChannelScrollStore.getState().getAnchor('k')).toEqual({
        messageId: 'msg-3',
        offset: 30,
      });

      act(() => {
        rerender(
          <MessageList
            messages={rows.filter((message) => message.id !== 'msg-3')}
            currentUserId="user-1"
            persistenceKey="k"
            hasMore={false}
            isLoading={false}
          />
        );
      });
      expect(list.scrollTop, 'exhausted history falls back to latest').toBe(900);
      expect(useChannelScrollStore.getState().getAnchor('k')).toBeUndefined();
    });

    it('lets Return to Latest cancel a pending anchor before the row arrives', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { rerender } = render(
        <MessageList
          messages={rows.filter((message) => message.id !== 'msg-3')}
          currentUserId="user-1"
          persistenceKey="k"
          hasMore
        />
      );
      const list = getList();
      stubScrollTo(list);
      list.scrollTop = 0;
      fireEvent.scroll(list);
      fireEvent.click(screen.getByRole('button', { name: /return to latest/i }));

      act(() => {
        rerender(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      });

      expect(list.scrollTop, 'explicit latest must prevent a deferred anchor restore').toBe(1000);
      expect(useChannelScrollStore.getState().getAnchor('k')).toBeUndefined();
    });

    it('keeps Return to Latest settled when a canceled anchor refills with unread rows', () => {
      useUnreadStore.getState().setUnreadCount('k', 4);
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const cachedRows = rows.filter((message) => message.id !== 'msg-3');
      const { rerender } = render(
        <MessageList messages={cachedRows} currentUserId="user-1" persistenceKey="k" hasMore />
      );
      const list = getList();
      stubScrollTo(list);
      list.scrollTop = 0;
      fireEvent.scroll(list);
      fireEvent.click(screen.getByRole('button', { name: /return to latest/i }));
      expect(list.scrollTop).toBe(900);

      rerender(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);

      expect(list.scrollTop, 'a canceled anchor must stay at latest after refill').toBe(1000);
    });

    it('falls back to the bottom when the anchored message is no longer in the list', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'gone', offset: 0 });
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(1000);
      expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
    });

    it('lands on the first unread message when the unread messages overflow the viewport', () => {
      useUnreadStore.getState().setUnreadCount('k', 4);
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      // Four unread of ten: the first unread is msg-6, at 600px.
      expect(getList().scrollTop).toBe(600);
      const button = screen.getByRole('button', { name: /return to latest/i });
      expect(button).toBeVisible();
      // msg-6 and msg-7 fill the viewport; the badge is what is still below.
      expect(button).toHaveTextContent('2');
    });

    it('counts the badge down as the user reads through the unread rows, and back up above them', () => {
      useUnreadStore.getState().setUnreadCount('k', 6);
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      const list = getList();
      expect(list.scrollTop).toBe(400); // msg-4 at the top, msg-5 below it
      const button = () => screen.getByRole('button', { name: /return to latest/i });
      expect(button()).toHaveTextContent('4'); // msg-6..msg-9
      list.scrollTop = 500; // msg-5 and msg-6 in view, 300px from the bottom
      fireEvent.scroll(list);
      expect(button()).toHaveTextContent('3'); // msg-7..msg-9
      list.scrollTop = 0; // eight others' rows below, but only six were unread
      fireEvent.scroll(list);
      expect(button()).toHaveTextContent('6');
    });

    it("skips the user's own messages when counting back to the first unread", () => {
      // The server's count excludes own messages; msg-7 and msg-8 are mine, so
      // three unread are msg-9, msg-6 and msg-5 — land on msg-5, not on msg-7.
      const mixed = rows.map((m, i) => (i === 7 || i === 8 ? { ...m, user_id: 'user-1' } : m));
      useUnreadStore.getState().setUnreadCount('k', 3);
      render(<MessageList messages={mixed} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(500);
      // Below the viewport: msg-7 and msg-8 (mine) and msg-9 — one unread.
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('1');
    });

    it('lands on the topmost row when the unread count exceeds the mounted rows', () => {
      useUnreadStore.getState().setUnreadCount('k', 25);
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      const list = getList();
      expect(list.scrollTop).toBe(0);
      // The badge is what lies below on the loaded page; the rest is above, unloaded.
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('8');
      // The landing did not move the list, so no echo is armed: a real scroll
      // down to the bottom is honoured at once.
      list.scrollTop = 800;
      fireEvent.scroll(list);
      expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
    });

    it('requests the older page when the unread count exceeds the mounted rows', () => {
      const onLoadMore = vi.fn();
      useUnreadStore.getState().setUnreadCount('k', 25);
      render(
        <MessageList
          messages={rows}
          currentUserId="user-1"
          persistenceKey="k"
          hasMore
          onLoadMore={onLoadMore}
        />
      );
      expect(getList().scrollTop).toBe(0);
      expect(onLoadMore).toHaveBeenCalledTimes(1);
    });

    it('does not request the older page when the first unread is mounted', () => {
      const onLoadMore = vi.fn();
      useUnreadStore.getState().setUnreadCount('k', 4);
      render(
        <MessageList
          messages={rows}
          currentUserId="user-1"
          persistenceKey="k"
          hasMore
          onLoadMore={onLoadMore}
        />
      );
      expect(getList().scrollTop).toBe(600);
      expect(onLoadMore).not.toHaveBeenCalled();
    });

    it('reports only messages that arrived while scrolled up on leave, not the seeded badge', () => {
      useUnreadStore.getState().setUnreadCount('k', 4);
      const onUnseenOnLeave = vi.fn();
      const { rerender, unmount } = render(
        <MessageList
          messages={rows}
          currentUserId="user-1"
          persistenceKey="k"
          onUnseenOnLeave={onUnseenOnLeave}
        />
      );
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('2');
      rerender(
        <MessageList
          messages={[...rows, { ...mockMessage, id: 'msg-10', user_id: 'user-2' }]}
          currentUserId="user-1"
          persistenceKey="k"
          onUnseenOnLeave={onUnseenOnLeave}
        />
      );
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('3');
      unmount();
      expect(onUnseenOnLeave).toHaveBeenCalledTimes(1);
      expect(onUnseenOnLeave).toHaveBeenCalledWith(1);
    });

    it('reads DM unread from the conversation when the list is a DM thread', () => {
      useDMStore.setState({
        conversations: [
          {
            id: 'k',
            isGroup: false,
            isPersonal: false,
            name: 'Someone',
            participants: [],
            lastMessage: null,
            unreadCount: 4,
            createdAt: '2025-01-01T00:00:00Z',
          },
        ],
      });
      render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" chatContext="dm" />
      );
      expect(getList().scrollTop).toBe(600);
    });

    it('goes to the bottom when the unread messages fit in the viewport', () => {
      useUnreadStore.getState().setUnreadCount('k', 1);
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(1000);
      expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
    });

    it('re-counts the badge when layout moves under a still viewport', () => {
      const ro = mockResizeObserver();
      try {
        useUnreadStore.getState().setUnreadCount('k', 6);
        render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
        expect(getList().scrollTop).toBe(400);
        const button = () => screen.getByRole('button', { name: /return to latest/i });
        expect(button()).toHaveTextContent('4');
        layout.clientHeight = 400; // the window grew: msg-4..msg-7 now in view
        ro.fire();
        expect(button()).toHaveTextContent('2');
      } finally {
        ro.restore();
      }
    });

    it('prefers the saved anchor over the first unread message', () => {
      useUnreadStore.getState().setUnreadCount('k', 4);
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-1', offset: 0 });
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(100);
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('4');
    });

    it('Return to Latest clears the unread badge and the next leave clears the anchor', () => {
      useUnreadStore.getState().setUnreadCount('k', 4);
      const { unmount } = render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      const list = getList();
      stubScrollTo(list);
      expect(list.scrollTop).toBe(600);
      fireEvent.click(screen.getByRole('button', { name: /return to latest/i }));
      expect(list.scrollTop).toBe(1000);
      expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
      unmount();
      expect(useChannelScrollStore.getState().getAnchor('k')).toBeUndefined();
    });
    it('keeps a first-unread landing and its badge when the echo of its own scroll samples a taller viewport', () => {
      useUnreadStore.getState().setUnreadCount('k', 4);
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      const list = getList();
      expect(list.scrollTop).toBe(600);
      layout.clientHeight = 700; // transient: 1000 - 600 - 700 < threshold
      fireEvent.scroll(list); // the echo of the landing scroll
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('2');
    });

    it('keeps the restored anchor and its button when the echo of its own scroll samples a taller viewport', () => {
      // The programmatic scroll that restores an anchor fires a scroll event a
      // frame later. If a sibling below the list is mid-way through mount-time
      // layout churn at that instant, the viewport reads taller and the list
      // looks near the bottom; the handler must not overturn the landing.
      const ro = mockResizeObserver();
      try {
        useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
        render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
        const list = getList();
        expect(list.scrollTop).toBe(330);

        layout.clientHeight = 700; // transient: 1000 - 330 - 700 < threshold
        fireEvent.scroll(list); // the echo of the landing scroll
        expect(screen.getByRole('button', { name: /return to latest/i })).toBeVisible();

        layout.clientHeight = 200; // churn settles
        ro.fire();
        expect(list.scrollTop).toBe(330);
        expect(screen.getByRole('button', { name: /return to latest/i })).toBeVisible();

        // A real scroll afterwards is still honoured.
        list.scrollTop = 800;
        fireEvent.scroll(list);
        expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
      } finally {
        ro.restore();
      }
    });

    it('re-derives Return to Latest from geometry when content resizes without a scroll', () => {
      const ro = mockResizeObserver();
      try {
        useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
        render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
        const list = getList();
        expect(screen.getByRole('button', { name: /return to latest/i })).toBeVisible();

        layout.rowHeight = 60; // rows below collapse: 600 - 330 - 200 < threshold
        ro.fire();
        expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();

        layout.rowHeight = 100; // they grow back: far from the bottom again
        ro.fire();
        expect(list.scrollTop).toBe(330);
        expect(screen.getByRole('button', { name: /return to latest/i })).toBeVisible();
      } finally {
        ro.restore();
      }
    });

    it('does not save or restore when persistenceKey is omitted', () => {
      useChannelScrollStore.getState().saveAnchor('ignored', { messageId: 'msg-3', offset: 0 });
      const { unmount } = render(<MessageList messages={rows} currentUserId="user-1" />);
      const list = getList();
      list.scrollTop = 330;
      fireEvent.scroll(list);
      unmount();
      expect(useChannelScrollStore.getState().anchors).toEqual({
        ignored: { messageId: 'msg-3', offset: 0 },
      });
    });
  });

  // ---- onLatestSeen (read marker while viewing, #2006) ----

  describe('onLatestSeen', () => {
    function setVisibility(state: 'visible' | 'hidden') {
      Object.defineProperty(document, 'visibilityState', { value: state, configurable: true });
    }

    // Real geometry, not jsdom's zero rects: "scrolled up" must leave the
    // previous latest row out of the near-bottom band, or an arrival reads as
    // seen. 200px viewport over 100px rows, as in the preservation block.
    let restoreLayout: () => void = () => {};
    let hasFocus: ReturnType<typeof vi.spyOn>;
    const seenLayout = { clientHeight: 200, rowHeight: 100 };
    const shrinkViewport = (px: number) => {
      seenLayout.clientHeight = px;
    };
    beforeEach(() => {
      seenLayout.clientHeight = 200;
      restoreLayout = installLayout(seenLayout);
      resetAllStores();
      // jsdom never reports focus; "seen" requires it.
      hasFocus = vi.spyOn(document, 'hasFocus').mockReturnValue(true);
    });
    afterEach(() => {
      restoreLayout();
      hasFocus.mockRestore();
      setVisibility('visible');
    });
    const getList = () => document.querySelector('.message-list') as HTMLElement;
    const arrival = (id = 'msg-arrived') => ({ ...mockMessage2, id });

    it("fires when another user's message arrives while near the bottom and the document is visible", () => {
      const onLatestSeen = vi.fn();
      const { rerender } = render(
        <MessageList messages={[mockMessage]} currentUserId="user-1" onLatestSeen={onLatestSeen} />
      );
      const arrived = { ...mockMessage2, id: 'msg-arrived' };
      rerender(
        <MessageList
          messages={[mockMessage, arrived]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
    });

    it('does not fire when the arriving message is the user’s own', () => {
      const onLatestSeen = vi.fn();
      const { rerender } = render(
        <MessageList messages={[mockMessage2]} currentUserId="user-1" onLatestSeen={onLatestSeen} />
      );
      const ownArrival = { ...mockMessage, id: 'msg-own-arrived' };
      rerender(
        <MessageList
          messages={[mockMessage2, ownArrival]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(onLatestSeen).not.toHaveBeenCalled();
    });

    it('does not fire when the user has scrolled up', () => {
      const onLatestSeen = vi.fn();
      const { rerender } = render(
        <MessageList messages={rows} currentUserId="user-1" onLatestSeen={onLatestSeen} />
      );
      const list = getList();
      list.scrollTop = 100;
      fireEvent.scroll(list);

      const arrived = { ...mockMessage2, id: 'msg-arrived' };
      rerender(
        <MessageList
          messages={[...rows, arrived]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(onLatestSeen).not.toHaveBeenCalled();
    });

    it('does not fire when the document is hidden', () => {
      setVisibility('hidden');
      const onLatestSeen = vi.fn();
      const { rerender } = render(
        <MessageList messages={[mockMessage]} currentUserId="user-1" onLatestSeen={onLatestSeen} />
      );
      const arrived = { ...mockMessage2, id: 'msg-arrived' };
      rerender(
        <MessageList
          messages={[mockMessage, arrived]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(onLatestSeen).not.toHaveBeenCalled();
    });

    it('fires on Return to Latest when the badge count is greater than zero', () => {
      const onLatestSeen = vi.fn();
      const { rerender } = render(
        <MessageList messages={rows} currentUserId="user-1" onLatestSeen={onLatestSeen} />
      );
      const list = getList();
      Object.defineProperty(list, 'scrollTo', {
        configurable: true,
        value: vi.fn((options: ScrollToOptions) => {
          list.scrollTop = options.top ?? list.scrollTop;
        }),
      });
      list.scrollTop = 100;
      fireEvent.scroll(list);

      // A message arrives while scrolled up — counted, not yet "seen".
      const arrived = { ...mockMessage2, id: 'msg-arrived' };
      rerender(
        <MessageList
          messages={[...rows, arrived]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(onLatestSeen).not.toHaveBeenCalled();
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('1');

      fireEvent.click(screen.getByRole('button', { name: /return to latest/i }));
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
    });

    it('fires when scrolling down reaches the bottom with unseen messages pending', () => {
      const onLatestSeen = vi.fn();
      const { rerender } = render(
        <MessageList messages={rows} currentUserId="user-1" onLatestSeen={onLatestSeen} />
      );
      const list = getList();
      list.scrollTop = 100;
      fireEvent.scroll(list);

      const arrived = { ...mockMessage2, id: 'msg-arrived' };
      rerender(
        <MessageList
          messages={[...rows, arrived]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(onLatestSeen).not.toHaveBeenCalled();

      // Scroll back down to the bottom without using the button (11 rows: 1100px).
      list.scrollTop = 900;
      fireEvent.scroll(list);
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
    });

    it('does not fire while the window is unfocused, then fires once when focus returns', () => {
      hasFocus.mockReturnValue(false);
      const onLatestSeen = vi.fn();
      const { rerender } = render(
        <MessageList messages={[mockMessage]} currentUserId="user-1" onLatestSeen={onLatestSeen} />
      );
      rerender(
        <MessageList
          messages={[mockMessage, arrival()]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(onLatestSeen).not.toHaveBeenCalled();

      hasFocus.mockReturnValue(true);
      act(() => {
        window.dispatchEvent(new Event('focus'));
      });
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
      expect(getList().scrollTop).toBe(200); // the one arrival fits: still at the bottom
      act(() => {
        window.dispatchEvent(new Event('focus')); // nothing new to mark
      });
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
    });

    it('stands on the first of an unfocused burst as soon as it overflows, and marks nothing until the user reads down', () => {
      hasFocus.mockReturnValue(false);
      const onLatestSeen = vi.fn();
      const onLatestLeft = vi.fn();
      const props = { currentUserId: 'user-1', onLatestSeen, onLatestLeft };
      const { rerender } = render(<MessageList messages={rows} {...props} />);
      const burst = ['a1', 'a2', 'a3', 'a4', 'a5'].map((id) => arrival(id));
      for (let n = 1; n <= burst.length; n++) {
        rerender(<MessageList messages={[...rows, ...burst.slice(0, n)]} {...props} />);
      }
      const list = getList();
      // Three rows fit the 200px viewport within the band; the fourth did not,
      // so the list stood on a1 (row 10, at 1000px) then and stopped following.
      expect(onLatestSeen).not.toHaveBeenCalled();
      expect(list.scrollTop).toBe(1000);
      expect(onLatestLeft).toHaveBeenCalledTimes(1);
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('3');

      hasFocus.mockReturnValue(true);
      act(() => {
        window.dispatchEvent(new Event('focus')); // nothing to mark: the burst is unread, not seen
      });
      expect(onLatestSeen).not.toHaveBeenCalled();
      expect(list.scrollTop).toBe(1000);

      // Reading down to the bottom marks it, as any scroll-down does.
      list.scrollTop = 1300;
      fireEvent.scroll(list);
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
      expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
    });

    it('lands on the first unseen row on focus regain when the viewport shrank under a burst that had fitted', () => {
      hasFocus.mockReturnValue(false);
      const onLatestSeen = vi.fn();
      const props = { currentUserId: 'user-1', onLatestSeen };
      const { rerender } = render(<MessageList messages={rows} {...props} />);
      rerender(<MessageList messages={[...rows, arrival('a1'), arrival('a2')]} {...props} />); // fits: pinned at 1200
      expect(getList().scrollTop).toBe(1200);
      shrinkViewport(40); // the window was resized while away: two rows no longer fit
      hasFocus.mockReturnValue(true);
      act(() => {
        window.dispatchEvent(new Event('focus'));
      });
      expect(onLatestSeen).not.toHaveBeenCalled();
      expect(getList().scrollTop).toBe(1000); // standing on a1
      expect(screen.getByRole('button', { name: /return to latest/i })).toBeInTheDocument();
    });

    it('counts arrivals shown while unfocused as unread if the user scrolls up before focus returns', () => {
      hasFocus.mockReturnValue(false);
      const onLatestSeen = vi.fn();
      const onUnseenOnLeave = vi.fn();
      const { rerender, unmount } = render(
        <MessageList
          messages={rows}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
          onUnseenOnLeave={onUnseenOnLeave}
        />
      );
      rerender(
        <MessageList
          messages={[...rows, arrival('a1')]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
          onUnseenOnLeave={onUnseenOnLeave}
        />
      );
      rerender(
        <MessageList
          messages={[...rows, arrival('a1'), arrival('a2')]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
          onUnseenOnLeave={onUnseenOnLeave}
        />
      );
      const list = getList();
      list.scrollTop = 100;
      fireEvent.scroll(list); // scrolled away, still unfocused
      expect(onLatestSeen).not.toHaveBeenCalled();
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('2');
      unmount();
      expect(onUnseenOnLeave).toHaveBeenCalledWith(2);
    });

    it('stands on the first of a batch appended at once when it overflows, instead of pinning and marking', () => {
      // A reconnect backfill commits every missed message in one store update.
      const onLatestSeen = vi.fn();
      const onLatestLeft = vi.fn();
      const props = { currentUserId: 'user-1', onLatestSeen, onLatestLeft };
      const { rerender } = render(<MessageList messages={rows} {...props} />);
      const batch = ['a1', 'a2', 'a3', 'a4', 'a5'].map((id) => arrival(id));
      rerender(<MessageList messages={[...rows, ...batch]} {...props} />);
      const list = getList();
      // Focused and following, but five rows do not fit: the list stands on
      // a1 (row 10, at 1000px) with a3..a5 below, and nothing is marked.
      expect(onLatestSeen).not.toHaveBeenCalled();
      expect(list.scrollTop).toBe(1000);
      expect(onLatestLeft).toHaveBeenCalledTimes(1);
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('3');
      list.scrollTop = 1300;
      fireEvent.scroll(list);
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
    });

    it('pins and marks a batch that fits, and counts a whole batch when scrolled up', () => {
      const onLatestSeen = vi.fn();
      const { rerender } = render(
        <MessageList messages={rows} currentUserId="user-1" onLatestSeen={onLatestSeen} />
      );
      rerender(
        <MessageList
          messages={[...rows, arrival('a1'), arrival('a2')]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(getList().scrollTop).toBe(1200);
      expect(onLatestSeen).toHaveBeenCalledTimes(1);

      const list = getList();
      list.scrollTop = 100;
      fireEvent.scroll(list);
      rerender(
        <MessageList
          messages={[
            ...rows,
            arrival('a1'),
            arrival('a2'),
            arrival('b1'),
            arrival('b2'),
            arrival('b3'),
          ]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('3');
    });

    it("marks an empty thread's first live message, but not the rows a fetch delivers", () => {
      const onLatestSeen = vi.fn();
      const props = { currentUserId: 'user-1', onLatestSeen };
      const { rerender } = render(<MessageList messages={[]} isLoading {...props} />);
      rerender(<MessageList messages={[]} isLoading={false} {...props} />); // loaded, and empty
      rerender(<MessageList messages={[arrival()]} isLoading={false} {...props} />);
      expect(onLatestSeen).toHaveBeenCalledTimes(1);

      // Hydration: the loading cycle ends WITH rows, so they are not arrivals.
      onLatestSeen.mockClear();
      const { rerender: rerender2 } = render(<MessageList messages={[]} isLoading {...props} />);
      rerender2(
        <MessageList messages={[mockMessage, arrival('h1')]} isLoading={false} {...props} />
      );
      expect(onLatestSeen).not.toHaveBeenCalled();
    });

    it('treats a page that replaced an evicted tail as one appended batch, and a deleted tail as nothing', () => {
      const onLatestSeen = vi.fn();
      const props = { currentUserId: 'user-1', onLatestSeen };
      const { rerender, unmount } = render(<MessageList messages={rows} {...props} />);
      // A reconnect that missed more than a page: the store now holds ten rows
      // sharing no id with what was mounted. Every one of them is new.
      const page = Array.from({ length: 10 }, (_, i) => arrival(`p${i}`));
      rerender(<MessageList messages={page} {...props} />);
      expect(onLatestSeen).not.toHaveBeenCalled();
      expect(getList().scrollTop).toBe(0); // standing on p0
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('8');
      unmount();

      // The latest row deleted: nothing new, nothing marked, nothing counted.
      onLatestSeen.mockClear();
      const { rerender: rerender2 } = render(<MessageList messages={rows} {...props} />);
      rerender2(<MessageList messages={rows.slice(0, 9)} {...props} />);
      expect(onLatestSeen).not.toHaveBeenCalled();
      expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
    });

    it('does not count the rows the first fetch appends to cached rows, but counts a later backfill', () => {
      // A remount on cached rows with unread waiting: the landing walks the
      // cached tail, then the fetch appends the messages received while
      // closed. Those are hydration — the open-time read covered them.
      useUnreadStore.getState().setUnreadCount('k', 4);
      const onUnseenOnLeave = vi.fn();
      const props = { currentUserId: 'user-1', persistenceKey: 'k', onUnseenOnLeave };
      const { rerender, unmount } = render(
        <MessageList messages={rows} isLoading={false} {...props} />
      );
      expect(getList().scrollTop).toBe(600);
      rerender(<MessageList messages={rows} isLoading {...props} />);
      const hydrated = [...rows, arrival('h1'), arrival('h2'), arrival('h3'), arrival('h4')];
      rerender(<MessageList messages={hydrated} isLoading={false} {...props} />);
      // A later cycle — a reconnect backfill — is an arrival and is counted.
      rerender(<MessageList messages={hydrated} isLoading {...props} />);
      rerender(
        <MessageList
          messages={[...hydrated, arrival('r1'), arrival('r2')]}
          isLoading={false}
          {...props}
        />
      );
      unmount();
      expect(onUnseenOnLeave).toHaveBeenCalledTimes(1);
      expect(onUnseenOnLeave).toHaveBeenCalledWith(2);
    });

    it('reports an arrival shown while unfocused as unread when the list unmounts before focus returns', () => {
      hasFocus.mockReturnValue(false);
      const onLatestSeen = vi.fn();
      const onUnseenOnLeave = vi.fn();
      const props = { currentUserId: 'user-1', onLatestSeen, onUnseenOnLeave };
      const { rerender, unmount } = render(<MessageList messages={rows} {...props} />);
      rerender(<MessageList messages={[...rows, arrival('a1')]} {...props} />);
      expect(onLatestSeen).not.toHaveBeenCalled();
      unmount(); // switched thread without ever looking
      expect(onUnseenOnLeave).toHaveBeenCalledWith(1);
    });

    it('counts every row of a batch shown while unfocused', () => {
      hasFocus.mockReturnValue(false);
      const onUnseenOnLeave = vi.fn();
      const props = { currentUserId: 'user-1', onUnseenOnLeave };
      const { rerender, unmount } = render(<MessageList messages={rows} {...props} />);
      rerender(<MessageList messages={[...rows, arrival('a1'), arrival('a2')]} {...props} />); // one update, fits
      expect(getList().scrollTop).toBe(1200);
      unmount();
      expect(onUnseenOnLeave).toHaveBeenCalledWith(2);
    });

    it('stands on the earliest unseen row, not the batch, when a later batch overflows while unfocused', () => {
      hasFocus.mockReturnValue(false);
      const onLatestLeft = vi.fn();
      const props = { currentUserId: 'user-1', onLatestLeft };
      const { rerender } = render(<MessageList messages={rows} {...props} />);
      rerender(<MessageList messages={[...rows, arrival('a1')]} {...props} />); // a1 shown, unseen
      const batch = ['a2', 'a3', 'a4', 'a5', 'a6'].map((id) => arrival(id));
      rerender(<MessageList messages={[...rows, arrival('a1'), ...batch]} {...props} />);
      // a1 is row 10, at 1000px; the batch alone would have landed at 1100.
      expect(getList().scrollTop).toBe(1000);
      expect(onLatestLeft).toHaveBeenCalledTimes(1);
      expect(screen.getByRole('button', { name: /return to latest/i })).toHaveTextContent('4');
    });

    it('marks rows a reconnect inserted before a preserved live tail', () => {
      // The store replaced the page with missed rows placed BEFORE the tail
      // that arrived live during the outage; the latest id did not change.
      const onLatestSeen = vi.fn();
      const props = { currentUserId: 'user-1', onLatestSeen };
      const { rerender } = render(<MessageList messages={rows} {...props} />);
      const missed = [arrival('r1'), arrival('r2')];
      rerender(<MessageList messages={[...rows.slice(0, 9), ...missed, rows[9]]} {...props} />);
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
    });

    it('flushes the pending marker and forgets unseen arrivals when the list empties while mounted', () => {
      hasFocus.mockReturnValue(false);
      const onLatestLeft = vi.fn();
      const onUnseenOnLeave = vi.fn();
      const props = { currentUserId: 'user-1', onLatestLeft, onUnseenOnLeave };
      const { rerender, unmount } = render(<MessageList messages={rows} {...props} />);
      rerender(<MessageList messages={[...rows, arrival('a1')]} {...props} />); // shown, unseen
      rerender(<MessageList messages={[]} {...props} />); // a purge emptied the store
      expect(onLatestLeft).toHaveBeenCalledTimes(1);
      unmount();
      expect(onUnseenOnLeave).not.toHaveBeenCalled();
    });

    it('marks the first row after a purge as an arrival, not hydration', () => {
      const onLatestSeen = vi.fn();
      const props = { currentUserId: 'user-1', onLatestSeen };
      const { rerender } = render(<MessageList messages={rows} {...props} />);
      rerender(<MessageList messages={[]} {...props} />); // a purge emptied the store
      rerender(<MessageList messages={[arrival('p1')]} {...props} />); // live, not a fetch
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
    });

    it('does not count an older page prepended by pagination as arrivals', () => {
      const onLatestSeen = vi.fn();
      const onUnseenOnLeave = vi.fn();
      const props = { currentUserId: 'user-1', onLatestSeen, onUnseenOnLeave };
      const { rerender, unmount } = render(<MessageList messages={rows} {...props} />);
      const list = getList();
      list.scrollTop = 100;
      fireEvent.scroll(list); // scrolled up, as a reader who paged back would be
      const older = Array.from({ length: 5 }, (_, i) => ({ ...arrival(`old-${i}`) }));
      rerender(<MessageList messages={[...older, ...rows]} {...props} />); // history, not new
      expect(screen.getByRole('button', { name: /return to latest/i })).not.toHaveTextContent(/\d/);
      expect(onLatestSeen).not.toHaveBeenCalled();
      unmount();
      expect(onUnseenOnLeave).not.toHaveBeenCalled();
    });

    it('does not fire for an edit of the latest row', () => {
      const onLatestSeen = vi.fn();
      const { rerender } = render(
        <MessageList messages={[mockMessage]} currentUserId="user-1" onLatestSeen={onLatestSeen} />
      );
      rerender(
        <MessageList
          messages={[mockMessage, arrival()]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      rerender(
        <MessageList
          messages={[mockMessage, { ...arrival(), content: 'edited' }]}
          currentUserId="user-1"
          onLatestSeen={onLatestSeen}
        />
      );
      expect(onLatestSeen).toHaveBeenCalledTimes(1);
    });

    it('does not fire on a scroll at the bottom, or on Return to Latest, with nothing pending', () => {
      const onLatestSeen = vi.fn();
      render(<MessageList messages={rows} currentUserId="user-1" onLatestSeen={onLatestSeen} />);
      const list = getList();
      Object.defineProperty(list, 'scrollTo', {
        configurable: true,
        value: vi.fn((options: ScrollToOptions) => {
          list.scrollTop = options.top ?? list.scrollTop;
        }),
      });
      list.scrollTop = 800;
      fireEvent.scroll(list); // at the bottom, nothing arrived
      list.scrollTop = 100;
      fireEvent.scroll(list); // up: the button appears with no count
      fireEvent.click(screen.getByRole('button', { name: /return to latest/i }));
      expect(onLatestSeen).not.toHaveBeenCalled();
    });

    it('fires onLatestLeft when the list unmounts, so an owner that stays mounted flushes', () => {
      const onLatestLeft = vi.fn();
      const { unmount } = render(
        <MessageList messages={rows} currentUserId="user-1" onLatestLeft={onLatestLeft} />
      );
      expect(onLatestLeft).not.toHaveBeenCalled();
      unmount();
      expect(onLatestLeft).toHaveBeenCalledTimes(1);
    });

    it('fires onLatestLeft once when the user scrolls up from the bottom', () => {
      const onLatestLeft = vi.fn();
      render(<MessageList messages={rows} currentUserId="user-1" onLatestLeft={onLatestLeft} />);
      const list = getList();
      list.scrollTop = 800;
      fireEvent.scroll(list); // still following
      expect(onLatestLeft).not.toHaveBeenCalled();
      list.scrollTop = 300;
      fireEvent.scroll(list); // left the latest message
      list.scrollTop = 100;
      fireEvent.scroll(list); // further up: already left
      expect(onLatestLeft).toHaveBeenCalledTimes(1);
      list.scrollTop = 800;
      fireEvent.scroll(list); // back to the bottom is not a leave
      expect(onLatestLeft).toHaveBeenCalledTimes(1);
    });
  });

  it('does not re-pin when the user has scrolled up (scroll button case)', () => {
    let roCallback: ResizeObserverCallback | null = null;
    class MockRO {
      constructor(cb: ResizeObserverCallback) {
        roCallback = cb;
      }
      observe = vi.fn();
      disconnect = vi.fn();
      unobserve = vi.fn();
    }
    const OriginalRO = globalThis.ResizeObserver;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (globalThis as any).ResizeObserver = MockRO;
    try {
      render(<MessageList messages={[mockMessage]} currentUserId="user-1" />);
      const list = document.querySelector('.message-list') as HTMLElement;
      Object.defineProperty(list, 'scrollHeight', { value: 1000, writable: true });
      Object.defineProperty(list, 'clientHeight', { value: 200, writable: true });
      // Simulate user being scrolled far up (>150px from bottom threshold)
      list.scrollTop = 100;
      list.dispatchEvent(new Event('scroll'));
      // Now content grows further. Re-pin must NOT happen because the user
      // intentionally scrolled away — that's where "Return to Latest" lives.
      Object.defineProperty(list, 'scrollHeight', { value: 1500, writable: true });
      roCallback?.([] as unknown as ResizeObserverEntry[], {} as ResizeObserver);
      expect(list.scrollTop).toBe(100);
    } finally {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (globalThis as any).ResizeObserver = OriginalRO;
    }
  });
});
