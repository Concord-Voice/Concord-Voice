import { describe, it, expect, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import MarkdownContent from '@/renderer/components/Markdown/MarkdownContent';
import {
  indexCategory,
  resetShortcodeIndex,
} from '@/renderer/components/EmojiPicker/shortcodeIndex';
import { resetAllStores } from '../../../helpers/store-helpers';

const emptyLookup = { users: new Map(), roles: new Map() };
const userLookup = {
  users: new Map([['abc-123', 'alice']]),
  roles: new Map([['role-42', 'Admin']]),
};

function renderMd(content: string, lookup = emptyLookup) {
  return render(
    <MarkdownContent id="m1" content={content} editedAt={null} mentionLookup={lookup} />
  );
}

function getMarkdownPreRule(): string {
  const css = readFileSync(
    resolve(__dirname, '../../../../src/renderer/components/Markdown/MarkdownContent.css'),
    'utf-8'
  );
  return css.match(/\.markdown-content pre\s*\{[^}]*\}/)?.[0] ?? '';
}

describe('MarkdownContent', () => {
  beforeEach(() => {
    resetAllStores();
    resetShortcodeIndex();
    indexCategory('smileys', [{ e: '😄', n: 'smile', s: false, c: ['smile'] }]);
  });

  describe('must-have rendering', () => {
    it('renders bold', () => {
      renderMd('**hello**');
      expect(screen.getByText('hello').tagName).toBe('STRONG');
    });

    it('renders italic', () => {
      renderMd('*hello*');
      expect(screen.getByText('hello').tagName).toBe('EM');
    });

    it('renders strikethrough', () => {
      renderMd('~~hello~~');
      expect(screen.getByText('hello').tagName).toBe('DEL');
    });

    it('renders inline code', () => {
      renderMd('`hello`');
      expect(screen.getByText('hello').tagName).toBe('CODE');
    });

    it('renders fenced code block', () => {
      renderMd('```\nhello\n```');
      const code = screen.getByText(/hello/);
      expect(code.closest('pre')).not.toBeNull();
    });

    it('soft-wraps medium inline code blocks so every line remains readable', () => {
      const codeLines = Array.from({ length: 10 }, (_, index) => `line ${index + 1};`);
      const { container } = renderMd(`\`\`\`js\n${codeLines.join('\n')}\n\`\`\``);
      const pre = container.querySelector('pre');

      expect(pre).not.toBeNull();
      expect(getMarkdownPreRule()).toContain('white-space: pre-wrap');
      for (const line of codeLines) {
        expect(pre?.textContent).toContain(line);
      }
    });

    it('wraps very long code lines instead of clipping them behind horizontal-only scroll', () => {
      const longLine = `const token = '${'a'.repeat(160)}';`;
      const { container } = renderMd(`\`\`\`js\n${longLine}\n\`\`\``);
      const pre = container.querySelector('pre');
      const preRule = getMarkdownPreRule();

      expect(pre).not.toBeNull();
      expect(preRule).toContain('max-width: 100%');
      expect(preRule).toContain('overflow-wrap: anywhere');
    });

    it('preserves hljs classes on fenced code block with explicit language', () => {
      // End-to-end test for PR #711: rehype-highlight emits
      // `<code class="hljs language-js">` and inner `<span class="hljs-*">`
      // tokens; the sanitize schema must allow both the bare `hljs` theme
      // anchor on <code> AND the `hljs-*` token classes on <span>, or syntax
      // highlighting silently breaks at the sanitize layer.
      const { container } = renderMd('```js\nconst x = 42;\n```');
      const codeEl = container.querySelector('pre code');
      expect(codeEl).not.toBeNull();
      const className = codeEl?.getAttribute('class') ?? '';
      expect(className).toContain('hljs');
      expect(className).toContain('language-js');
      // Inner token spans must survive sanitize — at least one hljs-* class
      // should appear in the rendered subtree (keyword, number, string, ...).
      const tokenSpans = container.querySelectorAll('[class^="hljs-"]');
      expect(tokenSpans.length).toBeGreaterThan(0);
    });

    it('renders heading hierarchy with H1 > H2 > H3 font sizes (#807)', () => {
      renderMd('# H1\n## H2\n### H3');
      const h1 = screen.getByRole('heading', { level: 1 });
      const h2 = screen.getByRole('heading', { level: 2 });
      const h3 = screen.getByRole('heading', { level: 3 });
      // MarkdownContent.css sets h1=1.6em, h2=1.3em, h3=1.1em.
      // jsdom resolves font-size to the declared em-suffixed string.
      const sizeOf = (el: HTMLElement): number => parseFloat(getComputedStyle(el).fontSize);
      // Strict ordering invariant.
      expect(sizeOf(h1)).toBeGreaterThan(sizeOf(h2));
      expect(sizeOf(h2)).toBeGreaterThan(sizeOf(h3));
      // Ratio-anchored assertion: with the new sizes the ratios are
      // h1/h2 = 1.6/1.3 ≈ 1.23 and h2/h3 = 1.3/1.1 ≈ 1.18. The pre-#807
      // values (1.3/1.18 ≈ 1.10 and 1.18/1.08 ≈ 1.09) would fail >1.15,
      // so a revert to the compressed values would re-trigger this test.
      // Ratios are unit-independent (em or px both resolve to the same ratio).
      expect(sizeOf(h1) / sizeOf(h2)).toBeGreaterThan(1.15);
      expect(sizeOf(h2) / sizeOf(h3)).toBeGreaterThan(1.15);
    });

    it('resets paragraph margin inside loose-list items (#807)', () => {
      // GFM "loose list" — blank lines between items force <p> wrapping inside <li>.
      const { container } = renderMd('- Item 1\n\n- Item 2\n\n- Item 3');
      const items = container.querySelectorAll('li');
      expect(items.length).toBe(3);
      // Loose-list mode is proven by the presence of <p> inside each <li>.
      for (const li of items) {
        const innerP = li.querySelector('p');
        expect(innerP).not.toBeNull();
        if (innerP) {
          // The CSS `.markdown-content li > p { margin: 0 }` must apply.
          // jsdom serializes the margin shorthand as "0" (unitless) when the
          // declared value is "0"; production browsers serialize it as "0px".
          // Accept either form — both indicate the rule applied.
          const margin = getComputedStyle(innerP).margin;
          expect(['0', '0px']).toContain(margin);
        }
      }
    });

    it('renders block quote', () => {
      renderMd('> hello');
      expect(screen.getByText('hello').closest('blockquote')).not.toBeNull();
    });

    it('renders unordered list', () => {
      renderMd('- a\n- b');
      expect(screen.getByText('a').closest('ul')).not.toBeNull();
    });

    it('renders ordered list', () => {
      renderMd('1. a\n2. b');
      expect(screen.getByText('a').closest('ol')).not.toBeNull();
    });

    it('preserves the typed start number for ordered lists', () => {
      const { container } = renderMd('2. test message');
      const list = container.querySelector('ol');
      expect(list).toHaveAttribute('start', '2');
      expect(screen.getByText('test message').closest('ol')).toBe(list);
    });

    it('renders explicit link via SafeLink', () => {
      renderMd('[hello](https://example.com)');
      const link = screen.getByText('hello');
      expect(link.tagName).toBe('A');
      expect(link).toHaveAttribute('href', 'https://example.com');
    });

    it('renders autolinked URL', () => {
      renderMd('visit https://example.com today');
      const link = screen.getByText('https://example.com');
      expect(link.tagName).toBe('A');
    });

    it('emits target="_blank" so the global link-color rule applies (#800)', () => {
      // The fix for #800 hangs theme-aware link styling off the
      // a[target="_blank"] global selector in index.css. SafeLink is the
      // single source of every chat-rendered anchor and must carry the
      // marker for that styling to land. If a future refactor of SafeLink
      // drops target="_blank", this assertion turns the silent regression
      // (links go back to the invisible UA-default blue) into a failed test.
      renderMd('[click me](https://example.com)');
      const link = screen.getByText('click me');
      expect(link.tagName).toBe('A');
      expect(link).toHaveAttribute('target', '_blank');
      expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    });

    it('autolinked URLs also carry target="_blank" (#800 coverage)', () => {
      // Autolinked URLs go through the same SafeLink renderer as explicit
      // [text](url) syntax, so the marker should be present here too.
      renderMd('see https://example.com for more');
      const link = screen.getByText('https://example.com');
      expect(link).toHaveAttribute('target', '_blank');
    });

    it('renders paragraphs', () => {
      renderMd('first\n\nsecond');
      expect(screen.getByText('first').tagName).toBe('P');
      expect(screen.getByText('second').tagName).toBe('P');
    });
  });

  describe('should-have rendering', () => {
    it('renders spoiler as click-to-reveal', () => {
      renderMd('||secret||');
      const el = screen.getByText('secret');
      expect(el).toHaveClass('spoiler');
      fireEvent.click(el);
      expect(el).toHaveClass('spoiler-revealed');
    });

    it('renders headers h1/h2/h3', () => {
      renderMd('# One\n## Two\n### Three');
      expect(screen.getByText('One').tagName).toBe('H1');
      expect(screen.getByText('Two').tagName).toBe('H2');
      expect(screen.getByText('Three').tagName).toBe('H3');
    });

    it('renders horizontal rule', () => {
      const { container } = renderMd('---');
      expect(container.querySelector('hr')).not.toBeNull();
    });

    it('substitutes :smile: with 😄', () => {
      renderMd('hi :smile:');
      expect(screen.getByText(/😄/)).toBeInTheDocument();
    });
  });

  describe('mentions', () => {
    it('renders <@userId> as resolved display name', () => {
      renderMd('hey <@abc-123>', userLookup);
      expect(screen.getByText('@alice')).toBeInTheDocument();
    });

    it('renders <@&roleId> as role name', () => {
      renderMd('attention <@&role-42>', userLookup);
      expect(screen.getByText('@Admin')).toBeInTheDocument();
    });
  });

  describe('safety', () => {
    it('strips script tags', () => {
      const { container } = renderMd('hello <script>alert(1)</script>');
      expect(container.querySelector('script')).toBeNull();
    });

    it('strips img tags', () => {
      const { container } = renderMd('![alt](http://x/x.png)');
      expect(container.querySelector('img')).toBeNull();
    });

    it('strips javascript: links', () => {
      const { container } = renderMd('[x](javascript:alert(1))');
      const anchors = container.querySelectorAll('a');
      anchors.forEach((a) => {
        expect(a.getAttribute('href') || '').not.toContain('javascript:');
      });
    });
  });
});
