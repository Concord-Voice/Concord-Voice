import { render, screen, userEvent } from '../../../test-utils';
import ImageCropEditor from '@/renderer/components/ui/ImageCropEditor';
import { vi, type Mock } from 'vitest';

// Mock apiFetch
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

// Canvas mocks are provided globally by tests/setup.ts.
// This file relies on those mocks — no local overrides needed.

describe('ImageCropEditor', () => {
  const onClose = vi.fn();
  const onConfirm = vi.fn();
  const file = new File(['pixels'], 'photo.png', { type: 'image/png' });

  const defaultProps = {
    isOpen: true,
    onClose,
    onConfirm,
    imageFile: file,
    title: 'Crop Avatar',
    cropShape: { type: 'circle' as const },
    output: { width: 512, height: 512, quality: 0.9 },
  };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders modal with correct title when open', () => {
    render(<ImageCropEditor {...defaultProps} />);
    expect(screen.getByText('Crop Avatar')).toBeInTheDocument();
  });

  it('does not render when isOpen is false', () => {
    render(<ImageCropEditor {...defaultProps} isOpen={false} />);
    expect(screen.queryByText('Crop Avatar')).not.toBeInTheDocument();
  });

  it('shows loading state initially', () => {
    render(<ImageCropEditor {...defaultProps} />);
    expect(screen.getByText('Loading image...')).toBeInTheDocument();
  });

  it('calls onClose when Cancel is clicked', async () => {
    // Simulate image loaded by triggering the Image onload
    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 1024;
      naturalHeight = 1024;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    const user = userEvent.setup();
    render(<ImageCropEditor {...defaultProps} />);

    // Wait for image to "load"
    await screen.findByText('Cancel');
    await user.click(screen.getByText('Cancel'));
    expect(onClose).toHaveBeenCalled();

    global.Image = originalImage;
  });

  it('calls onClose on Escape key', async () => {
    const user = userEvent.setup();
    render(<ImageCropEditor {...defaultProps} />);
    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalled();
  });

  it('renders zoom slider after image loads', async () => {
    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 1024;
      naturalHeight = 1024;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    render(<ImageCropEditor {...defaultProps} />);
    const slider = await screen.findByLabelText('Zoom level');
    expect(slider).toBeInTheDocument();
    expect(slider).toHaveAttribute('type', 'range');

    global.Image = originalImage;
  });

  it('shows drag hint text', async () => {
    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 1024;
      naturalHeight = 1024;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    render(<ImageCropEditor {...defaultProps} />);
    expect(await screen.findByText('Drag to reposition, scroll to zoom')).toBeInTheDocument();

    global.Image = originalImage;
  });

  it('shows small image warning when source is smaller than output', async () => {
    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 100;
      naturalHeight = 100;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    render(<ImageCropEditor {...defaultProps} />);
    expect(await screen.findByText(/Quality may/)).toBeInTheDocument();

    global.Image = originalImage;
  });

  it('calls onConfirm with data URL when no upload config', async () => {
    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 1024;
      naturalHeight = 1024;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    // Mock FileReader for the blobToDataUrl fallback
    const originalFileReader = global.FileReader;
    global.FileReader = class MockFileReader {
      result: string | ArrayBuffer | null = 'data:image/jpeg;base64,cropped';
      onload: ((ev: ProgressEvent<FileReader>) => void) | null = null;
      onerror: ((ev: ProgressEvent<FileReader>) => void) | null = null;
      readAsDataURL() {
        setTimeout(() => this.onload?.({} as ProgressEvent<FileReader>), 0);
      }
    } as unknown as typeof FileReader;

    const user = userEvent.setup();
    render(<ImageCropEditor {...defaultProps} />);

    await screen.findByText('Apply');
    await user.click(screen.getByText('Apply'));

    // Wait for async confirm handler
    await vi.waitFor(() => {
      expect(onConfirm).toHaveBeenCalledWith('data:image/jpeg;base64,cropped');
    });

    global.Image = originalImage;
    global.FileReader = originalFileReader;
  });

  it('uploads to object storage when upload config provided', async () => {
    const { apiFetch } = await import('@/renderer/services/apiClient');
    (apiFetch as Mock).mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ url: '/api/v1/media/avatars/123' }),
    });

    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 1024;
      naturalHeight = 1024;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    const user = userEvent.setup();
    render(
      <ImageCropEditor {...defaultProps} upload={{ endpoint: '/api/v1/media/upload/avatar' }} />
    );

    await screen.findByText('Apply');
    await user.click(screen.getByText('Apply'));

    await vi.waitFor(() => {
      expect(apiFetch).toHaveBeenCalledWith(
        '/api/v1/media/upload/avatar',
        expect.objectContaining({
          method: 'POST',
        })
      );
      expect(onConfirm).toHaveBeenCalledWith('/api/v1/media/avatars/123');
    });

    global.Image = originalImage;
  });

  // ── ARIA / a11y attributes ───────────────────────────────────────────

  it('renders canvas element inside crop button', async () => {
    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 1024;
      naturalHeight = 1024;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    const { container } = render(<ImageCropEditor {...defaultProps} />);
    await screen.findByText('Cancel');
    const canvas = container.querySelector('canvas');
    expect(canvas).toBeInTheDocument();
    // Canvas is a child of the interactive crop button — no aria-hidden needed
    expect(canvas?.closest('button')).toBeInTheDocument();

    global.Image = originalImage;
  });

  it('renders crop area as interactive button with aria-label', async () => {
    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 1024;
      naturalHeight = 1024;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    render(<ImageCropEditor {...defaultProps} />);
    await screen.findByText('Cancel');
    const cropArea = screen.getByRole('button', {
      name: 'Image crop editor — drag to reposition, scroll to zoom',
    });
    expect(cropArea).toBeInTheDocument();
    expect(cropArea).toHaveClass('image-crop-preview');

    global.Image = originalImage;
  });

  it('renders zoom slider with aria-label "Zoom level"', async () => {
    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 1024;
      naturalHeight = 1024;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    render(<ImageCropEditor {...defaultProps} />);
    const slider = await screen.findByLabelText('Zoom level');
    expect(slider).toBeInTheDocument();
    expect(slider).toHaveAttribute('aria-label', 'Zoom level');

    global.Image = originalImage;
  });

  it('shows upload error on failure', async () => {
    const { apiFetch } = await import('@/renderer/services/apiClient');
    (apiFetch as Mock).mockResolvedValue({
      ok: false,
      json: () => Promise.resolve({ error: 'File too large' }),
    });

    const originalImage = global.Image;
    global.Image = class MockImage {
      naturalWidth = 1024;
      naturalHeight = 1024;
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      set src(_: string) {
        setTimeout(() => this.onload?.(), 0);
      }
    } as unknown as typeof Image;

    const user = userEvent.setup();
    render(
      <ImageCropEditor {...defaultProps} upload={{ endpoint: '/api/v1/media/upload/avatar' }} />
    );

    await screen.findByText('Apply');
    await user.click(screen.getByText('Apply'));

    expect(await screen.findByText('File too large')).toBeInTheDocument();

    global.Image = originalImage;
  });
});
