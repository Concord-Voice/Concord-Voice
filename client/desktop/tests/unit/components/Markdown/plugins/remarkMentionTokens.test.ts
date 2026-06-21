import { describe, it, expect } from 'vitest';
import { unified } from 'unified';
import remarkParse from 'remark-parse';
import remarkGfm from 'remark-gfm';
import { remarkMentionTokens } from '@/renderer/components/Markdown/plugins/remarkMentionTokens';
import type { Root } from 'mdast';

function collectMentions(tree: Root): string[] {
  const out: string[] = [];
  const visit = (node: unknown): void => {
    if (typeof node !== 'object' || node === null) return;
    const n = node as { type?: string; value?: string; children?: unknown[] };
    if (n.type === 'mention-token' && typeof n.value === 'string') out.push(n.value);
    if (Array.isArray(n.children)) n.children.forEach(visit);
  };
  visit(tree);
  return out;
}

async function parse(input: string): Promise<Root> {
  const processor = unified().use(remarkParse).use(remarkGfm).use(remarkMentionTokens);
  const tree = processor.parse(input);
  return (await processor.run(tree)) as Root;
}

describe('remarkMentionTokens', () => {
  it('preserves <@userId> atomically', async () => {
    const tree = await parse('hi <@abc-123>');
    expect(collectMentions(tree)).toEqual(['<@abc-123>']);
  });

  it('preserves <@&roleId>', async () => {
    const tree = await parse('role <@&role-42> ping');
    expect(collectMentions(tree)).toEqual(['<@&role-42>']);
  });

  it('preserves @username', async () => {
    const tree = await parse('hey @alice!');
    expect(collectMentions(tree)).toEqual(['@alice']);
  });

  it('preserves @all and @here', async () => {
    const tree = await parse('@all @here');
    expect(collectMentions(tree)).toEqual(['@all', '@here']);
  });

  it('does not preserve mentions inside inline code', async () => {
    const tree = await parse('`<@abc>`');
    expect(collectMentions(tree)).toEqual([]);
  });

  it('bold around a mention does not swallow the mention', async () => {
    const tree = await parse('**<@abc-123>**');
    expect(collectMentions(tree)).toEqual(['<@abc-123>']);
  });
});
