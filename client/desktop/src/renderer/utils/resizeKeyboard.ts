import type React from 'react';

export interface ResizeKeyOptions {
  axis: 'horizontal' | 'vertical';
  /** 'grow' means ArrowRight/ArrowDown increases the value */
  direction: 'grow' | 'shrink';
  min: number;
  max: number;
  step?: number;
  largeStep?: number;
  getValue: () => number;
  setValue: (v: number) => void;
}

export function createResizeKeyHandler({
  axis,
  direction,
  min,
  max,
  step = 10,
  largeStep = 50,
  getValue,
  setValue,
}: ResizeKeyOptions): (e: React.KeyboardEvent) => void {
  const growKeys = axis === 'horizontal' ? ['ArrowRight'] : ['ArrowDown'];
  const shrinkKeys = axis === 'horizontal' ? ['ArrowLeft'] : ['ArrowUp'];

  return (e: React.KeyboardEvent) => {
    const amount = e.shiftKey ? largeStep : step;
    let delta = 0;

    if (growKeys.includes(e.key)) {
      delta = direction === 'grow' ? amount : -amount;
    } else if (shrinkKeys.includes(e.key)) {
      delta = direction === 'grow' ? -amount : amount;
    } else {
      return;
    }

    e.preventDefault();
    const next = Math.max(min, Math.min(max, getValue() + delta));
    setValue(next);
  };
}
