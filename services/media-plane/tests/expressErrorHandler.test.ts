import { describe, it, expect, vi } from 'vitest';
import type { Request, Response, NextFunction } from 'express';
import type { Logger } from 'winston';

import { createExpressErrorHandler } from '../src/lib/expressErrorHandler.js';

function makeMockLogger(): Logger {
  return { error: vi.fn() } as unknown as Logger;
}

describe('createExpressErrorHandler', () => {
  it('logs the error and returns 500 JSON when headers are not yet sent', () => {
    const log = makeMockLogger();
    const handler = createExpressErrorHandler(log);

    const status = vi.fn().mockReturnThis();
    const json = vi.fn();
    const res = { headersSent: false, status, json } as unknown as Response;
    const req = {} as Request;
    const next = vi.fn() as unknown as NextFunction;

    handler(new Error('boom'), req, res, next);

    expect(log.error).toHaveBeenCalledWith(
      'Unhandled Express error',
      expect.objectContaining({ error: 'boom' })
    );
    expect(status).toHaveBeenCalledWith(500);
    expect(json).toHaveBeenCalledWith({ error: 'Internal server error' });
    expect(next).not.toHaveBeenCalled();
  });

  it('delegates to next(err) when headers are already sent', () => {
    const log = makeMockLogger();
    const handler = createExpressErrorHandler(log);

    const status = vi.fn().mockReturnThis();
    const json = vi.fn();
    const res = { headersSent: true, status, json } as unknown as Response;
    const req = {} as Request;
    const next = vi.fn() as unknown as NextFunction;
    const err = new Error('late');

    handler(err, req, res, next);

    expect(log.error).toHaveBeenCalled();
    expect(status).not.toHaveBeenCalled();
    expect(json).not.toHaveBeenCalled();
    expect(next).toHaveBeenCalledWith(err);
  });

  it('forwards a stack trace in the log payload when err is an Error', () => {
    const log = makeMockLogger();
    const handler = createExpressErrorHandler(log);

    const err = new Error('with-stack');
    const res = {
      headersSent: false,
      status: vi.fn().mockReturnThis(),
      json: vi.fn(),
    } as unknown as Response;

    handler(err, {} as Request, res, vi.fn() as unknown as NextFunction);

    expect(log.error).toHaveBeenCalledWith(
      'Unhandled Express error',
      expect.objectContaining({ stack: err.stack })
    );
  });

  it('handles a string thrown via next("boom") without crashing', () => {
    const log = makeMockLogger();
    const handler = createExpressErrorHandler(log);

    const status = vi.fn().mockReturnThis();
    const json = vi.fn();
    const res = { headersSent: false, status, json } as unknown as Response;

    handler('boom', {} as Request, res, vi.fn() as unknown as NextFunction);

    expect(log.error).toHaveBeenCalledWith(
      'Unhandled Express error',
      expect.objectContaining({ error: 'boom', stack: undefined })
    );
    expect(status).toHaveBeenCalledWith(500);
    expect(json).toHaveBeenCalledWith({ error: 'Internal server error' });
  });

  it('handles a non-Error object via JSON.stringify fallback', () => {
    const log = makeMockLogger();
    const handler = createExpressErrorHandler(log);

    const status = vi.fn().mockReturnThis();
    const json = vi.fn();
    const res = { headersSent: false, status, json } as unknown as Response;

    handler(
      { code: 404, message: 'not found' },
      {} as Request,
      res,
      vi.fn() as unknown as NextFunction
    );

    expect(log.error).toHaveBeenCalledWith(
      'Unhandled Express error',
      expect.objectContaining({ error: '{"code":404,"message":"not found"}' })
    );
    expect(status).toHaveBeenCalledWith(500);
  });

  it("forwards a non-Error object's string stack property when present", () => {
    const log = makeMockLogger();
    const handler = createExpressErrorHandler(log);

    const status = vi.fn().mockReturnThis();
    const json = vi.fn();
    const res = { headersSent: false, status, json } as unknown as Response;

    const errLike = { code: 500, stack: 'fake-stack-trace-line\n at <anon>' };

    handler(errLike, {} as Request, res, vi.fn() as unknown as NextFunction);

    expect(log.error).toHaveBeenCalledWith(
      'Unhandled Express error',
      expect.objectContaining({ stack: 'fake-stack-trace-line\n at <anon>' })
    );
  });

  it('survives a circular-reference object (JSON.stringify would throw)', () => {
    const log = makeMockLogger();
    const handler = createExpressErrorHandler(log);

    const status = vi.fn().mockReturnThis();
    const json = vi.fn();
    const res = { headersSent: false, status, json } as unknown as Response;

    interface Circular {
      self?: Circular;
    }
    const circular: Circular = {};
    circular.self = circular;

    expect(() =>
      handler(circular, {} as Request, res, vi.fn() as unknown as NextFunction)
    ).not.toThrow();

    expect(log.error).toHaveBeenCalled();
    // Falls back to String(err) which yields '[object Object]'.
    expect(log.error).toHaveBeenCalledWith(
      'Unhandled Express error',
      expect.objectContaining({ error: '[object Object]' })
    );
    expect(status).toHaveBeenCalledWith(500);
    expect(json).toHaveBeenCalledWith({ error: 'Internal server error' });
  });

  it('survives a BigInt error value (JSON.stringify would throw)', () => {
    const log = makeMockLogger();
    const handler = createExpressErrorHandler(log);

    const status = vi.fn().mockReturnThis();
    const json = vi.fn();
    const res = { headersSent: false, status, json } as unknown as Response;

    expect(() =>
      handler(42n, {} as Request, res, vi.fn() as unknown as NextFunction)
    ).not.toThrow();

    expect(log.error).toHaveBeenCalledWith(
      'Unhandled Express error',
      expect.objectContaining({ error: '42' })
    );
    expect(status).toHaveBeenCalledWith(500);
  });

  it('survives an undefined err (JSON.stringify returns undefined, not a string)', () => {
    const log = makeMockLogger();
    const handler = createExpressErrorHandler(log);

    const status = vi.fn().mockReturnThis();
    const json = vi.fn();
    const res = { headersSent: false, status, json } as unknown as Response;

    expect(() =>
      handler(undefined, {} as Request, res, vi.fn() as unknown as NextFunction)
    ).not.toThrow();

    expect(log.error).toHaveBeenCalledWith(
      'Unhandled Express error',
      expect.objectContaining({ error: 'undefined' })
    );
    expect(status).toHaveBeenCalledWith(500);
  });
});
