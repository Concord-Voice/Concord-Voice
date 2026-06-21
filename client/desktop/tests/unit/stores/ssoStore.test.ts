import { describe, it, expect, beforeEach } from 'vitest';
import { useSSOStore } from '@/renderer/stores/ssoStore';
import { resetAllStores } from '../../helpers/store-helpers';

/**
 * ssoStore (#270) — covers the discriminated-union state machine that drives
 * the in-flight SSO UI. The store is ephemeral (no persist middleware), so
 * tests focus on the setState / reset transitions and verify each phase
 * narrows to the right shape.
 */

describe('ssoStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts in the idle phase', () => {
    expect(useSSOStore.getState().state).toEqual({ phase: 'idle' });
  });

  it('transitions to authenticating with a provider', () => {
    useSSOStore.getState().setState({ phase: 'authenticating', provider: 'google' });
    const s = useSSOStore.getState().state;
    expect(s.phase).toBe('authenticating');
    if (s.phase === 'authenticating') {
      expect(s.provider).toBe('google');
    }
  });

  it('transitions to register_required with provider, sso_token, email, name', () => {
    useSSOStore.getState().setState({
      phase: 'register_required',
      provider: 'google',
      ssoToken: 'tok-1',
      email: 'new@example.test',
      name: 'New User',
    });
    const s = useSSOStore.getState().state;
    expect(s.phase).toBe('register_required');
    if (s.phase === 'register_required') {
      expect(s.ssoToken).toBe('tok-1');
      expect(s.email).toBe('new@example.test');
      expect(s.name).toBe('New User');
    }
  });

  it('transitions to link_required with masked_email', () => {
    useSSOStore.getState().setState({
      phase: 'link_required',
      provider: 'google',
      ssoToken: 'link-tok',
      maskedEmail: 'a***@example.test',
    });
    const s = useSSOStore.getState().state;
    expect(s.phase).toBe('link_required');
    if (s.phase === 'link_required') {
      expect(s.maskedEmail).toBe('a***@example.test');
    }
  });

  it('transitions to mfa_required with a challenge token', () => {
    useSSOStore.getState().setState({
      phase: 'mfa_required',
      mfaChallengeToken: 'mfa-1',
    });
    const s = useSSOStore.getState().state;
    expect(s.phase).toBe('mfa_required');
    if (s.phase === 'mfa_required') {
      expect(s.mfaChallengeToken).toBe('mfa-1');
    }
  });

  it('transitions to error with a message', () => {
    useSSOStore.getState().setState({ phase: 'error', message: 'oauth_state_mismatch' });
    const s = useSSOStore.getState().state;
    expect(s.phase).toBe('error');
    if (s.phase === 'error') {
      expect(s.message).toBe('oauth_state_mismatch');
    }
  });

  it('reset() returns the store to idle from any other phase', () => {
    useSSOStore.getState().setState({ phase: 'error', message: 'something bad' });
    expect(useSSOStore.getState().state.phase).toBe('error');

    useSSOStore.getState().reset();
    expect(useSSOStore.getState().state).toEqual({ phase: 'idle' });
  });
});
