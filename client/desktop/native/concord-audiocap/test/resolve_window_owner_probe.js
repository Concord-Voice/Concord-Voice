// Real-runner probe. client/desktop/native/** is outside sonar.sources, so this is
// the ONLY coverage the OS call can have (spec §8). Three refusals.
//
// WHAT THIS PROVES, AND WHAT IT DOES NOT. Every assertion below is a REFUSAL, so
// a stub `return null;` for every input passes all three. This probe holds exactly
// one line: *a refusal never becomes an answer at the JS boundary* -- the `if (pid)`
// falsy-check shape #2161 is about. It says NOTHING about whether the resolver
// resolves.
//
// A standalone positive control is not possible here: it would need an external
// source of (handle, pid) pairs, and the only way to get one is a native window
// enumerator -- a seventh export widening a surface index.d.ts deliberately closes
// at six. The positive assertion lives in the Electron test that resolves a live
// BrowserWindow handle from inside the capture child (#3198 PR 2, Task 11), which
// gets the pair free from BrowserWindow.getMediaSourceId() and proves window-server
// reachability from the child in the same breath.
//
// On Linux every assertion passes trivially -- ResolveWindowOwner's #else arm always
// refuses. That is still worth running: it catches a missing export and a broken
// build on the row that builds fastest.
const assert = require('node:assert/strict');
const path = require('node:path');

const addon = require(path.join(__dirname, '..', 'build', 'Release', 'concord_audiocap.node'));

assert.equal(typeof addon.resolveWindowOwner, 'function', 'sixth export missing');

// The loader wrapper (index.js), not the raw .node binding above -- this file
// requires the binding DIRECTLY, so nothing here previously checked that
// index.js actually re-exports the function production code calls through.
const wrapper = require(path.join(__dirname, '..', 'index.js'));
assert.equal(
  typeof wrapper.resolveWindowOwner,
  'function',
  'index.js does not re-export resolveWindowOwner'
);

// REFUSAL 1: zero is never a window handle.
assert.equal(addon.resolveWindowOwner(0), null, 'handle 0 must refuse');

// REFUSAL 2: a handle no window owns. 0xfffffffe is chosen rather than 0xffffffff
// because the latter is INVALID_HANDLE_VALUE on Win32 and could be special-cased by
// the OS; this one is merely absent.
assert.equal(addon.resolveWindowOwner(0xff_ff_ff_fe), null, 'absent handle must refuse');

// REFUSAL 3: out of u32 range. Guarded in JS before the OS is asked.
assert.equal(addon.resolveWindowOwner(0x1_00_00_00_00), null, 'out-of-range must refuse');

// REFUSAL 4: no argument at all.
assert.equal(addon.resolveWindowOwner(), null, 'missing argument must refuse');

// REFUSAL 5: NaN is not a window handle.
assert.equal(addon.resolveWindowOwner(NaN), null, 'NaN must refuse');

// REFUSAL 6: a non-numeric argument.
assert.equal(addon.resolveWindowOwner('42'), null, 'a string argument must refuse');

// REFUSAL 7: a fractional value -- a window handle is always an integer.
assert.equal(addon.resolveWindowOwner(12.5), null, 'a fractional value must refuse');

// eslint-disable-next-line no-console -- this IS the probe output; the CI step reads stdout
console.log('resolve_window_owner_probe: 7 refusals OK, wrapper re-export OK');
