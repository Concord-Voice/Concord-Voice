import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { ErrorBoundary } from '../../../../src/renderer/components/ErrorBoundary';

const Throw = ({ message }: { message: string }) => {
  throw new Error(message);
};

describe('ErrorBoundary', () => {
  let consoleErrorSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    // Suppress React's default error log noise during throw-tests.
    // Restored in afterEach to prevent cross-test stub leakage (Vitest does
    // not auto-restore spies; see Copilot review on PR #793).
    consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    consoleErrorSpy.mockRestore();
  });

  it('renders children when no error', () => {
    render(
      <ErrorBoundary fallback={<div>fallback</div>}>
        <div>child</div>
      </ErrorBoundary>
    );
    expect(screen.getByText('child')).toBeInTheDocument();
  });

  it('renders fallback ReactNode when child throws', () => {
    render(
      <ErrorBoundary fallback={<div>caught-it</div>}>
        <Throw message="boom" />
      </ErrorBoundary>
    );
    expect(screen.getByText('caught-it')).toBeInTheDocument();
  });

  it('renders fallback function with caught error instance', () => {
    render(
      <ErrorBoundary fallback={(err) => <div>caught: {err.message}</div>}>
        <Throw message="boom" />
      </ErrorBoundary>
    );
    expect(screen.getByText('caught: boom')).toBeInTheDocument();
  });

  it('invokes onError callback with the caught error and componentStack', () => {
    const onError = vi.fn();
    render(
      <ErrorBoundary fallback={<div>fallback</div>} onError={onError}>
        <Throw message="boom" />
      </ErrorBoundary>
    );
    expect(onError).toHaveBeenCalledTimes(1);
    const [err, info] = onError.mock.calls[0];
    expect(err).toBeInstanceOf(Error);
    expect((err as Error).message).toBe('boom');
    expect(info).toHaveProperty('componentStack');
  });
});
