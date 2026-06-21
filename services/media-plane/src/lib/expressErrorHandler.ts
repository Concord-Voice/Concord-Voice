import type { Request, Response, NextFunction } from 'express';
import type { Logger } from 'winston';

/**
 * Express error handler that surfaces uncaught route errors via Winston
 * and returns a canonical 500 response. Replaces the previous handler
 * after the strip in #759 — see
 * [internal]specs/2026-04-27-759-strip-sentry-media-plane-design.md §4.2.
 *
 * Express's runtime error-handler contract allows `err` to be any value
 * (a route may call `next('boom')` or `next({ code: 404 })`), so this
 * factory accepts `unknown` and derives `message`/`stack` defensively.
 * When headers are already sent the request is delegated to Express's
 * default error handler via `next(err)` so the connection is torn down
 * cleanly rather than silently absorbed.
 */

/**
 * Derive a printable message from any value Express might pass to an
 * error handler. Hardened against `JSON.stringify` failure modes:
 *
 *   - **Throws** for circular references and BigInt values.
 *   - **Returns `undefined`** for `undefined`, functions, and symbols.
 *
 * The Express error handler is the last line of defense — if THIS
 * function throws, the surrounding 500 response and Winston log are
 * lost too. So serialization is wrapped in try/catch with a `String(err)`
 * fallback (also guarded), and a final `[unprintable error]` sentinel.
 *
 * Also extracted to its own function to avoid nested ternaries
 * (SonarQube `typescript:S3358`).
 */
function deriveMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;

  try {
    const serialized = JSON.stringify(err);
    if (typeof serialized === 'string') return serialized;
  } catch {
    // JSON.stringify can throw on circular refs / BigInt — fall through.
  }

  try {
    return String(err);
  } catch {
    return '[unprintable error]';
  }
}

/**
 * Derive a stack-trace string from any value Express might pass.
 * Returns `undefined` when no stack is available. Extracted to avoid
 * nested ternaries (SonarQube `typescript:S3358`).
 */
function deriveStack(err: unknown): string | undefined {
  if (err instanceof Error) return err.stack;
  if (
    typeof err === 'object' &&
    err !== null &&
    'stack' in err &&
    typeof err.stack === 'string'
  ) {
    return err.stack;
  }
  return undefined;
}

export function createExpressErrorHandler(logger: Logger) {
  return (err: unknown, _req: Request, res: Response, next: NextFunction): void => {
    logger.error('Unhandled Express error', {
      error: deriveMessage(err),
      stack: deriveStack(err),
    });

    if (res.headersSent) {
      next(err);
      return;
    }

    res.status(500).json({ error: 'Internal server error' });
  };
}
