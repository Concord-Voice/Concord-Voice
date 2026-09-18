import { act, renderHook, waitFor } from '@testing-library/react';
import { useMutualServersForAll } from '@/renderer/hooks/messaging/useMutualServers';

vi.mock('@/renderer/services/system/mutualServers', () => ({ getMutualServers: vi.fn() }));
import { getMutualServers } from '@/renderer/services/system/mutualServers';
const mockGetMutualServers = getMutualServers as ReturnType<typeof vi.fn>;

/**
 * Unit tests for the two rules `useMutualServersForAll` exists to enforce
 * (#2372). No DOM: `InviteServerPicker.test.tsx` covers the rendered picker,
 * but its "only SOME recipients" case asserts an absence
 * (`getByRole('Beta')).toBeEnabled()`) that is already true at first paint —
 * the assertion resolves on `waitFor`'s first synchronous check and proves
 * nothing about whether the intersection ran before it. These tests make the
 * ordering explicit with a positive control in the SAME test: a sibling
 * server id that DOES change from absent to present, so a no-op
 * implementation (or one that silently drops a recipient) cannot pass.
 */
describe('useMutualServersForAll', () => {
  beforeEach(() => {
    mockGetMutualServers.mockReset();
  });

  it('probes each recipient once and greys nothing before the probes settle', () => {
    mockGetMutualServers.mockReturnValue(new Promise(() => {}));
    const { result } = renderHook(() => useMutualServersForAll(['a', 'b']));
    expect(result.current.size).toBe(0);
    expect(mockGetMutualServers).toHaveBeenCalledWith('a');
    expect(mockGetMutualServers).toHaveBeenCalledWith('b');
  });

  it('issues no probe and greys nothing for an empty recipient list', () => {
    const { result } = renderHook(() => useMutualServersForAll([]));
    expect(result.current.size).toBe(0);
    expect(mockGetMutualServers).not.toHaveBeenCalled();
  });

  // ── Rule 1: EVERY recipient, not ANY ────────────────────────────────────────
  //
  // A server one recipient of a group DM is already in is still worth
  // inviting the others to. Greying it on ANY membership would remove a
  // working action. `s2`, shared by both recipients, is the POSITIVE
  // CONTROL: waiting on it becoming present is a real, ordering-sensitive
  // assertion (false at mount, true only once the intersection has actually
  // run), which is what makes the negative assertion that follows meaningful
  // rather than a check on a value that was already true at first paint.
  it('greys a server only when EVERY recipient shares it, not when only some do', async () => {
    mockGetMutualServers.mockImplementation((id: string) =>
      Promise.resolve(id === 'a' ? ['s1', 's2'] : ['s2'])
    );

    const { result } = renderHook(() => useMutualServersForAll(['a', 'b']));

    // Positive control: s2 is shared by BOTH recipients and must settle in.
    // This is what proves the effect ran and the intersection settled.
    await waitFor(() => expect(result.current.has('s2')).toBe(true));

    // s1 is shared by 'a' only. Under an ANY rule this would also be greyed;
    // under EVERY it must not be.
    expect(result.current.has('s1')).toBe(false);
  });

  // ── Rule 2: any null result forces NONE, never a partial intersection ──────
  //
  // A probe that could not answer must fail open, and specifically must not
  // be silently dropped from the intersection — dropping an unanswerable
  // recipient and intersecting only the ones who did answer would still grey
  // a server on partial information. The positive control here is the SAME
  // recipient set answering fully (no null) first, proving the mock and the
  // hook genuinely compute a non-empty intersection when nothing is missing;
  // adding the unanswerable recipient must then take it back to empty.
  it('a null result for any recipient forces NONE, even when the others agree on a shared server', async () => {
    mockGetMutualServers.mockImplementation((id: string) =>
      Promise.resolve(id === 'c' ? null : ['s1'])
    );

    const { result, rerender } = renderHook(
      ({ ids }: { ids: readonly string[] }) => useMutualServersForAll(ids),
      { initialProps: { ids: ['a', 'b'] as readonly string[] } }
    );

    // Positive control: without the unanswerable recipient, s1 is correctly
    // greyed — proves the intersection mechanism works at all.
    await waitFor(() => expect(result.current.has('s1')).toBe(true));

    // Add a recipient whose probe cannot be answered. If it were merely
    // dropped from the intersection rather than forcing NONE, 's1' would stay
    // greyed because 'a' and 'b' still agree on it.
    rerender({ ids: ['a', 'b', 'c'] });
    await waitFor(() => expect(mockGetMutualServers).toHaveBeenCalledWith('c'));
    await waitFor(() => expect(result.current.has('s1')).toBe(false));
  });

  // The key fence on the READ, which is a different mechanism from the `active`
  // guard on the write and needs its own case: `active` stops a stale
  // continuation from writing, while the fence stops an already-settled answer
  // for the PREVIOUS recipient set being served for the current one during the
  // window before the new probes settle.
  //
  // Concretely this is one DM's greying appearing in another: open a
  // conversation, let it settle, switch to a different conversation, and for
  // the moment before its probes answer the hook must say NONE rather than
  // reuse the last conversation's intersection.
  it('serves NONE for a new recipient set until its OWN probes settle', async () => {
    let resolveNext!: (v: readonly string[] | null) => void;
    mockGetMutualServers.mockImplementation((id: string) => {
      if (id === 'next') {
        return new Promise((r) => {
          resolveNext = r;
        });
      }
      return Promise.resolve(['s1']);
    });

    const { result, rerender } = renderHook(
      ({ ids }: { ids: readonly string[] }) => useMutualServersForAll(ids),
      { initialProps: { ids: ['a'] as readonly string[] } }
    );

    // Positive control: the first set settles and really does grey s1, so a
    // later absence cannot be explained by the hook simply never working.
    await waitFor(() => expect(result.current.has('s1')).toBe(true));

    // Switch to a set whose probe has NOT answered. `settled` still holds the
    // previous key's answer, so an unfenced read would serve s1 here.
    rerender({ ids: ['next'] });
    expect(result.current.has('s1')).toBe(false);
    expect(result.current.size).toBe(0);

    // And once its own probe answers, it serves that.
    resolveNext(['s2']);
    await waitFor(() => expect(result.current.has('s2')).toBe(true));
  });

  // A stale recipient set must not overwrite a newer one that settles first —
  // covers the `active` guard and the key-vs-settled comparison the hook uses
  // to answer synchronously on a key change.
  it('does not apply a stale recipient set answer once the ids have moved on', async () => {
    let resolveStale!: (v: readonly string[] | null) => void;
    mockGetMutualServers.mockImplementation((id: string) => {
      if (id === 'stale-only') {
        return new Promise((r) => {
          resolveStale = r;
        });
      }
      return Promise.resolve(['s1']);
    });

    const { result, rerender } = renderHook(
      ({ ids }: { ids: readonly string[] }) => useMutualServersForAll(ids),
      { initialProps: { ids: ['stale-only'] as readonly string[] } }
    );
    expect(result.current.size).toBe(0); // stale probe still in flight

    // The recipient set moves on before the stale probe settles.
    rerender({ ids: ['a', 'b'] });
    await waitFor(() => expect(result.current.has('s1')).toBe(true));

    // The stale probe now resolves, and it resolves with a DISTINCT id. Using
    // 's1' here — the same value the current probes return — is what made this
    // case vacuous: a stale answer that overwrote the current one would be
    // byte-identical to the correct answer, so neither the `active` guard nor
    // the key fence was actually covered. Both could be deleted and this test
    // stayed green (CodeRabbit, PR #3355).
    resolveStale(['stale-server']);

    // Flush the stale continuation before asserting. The assertion below is an
    // ABSENCE, and an absence asserted too early passes before the bad write
    // could even happen — which is the very failure mode under test.
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(result.current.has('s1')).toBe(true);
    // The load-bearing half: a stale answer must not leak in at all.
    expect(result.current.has('stale-server')).toBe(false);
  });
});
