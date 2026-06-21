import { describe, it, expect } from 'vitest';
import { generateRequestId } from '@/renderer/services/pipSignalingTypes';

describe('pipSignalingTypes', () => {
  describe('generateRequestId', () => {
    it('returns a string containing the pipId prefix', () => {
      const id = generateRequestId('controls-main');
      expect(id).toContain('controls-main');
    });

    it('returns unique IDs on successive calls', () => {
      const id1 = generateRequestId('pip-1');
      const id2 = generateRequestId('pip-1');
      const id3 = generateRequestId('pip-2');
      expect(id1).not.toBe(id2);
      expect(id2).not.toBe(id3);
    });

    it('includes a timestamp component (base-36 suffix)', () => {
      const id = generateRequestId('test');
      // Format: pipId-counter-timestamp(base36)
      const parts = id.split('-');
      // Last part should be a base-36 encoded timestamp
      const timestampPart = parts[parts.length - 1];
      const parsed = parseInt(timestampPart, 36);
      expect(parsed).toBeGreaterThan(0);
      // Should be a recent timestamp (within last minute)
      expect(parsed).toBeCloseTo(Date.now(), -4); // within ~10s
    });
  });
});
