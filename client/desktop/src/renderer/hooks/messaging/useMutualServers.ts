import { useEffect, useMemo, useState } from 'react';
import { getMutualServers } from '@/renderer/services/system/mutualServers';

const NONE: ReadonlySet<string> = new Set();

/**
 * Servers that EVERY one of `userIds` is already a member of, from the viewer's
 * side (#2372).
 *
 * "Every", not "any", because the picker uses this to grey a server out. In a
 * group DM, a server one recipient is already in is still worth inviting the
 * other eight to — greying it there would remove a working action. The
 * one-to-one case, which is what the report describes, is the degenerate form
 * of the same rule.
 *
 * Returns an empty set whenever ANY recipient's answer is unknown, rather than
 * intersecting the ones that did answer. A partial intersection would grey a
 * server on the strength of recipients we could ask about while saying nothing
 * about the one we could not — a confident-looking answer to a question that
 * was not fully answered. Empty degrades OPEN: nothing greys, the picker works
 * as it did before this feature, and a pointless invite costs a 409 the user
 * can read.
 */
export function useMutualServersForAll(userIds: readonly string[]): ReadonlySet<string> {
  // Keyed on the SET of ids, not the array's identity — callers derive this
  // list inside a render, so a dependency on the array itself would re-probe on
  // every keystroke in the composer.
  //
  // The comparator is explicit and deliberately NOT `localeCompare`, which is
  // what `typescript:S2871` suggests for sorting strings. The sort here is
  // canonicalisation for a cache key, not presentation: it has to be
  // DETERMINISTIC, and `localeCompare` is locale-sensitive by definition, so it
  // would let the same set of recipients hash to different keys on two machines
  // or across a locale change. Taking the rule's suggestion literally would
  // introduce the defect the rule exists to prevent, one layer over.
  const key = useMemo(
    () =>
      [...new Set(userIds)]
        .sort((a, b) => {
          if (a < b) return -1;
          return a > b ? 1 : 0;
        })
        .join(','),
    [userIds]
  );

  // The settled answer is stored WITH the key it answers, and the return below
  // compares them. That is what makes a key change yield `NONE` during the very
  // same render, without an effect writing state synchronously to reset it —
  // the shape `@eslint-react/set-state-in-effect` exists to discourage, and
  // which would also have let a previous recipient's answer paint for one frame.
  const [settled, setSettled] = useState<{ key: string; servers: ReadonlySet<string> } | null>(
    null
  );

  useEffect(() => {
    const ids = key === '' ? [] : key.split(',');
    if (ids.length === 0) return;

    let active = true;
    void Promise.all(ids.map((id) => getMutualServers(id))).then((results) => {
      if (!active) return;
      if (results.some((result) => result === null)) {
        setSettled({ key, servers: NONE });
        return;
      }
      const lists = results as readonly (readonly string[])[];
      const intersection = new Set(lists[0]);
      for (const list of lists.slice(1)) {
        const present = new Set(list);
        for (const id of intersection) {
          if (!present.has(id)) intersection.delete(id);
        }
      }
      setSettled({ key, servers: intersection });
    });

    return () => {
      active = false;
    };
  }, [key]);

  return settled?.key === key ? settled.servers : NONE;
}
