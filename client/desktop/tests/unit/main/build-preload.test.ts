// @vitest-environment node
import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { readFileSync, existsSync, mkdtempSync, rmSync } from 'node:fs';
import { resolve, dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';

const __dirname = dirname(fileURLToPath(import.meta.url));
const projectRoot = resolve(__dirname, '../../..');
const scriptPath = resolve(projectRoot, 'scripts/build-preload.mjs');

describe('build-preload.mjs', () => {
  let tmpDir: string;
  let outFile: string;

  beforeAll(() => {
    tmpDir = mkdtempSync(join(tmpdir(), 'preload-test-'));
    outFile = join(tmpDir, 'preload.js');

    try {
      execFileSync('node', [scriptPath, '--prod'], {
        cwd: projectRoot,
        env: { ...process.env, PRELOAD_OUTDIR: tmpDir },
        stdio: 'pipe',
      });
    } catch (err: unknown) {
      const stderr =
        err instanceof Error && 'stderr' in err
          ? String((err as NodeJS.ErrnoException & { stderr: Buffer }).stderr)
          : '';
      throw new Error(`build-preload.mjs failed:\n${stderr}`);
    }
  });

  afterAll(() => {
    if (tmpDir && existsSync(tmpDir)) {
      rmSync(tmpDir, { recursive: true });
    }
  });

  it('produces an output file', () => {
    expect(existsSync(outFile)).toBe(true);
  });

  it('output is CommonJS', () => {
    const content = readFileSync(outFile, 'utf-8');
    expect(
      content.includes('module.exports') ||
        content.includes('exports.') ||
        content.includes('Object.defineProperty(exports')
    ).toBe(true);
  });

  it('contains no relative require calls', () => {
    const content = readFileSync(outFile, 'utf-8');
    const relativeRequires = content.match(/require\(["']\.\.\//g) || [];
    const localRequires = content.match(/require\(["']\.\//g) || [];
    expect(relativeRequires).toHaveLength(0);
    expect(localRequires).toHaveLength(0);
  });

  it('only requires electron as an external', () => {
    const content = readFileSync(outFile, 'utf-8');
    const requires = [...content.matchAll(/require\(["']([^"']+)["']\)/g)].map((m) => m[1]);
    const nonElectronExternals = requires.filter((r) => r !== 'electron');
    expect(nonElectronExternals).toEqual([]);
  });

  it('preserves the contextBridge.exposeInMainWorld call', () => {
    const content = readFileSync(outFile, 'utf-8');
    expect(content).toContain('exposeInMainWorld');
  });

  it('does not emit source maps in prod mode', () => {
    expect(existsSync(join(tmpDir, 'preload.js.map'))).toBe(false);
  });
});
