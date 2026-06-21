import { describe, it, expect } from 'vitest';
import { unified } from 'unified';
import remarkParse from 'remark-parse';
import remarkRehype from 'remark-rehype';
import rehypeStringify from 'rehype-stringify';
import { rehypeInlineEmoji } from '@/renderer/components/Markdown/plugins/rehypeInlineEmoji';

async function process(md: string): Promise<string> {
  const file = await unified()
    .use(remarkParse)
    .use(remarkRehype)
    .use(rehypeInlineEmoji)
    .use(rehypeStringify)
    .process(md);
  return String(file);
}

describe('rehypeInlineEmoji', () => {
  it('wraps a single emoji inline in mixed text with span.emoji', async () => {
    const html = await process('hello 😄 world');
    expect(html).toContain('<span class="emoji">😄</span>');
    expect(html).toContain('hello ');
    expect(html).toContain(' world');
  });

  it('wraps multiple emojis in the same paragraph', async () => {
    const html = await process('a 😀 b 😎 c');
    expect(html).toContain('<span class="emoji">😀</span>');
    expect(html).toContain('<span class="emoji">😎</span>');
  });

  it('leaves text without emoji untouched', async () => {
    const html = await process('plain text only');
    expect(html).not.toContain('class="emoji"');
    expect(html).toContain('plain text only');
  });

  it('wraps emoji with ZWJ sequence (👨‍👩‍👧)', async () => {
    const html = await process('family 👨‍👩‍👧 here');
    expect(html).toContain('class="emoji"');
    expect(html).toContain('👨‍👩‍👧');
  });

  it('wraps emoji with skin-tone modifier (👋🏽)', async () => {
    const html = await process('wave 👋🏽 hi');
    expect(html).toContain('class="emoji"');
    expect(html).toContain('👋🏽');
  });

  it('does not wrap emoji inside code fences', async () => {
    const html = await process('```\n😄\n```');
    // code fences produce <pre><code>...</code></pre>, emoji is inside <code>
    // The plugin should leave code-node text alone
    expect(html).toContain('<code');
    // Verify the emoji didn't get a span wrapper inside the code block
    const codeBlockMatch = html.match(/<code[^>]*>([\s\S]*?)<\/code>/);
    expect(codeBlockMatch).not.toBeNull();
    if (codeBlockMatch) {
      expect(codeBlockMatch[1]).not.toContain('class="emoji"');
    }
  });

  it('does not wrap emoji inside inline code', async () => {
    const html = await process('`😄`');
    const codeMatch = html.match(/<code[^>]*>([\s\S]*?)<\/code>/);
    expect(codeMatch).not.toBeNull();
    if (codeMatch) {
      expect(codeMatch[1]).not.toContain('class="emoji"');
    }
  });
});
