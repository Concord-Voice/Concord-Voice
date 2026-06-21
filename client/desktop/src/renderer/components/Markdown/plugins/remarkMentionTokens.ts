import type { Plugin } from 'unified';
import type { Root, Text, PhrasingContent } from 'mdast';
import { visit, SKIP } from 'unist-util-visit';

const MENTION_RE = /<@&?[\w-]+>|@[\w.-]+/g;

interface MentionTokenNode {
  type: 'mention-token';
  value: string;
  data: { hName: string; hProperties: { className: string[]; dataMention: string } };
}

export const remarkMentionTokens: Plugin<[], Root> = () => {
  return (tree) => {
    visit(tree, 'text', (node: Text, index, parent) => {
      if (parent === undefined || index === undefined) return;
      const parentType = (parent as { type?: string }).type;
      if (parentType === 'inlineCode' || parentType === 'code') return;

      const value = node.value;
      if (!value.includes('@')) return;

      const matches = Array.from(value.matchAll(MENTION_RE));
      if (matches.length === 0) return;

      const newChildren: PhrasingContent[] = [];
      let lastIndex = 0;
      let mutated = false;

      for (const match of matches) {
        const matchIndex = match.index ?? 0;
        if (matchIndex > lastIndex) {
          newChildren.push({ type: 'text', value: value.slice(lastIndex, matchIndex) });
        }
        const token: MentionTokenNode = {
          type: 'mention-token',
          value: match[0],
          data: {
            hName: 'span',
            hProperties: {
              className: ['mention-highlight'],
              dataMention: match[0],
            },
          },
        };
        newChildren.push(token as unknown as PhrasingContent);
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

export default remarkMentionTokens;
