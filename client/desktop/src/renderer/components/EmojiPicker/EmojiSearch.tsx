import React, { useState, useRef, useEffect } from 'react';

interface EmojiSearchProps {
  onSearch: (query: string) => void;
  autoFocus?: boolean;
}

const EmojiSearch: React.FC<EmojiSearchProps> = ({ onSearch, autoFocus = true }) => {
  const [value, setValue] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const debounceRef = useRef<NodeJS.Timeout | null>(null);

  useEffect(() => {
    if (!autoFocus || !inputRef.current) return;

    // Small delay to ensure the picker is fully rendered
    const timer = setTimeout(() => inputRef.current?.focus(), 50);
    return () => clearTimeout(timer);
  }, [autoFocus]);

  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const newValue = e.target.value;
    setValue(newValue);

    if (debounceRef.current) {
      clearTimeout(debounceRef.current);
    }

    debounceRef.current = setTimeout(() => {
      onSearch(newValue);
    }, 150);
  };

  // Cleanup debounce on unmount
  useEffect(() => {
    return () => {
      if (debounceRef.current) {
        clearTimeout(debounceRef.current);
      }
    };
  }, []);

  return (
    <div className="emoji-picker-search">
      <input
        ref={inputRef}
        className="emoji-picker-search-input"
        type="text"
        placeholder="Search emoji..."
        value={value}
        onChange={handleChange}
        autoComplete="off"
        spellCheck={false}
      />
    </div>
  );
};

export default EmojiSearch;
