import { describe, it, expect } from 'vitest';
import type { CallEventStatus } from '@/renderer/types/chat';
import {
  describeMessagePreview,
  formatMessagePreview,
  truncateSenderName,
} from '@/renderer/utils/messaging/messagePreview';

describe('describeMessagePreview — the six-rung table', () => {
  const rungs: Array<[string, string | undefined, string, string]> = [
    ['photo', undefined, 'a Photo', 'Photo'],
    ['animated', undefined, 'a GIF', 'GIF'],
    ['video', undefined, 'a Video', 'Video'],
    ['audio', undefined, 'an Audio file', 'Audio'],
    ['file', 'application/pdf', 'a Doc', 'Doc'],
    ['file', 'application/x-tar', 'a File', 'File'],
  ];

  it.each(rungs)('file_type=%s mime=%s -> %s / %s', (fileType, mime, phrase, suffix) => {
    const d = describeMessagePreview({ attachmentType: fileType, attachmentMime: mime });
    expect(d.kind).toBe('attachment');
    expect(d.phrase).toBe(phrase);
    expect(d.suffix).toBe(suffix);
  });

  it('classifies a Klipy gif envelope as GIF with no media_files row', () => {
    const d = describeMessagePreview({ gifSlug: 'party-parrot' });
    expect(d.kind).toBe('gif');
    expect(d.phrase).toBe('a GIF');
  });

  it('never lets audio reach the Doc rung, even with a doc-shaped mime', () => {
    // Classifier order is fixed: only file_type='file' consults the allowlist.
    const d = describeMessagePreview({
      attachmentType: 'audio',
      attachmentMime: 'application/pdf',
    });
    expect(d.suffix).toBe('Audio');
  });

  it('ignores MIME parameters when matching the doc allowlist', () => {
    const d = describeMessagePreview({
      attachmentType: 'file',
      attachmentMime: 'text/plain; charset=utf-8',
    });
    expect(d.suffix).toBe('Doc');
  });

  it('returns kind empty when there is no text, gif or attachment', () => {
    expect(describeMessagePreview({ content: '' }).kind).toBe('empty');
  });
});

describe('describeMessagePreview — caption composition', () => {
  it('carries the tag alongside the caption so the caption keeps the lead', () => {
    const d = describeMessagePreview({ content: 'check this out', attachmentType: 'photo' });
    expect(d.kind).toBe('text');
    expect(d.text).toBe('check this out');
    expect(d.suffix).toBe('Photo');
  });

  it('carries no tag for a plain text message', () => {
    const d = describeMessagePreview({ content: 'just words' });
    expect(d.kind).toBe('text');
    expect(d.suffix).toBeUndefined();
  });
});

describe('formatMessagePreview — the flat notification rendering', () => {
  it('renames Image to Photo', () => {
    expect(formatMessagePreview({ attachmentType: 'photo' })).toBe('Photo');
  });

  it('splits uploaded GIFs out of the Photo rung', () => {
    expect(formatMessagePreview({ attachmentType: 'animated' })).toBe('GIF');
  });

  it('folds the former Attachment rung into File', () => {
    expect(formatMessagePreview({ attachmentType: 'something-unknown' })).toBe('File');
  });

  it('adds no sender prefix and no suffix to a captioned attachment (R10)', () => {
    expect(formatMessagePreview({ content: 'hello', attachmentType: 'photo' })).toBe('hello');
  });

  it('falls back when there is nothing to describe', () => {
    expect(formatMessagePreview({ content: '', fallback: 'Encrypted message' })).toBe(
      'Encrypted message'
    );
  });

  it('reads attachments[0] when attachmentType is absent', () => {
    expect(
      formatMessagePreview({ attachments: [{ file_type: 'video', mime_type: 'video/mp4' }] })
    ).toBe('Video');
  });
});

describe('truncateSenderName', () => {
  it('leaves a short name untouched', () => {
    expect(truncateSenderName('Alice')).toBe('Alice');
  });

  it('leaves a name exactly at the budget untouched', () => {
    expect(truncateSenderName('1234567890123456')).toBe('1234567890123456');
  });

  it('truncates at 16 graphemes', () => {
    expect(truncateSenderName('Alexandra Bartholomew')).toBe('Alexandra Bartho…');
  });

  it('never splits a surrogate pair', () => {
    // String.slice(0, 16) would cut this mid-pair and emit a lone surrogate.
    const name = '👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀👩‍🚀';
    const out = truncateSenderName(name);
    expect(out.endsWith('…')).toBe(true);
    expect([
      ...new Intl.Segmenter(undefined, { granularity: 'grapheme' }).segment(out),
    ]).toHaveLength(17);
  });
});

// Merged from tests/unit/utils/messagePreview.test.ts (#2364). That file tested
// this same module from a path that stopped mirroring src when #3045 moved the
// source into utils/messaging/ and left the test behind — which is why a check
// of the mirrored path concluded the module was untested. Its call-event
// coverage is not duplicated anywhere else, so the block is kept whole.
describe('formatMessagePreview — call events and attachment rungs', () => {
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
    // §4.7 media-taxonomy rename (#2364): the generic "Image" noun became the
    // more specific "Photo", and "animated" is its own GIF rung rather than
    // folding into the photo label.
    expect(formatMessagePreview({ content: '', attachmentType: 'photo' })).toBe('Photo');
    expect(formatMessagePreview({ content: '', attachmentType: 'animated' })).toBe('GIF');
    expect(formatMessagePreview({ content: '', attachments: [{ file_type: 'video/mp4' }] })).toBe(
      'Video'
    );
    expect(formatMessagePreview({ content: '', attachments: [{ file_type: 'audio/mpeg' }] })).toBe(
      'Audio'
    );
    // §4.7 also splits a doc-shaped MIME (from the DOC_MIME_TYPES allowlist)
    // out of the generic "File" rung into its own "Doc" label.
    expect(
      formatMessagePreview({ content: '', attachments: [{ file_type: 'application/pdf' }] })
    ).toBe('Doc');
  });

  it('falls back for unknown attachment and empty message previews', () => {
    // §4.7 media-taxonomy rename (#2364): the generic "Attachment" noun
    // became "File".
    expect(
      formatMessagePreview({ content: '', attachments: [{ file_type: 'model/gltf+json' }] })
    ).toBe('File');
    expect(formatMessagePreview({ content: null, fallback: 'Encrypted message' })).toBe(
      'Encrypted message'
    );
    expect(formatMessagePreview({ fallback: 'Encrypted message' })).toBe('Encrypted message');
    expect(formatMessagePreview({ content: '' })).toBe('');
  });

  it('classifies a Doc when the MIME carries whitespace before its parameter', () => {
    // RFC 2045 permits linear whitespace around the parameter separator.
    // Trimming before the split left `text/plain ` — a trailing space that
    // missed the allowlist and silently demoted the Doc to a File.
    expect(
      describeMessagePreview({
        content: '',
        attachmentType: 'file',
        attachmentMime: 'text/plain ; charset=utf-8',
      }).phrase
    ).toBe('a Doc');
  });

  it('strips bidi overrides so a display name cannot reorder the clause after it', () => {
    // Escapes, never literal controls: a literal here trips the repo's own
    // generic.unicode.security.bidi semgrep hook, and the assertion is about
    // what the function removes, not about what this file contains.
    const RLO = '\u202E';
    const PDF = '\u202C';
    // Renders as "You" under UAX #9, then lets the app's own " sent a Photo"
    // continue LTR -- a clean forged attribution needing no clipping at all.
    expect(truncateSenderName(`${RLO}uoY${PDF}`)).toBe('uoY');
    expect(truncateSenderName(`${RLO}uoY${PDF}`)).not.toContain(RLO);
  });

  it('never strands an opening override by truncating away its terminator', () => {
    const RLO = '\u202E';
    const PDF = '\u202C';
    // Balanced, and therefore self-contained everywhere else in the app. The
    // 16-grapheme cut used to keep the RLO and discard the PDF, so this
    // function MANUFACTURED an unbalanced string that was safe on arrival.
    const balanced = `${'A'.repeat(15)}${RLO}evil${PDF}`;
    const out = truncateSenderName(balanced);
    expect(out).not.toContain(RLO);
    expect(out).not.toContain(PDF);
  });

  it('agrees with the enum rung when a MIME arrives in the file_type position', () => {
    // The defensive rung used to send image/gif to Photo while the enum rung
    // sends `animated` to GIF — the two paths disagreeing about one content type.
    expect(describeMessagePreview({ content: '', attachmentType: 'image/gif' }).phrase).toBe(
      'a GIF'
    );
    expect(describeMessagePreview({ content: '', attachmentType: 'animated' }).phrase).toBe(
      'a GIF'
    );
  });
});
