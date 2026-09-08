# S2 — Windows Application Loopback probe

Standalone Win32 console program. No Node, no Electron, no Concord code. It exists to
settle the two questions [ADR-0043](../../../../[internal]0043-per-process-screen-share-audio-capture.md)
§ Verification names as **S2**, both of which can invalidate the design:

- **Risk 2 — is the window's owning PID the PID that renders its audio?**
  `PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE` is supposed to cover apps that
  render from a helper. This is the single most likely cause of "shares video, no audio",
  and it is why OBS still ships its Application Audio Capture as BETA.
- **Risk 3 — is build 20348 an SDK floor or a runtime floor?**
  Microsoft's sample states 20348 (Server 2022), which is *above* Windows 10 22H2 (19045).
  Some sources place the API at 19041. If 20348 is a runtime floor, consumer Windows 10
  gets nothing, and that is a product decision rather than an engineering detail.

**Settle both on hardware. Not from documentation.** A disconfirming result is the spike
succeeding.

## Build

Open **x64 Native Tools Command Prompt for VS 2022**, then:

```
cd client\desktop\spikes\s2-windows-loopback
build.cmd
```

The probe does **not** include `<audioclientactivationparams.h>` — it declares the three
activation structs locally, byte-compatible with the SDK's. That is not a shortcut. Including
the header would make the *build machine's SDK* a gate on a measurement about the *target
machine's runtime*, which is precisely the confusion risk 3 is about.

### Verifying the build without Windows

The probe cross-compiles cleanly, which is how it was checked before ever reaching a
Windows box:

```bash
brew install mingw-w64
x86_64-w64-mingw32-g++ -std=c++17 -Wall -Wextra s2probe.cpp -o s2probe.exe \
    -lole32 -lmmdevapi -luser32
```

Zero warnings under `-Wall -Wextra`, and `mmdevapi` resolves `ActivateAudioInterfaceAsync`,
so the activation ABI declared locally at the top of the file is the one the linker agrees
with. That check caught one build-breaking bug (`HRESULT_FROM_WIN32(ERROR_INVALID_PARAMETER)`
and `E_INVALIDARG` are the same value, `0x80070057`, so they cannot both be `case` labels).

It proves the program *builds*. It proves nothing about what the API *does* — that is
the whole point of running it on hardware.

## Run

```
s2probe.exe --list
```

Prints every visible top-level window with its `HWND`, PID, image name, and title. Pick the
target's `HWND` from that list.

```
s2probe.exe --hwnd 0x00000000000A1B2C --seconds 8
```

Each run does **two** captures back to back and writes two WAVs:

1. `INCLUDE_TARGET_PROCESS_TREE` — the real thing.
2. `EXCLUDE_TARGET_PROCESS_TREE` — **the positive control, and it is not optional.**

### Why the control is not optional

Silence from an include-mode tap does not prove the tap failed. The app may simply not have
been playing. Only the pair is informative:

| include | exclude | Meaning |
|---|---|---|
| audio | silent | **Works — conditionally.** See below: the passes are sequential. |
| silent | audio | **Risk 2 SUSPECTED** — see the caveat below. Not confirmed. |
| silent | silent | **Ambiguous — only you can settle it.** See below; do *not* read this as "nothing was playing". |
| audio | audio | **Not narrowing** — *or* unrelated audio contaminated the control. |

**A pass that did not produce a usable observation is not a row in that table.**
A capture can end up with no usable audio for *seven* distinct reasons and only one of them is silence, so each gets its own
verdict with the responsible `HRESULT` and the pair comparison refuses to run:

| Verdict | What actually happened |
|---|---|
| `ACTIVATION FAILED` | No process-loopback client could be opened — or one failed part way through the format ladder, so the rungs after it were never tried |
| `INITIALIZE FAILED` | Activated, **every** rung tried, none accepted |
| `CAPTURE SETUP FAILED` | A format was accepted, but `SetEventHandle`, `GetService` or `Start` did not take — the stream never ran |
| `TRUNCATED` | Some audio arrived, then the stream stopped signalling for the rest of the interval — a fraction of a capture is not a capture |
| `CAPTURE FAILED MID-STREAM` | The stream started and then a read failed |
| `NO DATA` | The stream started and not one packet ever arrived |
| `INCONCLUSIVE` | Packets arrived flagged `DATA_DISCONTINUITY` — frames were **lost** — and what remained was silent. The **first** packet after `Start()` is excluded from this: see the note below |

### Why the first packet's `DATA_DISCONTINUITY` does not count

`Start()` is a stream state transition, and the first packet has no predecessor to
be correlated with — so Microsoft's definition of the flag — data
"not correlated with the previous packet's device position", possibly from a
stream state transition — is satisfied at startup by construction. Whether WASAPI
actually raises it there is not documented either way: the `GetBuffer` page describes the flag purely as glitch detection and
never mentions startup.

The probe does not guess. The first packet's flag is **recorded and printed** but
kept out of the count that decides the verdict, because the two ways of guessing
cost wildly different amounts:

- **Count it, and it does fire at startup** — every silent pass becomes
  `INCONCLUSIVE`, so the include-audio / exclude-silent pair this probe exists to
  produce can never be reached. The instrument fails on every run.
- **Exclude it, and it does not fire** — one packet of glitch evidence is lost at
  the head of a multi-second capture.

If a run reports the startup marker, that is expected and affects nothing. A
mid-capture `DATA_DISCONTINUITY` still makes the pass inconclusive, as before.
Raised by Codex on PR #3154.


That distinction is load-bearing rather than fastidious. Every one of those states
produces zero frames or zero peak, which is byte-identical to a working tap on a
quiet app. Reading any of them as silence, against an audible control, prints "risk 2"
and sends the operator to hunt child PIDs for a capture that yielded nothing.

**`audio` + `silent` proves isolation ONLY IF the target kept playing.** The two
passes run **back to back, never together** — an earlier version of this file and of
the probe claimed nothing outside the tree was audible "at the same time", which is
impossible by construction. If playback ended between the passes (a track finishing
during the include capture is enough), a whole-system tap produces exactly this pair.
The probe now asks whether the target was still audible for the whole second capture,
and the answer is yours to supply.

**`silent` + `silent` is NOT "nothing was playing", and that wording was wrong here
until this was corrected.** A *broken* include tap delivers valid, zero-filled
packets, and if nothing outside the target's tree happened to make a sound the
control is silent too — producing this exact pair while the target was audible
throughout. That is the live-but-silent failure **S2 exists to find**, and calling
the run invalid would throw it away.

The probe asks the one question that separates them: **was the target audible while
this ran?** If no, start audio and repeat. If yes, this is a finding — the same
live-but-silent shape [ADR-0043](../../../../[internal]0043-per-process-screen-share-audio-capture.md)
D7 found on macOS — and it should be recorded with the `Initialize` rung the probe
reports as accepted.

**`silent` + `audio` is SUSPECTED, never confirmed, and the difference is not
pedantry.** The exclude pass proves only that *some* process outside the target's
tree emitted samples. A notification chime or a background player produces exactly
that signature. The program cannot tell whose audio it captured — **listen to the
exclude WAV.** If it is the target, risk 2 is real for this app — and the renderer
is *outside* the target's tree by definition, because exclude mode suppressed the
target and its descendants and you heard it anyway. So the child PIDs the probe
prints are exactly what **not** to try: every one of them is inside the tree just
proven not to be the source. Look outward — the target's parent, its siblings under
that parent, and any separate audio or media host the app spawns outside its own
tree. The Windows Volume Mixer names the process actually holding the audio session
while the sound is playing; that is the PID to re-run against. If it is something
else, silence the machine and run it again.

**Listen to both WAVs — when there are two to listen to.** The counters say non-zero
samples arrived; only your ears say they were the right app.

A pass can produce perfectly valid counters and **no recording**: the capture buffer
allocation can fail, or the write can. The probe reports `NO WAV` on that pass and
then refuses to tell you to listen to something it never wrote — an ambiguous verdict
that depends on listening becomes *unresolvable from that run*, and saying so is the
point. Re-run before recording anything about risk 2 from a pass with no artifact.

## The verdict layer is tested, and runs anywhere

`verdict.h` holds the decision layer — `CaptureResult`, `PassOutcome`, `classify()`
and `conclusive()`. It is a function of `CaptureResult` alone: no COM, no WASAPI, no
Windows API beyond the `HRESULT` type. `s2probe.cpp` includes it, so there is exactly
one definition and it is the shipped one.

Unix-like hosts:

```bash
c++ -std=c++17 -Wall -Wextra -Werror verdict_test.cc -o /tmp/verdict_test && /tmp/verdict_test
```

Windows, in the same **x64 Native Tools Command Prompt** used for `build.cmd`:

```
cl /nologo /EHsc /std:c++17 /W4 /WX verdict_test.cc /Fe:verdict_test.exe && verdict_test.exe
```

No `/FI` flag is needed: `verdict.h` pulls in `<windows.h>` itself under `_WIN32`, so
it compiles standalone on either platform rather than only when its includer happens
to have done so first.

**Why it exists.** The probe cannot run anywhere but Windows, so until this test was
written nothing about its verdicts had ever been *executed* — only compiled and
reasoned about. Eight review findings landed inside these few functions, every one a
state that **looked like silence and was not**: a ladder abandoned mid-way, a stream
that never started, a stream that lost frames, a stream that delivered no packet.

Beyond one case per outcome, it sweeps all **256** combinations of the result fields
and asserts the invariant the whole design rests on: **`conclusive()` can only ever
admit a pass that genuinely ran to completion.** Every false "risk 2" in review came
from a non-completed pass being treated as an observation, so that is checked
exhaustively rather than by example.

The first run earned its keep immediately — it failed, on a bug in its own portability
shim. `HRESULT` was typedef'd as `long`; that is 32 bits on Windows, where
`0x88890004` is negative and `FAILED()` is true, but 64 bits on Unix, where the same
value is positive. The `CaptureError` case classified as `Audio`. Production was never
affected, but a shim wrong about width would have made every failure-path assertion
pass for the wrong reason — worse than having no test.

## The app matrix

ADR-0043 is explicit that a result of "Chrome works" tells us almost nothing. Run the full
set — each row is a different way an app can render audio:

| App | Why it is on the list | include | exclude | Verdict | Notes |
|---|---|---|---|---|---|
| Chrome (a YouTube tab) | Renders from a child audio service — the canonical `INCLUDE_TREE` case | | | | |
| An Electron app (Slack, VS Code, Discord) | Same multi-process shape as our own client | | | | |
| Spotify (desktop) | Single-vendor app, its own mixer | | | | |
| VLC | Classic Win32 audio path | | | | |
| A game (any 3D title) | Exclusive/low-latency paths, often WASAPI-exclusive | | | | |
| A Java app | JVM audio goes through a different stack entirely | | | | |

Also record, once:

- **`OS build` line from the probe header** — this is the risk-3 answer, but read it
  with the verdict, not on its own. **Activation succeeding is NOT enough to put
  consumer Windows 10 in scope.** Activation can succeed on 19045 while every
  `Initialize` rung is rejected, or while `Start` fails — and retiring risk 3 on that
  would claim the API is usable there having never captured a sample.

  The bar is **at least one pass that reaches a conclusive verdict** (`AUDIO PRESENT`
  or `LIVE BUT SILENT`) on a below-20348 build. Anything else — `ACTIVATION FAILED`,
  `INITIALIZE FAILED`, `CAPTURE SETUP FAILED`, `NO DATA`, `INCONCLUSIVE` — leaves the
  floor unsettled, and the `HRESULT` printed with it is what to report.
- **Which `Initialize` combination took.** The probe walks a ladder of sample rate / channel
  count / buffer / periodicity and prints the `HRESULT` for each, because `GetMixFormat` is
  not supported on a process-loopback client and the accepted set is itself unknown. Whatever
  works here is what `rt/` will use.

## Reporting back

Paste the probe's full stdout for each app plus the filled table above. A partial result is a
fine outcome and directly sizes the capability ladder in ADR-0043 D6 — it is not a failure to
be worked around.

## Scope

This directory is a **throwaway spike** and lives outside `client/desktop/native/` on purpose,
so [`[internal]rules/native-audio.md`](../../../../[internal]rules/native-audio.md) does not bind
it. The JSF++ profile applies to `native/concord-audiocap/rt/`, where an allocation is audible;
it does not apply to a measurement instrument whose whole job is printing.

Delete this directory once S2's results are recorded in ADR-0043.
