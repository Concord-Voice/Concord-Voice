// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { parse as parseYaml } from 'yaml';
import { renderAppUpdateYaml } from './generate-app-update.mts';
import { UPDATE_ENDPOINT_URL } from '../src/constants/updateEndpoint.mts';
import { ALLOWED_WINDOWS_PUBLISHERS } from '../src/constants/allowedWindowsPublishers.mts';

describe('UPDATE_ENDPOINT_URL', () => {
  it('uses HTTPS', () => {
    expect(UPDATE_ENDPOINT_URL.startsWith('https://')).toBe(true);
  });

  it('points at the public GitHub mirror', () => {
    const parsed = new URL(UPDATE_ENDPOINT_URL);
    expect(parsed.host).toBe('github.com');
  });

  it('targets the public release latest/download path', () => {
    const parsed = new URL(UPDATE_ENDPOINT_URL);
    expect(parsed.pathname).toBe('/Concord-Voice/Concord-Voice/releases/latest/download');
  });

  it('has no query string or fragment', () => {
    const parsed = new URL(UPDATE_ENDPOINT_URL);
    expect(parsed.search).toBe('');
    expect(parsed.hash).toBe('');
  });
});

describe('renderAppUpdateYaml', () => {
  it('emits the generic provider', () => {
    const yaml = renderAppUpdateYaml();
    const parsed = parseYaml(yaml);
    expect(parsed.provider).toBe('generic');
  });

  it('emits the UPDATE_ENDPOINT_URL when called with no args', () => {
    const yaml = renderAppUpdateYaml();
    const parsed = parseYaml(yaml);
    expect(parsed.url).toBe(UPDATE_ENDPOINT_URL);
  });

  it('accepts an explicit url override (for testing/staging)', () => {
    const override = 'https://staging.example.test/api/v1/updates';
    const yaml = renderAppUpdateYaml(override);
    const parsed = parseYaml(yaml);
    expect(parsed.url).toBe(override);
  });

  it('produces YAML with exactly the four expected keys', () => {
    const yaml = renderAppUpdateYaml();
    const parsed = parseYaml(yaml);
    expect(Object.keys(parsed).sort()).toEqual([
      'provider',
      'publisherName',
      'updaterCacheDirName',
      'url',
    ]);
  });

  it('emits the Windows publisher allow-list as a YAML sequence (#2020)', () => {
    const parsed = parseYaml(renderAppUpdateYaml());
    // electron-updater's NsisUpdater.verifySignature reads publisherName ONLY
    // from this on-disk file (never from setFeedURL); presence here is what
    // arms the Windows install-time Authenticode gate.
    expect(parsed.publisherName).toEqual(['Concord Voice LLC']);
  });

  it('keeps the emitted allow-list in lockstep with the runtime constant', () => {
    const parsed = parseYaml(renderAppUpdateYaml());
    expect(parsed.publisherName).toEqual([...ALLOWED_WINDOWS_PUBLISHERS]);
  });

  it('emits only non-empty string publisher names', () => {
    const parsed = parseYaml(renderAppUpdateYaml());
    expect(Array.isArray(parsed.publisherName)).toBe(true);
    expect(parsed.publisherName.length).toBeGreaterThan(0);
    for (const name of parsed.publisherName) {
      expect(typeof name).toBe('string');
      expect(name.trim().length).toBeGreaterThan(0);
    }
  });

  it('pins the updater cache dir to the spaceless ConcordVoice', () => {
    const yaml = renderAppUpdateYaml();
    const parsed = parseYaml(yaml);
    // Decouples electron-updater's cache dir from app.getName()/productName so a
    // display-name change never churns ~/Library/Caches/<name>. See ADR-0020 D1.
    expect(parsed.updaterCacheDirName).toBe('ConcordVoice');
  });

  it('ends with a trailing newline (POSIX text-file convention)', () => {
    const yaml = renderAppUpdateYaml();
    expect(yaml.endsWith('\n')).toBe(true);
  });
});
