// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { parse as parseYaml } from 'yaml';
import { renderAppUpdateYaml } from './generate-app-update.mts';
import { UPDATE_ENDPOINT_URL } from '../src/constants/updateEndpoint.mts';

describe('UPDATE_ENDPOINT_URL', () => {
  it('uses HTTPS', () => {
    expect(UPDATE_ENDPOINT_URL.startsWith('https://')).toBe(true);
  });

  it('points at api.example.com', () => {
    const parsed = new URL(UPDATE_ENDPOINT_URL);
    expect(parsed.host).toBe('api.example.com');
  });

  it('targets the /api/v1/updates path', () => {
    const parsed = new URL(UPDATE_ENDPOINT_URL);
    expect(parsed.pathname).toBe('/api/v1/updates');
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

  it('produces YAML with exactly the three expected keys', () => {
    const yaml = renderAppUpdateYaml();
    const parsed = parseYaml(yaml);
    expect(Object.keys(parsed).sort()).toEqual(['provider', 'updaterCacheDirName', 'url']);
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
