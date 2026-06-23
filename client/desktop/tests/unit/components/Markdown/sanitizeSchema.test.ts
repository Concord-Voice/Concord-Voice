import { describe, it, expect } from 'vitest';
import { unified } from 'unified';
import rehypeParse from 'rehype-parse';
import rehypeSanitize from 'rehype-sanitize';
import rehypeStringify from 'rehype-stringify';
import { sanitizeSchema } from '@/renderer/components/Markdown/sanitizeSchema';

async function sanitizeHtml(input: string): Promise<string> {
  const file = await unified()
    .use(rehypeParse, { fragment: true })
    .use(rehypeSanitize, sanitizeSchema)
    .use(rehypeStringify)
    .process(input);
  return String(file);
}

describe('sanitizeSchema', () => {
  it('allows strong, em, del, code inline formatting', async () => {
    const html = await sanitizeHtml(
      '<p><strong>b</strong> <em>i</em> <del>d</del> <code>c</code></p>'
    );
    expect(html).toContain('<strong>b</strong>');
    expect(html).toContain('<em>i</em>');
    expect(html).toContain('<del>d</del>');
    expect(html).toContain('<code>c</code>');
  });

  it('strips img, iframe, video, audio, embed, object, svg, math tags', async () => {
    const tags = ['img', 'iframe', 'video', 'audio', 'embed', 'object', 'svg', 'math'];
    for (const tag of tags) {
      const html = await sanitizeHtml(`<${tag} src="x"></${tag}>`);
      expect(html).not.toContain(`<${tag}`);
    }
  });

  it('strips script, style, link, meta, form, input, button', async () => {
    const tags = ['script', 'style', 'link', 'meta', 'form', 'input', 'button'];
    for (const tag of tags) {
      const html = await sanitizeHtml(`<${tag}>x</${tag}>`);
      expect(html).not.toContain(`<${tag}`);
    }
  });

  it('strips event-handler attributes (onclick, onerror, onload)', async () => {
    const html = await sanitizeHtml('<a href="http://example.com" onclick="alert(1)">x</a>');
    expect(html).not.toContain('onclick');
    expect(html).toContain('href="http://example.com"');
  });

  it('rejects dangerous URL protocols on a[href]', async () => {
    const dangerous = [
      'javascript:alert(1)',
      'data:text/html,abc',
      'file:///etc/passwd',
      'blob:http://example.com/x',
      'vbscript:msgbox(1)',
    ];
    for (const href of dangerous) {
      const html = await sanitizeHtml(`<a href="${href}">x</a>`);
      expect(html).not.toContain(`href="${href}"`);
    }
  });

  it('allows http, https, mailto URL protocols', async () => {
    const safe = ['http://a', 'https://a', 'mailto:a@b'];
    for (const href of safe) {
      const html = await sanitizeHtml(`<a href="${href}">x</a>`);
      expect(html).toContain(`href="${href}"`);
    }
  });

  it('allows numeric ordered-list start values', async () => {
    const html = await sanitizeHtml('<ol start="2"><li>second</li></ol>');
    expect(html).toContain('<ol start="2">');
  });

  it('rejects non-numeric ordered-list start values', async () => {
    const html = await sanitizeHtml('<ol start="2x"><li>second</li></ol>');
    expect(html).not.toContain('start=');
  });

  it('allows class values starting with hljs- or language- on code', async () => {
    const html = await sanitizeHtml('<code class="hljs-keyword">if</code>');
    expect(html).toContain('class="hljs-keyword"');
  });

  it('allows the bare hljs class on code (rehype-highlight theme anchor)', async () => {
    // rehype-highlight emits `<code class="hljs language-xxx">` where the
    // bare `hljs` token is the CSS selector anchor for theme background +
    // default colors. Stripping it breaks highlighting even if per-token
    // classes survive. Regression test for PR #711.
    const html = await sanitizeHtml('<code class="hljs language-javascript">if</code>');
    expect(html).toContain('hljs');
    expect(html).toContain('language-javascript');
  });

  it('allows span classes in the allowlist', async () => {
    for (const cls of ['emoji', 'spoiler', 'spoiler-revealed', 'mention-highlight']) {
      const html = await sanitizeHtml(`<span class="${cls}">x</span>`);
      expect(html).toContain(`class="${cls}"`);
    }
  });

  it('allows hljs-* token classes on span (rehype-highlight inner tokens)', async () => {
    // rehype-highlight wraps keywords, strings, comments, etc. in
    // <span class="hljs-keyword"> etc. These must survive sanitize or
    // the code block loses all token colors. Regression test for #711.
    for (const cls of ['hljs-keyword', 'hljs-string', 'hljs-comment', 'hljs-number']) {
      const html = await sanitizeHtml(`<span class="${cls}">x</span>`);
      expect(html).toContain(`class="${cls}"`);
    }
  });

  it('allows the bare hljs class on span (defense-in-depth)', async () => {
    // Defensive: some highlight.js themes may emit a bare hljs on inner
    // spans. Accept it rather than silently strip.
    const html = await sanitizeHtml('<span class="hljs">x</span>');
    expect(html).toContain('class="hljs"');
  });

  it('rejects unknown span classes', async () => {
    const html = await sanitizeHtml('<span class="evil">x</span>');
    expect(html).not.toContain('evil');
  });

  it('rejects unknown code classes', async () => {
    const html = await sanitizeHtml('<code class="evil">x</code>');
    expect(html).not.toContain('class="evil"');
  });

  it('allows data-mention attribute on mention-highlight spans with valid token', async () => {
    const html = await sanitizeHtml(
      '<span class="mention-highlight" data-mention="<@abc-123>">@alice</span>'
    );
    // rehype-stringify in this version does not HTML-encode `<` in attribute
    // values; the core contract is that the valid token survives sanitization.
    expect(html).toMatch(/data-mention="(<|&#x3C;)@abc-123>"/);
    expect(html).toContain('class="mention-highlight"');
  });

  it('rejects invalid data-mention values', async () => {
    const html = await sanitizeHtml(
      '<span class="mention-highlight" data-mention="<script>alert(1)</script>">x</span>'
    );
    expect(html).not.toContain('alert(1)');
  });
});
