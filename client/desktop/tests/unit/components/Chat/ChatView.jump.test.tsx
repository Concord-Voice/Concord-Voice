/**
 * #3894 end to end: a Jump on a search hit, through the real ChatView, MessageList,
 * useMessageFetch, useJumpToMessage and SearchPanel. Only the network (MSW), the
 * crypto service, the composer, the row body and the search hook's result list are
 * stand-ins. Every assertion reads the DOM or the wire.
 */
import { StrictMode } from 'react';
import { render as bareRender, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { BrowserRouter } from 'react-router';
import { http, HttpResponse } from 'msw';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { server } from '../../../mocks/server';
import { ModalProvider } from '@/renderer/components/ui/ModalContext';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import type { Channel } from '@/renderer/types/chat';
import type { SearchResultItem } from '@/renderer/hooks/messaging/useChannelSearch';
import { mockUser, mockChannel } from '../../../mocks/fixtures';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred, type Deferred } from '../../../helpers/deferred';
import { settled } from '../../../helpers/settled';

// ---- Stand-ins for what this test is not about -----------------------------------------

vi.mock('@/renderer/services/messaging/pinService', () => ({
  pinMessage: vi.fn(),
  unpinMessage: vi.fn(),
  getChannelPins: vi.fn(async () => []),
}));

// The row body: the wrapper MessageList renders around it carries the id, the focus
// and the highlight, which is what this test is about.
vi.mock('@/renderer/components/Chat/Message', () => ({
  default: ({ message }: { message: { content: string } }) => <p>{message.content}</p>,
}));

vi.mock('@/renderer/components/Chat/MessageInput', () => ({
  default: () => <textarea className="message-input-textarea" aria-label="Message" />,
}));

vi.mock('@/renderer/hooks/messaging/useChannelSubscription', () => ({
  useChannelSubscription: vi.fn(),
}));

vi.mock('@/renderer/hooks/messaging/useMessaging', () => ({
  useMessaging: () => ({
    sendMessage: vi.fn(),
    markDelivered: vi.fn(),
    sendTyping: vi.fn(),
    getPendingCount: vi.fn(() => 0),
    getPendingMessagesForChannel: vi.fn(() => []),
  }),
}));

// Decryption is the identity: the rows below are already plain text.
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    createChannelOperationGuard: vi.fn(() => ({ assertCurrent: vi.fn() })),
    getChannelKey: vi.fn(async () => ({})),
    getChannelKeyByVersion: vi.fn(async () => ({})),
    decryptWithKey: vi.fn(async (ciphertext: string) => ciphertext),
    decryptForChannel: vi.fn(async (_channelId: string, ciphertext: string) => ciphertext),
    decryptForChannelWithVersion: vi.fn(
      async (_channelId: string, ciphertext: string) => ciphertext
    ),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

// The only mocked hook the brief allows: a fixed result list, whatever is typed.
const HITS: SearchResultItem[] = [
  {
    id: 'b-040',
    channelId: 'channel-b',
    authorName: 'Robin',
    createdAt: '2025-01-01T12:00:00Z',
    content: 'ferry timetable for the north pier',
  },
  {
    id: 'a-040',
    channelId: 'channel-a',
    authorName: 'Sam',
    createdAt: '2025-01-01T12:00:00Z',
    content: 'lunch order for friday',
  },
];
vi.mock('@/renderer/hooks/messaging/useChannelSearch', () => ({
  useChannelSearch: () => ({
    results: HITS,
    status: { kind: 'idle' },
    search: vi.fn(),
    loadMore: vi.fn(),
    cancel: vi.fn(),
    endSession: vi.fn(),
  }),
}));

import ChatView from '@/renderer/components/Chat/ChatView';
import { getChannelPins } from '@/renderer/services/messaging/pinService';

// ---- The server: channels A, B and C -----------------------------------------------------

const API = 'http://localhost:8080';
const BASE_TIME = Date.parse('2025-01-01T00:00:00Z');

/** Rows `prefix-001 … prefix-<count>`, newest first: the order the list route answers in. */
function history(prefix: string, count: number) {
  return Array.from({ length: count }, (_, i) => {
    const n = count - i;
    const created = new Date(BASE_TIME + n * 60_000).toISOString();
    return {
      id: `${prefix}-${String(n).padStart(3, '0')}`,
      user_id: 'user-2',
      username: 'robin',
      display_name: 'Robin',
      content: `message ${prefix}-${String(n).padStart(3, '0')}`,
      type: 'user',
      created_at: created,
      updated_at: created,
    };
  });
}

// A and B are 230 rows deep: the open page is 50 and a jump pages 100 at a time, so
// row 040 sits on the third page. C is a short channel to click over to.
const HISTORIES: Record<string, ReturnType<typeof history>> = {
  'channel-a': history('a', 230),
  'channel-b': history('b', 230),
  'channel-c': history('c', 20),
};

interface ListRequest {
  channelId: string;
  before: string | null;
  limit: number;
}
let requests: ListRequest[] = [];
/** Parked requests the server has answered, so D2 can tell the stale page was delivered. */
let parkedAnswered = 0;
/** One list request held back until the test lets it go (D2). */
let parked: {
  channelId: string;
  before: string;
  arrived: Deferred<void>;
  release: Deferred<void>;
} | null = null;

const requestsFor = (channelId: string) => requests.filter((r) => r.channelId === channelId);

const channel = (id: string, name: string, position: number): Channel => ({
  ...mockChannel,
  id,
  name,
  position,
});

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  requests = [];
  parkedAnswered = 0;
  parked = null;
  // mockReset restores the factory's `vi.fn(impl)` default: no pins.
  vi.mocked(getChannelPins).mockReset();
  useAuthStore.getState().setAccessToken('test-token-123');
  useUserStore.setState({ user: mockUser });
  useServerStore.setState({ activeServerId: 'server-1' });
  useChannelStore.setState({
    currentServerId: 'server-1',
    channels: [
      channel('channel-a', 'alpha', 0),
      channel('channel-b', 'beta', 1),
      channel('channel-c', 'gamma', 2),
    ],
    activeChannelId: 'channel-a',
  });
  server.use(
    // Whatever refreshes the server's channel list gets the same three channels back.
    http.get(`${API}/api/v1/servers/:id/channels`, () =>
      HttpResponse.json({ channels: useChannelStore.getState().channels })
    ),
    http.get(`${API}/api/v1/channels/:channelId/messages`, async ({ request, params }) => {
      const channelId = String(params.channelId);
      const url = new URL(request.url);
      const before = url.searchParams.get('before');
      const limit = Number(url.searchParams.get('limit'));
      requests.push({ channelId, before, limit });
      const hold = parked;
      if (hold && hold.channelId === channelId && hold.before === before) {
        hold.arrived.resolve();
        await hold.release.promise;
        parkedAnswered += 1;
      }
      const all = HISTORIES[channelId] ?? [];
      // The cursor is a message id; a cursor the server cannot find answers nothing.
      const from = before ? all.findIndex((m) => m.id === before) + 1 : 0;
      const page = before && from === 0 ? [] : all.slice(from, from + limit);
      return HttpResponse.json({ messages: page.map((m) => ({ ...m, channel_id: channelId })) });
    })
  );
});

afterEach(() => {
  server.resetHandlers();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
});

// ---- jsdom has no layout: the geometry MessageList reads, as MessageList.test.tsx injects it ----

/** The scroll container is `clientHeight` tall and every row `rowHeight` tall, top to bottom. */
function installLayout(layout: { clientHeight: number; rowHeight: number }) {
  const proto = Element.prototype;
  const original = {
    rect: proto.getBoundingClientRect,
    scrollHeight: Object.getOwnPropertyDescriptor(proto, 'scrollHeight'),
    clientHeight: Object.getOwnPropertyDescriptor(proto, 'clientHeight'),
  };
  const rect = (top: number, bottom: number) =>
    ({ top, bottom, left: 0, right: 0, width: 0, height: bottom - top, x: 0, y: top }) as DOMRect;
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
    if (original.scrollHeight) Object.defineProperty(proto, 'scrollHeight', original.scrollHeight);
    if (original.clientHeight) Object.defineProperty(proto, 'clientHeight', original.clientHeight);
  };
}

let restoreLayout: () => void = () => {};
let scrollIntoView: ReturnType<typeof vi.fn>;
beforeEach(() => {
  restoreLayout = installLayout({ clientHeight: 400, rowHeight: 100 });
  scrollIntoView = vi.fn();
  Object.defineProperty(Element.prototype, 'scrollIntoView', {
    configurable: true,
    writable: true,
    value: scrollIntoView,
  });
});
afterEach(() => restoreLayout());

// ---- The page ---------------------------------------------------------------------------

/** Stands in for the channel list: a click on one of these is the user opening a channel. */
function ChannelPicker() {
  const channels = useChannelStore((s) => s.channels);
  return (
    <nav aria-label="Channels">
      {channels.map((c) => (
        <button
          key={c.id}
          type="button"
          onClick={() => useChannelStore.getState().setActiveChannel(c.id)}
        >
          {`Open ${c.name}`}
        </button>
      ))}
    </nav>
  );
}

/** Bare RTL render: StrictMode has to be the topmost element for effects to replay. */
function renderApp() {
  return bareRender(
    <StrictMode>
      <BrowserRouter>
        <ModalProvider>
          <ChannelPicker />
          <ChatView />
        </ModalProvider>
      </BrowserRouter>
    </StrictMode>
  );
}

const headerName = () => document.querySelector('.chat-header-name')?.textContent;
const rowOf = (id: string) => document.querySelector<HTMLElement>(`[data-message-id="${id}"]`);
const searchButton = () => screen.getByRole('button', { name: 'Search messages' });
const panelHeading = () => screen.queryByRole('heading', { name: 'Search Messages' });
const cardFor = (hit: SearchResultItem) => {
  const card = screen.getByText(hit.content).closest<HTMLElement>('.search-result-card');
  if (!card) throw new Error('search card not found');
  return card;
};

async function openSearchAndJumpTo(
  user: ReturnType<typeof userEvent.setup>,
  hit: SearchResultItem
) {
  await user.click(searchButton());
  expect(searchButton()).toHaveAttribute('aria-expanded', 'true');
  expect(panelHeading()).toBeInTheDocument();
  await user.click(within(cardFor(hit)).getByRole('button', { name: 'Jump' }));
}

const LANDED = { timeout: 5000 };

describe('ChatView: Jump to a message that is not loaded (#3894)', () => {
  // kills: readiness gate removed
  it('D1 a hit in another channel switches to it, pages to the row, then focuses and highlights it', async () => {
    const user = userEvent.setup();
    renderApp();
    await screen.findByText('message a-230');
    expect(headerName()).toBe('alpha');
    expect(requestsFor('channel-b')).toHaveLength(0);

    await openSearchAndJumpTo(user, HITS[0]);

    // The jump lands: the row is focused and carries the highlight.
    await waitFor(() => {
      const row = rowOf('b-040');
      expect(row).not.toBeNull();
      expect(row).toHaveFocus();
      expect(row).toHaveClass('message-highlight');
    }, LANDED);
    const target = rowOf('b-040');

    expect(headerName()).toBe('beta');
    expect(scrollIntoView).toHaveBeenCalledTimes(1);
    expect(scrollIntoView.mock.contexts[0]).toBe(target);
    expect(document.querySelectorAll('.message-highlight')).toHaveLength(1);

    // The search panel closed as the channel changed, and no failure line is shown.
    await waitFor(() => expect(searchButton()).toHaveAttribute('aria-expanded', 'false'));
    expect(panelHeading()).not.toBeInTheDocument();
    expect(screen.queryByText(/Couldn't reach that message/)).not.toBeInTheDocument();

    // On the wire: the channel opened with its newest page, then each page asked for the
    // rows before the oldest one the list held.
    expect(requestsFor('channel-b')).toEqual([
      { channelId: 'channel-b', before: null, limit: 50 },
      { channelId: 'channel-b', before: 'b-181', limit: 100 },
      { channelId: 'channel-b', before: 'b-081', limit: 100 },
    ]);
  });

  // kills: mismatch pass removed
  it('D2 opening another channel while the jump is paging stops it: no more pages, no notice, no highlight', async () => {
    const user = userEvent.setup();
    parked = {
      channelId: 'channel-b',
      before: 'b-181',
      arrived: deferred(),
      release: deferred(),
    };
    const hold = parked;
    renderApp();
    await screen.findByText('message a-230');

    await openSearchAndJumpTo(user, HITS[0]);
    // The jump is mid-flight: B opened and its second page is on the wire, parked.
    await hold.arrived.promise;
    expect(headerName()).toBe('beta');
    expect(requestsFor('channel-b').map((r) => r.before)).toEqual([null, 'b-181']);

    // The user opens C, then comes back to B while the old page is still out.
    await user.click(screen.getByRole('button', { name: 'Open gamma' }));
    await screen.findByText('message c-020');
    expect(headerName()).toBe('gamma');
    await user.click(screen.getByRole('button', { name: 'Open beta' }));
    await screen.findByText('message b-230');
    expect(headerName()).toBe('beta');
    const requestsBeforeRelease = requestsFor('channel-b').length;

    // The old page lands on a list that is showing B again; the cancelled jump must not
    // use it to carry on paging.
    hold.release.resolve();
    // The page was asked for on the earlier visit to B, so it is dropped. Publishing it would
    // bring back anything purged or deleted while the chat was on C.
    await settled(() => parkedAnswered, 1);
    await settled(() => document.querySelectorAll('[data-message-id]').length, 0, {
      quietTurns: 30,
    });
    expect(screen.queryByText('message b-100')).not.toBeInTheDocument();
    await settled(() => requestsFor('channel-b').length, requestsBeforeRelease, { quietTurns: 30 });

    expect(requestsFor('channel-b').map((r) => r.before)).not.toContain('b-081');
    expect(requestsFor('channel-b')).toHaveLength(requestsBeforeRelease);
    expect(scrollIntoView).not.toHaveBeenCalled();
    expect(document.querySelector('.message-highlight')).toBeNull();
    expect(rowOf('b-040')).toBeNull();
    expect(screen.queryByText(/Couldn't reach that message/)).not.toBeInTheDocument();
  });

  // control
  it('D3 an unloaded hit in the open channel pages and highlights, and the panel closes', async () => {
    const user = userEvent.setup();
    renderApp();
    await screen.findByText('message a-230');
    expect(rowOf('a-040')).toBeNull();

    await openSearchAndJumpTo(user, HITS[1]);

    await waitFor(() => {
      const row = rowOf('a-040');
      expect(row).not.toBeNull();
      expect(row).toHaveFocus();
      expect(row).toHaveClass('message-highlight');
    }, LANDED);
    expect(scrollIntoView).toHaveBeenCalledTimes(1);
    expect(scrollIntoView.mock.contexts[0]).toBe(rowOf('a-040'));

    // Still in A; a same-channel jump closes the panel when it lands.
    expect(headerName()).toBe('alpha');
    await waitFor(() => expect(searchButton()).toHaveAttribute('aria-expanded', 'false'));
    expect(panelHeading()).not.toBeInTheDocument();
    expect(screen.queryByText(/Couldn't reach that message/)).not.toBeInTheDocument();

    // StrictMode opens A twice; the paging requests are the ones carrying a cursor.
    const paging = requestsFor('channel-a').filter((r) => r.before !== null);
    expect(paging).toEqual([
      { channelId: 'channel-a', before: 'a-181', limit: 100 },
      { channelId: 'channel-a', before: 'a-081', limit: 100 },
    ]);
    expect(requestsFor('channel-b')).toHaveLength(0);
  });
});

// ---- A pinned-message Jump closes its panel, so a failed jump must not leave focus on <body> ----

const MISSING_COPY = "Couldn't reach that message. It may have been deleted or expired.";

/** A pin shaped as the pins route sends it; `decryptPins` reads the identity-decrypt stand-in. */
const pinFor = (id: string) => ({
  id,
  channel_id: 'channel-a',
  user_id: 'user-2',
  username: 'robin',
  display_name: 'Robin',
  content: `pinned ${id}`,
  type: 'user',
  created_at: '2025-01-01T12:00:00Z',
  updated_at: '2025-01-01T12:00:00Z',
});

async function jumpFromPinnedPanel(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Pinned messages' }));
  const jump = await screen.findByRole('button', { name: 'Jump' });
  expect(screen.getByRole('textbox', { name: 'Message' })).not.toHaveFocus();
  await user.click(jump);
  // The panel closes with the click; its Jump button, which held focus, is gone.
  expect(screen.queryByRole('button', { name: 'Jump' })).not.toBeInTheDocument();
}

describe('ChatView: a pinned-message Jump keeps focus in the chat (#3894)', () => {
  // kills: focusComposerIfLost not called after a failed jump; isJumpFailure gate dropped
  it('D4 a pin whose row cannot be reached says why and hands focus to the composer', async () => {
    vi.mocked(getChannelPins).mockResolvedValue([pinFor('gone')]);
    const user = userEvent.setup();
    renderApp();
    await screen.findByText('message a-230');

    await jumpFromPinnedPanel(user);

    await screen.findByText(MISSING_COPY, undefined, LANDED);
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Message' })).toHaveFocus());
    expect(scrollIntoView).not.toHaveBeenCalled();
    expect(document.querySelector('.message-highlight')).toBeNull();
  });

  // control; kills: the composer takes focus on every outcome
  it('D5 a pin that is reached leaves focus on its row, not the composer', async () => {
    vi.mocked(getChannelPins).mockResolvedValue([pinFor('a-040')]);
    const user = userEvent.setup();
    renderApp();
    await screen.findByText('message a-230');
    expect(rowOf('a-040')).toBeNull();

    await jumpFromPinnedPanel(user);

    await waitFor(() => {
      const row = rowOf('a-040');
      expect(row).not.toBeNull();
      expect(row).toHaveFocus();
    }, LANDED);
    expect(screen.getByRole('textbox', { name: 'Message' })).not.toHaveFocus();
    expect(screen.queryByText(MISSING_COPY)).not.toBeInTheDocument();
  });
});
