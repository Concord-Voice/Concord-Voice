import { createJSONStorage, type PersistStorage } from 'zustand/middleware';

/**
 * localStorage for zustand's persist middleware that degrades to in-memory state
 * instead of throwing when the write is refused.
 *
 * A refused `setItem` — quota exhausted, private-window storage disabled, site
 * data blocked — otherwise propagates out of persist's own write and surfaces as
 * an unhandled rejection at whatever point in the app happened to mutate the
 * store. The store's in-memory value is already correct by then; only its
 * survival across a restart is lost, which is a degradation rather than a fault.
 *
 * Reads and removals are unwrapped deliberately: a failing `getItem` yields no
 * persisted state, which persist already treats as a cold start.
 */
export function createQuotaSafeStorage<T>(): PersistStorage<T> | undefined {
  return createJSONStorage<T>(() => {
    const storage = localStorage;
    return {
      getItem: storage.getItem.bind(storage),
      setItem: (name, value) => {
        try {
          storage.setItem(name, value);
        } catch {
          console.warn('Unable to persist store state; continuing in memory.');
        }
      },
      removeItem: storage.removeItem.bind(storage),
    };
  });
}
