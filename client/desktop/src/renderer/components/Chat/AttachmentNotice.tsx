import React from 'react';
import { AlertTriangle, Clock } from 'lucide-react';
import PremiumChip from '../common/PremiumChip';
import { useGateActivation } from '../../hooks/useGateActivation';
import { formatFileSize, MAX_ATTACHMENTS } from '../../utils/attachmentCrypto';
import { formatLimitBytes, PREMIUM_ATTACHMENT_BYTES } from '../../utils/entitlementLimits';
import type { AttachmentRejection } from '../../hooks/useFileUpload';
import './AttachmentNotice.css';

export interface AttachmentNoticeProps {
  readonly rejections: AttachmentRejection[];
  /** How many files from the SAME selection were accepted — drives the
   *  partial-batch prefix. */
  readonly acceptedCount?: number;
  /** Size of the whole selection. NOT derivable as acceptedCount +
   *  rejections.length: one `too-many` rejection stands for every surplus file,
   *  so that sum under-reports how many were actually discarded. */
  readonly selectionCount?: number;
  readonly tier: string;
  readonly onDismiss: () => void;
}

type Severity = 'error' | 'limitation';

interface NoticeCopy {
  text: string;
  severity: Severity;
}

/**
 * Copy for one rejection.
 *
 * Every branch names the file, its size, and the applicable number in text
 * (WCAG SC 3.3.1) — the severity colour is never the only signal.
 *
 * Limit numbers use `formatLimitBytes` so they read exactly as the pricing page
 * writes them ("32 MB"); the file's own measured size keeps full precision via
 * `formatFileSize` ("28.4 MB"). Mixing the two is what let the enforced number
 * and the advertised number drift apart in the first place.
 */
function describeRejection(r: AttachmentRejection, tier: string): NoticeCopy {
  if (r.kind === 'too-many') {
    // Sourced from the constant validateFiles enforces: a hardcoded 5 is the
    // same enforced-vs-displayed drift this whole module exists to prevent.
    return { text: `Maximum ${MAX_ATTACHMENTS} attachments per message.`, severity: 'error' };
  }
  if (r.kind === 'empty') {
    return { text: `${r.fileName} is empty.`, severity: 'error' };
  }

  const size = formatFileSize(r.fileSize ?? 0);

  // The client-capability gap: the user's PLAN allows more than this build can
  // encrypt. NOT a plan gate — so no upsell and no lock glyph. Telling someone
  // to upgrade for something they already bought is worse than saying nothing.
  if (r.limit.source === 'client-ceiling') {
    return {
      text:
        `${r.fileName} is ${size}. Your plan allows ` +
        `${formatLimitBytes(r.limit.entitlementBytes)}, but this version of Concord can send ` +
        `files up to ${formatLimitBytes(r.limit.limitBytes)}. Support for larger files is coming.`,
      severity: 'limitation',
    };
  }

  if (tier === 'premium') {
    return {
      text: `${r.fileName} is ${size} — over your ${formatLimitBytes(r.limit.limitBytes)} limit.`,
      severity: 'error',
    };
  }

  return {
    text:
      `${r.fileName} is ${size} — over the ${formatLimitBytes(r.limit.limitBytes)} free limit. ` +
      `Premium raises it to ${formatLimitBytes(PREMIUM_ATTACHMENT_BYTES)}.`,
    severity: 'error',
  };
}

/**
 * The single pre-queue rejection surface (#2157).
 *
 * Replaces the split between `checkAttachmentUpsell`'s advisory banner (which
 * read the live entitlement) and `validateFiles`'s hard rejection (which read a
 * flat constant). Those were calibrated to different numbers, so a free user
 * attaching 28 MiB saw no banner and was then refused against a 25 MB limit
 * nothing in the product promised.
 *
 * Post-queue transport/encrypt failures keep their own assertive
 * `.upload-error` region; the dividing line is deliberate so the two live
 * regions never compete for the same announcement.
 */
const AttachmentNotice: React.FC<AttachmentNoticeProps> = ({
  rejections,
  acceptedCount = 0,
  selectionCount,
  tier,
  onDismiss,
}) => {
  const { onActivate } = useGateActivation('upload-size');

  // Mounted UNCONDITIONALLY, hidden by `:empty`. A live region created and
  // populated in the same commit is missed by several screen readers, so the
  // element has to exist before it has anything to say.
  if (rejections.length === 0) {
    return <output className="attachment-notice" />;
  }

  const first = rejections[0];
  const { text, severity } = describeRejection(first, tier);
  const extra = rejections.length - 1;
  const total = selectionCount ?? acceptedCount + rejections.length;
  const prefix = acceptedCount > 0 ? `Added ${acceptedCount} of ${total} files. ` : '';
  const suffix = extra > 0 ? ` (+${extra} more)` : '';

  // Upsell only where upgrading actually changes the outcome.
  const showUpsell = severity === 'error' && first.kind === 'over-limit' && tier !== 'premium';

  return (
    <output className={`attachment-notice attachment-notice--${severity}`}>
      {severity === 'limitation' ? (
        <Clock size={16} aria-hidden="true" />
      ) : (
        <AlertTriangle size={16} aria-hidden="true" />
      )}
      <span className="attachment-notice__text">{`${prefix}${text}${suffix}`}</span>
      {showUpsell && <PremiumChip onActivate={onActivate} />}
      <button
        type="button"
        className="attachment-notice__dismiss"
        aria-label="Dismiss"
        onClick={onDismiss}
      >
        &times;
      </button>
    </output>
  );
};

export default AttachmentNotice;
