/**
 * AppleFlowError — stage-tagged error for the Apple sign-in orchestration
 * (#974). `code` is the renderer-visible taxonomy value (spec §Failure
 * taxonomy); `stage` is main-process log context only.
 *
 * The message IS the code, so an accidental `.message` log stays PII- and
 * token-free by construction ([internal]rules/observability.md — no raw
 * errors, no Error.cause chains to any sink).
 */
import type { AppleSignInErrorCode } from '../../../shared/appleSso';

export class AppleFlowError extends Error {
  readonly code: AppleSignInErrorCode;
  readonly stage: string;

  constructor(code: AppleSignInErrorCode, stage: string) {
    super(code);
    this.name = 'AppleFlowError';
    this.code = code;
    this.stage = stage;
  }
}
