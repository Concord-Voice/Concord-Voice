// @vitest-environment node
import { describe, it, expect, beforeAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { readFileSync, existsSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const projectRoot = resolve(__dirname, '../..');
const preloadPath = resolve(projectRoot, 'dist/preload/preload.js');

describe('preload sandbox contract (integration)', () => {
  beforeAll(() => {
    // Ensure preload bundle exists (CI runs tests before build).
    // Surfaces stderr on failure so developers can diagnose build issues from test output.
    try {
      execFileSync('npm', ['run', 'build:preload'], { cwd: projectRoot, stdio: 'pipe' });
    } catch (err: unknown) {
      const stderr =
        err instanceof Error && 'stderr' in err
          ? String((err as NodeJS.ErrnoException & { stderr: Buffer }).stderr)
          : '';
      throw new Error(`preload build failed:\n${stderr}`);
    }
  });

  it('dist/preload/preload.js exists after build', () => {
    expect(existsSync(preloadPath)).toBe(true);
  });

  it('contains no relative require calls (sandbox-incompatible)', () => {
    const content = readFileSync(preloadPath, 'utf-8');
    const relativeRequires = content.match(/require\(["']\.\.\//g) || [];
    const localRequires = content.match(/require\(["']\.\//g) || [];
    expect([...relativeRequires, ...localRequires]).toHaveLength(0);
  });

  it('only requires electron as a runtime external', () => {
    const content = readFileSync(preloadPath, 'utf-8');
    const requires = [...content.matchAll(/require\(["']([^"']+)["']\)/g)].map((m) => m[1]);
    const nonElectron = requires.filter((r) => r !== 'electron');
    expect(nonElectron).toEqual([]);
  });

  it('preserves contextBridge.exposeInMainWorld', () => {
    const content = readFileSync(preloadPath, 'utf-8');
    expect(content).toContain('exposeInMainWorld');
  });

  it('exposes credential-owner CAS channels in the sandbox bundle', () => {
    const content = readFileSync(preloadPath, 'utf-8');
    expect(content).toContain('auth:storeRefreshToken');
    expect(content).toContain('auth:clearTokensIfOwner');
    expect(content).toContain('auth:storeE2EEKeysIfOwner');
  });

  it('exposes main-owned SSO completion channels in the sandbox bundle', () => {
    const content = readFileSync(preloadPath, 'utf-8');
    expect(content).toContain('sso:completeRegistration');
    expect(content).toContain('sso:completeLink');
    // #2424 added this channel to the bridge but never to this lock — added
    // while touching the file, since this is exactly the test that exists to
    // catch a bridge channel going missing from the built preload.
    expect(content).toContain('sso:completeMFA');
    // #2394
    expect(content).toContain('sso:abandonReservation');
  });
});
