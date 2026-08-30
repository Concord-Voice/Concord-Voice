import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/react';
import { useRef } from 'react';
import { useSectionObserver } from '@/renderer/hooks/ui/useSectionObserver';

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

describe('useSectionObserver', () => {
  let originalIO: typeof IntersectionObserver;

  beforeEach(() => {
    instances = [];
    originalIO = globalThis.IntersectionObserver;
    (globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver =
      FakeIntersectionObserver;
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
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
});
