import { create, type StateCreator, type UseBoundStore } from 'zustand';
import { type StoreApi } from 'zustand/vanilla';

type BoundStore<T> = UseBoundStore<StoreApi<T>>;

function applyDevWarning<T>(store: BoundStore<T>): BoundStore<T> {
  if (process.env.NODE_ENV === 'production') return store;

  const originalUseStore = store;
  const warned = new Set<string>();

  const wrappedStore = ((selector?: unknown, equalityFn?: unknown) => {
    if (!selector) {
      const stack = new Error('stack capture').stack || '';
      const caller = stack.split('\n')[2]?.trim() || 'unknown';
      if (!warned.has(caller)) {
        warned.add(caller);
        console.warn(
          `[createStore] Store used without a selector — this subscribes to ALL fields and may cause unnecessary re-renders. Use useStore(s => s.field) instead. Called from: ${caller}`
        );
      }
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any -- Zustand's overloaded UseBoundStore call signature (selector + optional equalityFn across 4 arities) doesn't re-expose cleanly through a wrapper; the cast preserves the runtime behavior while the wrapper's own signature retains full generic typing
    return (originalUseStore as any)(selector, equalityFn);
  }) as BoundStore<T>;

  Object.assign(wrappedStore, originalUseStore);
  return wrappedStore;
}

export function createStore<T>() {
  return (initializer: StateCreator<T, [], []>): BoundStore<T> => {
    return applyDevWarning(create<T>()(initializer));
  };
}

export function wrapStore<S extends BoundStore<unknown>>(store: S): S {
  return applyDevWarning(store as BoundStore<unknown>) as S;
}
