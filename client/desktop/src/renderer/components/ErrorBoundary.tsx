import React, { type ReactNode, type ErrorInfo } from 'react';

type Fallback = ReactNode | ((error: Error) => ReactNode);

interface Props {
  fallback: Fallback;
  children: ReactNode;
  onError?: (error: Error, info: ErrorInfo) => void;
}

interface State {
  error: Error | null;
}

export class ErrorBoundary extends React.Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // Renderer-side log only. Main-process logs flow through their own sinks.
    // Logging error.message (string), not the error object — see #714 raw-err policy.
    console.error('ErrorBoundary caught:', error.message, info.componentStack);
    this.props.onError?.(error, info);
  }

  render(): ReactNode {
    if (this.state.error) {
      const { fallback } = this.props;
      return typeof fallback === 'function' ? fallback(this.state.error) : fallback;
    }
    return this.props.children;
  }
}
