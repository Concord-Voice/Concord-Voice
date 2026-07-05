import { describe, it, expect, vi } from 'vitest';
import type React from 'react';
import {
  extractTriggerToken,
  handleTypeaheadKeyDown,
} from '@/renderer/components/Chat/typeaheadAutocomplete';

describe('extractTriggerToken', () => {
  it('detects an @ token at start of input', () => {
    expect(extractTriggerToken('@ali', 4, '@')).toEqual({ text: 'ali', startIndex: 0 });
  });

  it('detects a : token after whitespace', () => {
    expect(extractTriggerToken('hi :sm', 6, ':')).toEqual({ text: 'sm', startIndex: 3 });
  });

  it('lowercases the query text', () => {
    expect(extractTriggerToken(':SMI', 4, ':')).toEqual({ text: 'smi', startIndex: 0 });
  });

  it('returns null when the trigger is mid-word (3:30, email@host)', () => {
    expect(extractTriggerToken('3:30', 4, ':')).toBeNull();
    expect(extractTriggerToken('email@host', 10, '@')).toBeNull();
  });

  it('returns null for a completed :smile: token', () => {
    expect(extractTriggerToken(':smile:', 7, ':')).toBeNull();
  });

  it('returns null when no trigger precedes the cursor', () => {
    expect(extractTriggerToken('hello', 5, ':')).toBeNull();
  });

  // #1754-review fix: a trigger after a tab is now recognized (tab is whitespace).
  it('treats a leading tab as a whitespace boundary (\\t:smile / \\t@ali)', () => {
    expect(extractTriggerToken('\t:sm', 4, ':')).toEqual({ text: 'sm', startIndex: 1 });
    expect(extractTriggerToken('\t@ali', 5, '@')).toEqual({ text: 'ali', startIndex: 1 });
  });

  it('stops the token scan at an intervening tab', () => {
    expect(extractTriggerToken(':a\tb', 4, ':')).toBeNull();
  });
});

describe('handleTypeaheadKeyDown', () => {
  const ev = (key: string) => ({ key, preventDefault: vi.fn() }) as unknown as React.KeyboardEvent;

  it('returns false when there are no options (so send/shortcuts still fire)', () => {
    expect(handleTypeaheadKeyDown(ev('Enter'), 0, 0, vi.fn(), vi.fn(), vi.fn())).toBe(false);
  });

  it('ArrowDown advances the selection with wraparound', () => {
    const setSel = vi.fn();
    expect(handleTypeaheadKeyDown(ev('ArrowDown'), 3, 2, setSel, vi.fn(), vi.fn())).toBe(true);
    const updater = setSel.mock.calls[0][0] as (i: number) => number;
    expect(updater(2)).toBe(0);
  });

  it('ArrowUp retreats the selection with wraparound', () => {
    const setSel = vi.fn();
    expect(handleTypeaheadKeyDown(ev('ArrowUp'), 3, 0, setSel, vi.fn(), vi.fn())).toBe(true);
    const updater = setSel.mock.calls[0][0] as (i: number) => number;
    expect(updater(0)).toBe(2);
  });

  it('Enter and Tab commit the current index', () => {
    const onCommit = vi.fn();
    expect(handleTypeaheadKeyDown(ev('Enter'), 3, 1, vi.fn(), onCommit, vi.fn())).toBe(true);
    expect(onCommit).toHaveBeenCalledWith(1);
    const onCommit2 = vi.fn();
    expect(handleTypeaheadKeyDown(ev('Tab'), 3, 2, vi.fn(), onCommit2, vi.fn())).toBe(true);
    expect(onCommit2).toHaveBeenCalledWith(2);
  });

  it('Escape closes', () => {
    const onClose = vi.fn();
    expect(handleTypeaheadKeyDown(ev('Escape'), 3, 0, vi.fn(), vi.fn(), onClose)).toBe(true);
    expect(onClose).toHaveBeenCalled();
  });

  it('returns false for an unhandled key', () => {
    expect(handleTypeaheadKeyDown(ev('x'), 3, 0, vi.fn(), vi.fn(), vi.fn())).toBe(false);
  });
});
