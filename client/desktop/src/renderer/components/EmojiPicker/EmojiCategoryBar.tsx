import React from 'react';
import { EmojiCategory } from './types';

interface EmojiCategoryBarProps {
  categories: EmojiCategory[];
  activeCategory: string;
  hasRecent: boolean;
  onSelect: (categoryId: string) => void;
}

const EmojiCategoryBar: React.FC<EmojiCategoryBarProps> = ({
  categories,
  activeCategory,
  hasRecent,
  onSelect,
}) => {
  return (
    <div className="emoji-picker-categories">
      {hasRecent && (
        <button
          type="button"
          className={`emoji-picker-category-tab ${activeCategory === 'recent' ? 'emoji-picker-category-tab--active' : ''}`}
          onClick={() => onSelect('recent')}
          title="Frequently Used"
          aria-label="Frequently Used"
        >
          🕐
        </button>
      )}
      {categories.map((cat) => (
        <button
          type="button"
          key={cat.id}
          className={`emoji-picker-category-tab ${activeCategory === cat.id ? 'emoji-picker-category-tab--active' : ''}`}
          onClick={() => onSelect(cat.id)}
          title={cat.name}
          aria-label={cat.name}
        >
          {cat.icon}
        </button>
      ))}
    </div>
  );
};

export default EmojiCategoryBar;
