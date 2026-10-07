import {
  canEditMessage,
  latestEditableOwnMessage,
} from '@/renderer/utils/chat/latestEditableOwnMessage';
import type { MessageWithStatus } from '@/renderer/types/chat';
import type { AttachmentSummary } from '@/renderer/types/chat';
import { mockMessage } from '../../../mocks/fixtures';

const ME = 'user-me';
const OTHER = 'user-other';

const attachment: AttachmentSummary = {
  id: 'att-1',
  file_type: 'file',
  mime_type: 'application/pdf',
  file_size: 1024,
};

let seq = 0;
function row(overrides: Partial<MessageWithStatus> & { id: string }): MessageWithStatus {
  seq += 1;
  return {
    ...mockMessage,
    user_id: ME,
    status: 'delivered',
    created_at: `2025-01-01T12:${String(seq % 60).padStart(2, '0')}:00Z`,
    ...overrides,
  };
}

describe('canEditMessage (#1959)', () => {
  beforeEach(() => {
    seq = 0;
  });

  it.each<[string, Partial<MessageWithStatus>, string, boolean]>([
    ['own delivered text message', {}, ME, true],
    ['own message with an absent status', { status: undefined }, ME, true],
    ['own message with type "user"', { type: 'user' }, ME, true],
    ['own message with an absent type', { type: undefined }, ME, true],
    ['message by another author', { user_id: OTHER }, ME, false],
    ['empty current user id, even on an orphan row', { user_id: '' }, '', false],
    ['own call_event system row', { type: 'call_event' }, ME, false],
    ['own expiration_event system row', { type: 'expiration_event' }, ME, false],
    ['own pending message', { status: 'pending' }, ME, false],
    ['own sent (unacknowledged) message', { status: 'sent' }, ME, false],
    ['own failed message', { status: 'failed' }, ME, false],
    ['own message that failed to decrypt', { decryptFailed: true }, ME, false],
    ['own message awaiting keys', { pendingKeys: true }, ME, false],
    [
      'the #1741 fail-closed decrypt placeholder (empty content, nothing else)',
      {
        content: '',
        decryptFailed: false,
        pendingKeys: false,
        status: undefined,
        attachments: undefined,
        gif_slug: undefined,
      },
      ME,
      false,
    ],
    [
      'the same placeholder shape with an empty attachments list',
      { content: '', attachments: [] },
      ME,
      false,
    ],
    ['empty text with an attachment', { content: '', attachments: [attachment] }, ME, true],
    ['empty text with a gif slug', { content: '', gif_slug: 'happy-cat-7' }, ME, true],
  ])('%s -> %s', (_label, overrides, currentUserId, expected) => {
    expect(canEditMessage(row({ id: 'm', ...overrides }), currentUserId)).toBe(expected);
  });
});

describe('latestEditableOwnMessage (#1959)', () => {
  beforeEach(() => {
    seq = 0;
  });

  it('returns the newest own delivered row over older own rows (#1959)', () => {
    const messages = [row({ id: 'own-1' }), row({ id: 'own-2' }), row({ id: 'own-3' })];
    expect(latestEditableOwnMessage(messages, ME)).toBe('own-3');
  });

  it('ignores other users LATER rows and returns the newest OWN row (#1959)', () => {
    const messages = [
      row({ id: 'own-1' }),
      row({ id: 'own-2' }),
      row({ id: 'other-1', user_id: OTHER }),
      row({ id: 'other-2', user_id: OTHER }),
    ];
    expect(latestEditableOwnMessage(messages, ME)).toBe('own-2');
  });

  it('treats an absent status as delivered (#1959)', () => {
    const messages = [row({ id: 'own-1', status: undefined })];
    expect(latestEditableOwnMessage(messages, ME)).toBe('own-1');
  });

  it('treats type undefined as a user row (#1959)', () => {
    const messages = [row({ id: 'own-1', type: undefined })];
    expect(latestEditableOwnMessage(messages, ME)).toBe('own-1');
  });

  it('treats type "user" as a user row (#1959)', () => {
    const messages = [row({ id: 'own-1', type: 'user' })];
    expect(latestEditableOwnMessage(messages, ME)).toBe('own-1');
  });

  describe('newest own row not editable returns null and never falls back to an older editable row (#1959)', () => {
    const older = () => row({ id: 'older-editable' });

    it('positive control: the older row alone is returned', () => {
      expect(latestEditableOwnMessage([older()], ME)).toBe('older-editable');
    });

    it.each(['pending', 'sent', 'failed'] as const)(
      'newest own row with status %s returns null',
      (status) => {
        const messages = [older(), row({ id: 'newest', status })];
        expect(latestEditableOwnMessage(messages, ME)).toBeNull();
      }
    );

    it('newest own row with decryptFailed returns null', () => {
      const messages = [older(), row({ id: 'newest', decryptFailed: true })];
      expect(latestEditableOwnMessage(messages, ME)).toBeNull();
    });

    it('newest own row with pendingKeys returns null', () => {
      const messages = [older(), row({ id: 'newest', pendingKeys: true })];
      expect(latestEditableOwnMessage(messages, ME)).toBeNull();
    });

    it('newest own row that is the empty decrypt placeholder returns null, not the older delivered row', () => {
      const placeholder = row({
        id: 'newest',
        content: '',
        decryptFailed: false,
        pendingKeys: false,
        status: undefined,
      });
      expect(canEditMessage(placeholder, ME)).toBe(false);
      expect(latestEditableOwnMessage([older()], ME)).toBe('older-editable');

      expect(latestEditableOwnMessage([older(), placeholder], ME)).toBeNull();
    });

    it('a non-editable newest own row still wins over an other user later row (#1959)', () => {
      const messages = [
        older(),
        row({ id: 'newest', status: 'pending' }),
        row({ id: 'other-1', user_id: OTHER }),
      ];
      expect(latestEditableOwnMessage(messages, ME)).toBeNull();
    });
  });

  describe('system rows', () => {
    it('skips a newest own call_event row and returns the previous own user row (#1959)', () => {
      const messages = [
        row({ id: 'own-user' }),
        row({ id: 'own-call', type: 'call_event', content: '' }),
      ];
      expect(latestEditableOwnMessage(messages, ME)).toBe('own-user');
    });

    it('skips an own expiration-event row too (#1959)', () => {
      const messages = [
        row({ id: 'own-user' }),
        row({ id: 'own-expiry', type: 'expiration_event', content: '' }),
      ];
      expect(latestEditableOwnMessage(messages, ME)).toBe('own-user');
    });

    it('returns null when the only own rows are system rows (#1959)', () => {
      const messages = [row({ id: 'own-call', type: 'call_event', content: '' })];
      expect(latestEditableOwnMessage(messages, ME)).toBeNull();
    });
  });

  it('returns null when the user has no rows (#1959)', () => {
    const messages = [row({ id: 'other-1', user_id: OTHER })];
    expect(latestEditableOwnMessage(messages, ME)).toBeNull();
  });

  it('returns null for an empty message list (#1959)', () => {
    expect(latestEditableOwnMessage([], ME)).toBeNull();
  });

  it('returns null for an empty current user id even when rows have an empty user_id (#1959)', () => {
    expect(latestEditableOwnMessage([row({ id: 'own-1' })], ME)).toBe('own-1');
    const messages = [row({ id: 'orphan', user_id: '' })];
    expect(latestEditableOwnMessage(messages, '')).toBeNull();
  });

  it('does not mutate the input array or its rows (#1959)', () => {
    const messages = [row({ id: 'own-1' }), row({ id: 'other-1', user_id: OTHER })];
    const snapshot = structuredClone(messages);
    const refs = [...messages];

    expect(latestEditableOwnMessage(messages, ME)).toBe('own-1');

    expect(messages).toEqual(snapshot);
    expect(messages).toHaveLength(refs.length);
    messages.forEach((m, i) => expect(m).toBe(refs[i]));
  });
});
