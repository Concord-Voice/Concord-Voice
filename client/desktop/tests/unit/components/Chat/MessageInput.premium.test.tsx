import { vi } from 'vitest';
import React from 'react';

// ─── Mocks (before component imports) ───────────────────────────────────────

vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({ default: () => null }));
vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => <div data-testid="user-panel" />,
}));
vi.mock('@/renderer/stores/layoutStore', () => ({ useLayoutStore: () => false }));

const mockAddFiles = vi.fn().mockReturnValue(null);
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

// Entitlement: FREE floor by default (maxMessageChars 5120, maxAttachmentBytes 25 MiB).
const entitlementOverrides: Record<string, unknown> = {};
function freeEntitlement() {
  return {
    tier: 'free',
    maxMessageChars: 5120,
    maxAttachmentBytes: 26_214_400,
    ...entitlementOverrides,
  };
}
vi.mock('@/renderer/hooks/useEntitlement', () => ({
  useEntitlement: vi.fn((selector: (e: Record<string, unknown>) => unknown) =>
    selector(freeEntitlement())
  ),
}));

// ─── Imports (after mocks) ──────────────────────────────────────────────────

import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import MessageInput from '@/renderer/components/Chat/MessageInput';

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

describe('MessageInput — L9 attachment-size upsell', () => {
  it('shows the non-modal banner with correct sizes for an over-limit file', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    const big = makeFile('huge.png', 40 * 1024 * 1024); // 40 MiB > 25 MiB free
    fireEvent.change(input, { target: { files: [big] } });
    const banner = document.querySelector('.attachment-upsell-banner') as HTMLElement;
    expect(banner).toBeInTheDocument();
    expect(banner.textContent).toContain('huge.png is');
    expect(banner.textContent).toContain('Free limit');
    expect(banner.textContent).toContain('Premium raises it to');
  });

  it('does NOT block — the file still flows to addFiles', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    const big = makeFile('huge.png', 40 * 1024 * 1024);
    fireEvent.change(input, { target: { files: [big] } });
    expect(mockAddFiles).toHaveBeenCalled();
  });

  it('no banner for a within-limit file', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    const small = makeFile('ok.png', 1 * 1024 * 1024); // 1 MiB
    fireEvent.change(input, { target: { files: [small] } });
    expect(document.querySelector('.attachment-upsell-banner')).not.toBeInTheDocument();
  });

  it('the banner is dismissible', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [makeFile('huge.png', 40 * 1024 * 1024)] } });
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(document.querySelector('.attachment-upsell-banner')).not.toBeInTheDocument();
  });

  it('entitled (premium attachment cap): no banner for a file within the higher cap', () => {
    setEntitlement({ tier: 'premium', maxAttachmentBytes: 100 * 1024 * 1024 });
    render(<MessageInput onSendMessage={onSendMessage} />);
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [makeFile('huge.png', 40 * 1024 * 1024)] } });
    expect(document.querySelector('.attachment-upsell-banner')).not.toBeInTheDocument();
  });

  it('labels an over-premium file with the current limit, not free-tier upsell copy', () => {
    setEntitlement({ tier: 'premium', maxAttachmentBytes: 100 * 1024 * 1024 });
    render(<MessageInput onSendMessage={onSendMessage} />);
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [makeFile('huge.png', 120 * 1024 * 1024)] } });
    const banner = document.querySelector('.attachment-upsell-banner') as HTMLElement;
    expect(banner.textContent).toContain('Current limit');
    expect(banner.textContent).not.toContain('Free limit');
    expect(banner.textContent).not.toContain('Premium raises it to');
  });

  it('clears the attachment upsell after the offending file is removed', () => {
    const oversizedFile = makeFile('huge.png', 40 * 1024 * 1024);
    mockUploadFiles = [{ file: oversizedFile, progress: 0, status: 'pending' }];
    render(<MessageInput onSendMessage={onSendMessage} />);
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [oversizedFile] } });
    expect(document.querySelector('.attachment-upsell-banner')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Remove upload' }));

    expect(mockRemoveFile).toHaveBeenCalledWith(0);
    expect(document.querySelector('.attachment-upsell-banner')).not.toBeInTheDocument();
  });

  it('clears the attachment upsell after a successful send', async () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [makeFile('huge.png', 40 * 1024 * 1024)] } });
    expect(document.querySelector('.attachment-upsell-banner')).toBeInTheDocument();

    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'ship it' } });
    fireEvent.keyDown(textarea, { key: 'Enter' });

    await waitFor(() => {
      expect(document.querySelector('.attachment-upsell-banner')).not.toBeInTheDocument();
    });
  });
});
