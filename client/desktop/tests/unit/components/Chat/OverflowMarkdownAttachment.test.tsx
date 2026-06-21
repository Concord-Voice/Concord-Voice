import { describe, it, expect, vi, beforeEach, beforeAll, afterEach, afterAll } from 'vitest';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import OverflowMarkdownAttachment from '@/renderer/components/Chat/OverflowMarkdownAttachment';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import type { AttachmentSummary } from '@/renderer/types/chat';

// ---------------------------------------------------------------------------
// Module-level mocks
// ---------------------------------------------------------------------------

vi.mock('@/renderer/components/Markdown/MarkdownContent', () => ({
  default: ({ content }: { content: string }) => (
    <div data-testid="markdown-content">{content}</div>
  ),
}));

// Mock the e2eeService with controllable getChannelKey
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getChannelKey: vi.fn(),
    decryptForChannel: vi.fn(),
  },
}));

// Mock attachmentCrypto.decryptFile so tests control the decrypted bytes
vi.mock('@/renderer/utils/attachmentCrypto', () => ({
  formatFileSize: (bytes: number) => {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  },
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
// Fixtures
// ---------------------------------------------------------------------------

const enc = new TextEncoder();

const sampleAttachment: AttachmentSummary = {
  id: 'att-1',
  file_type: 'file',
  mime_type: 'text/markdown',
  file_size: 6000,
};

// ---------------------------------------------------------------------------
// Helpers to set up mock decryptFile for a given plaintext string
// ---------------------------------------------------------------------------

async function setupDecryptFile(plaintextString: string) {
  const { decryptFile } = await import('@/renderer/utils/attachmentCrypto');
  const { e2eeService } = await import('@/renderer/services/e2eeService');
  const mockKey = {} as CryptoKey;
  (e2eeService.getChannelKey as ReturnType<typeof vi.fn>).mockResolvedValue(mockKey);
  (decryptFile as ReturnType<typeof vi.fn>).mockResolvedValue(enc.encode(plaintextString).buffer);
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('OverflowMarkdownAttachment', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  // -------------------------------------------------------------------------
  // 1. Initial render: preview + Expand button with file-size hint
  // -------------------------------------------------------------------------
  it('renders the preview body initially with an [Expand] button showing size hint', () => {
    const previewBody = 'First two hundred chars of the long message…';
    render(
      <OverflowMarkdownAttachment
        attachment={sampleAttachment}
        previewBody={previewBody}
        channelId="ch-1"
      />
    );

    expect(screen.getByTestId('markdown-content')).toHaveTextContent(previewBody);
    const expandBtn = screen.getByRole('button', { name: /expand/i });
    expect(expandBtn).toBeInTheDocument();
    expect(expandBtn.textContent).toMatch(/5\.9 KB|6\.0 KB|6 KB/);
  });

  // -------------------------------------------------------------------------
  // 2. Loading state appears after clicking Expand
  // -------------------------------------------------------------------------
  it('shows a loading spinner after clicking Expand', async () => {
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    // getChannelKey never resolves → component stays in loading state
    (e2eeService.getChannelKey as ReturnType<typeof vi.fn>).mockReturnValue(
      new Promise(() => {
        /* never resolves */
      })
    );
    server.use(
      http.get(
        'http://localhost:8080/api/v1/media/attachments/att-1',
        () =>
          new HttpResponse(new ArrayBuffer(100), {
            status: 200,
            headers: { 'Content-Type': 'application/octet-stream' },
          })
      )
    );

    render(
      <OverflowMarkdownAttachment
        attachment={sampleAttachment}
        previewBody="preview…"
        channelId="ch-1"
      />
    );
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));

    await waitFor(() => {
      expect(screen.getByRole('status')).toBeInTheDocument();
    });
    expect(screen.getByRole('status')).toHaveAttribute('aria-label', 'Loading full message');
  });

  // -------------------------------------------------------------------------
  // 3. Rendered state shows full content after fetch + decrypt succeed
  // -------------------------------------------------------------------------
  it('renders full content after expand + decrypt succeed', async () => {
    const fullContent = '# Full Heading\n\nFull body of the overflow message.';
    await setupDecryptFile(fullContent);
    server.use(
      http.get(
        'http://localhost:8080/api/v1/media/attachments/att-1',
        () =>
          new HttpResponse(enc.encode('encrypted-blob-mock').buffer, {
            status: 200,
            headers: { 'Content-Type': 'application/octet-stream' },
          })
      )
    );

    render(
      <OverflowMarkdownAttachment
        attachment={sampleAttachment}
        previewBody="preview…"
        channelId="ch-1"
      />
    );
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));

    await waitFor(() => {
      expect(screen.getByTestId('markdown-content')).toHaveTextContent(/Full Heading/);
    });
    expect(screen.getByTestId('markdown-content')).toHaveTextContent(/Full body of the overflow/);
    // Preview should be replaced — not duplicated
    expect(screen.getByTestId('markdown-content').textContent).not.toContain('preview…');
  });

  // -------------------------------------------------------------------------
  // 4. Fallback to file chip on validation failure (NULL byte in content)
  // -------------------------------------------------------------------------
  it('falls back to file chip when decrypted content fails isRenderableMarkdown', async () => {
    // NULL byte (\x00) is in the control-char reject set
    const corruptText = 'hello\x00world';
    await setupDecryptFile(corruptText);
    server.use(
      http.get(
        'http://localhost:8080/api/v1/media/attachments/att-1',
        () =>
          new HttpResponse(enc.encode('mock').buffer, {
            status: 200,
            headers: { 'Content-Type': 'application/octet-stream' },
          })
      )
    );

    render(
      <OverflowMarkdownAttachment
        attachment={sampleAttachment}
        previewBody="preview…"
        channelId="ch-1"
      />
    );
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));

    await waitFor(() => {
      expect(screen.getByText(/preview unavailable/i)).toBeInTheDocument();
    });
  });

  // -------------------------------------------------------------------------
  // 5. Fallback to file chip when content exceeds MAX_RENDERABLE_MD_BYTES
  // -------------------------------------------------------------------------
  it('falls back with "too large" message when decrypted content exceeds 256 KiB', async () => {
    const oversize = 'a'.repeat(300_000); // 300 KiB > 256 KiB cap
    await setupDecryptFile(oversize);
    server.use(
      http.get(
        'http://localhost:8080/api/v1/media/attachments/att-1',
        () =>
          new HttpResponse(enc.encode('mock').buffer, {
            status: 200,
            headers: { 'Content-Type': 'application/octet-stream' },
          })
      )
    );

    render(
      <OverflowMarkdownAttachment
        attachment={{ ...sampleAttachment, file_size: 300_000 }}
        previewBody="preview…"
        channelId="ch-1"
      />
    );
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));

    await waitFor(() => {
      expect(screen.getByText(/too large to preview/i)).toBeInTheDocument();
    });
  });

  // -------------------------------------------------------------------------
  // 6. Memoization: collapse + re-expand does not re-fetch or re-decrypt
  // -------------------------------------------------------------------------
  it('memoizes decrypted content — re-expand after collapse does not re-decrypt', async () => {
    const { decryptFile } = await import('@/renderer/utils/attachmentCrypto');
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const mockKey = {} as CryptoKey;
    const decryptSpy = vi.fn().mockResolvedValue(enc.encode('full content').buffer as ArrayBuffer);
    (e2eeService.getChannelKey as ReturnType<typeof vi.fn>).mockResolvedValue(mockKey);
    (decryptFile as ReturnType<typeof vi.fn>).mockImplementation(decryptSpy);

    server.use(
      http.get(
        'http://localhost:8080/api/v1/media/attachments/att-1',
        () =>
          new HttpResponse(enc.encode('mock').buffer, {
            status: 200,
            headers: { 'Content-Type': 'application/octet-stream' },
          })
      )
    );

    render(
      <OverflowMarkdownAttachment
        attachment={sampleAttachment}
        previewBody="preview…"
        channelId="ch-1"
      />
    );

    // First expand
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));
    await waitFor(() => expect(screen.getByText(/full content/)).toBeInTheDocument());

    // Collapse
    fireEvent.click(screen.getByRole('button', { name: /collapse/i }));
    expect(screen.getByRole('button', { name: /expand/i })).toBeInTheDocument();

    // Re-expand
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));
    await waitFor(() => expect(screen.getByText(/full content/)).toBeInTheDocument());

    // decryptFile called exactly once — memoization works
    expect(decryptSpy).toHaveBeenCalledTimes(1);
  });

  // -------------------------------------------------------------------------
  // 7. Unmount during in-flight expand does not trigger setState warnings
  // -------------------------------------------------------------------------
  it('does not setState after unmount during in-flight expand', async () => {
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const { decryptFile } = await import('@/renderer/utils/attachmentCrypto');

    // Mock getChannelKey to resolve (so fetch completes) but decryptFile to hang forever
    (e2eeService.getChannelKey as ReturnType<typeof vi.fn>).mockResolvedValue({} as CryptoKey);
    (decryptFile as ReturnType<typeof vi.fn>).mockImplementation(
      () =>
        new Promise(() => {
          /* never resolves */
        })
    );

    server.use(
      http.get(
        'http://localhost:8080/api/v1/media/attachments/att-1',
        () =>
          new HttpResponse(new ArrayBuffer(100), {
            status: 200,
            headers: { 'Content-Type': 'application/octet-stream' },
          })
      )
    );

    const consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { unmount } = render(
      <OverflowMarkdownAttachment
        attachment={sampleAttachment}
        previewBody="preview…"
        channelId="ch-1"
      />
    );

    fireEvent.click(screen.getByRole('button', { name: /expand/i }));
    // Loading state is now active
    await screen.findByRole('status');

    // Unmount while decrypt is in flight
    unmount();

    // Wait a tick — if the cleanup is wrong, React would warn about setState on
    // unmounted component. With the cancelled flag in place, no warning fires.
    await new Promise((r) => setTimeout(r, 50));

    expect(consoleErrorSpy).not.toHaveBeenCalledWith(expect.stringContaining('unmounted'));

    consoleErrorSpy.mockRestore();
  });

  // -------------------------------------------------------------------------
  // 8. Focus management — Collapse button is focused after entering rendered state
  // -------------------------------------------------------------------------
  it('focuses the Collapse button after entering rendered state', async () => {
    const { decryptFile } = await import('@/renderer/utils/attachmentCrypto');
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const mockKey = {} as CryptoKey;
    (e2eeService.getChannelKey as ReturnType<typeof vi.fn>).mockResolvedValue(mockKey);
    (decryptFile as ReturnType<typeof vi.fn>).mockResolvedValue(
      enc.encode('# Full Content').buffer as ArrayBuffer
    );

    server.use(
      http.get(
        'http://localhost:8080/api/v1/media/attachments/att-1',
        () =>
          new HttpResponse(enc.encode('mock').buffer, {
            status: 200,
            headers: { 'Content-Type': 'application/octet-stream' },
          })
      )
    );

    render(
      <OverflowMarkdownAttachment
        attachment={sampleAttachment}
        previewBody="preview…"
        channelId="ch-1"
      />
    );

    fireEvent.click(screen.getByRole('button', { name: /expand/i }));

    // Wait for rendered state — Collapse button should receive focus
    const collapseButton = await screen.findByRole('button', { name: /collapse/i });
    expect(collapseButton).toHaveFocus();
  });
});
