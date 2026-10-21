import { render, screen, fireEvent, act, within, waitFor, userEvent } from '../../../test-utils';
import { useRef, useState } from 'react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import SearchPanel, { type SearchPanelProps } from '@/renderer/components/Chat/SearchPanel';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { openForeignModal } from '../../../helpers/foreignModal';
import { mockMessage } from '../../../mocks/fixtures';
import { vi } from 'vitest';
import type {
  SearchResultItem,
  SearchStatus,
  UseChannelSearchResult,
} from '@/renderer/hooks/messaging/useChannelSearch';

// The hook has its own suites; here it is a mutable return value the panel renders.
let hookState: UseChannelSearchResult;
let hookCalls: Array<[string | null, { scopes?: string[] } | undefined]>;

vi.mock('@/renderer/hooks/messaging/useChannelSearch', () => ({
  useChannelSearch: (channelId: string | null, options?: { scopes?: string[] }) => {
    hookCalls.push([channelId, options]);
    return hookState;
  },
}));

const CHANNEL = 'channel-1';
const REACHED = '2026-07-17T12:00:00.000Z';
// The day formats in the machine's zone, so it reads as the 17th or the 18th.
const SEARCHED_BACK = /^Searched back to Jul 1[78], 2026\.$/;
const NO_MATCHES = /^No matches since Jul 1[78], 2026\.$/;
const NOT_SEARCHED = "Older messages haven't been searched.";

function mockHook(overrides: Partial<UseChannelSearchResult> = {}): UseChannelSearchResult {
  hookState = {
    results: [],
    status: { kind: 'idle' },
    search: vi.fn(),
    loadMore: vi.fn(),
    cancel: vi.fn(),
    endSession: vi.fn(),
    ...overrides,
  };
  return hookState;
}

function item(id: string, overrides: Partial<SearchResultItem> = {}): SearchResultItem {
  return {
    id,
    channelId: CHANNEL,
    authorName: `Author ${id}`,
    createdAt: '2025-01-01T12:00:00.000Z',
    content: `content of ${id}`,
    ...overrides,
  };
}

function manyResults(count: number): SearchResultItem[] {
  return Array.from({ length: count }, (_, i) => item(`m${i}`));
}

function loadRows(channelId: string, ids: string[]) {
  useChatStore.getState().setMessages(
    channelId,
    ids.map((id) => ({ ...mockMessage, id, channel_id: channelId }))
  );
}

// The owner of the panel hands it the control that opens it, as ChatView does.
function PanelWithOpener() {
  const openerRef = useRef<HTMLButtonElement>(null);
  const [open, setOpen] = useState(true);
  return (
    <>
      <button type="button" ref={openerRef} onClick={() => setOpen(true)}>
        Open search
      </button>
      <SearchPanel
        channelId={CHANNEL}
        isOpen={open}
        onClose={() => setOpen(false)}
        onScrollToMessage={() => undefined}
        returnFocusRef={openerRef}
      />
    </>
  );
}

describe('SearchPanel', () => {
  const defaultProps = {
    channelId: CHANNEL,
    isOpen: true,
    onClose: vi.fn(),
    onScrollToMessage: vi.fn(),
  };

  function renderPanel(query = '', props: Partial<SearchPanelProps> = {}) {
    const utils = render(<SearchPanel {...defaultProps} {...props} />);
    if (query) typeQuery(query);
    return utils;
  }

  function typeQuery(value: string) {
    fireEvent.change(screen.getByPlaceholderText('Search messages...'), { target: { value } });
  }

  function resultsList(): HTMLElement {
    const el = document.querySelector<HTMLElement>('.search-panel-results');
    if (!el) throw new Error('the results list is not rendered');
    return el;
  }

  function statusLine(): HTMLElement {
    const el = document.querySelector<HTMLElement>('.search-panel-status-line');
    if (!el) throw new Error('the status line is not rendered');
    return el;
  }

  function sizeList(list: HTMLElement, size: { scrollHeight: number; clientHeight: number }) {
    Object.defineProperties(list, {
      scrollHeight: { configurable: true, value: size.scrollHeight },
      clientHeight: { configurable: true, value: size.clientHeight },
      scrollTop: { configurable: true, writable: true, value: 0 },
    });
  }

  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    hookCalls = [];
    mockHook();
  });

  describe('input', () => {
    it('returns null when not open', () => {
      const { container } = render(<SearchPanel {...defaultProps} isOpen={false} />);
      expect(container.querySelector('.search-panel-backdrop')).not.toBeInTheDocument();
    });

    it('renders the search input', () => {
      renderPanel();
      expect(screen.getByPlaceholderText('Search messages...')).toBeInTheDocument();
    });

    it('passes the typed query to search', () => {
      renderPanel('hello');
      expect(hookState.search).toHaveBeenCalledWith('hello');
      expect(hookState.cancel).not.toHaveBeenCalled();
    });

    it.each(['', '   '])('cancels instead of searching for %j', (blank) => {
      renderPanel('hello');
      vi.mocked(hookState.search).mockClear();
      typeQuery(blank);
      expect(hookState.cancel).toHaveBeenCalledTimes(1);
      expect(hookState.search).not.toHaveBeenCalled();
    });

    it('clears the query when the channel changes, leaving the session to the hook', () => {
      const { rerender } = renderPanel('hello');
      expect(screen.getByPlaceholderText('Search messages...')).toHaveValue('hello');
      rerender(<SearchPanel {...defaultProps} channelId="channel-2" />);
      expect(screen.getByPlaceholderText('Search messages...')).toHaveValue('');
      expect(hookState.cancel).not.toHaveBeenCalled();
    });
  });

  describe('result cards', () => {
    it('shows the author, date and text of each result', () => {
      mockHook({ results: [item('a', { authorName: 'Test User', content: 'deployment guide' })] });
      renderPanel('deploy');
      expect(screen.getByText('Test User')).toBeInTheDocument();
      expect(screen.getByText('deployment guide')).toBeInTheDocument();
      expect(screen.getByText(/2025/)).toBeInTheDocument();
    });

    it('cuts long text at 120 characters without splitting a surrogate pair', () => {
      mockHook({ results: [item('a', { content: '😀'.repeat(121) })] });
      renderPanel('x');
      expect(screen.getByText('😀'.repeat(120) + '...')).toBeInTheDocument();
    });

    it('keeps text of exactly 120 characters whole', () => {
      mockHook({ results: [item('a', { content: 'y'.repeat(120) })] });
      renderPanel('y');
      expect(screen.getByText('y'.repeat(120))).toBeInTheDocument();
    });
  });

  describe('Jump', () => {
    it('shows only for a message loaded in this channel', () => {
      loadRows(CHANNEL, ['loaded']);
      mockHook({ status: { kind: 'finished' }, results: [item('loaded'), item('elsewhere')] });
      renderPanel('hello');
      const jumps = screen.getAllByRole('button', { name: 'Jump' });
      expect(jumps).toHaveLength(1);
      const card = jumps[0].closest('.search-result-card') as HTMLElement;
      expect(within(card).getByText('Author loaded')).toBeInTheDocument();
    });

    it('is withheld for a message that is only loaded in another channel', () => {
      loadRows('other-channel', ['elsewhere']);
      mockHook({ results: [item('elsewhere', { channelId: 'other-channel' })] });
      renderPanel('hello');
      expect(screen.queryByRole('button', { name: 'Jump' })).not.toBeInTheDocument();
    });

    it('appears once the message loads into the channel', () => {
      mockHook({ results: [item('late')] });
      renderPanel('hello');
      expect(screen.queryByRole('button', { name: 'Jump' })).not.toBeInTheDocument();
      act(() => loadRows(CHANNEL, ['late']));
      expect(screen.getByRole('button', { name: 'Jump' })).toBeInTheDocument();
    });

    it('scrolls to the message and closes the panel', () => {
      loadRows(CHANNEL, ['msg-1']);
      mockHook({ results: [item('msg-1')] });
      renderPanel('hello');
      fireEvent.click(screen.getByRole('button', { name: 'Jump' }));
      expect(defaultProps.onScrollToMessage).toHaveBeenCalledWith('msg-1');
      expect(defaultProps.onClose).toHaveBeenCalled();
    });
  });

  describe('status', () => {
    it.each<{ status: SearchStatus; text: RegExp }>([
      { status: { kind: 'running', reachedAt: REACHED }, text: /Searching… back to/ },
      { status: { kind: 'failed' }, text: /Search stopped\./ },
      { status: { kind: 'more', reachedAt: REACHED }, text: /No matches since/ },
    ])('$status.kind with no results never says "No results found"', ({ status, text }) => {
      mockHook({ status, results: [] });
      renderPanel('hello');
      // The visible line: the live region speaks the same sentence for `more`.
      expect(within(statusLine()).getByText(text)).toBeInTheDocument();
      expect(screen.queryByText(/No results found/)).not.toBeInTheDocument();
    });

    it('paused with no results says when the search resumes, never "No results found"', () => {
      vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] });
      vi.setSystemTime(new Date('2026-07-17T12:00:00.000Z'));
      mockHook({ status: { kind: 'paused', resumeAt: Date.now() + 30_000 }, results: [] });
      renderPanel('hello');
      expect(
        screen.getByText('Paused to avoid overloading the server. Resuming in 30 s.')
      ).toBeInTheDocument();
      expect(screen.queryByText(/No results found/)).not.toBeInTheDocument();
    });

    it('names the day a running search has reached', () => {
      mockHook({ status: { kind: 'running', reachedAt: REACHED } });
      renderPanel('hello');
      expect(screen.getByText(/^Searching… back to Jul 1[78], 2026$/)).toBeInTheDocument();
    });

    it('says only "Searching…" before the first page lands', () => {
      mockHook({ status: { kind: 'running', reachedAt: null } });
      renderPanel('hello');
      expect(screen.getByText('Searching…')).toBeInTheDocument();
    });

    it('names the day searched back to when there are results', () => {
      mockHook({ status: { kind: 'more', reachedAt: REACHED }, results: [item('a')] });
      renderPanel('hello');
      expect(within(statusLine()).getByText(SEARCHED_BACK)).toBeInTheDocument();
      expect(screen.queryByText(/No matches since/)).not.toBeInTheDocument();
    });

    it.each([[[item('a')]], [[]]])(
      'says nothing was searched when no point was reached, and still offers the button (results: %#)',
      (results) => {
        mockHook({ status: { kind: 'more', reachedAt: null }, results });
        renderPanel('hello');
        const line = within(statusLine());
        expect(line.getByText(NOT_SEARCHED)).toBeInTheDocument();
        expect(line.getByRole('button', { name: 'Search older messages' })).toBeInTheDocument();
      }
    );

    it('says the whole history was searched when a finished search has no matches', () => {
      mockHook({ status: { kind: 'finished' }, results: [] });
      renderPanel('hello');
      expect(
        within(statusLine()).getByText('No results found. Searched all messages.')
      ).toBeInTheDocument();
    });

    it('does not claim "No results found" for a finished search that has matches', () => {
      mockHook({ status: { kind: 'finished' }, results: [item('a')] });
      renderPanel('hello');
      // The visible line, not the live region, which says the same sentence.
      const line = document.querySelector<HTMLElement>('.search-panel-status-line');
      expect(line).toHaveTextContent(/^Searched all messages\.$/);
      expect(screen.queryByText(/No results found/)).not.toBeInTheDocument();
    });

    describe('when the index cannot hold more', () => {
      const FULL =
        /^Searched back to Jul 1[78], 2026\. Older messages aren't searched, to limit memory use\.$/;
      const NO_DATE = "Older messages aren't searched, to limit memory use.";

      it.each([[[item('a')]], [[]]])(
        'says how far it reached and why it stopped, with no button (results: %#)',
        (results) => {
          mockHook({ status: { kind: 'full', reachedAt: REACHED }, results });
          renderPanel('hello');
          const line = document.querySelector('.search-panel-status');
          expect(line).toHaveTextContent(FULL);
          expect(screen.queryByRole('button', { name: /Search older|Try again/ })).toBeNull();
          expect(screen.queryByText(/No results found|Searched all messages/)).toBeNull();
          expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        }
      );

      it('announces the same sentence in the live region', () => {
        mockHook({ status: { kind: 'full', reachedAt: REACHED } });
        renderPanel('hello');
        expect(screen.getByRole('status')).toHaveTextContent(FULL);
      });

      it('drops the date when no point was reached', () => {
        mockHook({ status: { kind: 'full', reachedAt: null } });
        renderPanel('hello');
        expect(document.querySelector('.search-panel-status')?.textContent).toBe(NO_DATE);
        expect(screen.getByRole('status').textContent).toBe(NO_DATE);
      });
    });

    it('shows nothing while the search has not started', () => {
      mockHook({ status: { kind: 'idle' } });
      renderPanel('hello');
      expect(document.querySelector('.search-panel-status')).not.toBeInTheDocument();
      expect(screen.queryByText(/No results found/)).not.toBeInTheDocument();
    });

    it.each(['', '   '])('shows no status line for the blank query %j', (blank) => {
      mockHook({ status: { kind: 'finished' }, results: [] });
      renderPanel(blank);
      expect(document.querySelector('.search-panel-status')).not.toBeInTheDocument();
    });

    it('raises an alert only for a failed search', () => {
      mockHook({ status: { kind: 'failed' } });
      const { unmount } = renderPanel('hello');
      expect(screen.getByRole('alert')).toHaveTextContent('Search stopped.');
      unmount();
      // A typed const, not `satisfies`: CI's pinned Semgrep cannot parse that operator.
      const quiet: SearchStatus[] = [
        { kind: 'running', reachedAt: null },
        { kind: 'more', reachedAt: null },
        { kind: 'paused', resumeAt: Date.now() + 5000 },
        { kind: 'full', reachedAt: REACHED },
        { kind: 'finished' },
      ];
      for (const status of quiet) {
        mockHook({ status });
        const view = renderPanel('hello');
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        view.unmount();
      }
    });
  });

  describe('pause countdown', () => {
    it('counts down each second, stops at zero, and leaves the live region alone', () => {
      vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] });
      vi.setSystemTime(new Date('2026-07-17T12:00:00.000Z'));
      mockHook({ status: { kind: 'paused', resumeAt: Date.now() + 3000 } });
      renderPanel('hello');
      const live = screen.getByRole('status');
      expect(screen.getByText(/Resuming in 3 s\./)).toBeInTheDocument();

      act(() => {
        vi.advanceTimersByTime(1000);
      });
      expect(screen.getByText(/Resuming in 2 s\./)).toBeInTheDocument();
      expect(live).toHaveTextContent('Search paused to avoid overloading the server.');
      expect(within(live).queryByText(/Resuming in/)).not.toBeInTheDocument();

      act(() => {
        vi.advanceTimersByTime(5000);
      });
      expect(screen.getByText(/Resuming in 0 s\./)).toBeInTheDocument();
    });

    it('stops ticking once the pause line is gone', () => {
      vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] });
      mockHook({ status: { kind: 'paused', resumeAt: Date.now() + 30_000 } });
      const { unmount } = renderPanel('hello');
      expect(vi.getTimerCount()).toBe(1);
      unmount();
      expect(vi.getTimerCount()).toBe(0);
    });
  });

  describe('older messages', () => {
    it('Search older messages runs one chunk', () => {
      mockHook({ status: { kind: 'more', reachedAt: null } });
      renderPanel('hello');
      fireEvent.click(screen.getByRole('button', { name: 'Search older messages' }));
      expect(hookState.loadMore).toHaveBeenCalledTimes(1);
    });

    it('Try again retries a failed chunk', () => {
      mockHook({ status: { kind: 'failed' } });
      renderPanel('hello');
      expect(screen.getByRole('alert')).toHaveTextContent('Search stopped.');
      fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
      expect(hookState.loadMore).toHaveBeenCalledTimes(1);
    });

    it.each<SearchStatus>([
      { kind: 'idle' },
      { kind: 'running', reachedAt: REACHED },
      { kind: 'paused', resumeAt: Date.now() + 30_000 },
      { kind: 'failed' },
      { kind: 'full', reachedAt: REACHED },
      { kind: 'finished' },
    ])('offers no "Search older messages" button while $kind', (status) => {
      mockHook({ status, results: [item('a')] });
      renderPanel('hello');
      expect(
        screen.queryByRole('button', { name: 'Search older messages' })
      ).not.toBeInTheDocument();
    });

    describe('focus after the button starts a chunk (WCAG 2.4.3)', () => {
      const more: SearchStatus = { kind: 'more', reachedAt: REACHED };
      const failed: SearchStatus = { kind: 'failed' };
      it.each([
        { label: 'Search older messages', from: more, activate: 'click' },
        { label: 'Search older messages', from: more, activate: 'Enter' },
        { label: 'Try again', from: failed, activate: 'click' },
        { label: 'Try again', from: failed, activate: 'Enter' },
      ] as const)(
        '$label by $activate moves focus to the status line',
        async ({ label, from, activate }) => {
          // The real hook reports 'running' as soon as a chunk starts, unmounting the button.
          const loadMore = vi.fn(() => {
            hookState = { ...hookState, status: { kind: 'running', reachedAt: REACHED } };
          });
          mockHook({ status: from, loadMore });
          const user = userEvent.setup();
          const { rerender } = renderPanel('hello');
          const button = screen.getByRole('button', { name: label });
          if (activate === 'click') {
            await user.click(button);
          } else {
            button.focus();
            await user.keyboard('{Enter}');
          }
          expect(loadMore).toHaveBeenCalledTimes(1);
          rerender(<SearchPanel {...defaultProps} />);
          expect(screen.queryByRole('button', { name: label })).not.toBeInTheDocument();
          expect(document.activeElement).not.toBe(document.body);
          await waitFor(() => expect(statusLine()).toHaveFocus());
          expect(statusLine()).toHaveTextContent(/Searching… back to/);
        }
      );

      it('scroll prefetch leaves focus where it was', () => {
        mockHook({ status: { kind: 'more', reachedAt: null }, results: manyResults(30) });
        renderPanel('hello');
        const input = screen.getByPlaceholderText('Search messages...');
        input.focus();
        const list = resultsList();
        sizeList(list, { scrollHeight: 500, clientHeight: 400 });
        list.scrollTop = 100;
        fireEvent.scroll(list);
        expect(hookState.loadMore).toHaveBeenCalledTimes(1);
        expect(input).toHaveFocus();
      });
    });

    it('offers "Try again" only after a failure', () => {
      mockHook({ status: { kind: 'more', reachedAt: null } });
      renderPanel('hello');
      expect(screen.queryByRole('button', { name: 'Try again' })).not.toBeInTheDocument();
    });
  });

  describe('scroll prefetch', () => {
    it('scrolling near the end prefetches; far from the end does not', () => {
      mockHook({ status: { kind: 'more', reachedAt: null }, results: manyResults(30) });
      renderPanel('hello');
      const list = resultsList();
      sizeList(list, { scrollHeight: 3000, clientHeight: 400 });
      fireEvent.scroll(list);
      expect(hookState.loadMore).not.toHaveBeenCalled();
      list.scrollTop = 2100; // 500 px from the end, under 1.5 panel heights (600 px)
      fireEvent.scroll(list);
      expect(hookState.loadMore).toHaveBeenCalledTimes(1);
    });

    it('starts at 1.5 panel heights from the end, not at or beyond it', () => {
      mockHook({ status: { kind: 'more', reachedAt: null }, results: manyResults(30) });
      renderPanel('hello');
      const list = resultsList();
      sizeList(list, { scrollHeight: 3000, clientHeight: 400 });
      list.scrollTop = 2000; // exactly 600 px from the end
      fireEvent.scroll(list);
      expect(hookState.loadMore).not.toHaveBeenCalled();
      list.scrollTop = 2001; // 599 px
      fireEvent.scroll(list);
      expect(hookState.loadMore).toHaveBeenCalledTimes(1);
    });

    it.each<SearchStatus>([
      { kind: 'idle' },
      { kind: 'running', reachedAt: null },
      { kind: 'paused', resumeAt: Date.now() + 30_000 },
      { kind: 'failed' },
      { kind: 'full', reachedAt: REACHED },
      { kind: 'finished' },
    ])('does not prefetch while $kind', (status) => {
      mockHook({ status, results: manyResults(30) });
      renderPanel('hello');
      const list = resultsList();
      sizeList(list, { scrollHeight: 500, clientHeight: 400 });
      list.scrollTop = 100; // at the very end
      fireEvent.scroll(list);
      expect(hookState.loadMore).not.toHaveBeenCalled();
    });
  });

  describe('session', () => {
    it('closing ends the search session', () => {
      mockHook({ status: { kind: 'running', reachedAt: null } });
      renderPanel('hello');
      fireEvent.click(screen.getByRole('button', { name: 'Close' }));
      expect(hookState.endSession).toHaveBeenCalledTimes(1);
      expect(defaultProps.onClose).toHaveBeenCalled();
    });

    it('dismissing from the backdrop ends the search session', () => {
      renderPanel('hello');
      fireEvent.click(screen.getByRole('button', { name: 'Close search' }));
      expect(hookState.endSession).toHaveBeenCalledTimes(1);
      expect(defaultProps.onClose).toHaveBeenCalled();
    });

    it('the panel being closed from outside ends the session and clears the query', () => {
      const { rerender } = renderPanel('hello');
      expect(hookState.endSession).not.toHaveBeenCalled();
      rerender(<SearchPanel {...defaultProps} isOpen={false} />);
      expect(hookState.endSession).toHaveBeenCalledTimes(1);
      rerender(<SearchPanel {...defaultProps} isOpen={true} />);
      expect(screen.getByPlaceholderText('Search messages...')).toHaveValue('');
    });

    it('an open panel does not end the session on its own', () => {
      renderPanel();
      expect(hookState.endSession).not.toHaveBeenCalled();
    });

    it('closing leaves a plain cancel to the session end', () => {
      renderPanel('hello');
      fireEvent.click(screen.getByRole('button', { name: 'Close' }));
      expect(hookState.cancel).not.toHaveBeenCalled();
    });
  });

  describe('live region', () => {
    // It speaks the sentence the visible line shows, so a screen reader hears how a chunk ended.
    it.each<{
      name: string;
      status: SearchStatus;
      results?: SearchResultItem[];
      text: string | RegExp;
    }>([
      { name: 'idle', status: { kind: 'idle' }, text: '' },
      {
        name: 'running',
        status: { kind: 'running', reachedAt: REACHED },
        text: 'Searching older messages',
      },
      {
        name: 'more with matches',
        status: { kind: 'more', reachedAt: REACHED },
        results: [item('a')],
        text: SEARCHED_BACK,
      },
      { name: 'more with none', status: { kind: 'more', reachedAt: REACHED }, text: NO_MATCHES },
      {
        name: 'more with no point reached',
        status: { kind: 'more', reachedAt: null },
        results: [item('a')],
        text: NOT_SEARCHED,
      },
      {
        name: 'paused',
        status: { kind: 'paused', resumeAt: Date.now() + 5000 },
        text: 'Search paused to avoid overloading the server.',
      },
      { name: 'failed', status: { kind: 'failed' }, text: '' },
      {
        name: 'finished with matches',
        status: { kind: 'finished' },
        results: [item('a')],
        text: /^Searched all messages\.$/,
      },
      {
        name: 'finished with none',
        status: { kind: 'finished' },
        text: /^No results found\. Searched all messages\.$/,
      },
    ])('is mounted and announces "$text" for $name', ({ status, results, text }) => {
      mockHook({ status, results: results ?? [] });
      renderPanel('hello');
      const live = screen.getByRole('status');
      expect(live.tagName).toBe('OUTPUT');
      expect(live).toHaveClass('search-panel-live');
      expect(live).toHaveTextContent(text);
      if (text === '') expect(live).toBeEmptyDOMElement();
    });

    it('is mounted before any search, so its first announcement is spoken', () => {
      renderPanel();
      expect(screen.getByRole('status')).toBeEmptyDOMElement();
    });

    it('announces a chunk start again after the panel went quiet', () => {
      mockHook({ status: { kind: 'failed' } });
      const { rerender } = renderPanel('hello');
      const live = screen.getByRole('status');
      expect(live).toBeEmptyDOMElement();
      mockHook({ status: { kind: 'running', reachedAt: null } });
      rerender(<SearchPanel {...defaultProps} />);
      expect(screen.getByRole('status')).toBe(live);
      expect(live).toHaveTextContent('Searching older messages');
    });

    it('announces where a chunk ended once it stops with more to search', () => {
      mockHook({ status: { kind: 'running', reachedAt: REACHED } });
      const { rerender } = renderPanel('hello');
      const live = screen.getByRole('status');
      expect(live).toHaveTextContent('Searching older messages');
      mockHook({ status: { kind: 'more', reachedAt: REACHED }, results: [item('a')] });
      rerender(<SearchPanel {...defaultProps} />);
      expect(screen.getByRole('status')).toBe(live);
      expect(live).toHaveTextContent(SEARCHED_BACK);
    });
  });

  describe('message list subscription', () => {
    // hookCalls gets one entry per render of the panel, which makes a re-render observable.
    it('re-renders an open panel when a message loads into its channel', () => {
      renderPanel();
      const rendered = hookCalls.length;
      act(() => loadRows(CHANNEL, ['late']));
      expect(hookCalls.length).toBeGreaterThan(rendered);
    });

    it('does not re-render a closed panel when a message loads into its channel', () => {
      renderPanel('', { isOpen: false });
      const rendered = hookCalls.length;
      act(() => loadRows(CHANNEL, ['late']));
      expect(hookCalls.length).toBe(rendered);
    });

    it('offers Jump for a row that loaded while the panel was closed', () => {
      mockHook({ results: [item('late')] });
      const { rerender } = renderPanel('', { isOpen: false });
      act(() => loadRows(CHANNEL, ['late']));
      rerender(<SearchPanel {...defaultProps} isOpen={true} />);
      expect(screen.getByRole('button', { name: 'Jump' })).toBeInTheDocument();
    });
  });

  describe('accessible names', () => {
    it('names the panel by its heading', () => {
      renderPanel();
      const panel = screen.getByRole('region', { name: 'Search Messages' });
      expect(panel).toHaveClass('search-panel');
      expect(within(panel).getByRole('heading', { name: 'Search Messages' })).toBeInTheDocument();
    });

    it('names the search field, which its placeholder does not', () => {
      renderPanel();
      expect(screen.getByRole('textbox', { name: 'Search messages' })).toBe(
        screen.getByPlaceholderText('Search messages...')
      );
    });
  });

  describe('Escape', () => {
    function pressEscape(target: Element, init: KeyboardEventInit = {}) {
      fireEvent.keyDown(target, { key: 'Escape', ...init });
    }

    it('closes the panel from the search field and ends the session', () => {
      renderPanel('hello');
      pressEscape(screen.getByPlaceholderText('Search messages...'));
      expect(hookState.endSession).toHaveBeenCalledTimes(1);
      expect(defaultProps.onClose).toHaveBeenCalledTimes(1);
    });

    it('closes the panel from any control inside it', () => {
      renderPanel('hello');
      pressEscape(screen.getByRole('button', { name: 'Close' }));
      expect(hookState.endSession).toHaveBeenCalledTimes(1);
      expect(defaultProps.onClose).toHaveBeenCalledTimes(1);
    });

    it.each(['Enter', 'a', 'Tab'])('does not close the panel for %s', (key) => {
      renderPanel('hello');
      fireEvent.keyDown(screen.getByPlaceholderText('Search messages...'), { key });
      expect(defaultProps.onClose).not.toHaveBeenCalled();
    });

    it('leaves the Escape that ends an IME composition to the composition', () => {
      renderPanel('hello');
      pressEscape(screen.getByPlaceholderText('Search messages...'), { isComposing: true });
      expect(defaultProps.onClose).not.toHaveBeenCalled();
    });

    it('keeps the key from reaching the page-wide shortcut listener', () => {
      renderPanel('hello');
      const reachedDocument = vi.fn();
      document.addEventListener('keydown', reachedDocument);
      pressEscape(screen.getByRole('button', { name: 'Close' }));
      document.removeEventListener('keydown', reachedDocument);
      expect(reachedDocument).not.toHaveBeenCalled();
    });

    it('answers only keys aimed at the panel', () => {
      renderPanel('hello');
      pressEscape(document.body);
      expect(defaultProps.onClose).not.toHaveBeenCalled();
    });

    it('leaves the Escape of a modal dialog in front to that dialog', () => {
      renderPanel('hello');
      const { input } = openForeignModal();
      pressEscape(input);
      expect(defaultProps.onClose).not.toHaveBeenCalled();
    });
  });

  describe('focus when the panel closes', () => {
    it.each([
      {
        how: 'the Close button',
        close: () => fireEvent.click(screen.getByRole('button', { name: 'Close' })),
      },
      {
        how: 'the backdrop',
        close: () => fireEvent.click(screen.getByRole('button', { name: 'Close search' })),
      },
      {
        how: 'Escape',
        close: () =>
          fireEvent.keyDown(screen.getByPlaceholderText('Search messages...'), { key: 'Escape' }),
      },
    ])('closing with $how returns focus to the control that opens the panel', ({ close }) => {
      render(<PanelWithOpener />);
      expect(screen.getByPlaceholderText('Search messages...')).toHaveFocus();
      close();
      expect(screen.queryByPlaceholderText('Search messages...')).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Open search' })).toHaveFocus();
    });

    it('leaves focus alone when Jump closes the panel, for the scroll to place', () => {
      loadRows(CHANNEL, ['msg-1']);
      mockHook({ results: [item('msg-1')] });
      render(<PanelWithOpener />);
      fireEvent.click(screen.getByRole('button', { name: 'Jump' }));
      expect(screen.queryByPlaceholderText('Search messages...')).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Open search' })).not.toHaveFocus();
    });

    it('closes without a control to return to', () => {
      renderPanel('hello');
      fireEvent.click(screen.getByRole('button', { name: 'Close' }));
      expect(defaultProps.onClose).toHaveBeenCalledTimes(1);
    });
  });

  // jsdom applies no stylesheet, so the rules are read from the source.
  describe('focus rings', () => {
    const css = readFileSync(
      resolve(__dirname, '../../../../src/renderer/components/Chat/SearchPanel.css'),
      'utf8'
    ).replace(/\/\*[\s\S]*?\*\//g, '');

    function ruleBody(selector: string): string | undefined {
      for (const block of css.split('}')) {
        const [head, body] = block.split('{');
        if (body !== undefined && head.trim() === selector) return body;
      }
      return undefined;
    }

    it.each([
      '.search-panel-close',
      '.search-result-jump',
      '.search-panel-more-button',
      '.search-panel-status-line',
    ])('%s draws its own ring in --state-focused, not the UA default', (selector) => {
      expect(ruleBody(`${selector}:focus-visible`)).toMatch(
        /outline:\s*2px solid var\(--state-focused\);/
      );
    });
  });

  describe('server-wide search', () => {
    const serverProps = {
      showServerWideToggle: true,
      accessibleChannelIds: ['channel-1', 'channel-2', 'channel-3'],
    };

    it('shows the toggle only when asked', () => {
      const { unmount } = renderPanel('', { showServerWideToggle: true });
      expect(screen.getByText('Search all channels in this server')).toBeInTheDocument();
      unmount();
      renderPanel('', { showServerWideToggle: false });
      expect(screen.queryByText('Search all channels in this server')).not.toBeInTheDocument();
    });

    it('searches the open channel until the toggle is on, then every accessible channel', () => {
      renderPanel('hello', serverProps);
      expect(hookCalls.at(-1)).toEqual([CHANNEL, { scopes: undefined }]);

      fireEvent.click(screen.getByRole('checkbox'));
      expect(hookCalls.at(-1)).toEqual([CHANNEL, { scopes: serverProps.accessibleChannelIds }]);
      // The query is re-run over the new scopes.
      expect(hookState.search).toHaveBeenCalledTimes(2);
      expect(hookState.search).toHaveBeenLastCalledWith('hello');
    });
  });
});
