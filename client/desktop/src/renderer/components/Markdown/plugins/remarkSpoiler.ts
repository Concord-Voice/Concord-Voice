import type { Plugin } from 'unified';
import type { Root, Text, PhrasingContent } from 'mdast';
import { visit, SKIP } from 'unist-util-visit';

interface SpoilerNode {
  type: 'spoiler';
  children: PhrasingContent[];
  data?: { hName: string; hProperties: { className: string[] } };
}

// Matches ||text|| where text has at least one non-pipe character.
// Non-greedy inner group ensures we pair the closest delimiters.
const SPOILER_RE = /\|\|([^|]+?)\|\|/g;

export const remarkSpoiler: Plugin<[], Root> = () => {
  return (tree) => {
    visit(tree, 'text', (node: Text, index, parent) => {
      if (parent === undefined || index === undefined) return;
      const parentType = (parent as { type?: string }).type;
      // Defensive skip for code contexts (in practice mdast inlineCode has a
      // string `value` not text children, so this never fires there).
      if (parentType === 'inlineCode' || parentType === 'code') return;

      const value = node.value;
      if (!value.includes('||')) return;

      const matches = Array.from(value.matchAll(SPOILER_RE));
      if (matches.length === 0) return;

      const newChildren: PhrasingContent[] = [];
      let lastIndex = 0;
      for (const match of matches) {
        const matchStart = match.index ?? 0;
        if (matchStart > lastIndex) {
          newChildren.push({ type: 'text', value: value.slice(lastIndex, matchStart) });
        }
        const inner = match[1];
        const spoiler: SpoilerNode = {
          type: 'spoiler',
          children: [{ type: 'text', value: inner }],
          data: { hName: 'span', hProperties: { className: ['spoiler'] } },
        };
        newChildren.push(spoiler as unknown as PhrasingContent);
        lastIndex = matchStart + match[0].length;
      }
      if (lastIndex < value.length) {
        newChildren.push({ type: 'text', value: value.slice(lastIndex) });
      }
      (parent as { children: PhrasingContent[] }).children.splice(index, 1, ...newChildren);
      return [SKIP, index + newChildren.length];
    });
  };
};

export default remarkSpoiler;
