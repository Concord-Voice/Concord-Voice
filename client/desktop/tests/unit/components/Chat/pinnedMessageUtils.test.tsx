import { render } from '../../../test-utils';
import { screen } from '@testing-library/react';
import { vi } from 'vitest';
import type { MessageWithUser } from '@/renderer/types/chat';

// Mock e2eeService before importing the module under test
const mockGetChannelKey = vi.fn();
const mockGetChannelKeyByVersion = vi.fn();
const mockDecryptWithKey = vi.fn();
const mockDecryptForChannel = vi.fn();
const mockDecryptForChannelWithVersion = vi.fn();

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
    getChannelKeyByVersion: (...args: unknown[]) => mockGetChannelKeyByVersion(...args),
    decryptWithKey: (...args: unknown[]) => mockDecryptWithKey(...args),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
  },
}));

// Mock the three shared render components to isolate PinContent's wiring from
// their internal complexity. Each mock surfaces enough markup for assertions.
vi.mock('@/renderer/components/Markdown/MarkdownContent', () => ({
  default: ({ content }: { content: string }) => (
    <div data-testid="markdown-content">{content}</div>
  ),
}));

vi.mock('@/renderer/components/Chat/GifEmbed', () => ({
  default: ({ slug }: { slug: string }) => <div data-testid="gif-embed">slug:{slug}</div>,
}));

vi.mock('@/renderer/components/Chat/AttachmentDisplay', () => ({
  default: ({
    attachments,
    channelId,
  }: {
    attachments: { id: string; filename: string }[];
    channelId: string;
  }) => (
    <div data-testid="attachment-display">
      <span data-testid="attachment-channel-id">{channelId}</span>
      {attachments.map((a) => (
        <span key={a.id}>{a.filename}</span>
      ))}
    </div>
  ),
}));

import {
  PinContent,
  decryptPins,
  type DecryptedPin,
} from '@/renderer/components/Chat/pinnedMessageUtils';

async function setE2EEInitialized(value: boolean) {
  const { e2eeService } = await import('@/renderer/services/e2eeService');
  Object.defineProperty(e2eeService, 'isInitialized', { value, writable: true });
}

const baseMsg: MessageWithUser = {
  id: 'msg-1',
  channel_id: 'ctx-1',
  user_id: 'user-1',
  content: 'hello',
  username: 'alice',
  display_name: 'Alice',
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
  pinned_at: '2025-01-01T01:00:00Z',
  pinned_by: 'user-1',
};

const decryptedTextPin: DecryptedPin = { ...baseMsg, decrypted: true };

// ---------------------------------------------------------------------------
// PinContent
// ---------------------------------------------------------------------------
describe('PinContent', () => {
  it('renders "Unable to decrypt" when decryptFailed is true', () => {
    const msg: DecryptedPin = { ...baseMsg, content: '', decryptFailed: true };
    render(<PinContent message={msg} />);
    expect(screen.getByText('Unable to decrypt')).toBeInTheDocument();
  });

  it('renders "Encrypted message" when not yet decrypted', () => {
    const msg: DecryptedPin = { ...baseMsg, content: 'ciphertext' };
    render(<PinContent message={msg} />);
    expect(screen.getByText('Encrypted message')).toBeInTheDocument();
  });

  it('decryptFailed takes priority over not-yet-decrypted', () => {
    const msg: DecryptedPin = { ...baseMsg, decryptFailed: true };
    render(<PinContent message={msg} />);
    expect(screen.getByText('Unable to decrypt')).toBeInTheDocument();
    expect(screen.queryByText('Encrypted message')).not.toBeInTheDocument();
  });

  it('renders text content via MarkdownContent', () => {
    render(<PinContent message={decryptedTextPin} />);
    const md = screen.getByTestId('markdown-content');
    expect(md).toHaveTextContent('hello');
  });

  it('renders GifEmbed when gif_slug is present', () => {
    const msg: DecryptedPin = { ...decryptedTextPin, content: '', gif_slug: 'happy-cat-7' };
    render(<PinContent message={msg} />);
    const gif = screen.getByTestId('gif-embed');
    expect(gif).toHaveTextContent('slug:happy-cat-7');
    expect(screen.queryByTestId('markdown-content')).not.toBeInTheDocument();
  });

  it('renders AttachmentDisplay when attachments are present', () => {
    const msg: DecryptedPin = {
      ...decryptedTextPin,
      content: '',
      attachments: [{ id: 'a-1', filename: 'photo.png', file_type: 'image/png', file_size: 1000 }],
    } as DecryptedPin;
    render(<PinContent message={msg} />);
    expect(screen.getByText('photo.png')).toBeInTheDocument();
  });

  it('passes the decrypted pin channel_id to AttachmentDisplay', () => {
    const msg: DecryptedPin = {
      ...decryptedTextPin,
      channel_id: 'conv-1',
      content: '',
      attachments: [{ id: 'a-1', filename: 'photo.png', file_type: 'image/png', file_size: 1000 }],
    } as DecryptedPin;
    render(<PinContent message={msg} />);
    expect(screen.getByTestId('attachment-channel-id')).toHaveTextContent('conv-1');
  });

  it('renders text + GIF + attachments together', () => {
    const msg: DecryptedPin = {
      ...decryptedTextPin,
      content: 'shared GIF + photo',
      gif_slug: 'wave-3',
      attachments: [{ id: 'a-2', filename: 'pic.jpg', file_type: 'image/jpeg', file_size: 2000 }],
    } as DecryptedPin;
    render(<PinContent message={msg} />);
    expect(screen.getByTestId('markdown-content')).toHaveTextContent('shared GIF + photo');
    expect(screen.getByTestId('gif-embed')).toHaveTextContent('slug:wave-3');
    expect(screen.getByText('pic.jpg')).toBeInTheDocument();
  });

  it('omits MarkdownContent when content is empty', () => {
    const msg: DecryptedPin = { ...decryptedTextPin, content: '' };
    render(<PinContent message={msg} />);
    expect(screen.queryByTestId('markdown-content')).not.toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// decryptPins
// ---------------------------------------------------------------------------
describe('decryptPins', () => {
  beforeEach(async () => {
    vi.clearAllMocks();
    await setE2EEInitialized(false);
  });

  it('passes messages through unchanged when e2ee not initialized', async () => {
    const msgs = [baseMsg, { ...baseMsg, id: 'msg-2' }];
    const result = await decryptPins('ctx-1', msgs);
    expect(result).toBe(msgs);
    expect(mockGetChannelKey).not.toHaveBeenCalled();
  });

  it('decrypts a message successfully', async () => {
    await setE2EEInitialized(true);
    const mockKey = {} as CryptoKey;
    mockGetChannelKey.mockResolvedValue(mockKey);
    mockDecryptWithKey.mockResolvedValue('decrypted text');

    const msg = { ...baseMsg, content: 'ciphertext' };
    const result = await decryptPins('ctx-1', [msg]);

    expect(result[0].content).toBe('decrypted text');
    expect(result[0].decrypted).toBe(true);
    expect(result[0].decryptFailed).toBeUndefined();
    expect(result[0].gif_slug).toBeUndefined();
  });

  it('uses the context id as channel_id for pinned rows without one', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey.mockResolvedValue('decrypted text');

    const msg = { ...baseMsg, channel_id: undefined as unknown as string, content: 'ciphertext' };
    const result = await decryptPins('conv-1', [msg]);

    expect(result[0].channel_id).toBe('conv-1');
  });

  it('applies unwrapGifEnvelope when plaintext is a GIF envelope', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey.mockResolvedValue('{"text":"hi friends","gif_slug":"wave-7"}');

    const msg = { ...baseMsg, content: 'ciphertext' };
    const result = await decryptPins('ctx-1', [msg]);

    expect(result[0].content).toBe('hi friends');
    expect(result[0].gif_slug).toBe('wave-7');
    expect(result[0].decrypted).toBe(true);
  });

  it('passes plain decrypted text through unchanged (no envelope)', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey.mockResolvedValue('plain markdown **text**');

    const msg = { ...baseMsg, content: 'ciphertext' };
    const result = await decryptPins('ctx-1', [msg]);

    expect(result[0].content).toBe('plain markdown **text**');
    expect(result[0].gif_slug).toBeUndefined();
  });

  it('sets decryptFailed when decryption throws', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey.mockRejectedValue(new Error('bad tag'));

    const msg = { ...baseMsg, content: 'ciphertext' };
    const result = await decryptPins('ctx-1', [msg]);

    expect(result[0].decryptFailed).toBe(true);
    expect(result[0].content).toBe('');
    expect(result[0].decrypted).toBeUndefined();
  });

  it('falls back to decryptForChannel when key fetch fails', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockRejectedValue(new Error('no key'));
    mockDecryptForChannel.mockResolvedValue('fallback plaintext');

    const msg = { ...baseMsg, content: 'ciphertext' };
    const result = await decryptPins('ctx-1', [msg]);

    expect(result[0].content).toBe('fallback plaintext');
    expect(result[0].decrypted).toBe(true);
    expect(mockDecryptForChannel).toHaveBeenCalledWith('ctx-1', 'ciphertext');
  });

  it('uses versioned key for messages with key_version > 1', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    const vKey = {} as CryptoKey;
    mockGetChannelKeyByVersion.mockResolvedValue(vKey);
    mockDecryptWithKey.mockResolvedValue('versioned plaintext');

    const versioned = { ...baseMsg, content: 'ct-v3', key_version: 3 };
    const result = await decryptPins('ctx-1', [versioned]);

    expect(result[0].content).toBe('versioned plaintext');
    expect(mockGetChannelKeyByVersion).toHaveBeenCalledWith('ctx-1', 3);
    expect(mockDecryptWithKey).toHaveBeenCalledWith('ct-v3', vKey);
  });

  it('falls back to decryptForChannelWithVersion when versioned key fetch fails', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockGetChannelKeyByVersion.mockRejectedValue(new Error('no versioned key'));
    mockDecryptForChannelWithVersion.mockResolvedValue('version fallback');

    const versioned = { ...baseMsg, content: 'ct-v2', key_version: 2 };
    const result = await decryptPins('ctx-1', [versioned]);

    expect(result[0].content).toBe('version fallback');
    expect(mockDecryptForChannelWithVersion).toHaveBeenCalledWith('ctx-1', 'ct-v2', 2);
  });
});
