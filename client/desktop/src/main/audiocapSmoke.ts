// concord-audiocap smoke-harness PARENT — runs in main (#3194).
//
// Forks the utilityProcess child, hands it the resolved addon path, and reports
// what capability() said. Main NEVER requires the addon itself: it only resolves
// the path, because it is the only process that knows app.isPackaged (a
// utilityProcess child has no `app` module). See ADR-0043 D5 and the guard at the
// top of audiocapSmokeChild.ts.
//
// Verification vehicle for #3194 only. The production host with PCM transport,
// MessagePort ring buffer and lifecycle management is #3195.
//
// runAudiocapSmoke HAS NO CALLER. main.ts never imports it, no CI step invokes it,
// and there is no dev trigger — so this module and its child compile into
// dist/main/ and ship inside app.asar without any path by which they execute in a
// packaged build. That was raised by three reviewers at #3194's review gate and
// the developer deferred the wiring to #3195, where the production host provides
// the caller. It is recorded here rather than left for the next reader to
// rediscover: unreachable shipped code is worth knowing about deliberately.

import path from 'node:path';
import { app, utilityProcess } from 'electron';
import { NATIVE_ADDON_ENV, resolveNativeAddonPath } from './nativeAddonPath';

export interface AudiocapSmokeResult {
  ok: boolean;
  capability?: unknown;
  error?: string;
}

/** How long to wait for the child to report before giving up. */
const SMOKE_TIMEOUT_MS = 10_000;

/**
 * Fork a utilityProcess, load the addon in it, and return what capability() said.
 *
 * Resolves rather than rejects on every failure path — this is a diagnostic, and
 * a harness that throws is harder to call from a packaged smoke check than one
 * that reports. A `false` here means "could not probe", which is distinct from a
 * probe that succeeded and reported `perProcessAudio: false`.
 */
export function runAudiocapSmoke(): Promise<AudiocapSmokeResult> {
  const addonPath = resolveNativeAddonPath(
    process.platform,
    app.isPackaged,
    process.resourcesPath,
    process.cwd()
  );

  // null is a legitimate outcome, not an error: ADR-0043 puts Linux/PipeWire out
  // of scope, so there is no addon to probe there.
  if (!addonPath) {
    return Promise.resolve({
      ok: false,
      error: `unsupported platform: ${process.platform}`,
    });
  }

  return new Promise((resolve) => {
    let settled = false;
    const finish = (result: AudiocapSmokeResult) => {
      if (settled) return;
      settled = true;
      resolve(result);
    };

    // BUILD THE CHILD ENVIRONMENT FROM AN ALLOWLIST (#3194 review: CWE-497 + CWE-178).
    //
    // This was `{ ...process.env }` followed by `delete childEnv[NATIVE_ADDON_ENV]`,
    // and that shape had two defects that an allowlist closes at once.
    //
    // 1. CWE-497, handing the addon main's whole environment. ADR-0043 D5 puts the
    //    addon in a utilityProcess precisely so a memory-safety bug in rt/ cannot own
    //    the process holding SSO tokens and the update path. Copying every variable
    //    into that process gives back part of what D5 buys —
    //    GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP is a documented main-process fallback, so
    //    a real secret is in scope. `utilityProcess.fork({env})` REPLACES the parent
    //    environment rather than merging it (measured on Electron 43 during the #3194
    //    red-team pass), so an allowlist genuinely is the whole environment the child
    //    sees — this is not defence that something else quietly undoes.
    //
    // 2. CWE-178, the case-sensitive delete. `{ ...process.env }` produces a PLAIN
    //    object whose keys are case-sensitive, while Windows env lookup is not. An
    //    attacker running `setx concord_audiocap_path ...` left a key the delete did
    //    not match and the child's own lookup still resolved — defeating exactly the
    //    denial-of-service protection the previous comment here claimed to provide.
    //    Under an allowlist a key that is not on the list never reaches the child at
    //    all, whatever its casing, so the collision is unrepresentable rather than
    //    filtered. That is why this is an allowlist and not a case-folding compare.
    //
    // The allowlist is deliberately minimal. PATH is required on Windows for the
    // dependent-DLL search LoadLibrary performs; SystemRoot and windir are required by
    // Windows itself for a process to start at all; TMPDIR/TEMP/TMP keep temp-file
    // resolution sane. Nothing here carries credentials.
    const ENV_ALLOWLIST = ['PATH', 'SystemRoot', 'windir', 'TMPDIR', 'TEMP', 'TMP'];
    const childEnv: NodeJS.ProcessEnv = {};
    for (const key of ENV_ALLOWLIST) {
      const value = process.env[key];
      if (value !== undefined) childEnv[key] = value;
    }
    // Set the variable ONLY when packaged. The loader treats CONCORD_AUDIOCAP_PATH as
    // a value that must EQUAL the one path it derives from process.resourcesPath — so
    // in dev, where the addon lives in the node-gyp output tree instead, any value at
    // all is refused. It is a cross-check, never a way to choose what gets loaded.
    if (app.isPackaged) {
      childEnv[NATIVE_ADDON_ENV] = addonPath;
    }

    const child = utilityProcess.fork(path.join(__dirname, 'audiocapSmokeChild.js'), [], {
      env: childEnv,
    });

    // Without this the harness hangs forever if the child neither reports nor
    // exits — the packaged smoke check would then stall rather than fail.
    const timer = setTimeout(() => {
      finish({ ok: false, error: `timed out after ${SMOKE_TIMEOUT_MS}ms` });
      child.kill();
    }, SMOKE_TIMEOUT_MS);

    child.on('message', (message) => {
      clearTimeout(timer);
      finish(message as AudiocapSmokeResult);
      child.kill();
    });

    // A child that threw — the D5 guard, or the loader failing to find the .node
    // — exits non-zero without messaging. That is a PACKAGING DEFECT surfacing,
    // and it must read as one rather than as an absent capability.
    child.on('exit', (code) => {
      clearTimeout(timer);
      finish({ ok: false, error: `child exited with code ${code} before reporting` });
    });
  });
}
