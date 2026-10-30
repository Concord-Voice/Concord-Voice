import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { runProbe, verifiedElectronVersion } from './runner.mjs';

const require = createRequire(import.meta.url);
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const executable = require('electron');
const expectedElectron = verifiedElectronVersion(root);
let bundleRoot = root;
if (process.argv.includes('--packaged')) {
  const packages = fs
    .readdirSync(path.join(root, 'out'))
    .filter((entry) => entry.endsWith('-darwin-arm64'));
  if (packages.length !== 1)
    throw new Error(`Expected one packaged macOS arm64 app, got ${packages.length}`);
  const productName = require(path.join(root, 'package.json')).productName;
  bundleRoot = path.join(
    root,
    'out',
    packages[0],
    `${productName}.app`,
    'Contents/Resources/app.asar'
  );
}
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'concord-engine-probes-'));
try {
  fs.mkdirSync(path.join(profile, 'engine'));
  fs.mkdirSync(path.join(profile, 'dialogs'));
  runProbe(
    executable,
    path.join(root, 'scripts/desktop-engine-probe/main.js'),
    [bundleRoot, path.join(profile, 'engine'), expectedElectron],
    'PROBE PASS desktop-engine'
  );
  runProbe(
    executable,
    path.join(root, 'scripts/dialog-top-layer-probe'),
    [`--probe-profile=${path.join(profile, 'dialogs')}`],
    'PROBE PASS dialog-top-layer'
  );
  if (process.platform === 'darwin' || process.platform === 'win32') {
    fs.mkdirSync(path.join(profile, 'native-host'));
    runProbe(
      executable,
      path.join(root, 'scripts/audiocap-host-probe'),
      [`--probe-profile=${path.join(profile, 'native-host')}`],
      'PROBE PASS'
    );
  }
} finally {
  // Remove profiles after Electron exits, when Windows has released its locks.
  fs.rmSync(profile, { recursive: true, force: true });
}
