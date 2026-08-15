import React from 'react';
import type { PurgeContext, TerminalPurgeResult } from '../../services/purgeApi';

interface PurgeResultProps {
  context: PurgeContext;
  result: TerminalPurgeResult;
  onDone: () => void;
}

/**
 * Render a Retry-After delay as a phrase. The budget itself is never named —
 * PURGE_RATE_LIMIT is operator-tunable, so any fixed allowance in copy is a
 * drift bug on a non-default deployment. Rounds up, so the countdown never
 * invites a retry the server would still refuse.
 */
export function formatRetryAfter(seconds: number): string | null {
  if (!Number.isFinite(seconds) || seconds <= 0) return null;
  if (seconds < 60) {
    const whole = Math.ceil(seconds);
    return `${whole} ${whole === 1 ? 'second' : 'seconds'}`;
  }
  if (seconds < 3600) {
    const minutes = Math.ceil(seconds / 60);
    return `${minutes} ${minutes === 1 ? 'minute' : 'minutes'}`;
  }
  const hours = Math.ceil(seconds / 3600);
  return `${hours} ${hours === 1 ? 'hour' : 'hours'}`;
}

/**
 * 404 copy, per context. purgeApi maps the status before any context branch, so
 * the noun is chosen here — the same split SuccessBody below already makes.
 */
const GONE_COPY: Record<PurgeContext, string> = {
  channel: 'This channel no longer exists.',
  server: 'This server no longer exists.',
  dm: 'This conversation no longer exists.',
  group: 'This conversation no longer exists.',
};

/**
 * Success text. A server-wide purge broadcasts deleted_count 0 per channel by
 * design, so its count is never rendered (copy deck §4).
 */
function SuccessBody({
  context,
  deletedCount,
  hiddenCount,
}: Readonly<{
  context: PurgeContext;
  deletedCount: number;
  hiddenCount: number;
}>) {
  if (context === 'server') {
    return (
      <output className="purge-modal__status">
        Messages purged. Channels you cannot moderate were skipped.
      </output>
    );
  }
  // An authorized purge of an already-empty scope returns 200 with zero. It is
  // a success, not an error and not an empty state to apologise for.
  if (deletedCount === 0 && hiddenCount === 0) {
    return (
      <output className="purge-modal__status">
        No messages matched that range. Nothing to purge.
      </output>
    );
  }
  return (
    <output className="purge-modal__status">
      {`Purged ${deletedCount} ${deletedCount === 1 ? 'message' : 'messages'}.`}
      {hiddenCount > 0 && ` ${hiddenCount} more hidden from you.`}
    </output>
  );
}

function ResultBody({
  context,
  result,
}: Readonly<{ context: PurgeContext; result: TerminalPurgeResult }>) {
  switch (result.kind) {
    case 'success':
      return (
        <SuccessBody
          context={context}
          deletedCount={result.deletedCount}
          hiddenCount={result.hiddenCount}
        />
      );
    case 'rateLimited': {
      const countdown =
        result.retryAfterSeconds === undefined ? null : formatRetryAfter(result.retryAfterSeconds);
      return (
        <p role="alert">
          {countdown === null
            ? 'Purge limit reached — try again later.'
            : `Purge limit reached. Try again in ${countdown}.`}
        </p>
      );
    }
    case 'unavailable':
      // The limiter failed closed because its backend is unreachable. The user
      // hit no limit, so no countdown exists and none is invented.
      return <p role="alert">Temporarily unavailable. Try again shortly.</p>;
    case 'notFound':
      return <p role="alert">{GONE_COPY[context]}</p>;
    case 'sessionExpired':
      // A 401 is refused before the handler runs, so — unlike `partial` — this
      // may and must say that nothing was deleted.
      return (
        <p role="alert">Your session expired. Nothing was purged. Sign in again, then try again.</p>
      );
    case 'networkError':
      // A transport rejection cannot distinguish "never sent" from "sent, the
      // server committed, the response was lost". On an irreversible operation
      // the second case makes "Nothing was purged" the more dangerous claim, so
      // this reads as uncertainty and refetches like `partial`.
      return (
        <>
          <p role="alert">
            We couldn&apos;t reach the server, so we can&apos;t confirm what happened —{' '}
            <strong>some messages may already have been purged</strong>.
          </p>
          <output className="purge-modal__status">We&apos;re refreshing this view.</output>
        </>
      );
    case 'unexpectedError':
      // Unlike `networkError`, a response DID arrive: every status that reaches
      // this branch is a non-5xx refusal the server made before the purge
      // handler could delete anything (invalid range, unroutable method), so —
      // like `sessionExpired` — it may say that nothing was purged.
      return <p role="alert">Something went wrong. Nothing was purged.</p>;
    case 'forbidden':
      // Deliberately generic — the server keeps unauthorized and nonexistent
      // indistinguishable on the server-purge path.
      return (
        <p role="alert">
          This purge couldn&apos;t be completed. You may not have permission for this scope.
        </p>
      );
    case 'partial':
      // A 500 can arrive AFTER batches were irreversibly deleted, so this must
      // never read as "nothing was deleted".
      return (
        <>
          <p role="alert">
            Something went wrong, but <strong>some messages may already have been purged</strong>.
          </p>
          <output className="purge-modal__status">We&apos;re refreshing this view.</output>
        </>
      );
  }
}

const PurgeResult: React.FC<PurgeResultProps> = ({ context, result, onDone }) => (
  <div className="purge-modal__result">
    <ResultBody context={context} result={result} />
    <div className="purge-modal__actions">
      <button type="button" className="purge-modal__done" onClick={onDone}>
        Done
      </button>
    </div>
  </div>
);

export default PurgeResult;
