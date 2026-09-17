import { unwrapGifEnvelope } from './gifEnvelope';
import type { CallEventPayload } from '../../types/chat';

interface PreviewAttachment {
  readonly file_type?: string | null;
  readonly mime_type?: string | null;
}

interface MessagePreviewOptions {
  readonly content?: string | null;
  readonly gifSlug?: string | null;
  readonly attachmentType?: string | null;
  readonly attachmentMime?: string | null;
  readonly attachments?: readonly PreviewAttachment[] | null;
  readonly callEventPayload?: CallEventPayload;
  readonly currentUserId?: string;
  readonly fallback?: string;
}

function genericCallLabel(payload: CallEventPayload): string {
  switch (payload.status) {
    case 'completed':
      return 'Voice call';
    case 'missed':
      return 'Missed voice call';
    case 'declined':
      return 'Voice call declined';
    case 'canceled':
      return 'Voice call canceled';
    case 'failed':
      return 'Voice call failed';
    default:
      return 'Voice call failed';
  }
}

export function formatCallEventPreview(payload: CallEventPayload, currentUserId?: string): string {
  const callerUserId = payload.caller_user_id;
  if (!callerUserId || !currentUserId) return genericCallLabel(payload);

  if (payload.status === 'completed') {
    if (callerUserId === currentUserId) return 'Outbound call answered';
    return 'Inbound call answered';
  }

  if (payload.status === 'missed') {
    return callerUserId === currentUserId ? 'Outbound call — no answer' : 'Inbound call missed';
  }

  return genericCallLabel(payload);
}

/** A rung of the media taxonomy. `phrase` carries its own article (C9: a label
 *  table, not an i18n dependency); `suffix` is the bare tag for the `·` form. */
export interface MediaLabel {
  readonly phrase: string;
  readonly suffix: string;
}

const PHOTO: MediaLabel = { phrase: 'a Photo', suffix: 'Photo' };
const GIF: MediaLabel = { phrase: 'a GIF', suffix: 'GIF' };
const VIDEO: MediaLabel = { phrase: 'a Video', suffix: 'Video' };
const AUDIO: MediaLabel = { phrase: 'an Audio file', suffix: 'Audio' };
const DOC: MediaLabel = { phrase: 'a Doc', suffix: 'Doc' };
const FILE: MediaLabel = { phrase: 'a File', suffix: 'File' };

/**
 * Doc-shaped MIME types. A source constant, never config — a config string that
 * selects a code path is code (`config_strings_become_code`).
 */
const DOC_MIME_TYPES: ReadonlySet<string> = new Set([
  'application/pdf',
  'application/msword',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'application/vnd.ms-excel',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  'application/vnd.ms-powerpoint',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  'application/rtf',
  'application/vnd.oasis.opendocument.text',
  'application/vnd.oasis.opendocument.spreadsheet',
  'application/vnd.oasis.opendocument.presentation',
  'text/plain',
  'text/csv',
  'text/markdown',
]);

/**
 * Exact-match allowlist over a SENDER-ASSERTED value (C11). The server takes
 * `mime_type` verbatim from the upload form and cross-checks it against
 * `file_type` nowhere, so this decides a display label and nothing else — it
 * must never gate rendering, download, or an authorization decision, and the raw
 * string is never returned to a caller.
 */
function docFromMime(mime: string | null | undefined): boolean {
  if (!mime) return false;
  // Split BEFORE trimming. RFC 2045 permits linear whitespace around the
  // parameter separator, so `text/plain ; charset=utf-8` trimmed-then-split
  // yields `text/plain ` — a trailing space that misses the allowlist and
  // silently demotes a Doc to File.
  return DOC_MIME_TYPES.has(mime.split(';')[0].trim().toLowerCase());
}

/**
 * Classifier order is FIXED and must not be inverted: the coarse `file_type`
 * enum maps directly, and ONLY `file` consults the MIME allowlist — which is
 * what keeps `audio/*` from ever reaching the Doc rung. Never MIME-first.
 */
function classifyMedia(
  fileType: string | null | undefined,
  mime: string | null | undefined
): MediaLabel | null {
  const t = fileType?.trim().toLowerCase();
  if (!t) return null;

  // The five values MessageAttachmentSchema.file_type admits. Exhaustive for
  // anything that crossed the dispatch boundary.
  if (t === 'photo') return PHOTO;
  if (t === 'animated') return GIF;
  if (t === 'video') return VIDEO;
  if (t === 'audio') return AUDIO;
  if (t === 'file') return docFromMime(mime) ? DOC : FILE;

  // Defensive only: historically some callers passed a MIME string in the
  // file_type position. Still not MIME-first — these run after the enum.
  if (t === 'image/gif') return GIF;
  if (t.startsWith('image/')) return PHOTO;
  if (t.startsWith('video/')) return VIDEO;
  if (t.startsWith('audio/')) return AUDIO;
  if (t.startsWith('application/') || t.startsWith('text/')) return docFromMime(t) ? DOC : FILE;

  return FILE;
}

/** Graphemes of sender name kept before the type word is allowed to truncate. */
const SENDER_NAME_BUDGET = 16;

/** Locale is constant, so one segmenter serves every call. Constructing one per
 *  call cost a fresh ICU segmenter for every attachment row on every render. */
const GRAPHEME_SEGMENTER = new Intl.Segmenter(undefined, { granularity: 'grapheme' });

/**
 * Unicode bidirectional formatting characters: ALM, LRM/RLM, the embedding and
 * override pairs (LRE/RLE/PDF/LRO/RLO), and the isolate set (LRI/RLI/FSI/PDI).
 *
 * Stripped rather than escaped, because a display name has no legitimate use
 * for an EXPLICIT override — implicit bidi already renders an Arabic or Hebrew
 * name correctly, and the container isolates it. Two distinct defects close
 * here, and both concern text the name does not own:
 *
 *  1. An unterminated RLO leaks out of the name and reorders the clause it is
 *     interpolated into, so the app's own words render backwards or read as a
 *     different sentence entirely.
 *  2. The grapheme cut below can STRAND an opening override whose terminator
 *     fell past the budget — a name balanced everywhere else in the app becomes
 *     unbalanced only here, manufactured by this function.
 *
 * Stripping BEFORE the cut makes (2) unreachable by construction rather than by
 * a balance check the truncation would have to re-derive.
 */
const BIDI_FORMAT_CHARS = /[\u061C\u200E\u200F\u202A-\u202E\u2066-\u2069]/gu;

/**
 * Truncates the NAME so CSS ellipsis never eats the type word — composing the
 * whole sentence and letting the tail go removes exactly the information this
 * fix adds (§4.5). Grapheme-aware via Intl.Segmenter, a Chromium builtin (C9):
 * `String.slice` cuts an emoji display name into a lone surrogate.
 *
 * The returned name is display text and is NEVER a delimiter: the caller must
 * still render it in its own element (see `.conversation-preview-sender`),
 * because stripping direction controls does not stop a name that simply READS
 * like the clause it precedes.
 */
export function truncateSenderName(name: string, budget: number = SENDER_NAME_BUDGET): string {
  const safe = name.replace(BIDI_FORMAT_CHARS, '');
  const graphemes = Array.from(GRAPHEME_SEGMENTER.segment(safe), (s) => s.segment);
  if (graphemes.length <= budget) return safe;
  return `${graphemes.slice(0, budget).join('')}…`;
}

/**
 * The structured form of a preview. One classifier, two renderings:
 * `formatMessagePreview` flattens this to today's string for notifications,
 * while the conversation list reads the structure to add attribution and the
 * `· Tag` suffix. Composing attribution INSIDE the formatter would double-
 * attribute notifications, which already name the sender in their title (R10).
 */
export interface PreviewDescriptor {
  readonly kind: 'call' | 'text' | 'gif' | 'attachment' | 'empty';
  /** The message's own text, or the call label when kind === 'call'. */
  readonly text?: string;
  /** Noun phrase with article. Present for 'gif'/'attachment', and for 'text' when the message ALSO carries media. */
  readonly phrase?: string;
  /** Bare tag. Present whenever `phrase` is. */
  readonly suffix?: string;
}

export function describeMessagePreview(options: MessagePreviewOptions): PreviewDescriptor {
  if (options.callEventPayload) {
    return {
      kind: 'call',
      text: formatCallEventPreview(options.callEventPayload, options.currentUserId),
    };
  }

  const { text, gifSlug: envelopeGifSlug } = unwrapGifEnvelope(options.content ?? '');
  const first = options.attachments?.[0];
  const media =
    classifyMedia(options.attachmentType, options.attachmentMime) ??
    classifyMedia(first?.file_type, first?.mime_type);
  const isGif = Boolean(options.gifSlug || envelopeGifSlug);

  if (text.trim()) {
    // Caption wins the leading characters; the tag is what ellipsis eats first,
    // so the captioned case degrades to plain text rather than to something new.
    const tag = media ?? (isGif ? GIF : null);
    return { kind: 'text', text, phrase: tag?.phrase, suffix: tag?.suffix };
  }
  if (isGif) return { kind: 'gif', phrase: GIF.phrase, suffix: GIF.suffix };
  if (media) return { kind: 'attachment', phrase: media.phrase, suffix: media.suffix };
  return { kind: 'empty' };
}

/**
 * Compile-time exhaustiveness. The `never` parameter is what fails the build
 * when a new `PreviewDescriptor['kind']` reaches this call; at runtime the
 * branch is unreachable, so it simply yields the caller's fallback.
 */
function unreachableKind(_kind: never, fallback: string): string {
  return fallback;
}

export function formatMessagePreview(options: MessagePreviewOptions): string {
  const described = describeMessagePreview(options);
  switch (described.kind) {
    case 'call':
    case 'text':
      return described.text ?? '';
    case 'gif':
    case 'attachment':
      return described.suffix ?? '';
    case 'empty':
      return options.fallback ?? '';
    default:
      // A rung added to PreviewDescriptor['kind'] later must fail typecheck
      // at this call rather than silently inheriting the empty-message
      // fallback. Routed through a helper because the inline form needs a
      // discard (`void exhaustive`) to keep the binding used, and that reads
      // as dead code to both a human and a static analyser.
      return unreachableKind(described.kind, options.fallback ?? '');
  }
}
