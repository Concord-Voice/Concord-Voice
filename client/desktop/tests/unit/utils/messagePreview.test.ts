import { describe, expect, it } from 'vitest';
import { formatMessagePreview } from '@/renderer/utils/messagePreview';

describe('formatMessagePreview', () => {
  it('keeps plaintext previews primary', () => {
    expect(formatMessagePreview({ content: 'hello', gifSlug: 'wave' })).toBe('hello');
  });

  it('labels GIF-only encrypted envelopes', () => {
    expect(formatMessagePreview({ content: '{"text":"","gif_slug":"night-sleep-18"}' })).toBe(
      'GIF'
    );
  });

  it('labels GIF metadata when the plaintext is empty', () => {
    expect(formatMessagePreview({ content: '', gifSlug: 'night-sleep-18' })).toBe('GIF');
  });

  it('maps image, video, audio, and file attachment previews', () => {
    expect(formatMessagePreview({ content: '', attachmentType: 'photo' })).toBe('Image');
    expect(formatMessagePreview({ content: '', attachmentType: 'animated' })).toBe('Image');
    expect(formatMessagePreview({ content: '', attachments: [{ file_type: 'video/mp4' }] })).toBe(
      'Video'
    );
    expect(formatMessagePreview({ content: '', attachments: [{ file_type: 'audio/mpeg' }] })).toBe(
      'Audio'
    );
    expect(
      formatMessagePreview({ content: '', attachments: [{ file_type: 'application/pdf' }] })
    ).toBe('File');
  });

  it('falls back for unknown attachment and empty message previews', () => {
    expect(
      formatMessagePreview({ content: '', attachments: [{ file_type: 'model/gltf+json' }] })
    ).toBe('Attachment');
    expect(formatMessagePreview({ content: null, fallback: 'Encrypted message' })).toBe(
      'Encrypted message'
    );
    expect(formatMessagePreview({ fallback: 'Encrypted message' })).toBe('Encrypted message');
    expect(formatMessagePreview({ content: '' })).toBe('');
  });
});
