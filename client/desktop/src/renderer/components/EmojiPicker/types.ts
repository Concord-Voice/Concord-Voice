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
  /**
   * Popover-mode placement (#2370). When `anchorCenterX` is present (the
   * composer only), `x`/`y` describe the ANCHOR button's bounding-rect right
   * edge and top edge — the picker measures itself and places ABOVE the
   * anchor via `resolveAnchoredPlacement` (`utils/ui/pickerAnchor.ts`), with
   * a caret pointing down at it. When `anchorCenterX` is absent (the other
   * consumers), `x`/`y` are the picker's own desired top-left corner and the
   * pre-#2370 clamp-and-flip placement runs byte-identically, with no arrow.
   * One optional field switches the meaning of both — see EmojiPicker.tsx's
   * placement `useLayoutEffect`.
   */
  position?: { x: number; y: number; anchorCenterX?: number };
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
