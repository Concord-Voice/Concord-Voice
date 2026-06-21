import React, { useRef, useState, useCallback, useEffect } from 'react';
import { EmojiEntry, SkinTone } from './types';

const COLS = 8;
const CELL_SIZE = 44;
const ROW_HEIGHT = CELL_SIZE;
const VIEWPORT_HEIGHT = 280;
const BUFFER_ROWS = 2;

interface EmojiGridProps {
  emojis: EmojiEntry[];
  onSelect: (emoji: string) => void;
  skinTone: SkinTone;
  isLoading: boolean;
  onHover?: (emoji: EmojiEntry | null) => void;
}

function applySkintone(emoji: string, supportsSkinTone: boolean, skinTone: SkinTone): string {
  if (!supportsSkinTone || !skinTone) return emoji;
  // For most emojis, skin tone modifier goes right after the base character
  // We insert after the first code point
  const codePoints = [...emoji];
  if (codePoints.length === 0) return emoji;
  return codePoints[0] + skinTone + codePoints.slice(1).join('');
}

const EmojiGrid: React.FC<EmojiGridProps> = ({
  emojis,
  onSelect,
  skinTone,
  isLoading,
  onHover,
}) => {
  const viewportRef = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);

  const totalRows = Math.ceil(emojis.length / COLS);
  const totalHeight = totalRows * ROW_HEIGHT;

  const visibleRows = Math.ceil(VIEWPORT_HEIGHT / ROW_HEIGHT);
  const startRow = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - BUFFER_ROWS);
  const endRow = Math.min(totalRows, startRow + visibleRows + BUFFER_ROWS * 2);

  const handleScroll = useCallback(() => {
    if (viewportRef.current) {
      setScrollTop(viewportRef.current.scrollTop);
    }
  }, []);

  // Reset scroll when emojis change (category switch or search)
  useEffect(() => {
    if (viewportRef.current) {
      viewportRef.current.scrollTop = 0;
      setScrollTop(0);
    }
  }, [emojis]);

  if (isLoading) {
    return (
      <div className="emoji-picker-loading">
        <span>Loading...</span>
      </div>
    );
  }

  if (emojis.length === 0) {
    return (
      <div className="emoji-picker-loading">
        <span>No emoji found</span>
      </div>
    );
  }

  const rows: React.ReactNode[] = [];
  for (let row = startRow; row < endRow; row++) {
    const startIdx = row * COLS;
    const cells: React.ReactNode[] = [];

    for (let col = 0; col < COLS; col++) {
      const idx = startIdx + col;
      if (idx >= emojis.length) break;

      const entry = emojis[idx];
      const displayEmoji = applySkintone(entry.e, entry.s, skinTone);

      cells.push(
        <button
          type="button"
          key={entry.n}
          className="emoji-picker-cell"
          onClick={() => onSelect(displayEmoji)}
          onMouseEnter={() => onHover?.(entry)}
          onMouseLeave={() => onHover?.(null)}
          aria-label={entry.n}
          title={entry.n}
        >
          {displayEmoji}
        </button>
      );
    }

    rows.push(
      <div
        key={row}
        className="emoji-picker-row"
        style={{
          position: 'absolute',
          top: row * ROW_HEIGHT,
          left: 0,
          right: 0,
          height: ROW_HEIGHT,
          display: 'flex',
          paddingLeft: 8,
        }}
      >
        {cells}
      </div>
    );
  }

  return (
    <div ref={viewportRef} className="emoji-picker-grid-viewport" onScroll={handleScroll}>
      <div className="emoji-picker-grid-spacer" style={{ height: totalHeight }}>
        {rows}
      </div>
    </div>
  );
};

export default EmojiGrid;
