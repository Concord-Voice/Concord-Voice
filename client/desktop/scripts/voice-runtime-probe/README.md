# Real voice runtime probe

This opt-in probe drives two isolated, sandboxed Electron renderers through an actual
local mediasoup worker. It runs production `VoiceService` sender/receiver transform
hooks, `e2eeWorker`, `MediaEncryption`, camera encoding ladders, ICE diagnostics and the
production A/V sampling timer. It also runs an independent detector on unmodified
real stats. Continuous synthetic audio and video must decrypt and play for at least
95 seconds. Every measured poll must advance both actual decoded and presented frame
counters within the progress deadline. Camera reproduction must retain the capture track,
advance new per-consumer counters, and receive its own production transform-verification verdict.

The fixture replaces account setup, key-service provisioning and signaling only.
It installs a fresh random test CSK and real transports into the production singleton.
It does **not** test login, join authorization, Socket.IO, RoomManager, key rotation,
screen capture or the app's complete receive-audio graph. No RTC object or `getStats`
method is mocked. Only production code increments `avSyncObservationCount`.

The detector positive control copies the real reports into a **separate** detector and
adds a declared 2 ms/s offset. This proves that detector can trip; it is not evidence of
a discontinuity induced in the actual engine or media stream. The healthy-call witnesses
always use untouched real reports.

Run from a normal ephemeral Linux or macOS runner, as a non-root user, with supported
Chromium sandboxing and a display (Xvfb is suitable on Linux). Install the exact locked
dependencies normally in both workspaces. No signing, secrets, database or .env is needed:

```bash
npm --prefix services/media-plane ci
npm --prefix client/desktop ci
cd client/desktop
MEDIA_PROBE_OUTPUT="$RUNNER_TEMP/voice-runtime.json" \
  node scripts/voice-runtime-probe/run.mjs
```

On a Linux runner with Xvfb already provisioned, prefix the command with `xvfb-run -a`.
The launcher explicitly requests Chromium sandboxing from Playwright; its Linux
default otherwise adds a flag that the fixture deliberately refuses.
Do not add `--no-sandbox`, disable isolation, or grant container capabilities to make it
pass. A host that cannot launch sandboxed Electron reports `NOT RUN`, exits nonzero and
cannot satisfy the media gate. The probe itself does not install host packages.

`MEDIA_PROBE_SERVER_DIR` may select another installed media-plane worktree (for example,
the server update under review). Its installed version must match its own lockfile.
`MEDIA_PROBE_DURATION_MS` defaults to 95000 and accepts only 95000..180000.
The existing supported `MEDIASOUP_WORKER_BIN` override can select an exact candidate
worker; record that separately from proving its ordinary installation/build path.
The artifact distinguishes the JavaScript package version from the resolved native
worker path and SHA-256. `MEDIA_PROBE_WORKER_SHA256`, when supplied from separately
verified candidate-artifact metadata, must match that native binary. Without it the
artifact explicitly reports native candidate digest binding as `NOT VERIFIED`; a
healthy runtime verdict does not establish upstream native-artifact provenance.
`MEDIA_PROBE_ELECTRON` may select an already installed executable; its actual version
must equal the resolved lockfile version, as must the installed package. Manifest and
root-lock declarations must agree and admit that resolved version. Missing `electron/path.txt`
without this override is an installation failure, never a reason for a lazy download.

The fixture's real-worker setup/ownership check can run without a display:

```bash
node --test scripts/voice-runtime-probe/local-sfu.test.mjs
node --test scripts/voice-runtime-probe/witness-contract.test.mjs
node --test scripts/voice-runtime-probe/audio-witness.test.mjs
```

Capture stdout or `MEDIA_PROBE_OUTPUT` as the CI artifact. `PASS` requires exact runtime
Electron/Chromium/Node/mediasoup versions, sandbox/isolation, real E2EE entered frames,
decoded synthetic content, selected ICE pair markers, a production sampler count above
zero, an independent usable/steady verdict, the separate detector fault control,
advancing decoded/presented frames, separately verified replacement consumers and
confirmed native-worker exit. Original verbose console logs,
keys, SDP and raw media stats are deliberately excluded from the artifact.

This harness is prepared for CI execution. Its real media leg has not run successfully
in the restricted authoring host; syntax/build checks do not constitute runtime proof.

A startup preflight checks the production service members this test-only integration
uses and fails before key setup when that contract changes. No production test API is
added. Failures retain only allowlisted transport/ICE/audio states, media counters,
and worker/window/error classifications. They retain no raw console arguments,
identity, key bytes, SDP or RTC reports. Partial diagnostics cannot satisfy a PASS gate.
The Linux launch regression uses an argument recorder to inspect the real Playwright
process boundary; that unit test does not execute Electron or prove renderer behavior.

The synthetic remote-audio witness follows the app's modern receive graph: a muted
audio element pulls the same remote stream that feeds the waveform analyser. It
resumes the server consumer before awaiting playback, which has a ten-second bound.
The element remains muted and the graph's output gain remains zero; the waveform
must still exceed the existing RMS threshold. Cleanup pauses and releases the owned
element and nodes without stopping the consumer's track or closing the shared context.
The lifecycle tests do not prove actual decrypted audio; the real engine run must do so.
