// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

let dir: string;
vi.mock('electron', () => ({ app: { getPath: () => dir } }));

import {
  readApprovalsFile,
  appendApprovalRecord,
  findApprovalRecord,
  _resetApprovalsCacheForTesting,
} from '@/main/selfHostedApprovals';

const record = (origin: string) => ({
  origin,
  approvedAt: '2026-07-30T12:00:00.000Z',
  lastSeenAddress: '10.0.0.5',
  tierAtApproval: 'tier2' as const,
});

describe('selfHostedApprovals', () => {
  beforeEach(() => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), 'approvals-'));
    _resetApprovalsCacheForTesting();
  });
  afterEach(() => fs.rmSync(dir, { recursive: true, force: true }));

  it('returns the empty set when the file is missing (first run is normal)', () => {
    expect(readApprovalsFile()).toEqual([]);
  });

  it('writes 0o600 and round-trips a record', () => {
    expect(appendApprovalRecord(record('https://concord.lan:8443'))).toBe(true);
    _resetApprovalsCacheForTesting();
    expect(readApprovalsFile()).toEqual([record('https://concord.lan:8443')]);
    const mode = fs.statSync(path.join(dir, 'self-hosted-approvals.json')).mode & 0o777;
    expect(mode).toBe(0o600);
  });

  it.each([
    ['not json', 'xxxxx'],
    ['wrong version', JSON.stringify({ version: 2, approvals: [] })],
    ['non-array approvals', JSON.stringify({ version: 1, approvals: {} })],
    [
      'origin mismatch',
      JSON.stringify({ version: 1, approvals: [{ ...record('https://concord.lan/path') }] }),
    ],
  ])('fails to the empty set on %s (never partial)', (_label, contents) => {
    fs.writeFileSync(path.join(dir, 'self-hosted-approvals.json'), contents);
    _resetApprovalsCacheForTesting();
    expect(readApprovalsFile()).toEqual([]);
  });

  it('never salvages the valid half of a partially malformed file', () => {
    fs.writeFileSync(
      path.join(dir, 'self-hosted-approvals.json'),
      JSON.stringify({
        version: 1,
        approvals: [record('https://good.lan'), { origin: 'not a url' }],
      })
    );
    _resetApprovalsCacheForTesting();
    expect(readApprovalsFile()).toEqual([]);
  });

  it('findApprovalRecord returns the MOST RECENT record for an origin', () => {
    const first = { ...record('https://homelab.lan'), tierAtApproval: 'public' as const };
    const second = { ...record('https://homelab.lan'), approvedAt: '2026-07-31T09:00:00.000Z' };
    expect(appendApprovalRecord(first)).toBe(true);
    expect(appendApprovalRecord({ ...record('https://other.lan') })).toBe(true);
    expect(appendApprovalRecord(second)).toBe(true);

    // Append-only file: a re-approval at a stronger tier must win over the earlier one.
    expect(findApprovalRecord('https://homelab.lan')).toEqual(second);
  });

  // A trust file is not a config file: anything wider than what this app can mint is
  // corrupt or hand-written, and a partial recovery of a trust file is a trust bug — so
  // each of these invalidates the WHOLE file, not just the offending record.
  describe('record validation bounds (#2354 review item 8)', () => {
    const write = (approvals: unknown[]) =>
      fs.writeFileSync(
        path.join(dir, 'self-hosted-approvals.json'),
        JSON.stringify({ version: 1, approvals })
      );

    it('rejects a non-http(s) origin that still round-trips through new URL().origin', () => {
      // `new URL('ftp://x.example').origin === 'ftp://x.example'`, so the canonicality
      // check alone admitted it.
      write([{ ...record('https://ok.lan'), origin: 'ftp://x.example' }]);
      expect(readApprovalsFile()).toEqual([]);
    });

    it('rejects an over-long origin', () => {
      write([{ ...record('https://ok.lan'), origin: `https://${'a'.repeat(400)}.lan` }]);
      expect(readApprovalsFile()).toEqual([]);
    });

    it.each(['approvedAt', 'lastSeenAddress'])('rejects an unbounded %s', (field) => {
      write([{ ...record('https://ok.lan'), [field]: 'x'.repeat(1000) }]);
      expect(readApprovalsFile()).toEqual([]);
    });

    it('rejects a records array past the cap', () => {
      write(Array.from({ length: 257 }, (_, i) => record(`https://h${i}.lan`)));
      expect(readApprovalsFile()).toEqual([]);
    });

    it('still accepts a full-size valid file at the cap', () => {
      write(Array.from({ length: 256 }, (_, i) => record(`https://h${i}.lan`)));
      expect(readApprovalsFile()).toHaveLength(256);
    });
  });

  it('findApprovalRecord returns undefined for an origin that was never approved', () => {
    expect(appendApprovalRecord(record('https://homelab.lan'))).toBe(true);
    expect(findApprovalRecord('https://stranger.lan')).toBeUndefined();
  });

  it('returns false and leaves no partial file when the write fails', () => {
    const spy = vi.spyOn(fs, 'renameSync').mockImplementation(() => {
      throw new Error('EIO');
    });
    try {
      expect(appendApprovalRecord(record('https://concord.lan'))).toBe(false);
      _resetApprovalsCacheForTesting();
      expect(readApprovalsFile()).toEqual([]);
    } finally {
      spy.mockRestore();
    }
    expect(fs.readdirSync(dir)).toEqual([]);
  });
});
