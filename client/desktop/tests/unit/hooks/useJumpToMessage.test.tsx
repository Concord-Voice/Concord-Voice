import { StrictMode, useEffect, useLayoutEffect, useRef, type RefObject } from 'react';
import { act, render, screen } from '@testing-library/react';
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import {
  useJumpToMessage,
  JUMP_REQUEST_BUDGET,
  type JumpOptions,
  type JumpOutcome,
  type UseJumpToMessageResult,
} from '@/renderer/hooks/messaging/useJumpToMessage';
import type { LoadUntilResult } from '@/renderer/hooks/messaging/useMessageFetch';
import type { MessageListHandle } from '@/renderer/components/Chat/MessageList';

// The copy the user reads (spec 3.8). Written out here rather than imported, so a reworded
// notice fails a test instead of passing against its own constant.
const TOO_FAR =
  "Couldn't reach that message. It's too far back to load. Scroll up in the chat, then try again.";
const MISSING = "Couldn't reach that message. It may have been deleted or expired.";
const FAILED = "Couldn't reach that message. Loading older messages failed. Try again in a moment.";

type LoadUntilFn = (
  messageId: string,
  maxRequests: number,
  signal?: AbortSignal
) => Promise<LoadUntilResult>;

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

/** One recorded `loadUntil` call, parked until the test settles it. */
interface ParkedLoad {
  messageId: string;
  max: number;
  signal: AbortSignal | undefined;
  settle: (result: LoadUntilResult) => void;
}

interface Probe {
  jump: UseJumpToMessageResult['jump'] | null;
  /** `channelId` seen by every COMMITTED render, in order (row B10b). */
  committed: Array<string | null>;
  /** Promise of the jump the harness itself started on mount (row B12). */
  autoJump: Promise<JumpOutcome> | null;
}

interface HarnessProps {
  probe: Probe;
  channelId: string | null;
  /** When set, the harness shows the real channel store's active channel instead. */
  channelFromStore?: boolean;
  isHistoryReady: boolean;
  hasInitialHistoryError: boolean;
  loadUntil: LoadUntilFn;
  listRef: RefObject<MessageListHandle | null>;
  switchChannel?: (channelId: string) => boolean;
  /** Start one jump when the component mounts; a ref guards the StrictMode replay. */
  autoStart?: { channelId: string; messageId: string };
}

function Harness(props: Readonly<HarnessProps>) {
  const { probe, autoStart, channelFromStore } = props;
  const storeChannelId = useChannelStore((s) => s.activeChannelId);
  const channelId = channelFromStore ? storeChannelId : props.channelId;
  const { jump, notice } = useJumpToMessage({
    channelId,
    isHistoryReady: props.isHistoryReady,
    hasInitialHistoryError: props.hasInitialHistoryError,
    loadUntil: props.loadUntil,
    listRef: props.listRef,
    switchChannel: props.switchChannel,
  });
  useLayoutEffect(() => {
    probe.jump = jump;
    probe.committed.push(channelId);
  });
  const started = useRef(false);
  useEffect(() => {
    if (!autoStart || started.current) return;
    started.current = true;
    probe.autoJump = jump(autoStart.channelId, autoStart.messageId);
  }, [autoStart, jump, probe]);
  return <output>{notice}</output>;
}

interface SetupOptions {
  channelId?: string | null;
  channelFromStore?: boolean;
  isHistoryReady?: boolean;
  hasInitialHistoryError?: boolean;
  /** Absent: the surface cannot cross channels. `'store'`: writes the real channel store. */
  switchChannel?: 'store' | ((channelId: string) => boolean);
  strict?: boolean;
  autoStart?: { channelId: string; messageId: string };
}

function setup(options: SetupOptions = {}) {
  const probe: Probe = { jump: null, committed: [], autoJump: null };
  const parked: ParkedLoad[] = [];
  const events: string[] = [];
  const loadUntil = vi.fn<LoadUntilFn>(
    (messageId, max, signal) =>
      new Promise<LoadUntilResult>((resolve) => {
        parked.push({ messageId, max, signal, settle: resolve });
      })
  );
  // The fake list: `scrollToMessage` answers from `mounted`, which a test flips to say the row
  // is (or is not) on screen at that moment.
  const state = { mounted: false };
  const handle = {
    scrollToMessage: vi.fn((_messageId: string) => {
      events.push('scroll');
      return state.mounted;
    }),
    scrollToBottomIfNear: vi.fn(),
  };
  const listRef: RefObject<MessageListHandle | null> = { current: handle };
  const storeSwitch = vi.fn((channelId: string) => {
    events.push('switch');
    useChannelStore.getState().setActiveChannel(channelId);
    return true;
  });
  const switchChannel = options.switchChannel === 'store' ? storeSwitch : options.switchChannel;

  const base: HarnessProps = {
    probe,
    channelId: options.channelId === undefined ? 'c1' : options.channelId,
    channelFromStore: options.channelFromStore,
    isHistoryReady: options.isHistoryReady ?? true,
    hasInitialHistoryError: options.hasInitialHistoryError ?? false,
    loadUntil,
    listRef,
    switchChannel,
    autoStart: options.autoStart,
  };
  let current = base;
  const wrap = (props: HarnessProps) =>
    options.strict ? (
      <StrictMode>
        <Harness {...props} />
      </StrictMode>
    ) : (
      <Harness {...props} />
    );
  const view = render(wrap(current));

  return {
    probe,
    parked,
    events,
    loadUntil,
    handle,
    state,
    storeSwitch,
    /** Re-render the harness with changed props (inside RTL's `act`). */
    update(patch: Partial<HarnessProps>) {
      current = { ...current, ...patch };
      view.rerender(wrap(current));
    },
    /** Take the owner away, as leaving the chat surface does. */
    unmount() {
      view.unmount();
    },
    /** Start a jump and keep its outcome readable without awaiting it. */
    async startJump(channelId: string, messageId: string, jumpOptions?: JumpOptions) {
      const jump = probe.jump;
      if (!jump) throw new Error('harness has not committed yet');
      const result: { value: JumpOutcome | undefined } = { value: undefined };
      await act(async () => {
        void jump(channelId, messageId, jumpOptions).then((outcome) => {
          result.value = outcome;
        });
      });
      return result;
    },
    /** Settle the nth parked `loadUntil` and let the hook's reaction run. */
    async settle(index: number, outcome: LoadUntilResult) {
      const call = parked[index];
      if (!call) throw new Error(`no parked loadUntil call at ${index}`);
      await act(async () => {
        call.settle(outcome);
      });
    },
  };
}

type Harnessed = ReturnType<typeof setup>;

/** Let pending microtasks (a resolved jump promise) run inside `act`. */
async function flush() {
  await act(async () => {});
}

function noticeText() {
  return screen.getByRole('status');
}

describe('useJumpToMessage (#3894)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  // control
  it('B1 a row that is already mounted is reached with no paging', async () => {
    const h: Harnessed = setup();
    h.state.mounted = true;

    const outcome = await h.startJump('c1', 'm1');

    expect(h.handle.scrollToMessage).toHaveBeenCalledWith('m1');
    expect(outcome.value).toBe('reached');
    expect(h.loadUntil).not.toHaveBeenCalled();
    expect(noticeText()).toBeEmptyDOMElement();
  });

  // kills: readiness pass removed
  it('B2 nothing runs until isHistoryReady flips', async () => {
    const h = setup({ isHistoryReady: false });

    const outcome = await h.startJump('c1', 'm1');
    await flush();
    // The jump is pending and the hook has touched neither the list nor the fetch.
    expect(outcome.value).toBeUndefined();
    expect(h.handle.scrollToMessage).not.toHaveBeenCalled();
    expect(h.loadUntil).not.toHaveBeenCalled();

    h.update({ isHistoryReady: true });
    await flush();

    // Positive control: the same jump does run once history is ready.
    expect(h.handle.scrollToMessage).toHaveBeenCalledWith('m1');
    expect(h.loadUntil).toHaveBeenCalledTimes(1);
  });

  // kills: found not stored
  it('B3 found: the list is asked again, the jump is reached, loadUntil got the full budget', async () => {
    const h = setup();

    const outcome = await h.startJump('c1', 'm1');
    expect(h.loadUntil).toHaveBeenCalledTimes(1);
    expect(h.loadUntil).toHaveBeenCalledWith('m1', JUMP_REQUEST_BUDGET, expect.any(AbortSignal));
    expect(JUMP_REQUEST_BUDGET).toBe(10);
    expect(outcome.value).toBeUndefined();
    const askedBefore = h.handle.scrollToMessage.mock.calls.length;

    h.state.mounted = true; // the paged row mounts
    await h.settle(0, { status: 'found', requests: 3 });

    expect(h.handle.scrollToMessage.mock.calls.length).toBeGreaterThan(askedBefore);
    expect(outcome.value).toBe('reached');
    // Still one call: a found row is not paged for again.
    expect(h.loadUntil).toHaveBeenCalledTimes(1);
    expect(noticeText()).toBeEmptyDOMElement();
  });

  // kills: pass 6 removed (loops); found not stored
  it('B4 found but never mounted ends missing instead of looping', async () => {
    const h = setup();
    // Bounded fake: if the hook loops, the sixth call ends the run with a different outcome.
    h.loadUntil.mockImplementation(async (messageId, max, signal) => {
      h.parked.push({ messageId, max, signal, settle: () => {} });
      return h.parked.length > 5
        ? { status: 'failed', requests: 0 }
        : { status: 'found', requests: 2 };
    });

    const outcome = await h.startJump('c1', 'm1');
    await flush();

    expect(h.loadUntil).toHaveBeenCalledTimes(1);
    expect(outcome.value).toBe('missing');
    expect(noticeText()).toHaveTextContent(MISSING);
  });

  // kills: budget not subtracted
  it('B5 each interrupted page spends its requests, then the jump is too-far', async () => {
    const h = setup();
    h.loadUntil.mockImplementation(async (messageId, max, signal) => {
      h.parked.push({ messageId, max, signal, settle: () => {} });
      // Bounded fake: a hook that never subtracts would keep paging.
      return h.parked.length > 4
        ? { status: 'failed', requests: 0 }
        : { status: 'interrupted', requests: h.parked.length === 1 ? 4 : 6 };
    });

    const outcome = await h.startJump('c1', 'm1');
    await flush();

    expect(h.parked.map((call) => call.max)).toEqual([10, 6]);
    expect(outcome.value).toBe('too-far');
    expect(noticeText()).toHaveTextContent(TOO_FAR);
  });

  // kills: failure mapping wrong; wrong copy
  it.each([
    ['exhausted', 'missing', MISSING],
    ['failed', 'failed', FAILED],
    ['too-far', 'too-far', TOO_FAR],
  ] as const)('B6 a %s page resolves %s with its exact notice', async (status, expected, copy) => {
    const h = setup();

    const outcome = await h.startJump('c1', 'm1');
    expect(h.loadUntil).toHaveBeenCalledTimes(1);
    expect(noticeText()).toBeEmptyDOMElement();

    await h.settle(0, { status, requests: 1 });

    expect(outcome.value).toBe(expected);
    expect(noticeText()).toHaveTextContent(copy);
  });

  // kills: rejection not mapped to failed
  it('B16 a loadUntil that rejects ends the jump as failed with its notice', async () => {
    const h = setup();
    h.loadUntil.mockImplementationOnce(() => Promise.reject(new Error('boom')));

    const outcome = await h.startJump('c1', 'm1');
    await flush();

    expect(h.loadUntil).toHaveBeenCalledTimes(1);
    expect(outcome.value).toBe('failed');
    expect(noticeText()).toHaveTextContent(FAILED);
  });

  // kills: inline rule ignored
  it('B6 an inline same-channel failure sets no notice, a plain one does', async () => {
    const h = setup();

    const inline = await h.startJump('c1', 'm1', { inline: true });
    await h.settle(0, { status: 'exhausted', requests: 1 });
    expect(inline.value).toBe('missing');
    expect(noticeText()).toBeEmptyDOMElement();

    // Control: the identical failure without `inline` is announced.
    const plain = await h.startJump('c1', 'm2');
    await h.settle(1, { status: 'exhausted', requests: 1 });
    expect(plain.value).toBe('missing');
    expect(noticeText()).toHaveTextContent(MISSING);
  });

  // kills: inline rule ignored
  it('B6 an inline jump that crossed channels still sets the notice', async () => {
    const h = setup({ channelFromStore: true, switchChannel: 'store' });
    act(() => {
      useChannelStore.getState().setActiveChannel('c1');
    });

    const outcome = await h.startJump('c2', 'm1', { inline: true });
    expect(useChannelStore.getState().activeChannelId).toBe('c2');
    await h.settle(0, { status: 'exhausted', requests: 1 });

    expect(outcome.value).toBe('missing');
    expect(noticeText()).toHaveTextContent(MISSING);
  });

  // kills: generation check removed; abort on finish removed
  it('B7 a second jump cancels the first, aborts its paging and ignores its late result', async () => {
    const h = setup();

    const first = await h.startJump('c1', 'm1');
    expect(h.parked).toHaveLength(1);
    const firstSignal = h.parked[0].signal;
    expect(firstSignal?.aborted).toBe(false);

    const second = await h.startJump('c1', 'm2');
    await flush();

    expect(first.value).toBe('cancelled');
    expect(firstSignal?.aborted).toBe(true);
    expect(h.parked).toHaveLength(2);
    expect(h.parked[1].messageId).toBe('m2');
    expect(h.parked[1].signal?.aborted).toBe(false);

    // The first page lands late and says the history ended. It must not end the second jump.
    await h.settle(0, { status: 'exhausted', requests: 1 });
    expect(second.value).toBeUndefined();
    expect(noticeText()).toBeEmptyDOMElement();

    // Positive control: the second jump is alive and finishes on its own page.
    h.state.mounted = true;
    await h.settle(1, { status: 'found', requests: 1 });
    expect(second.value).toBe('reached');
    expect(first.value).toBe('cancelled');
  });

  // kills: abort listener removed
  it('B8 an aborted signal cancels the jump and aborts its paging', async () => {
    const h = setup();
    const controller = new AbortController();

    const outcome = await h.startJump('c1', 'm1', { signal: controller.signal });
    expect(h.parked).toHaveLength(1);
    const pagingSignal = h.parked[0].signal;
    expect(pagingSignal?.aborted).toBe(false);

    await act(async () => {
      controller.abort();
    });

    expect(outcome.value).toBe('cancelled');
    expect(pagingSignal?.aborted).toBe(true);
    expect(noticeText()).toBeEmptyDOMElement();
  });

  // kills: pre-aborted check removed
  it('B8 a signal that is already aborted cancels without paging', async () => {
    const h = setup();
    const controller = new AbortController();
    controller.abort();

    const outcome = await h.startJump('c1', 'm1', { signal: controller.signal });
    await flush();

    expect(outcome.value).toBe('cancelled');
    expect(h.loadUntil).not.toHaveBeenCalled();
    expect(noticeText()).toBeEmptyDOMElement();

    // Control: the jump after it runs normally, so the first did not leave the hook stuck.
    await h.startJump('c1', 'm2');
    expect(h.loadUntil).toHaveBeenCalledTimes(1);
  });

  // kills: mismatch pass removed
  it('B9 leaving the channel mid-jump cancels it, silently, and stops the paging', async () => {
    const h = setup();

    const outcome = await h.startJump('c1', 'm1');
    expect(h.parked).toHaveLength(1);
    const pagingSignal = h.parked[0].signal;

    h.update({ channelId: 'c2' });
    await flush();

    expect(outcome.value).toBe('cancelled');
    expect(pagingSignal?.aborted).toBe(true);
    expect(noticeText()).toBeEmptyDOMElement();

    // The abandoned page lands anyway: nothing further is requested.
    h.state.mounted = true;
    await h.settle(0, { status: 'found', requests: 2 });
    expect(h.loadUntil).toHaveBeenCalledTimes(1);
    expect(outcome.value).toBe('cancelled');
  });

  // kills: crossed exemption removed
  it('B10 a crossed jump survives renders that still show the old channel', async () => {
    // A switch that agrees but has not committed: the list still shows c1.
    const lateSwitch = vi.fn(() => true);
    const h = setup({ channelFromStore: true, switchChannel: lateSwitch });
    act(() => {
      useChannelStore.getState().setActiveChannel('c1');
    });

    const outcome = await h.startJump('c2', 'm1');
    expect(lateSwitch).toHaveBeenCalledWith('c2');
    h.update({ isHistoryReady: false });
    h.update({ isHistoryReady: true });
    await flush();

    // Still showing c1: the jump waits, it neither ends nor pages the wrong channel.
    expect(useChannelStore.getState().activeChannelId).toBe('c1');
    expect(outcome.value).toBeUndefined();
    expect(h.loadUntil).not.toHaveBeenCalled();

    // The switch commits: the same jump now runs in c2.
    act(() => {
      useChannelStore.getState().setActiveChannel('c2');
    });
    await flush();
    expect(h.loadUntil).toHaveBeenCalledTimes(1);
    expect(h.loadUntil).toHaveBeenCalledWith('m1', JUMP_REQUEST_BUDGET, expect.any(AbortSignal));
    h.state.mounted = true;
    await h.settle(0, { status: 'found', requests: 1 });
    expect(outcome.value).toBe('reached');
  });

  // kills: refusal result ignored
  it('B10 a refused switch is missing, with a notice on the channel the user stays in', async () => {
    const refuse = vi.fn(() => false);
    const h = setup({ channelFromStore: true, switchChannel: refuse });
    act(() => {
      useChannelStore.getState().setActiveChannel('c1');
    });

    const outcome = await h.startJump('c2', 'm1');
    await flush();

    expect(refuse).toHaveBeenCalledWith('c2');
    expect(outcome.value).toBe('missing');
    expect(useChannelStore.getState().activeChannelId).toBe('c1');
    expect(noticeText()).toHaveTextContent(MISSING);
    expect(h.loadUntil).not.toHaveBeenCalled();
  });

  // kills: absent switchChannel not refused
  it('B10 a surface without switchChannel cannot cross: cancelled, no notice', async () => {
    const h = setup();

    const outcome = await h.startJump('c2', 'm1');
    await flush();

    expect(outcome.value).toBe('cancelled');
    expect(h.loadUntil).not.toHaveBeenCalled();
    expect(noticeText()).toBeEmptyDOMElement();
  });

  // kills: switchChannel moved after an await
  it('B10b a crossed jump commits only the target channel and switches once, before the first scroll', async () => {
    const h = setup({ channelFromStore: true, switchChannel: 'store' });
    act(() => {
      useChannelStore.getState().setActiveChannel('c1');
    });
    h.state.mounted = true; // the target row is on screen as soon as c2 is
    expect(h.probe.committed.at(-1)).toBe('c1');
    const committedBefore = h.probe.committed.length;

    const jump = h.probe.jump;
    if (!jump) throw new Error('harness has not committed yet');
    const outcome: { value: JumpOutcome | undefined } = { value: undefined };
    // A click handler is synchronous: React commits when it returns, before any microtask,
    // so the switch must already have happened and the first commit must show the target.
    act(() => {
      void jump('c2', 'm1').then((o) => {
        outcome.value = o;
      });
    });
    expect(h.storeSwitch).toHaveBeenCalledTimes(1);
    expect(h.probe.committed.at(-1)).toBe('c2');
    await flush();

    // Positive control: the jump ran and renders were committed for it.
    expect(outcome.value).toBe('reached');
    const during = h.probe.committed.slice(committedBefore);
    expect(during.length).toBeGreaterThan(0);
    // Not one committed render showed the old channel.
    expect(during.every((channelId) => channelId === 'c2')).toBe(true);
    expect(h.storeSwitch).toHaveBeenCalledTimes(1);
    expect(h.events.filter((event) => event === 'switch')).toHaveLength(1);
    expect(h.events.indexOf('switch')).toBeLessThan(h.events.indexOf('scroll'));
  });

  // kills: wait removed
  it('B11 an interrupted page waits for readiness, then resumes with the reduced budget', async () => {
    const h = setup();

    const outcome = await h.startJump('c1', 'm1');
    expect(h.parked).toHaveLength(1);

    // A refetch starts while the page is out, and the page comes back interrupted.
    h.update({ isHistoryReady: false });
    await flush();
    const askedBefore = h.handle.scrollToMessage.mock.calls.length;
    await h.settle(0, { status: 'interrupted', requests: 4 });
    await flush();

    expect(outcome.value).toBeUndefined();
    expect(h.loadUntil).toHaveBeenCalledTimes(1);
    expect(h.handle.scrollToMessage.mock.calls.length).toBe(askedBefore);

    h.update({ isHistoryReady: true });
    await flush();

    expect(h.loadUntil).toHaveBeenCalledTimes(2);
    expect(h.parked[1].max).toBe(JUMP_REQUEST_BUDGET - 4);
    h.state.mounted = true;
    await h.settle(1, { status: 'found', requests: 1 });
    expect(outcome.value).toBe('reached');
  });

  // kills: effect cleanup cancels
  it('B12 under a topmost StrictMode the jump completes and pages once', async () => {
    const h = setup({
      strict: true,
      autoStart: { channelId: 'c1', messageId: 'm1' },
    });
    await flush();

    const autoJump = h.probe.autoJump;
    expect(autoJump).not.toBeNull();
    // The replayed mount effects reached the hook with a pending jump, and it paged exactly once.
    expect(h.loadUntil).toHaveBeenCalledTimes(1);

    const result: { value: JumpOutcome | undefined } = { value: undefined };
    void autoJump?.then((value) => {
      result.value = value;
    });
    h.state.mounted = true;
    await h.settle(0, { status: 'found', requests: 2 });

    expect(result.value).toBe('reached');
    expect(h.loadUntil).toHaveBeenCalledTimes(1);
    expect(h.parked[0].signal?.aborted).toBe(false);
  });

  // kills: owner-unmount cleanup removed (Codex r1 on #3995)
  it('B17 the owner unmounting while a jump waits for readiness cancels it and drops the abort listener', async () => {
    const h = setup({ isHistoryReady: false });
    const controller = new AbortController();
    const removeListener = vi.spyOn(controller.signal, 'removeEventListener');

    const outcome = await h.startJump('c1', 'm1', { signal: controller.signal });
    // Waiting on readiness: no dependency can change once the owner is gone, so nothing else ends it.
    expect(h.loadUntil).not.toHaveBeenCalled();
    expect(outcome.value).toBeUndefined();
    expect(removeListener).not.toHaveBeenCalled();

    h.unmount();
    await flush();

    expect(outcome.value).toBe('cancelled');
    expect(removeListener).toHaveBeenCalledWith('abort', expect.any(Function));
  });

  // kills: owner-unmount cleanup removed (Codex r1 on #3995)
  it('B17b the owner unmounting while a page runs aborts its paging', async () => {
    const h = setup();

    const outcome = await h.startJump('c1', 'm1');
    expect(h.parked).toHaveLength(1);
    const pagingSignal = h.parked[0].signal;
    expect(pagingSignal?.aborted).toBe(false);

    h.unmount();
    await flush();

    expect(outcome.value).toBe('cancelled');
    expect(pagingSignal?.aborted).toBe(true);
  });

  // kills: auth pass removed
  it('B13 a new auth lifecycle mid-jump cancels it', async () => {
    const h = setup();

    const outcome = await h.startJump('c1', 'm1');
    expect(h.loadUntil).toHaveBeenCalledTimes(1);

    act(() => {
      useAuthStore.getState().beginAuthLifecycle('other-account-token', 'other-session');
    });
    // The page lands and the row would be mounted: only the auth pass can stop the jump now.
    h.state.mounted = true;
    const askedBefore = h.handle.scrollToMessage.mock.calls.length;
    await h.settle(0, { status: 'found', requests: 1 });

    expect(outcome.value).toBe('cancelled');
    expect(h.handle.scrollToMessage.mock.calls.length).toBe(askedBefore);
    expect(noticeText()).toBeEmptyDOMElement();
  });

  // kills: pass 4 removed
  it('B14 a channel whose first load failed ends the jump as failed', async () => {
    const h = setup({ hasInitialHistoryError: true });
    h.state.mounted = true; // a skipped pass 4 would reach the row instead

    const outcome = await h.startJump('c1', 'm1');
    await flush();

    expect(outcome.value).toBe('failed');
    expect(noticeText()).toHaveTextContent(FAILED);
    expect(h.loadUntil).not.toHaveBeenCalled();
    expect(h.handle.scrollToMessage).not.toHaveBeenCalled();
  });

  // kills: notice not cleared by the next jump
  it('B15 the next jump clears the previous notice', async () => {
    const h = setup();

    const failed = await h.startJump('c1', 'm1');
    await h.settle(0, { status: 'exhausted', requests: 1 });
    expect(failed.value).toBe('missing');
    expect(noticeText()).toHaveTextContent(MISSING);

    const next = await h.startJump('c1', 'm2');

    expect(next.value).toBeUndefined();
    expect(h.loadUntil).toHaveBeenCalledTimes(2);
    expect(noticeText()).toBeEmptyDOMElement();
  });

  // kills: tag check removed
  it('B15 a notice does not follow the user to another channel', async () => {
    const h = setup();

    await h.startJump('c1', 'm1');
    await h.settle(0, { status: 'exhausted', requests: 1 });
    expect(noticeText()).toHaveTextContent(MISSING);

    h.update({ channelId: 'c2' });

    // Dropped during the render that shows c2: no effect, no jump, no trigger from the hook.
    expect(noticeText()).toBeEmptyDOMElement();
    expect(h.probe.committed.at(-1)).toBe('c2');
  });
});
