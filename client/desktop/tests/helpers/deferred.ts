/**
 * A promise together with its externally-callable `resolve` / `reject`.
 *
 * Consolidates eighteen near-identical local definitions that had drifted into
 * four shapes: a nullable resolver guarded by a "not initialized" throw, a
 * definite-assignment (`let resolve!`) form, a form that also exposed `reject`,
 * and a non-generic `Promise<void>` form.
 *
 * The guard variants were dead code — the `Promise` executor runs
 * synchronously during construction, so the resolver is always assigned before
 * `deferred()` returns. Definite assignment expresses that directly.
 *
 * `T` defaults to `void` so the callers that only need a completion signal can
 * write `deferred()` and call `resolve()` with no argument.
 */
export interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T | PromiseLike<T>) => void;
  reject: (reason?: unknown) => void;
}

export function deferred<T = void>(): Deferred<T> {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}
