import { describe, expect, it } from 'vitest';
import type { CallEventStatus } from '@/renderer/types/chat';
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

  it.each([
    ['user-1', 'completed', 'Outbound call answered'],
    ['user-2', 'completed', 'Inbound call answered'],
    ['user-1', 'missed', 'Outbound call — no answer'],
    ['user-2', 'missed', 'Inbound call missed'],
    ['user-1', 'declined', 'Voice call declined'],
    ['user-2', 'canceled', 'Voice call canceled'],
    ['user-1', 'failed', 'Voice call failed'],
  ])('labels caller %s with status %s as %s', (callerUserId, status, expected) => {
    const options = {
      content: '',
      callEventPayload: {
        caller_user_id: callerUserId,
        participant_user_ids: ['user-1', 'user-2'],
        started_at: '2026-07-13T12:00:00.000Z',
        status: status as CallEventStatus,
        duration_seconds: 0,
      },
      currentUserId: 'user-1',
    };
    expect(formatMessagePreview(options)).toBe(expected);
  });

  it('does not infer missed from an incomplete teardown participant snapshot', () => {
    const options = {
      content: '',
      callEventPayload: {
        caller_user_id: 'user-2',
        participant_user_ids: ['user-2', 'user-3'],
        started_at: '2026-07-13T12:00:00.000Z',
        status: 'completed' as CallEventStatus,
        duration_seconds: 60,
      },
      currentUserId: 'user-1',
    };
    expect(formatMessagePreview(options)).toBe('Inbound call answered');
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
