import { describe, it, expect, beforeEach } from 'vitest';
import { render } from '../../../test-utils';
import MarkdownContent from '@/renderer/components/Markdown/MarkdownContent';
import { XSS_PAYLOADS } from '@/renderer/components/Markdown/markdown-xss-corpus';
import { resetAllStores } from '../../../helpers/store-helpers';

const emptyLookup = { users: new Map(), roles: new Map() };

const DENIED_TAGS = new Set([
  'IMG',
  'IFRAME',
  'VIDEO',
  'AUDIO',
  'EMBED',
  'OBJECT',
  'SVG',
  'MATH',
  'SCRIPT',
  'STYLE',
  'LINK',
  'META',
  'FORM',
  'INPUT',
  'BUTTON',
  'TEXTAREA',
  'SELECT',
  'CANVAS',
  'NOSCRIPT',
  'BASE',
  'APPLET',
  'MARQUEE',
  'DETAILS',
  'SUMMARY',
]);

const DENIED_PROTOCOLS = /^(javascript|data|file|blob|vbscript):/i;

describe('Markdown XSS corpus', () => {
  beforeEach(() => {
    resetAllStores();
  });

  for (const payload of XSS_PAYLOADS) {
    it(`payload: ${payload.name}`, () => {
      const { container } = render(
        <MarkdownContent
          id="xss"
          content={payload.input}
          editedAt={null}
          mentionLookup={emptyLookup}
        />
      );

      container.querySelectorAll('*').forEach((el) => {
        expect(
          DENIED_TAGS.has(el.tagName),
          `Denied tag rendered: ${el.tagName} for ${payload.name}`
        ).toBe(false);
      });

      container.querySelectorAll('a').forEach((a) => {
        const href = a.getAttribute('href') ?? '';
        expect(
          DENIED_PROTOCOLS.test(href),
          `Dangerous href allowed: ${href} for ${payload.name}`
        ).toBe(false);
      });

      container.querySelectorAll('*').forEach((el) => {
        for (const attr of Array.from(el.attributes)) {
          expect(
            attr.name.startsWith('on'),
            `Event attribute ${attr.name} present on ${el.tagName} for ${payload.name}`
          ).toBe(false);
        }
      });
    });
  }
});
