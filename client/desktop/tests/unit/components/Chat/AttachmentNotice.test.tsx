import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import AttachmentNotice from '@/renderer/components/Chat/AttachmentNotice';
import type { AttachmentRejection } from '@/renderer/hooks/useFileUpload';
import { MAX_ATTACHMENTS } from '@/renderer/utils/attachmentCrypto';
import {
  FREE_ATTACHMENT_BYTES,
  PREMIUM_ATTACHMENT_BYTES,
  resolveAttachmentLimit,
} from '@/renderer/utils/entitlementLimits';

vi.mock('@/renderer/utils/openSubscriptionPage', () => ({
  openSubscriptionPage: vi.fn(),
}));

// MAX_ATTACHMENTS is really 5, which is also the number the old hardcoded copy
// used — so a test asserting "Maximum 5" passes whether or not the component
// sources the constant. Mocking it to a DIFFERENT value is what makes the
// assertion discriminate (#2837 mutation sweep).
const MOCK_MAX_ATTACHMENTS = 7;
vi.mock('@/renderer/utils/attachmentCrypto', async () => {
  const actual = await vi.importActual<typeof import('@/renderer/utils/attachmentCrypto')>(
    '@/renderer/utils/attachmentCrypto'
  );
  return { ...actual, MAX_ATTACHMENTS: 7 };
});

const freeLimit = resolveAttachmentLimit({ userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES });
/** Premium clamped by the legacy fallback ceiling, so source === 'legacy-upload-path'. */
const ceilingLimit = resolveAttachmentLimit({ userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES });
/** Premium over its OWN entitlement — the ceiling did not bind.
 *  Both byte fields must be the premium entitlement: spreading `ceilingLimit`
 *  and flipping only `source` left `limitBytes` at the 128 MiB client ceiling,
 *  which models a 128 MiB *entitlement* — a state that cannot exist — and so
 *  asserted the wrong number in the copy (#2837 review, row 3). */
const premiumEntitlementLimit = {
  limitBytes: PREMIUM_ATTACHMENT_BYTES,
  entitlementBytes: PREMIUM_ATTACHMENT_BYTES,
  source: 'entitlement' as const,
};

function rejection(over: Partial<AttachmentRejection> = {}): AttachmentRejection {
  return {
    kind: 'over-limit',
    fileName: 'big.png',
    fileSize: 40_000_000,
    limit: freeLimit,
    ...over,
  };
}

describe('AttachmentNotice', () => {
  it('renders an empty live region when idle so AT observes it before it fills', () => {
    const { container } = render(
      <AttachmentNotice rejections={[]} tier="free" onDismiss={vi.fn()} />
    );
    const region = container.querySelector('output');
    expect(region).not.toBeNull();
    expect(region).toBeEmptyDOMElement();
  });

  it('names both the free limit and the correct premium uplift for a free user', () => {
    render(<AttachmentNotice rejections={[rejection()]} tier="free" onDismiss={vi.fn()} />);
    expect(screen.getByText(/over the 32 MB free limit/)).toBeInTheDocument();
    // Regression: this read "512 MB" before #2157 corrected the constant.
    expect(screen.getByText(/Premium raises it to 256 MB/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Premium/ })).toBeInTheDocument();
  });

  it('shows no upsell to a premium user over their own limit', () => {
    render(
      <AttachmentNotice
        rejections={[
          rejection({
            fileName: 'huge.bin',
            fileSize: 300_000_000,
            limit: premiumEntitlementLimit,
          }),
        ]}
        tier="premium"
        onDismiss={vi.fn()}
      />
    );
    expect(screen.getByText(/over your 256 MB limit/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Premium/ })).not.toBeInTheDocument();
  });

  // R4: a lock next to "your plan allows 256 MB" tells the user to buy
  // something they already bought.
  it('uses a clock and no upsell for the server-version gap', () => {
    const { container } = render(
      <AttachmentNotice
        rejections={[
          rejection({ fileName: 'film.mp4', fileSize: 200_000_000, limit: ceilingLimit }),
        ]}
        tier="premium"
        onDismiss={vi.fn()}
      />
    );
    expect(
      screen.getByText(/Your plan allows 256 MB, but this server accepts files up to 128 MB/)
    ).toBeInTheDocument();
    // The attribution matters: this branch fires when the CLIENT is the newer
    // side, so blaming "this version of Concord" points at the wrong end, and
    // "support is coming" is false — it has arrived, on the server's side.
    expect(screen.queryByText(/this version of Concord/)).toBeNull();
    expect(screen.queryByText(/coming/)).toBeNull();
    expect(container.querySelector('.lucide-clock')).not.toBeNull();
    expect(container.querySelector('.lucide-triangle-alert')).toBeNull();
    // PremiumChip owns the lock glyph app-wide; a capability gap must not wear it.
    expect(container.querySelector('.premium-chip__glyph')).toBeNull();
    expect(screen.queryByRole('button', { name: /Premium/ })).not.toBeInTheDocument();
  });

  it('summarises a partial batch and counts the extra rejections', () => {
    render(
      <AttachmentNotice
        rejections={[
          rejection({ fileName: 'a.png' }),
          rejection({ fileName: 'b.png', fileSize: 41_000_000 }),
        ]}
        acceptedCount={3}
        tier="free"
        onDismiss={vi.fn()}
      />
    );
    expect(screen.getByText(/Added 3 of 5 files\./)).toBeInTheDocument();
    expect(screen.getByText(/a\.png/)).toBeInTheDocument();
    expect(screen.getByText(/\(\+1 more\)/)).toBeInTheDocument();
  });

  // VULN-004: the total must come from the real selection size. One `too-many`
  // rejection can stand for several discarded files, so acceptedCount +
  // rejections.length under-reports the denominator.
  it('reports the true selection size, not accepted + rejections', () => {
    render(
      <AttachmentNotice
        rejections={[{ kind: 'too-many', limit: freeLimit }]}
        acceptedCount={5}
        selectionCount={8}
        tier="free"
        onDismiss={vi.fn()}
      />
    );
    expect(screen.getByText(/Added 5 of 8 files\./)).toBeInTheDocument();
    expect(screen.queryByText(/Added 5 of 6 files\./)).not.toBeInTheDocument();
  });

  it('renders the empty-file branch', () => {
    render(
      <AttachmentNotice
        rejections={[rejection({ kind: 'empty', fileName: 'e.png', fileSize: 0 })]}
        tier="free"
        onDismiss={vi.fn()}
      />
    );
    expect(screen.getByText(/e\.png is empty\./)).toBeInTheDocument();
    // An empty file is not a limit problem, so no upgrade affordance.
    expect(screen.queryByRole('button', { name: /Premium/ })).not.toBeInTheDocument();
  });

  it('renders the too-many branch', () => {
    render(
      <AttachmentNotice
        rejections={[{ kind: 'too-many', limit: freeLimit }]}
        tier="free"
        onDismiss={vi.fn()}
      />
    );
    // Renders the mocked constant, not a literal — see MOCK_MAX_ATTACHMENTS.
    expect(screen.getByText(/Maximum 7 attachments per message\./)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Premium/ })).not.toBeInTheDocument();
  });

  // Review row 6: the count was hardcoded while MAX_ATTACHMENTS enforced it.
  it('sources the too-many count from the enforced constant', () => {
    render(
      <AttachmentNotice
        rejections={[{ kind: 'too-many', limit: freeLimit }]}
        tier="free"
        onDismiss={vi.fn()}
      />
    );
    // Would fail against a hardcoded "Maximum 5".
    expect(MAX_ATTACHMENTS).toBe(MOCK_MAX_ATTACHMENTS);
    expect(screen.getByText(/Maximum 7 attachments per message\./)).toBeInTheDocument();
    expect(screen.queryByText(/Maximum 5 attachments/)).not.toBeInTheDocument();
  });

  it('calls onDismiss so the host can return focus to the textarea', async () => {
    const onDismiss = vi.fn();
    render(
      <AttachmentNotice
        rejections={[rejection({ kind: 'empty', fileName: 'e.png', fileSize: 0 })]}
        tier="free"
        onDismiss={onDismiss}
      />
    );
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(onDismiss).toHaveBeenCalledOnce();
  });

  it('never conveys severity by colour alone — every branch carries a sentence', () => {
    const { container } = render(
      <AttachmentNotice rejections={[rejection()]} tier="free" onDismiss={vi.fn()} />
    );
    const text = container.querySelector('.attachment-notice__text');
    expect(text?.textContent?.length ?? 0).toBeGreaterThan(20);
  });
});
