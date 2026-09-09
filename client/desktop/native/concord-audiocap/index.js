'use strict';

// concord-audiocap loader.
//
// Deliberately thin. The addon is loaded in a utilityProcess (ADR-0043 D5), never
// in main and never in the renderer -- a memory-safety bug in rt/ must not own the
// process holding SSO tokens and the update path.
//
// PATH RESOLUTION (ADR-0043 PR 5, #3194). A packaged build ships the .node OUTSIDE
// app.asar via forge `extraResource`, because dlopen/LoadLibrary need a real path
// on disk and everything inside the archive is virtual.
//
// THIS FILE derives the path it loads, from its own process.resourcesPath. Main also
// computes one -- it is the only process that knows app.isPackaged, since a
// utilityProcess child has no `app` module -- and passes it on the fork env, but that
// value only has to AGREE; it never selects the target. See the F1 block below, and
// src/main/nativeAddonPath.ts, which owns main's half and exports this same variable
// name. (Until #3194's red-team pass this header said main resolved the path and
// passed it here, which described the pre-F1 code and contradicted the F1 block
// twenty lines below it.)
//
// asar's `unpack`/`unpackDir` are NOT an alternative and never will be. They call
// minimatch's default export, which minimatch 10 does not have, and the minimatch
// 10 override is permanent security work that is not capped back. The blocker is
// therefore permanent, and Route 2 routes AROUND it rather than clearing it.

const fs = require('node:fs');
const path = require('node:path');

const ENV_PATH = 'CONCORD_AUDIOCAP_PATH';
const BINARY = 'concord_audiocap.node';

// The node-gyp output tree, resolved relative to THIS file rather than to cwd —
// it is where the build actually writes, whatever directory the app was started
// from.
const DEV_FALLBACK = path.resolve(__dirname, 'build', 'Release', BINARY);

// CONCORD_AUDIOCAP_PATH IS A VALUE THAT MUST AGREE, NEVER A SPECIFIER (#3194 F1).
//
// This used to be `process.env[ENV_PATH] || DEV_FALLBACK` handed straight to
// require(). The sink is require(), not dlopen(), and that is what made it
// serious: the payload need not be a Mach-O, so a plain .js file works. Then
// macOS library validation, Windows Authenticode and
// EnableEmbeddedAsarIntegrityValidation are ALL irrelevant -- no signed binary is
// loaded, and nothing is written into the app bundle, so `codesign --verify`
// stays green and macOS App Management never prompts. Any unprivileged user
// process can set the variable (`launchctl setenv`, `setx`), and the resulting
// JS runs inside a notarized process holding Screen Recording and Microphone
// grants -- with capability() attacker-authored and free to forge
// perProcessAudio:true, which is #2161's defect.
//
// The channel needs LESS privilege than replacing the .node on disk, and defeats
// a control that route does not. ADR-0043 R6 called local replacement "a
// restatement of pre-existing local-write risk"; true of the Mach-O swap, false
// of this.
//
// THE ADDON LOADS ONLY IN A utilityProcess (ADR-0043 D5).
//
// This guard used to live only in src/main/audiocapSmokeChild.ts, which made it a
// property of ONE CALL SITE rather than of the artifact. Any future
// `require('../../native/concord-audiocap')` from src/main would have loaded a
// memory-unsafe addon into the process holding SSO tokens and the update path,
// with nothing objecting. The check belongs where the dlopen happens.
//
// Gated on `process.versions.electron` so the unit tests, which import this module
// under plain Node, still exercise the path logic below. That is a test-visibility
// gate, not a fail-open: outside Electron there is no privileged process to protect.
if (process.versions.electron && process.type !== 'utility') {
  throw new Error(
    `concord-audiocap must load in a utilityProcess (ADR-0043 D5); process.type was ` +
      `"${String(process.type)}". A memory-safety bug in rt/ must not own the process ` +
      'holding SSO tokens and the update path (#3194).'
  );
}

// So: derive the ONE permitted path here and require the variable to equal it.
// An allowlist of one, not sanitization -- there is no traversal to scrub and no way
// to express a second target.
//
// PINNING THE PATH IS NOT PINNING WHAT IS AT IT (#3194 red-team, VULN-1). An earlier
// version of this comment added "no extension to check", and that reasoning is what
// missed the following: require() resolves a MODULE, not a file. A DIRECTORY named
// concord_audiocap.node resolves index.js inside it as a CommonJS package; a SYMLINK
// resolves to its realpath, so the .js handler runs instead of the .node one. Either
// executes attacker JS in this privileged process with capability() attacker-authored
// -- the identical F1 sink, entered through the ACCEPTED branch of this allowlist,
// and again with no Mach-O loaded, so library validation, Authenticode and asar
// integrity are all bypassed.
//
// It costs local write into <Resources>/ rather than `launchctl setenv`. ADR-0043 R6
// dismissed that as "a restatement of pre-existing local-write risk" -- true of the
// Mach-O swap, which macOS library validation genuinely blocks (the utility host is
// the plain Helper.app, signed against default.darwin.plist, which does NOT carry
// com.apple.security.cs.disable-library-validation). This route is the one way that
// local write becomes execution here, so R6 is false for it too.
//
// main sets the variable only when packaged (see audiocapSmoke.ts); dev uses
// DEV_FALLBACK.
const requested = process.env[ENV_PATH];
let target = DEV_FALLBACK;
if (requested) {
  const permitted = process.resourcesPath ? path.join(process.resourcesPath, BINARY) : null;
  if (permitted === null || path.resolve(requested) !== permitted) {
    throw new Error(
      `concord-audiocap: ${ENV_PATH} does not name this build's addon. ` +
        `Refusing to load "${requested}". ` +
        (permitted === null
          ? 'This process has no resourcesPath, so no packaged addon path exists to match.'
          : `The only permitted value is "${permitted}".`) +
        ' This variable is a cross-check, not a way to choose what gets loaded (#3194).'
    );
  }
  target = permitted;
}

// lstatSync, NEVER statSync: stat FOLLOWS the link, so the symlink arm survives a
// stat-based guard entirely. A missing file falls through deliberately -- absent is a
// packaging defect and must produce the load error below, not a trust refusal.
//
// KNOWN RESIDUAL, stated rather than hidden: this is a check-then-use, so an attacker
// who ALREADY has write access to <Resources>/ can swap the file between the lstat and
// the require. Both the red-team pass and Gitar's review found it independently.
//
// It is not closable at this layer, and the obvious remedy does not work. Opening the
// file with O_NOFOLLOW and fstat-ing the descriptor -- the suggested alternative --
// closes that descriptor and then calls require(target), which re-resolves the path
// string from scratch; the window moves from after-lstat to after-close and is the same
// width. O_NOFOLLOW is also redundant with the symlink rejection lstat already performs.
// Eliminating it outright would need require() to load FROM a descriptor, which Node
// does not offer.
//
// What the guard does buy is the UN-RACED primitive, which is the whole of what the
// proof-of-concept used: planting a directory or symlink and waiting. The residual needs
// the attacker to win a race they can only enter by already holding the local write that
// ADR-0043 R6 treats as the threat boundary. Do not "fix" this with O_NOFOLLOW and call
// it closed.
let targetStat;
try {
  targetStat = fs.lstatSync(target);
} catch {
  targetStat = null;
}
if (targetStat && !targetStat.isFile()) {
  throw new Error(
    `concord-audiocap: ${target} is not a regular file. ` +
      'A directory or a symlink there is resolved by require() as a JavaScript module, ' +
      'which is the #3194 F1 sink wearing a different hat. Refusing to load it.'
  );
}

let binding;
try {
  binding = require(target);
} catch (cause) {
  // A load failure is a PACKAGING DEFECT, not a capability outcome. It must never
  // collapse into perProcessAudio:false -- that is a legitimate silent video-only
  // rung, and hiding a packaging regression behind it means a user reports the
  // missing audio instead of CI. Fail loudly, and name the route that exists.
  const err = new Error(
    `concord-audiocap native addon is not built or not loadable (${target}). ` +
      'In dev: run "npm run build:native" in client/desktop. ' +
      `In a packaged build: ${ENV_PATH} should name the .node that forge ` +
      'packagerConfig.extraResource copied to <Resources>/; if it is absent, the ' +
      'packaging step that builds the addon did not run.'
  );
  err.cause = cause;
  throw err;
}

/**
 * Platform capability probe. Feeds ADR-0043 D6's capability ladder, whose bottom
 * rung is "below the OS floor -> video-only, and the UI says so".
 *
 * Never treat a false `perProcessAudio` as a reason to widen the capture to a
 * system mix on a window target. That is #2161's defect wearing native clothes.
 *
 * @returns {{platform: string, osVersion: string, perProcessAudio: boolean, reason: string}}
 */
function capability() {
  return binding.capability();
}

module.exports = { capability };
