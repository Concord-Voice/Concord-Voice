// THE TOP-LAYER DIALOG CONTRACTS (frontend.md § Platform-behavior register).
//
// The global overlays (useTopLayerDialog: ForceUpdate, ConnectionLost,
// AttestationFailed, Changelog) rely on three engine behaviours no signature
// expresses, and their jsdom tests EMULATE or skip them, so the suite stays
// green if a Chromium release changes any:
//
//   1. A showModal() dialog escapes an `inert` ancestor. The overlays render
//      inside #root, which ModalContext makes inert while any ui/Modal is open.
//      If this stops holding, every overlay opened over a ui/Modal is dead to
//      focus and clicks while it paints on top.
//   2. closedby="none" blocks Escape outright: no `cancel`, no `close`, the
//      dialog stays open. ForceUpdate and ConnectionLost depend on it to stay
//      up. If it stops holding, Escape dismisses a mandatory overlay.
//   3. closedby="none" disables only the dialog's OWN close watcher. Dialogs
//      shown without user activation share a close-watcher group, so Escape on
//      a mandatory overlay closes the dialog beneath it. useTopLayerDialog
//      cancels the Escape keydown, which stops the request. If the grouping
//      goes away the guard is merely redundant; if cancelling stops working,
//      Escape on an update or reconnection overlay dismisses the dialog under it.
//
// Each behaviour is measured against a positive control that shares its
// harness, so a PASS cannot come from a harness that does nothing: a plain
// button under the same inert ancestor must refuse focus, the same injected
// Escape must close an ordinary modal dialog, and one Escape must close two
// grouped ordinary dialogs.
//
// RUN IT on any Electron/Chromium bump (/weekly-deps § 4.C walks the register):
//
//     cd client/desktop
//     timeout 60 npx electron scripts/dialog-top-layer-probe; # read PROBE PASS/FAIL
//
// Judge the run by the PROBE PASS / PROBE FAIL line. `timeout` is there
// because this Electron runtime can linger after app.exit() on macOS (see
// audiocap-target-probe). Not wired to CI: the headless Linux probe job has no
// focusable window to deliver the Escape to.
'use strict';

const { app, BrowserWindow } = require('electron');

const GROUP_PAGE = `<!doctype html><meta charset="utf-8"><body>
<dialog id="under"><button>under</button></dialog>
<dialog id="top"><button>top</button></dialog>
</body>`;

const PAGE = `<!doctype html><meta charset="utf-8"><body>
<div id="root" inert>
  <button id="plain">plain</button>
  <dialog id="nested"><button id="inside">inside</button></dialog>
</div>
<dialog id="ordinary"><button>ordinary</button></dialog>
<dialog id="locked" closedby="none"><button>locked</button></dialog>
</body>`;

const results = [];
function check(name, ok, detail) {
  results.push(ok);
  process.stdout.write(`${ok ? 'ok  ' : 'FAIL'} ${name} :: ${JSON.stringify(detail)}\n`);
}

async function main() {
  await app.whenReady();
  const win = new BrowserWindow({ width: 480, height: 360, show: true });
  await win.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(PAGE)}`);
  win.focus();
  const js = (code) => win.webContents.executeJavaScript(code, true);

  // 1. The inert escape.
  const inert = await js(`(() => {
    const plain = document.getElementById('plain');
    plain.focus();
    const plainFocused = document.activeElement === plain;
    const nested = document.getElementById('nested');
    nested.showModal();
    const inside = document.getElementById('inside');
    inside.focus();
    const insideFocused = document.activeElement === inside;
    nested.close();
    return { plainFocused, insideFocused };
  })()`);
  check(
    'control: a plain button under an inert ancestor refuses focus',
    !inert.plainFocused,
    inert
  );
  check('a showModal() dialog under an inert ancestor takes focus', inert.insideFocused, inert);

  // 2. Escape, delivered as real input through the browser's input pipeline.
  async function escapeOn(id) {
    await js(`(() => {
      const d = document.getElementById('${id}');
      window.__events = [];
      d.addEventListener('cancel', (e) => __events.push('cancel(cancelable=' + e.cancelable + ')'));
      d.addEventListener('close', () => __events.push('close'));
      d.showModal();
      d.querySelector('button').focus();
    })()`);
    win.webContents.sendInputEvent({ type: 'keyDown', keyCode: 'Escape' });
    win.webContents.sendInputEvent({ type: 'keyUp', keyCode: 'Escape' });
    await new Promise((resolve) => setTimeout(resolve, 300));
    return js(`(() => {
      const d = document.getElementById('${id}');
      const seen = { open: d.open, events: __events.slice() };
      if (d.open) d.close();
      return seen;
    })()`);
  }
  const ordinary = await escapeOn('ordinary');
  check(
    'control: Escape closes an ordinary modal dialog',
    !ordinary.open && ordinary.events.includes('close'),
    ordinary
  );
  const locked = await escapeOn('locked');
  check(
    'closedby="none": Escape fires neither cancel nor close, and the dialog stays open',
    locked.open && locked.events.length === 0,
    locked
  );

  // 3. Close-watcher grouping. Both dialogs are shown with NO user activation
  // (userGesture false), as a network-raised overlay is, so they share one
  // group. Each case loads a fresh page so no earlier gesture leaves
  // activation behind.
  async function grouped(lockedTop, cancelKeydown) {
    await win.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(GROUP_PAGE)}`);
    win.focus();
    await win.webContents.executeJavaScript(
      `(() => {
        window.__events = [];
        const under = document.getElementById('under');
        const top = document.getElementById('top');
        for (const d of [under, top]) {
          d.addEventListener('cancel', () => __events.push(d.id + ':cancel'));
          d.addEventListener('close', () => __events.push(d.id + ':close'));
        }
        if (${lockedTop}) top.setAttribute('closedby', 'none');
        if (${cancelKeydown}) {
          top.addEventListener('keydown', (e) => { if (e.key === 'Escape') e.preventDefault(); });
        }
        under.showModal();
        top.showModal();
        top.querySelector('button').focus();
      })()`,
      false
    );
    win.webContents.sendInputEvent({ type: 'keyDown', keyCode: 'Escape' });
    win.webContents.sendInputEvent({ type: 'keyUp', keyCode: 'Escape' });
    await new Promise((resolve) => setTimeout(resolve, 300));
    return win.webContents.executeJavaScript(
      `({ under: document.getElementById('under').open, top: document.getElementById('top').open, events: __events.slice() })`,
      false
    );
  }
  const bothOrdinary = await grouped(false, false);
  check(
    'control: with no activation, one Escape closes both grouped dialogs',
    !bothOrdinary.under && !bothOrdinary.top,
    bothOrdinary
  );
  const passThrough = await grouped(true, false);
  check(
    'closedby="none" on top of a grouped dialog: Escape still closes the one beneath (the guard is needed)',
    !passThrough.under && passThrough.top,
    passThrough
  );
  const guarded = await grouped(true, true);
  check(
    'cancelling the Escape keydown on the closedby="none" dialog keeps the one beneath open',
    guarded.under && guarded.top && guarded.events.length === 0,
    guarded
  );

  const pass = results.every(Boolean);
  process.stdout.write(
    `${pass ? 'PROBE PASS' : 'PROBE FAIL'} (Electron ${process.versions.electron}, Chromium ${process.versions.chrome})\n`
  );
  app.exit(pass ? 0 : 1);
}

main().catch((err) => {
  process.stdout.write(`PROBE FAIL (error: ${err instanceof Error ? err.message : 'unknown'})\n`);
  app.exit(1);
});
