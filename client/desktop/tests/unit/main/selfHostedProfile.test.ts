// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('electron', () => ({
  app: { getPath: () => '/tmp/td' },
}));

import {
  _resetSelfHostedProfileForTesting,
  isValidatedSelfHostedApiBase,
  profileIdForApiBase,
  profilePathsForApiBase,
  rememberValidatedSelfHostedApiBase,
} from '../../../src/main/selfHostedProfile';

describe('selfHostedProfile', () => {
  beforeEach(() => {
    _resetSelfHostedProfileForTesting();
  });

  it('keeps SaaS token, metadata, E2EE, and machine-id files at the pinned root', () => {
    expect(profileIdForApiBase('https://api.concordvoice.chat')).toBe('saas');

    expect(profilePathsForApiBase('https://api.concordvoice.chat')).toEqual({
      tokenFile: '/tmp/td/secure-token.dat',
      metaFile: '/tmp/td/token-meta.json',
      e2eeFile: '/tmp/td/secure-e2ee.dat',
      machineIdFile: '/tmp/td/machine-id.json',
    });
  });

  it('maps a self-hosted origin into a stable hashed profile directory', () => {
    const first = profilePathsForApiBase('https://homelab.lan');
    const second = profilePathsForApiBase('https://homelab.lan/');

    expect(profileIdForApiBase('https://homelab.lan')).toMatch(/^selfhost-[0-9a-f]{16}$/);
    expect(first).toEqual(second);
    expect(first.tokenFile).toMatch(/^\/tmp\/td\/profiles\/[0-9a-f]{64}\/secure-token\.dat$/);
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

  it('remembers only successfully validated self-hosted origins', () => {
    expect(isValidatedSelfHostedApiBase('https://homelab.lan')).toBe(false);

    rememberValidatedSelfHostedApiBase('https://homelab.lan/path-that-gets-normalized-away');

    expect(isValidatedSelfHostedApiBase('https://homelab.lan')).toBe(true);
    expect(isValidatedSelfHostedApiBase('https://workshop.lan')).toBe(false);
  });
});
