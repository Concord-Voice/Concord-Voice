'use strict';

// concord-audiocap loader.
//
// Deliberately thin. The addon is loaded in a utilityProcess (ADR-0043 D5), never
// in main and never in the renderer -- a memory-safety bug in rt/ must not own the
// process holding SSO tokens and the update path.
//
// The plain require() below is correct in DEV, which is all this PR ships. It is
// NOT yet correct inside a packaged app: a packed .node cannot be dlopen'd, and
// asar's unpack option is currently non-functional in this repo (see the blocker
// recorded in forge.config.ts's asar block -- @electron/asar@3.4.1 calls
// minimatch's default export, which minimatch 10 does not have). Resolving that,
// or shipping via extraResource instead, is PR 5's work. Until then a packaged
// build fails LOUDLY here rather than falling back silently, which is the point.

const BUILT = './build/Release/concord_audiocap.node';

let binding;
try {
  binding = require(BUILT);
} catch (cause) {
  const err = new Error(
    `concord-audiocap native addon is not built or not loadable (${BUILT}). ` +
      'Run "npm run build:native" in client/desktop. ' +
      'If this is a packaged build, check that forge packagerConfig.asar.unpackDir ' +
      'still covers native/**; a packed .node cannot load.'
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
