// concord-audiocap smoke-harness CHILD — runs INSIDE a utilityProcess.
//
// Loads the native addon, probes capability(), and reports the result to the
// parent. It does NOT exit itself — it idles until the parent's child.kill().
//
// NOT YET WIRED, AND THAT IS A DEFERRED OBLIGATION RATHER THAN A DESIGN CHOICE.
// #3194's acceptance criterion is that the packaged addon is "verified by running
// it, not by inspecting the archive", and an earlier revision of this comment
// claimed this file satisfies it. It does not: nothing calls runAudiocapSmoke —
// not main.ts, not CI, not a dev trigger — so the criterion is met today only by
// a manual run against a local packaged build. CI asserts the .node is a regular,
// non-symlink, non-empty file in <Resources>/, which is a stronger INSPECTION and
// still not an EXECUTION: it checks no Mach-O magic, no ABI, and never calls
// capability().
//
// Wiring this into the release job was deferred to #3195 by the developer at
// #3194's review gate, where it gains a real production caller. Until then, treat
// a green packaging leg as proof the file SHIPPED, never as proof it LOADS.
//
// THIS IS NOT THE PRODUCTION HOST. No PCM transport, no MessagePort ring buffer,
// no crash/respawn lifecycle, no teardown on will-quit — all of that is #3195,
// which grows from this seed rather than replacing it.
//
// WHY THIS FILE LIVES UNDER src/main/ DESPITE RUNNING ELSEWHERE. There is no
// src/utility/ build path: tsconfig.main.json includes only src/main/** and
// src/shared/**, and `npm run build` has three legs (renderer, preload, main).
// utilityProcess.fork() takes a path to a built JS file and does not care which
// tsconfig produced it, so compiling through the existing main leg costs nothing,
// where a fourth build target for two files would be the wrong trade.
//
// The rule this appears to bend does not actually bend: native-audio.md:89
// constrains where the addon RUNS, not where its source file LIVES. The
// assertion below is what makes that true rather than merely claimed.

import { createRequire } from 'node:module';

// ADR-0043 D5. A memory-safety bug in rt/ must not own the process holding SSO
// tokens and the update path, so the addon may load ONLY in a utilityProcess.
//
// This is a GUARD, not a comment. Electron types process.type as
// 'browser' | 'renderer' | 'service-worker' | 'worker' | 'utility'
// (electron.d.ts), and a plain Node process has no `type` at all — so anything
// that is not a utility child fails here, loudly, on the first wrong call,
// instead of silently widening the trust boundary D5 exists to keep narrow.
if (process.type !== 'utility') {
  throw new Error(
    `audiocapSmokeChild must run in a utilityProcess (ADR-0043 D5); process.type was ` +
      `"${String(process.type)}". Loading the native addon in main or the renderer would put ` +
      `a memory-safety bug in the process that holds SSO tokens and the update path.`
  );
}

// createRequire rather than a bare require(): `module: CommonJS` would accept
// either, but no other file under src/main/ uses a bare require, and this keeps
// the resolution explicit. Relative to THIS file in both layouts —
// src/main/ -> client/desktop/native/, and dist/main/ -> /native inside app.asar,
// which is exactly where the ignore lookahead admits the loader.
const requireFromHere = createRequire(__filename);
const addon = requireFromHere('../../native/concord-audiocap') as {
  capability: () => unknown;
};

process.parentPort.postMessage({ ok: true, capability: addon.capability() });
