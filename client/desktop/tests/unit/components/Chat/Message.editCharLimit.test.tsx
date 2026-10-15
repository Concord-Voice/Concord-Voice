import { vi } from 'vitest';
import { act, within } from '@testing-library/react';
import { render, screen, fireEvent } from '../../../test-utils';
import Message from '@/renderer/components/Chat/Message';
import { mockMessage, mockMember } from '../../../mocks/fixtures';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { useSubscriptionStore, FREE_ENTITLEMENT } from '@/renderer/stores/auth/subscriptionStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// The inline edit box holds an edit to the per-message character limit (#3857). The composer
// can turn an over-limit message into a .md attachment; an edit has no such path, so an
// over-limit edit is refused and kept open. The limit comes from the live entitlement, so these
// cases drive the real subscription store rather than a stub: the limit is read, not passed.

const FREE = 5120;
const PREMIUM = 10240;
const REASON_FREE = 'This message is over your 5,120-character limit. Shorten it to save.';

const setTier = (tier: 'free' | 'premium') =>
  act(() => {
    useSubscriptionStore
      .getState()
      .setEntitlement(
        tier === 'premium'
          ? { ...FREE_ENTITLEMENT, tier: 'premium', maxMessageChars: PREMIUM }
          : FREE_ENTITLEMENT
      );
  });

const body = (n: number) => 'a'.repeat(n);

function renderEdit(content = 'Original text', onEdit = vi.fn()) {
  const message = { ...mockMessage, content };
  render(
    <Message
      surfaceId="s1"
      message={message}
      currentUserId="user-1"
      onEdit={onEdit}
      showAvatar={true}
    />
  );
  act(() => useChatStore.getState().setEditingMessage('s1', message.id));
  const textarea = screen.getByRole('textbox', { name: 'Edit message' }) as HTMLTextAreaElement;
  return { onEdit, textarea };
}

const draft = (textarea: HTMLTextAreaElement, value: string) =>
  fireEvent.change(textarea, { target: { value } });

const saveButton = () => screen.getByRole('button', { name: 'Save' });
const reason = () => screen.queryByText(/character limit\. Shorten it to save\./);
const counter = () => document.querySelector('.message-edit-counter');

describe('Message edit box: per-message character limit (#3857)', () => {
  beforeEach(() => {
    resetAllStores();
    useSubscriptionStore.getState().reset(); // free floor: 5120
    useMemberStore.getState().addMember(mockMember);
  });

  describe('under the limit', () => {
    it('saves with Enter and shows no reason', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, body(100));

      expect(saveButton()).toBeEnabled();
      expect(reason()).not.toBeInTheDocument();
      expect(textarea).not.toHaveAttribute('aria-describedby');

      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).toHaveBeenCalledWith('msg-1', body(100));
    });

    it('saves with the Save button', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, 'A shorter, changed edit');

      fireEvent.click(saveButton());
      expect(onEdit).toHaveBeenCalledWith('msg-1', 'A shorter, changed edit');
    });
  });

  describe('exactly AT the limit (the boundary is inclusive)', () => {
    it('saves with Enter', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, body(FREE));

      expect(saveButton()).toBeEnabled();
      expect(reason()).not.toBeInTheDocument();
      fireEvent.keyDown(textarea, { key: 'Enter' });

      expect(onEdit).toHaveBeenCalledTimes(1);
      expect(onEdit).toHaveBeenCalledWith('msg-1', body(FREE));
    });

    it('saves with the Save button, and the counter reads n/max without the error level', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, body(FREE));

      expect(counter()).toHaveTextContent(`${FREE}/${FREE}`);
      expect(counter()).not.toHaveClass('message-edit-counter-error');
      fireEvent.click(saveButton());
      expect(onEdit).toHaveBeenCalledWith('msg-1', body(FREE));
    });
  });

  describe('one over the limit', () => {
    const over = body(FREE + 1);

    it('disables Save, so the button cannot save either', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, over);

      expect(saveButton()).toBeDisabled();
      fireEvent.click(saveButton());
      expect(onEdit).not.toHaveBeenCalled();
    });

    it('does not save on Enter, and keeps the box open with the text intact', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, over);
      // Gate: the draft under test is in the box and owns focus.
      expect(textarea.value).toHaveLength(FREE + 1);
      expect(textarea).toHaveFocus();

      fireEvent.keyDown(textarea, { key: 'Enter' });

      expect(onEdit).not.toHaveBeenCalled();
      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: 's1',
        messageId: 'msg-1',
      });
      const stillThere = screen.getByRole('textbox', { name: 'Edit message' });
      expect(stillThere).toBe(textarea);
      expect((stillThere as HTMLTextAreaElement).value).toBe(over); // never truncated
      expect(stillThere).toHaveFocus();
    });

    it('states the reason with the actual limit, wired to the textarea by aria-describedby', () => {
      const { textarea } = renderEdit();
      draft(textarea, over);

      const message = screen.getByText(REASON_FREE);
      expect(message.id).not.toBe('');
      expect(textarea).toHaveAttribute('aria-describedby', message.id);
      expect(textarea).toHaveAccessibleDescription(REASON_FREE);
    });

    it('shows the counter at the error level', () => {
      const { textarea } = renderEdit();
      draft(textarea, over);

      expect(counter()).toHaveTextContent(`${FREE + 1}/${FREE}`);
      expect(counter()).toHaveClass('message-edit-counter-error');
    });

    it('lets the user shorten it back to the limit and then saves', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, over);
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).not.toHaveBeenCalled();

      draft(textarea, body(FREE));
      expect(reason()).not.toBeInTheDocument();
      expect(textarea).not.toHaveAttribute('aria-describedby');
      expect(saveButton()).toBeEnabled();

      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).toHaveBeenCalledWith('msg-1', body(FREE));
    });

    it('still lets the user cancel with Escape, without saving', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, over);
      fireEvent.keyDown(textarea, { key: 'Escape' });
      expect(useChatStore.getState().editingMessage).toBeNull();
      expect(onEdit).not.toHaveBeenCalled();
    });
  });

  describe('keys and input bounds', () => {
    it('Escape cancels an in-limit edit rather than saving it', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, 'A changed edit');
      fireEvent.keyDown(textarea, { key: 'Escape' });
      expect(useChatStore.getState().editingMessage).toBeNull();
      expect(onEdit).not.toHaveBeenCalled();
    });

    it('Shift+Enter does not save', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, 'A changed edit');
      fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: true });
      expect(onEdit).not.toHaveBeenCalled();
    });

    it('discards input that would take the draft past the 1 MiB renderer ceiling', () => {
      const { textarea } = renderEdit();
      draft(textarea, body(1_048_577));
      expect(textarea.value).toBe('Original text');
      draft(textarea, body(1_048_576));
      expect(textarea.value).toHaveLength(1_048_576);
    });

    it('keeps the reason live region mounted while under the limit, so its text is announced', () => {
      const { textarea } = renderEdit();
      const box = document.querySelector('.message-edit-box') as HTMLElement;
      const region = within(box).getByRole('status');
      expect(region).toBeEmptyDOMElement();

      draft(textarea, body(FREE + 1));
      expect(within(box).getByRole('status')).toBe(region);
      expect(region).toHaveTextContent(REASON_FREE);
    });
  });

  describe('trimmed versus untrimmed length', () => {
    it('saves a draft whose trailing whitespace pushes the raw length over but whose trimmed length is at the limit', () => {
      const { onEdit, textarea } = renderEdit();
      const padded = `${body(FREE)}   \n\t `;
      draft(textarea, padded);
      // Gate: the raw draft really is over the limit.
      expect(textarea.value.length).toBeGreaterThan(FREE);

      expect(saveButton()).toBeEnabled();
      expect(reason()).not.toBeInTheDocument();
      fireEvent.keyDown(textarea, { key: 'Enter' });

      // The save sends the trimmed text: exactly what the guard measured.
      expect(onEdit).toHaveBeenCalledWith('msg-1', body(FREE));
    });

    it('does not count whitespace toward the counter', () => {
      const { textarea } = renderEdit();
      draft(textarea, `${body(3839)}${' '.repeat(50)}`);
      expect(counter()).not.toBeInTheDocument();
    });

    it('refuses when the trimmed text is over, even if padding is irrelevant', () => {
      const { onEdit, textarea } = renderEdit();
      draft(textarea, `  ${body(FREE + 1)}  `);

      expect(saveButton()).toBeDisabled();
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).not.toHaveBeenCalled();
      expect(screen.getByText(REASON_FREE)).toBeInTheDocument();
    });

    it('keeps Save disabled when the only change is trailing whitespace', () => {
      // Before #3857 the button compared the RAW draft with the original and enabled Save for
      // a change the save (which trims) then silently dropped.
      const { onEdit, textarea } = renderEdit('Original text');
      draft(textarea, 'Original text   ');

      expect(saveButton()).toBeDisabled();
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).not.toHaveBeenCalled();
    });

    it('keeps Save disabled for a whitespace-only draft', () => {
      const { textarea } = renderEdit();
      draft(textarea, '   \n  ');
      expect(saveButton()).toBeDisabled();
    });
  });

  describe('the counter appears at 75% of the limit', () => {
    it('is absent just below 75% and shows n/max at 75% (free)', () => {
      const { textarea } = renderEdit();
      draft(textarea, body(3839));
      expect(counter()).not.toBeInTheDocument();

      draft(textarea, body(3840));
      expect(counter()).toHaveTextContent('3840/5120');
      expect(counter()).toHaveClass('message-edit-counter-normal');
    });

    it('warns from 95% of the limit', () => {
      const { textarea } = renderEdit();
      draft(textarea, body(4864));
      expect(counter()).toHaveClass('message-edit-counter-warn');
    });

    it('scales to the premium limit', () => {
      setTier('premium');
      const { textarea } = renderEdit();
      draft(textarea, body(5000));
      // 5000 is past 75% of the FREE limit and under 75% of the premium one.
      expect(counter()).not.toBeInTheDocument();

      draft(textarea, body(7680));
      expect(counter()).toHaveTextContent('7680/10240');
    });

    it('is absent for a short edit', () => {
      const { textarea } = renderEdit();
      draft(textarea, 'short');
      expect(counter()).not.toBeInTheDocument();
    });
  });

  describe('a user who dropped from premium to free', () => {
    const oldLong = body(6000); // saved under the premium limit

    it('opens an old message over the free limit already refused, with the reason', () => {
      const { onEdit, textarea } = renderEdit(oldLong);

      expect(textarea.value).toBe(oldLong);
      expect(screen.getByText(REASON_FREE)).toBeInTheDocument();
      expect(textarea).toHaveAccessibleDescription(REASON_FREE);
      expect(saveButton()).toBeDisabled();
      expect(counter()).toHaveTextContent('6000/5120');

      // A small edit that leaves it over the limit is still refused, and keeps its text.
      draft(textarea, `${oldLong}!`);
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).not.toHaveBeenCalled();
      expect(textarea.value).toBe(`${oldLong}!`);
    });

    it('saves once the message is shortened to the free limit', () => {
      const { onEdit, textarea } = renderEdit(oldLong);
      draft(textarea, oldLong.slice(0, FREE));

      expect(reason()).not.toBeInTheDocument();
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).toHaveBeenCalledWith('msg-1', oldLong.slice(0, FREE));
    });

    it('is not refused while the tier is still premium', () => {
      setTier('premium');
      const { onEdit, textarea } = renderEdit(oldLong);
      draft(textarea, `${oldLong} and a little more`);

      expect(reason()).not.toBeInTheDocument();
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).toHaveBeenCalledWith('msg-1', `${oldLong} and a little more`);
    });

    it('applies a downgrade that lands while the box is open, without losing the draft', () => {
      setTier('premium');
      const { onEdit, textarea } = renderEdit(oldLong);
      draft(textarea, `${oldLong} edited`);
      expect(saveButton()).toBeEnabled();
      expect(reason()).not.toBeInTheDocument();

      setTier('free');

      expect(saveButton()).toBeDisabled();
      expect(screen.getByText(REASON_FREE)).toBeInTheDocument();
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).not.toHaveBeenCalled();
      expect(textarea.value).toBe(`${oldLong} edited`);
    });
  });

  describe('premium limit', () => {
    it('names the premium number in the reason and holds the edit to it', () => {
      setTier('premium');
      const { onEdit, textarea } = renderEdit();
      draft(textarea, body(PREMIUM + 1));

      expect(
        screen.getByText('This message is over your 10,240-character limit. Shorten it to save.')
      ).toBeInTheDocument();
      expect(screen.queryByText(REASON_FREE)).not.toBeInTheDocument();
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).not.toHaveBeenCalled();

      draft(textarea, body(PREMIUM));
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).toHaveBeenCalledWith('msg-1', body(PREMIUM));
    });
  });
});
