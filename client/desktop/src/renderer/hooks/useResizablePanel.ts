import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from 'react';

interface UseResizablePanelOptions {
  width: number;
  minWidth: number;
  maxWidth: number;
  /** 'left' = drag handle is on the right edge, 'right' = drag handle is on the left edge */
  side: 'left' | 'right';
  disabled?: boolean;
  onWidthCommit: (width: number) => void;
}

export function useResizablePanel({
  width,
  minWidth,
  maxWidth,
  side,
  disabled = false,
  onWidthCommit,
}: UseResizablePanelOptions) {
  const [draftWidth, setDraftWidth] = useState<number | null>(null);
  const isDraggingRef = useRef(false);
  const startXRef = useRef(0);
  const startWidthRef = useRef(0);
  const draftWidthRef = useRef(width);
  const minWidthRef = useRef(minWidth);
  const maxWidthRef = useRef(maxWidth);
  const sideRef = useRef(side);
  const onWidthCommitRef = useRef(onWidthCommit);
  const previousCursorRef = useRef('');
  const previousUserSelectRef = useRef('');
  const isResizing = draftWidth !== null;

  useEffect(() => {
    onWidthCommitRef.current = onWidthCommit;
  }, [onWidthCommit]);

  const onMouseDown = useCallback(
    (event: React.MouseEvent) => {
      if (disabled || isDraggingRef.current) return;

      event.preventDefault();
      isDraggingRef.current = true;
      startXRef.current = event.clientX;
      startWidthRef.current = width;
      draftWidthRef.current = width;
      minWidthRef.current = minWidth;
      maxWidthRef.current = maxWidth;
      sideRef.current = side;
      previousCursorRef.current = document.body.style.cursor;
      previousUserSelectRef.current = document.body.style.userSelect;
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';
      setDraftWidth(width);
    },
    [disabled, maxWidth, minWidth, side, width]
  );

  useEffect(() => {
    if (!isResizing) return;

    const onMouseMove = (event: MouseEvent) => {
      const delta = event.clientX - startXRef.current;
      const nextWidth =
        sideRef.current === 'left' ? startWidthRef.current + delta : startWidthRef.current - delta;
      const clamped = Math.max(minWidthRef.current, Math.min(maxWidthRef.current, nextWidth));
      draftWidthRef.current = clamped;
      setDraftWidth(clamped);
    };

    const onMouseUp = () => {
      if (!isDraggingRef.current) return;
      const committedWidth = draftWidthRef.current;
      isDraggingRef.current = false;
      setDraftWidth(null);
      onWidthCommitRef.current(committedWidth);
    };

    globalThis.addEventListener('mousemove', onMouseMove);
    globalThis.addEventListener('mouseup', onMouseUp);
    return () => {
      isDraggingRef.current = false;
      globalThis.removeEventListener('mousemove', onMouseMove);
      globalThis.removeEventListener('mouseup', onMouseUp);
      document.body.style.cursor = previousCursorRef.current;
      document.body.style.userSelect = previousUserSelectRef.current;
    };
  }, [isResizing]);

  const onKeyDown = useCallback(
    (event: KeyboardEvent) => {
      if (disabled) return;

      const multiplier = event.shiftKey ? 5 : 1;
      let step: number;
      if (event.key === 'ArrowRight') {
        step = (side === 'left' ? 10 : -10) * multiplier;
      } else if (event.key === 'ArrowLeft') {
        step = (side === 'left' ? -10 : 10) * multiplier;
      } else {
        return;
      }

      event.preventDefault();
      onWidthCommit(Math.max(minWidth, Math.min(maxWidth, width + step)));
    },
    [disabled, maxWidth, minWidth, onWidthCommit, side, width]
  );

  return {
    width: draftWidth ?? width,
    isResizing,
    onMouseDown,
    onKeyDown,
  };
}
