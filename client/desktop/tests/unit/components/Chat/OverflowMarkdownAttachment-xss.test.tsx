/**
 * XSS corpus regression through OverflowMarkdownAttachment.
 *
 * Spec §7.3 architectural property: reusing <MarkdownContent> unchanged means
 * the existing XSS corpus continues to apply through the new overflow render
 * path. This test codifies that invariant so any future change that breaks the
 * sanitization pipeline fails CI.
 *
 * NOTE: MarkdownContent is intentionally NOT mocked here — the real component
 * and its rehypeSanitize pipeline must run for the XSS assertions to be
 * meaningful.
 */
import { describe, it, expect, vi, beforeEach, beforeAll, afterEach, afterAll } from 'vitest';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import OverflowMarkdownAttachment from '@/renderer/components/Chat/OverflowMarkdownAttachment';
import { XSS_PAYLOADS } from '@/renderer/components/Markdown/markdown-xss-corpus';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';

// ---------------------------------------------------------------------------
// Module-level mocks — only crypto/network layers; MarkdownContent is real.
// ---------------------------------------------------------------------------

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getChannelKey: vi.fn(),
    decryptForChannel: vi.fn(),
  },
}));

vi.mock('@/renderer/utils/attachmentCrypto', () => ({
  formatFileSize: (bytes: number) => `${bytes} B`,
  decryptFile: vi.fn(),
}));

// ---------------------------------------------------------------------------
// MSW server
// ---------------------------------------------------------------------------

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

// ---------------------------------------------------------------------------
// Denial sets — mirrored from markdown-xss-corpus.test.tsx
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const enc = new TextEncoder();

async function setupDecryptFile(plaintextString: string) {
  const { decryptFile } = await import('@/renderer/utils/attachmentCrypto');
  const { e2eeService } = await import('@/renderer/services/e2eeService');
  (e2eeService.getChannelKey as ReturnType<typeof vi.fn>).mockResolvedValue({} as CryptoKey);
  (decryptFile as ReturnType<typeof vi.fn>).mockResolvedValue(enc.encode(plaintextString).buffer);
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('OverflowMarkdownAttachment XSS corpus regression', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  for (const payload of XSS_PAYLOADS) {
    it(`sanitizes corpus entry: ${payload.name}`, async () => {
      await setupDecryptFile(payload.input);

      server.use(
        http.get(
          'http://localhost:8080/api/v1/media/attachments/att-xss',
          () =>
            new HttpResponse(enc.encode('mock-ciphertext').buffer, {
              status: 200,
              headers: { 'Content-Type': 'application/octet-stream' },
            })
        )
      );

      const { container } = render(
        <OverflowMarkdownAttachment
          attachment={{
            id: 'att-xss',
            file_type: 'file',
            mime_type: 'text/markdown',
            file_size: payload.input.length,
          }}
          previewBody="preview…"
          channelId="ch-1"
        />
      );

      // Click Expand to trigger fetch + decrypt + render through MarkdownContent
      fireEvent.click(screen.getByRole('button', { name: /expand/i }));

      // Wait for the rendered state — the Collapse button appears when content is ready.
      // If the content fails isRenderableMarkdown the component falls back to the
      // file-chip (preview-unavailable), which contains no dangerous HTML anyway.
      // Either outcome satisfies the XSS invariant.
      await waitFor(
        () => {
          const collapseBtn = container.querySelector('.overflow-md-attachment__collapse');
          const fallback = container.querySelector('.overflow-md-attachment--fallback');
          expect(collapseBtn !== null || fallback !== null).toBe(true);
        },
        { timeout: 3000 }
      );

      // Scope assertions to the MarkdownContent subtree (.markdown-content) only.
      // OverflowMarkdownAttachment renders its own <button> elements for
      // Expand/Collapse — those are legitimate UI chrome, not attacker-controlled
      // content, and must not be included in the XSS scan.
      const mdRoot = container.querySelector('.markdown-content');

      // If the component fell back to the file-chip (preview-unavailable or too-large),
      // there is no .markdown-content subtree with attacker payload — the fallback
      // renders static UI only. The XSS invariant is trivially satisfied.
      if (mdRoot === null) return;

      // Assert: no denied tags survive the sanitizer within MarkdownContent output
      mdRoot.querySelectorAll('*').forEach((el) => {
        expect(
          DENIED_TAGS.has(el.tagName),
          `Denied tag rendered: ${el.tagName} for payload "${payload.name}"`
        ).toBe(false);
      });

      // Assert: no dangerous href protocols within MarkdownContent output
      mdRoot.querySelectorAll('a').forEach((a) => {
        const href = a.getAttribute('href') ?? '';
        expect(
          DENIED_PROTOCOLS.test(href),
          `Dangerous href allowed: ${href} for payload "${payload.name}"`
        ).toBe(false);
      });

      // Assert: no event-handler attributes on any element within MarkdownContent output
      mdRoot.querySelectorAll('*').forEach((el) => {
        for (const attr of Array.from(el.attributes)) {
          expect(
            attr.name.startsWith('on'),
            `Event attribute ${attr.name} present on ${el.tagName} for payload "${payload.name}"`
          ).toBe(false);
        }
      });
    });
  }
});
