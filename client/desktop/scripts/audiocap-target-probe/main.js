// THE WINDOW-SERVER REACHABILITY GATE (#3198, plan Task 11 Step 1b).
//
// WHY IT EXISTS, STATED BEFORE THE CODE.
// ADR-0043 D5 puts the handle->PID resolve in the capture child, and on macOS
// that resolve is `CGWindowListCopyWindowInfo`, which returns NULL outside a
// Quartz GUI session. Whether an Electron `utilityProcess` child is inside that
// session was UNPROVEN: the addon's own 46/46 positive control ran in a plain
// terminal Node process, which is, and a utility child is a different animal.
// If it is not, every macOS per-process share refuses silently, forever.
//
// THE UNIT TEST CANNOT SEE THIS, AND NEITHER CAN CI. `audiocapChild.test.ts`
// drives a fake resolver, so it proves the wiring and nothing about the OS.
// `native-audiocap.yml`'s resolver step asserts three REFUSALS, which a stub
// `return null;` satisfies. And the refusal half of the contract arrives
// identically in both worlds -- a bad handle is refused whether or not the
// child can reach the window server -- so a refusal-only check is structurally
// blind to exactly this failure.
//
// THE POSITIVE CONTROL IS THE CONTRAST, NOT A REPORTED PID.
//   LIVE handle -> no fault{stage:'target'}; the session reaches the addon.
//   BAD  handle -> fault{stage:'target'} arrives.
// Asserting that the child resolved "main's own pid" would be the more direct
// statement and is FORBIDDEN: it sends a resolved PID across a process
// boundary, which is the one thing invariant I-PID exists to prevent, and this
// file would then be the first violation of the design it was written to prove.
// Nothing below ever asks the child what it resolved.
//
// WHERE IT RUNS. Locally, on macOS or Windows, with a real GUI session:
//
//     cd client/desktop && npm run build:main && npm run build:native
//     timeout 100 npx electron scripts/audiocap-target-probe; # read PROBE PASS/FAIL
//
// `timeout` is not belt-and-braces. MEASURED on macOS 26.6.2 / Electron 44.1.1:
// the process prints its whole report and then LINGERS past `app.exit()`,
// needing a SIGTERM -- observed identically from a bare `app.exit()` in a
// BrowserWindow-only spike with no children at all, so it is a launch-wrapper
// quirk of this runtime and not something this probe leaves running. Judge the
// run by the PROBE PASS / PROBE FAIL line, never by the exit code.
//
// It is deliberately NOT wired to CI. `build.yml`'s `electron-host-probe` job
// runs on `blacksmith-2vcpu-ubuntu-2404` under `--ozone-platform=headless`,
// which has no window server and no addon (`resolveNativeAddonPath` supports
// darwin/win32 only), and #3198 spec section 8 rules out adding a macOS runner.
// So this is a re-runnable gate a human runs, not an enforced one -- run it on
// any change to `window_owner.cc`, to the child's resolve, or to the Electron
// major.
'use strict';

const { app, BrowserWindow, MessageChannelMain, utilityProcess } = require('electron');
const path = require('node:path');

const WINDOW_SOURCE_ID = /^window:([1-9]\d*):(0|[1-9]\d*)$/;

// 0xfffffffe rather than 0xffffffff: the latter is INVALID_HANDLE_VALUE on
// Win32 and could be special-cased by the OS; this one is merely absent.
const BAD_HANDLE = 0xff_ff_ff_fe;

const CHILD = path.resolve(__dirname, '../../dist/main/audiocapChild.js');
const HELLO_TIMEOUT_MS = 10_000;
// The child answers a `start` synchronously -- resolve, then `addon.start` --
// so this only has to outlast one OS tap attempt.
const SETTLE_MS = 3_000;

const results = [];
let failed = 0;

function record(ok, line) {
  if (!ok) failed += 1;
  results.push(`${ok ? 'ok  ' : 'FAIL'} ${line}`);
}

/**
 * Fork a real capture child, take its `hello`, send one `start` carrying
 * `windowHandle`, and return every control message it posted afterwards.
 *
 * A REAL child and the REAL addon, not a harness: the whole question is what
 * the OS does when this process asks it, which no fake can answer.
 */
async function runStart(windowHandle) {
  const child = utilityProcess.fork(CHILD, [], { stdio: 'pipe' });
  // The child's own diagnostics, unfiltered -- this is a developer's machine
  // and the addon is their own build.
  child.stderr?.on('data', (c) => process.stdout.write(`[child stderr] ${String(c)}`));
  child.stdout?.on('data', (c) => process.stdout.write(`[child stdout] ${String(c)}`));

  const messages = [];
  child.on('message', (m) => messages.push(m));

  try {
    await new Promise((resolve, reject) => {
      const timer = setTimeout(
        () => reject(new Error('no hello within the handshake budget')),
        HELLO_TIMEOUT_MS
      );
      child.on('message', (m) => {
        if (m && m.kind === 'hello') {
          clearTimeout(timer);
          resolve();
        }
      });
      child.on('exit', () => {
        clearTimeout(timer);
        reject(new Error('the child exited before saying hello'));
      });
    });

    const { port1 } = new MessageChannelMain();
    child.postMessage(
      {
        kind: 'start',
        quantumMs: 10,
        sampleRate: 48000,
        channels: 2,
        frameCount: 480,
        creditBound: 8,
        ringSlots: 8,
        windowHandle,
      },
      [port1]
    );

    await new Promise((r) => setTimeout(r, SETTLE_MS));
    // Unconditional: a start that SUCCEEDED opened a real OS tap, and the probe
    // must not leave one behind. Killing the child is what actually reaps it,
    // but asking first is the documented teardown.
    child.postMessage({ kind: 'stop' });
    await new Promise((r) => setTimeout(r, 250));
    return messages.filter((m) => m && m.kind === 'fault');
  } finally {
    child.kill();
  }
}

app.whenReady().then(async () => {
  // `show: true` and real content, both load-bearing. `getMediaSourceId()` needs
  // a window that exists, and `CGWindowListCopyWindowInfo` with
  // kCGWindowListOptionIncludingWindow is measured to be an ON-SCREEN filter
  // (67/67 on-screen windows returned a record, 0/467 off-screen ones did), so
  // an unshown window would refuse for a reason that is not the one under test.
  // 'ready-to-show' fires on the first paint of a LOADED page and never fires
  // at all for a window with nothing in it.
  const win = new BrowserWindow({ show: true, width: 320, height: 240 });
  let sourceId = '';
  let liveHandle = 0;
  try {
    await win.loadURL('data:text/html,<title>audiocap target probe</title><body>probe</body>');
    win.show();
    await new Promise((r) => setTimeout(r, 750));

    sourceId = win.getMediaSourceId();
    const parsed = WINDOW_SOURCE_ID.exec(sourceId);
    if (!parsed) throw new Error(`getMediaSourceId did not parse: ${sourceId}`);
    liveHandle = Number(parsed[1]);

    // ── The positive control ──────────────────────────────────────────────
    const liveFaults = await runStart(liveHandle);
    // Widened past `stage === 'target'` alone: a child that faulted early --
    // 'guard', 'load', 'capability' -- never reached the resolve at all, so
    // `liveTarget.length === 0` passed just as readily for "never got there" as
    // for "resolved successfully". That disambiguation used to live only in the
    // `results.push` a few lines down, which is stdout-only and never affects
    // PASS/FAIL. Record it for real.
    record(
      liveFaults.every((f) => !['guard', 'load', 'capability', 'target'].includes(f.stage)),
      `a LIVE window handle is resolved by the child ` +
        `(faults: ${JSON.stringify(liveFaults.map((f) => f.stage))})`
    );
    // Which arm it took, for the reader. Reaching 'start' proves the target
    // stage was passed; no fault at all means the addon started and was stopped.
    results.push(
      `     live start reached: ${
        liveFaults.length === 0 ? 'the addon started' : `stage ${liveFaults[0].stage}`
      }`
    );

    // ── The negative control ──────────────────────────────────────────────
    // A SECOND child, not a second start on the first: the sink gate is armed
    // once per process and `close()` is permanent, so a second start in one
    // child is refused with `Poisoned` before any of this is exercised.
    const badFaults = await runStart(BAD_HANDLE);
    record(
      badFaults.some((f) => f.stage === 'target'),
      `a KNOWN-BAD window handle is refused at stage 'target' ` +
        `(faults: ${JSON.stringify(badFaults.map((f) => f.stage))})`
    );
  } catch (err) {
    failed += 1;
    results.push(`FAIL probe error: ${err && err.message ? err.message : String(err)}`);
  }

  win.destroy();
  // eslint-disable-next-line no-console -- this IS the probe output; a human reads stdout
  console.log(
    `\n=== audiocap target probe (${process.platform}, electron ${process.versions.electron}) ===\n` +
      `sourceId ${sourceId}\n${results.join('\n')}\n`
  );
  // eslint-disable-next-line no-console -- the PASS/FAIL line the runner greps for
  console.log(failed === 0 ? 'PROBE PASS' : `PROBE FAIL (${failed})`);
  app.exit(failed === 0 ? 0 : 1);
});

// A hung probe must fail, not hang the machine.
setTimeout(() => {
  // eslint-disable-next-line no-console -- same stdout contract, on the hung path
  console.log('PROBE FAIL (timeout)');
  app.exit(2);
}, 90_000).unref();
