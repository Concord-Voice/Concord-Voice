import React, { useState, useMemo, useEffect } from 'react';
import { useKeyboardShortcutStore, type KeyCombo } from '../../stores/keyboardShortcutStore';
import { keyboardShortcutService } from '../../services/keyboardShortcutService';
import Modal from './Modal';
import './ShortcutOverlay.css';

// Helper to render a key combo as platform-appropriate labels
function formatKeyCombo(combo: KeyCombo, isMac: boolean): string[] {
  const keys: string[] = [];
  if (combo.ctrl) keys.push(isMac ? '⌘' : 'Ctrl');
  if (combo.alt) keys.push(isMac ? '⌥' : 'Alt');
  if (combo.shift) keys.push('Shift');
  // Format the key name
  const keyLabel = formatKey(combo.key);
  keys.push(keyLabel);
  return keys;
}

function formatKey(key: string): string {
  switch (key) {
    case 'ArrowUp':
      return '↑';
    case 'ArrowDown':
      return '↓';
    case 'ArrowLeft':
      return '←';
    case 'ArrowRight':
      return '→';
    case 'Escape':
      return 'Esc';
    case ' ':
      return 'Space';
    default:
      return key.length === 1 ? key.toUpperCase() : key;
  }
}

const CATEGORY_LABELS: Record<string, string> = {
  navigation: 'Navigation',
  messaging: 'Messaging',
  app: 'Application',
  voice: 'Audio & Video',
};

const CATEGORY_ORDER = ['navigation', 'messaging', 'app', 'voice'];

const ShortcutOverlay: React.FC = () => {
  const overlayOpen = useKeyboardShortcutStore((s) => s.overlayOpen);
  const shortcuts = useKeyboardShortcutStore((s) => s.shortcuts);
  const closeOverlay = useKeyboardShortcutStore((s) => s.closeOverlay);
  const [filter, setFilter] = useState('');
  const isMac = keyboardShortcutService.isMacPlatform;

  const filtered = useMemo(() => {
    if (!filter) return shortcuts;
    const lower = filter.toLowerCase();
    return shortcuts.filter(
      (s) => s.label.toLowerCase().includes(lower) || s.category.toLowerCase().includes(lower)
    );
  }, [shortcuts, filter]);

  // Group by category
  const grouped = useMemo(() => {
    const groups = new Map<string, typeof filtered>();
    for (const cat of CATEGORY_ORDER) {
      const items = filtered.filter((s) => s.category === cat);
      if (items.length > 0) groups.set(cat, items);
    }
    return groups;
  }, [filtered]);

  // Reset filter each time the overlay opens
  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets the search filter when the overlay opens; the dep array is [overlayOpen] only, so this setter cannot re-trigger the effect (no render loop)
    if (overlayOpen) setFilter('');
  }, [overlayOpen]);

  if (!overlayOpen) return null;

  return (
    <Modal isOpen={overlayOpen} onClose={closeOverlay} title="Keyboard Shortcuts" width="medium">
      <div className="shortcut-overlay">
        <input
          type="text"
          className="shortcut-search"
          placeholder="Search shortcuts..."
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          autoFocus
        />
        <div className="shortcut-list">
          {grouped.size === 0 && (
            <div className="shortcut-empty">No shortcuts match your search</div>
          )}
          {Array.from(grouped.entries()).map(([category, items]) => (
            <div key={category} className="shortcut-category">
              <h4 className="shortcut-category-title">{CATEGORY_LABELS[category] || category}</h4>
              {items.map((shortcut) => (
                <div key={shortcut.id} className="shortcut-row">
                  <span className="shortcut-label">{shortcut.label}</span>
                  <span className="shortcut-keys">
                    {formatKeyCombo(shortcut.combo, isMac).map((k) => (
                      <kbd key={k} className="shortcut-kbd">
                        {k}
                      </kbd>
                    ))}
                  </span>
                </div>
              ))}
            </div>
          ))}
        </div>
      </div>
    </Modal>
  );
};

export default ShortcutOverlay;
