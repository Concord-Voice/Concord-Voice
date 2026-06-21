import { describe, it, expect } from 'vitest';
import { unified } from 'unified';
import remarkParse from 'remark-parse';
import { remarkSpoiler } from '@/renderer/components/Markdown/plugins/remarkSpoiler';
import type { Root } from 'mdast';

async function parseToAst(input: string): Promise<Root> {
  const processor = unified().use(remarkParse).use(remarkSpoiler);
  const tree = processor.parse(input);
  return (await processor.run(tree)) as Root;
}

function collectSpoilers(tree: Root): string[] {
  const out: string[] = [];
  const visit = (node: unknown): void => {
    if (typeof node !== 'object' || node === null) return;
    const n = node as { type?: string; value?: unknown; children?: unknown[] };
    if (n.type === 'spoiler') {
      const texts = (n.children ?? [])
        .filter((c) => typeof c === 'object' && c !== null && (c as { type?: string }).type === 'text')
        .map((c) => (c as { value: string }).value);
      out.push(texts.join(''));
      return;
    }
    if (Array.isArray(n.children)) n.children.forEach(visit);
  };
  visit(tree);
  return out;
}

describe('remarkSpoiler', () => {
  it('parses ||text|| into a spoiler node', async () => {
    const tree = await parseToAst('hello ||secret|| world');
    expect(collectSpoilers(tree)).toEqual(['secret']);
  });

  it('parses multiple spoilers in one paragraph', async () => {
    const tree = await parseToAst('||a|| and ||b||');
    expect(collectSpoilers(tree)).toEqual(['a', 'b']);
  });

  it('does not treat single || as a spoiler', async () => {
    const tree = await parseToAst('pipe || only');
    expect(collectSpoilers(tree)).toEqual([]);
  });

  it('does not parse spoilers inside inline code', async () => {
    const tree = await parseToAst('`||not a spoiler||`');
    expect(collectSpoilers(tree)).toEqual([]);
  });

  it('does not parse spoilers inside fenced code blocks', async () => {
    const tree = await parseToAst('```\n||not a spoiler||\n```');
    expect(collectSpoilers(tree)).toEqual([]);
  });

  it('handles empty spoiler as literal text (no substitution)', async () => {
    const tree = await parseToAst('before |||| after');
    expect(collectSpoilers(tree)).toEqual([]);
  });
});
