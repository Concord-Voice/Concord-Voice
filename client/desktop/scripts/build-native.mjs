#!/usr/bin/env node
// Build the concord-audiocap native addon (ADR-0043 PR 4).
//
// This exists instead of an npm-script one-liner because CI builds on Windows as
// well as macOS, and the Electron target arguments have to be COMPUTED (from the
// installed electron version and the host arch). A `$(...)` substitution in a
// package.json script is a bash-ism that silently produces a literal string under
// cmd.exe -- which would build against the wrong ABI and fail at load time, far
// from the cause.
//
// What --runtime actually buys, MEASURED rather than assumed:
//
// It selects which headers the compile sees and, on Windows, which node.lib the
// link resolves against. It does NOT make the artifact runtime-specific. This is an
// N-API addon (NAPI_VERSION=8 in binding.gyp), and N-API is ABI-stable across
// runtimes -- an Electron-targeted build loaded cleanly under Node 26 here, whose
// NODE_MODULE_VERSION differs from Electron 43's. A NAN or raw-V8 addon would have
// been rejected; this one is not, by design.
//
// So the practical consequence is the opposite of the usual native-addon folklore:
// one build serves both the app's utilityProcess and any Node-hosted smoke test,
// and CI does not need an Electron install to exercise the addon. Do not add a
// per-runtime rebuild step on the assumption that it is required.
//
// Default is electron because that is what the shipped product compiles against.

import { spawnSync } from 'node:child_process';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const require = createRequire(import.meta.url);
const here = path.dirname(fileURLToPath(import.meta.url));
const desktopRoot = path.resolve(here, '..');
const addonDir = path.join(desktopRoot, 'native', 'concord-audiocap');

const runtime = process.argv.includes('--node') ? 'node' : 'electron';

// --arch is EXPLICIT because defaulting to process.arch silently made the CI
// matrix a lie: one macos-latest row built only the runner's own arm64, so
// ADR-0043 D1's required darwin-x64 target was never compiled and a broken x64
// artifact could ship under a green matrix. Found by Codex on PR #3155.
// Both spellings, and a HARD FAILURE on a malformed one. The space form alone was
// a re-run of the very bug the comment above describes: `npm run build:native --
// --arch=x64` is the natural npm invocation, and it fell through to process.arch
// silently -- making the CI matrix a lie again, the second time. A trailing bare
// `--arch` did the same. Found by @code-reviewer on PR #3155.
const KNOWN_ARCHES = ['x64', 'arm64'];
function resolveArch(argv) {
  const eq = argv.find((a) => a.startsWith('--arch='));
  if (eq !== undefined) {
    return eq.slice('--arch='.length);
  }
  const i = argv.indexOf('--arch');
  if (i === -1) {
    return process.arch;
  }
  return argv[i + 1];
}
const arch = resolveArch(process.argv);
if (!arch || arch.startsWith('--')) {
  console.error('--arch was given without a value. Use --arch=x64 or --arch x64.');
  process.exit(1);
}
if (!KNOWN_ARCHES.includes(arch)) {
  // A refusal, not a warning: an unrecognised arch builds SOMETHING and the matrix
  // row then reports green for a target it never compiled.
  console.error(`unknown --arch "${arch}". Expected one of: ${KNOWN_ARCHES.join(', ')}.`);
  process.exit(1);
}
// --synthetic adds the CI/TEST-ONLY second gyp target (#3245 review). It does NOT
// change what the shipped addon contains: `-Daudiocap_synthetic=1` makes gyp
// APPEND `concord_audiocap_synthetic` to the targets list, and the
// CONCORD_AUDIOCAP_SYNTHETIC define lives only on that target. The default
// `concord_audiocap` is built from the same sources with the macro undefined,
// exactly as it is without this flag.
//
// It lives here rather than as a raw `node-gyp rebuild -- -D...` in a workflow,
// and that is not tidiness. A hand-rolled invocation loses --runtime/--target/
// --dist-url (so it compiles against the wrong headers) and, on Windows, re-enters
// the node-gyp.cmd EINVAL trap this whole file exists to avoid. Both failures are
// documented at length above; do not reintroduce them in YAML.
const synthetic = process.argv.includes('--synthetic');

const args = ['rebuild', `--arch=${arch}`];

if (runtime === 'electron') {
  const electronVersion = require('electron/package.json').version;
  args.push(
    '--runtime=electron',
    `--target=${electronVersion}`,
    '--dist-url=https://electronjs.org/headers'
  );
  console.log(`building concord-audiocap for Electron ${electronVersion} (${arch})`);
} else {
  console.log(`building concord-audiocap for Node ${process.versions.node} (${arch})`);
}
// Gyp defines go after a `--` separator, which node-gyp forwards verbatim to gyp.
// Appended LAST so it cannot be swallowed by the runtime flags above.
if (synthetic) {
  args.push('--', '-Daudiocap_synthetic=1');
  console.log('  + synthetic target (concord_audiocap_synthetic.node) — CI/test only');
}

// Spawn node-gyp's JS ENTRY POINT with this Node, never the node_modules/.bin
// shim. That is not stylistic:
//
//   - On Windows the shim is node-gyp.cmd, and since Node 18.20 / 20.12 (the
//     CVE-2024-27980 argument-injection fix) child_process.spawn REFUSES to
//     execute a .cmd or .bat with shell: false. It fails with EINVAL, which names
//     neither the file type nor the reason. Measured on windows-latest, where this
//     script's first CI run died exactly there while macOS and Linux passed.
//   - The obvious workaround, shell: true, is the thing that CVE was about: it
//     hands the arguments to cmd.exe for a second round of parsing. Resolving the
//     .js and running it under process.execPath sidesteps both the restriction and
//     the reason for it.
//
// require.resolve also survives npm hoisting, where a hardcoded
// node_modules/node-gyp path does not. node-gyp is a declared devDependency so a
// forge minor bump cannot take the toolchain away.
let nodeGypJs;
try {
  nodeGypJs = require.resolve('node-gyp/bin/node-gyp.js');
} catch (cause) {
  console.error(
    'could not resolve node-gyp. Run "npm ci" in client/desktop. ' +
      `(${cause instanceof Error ? cause.message : String(cause)})`
  );
  process.exit(1);
}

const result = spawnSync(process.execPath, [nodeGypJs, ...args], {
  cwd: addonDir,
  stdio: 'inherit',
  shell: false,
});
if (result.error) {
  console.error(`could not run ${nodeGypJs}: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status ?? 1);
