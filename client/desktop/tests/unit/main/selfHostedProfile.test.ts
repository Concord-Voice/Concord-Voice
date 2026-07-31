// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from 'vitest';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// A real temp dir so the durable approval store round-trips through this suite.
// The factory closure reads `dir` lazily (at getPath() call time), so the hoisted
// vi.mock does not observe the TDZ.
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'profile-'));
vi.mock('electron', () => ({ app: { getPath: () => dir } }));

import {
  _resetSelfHostedProfileForTesting,
  approvalTierForApiBase,
  beginPendingApproval,
  clearPendingApproval,
  commitSelfHostedApproval,
  isTier2DialApproved,
  isValidatedSelfHostedApiBase,
  loadApprovedSelfHostedOrigins,
  profileIdForApiBase,
  profilePathsForApiBase,
} from '../../../src/main/selfHostedProfile';

describe('selfHostedProfile', () => {
  beforeEach(() => {
    _resetSelfHostedProfileForTesting();
  });

  it('keeps SaaS token, metadata, E2EE, and machine-id files at the pinned root', () => {
    expect(profileIdForApiBase('https://api.concordvoice.chat')).toBe('saas');

    expect(profilePathsForApiBase('https://api.concordvoice.chat')).toEqual({
      tokenFile: path.join(dir, 'secure-token.dat'),
      metaFile: path.join(dir, 'token-meta.json'),
      e2eeFile: path.join(dir, 'secure-e2ee.dat'),
      machineIdFile: path.join(dir, 'machine-id.json'),
    });
  });

  it('maps a self-hosted origin into a stable hashed profile directory', () => {
    const first = profilePathsForApiBase('https://homelab.lan');
    const second = profilePathsForApiBase('https://homelab.lan/');

    expect(profileIdForApiBase('https://homelab.lan')).toMatch(/^selfhost-[0-9a-f]{16}$/);
    expect(first).toEqual(second);
    expect(first.tokenFile).toMatch(
      new RegExp(
        `^${dir.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}/profiles/[0-9a-f]{64}/secure-token\\.dat$`
      )
    );
    expect(first.metaFile).toBe(first.tokenFile.replace('secure-token.dat', 'token-meta.json'));
    expect(first.e2eeFile).toBe(first.tokenFile.replace('secure-token.dat', 'secure-e2ee.dat'));
    expect(first.machineIdFile).toBe(
      first.tokenFile.replace('secure-token.dat', 'machine-id.json')
    );
  });

  it('separates different self-hosted origins', () => {
    const a = profilePathsForApiBase('https://homelab.lan');
    const b = profilePathsForApiBase('https://workshop.lan');

    expect(a.tokenFile).not.toBe(b.tokenFile);
    expect(a.machineIdFile).not.toBe(b.machineIdFile);
  });

  it('commitSelfHostedApproval is the single writer of the validated-origin set', () => {
    expect(isValidatedSelfHostedApiBase('https://homelab.lan')).toBe(false);
    expect(commitSelfHostedApproval('https://homelab.lan', '10.0.0.5')).toBe(true);
    expect(isValidatedSelfHostedApiBase('https://homelab.lan')).toBe(true);
    expect(isValidatedSelfHostedApiBase('https://workshop.lan')).toBe(false);
  });

  it('never mints the SaaS origin', () => {
    expect(commitSelfHostedApproval('https://api.concordvoice.chat', '1.2.3.4')).toBe(true);
    expect(isValidatedSelfHostedApiBase('https://api.concordvoice.chat')).toBe(false);
  });

  it('does not mint the in-memory set when the durable write fails', () => {
    const spy = vi.spyOn(fs, 'renameSync').mockImplementation(() => {
      throw new Error('EIO');
    });
    try {
      expect(commitSelfHostedApproval('https://unwritable.lan', '10.0.0.7')).toBe(false);
      expect(isValidatedSelfHostedApiBase('https://unwritable.lan')).toBe(false);
    } finally {
      spy.mockRestore();
    }
  });

  // #2354 follow-up: origin trust and tier-2 DIAL permission are two grants, and the
  // ceremony's displayed address class is what bounds the second one.
  describe('consent tier bounds the dial', () => {
    it('records a public-address ceremony as public and refuses a tier-2 dial', () => {
      expect(commitSelfHostedApproval('https://publicly-seen.lan', '203.0.113.10')).toBe(true);

      expect(isValidatedSelfHostedApiBase('https://publicly-seen.lan')).toBe(true); // custody: yes
      expect(approvalTierForApiBase('https://publicly-seen.lan')).toBe('public');
      expect(isTier2DialApproved('https://publicly-seen.lan')).toBe(false); // dial: no
    });

    it('records a private-address ceremony as tier2 and permits the dial', () => {
      expect(commitSelfHostedApproval('https://nas.lan', '10.0.0.9')).toBe(true);

      expect(approvalTierForApiBase('https://nas.lan')).toBe('tier2');
      expect(isTier2DialApproved('https://nas.lan')).toBe(true);
    });

    it('treats loopback and CGNAT ceremonies as tier2 too', () => {
      expect(commitSelfHostedApproval('http://localhost:8443', '127.0.0.1')).toBe(true);
      expect(commitSelfHostedApproval('https://cgnat.lan', '100.64.0.7')).toBe(true);

      expect(isTier2DialApproved('http://localhost:8443')).toBe(true);
      expect(isTier2DialApproved('https://cgnat.lan')).toBe(true);
    });

    it('a later tier-2 re-approval supersedes the earlier public one', () => {
      expect(commitSelfHostedApproval('https://moved.lan', '203.0.113.10')).toBe(true);
      expect(isTier2DialApproved('https://moved.lan')).toBe(false);

      // The server moved onto the LAN; the user re-ran the ceremony on that address.
      expect(commitSelfHostedApproval('https://moved.lan', '192.168.1.20')).toBe(true);

      expect(approvalTierForApiBase('https://moved.lan')).toBe('tier2');
      expect(isTier2DialApproved('https://moved.lan')).toBe(true);
    });

    it('fails closed for an unapproved origin and for SaaS', () => {
      expect(approvalTierForApiBase('https://stranger.lan')).toBeNull();
      expect(isTier2DialApproved('https://stranger.lan')).toBe(false);

      commitSelfHostedApproval('https://api.concordvoice.chat', '1.2.3.4');
      expect(approvalTierForApiBase('https://api.concordvoice.chat')).toBeNull();
      expect(isTier2DialApproved('https://api.concordvoice.chat')).toBe(false);
    });

    it('does not carry the tier across a cleared in-memory set until rehydration', () => {
      commitSelfHostedApproval('https://persisted-tier.lan', '10.0.0.9');
      _resetSelfHostedProfileForTesting();

      expect(isTier2DialApproved('https://persisted-tier.lan')).toBe(false);
      loadApprovedSelfHostedOrigins();
      expect(isTier2DialApproved('https://persisted-tier.lan')).toBe(true);
    });
  });

  // The grant a ceremony hands the probe is provisional: it must authorize the dial the
  // probe needs and NOTHING else, and it must not outlive that probe.
  describe('provisional ceremony grant (#2354 review item 3)', () => {
    const origin = 'https://pending.lan';

    it('authorizes the tier-2 dial for its own origin only, and never origin trust', () => {
      expect(isTier2DialApproved(origin)).toBe(false);

      beginPendingApproval(origin, '10.0.0.5');
      expect(isTier2DialApproved(origin)).toBe(true);
      // Scoped to the exact origin the ceremony asked about.
      expect(isTier2DialApproved('https://other-pending.lan')).toBe(false);
      // A dial is not a credential store: nothing durable was minted.
      expect(isValidatedSelfHostedApiBase(origin)).toBe(false);
      expect(approvalTierForApiBase(origin)).toBeNull();
    });

    it('is revoked by clearPendingApproval, so a failed probe leaves no dial permission', () => {
      beginPendingApproval(origin, '10.0.0.5');
      clearPendingApproval();
      expect(isTier2DialApproved(origin)).toBe(false);
      expect(isValidatedSelfHostedApiBase(origin)).toBe(false);
    });

    it('is additive only — a public-address ceremony never grants a tier-2 dial', () => {
      beginPendingApproval(origin, '203.0.113.10');
      expect(isTier2DialApproved(origin)).toBe(false);
    });

    it('cannot narrow an existing durable tier-2 grant', () => {
      commitSelfHostedApproval(origin, '10.0.0.9');
      beginPendingApproval(origin, '203.0.113.10'); // weaker, concurrent ceremony
      expect(isTier2DialApproved(origin)).toBe(true);
    });
  });

  // Item 4: the tier fold used to be `=== 'public' ? 'public' : 'tier2'`, so a tier-1 or
  // unparseable address minted the MORE permissive label in the one function that grants
  // dial permission. Unreachable from main.ts today; locked so it stays a closed default.
  describe('unapprovable addresses mint nothing (#2354 review item 4)', () => {
    it.each([
      ['a tier-1 address', '169.254.169.254'],
      ['an unparseable address', 'not-an-ip'],
      ['an empty address', ''],
    ])('refuses to commit %s', (_label, address) => {
      const origin = `https://unapprovable-${address.length}.lan`;
      expect(commitSelfHostedApproval(origin, address)).toBe(false);
      expect(isValidatedSelfHostedApiBase(origin)).toBe(false);
      expect(approvalTierForApiBase(origin)).toBeNull();
    });

    it('grants no provisional dial for a tier-1 address either', () => {
      beginPendingApproval('https://unapprovable-pending.lan', '169.254.169.254');
      expect(isTier2DialApproved('https://unapprovable-pending.lan')).toBe(false);
    });
  });

  it('loadApprovedSelfHostedOrigins hydrates the set from the durable store', () => {
    commitSelfHostedApproval('https://persisted.lan', '10.0.0.9');
    _resetSelfHostedProfileForTesting(); // clears the in-memory set only
    expect(isValidatedSelfHostedApiBase('https://persisted.lan')).toBe(false);
    loadApprovedSelfHostedOrigins();
    expect(isValidatedSelfHostedApiBase('https://persisted.lan')).toBe(true);
  });
});
