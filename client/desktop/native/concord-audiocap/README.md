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

## Where it loads, and who drives it

`index.js` **throws** unless `process.type === 'utility'` (ADR-0043 D5): a
memory-safety bug in `rt/` must not own the process holding SSO tokens and the
update path. The guard is gated on `process.versions.electron`, so unit tests
importing this module under plain Node still exercise the path logic — which also
means CI's `node -e "require(...)"` load step proves nothing about the guard. The
job that does is `electron-host-probe` in `build.yml`, a main-only Electron app
asserting the require **fails** (#3195).

The `utilityProcess` that hosts it, the control channel main uses to drive
`start`/`stop`, and the PCM path out to the renderer are all TypeScript and live
outside this package: `src/main/audiocapHost.ts`, `src/main/audiocapChild.ts`,
`src/preload/audiocapRelay.ts`, `src/renderer/services/voice/screenAudioBridge.ts`.
See [`docs/architecture.md`](../../../../docs/architecture.md) § "Per-process
screen-share audio".

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

`start(options, onQuantumAvailable)` / `drain(into)` / `stop()` / `status()` — the
PCM transport seam (#3195, #3197).

**macOS has a real backend since #3197 PR 2.** `start()` on a macOS 14.4+ machine
no longer answers `NoBackend`; with no target supplied it answers `NoTarget`,
which is the refusal happening *before* any Core Audio call. The state machine
lives in `rt/platform/macos/tap_backend.h` — header-only and free of any Apple
header, over a POD of function pointers in `hal_api.h` — so all of it runs on the
Linux ASAN/UBSAN/TSAN legs against a fake HAL. `tap_backend.mm` is ten wrappers,
a version read and the singleton, and is the only file here that names Core Audio.

**`status()` carries two counters R9 forced.** Consent denial does not fail
`start()`: every Core Audio call returns `noErr` and the IOProc fires on schedule
carrying zeros, so `callbackTotal` cannot see it. `signalTotal` counts callbacks
that admitted at least one non-zero sample, and `silentSinceStart` latches once
callbacks have arrived, `signalTotal` is still zero and a 10 s budget has elapsed.

**`silentSinceStart` is ADVISORY and must not be read as a permission verdict.**
A granted tap on a paused or muted app produces byte-identical all-zero output —
measured — so nothing at this seam distinguishes the two. It never faults, never
stops a capture, and is cleared permanently by the first non-zero sample.

```js
const { start, drain, stop } = require('./native/concord-audiocap');
start(
  { quantumMs: 10, sampleRate: 48000, channels: 2, frameCount: 480, ringSlots: 8 },
  onQuantumAvailable
);
// { ok: true }  in a CI/test build
// RELEASE build, and it is PLATFORM-DEPENDENT since #3197 PR 2:
//   macOS 14.4+ -> { ok: false, reason: 'NoTarget' }  (a real backend refused
//                  this target -- the options above carry no targetPids)
//   elsewhere   -> { ok: false, reason: 'NoBackend' } (no producer compiled in)
// -- see below
```

`options` is checked for **exact equality** with the constants compiled into
[`rt/quantum_header.h`](rt/quantum_header.h), which mirror
`src/shared/audiocapProtocol.ts`. It is not a negotiation: a mismatch means the two
ends of the transport disagree about the wire, which is a fault to report
(`BadOptions`) rather than a geometry to adopt.

`onQuantumAvailable` is a **bare availability edge with no payload**. It is a
threadsafe function with `max_queue_size = 1`, always called non-blocking, so
signals coalesce; audio flows through the ring, never through that queue. Drain in a
loop until `drain()` returns `{ ok: false }` or you reach your credit bound — a
missing signal is not a missing quantum.

`drain(into)` takes a 3872-byte `ArrayBuffer` and fills it in place. It allocates
nothing on the payload path, and it **fails closed when capture is not running**, so
a signal still pending when `stop()` lands cannot deliver audio for a share that has
ended.

`stop()` does **not** join a producer thread. Teardown is a POST-CONDITION —
"when `stop()` returns, no further sink call occurs from any thread and every OS
artefact is destroyed" — rather than a mechanism, so a thread-driven backend can
satisfy it by joining and a callback-driven one (a Core Audio `IOProc`, a WASAPI
callback) by stopping and destroying the OS artefact directly. `rt/sink_gate.h`
enforces the post-condition independently of whether a backend keeps its word:
`stop()` calls the backend's own stop, closes the gate, and waits bounded for
in-flight sink calls to quiesce **and for the pump's activity witness
(`QuantumPump::activityTotal()`, moved by both `noteCallback()` and `submit()`) to
hold still** — the gate alone answers "is anyone inside right now", never "does a
producer still exist". The release-or-abandon decision is then read from the
`poisoned` latch rather than from what that wait saw: the handle is released only
when the wait quiesced AND the process is not already poisoned, because `stop()` is
idempotent and a later run finding a quiet gate must not release a handle an earlier
one deliberately abandoned. On timeout it abandons the threadsafe-function
handle rather than releasing it and latches a process-lifetime `poisoned` flag
(`rt/teardown.h`); a poisoned process answers every later `start()` with
`Poisoned` and can only be killed. `rt/capture_backend.h` is the platform-neutral
contract both #3196 and #3197 implement against, `rt/quantum_pump.h` regroups
whatever-sized OS callback deliveries into fixed 480-frame quanta, and
`rt/frame_pack.h` is the pure channel/layout repacketizer — mono is duplicated to
stereo by a byte shuffle, never mixed or resampled. Neither file does
floating-point arithmetic on a sample, which keeps both inside the JSF++ profile
with no new deviation.

**Windows has no real backend yet; macOS does.** On Windows and Linux a release
build has no producer at all and `start()` returns `NoBackend`, which the host
resolves to video-only with a reason — never to a system mix (#2161). Windows
ProcessLoopback is #3196.

On **macOS 14.4 and above** the Core Audio process tap shipped with #3197 PR 2, so
a release build there has a real producer: `platformBackend()` returns it and
`start()` reaches target validation, answering `NoTarget` when no target is
supplied rather than `NoBackend`. Below the 14.4 product floor `platformBackend()`
returns `nullptr` and `NoBackend` is still the answer. Nothing is reachable from
the UI on either platform until #3198 wires the ladder rung that supplies a
target. The Windows floor constant in
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

**The synthetic source is opt-in at CONFIGURE time, and that is constraint C10.**

```bash
npx node-gyp rebuild -- -Daudiocap_synthetic=1   # adds concord_audiocap_synthetic.node
```

A default build produces one target. The CI/test variant is a second `.node` with a
second name, and `index.js` can only ever resolve `concord_audiocap.node` — so the
synthetic binary is not merely excluded from `app.asar`, it is never compiled on the
release path and is unreachable through the shipped loader even in a dev tree. If a
change here ever requires editing `EXPECTED_NATIVE` in `scripts/verify-asar-payload.sh`
or the `native/` lookahead in `forge.config.ts`, the gyp gate is wrong — not the
allowlist.

**One build serves both runtimes, and that is measured rather than assumed.** This
is an N-API addon (`NAPI_VERSION=8`), and N-API is ABI-stable across runtimes: the
Electron-targeted artifact loaded cleanly under Node 26, whose `NODE_MODULE_VERSION`
differs from Electron 43's. A NAN or raw-V8 addon would have been rejected. So
`--runtime` selects headers and, on Windows, the `node.lib` to link against — it does
not make the artifact runtime-specific. Do not add a per-runtime rebuild step on the
assumption that one is required.

**No new runtime dependency, and one new _declaration_.** The seam uses the plain C
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
clang++ -std=c++17 -Wall -Wextra -Werror -g -O1 -pthread \
    test/rt_contract_test.cc -o /tmp/rt_contract_test && /tmp/rt_contract_test
```

`rt_contract_test.cc` is the backend-contract, gate and teardown suite; it uses a bare
`CHECK` macro and no framework, and **only its exit status is a signal**. Its printed
check count varies between runs by design — the concurrent cases `CHECK` inside spin
loops, so the total tracks machine load, not coverage.

**A local macOS compile is not sufficient verification.** macOS uses libc++ and the
CI runners use libstdc++, and the two differ in what they supply _transitively_.
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
runs two sanitizer configurations on Linux runners — `address+undefined` and
`thread`, each compiling and running BOTH `test/quantum_ring_test.cc` and
`test/rt_contract_test.cc` under the same flags — plus a libFuzzer smoke leg of two
targets at 60 seconds each, `fuzz_quantum_ring` and `fuzz_quantum_pump`. That budget
was **split, not doubled**: adding the pump target did not lengthen the job.

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
  _absolute_ values (`|| w < r`), and that is what the rollover test pins.

## Why the ring is slot-oriented

A byte-oriented ring splits a write across the wrap point, which is where
essentially every ring-buffer bug lives. `QuantumRing` is a ring of **fixed-size
slots**: a producer claims one whole slot, a consumer releases one whole slot, and
nothing is ever split. It costs a little tail padding on a short quantum and removes
an entire defect class from the one component the ADR requires be fuzzed. For code
reachable across a trust boundary that trade is the right way round.

A full ring is a **drop, never a block** — ADR-0043 D4c: back-pressure inside a
real-time callback can only ever be expressed as a drop, and the drop is the newest
quantum. `overrun()` counts them, because a bound that fails as quietly as no bound
buys nothing.

It **saturates rather than wrapping**, and that is the whole reason it is not a
plain `fetch_add`: this counter is the drop _witness_ — it is stamped into every
quantum header and surfaced in the UI — and a wrapped `u32` reads as "no drops",
which is the one answer it must never be able to give.

**There is exactly one drop site, by construction.** `ringSlots == creditBound == 8`,
so a consumer that refuses to drain past its credit bound _is_ a ring that fills:
credit exhaustion and ring overflow are the same event, counted once, with nothing
to reconcile between them. A second bound anywhere — a queue behind the signal, a
free list, a catch-up batch on the sender — would create a second drop site that
nothing counts.

**The counter reaches the consumer up to `ringSlots` quanta late, and the `seq` gap
does not.** `overrunTotal` is stamped at _capture_ time, beside a capture-time
timestamp, so the quanta already queued when a drop happens still carry the older
count; a consumer resuming after a stall drains those first. The independent witness
— a gap in `seq` — is visible on the very first quantum after the stall. Both
observers are required, and this is why: neither is a substitute for the other.
