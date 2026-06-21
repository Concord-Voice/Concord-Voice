import { describe, it, expect, beforeEach } from 'vitest';
import { usePendingRegistrationStore } from './pendingRegistrationStore';

describe('pendingRegistrationStore', () => {
  beforeEach(() => {
    usePendingRegistrationStore.getState().clearPending();
    sessionStorage.clear();
  });

  it('starts empty', () => {
    const s = usePendingRegistrationStore.getState();
    expect(s.pendingId).toBeNull();
    expect(s.email).toBeNull();
    expect(s.resendsRemaining).toBe(4);
  });

  it('setPending populates all fields', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'abc-123',
      email: 'a@b.com',
      expires_at: '2026-04-17T22:00:00Z',
      code_expires_at: '2026-04-17T21:02:00Z',
    });
    const s = usePendingRegistrationStore.getState();
    expect(s.pendingId).toBe('abc-123');
    expect(s.email).toBe('a@b.com');
    expect(s.expiresAt).toBe('2026-04-17T22:00:00Z');
  });

  it('isExpired returns true when expiresAt is in the past', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'x',
      email: 'a@b.com',
      expires_at: '2020-01-01T00:00:00Z',
      code_expires_at: '2020-01-01T00:00:00Z',
    });
    expect(usePendingRegistrationStore.getState().isExpired()).toBe(true);
  });

  it('updateAfterResend decrements resendsRemaining', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'x',
      email: 'a@b.com',
      expires_at: new Date(Date.now() + 60_000).toISOString(),
      code_expires_at: new Date(Date.now() + 60_000).toISOString(),
    });
    usePendingRegistrationStore.getState().updateAfterResend({
      code_expires_at: new Date(Date.now() + 120_000).toISOString(),
      resends_remaining: 3,
    });
    expect(usePendingRegistrationStore.getState().resendsRemaining).toBe(3);
  });

  it('isExpired returns false when expiresAt is in the future', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'x',
      email: 'a@b.com',
      expires_at: new Date(Date.now() + 60_000).toISOString(),
      code_expires_at: new Date(Date.now() + 60_000).toISOString(),
    });
    expect(usePendingRegistrationStore.getState().isExpired()).toBe(false);
  });

  it('isExpired returns false when expiresAt is null', () => {
    // Fresh store has no expiresAt
    expect(usePendingRegistrationStore.getState().isExpired()).toBe(false);
  });

  it('updateEmail swaps email and refreshes code expiry + resend counters', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'pid',
      email: 'old@example.com',
      expires_at: new Date(Date.now() + 60_000).toISOString(),
      code_expires_at: new Date(Date.now() + 60_000).toISOString(),
    });
    // Burn a resend so we can verify updateEmail resets it
    usePendingRegistrationStore.getState().updateAfterResend({
      code_expires_at: new Date(Date.now() + 120_000).toISOString(),
      resends_remaining: 2,
    });

    const newExpiry = new Date(Date.now() + 180_000).toISOString();
    usePendingRegistrationStore.getState().updateEmail('new@example.com', newExpiry);

    const s = usePendingRegistrationStore.getState();
    expect(s.email).toBe('new@example.com');
    expect(s.codeExpiresAt).toBe(newExpiry);
    expect(s.resendsRemaining).toBe(4);
    expect(s.lastResendAt).toBeNull();
  });

  it('clearPending resets all fields back to initial state', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'pid',
      email: 'a@b.com',
      expires_at: new Date(Date.now() + 60_000).toISOString(),
      code_expires_at: new Date(Date.now() + 60_000).toISOString(),
    });
    usePendingRegistrationStore.getState().clearPending();
    const s = usePendingRegistrationStore.getState();
    expect(s.pendingId).toBeNull();
    expect(s.email).toBeNull();
    expect(s.expiresAt).toBeNull();
    expect(s.codeExpiresAt).toBeNull();
    expect(s.resendsRemaining).toBe(4);
    expect(s.lastResendAt).toBeNull();
  });

  it('persists via sessionStorage (not localStorage)', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'persist-id',
      email: 'a@b.com',
      expires_at: new Date(Date.now() + 60_000).toISOString(),
      code_expires_at: new Date(Date.now() + 60_000).toISOString(),
    });
    const fromSession = sessionStorage.getItem('concord-pending-registration');
    expect(fromSession).toContain('persist-id');
    expect(localStorage.getItem('concord-pending-registration')).toBeNull();
  });
});
