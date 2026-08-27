import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '../../../test-utils';
import userEvent from '@testing-library/user-event';
import AttachmentDisplay, {
  extFromMime,
  __resetBlobCacheForTests,
} from '@/renderer/components/Chat/AttachmentDisplay';
import type { AttachmentSummary } from '@/renderer/types/chat';
import { mockAttachment, mockAttachment2 } from '../../../mocks/fixtures';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { fireEvent } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';
import { MAX_DECRYPTABLE_ATTACHMENT_BYTES } from '@/renderer/utils/entitlementLimits';
import { AttachmentTooLargeError } from '@/renderer/utils/boundedResponseBody';

// Mock OverflowMarkdownAttachment so we can assert dispatch without exercising
// the full decrypt/expand path (covered by OverflowMarkdownAttachment.test.tsx).
vi.mock('@/renderer/components/Chat/OverflowMarkdownAttachment', () => ({
  __esModule: true,
  default: ({
    attachment,
    previewBody,
    channelId,
  }: {
    attachment: AttachmentSummary;
    previewBody: string;
    channelId: string;
  }) => (
    <div data-testid="overflow-md" data-attachment-id={attachment.id} data-channel-id={channelId}>
      OVERFLOW: {previewBody}
    </div>
  ),
}));

// Mock apiClient
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

// Mock e2eeService
const mockGetChannelKey = vi.fn();
const mockGetChannelKeyByVersion = vi.fn();
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return true;
    },
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
    getChannelKeyByVersion: (...args: unknown[]) => mockGetChannelKeyByVersion(...args),
  },
}));

/** decryptAttachmentBlob is the format-aware entry point the component now uses.
 *
 *  Default behaviour mirrors production: dispatch to the (mocked) legacy
 *  decryptFile and wrap the result in a Blob. The Blob's `size` is shadowed
 *  with the decrypted byteLength so `bufferOfDeclaredSize` keeps working — the
 *  cache accounts the DECRYPTED payload, and a real Blob would report the 8
 *  bytes the fake actually holds rather than the size under test. Real format
 *  dispatch is covered at the unit level in attachmentChunkedCrypto.test.ts. */
const mockDecryptAttachmentBlob = vi.fn();
vi.mock('@/renderer/utils/attachmentChunkedCrypto', async () => {
  const actual = await vi.importActual<typeof import('@/renderer/utils/attachmentChunkedCrypto')>(
    '@/renderer/utils/attachmentChunkedCrypto'
  );
  return {
    ...actual,
    decryptAttachmentBlob: (...args: unknown[]) => mockDecryptAttachmentBlob(...args),
  };
});

import { BLOB_CACHE_RETAIN_MAX_BYTES } from '@/renderer/components/Chat/AttachmentDisplay';
import {
  UnsupportedAttachmentFormatError,
  AttachmentIntegrityError,
} from '@/renderer/utils/attachmentChunkedCrypto';

// Mock decryptFile
const mockDecryptFile = vi.fn();
vi.mock('@/renderer/utils/attachmentCrypto', async () => {
  const actual = await vi.importActual<typeof import('@/renderer/utils/attachmentCrypto')>(
    '@/renderer/utils/attachmentCrypto'
  );
  return {
    ...actual,
    decryptFile: (...args: unknown[]) => mockDecryptFile(...args),
  };
});

function mockFetchSuccess(data = new ArrayBuffer(100), mimeType = 'image/png') {
  mockApiFetch.mockResolvedValue({
    ok: true,
    arrayBuffer: () => Promise.resolve(data),
    headers: new Headers({ 'X-File-Mime-Type': mimeType }),
  });
}

function mockFetchFailure() {
  mockApiFetch.mockResolvedValue({
    ok: false,
    status: 500,
  });
}

// Helper to override IntersectionObserver so it fires immediately
let ioCallback: IntersectionObserverCallback | null = null;
function installImmediateIO() {
  (globalThis as unknown as Record<string, unknown>).IntersectionObserver = class {
    constructor(cb: IntersectionObserverCallback) {
      ioCallback = cb;
    }
    observe() {
      // Immediately trigger intersection
      if (ioCallback) {
        ioCallback(
          [{ isIntersecting: true } as IntersectionObserverEntry],
          this as unknown as IntersectionObserver
        );
      }
    }
    disconnect = vi.fn();
    unobserve = vi.fn();
  };
}

const OriginalIO = globalThis.IntersectionObserver;

describe('AttachmentDisplay', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Vitest isolates modules per FILE, not per test, so the blob cache and its
    // byte accounting persist across every test here. The budget assertions are
    // absolute ("admitting the third forces the first out"), so a leftover entry
    // from an earlier test would silently invalidate them (#2837 review, row 9).
    __resetBlobCacheForTests();
    mockDecryptFile.mockImplementation((data: ArrayBuffer) => Promise.resolve(data));
    mockDecryptAttachmentBlob.mockImplementation(
      async (bytes: Uint8Array, key: unknown, mime: string) => {
        const plain = (await mockDecryptFile(bytes.buffer ?? bytes, key)) as ArrayBuffer;
        const blob = new Blob([], { type: mime });
        Object.defineProperty(blob, 'size', { value: plain.byteLength });
        return blob;
      }
    );
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
  });

  afterEach(() => {
    // Restore original IntersectionObserver
    (globalThis as unknown as Record<string, unknown>).IntersectionObserver = OriginalIO;
    ioCallback = null;
  });

  it('renders nothing when no attachments', () => {
    const { container } = render(<AttachmentDisplay attachments={[]} channelId="ch-1" />);
    expect(container.firstChild).toBeNull();
  });

  // --- Image attachments ---
  it('renders image container for photo type', () => {
    const { container } = render(
      <AttachmentDisplay attachments={[mockAttachment]} channelId="ch-1" />
    );
    expect(container.querySelector('.attachment-image-container')).toBeInTheDocument();
  });

  it('decrypts image (always encrypted)', async () => {
    mockFetchSuccess();
    installImmediateIO();
    const img: AttachmentSummary = { ...mockAttachment, id: 'img-enc-1' };

    render(<AttachmentDisplay attachments={[img]} channelId="ch-1" />);

    await waitFor(() => {
      expect(mockDecryptFile).toHaveBeenCalled();
    });
    expect(mockGetChannelKey).toHaveBeenCalledWith('ch-1');
  });

  it('shows error when image fetch fails', async () => {
    mockFetchFailure();
    installImmediateIO();
    const img: AttachmentSummary = { ...mockAttachment, id: 'img-fail-1' };

    render(<AttachmentDisplay attachments={[img]} channelId="ch-1" />);

    await waitFor(() => {
      expect(screen.getByText('Failed to load image')).toBeInTheDocument();
    });
  });

  // --- Image lightbox + right-click save (#1729) ---
  async function renderLoadedImage(id: string) {
    mockFetchSuccess(new ArrayBuffer(64), 'image/png');
    installImmediateIO();
    const img: AttachmentSummary = { ...mockAttachment, id, mime_type: 'image/png' };
    const utils = render(<AttachmentDisplay attachments={[img]} channelId="ch-1" />);
    const el = await waitFor(() => {
      const node = utils.container.querySelector('img.attachment-image');
      expect(node).toBeInTheDocument();
      return node as HTMLImageElement;
    });
    return { ...utils, img: el, id };
  }

  it('opens the lightbox on image click and Escape closes it (#1729)', async () => {
    const { img } = await renderLoadedImage('img-click');
    expect(screen.queryByRole('dialog', { name: 'Image viewer' })).not.toBeInTheDocument();
    fireEvent.click(img);
    expect(screen.getByRole('dialog', { name: 'Image viewer' })).toBeInTheDocument();
    // Escape routes back through the wiring's onClose → unmounts the lightbox.
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Image viewer' })).not.toBeInTheDocument();
  });

  it('exposes the image open trigger as a real <button> (keyboard-accessible) (#1729 a11y)', async () => {
    await renderLoadedImage('img-key');
    // A native <button> wraps the image, so Enter/Space activation is built in
    // (no role/keydown shim on the non-interactive <img>).
    expect(screen.getByRole('button', { name: 'Open image in viewer' })).toBeInTheDocument();
  });

  it('right-click opens a menu with Open + Save image (#1729)', async () => {
    const { img } = await renderLoadedImage('img-ctx');
    fireEvent.contextMenu(img);
    expect(screen.getByText('Open')).toBeInTheDocument();
    expect(screen.getByText('Save image…')).toBeInTheDocument();
  });

  it('right-click Open launches the lightbox', async () => {
    const { img } = await renderLoadedImage('img-ctx-open');
    fireEvent.contextMenu(img);
    fireEvent.click(screen.getByText('Open'));
    expect(screen.getByRole('dialog', { name: 'Image viewer' })).toBeInTheDocument();
  });

  it('Save image hands the decrypted bytes to the native Save-As IPC', async () => {
    const saveImageAs = vi.fn().mockResolvedValue({ ok: true });
    // window.electron is defined non-configurable (writable:true) in setup.ts, so
    // assign directly rather than vi.stubGlobal (which redefines → throws here).
    const origElectron = window.electron;
    window.electron = { ...origElectron, saveImageAs } as typeof window.electron;
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ arrayBuffer: () => Promise.resolve(new ArrayBuffer(8)) })
    );
    try {
      const { img, id } = await renderLoadedImage('img-save');
      fireEvent.contextMenu(img);
      fireEvent.click(screen.getByText('Save image…'));
      await waitFor(() => {
        expect(saveImageAs).toHaveBeenCalledWith(
          expect.any(ArrayBuffer),
          expect.stringContaining(`image-${id}`)
        );
      });
    } finally {
      window.electron = origElectron;
      vi.unstubAllGlobals();
    }
  });

  it('swallows a Save failure when the blob fetch rejects (no crash)', async () => {
    const saveImageAs = vi.fn();
    const origElectron = window.electron;
    window.electron = { ...origElectron, saveImageAs } as typeof window.electron;
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('blob gone')));
    try {
      const { img } = await renderLoadedImage('img-save-fail');
      fireEvent.contextMenu(img);
      fireEvent.click(screen.getByText('Save image…'));
      // The rejection is caught in handleSaveImage; saveImageAs is never reached.
      await new Promise((r) => setTimeout(r, 0));
      expect(saveImageAs).not.toHaveBeenCalled();
    } finally {
      window.electron = origElectron;
      vi.unstubAllGlobals();
    }
  });

  // --- File attachments ---
  it('renders file attachment with download button', () => {
    render(<AttachmentDisplay attachments={[mockAttachment2]} channelId="ch-1" />);
    expect(screen.getByRole('button', { name: /download/i })).toBeInTheDocument();
  });

  it('renders file size and mime type', () => {
    render(<AttachmentDisplay attachments={[mockAttachment2]} channelId="ch-1" />);
    expect(screen.getByText('2.0 MB')).toBeInTheDocument();
    expect(screen.getByText('application/pdf')).toBeInTheDocument();
  });

  it('downloads file on click', async () => {
    mockFetchSuccess(new ArrayBuffer(50), 'application/pdf');
    const user = userEvent.setup();

    render(<AttachmentDisplay attachments={[mockAttachment2]} channelId="ch-1" />);

    await user.click(screen.getByRole('button', { name: /download/i }));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/media/attachments/attach-2');
    });
  });

  // --- Video attachments ---
  it('renders video load button', () => {
    const videoAttachment: AttachmentSummary = {
      id: 'vid-1',
      file_type: 'video',
      mime_type: 'video/mp4',
      file_size: 5000000,
    };
    render(<AttachmentDisplay attachments={[videoAttachment]} channelId="ch-1" />);
    expect(screen.getByText('Load video')).toBeInTheDocument();
  });

  it('loads video on button click', async () => {
    mockFetchSuccess(new ArrayBuffer(100), 'video/mp4');
    const user = userEvent.setup();
    const videoAttachment: AttachmentSummary = {
      id: 'vid-2',
      file_type: 'video',
      mime_type: 'video/mp4',
      file_size: 5000000,
    };
    const { container } = render(
      <AttachmentDisplay attachments={[videoAttachment]} channelId="ch-1" />
    );

    await user.click(screen.getByText('Load video'));

    await waitFor(() => {
      expect(container.querySelector('video')).toBeInTheDocument();
    });
  });

  it('renders loaded video without native controls (themed bar instead)', async () => {
    mockFetchSuccess(new ArrayBuffer(100), 'video/mp4');
    const user = userEvent.setup();
    const videoAttachment: AttachmentSummary = {
      id: 'vid-themed',
      file_type: 'video',
      mime_type: 'video/mp4',
      file_size: 5000000,
    };
    const { container } = render(
      <AttachmentDisplay attachments={[videoAttachment]} channelId="ch-1" />
    );

    await user.click(screen.getByText('Load video'));

    await waitFor(() => {
      const video = container.querySelector('video');
      expect(video).toBeInTheDocument();
      // Approach B: native controls are replaced by the themed React bar.
      expect(video?.hasAttribute('controls')).toBe(false);
    });
    expect(screen.getByRole('button', { name: 'Play' })).toBeInTheDocument();
  });

  it('auto-loads video when scrolled into view (IntersectionObserver)', async () => {
    mockFetchSuccess(new ArrayBuffer(100), 'video/mp4');
    installImmediateIO();
    const videoAttachment: AttachmentSummary = {
      id: 'vid-auto',
      file_type: 'video',
      mime_type: 'video/mp4',
      file_size: 5000000,
    };
    const { container } = render(
      <AttachmentDisplay attachments={[videoAttachment]} channelId="ch-1" />
    );
    await waitFor(() => {
      expect(container.querySelector('video')).toBeInTheDocument();
    });
    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/media/attachments/vid-auto');
  });

  it('renders rich placeholder with mime type and file size for video', () => {
    const videoAttachment: AttachmentSummary = {
      id: 'vid-meta',
      file_type: 'video',
      mime_type: 'video/mp4',
      file_size: 5_000_000,
    };
    render(<AttachmentDisplay attachments={[videoAttachment]} channelId="ch-1" />);
    // Mime + size shown in the rich placeholder meta line
    expect(screen.getByText(/video\/mp4/)).toBeInTheDocument();
    expect(screen.getByText(/4\.8 MB|5\.0 MB|4\.77 MB/)).toBeInTheDocument();
  });

  it('shows retry text when video load fails', async () => {
    mockFetchFailure();
    installImmediateIO();
    const videoAttachment: AttachmentSummary = {
      id: 'vid-fail',
      file_type: 'video',
      mime_type: 'video/mp4',
      file_size: 1000,
    };
    render(<AttachmentDisplay attachments={[videoAttachment]} channelId="ch-1" />);
    await waitFor(() => {
      expect(screen.getByText(/Failed to load video — retry/)).toBeInTheDocument();
    });
  });

  // --- Audio attachments ---
  it('renders audio load button', () => {
    const audioAttachment: AttachmentSummary = {
      id: 'aud-1',
      file_type: 'audio',
      mime_type: 'audio/mpeg',
      file_size: 3000000,
    };
    render(<AttachmentDisplay attachments={[audioAttachment]} channelId="ch-1" />);
    expect(screen.getByText('Load audio')).toBeInTheDocument();
  });

  it('loads audio on button click', async () => {
    mockFetchSuccess(new ArrayBuffer(100), 'audio/mpeg');
    const user = userEvent.setup();
    const audioAttachment: AttachmentSummary = {
      id: 'aud-2',
      file_type: 'audio',
      mime_type: 'audio/mpeg',
      file_size: 3000000,
    };
    const { container } = render(
      <AttachmentDisplay attachments={[audioAttachment]} channelId="ch-1" />
    );

    await user.click(screen.getByText('Load audio'));

    await waitFor(() => {
      expect(container.querySelector('audio')).toBeInTheDocument();
    });
  });

  // --- Multiple / animated ---
  it('renders multiple attachment types together', () => {
    const { container } = render(
      <AttachmentDisplay attachments={[mockAttachment, mockAttachment2]} channelId="ch-1" />
    );
    expect(container.querySelector('.attachment-image-container')).toBeInTheDocument();
    expect(container.querySelector('.attachment-file-card')).toBeInTheDocument();
  });

  it('renders animated type as image container', () => {
    const gifAttachment: AttachmentSummary = {
      id: 'gif-1',
      file_type: 'animated',
      mime_type: 'image/gif',
      file_size: 500000,
    };
    const { container } = render(
      <AttachmentDisplay attachments={[gifAttachment]} channelId="ch-1" />
    );
    expect(container.querySelector('.attachment-image-container')).toBeInTheDocument();
  });

  // --- FileIcon coverage ---
  it('renders correct icon for file type', () => {
    const unknownFile: AttachmentSummary = {
      id: 'unk-1',
      file_type: 'file',
      mime_type: 'application/octet-stream',
      file_size: 1000,
    };
    const { container } = render(
      <AttachmentDisplay attachments={[unknownFile]} channelId="ch-1" />
    );
    expect(container.querySelector('.attachment-file-card')).toBeInTheDocument();
  });

  // --- Layout-shift fixes (bug #1: vertical expand on send) ---

  it('reserves the clamped display box on the container when summary has dimensions', () => {
    // 1600×1200 → max-width 400 with 4:3 aspect → 400×300
    const sized: AttachmentSummary = {
      ...mockAttachment,
      id: 'sized-1',
      width: 1600,
      height: 1200,
    };
    const { container } = render(<AttachmentDisplay attachments={[sized]} channelId="ch-1" />);
    const box = container.querySelector('.attachment-image-container') as HTMLElement;
    expect(box.style.width).toBe('400px');
    expect(box.style.height).toBe('300px');
  });

  it('clamps tall images by max-height while preserving aspect ratio', () => {
    // 600×1200 → max-height 300 with 1:2 aspect → 150×300
    const tall: AttachmentSummary = {
      ...mockAttachment,
      id: 'tall-1',
      width: 600,
      height: 1200,
    };
    const { container } = render(<AttachmentDisplay attachments={[tall]} channelId="ch-1" />);
    const box = container.querySelector('.attachment-image-container') as HTMLElement;
    expect(box.style.width).toBe('150px');
    expect(box.style.height).toBe('300px');
  });

  it('does not upscale small images beyond their natural size', () => {
    const small: AttachmentSummary = {
      ...mockAttachment,
      id: 'small-1',
      width: 100,
      height: 80,
    };
    const { container } = render(<AttachmentDisplay attachments={[small]} channelId="ch-1" />);
    const box = container.querySelector('.attachment-image-container') as HTMLElement;
    expect(box.style.width).toBe('100px');
    expect(box.style.height).toBe('80px');
  });

  // --- Reduce Animations hover-to-play (#571 item #6B) ---

  it('animated attachment under Reduce Animations shows the hover-to-play hint', async () => {
    mockFetchSuccess(new ArrayBuffer(100), 'image/gif');
    installImmediateIO();
    useSettingsStore.setState((s) => ({
      appearance: { ...s.appearance, reduceAnimations: true },
    }));
    const gif: AttachmentSummary = {
      id: 'gif-reduced-1',
      file_type: 'animated',
      mime_type: 'image/gif',
      file_size: 1000,
    };
    const { container } = render(<AttachmentDisplay attachments={[gif]} channelId="ch-1" />);
    await waitFor(() => {
      expect(container.querySelector('.attachment-reduced-motion-hint')).toBeInTheDocument();
    });
    // The <img> is not rendered while the hint is shown
    expect(container.querySelector('img')).toBeNull();
  });

  it('animated attachment renders the <img> on hover and removes it on mouseleave', async () => {
    mockFetchSuccess(new ArrayBuffer(100), 'image/gif');
    installImmediateIO();
    useSettingsStore.setState((s) => ({
      appearance: { ...s.appearance, reduceAnimations: true },
    }));
    const gif: AttachmentSummary = {
      id: 'gif-reduced-hover',
      file_type: 'animated',
      mime_type: 'image/gif',
      file_size: 1000,
    };
    const { container } = render(<AttachmentDisplay attachments={[gif]} channelId="ch-1" />);
    await waitFor(() =>
      expect(container.querySelector('.attachment-reduced-motion-hint')).toBeInTheDocument()
    );
    const box = container.querySelector('.attachment-image-container') as HTMLElement;
    fireEvent.mouseEnter(box);
    await waitFor(() => expect(container.querySelector('img')).toBeInTheDocument());
    fireEvent.mouseLeave(box);
    await waitFor(() => expect(container.querySelector('img')).toBeNull());
  });

  it('animated attachment autoplays (no hint) when Reduce Animations is OFF', async () => {
    mockFetchSuccess(new ArrayBuffer(100), 'image/gif');
    installImmediateIO();
    useSettingsStore.setState((s) => ({
      appearance: { ...s.appearance, reduceAnimations: false },
    }));
    const gif: AttachmentSummary = {
      id: 'gif-auto',
      file_type: 'animated',
      mime_type: 'image/gif',
      file_size: 1000,
    };
    const { container } = render(<AttachmentDisplay attachments={[gif]} channelId="ch-1" />);
    await waitFor(() => expect(container.querySelector('img')).toBeInTheDocument());
    expect(container.querySelector('.attachment-reduced-motion-hint')).toBeNull();
  });

  it('static photo ignores Reduce Animations', async () => {
    mockFetchSuccess();
    installImmediateIO();
    useSettingsStore.setState((s) => ({
      appearance: { ...s.appearance, reduceAnimations: true },
    }));
    const photo: AttachmentSummary = { ...mockAttachment, id: 'photo-reduced-1' };
    const { container } = render(<AttachmentDisplay attachments={[photo]} channelId="ch-1" />);
    await waitFor(() => expect(container.querySelector('img')).toBeInTheDocument());
    expect(container.querySelector('.attachment-reduced-motion-hint')).toBeNull();
  });

  it('leaves the container unsized when summary has no dimensions (legacy rows)', () => {
    const noDims: AttachmentSummary = { ...mockAttachment, id: 'nodims-1' };
    const { container } = render(<AttachmentDisplay attachments={[noDims]} channelId="ch-1" />);
    const box = container.querySelector('.attachment-image-container') as HTMLElement;
    expect(box.style.width).toBe('');
    expect(box.style.height).toBe('');
  });

  it('leaves the container unsized when only width is provided (no height)', () => {
    const partialDims: AttachmentSummary = { ...mockAttachment, id: 'partial-w', width: 400 };
    const { container } = render(
      <AttachmentDisplay attachments={[partialDims]} channelId="ch-1" />
    );
    const box = container.querySelector('.attachment-image-container') as HTMLElement;
    expect(box.style.width).toBe('');
    expect(box.style.height).toBe('');
  });

  it('leaves the container unsized when only height is provided (no width)', () => {
    const partialDims: AttachmentSummary = { ...mockAttachment, id: 'partial-h', height: 300 };
    const { container } = render(
      <AttachmentDisplay attachments={[partialDims]} channelId="ch-1" />
    );
    const box = container.querySelector('.attachment-image-container') as HTMLElement;
    expect(box.style.width).toBe('');
    expect(box.style.height).toBe('');
  });

  it('animated attachment under Reduce Animations shows hover-to-play hint and no img initially', async () => {
    mockFetchSuccess(new ArrayBuffer(100), 'image/gif');
    installImmediateIO();
    useSettingsStore.setState((s) => ({
      appearance: { ...s.appearance, reduceAnimations: true },
    }));
    const gif: AttachmentSummary = {
      id: 'gif-hint-only',
      file_type: 'animated',
      mime_type: 'image/gif',
      file_size: 2000,
    };
    const { container } = render(<AttachmentDisplay attachments={[gif]} channelId="ch-1" />);
    await waitFor(() => {
      expect(container.querySelector('.attachment-reduced-motion-hint')).toBeInTheDocument();
    });
    expect(container.querySelector('img')).toBeNull();
  });

  it('width and height attributes are respected and clamped when provided', () => {
    // 800×600 fits within 400×300 at 0.5 ratio → 400×300
    const sized: AttachmentSummary = {
      ...mockAttachment,
      id: 'sized-wh',
      width: 800,
      height: 600,
    };
    const { container } = render(<AttachmentDisplay attachments={[sized]} channelId="ch-1" />);
    const box = container.querySelector('.attachment-image-container') as HTMLElement;
    expect(box.style.width).toBe('400px');
    expect(box.style.height).toBe('300px');
  });

  it('missing width/height leaves container without inline size constraints', () => {
    const noDims: AttachmentSummary = {
      id: 'no-wh-dims',
      file_type: 'photo',
      mime_type: 'image/jpeg',
      file_size: 50000,
    };
    const { container } = render(<AttachmentDisplay attachments={[noDims]} channelId="ch-1" />);
    const box = container.querySelector('.attachment-image-container') as HTMLElement;
    // No inline width/height — falls back to CSS max-width/max-height
    expect(box.style.width).toBe('');
    expect(box.style.height).toBe('');
  });
});

describe('AttachmentDisplay text/markdown dispatch', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  const mdAttachment: AttachmentSummary = {
    id: 'att-md-1',
    file_type: 'file',
    mime_type: 'text/markdown',
    file_size: 6000,
  };

  it('routes text/markdown attachments to OverflowMarkdownAttachment', () => {
    render(
      <AttachmentDisplay
        attachments={[mdAttachment]}
        channelId="ch-1"
        messageBody="preview text…"
      />
    );
    expect(screen.getByTestId('overflow-md')).toBeInTheDocument();
    expect(screen.getByTestId('overflow-md')).toHaveAttribute('data-attachment-id', 'att-md-1');
    expect(screen.getByTestId('overflow-md')).toHaveAttribute('data-channel-id', 'ch-1');
    expect(screen.getByText(/preview text/)).toBeInTheDocument();
  });

  it('still routes non-markdown files to the generic FileAttachment', () => {
    const pdfAttachment: AttachmentSummary = {
      id: 'att-pdf-1',
      file_type: 'file',
      mime_type: 'application/pdf',
      file_size: 100000,
    };
    render(<AttachmentDisplay attachments={[pdfAttachment]} channelId="ch-1" messageBody="" />);
    // Generic file chip rendered — overflow component NOT mounted
    expect(screen.queryByTestId('overflow-md')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /download/i })).toBeInTheDocument();
  });
});

describe('extFromMime (#1729 Save-As default name)', () => {
  it('maps known image MIME types to extensions', () => {
    expect(extFromMime('image/png')).toBe('.png');
    expect(extFromMime('image/jpeg')).toBe('.jpg');
    expect(extFromMime('image/gif')).toBe('.gif');
    expect(extFromMime('image/webp')).toBe('.webp');
    expect(extFromMime('image/avif')).toBe('.avif');
    expect(extFromMime('image/svg+xml')).toBe('.svg');
  });

  it('returns "" for unmapped or absent MIME types', () => {
    expect(extFromMime('application/pdf')).toBe('');
    expect(extFromMime('')).toBe('');
    expect(extFromMime(undefined)).toBe('');
  });
});

// ---------------------------------------------------------------------------
// #2157 — download guard (Task 5) + blob-cache byte budget (Task 6 / A1)
// ---------------------------------------------------------------------------

function photoOf(id: string, fileSize: number): AttachmentSummary {
  return { ...mockAttachment, id, mime_type: 'image/png', file_size: fileSize };
}

/** A real (small) ArrayBuffer whose `byteLength` reports `bytes`. The blob cache
 *  accounts for `byteLength`, so shadowing it exercises the budget without
 *  allocating hundreds of megabytes inside jsdom. */
function bufferOfDeclaredSize(bytes: number): ArrayBuffer {
  const buf = new ArrayBuffer(8);
  Object.defineProperty(buf, 'byteLength', { value: bytes });
  return buf;
}

describe('AttachmentDisplay typed format failures (#2157 PR 2)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockApiFetch.mockResolvedValue({
      ok: true,
      arrayBuffer: () => Promise.resolve(new ArrayBuffer(64)),
      headers: new Headers({ 'X-File-Mime-Type': 'image/png' }),
    });
    installImmediateIO();
  });

  const renderFailingWith = async (err: Error) => {
    mockDecryptAttachmentBlob.mockRejectedValue(err);
    const { container } = render(
      <AttachmentDisplay attachments={[photoOf('typed-1', 1024)]} channelId="ch-1" />
    );
    await waitFor(() => {
      expect(container.querySelector('.attachment-error')).toBeInTheDocument();
    });
    return container;
  };

  it('says the build cannot open the format, not that loading failed', async () => {
    const c = await renderFailingWith(
      new UnsupportedAttachmentFormatError('unsupported version 3')
    );
    const text = c.querySelector('.attachment-error')?.textContent ?? '';
    expect(text).toMatch(/newer version|cannot open|not supported/i);
    // Terminal: retrying re-fetches the same bytes and fails the same way, so
    // nothing may invite the reader to try again. The media surface expresses
    // retry through the load button's title, which is the affordance to check.
    const retryish = Array.from(c.querySelectorAll('button')).filter((b) =>
      /retry/i.test(b.getAttribute('title') ?? b.textContent ?? '')
    );
    expect(retryish).toHaveLength(0);
  });

  it('says integrity could not be verified, which is a DIFFERENT message', async () => {
    const unsupported = await renderFailingWith(
      new UnsupportedAttachmentFormatError('unsupported version 3')
    );
    const unsupportedText = unsupported.querySelector('.attachment-error')?.textContent ?? '';

    vi.clearAllMocks();
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockApiFetch.mockResolvedValue({
      ok: true,
      arrayBuffer: () => Promise.resolve(new ArrayBuffer(64)),
      headers: new Headers({ 'X-File-Mime-Type': 'image/png' }),
    });
    const integrity = await renderFailingWith(
      new AttachmentIntegrityError('attachment chunk 0 of 1 failed authentication')
    );
    const integrityText = integrity.querySelector('.attachment-error')?.textContent ?? '';

    expect(integrityText).toMatch(/verif|altered|damaged/i);
    // The distinction is the point: "this build is too old" and "these bytes
    // are not what was sent" call for different responses from the reader.
    expect(integrityText).not.toBe(unsupportedText);
    const integrityRetry = Array.from(integrity.querySelectorAll('button')).filter((b) =>
      /retry/i.test(b.getAttribute('title') ?? b.textContent ?? '')
    );
    expect(integrityRetry).toHaveLength(0);
  });
});

describe('AttachmentDisplay download size guard (#2157)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockDecryptFile.mockImplementation((data: ArrayBuffer) => Promise.resolve(data));
    mockDecryptAttachmentBlob.mockImplementation(
      async (bytes: Uint8Array, key: unknown, mime: string) => {
        const plain = (await mockDecryptFile(bytes.buffer ?? bytes, key)) as ArrayBuffer;
        const blob = new Blob([], { type: mime });
        Object.defineProperty(blob, 'size', { value: plain.byteLength });
        return blob;
      }
    );
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
  });

  afterEach(() => {
    (globalThis as unknown as Record<string, unknown>).IntersectionObserver = OriginalIO;
    ioCallback = null;
  });

  // R2: the guard is sized to the PREMIUM entitlement, not to the interim upload
  // ceiling — PR 2 will produce files above that ceiling and this build must
  // still open them.
  it('still opens a 200 MiB attachment', async () => {
    mockFetchSuccess();
    installImmediateIO();

    const { container } = render(
      <AttachmentDisplay attachments={[photoOf('guard-200mib', 209_715_200)]} channelId="ch-1" />
    );

    await waitFor(() =>
      expect(container.querySelector('img.attachment-image')).toBeInTheDocument()
    );
    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/media/attachments/guard-200mib');
  });

  it('rejects on file_size without issuing a network request', async () => {
    mockFetchSuccess();
    installImmediateIO();

    render(
      <AttachmentDisplay
        attachments={[photoOf('guard-oversize', MAX_DECRYPTABLE_ATTACHMENT_BYTES + 1)]}
        channelId="ch-1"
      />
    );

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'This file is 256.0 MB and is too large to open in this version of Concord.'
    );
    // The whole point of gating on the summary: no bytes are ever requested.
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('treats a Content-Length above the guard as a rejection and never reads the body', async () => {
    const arrayBuffer = vi.fn(() => Promise.resolve(new ArrayBuffer(8)));
    mockApiFetch.mockResolvedValue({
      ok: true,
      arrayBuffer,
      headers: new Headers({
        'X-File-Mime-Type': 'image/png',
        'Content-Length': String(MAX_DECRYPTABLE_ATTACHMENT_BYTES + 1),
      }),
    });
    installImmediateIO();

    render(
      <AttachmentDisplay attachments={[photoOf('guard-content-length', 1024)]} channelId="ch-1" />
    );

    expect(await screen.findByRole('alert')).toHaveTextContent(
      /too large to open in this version of Concord/
    );
    expect(arrayBuffer).not.toHaveBeenCalled();
    expect(mockDecryptFile).not.toHaveBeenCalled();
  });

  it('refuses an oversized file-card download in the chip footprint', async () => {
    const user = userEvent.setup();

    render(
      <AttachmentDisplay
        attachments={[
          {
            ...mockAttachment2,
            id: 'guard-file-card',
            file_size: MAX_DECRYPTABLE_ATTACHMENT_BYTES + 1,
          },
        ]}
        channelId="ch-1"
      />
    );

    await user.click(screen.getByRole('button', { name: /download/i }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      /too large to open in this version of Concord/
    );
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('refuses an oversized video and replaces the load button with the notice', async () => {
    const user = userEvent.setup();
    const video: AttachmentSummary = {
      id: 'guard-video',
      file_type: 'video',
      mime_type: 'video/mp4',
      file_size: MAX_DECRYPTABLE_ATTACHMENT_BYTES + 1,
    };

    render(<AttachmentDisplay attachments={[video]} channelId="ch-1" />);

    await user.click(screen.getByText('Load video'));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      /too large to open in this version of Concord/
    );
    expect(screen.queryByText('Load video')).not.toBeInTheDocument();
    expect(mockApiFetch).not.toHaveBeenCalled();
  });
});

describe('AttachmentDisplay blob cache byte budget (#2157 A1)', () => {
  const originalCreateObjectURL = URL.createObjectURL;
  const originalRevokeObjectURL = URL.revokeObjectURL;
  let urlSeq = 0;
  let revoked: string[] = [];

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    revoked = [];
    // setup.ts stubs createObjectURL with a CONSTANT url, which cannot express
    // "which entry was evicted" — hand out a distinct url per blob instead.
    URL.createObjectURL = () => `blob:cache-${++urlSeq}`;
    URL.revokeObjectURL = (url: string) => {
      revoked.push(url);
    };
  });

  afterEach(() => {
    URL.createObjectURL = originalCreateObjectURL;
    URL.revokeObjectURL = originalRevokeObjectURL;
    (globalThis as unknown as Record<string, unknown>).IntersectionObserver = OriginalIO;
    ioCallback = null;
  });

  /** Loads one photo per render, in declaration order, and returns the blob url
   *  each one was cached under (read off the rendered <img>, so the assertion
   *  gates on the DOM rather than on the mock). */
  async function loadPhotos(sizes: readonly number[], idPrefix: string): Promise<string[]> {
    const urls: string[] = [];
    for (let i = 0; i < sizes.length; i++) {
      const bytes = sizes[i];
      mockApiFetch.mockResolvedValue({
        ok: true,
        arrayBuffer: () => Promise.resolve(new ArrayBuffer(8)),
        headers: new Headers({ 'X-File-Mime-Type': 'image/png' }),
      });
      mockDecryptFile.mockResolvedValue(bufferOfDeclaredSize(bytes));
      installImmediateIO();

      const { container, unmount } = render(
        <AttachmentDisplay attachments={[photoOf(`${idPrefix}-${i}`, bytes)]} channelId="ch-1" />
      );
      const img = await waitFor(() => {
        const node = container.querySelector('img.attachment-image');
        expect(node).toBeInTheDocument();
        return node as HTMLImageElement;
      });
      urls.push(img.getAttribute('src') ?? '');
      unmount();
    }
    return urls;
  }

  it('does not let one huge attachment evict the whole cache', async () => {
    // A 256 MiB entry equals the entire budget, so admitting it drained
    // everything else and then occupied all of it. Blobs above the retain
    // threshold are no longer cached at all.
    await loadPhotos([1024, 1024], 'huge-pre');
    // Measure the DELTA rather than the absolute set: blobUrlCache and
    // cachedBytes are module-level and persist across every test in this file,
    // so an absolute assertion would be reading other tests' leftovers.
    const before = revoked.length;

    const [huge] = await loadPhotos([BLOB_CACHE_RETAIN_MAX_BYTES + 1], 'huge-main');

    // Admitting it must cost NO collateral eviction — that is the whole point.
    expect(revoked.filter((u) => u !== huge)).toHaveLength(before);
    // And it revokes itself on unmount rather than being retained.
    expect(revoked).toContain(huge);
  });

  it('revokes an uncached huge url when its last surface unmounts', async () => {
    // The cache is the only revoker, so declining to cache something would leak
    // it forever unless the retention hook takes over. This is that path.
    mockApiFetch.mockResolvedValue({
      ok: true,
      arrayBuffer: () => Promise.resolve(new ArrayBuffer(8)),
      headers: new Headers({ 'X-File-Mime-Type': 'image/png' }),
    });
    mockDecryptFile.mockResolvedValue(bufferOfDeclaredSize(BLOB_CACHE_RETAIN_MAX_BYTES + 1));
    installImmediateIO();

    const { container, unmount } = render(
      <AttachmentDisplay
        attachments={[photoOf('leak-1', BLOB_CACHE_RETAIN_MAX_BYTES + 1)]}
        channelId="ch-1"
      />
    );
    const img = await waitFor(() => {
      const node = container.querySelector('img.attachment-image');
      expect(node).toBeInTheDocument();
      return node as HTMLImageElement;
    });
    const url = img.getAttribute('src') ?? '';
    expect(revoked).not.toContain(url); // still on screen

    unmount();
    expect(revoked).toContain(url);
  });

  it('evicts oldest-first once the byte budget is exceeded and revokes the evicted url', async () => {
    // Three 100 MiB entries against a 256 MiB budget: admitting the third
    // forces the first out, and only the first.
    const [first, second, third] = await loadPhotos(
      [104_857_600, 104_857_600, 104_857_600],
      'budget'
    );

    expect(revoked).toContain(first);
    expect(revoked).not.toContain(second);
    expect(revoked).not.toContain(third);
  });

  it('does not wedge on a single entry larger than the whole budget', async () => {
    // The giant is admitted (emptying the map on the way in) and is then itself
    // evicted by the next admission — the loop must terminate, not spin.
    const [giant, small] = await loadPhotos([MAX_DECRYPTABLE_ATTACHMENT_BYTES, 1024], 'wedge');

    expect(revoked).toContain(giant);
    expect(revoked).not.toContain(small);
  });

  it('keeps the byte accounting straight when two surfaces race on one attachment', async () => {
    // Both surfaces mount before either fetch settles. Coalescing is what keeps
    // the accounting straight now: they share ONE in-flight load and one insert,
    // so `cacheBlobUrl`'s supersede branch is never reached. This guards the
    // invariant (100 MiB counted once), not that branch — which has no coverage
    // here, and should not be assumed to (#2837 review, row 10).
    mockApiFetch.mockResolvedValue({
      ok: true,
      arrayBuffer: () => Promise.resolve(new ArrayBuffer(8)),
      headers: new Headers({ 'X-File-Mime-Type': 'image/png' }),
    });
    mockDecryptFile.mockResolvedValue(bufferOfDeclaredSize(104_857_600));
    installImmediateIO();

    const first = render(
      <AttachmentDisplay attachments={[photoOf('race', 104_857_600)]} channelId="ch-1" />
    );
    const second = render(
      <AttachmentDisplay attachments={[photoOf('race', 104_857_600)]} channelId="ch-1" />
    );

    const surviving = await waitFor(() => {
      expect(first.container.querySelector('img.attachment-image')).toBeInTheDocument();
      const node = second.container.querySelector('img.attachment-image');
      expect(node).toBeInTheDocument();
      return (node as HTMLImageElement).getAttribute('src') ?? '';
    });

    // 100 MiB counted once leaves room for a second 100 MiB admission under the
    // 256 MiB budget. Counted twice it does not, and the surviving entry is
    // evicted to make way — which is how a phantom becomes visible.
    await loadPhotos([104_857_600], 'race-next');
    expect(revoked).not.toContain(surviving);

    first.unmount();
    second.unmount();
  });

  // VULN-002 (#2157 adversarial review). Two surfaces racing used to mint two
  // blob urls, and the superseded one left the Map without being revoked — so
  // evictOldestBlob could never reach it and the Blob was pinned for the life
  // of the document, while `cachedBytes` reported memory nobody controlled.
  // Coalescing removes the duplicate rather than cleaning up after it.
  it('coalesces concurrent loads of one attachment onto a single fetch and url', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      arrayBuffer: () => Promise.resolve(new ArrayBuffer(8)),
      headers: new Headers({ 'X-File-Mime-Type': 'image/png' }),
    });
    mockDecryptFile.mockResolvedValue(bufferOfDeclaredSize(104_857_600));
    installImmediateIO();

    const surfaces = [0, 1, 2, 3].map(() =>
      render(
        <AttachmentDisplay attachments={[photoOf('coalesce', 104_857_600)]} channelId="ch-1" />
      )
    );

    const srcs = await waitFor(() =>
      surfaces.map((s) => {
        const node = s.container.querySelector('img.attachment-image');
        expect(node).toBeInTheDocument();
        return (node as HTMLImageElement).getAttribute('src') ?? '';
      })
    );

    // One url handed to all four, so there is no superseded entry to orphan.
    expect(new Set(srcs).size).toBe(1);
    // One network fetch and one decrypt, not four.
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
    expect(mockDecryptFile).toHaveBeenCalledTimes(1);
    expect(revoked).not.toContain(srcs[0]);

    for (const s of surfaces) s.unmount();
  });

  // VULN-002-COLLATERAL (#2157 adversarial review): the byte budget used to
  // revoke a url a mounted <img> was still pointing at — a silently broken
  // image, and handleSaveImage's fetch of it failing into a swallowed catch.
  // The budget bounds cache RETENTION, never what the user is looking at.
  it('never revokes a url while a surface is still rendering it', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      arrayBuffer: () => Promise.resolve(new ArrayBuffer(8)),
      headers: new Headers({ 'X-File-Mime-Type': 'image/png' }),
    });
    mockDecryptFile.mockResolvedValue(bufferOfDeclaredSize(104_857_600));
    installImmediateIO();

    // Mount each and let it settle, which is the real shape of the hazard: two
    // 100 MiB images already on screen when a third arrives. 300 MiB against a
    // 256 MiB budget forces an eviction, and every candidate is mounted.
    const surfaces = [];
    const srcs: string[] = [];
    for (const id of ['live-a', 'live-b', 'live-c']) {
      const view = render(
        <AttachmentDisplay attachments={[photoOf(id, 104_857_600)]} channelId="ch-1" />
      );
      surfaces.push(view);
      srcs.push(
        await waitFor(() => {
          const node = view.container.querySelector('img.attachment-image');
          expect(node).toBeInTheDocument();
          return (node as HTMLImageElement).getAttribute('src') ?? '';
        })
      );
    }

    // Exceeding the budget is the correct outcome here; breaking a live image
    // is not. Nothing on screen may be revoked.
    for (const src of srcs) expect(revoked).not.toContain(src);

    // Once they leave the screen the bytes become reclaimable again — the
    // budget is enforced, just never at the cost of what the user is viewing.
    for (const view of surfaces) view.unmount();
    await loadPhotos([104_857_600], 'after-release');
    expect(revoked).toContain(srcs[0]);
  });

  // Review row 1: `a.click()` starts an ASYNCHRONOUS browser download that
  // reads the blob over time. An earlier version reasoned only about the
  // synchronous window before the click and left the file card unretained, so a
  // later load could evict and revoke the url mid-read.
  it("keeps a downloading file card's url alive while other attachments load", async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      arrayBuffer: () => Promise.resolve(new ArrayBuffer(8)),
      headers: new Headers({ 'X-File-Mime-Type': 'application/pdf' }),
    });
    mockDecryptFile.mockResolvedValue(bufferOfDeclaredSize(104_857_600));

    // Capture the href the download anchor actually receives.
    let downloadedUrl = '';
    const realCreate = document.createElement.bind(document);
    const createSpy = vi.spyOn(document, 'createElement').mockImplementation((tag: string) => {
      const el = realCreate(tag) as HTMLElement;
      if (tag === 'a') {
        Object.defineProperty(el, 'href', {
          set(v: string) {
            downloadedUrl = v;
          },
          get: () => downloadedUrl,
          configurable: true,
        });
        (el as HTMLAnchorElement).click = () => undefined;
      }
      return el;
    });

    const card = render(
      <AttachmentDisplay
        attachments={[
          {
            ...mockAttachment,
            id: 'dl',
            file_type: 'file',
            mime_type: 'application/pdf',
            file_size: 104_857_600,
          },
        ]}
        channelId="ch-1"
      />
    );
    fireEvent.click(screen.getByRole('button', { name: 'Download attachment' }));
    await waitFor(() => expect(downloadedUrl).toMatch(/^blob:cache-/));
    createSpy.mockRestore();

    // Two more 100 MiB attachments: 300 MiB against a 256 MiB budget forces
    // eviction while the card is mounted and its download still reading.
    await loadPhotos([104_857_600, 104_857_600], 'dl-pressure');

    expect(revoked).not.toContain(downloadedUrl);

    // Once the card unmounts the bytes become reclaimable again.
    card.unmount();
    await loadPhotos([104_857_600], 'dl-after');
    expect(revoked).toContain(downloadedUrl);
  });

  // Gitar (#2837): `truncated` was plumbed through the error and the notice but
  // never captured at the catch sites, so a mid-stream-cancelled refusal still
  // stated its partial byte count as the file's real size.
  //
  // The component's contract is PROPAGATING the flag, not producing it — a
  // genuine cancellation needs a >256 MiB body, which is not something to
  // stream inside jsdom. readBoundedBody's own truncated behaviour is covered
  // in tests/unit/utils/boundedResponseBody.test.ts; here the error is thrown
  // directly so the copy branch is what is under test.
  it('says "over N" when the refusal was truncated mid-stream', async () => {
    mockApiFetch.mockImplementation(() => {
      const err = new AttachmentTooLargeError(268_435_456, true);
      return Promise.reject(err);
    });
    installImmediateIO();

    render(<AttachmentDisplay attachments={[photoOf('truncated', 1024)]} channelId="ch-1" />);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toMatch(/^This file is over 256\.0 MB/);
  });

  it('states the size outright when the refusal was an honest declared size', async () => {
    mockApiFetch.mockClear();
    installImmediateIO();
    render(
      <AttachmentDisplay
        attachments={[photoOf('honest', MAX_DECRYPTABLE_ATTACHMENT_BYTES + 1)]}
        channelId="ch-1"
      />
    );
    const alert = await screen.findByRole('alert');
    // Refused on file_size before any fetch, so the count is the real size.
    expect(alert.textContent).toMatch(/^This file is 256\.0 MB/);
    expect(alert.textContent).not.toMatch(/is over /);
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('still caps entry count for a channel full of thumbnails', async () => {
    // 60 x 1 KiB is ~60 KiB total, so the byte budget cannot be the cause of an
    // eviction here — only the secondary entry cap can.
    const urls = await loadPhotos(new Array(60).fill(1024), 'thumb');

    expect(revoked).toContain(urls[0]);
    expect(revoked).not.toContain(urls[59]);
  });
});

describe('AttachmentDisplay decrypts under the epoch the file was sealed with', () => {
  // Every mandatory revocation rotates the CSK. Decrypting with the LATEST key
  // permanently orphaned every attachment sealed before the most recent
  // rotation -- the key still existed and was fetchable, the client just never
  // asked for it. Worse, the failure surfaced as "may be damaged or altered":
  // a routine rotation reported to the user as tampering.
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockGetChannelKeyByVersion.mockResolvedValue({} as CryptoKey);
    mockDecryptAttachmentBlob.mockResolvedValue(new Blob([new Uint8Array(8)]));
    installImmediateIO();
  });

  /** Renders one photo whose download carries `keyVersionHeader`, and waits
   *  until the component has resolved SOME channel key -- the assertion under
   *  test is always which of the two it asked for.
   *
   *  The id must be unique per call: decrypted blobs are cached module-wide by
   *  attachment id, so a repeated id is served from cache and never downloads
   *  (which reads as "the key was never fetched" rather than as a cache hit). */
  let epochCaseId = 0;
  const renderWithEpochHeader = async (
    keyVersionHeader: string | null,
    opts: { expectError?: boolean } = {}
  ) => {
    epochCaseId += 1;
    const headers = new Headers({ 'X-File-Mime-Type': 'image/png' });
    if (keyVersionHeader !== null) headers.set('X-File-Key-Version', keyVersionHeader);
    mockApiFetch.mockResolvedValue({
      ok: true,
      arrayBuffer: () => Promise.resolve(new ArrayBuffer(64)),
      headers,
    });
    const { container } = render(
      <AttachmentDisplay attachments={[photoOf(`epoch-${epochCaseId}`, 1024)]} channelId="ch-1" />
    );
    if (opts.expectError) {
      await waitFor(() => {
        expect(container.querySelector('.attachment-error')).toBeInTheDocument();
      });
    } else {
      await waitFor(() => {
        expect(
          mockGetChannelKey.mock.calls.length + mockGetChannelKeyByVersion.mock.calls.length
        ).toBeGreaterThan(0);
      });
    }
    return { container };
  };

  it('asks for the epoch named by X-File-Key-Version', async () => {
    await renderWithEpochHeader('7');

    expect(mockGetChannelKeyByVersion).toHaveBeenCalledWith('ch-1', 7);
    expect(mockGetChannelKey).not.toHaveBeenCalled();
  });

  it('falls back to the current key when no epoch is on record', async () => {
    // Rows predating the client-attested epoch carry a NULL key_version, and
    // for those the current key IS the right one.
    await renderWithEpochHeader(null);

    expect(mockGetChannelKey).toHaveBeenCalledWith('ch-1');
    expect(mockGetChannelKeyByVersion).not.toHaveBeenCalled();
  });

  // No leading-space case here: Headers.set trims per spec, so " 7" arrives as
  // "7". attachmentChunkedCrypto.test.ts covers it against the parser directly.
  it.each(['0', '-1', 'abc', '1.5', '', '1e3'])(
    'refuses to guess a key for the mangled epoch %j',
    async (raw) => {
      // NOT a fallback to the current key. Falling back would decrypt with the
      // WRONG CSK, fail GCM, and be reported as tampering -- the exact
      // misdiagnosis this change exists to remove. A mangled header is a
      // transport fault, so it says so and fetches no key at all.
      await renderWithEpochHeader(raw, { expectError: true });

      expect(mockGetChannelKey).not.toHaveBeenCalled();
      expect(mockGetChannelKeyByVersion).not.toHaveBeenCalled();
    }
  );

  it('calls a mangled epoch a THIS-DEVICE fault, not a damaged file', async () => {
    const { container } = await renderWithEpochHeader('abc', { expectError: true });
    const text = container.querySelector('.attachment-error')?.textContent ?? '';

    expect(text).not.toMatch(/damaged|altered|verified/i);
    expect(text).toMatch(/could not decrypt|this device/i);
    // Terminal: retrying re-fetches the same mangled header.
    const retryish = Array.from(container.querySelectorAll('button')).filter((b) =>
      /retry/i.test(b.getAttribute('title') ?? b.textContent ?? '')
    );
    expect(retryish).toHaveLength(0);
  });
});
