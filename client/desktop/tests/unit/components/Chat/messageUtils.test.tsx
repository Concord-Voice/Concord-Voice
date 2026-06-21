import { render } from '@testing-library/react';
import {
  getEmojiOnlyCount,
  getEmojiSizeClass,
  resolveMentionDisplay,
  renderEmoji,
  renderContent,
  type MentionLookup,
} from '@/renderer/components/Chat/messageUtils';

describe('messageUtils', () => {
  describe('getEmojiOnlyCount', () => {
    it('returns 0 for empty string', () => {
      expect(getEmojiOnlyCount('')).toBe(0);
    });

    it('returns 0 for null/undefined-ish input', () => {
      expect(getEmojiOnlyCount(null as unknown as string)).toBe(0);
    });

    it('returns 1 for a single emoji', () => {
      expect(getEmojiOnlyCount('😀')).toBe(1);
    });

    it('returns correct count for multiple emoji', () => {
      expect(getEmojiOnlyCount('😀😂🎉')).toBe(3);
    });

    it('returns 0 for mixed text and emoji', () => {
      expect(getEmojiOnlyCount('hello 😀')).toBe(0);
    });

    it('returns 0 for plain text', () => {
      expect(getEmojiOnlyCount('hello world')).toBe(0);
    });

    it('handles ZWJ sequences as single emoji', () => {
      // Family emoji (ZWJ sequence)
      expect(getEmojiOnlyCount('👨‍👩‍👧')).toBe(1);
    });

    it('handles flag emoji as single emoji', () => {
      expect(getEmojiOnlyCount('🇺🇸')).toBe(1);
    });

    it('handles skin tone modifiers as single emoji', () => {
      expect(getEmojiOnlyCount('👋🏽')).toBe(1);
    });

    it('counts emoji separated by whitespace', () => {
      expect(getEmojiOnlyCount('😀 😂')).toBe(2);
    });
  });

  describe('getEmojiSizeClass', () => {
    it('returns empty string for 0', () => {
      expect(getEmojiSizeClass(0)).toBe('');
    });

    it('returns empty string for negative', () => {
      expect(getEmojiSizeClass(-1)).toBe('');
    });

    it('returns emoji-jumbo-1 for 1', () => {
      expect(getEmojiSizeClass(1)).toBe('emoji-jumbo-1');
    });

    it('returns emoji-jumbo-2 for 2', () => {
      expect(getEmojiSizeClass(2)).toBe('emoji-jumbo-2');
    });

    it('returns emoji-jumbo-3 for 3', () => {
      expect(getEmojiSizeClass(3)).toBe('emoji-jumbo-3');
    });

    it('returns emoji-jumbo-4 for 4', () => {
      expect(getEmojiSizeClass(4)).toBe('emoji-jumbo-4');
    });

    it('returns emoji-jumbo-5 for 5', () => {
      expect(getEmojiSizeClass(5)).toBe('emoji-jumbo-5');
    });

    it('returns empty string for 6+', () => {
      expect(getEmojiSizeClass(6)).toBe('');
      expect(getEmojiSizeClass(10)).toBe('');
    });
  });

  describe('resolveMentionDisplay', () => {
    const lookup: MentionLookup = {
      users: new Map([['user-1', 'Test User']]),
      roles: new Map([['role-1', 'Admin']]),
    };

    it('resolves user token to display name', () => {
      expect(resolveMentionDisplay('<@user-1>', lookup)).toBe('@Test User');
    });

    it('resolves role token to role name', () => {
      expect(resolveMentionDisplay('<@&role-1>', lookup)).toBe('@Admin');
    });

    it('returns raw token for unresolved user', () => {
      expect(resolveMentionDisplay('<@unknown-id>', lookup)).toBe('<@unknown-id>');
    });

    it('returns raw token for unresolved role', () => {
      expect(resolveMentionDisplay('<@&unknown-role>', lookup)).toBe('<@&unknown-role>');
    });

    it('returns plain @username as-is', () => {
      expect(resolveMentionDisplay('@someuser', lookup)).toBe('@someuser');
    });

    it('returns @all as-is', () => {
      expect(resolveMentionDisplay('@all', lookup)).toBe('@all');
    });

    it('returns @here as-is', () => {
      expect(resolveMentionDisplay('@here', lookup)).toBe('@here');
    });
  });

  describe('renderEmoji', () => {
    it('returns plain text when no emoji present', () => {
      const result = renderEmoji('hello world');
      expect(result).toHaveLength(1);
      expect(result[0]).toBe('hello world');
    });

    it('wraps emoji in span elements', () => {
      const result = renderEmoji('😀');
      expect(result).toHaveLength(1);

      const { container } = render(<>{result}</>);
      const span = container.querySelector('.emoji');
      expect(span).not.toBeNull();
      expect(span!.textContent).toBe('😀');
    });

    it('handles text mixed with emoji', () => {
      const result = renderEmoji('hello 😀 world');
      // Should be: "hello ", <span>😀</span>, " world"
      expect(result.length).toBe(3);

      const { container } = render(<>{result}</>);
      expect(container.textContent).toBe('hello 😀 world');
      expect(container.querySelectorAll('.emoji')).toHaveLength(1);
    });

    it('returns empty array for empty string', () => {
      const result = renderEmoji('');
      expect(result).toHaveLength(0);
    });
  });

  describe('renderContent', () => {
    const lookup: MentionLookup = {
      users: new Map([['user-1', 'Test User']]),
      roles: new Map([['role-1', 'Admin']]),
    };

    const emptyLookup: MentionLookup = {
      users: new Map(),
      roles: new Map(),
    };

    it('renders plain text', () => {
      const result = renderContent('hello world', emptyLookup);
      const { container } = render(<>{result}</>);
      expect(container.textContent).toBe('hello world');
    });

    it('renders mention as highlighted span', () => {
      const result = renderContent('hello <@user-1>', lookup);
      const { container } = render(<>{result}</>);

      const mention = container.querySelector('.mention-highlight');
      expect(mention).not.toBeNull();
      expect(mention!.textContent).toBe('@Test User');
    });

    it('renders role mention as highlighted span', () => {
      const result = renderContent('check <@&role-1> please', lookup);
      const { container } = render(<>{result}</>);

      const mention = container.querySelector('.mention-highlight');
      expect(mention).not.toBeNull();
      expect(mention!.textContent).toBe('@Admin');
    });

    it('renders emoji within non-mention text', () => {
      const result = renderContent('hello 😀 <@user-1>', lookup);
      const { container } = render(<>{result}</>);

      expect(container.querySelector('.emoji')).not.toBeNull();
      expect(container.querySelector('.mention-highlight')).not.toBeNull();
    });

    it('handles text with no mentions as emoji-only rendering', () => {
      const result = renderContent('hello 😀 world', emptyLookup);
      const { container } = render(<>{result}</>);

      expect(container.textContent).toBe('hello 😀 world');
      expect(container.querySelector('.emoji')).not.toBeNull();
    });

    it('handles multiple mentions', () => {
      const result = renderContent('<@user-1> and <@&role-1>', lookup);
      const { container } = render(<>{result}</>);

      const mentions = container.querySelectorAll('.mention-highlight');
      expect(mentions).toHaveLength(2);
      expect(mentions[0].textContent).toBe('@Test User');
      expect(mentions[1].textContent).toBe('@Admin');
    });

    it('renders text after last mention', () => {
      const result = renderContent('<@user-1> is here', lookup);
      const { container } = render(<>{result}</>);

      expect(container.textContent).toBe('@Test User is here');
    });
  });
});
