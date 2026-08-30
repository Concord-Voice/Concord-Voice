import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render } from '../../../test-utils';
import Message from '@/renderer/components/Chat/Message';
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockMember } from '../../../mocks/fixtures';
import { indexCategory } from '@/renderer/components/EmojiPicker/shortcodeIndex';
import smileys from '@/renderer/data/emoji/smileys.json';
import type { MessageWithStatus } from '@/renderer/types/chat';

// Keep the integration surface narrow — the jumbo-sizing decision is the subject.
vi.mock('@/renderer/components/Chat/AttachmentDisplay', () => ({ default: () => null }));
vi.mock('@/renderer/components/Chat/GifEmbed', () => ({ default: () => null }));

function makeMessage(content: string): MessageWithStatus {
  return {
    id: 'm-1',
    channel_id: 'channel-1',
    content,
    user_id: mockMember.user_id,
    username: mockMember.username,
    display_name: mockMember.display_name,
    status: 'delivered',
    created_at: '2026-07-05T00:00:00Z',
    updated_at: '2026-07-05T00:00:00Z',
  } as MessageWithStatus;
}

/**
 * Regression fence for #2070: an emoji-only message written as a `:shortcode:`
 * must get the SAME jumbo scaling as the equivalent literal emoji (parity with
 * picker-inserted emoji). Before the fix the jumbo decision ran on raw
 * pre-expansion content, so `:smile:` counted as 0 emoji → no `emoji-jumbo-N`
 * class → it rendered at inline text size while a literal 😄 rendered jumbo.
 */
describe('Message :shortcode: jumbo scaling (#2070)', () => {
  beforeEach(() => {
    resetAllStores();
    // Idempotent: the module seeds smileys at import; assert :smile: resolves.
    indexCategory('smileys', smileys);
    useMemberStore.getState().addMember(mockMember);
  });

  it('renders a :smile:-only message at emoji-jumbo-1 (parity with a literal 😄)', () => {
    const { container } = render(
      <Message message={makeMessage(':smile:')} currentUserId="user-2" showAvatar={true} />
    );
    const messageText = container.querySelector('.message-text');
    // The jumbo class is the discriminator: absent before the fix, present after.
    expect(messageText?.className).toContain('emoji-jumbo-1');
    const emoji = container.querySelector('.message-text .emoji');
    expect(emoji?.textContent).toBe('😄'); // expanded glyph, never the literal ":smile:"
  });

  it('renders a literal 😄-only message at emoji-jumbo-1 (baseline both paths must match)', () => {
    const { container } = render(
      <Message message={makeMessage('😄')} currentUserId="user-2" showAvatar={true} />
    );
    const messageText = container.querySelector('.message-text');
    expect(messageText?.className).toContain('emoji-jumbo-1');
  });

  it('renders :smile::smile: at emoji-jumbo-2 (count scales with expanded emoji)', () => {
    const { container } = render(
      <Message message={makeMessage(':smile::smile:')} currentUserId="user-2" showAvatar={true} />
    );
    expect(container.querySelector('.message-text')?.className).toContain('emoji-jumbo-2');
  });

  it('does NOT jumbo-scale mixed ":smile: hi" (still normal inline text)', () => {
    const { container } = render(
      <Message message={makeMessage(':smile: hi')} currentUserId="user-2" showAvatar={true} />
    );
    expect(container.querySelector('.message-text')?.className).not.toContain('emoji-jumbo');
  });

  it('preserves the code-span guard: `:smile:` renders literal, no expansion, no jumbo', () => {
    const { container } = render(
      <Message message={makeMessage('`:smile:`')} currentUserId="user-2" showAvatar={true} />
    );
    const messageText = container.querySelector('.message-text');
    expect(messageText?.className).not.toContain('emoji-jumbo');
    expect(messageText?.textContent).toContain(':smile:'); // literal, not the 😄 glyph
  });

  it('preserves the indented-code-block guard: "    :smile:" stays literal, no jumbo', () => {
    // A 4-space indent is a markdown code block; remarkEmojiShortcodes skips it,
    // so markdown renders literal ":smile:". The jumbo decision must agree and
    // NOT expand it into a jumbo 😄 (the divergence #2070 set out to eliminate).
    const { container } = render(
      <Message message={makeMessage('    :smile:')} currentUserId="user-2" showAvatar={true} />
    );
    const messageText = container.querySelector('.message-text');
    expect(messageText?.className).not.toContain('emoji-jumbo');
    expect(messageText?.textContent).toContain(':smile:'); // literal, not the 😄 glyph
    expect(container.querySelector('.message-text code')).not.toBeNull(); // rendered as code
  });

  it('does NOT jumbo-scale an unknown shortcode (:definitelynotareal:)', () => {
    const { container } = render(
      <Message
        message={makeMessage(':definitelynotareal:')}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(container.querySelector('.message-text')?.className).not.toContain('emoji-jumbo');
  });

  // The indented-code guard also corrects the PREEXISTING literal-emoji case
  // (`    😄` is a code block markdown renders literally, not a jumbo emoji).
  it('does NOT jumbo-scale an indented (code-block) literal "    😄"', () => {
    const { container } = render(
      <Message message={makeMessage('    😄')} currentUserId="user-2" showAvatar={true} />
    );
    expect(container.querySelector('.message-text')?.className).not.toContain('emoji-jumbo');
  });

  // code-reviewer note (PR #2073): fence the intended multi-line collapse — a
  // shortcode-only message across lines counts as emoji-only (whitespace is
  // stripped), matching the preexisting literal `😄\n😄` jumbo behavior.
  it('jumbo-scales a multi-line shortcode-only message ":smile:\\n:smile:" as jumbo-2', () => {
    const { container } = render(
      <Message message={makeMessage(':smile:\n:smile:')} currentUserId="user-2" showAvatar={true} />
    );
    expect(container.querySelector('.message-text')?.className).toContain('emoji-jumbo-2');
  });
});
