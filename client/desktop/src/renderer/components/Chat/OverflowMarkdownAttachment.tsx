import { useCallback, useEffect, useRef, useState } from 'react';
import { ChevronDown, ChevronUp, FileText, Loader2 } from 'lucide-react';
import MarkdownContent from '../Markdown/MarkdownContent';
import { apiFetch } from '../../services/apiClient';
import { e2eeService } from '../../services/e2eeService';
import { decryptFile, formatFileSize } from '../../utils/attachmentCrypto';
import { isRenderableMarkdown, MAX_RENDERABLE_MD_BYTES } from '../../utils/renderableMarkdown';
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
 * Discriminated union covering all 5 display states.
 *
 * - collapsed: shows preview + Expand button
 * - loading: fetch + decrypt in flight; shows preview + spinner
 * - rendered: full content decoded; shows full markdown + Collapse button
 * - preview-unavailable: fetch or decrypt failed, or isRenderableMarkdown returned false
 * - too-large: decrypted bytes > MAX_RENDERABLE_MD_BYTES
 */
type ExpandedState =
  | { kind: 'collapsed' }
  | { kind: 'loading' }
  | { kind: 'rendered'; content: string }
  | { kind: 'preview-unavailable' }
  | { kind: 'too-large' };

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

    let cancelled = false;

    (async () => {
      try {
        const response = await apiFetch(`/api/v1/media/attachments/${attachment.id}`);
        if (!response.ok) {
          if (!cancelled) setState({ kind: 'preview-unavailable' });
          return;
        }

        const ciphertextBuffer = await response.arrayBuffer();
        const channelKey = await e2eeService.getChannelKey(channelId);
        const decryptedBuffer = await decryptFile(ciphertextBuffer, channelKey);
        const decryptedBytes = new Uint8Array(decryptedBuffer);

        // Size gate first (cheaper than the full UTF-8 decode + regex scan).
        if (decryptedBytes.byteLength > MAX_RENDERABLE_MD_BYTES) {
          if (!cancelled) setState({ kind: 'too-large' });
          return;
        }

        if (!isRenderableMarkdown(decryptedBytes)) {
          if (!cancelled) setState({ kind: 'preview-unavailable' });
          return;
        }

        const text = new TextDecoder('utf-8').decode(decryptedBytes);
        if (!cancelled) {
          setCachedContent(text);
          setState({ kind: 'rendered', content: text });
        }
      } catch {
        if (!cancelled) setState({ kind: 'preview-unavailable' });
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [state.kind, attachment.id, channelId, cachedContent]);

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
  // Fallback states: preview-unavailable or too-large
  // ------------------------------------------------------------------
  const fallbackMessage =
    state.kind === 'too-large'
      ? `Markdown file (${formatFileSize(attachment.file_size)}) — too large to preview, download to view.`
      : 'Preview unavailable — download to view.';

  return (
    <div className="overflow-md-attachment overflow-md-attachment--fallback">
      <FileText size={16} aria-hidden="true" />
      {/* AttachmentSummary carries no filename field; "message.md" is the
          conventional display name for overflow markdown attachments. */}
      <span className="overflow-md-attachment__filename">message.md</span>
      <span className="overflow-md-attachment__message">{fallbackMessage}</span>
    </div>
  );
};

export default OverflowMarkdownAttachment;
