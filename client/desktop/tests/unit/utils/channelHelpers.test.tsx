import React from 'react';
import { render } from '../../test-utils';
import {
  NAME_MIN,
  NAME_MAX,
  validateChannelName,
  getChannelTypeIcon,
} from '@/renderer/utils/channelHelpers';

describe('channelHelpers', () => {
  describe('constants', () => {
    // 1. NAME_MIN is 3 and NAME_MAX is 100
    it('NAME_MIN is 3', () => {
      expect(NAME_MIN).toBe(3);
    });

    it('NAME_MAX is 100', () => {
      expect(NAME_MAX).toBe(100);
    });
  });

  describe('validateChannelName', () => {
    // 2. Returns error for empty string
    it('returns error for empty string', () => {
      expect(validateChannelName('')).toBe('Channel name is required');
    });

    // 3. Returns error for whitespace-only
    it('returns error for whitespace-only string', () => {
      expect(validateChannelName('   ')).toBe('Channel name is required');
    });

    // 4. Returns error for too short (2 chars)
    it('returns error for name shorter than NAME_MIN', () => {
      expect(validateChannelName('ab')).toBe(
        `Channel name must be at least ${NAME_MIN} characters`
      );
    });

    // 5. Returns undefined for valid name (3+ chars)
    it('returns undefined for valid name at minimum length', () => {
      expect(validateChannelName('abc')).toBeUndefined();
    });

    it('returns undefined for typical valid name', () => {
      expect(validateChannelName('general-chat')).toBeUndefined();
    });

    // 6. Returns error for too long (101 chars)
    it('returns error for name longer than NAME_MAX', () => {
      const longName = 'a'.repeat(101);
      expect(validateChannelName(longName)).toBe(
        `Channel name must be at most ${NAME_MAX} characters`
      );
    });

    // 7. Returns undefined at max length (100 chars)
    it('returns undefined for name at exactly NAME_MAX length', () => {
      const maxName = 'a'.repeat(100);
      expect(validateChannelName(maxName)).toBeUndefined();
    });
  });

  describe('getChannelTypeIcon', () => {
    // 8. Returns Hash for 'text'
    it('returns Hash icon for text channel type', () => {
      const icon = getChannelTypeIcon('text');
      const { container } = render(<>{icon}</>);
      const svg = container.querySelector('svg');
      expect(svg).toBeInTheDocument();
      // lucide-react Hash icon
      expect(svg).toHaveClass('lucide-hash');
    });

    // 9. Returns Volume2 for 'voice'
    it('returns Volume2 icon for voice channel type', () => {
      const icon = getChannelTypeIcon('voice');
      const { container } = render(<>{icon}</>);
      const svg = container.querySelector('svg');
      expect(svg).toBeInTheDocument();
      expect(svg).toHaveClass('lucide-volume-2');
    });

    // 10. Returns Pin for 'bulletin'
    it('returns Pin icon for bulletin channel type', () => {
      const icon = getChannelTypeIcon('bulletin');
      const { container } = render(<>{icon}</>);
      const svg = container.querySelector('svg');
      expect(svg).toBeInTheDocument();
      expect(svg).toHaveClass('lucide-pin');
    });

    // 11. Returns Hash for unknown type (default case)
    it('returns Hash icon for unknown channel type', () => {
      const icon = getChannelTypeIcon('unknown' as 'text');
      const { container } = render(<>{icon}</>);
      const svg = container.querySelector('svg');
      expect(svg).toBeInTheDocument();
      expect(svg).toHaveClass('lucide-hash');
    });
  });
});
