import React, { createContext, useCallback, useContext, useMemo, useRef } from 'react';

interface StackEntry {
  id: string;
  depth: number;
}

interface ModalStackContextValue {
  register: (id: string, depth: number) => void;
  unregister: (id: string) => void;
  isTopmost: (id: string) => boolean;
}

const ModalStackContext = createContext<ModalStackContextValue | null>(null);

/**
 * Tracks nesting depth so nested modals are always considered "above" their parent,
 * regardless of React effect execution order.
 */
export const ModalDepthContext = createContext(0);

export function ModalProvider({ children }: Readonly<{ children: React.ReactNode }>) {
  const stackRef = useRef<StackEntry[]>([]);

  const register = useCallback((id: string, depth: number) => {
    stackRef.current = [...stackRef.current, { id, depth }];
  }, []);

  const unregister = useCallback((id: string) => {
    stackRef.current = stackRef.current.filter((entry) => entry.id !== id);
  }, []);

  const isTopmost = useCallback((id: string) => {
    const stack = stackRef.current;
    if (stack.length === 0) return false;
    // Highest depth wins; among same depth, last registered wins
    let top = stack[0];
    for (let i = 1; i < stack.length; i++) {
      if (stack[i].depth >= top.depth) {
        top = stack[i];
      }
    }
    return top.id === id;
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
