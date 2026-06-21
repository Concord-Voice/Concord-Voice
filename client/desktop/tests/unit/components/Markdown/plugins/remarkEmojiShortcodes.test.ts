import { describe, it, expect, beforeEach } from 'vitest';
import { unified } from 'unified';
import remarkParse from 'remark-parse';
import { remarkEmojiShortcodes } from '@/renderer/components/Markdown/plugins/remarkEmojiShortcodes';
import {
  indexCategory,
  resetShortcodeIndex,
} from '@/renderer/components/EmojiPicker/shortcodeIndex';
import type { Root } from 'mdast';

function collectText(tree: Root): string {
  const parts: string[] = [];
  const visit = (node: unknown): void => {
    if (typeof node !== 'object' || node === null) return;
    const n = node as { type?: string; value?: string; children?: unknown[] };
    if ((n.type === 'text' || n.type === 'inlineCode') && typeof n.value === 'string') {
      parts.push(n.value);
    }
    if (Array.isArray(n.children)) n.children.forEach(visit);
  };
  visit(tree);
  return parts.join('');
}

async function parse(input: string): Promise<Root> {
  const processor = unified().use(remarkParse).use(remarkEmojiShortcodes);
  const tree = processor.parse(input);
  return (await processor.run(tree)) as Root;
}

describe('remarkEmojiShortcodes', () => {
  beforeEach(() => {
    resetShortcodeIndex();
    indexCategory('smileys', [
      { e: '😄', n: 'smile', s: false, c: ['smile'] },
      { e: '👍', n: 'thumbs up', s: false, c: ['thumbsup', '+1'] },
    ]);
  });

  it('replaces a known shortcode with its emoji', async () => {
    const tree = await parse('hi :smile:!');
    expect(collectText(tree)).toBe('hi 😄!');
  });

  it('replaces multiple shortcodes in the same text', async () => {
    const tree = await parse(':smile: and :thumbsup:');
    expect(collectText(tree)).toBe('😄 and 👍');
  });

  it('leaves unknown shortcodes untouched', async () => {
    const tree = await parse('hello :unknown_shortcode:');
    expect(collectText(tree)).toBe('hello :unknown_shortcode:');
  });

  it('does not replace inside inline code', async () => {
    const tree = await parse('`:smile:`');
    expect(collectText(tree)).toBe(':smile:');
  });

  it('handles shortcodes with + character (e.g. :+1:)', async () => {
    const tree = await parse(':+1:');
    expect(collectText(tree)).toBe('👍');
  });
});
