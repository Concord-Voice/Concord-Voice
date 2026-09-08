# concord-audiocap

Per-process screen-share audio capture. The decision, the alternatives, and every
constraint below live in
[ADR-0043](../../../../[internal]0043-per-process-screen-share-audio-capture.md);
this file is the operating manual.

## The boundary

```
rt/     real-time capture core   — JSF++ BINDS      ([internal]rules/native-audio.md)
napi/   Node-API binding layer   — JSF++ DOES NOT
test/   tests and the fuzz target
```

`rt/` runs on an OS audio callback thread, where an allocation is audible and an
exception is unbounded latency. `napi/` talks to V8, which allocates and throws by
nature. **Do not move code across that line to dodge a rule.** Move the rule's
justification instead, which means editing the ADR and
[`[internal]rules/native-audio.md`](../../../../[internal]rules/native-audio.md) — in
that order, before the code.

**Part** of the boundary is enforced mechanically, and the rest is a review
obligation — conflating the two is how a rule quietly stops being one.
`scripts/check-native-jsf-profile.sh` runs in pre-commit and in CI, and has its own
self-test because a guard that has stopped detecting anything reports a clean tree
and reads exactly like a pass. It has done so twice: a C++14 digit separator
(`48'000u`) once blanked the rest of the file, and `using namespace std;` once made
the whole AV 206 allowlist inert. Both were found in review of PR #3155, both now
have fixtures.

It mechanically checks **AV 206, 208, 189, 20, 22, 24, 25 and 209**. The rest of
the binding table in [`native-audio.md`](../../../../[internal]rules/native-audio.md)
— AV 1, 115, 119, 142, 197, 201, 207, 215 — is **not** checked, and saying so is
the point: direct recursion (AV 119), `return p + 1;` (AV 215) and a 250-line
function (AV 1) all pass the guard today. Those are review obligations, not
guarantees.

## What exists today

`capability()` — the input to ADR-0043 D6's capability ladder.

```js
const { capability } = require('./native/concord-audiocap');
capability();
// { platform: 'darwin', osVersion: '14.4.1', perProcessAudio: true, reason: '' }
```

A false `perProcessAudio` means the ladder's bottom rung applies: **share video
only, and say so in the UI.** It never means fall back to a system mix — a window
target must never obtain one. That is #2161's invariant, and widening the capture
on a capability miss is that same defect wearing native clothes.

Capture itself is not implemented yet. The Windows floor constant in
[`napi/addon.cc`](napi/addon.cc) is **provisional** and is the single line spike S2
changes; it is written alone and named so that settling risk 3 on hardware is a
one-line edit rather than a hunt. The probe that settles it is in
[`../../spikes/s2-windows-loopback/`](../../spikes/s2-windows-loopback/).

## Build

```bash
cd client/desktop
npm run build:native          # Electron ABI (what the app loads)
npm run build:native:node     # Node ABI
```

Both produce `native/concord-audiocap/build/Release/concord_audiocap.node`, which is
gitignored — it is per-platform build output, never committed.

**One build serves both runtimes, and that is measured rather than assumed.** This
is an N-API addon (`NAPI_VERSION=8`), and N-API is ABI-stable across runtimes: the
Electron-targeted artifact loaded cleanly under Node 26, whose `NODE_MODULE_VERSION`
differs from Electron 43's. A NAN or raw-V8 addon would have been rejected. So
`--runtime` selects headers and, on Windows, the `node.lib` to link against — it does
not make the artifact runtime-specific. Do not add a per-runtime rebuild step on the
assumption that one is required.

**No new runtime dependency, and one new *declaration*.** The seam uses the plain C
`node_api.h` rather than `node-addon-api`, which is ADR-0043 D1's own supply-chain
argument applied to our own build, and satisfies AV 208 at the seam by construction —
the C API has no exceptions to disable.

`node-gyp` did change: it moves from a **transitive** dependency of electron-forge to
a declared `devDependency`, at the version already resolved in the lockfile. Nothing
new enters the dependency tree, but calling that "no new dependencies" overstates it —
the declaration exists so a forge minor bump cannot take the toolchain away, and it is
a change a reviewer should see rather than one the wording glosses over.

## Test

```bash
cd client/desktop/native/concord-audiocap
clang++ -std=c++17 -Wall -Wextra -Werror -g -O1 -pthread \
    test/quantum_ring_test.cc -o /tmp/quantum_ring_test && /tmp/quantum_ring_test
```

**A local macOS compile is not sufficient verification.** macOS uses libc++ and the
CI runners use libstdc++, and the two differ in what they supply *transitively*.
`std::abort()` compiled clean here and failed both sanitizer legs on the first CI run
because `<cstdlib>` was missing — libc++ happened to pull it in, libstdc++ did not. So
every include in these files names the symbols it is there for, and adding a `std::`
facility means adding its header even when the local build already passes without it.

**Nor is a local build of the addon itself.** `scripts/build-native.mjs` spawns
node-gyp's JS entry point under `process.execPath` rather than the
`node_modules/.bin` shim, because on Windows that shim is a `.cmd` and since Node
18.20 / 20.12 — the CVE-2024-27980 argument-injection fix — `spawn` REFUSES to
execute a `.cmd` or `.bat` with `shell: false`, failing with a bare `EINVAL` that
names neither the file type nor the reason. macOS and Linux never hit it. The
obvious workaround, `shell: true`, is precisely what that CVE was about.

**Sanitizers run in CI, not here.** ASAN and TSAN map large fixed shadow regions,
which a sandboxed local shell can refuse — the runtime then hangs or segfaults with
no diagnostic, which looks like a test failure and is not one. `native-audiocap.yml`
runs three configurations on Linux runners: `address+undefined`,
`thread`, and a 120-second libFuzzer smoke run over the ring.

`-fno-sanitize-recover=all` is set on the UBSAN leg deliberately. UBSAN's default is
to print and continue, which exits 0 and reports a green job while having found the
defect. Nothing downstream re-checks this, so a job that exits green already
holding the answer is the one outcome worth engineering against. Note these legs
are visible, not merge-blocking — only `Gitar` and `SonarCloud Code Analysis` are
in the ruleset's required contexts (ADR-0043 § Verification explains why adding
these is not a one-line follow-up).

### The tests were falsified, and two results are worth carrying forward

Seven mutations were injected into the ring and each had to turn the suite red.
Two findings came out of it:

- **A single-threaded test structurally cannot catch a weakened memory ordering.**
  Relaxing `push()`'s release store left every sequential test green. The concurrent
  producer/consumer test catches it on **arm64**, whose weak ordering makes the
  reordering observable — but CI runs on x86-64, where TSO makes a relaxed store
  behave almost identically to a release store and that same test would very likely
  stay green. TSAN is in the matrix because it detects the missing happens-before
  edge directly rather than hoping the hardware reorders during this run.
- **`(int32_t)w - (int32_t)r` is not a mutation.** It has the same bit pattern as the
  unsigned subtraction in two's complement, so for the small differences this ring
  deals in it returns an identical answer — UB under the standard, but not a
  behavioural defect. The wrap-safety property is genuinely broken by comparing
  *absolute* values (`|| w < r`), and that is what the rollover test pins.

## Why the ring is slot-oriented

A byte-oriented ring splits a write across the wrap point, which is where
essentially every ring-buffer bug lives. `QuantumRing` is a ring of **fixed-size
slots**: a producer claims one whole slot, a consumer releases one whole slot, and
nothing is ever split. It costs a little tail padding on a short quantum and removes
an entire defect class from the one component the ADR requires be fuzzed. For code
reachable across a trust boundary that trade is the right way round.

A full ring is a **drop, never a block** — ADR-0043 D4c: back-pressure inside a
real-time callback can only ever be expressed as a drop, and the drop is the newest
quantum. `dropped()` counts them, because a bound that fails as quietly as no bound
buys nothing.
