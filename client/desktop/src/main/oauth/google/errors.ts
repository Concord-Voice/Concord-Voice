import type { SSOSignInErrorCode } from '../../../shared/sso';

export class GoogleFlowError extends Error {
  readonly code: SSOSignInErrorCode;
  readonly stage: string;
  constructor(code: SSOSignInErrorCode, stage: string) {
    super(code); // message IS the code — never leaks PII (observability §1)
    this.name = 'GoogleFlowError';
    this.code = code;
    this.stage = stage;
  }
}
