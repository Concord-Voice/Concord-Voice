import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '../../../test-utils';
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
  return { file, progress: 0, status, previewUrl };
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

    const { container } = render(<AttachmentUploadPreview files={files} onRemove={vi.fn()} />);

    const progressFill = container.querySelector('.attachment-progress-fill');
    expect(progressFill).toBeInTheDocument();
    expect(progressFill).toHaveStyle({ width: '50%' });
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
