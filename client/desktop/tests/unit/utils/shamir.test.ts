// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { split, combine } from '@/renderer/utils/shamir';

describe('Shamir Secret Sharing', () => {
  describe('split + combine round-trips', () => {
    it('reconstructs secret from all shares', () => {
      const secret = crypto.getRandomValues(new Uint8Array(32));
      const shares = split(secret, 5, 3);
      expect(shares.length).toBe(5);

      const reconstructed = combine(shares);
      expect(reconstructed).toEqual(secret);
    });

    it('reconstructs secret from exactly k shares', () => {
      const secret = crypto.getRandomValues(new Uint8Array(64));
      const shares = split(secret, 5, 3);

      // Use shares 0, 2, 4 (non-consecutive to test index handling)
      const subset = [shares[0], shares[2], shares[4]];
      const reconstructed = combine(subset);
      expect(reconstructed).toEqual(secret);
    });

    it('works with different subsets of k shares', () => {
      const secret = crypto.getRandomValues(new Uint8Array(16));
      const shares = split(secret, 5, 3);

      // Every combination of 3 shares should reconstruct correctly
      const combos = [
        [0, 1, 2],
        [0, 1, 3],
        [0, 1, 4],
        [0, 2, 3],
        [0, 2, 4],
        [0, 3, 4],
        [1, 2, 3],
        [1, 2, 4],
        [1, 3, 4],
        [2, 3, 4],
      ];
      for (const combo of combos) {
        const subset = combo.map((i) => shares[i]);
        const reconstructed = combine(subset);
        expect(reconstructed).toEqual(secret);
      }
    });

    it('round-trips for various secret lengths', () => {
      for (const len of [1, 4, 16, 32, 64, 128, 256]) {
        const secret = crypto.getRandomValues(new Uint8Array(len));
        const shares = split(secret, 4, 3);
        const reconstructed = combine(shares.slice(0, 3));
        expect(reconstructed).toEqual(secret);
      }
    });
  });

  describe('threshold enforcement', () => {
    it('does NOT correctly reconstruct with fewer than k shares (overwhelmingly)', () => {
      const secret = crypto.getRandomValues(new Uint8Array(32));
      const shares = split(secret, 5, 3);

      // With k-1=2 shares, reconstruction should fail
      const insufficient = shares.slice(0, 2);
      const wrong = combine(insufficient);
      // With 32 bytes, the probability of accidental match is ~2^-256
      expect(wrong).not.toEqual(secret);
    });

    it('combine rejects fewer than 2 shares', () => {
      const secret = crypto.getRandomValues(new Uint8Array(32));
      const shares = split(secret, 5, 3);

      expect(() => combine([shares[0]])).toThrow('Need at least 2 shares');
    });
  });

  describe('edge cases', () => {
    it('k=2, n=2 (minimum configuration)', () => {
      const secret = crypto.getRandomValues(new Uint8Array(32));
      const shares = split(secret, 2, 2);
      expect(shares.length).toBe(2);

      const reconstructed = combine(shares);
      expect(reconstructed).toEqual(secret);
    });

    it('k=n (all shares required)', () => {
      const secret = crypto.getRandomValues(new Uint8Array(16));
      const shares = split(secret, 4, 4);

      const reconstructed = combine(shares);
      expect(reconstructed).toEqual(secret);

      // Missing one share should fail
      const wrong = combine(shares.slice(0, 3));
      expect(wrong).not.toEqual(secret);
    });

    it('single-byte secret', () => {
      for (let val = 0; val < 256; val += 37) {
        // Test representative byte values
        const secret = new Uint8Array([val]);
        const shares = split(secret, 3, 2);
        const reconstructed = combine(shares.slice(0, 2));
        expect(reconstructed).toEqual(secret);
      }
    });

    it('secret of all zeros', () => {
      const secret = new Uint8Array(32); // all zeros
      const shares = split(secret, 3, 2);
      const reconstructed = combine(shares.slice(0, 2));
      expect(reconstructed).toEqual(secret);
    });

    it('secret of all 0xFF', () => {
      const secret = new Uint8Array(32).fill(0xff);
      const shares = split(secret, 3, 2);
      const reconstructed = combine(shares.slice(0, 2));
      expect(reconstructed).toEqual(secret);
    });
  });

  describe('input validation', () => {
    it('rejects k < 2', () => {
      const secret = new Uint8Array(16);
      expect(() => split(secret, 3, 1)).toThrow('Threshold must be at least 2');
    });

    it('rejects n < k', () => {
      const secret = new Uint8Array(16);
      expect(() => split(secret, 2, 3)).toThrow('Number of shares must be >= threshold');
    });

    it('rejects n > 255', () => {
      const secret = new Uint8Array(16);
      expect(() => split(secret, 256, 2)).toThrow('Maximum 255 shares');
    });
  });

  describe('share properties', () => {
    it('each share has same length as secret', () => {
      const secret = crypto.getRandomValues(new Uint8Array(48));
      const shares = split(secret, 5, 3);
      for (const share of shares) {
        expect(share.data.length).toBe(secret.length);
      }
    });

    it('share indices are 1-based and sequential', () => {
      const secret = crypto.getRandomValues(new Uint8Array(16));
      const shares = split(secret, 5, 3);
      for (let i = 0; i < shares.length; i++) {
        expect(shares[i].index).toBe(i + 1);
      }
    });

    it('shares are distinct from each other and from the secret', () => {
      const secret = crypto.getRandomValues(new Uint8Array(32));
      const shares = split(secret, 5, 3);

      // Each share should differ from the secret
      for (const share of shares) {
        expect(share.data).not.toEqual(secret);
      }

      // Shares should differ from each other
      for (let i = 0; i < shares.length; i++) {
        for (let j = i + 1; j < shares.length; j++) {
          expect(shares[i].data).not.toEqual(shares[j].data);
        }
      }
    });
  });
});
