import { useCallback, useEffect, useRef, useState } from 'react';
import { ChevronDown, ChevronUp, FileText, Loader2 } from 'lucide-react';
import MarkdownContent from '../Markdown/MarkdownContent';
import { apiFetch } from '../../services/system/apiClient';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { formatFileSize } from '../../utils/attachmentCrypto';
import { decryptAttachmentBlob, parseKeyVersionHeader } from '../../utils/attachmentChunkedCrypto';
import {
  AttachmentTooLargeError,
  readBoundedBody,
  tooLargeMessage,
} from '../../utils/boundedResponseBody';
import { isRenderableMarkdown, MAX_RENDERABLE_MD_BYTES } from '../../utils/renderableMarkdown';
import { MAX_DECRYPTABLE_ATTACHMENT_BYTES } from '../../utils/entitlementLimits';
import type { AttachmentSummary } from '../../types/chat';
import type { MentionLookup } from './messageUtils';
import './OverflowMarkdownAttachment.css';

interface OverflowMarkdownAttachmentProps {
  /** The attachment summary for the overflow .md file. */
  attachment: AttachmentSummary;
  /** Already-decrypted preview body from the parent message — typically ends with '…'. */
  previewBody: string;
  /** Channel ID used to look up the decryption key. */
  channelId: string;
}

/** Stable empty lookup so MarkdownContent memoization doesn't bust on every render. */
const EMPTY_MENTION_LOOKUP: MentionLookup = {
  users: new Map(),
  roles: new Map(),
};

/**
 * Discriminated union covering all 6 display states.
 *
 * - collapsed: shows preview + Expand button
 * - loading: fetch + decrypt in flight; shows preview + spinner
 * - rendered: full content decoded; shows full markdown + Collapse button
 * - preview-unavailable: fetch or decrypt failed, or isRenderableMarkdown returned false
 * - too-large: decrypted bytes > MAX_RENDERABLE_MD_BYTES (a RENDER-cost refusal)
 * - too-large-to-open: declared bytes > MAX_DECRYPTABLE_ATTACHMENT_BYTES, refused
 *   before any allocation (a DOWNLOAD refusal — a different limit for a
 *   different cost, so it carries the size that tripped it)
 */
type ExpandedState =
  | { kind: 'collapsed' }
  | { kind: 'loading' }
  | { kind: 'rendered'; content: string }
  | { kind: 'preview-unavailable' }
  | { kind: 'too-large' }
  | { kind: 'too-large-to-open'; bytes: number; truncated?: boolean };

/** The subset of ExpandedState a completed load can produce. */
type LoadOutcome = Extract<
  ExpandedState,
  | { kind: 'rendered' }
  | { kind: 'preview-unavailable' }
  | { kind: 'too-large' }
  | { kind: 'too-large-to-open' }
>;

/**
 * Fetch, bound, decrypt and validate the overflow `.md` body, returning the
 * state to apply. Never throws and never touches React, so the effect below
 * stays a thin apply step rather than carrying the whole chain's branching
 * (SonarQube S3776 — the inline version reached cognitive complexity 19).
 *
 * Folding the size refusal into the single catch also widens it: an
 * AttachmentTooLargeError raised anywhere in the chain now reports as
 * `too-large-to-open` rather than only the one raised by readBoundedBody.
 */
async function loadOverflowMarkdown(fileId: string, channelId: string): Promise<LoadOutcome> {
  try {
    const response = await apiFetch(`/api/v1/media/attachments/${fileId}`);
    if (!response.ok) return { kind: 'preview-unavailable' };

    // Measure the body as it arrives rather than trusting either declared size:
    // `file_size` is server-supplied metadata, and Content-Length is absent
    // under chunked transfer encoding and understates a gzipped body.
    const ciphertext = await readBoundedBody(response, MAX_DECRYPTABLE_ATTACHMENT_BYTES);
    // Same epoch selection as AttachmentDisplay: a CSK rotation must not orphan
    // an overflow document that is otherwise perfectly readable.
    const keyVersion = parseKeyVersionHeader(response.headers.get('X-File-Key-Version'));
    const channelKey =
      keyVersion === null
        ? await e2eeService.getChannelKey(channelId)
        : await e2eeService.getChannelKeyByVersion(channelId, keyVersion);
    // Format-aware: handles both the chunked v2 envelope and the legacy
    // single-shot blob, dispatched deterministically before any key is used.
    // The render ceiling is orders of magnitude below the download ceiling, so
    // decrypting past it is work spent on bytes this component is about to
    // discard. Passing the limit stops the loop one chunk after it is exceeded;
    // the Blob is then a PREFIX, which is why the size check below still runs
    // and still rejects.
    const blob = await decryptAttachmentBlob(
      new Uint8Array(ciphertext),
      channelKey,
      'text/markdown',
      MAX_RENDERABLE_MD_BYTES
    );

    // Render-cost gate, distinct from the download guard above: cheaper than
    // the full UTF-8 decode plus regex scan that follows.
    //
    // Checked against the Blob BEFORE materialising it, where it used to run
    // after — the gate now precedes the allocation it exists to avoid rather
    // than following it.
    if (blob.size > MAX_RENDERABLE_MD_BYTES) return { kind: 'too-large' };

    const decrypted = new Uint8Array(await blob.arrayBuffer());
    if (!isRenderableMarkdown(decrypted)) return { kind: 'preview-unavailable' };

    return { kind: 'rendered', content: new TextDecoder('utf-8').decode(decrypted) };
  } catch (err) {
    if (err instanceof AttachmentTooLargeError) {
      return { kind: 'too-large-to-open', bytes: err.byteLength, truncated: err.truncated };
    }
    return { kind: 'preview-unavailable' };
  }
}

const OverflowMarkdownAttachment: React.FC<OverflowMarkdownAttachmentProps> = ({
  attachment,
  previewBody,
  channelId,
}) => {
  const [state, setState] = useState<ExpandedState>({ kind: 'collapsed' });
  // Memoized decrypted text — populated on first successful expand; guarded against
  // re-fetch on subsequent expand cycles (collapse does not evict this cache).
  const [cachedContent, setCachedContent] = useState<string | null>(null);

  // Ref for the Collapse button — used to restore keyboard focus after the
  // Expand button disappears and the component transitions to rendered state.
  const collapseRef = useRef<HTMLButtonElement>(null);

  // Kick off the fetch + decrypt chain when loading state is entered.
  // The cancelled flag prevents setState calls after unmount (same pattern as GifEmbed.tsx).
  useEffect(() => {
    if (state.kind !== 'loading') return;

    // Memoization: re-use already-decrypted content without another network round-trip.
    if (cachedContent !== null) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: transitions state from loading→rendered when cached content is available; not a render loop
      setState({ kind: 'rendered', content: cachedContent });
      return;
    }

    // Guards run BEFORE the allocating operation, not between two of them.
    // `file_size` rides on the summary, so an oversized attachment costs no
    // network at all — this is a different limit from MAX_RENDERABLE_MD_BYTES
    // below, which is about render cost once the bytes are already resident.
    if (attachment.file_size > MAX_DECRYPTABLE_ATTACHMENT_BYTES) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: refuses the download on the declared size before any fetch is issued; not a render loop
      setState({ kind: 'too-large-to-open', bytes: attachment.file_size });
      return;
    }

    let cancelled = false;

    void (async () => {
      const outcome = await loadOverflowMarkdown(attachment.id, channelId);
      if (cancelled) return;
      if (outcome.kind === 'rendered') setCachedContent(outcome.content);
      setState(outcome);
    })();

    return () => {
      cancelled = true;
    };
  }, [state.kind, attachment.id, attachment.file_size, channelId, cachedContent]);

  // Move keyboard focus to the Collapse button once the rendered state is entered,
  // so keyboard users are not stranded at <body> after the Expand button disappears.
  useEffect(() => {
    if (state.kind === 'rendered') {
      collapseRef.current?.focus();
    }
  }, [state.kind]);

  const handleExpand = useCallback((): void => {
    setState({ kind: 'loading' });
  }, []);

  const handleCollapse = useCallback((): void => {
    // Return to collapsed without evicting the cached content.
    setState({ kind: 'collapsed' });
  }, []);

  // ------------------------------------------------------------------
  // Collapsed state
  // ------------------------------------------------------------------
  if (state.kind === 'collapsed') {
    return (
      <div className="overflow-md-attachment overflow-md-attachment--collapsed">
        <MarkdownContent
          id={`overflow-preview-${attachment.id}`}
          content={previewBody}
          editedAt={null}
          mentionLookup={EMPTY_MENTION_LOOKUP}
        />
        <button type="button" className="overflow-md-attachment__expand" onClick={handleExpand}>
          <ChevronDown size={16} aria-hidden="true" />
          <span>Expand ({formatFileSize(attachment.file_size)})</span>
        </button>
      </div>
    );
  }

  // ------------------------------------------------------------------
  // Loading state
  // ------------------------------------------------------------------
  if (state.kind === 'loading') {
    return (
      <div className="overflow-md-attachment overflow-md-attachment--loading">
        <MarkdownContent
          id={`overflow-preview-${attachment.id}`}
          content={previewBody}
          editedAt={null}
          mentionLookup={EMPTY_MENTION_LOOKUP}
        />
        <output aria-label="Loading full message" className="overflow-md-attachment__spinner">
          <Loader2 size={16} className="spinner" aria-hidden="true" />
          <span>Loading…</span>
        </output>
      </div>
    );
  }

  // ------------------------------------------------------------------
  // Rendered state
  // ------------------------------------------------------------------
  if (state.kind === 'rendered') {
    return (
      <div className="overflow-md-attachment overflow-md-attachment--rendered">
        <MarkdownContent
          id={`overflow-full-${attachment.id}`}
          content={state.content}
          editedAt={null}
          mentionLookup={EMPTY_MENTION_LOOKUP}
        />
        <button
          ref={collapseRef}
          type="button"
          className="overflow-md-attachment__collapse"
          onClick={handleCollapse}
        >
          <ChevronUp size={16} aria-hidden="true" />
          <span>Collapse</span>
        </button>
      </div>
    );
  }

  // ------------------------------------------------------------------
  // Fallback states: preview-unavailable, too-large, or too-large-to-open
  // ------------------------------------------------------------------
  let fallbackMessage: string;
  if (state.kind === 'too-large-to-open') {
    // Shared with AttachmentDisplay so the wording cannot drift between the
    // two refusal surfaces (two test files assert it).
    fallbackMessage = tooLargeMessage(state.bytes, state.truncated, formatFileSize);
  } else if (state.kind === 'too-large') {
    fallbackMessage = `Markdown file (${formatFileSize(attachment.file_size)}) — too large to preview, download to view.`;
  } else {
    fallbackMessage = 'Preview unavailable — download to view.';
  }

  // The refusal is announced: the user just pressed Expand and the content will
  // never arrive. The other two fallbacks are static chip copy.
  const fallbackRole = state.kind === 'too-large-to-open' ? 'alert' : undefined;

  return (
    <div className="overflow-md-attachment overflow-md-attachment--fallback" role={fallbackRole}>
      <FileText size={16} aria-hidden="true" />
      {/* AttachmentSummary carries no filename field; "message.md" is the
          conventional display name for overflow markdown attachments. */}
      <span className="overflow-md-attachment__filename">message.md</span>
      <span className="overflow-md-attachment__message">{fallbackMessage}</span>
    </div>
  );
};

export default OverflowMarkdownAttachment;
