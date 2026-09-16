// A4 + focus semantics — useWindowFocus / readWindowFocus module singleton.
// See [internal]specs/2026-09-12-2369-gif-playback-gating-design.md §2.2, §4.
//
// tests/setup.ts installs a fixed `document.hasFocus = () => true` for the whole
// suite (jsdom's real default is unfocused, which is the wrong baseline for most
// tests). Every test here that needs a specific focus state installs its own
// `vi.spyOn(document, 'hasFocus')` and restores it afterward, per that file's
// documented opt-out pattern.
import { renderHook, act } from '@testing-library/react';
import {
  useWindowFocus,
  readWindowFocus,
  __resetWindowFocusForTests,
} from '@/renderer/hooks/ui/useWindowFocus';

describe('useWindowFocus / readWindowFocus', () => {
  beforeEach(() => {
    __resetWindowFocusForTests();
    document.body.innerHTML = '';
  });

  afterEach(() => {
    __resetWindowFocusForTests();
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  // Codex review, PR #3291: a disabled caller must register NO listener, not
  // merely ignore the value — the point is that it never re-renders.
  it('enabled=false registers no listener at all and always reports focused', () => {
    const addSpy = vi.spyOn(globalThis, 'addEventListener');
    vi.spyOn(document, 'hasFocus').mockReturnValue(false);

    const { result } = renderHook(() => useWindowFocus(false));

    // Reports focused (the non-pausing answer) despite the document being blurred.
    expect(result.current).toBe(true);
    // And crucially: it did not wire itself to focus/blur.
    const wired = addSpy.mock.calls.filter(([type]) => type === 'focus' || type === 'blur');
    expect(wired).toHaveLength(0);

    // A real focus transition leaves it untouched, because nothing is listening.
    act(() => {
      globalThis.dispatchEvent(new Event('blur'));
    });
    expect(result.current).toBe(true);
  });

  it('enabled=true DOES wire the listeners — the control for the case above', () => {
    const addSpy = vi.spyOn(globalThis, 'addEventListener');
    renderHook(() => useWindowFocus(true));
    const wired = addSpy.mock.calls.filter(([type]) => type === 'focus' || type === 'blur');
    expect(wired.length).toBeGreaterThan(0);
  });

  it('THE LOAD-BEARING CASE: focusing a button inside the document does not change the value (A4, capture-phase trap)', () => {
    const hasFocusSpy = vi.spyOn(document, 'hasFocus').mockReturnValue(false);
    const { result } = renderHook(() => useWindowFocus());
    expect(result.current).toBe(false);

    // The window itself never receives this event — focus does not bubble —
    // so a correct (bubble-phase) listener never re-runs compute(). Flip the
    // underlying signal to prove that: if anything DID re-run compute(), it
    // would now see `true` and the assertion below would catch it.
    hasFocusSpy.mockReturnValue(true);
    const button = document.createElement('button');
    document.body.appendChild(button);
    act(() => {
      button.dispatchEvent(new Event('focus'));
    });

    expect(result.current).toBe(false);
  });

  it('window blur sets focused false; window focus sets it true', () => {
    const hasFocusSpy = vi.spyOn(document, 'hasFocus').mockReturnValue(true);
    const { result } = renderHook(() => useWindowFocus());
    expect(result.current).toBe(true);

    hasFocusSpy.mockReturnValue(false);
    act(() => {
      window.dispatchEvent(new Event('blur'));
    });
    expect(result.current).toBe(false);

    hasFocusSpy.mockReturnValue(true);
    act(() => {
      window.dispatchEvent(new Event('focus'));
    });
    expect(result.current).toBe(true);
  });

  it('visibilitychange with document.hidden true reports unfocused', () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(true);
    const hiddenDescriptor = Object.getOwnPropertyDescriptor(document, 'hidden');
    Object.defineProperty(document, 'hidden', { value: false, configurable: true });

    try {
      const { result } = renderHook(() => useWindowFocus());
      expect(result.current).toBe(true);

      Object.defineProperty(document, 'hidden', { value: true, configurable: true });
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
      expect(result.current).toBe(false);
    } finally {
      if (hiddenDescriptor) {
        Object.defineProperty(document, 'hidden', hiddenDescriptor);
      } else {
        Object.defineProperty(document, 'hidden', { value: false, configurable: true });
      }
    }
  });

  it('seeding: a component mounting while already unfocused reads false without any event firing', () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(false);

    const { result } = renderHook(() => useWindowFocus());

    // No event was ever dispatched — the value comes from the synchronous
    // `compute()` seed at subscribe time.
    expect(result.current).toBe(false);
  });

  it('singleton: mounting N components installs exactly one listener pair; unmounting all removes them', () => {
    const addSpy = vi.spyOn(globalThis, 'addEventListener');
    const removeSpy = vi.spyOn(globalThis, 'removeEventListener');

    const a = renderHook(() => useWindowFocus());
    const b = renderHook(() => useWindowFocus());
    const c = renderHook(() => useWindowFocus());

    const focusAdds = addSpy.mock.calls.filter(([type]) => type === 'focus').length;
    const blurAdds = addSpy.mock.calls.filter(([type]) => type === 'blur').length;
    expect(focusAdds).toBe(1);
    expect(blurAdds).toBe(1);

    a.unmount();
    b.unmount();
    c.unmount();

    const focusRemoves = removeSpy.mock.calls.filter(([type]) => type === 'focus').length;
    const blurRemoves = removeSpy.mock.calls.filter(([type]) => type === 'blur').length;
    expect(focusRemoves).toBe(1);
    expect(blurRemoves).toBe(1);
  });

  it('readWindowFocus reads through to the DOM with no subscriber, and returns the cached snapshot while subscribed', () => {
    const hasFocusSpy = vi.spyOn(document, 'hasFocus');

    hasFocusSpy.mockReturnValue(true);
    expect(readWindowFocus()).toBe(true);
    hasFocusSpy.mockReturnValue(false);
    expect(readWindowFocus()).toBe(false); // no subscriber: reads fresh every call

    const { unmount } = renderHook(() => useWindowFocus());
    // Subscribing seeds `cached` from the current DOM state.
    expect(readWindowFocus()).toBe(false);

    // Mutate the underlying signal WITHOUT notifying (no event dispatched).
    // While subscribed, readWindowFocus must return the stale cached value,
    // not read through to the DOM.
    hasFocusSpy.mockReturnValue(true);
    expect(readWindowFocus()).toBe(false);

    unmount();
    // Unsubscribed again: reads through fresh.
    expect(readWindowFocus()).toBe(true);
  });
});
