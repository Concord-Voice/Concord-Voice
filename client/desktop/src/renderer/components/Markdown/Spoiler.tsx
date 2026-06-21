import React, { useState, type ReactNode, type KeyboardEvent } from 'react';
import './Spoiler.css';

interface SpoilerProps {
  children?: ReactNode;
}

const Spoiler: React.FC<SpoilerProps> = ({ children }) => {
  const [revealed, setRevealed] = useState(false);

  const reveal = (): void => {
    setRevealed(true);
  };

  // Native <button> elements activate on Enter / Space via the browser's
  // default activation behavior, but jsdom (used by our tests) does not
  // synthesize click events from keydown. Handle keys explicitly so the
  // keyboard path is deterministic in both environments.
  const handleKey = (e: KeyboardEvent<HTMLButtonElement>): void => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      reveal();
    }
  };

  return (
    <button
      type="button"
      className={revealed ? 'spoiler spoiler-revealed' : 'spoiler'}
      onClick={reveal}
      onKeyDown={handleKey}
      aria-label={revealed ? undefined : 'Hidden spoiler content. Press to reveal.'}
    >
      {children}
    </button>
  );
};

export default Spoiler;
