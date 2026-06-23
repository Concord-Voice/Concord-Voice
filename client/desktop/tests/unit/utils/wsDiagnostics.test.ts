import { describe, expect, it } from 'vitest';
import { summarizeWsDiagnostic, summarizeWsServerError } from '@/renderer/utils/wsDiagnostics';

describe('summarizeWsDiagnostic', () => {
  it('summarizes CloseEvent metadata without logging the raw event', () => {
    expect(
      summarizeWsDiagnostic(new CloseEvent('close', { code: 1006, reason: 'abnormal' }))
    ).toEqual({
      type: 'CloseEvent',
      event: 'close',
      code: 1006,
      reason: 'abnormal',
    });
  });

  it('omits blank CloseEvent reasons', () => {
    expect(summarizeWsDiagnostic(new CloseEvent('close', { code: 1005 }))).toEqual({
      type: 'CloseEvent',
      event: 'close',
      code: 1005,
    });
  });

  it('summarizes Error instances with the redacted message helper', () => {
    expect(summarizeWsDiagnostic(new TypeError('socket failed'))).toEqual({
      type: 'TypeError',
      message: 'socket failed',
    });
  });

  it('summarizes generic Event instances by constructor and event type', () => {
    expect(summarizeWsDiagnostic(new Event('error'))).toEqual({
      type: 'Event',
      event: 'error',
    });
  });

  it('falls back to the value type for non-event values', () => {
    expect(summarizeWsDiagnostic(undefined)).toEqual({ type: 'undefined' });
    expect(summarizeWsDiagnostic('error')).toEqual({ type: 'string' });
  });
});

describe('summarizeWsServerError', () => {
  it('summarizes whitelisted server error fields', () => {
    expect(
      summarizeWsServerError({
        code: 'stale_epoch',
        channel_id: 'channel-1',
        current_epoch: 12,
      })
    ).toEqual({
      type: 'server_error',
      code: 'stale_epoch',
      channelId: 'channel-1',
      currentEpoch: 12,
    });
  });

  it('uses unknown for missing or non-string server codes', () => {
    expect(summarizeWsServerError({})).toEqual({
      type: 'server_error',
      code: 'unknown',
    });
    expect(summarizeWsServerError({ code: 401 })).toEqual({
      type: 'server_error',
      code: 'unknown',
    });
  });
});
