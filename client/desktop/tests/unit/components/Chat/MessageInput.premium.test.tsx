import { vi } from 'vitest';
import React from 'react';

// ─── Mocks (before component imports) ───────────────────────────────────────

vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({ default: () => null }));
vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => <div data-testid="user-panel" />,
}));
vi.mock('@/renderer/stores/ui/layoutStore', () => ({ useLayoutStore: () => false }));

/** addFiles returns { accepted, rejections } — the accepted COUNT is reported
 *  explicitly rather than derived, because one `too-many` rejection can stand
 *  for several discarded files. */
const mockAddFiles = vi.fn().mockResolvedValue({ accepted: 0, rejections: [] });
let mockUploadFiles: Array<{ file: File; progress: number; status: 'pending' }> = [];
const mockRemoveFile = vi.fn((index: number) => {
  mockUploadFiles = mockUploadFiles.filter((_, i) => i !== index);
});
vi.mock('@/renderer/hooks/useFileUpload', () => ({
  useFileUpload: () => ({
    files: mockUploadFiles,
    addFiles: mockAddFiles,
    removeFile: mockRemoveFile,
    clearFiles: vi.fn(),
    uploadAll: vi.fn().mockResolvedValue({ ids: [], summaries: [] }),
    isUploading: false,
    hasFiles: mockUploadFiles.length > 0,
  }),
}));
vi.mock('@/renderer/components/Chat/AttachmentUploadPreview', () => ({
  default: ({ onRemove }: { onRemove: (index: number) => void }) => (
    <button type="button" onClick={() => onRemove(0)}>
      Remove upload
    </button>
  ),
}));

// Entitlement: FREE floor by default (maxMessageChars 5120, maxAttachmentBytes 32 MiB —
// mirrors the Go free floor; this said 25 MiB before #2157, which was the bug).
const entitlementOverrides: Record<string, unknown> = {};
function freeEntitlement() {
  return {
    tier: 'free',
    maxMessageChars: 5120,
    maxAttachmentBytes: 33_554_432,
    ...entitlementOverrides,
  };
}
vi.mock('@/renderer/hooks/useEntitlement', () => ({
  useEntitlement: vi.fn((selector: (e: Record<string, unknown>) => unknown) =>
    selector(freeEntitlement())
  ),
}));

// ─── Imports (after mocks) ──────────────────────────────────────────────────

import { act } from 'react';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import MessageInput from '@/renderer/components/Chat/MessageInput';
import {
  FREE_ATTACHMENT_BYTES,
  PREMIUM_ATTACHMENT_BYTES,
  resolveAttachmentLimit,
} from '@/renderer/utils/entitlementLimits';

function setEntitlement(overrides: Record<string, unknown>) {
  for (const k of Object.keys(entitlementOverrides)) delete entitlementOverrides[k];
  Object.assign(entitlementOverrides, overrides);
}

function makeFile(name: string, size: number): File {
  const f = new File(['x'], name, { type: 'image/png' });
  Object.defineProperty(f, 'size', { value: size });
  return f;
}

const onSendMessage = vi.fn();

beforeEach(() => {
  vi.clearAllMocks();
  mockUploadFiles = [];
  setEntitlement({});
});

// ─── L7: message length informational gate ──────────────────────────────────

describe('MessageInput — L7 message-length (informational)', () => {
  it('uses the entitlement maxMessageChars as the counter limit when no prop given', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'a'.repeat(3840) } }); // 75% of 5120
    expect(screen.getByText(/\/5120/)).toBeInTheDocument();
  });

  it('an explicit maxLength prop still wins over the entitlement', () => {
    render(<MessageInput onSendMessage={onSendMessage} maxLength={20} />);
    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'a'.repeat(19) } });
    expect(screen.getByText('19/20')).toBeInTheDocument();
  });

  it('NEVER blocks send at the limit — onSendMessage still fires (server is authority)', () => {
    render(<MessageInput onSendMessage={onSendMessage} maxLength={10} />);
    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'a'.repeat(10) } });
    fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onSendMessage).toHaveBeenCalled();
  });

  it('shows the ".md attachment · 2× with Premium" hint at the limit', () => {
    render(<MessageInput onSendMessage={onSendMessage} maxLength={10} />);
    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'a'.repeat(10) } });
    const hint = document.querySelector('.counter-overflow-hint') as HTMLElement;
    expect(hint).toBeInTheDocument();
    expect(hint.textContent).toContain('.md attachment');
    expect(hint.textContent).toContain('2× with Premium');
  });

  it('a11y U1: the live region announces only at thresholds, not below 75%', () => {
    const { container } = render(<MessageInput onSendMessage={onSendMessage} maxLength={100} />);
    const live = container.querySelector('[aria-live="polite"]') as HTMLElement;
    const textarea = screen.getByRole('textbox');
    // Below 75% → empty announcement.
    fireEvent.change(textarea, { target: { value: 'a'.repeat(50) } });
    expect(live.textContent).toBe('');
    // 75% band -> static text, and it must not change on every keystroke.
    fireEvent.change(textarea, { target: { value: 'a'.repeat(75) } });
    const seventyFivePercentText = live.textContent;
    expect(seventyFivePercentText).toBe('Message has reached 75% of the 100-character limit.');
    fireEvent.change(textarea, { target: { value: 'a'.repeat(76) } });
    expect(live.textContent).toBe(seventyFivePercentText);
    // 90% band → "Approaching".
    fireEvent.change(textarea, { target: { value: 'a'.repeat(90) } });
    expect(live.textContent).toMatch(/Approaching/);
    // At limit → "at the … limit".
    fireEvent.change(textarea, { target: { value: 'a'.repeat(100) } });
    expect(live.textContent).toMatch(/limit/);
  });

  it('clamps excessive entitlement message caps before they drive the renderer limit', () => {
    setEntitlement({ tier: 'premium', maxMessageChars: 999_999 });
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'a'.repeat(8000) } });
    expect(screen.getByText('8000/10240')).toBeInTheDocument();
    expect(screen.queryByText(/999999/)).not.toBeInTheDocument();
  });

  it('falls back to the tier ceiling for non-finite entitlement message caps', () => {
    setEntitlement({ tier: 'premium', maxMessageChars: Number.POSITIVE_INFINITY });
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'a'.repeat(8000) } });
    expect(screen.getByText('8000/10240')).toBeInTheDocument();
  });

  it('does not show the premium message-limit upsell to premium users at their own cap', () => {
    setEntitlement({ tier: 'premium', maxMessageChars: 10240 });
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'a'.repeat(10240) } });
    expect(screen.getByText(/10240\/10240/)).toHaveTextContent('.md attachment');
    expect(screen.queryByText(/with Premium/)).not.toBeInTheDocument();
  });
});

// ─── L9: attachment-size upsell banner ──────────────────────────────────────

describe('MessageInput — attachment rejection notice (#2157)', () => {
  const freeLimit = resolveAttachmentLimit({ userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES });
  const ceilingLimit = resolveAttachmentLimit({
    userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES,
  });

  function overLimit(name: string, size: number, limit = freeLimit) {
    return [{ kind: 'over-limit' as const, fileName: name, fileSize: size, limit }];
  }

  /** Shape addFiles now returns. */
  function accepting(accepted: number, rejections: unknown[]) {
    return { accepted, rejections };
  }

  function attach(file: File) {
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [file] } });
  }

  it('renders the rejection addFiles returned, with both limit numbers', async () => {
    mockAddFiles.mockResolvedValueOnce(accepting(0, overLimit('huge.png', 40 * 1024 * 1024)));
    render(<MessageInput onSendMessage={onSendMessage} />);
    attach(makeFile('huge.png', 40 * 1024 * 1024));
    // addFiles is async since #2157 PR 2 (it sniffs leading bytes), so the
    // partition lands a microtask later than the attach event.
    await act(async () => {});

    const notice = document.querySelector('.attachment-notice') as HTMLElement;
    expect(notice.textContent).toContain('huge.png is');
    expect(notice.textContent).toContain('over the 32 MB free limit');
    expect(notice.textContent).toContain('Premium raises it to 256 MB');
  });

  // The file no longer "flows through" a permissive banner — enforcement and
  // messaging are the same decision now, taken inside addFiles.
  it('routes every attach path through addFiles', async () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    attach(makeFile('a.png', 1024));
    // addFiles is async since #2157 PR 2 (it sniffs leading bytes), so the
    // partition lands a microtask later than the attach event.
    await act(async () => {});
    expect(mockAddFiles).toHaveBeenCalled();
  });

  it('shows nothing when addFiles accepts everything', async () => {
    mockAddFiles.mockResolvedValueOnce(accepting(1, []));
    render(<MessageInput onSendMessage={onSendMessage} />);
    attach(makeFile('ok.png', 1024 * 1024));
    // addFiles is async since #2157 PR 2 (it sniffs leading bytes), so the
    // partition lands a microtask later than the attach event.
    await act(async () => {});
    expect(document.querySelector('.attachment-notice')).toBeEmptyDOMElement();
  });

  it('is dismissible and returns focus to the textarea', async () => {
    mockAddFiles.mockResolvedValueOnce(accepting(0, overLimit('huge.png', 40 * 1024 * 1024)));
    render(<MessageInput onSendMessage={onSendMessage} />);
    attach(makeFile('huge.png', 40 * 1024 * 1024));
    // addFiles is async since #2157 PR 2 (it sniffs leading bytes), so the
    // partition lands a microtask later than the attach event.
    await act(async () => {});

    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));

    expect(document.querySelector('.attachment-notice')).toBeEmptyDOMElement();
    // Collapsing the region must not drop focus to <body>.
    expect(document.activeElement).toBe(screen.getByRole('textbox'));
  });

  it('gives a premium user over their own limit no upsell', async () => {
    setEntitlement({ tier: 'premium', maxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES });
    mockAddFiles.mockResolvedValueOnce(
      accepting(
        0,
        overLimit('huge.bin', 300 * 1024 * 1024, {
          // Both byte fields carry the premium entitlement. Spreading
          // `ceilingLimit` and flipping only `source` left limitBytes at the
          // 128 MiB client ceiling, modelling a 128 MiB entitlement that cannot
          // exist — and so asserted the wrong number (#2837 review, row 3).
          limitBytes: PREMIUM_ATTACHMENT_BYTES,
          entitlementBytes: PREMIUM_ATTACHMENT_BYTES,
          source: 'entitlement' as const,
        })
      )
    );
    render(<MessageInput onSendMessage={onSendMessage} />);
    attach(makeFile('huge.bin', 300 * 1024 * 1024));
    // addFiles is async since #2157 PR 2 (it sniffs leading bytes), so the
    // partition lands a microtask later than the attach event.
    await act(async () => {});

    const notice = document.querySelector('.attachment-notice') as HTMLElement;
    expect(notice.textContent).toContain('over your 256 MB limit');
    expect(notice.textContent).not.toContain('Premium raises it to');
    expect(screen.queryByRole('button', { name: /Premium/ })).not.toBeInTheDocument();
  });

  it('explains the server-version gap without an upsell', async () => {
    setEntitlement({ tier: 'premium', maxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES });
    mockAddFiles.mockResolvedValueOnce(
      accepting(0, overLimit('film.mp4', 200 * 1024 * 1024, ceilingLimit))
    );
    render(<MessageInput onSendMessage={onSendMessage} />);
    attach(makeFile('film.mp4', 200 * 1024 * 1024));
    // addFiles is async since #2157 PR 2 (it sniffs leading bytes), so the
    // partition lands a microtask later than the attach event.
    await act(async () => {});

    const notice = document.querySelector('.attachment-notice') as HTMLElement;
    expect(notice.textContent).toContain('Your plan allows 256 MB');
    expect(notice.textContent).toContain('this server accepts files up to 128 MB');
    expect(screen.queryByRole('button', { name: /Premium/ })).not.toBeInTheDocument();
  });

  it('clears the notice after the offending file is removed', async () => {
    const oversized = makeFile('huge.png', 40 * 1024 * 1024);
    mockUploadFiles = [{ file: oversized, progress: 0, status: 'pending' }];
    mockAddFiles.mockResolvedValueOnce(accepting(0, overLimit('huge.png', 40 * 1024 * 1024)));
    render(<MessageInput onSendMessage={onSendMessage} />);
    attach(oversized);
    // addFiles is async since #2157 PR 2 (it sniffs leading bytes), so the
    // partition lands a microtask later than the attach event.
    await act(async () => {});
    expect(document.querySelector('.attachment-notice')).not.toBeEmptyDOMElement();

    fireEvent.click(screen.getByRole('button', { name: 'Remove upload' }));

    expect(mockRemoveFile).toHaveBeenCalledWith(0);
    expect(document.querySelector('.attachment-notice')).toBeEmptyDOMElement();
  });

  it('clears the notice after a successful send', async () => {
    mockAddFiles.mockResolvedValueOnce(accepting(0, overLimit('huge.png', 40 * 1024 * 1024)));
    render(<MessageInput onSendMessage={onSendMessage} />);
    attach(makeFile('huge.png', 40 * 1024 * 1024));
    // addFiles is async since #2157 PR 2 (it sniffs leading bytes), so the
    // partition lands a microtask later than the attach event.
    await act(async () => {});
    expect(document.querySelector('.attachment-notice')).not.toBeEmptyDOMElement();

    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'ship it' } });
    fireEvent.keyDown(textarea, { key: 'Enter' });

    await waitFor(() => {
      expect(document.querySelector('.attachment-notice')).toBeEmptyDOMElement();
    });
  });
});
