import type { Plugin } from 'unified';
import type { Root, Element, Text, ElementContent } from 'hast';
import { visit, SKIP } from 'unist-util-visit';

// Match complete emoji sequences. Mirror the pattern from messageUtils.EMOJI_REGEX
// so the rendered .emoji class behavior is identical to the legacy path.
const EMOJI_ATOM = /(?:\p{Emoji_Presentation}|\p{Emoji}\uFE0F)/u;
const EMOJI_RE = new RegExp(
  String.raw`(?:\p{Regional_Indicator}{2}` +
    String.raw`|\p{Emoji_Presentation}[\u{E0020}-\u{E007E}]+\u{E007F}` +
    String.raw`|${EMOJI_ATOM.source}(?:\u{200D}${EMOJI_ATOM.source}|\p{Emoji_Modifier}|\uFE0F?\u{20E3})*)`,
  'gu'
);

export const rehypeInlineEmoji: Plugin<[], Root> = () => {
  return (tree) => {
    visit(tree, 'text', (node: Text, index, parent) => {
      if (parent === undefined || index === undefined) return;
      if ((parent as Element).tagName === 'code' || (parent as Element).tagName === 'pre') {
        return;
      }

      const value = node.value;
      if (!value) return;

      const matches = Array.from(value.matchAll(EMOJI_RE));
      if (matches.length === 0) return;

      const newChildren: ElementContent[] = [];
      let lastIndex = 0;

      for (const match of matches) {
        const matchIndex = match.index ?? 0;
        if (matchIndex > lastIndex) {
          newChildren.push({
            type: 'text',
            value: value.slice(lastIndex, matchIndex),
          });
        }
        newChildren.push({
          type: 'element',
          tagName: 'span',
          properties: { className: ['emoji'] },
          children: [{ type: 'text', value: match[0] }],
        });
        lastIndex = matchIndex + match[0].length;
      }
      if (lastIndex < value.length) {
        newChildren.push({
          type: 'text',
          value: value.slice(lastIndex),
        });
      }

      (parent as { children: ElementContent[] }).children.splice(index, 1, ...newChildren);
      return [SKIP, index + newChildren.length];
    });
  };
};

export default rehypeInlineEmoji;
