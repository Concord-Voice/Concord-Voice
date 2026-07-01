import { render, screen } from '../../../test-utils';
import MessageList from '@/renderer/components/Chat/MessageList';
import { mockMessage, mockMessage2 } from '../../../mocks/fixtures';
import { useChannelScrollStore } from '@/renderer/stores/channelScrollStore';
import { vi } from 'vitest';

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

  it('observes the inner content wrapper (not the scroll container)', () => {
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
      // Exactly one element observed: the .message-list-content wrapper.
      // The previous implementation observed the scroll container + bottom
      // sentinel, neither of which resize on media-load growth.
      expect(observed).toHaveLength(1);
      expect(observed[0].classList.contains('message-list-content')).toBe(true);
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

  // ---- Scroll position preservation (WS3 #7) ----

  describe('scroll position preservation', () => {
    beforeEach(() => {
      useChannelScrollStore.setState({ positions: {}, latestMessageIds: {} });
    });

    it('restores saved scroll position on mount when persistenceKey has a saved value', () => {
      useChannelScrollStore.getState().saveScroll('chan-42', 350);
      render(
        <MessageList
          messages={[mockMessage, mockMessage2]}
          currentUserId="user-1"
          persistenceKey="chan-42"
        />
      );
      const list = document.querySelector('.message-list') as HTMLElement;
      expect(list.scrollTop).toBe(350);
    });

    it('ignores stale saved scroll when the latest message has changed', () => {
      // regression for #2006
      const originalScrollHeight = Object.getOwnPropertyDescriptor(
        HTMLElement.prototype,
        'scrollHeight'
      );
      Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
        configurable: true,
        get() {
          return this.classList?.contains('message-list') ? 1000 : 0;
        },
      });

      try {
        useChannelScrollStore.getState().saveScroll('chan-stale', 350, mockMessage.id);
        render(
          <MessageList
            messages={[mockMessage, mockMessage2]}
            currentUserId="user-1"
            persistenceKey="chan-stale"
          />
        );

        const list = document.querySelector('.message-list') as HTMLElement;
        expect(list.scrollTop).toBe(1000);
      } finally {
        if (originalScrollHeight) {
          Object.defineProperty(HTMLElement.prototype, 'scrollHeight', originalScrollHeight);
        } else {
          delete (HTMLElement.prototype as { scrollHeight?: number }).scrollHeight;
        }
      }
    });

    it('ignores stale saved scroll after cached messages finish loading', () => {
      const originalScrollHeight = Object.getOwnPropertyDescriptor(
        HTMLElement.prototype,
        'scrollHeight'
      );
      Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
        configurable: true,
        get() {
          return this.classList?.contains('message-list') ? 1000 : 0;
        },
      });

      const cachedLast = { ...mockMessage2, id: 'cached-last-message' };
      const fetchedLast = { ...mockMessage2, id: 'fetched-last-message' };

      try {
        useChannelScrollStore.getState().saveScroll('chan-cached-stale', 350, cachedLast.id);
        const { rerender } = render(
          <MessageList
            messages={[mockMessage, cachedLast]}
            currentUserId="user-1"
            isLoading={false}
            persistenceKey="chan-cached-stale"
          />
        );
        const list = document.querySelector('.message-list') as HTMLElement;
        expect(list.scrollTop).toBe(350);

        rerender(
          <MessageList
            messages={[mockMessage, cachedLast]}
            currentUserId="user-1"
            isLoading={true}
            persistenceKey="chan-cached-stale"
          />
        );
        rerender(
          <MessageList
            messages={[mockMessage, fetchedLast]}
            currentUserId="user-1"
            isLoading={false}
            persistenceKey="chan-cached-stale"
          />
        );

        expect(list.scrollTop).toBe(1000);
      } finally {
        if (originalScrollHeight) {
          Object.defineProperty(HTMLElement.prototype, 'scrollHeight', originalScrollHeight);
        } else {
          delete (HTMLElement.prototype as { scrollHeight?: number }).scrollHeight;
        }
      }
    });

    it('keeps saved scroll when only the latest optimistic message server id changed', () => {
      const optimisticLast = {
        ...mockMessage2,
        id: 'temp-last-message',
        clientMessageId: 'client-last-message',
      };
      const confirmedLast = {
        ...mockMessage2,
        id: 'server-last-message',
        clientMessageId: 'client-last-message',
      };

      useChannelScrollStore
        .getState()
        .saveScroll('chan-optimistic', 350, optimisticLast.clientMessageId);

      render(
        <MessageList
          messages={[mockMessage, confirmedLast]}
          currentUserId="user-1"
          persistenceKey="chan-optimistic"
        />
      );

      const list = document.querySelector('.message-list') as HTMLElement;
      expect(list.scrollTop).toBe(350);
    });

    it('saves scroll position on unmount', () => {
      const { unmount } = render(
        <MessageList messages={[mockMessage]} currentUserId="user-1" persistenceKey="chan-save" />
      );
      const list = document.querySelector('.message-list') as HTMLElement;
      Object.defineProperty(list, 'scrollTop', { value: 175, writable: true });
      unmount();
      expect(useChannelScrollStore.getState().getScroll('chan-save')).toBe(175);
    });

    it('keeps saved scroll when mounting during an in-flight load', () => {
      useChannelScrollStore.getState().saveScroll('chan-loading-mount', 350, mockMessage.id);
      const { rerender } = render(
        <MessageList
          messages={[mockMessage]}
          currentUserId="user-1"
          isLoading={true}
          persistenceKey="chan-loading-mount"
        />
      );

      rerender(
        <MessageList
          messages={[mockMessage]}
          currentUserId="user-1"
          isLoading={false}
          persistenceKey="chan-loading-mount"
        />
      );

      const list = document.querySelector('.message-list') as HTMLElement;
      expect(list.scrollTop).toBe(350);
    });

    it('saves the latest scroll position when unmounting during loading', () => {
      const { rerender, unmount } = render(
        <MessageList
          messages={[mockMessage]}
          currentUserId="user-1"
          isLoading={false}
          persistenceKey="chan-loading-save"
        />
      );
      const list = document.querySelector('.message-list') as HTMLElement;
      Object.defineProperty(list, 'scrollTop', { value: 175, writable: true, configurable: true });

      rerender(
        <MessageList
          messages={[mockMessage]}
          currentUserId="user-1"
          isLoading={true}
          persistenceKey="chan-loading-save"
        />
      );
      list.scrollTop = 425;
      unmount();

      expect(useChannelScrollStore.getState().getScroll('chan-loading-save')).toBe(425);
    });

    it('does not restore when there is no saved value (keeps auto-bottom behavior)', () => {
      render(
        <MessageList messages={[mockMessage]} currentUserId="user-1" persistenceKey="chan-new" />
      );
      // No saved value — listRef.scrollTop should stay 0 (jsdom default)
      const list = document.querySelector('.message-list') as HTMLElement;
      expect(list.scrollTop).toBe(0);
    });

    it('does not save or restore when persistenceKey is omitted', () => {
      useChannelScrollStore.getState().saveScroll('ignored', 999);
      const { unmount } = render(<MessageList messages={[mockMessage]} currentUserId="user-1" />);
      const list = document.querySelector('.message-list') as HTMLElement;
      expect(list.scrollTop).toBe(0);
      Object.defineProperty(list, 'scrollTop', { value: 500, writable: true });
      unmount();
      // Store should be unchanged — no side effects without a key
      expect(useChannelScrollStore.getState().getScroll('ignored')).toBe(999);
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
