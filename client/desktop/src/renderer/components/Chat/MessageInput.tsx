import React, { useState, useRef, useEffect, useCallback } from 'react';
import { Smile, ImagePlay, Paperclip, Lock, UserPlus } from 'lucide-react';
import MessageInputContextMenu from './MessageInputContextMenu';
import MentionAutocomplete, { type MentionAutocompleteHandle } from './MentionAutocomplete';
import EmojiPicker from '../EmojiPicker/LazyEmojiPicker';
import LazyGifPicker from '../GifPicker/LazyGifPicker';
import UserPanel from '../User/UserPanel';
import SyntaxHelpModal from '../Markdown/SyntaxHelpModal';
import { InviteServerPicker } from './InviteServerPicker';
import { useClientConfigStore } from '../../stores/clientConfigStore';
import { useLayoutStore } from '../../stores/layoutStore';
import { buildAddendum, encodeMentionMeta, type ParsedMention } from '../../utils/mentions';
import type { AttachmentSummary, MessageWithStatus } from '../../types/chat';
import { useFileUpload } from '../../hooks/useFileUpload';
import { useDraftMessage } from '../../hooks/useDraftMessage';
import { useEntitlement } from '../../hooks/useEntitlement';
import { useChatStore } from '../../stores/chatStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { useInviteStore } from '../../stores/inviteStore';
import { buildInviteUrl } from '../../utils/inviteUrl';
import AttachmentUploadPreview from './AttachmentUploadPreview';
import ReplyPreviewBar from './ReplyPreviewBar';
import { composeMarkdownOverflow } from '../../utils/overflowToMarkdown';
import { MAX_ATTACHMENTS, formatFileSize } from '../../utils/attachmentCrypto';
import { PREMIUM_ATTACHMENT_BYTES, clampMessageCharsForTier } from '../../utils/entitlementLimits';
import './MessageInput.css';

/** Premium raises the free message limit by this factor (UX hint copy only —
 *  the server is the authority on the real premium caps). */
const PREMIUM_MULTIPLIER = 2;
/** Counter announcement thresholds (a11y U1): announce ONLY at 75% / 90% /
 *  at-limit — never per keystroke. */
const COUNTER_ANNOUNCE_RATIOS = [0.75, 0.9, 1] as const;
/** Renderer-side DoS ceiling: paste/typing beyond this is silently discarded.
 *  Content between the active entitlement limit (maxLength) and
 *  DOS_PROTECTION_LIMIT reaches handleSend intact so the overflow path can
 *  synthesize a .md attachment. */
const DOS_PROTECTION_LIMIT = 1_048_576; // 1 MiB plaintext
// Counter visibility and warn thresholds are expressed as ratios of the
// active limit (maxLength), so the same 75%/95% behaviour holds at any tier
// (e.g. free 5120: 3840/5120 = 0.75, 4864/5120 = 0.95; premium 10240 scales).
const COUNTER_VISIBLE_RATIO = 0.75;
const COUNTER_WARN_RATIO = 0.95;

/**
 * Determines whether content exceeds maxLength and, if so, synthesizes the
 * overflow .md payload. Extracted from handleSend to keep its cognitive
 * complexity within SonarQube's budget.
 *
 * NOTE: do NOT call addFiles with the returned overflowFiles — that schedules a
 * React state update and uploadAll closes over the pre-update `files` snapshot,
 * silently dropping the overflow file. Pass them via uploadAll's `additionalFiles`
 * parameter instead.
 */
function prepareOverflowPayload(
  trimmedContent: string,
  maxLength: number
): { finalContent: string; overflowFiles: File[] } {
  if (trimmedContent.length <= maxLength) {
    return { finalContent: trimmedContent, overflowFiles: [] };
  }
  const { previewText, fileBlob, filename } = composeMarkdownOverflow(trimmedContent);
  return {
    finalContent: previewText,
    overflowFiles: [new File([fileBlob], filename, { type: 'text/markdown' })],
  };
}

/**
 * Runs the upload step inside handleSend, handling both pending user-added
 * files and the optional overflow .md synthesized by prepareOverflowPayload.
 * Returns the combined attachment ids + summaries on success, or null when an
 * error was surfaced (the caller should bail without sending).
 *
 * Extracted from handleSend to keep its cognitive complexity ≤ 15.
 */
async function tryUploadAttachments(
  uploadAll: (
    channelId: string,
    conversationId?: string,
    additionalFiles?: File[]
  ) => Promise<{ ids: string[]; summaries: AttachmentSummary[] }>,
  channelId: string,
  conversationId: string | undefined,
  overflowFiles: File[],
  setUploadError: (msg: string | null) => void,
  setUploadStatus: (msg: string | null) => void
): Promise<{ ids: string[]; summaries: AttachmentSummary[] } | null> {
  setUploadError(null);
  setUploadStatus(null);
  try {
    const result = await uploadAll(
      channelId,
      conversationId,
      overflowFiles.length > 0 ? overflowFiles : undefined
    );
    return result;
  } catch {
    setUploadError('Failed to upload attachments');
    return null;
  }
}

function getCounterClass(charCount: number, limit: number): string {
  if (charCount > limit) return 'counter error';
  if (charCount >= Math.floor(limit * COUNTER_WARN_RATIO)) return 'counter warn';
  return 'counter';
}

export interface MessageInputProps {
  onSendMessage: (
    content: string,
    mentionMeta?: string,
    replyToId?: string,
    attachmentIds?: string[],
    attachments?: AttachmentSummary[],
    gifSlug?: string
  ) => void;
  onTyping?: (isTyping: boolean) => void;
  placeholder?: string;
  maxLength?: number;
  disabled?: boolean;
  channelName?: string;
  /** Server ID for permission-gated mention autocomplete (undefined in DMs) */
  serverId?: string;
  /** Channel ID for SBAC override resolution (undefined in DMs) */
  channelId?: string;
  /** DM conversation ID — used to source mention candidates from DM participants */
  conversationId?: string;
  /** Message being replied to (preview bar above textarea) */
  replyingTo?: MessageWithStatus | null;
  /** Cancel the reply */
  onCancelReply?: () => void;
  /** Whether the user has permission to attach files */
  canAttachFiles?: boolean;
}

const MessageInput: React.FC<MessageInputProps> = ({
  onSendMessage,
  onTyping,
  placeholder = 'Type a message...',
  maxLength: maxLengthProp,
  disabled = false,
  channelName,
  serverId,
  channelId,
  conversationId,
  replyingTo,
  onCancelReply,
  canAttachFiles = true,
}) => {
  const channelPanelPinned = useLayoutStore((s) => s.channelPanelPinned);
  // L7/L9 (#1301): informational-only premium caps. The message char-limit and
  // attachment-size limit are server-authoritative; these only drive UX hints —
  // NEVER a hard client block (the server re-checks every send/upload).
  const entitlementTier = useEntitlement((e) => e.tier);
  const maxMessageChars = useEntitlement((e) => e.maxMessageChars);
  const maxAttachmentBytes = useEntitlement((e) => e.maxAttachmentBytes);
  // The plaintext char split is client-authoritative (server sees only
  // ciphertext under E2EE). Source it from the live entitlement so the counter
  // and the .md overflow react to free (5120) / premium (10240) without a
  // reload; the `maxLength` prop remains a test/override seam.
  const maxLength = maxLengthProp ?? clampMessageCharsForTier(entitlementTier, maxMessageChars);
  const [content, setContent] = useState('');
  // L9: a non-modal inline banner for an over-limit attachment attempt.
  const [attachUpsell, setAttachUpsell] = useState<string | null>(null);
  const [helpModalOpen, setHelpModalOpen] = useState(false);
  const draftTargetId = channelId || conversationId;
  const {
    initialDraft,
    saveDraft: saveDraftDebounced,
    clearDraft: clearDraftNow,
  } = useDraftMessage(draftTargetId);
  const [dragOver, setDragOver] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [uploadStatus, setUploadStatus] = useState<string | null>(null);
  const isSendingRef = useRef(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const {
    files: uploadFiles,
    addFiles,
    removeFile,
    clearFiles,
    uploadAll,
    isUploading,
    hasFiles,
  } = useFileUpload();
  const [cursorPos, setCursorPos] = useState(0);
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number } | null>(null);
  const [showEmojiPicker, setShowEmojiPicker] = useState(false);
  const [emojiPickerPos, setEmojiPickerPos] = useState<{
    x: number;
    y: number;
    anchorCenterX: number;
  }>({ x: 0, y: 0, anchorCenterX: 0 });
  const [showGifPicker, setShowGifPicker] = useState(false);
  const [gifPickerPos, setGifPickerPos] = useState<{ x: number; y: number; anchorCenterX: number }>(
    { x: 0, y: 0, anchorCenterX: 0 }
  );
  const gifsEnabled = useClientConfigStore((s) => s.featureFlags.gifsEnabled ?? false);
  const [invitePickerOpen, setInvitePickerOpen] = useState(false);
  const createInvite = useInviteStore((s) => s.createInvite);
  const fetchChannelOverrides = usePermissionStore((s) => s.fetchChannelOverrides);
  const [showMentions, setShowMentions] = useState(false);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const emojiBtnRef = useRef<HTMLButtonElement>(null);
  const gifBtnRef = useRef<HTMLButtonElement>(null);
  const typingTimeoutRef = useRef<NodeJS.Timeout | null>(null);
  const isTypingRef = useRef(false);
  // Track mentions selected via autocomplete (for building the addendum)
  const selectedMentionsRef = useRef<ParsedMention[]>([]);
  const mentionAutocompleteRef = useRef<MentionAutocompleteHandle | null>(null);

  // Auto-focus textarea when replyingTo changes
  useEffect(() => {
    if (replyingTo && textareaRef.current) {
      textareaRef.current.focus();
    }
  }, [replyingTo]);

  // Restore draft content and reply context when channel/conversation changes
  useEffect(() => {
    if (initialDraft) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- external-data sync: hydrate the input from the persisted draft on channel/conversation switch
      setContent(initialDraft.text);
      // Restore reply context if the draft had one and the message is still in memory
      if (initialDraft.replyToId && draftTargetId) {
        const messages = useChatStore.getState().messagesByChannel.get(draftTargetId);
        const replyMsg = messages?.find((m) => m.id === initialDraft.replyToId);
        if (replyMsg) {
          useChatStore.getState().setReplyingTo(draftTargetId, replyMsg);
        }
      }
    } else {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- external-data sync: clear the input when the target channel has no persisted draft
      setContent('');
    }
  }, [initialDraft, draftTargetId]);

  // Warm the channel's SBAC permission overrides so MentionAutocomplete can compute the
  // viewer's channel-effective mention permissions before the first '@'. No-op in DMs.
  useEffect(() => {
    if (serverId && channelId) {
      // Fire-and-forget; fetchChannelOverrides handles its own errors internally. Using
      // `.catch` (the codebase's fire-and-forget idiom) instead of the `void` operator
      // keeps both no-floating-promises and SonarQube S3735 satisfied.
      fetchChannelOverrides(channelId).catch(() => {
        /* swallowed: the store action already handles/logs failures */
      });
    }
  }, [serverId, channelId, fetchChannelOverrides]);

  // Save draft when reply context changes (so switching channels preserves reply state)
  const prevReplyRef = useRef(replyingTo);
  useEffect(() => {
    if (prevReplyRef.current !== replyingTo) {
      prevReplyRef.current = replyingTo;
      if (draftTargetId) {
        saveDraftDebounced(content, replyingTo);
      }
    }
  }, [replyingTo, draftTargetId, content, saveDraftDebounced]);

  const handleContextMenu = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setContextMenu({ x: e.clientX, y: e.clientY });
  }, []);

  const handlePasteFromMenu = useCallback(
    (text: string) => {
      const textarea = textareaRef.current;
      if (!textarea) {
        // Fallback: just append
        setContent((prev) => {
          const combined = prev + text;
          return combined.length <= DOS_PROTECTION_LIMIT
            ? combined
            : combined.slice(0, DOS_PROTECTION_LIMIT);
        });
        return;
      }

      const start = textarea.selectionStart;
      const end = textarea.selectionEnd;
      const before = content.slice(0, start);
      const after = content.slice(end);
      const combined = before + text + after;
      const newContent =
        combined.length <= DOS_PROTECTION_LIMIT
          ? combined
          : combined.slice(0, DOS_PROTECTION_LIMIT);

      setContent(newContent);

      // Restore cursor position after the pasted text
      requestAnimationFrame(() => {
        const newCursor = Math.min(start + text.length, newContent.length);
        textarea.selectionStart = newCursor;
        textarea.selectionEnd = newCursor;
        textarea.focus();
      });
    },
    [content]
  );

  const handleEmojiSelect = useCallback(
    (emoji: string) => {
      const textarea = textareaRef.current;
      if (!textarea) {
        setContent((prev) => {
          const combined = prev + emoji;
          return combined.length <= DOS_PROTECTION_LIMIT
            ? combined
            : combined.slice(0, DOS_PROTECTION_LIMIT);
        });
        setShowEmojiPicker(false);
        return;
      }

      const start = textarea.selectionStart;
      const end = textarea.selectionEnd;
      const before = content.slice(0, start);
      const after = content.slice(end);
      const combined = before + emoji + after;
      const newContent =
        combined.length <= DOS_PROTECTION_LIMIT
          ? combined
          : combined.slice(0, DOS_PROTECTION_LIMIT);

      setContent(newContent);
      setShowEmojiPicker(false);

      requestAnimationFrame(() => {
        const newCursor = Math.min(start + emoji.length, newContent.length);
        textarea.selectionStart = newCursor;
        textarea.selectionEnd = newCursor;
        textarea.focus();
      });
    },
    [content]
  );

  const getPickerPosition = useCallback((anchor: HTMLButtonElement | null, width: number) => {
    if (!anchor) return { x: 0, y: 0, anchorCenterX: 0 };
    const rect = anchor.getBoundingClientRect();
    const pickerHeight = 520;
    const anchorCenterX = rect.left + rect.width / 2;

    // Position above the button, right-aligned to the button so the arrow
    // tail can point down at the anchor. 12px gap leaves room for the tail.
    let x = rect.right - width;
    let y = rect.top - pickerHeight - 12;

    // Viewport clamping
    x = Math.max(8, Math.min(x, globalThis.innerWidth - width - 8));
    y = Math.max(8, y);

    return { x, y, anchorCenterX };
  }, []);

  const toggleEmojiPicker = useCallback(() => {
    if (showEmojiPicker) {
      setShowEmojiPicker(false);
    } else {
      setShowGifPicker(false);
      setEmojiPickerPos(getPickerPosition(emojiBtnRef.current, 352));
      setShowEmojiPicker(true);
    }
  }, [showEmojiPicker, getPickerPosition]);

  const toggleGifPicker = useCallback(() => {
    if (showGifPicker) {
      setShowGifPicker(false);
    } else {
      setShowEmojiPicker(false);
      setGifPickerPos(getPickerPosition(gifBtnRef.current, 370));
      setShowGifPicker(true);
    }
  }, [showGifPicker, getPickerPosition]);

  const handleGifSelect = useCallback(
    (gifSlug: string) => {
      onSendMessage(' ', undefined, replyingTo?.id, undefined, undefined, gifSlug);
      setShowGifPicker(false);
      onCancelReply?.();
    },
    [onSendMessage, replyingTo, onCancelReply]
  );

  const handlePickInviteServer = useCallback(
    async (serverId: string) => {
      setInvitePickerOpen(false);
      const invite = await createInvite(serverId);
      if (!invite) return; // createInvite records its own error in the store
      const url = buildInviteUrl(invite.code);
      setContent((prev) => (prev.trim() ? `${prev.trimEnd()} ${url}` : url));
    },
    [createInvite]
  );

  const adjustTextareaHeight = useCallback(() => {
    const textarea = textareaRef.current;
    if (!textarea) return;

    // When empty, collapse to a single row
    if (!content) {
      textarea.style.height = 'auto';
      return;
    }

    // Cap at 25% of the closest .chat-view ancestor (or 200px fallback)
    const chatView = textarea.closest('.chat-view');
    const maxH = chatView ? chatView.clientHeight * 0.25 : 200;

    textarea.style.height = 'auto';
    textarea.style.height = Math.min(textarea.scrollHeight, maxH) + 'px';
  }, [content]);

  useEffect(() => {
    adjustTextareaHeight();
  }, [adjustTextareaHeight]);

  // Re-adjust on window resize so the textarea doesn't retain a stale height
  useEffect(() => {
    const onResize = () => adjustTextareaHeight();
    globalThis.addEventListener('resize', onResize);
    return () => globalThis.removeEventListener('resize', onResize);
  }, [adjustTextareaHeight]);

  // Cleanup typing state on unmount
  useEffect(() => {
    return () => {
      if (typingTimeoutRef.current) {
        clearTimeout(typingTimeoutRef.current);
      }
      if (onTyping && isTypingRef.current) {
        onTyping(false);
      }
    };
  }, [onTyping]);

  // Detect @ trigger for mention autocomplete. showMentions is derived from content+cursorPos,
  // which are mutated by 6+ handlers (typing, emoji/gif/mention insert, draft restore, send);
  // centralizing the derivation in one effect is clearer than scattering @-detection across
  // every handler, and Escape-to-dismiss needs an imperative override the derived value can't
  // express — so this is an intentional set-state-in-effect, not the anti-pattern.
  /* eslint-disable @eslint-react/set-state-in-effect -- intentional centralized derivation; see comment above */
  useEffect(() => {
    if (!content) {
      setShowMentions(false);
      return;
    }
    // Check if there's an active @query at cursor position
    let i = cursorPos - 1;
    while (i >= 0 && content[i] !== '@' && content[i] !== ' ' && content[i] !== '\n') {
      i--;
    }
    if (
      i >= 0 &&
      content[i] === '@' &&
      (i === 0 || content[i - 1] === ' ' || content[i - 1] === '\n')
    ) {
      setShowMentions(true);
    } else {
      setShowMentions(false);
    }
  }, [content, cursorPos]);
  /* eslint-enable @eslint-react/set-state-in-effect -- re-enable after the intentional centralized-derivation effect above */

  const handleMentionSelect = useCallback(
    (mention: ParsedMention, replacementText: string) => {
      const textarea = textareaRef.current;
      if (!textarea) return;

      const before = content.slice(0, mention.start);
      const after = content.slice(cursorPos);
      const newContent = before + replacementText + after;

      setContent(newContent);
      setShowMentions(false);

      // Track the selected mention for building the addendum on send
      selectedMentionsRef.current.push({
        ...mention,
        start: mention.start,
        end: mention.start + replacementText.trimEnd().length,
      });

      // Move cursor after the inserted mention
      requestAnimationFrame(() => {
        const newCursor = mention.start + replacementText.length;
        textarea.selectionStart = newCursor;
        textarea.selectionEnd = newCursor;
        setCursorPos(newCursor);
        textarea.focus();
      });
    },
    [content, cursorPos]
  );

  const buildMentionMeta = (trimmedContent: string): string | undefined => {
    if (selectedMentionsRef.current.length === 0) return undefined;
    const validMentions = selectedMentionsRef.current.filter((m) => trimmedContent.includes(m.raw));
    const addendum = buildAddendum(validMentions);
    return addendum ? encodeMentionMeta(addendum) : undefined;
  };

  const stopTypingIndicator = () => {
    if (onTyping && isTypingRef.current) {
      isTypingRef.current = false;
      onTyping(false);
    }
    if (typingTimeoutRef.current) {
      clearTimeout(typingTimeoutRef.current);
      typingTimeoutRef.current = null;
    }
  };

  const handleSend = async () => {
    // Guard against concurrent invocations (e.g., double-tap Enter before
    // isUploading becomes true on the next render cycle).
    if (isSendingRef.current) return;
    isSendingRef.current = true;

    try {
      const trimmedContent = content.trim();
      const isOverflow = trimmedContent.length > maxLength;

      if (isOverflow && uploadFiles.length >= MAX_ATTACHMENTS) {
        // Edge case: all 5 attachment slots are taken AND the message is over the cap.
        // The overflow path needs a free slot for the .md attachment — block with a
        // user-visible error so they can remove one attachment first.
        setUploadError('Remove an attachment to send a long message as a .md file.');
        return;
      }

      const hasContent = trimmedContent.length > 0;
      if (!hasContent && !hasFiles) return;

      stopTypingIndicator();

      // Synthesize the overflow .md file if content exceeds the policy cap.
      const { finalContent, overflowFiles } = prepareOverflowPayload(trimmedContent, maxLength);

      const mentionMeta = buildMentionMeta(finalContent);

      // Upload attachments (including any overflow .md file passed as additionalFiles).
      // `hasFiles` reflects pending user-added files; `isOverflow` covers the
      // synthesized .md that is passed synchronously via additionalFiles.
      let attachmentIds: string[] | undefined;
      let attachmentSummaries: AttachmentSummary[] | undefined;
      if (hasFiles || isOverflow) {
        const targetId = conversationId || channelId || '';
        const uploadResult = await tryUploadAttachments(
          uploadAll,
          targetId,
          conversationId,
          overflowFiles,
          setUploadError,
          setUploadStatus
        );
        if (uploadResult === null) return; // error already toasted
        if (uploadResult.ids.length > 0) {
          attachmentIds = uploadResult.ids;
          attachmentSummaries = uploadResult.summaries;
        }
      }

      const sendContent = finalContent || (attachmentIds ? ' ' : '');
      if (!sendContent) return;

      onSendMessage(sendContent, mentionMeta, replyingTo?.id, attachmentIds, attachmentSummaries);

      if (isOverflow) {
        setUploadStatus('Long message sent as a .md attachment.');
      } else {
        setUploadError(null);
      }

      setContent('');
      clearDraftNow();
      setShowMentions(false);
      selectedMentionsRef.current = [];
      clearFiles();
      setAttachUpsell(null);
      onCancelReply?.();
      if (textareaRef.current) {
        textareaRef.current.style.height = 'auto';
      }
    } finally {
      isSendingRef.current = false;
    }
  };

  /**
   * L9 (#1301): inspect a selection for a file over the free attachment-size
   * cap and surface a non-modal upsell banner. Returns the over-limit file's
   * banner text (also stored in `attachUpsell`) or null. This does NOT block —
   * the file still flows to `addFiles`, whose own hard MAX_FILE_SIZE validation
   * (the DoS ceiling) decides acceptance. The banner is purely informational.
   */
  const checkAttachmentUpsell = (incoming: FileList | File[]): void => {
    const over = Array.from(incoming).find((f) => f.size > maxAttachmentBytes);
    if (!over) {
      setAttachUpsell(null);
      return;
    }
    const prefix = `${over.name} is ${formatFileSize(over.size)}.`;
    setAttachUpsell(
      entitlementTier === 'premium'
        ? `${prefix} Current limit ${formatFileSize(maxAttachmentBytes)}.`
        : `${prefix} Free limit ${formatFileSize(
            maxAttachmentBytes
          )}. Premium raises it to ${formatFileSize(PREMIUM_ATTACHMENT_BYTES)}.`
    );
  };

  const handleRemoveFile = useCallback(
    (index: number) => {
      removeFile(index);
      setAttachUpsell(null);
    },
    [removeFile]
  );

  const handleFileSelect = (e: React.ChangeEvent<HTMLInputElement>) => {
    if (e.target.files && e.target.files.length > 0) {
      checkAttachmentUpsell(e.target.files);
      const error = addFiles(e.target.files);
      if (error) setUploadError(error);
      else setUploadError(null);
    }
    // Reset input so same file can be re-selected
    e.target.value = '';
  };

  const handleDragOver = (e: React.DragEvent) => {
    if (!canAttachFiles) return;
    e.preventDefault();
    e.stopPropagation();
    setDragOver(true);
  };

  const handleDragLeave = (e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setDragOver(false);
  };

  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setDragOver(false);
    if (!canAttachFiles || !e.dataTransfer.files.length) return;
    checkAttachmentUpsell(e.dataTransfer.files);
    const error = addFiles(e.dataTransfer.files);
    if (error) setUploadError(error);
    else setUploadError(null);
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // Let mention autocomplete handle keyboard events first
    if (showMentions && mentionAutocompleteRef.current) {
      const handled = mentionAutocompleteRef.current.handleKeyDown(e);
      if (handled) return;
    }

    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  };

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    // Allow input slightly above the policy cap (maxLength + 1000) so the
    // counter enters the `error` state and the send button is disabled for
    // the over-limit UX — the overflow path then handles large content on
    // send. Clipboard pastes are bounded by DOS_PROTECTION_LIMIT (1 MiB),
    // which is enforced by handlePasteFromMenu and the native-paste handler.
    const HARD_CAP = maxLength + 1000;
    if (e.target.value.length <= HARD_CAP) {
      setContent(e.target.value);
      saveDraftDebounced(e.target.value, replyingTo);
      setCursorPos(e.target.selectionStart);
    }

    // Typing indicator logic
    if (onTyping) {
      if (!isTypingRef.current && e.target.value.length > 0) {
        isTypingRef.current = true;
        onTyping(true);
      }

      // Clear previous timeout
      if (typingTimeoutRef.current) {
        clearTimeout(typingTimeoutRef.current);
      }

      // If input is cleared, stop typing immediately
      if (e.target.value.length === 0) {
        if (isTypingRef.current) {
          isTypingRef.current = false;
          onTyping(false);
        }
      } else {
        // Set timeout to stop typing after 3 seconds of no input
        typingTimeoutRef.current = setTimeout(() => {
          if (isTypingRef.current) {
            isTypingRef.current = false;
            onTyping(false);
          }
        }, 3000);
      }
    }
  };

  // Track cursor position on click/selection
  const handleSelect = useCallback(() => {
    const textarea = textareaRef.current;
    if (textarea) {
      setCursorPos(textarea.selectionStart);
    }
  }, []);

  const charCount = content.length;
  const counterVisible = charCount >= Math.floor(maxLength * COUNTER_VISIBLE_RATIO);
  const counterClass = getCounterClass(charCount, maxLength);
  const atLimit = charCount >= maxLength;
  const showPremiumLimitHint = entitlementTier !== 'premium';

  // L7 (a11y U1): announce ONLY at 75% / 90% / at-limit thresholds. The message
  // is a function of the highest band CROSSED, so the aria-live text changes
  // (and re-announces) only on a band transition — never per keystroke.
  const announceRatio = [...COUNTER_ANNOUNCE_RATIOS]
    .reverse()
    .find((r) => charCount >= Math.floor(maxLength * r));
  let counterAnnouncement = '';
  if (announceRatio === 1) {
    counterAnnouncement = `Message at the ${maxLength}-character limit. Longer messages send as a .md attachment.`;
  } else if (announceRatio === 0.9) {
    counterAnnouncement = `Approaching the ${maxLength}-character limit.`;
  } else if (announceRatio === 0.75) {
    counterAnnouncement = `Message has reached 75% of the ${maxLength}-character limit.`;
  }

  return (
    <div className="message-input-container">
      {/* Always-visible E2EE status indicator */}
      <div className="e2ee-status-bar encrypted">
        <Lock size={14} />
        <span>Messages are Encrypted End-to-End</span>
      </div>

      <section
        aria-label="Message composition area"
        className={`message-input-wrapper ${dragOver ? 'drag-over' : ''}`}
        style={{ position: 'relative' }}
        onDragOver={handleDragOver}
        onDragLeave={handleDragLeave}
        onDrop={handleDrop}
      >
        {showMentions && (
          <MentionAutocomplete
            ref={mentionAutocompleteRef}
            text={content}
            cursorPosition={cursorPos}
            serverId={serverId}
            channelId={channelId}
            conversationId={conversationId}
            onSelect={handleMentionSelect}
            onClose={() => setShowMentions(false)}
            anchorRef={textareaRef}
          />
        )}

        {replyingTo && (
          <ReplyPreviewBar
            repliedTo={{
              id: replyingTo.id,
              user_id: replyingTo.user_id,
              username: replyingTo.username,
              display_name: replyingTo.display_name,
              content: replyingTo.content,
            }}
            variant="input"
            onCancel={onCancelReply}
          />
        )}

        <div className="message-input-box" onContextMenu={handleContextMenu}>
          {!channelPanelPinned && (
            <div className="message-input-user-panel">
              <UserPanel compact />
            </div>
          )}
          {hasFiles && <AttachmentUploadPreview files={uploadFiles} onRemove={handleRemoveFile} />}
          {uploadError && <div className="upload-error">{uploadError}</div>}
          {uploadStatus && !uploadError && <div className="upload-status">{uploadStatus}</div>}
          {/* L9: non-modal inline attachment-size upsell banner (#1301). */}
          {attachUpsell && (
            <output className="attachment-upsell-banner">
              <span>{attachUpsell}</span>
              <button
                type="button"
                className="attachment-upsell-dismiss"
                aria-label="Dismiss"
                onClick={() => setAttachUpsell(null)}
              >
                ×
              </button>
            </output>
          )}
          <textarea
            ref={textareaRef}
            className="message-input-textarea"
            placeholder={channelName ? `Message #${channelName}` : placeholder}
            value={content}
            onChange={handleChange}
            onKeyDown={handleKeyDown}
            onSelect={handleSelect}
            onPaste={(e) => {
              if (canAttachFiles && e.clipboardData.files.length > 0) {
                e.preventDefault();
                checkAttachmentUpsell(e.clipboardData.files);
                const error = addFiles(e.clipboardData.files);
                if (error) setUploadError(error);
                else setUploadError(null);
              }
            }}
            disabled={disabled}
            rows={1}
            aria-label="Message input"
          />

          <div className="message-input-actions">
            {counterVisible && (
              <span className={counterClass}>
                {charCount}/{maxLength}
                {/* L7: at the limit, advertise the .md-attachment overflow path
                    and the premium uplift. Informational — send is NEVER blocked. */}
                {atLimit && (
                  <span className="counter-overflow-hint">
                    {' '}
                    · .md attachment
                    {showPremiumLimitHint && <> · {PREMIUM_MULTIPLIER}× with Premium</>}
                  </span>
                )}
              </span>
            )}
            {/* L7 (a11y U1): threshold-only announcer — text changes only on a
                band transition, so AT reads it at 75% / 90% / at-limit, not on
                every keystroke. */}
            <span className="sr-only" aria-live="polite">
              {counterAnnouncement}
            </span>

            <button
              className="send-button"
              onClick={handleSend}
              disabled={disabled || isUploading || (!content.trim() && !hasFiles)}
              aria-label="Send message"
            >
              <svg width="20" height="20" viewBox="0 0 20 20" fill="none">
                <path d="M2 10l16-8-8 16-2-6-6-2z" fill="currentColor" />
              </svg>
            </button>
          </div>
        </div>

        <div className="input-hint">
          Supports <strong>bold</strong>, <em>italic</em>, <code>code</code>, and more.{' '}
          <button
            type="button"
            className="help-icon"
            aria-label="Markdown syntax help"
            onClick={() => setHelpModalOpen(true)}
          >
            ?
          </button>
        </div>

        <div className="message-input-hints">
          <span className="hint">
            Press <kbd>Enter</kbd> to send, <kbd>Shift+Enter</kbd> for new line
          </span>
          <div className="message-input-extras">
            <button
              ref={emojiBtnRef}
              className={`media-btn ${showEmojiPicker ? 'active' : ''}`}
              title="Emoji"
              onClick={toggleEmojiPicker}
            >
              <Smile size={18} />
            </button>
            <button
              ref={gifBtnRef}
              className={`media-btn ${showGifPicker ? 'active' : ''}`}
              title="GIF"
              disabled={!gifsEnabled}
              onClick={toggleGifPicker}
            >
              <ImagePlay size={18} />
            </button>
            <button
              className="media-btn"
              title="Attach file"
              disabled={!canAttachFiles}
              onClick={() => fileInputRef.current?.click()}
            >
              <Paperclip size={18} />
            </button>
            <input
              ref={fileInputRef}
              type="file"
              multiple
              style={{ display: 'none' }}
              onChange={handleFileSelect}
            />
            {conversationId && (
              <div className="message-input__invite" style={{ position: 'relative' }}>
                <button
                  type="button"
                  className="media-btn"
                  onClick={() => setInvitePickerOpen((o) => !o)}
                  aria-label="Invite to a server"
                  title="Invite to a server"
                >
                  <UserPlus size={18} />
                </button>
                {invitePickerOpen && (
                  <InviteServerPicker
                    onPick={handlePickInviteServer}
                    onClose={() => setInvitePickerOpen(false)}
                  />
                )}
              </div>
            )}
          </div>
        </div>
      </section>

      {showEmojiPicker && (
        <EmojiPicker
          mode="popover"
          position={emojiPickerPos}
          onSelect={handleEmojiSelect}
          onClose={() => setShowEmojiPicker(false)}
        />
      )}

      {showGifPicker && (
        <LazyGifPicker
          position={gifPickerPos}
          onSelect={handleGifSelect}
          onClose={() => setShowGifPicker(false)}
        />
      )}

      {contextMenu && (
        <MessageInputContextMenu
          position={contextMenu}
          onClose={() => setContextMenu(null)}
          onPaste={handlePasteFromMenu}
          onOpenEmojiPicker={toggleEmojiPicker}
        />
      )}

      <SyntaxHelpModal open={helpModalOpen} onClose={() => setHelpModalOpen(false)} />
    </div>
  );
};

export default MessageInput;
