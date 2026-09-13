import { act, fireEvent, render, screen } from '../../../test-utils';
import MessageList from '@/renderer/components/Chat/MessageList';
import { mockMessage, mockMessage2 } from '../../../mocks/fixtures';
import { useChannelScrollStore } from '@/renderer/stores/chat/channelScrollStore';
import { vi } from 'vitest';
import { StrictMode } from 'react';
import { render as bareRender } from '@testing-library/react';

// Mock the Message component to simplify testing
vi.mock('@/renderer/components/Chat/Message', () => ({
  default: ({ message }: { message: any }) => <div data-testid="message">{message.content}</div>,
}));

describe('MessageList', () => {
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

  describe('scroll position preservation', () => {
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

    let restoreLayout: () => void = () => {};
    // 200px viewport over ten 100px rows: 1000px of content, bottom at 800.
    const layout = { clientHeight: 200, rowHeight: 100 };

    beforeEach(() => {
      layout.rowHeight = 100;
      layout.clientHeight = 200;
      restoreLayout = installLayout(layout);
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

      rerender(<MessageList messages={[]} currentUserId="user-1" persistenceKey="k" />);
      rerender(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(1000);
      expect(returnToLatest()).not.toBeInTheDocument();
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

    it('falls back to the bottom when the anchored message is no longer in the list', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'gone', offset: 0 });
      render(<MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />);
      expect(getList().scrollTop).toBe(1000);
      expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
    });

    it('Return to Latest puts the list back in following mode so the next leave clears the anchor', () => {
      useChannelScrollStore.getState().saveAnchor('k', { messageId: 'msg-3', offset: 30 });
      const { unmount } = render(
        <MessageList messages={rows} currentUserId="user-1" persistenceKey="k" />
      );
      const list = getList();
      stubScrollTo(list);
      expect(list.scrollTop).toBe(330);
      fireEvent.click(screen.getByRole('button', { name: /return to latest/i }));
      expect(list.scrollTop).toBe(1000);
      expect(screen.queryByRole('button', { name: /return to latest/i })).not.toBeInTheDocument();
      unmount();
      expect(useChannelScrollStore.getState().getAnchor('k')).toBeUndefined();
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
