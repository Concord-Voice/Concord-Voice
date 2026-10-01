import { describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent } from '../../../test-utils';
import PurgeResult from '@/renderer/components/Purge/PurgeResult';
import type { PurgeContext, TerminalPurgeResult } from '@/renderer/services/messaging/purgeApi';

// #3455: the two result kinds the self-purge soft-lock adds. Both come from a
// gate that runs before any purge batch, so both may — and must — say that
// nothing was purged (contrast `partial`, which must never).

function renderResult(result: TerminalPurgeResult, context: PurgeContext = 'channel') {
  return render(<PurgeResult context={context} result={result} onDone={vi.fn()} />);
}

describe('PurgeResult — verificationLimited', () => {
  it('names the verification budget, not the purge limit, and says nothing was purged', () => {
    renderResult({ kind: 'verificationLimited', retryAfterSeconds: 300 });

    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('Too many verification attempts. Nothing was purged.');
    expect(alert).not.toHaveTextContent(/purge limit/i);
  });

  it('carries a countdown derived from Retry-After', () => {
    renderResult({ kind: 'verificationLimited', retryAfterSeconds: 300 });
    expect(screen.getByRole('alert')).toHaveTextContent('Try again in 5 minutes.');
  });

  it('rounds a sub-minute wait in seconds', () => {
    renderResult({ kind: 'verificationLimited', retryAfterSeconds: 45 });
    expect(screen.getByRole('alert')).toHaveTextContent('Try again in 45 seconds.');
  });

  it.each([[undefined], [0], [-3], [Number.NaN]])(
    'invents no countdown for a delay of %s',
    (retryAfterSeconds) => {
      renderResult({ kind: 'verificationLimited', retryAfterSeconds });
      const alert = screen.getByRole('alert');
      expect(alert).toHaveTextContent('Try again later.');
      expect(alert).not.toHaveTextContent(/try again in/i);
    }
  );

  it('is worded the same for a server-wide purge', () => {
    renderResult({ kind: 'verificationLimited' }, 'server');
    expect(screen.getByRole('alert')).toHaveTextContent('Nothing was purged.');
  });
});

describe('PurgeResult — softLockFailed', () => {
  it('shows the server text followed by "Nothing was purged."', () => {
    renderResult({
      kind: 'softLockFailed',
      message: 'Set up an authenticator app or security key to do this.',
    });
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Set up an authenticator app or security key to do this. Nothing was purged.'
    );
  });

  it('falls back to generic copy when the server sent no text', () => {
    renderResult({ kind: 'softLockFailed' });
    expect(screen.getByRole('alert')).toHaveTextContent(
      "This purge couldn't be completed. Nothing was purged."
    );
  });

  it('appends a countdown only when Retry-After was present', () => {
    renderResult({ kind: 'softLockFailed', retryAfterSeconds: 120 });
    expect(screen.getByRole('alert')).toHaveTextContent(
      "This purge couldn't be completed. Nothing was purged. You can try again in 2 minutes."
    );
  });

  it.each([[undefined], [0]])('has no countdown for a delay of %s', (retryAfterSeconds) => {
    renderResult({ kind: 'softLockFailed', retryAfterSeconds });
    expect(screen.getByRole('alert')).not.toHaveTextContent(/try again in/i);
  });

  it('never claims that messages may already be gone', () => {
    renderResult({ kind: 'softLockFailed', message: 'x' });
    expect(screen.getByRole('alert')).not.toHaveTextContent(/may already/i);
  });
});

describe('PurgeResult — Done', () => {
  it.each([
    ['verificationLimited', { kind: 'verificationLimited' }],
    ['softLockFailed', { kind: 'softLockFailed' }],
  ] as const)('%s offers Done', async (_, result) => {
    const user = userEvent.setup();
    const onDone = vi.fn();
    render(<PurgeResult context="channel" result={result} onDone={onDone} />);

    await user.click(screen.getByRole('button', { name: 'Done' }));
    expect(onDone).toHaveBeenCalledTimes(1);
  });
});
