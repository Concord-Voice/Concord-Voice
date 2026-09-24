# media-policer-probe (#2153)

A **local, manual** gate for the media-rate policer. It is never wired to CI: it needs a GUI
session, the dev stack, and two signed-in accounts. Read the `PROBE …` line, never just the
exit code.

| Line                                  | Meaning                                                                           | Exit  |
| ------------------------------------- | --------------------------------------------------------------------------------- | ----- |
| `PROBE PASS trip-audio`               | the mic latched                                                                   | 0     |
| `PROBE FAIL trip-audio`               | the sender owed a trip and no latch followed within one interval + 5 s: **a policer defect** | 1     |
| `PROBE INEFFECTIVE trip-audio`        | the sender never owed a trip: **the probe method failed**, not the policer        | 2     |
| `PROBE FIT` / `PROBE MISFIT envelope` | every stock peak is ≤ / > 0.8 × its limit (spec §9 fit criterion)                 | 0 / 3 |

## Setup

1. `./scripts/concord-dev.sh up --skip-client`, then run vite on `localhost:3001`.
   If a sibling session already holds `[::1]:3001`, run
   `npx vite --host 127.0.0.1 --port 3001` and pass `--ipv4-vite` to `launch`.
2. `cd client/desktop && npm run build:preload && npx tsc -p tsconfig.main.json`
3. `node scripts/media-policer-probe/probe.mjs launch --clients 2 --base-port 9301`
   - Each client gets its own `--user-data-dir`, and so its own single-instance lock.
   - Each client gets `--allow-loopback-in-peer-connection`: the dev media plane announces
     `127.0.0.1`, and without the flag ICE fails with zero remote candidates.
   - Each client gets fake media devices, so no TCC prompt appears.
4. Register or sign in two accounts, put them on one server, and join the same voice channel.
   The dev email code is in `logs/control-plane.log` (`DEV MODE — email verification code`).
5. `node scripts/media-policer-probe/probe.mjs status --port 9301`, and the same with `--port 9302`.
   - `status` aborts with `SECOND MODULE INSTANCE` if vite HMR has handed the probe a fresh module.
   - Restart vite **and** both clients after **any** source edit.

## Positive control — `trip-audio`

```
node scripts/media-policer-probe/probe.mjs trip-audio --port 9301 --max-bitrate 510000
node scripts/media-policer-probe/probe.mjs status --port 9302   # peer: client 1 isMuted true
node scripts/media-policer-probe/probe.mjs status --port 9301   # owner: mediaPolicyPaused.mic set
```

`--ceiling`, `--min-ptime` and `--max-manual` select the caps that `deriveLimits` uses. They
default to the free tier: 96 000 / 20 / 5 000 000, giving 216 000 bps, 75 pps and 7 500 000 bps.

### Method

`trip-audio` behaves like a non-cooperative client (CV-CAN-020). It:

1. closes the stock mic through `voiceService.closeProducer('mic')`;
2. produces a new `mic` through the app's own `produceEncrypted`, so E2EE stays attached, with a
   looping white-noise track and `opusMaxAverageBitrate` set to `--max-bitrate`;
3. stops the stock stats loop (`stopPacketLossMonitor`).

Each step exists because a simpler method was measured and failed:

- **The stats loop rewrites the mic's `maxBitrate`** to tier × FEC headroom every 5 s, so
  raising `encodings[0].maxBitrate` on the stock mic lasts one tick at most. Measured
  2026-09-23: 93.7 kbps peak against the provisional 212 kbps limit; and a re-produced 510 kbps mic ran at
  537 kbps for 5 s, then flat at 96 kbps × 1.28 = 123 kbps. A patched client does not run it.
- **The SDP `maxaveragebitrate` is not a cap.** With the loop stopped, a stock mic declared at
  `maxaveragebitrate=96000` sent 145 kbps at `maxBitrate` 144 000. mediasoup-client writes it
  from `codecOptions` (`RemoteMediaSection.js`) into the local answer only, so the server's
  `enforceAudioTierGate` never sees a declared bitrate: this producer declares ptime 20 and no
  bitrate, and only measurement can catch it.
- **White noise**, because the stock mic source is a sparse fake-device beep and Opus cannot
  compress noise, so the encoder spends its whole target whatever its VBR does.

### Verdict

The policer trips on **sustained** excess, not on a peak. Debt grows by `bits − limit·dt` and
trips at `limit × TRIP_BUDGET_S` (`settleSlot` in `mediaPolicer.ts`). The probe replays that
same bucket on the client's own counters:

- **INEFFECTIVE:** the client-side bucket never filled, so no trip was owed.
- **FAIL:** the bucket filled and no latch followed within `POLICER_INTERVAL_MS` + 5 s.

A single burst above the limit that the bucket absorbs is correct policer behaviour, not a
FAIL. The first version of this probe compared a 1-second peak with the limit, and reported
exactly that burst as a FAIL.

After a PASS the mic stays latched: the noise producer is paused server-side. Leave and rejoin
to get a stock mic back.

A second strike inside the strike window evicts. Expect client 1's "Removed from voice"
dialog, and a "You can't join voice yet" dialog on a rejoin attempt. Those are the §1c/§1d
captures for T9.

## Envelope scenarios (T0)

```
node scripts/media-policer-probe/probe.mjs envelope --port 9301 --label mic-standard --seconds 600 --out /private/tmp/t0-mic-standard.jsonl
```

- `--out` **refuses any path inside the repository**. A per-second rate series leaks speech
  activity (C8) and is never committed. The printed peaks go into the T0 note.
- Scenarios (spec §9). Set the state first, then run `envelope`:
  - **Stock mic at each free tier.** Pick the tier in Settings and record with
    `--label mic-<tier>`. Induce loss until FEC reaches +50%. On macOS: `dnctl` plus `pfctl` on
    `lo0` against the media plane's RTC port range; undo it afterwards.
  - **DTX on and off.** Use the tier's DTX setting.
  - **Genuine stereo system audio (music).** Start a share with sound by hand, then run with
    `--label screen-audio-music`.
  - **Camera simulcast.** Pass `--camera-on`. Make a scene change (wave at the fake device, or
    swap the device) to force keyframes.
  - **Screen share.** Start it by hand. Scroll or switch windows to force keyframe bursts.
- These numbers are **client-side outbound bytes including RTP headers**. The authoritative T0
  figures are the **server-side** counters from the Task 3 reader, taken through a local,
  never-committed debug log (spec §9). Use this probe to set up and correlate scenarios, not to
  replace that log.

## T0 evidence

The measured scenario peaks, the positive-control trip time and the final constants are in
`[internal]reports/2026-09-23-2153-media-policer-t0.md`. The probe imports its limits
from `services/media-plane/src/lib/mediaPolicer.ts` at run time, so they always match what the
media plane enforces; `--ceiling`, `--min-ptime` and `--max-manual` are tier caps, not constants.
