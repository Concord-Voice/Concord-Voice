import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, waitFor, act } from '../../../test-utils';
import Message from '@/renderer/components/Chat/Message';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockMember } from '../../../mocks/fixtures';
import type { MessageWithStatus } from '@/renderer/types/chat';

// Keep integration surface narrow — markdown pipeline is the subject under test.
vi.mock('@/renderer/components/Chat/AttachmentDisplay', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/Chat/GifEmbed', () => ({
  default: () => null,
}));

function makeMessage(content: string, editedAt: string | null = null): MessageWithStatus {
  return {
    id: 'm-1',
    channel_id: 'channel-1',
    content,
    user_id: mockMember.user_id,
    username: mockMember.username,
    display_name: mockMember.display_name,
    status: 'delivered',
    created_at: '2026-04-19T00:00:00Z',
    updated_at: editedAt ?? '2026-04-19T00:00:00Z',
    edited_at: editedAt ?? undefined,
  } as MessageWithStatus;
}

/**
 * Harness that mirrors production wiring: a parent component subscribes to
 * `useChatStore` and passes the current message object down to `<Message>`.
 * Any edit — sender-local or arriving via WS `message_update` — routes through
 * `chatStore.updateMessage`, which publishes a new message reference. The
 * subscription re-renders, the new prop flows in, and the memoized
 * `MarkdownContent` sees changed primitive fields → pipeline re-runs.
 */
function MessageHarness() {
  const message = useChatStore((s) => s.messagesByChannel.get('channel-1')?.[0]);
  if (!message) return null;
  return <Message message={message} currentUserId="user-2" showAvatar={true} />;
}

/**
 * Regression fence for #684 (closes #595/#596/#656): when an edit lands via either
 * the sender-local `updateMessage` call OR the WS `message_update` handler (both
 * route through `chatStore.updateMessage`), the new primitive fields flow as props
 * into the memoized `MarkdownContent`, its `propsEqual` comparator sees the diff,
 * and the pipeline re-parses the updated markdown.
 */
describe('Message edit propagation through Markdown pipeline', () => {
  beforeEach(() => {
    resetAllStores();
    useMemberStore.getState().addMember(mockMember);
    useChatStore.getState().addMessage('channel-1', makeMessage('**original**'));
  });

  it('updates rendered markdown on sender-path editMessage (local updateMessage)', async () => {
    render(<MessageHarness />);
    expect(screen.getByText('original').tagName).toBe('STRONG');

    act(() => {
      useChatStore.getState().updateMessage('channel-1', 'm-1', {
        content: '*updated*',
        edited_at: '2026-04-19T00:05:00Z',
        updated_at: '2026-04-19T00:05:00Z',
      });
    });

    await waitFor(() => {
      expect(screen.getByText('updated').tagName).toBe('EM');
    });
  });

  it('updates rendered markdown on WS message_update path (same updateMessage call)', async () => {
    render(<MessageHarness />);
    expect(screen.getByText('original').tagName).toBe('STRONG');

    // Simulates useWebSocketMessages.ts's on('message_update', ...) handler
    // which invokes the same `updateMessage` action that the sender path uses.
    act(() => {
      useChatStore.getState().updateMessage('channel-1', 'm-1', {
        content: '~~gone~~',
        edited_at: '2026-04-19T00:06:00Z',
        updated_at: '2026-04-19T00:06:00Z',
      });
    });

    await waitFor(() => {
      expect(screen.getByText('gone').tagName).toBe('DEL');
    });
  });

  it('keeps markdown stable when only edited_at changes (memo re-evaluates, output matches)', async () => {
    render(<MessageHarness />);
    expect(screen.getByText('original').tagName).toBe('STRONG');

    act(() => {
      useChatStore.getState().updateMessage('channel-1', 'm-1', {
        edited_at: '2026-04-19T00:10:00Z',
        updated_at: '2026-04-19T00:10:00Z',
      });
    });

    // edited_at change invalidates memo; content stays `**original**` → still STRONG.
    await waitFor(() => {
      expect(screen.getByText('original').tagName).toBe('STRONG');
    });
  });
});
