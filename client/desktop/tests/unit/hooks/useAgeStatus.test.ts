import { describe, it, expect } from 'vitest';
import { renderHook } from '../../test-utils';
import { useAgeStatus } from '@/renderer/hooks/useAgeStatus';

describe('useAgeStatus', () => {
  it('returns nsfwAuth=unknown while no producer exists (inert seam)', () => {
    const { result } = renderHook(() => useAgeStatus());
    expect(result.current).toEqual({ nsfwAuth: 'unknown' });
  });
});
