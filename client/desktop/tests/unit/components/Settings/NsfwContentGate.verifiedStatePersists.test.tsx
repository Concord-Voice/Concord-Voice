import { render, screen } from '../../../test-utils';
import { describe, it, expect, beforeAll, afterAll, afterEach, beforeEach, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';

// regression for #1763 (public Concord-Voice#3): after a user has verified their age,
// returning to the age-verification surface re-rendered the first-run DOB-entry default
// instead of the verified state — the verified OUTCOME was never read back. The durable
// record already exists server-side (age_verification_records, written by PUT /age/claim);
// the gap was a missing read path. This test drives the REAL useAgeStatus hook against a
// mocked authoritative status response and asserts the gate reflects it on a fresh mount.

// Isolate the component from the signing service — this test exercises the READ/rehydrate
// path, never submit. The hook under test (useAgeStatus) stays REAL.
vi.mock('@/renderer/services/ageClaim/ageClaimService', () => ({
  submitSignedAgeClaim: vi.fn(),
}));

import NsfwContentGate from '@/renderer/components/Settings/NsfwContentGate';

const API_BASE = 'http://localhost:8080';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('NsfwContentGate — verified state persists across mounts (regression #1763)', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('rehydrates the verified state on mount instead of the first-run DOB form', async () => {
    // The durable server record says this user already verified as an adult (>=18).
    server.use(
      http.get(`${API_BASE}/api/v1/age/status`, () =>
        HttpResponse.json({ verified: true, valid_age: true, nsfw_auth: true })
      )
    );

    render(<NsfwContentGate />);

    // The gate must reflect the authoritative verified record — never re-prompt for DOB.
    expect(await screen.findByText(/already verified/i)).toBeInTheDocument();
    expect(screen.queryByRole('spinbutton', { name: /year/i })).not.toBeInTheDocument();
  });
});
