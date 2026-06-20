import { build } from 'esbuild';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const projectRoot = resolve(__dirname, '..');

const isDev = process.argv.includes('--dev') ||
  (process.env.NODE_ENV !== 'production' && !process.argv.includes('--prod'));

// Allow callers (e.g. tests) to override the output directory.
const outdir = process.env.PRELOAD_OUTDIR
  ? resolve(process.env.PRELOAD_OUTDIR)
  : resolve(projectRoot, 'dist/preload');

try {
  await build({
    entryPoints: [resolve(projectRoot, 'src/preload/preload.ts')],
    outfile: resolve(outdir, 'preload.js'),
    bundle: true,
    format: 'cjs',
    platform: 'node',
    target: 'node20',
    external: ['electron'],
    sourcemap: isDev ? 'linked' : false,
    minify: !isDev,
    logLevel: 'info',
    tsconfig: resolve(projectRoot, 'tsconfig.preload.json'),
  });
} catch (err) {
  console.error('\n[build-preload] esbuild failed:');
  if (err.errors?.length) {
    for (const e of err.errors.slice(0, 5)) {
      console.error(`  ${e.location?.file ?? '<unknown>'}:${e.location?.line ?? '?'} — ${e.text}`);
    }
  } else {
    console.error(' ', err.message ?? err);
  }
  process.exit(1);
}
