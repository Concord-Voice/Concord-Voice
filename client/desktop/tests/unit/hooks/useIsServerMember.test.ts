import { describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useIsServerMember } from '@/renderer/hooks/messaging/useIsServerMember';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { mockServer, mockServer2 } from '../../mocks/fixtures';

/**
 * The hook is four lines; what needs pinning is the CONTRACT those four lines
 * encode, because every caller reads the tri-state and two of the three states
 * are easy to collapse by accident.
 */
describe('useIsServerMember', () => {
  beforeEach(() => {
    useServerStore.setState({ servers: [] });
  });

  it('answers `undefined`, NOT false, when there is no server id', () => {
    // The whole reason this returns `boolean | undefined`. `undefined` means
    // "cannot tell" — an older self-hosted control plane omits `server_id` from
    // the invite preview — and a caller must offer Join anyway so the server's
    // own 409 can be the authority. Collapsing it to `false` would read as
    // "we know you are not a member", which is a different claim.
    const { result } = renderHook(() => useIsServerMember(undefined));
    expect(result.current).toBeUndefined();
    // Stated separately: `toBeUndefined` alone would also pass for `false` if a
    // future refactor made the hook return nothing at all on this path.
    expect(result.current).not.toBe(false);
  });

  it('answers true for a server the user is in', () => {
    useServerStore.setState({ servers: [mockServer] });
    const { result } = renderHook(() => useIsServerMember(mockServer.id));
    expect(result.current).toBe(true);
  });

  it('answers false for a server the user is NOT in, while in others', () => {
    // The control matters more than the assertion. Asserting `false` against an
    // EMPTY store passes against a hook that returns `false` unconditionally,
    // and against one that tests `servers.length` instead of matching the id.
    // A populated store the target is absent from kills both.
    useServerStore.setState({ servers: [mockServer2] });
    const { result } = renderHook(() => useIsServerMember(mockServer.id));
    expect(result.current).toBe(false);
  });

  it('re-renders false to true when a join commits, with no remount', () => {
    // The docblock's load-bearing claim: a "Joined" state derived from this
    // updates in the same tick as the join, with no cache to invalidate. That
    // is only true while the hook subscribes to the store — read the array once
    // and every assertion above still passes while the UI goes stale.
    useServerStore.setState({ servers: [] });
    const { result } = renderHook(() => useIsServerMember(mockServer.id));
    expect(result.current).toBe(false);

    act(() => {
      // Through `addServer`, which is the path `inviteStore.joinServer` takes,
      // rather than a bare setState that would pin nothing about the real flow.
      useServerStore.getState().addServer(mockServer);
    });

    expect(result.current).toBe(true);
  });
});
