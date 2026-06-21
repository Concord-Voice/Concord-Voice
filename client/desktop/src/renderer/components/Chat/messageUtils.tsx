import React from 'react';
import MarkdownContent from '../Markdown/MarkdownContent';
import type { MentionLookup } from '../Markdown/mentionTypes';

// Mention resolution moved to the Markdown/mentionTypes leaf module to break an
// import cycle. Re-exported here so existing Chat consumers keep importing these
// symbols from messageUtils; MentionLookup is also used internally below.
export { resolveMentionDisplay } from '../Markdown/mentionTypes';
export type { MentionLookup } from '../Markdown/mentionTypes';

/**
 * Matches a single "emoji atom": either a presentation emoji or a text-default
 * emoji followed by the variation selector (VS16). Characters like ☠ (U+2620) are
 * \p{Emoji} but NOT \p{Emoji_Presentation} — VS16 promotes them to emoji display.
 */
const EMOJI_ATOM = /(?:\p{Emoji_Presentation}|\p{Emoji}\uFE0F)/u;

/**
 * Matches complete emoji sequences including:
 * - Regional indicator pairs (country flags: 🇺🇸)
 * - Tag sequences (subdivision flags: 🏴󠁧󠁢󠁥󠁮󠁧󠁿)
 * - ZWJ sequences (🏴‍☠️, ❤️‍🔥, 👁️‍🗨️, 👨‍👩‍👧)
 * - Skin tone modifiers (👋🏽)
 * - Keycap sequences (1️⃣)
 *
 * Built from EMOJI_ATOM to keep regex complexity under SonarQube's limit of 20.
 */
export const EMOJI_REGEX = new RegExp(
  String.raw`(?:\p{Regional_Indicator}{2}` +
    String.raw`|\p{Emoji_Presentation}[\u{E0020}-\u{E007E}]+\u{E007F}` +
    String.raw`|${EMOJI_ATOM.source}(?:\u{200D}${EMOJI_ATOM.source}|\p{Emoji_Modifier}|\uFE0F?\u{20E3})*)`,
  'gu'
);

/**
 * Detect emoji-only messages and return the count.
 * Returns 0 if the message contains any non-emoji text.
 * Uses Unicode emoji properties to match all standard emoji sequences.
 */
export function getEmojiOnlyCount(text: string): number {
  if (!text || text.length === 0) return 0;
  const stripped = text.replaceAll(EMOJI_REGEX, '').replaceAll(/[\s\u{FE0F}\u{200D}]/gu, '');
  if (stripped.length > 0) return 0;
  const matches = text.match(EMOJI_REGEX);
  return matches ? matches.length : 0;
}

/**
 * Returns a CSS class for emoji-only messages based on emoji count.
 * 1 emoji = largest (48px), linearly scales down to base (24px) at 5+.
 */
export function getEmojiSizeClass(count: number): string {
  if (count <= 0) return '';
  if (count === 1) return 'emoji-jumbo-1';
  if (count === 2) return 'emoji-jumbo-2';
  if (count === 3) return 'emoji-jumbo-3';
  if (count === 4) return 'emoji-jumbo-4';
  if (count === 5) return 'emoji-jumbo-5';
  return '';
}

/** Render emoji spans within a text segment (no mention handling). */
export function renderEmoji(text: string): React.ReactNode[] {
  const parts: React.ReactNode[] = [];
  let lastIndex = 0;
  let match: RegExpExecArray | null;

  EMOJI_REGEX.lastIndex = 0;
  while ((match = EMOJI_REGEX.exec(text)) !== null) {
    if (match.index > lastIndex) {
      parts.push(text.slice(lastIndex, match.index));
    }
    parts.push(
      <span key={match.index} className="emoji">
        {match[0]}
      </span>
    );
    lastIndex = EMOJI_REGEX.lastIndex;
  }
  if (lastIndex < text.length) {
    parts.push(text.slice(lastIndex));
  }
  return parts;
}

export function renderContent(text: string, lookup: MentionLookup): React.ReactNode[] {
  // If text is emoji-only, stay on the jumbo-compatible renderEmoji path
  // (MessageTextContent's jumbo-sizing logic depends on emoji span identity).
  const emojiCount = getEmojiOnlyCount(text);
  if (emojiCount > 0) {
    return renderEmoji(text);
  }

  // Otherwise, delegate to the MarkdownContent pipeline.
  // The id/editedAt here are placeholders — the canonical caller in Message.tsx
  // (Task 17) uses real message.id/edited_at for memo keying.
  return [
    <MarkdownContent key="md" id="inline" content={text} editedAt={null} mentionLookup={lookup} />,
  ];
}
