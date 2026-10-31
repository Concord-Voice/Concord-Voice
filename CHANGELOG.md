# Changelog

All notable changes to Concord Voice are recorded in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
This file names the pending section `[Prerelease]`, not `[Unreleased]`, after
the release channel that ships it first.

To add an entry, write one line under the matching `###` category in
`[Prerelease]`: Added, Changed, Deprecated, Removed, Fixed, or Security. Start
the line with a bold area tag, such as `**Voice:**`. Mark a breaking change with
`**BREAKING**` after the tag. The category sets the recommended version bump:
Added, Deprecated and Removed suggest a minor, and the rest suggest a patch. A
**BREAKING** entry suggests a major (a minor before 1.0). Keep one summary of 1-3
sentences above the categories. The post-update dialog shows only that summary.
Do not add a paragraph for each change. Edit the summary only when your change
belongs in it, and write it if the section has none. Do not edit the
`**N Total Changes**` line. The promoter recounts it when it cuts a release.

## [Prerelease]

Paid plans are ready: Supersonic and Wing subscriptions, a 30-day Supersonic
trial, plan changes, and code credit. Servers can require MFA for dangerous
actions, purges and message timers can keep pinned messages, and the microphone
gate can learn from your voice.

**107 Total Changes** | 14 Added | 23 Changed | 48 Fixed | 22 Security

### Added

- **Tray:** The tray icon now shows both stars of the Concord Voice mark, and it reflects your status. It shows when you are in voice, muted, deafened, sharing your screen, reconnecting, or have unread messages or mentions. On macOS and Windows, click the icon to open a menu with Mute, Deafen, Leave voice, Stop sharing, your presence, and a shortcut to your newest unread conversation. On Linux, click the icon to open the window; the menu comes from your desktop's tray host.
- **Settings:** Settings ▸ Appearance has a new Tray Icon setting (Menu Bar Icon on macOS). Choose Automatic, Light, Dark, or Colour to match your taskbar or menu bar.
- **Billing:** Settings ▸ Subscription lists each plan with its monthly and yearly price and opens a secure Stripe page to subscribe or manage billing.
- **Billing:** New subscribers get a 30-day free trial of Supersonic. The card is collected first, and billing starts when the trial ends. Wing has no trial.
- **Billing:** Subscribers can change plans in Settings, see the amount due before an immediate switch, and withdraw a change that is booked for renewal.
- **Billing:** A Wing subscription can share Supersonic with one friend. Invite the friend from Settings ▸ Subscription. You can change your +1 once every 30 days.
- **Billing:** A premium code redeemed on a paid subscription becomes account credit that your next renewals use first. A code for a different plan shows the credit and asks you to confirm.
- **Servers:** Owners can require MFA for dangerous actions from Server Settings. Members then confirm each dangerous action with a code or security key ([#3454](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3454), [#3456](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3456), [#3675](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3675)). API: send `mfa_code` on a retry after a 403 `mfa_required`. `GET /api/v1/server/capabilities` reports `features.mfaEnforcedDangerousActions`.
- **Servers:** Delete Role asks you to confirm and names the role first ([#3675](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3675)).
- **Messages:** Every purge has an "Include pinned messages" option, off by default ([#3458](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3458), [#3552](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3552)). API: `include_pinned` and `features.purgeKeepsPinned`.
- **Messages:** Message expiration timers can keep pinned messages, and new timers keep them by default ([#3459](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3459)). API: `include_pinned` and `features.expirationPinnedPolicySupported`.
- **Messages:** Up Arrow in an empty message box edits your last message ([#1959](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1959)).
- **Voice:** Auto Calibrate and Manual Calibrate can suggest a fixed microphone gate threshold from a room-and-voice check. Manual Calibrate also shows a live meter ([#4118](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4118), [#3637](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3637)).
- **Operations:** `provision-secrets.yml` now carries `APP_ATTEST_APP_IDS`, so production can turn on iOS App Attest from a GitHub variable (ADR-0010 D17).
- **Operations:** Blue/green media deployment can move new voice rooms to a second instance while existing rooms drain. It is off by default ([#2752](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2752)).
- **Operations:** A separate West staging deployment path is available. It stays off until its secrets, access, and checks are complete. West is not a production site ([#2751](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2751)).
- **Operations:** Groundwork, off by default: managed ingress verification ([#3667](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3667)), managed egress ([#4048](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4048)), Redis credential rotation ([#3930](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3930)), TURN credential rotation ([#3931](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3931)), and shared media listeners. Each needs host checks and approval before activation.

### Changed

- **Voice:** The desktop call library handles mixed audio and video RTP header extensions with mediasoup-client 3.25.3.
- **Voice:** Updated server voice transport dependencies.
- **Accessibility:** The desktop app follows the system's reduced-motion setting, hides decorative icons from screen readers, and shows text while bundled fonts load ([#4181](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4181)).
- **Desktop:** Updated runtime and updater dependencies.
- **Plans:** Every account can create any number of servers, search as far back as a subscriber, and customize theme colors. One account can create up to 10 servers in 24 hours ([#3861](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3861)).
- **Desktop:** Updated navigation dependencies.
- **Mobile:** The in-development mobile client, not yet released, now builds on React Native 0.88.0.
- **Web:** Opening the web address in a browser shows a link to download the desktop app, not an "Update Required" prompt.
- **Desktop:** Updated installer tooling.
- **Search:** Message search looks back 90 days or 600 messages at a time, loads older results as you scroll, and says when it is paused or stopped ([#3875](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3875)).
- **Search:** Jump reaches messages that are not loaded or are in another channel, from search results, pins, and reply quotes ([#3894](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3894)).
- **Settings:** Subscription is under Account, protection settings are under Security, and device permissions are under About & Updates ([#3780](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3780)).
- **Settings:** Sections start collapsed and remember what you expand while you use Settings and for 30 minutes after you leave ([#2365](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2365), [#3651](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3651)).
- **Settings:** Audio and video device controls sit with their settings. An app-only Microphone Level (0–200%) shows when automatic gain control (AGC) is off ([#3635](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3635)).
- **Security:** Purge, purge protection, DM Clear, and Security settings prompts ask only for the methods your account uses. A passkey or security key works in one step ([#3660](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3660), [#3673](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3673)).
- **Privacy:** New accounts start with stricter defaults for friend requests, voice details, call presence, and notification previews. Existing choices do not change.
- **Friends:** Under Mutual Servers, people who share a server or a mutual friend with you, or who have your friend code, can send you a request. Username changes wait six months on Free and three on Premium.
- **Recovery:** Recovery Circle recovery is temporarily paused. Your saved contacts stay in Settings. Use another recovery method to regain access.
- **Voice:** The Noise Gate has Dynamic, Auto Calibrate, Manual Calibrate, and Off modes. Dynamic adapts during a call or Microphone Test ([#3657](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3657)).
- **Voice:** With AGC on, calls and Microphone Test limit outgoing microphone sample peaks to −6 dBFS through one shared processing path ([#3653](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3653)).
- **Voice:** Turning DTX off in Advanced settings disables it for every quality preset.
- **Voice:** Voice, camera, and screen sends use separate plan-based bitrate ceilings. If video use stays over the limit, the server pauses video for a cooldown and voice continues.
- **Voice:** When voice is paused for server maintenance, joining a call says so and suggests that you try again in a few minutes.
- **Voice:** When operators enable blue/green mode, calls recover after the old media instance stops at its two-hour drain cutoff ([#2752](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2752)).
- **Video:** Enable HDR and Prioritize HDR Codecs are separate settings. Neither setting guarantees HDR capture or display.
- **Video:** Webcam speaker frames keep a fixed size and show a steady highlight ([#3595](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3595), [#3596](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3596)).
- **GIFs:** GIF personalization sends KLIPY an ID derived from your Personalization ID. With personalization off, GIF browsing uses a temporary ID that changes about every 30 minutes.
- **Bug reports:** Screenshots have storage allowances and expire after 72 hours if the report is abandoned.
- **Desktop:** Electron 44.5.0 ([#3599](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3599)), Electron Forge 8 packaging ([#3794](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3794)), lucide-react 1.48.0 ([#3645](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3645)), mediasoup-client 3.24.1, and socket.io-client 4.8.4 ([#3602](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3602)).
- **Server:** The control plane builds with Go 1.27.2 ([#3823](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3823)).

### Fixed

- **Voice:** The media server now forwards your app's attestation when it checks your voice access, a step toward letting servers require an attested app ([#4211](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4211)).
- **Sign-in:** A signed-out session no longer retries its rejected refresh token every 10 seconds in the background ([#4205](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4205)).
- **Voice:** Calling someone back in a DM joins the call still open there instead of silently stopping, a failed call start now shows an error, your camera comes back after a brief connection drop, and 1:1 DMs show Join voice call during a call ([#4179](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/4179)).
- **Voice:** Microphone processing failures now notify the existing error handler and release the failed audio node once ([#4147](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4147)).
- **Billing:** Blocking someone ends a Wing seat between you, whichever side blocks. Unfriending or blocking also cancels a pending seat invite at once; unfriending alone keeps an active seat ([#4172](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4172)).
- **Billing:** Switching from a monthly to a yearly plan works again. It credits the unused part of the month and keeps your billing date.
- **Billing:** Subscribing twice at the same time no longer creates a second subscription.
- **Billing:** Opening billing no longer withdraws a booked plan change. Cancellation keeps access through the paid period, and the plan card shows your actual price.
- **Billing:** Redeeming two codes at the same time no longer fails ([#4019](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4019)).
- **Billing:** Subscription updates apply in order, so an older downgrade cannot follow a newer upgrade ([#4079](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4079)).
- **Billing:** Recovery changes only subscriptions that its environment owns, and canceled seat requests still notify members ([#4110](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4110)).
- **Chat:** The desktop chat connection no longer drops and reconnects every 14 minutes.
- **Chat:** Chat no longer drops for everyone every 30 seconds when someone sits in a voice channel that none of their friends can see.
- **Chat:** Changing activity sharing settings in large servers no longer disconnects unrelated users.
- **Chat:** Chat stays responsive under excessive connection traffic ([#3897](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3897)).
- **Messages:** Failed channel messages keep their text and retry controls after a brief disconnect ([#4079](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4079)).
- **Messages:** History no longer skips messages that share a timestamp, in channels or DMs ([#3875](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3875), [#3989](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3989)).
- **Messages:** An edit is held to the character limit ([#3857](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3857), [#3873](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3873)).
- **Messages:** A delayed failed edit no longer closes an edit in another chat panel or loses its text.
- **Messages:** Attachments upload and send in call text chat ([#3985](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3985)).
- **Messages:** Attachments that scroll out of view release their decrypted data ([#4086](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4086)).
- **Messages:** A server purge refreshes only the channels it changed ([#4052](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4052)).
- **Messages:** Reading a server's channels clears its unread indicator ([#3777](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3777)).
- **Invites:** Invite links keep their previews and open the selected invite after sign-in. Profile and server images load through the signed-in account ([#3942](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3942)).
- **Friends:** Copying a friend code no longer leaves a timer running after you leave Add Friend.
- **Roles:** Failed role and override saves show an error. A purge refused for MFA asks for a code or links to setup ([#3675](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3675)).
- **Voice:** Calls keep your mute and deafen choices through recovery, and DM calls keep their call type ([#4086](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4086)).
- **Voice:** Dynamic gating releases stale thresholds and keeps phrase evidence through pauses. Invalid saved thresholds reset to the default ([#4113](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4113)).
- **Voice:** The microphone resumes after a settings change with the Dynamic noise gate ([#3670](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3670)).
- **Voice:** If AGC fails, the microphone stays paused ([#3658](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3658)).
- **Voice:** Joins capture the selected microphone once and explain how to recover if capture fails ([#3619](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3619)).
- **Voice:** Audio menus show one Default option that follows the operating system ([#3619](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3619)).
- **Voice:** Join errors show in a small centered prompt and no longer push the layout down.
- **Voice:** Removing someone from voice during their join waits for their connection to close ([#3870](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3870)).
- **Voice:** Disconnects wait for media cleanup before the session is released ([#3778](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3778), [#2624](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2624)).
- **Voice:** Voice moderation and group DM removals survive broker outages ([#3818](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3818), [#2623](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2623)).
- **Voice:** Voice startup waits for enforcement. If enforcement fails, new joins stop and calls close within 30 seconds ([#3852](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3852)).
- **Voice:** Blue/green retries keep call and room ownership ([#3941](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3941)).
- **Video:** Video stays on when someone with incompatible video support joins ([#4051](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4051)).
- **Video:** H.264 video no longer goes black after another camera turns on ([#3985](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3985)).
- **Video:** Camera swaps keep local previews, and late media from an earlier session is closed ([#3650](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3650)).
- **Video:** Camera recovery uses codec and HDR settings that you save during a restart ([#4079](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4079)).
- **Sign-in:** Email-code sign-in sends the code ([#3672](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3672)).
- **Sign-in:** Sign-in offers backup codes only when they can work ([#3567](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3567)).
- **Sign-in:** Session checks can use your security key ([#3663](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3663)).
- **Settings:** Verification setup no longer loses unsaved settings or applies a stale confirmation ([#3452](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3452), [#3711](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3711), [#3717](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3717)).
- **Accessibility:** Settings uses a native microphone level meter and respects reduced motion ([#4113](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4113)). Screen readers skip decorative security icons ([#3869](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3869)).
- **Desktop:** Pilot builds can connect alongside Stable builds ([#3905](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3905)).
- **Desktop:** Windows updates recheck the installer before restart, including saved downloads ([#2455](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2455)).
- **Desktop:** Group DM icons load through authenticated requests ([#3771](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3771)).
- **Server:** Animated image uploads use less memory ([#3981](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3981)).
- **Self-hosting:** Calls through the bundled TURN relay work when the relay and media server share one public address. An IPv6 loopback rule no longer blocks IPv4 peers.
- **Self-hosting:** The offline ingress verifier refuses ambiguous NAT address and source-port pools instead of reading them as fixed translations ([#4180](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4180)).
- **Operations:** A production deploy keeps running on the server when its SSH tunnel drops mid-rollout, and the deploy reconnects to that run instead of failing ([#4178](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4178)).
- **Voice:** Signing out or switching accounts while a DM call is starting no longer lets that call start for the next account.

### Security

- **Voice:** One account can no longer fill the voice server's connection capacity with idle connections and lock other users out of voice.
- **Accounts:** A device signed out while its account-deletion request was still processing can no longer delete the account ([#4192](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4192)).
- **Accounts:** Signing out a device or revoking a session cuts it off everywhere at once. Its open connection and any voice call end before the change is confirmed, and a signed-out device can no longer act on your account.
- **Attestation:** iOS App Attest binds each attested key to the account that attested it, ties the attestation session header to the signed-in session, and caps the verify request size (ADR-0010 D17).
- **Attestation:** The server no longer accepts an attestation record that is not tied to an account, so such a record cannot stand in for another user ([#4211](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4211)).
- **Dependencies:** Updated golang.org/x/net and fast-uri for security fixes ([#4181](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4181)).
- **Desktop:** Updated the URI parser security floor to address upstream normalization advisories.
- **Accounts:** Sign-in protection accepts a key re-encryption only from the maintenance run that started it. It refuses to roll back while its state is unknown ([#4137](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4137)).
- **Accounts:** An account with suspicious session use stays blocked until session revocation finishes.
- **Accounts:** Account recovery asks for the authenticator you reserved for recovery ([#3868](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3868)).
- **Accounts:** Trusted-device recovery requires matching fingerprints on both devices ([#3582](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3582)). Update both devices and start a new request.
- **Accounts:** Security-key confirmations go only to the server that asked ([#3690](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3690)).
- **Accounts:** Session refresh checks the device identifier ([#3865](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3865)). Concurrent sign-ins cannot reuse a recovery code ([#3860](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3860)).
- **Desktop:** The desktop rechecks its saved web app when the signature changes and refuses an invalid one.
- **Desktop:** Electron 44.9.1 blocks compromised eMudhra intermediate certificates ([#4071](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/4071), [#4074](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4074)).
- **Real time:** Control connections close when their access token expires ([#3895](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3895)).
- **Real time:** An account can keep up to 8 WebSocket connections and a process accepts up to 10,000 ([#3892](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3892)).
- **Real time:** Profile changes reach only people with a current relationship ([#3891](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3891)). Account deletion clears status only for its prior viewers ([#4055](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/4055)).
- **Messages:** DMs store participant messages only as encrypted messages ([#3864](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3864)).
- **Messages:** The server requires an explicit key version on every message. Upgrade all control-plane writers before you apply the migration ([#2844](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2844), [#3935](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3935)).
- **Messages:** Attachment deletion requires current access to the chat. Group DM icon uploads and reads require current admin role or membership ([#2625](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2625), [#3779](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3779)).
- **Messages:** Erased feedback screenshots no longer load from the image endpoint ([#3982](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3982)).
- **Plans:** Animated avatars, banners, and server icons follow the tier rules on every route ([#3856](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3856), [#3874](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3874)).
- **Plans:** Voice refuses microphone streams that do not declare packet time or bitrate ([#3858](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3858), [#3872](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3872)). Repeated over-limit requests are flagged and resync your plan limits ([#3877](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3877)).
- **Self-hosting:** Disabled feedback refuses submissions, and the installer rejects unsafe settings and ambiguous retained data. Release publication verifies proof and attestations.
- **Operations:** Redis health checks keep passwords out of Docker event commands. Existing deployments need the updated container configuration.
- **Operations:** CI fails on fixable high or critical vulnerabilities in server images ([#3987](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3987)).
- **Dependencies:** Go 1.27.3 ([GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809)), Go 1.26.9 and `golang.org/x/net` 0.60.0 ([#3623](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3623)), and Vite 8.3.3 ([GHSA-rq7h-c2jc-7f22](https://github.com/vitejs/vite/security/advisories/GHSA-rq7h-c2jc-7f22), [GHSA-vfpm-58rq-9qcg](https://github.com/vitejs/vite/security/advisories/GHSA-vfpm-58rq-9qcg), [GHSA-9jrq-w75r-8gcw](https://github.com/vitejs/vite/security/advisories/GHSA-9jrq-w75r-8gcw)).
- **Dependencies:** `source-map-js` 1.2.2 ([CVE-2026-93749](https://github.com/advisories/GHSA-68fv-2mgg-jv7q)), `shell-quote` 1.12.0 ([CVE-2026-102422](https://github.com/advisories/GHSA-pqg4-j6r4-53mv)), media-plane forwarded-address parser ([#3620](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3620)), and `sprintf-js` removed ([#3776](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3776)).

## [0.2.48] - 2026-10-04

Screen sharing stays steady when a capture changes, and voice pauses a stream
that sends far more than your plan allows. Security prompts ask for the right
proof in the right places, never a code your account cannot provide and never
your password twice.

**88 Total Changes** | 2 Added | 16 Changed | 57 Fixed | 13 Security

### Added

- **Bug reports:** The bug-report form accepts up to four screenshots (PNG, JPEG, or WebP, 5 MB each). Re-encoding removes camera metadata such as GPS location ([#1747](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1747)).
- **Messages:** Deleting more than 15 messages in 30 seconds, or 100 in 24 hours, asks you to confirm it is you. API: optional `{"mfa_code", "step_up_token"}` body and new `POST /api/v1/auth/step-up/password` (migration 000162) ([#3455](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3455), [#3509](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3509)).

### Changed

- **Voice:** The voice server pauses a microphone, camera, or screen share that keeps sending far above your plan. A second time within an hour removes you from the call for 15 minutes ([#3413](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3413), [#2153](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2153)).
- **Voice:** Screen sharing with sound sends at your plan's audio quality (96 kbps on Free) ([#3413](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3413)).
- **Messages:** API clients can hide a DM or group from one participant's list, or clear that participant's history before a server-stamped cutoff ([#3306](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3306)).
- **Messages:** DMs offer separate Hide, Clear history for me, and Leave group controls ([#3434](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3434)).
- **Messages:** Purge Messages uses one range menu and says first that a DM purge removes only your own messages ([#3468](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3468)).
- **Messages:** The Hide confirmation no longer uses warning styling, and Leave group has its own section in Group Info ([#3468](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3468)).
- **Servers:** Owners and Administrators can require MFA for dangerous actions through `PUT /api/v1/servers/{id}/mfa-enforcement`. Email and text codes do not count ([#3464](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3464), [#3453](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3453)).
- **Appearance:** Font Size moved to Appearance ▸ Application Font and stays usable with Dyslexic Support on ([#3401](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3401), [#2367](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2367)).
- **Accessibility:** UI Scale runs from 50% to 200% and zooms the whole interface. Above 100%, the window width limits the zoom ([#3414](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3414), [#2367](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2367)).
- **Appearance:** Application Font has two modes. One Font applies your pick everywhere, and Font by Area sets fonts for Messages, Headings, Navigation, and Interface ([#2366](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2366), [#3429](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3429)).
- **Appearance:** Application Font highlights your mode. Theme Default and Concord Voice Default each have a description ([#3457](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3457)).
- **Security:** After you add an authenticator app, Concord says whether it created, kept, or failed to create your recovery key ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Server:** DM-block and credential-epoch cleanup records retryable voice ejections and fences stale callbacks ([#3140](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3140)).
- **Server:** DM writes recheck current membership before they store keys or uploads ([#3141](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3141)).
- **Server:** Server changes serialize authority and membership at their write boundary, so stale operations fail closed ([#3142](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3142)).
- **Voice:** Voice joins verify admission before they open a session, so an old disconnect cannot remove a newer join ([#3143](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3143)).

### Fixed

- **Bug reports:** Screenshots removed while logs are collected are no longer sent with the report.
- **Messages:** Messages and attachments always carry the version of the key that encrypted them ([#3565](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3565)).
- **Messages:** A purge target names the same author at admission and deletion. A malformed `target_user_id` returns 400 ([#3553](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3553)).
- **Messages:** A hidden conversation comes back as soon as someone messages it ([#3468](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3468)).
- **Messages:** Group messages from before someone left are readable again ([#3472](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3472)).
- **Messages:** Purging a DM stops after 10 seconds, and removed messages disappear for others at once ([#3442](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3442)).
- **Messages:** Empty conversations that everyone has hidden are cleaned up, and the last group member can leave ([#3402](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3402), [#2821](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2821)).
- **Messages:** Return to Latest stays at the latest message while older history loads.
- **Messages:** A saved chat position waits for the history page that holds it.
- **Self-hosting:** Self-hosted servers keep Google and Apple sign-in unavailable. Password sign-in still works ([#1619](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1619), [#3546](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3546)).
- **Sign-in:** Remember Me keeps your saved sign-in when the keychain is briefly unavailable ([#3560](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3560)).
- **Sign-in:** Two-factor sign-in no longer hangs when your encryption keys cannot be unlocked ([#3466](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3466), [#3453](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3453)).
- **Security:** Email MFA can be turned on again, and SMS shows as coming soon ([#3519](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3519)).
- **Security:** Regenerating backup codes from Settings works again ([#3466](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3466), [#3453](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3453)).
- **Security:** Try again on the recovery-key screen counts each attempt ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Security:** Reset TOTP accepts a typed backup code ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Security:** Turning on two-factor authentication no longer asks for your code twice ([#3437](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3437)).
- **Security:** A damaged two-factor record no longer crashes sign-in checks or the `mfa-rekey` tool ([#3420](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3420)).
- **Security:** Accounts with only email or text codes are not asked for a code they cannot check ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Security:** Removing your last authenticator or security key while email or text codes are on is refused, with a reason ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Security:** Purging messages no longer asks for your password twice ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Security:** A wrong password or code marks the field that was wrong, and screen readers announce it ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Security:** Retrying a lost recovery-key save resends the same key ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Security:** A temporarily unavailable verification check says so instead of "too many attempts" ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Security:** Account-security actions report a failure instead of claiming success ([#3461](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3461)).
- **Security:** The "Verify Your Identity" prompt opens on top of Settings and takes the keyboard ([#3423](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3423)).
- **Servers:** Requests to a server that is deleted mid-request answer as for a non-member, not with a server error ([#3508](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3508)).
- **Servers:** Channel and category permission overrides save again. API: permission bitfields now cross JSON as decimal strings (breaking change) ([#3473](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3473), [#3406](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3406)).
- **Servers:** Server owners can mention @all and @here again ([#3464](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3464), [#3453](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3453)).
- **Servers:** Text-only channel changes work on busy voice servers ([#3480](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3480)).
- **Invites:** Inviting someone to a server they are already in is greyed out ([#2372](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2372)).
- **Invites:** The invite button in a DM lists every server you can invite to ([#2372](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2372)).
- **Invites:** An invite to a server you are already in says "Joined" ([#2372](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2372)).
- **Voice:** A failed voice join no longer removes someone who joined the next call ([#3479](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3479), [#3480](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3480)).
- **Voice:** Voice access cleanup no longer sends an in-flight update to someone whose access was revoked ([#3480](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3480)).
- **Voice:** Voice activity clears before a privacy change reconnects the app ([#3412](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3412)).
- **Voice:** Moving between voice channels on different servers no longer loses the old-channel leave ([#3415](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3415)).
- **Voice:** Voice channel lists recover after stale sessions without wrong counts or repeated leave sounds ([#3304](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3304), [#3298](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3298), [#3370](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3370)).
- **Presence:** Rich Presence sharing settings save every time ([#3444](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3444)).
- **Screen share:** Free accounts start screen sharing at 1080p, not a Premium resolution ([#3436](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3436)).
- **Screen share:** Sharing an app with sound says when the sound could not be captured ([#3405](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3405)).
- **Screen share:** App audio captures helper processes and says when the sound stops ([#3445](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3445), [#3394](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3394)).
- **Screen share:** Switching share sources no longer drops or leaks per-app audio ([#3369](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3369)).
- **Desktop:** A malformed server response no longer replaces the desktop's working configuration ([#3481](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3481), [#2240](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2240)).
- **Desktop:** Update, reconnection, and what's-new windows work while Settings is open ([#3424](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3424)).
- **Desktop:** Links opened from a picture-in-picture window open in your browser ([#3416](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3416)).
- **Desktop:** Right-click menus no longer open partly off-screen ([#3414](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3414), [#2367](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2367)).
- **Appearance:** Brand-colored buttons and avatars meet 4.5:1 contrast in every color scheme ([#3514](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3514)).
- **Appearance:** Premium labels and Admin badges are readable in every color scheme ([#3439](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3439)).
- **Appearance:** The sign-in logo is readable in light themes.
- **Appearance:** Dyslexic Support changes every piece of text ([#2366](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2366)).
- **Appearance:** Your app font reaches text boxes, buttons, and menus ([#2366](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2366)).
- **Accessibility:** Error text in code and security-key prompts is easier to read ([#3455](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3455), [#3509](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3509)).
- **Accessibility:** The Basic / Advanced switch in Audio and Video Configuration works with a keyboard and screen reader ([#3428](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3428)).
- **Accessibility:** The audio quality labels work as buttons for keyboard and screen-reader users ([#3430](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3430)).
- **Accessibility:** Keyboard focus rings are visible in every theme and no longer look like a selection ([#3438](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3438), [#798](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/798)).
- **Operations:** A dirty database migration stops control-plane startup until an operator recovers it ([#3138](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3138)).

### Security

- **Accounts:** A backup code no longer signs you in past an authenticator app set to recovery only ([#3563](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3563)).
- **Accounts:** An authenticator-app code works only once ([#3466](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3466), [#3453](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3453)).
- **Accounts:** A security-key confirmation counts only for the action you confirmed ([#3467](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3467), [#3453](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3453)).
- **Accounts:** Weakening your account's recovery asks you to prove it is you ([#3433](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3433)).
- **Accounts:** Two-factor sign-in keeps its attempt limits, and your code, through a server fault ([#3460](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3460)).
- **Accounts:** Account safety checks and moderation agree on one spelling of your account's identifier ([#3362](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3362)).
- **Messages:** Once every member has cleared a DM or group's history, the server deletes those messages and attachments ([#3462](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3462)).
- **Presence:** A dropped voice connection no longer leaves your activity visible to people you later hide it from ([#3446](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3446)).
- **Operations:** The control plane treats an unset `ENVIRONMENT` as production, and refuses `CONCORD_ENV=test` outside development or test ([#3519](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3519)).
- **Operations:** Identity-anomaly audit records keep the request's final authorization result ([#3369](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3369)).
- **Dependencies:** The voice server's connection library closes GHSA-2GC4-CQFQ-P2GV ([#3507](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3507)).
- **Dependencies:** Electron 44.4.3 carries current Chromium, V8, and PDFium security fixes ([#3132](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3132)).
- **Dependencies:** macOS installer builds no longer pull the legacy DMG dependency chain ([#2727](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2727), [#3396](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3396)).

---

## [0.2.47] - 2026-09-18

Screen sharing gets tabs, a sound switch, per-app audio, and in-place source
switching. Shared chats can set message timers, and Rich Presence shows what
is shared and who may receive it.

**62 Total Changes** | 13 Added | 5 Changed | 37 Fixed | 7 Security

### Added

- **Messages:** Changing the message timer leaves a note in the conversation that says who changed it and to what ([#1351](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1351)).
- **Messages:** The server stores message expiry policy for channels, DMs, and group DMs, and applies it in resumable batches ([#3144](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3144), [#2195](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2195)).
- **Messages:** Eligible chats can set message expiration to Off, 1 hour, 24 hours, 7 days, or 30 days, for new or existing messages ([#2197](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2197)).
- **Messages:** Opening a busy channel or DM drops you at your first unread message, and messages you watch arrive count as read ([#3289](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3289), [#3329](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3329), [#2006](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2006)).
- **Presence:** Rich Presence activity shows in member rows and profiles, and your panel shows its eligible audience ([#2235](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2235)).
- **Screen share:** The share picker has Screens and Windows tabs with thumbnail grids ([#3145](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3145), [#3204](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3204)).
- **Screen share:** Screen sharing carries your computer's sound by default, with a switch to turn it off. Linux does not support it yet ([#3145](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3145), [#3198](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3198)).
- **Screen share:** Sharing one window can send only that app's sound, where the operating system supports it ([#3198](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3198)).
- **Screen share:** A Switch button changes what you share without dropping viewers ([#3145](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3145)).
- **Voice:** Each person's tile has a one-click mute that only you hear ([#3204](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3204)).
- **Voice:** Voice participants show their profile photo in the channel sidebar ([#2360](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2360)).
- **GIFs:** GIFs stop playing when you switch away from Concord, including animated WebP and PNG ([#3291](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3291), [#2369](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2369)).
- **GIFs:** Settings ▸ Accessibility ▸ Display has a GIF Playback control: Auto, Always, or Hover only ([#3291](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3291), [#2369](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2369)).

### Changed

- **Messages:** Message expiration and purging are separate controls. The timer shows next to the encryption notice, and Purge has its own header button ([#1351](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1351)).
- **Messages:** Expired messages leave chats and pinned views together, in five-minute sweeps ([#2196](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2196)).
- **Voice:** Call buttons are grouped by what they do, and only Leave is red ([#3204](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3204)).
- **Voice:** Picture-in-Picture and the stay-active setting moved into a More menu ([#3204](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3204)).
- **Voice:** Busy servers give each new voice room to the least busy CPU core ([#3157](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3157), [#3149](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3149)).

### Fixed

- **Voice:** Per-person volume, master output volume, quiet-user boost, and output device choice work again ([#3351](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3351)).
- **Voice:** The chat button is back in DM calls ([#3351](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3351)).
- **Voice:** Right-clicking someone in a DM call opens their menu ([#3351](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3351)).
- **Voice:** Voice no longer drops people who are still connected when a media server's clock runs ahead ([#3187](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3187)).
- **Voice:** A media server with a badly wrong clock can no longer freeze the rooms it serves ([#3205](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3205)).
- **Video:** Turning your camera on in a call works again ([#3348](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3348)).
- **Video:** Video drops to a smaller size before it freezes when your computer is struggling ([#3268](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3268), [#3279](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3279), [#3094](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3094)).
- **Video:** Joining a call late no longer leaves your camera stuck at one quality ([#3275](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3275)).
- **Video:** The controls on a video can be reached with a keyboard ([#3204](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3204)).
- **Screen share:** Screen sharing respects your quality setting again ([#3348](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3348)).
- **Screen share:** Changing screen-share quality mid-share reaches everyone watching ([#3260](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3260), [#2208](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2208)).
- **Screen share:** Screen sharing on macOS 14.2 and later carries sound. macOS 12 and earlier share picture only ([#3145](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3145)).
- **Screen share:** The share window keeps keyboard focus until you choose or close it ([#3204](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3204)).
- **Screen share:** The screen-share volume slider shows only when you point at the video ([#3351](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3351)).
- **Messages:** Rotating a DM's encryption key no longer locks everyone out of the conversation ([#3343](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3343)).
- **Messages:** A DM with only a photo or file says what it is in your DM list, not "Encrypted message" ([#2364](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2364)).
- **Messages:** Chats come back to where you left them, and Return to Latest stays at the bottom ([#2006](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2006), [#3282](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3282)).
- **Notifications:** The unread count on the app icon clears when you read the messages ([#2403](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2403)).
- **Notifications:** The app icon stops bouncing after a notification ([#2403](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2403)).
- **Presence:** If you drop offline, you come back on your own ([#3332](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3332)).
- **Presence:** You no longer drop offline or lose your connection while Concord sits in the background ([#3328](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3328)).
- **Presence:** Going invisible no longer knocks everyone else offline ([#3238](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3238)).
- **Presence:** "Eligible audience" shows only when you point at your user area ([#3351](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3351)).
- **Sign-in:** SSO sessions survive refresh when optional session metadata is missing ([#3290](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3290)).
- **GIFs:** The GIF and emoji pickers sit against their buttons with an arrow, and are about 15% larger ([#3278](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3278), [#2370](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2370)).
- **GIFs:** Saving a GIF no longer sends you back to the top ([#3266](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3266), [#2370](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2370)).
- **Appearance:** Secondary text throughout the app is readable, including in custom themes ([#3341](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3341)).
- **Appearance:** Midnight Sky's secondary text is readable on raised panels ([#3335](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3335)).
- **Appearance:** The DM voice-call record is readable in every theme ([#3334](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3334)).
- **Appearance:** 23 fixed colors now follow your theme in light themes ([#3270](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3270)).
- **Appearance:** Accent-colored buttons keep a readable label, High Contrast included ([#3285](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3285)).
- **Appearance:** Text on red and green buttons is legible in every theme ([#3285](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3285)).
- **Appearance:** Server-enforced mutes show amber again, and missing hover highlights are back ([#3270](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3270)).
- **Appearance:** The documents in Settings ▸ About have their dividers and boxes ([#3270](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3270)).
- **Appearance:** Call buttons and badges follow your theme ([#3204](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3204)).
- **Desktop:** The DM profile window opens in the middle of the screen ([#3351](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3351)).
- **Desktop:** The installed app is about 37 MB smaller and no longer ships its source code or an internal test config ([#3156](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3156)).

### Security

- **Messages:** Unread counts no longer leak from channels you can see but cannot read ([#2403](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2403)).
- **Desktop:** Screen-share audio loads its native helper directly and refuses a substituted JavaScript module ([#3300](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3300)).
- **Voice:** Voice rooms no longer stay full after everyone has left ([#3116](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3116)).
- **Operations:** The control plane and media plane emit privacy-safe security events for Nightwatch ingestion ([#2273](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2273)).
- **Sign-in:** Release-attestation tokens accept only the one signature algorithm GitHub uses ([#3207](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3207)).
- **Sign-in:** Google and Apple sign-in accept only the exact signature type those providers use ([#3207](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3207)).
- **Dependencies:** Desktop and Admin resolve patched Undici releases, and Worker deployments use the audited lockfile's Wrangler ([#3305](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3305), [#3133](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3133)).

---

## [0.2.46] - 2026-09-07

Voice connects on networks that block UDP, and Picture-in-Picture voice windows
get a private line and a one-time credential. Server updates no longer cut your
connection short, and a broken update can no longer pass as a success.

**9 Total Changes** | 7 Fixed | 2 Security

### Fixed

- **Voice:** Voice works on networks that block UDP, through the TCP and TLS relays the server offers ([#3104](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3104)).
- **Voice:** Calls no longer spend setup time on a TCP route that no server accepts. Operators can still enable it ([#3105](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3105)).
- **Voice:** Closing Picture-in-Picture voice during startup disconnects it cleanly ([#3128](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3128)).
- **Presence:** Your voice and call status no longer disappears every hour ([#3102](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3102)).
- **Server:** Server updates no longer cut your connection short ([#3106](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3106)).
- **Operations:** An update that cannot reach its database fails its health check instead of passing ([#3106](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3106)).
- **Messages:** Deleting a message retires its unshared attachment ([#3100](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3100)).

### Security

- **Voice:** Picture-in-Picture voice windows get their own one-time credential and private channel ([#3104](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/3104)).
- **Messages:** Group deletion rechecks the administrator under lock ([#3100](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3100)).

## [0.2.45] - 2026-09-02

Voice channels and private calls can be joined again. The desktop runtime moves
to Electron 43.5.1, and sign-in rejects extension data that Concord did not
request.

**8 Total Changes** | 5 Changed | 1 Fixed | 2 Security

### Changed

- **Desktop:** Electron 43.5.1 brings upstream Chromium, Node.js, and V8 fixes ([#3087](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3087)).
- **Messages:** Large encrypted attachments can use consistently sized upload pieces on compatible clients ([#3086](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3086)).
- **Video:** Video forwarding keeps temporal quality at or below the requested layer ([#3055](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3055)).
- **Dependencies:** `lucide-react` and the media-plane `qs` dependency moved to reviewed releases ([#3073](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3073), [#3098](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3098)).
- **Dependencies:** Development and CI tooling was refreshed without runtime changes ([#3099](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3099), [#3097](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3097), [#3096](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3096), [#3065](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3065), [#3074](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3074), [#3071](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3071), [#3070](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3070), [#3085](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3085)).

### Fixed

- **Voice:** Voice channels and calls can be joined again. The minimum-version check no longer applies to internal service calls ([#3088](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3088)).

### Security

- **Sign-in:** WebAuthn sign-in and enrollment reject client extension output that Concord did not request ([#3090](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3090)).
- **Dependencies:** The control plane drops the advisory-affected MongoDB driver release and updates `quic-go` and `sonic` ([#3091](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3091)).

## [0.2.44] - 2026-09-01

You can choose who sees your voice-channel and private-call activity. The
attachment stack is ready for the hosted storage cutover, and deletions keep
the media and activity cleanup they trigger.

**17 Total Changes** | 2 Added | 8 Changed | 7 Fixed

### Added

- **Presence:** Settings ▸ Rich Presence has audience and detail controls for server voice and private calls ([#3042](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3042)).
- **Server:** The server can require a minimum desktop version before attachment format changes ([#3053](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3053)).

### Changed

- **Presence:** The desktop keeps voice and call activity current internally ([#3038](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3038)).
- **Messages:** Encrypted attachment uploads negotiate their storage format with the server ([#3044](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3044)).
- **Operations:** Production validates the R2 attachment destination before cutover ([#3050](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3050)).
- **Desktop:** Desktop utilities are organized by concern, with no behavior change ([#3045](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3045)).
- **Dependencies:** `jose`, `lucide-react`, and the build-only `xmldom` resolution moved to reviewed releases ([#3069](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3069), [#3062](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3062), [#3078](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3078)).
- **Dependencies:** Admin, desktop, and media-plane development tooling was refreshed ([#3068](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3068), [#3063](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3063), [#3064](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3064), [#3066](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3066), [#3067](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3067), [#3059](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3059), [#3056](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3056), [#3060](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3060)).
- **Dependencies:** Browser compatibility metadata no longer resolves to advisory-affected releases ([#3080](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3080)).
- **Operations:** Weekly dependency triage fails closed on missing PR CI ([#3081](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3081)).

### Fixed

- **Presence:** Deleting a server no longer leaves its voice activity visible after a restart ([#3054](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3054)).
- **Presence:** Removing someone from a private conversation, or deleting its creator, no longer leaves stale voice activity ([#3052](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3052)).
- **Messages:** Removing someone from a private conversation no longer deadlocks with a concurrent account deletion ([#3079](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3079)).
- **Messages:** Encrypted attachments stay on the compatible storage format during rollouts ([#3051](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3051)).
- **Messages:** Profile-media cleanup survives storage failures ([#3043](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3043)).
- **Messages:** Large encrypted attachments are prepared for consistently sized upload pieces ([#3039](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3039)).
- **Invites:** An invite preview no longer shows details from a different invite while it loads ([#3037](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3037)).

## [0.2.43] - 2026-08-30

The app can reach the servers again after a certificate change locked every
installed copy out. Deleting your account removes the files it uploaded.

**5 Total Changes** | 1 Changed | 1 Fixed | 3 Security

### Changed

- **Desktop:** Desktop state is grouped by what it controls, with no behavior change ([#3033](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3033)).

### Fixed

- **Desktop:** The app checks server certificates the way a browser does, so a provider certificate change no longer locks everyone out.

### Security

- **Desktop:** Switching between the app and the web version no longer leaves one unable to load your servers ([#3032](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3032)).
- **Accounts:** Deleting your account removes the files you uploaded ([#3019](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3019)).
- **Servers:** Changing a server's owner rechecks active voice access at once ([#2991](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2991)).

## [0.2.42] - 2026-08-29

Interface fixes reach you again without an app update. Invite links arrive,
open, and show the server, and the GIF picker's Categories and Recent tabs
work.

**9 Total Changes** | 7 Fixed | 2 Security

### Fixed

- **Desktop:** Interface fixes reach you again without an app update ([#3014](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3014)).
- **Invites:** "Open in Concord" on an invite link works ([#2363](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2363)).
- **Invites:** An invite that arrived while you were offline appears when you return ([#2363](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2363)).
- **Invites:** A server you join from a link appears in the sidebar at once ([#2363](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2363)).
- **GIFs:** The GIF picker's Categories and Recent tabs show content ([#2976](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2976)).
- **GIFs:** The GIF picker keeps its size when a tab is empty ([#2976](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2976)).
- **Chat:** A burst of reconnections no longer holds up messages ([#2975](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2975)).

### Security

- **Presence:** Someone who has just lost access can no longer be shown that you are online ([#3010](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3010)).
- **Desktop:** Screen-capture protection reports itself as off where it cannot be enforced ([#2990](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2990)).

## [0.2.41] - 2026-08-27

Attachments can be as large as your plan says, you can choose who may send you
a friend request, and server roles can be reordered. Encrypted voice and video
are no longer garbled on recent engine builds.

**12 Total Changes** | 3 Added | 1 Changed | 6 Fixed | 2 Security

### Added

- **Messages:** Attachments up to 256 MiB upload in encrypted pieces ([#2157](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2157)).
- **Friends:** Settings ▸ Privacy sets who may send you a friend request ([#2888](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2888), [#2911](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2911)).
- **Servers:** Server roles can be reordered by drag, keyboard, or toolbar, with Apply Order ([#2359](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2359)).

### Changed

- **Desktop:** Concord trusts a list of interface-signing keys, so a key change does not disable the cache ([#2958](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2958)).

### Fixed

- **Voice:** Voice and video are no longer garbled on recent engine builds ([#2866](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2866)).
- **Voice:** Picture-in-picture windows decrypt what they show ([#2870](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2870)).
- **Chat:** The connection no longer drops every few minutes ([#2873](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2873)).
- **Voice:** A voice server queue overflow no longer disconnects everyone on that machine ([#2868](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2868)).
- **Messages:** Mentions in DMs show names again ([#2368](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2368)).
- **Presence:** A restart no longer leaves your activity showing after you leave ([#2910](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2910)).

### Security

- **Friends:** A storage fault no longer reveals which friend codes exist ([#2860](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2860)).
- **Desktop:** An opt-in setting on macOS and Windows keeps Concord windows out of screen captures ([#2977](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2977)).

---

## [0.2.40] - 2026-08-20

Friend codes have a web page of their own. It shows who the code belongs to and
offers to open Concord, and it looks the same whether the code is live or not.

**8 Total Changes** | 1 Added | 1 Changed | 5 Fixed | 1 Security

### Added

- **Friends:** Friend code links open a page with the owner's name and avatar, and a button that opens Concord. Dead codes show a neutral page of the same shape ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)).

### Changed

- **Presence:** A friend or privacy change can delay a Custom Status save for up to 30 seconds, with a `Retry-After` header ([#2823](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2823)).

### Fixed

- **Friends:** Several friend or invite links clicked quickly all open, in order ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)).
- **Friends:** A brief server problem no longer reports a working friend code as dead ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)).
- **Friends:** Revoking a friend code says when it fails ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)).
- **Presence:** Leaving, or being kicked or banned from, a server stops your activity showing there at once ([#2447](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2447)).
- **Presence:** Deleting a server or your account no longer leaves your activity visible to others ([#2447](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2447)).

### Security

- **Friends:** A friend link clicked before sign-in no longer follows you into someone else's account ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)).

## [0.2.39] - 2026-07-31

A self-hosted server address is checked when the app connects, not only when
you type it. A server on your own network needs your approval in a dialog that
the web layer cannot draw or click.

**1 Total Changes** | 1 Security

### Security

- **Self-hosting:** A compromised web layer can no longer make the app read your private network ([#2668](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2668)).

## [0.2.38] - 2026-07-30

Encrypted channel keys reach every member of a busy server without stalling or
handing out a revoked key. The DM and server sidebars share one adaptive layout.

**14 Total Changes** | 2 Changed | 3 Fixed | 9 Security

### Changed

- **Desktop:** The DM and server sidebars share one adaptive layout ([#2510](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2510)).
- **Dependencies:** mediasoup 3.22.0 and the current React release ([#2513](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2513), [#2515](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2515), [#2517](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2517)).

### Fixed

- **Encryption:** Channel keys are delivered in batches ([#2503](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2503)).
- **Encryption:** Key distribution can be cancelled while it waits to retry ([#2541](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2541)).
- **Encryption:** Key recovery resumes after a repeated rotation ([#2539](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2539)).

### Security

- **Sign-in:** Backing out of a social sign-in no longer costs a later password sign-up its encryption keys ([#2655](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2655)).
- **Sign-in:** Two-step verification codes are compared in constant time ([#2654](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2654)).
- **Encryption:** The server refuses key distribution into a revoked epoch ([#2534](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2534)).
- **Encryption:** Manual channel rotation is fenced to the current credentials ([#2540](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2540)).
- **Encryption:** A channel's active key epoch survives concurrent writes ([#2542](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2542)).
- **Sign-in:** Signing out other sessions keeps the one you are using ([#2501](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2501)).
- **Presence:** Rich Presence suppression fails closed ([#2522](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2522)).
- **Presence:** Returning from invisible does not replay a stale suppression ([#2502](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2502)).
- **Sign-in:** Static-analysis findings in authentication code are resolved ([#2525](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2525)).

## [0.2.37] - 2026-07-27

A stolen or superseded token can no longer outlive the reset meant to end it,
and a disabled account is disconnected at once. Recovery keys use P-384.

**8 Total Changes** | 3 Fixed | 5 Security

### Fixed

- **Desktop:** Electron 43.1.1 fixes crashes when the app archive is replaced while Concord runs ([#2492](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2492)).
- **Encryption:** The app bounds how many key requests it sends at once ([#2482](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2482)).
- **Messages:** Bulk message removal records its progress, so an interrupted purge can finish ([#2483](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2483)).

### Security

- **Sign-in:** Linking a social account is bound to the password you just verified ([#2458](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2458)).
- **Sign-in:** Token-theft revocation and session creation cannot interleave ([#2460](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2460), [#2457](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2457)).
- **Sign-in:** A disabled account is disconnected from an open connection ([#2480](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2480)).
- **Encryption:** Recovery keys use P-384, and weaker curves are rejected ([#2355](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2355)).
- **Presence:** Going invisible or offline clears activity that was already published ([#2461](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2461)).

## [0.2.36] - 2026-07-26

Desktop actions that touch your camera, clipboard, updates, or permissions
verify that the request came from Concord Voice itself.

**1 Total Changes** | 1 Security

### Security

- **Desktop:** Screen capture, clipboard, update, permission, lifecycle, and developer controls act only for a permitted Concord frame ([#2351](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2351)).

## [0.2.35] - 2026-07-25

Windows updates install when you click Restart.

**1 Total Changes** | 1 Fixed

### Fixed

- **Desktop:** Windows updates install from the Restart button, and leftover installer files are cleaned up later ([#2402](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2402)).

## [0.2.34] - 2026-07-25

This release carries everything from 0.2.33, which was never published. Google
and Apple sign-in are bound to the current account state, and the "can't reach
Concord servers" warning clears itself.

**4 Total Changes** | 1 Fixed | 3 Security

### Fixed

- **Desktop:** The "can't reach Concord servers" warning clears itself once the app connects ([#2401](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2401)).

### Security

- **Sign-in:** Social sign-in is bound to your current account state ([#2453](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2453)).
- **Dependencies:** The desktop router update closes GHSA-QWWW-VCR4-C8H2 ([#2442](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2442)).
- **Encryption:** Key self-healing checks the recipient's current key version ([#2440](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2440)).

## [0.2.33] - 2026-07-24

This version was never published, and its contents shipped in 0.2.34.
Sessions, encryption keys, and password changes are tied to the exact
credentials that authorized them.

**14 Total Changes** | 6 Fixed | 8 Security

### Fixed

- **Encryption:** Key recovery stays reachable after a password sign-in ([#2435](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2435)).
- **Presence:** Custom Status visibility settles consistently ([#2419](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2419)).
- **Presence:** Rich Presence cleanup resumes after an interruption ([#2408](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2408)).
- **Voice:** Private calls appear only once both sides are admitted ([#2407](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2407)).
- **Presence:** Hidden presence stays hidden across servers ([#2404](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2404), [#2405](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2405), [#2232](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2232)).
- **Sign-in:** Sign-in and voice lifecycle code is simpler, with no behavior change ([#2421](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2421)).

### Security

- **Sign-in:** Two-step verification during a social sign-in completes inside the protected process ([#2436](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2436)).
- **Encryption:** Encryption keys are committed by the sign-in that created them ([#2434](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2434)).
- **Sign-in:** A password change that succeeded is never reported as a failure ([#2431](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2431)).
- **Encryption:** Key delivery checks the recipient against a concurrent key reset ([#2430](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2430)).
- **Sign-in:** A slow rejected request cannot end a rotated session ([#2432](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2432)).
- **Sign-in:** Grace-period session refresh follows an exact token lineage ([#2428](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2428)).
- **Sign-in:** Every session is bound to the credentials that authorized it ([#2427](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2427), [#2397](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2397)).
- **Encryption:** A password change re-encrypts every encrypted setting in one transaction ([#2395](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2395)).

---

## [0.2.32] - 2026-07-22

Sign-in and session refresh stay with the account and server you chose, even
when two sign-ins overlap or you switch servers.

**2 Total Changes** | 1 Changed | 1 Fixed

### Changed

- **Dependencies:** mediasoup, Vite, the icon set, and the media server's request parser moved to current releases ([#2379](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2379), [#2384](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2384), [#2380](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2380), [#2389](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2389)).

### Fixed

- **Sign-in:** Sign-in and refresh stay with the right account and server, and a superseded sign-in clears its keys from memory ([#2374](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2374)).

## [0.2.31] - 2026-07-22

Groundwork for showing what your friends are doing. The server can publish
voice and call activity, and the desktop validates it without showing it yet.

**1 Total Changes** | 1 Changed

### Changed

- **Presence:** The server can publish authorized voice and call activity, and the desktop validates the new format ([#2391](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2391)).

## [0.2.30] - 2026-07-18

Reconnecting after a network drop restores what changed while you were away.
Moderators can remove a member's messages when they kick or ban them, and Apple
sign-in works on desktop.

**8 Total Changes** | 3 Added | 2 Fixed | 3 Security

### Added

- **Servers:** Kicking or banning a member can also purge their messages in that server ([#2342](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2342)).
- **Operations:** Operators see aggregate account activity in an Admin workspace, as counts only ([#2338](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2338)).
- **Operations:** Operators can rotate the two-step verification encryption key safely ([#2345](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2345)).

### Fixed

- **Chat:** Reconnecting refreshes your memberships and back-fills the open conversation, and keeps drafts and queued messages ([#2327](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2327), [#2358](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2358)).
- **Sign-in:** Apple sign-in completes on desktop through the hosted bridge ([#2323](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2323)).

### Security

- **Encryption:** A password change that loses your encryption keys signs you out ([#2357](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2357)).
- **Encryption:** A sign-in that has ended cannot clear the keys of the one that replaced it ([#2337](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2337)).
- **Server:** Security headers are sent once, from one place ([#2328](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2328)).

## [0.2.29] - 2026-07-15

Moderators can remove a member's messages in bulk. The codec in Settings is the
one in use, and H.264 calls are encrypted per access unit. Operators get an
Admin console.

**8 Total Changes** | 3 Added | 3 Fixed | 2 Security

### Added

- **Servers:** Moderators can delete a member's messages across a channel, server, or conversation. The audit record keeps counts only ([#2290](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2290)).
- **Operations:** The Admin Portal console shows instance health and aggregate metrics ([#2268](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2268)).
- **Presence:** Rich Presence category settings persist across restarts ([#2275](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2275)).

### Fixed

- **Video:** The codec shown in Settings is the one in use ([#2242](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2242)).
- **Video:** H.264 video is encrypted per access unit, and HDR target selection is corrected ([#2298](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2298)).
- **Operations:** Admin metrics stay in bounds, are labelled, and include host telemetry ([#2286](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2286), [#2287](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2287), [#2281](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2281)).

### Security

- **Operations:** Desktop release signing is isolated from pull requests ([#2296](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2296)).
- **Presence:** The server authorizes voice activity before it publishes it ([#2282](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2282)).

## [0.2.28] - 2026-07-14

DM call previews say what happened, and Custom Status counts characters the way
you would.

**3 Total Changes** | 1 Changed | 2 Fixed

### Changed

- **Dependencies:** Desktop build and test tooling was refreshed ([#2265](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2265), [#2264](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2264), [#2261](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2261)).

### Fixed

- **Messages:** A DM call entry describes its outcome ([#2245](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2245)).
- **Presence:** Custom Status counts characters, not bytes ([#2250](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2250)).

## [0.2.27] - 2026-07-14

Activity History arrives: an opt-in, self-only record of your own voice and
call time. A call that drops to software encoding re-selects its codec.

**7 Total Changes** | 2 Added | 4 Fixed | 1 Security

### Added

- **Privacy:** Activity History is an opt-in, self-only record of your voice and call intervals, pruned on your schedule ([#1235](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1235)).
- **Operations:** A read-only admin metrics API serves aggregate counts ([#2228](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2228), [#2218](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2218)).

### Fixed

- **Video:** Video re-selects a hardware-capable codec mid-call after a software fallback ([#2187](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2187)).
- **Chat:** A late layout change no longer scrolls you away from the newest message ([#2227](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2227)).
- **Messages:** Edited encrypted messages display correctly ([#2217](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2217)).
- **Desktop:** The outgoing-call and What's New dialogs no longer appear behind other windows ([#2225](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2225)).

### Security

- **Desktop:** The updater's parser no longer spends excessive CPU on crafted recovery-feed YAML ([#2247](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2247)).

## [0.2.26] - 2026-07-12

Everyone can share their screen at once, each stream has its own volume and
mute, and viewers get the resolution their window needs. Codec selection checks
the live call for hardware encoding.

**19 Total Changes** | 4 Added | 2 Changed | 8 Fixed | 5 Security

### Added

- **Screen share:** Everyone can share at once, and each viewer gets the quality that fits their window ([#2160](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2160), [#2185](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2185)).
- **Screen share:** Each shared screen has its own volume and mute ([#2169](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2169)).
- **Voice:** A voice tile view fills the available space ([#2177](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2177)).
- **Presence:** You can hide your Custom Status from specific people ([#2191](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2191)).

### Changed

- **Billing:** Subscriptions return to the free tier on schedule when a plan period ends ([#2168](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2168)).
- **Settings:** Settings ▸ Sounds and Notifications is now Notifications ([#2188](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2188)).

### Fixed

- **Video:** Hardware video encoding is confirmed from the live call ([#2184](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2184), [#2189](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2189)).
- **Voice:** Joining during a brief server blip retries instead of failing ([#2181](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2181)).
- **Voice:** Picture-in-picture windows close promptly when a call ends ([#2215](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2215)).
- **Screen share:** System audio is captured only for whole-screen shares ([#2165](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2165)).
- **Screen share:** Tuning into a second screen keeps the focused one ([#2156](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2156)).
- **Screen share:** Quality demand no longer flaps for AV1 and VP9 shares ([#2212](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2212)).
- **Desktop:** Interface and app update states show separately ([#2183](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2183)).
- **Notifications:** Follow-up fixes from the desktop notification work ([#2167](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2167)).

### Security

- **Voice:** The media server checks permission to speak or share ([#2140](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2140)).
- **Voice:** An empty server-supplied voice identity fails closed ([#2152](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2152)).
- **Video:** Channel video limits follow the server's plan, not the owner's ([#2175](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2175)).
- **Screen share:** Screen-share resolution and frame rate follow your plan as a combined pixel rate ([#2172](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2172)).
- **Server:** Shutdown drains in-flight requests before closing live connections ([#2203](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2203), [#2149](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2149), [#2214](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2214)).

## [0.2.25] - 2026-07-09

A full audit of channel and message permissions checks visibility on every path
that can reveal it. The composer adds emoji autocomplete and picker shortcuts.

**18 Total Changes** | 4 Added | 4 Fixed | 10 Security

### Added

- **Messages:** Type a `:shortcode:` for inline emoji suggestions ([#2081](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2081)).
- **Messages:** Ctrl+E opens emoji and Ctrl+G opens GIFs from the composer ([#2079](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2079)).
- **Screen share:** You choose which shared screens to watch ([#2126](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2126)).
- **Bug reports:** A preview shows the redacted diagnostic log before you send a report ([#2086](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2086), [#2082](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2082)).

### Fixed

- **Messages:** The composer no longer jumps when a picker first opens ([#2075](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2075), [#2085](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2085)).
- **Messages:** Emoji-only messages render large again ([#2073](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2073)).
- **Settings:** Privacy and Security settings no longer sign you out ([#2067](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2067)).
- **Accessibility:** Dialogs trap focus and announce themselves to screen readers ([#2089](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2089)).

### Security

- **Servers:** Channel metadata, unread counts, attachments, keys, broadcasts, and voice lists require permission to view the channel ([#2134](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2134), [#2136](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2136), [#2138](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2138), [#2132](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2132), [#2130](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2130)).
- **Servers:** Kicked or banned members are evicted from live connections ([#2133](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2133), [#2135](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2135)).
- **Privacy:** Group conversation creation respects your privacy settings ([#2127](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2127)).
- **Servers:** The database refuses to bind a channel to another server's category ([#2131](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2131)).
- **Sign-in:** SSO state and one-time tokens are consumed atomically, and attestation tokens are bound to their account ([#2129](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2129), [#2139](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2139)).
- **Voice:** Voice display names and avatars come from the server ([#2143](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2143)).
- **Operations:** Admin enrollment links keep their token out of the URL ([#2137](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2137)).
- **Server:** The server refuses a wildcard origin in production ([#2128](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2128)).
- **Dependencies:** The tar library update blocks malicious archives in build tooling ([#2142](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2142)).
- **Sign-in:** Running out of verification attempts returns the same shape as other failures ([#2083](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2083)).

## [0.2.24] - 2026-07-05

Username search, the friend-request sound, password-reset email, and the
verification-code window work again. Voice adds a stage that shows who is
talking.

**11 Total Changes** | 2 Added | 8 Fixed | 1 Security

### Added

- **Voice:** A voice stage puts the active speaker in front and marks the call as encrypted ([#2059](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2059), [#2062](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2062)).
- **Sign-in:** The sign-in screen has a show-password toggle ([#2061](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2061)).

### Fixed

- **Encryption:** The app notices when your keychain cannot save your keys for next launch ([#2068](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2068)).
- **Friends:** Searching for a user by name works again ([#2057](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2057)).
- **Friends:** Friend requests make a sound again ([#2054](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2054)).
- **Sign-in:** Password-reset email is delivered ([#2058](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2058)).
- **Sign-in:** The email verification code window is long enough to use ([#2055](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2055)).
- **Voice:** One-to-one calls show a single bottom bar ([#2056](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2056)).
- **Settings:** Settings and help overlays can be closed without the keyboard ([#2064](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2064)).
- **GIFs:** A GIF that fails to load no longer signs you out ([#2051](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2051)).

### Security

- **Server:** The server refuses test-mode configuration in production ([#2060](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2060)).

---

## [0.2.23] - 2026-07-04

You can see and manage your subscription in the app, and call quality, upload
limits, and server features follow your plan. When bandwidth is tight, Concord
drops webcam first, then screen share, so voice stays clear.

**6 Total Changes** | 4 Added | 1 Changed | 1 Fixed

### Added

- **Billing:** Settings ▸ Subscription shows your plan and status and redeems codes ([#2043](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2043)).
- **Billing:** Call quality, per-room camera and screen-share limits, and server features follow your plan. Webcam drops first, then screen share, before audio ([#2042](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2042), [#2039](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2039), [#2038](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2038)).
- **Billing:** Server-creation and search-depth limits scale with your subscription ([#2040](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2040)).
- **GIFs:** The GIF pipeline keeps animation and guards against oversized files ([#2041](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2041)).

### Changed

- **Operations:** Release workflows use tighter least privilege, and the build toolchain and dependencies were refreshed ([#2049](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2049), [#2046](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2046), [#2036](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2036), [#2035](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2035)).

### Fixed

- **Sign-in:** SSO sign-up gets a 15-minute token window and recovers if it expires ([#2047](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2047)).

## [0.2.22] - 2026-07-02

After you update, the next launch tells you what changed. This file became the
public record of every release, and CI refuses a version without an entry.

**2 Total Changes** | 2 Added

### Added

- **Desktop:** A "What's new" dialog covers every version since the one you had. It shows once and works offline ([#2034](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2034)).
- **Desktop:** This changelog is the canonical public record, and CI refuses a release without its entry ([#2034](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2034)).

## [0.2.21] - 2026-07-02

Windows verifies an update's signature before it installs, and public releases
carry build provenance you can check. Pinning works again, and scroll stays
where you left it.

**12 Total Changes** | 1 Added | 1 Changed | 7 Fixed | 3 Security

### Added

- **Billing:** Avatar, banner, and server-icon size limits scale with your plan ([#2010](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2010)).

### Changed

- **Operations:** Tooling, CI, and documentation upkeep ([#2016](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2016), [#2017](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2017), [#2019](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2019), [#2025](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2025)).

### Fixed

- **Messages:** Server message pinning works again, and pinned media stays in its panel ([#2011](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2011)).
- **Messages:** Switching channels quickly no longer snaps you to a stale position ([#2008](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2008)).
- **Notifications:** DM and notification previews say "Photo" instead of a placeholder ([#2015](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2015)).
- **Messages:** The composer menu renders above the placeholder text ([#2014](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2014)).
- **Server:** The client no longer makes permission checks it was never entitled to make ([#1990](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1990), [#2009](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2009)).
- **Desktop:** UI and app update checks share one path ([#2013](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2013)).
- **Sign-in:** The suggested identity at sign-up no longer carries a stray "@concordvoice.chat" suffix ([#2018](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2018)).

### Security

- **Desktop:** Windows checks the code signature of each update against the expected publisher ([#2023](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2023)).
- **Operations:** Every publicly mirrored desktop release asset carries verifiable build provenance ([#1988](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1988)).
- **Operations:** A misconfigured attestation deployment fails loudly ([#2024](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2024)).

## [0.2.20] - 2026-06-30

Self-hosted servers get every feature with no plan limits. The licence, privacy
policy, and terms are one click away in About.

**2 Total Changes** | 2 Added

### Added

- **Self-hosting:** Self-hosted servers unlock every premium entitlement with no subscription ([#1985](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1985)).
- **Settings:** Settings ▸ About shows the licence, terms, and other notices ([#1987](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1987)).

## [0.2.19] - 2026-06-30

You can sign in to a Concord Voice server that you run yourself. A client stuck
on an old interface can recover without a reinstall.

**3 Total Changes** | 1 Added | 1 Changed | 1 Fixed

### Added

- **Self-hosting:** Desktop sign-in can route to an operator-run server ([#1982](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1982)).

### Changed

- **Operations:** Deployment and tooling upkeep.

### Fixed

- **Desktop:** The app checks the public release feed for updates, so a stranded client can recover ([#1984](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1984)).

## [0.2.18] - 2026-06-29

Certificate pinning is back on for connections to Concord Voice servers.

**1 Total Changes** | 1 Security

### Security

- **Desktop:** Certificate pinning for connections to Concord services is restored after it lapsed ([#1983](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1983)).

## [0.2.17] - 2026-06-29

You can react to DMs with emoji. The app connects reliably at launch, and a
certificate rotation no longer locks you out.

**4 Total Changes** | 1 Added | 1 Changed | 2 Fixed

### Added

- **Messages:** DMs take emoji reactions ([#1976](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1976)).

### Changed

- **Operations:** Tooling, CI, and dependency upkeep ([#1979](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1979)).

### Fixed

- **Chat:** A race in the WebSocket sign-in handshake no longer leaves you disconnected after launch ([#1978](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1978)).
- **Desktop:** The client trusts the rotated API certificate pin ([#1980](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1980)).

## [0.2.16] - 2026-06-29

You choose how much a notification shows on screen. Custom themes are free, and
opening DMs no longer interrupts your call.

**5 Total Changes** | 2 Added | 1 Changed | 2 Fixed

### Added

- **Notifications:** A setting controls how much message content desktop notifications show ([#1970](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1970)).
- **Appearance:** Custom themes are free for everyone ([#1974](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1974)).

### Changed

- **Operations:** A legacy automation credential was retired, and the supply-chain list refresh is guarded ([#1968](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1968), [#1969](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1969)).

### Fixed

- **Voice:** Call audio keeps playing when you open DMs ([#1972](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1972)).
- **Desktop:** macOS relaunches after an update installs ([#1973](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1973)).

## [0.2.15] - 2026-06-29

Moderators can time a member out instead of removing them. Passkey registration
is stricter, and erasing your account revokes the session that asked for it.

**5 Total Changes** | 2 Added | 1 Changed | 2 Security

### Added

- **Servers:** Moderators can time a member out for a set duration ([#1967](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1967)).
- **Desktop:** On first launch outside /Applications, macOS offers to move the app there ([#1966](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1966)).

### Changed

- **Operations:** Tooling, CI, and deployment upkeep.

### Security

- **Sign-in:** Passkey registration requires platform-authenticator options ([#1963](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1963)).
- **Accounts:** Erasing your account revokes your current access token ([#1965](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1965)).

## [0.2.14] - 2026-06-28

A voice channel can set one audio quality for everyone in it. Usernames are
case-insensitive everywhere, and self-hosted certificate failures are reported.

**10 Total Changes** | 3 Added | 2 Changed | 3 Fixed | 2 Security

### Added

- **Voice:** A channel can set one audio quality tier for every member, bounded by the server's tier ([#1946](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1946)).
- **Video:** Settings ▸ Audio & Video has SVC and simulcast toggles ([#1929](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1929)).
- **Self-hosting:** One script provisions a self-signed, Let's Encrypt, or bring-your-own certificate ([#1919](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1919)).

### Changed

- **Settings:** My Profile moved into Settings ▸ Account, and the SSO controls were polished ([#1930](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1930), [#1951](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1951)).
- **Operations:** Tooling, CI, and dependency upkeep.

### Fixed

- **Accounts:** Usernames are case-insensitive everywhere ([#1936](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1936)).
- **Notifications:** Notification settings match the macOS permission state ([#1960](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1960)).
- **Self-hosting:** Self-hosted certificate provisioning errors are reported ([#1964](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1964)).

### Security

- **Sign-in:** Email MFA setup is hardened for production ([#1961](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1961)).
- **Desktop:** The cached-UI origin is on the server allowlists before the cache activates ([#1928](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1928)).

## [0.2.13] - 2026-06-28

Invite bubbles name the person who invited you. Linux updates are verified
before they install, and the signed offline interface cache is live.

**5 Total Changes** | 1 Changed | 2 Fixed | 2 Security

### Changed

- **Operations:** A retired sign-in configuration value was removed ([#1918](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1918), [#1920](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1920)).

### Fixed

- **Invites:** Invite bubbles name the person who sent the invite ([#1909](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1909)).
- **Settings:** System Permissions settings are easier to find and understand ([#1910](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1910)).

### Security

- **Desktop:** Linux AppImage, deb, and rpm updates carry Ed25519 signatures, and an unverified update does not install ([#1923](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1923)).
- **Desktop:** The signed offline UI cache is live and verified before it runs ([#1907](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1907), [#1908](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1908)).

## [0.2.12] - 2026-06-27

Your camera and screen share stay live when the call switches video quality
layers.

**3 Total Changes** | 1 Changed | 2 Fixed

### Changed

- **Operations:** Engineering write-ups, storage-config groundwork, and static-analysis cleanup ([#1900](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1900), [#1901](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1901), [#1906](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1906)).

### Fixed

- **Video:** Camera and screen share stay live when video layers switch ([#1903](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1903)).
- **Accounts:** Age-verification status survives a reload ([#1904](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1904)).

## [0.2.11] - 2026-06-26

Hardware video encoding is offered only when your graphics hardware supports
it.

**1 Total Changes** | 1 Fixed

### Fixed

- **Video:** Hardware encoding uses only codecs your GPU accelerates ([#1879](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1879)).

## [0.2.10] - 2026-06-26

You can run your own Concord Voice instance. AV1 video no longer goes black in
encrypted calls.

**3 Total Changes** | 1 Added | 1 Changed | 1 Fixed

### Added

- **Self-hosting:** A guided installer sets up a self-hosted instance, and `concord-selfhost` manages it ([#1892](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1892)).

### Changed

- **Operations:** Supply-chain indicators refreshed, and macOS builds retry a transient DMG flake ([#1877](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1877), [#1894](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1894)).

### Fixed

- **Video:** AV1 video decrypts reliably in encrypted calls (frame crypto v4) ([#1896](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1896)).

## [0.2.9] - 2026-06-24

Video no longer goes black after a key rotates mid-call, and the offline
interface cache is signed.

**6 Total Changes** | 2 Added | 1 Changed | 2 Fixed | 1 Security

### Added

- **Desktop:** The app keeps a signed last-known-good copy of the interface ([#1880](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1880)).
- **Server:** `GET /api/v1/server/capabilities` tells a client what a server supports ([#1883](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1883)).

### Changed

- **Operations:** Tooling and workflow upkeep ([#1884](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1884), [#1888](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1888)).

### Fixed

- **Video:** Video frames carry their key version, so video survives a mid-call key rotation ([#1885](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1885)).
- **Operations:** Public release mirroring triggers only on real releases ([#1887](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1887)).

### Security

- **Desktop:** Packaged builds accept only the official API address or a verified self-hosted server ([#1882](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1882)).

## [0.2.8] - 2026-06-24

Audio devices switch mid-call, DM calls open with sound, and encrypted video
keeps up with layered streams. Older DM attachments decrypt again.

**9 Total Changes** | 2 Changed | 7 Fixed

### Changed

- **Invites:** The invite landing page is restyled ([#1869](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1869)).
- **Operations:** Tooling, CI, and test upkeep, including automated contrast checks ([#1864](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1864), [#1871](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1871), [#1875](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1875)).

### Fixed

- **Desktop:** The client retries the latest UI after a cold-start network problem, and session-only sign-in survives a reload ([#1872](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1872)).
- **Video:** Encrypted video stays in sync with layered and simulcast streams ([#1865](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1865)).
- **Voice:** You can switch audio devices mid-call ([#1866](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1866)).
- **Voice:** DM calls open the call view and play remote audio ([#1876](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1876)).
- **Messages:** Older DM attachments decrypt correctly ([#1867](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1867)).
- **Presence:** Your DM list shows accurate online status ([#1874](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1874)).
- **Messages:** Enlarged images are centered ([#1868](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1868)).

## [0.2.7] - 2026-06-23

> Versions 0.2.4 to 0.2.6 never shipped. Everything they carried ships here.

Images open in a lightbox, and invite links can be public. Camera quality adapts
in group calls, and video recovers once a key catches up.

**10 Total Changes** | 3 Added | 1 Changed | 5 Fixed | 1 Security

### Added

- **Messages:** Click an image to see it full size, zoom, and save it ([#1813](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1813)).
- **Invites:** Public invite links open a landing page ([#1821](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1821)).
- **Video:** SVC-first camera layers send each viewer the resolution they need ([#1831](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1831)).

### Changed

- **Dependencies:** Electron 42.4.1, mediasoup 3.20.9, and release-pipeline upkeep.

### Fixed

- **Video:** Encrypted video recovers after a key epoch desync ([#1784](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1784)).
- **Voice:** Output device changes apply mid-call ([#1823](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1823)).
- **Messages:** A batch of chat and UI fixes for DMs, mentions, lists, code, menus, and voice frames ([#1798](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1798) to [#1836](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1836)).
- **Desktop:** Linux packages launch reliably, show menu icons, and keep RPM sandbox permissions ([#1801](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1801), [#1825](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1825), [#1863](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1863)).
- **Server:** Media uploads return "service unavailable" when storage is disabled ([#1826](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1826)).

### Security

- **Servers:** Channel permissions are enforced on the WebSocket path as well as REST ([#1811](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1811)).

## [0.2.3] - 2026-06-22

The composer shows how many characters you have left and keeps what you typed
when you go over.

**2 Total Changes** | 1 Added | 1 Changed

### Added

- **Messages:** A live character counter flags over-limit text before you send ([#1709](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1709)).

### Changed

- **Operations:** Build, scanning, and documentation upkeep ([#1780](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1780), [#1781](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1781), [#1764](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1764), [#1783](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1783)).

## [0.2.2] - 2026-06-22

Premium features show what they unlock and can be redeemed with a code. Voice
crackle is fixed, and the app reconnects after a server deploy.

**8 Total Changes** | 5 Added | 1 Changed | 2 Fixed

### Added

- **Billing:** Premium lock badges, redemption codes, and server-enforced quality limits per plan ([#1724](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1724), [#1730](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1730), [#1737](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1737)).
- **Feedback:** A public Known Issues list is linked from the feedback dialog ([#1736](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1736), [#1769](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1769)).
- **Desktop:** Settings ▸ About has a "Load latest UI" button, and launch retries it ([#1779](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1779)).
- **Operations:** Operator console sign-in needs a password and a WebAuthn hardware key ([#1703](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1703)).
- **GIFs:** New accounts load KLIPY GIFs automatically. Existing choices do not change ([#1774](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1774)).

### Changed

- **Operations:** Feedback triage, mirror fixes, and opt-in audio diagnostics ([#1731](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1731), [#1733](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1733), [#1762](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1762), [#1771](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1771), [#1772](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1772), [#1775](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1775), [#1776](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1776)).

### Fixed

- **Voice:** Small calls no longer clip or garble the first words of speech ([#1765](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1765), [#1777](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1777)).
- **Chat:** The app reconnects after a server deploy, and a sign-in blip keeps Remember Me ([#1770](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1770)).

---

## [0.2.1] - 2026-06-20

> Patch release. v0.2.0-Beta shipped the macOS client as a `.zip` only. This
> release attaches the signed and notarized `.dmg` installer as well.

Friend categories arrive: group your DMs with your own emoji and colors. The
grouping is encrypted with your key, so the server cannot read it.

**2 Total Changes** | 1 Added | 1 Fixed

### Added

- **Friends:** Friend categories group your DMs, encrypted with your key ([#1704](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1704)).

### Fixed

- **Desktop:** Releases attach the macOS `.dmg` installer as `ConcordVoice-<version>-macos-<arch>.dmg`. Auto-update stays on the `.zip` ([#1722](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1722)).

## [0.2.0-Beta] - 2026-06-20

> Release-level rollup of Phase 2A and Phase 2B. Per-revision detail is in the
> 0.1.12 to 0.1.18 entries below.

The Beta release. Sign in with Google or Apple, add two-step verification or a
passkey, and recover a locked account. Roles and permissions arrive with GIFs,
server moderation, and end-to-end encrypted attachments.

**101 Total Changes** | 38 Added | 8 Changed | 3 Removed | 30 Fixed | 22 Security

### Added

- **Sign-in:** Google sign-in and registration ([#808](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/808)).
- **Sign-in:** Sign in with Apple, aware of Apple's private relay ([#824](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/824)).
- **Security:** TOTP, WebAuthn/FIDO2, backup codes, recovery circles, and trusted devices ([#202](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/202)).
- **Accounts:** `POST /api/v1/privacy/erase-account` erases an account across every linked table ([#717](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/717)).
- **Accounts:** Zero-knowledge account key recovery ([#328](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/328)).
- **Servers:** Role- and server-based access control with audit logging ([#242](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/242), [#548](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/548)).
- **Server:** User images moved from PostgreSQL to S3-compatible storage with two-tier access ([#325](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/325)).
- **Servers:** Server ownership transfer with MFA, email confirmation, and reversal tokens ([#351](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/351)).
- **Sign-in:** Email verification on registration, later sent through Resend ([#273](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/273)).
- **Sign-in:** Registrations stay pending for 15 minutes until verified ([#688](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/688)).
- **Messages:** Reactions, replies, pinning, encrypted search, attachments, drafts, notifications, shortcuts, group DMs, and extended Markdown ([#459](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/459), [#463](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/463), [#465](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/465), [#468](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/468), [#470](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/470), [#477](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/477), [#478](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/478), [#479](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/479), [#472](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/472), [#711](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/711)).
- **GIFs:** KLIPY GIF search and picker through a privacy proxy ([#557](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/557)).
- **Voice:** Server-enforced mute and deafen, applied at the media server ([#546](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/546)).
- **Notifications:** @mention routing in encrypted channels without decryption ([#310](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/310)).
- **Encryption:** DM key-epoch enforcement and revocation ([#298](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/298)).
- **Encryption:** Channel keys rotate when a member is removed.
- **Desktop:** Auto-updater with rollback, splash screen, progress, and error states ([#155](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/155)).
- **Desktop:** Server-proxied desktop updates with no per-client telemetry ([#264](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/264)).
- **Desktop:** Hot-update SPA pipeline with a bundle-hash deploy contract ([#429](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/429), [#773](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/773)).
- **Desktop:** The desktop falls back to the bundled SPA when a hot update fails ([#831](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/831), [#832](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/832), [#835](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/835)).
- **Chat:** WebSocket reconnect race fix and subscribe-barrier protocol ([#769](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/769)).
- **Self-hosting:** Self-hosted coturn STUN/TURN ([#124](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/124), [#577](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/577)).
- **Server:** Public media proxy for public assets ([#570](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/570)).
- **Sign-in:** Token theft detection binds sessions to machine ID and IP ([#89](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/89), [#702](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/702)).
- **Sign-in:** The main process refreshes the access token before expiry ([#329](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/329)).
- **Appearance:** Per-user profile theming, profile cards, and avatar theming ([#251](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/251), [#252](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/252)).
- **Appearance:** Image crop editors for profile and server images ([#357](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/357)).
- **Accounts:** Username rules, period support, and a yearly change cooldown ([#330](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/330)).
- **Desktop:** OS-level permission management ([#321](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/321)).
- **Friends:** Friend requests with accept and decline ([#203](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/203)).
- **Notifications:** Notification sounds, per-category volumes, and looping DM call sounds ([#375](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/375), [#394](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/394), [#743](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/743), [#554](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/554)).
- **Desktop:** A Developer Mode setting enables DevTools in Alpha and Beta builds ([#567](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/567)).
- **Desktop:** macOS builds are signed and notarized ([#641](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/641)).
- **Desktop:** Windows `Setup.exe` is signed ([#649](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/649)).
- **Operations:** Docker network segmentation, request-ID propagation, and Redis auth ([#442](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/442)).
- **Operations:** GitHub Actions CI with tests, coverage, SonarQube, and Semgrep ([#128](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/128), [#130](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/130), [#457](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/457)).
- **Operations:** A supply-chain IOC scanner ([#722](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/722), [#781](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/781)).
- **Operations:** An AI-generated code governance framework ([#454](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/454) to [#458](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/458), [#500](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/500) to [#522](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/522)).

### Changed

- **Dependencies:** React 19.2.4, react-router-dom 7.13.1, Zustand 5.0.12, ESLint 10 flat config, and Vite 8.0.0 ([#181](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/181), [#182](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/182), [#183](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/183), [#184](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/184), [#185](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/185), [#186](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/186), [#287](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/287)).
- **Dependencies:** Go 1.26.1 with `govulncheck` ([#193](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/193)).
- **Dependencies:** Electron 41, mediasoup 3.19.18, mediasoup-client 3.18.7, and TypeScript 6.0.2.
- **Encryption:** The client derives the password key with Argon2id instead of PBKDF2 ([#117](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/117)).
- **Operations:** macOS notarization uses an App Store Connect API key ([#826](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/826)).
- **Operations:** Lower cognitive complexity across Go handlers and TypeScript code ([#418](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/418), [#419](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/419), [#498](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/498), [#505](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/505), [#550](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/550) to [#553](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/553)).
- **Operations:** Documentation audit for the Beta release gate ([#214](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/214), [#823](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/823)).
- **Operations:** MCP server config split into per-host files ([#838](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/838)).

### Removed

- **Operations:** Sentry telemetry removed from all three services, and its MCP config removed ([#770](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/770), [#780](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/780), [#793](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/793), [#796](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/796), [#838](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/838)).
- **Operations:** The unused Postgres MCP server config ([#838](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/838)).
- **Desktop:** The deprecated `WebSocketMessage` type alias. `WebSocketEvent` is the canonical name ([#1185](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1185), [#1184](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1184)).

### Fixed

- **Encryption:** Channel-key OperationError reports a structured diagnostic ([#765](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/765)).
- **Encryption:** WebRTC BUNDLE no longer misroutes encrypted voice and video between consumers ([#291](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/291), [#292](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/292), [#293](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/293)).
- **Encryption:** Media encryption moved from Insertable Streams to RTCRtpScriptTransform ([#355](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/355)).
- **Encryption:** A session-scoped cache stops the key request flood ([#241](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/241)).
- **Encryption:** Frame decryption recovers, and CSK rotation is hardened ([#232](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/232), [#284](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/284)).
- **Encryption:** DM key epochs, presence sync, and key distribution are fixed ([#298](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/298)).
- **Messages:** Replied-to messages decrypt on REST fetch ([#542](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/542)).
- **Messages:** DM threads update their preview and order in real time ([#541](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/541)).
- **Messages:** Message editing works in encrypted channels and DMs ([#220](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/220)).
- **Voice:** Black-screen recovery, codec selection, screen-share audio, and audio that persists across navigation ([#198](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/198), [#227](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/227), [#295](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/295), [#299](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/299), [#396](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/396)).
- **Server:** Hub goroutine races and flaky voice tests are resolved ([#476](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/476), [#544](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/544)).
- **Chat:** A lost connection reconnects the WebSocket instead of reloading the page ([#194](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/194), [#439](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/439)).
- **Presence:** You no longer show as Offline while connected ([#1535](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1535)).
- **Server:** The Postgres "invalid length of startup packet" flood is fixed ([#779](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/779)).
- **Voice:** The PiP window loads the SPA route ([#815](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/815)).
- **Voice:** PiP signaling and local user identification are fixed ([#415](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/415)).
- **Desktop:** Login on the bundled SPA fallback uses the `app://` scheme ([#832](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/832)).
- **Sign-in:** The `/mfa/verify` response parses `access_token` ([#814](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/814)).
- **Desktop:** The updater trust path is hardened on macOS, Windows, and CI ([#655](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/655)).
- **Operations:** CI verifies ASAR integrity ([#686](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/686)).
- **Desktop:** The preload script is bundled with esbuild for the sandbox ([#678](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/678)).
- **Operations:** Docker Compose passes the MFA encryption key ([#225](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/225)).
- **GIFs:** The KLIPY media proxy no longer returns 401, and its routes sit under `/gifs` ([#687](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/687), [#580](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/580)).
- **GIFs:** KLIPY GIFs render on every decrypt path ([#566](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/566)).
- **Operations:** NATS config and coturn TLS and external IP are fixed ([#576](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/576), [#577](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/577)).
- **Desktop:** Escape closes only the top modal ([#480](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/480)).
- **Messages:** Server role styling no longer affects DMs ([#543](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/543)).
- **Accessibility:** Semantic HTML, keyboard navigation, and ARIA pass ([#380](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/380), [#427](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/427), [#482](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/482)).
- **Messages:** A Markdown syntax help dialog and help icon ([#748](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/748)).
- **Operations:** Beacon verification, the `/spa/` nginx route, and a defensive sentinel ([#766](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/766)).

### Security

- **Desktop:** Main-process logs no longer pass raw `Error` objects ([#714](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/714)).
- **Sign-in:** Ten token-suffix leaks are removed from the token manager ([#704](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/704)).
- **Desktop:** Passive navigation allows only `https:`. Clicked links still allow `http:`, `https:`, and `mailto:` ([#774](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/774)).
- **Desktop:** The updater pins the TLS certificate for `api.concordvoice.chat` ([#719](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/719)).
- **Operations:** nginx closes H2C smuggling and Host header injection ([#525](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/525)).
- **Operations:** A GitHub Actions shell injection is resolved ([#523](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/523)).
- **Server:** CORS rejects null or empty origins and validates custom headers ([#259](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/259)).
- **Server:** Hardcoded dev credentials are removed, and production guards are added ([#260](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/260)).
- **Operations:** Infrastructure resists brute-force probes and vulnerability scanners ([#153](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/153), [#189](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/189)).
- **Dependencies:** npm overrides fix transitive vulnerabilities ([#289](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/289), [#314](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/314)).

**Known issues** (not counted as changes):

- The "No Internet" Retry button can stay stuck after the network returns. Quit and relaunch ([#817](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/817)).
- Chat Markdown has heading size, code block, and spacing errors ([#807](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/807)).
- The Pinned Messages panel drops media and shows GIFs as raw text ([#805](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/805)).
- The GIF picker can hit a rate limit while you scroll ([#804](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/804)).
- The Member List "+ Add Role" dropdown is clipped inside its card ([#799](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/799)).
- The profile editor can lose unsaved edits when your user record refreshes ([#707](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/707)).

**Upgrading from v0.1.0-Alpha** (not counted as changes):

- No end-user breaking changes. New features are additive.
- Migrations 000054 to 000061 apply on first server run. Migration 000059 makes `refresh_tokens.user_id` cascade on delete.
- The client re-wraps legacy PBKDF2 keys with Argon2id at next login (migration 000034) ([#117](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/117)).
- Passive navigation allows only `https:` ([#774](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/774), [#1172](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1172)).
- A password change revokes refresh tokens, so other devices sign out at their next refresh.
- Self-hosted operators who want Apple sign-in must set the Apple credentials in the control-plane `.env`.

---

## [0.1.41] - 2026-05-20

Windows and Linux get native window buttons, and the app gets a branded title
bar. You can now choose what the close and minimize buttons do.

**7 Total Changes** | 4 Added | 3 Changed

### Added

- **Desktop:** Windows and Linux show native close, minimize, and maximize buttons. macOS keeps its traffic lights ([#806](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/806)).
- **Desktop:** A branded title bar shows the app name, the version, and the active SPA build, and updates when the SPA updates.
- **Desktop:** The window remembers its size, position, and maximized state, and checks the saved position against your current displays.
- **Desktop:** Settings ▸ Appearance ▸ Client Behavior lets you send the close and minimize buttons to the tray, the taskbar or dock, or quit. Invalid combinations are greyed out with a reason. The tray icon itself ships with ([#1099](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1099)).

### Changed

- **Desktop:** Four new window IPC channels and one SPA version event. Each handler validates its input.
- **Desktop:** Picture-in-picture windows no longer draw an OS shadow on macOS.
- **Appearance:** The Windows and Linux title bar follows your theme, including the system light or dark setting.

---

## [0.1.40] - 2026-05-20

v0.1.39 never published because the Linux build failed. This release carries
all of v0.1.39 plus the Linux build fix.

**2 Total Changes** | 1 Changed | 1 Fixed

### Fixed

- **Desktop:** Linux builds package again. A display-name change had renamed the Linux binary to one with a space, so the packager could not find it. Linux keeps `concord-voice`, and macOS and Windows keep "Concord Voice".

### Changed

- **Desktop:** The packaging test checks the binary name per platform.

---

## [0.1.39] - 2026-05-20

The release workflow publishes again after it skipped every release since
v0.1.34. This release also carries the unshipped v0.1.36 to v0.1.38 changes.

**8 Total Changes** | 6 Changed | 2 Fixed

### Changed

- **Desktop:** The product name is "Concord Voice" everywhere, including the Dock, the Windows registry, and the Linux desktop entry.
- **Desktop:** The company name is "Concord Voice LLC" in the license and About screen.
- **Notifications:** You can mute messages and voice separately for each server and DM ([#985](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/985), [#84](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/84)).
- **Messages:** The server no longer sends an `is_encrypted` field, because every room is encrypted. WebSocket messages without `key_version` 1 or higher are refused with close code 4400 ([#1042](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1042), [#201](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/201)).
- **Messages:** The desktop no longer reads `is_encrypted` ([#1031](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1031)).
- **Voice:** The media server drops `is_encrypted` and documents that SRTP is always on ([#1032](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1032)).

### Fixed

- **Desktop:** The release workflow no longer skips publishing on a push to main.
- **Desktop:** The first WebSocket connection drop no longer logs noise, and the auth ticket no longer shows in the console ([#1046](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1046)).

---

## [0.1.18] - 2026-04-09

The desktop reports crashes and errors, and scrubs private data before it
sends them.

**3 Total Changes** | 2 Added | 1 Changed

### Added

- **Desktop:** Error tracking for the main and renderer processes. It fails closed and drops key material, personal data, and console logs before it sends a report ([#586](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/586)).
- **Development:** A documentation-drift reviewer for pull requests.

### Changed

- **Development:** The project guides and architecture docs describe the current state of the app.

---

## [0.1.17] - 2026-04-09

A quality pass fixes GIF routes and hardens the voice relay servers.

**6 Total Changes** | 1 Added | 1 Changed | 4 Fixed

### Added

- **Development:** A script keeps the internal project counts current ([#581](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/581)).

### Changed

- **Development:** Tool server settings use one naming scheme ([#581](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/581)).

### Fixed

- **GIFs:** GIF requests reach the right server route ([#580](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/580)).
- **Development:** Review comments from an earlier pull request are addressed ([#579](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/579)).
- **Voice:** The voice relay server uses its own TLS certificate, covers its hostname, and renews it automatically.
- **Operations:** The message bus and voice relay settings are correct for a single server.

---

## [0.1.16] - 2026-04-08

Public images load without sign-in, and a quality pass fixes many small bugs.

**3 Total Changes** | 1 Added | 2 Fixed

### Added

- **Servers:** Public images load without sign-in ([#570](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/570)).

### Fixed

- **Appearance:** Avatars keep a fixed 40 by 40 pixel size, so the layout no longer shifts ([#570](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/570)).
- **Desktop:** A broad sweep fixes app, server, and infrastructure bugs ([#571](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/571)).

---

## [0.1.15] - 2026-04-07

GIF search arrives, and the server fetches GIFs for you so the GIF provider
never sees your address.

**9 Total Changes** | 2 Added | 1 Changed | 6 Fixed

### Added

- **GIFs:** GIF search and a GIF picker. The server fetches GIFs on your behalf, and the About screen credits the provider ([#483](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/483), [#557](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/557)).
- **Desktop:** Alpha builds have a Developer Mode switch that opens DevTools ([#567](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/567)).

### Changed

- **Dependencies:** Routine updates to build, test, and sign-in libraries ([#559](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/559), [#560](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/560), [#561](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/561), [#564](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/564), [#565](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/565)).

### Fixed

- **Desktop:** Windows builds work again ([#569](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/569)).
- **Messages:** Chat messages, GIFs, image crops, and SPA updates display correctly ([#566](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/566)).
- **GIFs:** The GIF provider logo follows your theme.
- **GIFs:** GIFs decrypt in history as well as in live chat, and every image size loads.
- **Desktop:** The SPA keeps its strict content security policy on every response.
- **GIFs:** The GIF picker and its settings work with a keyboard and screen reader.

---

## [0.1.14] - 2026-04-06

Server roles now control the right-click moderation actions. Voice code is
split into smaller parts.

**2 Total Changes** | 1 Changed | 1 Fixed

### Changed

- **Voice:** Voice settings, the voice service, encryption helpers, and codec choice are split into smaller parts ([#550](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/550), [#551](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/551), [#552](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/552), [#553](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/553)).

### Fixed

- **Servers:** Right-click kick, ban, and mute follow your server role ([#548](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/548)).

---

## [0.1.13] - 2026-04-04

Chat gets reactions, replies, pins, search, attachments, and group DMs.
Servers can mute and deafen members, and the voice server enforces it.

**34 Total Changes** | 15 Added | 7 Changed | 10 Fixed | 2 Security

### Added

- **Voice:** Server mute and deafen, enforced by the voice server ([#488](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/488)).
- **Messages:** Emoji reactions that sync live ([#169](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/169), [#459](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/459)).
- **Messages:** Replies that quote the original message ([#170](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/170), [#463](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/463)).
- **Messages:** Pinned messages with a pin list for each channel ([#171](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/171), [#465](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/465)).
- **Messages:** Encrypted search that decrypts and searches history on your device ([#172](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/172), [#468](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/468)).
- **Messages:** File and image attachments with preview and download ([#178](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/178), [#470](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/470)).
- **Messages:** Group DMs with admin controls ([#208](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/208)).
- **Messages:** Unsent drafts are saved for each channel ([#174](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/174), [#477](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/477)).
- **Notifications:** Desktop notifications for mentions and new DMs ([#175](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/175), [#478](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/478)).
- **Desktop:** Configurable keyboard shortcuts with a help overlay ([#176](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/176), [#479](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/479)).
- **Desktop:** One right-click menu across the app, with copy and role assignment ([#446](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/446)).
- **Desktop:** The SPA can update without a new desktop release ([#429](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/429)).
- **Development:** Shared chat logic for fetch, paging, decryption, and send ([#545](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/545)).
- **Operations:** Services run on separate networks, requests carry an ID, and Redis requires auth ([#442](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/442)).
- **Development:** An AI-generated code policy, code owners, and static analysis ([#454](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/454), [#458](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/458), [#500](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/500), [#504](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/504), [#507](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/507), [#518](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/518), [#522](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/522)).

### Changed

- **Messages:** The message component is split into smaller parts ([#451](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/451)).
- **Messages:** The DM list updates live with the latest message ([#541](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/541)).
- **Dependencies:** TypeScript 6, ESLint 10, mediasoup, Electron, and other updates ([#411](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/411), [#414](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/414), [#511](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/511), [#528](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/528), [#529](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/529), [#532](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/532), [#536](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/536), [#538](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/538), [#539](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/539)).
- **Accounts:** Account email uses a new provider with branded templates.
- **Development:** CI adds static analysis, quality gates, test sharding, and caching ([#457](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/457), [#471](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/471)).
- **Development:** Server and app code is simpler to read ([#418](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/418), [#419](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/419), [#498](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/498)).
- **Desktop:** Modals share one set of panels ([#365](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/365), [#481](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/481)).

### Fixed

- **Messages:** Replied-to messages decrypt when history loads, not only live ([#542](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/542)).
- **Messages:** Server role colors no longer appear in DMs ([#543](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/543)).
- **Development:** WebSocket and voice tests no longer depend on timing ([#476](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/476), [#544](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/544)).
- **Servers:** Assigning a role from the right-click menu reports failures ([#447](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/447)).
- **Desktop:** Escape closes only the top modal ([#480](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/480)).
- **Video:** Video fills the voice area ([#443](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/443)).
- **Desktop:** A lost connection reconnects without a page reload ([#194](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/194), [#439](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/439)).
- **Accessibility:** Semantic markup, keyboard navigation, and screen-reader labels across the app ([#380](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/380), [#427](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/427)).
- **Development:** Safe database query warnings are marked as reviewed ([#524](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/524)).
- **Voice:** The media server reaches Redis.

### Security

- **Operations:** The web server blocks request smuggling and Host header injection ([#525](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/525)).
- **Development:** A CI workflow no longer allows shell injection ([#523](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/523)).

---

## [0.1.12] - 2026-03-27

Accounts get two-factor sign-in, security keys, and email verification.
Servers get roles and permissions, and the desktop updates itself.

**36 Total Changes** | 16 Added | 6 Changed | 10 Fixed | 4 Security

### Added

- **Accounts:** Two-factor sign-in with authenticator apps, security keys, backup codes, recovery contacts, and trusted devices ([#89](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/89)).
- **Servers:** Roles and permissions with an audit log ([#82](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/82)).
- **Accounts:** Email verification at sign-up ([#269](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/269)).
- **Servers:** Images are kept in object storage, not the database ([#166](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/166)).
- **Servers:** Ownership transfer with two-factor confirmation, email confirmation, and a way to reverse it ([#244](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/244)).
- **Desktop:** Automatic updates with rollback, a splash screen, and progress ([#155](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/155), [#381](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/381), [#387](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/387)).
- **Accounts:** A stolen sign-in token is revoked when it appears on another device or address ([#89](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/89)).
- **Accounts:** Sign-in tokens refresh before they expire ([#240](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/240), [#254](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/254)).
- **Messages:** Removing a member rotates the channel key ([#96](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/96)).
- **Messages:** Revoked DM keys are refused on the live connection ([#122](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/122)).
- **Messages:** Mentions reach people without the server reading the message ([#118](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/118)).
- **Desktop:** The app asks for OS permissions and respects them ([#197](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/197)).
- **Voice:** A self-hosted relay helps calls connect through strict networks ([#124](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/124)).
- **Development:** CI runs tests, coverage, and code quality checks in parallel ([#128](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/128), [#130](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/130)).
- **Development:** Many new tests toward the 80% coverage gate.
- **Desktop:** Install and update steps are logged to a file ([#383](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/383)).

### Changed

- **Dependencies:** React 19, React Router 7, Zustand 5, Vite 8, Go 1.26, Electron 41, and mediasoup 3.19 ([#181](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/181), [#182](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/182), [#183](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/183), [#287](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/287), [#193](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/193)).
- **Development:** ESLint 10 flat config and Redis client 5 ([#184](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/184), [#185](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/185), [#186](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/186), [#187](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/187)).
- **Messages:** Encryption keys are derived with Argon2id instead of PBKDF2 ([#117](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/117)).
- **Messages:** Shared message fetch, decrypt, and paging logic ([#177](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/177)).
- **Desktop:** The app is named "Concord Voice" on every platform, with the correct version, icon, and publisher ([#385](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/385), [#386](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/386)).
- **Desktop:** The updater no longer shares files with other Electron apps ([#382](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/382)).

### Fixed

- **Voice:** Encrypted voice and video no longer mix up streams ([#291](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/291)).
- **Voice:** The microphone works in encrypted voice on macOS ([#295](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/295)).
- **Video:** Codec choice and screen-share sound work ([#299](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/299)).
- **Voice:** Audio continues when you move to another view ([#396](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/396)).
- **Video:** Black screens and missing audio in calls are fixed ([#227](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/227)).
- **Messages:** Edits save ([#161](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/161)).
- **Appearance:** Profiles use the owner's color scheme ([#165](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/165)).
- **Desktop:** Clicking a popover's button again closes it ([#167](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/167)).
- **Screen share:** Screen sharing uses your video settings ([#198](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/198)).
- **Servers:** Changing a role no longer crashes the app ([#249](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/249)).

### Security

- **Operations:** The server refuses empty or null origins and checks custom headers ([#259](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/259)).
- **Operations:** Development credentials are removed from the code, with production guards ([#260](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/260)).
- **Operations:** The infrastructure blocks brute-force scans ([#153](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/153)).
- **Dependencies:** Vulnerable indirect packages are pinned to fixed versions ([#289](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/289)).

---

## [0.1.0-alpha] - 2026-03-03

The first alpha: encrypted accounts, servers and text chat, voice and video,
and the desktop app.

**4 Total Changes** | 4 Added

### Added

- **Accounts:** Sign-up, sign-in, sessions, Argon2id passwords, and rate limits, with end-to-end encryption using RSA-OAEP 4096 and AES-256-GCM.
- **Servers:** Servers and channels, live encrypted messaging, presence, 12 color schemes, and an OpenAPI 3.0 reference.
- **Voice:** Voice, video, and screen sharing with 7 audio quality tiers, VP9, AV1, VP8, and H.264, channel categories, DMs, an emoji picker, a theme builder, and a mic test.
- **Operations:** Docker Compose for development, staging, and production, a relay server, cross-platform desktop builds, and pre-commit hooks.
