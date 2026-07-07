import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef } from 'react';

interface StackEntry {
  id: string;
  depth: number;
  el: HTMLElement | null;
}

interface ModalStackContextValue {
  register: (id: string, depth: number, el: HTMLElement | null) => void;
  unregister: (id: string) => void;
  isTopmost: (id: string) => boolean;
}

const ModalStackContext = createContext<ModalStackContextValue | null>(null);

/**
 * Tracks nesting depth so nested modals are always considered "above" their parent,
 * regardless of React effect execution order.
 */
export const ModalDepthContext = createContext(0);

// Highest depth wins; among equal depth, last registered wins.
function topmostId(stack: StackEntry[]): string | null {
  if (stack.length === 0) return null;
  let top = stack[0];
  for (let i = 1; i < stack.length; i++) {
    if (stack[i].depth >= top.depth) {
      top = stack[i];
    }
  }
  return top.id;
}

// Background-inert coordination (#2087). Modals portal out to document.body, so
// the app root (#root) can be made `inert` while any modal is open WITHOUT
// inert-ing the modal itself. Within a nested stack, every non-topmost overlay is
// also inert, so only the topmost modal is reachable — by keyboard, pointer, AND
// screen-reader virtual cursor (closing the aria-modal-without-inert gap a
// declarative <dialog open> leaves, unlike showModal()). Each overlay also gets a
// depth-based z-index so a nested child paints above its parent regardless of
// portal insertion order (React inserts nested portals child-first).
//
// This runs SYNCHRONOUSLY inside register/unregister — NOT in a deferred effect.
// React flushes passive-effect cleanups in definition order on close, and Modal's
// register effect is defined before its focus-restore effect, so unregister clears
// #root's inert BEFORE the focus-restore cleanup runs. Deferring inert removal to
// a re-render-gated effect would run it AFTER focus restoration, leaving #root
// inert and making `.focus()` on the invoker (a descendant of #root) a no-op —
// silently breaking focus return (WCAG 2.4.3). Do NOT reintroduce that async gap.
function syncInert(stack: StackEntry[]) {
  const root = document.getElementById('root');
  const top = topmostId(stack);
  if (root) {
    if (stack.length > 0) root.setAttribute('inert', '');
    else root.removeAttribute('inert');
  }
  for (const entry of stack) {
    if (!entry.el) continue;
    entry.el.style.zIndex = String(1000 + entry.depth);
    if (entry.id === top) entry.el.removeAttribute('inert');
    else entry.el.setAttribute('inert', '');
  }
}

export function ModalProvider({ children }: Readonly<{ children: React.ReactNode }>) {
  const stackRef = useRef<StackEntry[]>([]);

  const register = useCallback((id: string, depth: number, el: HTMLElement | null) => {
    stackRef.current = [...stackRef.current, { id, depth, el }];
    syncInert(stackRef.current);
  }, []);

  const unregister = useCallback((id: string) => {
    stackRef.current = stackRef.current.filter((entry) => entry.id !== id);
    syncInert(stackRef.current);
  }, []);

  const isTopmost = useCallback((id: string) => topmostId(stackRef.current) === id, []);

  // Safety net: never strand #root inert if the provider unmounts with a modal open.
  useEffect(() => {
    return () => {
      document.getElementById('root')?.removeAttribute('inert');
    };
  }, []);

  const value = useMemo(
    () => ({ register, unregister, isTopmost }),
    [register, unregister, isTopmost]
  );

  // eslint-disable-next-line @eslint-react/no-context-provider -- Context.Provider is the correct API for this provider component; React 19 Context-as-JSX refactor deferred
  return <ModalStackContext.Provider value={value}>{children}</ModalStackContext.Provider>;
}

export function useModalStack(): ModalStackContextValue {
  // eslint-disable-next-line @eslint-react/no-use-context -- useContext is the correct API for this guard hook; use() would change conditional-hook semantics
  const context = useContext(ModalStackContext);
  if (!context) {
    throw new Error('useModalStack must be used within a ModalProvider');
  }
  return context;
}
