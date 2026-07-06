import { describe, it, expect } from 'vitest';
import { pseudonymizeLogUuids } from '@/renderer/utils/pseudonymizeLogUuids';

const A = '550e8400-e29b-41d4-a716-446655440000';
const B = '00000000-0000-4000-8000-000000000001';

describe('pseudonymizeLogUuids', () => {
  it('maps the same UUID to the same token within one report', () => {
    const out = pseudonymizeLogUuids(`channel ${A} then again ${A}`);
    expect(out).toBe('channel <id:1> then again <id:1>');
  });
  it('maps distinct UUIDs to distinct tokens', () => {
    const out = pseudonymizeLogUuids(`${A} and ${B}`);
    expect(out).toBe('<id:1> and <id:2>');
  });
  it('is fresh per call — no cross-report linkability', () => {
    expect(pseudonymizeLogUuids(B)).toBe('<id:1>'); // B is <id:1> here, was <id:2> above
  });
  it('leaves non-UUID text untouched', () => {
    expect(pseudonymizeLogUuids('epoch 7 [E2EE] ok')).toBe('epoch 7 [E2EE] ok');
  });
});
