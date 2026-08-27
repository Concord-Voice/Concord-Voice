import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import AttachmentUploadPreview from '@/renderer/components/Chat/AttachmentUploadPreview';
import type { FileUploadState } from '@/renderer/hooks/useFileUpload';

function createFileEntry(
  name: string,
  size: number,
  type: string,
  status: FileUploadState['status'] = 'pending',
  previewUrl?: string
): FileUploadState {
  const file = new File([new ArrayBuffer(size)], name, { type });
  return { file, uploadId: `up-${name}`, progress: 0, status, previewUrl };
}

describe('AttachmentUploadPreview', () => {
  it('renders nothing when no files', () => {
    const { container } = render(<AttachmentUploadPreview files={[]} onRemove={vi.fn()} />);
    expect(container.querySelector('.attachment-upload-preview')).toBeNull();
  });

  it('renders file entries with names', () => {
    const files = [
      createFileEntry('photo.png', 5000, 'image/png'),
      createFileEntry('doc.pdf', 10000, 'application/pdf'),
    ];

    render(<AttachmentUploadPreview files={files} onRemove={vi.fn()} />);

    expect(screen.getByText('photo.png')).toBeInTheDocument();
    expect(screen.getByText('doc.pdf')).toBeInTheDocument();
  });

  it('displays file sizes', () => {
    const files = [createFileEntry('test.txt', 2048, 'text/plain')];

    render(<AttachmentUploadPreview files={files} onRemove={vi.fn()} />);

    expect(screen.getByText('2.0 KB')).toBeInTheDocument();
  });

  it('shows image thumbnails when previewUrl is provided', () => {
    const files = [
      createFileEntry('photo.png', 1000, 'image/png', 'pending', 'blob:http://localhost/abc'),
    ];

    render(<AttachmentUploadPreview files={files} onRemove={vi.fn()} />);

    const img = screen.getByAltText('photo.png');
    expect(img).toBeInTheDocument();
    expect(img).toHaveAttribute('src', 'blob:http://localhost/abc');
  });

  it('shows remove buttons for each file', () => {
    const files = [
      createFileEntry('a.png', 100, 'image/png'),
      createFileEntry('b.pdf', 200, 'application/pdf'),
    ];

    render(<AttachmentUploadPreview files={files} onRemove={vi.fn()} />);

    const removeButtons = screen.getAllByRole('button', { name: /Remove/ });
    expect(removeButtons).toHaveLength(2);
  });

  it('calls onRemove with correct index', async () => {
    const onRemove = vi.fn();
    const files = [
      createFileEntry('a.png', 100, 'image/png'),
      createFileEntry('b.pdf', 200, 'application/pdf'),
    ];

    render(<AttachmentUploadPreview files={files} onRemove={onRemove} />);

    const removeButtons = screen.getAllByRole('button', { name: /Remove/ });
    removeButtons[1].click();

    expect(onRemove).toHaveBeenCalledWith(1);
  });

  it('shows progress bar when uploading', () => {
    const files = [
      {
        ...createFileEntry('test.png', 100, 'image/png', 'uploading'),
        progress: 50,
      },
    ];

    render(<AttachmentUploadPreview files={files} onRemove={vi.fn()} />);

    // Native <progress> carries the value itself; there is no fill div to
    // inspect, and the percentage lives on the element rather than in a style.
    const bar = screen.getByRole('progressbar');
    expect(bar.tagName).toBe('PROGRESS');
    expect(bar).toHaveValue(50);
  });

  it('shows error label when upload fails', () => {
    const files = [
      {
        ...createFileEntry('test.png', 100, 'image/png', 'error'),
        error: 'Upload failed',
      },
    ];

    render(<AttachmentUploadPreview files={files} onRemove={vi.fn()} />);

    expect(screen.getByText('Upload failed')).toBeInTheDocument();
  });

  it('applies error class to failed items', () => {
    const files = [
      {
        ...createFileEntry('test.png', 100, 'image/png', 'error'),
        error: 'Failed',
      },
    ];

    const { container } = render(<AttachmentUploadPreview files={files} onRemove={vi.fn()} />);

    const item = container.querySelector('.attachment-preview-item');
    expect(item).toHaveClass('error');
  });
});

describe('AttachmentUploadPreview — progress, cancel, and accessibility', () => {
  it('exposes the bar as a progressbar with the committed value', () => {
    const entry = {
      ...createFileEntry('big.bin', 100, 'application/octet-stream', 'uploading'),
      progress: 25,
      bytesSent: 2_097_152,
    };
    Object.defineProperty(entry.file, 'size', { value: 8_388_608 });

    render(<AttachmentUploadPreview files={[entry]} onRemove={vi.fn()} />);

    const bar = screen.getByRole('progressbar');
    // A native <progress>, not a div wearing the role: the element supplies
    // min/max/now semantics itself, so there are no aria-value* attributes to
    // keep in sync with the rendered state.
    expect(bar.tagName).toBe('PROGRESS');
    expect(bar).toHaveValue(25);
    expect(bar).toHaveAttribute('max', '100');
    expect(bar).toHaveAttribute('aria-valuetext', '2.0 MB / 8.0 MB');
  });

  it('OMITS aria-valuenow while preparing — an unknown value must not read as a real one', () => {
    const entry = createFileEntry('big.bin', 100, 'application/octet-stream', 'preparing');

    render(<AttachmentUploadPreview files={[entry]} onRemove={vi.fn()} />);

    const bar = screen.getByRole('progressbar');
    // OMITTING `value` is how HTML spells indeterminate. The platform enforces
    // it -- :indeterminate matches -- where the previous div had to remember to
    // drop aria-valuenow. A stale number would make a frozen upload read live.
    expect(bar).not.toHaveAttribute('value');
    expect(bar.matches(':indeterminate')).toBe(true);
    expect(screen.getByText('Preparing…')).toBeInTheDocument();
  });

  it('goes indeterminate when stalled and names no cause', () => {
    const entry = {
      ...createFileEntry('big.bin', 100, 'application/octet-stream', 'uploading'),
      progress: 40,
      bytesSent: 1024,
      stalled: true,
    };

    render(<AttachmentUploadPreview files={[entry]} onRemove={vi.fn()} />);

    const bar = screen.getByRole('progressbar');
    expect(bar).not.toHaveAttribute('value');
    expect(bar.matches(':indeterminate')).toBe(true);
    expect(screen.getByText('Still uploading…')).toBeInTheDocument();
    // A stall cannot distinguish a slow chunk from a token refresh from a dead
    // link, so it must not claim any of them.
    expect(screen.queryByText(/reconnect|offline|network|connection/i)).toBeNull();
  });

  it('cancels an in-flight upload instead of removing the row', () => {
    const onRemove = vi.fn();
    const onCancel = vi.fn();
    const entry = createFileEntry('big.bin', 100, 'application/octet-stream', 'uploading');

    render(<AttachmentUploadPreview files={[entry]} onRemove={onRemove} onCancel={onCancel} />);

    const btn = screen.getByLabelText('Cancel upload of big.bin');
    fireEvent.click(btn);

    expect(onCancel).toHaveBeenCalledWith('up-big.bin');
    expect(onRemove).not.toHaveBeenCalled();
  });

  it('removes — not cancels — a row that is not live', () => {
    const onRemove = vi.fn();
    const onCancel = vi.fn();
    const entry = createFileEntry('done.bin', 100, 'application/octet-stream', 'done');

    render(<AttachmentUploadPreview files={[entry]} onRemove={onRemove} onCancel={onCancel} />);

    fireEvent.click(screen.getByLabelText('Remove done.bin'));

    expect(onRemove).toHaveBeenCalledWith(0);
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('shows a cancelled row as cancelled, not as an error', () => {
    const entry = createFileEntry('gone.bin', 100, 'application/octet-stream', 'cancelled');

    render(<AttachmentUploadPreview files={[entry]} onRemove={vi.fn()} />);

    expect(screen.getByText('Cancelled')).toBeInTheDocument();
    expect(screen.queryByRole('progressbar')).toBeNull();
    // A user who pressed stop has not hit a failure.
    expect(screen.queryByText(/failed/i)).toBeNull();
  });

  it('renders no progressbar at all for a queued file', () => {
    const entry = createFileEntry('queued.bin', 2048, 'application/octet-stream', 'pending');

    render(<AttachmentUploadPreview files={[entry]} onRemove={vi.fn()} />);

    expect(screen.queryByRole('progressbar')).toBeNull();
    expect(screen.getByText('2.0 KB')).toBeInTheDocument();
  });
});
