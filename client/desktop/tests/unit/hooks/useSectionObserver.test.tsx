import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, screen } from '../../test-utils';
import { useRef } from 'react';
import { useSectionObserver } from '@/renderer/hooks/ui/useSectionObserver';
import { resetAllStores } from '../../helpers/store-helpers';

interface CapturedInstance {
  callback: IntersectionObserverCallback;
  options: IntersectionObserverInit | undefined;
  observe: ReturnType<typeof vi.fn>;
  disconnect: ReturnType<typeof vi.fn>;
}

let instances: CapturedInstance[] = [];

class FakeIntersectionObserver {
  callback: IntersectionObserverCallback;
  options: IntersectionObserverInit | undefined;
  observe = vi.fn();
  unobserve = vi.fn();
  disconnect = vi.fn();
  takeRecords = vi.fn(() => []);
  root = null;
  rootMargin = '';
  thresholds: ReadonlyArray<number> = [];

  constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
    this.callback = callback;
    this.options = options;
    instances.push({
      callback,
      options,
      observe: this.observe,
      disconnect: this.disconnect,
    });
  }
}

function makeEntry(id: string, isIntersecting: boolean, top: number): IntersectionObserverEntry {
  const target = document.getElementById(id) as Element;
  return {
    target,
    isIntersecting,
    boundingClientRect: { top } as DOMRectReadOnly,
    intersectionRatio: isIntersecting ? 1 : 0,
    intersectionRect: {} as DOMRectReadOnly,
    rootBounds: null,
    time: 0,
  } as IntersectionObserverEntry;
}

function Harness({ onActiveSubsection }: { onActiveSubsection: (id: string) => void }) {
  const contentRef = useRef<HTMLDivElement>(null);
  useSectionObserver(contentRef, 'active', onActiveSubsection);
  return (
    <div className="settings-page-content">
      <div ref={contentRef}>
        <div id="section-a" />
        <div id="section-b" />
      </div>
    </div>
  );
}

function HarnessNoRoot({ onActiveSubsection }: { onActiveSubsection: (id: string) => void }) {
  const contentRef = useRef<HTMLDivElement>(null);
  useSectionObserver(contentRef, 'active', onActiveSubsection);
  return (
    <div ref={contentRef}>
      <div id="section-a" />
    </div>
  );
}

const APP_DESTINATIONS = ['section-profile', 'section-subscription', 'section-presence-history'];

function AppHarness({
  onActiveSubsection,
  explicit = true,
}: {
  onActiveSubsection: (id: string) => void;
  explicit?: boolean;
}) {
  const contentRef = useRef<HTMLDivElement>(null);
  useSectionObserver(
    contentRef,
    'account',
    onActiveSubsection,
    explicit ? APP_DESTINATIONS : undefined
  );
  return (
    <>
      <button type="button">Outside Settings</button>
      <div className="settings-page-content">
        <div ref={contentRef}>
          <details id="section-profile">
            <summary>Profile Information</summary>
          </details>
          <details id="section-subscription" open>
            <summary>Subscription</summary>
          </details>
          <details id="section-presence-history" open>
            <summary>Activity History</summary>
            <h3 id="presence-history-heading" tabIndex={-1}>
              History records
            </h3>
          </details>
        </div>
      </div>
    </>
  );
}

function focusedDestinationFixture(
  destination: 'section-subscription' | 'section-presence-history',
  onActiveSubsection: (id: string) => void,
  explicit = true
) {
  render(<AppHarness onActiveSubsection={onActiveSubsection} explicit={explicit} />);
  vi.advanceTimersByTime(50);
  const viewport = document.querySelector<HTMLElement>('.settings-page-content');
  const section = document.getElementById(destination);
  const focusTarget =
    destination === 'section-presence-history'
      ? screen.getByRole('heading', { name: 'History records' })
      : section?.querySelector<HTMLElement>('summary');
  if (!viewport || !section || !focusTarget)
    throw new Error('Focused destination fixture is missing');
  // Geometry is supplied at the native layout boundary: a short collapsed
  // Profile row is fully visible above a taller, partly visible destination,
  // whose heading/summary is still visible within the actual scroll viewport.
  const bounds = new DOMRect(0, 100, 800, 400);
  vi.spyOn(viewport, 'getBoundingClientRect').mockReturnValue(bounds);
  const focusBounds = vi
    .spyOn(focusTarget, 'getBoundingClientRect')
    .mockReturnValue(new DOMRect(20, 250, 300, 32));
  focusTarget.focus();
  expect(focusTarget).toHaveFocus();
  const profileEntry: IntersectionObserverEntry = {
    ...makeEntry('section-profile', true, 120),
    boundingClientRect: new DOMRect(20, 120, 760, 32),
    intersectionRect: new DOMRect(20, 120, 760, 32),
    rootBounds: bounds,
    intersectionRatio: 1,
  };
  const destinationEntry: IntersectionObserverEntry = {
    ...makeEntry(destination, true, 180),
    boundingClientRect: new DOMRect(20, 180, 760, 640),
    intersectionRect: new DOMRect(20, 180, 760, 320),
    rootBounds: bounds,
    intersectionRatio: 0.5,
  };
  const observer = instances[0];
  const notify = (entry = destinationEntry) =>
    observer.callback([profileEntry, entry], {} as IntersectionObserver);
  return { focusTarget, focusBounds, notify, destinationEntry };
}

describe('useSectionObserver', () => {
  let originalIO: typeof IntersectionObserver;

  beforeEach(() => {
    resetAllStores();
    instances = [];
    originalIO = globalThis.IntersectionObserver;
    (globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver =
      FakeIntersectionObserver;
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers();
    (globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = originalIO;
  });

  it('creates the observer with the correct root, threshold, and rootMargin after the render delay', () => {
    const onActiveSubsection = vi.fn();
    render(<Harness onActiveSubsection={onActiveSubsection} />);

    // Not created yet — still inside the 50ms setTimeout.
    expect(instances).toHaveLength(0);

    vi.advanceTimersByTime(50);

    expect(instances).toHaveLength(1);
    const inst = instances[0];
    expect(inst.options?.root).toBeInstanceOf(HTMLElement);
    expect((inst.options?.root as HTMLElement).className).toBe('settings-page-content');
    expect(inst.options?.threshold).toEqual([0, 0.1, 0.25, 0.5]);
    expect(inst.options?.rootMargin).toBe('-10% 0px -50% 0px');
  });

  it('observes both section elements', () => {
    const onActiveSubsection = vi.fn();
    render(<Harness onActiveSubsection={onActiveSubsection} />);
    vi.advanceTimersByTime(50);

    const inst = instances[0];
    expect(inst.observe).toHaveBeenCalledTimes(2);
    const observedIds = inst.observe.mock.calls.map((call) => (call[0] as Element).id);
    expect(observedIds).toEqual(expect.arrayContaining(['section-a', 'section-b']));
  });

  it('reports the topmost visible section id with the prefix stripped', () => {
    const onActiveSubsection = vi.fn();
    render(<Harness onActiveSubsection={onActiveSubsection} />);
    vi.advanceTimersByTime(50);

    const inst = instances[0];
    inst.callback(
      [makeEntry('section-a', true, 100), makeEntry('section-b', true, 20)],
      {} as IntersectionObserver
    );

    expect(onActiveSubsection).toHaveBeenLastCalledWith('b');
  });

  it('reports the remaining visible section once the topmost one leaves', () => {
    const onActiveSubsection = vi.fn();
    render(<Harness onActiveSubsection={onActiveSubsection} />);
    vi.advanceTimersByTime(50);

    const inst = instances[0];
    inst.callback(
      [makeEntry('section-a', true, 100), makeEntry('section-b', true, 20)],
      {} as IntersectionObserver
    );
    expect(onActiveSubsection).toHaveBeenLastCalledWith('b');

    inst.callback([makeEntry('section-b', false, 20)], {} as IntersectionObserver);

    expect(onActiveSubsection).toHaveBeenLastCalledWith('a');
  });

  it('preserves the last destination when all observed sections leave the viewport', () => {
    const onActiveSubsection = vi.fn();
    render(<Harness onActiveSubsection={onActiveSubsection} />);
    vi.advanceTimersByTime(50);
    const inst = instances[0];
    inst.callback(
      [makeEntry('section-a', true, 100), makeEntry('section-b', true, 20)],
      {} as IntersectionObserver
    );
    // A positive report first proves the observer is live before the empty
    // viewport asks it to leave the current destination unchanged.
    expect(onActiveSubsection).toHaveBeenCalledTimes(1);
    expect(onActiveSubsection).toHaveBeenLastCalledWith('b');

    expect(() =>
      inst.callback(
        [makeEntry('section-a', false, -100), makeEntry('section-b', false, -200)],
        {} as IntersectionObserver
      )
    ).not.toThrow();

    expect(onActiveSubsection).toHaveBeenCalledTimes(1);
    expect(onActiveSubsection).toHaveBeenLastCalledWith('b');
  });

  it('clears the timer and disconnects the observer on unmount without throwing', () => {
    const onActiveSubsection = vi.fn();
    const { unmount } = render(<Harness onActiveSubsection={onActiveSubsection} />);
    vi.advanceTimersByTime(50);

    const inst = instances[0];
    expect(() => unmount()).not.toThrow();
    expect(inst.disconnect).toHaveBeenCalledTimes(1);
  });

  it('unmounts cleanly before the timer fires (no observer created yet)', () => {
    const onActiveSubsection = vi.fn();
    const { unmount } = render(<Harness onActiveSubsection={onActiveSubsection} />);

    expect(() => unmount()).not.toThrow();
    vi.advanceTimersByTime(50);
    expect(instances).toHaveLength(0);
  });

  it('does not create an observer when no .settings-page-content ancestor exists', () => {
    const onActiveSubsection = vi.fn();
    expect(() => render(<HarnessNoRoot onActiveSubsection={onActiveSubsection} />)).not.toThrow();
    vi.advanceTimersByTime(50);

    expect(instances).toHaveLength(0);
    expect(onActiveSubsection).not.toHaveBeenCalled();
  });

  it.each([
    ['section-subscription', 'subscription'],
    ['section-presence-history', 'presence-history'],
  ] as const)(
    'keeps the visible focused %s current despite a shorter fully visible section',
    (destination, current) => {
      const onActiveSubsection = vi.fn();
      const { notify } = focusedDestinationFixture(destination, onActiveSubsection);
      notify();

      expect(onActiveSubsection).toHaveBeenLastCalledWith(current);
    }
  );

  it('uses normal scroll selection once focus moves outside the Settings viewport', () => {
    const onActiveSubsection = vi.fn();
    const { notify } = focusedDestinationFixture('section-subscription', onActiveSubsection);
    const outside = screen.getByRole('button', { name: 'Outside Settings' });
    outside.focus();
    expect(outside).toHaveFocus();
    notify();

    expect(onActiveSubsection).toHaveBeenLastCalledWith('profile');
  });

  it('uses normal scroll selection when the focused control scrolls outside the viewport', () => {
    const onActiveSubsection = vi.fn();
    const { notify, focusTarget, focusBounds } = focusedDestinationFixture(
      'section-subscription',
      onActiveSubsection
    );
    // The larger section still intersects, while its focused summary has moved
    // above the viewport; document focus alone must not pin the old destination.
    focusBounds.mockReturnValue(new DOMRect(20, 20, 300, 32));
    expect(focusTarget).toHaveFocus();
    notify();

    expect(onActiveSubsection).toHaveBeenLastCalledWith('profile');
  });

  it('uses normal scroll selection when the focused section is no longer intersecting', () => {
    const onActiveSubsection = vi.fn();
    const { notify, destinationEntry } = focusedDestinationFixture(
      'section-presence-history',
      onActiveSubsection
    );
    notify({ ...destinationEntry, isIntersecting: false, intersectionRatio: 0 });

    expect(onActiveSubsection).toHaveBeenLastCalledWith('profile');
  });

  it('preserves the default three-argument server observer even with focus on a visible control', () => {
    const onActiveSubsection = vi.fn();
    const { notify } = focusedDestinationFixture('section-subscription', onActiveSubsection, false);
    notify();

    expect(onActiveSubsection).toHaveBeenLastCalledWith('profile');
  });
});
