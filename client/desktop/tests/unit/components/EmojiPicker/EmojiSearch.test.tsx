import React from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import EmojiSearch from '@/renderer/components/EmojiPicker/EmojiSearch';

// Debounce tests use fireEvent.change (synchronous) + vi.advanceTimersByTime
// so we avoid the userEvent/fake-timer async deadlock entirely.

describe('EmojiSearch', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders an input with the correct placeholder', () => {
    render(<EmojiSearch onSearch={vi.fn()} />);
    expect(screen.getByPlaceholderText('Search emoji...')).toBeInTheDocument();
  });

  it('has type="text" and autocomplete off', () => {
    render(<EmojiSearch onSearch={vi.fn()} />);
    const input = screen.getByPlaceholderText('Search emoji...') as HTMLInputElement;
    expect(input.type).toBe('text');
    expect(input.autocomplete).toBe('off');
  });

  it('calls onSearch after the 150ms debounce when the user types', () => {
    const onSearch = vi.fn();
    render(<EmojiSearch onSearch={onSearch} />);
    const input = screen.getByPlaceholderText('Search emoji...');

    fireEvent.change(input, { target: { value: 'smile' } });

    // Debounce has not fired yet
    expect(onSearch).not.toHaveBeenCalled();

    vi.advanceTimersByTime(150);
    expect(onSearch).toHaveBeenCalledWith('smile');
    expect(onSearch).toHaveBeenCalledTimes(1);
  });

  it('debounces multiple keystrokes into a single call', () => {
    const onSearch = vi.fn();
    render(<EmojiSearch onSearch={onSearch} />);
    const input = screen.getByPlaceholderText('Search emoji...');

    fireEvent.change(input, { target: { value: 'a' } });
    vi.advanceTimersByTime(50);
    fireEvent.change(input, { target: { value: 'ab' } });
    vi.advanceTimersByTime(50);
    fireEvent.change(input, { target: { value: 'abc' } });
    vi.advanceTimersByTime(150);

    // Only the last value after 150ms of silence
    expect(onSearch).toHaveBeenCalledTimes(1);
    expect(onSearch).toHaveBeenCalledWith('abc');
  });

  it('does not call onSearch before the debounce window', () => {
    const onSearch = vi.fn();
    render(<EmojiSearch onSearch={onSearch} />);
    const input = screen.getByPlaceholderText('Search emoji...');

    fireEvent.change(input, { target: { value: 'test' } });
    vi.advanceTimersByTime(100); // less than 150ms

    expect(onSearch).not.toHaveBeenCalled();
  });

  it('autoFocus=true focuses the input after 50ms', () => {
    render(<EmojiSearch onSearch={vi.fn()} autoFocus={true} />);
    const input = screen.getByPlaceholderText('Search emoji...');

    // Before the 50ms timeout fires, focus has not been set
    expect(document.activeElement).not.toBe(input);

    vi.advanceTimersByTime(50);
    expect(document.activeElement).toBe(input);
  });

  it('autoFocus=false does not focus the input', () => {
    render(<EmojiSearch onSearch={vi.fn()} autoFocus={false} />);
    const input = screen.getByPlaceholderText('Search emoji...');

    vi.advanceTimersByTime(200);
    expect(document.activeElement).not.toBe(input);
  });

  it('clears the debounce timer on unmount', () => {
    const onSearch = vi.fn();
    const { unmount } = render(<EmojiSearch onSearch={onSearch} />);
    const input = screen.getByPlaceholderText('Search emoji...');

    fireEvent.change(input, { target: { value: 'hello' } });
    unmount();

    // Advancing time after unmount must not trigger onSearch
    vi.advanceTimersByTime(300);
    expect(onSearch).not.toHaveBeenCalled();
  });
});
