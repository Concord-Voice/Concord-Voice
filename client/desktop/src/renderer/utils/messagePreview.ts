import { unwrapGifEnvelope } from './gifEnvelope';

interface PreviewAttachment {
  readonly file_type?: string | null;
}

interface MessagePreviewOptions {
  readonly content?: string | null;
  readonly gifSlug?: string | null;
  readonly attachmentType?: string | null;
  readonly attachments?: readonly PreviewAttachment[] | null;
  readonly fallback?: string;
}

function mediaLabel(type: string | null | undefined): string | null {
  const normalized = type?.toLowerCase();
  if (!normalized) return null;

  if (normalized === 'photo' || normalized.startsWith('image/')) return 'Image';
  if (normalized === 'animated') return 'Image';
  if (normalized === 'video' || normalized.startsWith('video/')) return 'Video';
  if (normalized === 'audio' || normalized.startsWith('audio/')) return 'Audio';
  if (normalized === 'file' || normalized.startsWith('application/')) return 'File';

  return 'Attachment';
}

export function formatMessagePreview(options: MessagePreviewOptions): string {
  const { text, gifSlug: envelopeGifSlug } = unwrapGifEnvelope(options.content ?? '');
  if (text.trim()) return text;
  if (options.gifSlug || envelopeGifSlug) return 'GIF';

  return (
    mediaLabel(options.attachmentType) ??
    mediaLabel(options.attachments?.[0]?.file_type) ??
    options.fallback ??
    ''
  );
}
