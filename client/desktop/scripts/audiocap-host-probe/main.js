// THE ELECTRON HOST PROBE (#3195, design §10a).
//
// WHY THIS EXISTS, AND WHY IT CANNOT BE A UNIT TEST.
// `native-audiocap.yml`'s load step runs `node -e "require('./native/concord-audiocap')"`
// under PLAIN NODE. The addon's D5 guard is gated on `process.versions.electron`
// (`native/concord-audiocap/index.js:68`), so under plain Node it is INERT — CI's only
// execution of the addon exercises the one branch a packaged build never takes, and
// every "no addon in main or the renderer" claim proven there is vacuous.
//
// This is a minimal main-only Electron app. NO BrowserWindow, so it needs no display
// server and runs headless on every runner.
//
// WHAT RUNS WHERE, AND WHY IT IS SPLIT.
// `resolveNativeAddonPath` supports only `darwin` and `win32`
// (`src/main/nativeAddonPath.ts:31`). On Linux the host returns `unsupported-os` and
// never forks, so the fork-dependent assertions are structurally unreachable there.
// Widening `SUPPORTED` to make CI pass would change production behaviour to serve a
// test, and Linux genuinely has no per-process backend — so instead:
//
//   A1  every platform   `require(<addon>)` THROWS in main (the D5 guard is live)
//   A2+ darwin/win32     the fork-dependent five
//
// A1 is the assertion §10a calls "the one that cannot be made anywhere else", and it
// is deliberately NOT gated on platform: it is the whole reason this app exists.
'use strict';

const { app } = require('electron');
const path = require('node:path');
const { createRequire } = require('node:module');

const results = [];
let failed = 0;

function check(name, fn) {
  try {
    fn();
    results.push(`ok   ${name}`);
  } catch (err) {
    failed += 1;
    results.push(`FAIL ${name}: ${err && err.message ? err.message : String(err)}`);
  }
}

async function checkAsync(name, fn) {
  try {
    await fn();
    results.push(`ok   ${name}`);
  } catch (err) {
    failed += 1;
    results.push(`FAIL ${name}: ${err && err.message ? err.message : String(err)}`);
  }
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg);
}

const ADDON_DIR = path.resolve(__dirname, '../../native/concord-audiocap');

app.whenReady().then(async () => {
  // ── A1 ── The D5 guard is LIVE in a real Electron main process.
  // This is the assertion no other job in the repo can make. It is not gated on
  // platform because the guard is not: it refuses on `process.type` alone.
  check('A1 the addon REFUSES to load in main (D5 guard live under Electron)', () => {
    assert(process.type === 'browser', `expected to be in main, got process.type=${process.type}`);
    assert(
      typeof process.versions.electron === 'string' && process.versions.electron.length > 0,
      'process.versions.electron is unset -- the D5 guard would be INERT, as it is under plain Node'
    );
    let threw = null;
    try {
      createRequire(path.join(ADDON_DIR, 'index.js'))(ADDON_DIR);
    } catch (err) {
      threw = err;
    }
    assert(
      threw !== null,
      'loading the addon in MAIN did not throw -- the D5 guard is not enforcing'
    );
  });

  const forkCapable = process.platform === 'darwin' || process.platform === 'win32';

  if (!forkCapable) {
    results.push(
      `skip A2-A6 fork-dependent assertions: resolveNativeAddonPath supports only darwin/win32, ` +
        `so on ${process.platform} the host returns unsupported-os and never forks`
    );
  } else {
    const host = require(path.resolve(__dirname, '../../dist/main/audiocapHost.js'));

    // ── A2 ── The capability probe has a real caller and actually settles.
    // This is inherited item (a): #3194 shipped this mechanism with no caller.
    await checkAsync('A2 the capability probe forks, settles, and records an answer', async () => {
      const out = await host.probeAudiocapCapability();
      assert(out && typeof out === 'object', 'probe returned nothing');
      assert('ok' in out, `probe result has no ok field: ${JSON.stringify(out)}`);
      if (out.ok) {
        assert(typeof out.perProcessAudio === 'boolean', 'perProcessAudio is not a boolean');
      } else {
        assert(typeof out.reason === 'string', 'a failed probe carries no mechanism string');
      }
      results.push(`     probe -> ${JSON.stringify(out)}`);
    });

    // ── A6 ── The probe MUST have reaped its own child before teardown.
    //
    // THIS USED TO ONLY LOG, and CodeRabbit was right to call that out on PR #3245:
    // it read `currentAudiocapGeneration()`, pushed the number into the output and
    // could not fail, so the job reported PROBE PASS whether or not a native-code
    // child was still alive. An assertion that cannot fail is not an assertion —
    // the same vacuity this PR spent its whole review budget hunting elsewhere.
    //
    // `currentAudiocapGeneration()` is LIVENESS-scoped (`session?.generation ?? 0`),
    // so 0 means "no live session" — the probe's `finally` reaped its child. That is
    // now asserted rather than narrated.
    //
    // WHAT THIS STILL CANNOT SEE, stated rather than implied: it proves the probe
    // reaped ITS OWN child, not that `app.exit()` would reap a child left running.
    // That needs an outer supervisor holding the PID across process death, which is
    // a different harness. It is also already answered empirically — a separate
    // Electron probe run during this PR confirmed Chromium reaps utilityProcess
    // children when the parent dies (a deliberately unreaped child was gone once the
    // app exited). Recorded in ADR-0043 rather than re-derived here.
    check('A6 the probe reaped its own child before teardown', () => {
      const gen = host.currentAudiocapGeneration();
      results.push(`     generation after probe = ${gen}`);
      assert(
        gen === 0,
        `a forked child is STILL LIVE after the probe settled (generation ${gen}); ` +
          'probeAudiocapCapability must reap in its finally on every outcome'
      );
    });
  }

  const summary = results.join('\n');
  // eslint-disable-next-line no-console -- this IS the probe output; a CI job reads stdout
  console.log(
    `\n=== audiocap host probe (${process.platform}, electron ${process.versions.electron}) ===\n${summary}\n`
  );
  // eslint-disable-next-line no-console -- the PASS/FAIL line the job greps for
  console.log(failed === 0 ? 'PROBE PASS' : `PROBE FAIL (${failed})`);
  app.exit(failed === 0 ? 0 : 1);
});

// A hung probe must fail the job, not hang the runner.
setTimeout(() => {
  // eslint-disable-next-line no-console -- same stdout contract, on the hung-probe path
  console.log('PROBE FAIL (timeout)');
  app.exit(2);
}, 60_000).unref();
