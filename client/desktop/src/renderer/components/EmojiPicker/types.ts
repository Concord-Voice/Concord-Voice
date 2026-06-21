export interface EmojiEntry {
  e: string; // emoji character
  n: string; // name/description for search
  s: boolean; // skin tone support
  c: string[]; // shortcode aliases (stored without colons, e.g. ["smile", "grin"])
}

export interface EmojiCategory {
  id: string;
  name: string;
  icon: string;
  file: string;
  count: number;
}

export interface EmojiPickerProps {
  onSelect: (emoji: string) => void;
  onClose: () => void;
  mode?: 'popover' | 'inline';
  position?: { x: number; y: number };
}

export type SkinTone = '' | '\u{1F3FB}' | '\u{1F3FC}' | '\u{1F3FD}' | '\u{1F3FE}' | '\u{1F3FF}';

export const SKIN_TONES: { tone: SkinTone; label: string; preview: string }[] = [
  { tone: '', label: 'Default', preview: '👋' },
  { tone: '\u{1F3FB}', label: 'Light', preview: '👋🏻' },
  { tone: '\u{1F3FC}', label: 'Medium-Light', preview: '👋🏼' },
  { tone: '\u{1F3FD}', label: 'Medium', preview: '👋🏽' },
  { tone: '\u{1F3FE}', label: 'Medium-Dark', preview: '👋🏾' },
  { tone: '\u{1F3FF}', label: 'Dark', preview: '👋🏿' },
];
