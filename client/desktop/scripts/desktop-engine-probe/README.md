# Desktop engine probe

After `npm ci && npm run build`, run:

```sh
node scripts/desktop-engine-probe/run.mjs
```

Linux needs a functioning Chromium sandbox and a focusable display, such as
`xvfb-run -a node scripts/desktop-engine-probe/run.mjs`. The runner bounds each
Electron child to 60 seconds and requires both a clean exit and its explicit
`PROBE PASS` line. A timeout, a crash, a missing verdict, or a failed assertion
fails the command. Never add `--no-sandbox` to run it.

The fixture retains its application lifetime while it closes one origin window
and opens the next. Its explicit success/failure exits and the unchanged child
deadline own final shutdown.

The probe loads the actual built sandboxed preload, calls the production
attestation IPC handler from trusted and untrusted real renderer frames, and
runs the production microphone host with the Vite-emitted worklet. It checks
the limited stereo impulse, lookahead delay and processor-error path on both
`app://concord` and `spa-cache://concord` fixture origins. The existing dialog
probe supplies inertness, Escape and close-watcher positive controls. Output
records the actual Electron, Chromium and Node versions.
On macOS and Windows it also runs the existing native-audio host probe against
the addon built for the selected Electron, including its main-process refusal,
utility-process capability exchange and child teardown checks. It does not
start physical audio capture.
The runner binds the declared Electron range, lockfile and installed package
to the executing binary. Storage checks use public fixture data only: Linux's
basic-text backend must refuse persistence; Windows and macOS must complete an
OS-protected asynchronous roundtrip. Calibration runs on an offline graph and
requires complete epoch-tagged windows, the 10/20-second frame caps, and silent
output with a nonzero impulse as its positive control.

`node scripts/desktop-engine-probe/run.mjs --packaged` reads preload, IPC modules
and the emitted worklet from the single produced macOS arm64 `app.asar`. It
still launches Electron's test host, rather than launching the installed app.
The fixture protocols do not perform signed-cache admission. The probe blocks
all external requests and uses a fresh temporary Electron profile.

A fixture-only session frame preload records its actual `process.sandboxed`,
`process.contextIsolated` and renderer type through a frozen context-bridge value.
The built application preload remains unchanged and must independently expose its
working attestation bridge. Electron supplies the preload's `process` as a function
parameter; it is not a global that a later world-999 script can read. Missing,
mistyped or unsafe environment observations fail the probe. Renderer failures carry
their stage and a bounded error message; raw console arguments are not forwarded.

The normal unsigned macOS PR smoke runs the packaged mode after its fuse,
ASAR-digest and payload checks. The separate read-only Windows/Linux workflow
runs the built-file mode. These probes do not establish production signing,
physical capture, platform permission prompts, update install/relaunch or
end-to-end encrypted call and A/V synchronisation evidence. Those gates need
their corresponding artifact and media probes.

The error arm deliberately loads a labeled fixture worklet whose `process()`
throws, through a second bundle of the unchanged production microphone host.
Impulse and calibration keep the Vite-emitted production worklet. Success requires
at least one trusted native failure event on that exact node: `processorerror`
and/or Chromium's verified `error` compatibility event, without duplicates of
either name. It also requires exactly one production error callback and successful
forwarded native port-close and disconnect calls before manual cleanup. The helper's contract tests model evidence refusal; they
do not prove an actual engine event. This fault control does not claim to verify
exception delivery from a MessagePort handler, which the earlier invalid-message
injection did not establish on the tested engines. No event is synthesized.
The fault graph has a real nonzero buffer source. A deadline reports only fixed
input/render flags, native-event/callback/retirement counts and fixed-label
constructor/process-entry counts. Entry markers are diagnostic only and may be
lost when the production host closes its port; they never satisfy a PASS gate.
The result reports both event names and their trusted same-node counts separately.
`standardProcessorError` is `BROKEN` when no native `processorerror` arrives, even
when the compatibility event proves that the production host retires the node.
An absent compatibility event is `NOT RUN`; it is never inferred to work from the
standard event. The observer neither synthesizes an event nor calls the production
callback or retirement methods. The production host handles both native names
through the same once-only failure path without reading the error payload.
