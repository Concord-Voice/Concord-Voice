import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '../../../test-utils';
import userEvent from '@testing-library/user-event';
import AttachmentDisplay from '@/renderer/components/Chat/AttachmentDisplay';
import type { AttachmentSummary } from '@/renderer/types/chat';
import { mockAttachment, mockAttachment2 } from '../../../mocks/fixtures';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { fireEvent } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';

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
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return true;
    },
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
  },
}));

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
    mockDecryptFile.mockImplementation((data: ArrayBuffer) => Promise.resolve(data));
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
