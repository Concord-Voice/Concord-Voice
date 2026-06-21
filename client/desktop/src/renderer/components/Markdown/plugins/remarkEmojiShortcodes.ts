import type { Plugin } from 'unified';
import type { Root, Text, PhrasingContent } from 'mdast';
import { visit, SKIP } from 'unist-util-visit';
import { lookupShortcode } from '@/renderer/components/EmojiPicker/shortcodeIndex';

const SHORTCODE_RE = /:([a-z0-9_+-]+):/gi;

export const remarkEmojiShortcodes: Plugin<[], Root> = () => {
  return (tree) => {
    visit(tree, 'text', (node: Text, index, parent) => {
      if (parent === undefined || index === undefined) return;
      const parentType = (parent as { type?: string }).type;
      if (parentType === 'inlineCode' || parentType === 'code') return;

      const value = node.value;
      if (!value.includes(':')) return;

      const matches = Array.from(value.matchAll(SHORTCODE_RE));
      if (matches.length === 0) return;

      const newChildren: PhrasingContent[] = [];
      let lastIndex = 0;
      let mutated = false;

      for (const match of matches) {
        const matchIndex = match.index ?? 0;
        const code = match[1].toLowerCase();
        const emoji = lookupShortcode(code);
        if (emoji === undefined) continue;
        if (matchIndex > lastIndex) {
          newChildren.push({ type: 'text', value: value.slice(lastIndex, matchIndex) });
        }
        newChildren.push({ type: 'text', value: emoji });
        lastIndex = matchIndex + match[0].length;
        mutated = true;
      }

      if (!mutated) return;
      if (lastIndex < value.length) {
        newChildren.push({ type: 'text', value: value.slice(lastIndex) });
      }
      (parent as { children: PhrasingContent[] }).children.splice(index, 1, ...newChildren);
      return [SKIP, index + newChildren.length];
    });
  };
};

export default remarkEmojiShortcodes;
